# Session: SCRAM-SHA-256 auth + TLS on pgwire

- **Session ID**: `73ec770b-d423-4d13-9156-788797cbca68`
- **Date**: 2026-07-11
- **Prior context**: follows the production-hardening session
  (`97ebb36`); the audit's remaining blocker was "trust-all auth, no
  TLS". User asked: "start SCRAM auth plus TLS on pgwire".
- **State at end**: implemented, all pgwire tests green including
  `-race`, plus an end-to-end smoke test of the real `bytdbd` binary
  (TLS + auth file + pgx client). Not yet done: SCRAM-SHA-256-PLUS
  (channel binding) — see Follow-ups.

## What was built

Three new optional `Server` fields (`pgwire/pgwire.go`); all nil/false
defaults preserve the old trust-everyone, decline-SSL behavior:

- **`TLSConfig *tls.Config`** — server answers `SSLRequest` with `'S'`
  and upgrades via `tls.Server`; nil still declines with `'N'`.
- **`RequireTLS bool`** — refuses non-TLS startups with FATAL `28000`,
  like a hostssl-only pg_hba.conf.
- **`Auth *Credentials`** — SCRAM-SHA-256 (RFC 5802/7677) required
  when set; the registry is RWMutex-guarded and may be re-keyed live.

### `pgwire/auth.go` (new — the whole SCRAM server)

- `Credentials`: user → verifier registry. `SetPassword` (derives),
  `SetVerifier` (parses Postgres `rolpassword` format
  `SCRAM-SHA-256$<iter>:<b64salt>$<b64StoredKey>:<b64ServerKey>`),
  `MakeVerifier` (for building auth files without plaintext).
- Key schedule uses stdlib `crypto/pbkdf2` (Go ≥1.24) with 4096
  iterations (Postgres default), 16-byte salts.
- **SASLprep**: passwords normalized with `precis.OpaqueString`
  (golang.org/x/text) — the *same profile pgx applies client-side*
  (see `pgconn/auth_scram.go`), so non-ASCII passwords derive
  identical keys on both ends; falls back to raw bytes on
  normalization failure, mirroring Postgres `pg_saslprep`.
- **Unknown-user mock**: `lookup` hands out a deterministic fake
  verifier (HMAC of the username under a per-server random `mockKey`)
  so unknown users run the full exchange and fail only at proof
  verification — same message ("password authentication failed for
  user ..."), same SQLSTATE (28P01), same timing as a wrong password.
  Salt is stable per user (a fresh salt per attempt would be a tell).
- `scramConv` carries the conversation; proof check recovers ClientKey
  from `proof XOR HMAC(StoredKey, AuthMessage)` and compares
  `H(ClientKey)` to StoredKey in constant time, with the mocked flag
  folded in *after* the arithmetic. Server signature (`v=`) gives
  mutual auth.
- gs2 header: accepts `n,,` and `y,,` — **pgx sends `y,,` on TLS**
  when only SCRAM-SHA-256 is advertised (verified in pgx source);
  `p=` is rejected (channel binding not advertised). The `c=` echo in
  client-final is verified against the actual received header.
- The SCRAM username (`n=`) is ignored; the startup-message `user` is
  what authenticates, as in Postgres.

### `pgwire/conn.go` startup rework

- `maybeStartTLS`: refuses bytes already buffered behind the
  SSLRequest (`c.r.Buffered() > 0`) — a compliant client waits for
  `'S'`, so pipelined bytes are a plaintext-smuggling attempt. After
  handshake, `c.nc/c.r/c.w` are rebuilt around the `tls.Conn`;
  `tlsOn` gates RequireTLS and refuses a second SSLRequest with `'N'`.
- Startup params are now parsed (`startupParams`) for `user`.
- **`startupTimeout` = 30s** read deadline over the whole pre-session
  phase (startup + TLS handshake + SCRAM), cleared once the session is
  established — Postgres's `authentication_timeout` idea. Must be
  cleared explicitly because `armIdleDeadline` only manages deadlines
  when `idleTx > 0`.
- CancelRequest stays reachable **without** auth, deliberately: the
  crypto-random secret is the credential, and the issuing session may
  be blocked inside a query, unable to authenticate a second
  connection. pgx's cancel connection repeats SSLRequest + TLS then
  sends CancelRequest instead of a startup message — works unchanged.

### `pgwire/proto.go`

Added `msgSASLResponse = 'p'` and auth request codes `authOK`(0),
`authSASL`(10), `authSASLContinue`(11), `authSASLFinal`(12).

### `cmd/bytdbd`

New flags: `-tls-cert`/`-tls-key` (both or neither; MinVersion TLS1.2
since Go's server default still admits 1.0), `-require-tls`,
`-auth-file` (lines of `user:password` or `user:SCRAM-SHA-256$...`;
split on *first* colon — verifiers contain colons; `#` comments).
All config validated **before** `bytdb.Open` so `log.Fatal` never
skips the deferred `e.Close()`.

## Tests (`pgwire/auth_tls_test.go`, new)

All through real pgx: `TestTLS` (asserts `*tls.Conn` under the driver,
plaintext still welcome without RequireTLS), `TestRequireTLS` (28000
FATAL), `TestSCRAMAuth` (good login; wrong password and unknown user
both 28P01; live re-key), `TestSCRAMOverTLS` (the `y,,` gs2 path),
`TestVerifierAndSASLprep` (MakeVerifier→SetVerifier round trip;
password containing U+00A0 — a char SASLprep actually rewrites — via
`pgx.ConnectConfig` since the char won't URL-encode; malformed
verifiers rejected up front), `TestCancelWithAuth` (CancelRequest
through TLS+auth; uses explicit `PgConn().CancelRequest` like
cancel_test.go — a ctx-deadline Exec makes pgx force-close the conn
instead).

Self-signed test certs generated in-process (ECDSA P-256, IP SAN
127.0.0.1); pgx `sslmode=require` doesn't verify chains (libpq
semantics) so no CA setup needed.

## Gotchas hit

- bytdb has no `CREATE TABLE IF NOT EXISTS` — test helper failed with
  42601 until switched to plain CREATE/DROP per round trip.
- gofmt: import group ordering (`crand` alias sorts after `pbkdf2`)
  and doc-comment list reflow — run `gofmt -w` before finishing.
- `go doc crypto/pbkdf2`: stdlib `pbkdf2.Key` is generic and returns
  `(key, error)`; error only on invalid params → panic as impossible.

## Smoke test (real binary)

openssl-generated P-256 cert + auth file; `bytdbd` on 127.0.0.1:5599
with `-require-tls -auth-file`; scratchpad pgx client verified: round
trip over TLS+SCRAM ok, wrong password 28P01, plaintext 28000. (No
psql on this machine.)

## Follow-ups

- **SCRAM-SHA-256-PLUS** (tls-server-end-point channel binding):
  clients with `channel_binding=require` will refuse this server.
  Needs advertising the PLUS mechanism when `tlsOn` and hashing the
  server cert per RFC 5929.
- Package doc + Server field docs updated; docs site (mkdocs) not
  touched this session.
- Root-module `go.mod`/`go.sum` were already dirty before this session
  (btypedb bump); not part of this change.
