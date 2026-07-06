# Session: bytdb-milestone-15-aggregate-expressions

- **Session ID**: `de6dff2b-da5a-4014-bab0-883dba5af079` (same session as `2026-0706-0042-bytdb-milestone-14-group-ordinals-and-savepoints.md`, continued)
- **Date**: 2026-07-06
- **Repo**: `~/projs/go/bytdb` (btypedb untouched)
- **Result**: milestone 15 complete — GROUP BY expressions + expression items in aggregate queries + aggregate calls over expressions, verified against psql 17.5. Commit `c8728c4`, pushed. All suites green under `-race`.

## The design: rewrite against the keys

One architecture delivered three deferred items at once. `resolveAgg` treats every GROUP BY key as an expression and **rewrites** every select item, HAVING condition, and sort key against them:

1. **Keys** (`aggKey{ex, ord, typ, txt}`): from GROUP BY columns, expressions, or ordinals (ordinals may now name expression items). `txt` is a canonical rendering (`exprKey`) where **columns render by resolved combined-row ordinal** — so `city` and `users.city` match — literals render type-tagged, and aggregates/subqueries render by pointer so they never match. Plain-column keys keep `ord` for a scan-time fast path. Aggregates inside a key → 42803-style error; duplicate `txt` → error (pre-existing divergence from PG, which dedups silently).
2. **`rewrite(e)`**: whole-subtree match against a key → `exGroupRef{idx, typ}`; `*ExAgg` → `exAccRef{idx, typ}` via deduplicating `accumFor`; leftover `*ExCol` → PG's `column "x" must appear in the GROUP BY clause or be used in an aggregate function` (after resolving, so unknown columns still say "no such column"); `ExLit`/`ExSub` pass through; composites rebuild around rewritten children. The new nodes are unexported, carry their types (`exprType` can't see aggQuery), and evaluate through `exEnv.grp *group` — set per group in the group phase.
3. **Execution** (`execSelectAgg`): scan phase evaluates key exprs + accumulator args per row (env.row = vals); group phase (moved *inside* the read txn, since rewritten exprs may hit the catalog via casts/subqueries) sets `env.grp` and runs HAVING (`evalTruth`), sorts (precomputed per-group values), then outputs — all through the ordinary evaluator, so functions/CASE/arith/casts compose over grouped data for free. The old `aggRef`/`havingLeaf`/`havingRefs`/`valueOf`/`checkPred`-loop machinery is deleted; HAVING is one Expr via `havingToExpr` (Pred leaves, aggregate items included, converted with ExCmp/ExIsNull; Cond leaves pass their Ex).

## What fell out for free

- Literal items in aggregate queries (`SELECT 'x', count(*)`) — the old "literals are not supported" restriction is gone.
- Expressions over grouped columns without being keys themselves (`SELECT city || '!' ... GROUP BY city`) — rewrite descends and matches the inner column.
- `count(*) + 1` in items and HAVING; ORDER BY full expressions; ORDER BY output aliases in aggregate queries (`count(*) AS n ... ORDER BY n`).
- Zero-row ungrouped queries still emit one group; rewritten exprs evaluate over it (`count(*) + 1` → 1).

## Aggregate expression arguments

- `ExAgg` gained `Arg Expr`; parser's aggregate branch parses a full expression, keeps `ExCol` in the legacy `Col` slot, rejects nested aggregates at parse ("aggregate function calls cannot be nested"). `lowerItem`/`simpleItem` keep Arg-form aggregates as expression items so legacy paths never see them.
- `accum` gained `argEx`, evaluated per input row (`add(env, vals)`); COUNT(*) is now `ord < 0 && argEx == nil`. Column args keep the strict static "requires a numeric column" check; expression args check numericness at evaluation ("requires a numeric argument") because `exprType` is best-effort (unknown functions default to text — `sum(coalesce(...))` types as float).
- `walkExpr` now descends into `ExAgg.Arg` (findAgg, param noting).

## The bug worth remembering

`bindSelect` cloning GroupBy as `make([]GroupItem, len(...))` turned **nil into empty-non-nil**, and `isAggregate` checks `s.GroupBy != nil` — so every parameterized query (bind always clones) became an aggregate and plain `select name ... order by name` died with must-appear-in-GROUP-BY. Fix: only clone when `len > 0`, preserving nil-as-"not aggregate". The same nil-signal exists in expr.go's union/subquery guards. Existing tests caught it instantly — the parity suite is load-bearing.

Also: `isAggregate` now walks Ex items with `findAgg` (previously `select count(*) + 1 from t` wasn't even routed to the aggregate path), and params.go notes/binds GroupBy exprs and ExAgg.Arg (missing either breaks $n counting or leaves unbound params).

## Naming/typing

`resultCols` rebuilt: [AS] wins; legacy aggregate items keep "fn(col)"; expression items use `exprName` — `exprName(*ExAgg)` added returning the bare function name, so `sum(a*2)` names its column `sum`, as PG does. Types come from `exprType` over the rewritten tree (exGroupRef/exAccRef carry their types).

## Stale test updated

`select 'x', count(*) from users group by 1` was asserted as an error (ordinal → literal item); now legal (constant key, PG-compatible). Replaced with new error cases: ungrouped column inside an expression, nested aggregates, non-numeric SUM arg at eval, aggregate in GROUP BY.

## State at end of session

- Milestones 1–15 done. bytdb `c8728c4` pushed; btypedb still at `v0.4.0`/`ca31f64`.
- README Later list is down to: DESC key columns, CHECK/NOT NULL, EXPLAIN.

## Next (deferred)

- Wire: cancellation, COPY, portal suspension.
- Engine: DESC key columns, CHECK/NOT NULL, EXPLAIN.
- SQL: `= ANY(array)` for real; `count(distinct x)`; correlated subqueries referencing grouped columns in aggregate items (currently "column is not yet available here" at eval).
- Someday: tag bytdb itself so pgwire's `replace` can become a versioned require.
