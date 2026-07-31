# Session: OCC Stage 1 Prototype (concurrent writes in btypedb)

- **Session ID:** `b2bb2b25-e109-4edf-8e8e-8dfa2c515208` (continuation; same session as the design doc)
- **Date:** 2026-07-30, 23:22
- **Deliverable:** btypedb branch **`occ-stage1`**, commit `9361742` (branched from v0.6.2 / `f67e2b6`)
- **Design:** `ai_docs/plans/2026-0730-concurrent-writes-occ.md` (bytdb repo, committed earlier as `8c2b269`)

## What was built

Stage 1 of the concurrent-writes plan, implemented in `~/projs/go/btypedb`:
opt-in OCC (`WithConcurrentWrites()`, default off) that shrinks `writerMu`
from transaction-lifetime to commit-time. Six files, ~950 lines added.

### New file `occ.go`
- `ErrTxConflict` — retryable sentinel; first committer wins (snapshot
  isolation). Future pgwire mapping: SQLSTATE 40001.
- `WithConcurrentWrites()` option → `options.concurrentWrites` → `DB.occ`.
- `logicalOp[K,V]` (del/key/val/absolute-deadline) — the replayable write
  set, mirroring `tx.pending` (sealed WAL bytes can't be re-applied to
  trees, so decoded ops are kept alongside).
- `keyRange[K]` — half-open [min,max) claims from DeleteRange.
- `commitRing[K]` — fixed 1024-entry circular buffer of recent commits'
  changed keys/ranges; `since(base, current)` returns the exact span or
  !ok when evicted (= conflict, degrade to retry, never misvalidate).
- `occMaxEntryKeys = 4096` — bigger commits become wildcard entries that
  conflict with everything (bulk load aborts concurrent writers).
- `occConflicts` — overlap in any of: their-keys×our-keys,
  their-keys×our-ranges, their-ranges×our-keys, their-ranges×our-ranges
  (last one conservative: "claims are exclusive" keeps the commute
  argument local).

### Changed
- `tx.go` — `Begin(true)` skips `writerMu` in occ mode and records
  `baseVersion`; `Commit` takes `writerMu` only across validate → WAL
  append → pointer swap. Fast path (head unmoved) is byte-identical to
  the old commit. Moved head: validate via ring, then replay ops onto a
  copy of the current head (`tx.state` from the stale base is discarded,
  not merged). `holdsWriter` flag keeps lock/unlock sites paired across
  modes; Rollback releases only if held. Ops recorded in `setInternal`,
  `Delete`, `DeleteRange` (which also claims its interval — even when it
  matched zero keys, provided the tx publishes at all).
- `db.go` — `DB.occ`, `commitVersion`, `recent` ring (all under `mu`);
  direct one-shot `Set`/`Delete` push one-key ring entries so open OCC
  transactions see them as commits.
- `savepoint.go` — savepoints record `opsLen`/`wrangesLen`; RollbackTo
  truncates the OCC write set in lockstep with `pending` (a lingering
  rolled-back op would cause spurious conflicts AND replay a write the
  WAL never logged).
- `ttl.go` — sweeper treats `ErrTxConflict` as benign (user commit beat
  it to an expired key; next tick re-sweeps).

Untouched by design: WAL format, group commit, replication
(`LogState`/`ReadLogRange`/epochs), backup, compaction. Default mode
unchanged — full pre-existing suite passes with the option off.

## Semantics discovered/pinned during testing

- **Empty DeleteRange publishes nothing** (`nops == 0` early-return), so
  its interval claim never enters the ring and a concurrent insert
  survives. Correct — a no-op tx serializes as if first — but it bounds
  the TRUNCATE guarantee: pinned by `TestOCCEmptyRangeNoPublishNoClaim`.
  If bytdb's Truncate must conflict even on an empty table, Stage 2
  should write a marker key.
- The claim DOES hold for an empty range when the tx publishes anything
  else (`TestOCCEmptyRangeStillClaims`).

## Tests (occ_test.go — all green, plain + `-race`)

Write-write conflict / disjoint replay / direct-write-vs-tx conflict /
range-delete-vs-insert both orders / empty-range both semantics / ring
eviction aborts / wildcard bulk commit / savepoint shrinks write set /
default-mode lost-update check / bank-transfer invariant with retry
(286 conflicts retried, sum conserved, survives reopen+WAL replay) /
8-writer disjoint reopen / conflict-then-retry.

## Benchmarks (M3, 8 cores, SyncNever, parallel Updates)

| tx shape | serialized | occ |
|---|---|---|
| light: 20 reads + 5 writes | 17.5µs | 24.6µs (~40% slower) |
| heavy: 2000 reads + 5 writes | 499.8µs | **126.6µs (~4× faster)** |

Matches the design doc's prediction: OCC pays per-commit overhead (extra
`state.copy()`, validation, near-constant replay under sustained load)
and wins once in-transaction work dominates — the bytdb SQL layer's
profile (FK/unique checks, expression eval currently run under the
writer lock).

## State / next steps

- btypedb `occ-stage1` @ `9361742`; bytdb + pgwire build and bytdb root
  tests pass against it via go.work. main (v0.6.2) untouched.
- Next candidates: shave the light-tx overhead (replay-path
  `state.copy()` is the prime suspect — consider applying ops directly
  to a re-copied head only when nops is small vs re-validating); then
  Stage 2 (bytdb non-transactional sequences) so SQL can use the mode.
- Not started: Stages 2–4 (sequences, SERIALIZABLE read sets, pgwire
  40001 surface).
