# Performance and robustness pass across all six packages

Session: 2502ad7f-7d01-49e7-a443-b0e705d087e0
Date: 2026-08-05
Commit: 4979e63 "Performance and robustness pass across all six packages"

## What happened

One more review-and-fix sweep over bytdb, requested as "performance and
robustness take." Three parallel review agents combed the root engine,
`sql/`, and the periphery (`stdlib/`, `pgwire/`, `replicate/`,
`tuple/`); every finding was re-verified against the code before
fixing. 16 fixes landed, each with a regression test where one made
sense. All modules pass `go vet` and race-enabled test suites; the new
tuple decoder survived 8.2M fuzz execs.

## Fixes (root engine)

1. **updateDDL panic guard** (`engine.go`, `txn.go` guardPanic): DDL
   closures run caller-supplied callbacks (AddCheck's per-row
   `validate`, AlterSequence's `mutate`); a panic there escaped
   `kv.Update` with the writer lock held — every future write blocked
   forever, reads kept working (masking it). Same recover→rollback→
   re-panic pattern WriteTxn already had. Test:
   `TestDDLPanicReleasesWriterLock`.
2. **writerGID clear-after-unlock race** (`txn.go`): `Commit`/`Rollback`
   cleared the reentrancy marker *after* the lock released, clobbering a
   racing next writer's marker (silently disabling
   `checkReentrantWrite`); `writeTxn`'s panic path had the same bug via
   defer LIFO order. Marker now clears before the lock releases;
   `releaseWriter` is one-shot so `defer tx.Rollback()` after a
   successful Commit can't re-clear.
3. **OCC backward SetSeq clobbered** (`seqalloc.go`): a draw extension
   took `max(stale in-memory floor, stored)`, so a committed SetSeq that
   moved the counter *down* (restore, setval) was durably overwritten —
   an outcome no serial order produces. Extensions now treat
   `stored < anchored watermark` as an external reset (`errSeqReset` →
   re-anchor); `bumpCounterAllocTo` keeps the max (its floor is a
   semantic requirement). Test:
   `TestOCCBackwardSetDuringExtensionReanchors`.
4. **descCache never evicted**: DropTable/RenameTable leaked one parsed
   descriptor per name forever. Added `cacheEvict` post-commit.
5. **decodeRow O(columns²)**: per-row column-ID resolution was a linear
   scan per stored value. `TableDesc.ordByID` (built in `parseDesc`,
   dropped by `clone()` with linear fallback) makes it O(1).

## Fixes (sql)

6. **Uncorrelated subqueries memoized per statement** (`sql/subcache.go`,
   the headline fix): every ExSub/ANY/EXISTS re-planned and re-ran per
   outer row — `WHERE x >= (SELECT min(x) FROM a)` was perfectly
   quadratic (repro'd: 2k rows 670ms, 4k rows 2.68s). Now a static
   default-deny correlation test (every column ref must resolve in the
   subquery's own scope; nested subqueries/windows/CTEs/sequence writes
   assume the worst) gates a per-statement memo keyed by `*Select`,
   seeded on root exEnvs. Matches Postgres InitPlan semantics (also for
   VALUES: uncorrelated subqueries evaluate once per statement).
   Measured after: **4k rows 2.2ms (~1200×)**.
7. **Nested-loop join re-planned per outer row** (`sql/join.go` rowPlan,
   `sql/plan.go` rebind): template Preds now allocated once with Vals
   mutated per row, plan built on first row and value-refreshed after —
   sound because the access path depends only on which columns are bound
   and their fixed per-column types.
8. **float→int cast**: Go's out-of-range conversion is
   implementation-defined (arm64 saturates, amd64 wraps); `1e300::int`
   returned garbage. Now `math.RoundToEven` + range/NaN check →
   "bigint out of range" (22003), matching Postgres (also: rounds, not
   truncates — `3.9::bigint` = 4). `::float` accepted as double
   precision alias.
9. **nextval in CTE/derived table**: `selectWritesSequences` didn't walk
   `s.With`, so `WITH x AS (SELECT nextval('s'))` dispatched read-only
   and failed. Fixed + tests.
10. **Regex cache unbounded**: `WHERE a LIKE b` (pattern from data)
    pinned one compiled regex per distinct value forever. Bounded at
    1024 with wholesale reset on overflow.
11. **Per-row exEnv heap copies** hoisted in the SELECT projection loop
    (per-statement now), RETURNING (per-row, was per-expression), CHECK
    evaluation, and AddCheck validation.

## Fixes (periphery)

12. **stdlib: Tx.Commit of an aborted block returned nil** while every
    write was discarded (COMMIT→ROLLBACK Tag was dropped) — silent data
    loss signal, repro'd live. Now returns `ErrCommitRolledBack`
    (errors.Is-able; lib/pq `ErrInFailedTransaction` / pgx
    `ErrTxCommitRollback` parity). Test: `TestCommitOfAbortedBlockErrors`.
13. **pgwire: Bind format-code panic**: counts other than 0/1/n indexed
    `formatFor` out of range — remotely triggerable panic (repro'd on
    both the param and result paths; recover fence turned it into a
    killed connection + XX000). Now validated with Postgres's own
    wording. Raw-wire regression test
    `TestBindFormatCountMismatchIsProtocolError`.
14. **pgwire: pre-auth frames capped at 16 KiB** (was 64 MiB): startup
    and SASL messages; Postgres caps startup at 10,000 bytes. N
    unauthenticated sockets could previously buffer N×64 MiB before any
    refusal.
15. **replicate: pruning could delete the only manifested generation**:
    retention counted generations by name alone, so restarts/compactions
    during slow shipping (incomplete generations) pushed the only
    restorable one past the horizon — the exact roll-backward the
    manifest machinery prevents. Newest manifested generation is now
    never pruned. Test: `TestPruneKeepsNewestManifestedGeneration`.
16. **tuple: unescape** built every decoded string byte-at-a-time from a
    zero-cap slice (≈10 reallocations per 1 KB value, per element, per
    row). Now two-pass: locate terminator + count escapes, then one
    exact-size allocation (no-escape fast path = copy or XOR loop).
    8.2M round-trip fuzz execs clean.

## Verified clean (no action needed)

Reviewers explicitly checked and cleared: parser recursion depth
bounds, int64 arithmetic overflow (incl. MinInt64/-1), LIMIT/OFFSET
saturation, NULL/Kleene semantics for ANY/ALL/IN, session transaction
lifecycle (no leaked txns), stdlib driver contracts (Rows.Next EOF,
no []byte aliasing — tuple.Decode copies), pgwire framing/SCRAM/
deadlines/cancel keys, replicate ship-cursor ordering and restore's
fsync+rename dance, tuple ordering-preservation edge cases.

## Known remaining (documented, deliberate)

- **Correlated subqueries are still per-row** with the correlated
  predicate as a post-scan Cond filter (no index pushdown): measured
  27s for a 4k×4k correlated point-lookup pattern. Fixing needs
  outer-value binding before planScan (the machinery from fix #7 is a
  step in that direction). Documented in the skill's gotchas — rewrite
  as JOIN for large outer sets.
- The env memo is seeded only on statement-root exEnvs; any future
  executor path that builds its own root env without seeding simply
  skips subquery caching (safe direction).

## Housekeeping

- Skill `.claude/skills/bytdb-fast-memory-based-db/SKILL.md` updated:
  ErrCommitRolledBack, InitPlan note, correlated-subquery gotcha.
- `sql/vars.go` had pre-existing gofmt drift — formatted in passing.
- `pgwire/proto_shim_test.go` keeps the old `readBody(r)` single-arg
  shape for tests written against the post-auth 64 MiB limit.
