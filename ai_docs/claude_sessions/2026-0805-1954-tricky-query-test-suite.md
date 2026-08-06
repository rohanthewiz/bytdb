# Tricky-query test suite: pinning Postgres semantics beyond crash-hunting

Session: 75c2ba9c-6d1f-4b64-8d11-209efc6befee
Date: 2026-08-05

## What happened

Follow-up to the DISTINCT qualified-ORDER-BY fix: that bug returned a
clean error, so the fuzz targets (panic-only) and the planner
equivalence property (WHERE/ORDER BY over plain selects) were both
blind to it. Added a suite aimed at that gap — wrong answers and
wrong rejections on small-but-devious queries.

## Added

### sql/tricky_test.go (new)

Deterministic corner tests, every expectation Postgres-verified:

- **TestTrickyNameResolution** — ORDER BY output-alias shadowing a
  source column (`select sal as id ... order by id` sorts by sal),
  ordering by unselected columns, positional keys mixed with named,
  and `emp.sal` rejected once the binding is `emp e`.
- **TestTrickyThreeValuedLogic** — Kleene shortcuts
  (`NULL AND false = false`, `NULL OR true = true`), NOT(UNKNOWN)
  staying UNKNOWN via `dept = dept` / `not (dept = dept)` (4 vs 0
  rows), BETWEEN with a column-borne NULL bound, count(CASE-no-ELSE),
  `'x' || null` → NULL.
- **TestTrickyIntegerArithmetic** — trunc-toward-zero division,
  dividend-sign modulo, MaxInt64+1 and (MinInt64)/-1 erroring
  "bigint out of range", MinInt64 % -1 = 0.
- **TestTrickyDistinctUnionNulls** — NULLs equal for DISTINCT/UNION
  dedup though unknown for `=`; UNION ALL keeps duplicates; ORDER BY
  binds to the whole union.
- **TestTrickyScalarSubqueries** — empty scalar subquery → NULL,
  multi-row → error, correlated per-group max, NOT IN poisoned by
  subquery NULL, EXISTS never matching NULL keys.
- **TestTrickyAggregateEdges** — global aggregate over zero rows =
  one row (count 0, others NULL) vs grouped = zero rows; HAVING
  without GROUP BY; count(col) vs count(*); NULLs as one group;
  sum stays int64 unless a float enters the expression.
- **TestTrickyWindowEdges** — the RANGE-default peer trap:
  `sum(sal) over (order by sal)` with ties jumps by peer groups
  (80,170,370,370,610,610); rank vs dense_rank vs row_number.
- **TestTrickyLeftJoinFilters** — same predicate in ON (restricts
  matching, all left rows survive) vs WHERE (kills NULL-extended
  rows); anti-join via `where right.key is null`; comma-FROM cross
  join.
- **TestTrickyOrderLimitEdges** — ASC NULLS LAST / DESC NULLS FIRST
  defaults, LIMIT 0, OFFSET past end, ORDER BY on an expression.
- **TestOrderByFormulationEquivalence** — the property behind the
  DISTINCT fix, generalized: 300 random projections (± DISTINCT,
  ± table alias), each ORDER BY key spelled three ways (bare,
  qualified, positional); all spellings must agree — same rows
  (multiset + per-result order-spec check, since ties are common)
  or same failure. Would have caught the original bug directly.

### sql/exec_fuzz_test.go

16 new FuzzExec seeds mirroring the tricky shapes (DISTINCT +
qualified ORDER BY, alias shadowing, union-in-derived-table, window
ties, Kleene shortcuts, MinInt64 edges, ON-vs-WHERE LEFT JOIN) so
mutation explores from wrong-answer boundaries, not just crash
surfaces. 30s smoke run: 3.4M execs, no crashes.

## Findings (deliberate divergences pinned, not bugs)

1. `WHERE x = NULL` and `BETWEEN NULL AND ...` are rejected at parse
   time (sql/parser.go:2545) with "cannot compare with NULL; use
   IS [NOT] NULL" — intentional strictness, friendlier than
   Postgres's silent empty result. Tests pin the rejection.
2. The literal `-9223372036854775808` lexes as unary minus over
   9223372036854775808, which exceeds int64 and becomes a float
   (mirrors Postgres promoting to numeric) — so MinInt64 arithmetic
   edges only trigger via expressions like
   `(-9223372036854775807 - 1)`. Tests pin both paths.

## Verification

- `go test ./...` green in the root module; pgwire and bench modules
  green; `go vet ./sql` clean.
- `go test -fuzz=FuzzExec -fuzztime 30s ./sql` clean with the new
  seeds.

## Files touched

- sql/tricky_test.go — new
- sql/exec_fuzz_test.go — seed additions
