# Session: Milestone 29 — expression values in INSERT, SELECT DISTINCT

- **Session ID**: `cb8308f8-23df-47c1-a148-b1dce42059e6`
- **Date**: 2026-07-11
- **Prior context**: continued from the m28 session doc (outstanding-list
  sweep) on post-`95285c6` main. User said "continue with milestone 29:
  expression values in INSERT and SELECT DISTINCT" — the first two items
  on the m28 Later list (sequence functions in DEFAULTs deferred).
- **State at end**: both features implemented, tests green in both
  modules, `go vet`/`gofmt` clean, verified end to end over the wire
  (18 checks, simple + extended protocols). Committed and pushed at
  session close.

## 1. Expression values in INSERT

`INSERT INTO u VALUES (nextval('ids'), 'by'||'tdb', 2*3)` now works —
the m28 gotcha closed. VALUES entries parse through the full expression
grammar and evaluate per row at execution.

- **Parser** (`parser.go` `insertValue`, called from `insert()`): each
  entry parses via `p.expression()`. A bare `*ExLit` unwraps to its
  parsed value — the historical representation (int64/float64/string/
  bool/nil/`Param`/`defaultMarker`) that binding, describe, and engine
  coercion key off — so plain-literal inserts are byte-for-byte
  unchanged. Anything richer stays an `Expr` inside `Insert.Rows`'s
  `[]any`. Aggregates ("aggregate functions are not allowed in VALUES")
  and windows rejected at parse, PG wording. The `DEFAULT` keyword
  check stays *ahead* of the expression grammar (it would read as a
  column ref).
- **Execution** (`exec.go` `resolveExprValues`): runs right after
  `resolveDefaultMarkers`, before the ON CONFLICT probe / CHECKs /
  engine insert — so all of those see the evaluated value. Environment
  is `&exEnv{d, tx, sc: &scope{}}` built once per statement: the write
  txn makes `nextval`/`setval`/scalar subqueries work, the **empty
  scope** makes column references fail with "no such column".
  Clone-on-first-expr mirrors resolveDefaultMarkers: rows alias the
  parsed AST, so prepared statements re-evaluate volatile calls each
  execution (fresh sequence draws — tested).
- **Params**: `numParams`/`bindParams` (params.go) treat an `Expr` row
  value via `noteExprVals`/`bindExpr` (placeholders live in the tree as
  `ExLit{Val: Param}`); `describeInto`'s Insert case (describe.go)
  infers a placeholder inside an expression as the target column's
  type, best-effort — same policy as UPDATE SET expressions. Over the
  wire pgx encodes `$1 + 1` into an int column as int8.
- Identity-draw recording (`vals[i] == nil` → lastval) and RETURNING
  ride the evaluated values unchanged. EXPLAIN INSERT untouched.

## 2. SELECT DISTINCT

- `Select.Distinct` bool (ast.go). Parser (`selectCore`): accepts
  `DISTINCT`, treats `ALL` as the explicit default, rejects
  `DISTINCT ON (...)` outright ("SELECT DISTINCT ON is not supported")
  rather than misreading it. `count(DISTINCT x)` untouched (separate
  parse site). Note `sql/join_test.go:145` uses a table aliased
  `distinct_marker` — acceptKw matches whole tokens, so it's safe.
- **One wrapper for every shape** (`exec.go` `execSelectDistinct`,
  dispatched first in `runSelectCore`): the core re-enters
  runSelectCore with Distinct/OrderBy/Limit/Offset stripped (so plain,
  aggregate, windowed cores all work, and collection early-cutoffs
  can't under-produce), projected rows dedup via `dedupRows` (tuple
  encoding: NULLs equal NULLs), then sort/offset/limit apply to the
  deduped set. UNION arms get it free — execUnion's head/arm calls go
  through runSelectCore.
- **ORDER BY restriction** (PG's rule + message: "for SELECT DISTINCT,
  ORDER BY expressions must appear in select list"): output column
  names and positions only — a dropped sort key would decide which
  duplicate survives. Factored `outputSortKeys(order, cols, errName,
  errShape)` and `sortLimitRows` out of execUnion; UNION keeps its
  exact old messages via the two error params.
- **Subquery evaluators** (expr.go, bypass runSelectCore): scalar
  `evalSubquery` collapses duplicates *before* the one-row rule —
  streams values through a seen-set, stops at OFFSET+2 distinct values,
  then applies offset/limit/one-row on the distinct list. ANY/ALL
  (`collectSubColumn`) and `ARRAY(SELECT ...)` (`evalArraySub`) dedup
  via new `dedupScalars` (semantically a no-op for ANY/ALL, done for
  consistency). EXISTS ignores Distinct, correctly. expr.go now
  imports `tuple`.
- **EXPLAIN** (`explain.go` `armNode`, wrapping coreNode): Unique node
  between Sort and the scan, matching execution order (dedup → sort).
  armNode strips OrderBy from the core so the order-eliminating-scan
  path is never falsely claimed for a distinct query; used for both
  the non-union body and each UNION arm.

## Tests

- `sql/insert_expr_test.go`: TestInsertExprValues (arith/concat/func/
  CASE/cast/DEFAULT mix, RETURNING), TestInsertExprNextval (draws,
  currval/lastval, re-execution draws fresh), TestInsertExprSubquery-
  AndParams (scalar subquery value, $n in expressions per execution,
  conflict probe on evaluated value), TestInsertExprErrors (column ref,
  aggregate, window, engine coercion error).
- `sql/distinct_test.go`: TestSelectDistinct (NULL collapse, multi-col,
  expression items + alias order, position order, limit counts
  distinct rows, SELECT ALL), TestSelectDistinctShapes (over GROUP BY,
  over a window, DISTINCT UNION-ALL arm, scalar subquery both ways,
  ANY, ARRAY(SELECT DISTINCT), no-FROM), TestSelectDistinctErrors
  (dropped-column/expression/qualified ORDER BY, bad position,
  DISTINCT ON), TestSelectDistinctExplain (Sort above Unique).
- sequence_test.go's stale "INSERT VALUES takes literals" comment
  updated (draw-then-insert still tested as the driver pattern).

## Verification (wire-level)

Same recipe as m24–m28 (`.claude/skills/verify/SKILL.md`): built
`pgwire/cmd/bytdbd`, scratch db on 127.0.0.1:5439, pgx v5 client.
18 checks passed: nextval/concat/arith in VALUES over the simple
protocol, `$1 + 1` + scalar subquery VALUES over the extended protocol
(describe infers int8), conflict probe on the evaluated value,
RETURNING, both rejection wordings; DISTINCT single/multi column,
limit/offset after dedup, $n filter, over groups, over a window, UNION
arm, scalar subquery, EXPLAIN Unique node, and both DISTINCT errors.

## Docs

- README: feature summary + Milestone 29 entry; Later list now:
  sequence functions in column DEFAULTs, DISTINCT ordered by
  select-list expressions, DISTINCT ON.
- docs/gotchas.md: removed the expressions-in-VALUES row; nextval-in-
  DEFAULT row now points at VALUES; added DISTINCT ON and
  DISTINCT-ORDER-BY-expression rows.
- docs/features.md: new "### DISTINCT" subsection, expression-values
  bullet under Expressions, sequences section updated (nextval direct
  in INSERT).

## Gotchas / notes for next time

- Milestone numbering: 29 = these two features. Next arc starts at 30.
- `Insert.Rows` `[]any` now holds a fourth value kind: `Expr` (beside
  literals, `Param`, `defaultMarker`). Anything new that walks Rows
  must type-switch on Expr (numParams/bindParams/describeInto are the
  three existing walkers, all updated).
- DISTINCT ORDER BY takes **output names/positions only** — PG also
  allows expressions that textually match a select item; that subset
  is on the Later list.
- `execSelectDistinct` materializes the full core result before dedup:
  `SELECT DISTINCT x FROM huge LIMIT 5` scans everything (no early
  cutoff — correctness requires it; a streaming dedup-with-cutoff is a
  possible future optimization).
- The DISTINCT scalar-subquery path early-stops at OFFSET+2 distinct
  values; LIMIT can only shrink further, so that bound is decisive.
- ANY/ALL over SELECT DISTINCT is semantically identical to the plain
  form; the dedup there only trims the comparison list.
- `nextval` in VALUES draws are transactional like all bytdb sequence
  ops (m28 divergence note still applies: rolled-back draws are
  reused, PG burns them). A multi-row INSERT that fails on row N rolls
  back rows *and* draws.
- Still open (README Later): sequence functions in column DEFAULTs,
  DISTINCT ON, DISTINCT ORDER BY select-list expressions.
