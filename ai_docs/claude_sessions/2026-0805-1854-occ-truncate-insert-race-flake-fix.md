# Flaky TestOCCTruncateInsertRace: stale identity draw surfaced as duplicate-PK error

Session: ac6ad264-2243-4f0f-8cdd-3c30eded091c
Date: 2026-08-05

## What happened

Investigated the flaky `TestOCCTruncateInsertRace` (seq_occ_test.go).
It passed 30 straight runs with `-race`, but a stress harness (compiled
test binary, 200 iterations x count=5, `GOMAXPROCS` cycled 1..8 per
iteration) reproduced it by iteration 15:

    --- FAIL: TestOCCTruncateInsertRace (0.02s)
        seq_occ_test.go:582: duplicate primary key

Line 582 is the errs-channel drain, so a writer goroutine received a
hard `duplicate primary key` error — not the retryable
`btypedb.ErrTxConflict` the test (and the OCC contract) treats as the
expected loss mode. The flake was a real engine bug in error
classification, not a bad test.

## Root cause

In concurrent-writes (OCC) mode, `Txn.Truncate(..., restartIdentity)`
deletes the rows and the identity counter key transactionally, then
flushes the in-memory allocator invalidations only AFTER the kv commit
(`flushSeqInvalidations`, txn.go — necessarily post-commit, since an
aborted truncate must not disturb the allocators). That leaves a
window:

1. Truncate's commit lands (rows + counter key gone, visible to new
   snapshots); invalidation flush has not run yet.
2. Writer A opens a snapshot AFTER that commit (sees an empty table)
   but draws its id from the STALE allocator's in-memory cache — say 7
   from the old [1,33) reservation. Draws never touch the counter key
   (that's the whole point of seqalloc.go), so OCC validation cannot
   see the staleness. Probe finds nothing; commit does not conflict
   with the truncate (snapshot postdates it). Row 7 lands.
3. Invalidation flushes; the next writer's allocator re-anchors at 1
   (key deleted) and draws 1, 2, ... up to 7 — whose uniqueness probe
   finds A's row and errored with `duplicate primary key`.

Uniqueness itself was never violated — the probe (or, when neither has
committed, kv write-write validation) always stops the second row. The
bug was purely that an innocent writer got a data-corruption-looking
error for a value it never chose. The window cannot be closed:
invalidating before commit lets a concurrent re-anchor read pre-commit
state and cache it right back, and there is no atomic
commit+invalidate hook into the kv store. So the fix classifies the
collision correctly instead.

## Changes

- `identity.go` — `fillIdentity` now returns the ordinals of the
  columns it auto-drew (`drawn []int`) alongside the resolved values.
  New helper `drawnRaced(e, cols, drawn)`: true when OCC mode and any
  colliding key column was auto-drawn this insert — with a long doc
  comment enumerating why such a collision can only be a race
  (truncate-restart window above, or a concurrent explicit-value
  insert colliding with a draw just before its counter bump lands),
  never caller error.
- `dml.go` — `insertRow`'s duplicate-PK probe and unique-index probe
  both return `serr.Wrap(btypedb.ErrTxConflict, "reason", "identity
  draw raced a concurrent counter reset", ...)` when `drawnRaced`
  says so; the plain `duplicate primary key` / `unique index
  violation` errors are unchanged otherwise. serr implements
  `Unwrap`, so `errors.Is(err, btypedb.ErrTxConflict)` matches
  through the wrap (stdlib.go:483 already relies on this).
- Serialized (non-OCC) mode is deliberately untouched: draws there
  are transactional and single-writer, so a drawn-value collision can
  only follow a deliberate backward `SetSeq` — Postgres reports
  duplicate key there too, and `ErrTxConflict` is documented as never
  returned in that mode (engine.go).
- `txn.go` — rewrote the Truncate comment that claimed "the primary
  key check still guards against collisions either way" to describe
  the actual loss mode and point at `drawnRaced`.
- `seq_occ_test.go` — test header comment now names both loss modes
  (range-claim conflict, re-issued draw) as `ErrTxConflict`.

## Verification

- Full suite `go test -race ./...` green (bytdb 30s, sql 17s, all
  packages).
- Stress harness re-run against the fixed binary: 400 iterations x
  count=5 (2,000 runs) with `GOMAXPROCS` cycled 1..8 — zero failures.
  Pre-fix the same harness failed by iteration 15 (~75 iterations x5
  runs).

## Notes / possible follow-ups

- Downstream mappings hold: pgwire maps `ErrTxConflict` → SQLSTATE
  40001 (pgwire/errors.go), `stdlib.IsRetryable` matches it, and
  SQL-layer ON CONFLICT probes keys directly (sql/upsert.go) rather
  than string-matching the duplicate error, so upsert semantics are
  unaffected.
- The same reclassification covers unique secondary indexes over
  identity columns, though no test exercises that shape yet — a
  targeted test could be added if it ever matters.
- Gopls suggests the test file's `wg.Add(1)`/`go func` pairs could
  use Go 1.25's `WaitGroup.Go` (pre-existing style hints, left
  alone).
