# Session: OCC Stage 2 — sequences and identity leave the conflict scope

- **Session ID:** `ba54d7e0-d229-419a-aeb1-cd8fa772aa52`
- **Date:** 2026-07-30, 23:50
- **Deliverables:**
  - bytdb branch **`occ-stage2`**, commit `1d497a6` (branched from main `7ea422e`)
  - btypedb branch **`occ-stage1`**, commit `0a09c79` (on top of Stage 1's `9361742`)
- **Design:** `ai_docs/plans/2026-0730-concurrent-writes-occ.md` (Stage 2 section)
- **Prior session:** `2026-0730-2322-occ-stage1-prototype.md`

## What was built

Stage 2 of the concurrent-writes plan: under `WithConcurrentWrites()`
(now re-exported by bytdb), sequence and identity allocation becomes
**non-transactional and gap-tolerant** — the Postgres position — so
concurrent inserts into identity tables stop conflicting on their
counter keys. Without this, OCC buys nothing for the most common write
shape.

### btypedb (`0a09c79`)
- `DB.ConcurrentWrites()` and `Tx.Writable()` accessors — the two
  read-only facts bytdb needs to route allocation around transactions
  while still enforcing the read-only guard.

### bytdb: new `seqalloc.go`
- `counterAlloc` — per-counter in-memory allocator (own mutex, so one
  hot table never blocks another) for identity columns and named
  sequences. Durable **high-watermark** stored at the *same* 8-byte
  key as before (batch `seqAllocBatch = 32`): the watermark is written
  BEFORE any value below it is handed out, so crash recovery resumes
  past everything possibly in use. Gaps, never reuse.
- `seqObjAlloc` — same idea for CREATE SEQUENCE objects, honoring the
  declared `CACHE` (default 1, exactly Postgres). Each reservation
  re-reads the stored descriptor inside the write txn, so ALTER picks
  up options next batch and DROP surfaces "does not exist".
- Watermark writes are **short existence-checked kv transactions**
  (`seqPut`, bounded `ErrTxConflict` retry ×10) — that is what removes
  counter keys from user write sets. `a.existed && !stored` →
  `errSeqReset` → allocator marked dead, dropped by pointer-compare
  (`dropCounterAlloc`), caller re-anchors — a racing allocator can
  never resurrect a counter that a concurrent DROP/TRUNCATE deleted.
- Invalidation helpers (`invalidateCounter`/`Prefix`,
  `invalidateSeqObj`): always safe (re-anchor just re-reads stored
  state); called AFTER the durable change lands.

### bytdb: routing (mode split on `Engine.occ`)
- `seq.go`: `NextSeq`/`SetSeq`/`PeekSeq`/`DeleteSeq` (Engine + Txn
  forms) route to allocators under OCC. Txn forms guard with
  `Tx.Writable()`; draws burn on rollback; `SetSeq`/`DeleteSeq` via
  Txn take effect immediately (documented, Postgres setval semantics).
  `PeekSeq` reports the live allocator (stored value is a watermark,
  ahead by the unhanded cache).
- `seqobj.go`: `stepSeq` extracted as the single step function shared
  by transactional `NextVal` and the allocator (no drift on
  bound/cycle behavior). `NextVal`/`SetVal` route under OCC.
  Create/Alter/DropSequence invalidate after success.
- `identity.go`/`dml.go`: `fillIdentity` and `insertRow` now take
  `*Engine`; NULL draws → `allocCounter`, explicit values →
  `bumpCounterAllocTo` (no-write fast path when already covered).
- `txn.go`: `Txn.dirtySeqPrefixes` — Truncate RESTART IDENTITY records
  its counter prefix; flushed to `invalidateCounterPrefix` only on
  successful commit (WriteTxn captures the Txn; `Txn.Commit` flushes).
  Doc comments updated for both isolation modes.
- `ddl.go`: DropTable invalidates the table's counter prefix,
  DropColumn the specific counter key, both post-commit. `updateDDL`
  retries `ErrTxConflict` ×10 (closures are pure functions of their
  snapshot — table-ID counter stays transactional, so two racing
  CreateTables conflict and retry).

Serialized (default) mode is byte-identical — full pre-existing suite
green, allocator code unreachable.

## Semantics pinned / documented races

- Draws, bumps, setval, and Txn-level SetSeq/DeleteSeq are
  non-transactional under OCC — burned on rollback, immediate effect.
- Truncate-RESTART vs in-flight cached draw: a draw between the
  truncate's publish and the invalidation flush keeps its high value
  (gap, not collision — PK uniqueness still enforced). Nanosecond
  window, documented in `Txn.Truncate`.
- Concurrent SetSeq-down vs active draws is the caller's contract
  (same wording as the serialized doc).
- Empty-table truncate claim semantics from Stage 1 unchanged.

## Tests (seq_occ_test.go — 15, green plain + `-race`)

Concurrent identity inserts (8×50, zero conflicts asserted, all IDs
distinct) / watermark reopen (gap ≤ 1 batch, no reuse) / rollback
burns / SetSeq immediate / PeekSeq exact / DeleteSeq restarts at 1 /
explicit bump survives rollback / concurrent NextVal CACHE 5 distinct /
bounds clipped mid-batch + cycle wrap / setval immediate / ALTER drops
cache / truncate restart (commit vs rollback) / drop-table fresh
identity / read-only guard / truncate-vs-insert race (uniqueness under
any interleaving).

## Benchmarks (M3, 8 writers, SyncNever, in seq_occ_test.go)

| shape | serialized | occ |
|---|---|---|
| single identity insert | 11.8µs | 11.3µs (~tie; commit-dominated, zero counter conflicts) |
| 400 reads + identity insert | 150.4µs | **52.7µs (~2.9× faster)** |

Matches Stage 1's kv-layer finding, now confirmed end-to-end at the
SQL-engine layer.

## State / next steps

- bytdb `occ-stage2` @ `1d497a6` (requires btypedb `occ-stage1` @
  `0a09c79` via go.work); bytdb main untouched.
- Not started: Stage 3 (opt-in SERIALIZABLE via bytdb-layer read sets;
  DDL `schemaVersion`), Stage 4 (pgwire 40001 mapping, bounded
  auto-commit retry, mkdocs Concurrency page).
- Open question for Stage 4: Engine-level one-shot writes (Insert,
  Update, Delete) currently surface `ErrTxConflict` raw — auto-retry
  belongs with the pgwire auto-commit work.
