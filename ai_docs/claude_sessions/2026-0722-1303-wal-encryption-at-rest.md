# bytdb WAL encryption at rest (value-only, AES-256-GCM)

- **Session ID:** `686d0694-6ff9-4a07-88ee-af201ca7e138`
- **Date:** 2026-07-22
- **Scope:** Add row-level encryption to the write-ahead log — ciphertext on
  disk, plaintext in memory — spanning btypedb (core), bytdb (relational
  layer), and bytdbd (daemon). Planned then implemented; all tests green.
- **Plan file:** `~/.claude/plans/linked-growing-taco.md`

## Goal

User: *"What would it take to add row-level encryption on the WAL — can be
unencrypted in memory."* After exploration + planning, two decisions were put
to the user via AskUserQuestion:

- **Scope → value-only.** Each record's VALUE (non-PK column data) is sealed;
  the tuple-encoded KEY/PK stays cleartext on disk. Fine for surrogate-ID PKs;
  PK column values are NOT protected.
- **Key → raw 32-byte master from the caller.** No passphrase/KDF, no envelope
  wrapping in v1.

## Why "plaintext in memory" is automatic

btypedb's `state.set` stores the **decoded** value (`db.go:246,256,373`;
`tx.go:221`) and the in-memory B-tree orders by the **decoded key**, not on-disk
bytes (`codec.go:12-13`). So encrypting the on-disk value touches no
index/tuple/ordering/query code — queries pay zero crypto cost. Threat model:
protects data at rest (stolen disk / backup / S3 object); does NOT protect a
running process's memory, and does NOT hide PK column values (value-only).

## Design

- **AES-256-GCM, stdlib only** — `crypto/aes`+`cipher`+`hkdf`+`rand`+`sha256`.
  No new module deps (btypedb go.mod still just serr + btype).
- **Two subkeys via HKDF-SHA256(master)**: `kRec` (record sealing) + `kKCV`
  (header key-check value). Separate key spaces so the KCV's *fixed* all-zero
  nonce can never collide-with-reuse against the random per-record nonces (a
  reused (key,nonce) is catastrophic for GCM). Caller's raw master is never used
  directly as a cipher key.
- **Per-record seal:** value → `nonce(12) ‖ GCM_Seal(val, AAD = op‖key)`, stored
  inline. `vlen` grows by 28 B (12 nonce + 16 tag). AAD binds op+key so a
  relocated/tampered record fails to open.
- **CRC stays outside the ciphertext.** The `op|klen|vlen | key | val | crc32`
  skeleton is unchanged; CRC now covers ciphertext, so torn-tail detection,
  `scanForRecord`, and crash recovery all run BEFORE any key is needed, and CRC
  still catches at-rest bit-rot. The AEAD tag is the cryptographic integrity
  layer on top.

## On-disk header — v1 stays 16 B, new v2 = 44 B

```
v2:  magic(8) | version(4)=2 | crc(4) over [0:12] | flags(4) | kcv(24)
     \________ compatibility prefix (identical to v1) ________/
```

Key move: the leading 16 bytes are **byte-identical** to the v1
`magic|version|crc` prefix. That let `prepareHeader` validate the CRC **first**,
then trust the version — a junk/torn header (garbage version field) is detected
as torn → start-over, NOT mis-reported as `ErrNewerFormat`. It also means an
OLD reader (whose `logFormatVersion` is still 1) rejects a v2 file cleanly for
free. `flags` = encrypted bit | cipher-id nibble | scope bit. `kcv` =
`GCM(kKCV, zero-nonce, "btydbKCV", AAD=hdr[0:20])`. Header is
**deterministic-in-key**, so torn-header detection (prefix of a known image)
survives and a wrong key is caught by a byte-compare of the KCV region →
`ErrWrongKey`, before any record is read.

`logFormatVersion` bumped 1→2, split into `plainFormatVersion=1` /
`encFormatVersion=2` (the old constant played both "written into header" and
"max supported" roles, which now differ).

`prepareHeader` fast-fail: v1+key → `ErrNotEncrypted`; v2+no-key →
`ErrKeyRequired`; wrong key → `ErrWrongKey`; cipher/scope flags mismatch →
`ErrCipherMismatch`; version>2 → `ErrNewerFormat`.

## Edge cases (deliberate)

- **Deletes stay plaintext** (`vlen=0`, byte-identical to today) — no column
  data to protect, key already cleartext. `tx.go:242`/`:281` left as
  `appendRecord`.
- **opBatch header stays plaintext** — `replayLog` reads the u64 count before it
  could decrypt anything; batch atomicity untouched, members individually
  sealed.
- **TTL deadline sealed for free** — the 8-byte `prependDeadline` prefix is
  already part of the `valArg` handed to framing, so it lands inside the
  ciphertext with no special case; the `len < ttlPrefixSize` guard runs
  post-decrypt.
- **Compaction re-seals** from in-memory plaintext with fresh nonces + a fresh
  v2 header (this is also the future re-encrypt/rotation seam); walSize math and
  header write use `headerFor(db.cipher)` (16 or 44). The raw tail-copy in
  `Compact` is valid for same-key compaction (tail is already sealed under the
  same kRec).

## Files changed

**btypedb** (all local-only, uncommitted at session start):
- `encrypt.go` (NEW): `walCipher{rec,kcv AEAD}`, `newWalCipher` (HKDF),
  `sealValue`/`appendSealedRecord`/`openRecord` (nil cipher or opDelete =
  passthrough), `header()`/`headerFor()`, flags/offset consts.
- `wal.go`: version constants split + `logHeaderSizeV2=44`; `logHeader()` writes
  plainFormatVersion; `prepareHeader(f, wc)` rewritten (CRC-first, format
  reconciliation).
- `db.go`: 4 error sentinels; `options.encKey`; `WithEncryptionKey([]byte)`
  (copies key); `DB.cipher`; Open builds cipher → prepareHeader + replay seam
  (`openRecord` before decode); `appendToLog` seals.
- `tx.go`: `setInternal` routes through `appendSealedRecord`.
- `compact.go`: header via `headerFor`, walSize via `len(headerFor)`,
  `writeSnapshot` re-seals.

**bytdb**:
- `engine.go`: `WithEncryptionKey` re-export (mirrors `WithSyncNever`).
- `pgwire/cmd/bytdbd/main.go`: `-encryption-key-file` / `-encryption-key-env`
  flags (raw-32 | 64-hex | base64; never on argv → no ps/history leak);
  `loadEncryptionKey`/`decodeEncryptionKey` helpers.

## Replication / backup — zero code change

`replication.go` / `backup.go` / `replicate/*` `io.Copy` raw log bytes, so
replica `.wlog` chunks, `ReadLogRange` output, and `BackupTo` files are
ciphertext (header included) automatically. Follower/Restore/backup need the
same master key to Open; lost key = permanent data loss. Enabling encryption on
an existing DB is a compaction (bumps `fileEpoch`) → fresh generation from
offset 0. Migration plaintext↔encrypted is NOT in-place (Open-with-key refuses a
v1 file) — offline copy: `Open(old)` → iterate `All()` → `Set` into
`Open(new, WithEncryptionKey)`.

## Tests (all green across all 3 modules)

- `btypedb/encrypt_test.go`: round-trip (+ on-disk confidentiality: plaintext
  value absent, cleartext key present), wrong-key, key-required, key-on-plaintext,
  bad-key-length, header-determinism, TTL+batch+delete, tamper-detection
  (flip ciphertext byte + fix CRC → hard error), torn-tail repair, compaction
  re-seal (fresh nonces, no ciphertext overlap), nonce-distinctness, + fuzz
  `FuzzOpenRecord` / `FuzzOpenEncryptedFile` (~8s each, no crashes).
- `bytdb/engine_encrypt_test.go`: engine round-trip, wrong-key/key-required,
  encrypted Backup round-trip.
- `bytdb/replicate/encrypt_replication_test.go`: ship→store is ciphertext →
  Restore → Open needs key (no-key → ErrKeyRequired).
- `bytdb/pgwire/encrypt_e2e_test.go`: real pgx round-trip, server restart on the
  encrypted file, plaintext absent on disk, no-key open fails.

All existing tests still pass — the plaintext path is byte-for-byte unchanged
(v1 header untouched; the tricky part was keeping `TestTornHeaderStartsOver` /
`TestPowerLossEveryPrefix` / `TestNewerFormatVersionRefused` green, which drove
the compatibility-prefix + CRC-first design).

## Deferred by design (documented in plan)

- **Key rotation** — compact-and-re-seal under a new key; but `Compact`'s raw
  tail-copy is invalid across keys, so rotation must be a full pause-rewrite
  until an envelope / wrapped-DEK **v3** header (reserve flags bits) is added.
- Plaintext↔encrypted **migration helper** (offline copy only for now).
- **key+value scope** (full-row incl. PK) and **ChaCha20-Poly1305** alt cipher.

## Status / next

Implemented + tested, **not yet released**. Per the established flow, shipping
wants a **btypedb version bump + repin** in bytdb root + pgwire go.mod (like the
v0.6.x chain). Not done this session unless requested.
