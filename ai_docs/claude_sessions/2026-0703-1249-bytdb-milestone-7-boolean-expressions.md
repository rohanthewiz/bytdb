# Session: bytdb-milestone-7-boolean-expressions

- **Session ID**: `123070ed-1461-43bc-a7b6-b301963f11ba`
- **Date**: 2026-07-03
- **Continues**: milestones 5–6 earlier in the same live session (doc saved in the btypedb repo: `~/projs/go/btypedb/ai_docs/claude_sessions/2026-0703-1230-bytdb-milestones-5-6-sql-frontend-and-aggregates.md`). Session docs move into this repo from here on.
- **Repo**: `~/projs/go/bytdb` (github.com/rohanthewiz/bytdb)
- **Result**: milestone 7 complete — `e86a7eb`: AND/OR/NOT boolean expressions in WHERE and HAVING with SQL three-valued logic. All tests green under `-race`; verified as an external consumer.

## What changed (`e86a7eb`)

- **AST**: `Where []Pred` / `Having []AggPred` → `BoolExpr` trees. Nodes: n-ary `And`/`Or` (parser flattens same-operator chains), `Not`, and leaf `*Pred{Item SelectItem, Op, Val}`. `AggPred` deleted — because the leaf carries a `SelectItem`, HAVING shares the identical tree/evaluator with aggregate calls as leaves. `walkPreds(e, visit)` is the shared tree walker.
- **Parser**: precedence climber `boolExpr(allowAgg)` → `boolAnd` → `boolNot` → `predLeaf`; OR loosest, then AND, then NOT (right-recursive, stacks), parens group in `boolNot`. `predOperand(allowAgg)` unifies the old WHERE operand and HAVING item parsing: aggregate call allowed only when `allowAgg` (WHERE gets "aggregates are not allowed in WHERE; use HAVING"). Literal-first comparisons still flip.
- **Three-valued logic** (`tri` in exec.go): `checkPred` returns true/false/unknown (comparison touching NULL or incomparable kinds → unknown; IS [NOT] NULL always definite); `evalBool(e, leaf)` implements Kleene AND/OR/NOT; rows/groups match only on `triTrue`. This is the semantic reason the tree rewrite couldn't keep bool: `NOT (v = 1)` over NULL must stay unknown, not flip to true. Classic checks in tests: `v = 1 OR v != 1` excludes NULL; `NOT v IS NULL` ≡ `IS NOT NULL`.
- **Planner**: `planScan(desc, where BoolExpr)`; `conjuncts()` extracts only leaves of top-level AND chains for eq/lo/hi pushdown — anything under OR/NOT is residual-only. Column validation walks the whole tree (incl. OR/NOT subtrees). `plan.preds []Pred` → `plan.filter BoolExpr` (the whole WHERE; `matches` = `evalBool == triTrue`). Pushdown-beside-OR works: `id >= 10 AND (a OR b)` pushes the id bound.
- **agg.go**: `q.having BoolExpr` + `havingRefs map[*Pred]aggRef` (leaf pointers → group-col/accumulator refs, resolved via `walkPreds`); group filter is `evalBool` with a leaf func reading `valueOf(g, ref)`. Old bool `checkPred` deleted; the tri version in exec.go is the single comparison evaluator.

## Testing / verification

- Parser: precedence, paren grouping, n-ary flattening, stacked NOT (`TestParseBoolExprs`).
- Planner: OR-only → nothing pushed even when every branch hits the PK; NOT blocks its subtree; conjunct beside OR still pushes; column validation under OR/NOT errors (`TestPlanORIsResidualOnly`, `TestPlanConjunctBesideOR`).
- E2E: OR/AND precedence, parens, NOT over OR, UPDATE/DELETE with boolean WHERE, HAVING with OR and NOT over aggregates, three-valued suite (`TestSQLThreeValuedLogic`).
- Consumer demo (scratchpad module, `replace` → local repo) re-run end-to-end including reopen persistence; `NOT score = 9.5` over NULL scores correctly returns zero rows.

## State at end of session

- bytdb main = `e86a7eb`, pushed. Milestones 1–7 done. Tests green under `-race`. Deps unchanged (btypedb v0.3.0, serr v1.3.0, Go 1.26); sql package still zero new deps.
- Session-long finding still open: serr `%v` prints only the message — structured fields (want/got/pos) need folding into client-facing messages when a REPL/pgwire layer arrives.

## Next (deferred list, in order)

- **Joins** (nested-loop first; needs multi-table FROM, qualified column refs — the flat `Item.Col` naming will need table qualification).
- **Prepared statements** (`$1` placeholders already lex; wire Pred.Val/Insert values to bind params).
- **`bytdb-pgwire` module** (separate go.mod; `Result{Cols, Types, Rows}` is already the RowDescription/DataRow shape; autocommit-only first; `pg_catalog` is the ORM long tail).
- Engine: DESC key columns, CHECK/NOT NULL, savepoints, EXPLAIN (plan struct is ready to print: get/index/from/stops/filter).
