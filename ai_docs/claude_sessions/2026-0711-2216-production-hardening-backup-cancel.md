# Session: Production hardening — audit, WAL header/corruption, backup, cancellation

- **Session ID**: `c6d09bbd-d2a2-439d-97a1-3b1588047a86`
- **Date**: 2026-07-11
- **Prior context**: fresh session on post-`bb4345d` main (after m29).
  User asked what's missing in bytdb for mission-critical production
  use; four parallel audit agents swept durability, concurrency, pgwire
  security/operability, and SQL/testing. User then said: save the
  analysis, implement remediation items 1–3, commit, and assess which
  remaining domain gaps are reasonably addressable.
- **State at end**: all three items implemented and committed across
  two repos (btypedb `5090008` + `571af84`; bytdb `019dbe8` + `97ebb36`),
  every suite green (`btypedb`, `bytdb`, `bytdb/sql`, `bytdb/tuple`,
  `pgwire`), race-detector clean on the concurrency-sensitive tests.
  **Pending**: push btypedb, tag (v0.5.0 suggested), bump bytdb go.mod
  from btypedb v0.4.0 — standalone (non-workspace) bytdb builds break
  until then. Full audit lives at `ai_docs/production-readiness-audit.md`.

## 0. The audit (what drove the work)

Hard blockers found: (1) no backup/restore/PITR/replication of any
kind; (2) mid-file WAL corruption silently truncates everything after
it (`replayLog` treated any CRC failure as end-of-log); (3) hard RAM
ceiling, full-log replay on open, snapshot pinning; (4) zero write
concurrency and *no way to interrupt a runaway writer* (no ctx in the
executor, CancelRequest unimplemented, BackendKeyData secret hardcoded
0); (5) trust-all auth, no TLS; (6) commit-visible-before-durable
window. Remediation order chosen: corruption fix + header → online
backup → cancellation/timeouts → auth/TLS (not yet done).

## 1. btypedb: WAL header + mid-file corruption detection (`5090008`)

btypedb was module-cache-only; cloned to `~/projs/go/btypedb` and added
to bytdb's `go.work` (plus `go mod tidy` after `go work sync` bumped
serr without its go.sum entry).

- **Header** (`wal.go`): 16 bytes — `"btydbLOG"` magic + LE uint32
  version (1) + CRC-32 over the first 12. The whole v1 header is a
  compile-time constant (`logHeader()`), which is what makes a torn
  header write detectable as a strict prefix of it. `prepareHeader`
  classifies on open: empty → write+sync header; magic+valid CRC →
  version-gate (`ErrNewerFormat` if newer); magic prefix shorter than
  16 → torn first creation, start over (safe because the header is
  synced before any record can be written); no magic → legacy format-0,
  records at offset 0. Compaction always writes the header into its
  temp file — that's the legacy upgrade path. First magic byte 'b' is
  outside the 1..4 op range, so a legacy log can never false-match.
- **Corruption vs torn tail** (`wal.go`, `db.go`): `replayLog` now
  returns `(validLen, parsedLen)` — parsedLen advances past every
  CRC-valid record *including members of an incomplete trailing batch*
  (scanning from validLen would misread a torn batch's own intact
  members as post-corruption data). Open scans `[parsedLen, EOF)` with
  `scanForRecord` — every byte offset tried, so framing re-syncs no
  matter how the damage shifted boundaries; a false positive needs a
  CRC-32 collision on plausible framing. Intact record found → refuse
  with `ErrCorrupt` (serr has Unwrap, so `errors.Is` works); nothing →
  torn tail, truncate as before. `WithTruncateAtCorruption()` restores
  salvage behavior. A nested batch header inside a batch leaves
  parsedLen *before* it so the scan flags it (can't come from a crash —
  writes stop at the tear).
- **Gotcha hit**: `TestPowerLossEveryPrefix` prefix 10 + junk — header
  torn mid-write *with sector junk* has valid magic but bad CRC.
  Resolution: on header-CRC mismatch, scan the rest for an intact
  record; only refuse if one exists (bitrot on a live DB), else
  start over (torn first creation). `TestTxEmptyCommit` updated: an
  empty DB file is now `logHeaderSize` bytes, not 0.
- **Tests** (`header_test.go`): header round-trip, legacy open +
  compaction upgrade, newer-version refusal, torn header at every
  prefix ± junk, mid-file bitflip → `ErrCorrupt` then salvage-open
  recovers the prefix, corrupt-header-with-data refusal, torn tail
  still silent.

## 2. btypedb: online backup (`571af84`) + engine passthrough

- **Mechanism** (`backup.go`): the log is append-only, so bytes below a
  captured length are immutable. `BackupTo` takes `compactMu` (pins
  file identity — compaction renames/swaps the fd), snapshots
  `db.file`/`db.walSize` under `mu`, releases, and `io.Copy`s a
  `SectionReader [0, walSize)` while writers keep appending past it.
  Consistency falls out of batch framing (same argument as crash
  recovery). walSize only advances on successful appends, so a torn
  record after a failed append is excluded — backup works in the
  sticky-writeErr read-only state, exactly when it matters most.
  `Backup(destPath)` = temp file + fsync + atomic rename + SyncDir via
  the `fsys` seam. No writer blocking at any point.
- **bytdb**: `Engine.Backup(destPath)` passthrough (`engine.go`);
  `backup_test.go` proves post-backup rows *and* a post-backup CREATE
  TABLE don't leak into the restored copy (catalog and rows share the
  one keyspace).

## 3. bytdb: cancellation + statement_timeout + CancelRequest (`97ebb36`)

- **sql layer**: `DB.ctx` field riding the same copy-the-DB convention
  as `tx` (`runCtx` stamps a copy; the WriteTxn-for-sequences path
  carries it along). `ExecCtx` on DB/Stmt/Session, `ExecStmtCtx` on
  Session. The single row pump is `scanPlan` — it gained a leading ctx
  param: polled once at entry (bounds correlated-subquery and
  inner-join re-entry storms) and every `cancelEvery = 256` rows.
  `joinRun` carries ctx from `env.d.ctx`; `collectPKs` (DELETE) and the
  UPDATE materialize loop pass `d.ctx`. Post-collection sort/agg/window
  phases are not polled — documented limitation, scans dominate.
- **SET/RESET** (`ast.go` SetVar, `parser.go` `setStmt`/`setValue`,
  `describe.go` tag): `SET [SESSION|LOCAL] name {=|TO} value[, ...]`,
  `SET TIME ZONE`, `RESET name`, `RESET ALL`. Session gives
  `statement_timeout` semantics (`parseTimeout`: bare int = ms; units
  us/ms/s/min/h/d; 0/DEFAULT disables; negatives error); all other
  parameters are remembered in `Session.vars` and ignored so driver
  housekeeping succeeds. Bare DB errors on `SET statement_timeout`
  (no session state to honor it) and no-ops the rest. A successful SET
  deliberately does not join an open block (nothing transactional to
  unwind); a failing one aborts the block via the normal path.
- **Session.runCtx** wraps every statement in
  `context.WithTimeout(ctx, s.timeout)` when set.
- **pgwire**: conn gets `pid` + crypto-random `secret` (panic if the
  randomness source fails — a zero fallback would issue forgeable
  keys), assigned at accept and registered in `Server.backends`
  (pid→conn, under `s.mu`, deregistered with the conn). BackendKeyData
  sends the real pair. `codeCancelRequest` in startup reads pid+secret,
  calls `Server.cancelBackend` (silent on miss — no probing oracle),
  returns `errCancelHandled` which `run` treats as a clean exit.
  `armCancel`/`disarmCancel`/`cancelQuery` under `cancelMu` — one scope
  per extended-protocol Execute; one scope spanning a whole
  simple-protocol batch (so a cancel between statements still stops
  it). `errors.go`: `errors.Is` on context.Canceled/DeadlineExceeded →
  `cancelBody` with SQLSTATE 57014 and Postgres's exact wording
  ("canceling statement due to user request" / "due to statement
  timeout") — clients pattern-match both.
- **Test gotcha**: pgx's *default* context watcher hard-closes the
  client conn on ctx cancellation, so asserting connection survival
  that way fails. Right way: run the query with a background ctx on a
  goroutine and fire `c.PgConn().CancelRequest(...)` — real cancel
  wire message, connection provably survives with 57014. Also tested:
  wrong-secret forgery over a hand-built raw frame is ignored;
  statement_timeout over the wire returns 57014 + session survives.
  sql-level tests: pre-canceled ctx, cancel mid triple-cross-join,
  timeout aborts a write with no partial rows.
- **Docs**: `docs/gotchas.md` (cancellation works now; backup +
  corruption-refusal notes under Operational) and the `pgwire.go`
  package doc updated.

## 4. Remaining-gaps assessment (delivered, not implemented)

- **Cheap**: TRUNCATE (range deletes exist), VARCHAR(n) enforcement,
  UNIQUE-constraint sugar, honest server_version, bytdbd sync-policy
  flag + conn cap, query logging.
- **Reasonable, high-value**: auth (SCRAM) + TLS ← next per remediation
  order; TIMESTAMP/DATE as int64 micros (tuple already orders int64) +
  UUID; FKs without cascades (single writer = no concurrency hazards);
  derived tables via the existing virtual-table join machinery → CTEs →
  views; hash join (no stats needed); pg_stat_activity off the new
  backends registry; ALTER RENAME (rows keyed by column ID).
- **Middle ground**: NUMERIC(p≤18,s) as scaled int64.
- **Rewrites, stay deferred**: MVCC writers, replication/HA,
  larger-than-RAM, streaming/external sort, cost model, triggers,
  JSON/arrays.
- Suggested order: auth/TLS → timestamps → FKs → derived tables/CTEs →
  hash join.

## Files touched

- **btypedb**: `wal.go`, `db.go`, `compact.go`, `backup.go` (new),
  `header_test.go` (new), `backup_test.go` (new), `tx_test.go`,
  `go.mod`/`go.sum`.
- **bytdb**: `engine.go`, `go.work` (+ `../btypedb`), `backup_test.go`
  (new), `docs/gotchas.md`, `ai_docs/production-readiness-audit.md`
  (new); `sql/`: `sql.go`, `session.go`, `exec.go`, `join.go`,
  `ast.go`, `parser.go`, `describe.go`, `cancel_test.go` (new);
  `pgwire/`: `pgwire.go`, `conn.go`, `errors.go`, `cancel_test.go` (new).
