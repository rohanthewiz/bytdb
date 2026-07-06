# Session: bytdb-milestone-18-any-all-arrays

- **Session ID**: `385748f4-cfa2-4748-a88e-e00cc4d5d609`
- **Date**: 2026-07-06
- **Repo**: `~/projs/go/bytdb` (btypedb untouched, still `v0.4.0`)
- **Result**: milestone 18 complete — `op ANY(...)` / `op ALL(...)` over arrays and subqueries. Commit `06319ee`, **not pushed yet**. Both modules green under `-race`, `go vet` clean, verified end-to-end with psql 17 driving bytdbd. Window functions scoped and started (next section).

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

## Window functions — scoping (next milestone, IN PROGRESS)

Bigger than M18: a **third execution shape**, not an expression node. `runSelectCore` (`exec.go:743`) currently forks only `execSelect` (streaming, projects per row) vs `execSelectAgg` (N rows→1/group). Windows compute across a partition but **emit every input row** — fits neither. New stage runs after WHERE/join, before ORDER BY/LIMIT.

**Plan (MVP + aggregate windows as one milestone; frames + GROUP-BY-coexistence deferred):**
1. Parse `OVER (PARTITION BY exprs ORDER BY exprs)` after a call → new `ExWindow{Fn, Args, PartitionBy, OrderBy}`. Mirror aggregate-call parsing (`parser.go:1631`); reuse expr + ORDER BY parsers.
2. `hasWindow()` predicate + branch in `runSelectCore`. **Reject window+GROUP BY in v1**; windows over base rows only.
3. Executor stage (~`execSelectAgg`-sized): materialize post-filter rows (reuse `execSelect`'s `rows [][]any` buffer) → partition by `tuple.Encode` of PARTITION BY keys (same trick as `newGroup`) → sort each partition via `sortKey`/`orderCmp` → assign per row: `ROW_NUMBER`=ordinal, `RANK`/`DENSE_RANK`=compare to prior sort key, `SUM/COUNT/... OVER`=reuse `accum` → stitch column back, hand to existing sort/limit/projection tail.
4. Plumb the 7 sites for `ExWindow` (EXPLAIN → "WindowAgg" node).

**What makes it tractable**: partitioning reuses `tuple.Encode` grouping equivalence; per-partition aggregates reuse `accum`. **What makes it work**: accept materialize-then-assign (no streaming).

**Deferred**: `ROWS/RANGE BETWEEN` frames, `LAG/LEAD/FIRST_VALUE`, window+GROUP BY together, named `WINDOW` clause.

## State at end of session

- Milestones 1–18 done. bytdb `06319ee` committed **but not pushed**; btypedb still `v0.4.0`.
- README status bumped to 1–18; Later list: window functions, ORDER BY exploiting index order.
- Window functions: scoped, not yet coded. Start at step 1 (parse OVER → `ExWindow`).

## Next

- **Now**: implement window MVP — `ROW_NUMBER/RANK/DENSE_RANK OVER (PARTITION BY ... ORDER BY ...)` + aggregate windows (`SUM(x) OVER (...)`).
- Push `06319ee` (and the window commit) when ready.
- Later: frames, `LAG`/`LEAD`, ORDER BY exploiting index order, wire (cancellation, COPY, portal suspension).
