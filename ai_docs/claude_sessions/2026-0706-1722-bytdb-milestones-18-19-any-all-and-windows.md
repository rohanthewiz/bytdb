# Session: bytdb-milestones-18-19-any-all-and-window-functions

- **Session ID**: `385748f4-cfa2-4748-a88e-e00cc4d5d609`
- **Date**: 2026-07-06
- **Repo**: `~/projs/go/bytdb` (btypedb untouched, still `v0.4.0`)
- **Result**: milestones 18 **and** 19 complete, **pushed** (`main` now at `02e34d0`). M18 = `op ANY(...)` / `op ALL(...)` over arrays and subqueries (`06319ee`). M19 = window functions (`02e34d0`). Both modules green under `-race`, `go vet` clean, verified end-to-end with psql 17 driving bytdbd.

## op ANY / op ALL (milestone 18)

Replaced the old `ExAny` stub (which errored "ANY is not supported") with a real implementation for **any** comparison operator, plus ALL.

- **RHS forms** (`anyElements` in `expr.go`): `ARRAY[...]` constructor (new `ExArray` node), a single-column subquery (`= ANY(SELECT ...)` via `collectSubColumn`, modeled on `evalArraySub`), or a value that reads as an array — a `[]any` or a Postgres `'{...}'` array-literal string (`parseArrayLiteral`, elements stay text). **Array-typed casts are peeled**: `anyElements` sees `*ExCast` with a `[]`-suffixed type and recurses on the operand, so `'{1,2}'::int[]` and `ARRAY[]::int[]` work (plain `castVal` still rejects `[]` casts — the peel happens before eval).
- **Semantics** (`evalAnyAll`): ANY climbs from false→true on first `triTrue`; ALL falls from true→false on first `triFalse`; a `triUnknown` holds the result unknown unless a decisive element overrides. **Empty array returns `n.All`** (ANY→false, ALL→true) even against a NULL left side — matching PG. So `= ANY` generalizes `IN`, `<> ALL` generalizes `NOT IN`. Elements coerce to the left operand's type via the existing `checkPred` string→type fallback.
- `ExArray` also evaluates as a **value** → renders `{1,2,3}` (reuses `valText`), so `SELECT ARRAY[1,2,3]` works like `evalArraySub`'s output.

### Parser
- `exprPrimary`: `array` ident followed by `[` → `ExArray` (empty `array[]` allowed). Sits before the generic ident/colRef path so `ARRAY[...]` no longer mis-parses as `ExIndex` into a column named "array". Lexer lowercases idents, so `t.text == "array"`.
- ANY branch (~`parser.go:1441`): now matches `any` **or** `all`; after `(`, a bare `select` → `ExSub`, else `p.expression()`. Sets `ExAny.All`.

### The 7-site expression-node checklist (same as prior milestones)
Adding `ExArray` + the `ExAny.All` field touched: `ast.go` (type, `expr()`, `walkExpr`), `parser.go`, `expr.go` (eval), `explain.go` (`ARRAY[...]` render + `ANY`/`ALL` keyword), `params.go` (`bindExpr`, preserve `All`), `agg.go` **twice** (aggKey render — keyed `any%d/%v` so ANY vs ALL don't collide; and the rewrite/`subExpr` descent — clone `Elems`). `numParams`/`validateCheckExpr` ride on `walkExpr` for free.

### Gotchas
- The empty-array return was first written `!n.All == false` (unreadable) → simplified to `return n.All, nil`.
- `array[]::int[]` parses as `ExCast{ExArray{}, "int[]"}`, **not** `ExArray` — hence the cast-peel in `anyElements`, which also unlocks the idiomatic `'{1,2,3}'::int[]`.
- `parseArrayLiteral` handles quotes, backslash escapes inside quotes, empty `{}`→nil, and bare `NULL`→nil element (quoted `"NULL"`→literal string).

### Verification
- `TestExprAnyAll` in `expr_test.go`: array/subquery/literal-string forms, `>`/`>=`/`<>` ops, empty-array ANY/ALL, `SELECT ARRAY[...]` render.
- psql 17 over the wire (daemon on `:5455`): all forms + `EXPLAIN` showing `Filter: (id = ANY(ARRAY[1, 3]))`.

## Window functions (milestone 19) — DONE

A **third SELECT execution shape** (`window.go`, new file). `runSelectCore` forked only `execSelect` (streaming, per-row project) vs `execSelectAgg` (N→1/group); windows compute across a partition but **emit every input row**, so they need `execSelectWindow`: materialize post-filter rows → partition → sort each partition → assign per-row → hand to the shared sort/offset/project tail.

- **AST**: new `ExWindow{Win WinFunc, Agg AggFunc, Arg, Star, Partition []Expr, OrderBy []OrderItem}` — exactly one of `Win` (ranking family: `WinRowNumber/WinRank/WinDenseRank`, via `winNames`) or `Agg` set. `fnName()` returns the name either way. Same 7-site checklist as M18 (`walkExpr`, `expr()`, parser, eval, explain, `bindExpr`, and `exprType`/`exprName`).
- **Parser**: after an aggregate call, if next token is `over` → wrap `ExAgg` into `ExWindow` (converts `Col`→`&ExCol`, rejects `DISTINCT`). Ranking fns (`row_number/rank/dense_rank`) matched before the aggregate branch, require `()` then `OVER`. `windowOver()` parses `OVER (PARTITION BY exprs ORDER BY exprs [DESC|ASC])`; rejects `ROWS/RANGE/GROUPS` frame keywords.
- **Executor** (`execSelectWindow`): materialize only the `sc.width` base columns. Rewrite each `ExWindow` in select items + ORDER BY into an `exWinRef{idx, typ, name}` placeholder (`rewriteWindows`, modeled on `bindExpr`; unhandled nesting survives to a loud eval error). Precompute `winvals[row][winIdx]`, then evaluate the rewritten exprs per row with `env.win` set so `exWinRef` resolves — reusing `projectSelect` + `buildSortKeys` + `sortOffsetProject`.
- **`computeWindow`**: partition by `tuple.Encode` of PARTITION BY vals (NULLs partition together, same as GROUP BY), preserve first-seen order, sort each partition by the window ORDER BY (`sortPartition` via `orderCmp`), then `assignWindow`.
- **`assignWindow`**: `ROW_NUMBER`=1-based ordinal; `RANK`=`n+1` on order-key change (gaps after ties), `DENSE_RANK`=+1 per distinct key; aggregate = reuse `accum` (`intSum` from arg type) — **whole partition when no ORDER BY**, else **running with the Postgres RANGE peer-sharing default** (peers sharing an order key all see the value accumulated through the *end* of their peer group). `equalVals` treats two NULLs as equal peers.
- **`env.win []any`** added to `exEnv`; `exWinRef` reads `env.win[idx]`. Bare `ExWindow` at eval → clear error "only allowed in SELECT list and ORDER BY".
- **EXPLAIN**: `coreNode` wraps a `WindowAgg` node listing `Window: fn(...) OVER (...)` per window (`windowText`).
- **Guardrails, all verified via psql**: explicit frames → "explicit window frames (ROWS/RANGE/GROUPS) are not supported"; window+GROUP BY → "window functions with GROUP BY or aggregates are not supported"; `DISTINCT` in window → rejected.
- **Refactor**: extracted `sortOffsetProject` (sort→offset→limit→project tail) and `buildSortKeys` (ORDER BY → combined-row keys + hidden exprs) from `execSelect` so plain and window paths share them, no divergence.
- **Naming gotcha**: a bare `row_number() over (...)` would lose its column name once rewritten to `exWinRef` — so `exWinRef` carries `name` (set from `fnName()`) and `exprName` has a case for it. Nested (`... + 1`) correctly names `?column?` like PG.
- **Tests**: `TestExprWindow` (row_number partitioned, whole-partition sum, running sum, `count(*) over ()`, bare name) + `TestExprWindowRankTies` (RANK gaps vs DENSE_RANK). psql end-to-end confirmed peer-sharing running sums and the WindowAgg plan.

**Deferred** (next window milestone): `ROWS/RANGE BETWEEN` explicit frames, `LAG`/`LEAD`/`FIRST_VALUE`/`NTH_VALUE`, window+GROUP BY coexistence, named `WINDOW w AS (...)` clause.

## State at end of session

- Milestones 1–19 done and **pushed**; `main` at `02e34d0`. btypedb still `v0.4.0`.
- README status bumped to 1–19; Later list: window frames + LAG/LEAD, ORDER BY exploiting index order.

## Next

- Window follow-up: frames (`ROWS/RANGE BETWEEN`), `LAG`/`LEAD`/`FIRST_VALUE`.
- ORDER BY exploiting index order (DESC indexes → bounded `ORDER BY x DESC LIMIT n` scan).
- Wire: cancellation, COPY, portal suspension.
- Someday: tag btypedb so pgwire's `replace` becomes a versioned `require`.
