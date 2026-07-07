# Session: bytdb-milestone-21-bounded-reverse-scans

- **Session ID**: `4e8277bd-97b6-49db-a20f-964848615a1a`
- **Date**: 2026-07-07
- **Repo**: `~/projs/go/bytdb` (btypedb untouched, still `v0.4.0`)
- **Result**: milestone 21 complete — bounded reverse scans, committed as `8381f7c`. Any pushed-down scan region can now be read backward, so `WHERE cat = 2 ORDER BY pri DESC LIMIT n` on a `(cat, pri)` index walks just the tail of the region backward: no sort, early termination. Green under `-race`, `go vet` clean, both modules, verified end-to-end with psql 16 driving bytdbd.

## The idea

Milestone 20 allowed `plan.reverse` only for an *unbounded* scan; a pushed lower bound or early-stop kept the sort. The lift: a forward plan's region is **already key-delimited on both edges** — `from` is the low edge, and the `stops` list describes the high edge — so reversing just converts the stops into the backward walk's entry key bound. No per-row stop checks run in reverse (they are forward-directional); both edges become pure engine key bounds.

## Implementation

### Engine — inclusive upper bounds (root package)
- `ScanRangeRev`/`ScanIndexRev` (Engine **and** Txn) gained a `toIncl bool` parameter: when set, the upper bound closes over the **whole `to`-prefix group** — `end = tuple.PrefixEnd(boundKey/indexBound(to))`. An exclusive partial-prefix bound cannot express "all of the `a = 1` group", which is exactly the upper edge of an equality-pinned region. `scanRowsRev` (dml.go) / `scanIndexRowsRev` (index.go) apply it after encoding the bound; nil `to` ignores the flag.

### Planner (sql/plan.go)
- `orderSatisfied`: dropped the `p.from == nil && len(p.stops) == 0` gate — `try(true)` now runs for any non-get plan. Comment rewritten: bounds are no obstacle; the reverse walk enters at the high edge (revEnd) and stops at `from`.
- New `(*plan).revEnd() (to []any, toIncl bool)`: walks `p.stops` in path-column order. Each `stopNE` contributes its pinned equality value; the range stop (at most one, last) contributes its value with the bound **open** for `stopGE`/`stopLE` (forward stopped AT the value → exclude its group) or **closed** for `stopGT`/`stopLT` (stopped only PAST it → include). No range stop → closed over the whole eq region. DESC-column values byte-invert inside `indexBound`'s `appendDirected`, same as when pushed forward — `revEnd` itself is direction-agnostic.
- `plan.reverse` comment updated; `chooseOrderedPlan` doc updated. The secondary-index override branch (full-scan + LIMIT only) is unchanged.

### Executor (sql/exec.go)
- `scanPlan` reverse cases now pass real bounds: `tx.ScanRangeRev(table, p.from, to, incl)` / `tx.ScanIndexRev(table, p.index, p.from, to, incl)` with `to, incl := p.revEnd()`. `from` — the forward entry — becomes where the engine stops descending.
- The per-row region check became `if !p.reverse && stopped(p.stops, row)` — stop checks are skipped in reverse.

### EXPLAIN
- No changes needed: `step0Plan` already renders the order-aware plan, `p.pushed` already carries the conjuncts, so a bounded backward scan shows `Index Scan Backward using ... ` + `Index Cond: (...)` for free.

## Correctness notes

- **Same region, read backward.** Forward `from` is inclusive (residual filter discards `=` rows for `>`/`<` pushes); reverse uses it as the inclusive stop edge (`k < start → return`). The stops never trigger *within* the forward region by construction, so converting them to the entry bound yields the identical row set.
- **NULLs need no new handling.** Ordering columns must still be NOT NULL (`orderSatisfied`'s `nonNull`), so the NULL groups that stops treat specially can't appear among ordered rows; regions that do contain NULL tails (e.g. DESC column with only an upper push) visit them first in reverse and the residual filter discards them, as forward did at the end.
- **Kind → inclusivity table**: `stopGE`/`stopLE` → open bound (exclude the stop value's group); `stopGT`/`stopLT` → closed (`toIncl`, include it); only-`stopNE` → closed over the eq region.

## Verification

- **Engine**: `reverse_test.go` updated for the new signatures + new cases: `ScanRangeRev` with `toIncl` (pk `<= 4` group included), bounded `ScanIndexRev` open vs closed (`30 <= age < 45` vs `<= 45`).
- **SQL** (`sql/order_test.go`): the two milestone-20 "deferred: still sorts" cases flipped to backward plans — `WHERE id > 2 ORDER BY id DESC` (pk) and `WHERE a = 2 ORDER BY b DESC` (composite). New: upper-bound reverse (`id < 4` / `id <= 4`), both-edges (`id > 1 AND id < 5`), DESC-index range read backward (`a > 10` / `a >= 10 ORDER BY a` on `(a desc)`), and `TestOrderBoundedReverse` — the motivating `(cat, pri)` shape with LIMIT and `pri >= 20 AND pri <= 40` both-edges ranges.
- **psql 16** over the wire (bytdbd on `127.0.0.1:5455`): `Index Scan Backward using by_cat_pri` with `Index Cond: (cat = 2)` under Limit; `Index Cond: ((cat = 2) AND (pri >= 20) AND (pri <= 40))` with no Sort; `ev_pkey` backward for `id > 2 ORDER BY id DESC`; `by_a_desc` backward with `Index Cond: (a > 10)` for ascending order; nullable column still Sort + NULLS FIRST.
- `go vet ./...` clean, `go test -race ./...` green in both modules (bytdb, sql, tuple, pgwire).

## State at end of session

- Milestones 1–21 done; `main` at `8381f7c` (not yet pushed this session). btypedb still `v0.4.0`. README bumped to 1–21 with a milestone-21 entry; "Later" list now: order-aware path selection among redundant indexes, window frames (`ROWS/RANGE BETWEEN`), `LAG`/`LEAD`/`FIRST_VALUE`.

## Next

- **Order-aware path selection**: when WHERE ties on score (redundant indexes like `(a)` and `(a,b)`), prefer the path that also serves ORDER BY.
- Window follow-up: frames (`ROWS/RANGE BETWEEN`), `LAG`/`LEAD`/`FIRST_VALUE`.
- Wire: cancellation, COPY, portal suspension.
- Someday: tag btypedb so pgwire's `replace` becomes a versioned `require`.
