# Session: bytdb-milestone-20-order-by-exploiting-index-order

- **Session ID**: `da764c69-e786-46c6-b865-f317181bd074`
- **Date**: 2026-07-06
- **Repo**: `~/projs/go/bytdb` (btypedb untouched, still `v0.4.0`)
- **Result**: milestone 20 complete — ORDER BY exploiting index order. Single-table SELECTs whose sort matches a scan's key order skip the sort (and, under a LIMIT, stop early); a fully-reversed match runs the scan **backward** via new engine reverse scans. Green under `-race`, `go vet` clean, both modules build/test, verified end-to-end with psql 17 driving bytdbd.

## The idea

Three wins, all for a **single-table, non-aggregate, non-window** SELECT:

1. **Skip the sort** when the chosen scan already yields rows in ORDER BY order.
2. **Early termination** — with the sort gone, collection stops at OFFSET+LIMIT (the existing `len(keys)>0 || ...` guard in `execSelect`).
3. **Reverse scan** — when the ORDER BY is the exact reverse of the path's order, walk the path backward.

Every access path yields a well-defined logical order: for each key column in order, ascending, or **descending for a DESC index column** (their keys are stored byte-inverted, so a forward `Ascend` already produces descending values — milestone 16). The primary key is always ascending.

## Key correctness constraints

- **NULLs** are the catch. The key encoding sorts NULLs *before* every value ascending (plan.go's long-standing comment), but `orderCmp` (the sort's comparator) puts NULLs *last* ascending / *first* descending — Postgres's `NULLS LAST`/`FIRST` default. So index order and sort order disagree on NULL placement. Fix: **every ordering column must be NOT NULL** (a PK column always is; else `desc.Columns[ord].NotNull`). Nullable columns keep the sort. Verified over psql that a nullable-column index is declined and NULLs still land last/first.
- **Reverse only when unbounded** (`p.from == nil && len(p.stops) == 0`). A pushed lower bound / early-stop describes a forward-only region; reversing it correctly (start at the high edge, stop at the low) is deferred. So `WHERE id>2 ORDER BY id DESC` still sorts. Forward with bounds is fine (`WHERE id>2 ORDER BY id` elides).
- **Equality-pinned prefix carries no order** and is skipped in both the keys and the path columns, so `WHERE a=1 ORDER BY b` reads a `(a,b)` index directly (the eq columns come from `p.stops` with `kind==stopNE`).

## Implementation

### Engine — reverse scans (root package)
- `kvView` gains `Descend(from string)` (both `*btypedb.DB` and `*btypedb.Tx` already have it).
- `scanRowsRev` (dml.go) / `scanIndexRowsRev` (index.go): mirror the forward scans over btypedb's `Descend(end)`. `Descend(from)` starts at the last key `<= from`; pass the **exclusive** upper `end`, skip the first yields where `k >= end`, stop once `k < start`. Yields the same `[start, end)` region, descending. Supports bounds generally (SQL only calls them unbounded, but engine tests exercise bounded).
- `Engine.ScanRangeRev`/`ScanIndexRev` + `Txn.ScanRangeRev`/`ScanIndexRev` (the usual Engine+Txn pairing). Index reverse uses a snapshot (`Begin(false)`) so entry→row lookups don't re-enter a read lock, same as forward.

### Planner (sql/plan.go)
- `plan.reverse bool` — read the path backward; **only ever set for an unbounded scan**.
- `(*plan).orderSatisfied(desc, keys) (ok, reverse bool)`: path key columns + per-column ascending direction (PK all-asc; index `!idx.DescAt(i)`); eq-prefix from `stopNE` stops. `try(rev)` walks the sort keys, skipping eq-pinned columns, matching the rest one-for-one against the remaining path columns, each demanding the same direction (`k.desc == colAsc → mismatch`), and requiring `nonNull(ord)`. Forward tried first; reverse only if unbounded.
- `chooseOrderedPlan(desc, alias, where, keys, limited)`: starts from the WHERE-driven `planScan`; if that path satisfies the order → use it (set `reverse`). Else, **only** when the base plan pushed nothing (plain seq scan) **and** there's a LIMIT, search secondary indexes for one whose order serves the sort (trading full-scan+sort for a bounded ordered index walk; reuses `p.filter`/`p.binds` since the index visits every row once — every row has exactly one entry per index).

### Executor (sql/exec.go, sql/join.go)
- `orderedScan(fp, s, keys)`: eligibility gate — single real table (`len(fp.steps)==1`, `st.rows==nil`, `len(tmpls)==0`), every key a base column (`k.ord < width`; ORDER BY expressions have `ord >= width` → declined). Calls `chooseOrderedPlan` with `combineStatic(step.static)` (the same WHERE `rec` would plan) and `s.Limit >= 0`.
- `execSelect`: after `buildSortKeys`, if `orderedScan` says eliminate → `fp.steps[0].plan = p; keys = nil`. Nil keys make collection early-stop and `sortOffsetProject` skip the sort.
- `joinStep.plan *plan` (precomputed path); `rec` uses it when set, else plans per outer row as before.
- `scanPlan`: a 4-way switch selects `ScanRangeRev`/`ScanIndexRev` when `p.reverse`, else the existing forward `ScanRange`/`ScanIndex`. Reverse passes nil bounds; `stopped(p.stops,...)` is a no-op (stops empty).

### EXPLAIN (sql/explain.go)
- `explainer` gains `step0Plan *plan` + `orderElim bool`, reset per SELECT core. `coreNode` reordered so the order decision (via `orderedScan`) runs **before** `fromNode`; `scanNode` renders `step0Plan` instead of re-planning.
- Title: `Index Scan[ Backward] using <idx|tbl_pkey> on ...`; a forward order-eliminated primary scan shows `Index Scan using <tbl>_pkey` (not `Seq Scan`) so the plan reads as PG-faithful. `selectNode` drops the `Sort` node when `orderElim`.

## Gotchas / decisions

- **Tie order changes under reverse.** A reverse index/primary scan reverses the PK suffix too, so `ORDER BY a DESC` (no tiebreaker) returns ties in pk-**descending** order vs the old stable sort's pk-ascending. Both are valid SQL (unspecified tie order). Ascending forward is unchanged (pk-asc ties). Tests use tie-free data to stay deterministic.
- **`orderSatisfied` catches `ORDER BY a DESC, id`**: index `(a)` implicitly orders `(a, pk)`, but the second key `id` runs past the single path column → not satisfied → sorts. So we never claim reverse for a query that also orders by pk (which reverse would flip).
- **Redundant-index tie**: `planScan` breaks score ties by creation order, so with both `(a)` and `(a,b)` indexes present, `WHERE a=1 ORDER BY b` picks `(a)` and still sorts. With only `(a,b)` (the realistic schema) it works. Order-aware path selection among redundant indexes is deferred.
- First wrote `combineStatic` to mirror `rec`'s static/template combination exactly so `chooseOrderedPlan` plans the identical WHERE.

## Verification

- **Unit**: `sql/order_test.go` — primary asc/desc (+range, +offset), index walk (asc/desc, no-LIMIT keeps sort), composite eq-prefix, DESC index (forward for DESC, backward for ASC), nullable-declined (NULLS last/first), expr-declined, join-declined. `reverse_test.go` — `ScanRangeRev`/`ScanIndexRev` full + bounded + "reverse == forward reversed" + in-txn with uncommitted writes.
- **psql 17** over the wire (`bytdbd` on `127.0.0.1:5455`): `Index Scan Backward using t_pkey` for `ORDER BY id DESC LIMIT`; `by_a` forward/backward for `ORDER BY a [DESC] LIMIT`; `by_cat_pri` with `Index Cond: (cat = 2)` and no Sort; nullable `v` → Sort+Seq Scan with correct NULLS LAST/FIRST.
- `go vet ./...` clean, `go test -race ./...` green (bytdb, sql, tuple, pgwire).

## State at end of session

- Milestones 1–20 done. btypedb still `v0.4.0`. README bumped to 1–20 with a milestone-20 entry.
- New "Later" list: bounded reverse scans (eq-prefix + DESC), order-aware path selection among redundant indexes, window frames (`ROWS/RANGE BETWEEN`), `LAG`/`LEAD`/`FIRST_VALUE`.

## Next

- **Bounded reverse**: reverse within an equality prefix / range region (e.g. `WHERE status='x' ORDER BY created DESC LIMIT 20` on a `(status, created)` index) — start at the region's high edge, stop at the low.
- **Order-aware path selection**: when WHERE ties on score, prefer the path that also serves ORDER BY.
- Window follow-up: frames, `LAG`/`LEAD`/`FIRST_VALUE`.
- Wire: cancellation, COPY, portal suspension.
- Someday: tag btypedb so pgwire's `replace` becomes a versioned `require`.
