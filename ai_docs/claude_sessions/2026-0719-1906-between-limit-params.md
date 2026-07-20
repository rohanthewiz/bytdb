# Session: BETWEEN + parameterized LIMIT/OFFSET (v0.6.2)

Session ID: `c3b2b2fe-078c-45b4-a7da-5878cf7da981`
Date: 2026-07-19 (evening)

## Context / motivation

The church-platform repo hit two bytdb gaps and worked around them
locally (explicit >=/<= in its recurrence CHECKs; interpolated ints in
three raw LIMIT/OFFSET queries). Both fixed upstream here so the
workarounds can be reverted.

## Gap 1 — BETWEEN (commit `91778f1`, in `sql`)

BETWEEN was absent from the whole scalar grammar, not just CHECK —
the only prior `between` handling was the window-frame mini-grammar.

- `parser.go` (`exprCmp`): `[NOT] BETWEEN [SYMMETRIC|ASYMMETRIC]`
  postfix cases beside `[NOT] IN` / `[NOT] LIKE`; new `betweenTail`
  next to `likeTail`. **No AST node** — pure desugar:
  - `x BETWEEN a AND b` → `ExAnd{ExCmp{OpGE}, ExCmp{OpLE}}`
  - `x NOT BETWEEN a AND b` → `ExOr{ExCmp{OpLT}, ExCmp{OpGT}}`
  - SYMMETRIC → in-range(a,b) OR in-range(b,a); its NOT wraps ExNot.
- Bounds parse at the additive level (`exprAdd`), so the separating
  AND stays boolean — Postgres's b_expr restriction. Placeholders in
  bounds work like any comparison.
- Because it desugars, evaluation (`evalTruth`), CHECK validation
  (`validateCheckExpr`), the stored-text re-parse on every write
  (`parseCheckExpr`), and param binding all needed zero changes.

## Gap 2 — $n in LIMIT/OFFSET (same commit)

- `ast.go`: `Select.LimitParam/OffsetParam Param` (0 = none). The
  executor keeps reading the `int64` twins; parse leaves them at
  their no-op defaults (-1 / 0).
- `parser.go` (`nonNegInt`): now returns `(int64, Param, error)`; a
  tParam token routes through `literal()` (reusing $n numbering and
  the 65535 cap).
- `params.go`: the bind walk got a small refactor — the bare
  `sub func(any) any` closure became a `binder` struct (vals + err)
  threaded through bindSelect/bindBool/bindExpr/bindReturning, so a
  value position with shape requirements can error. `binder.count`
  resolves the placeholder on the per-execution copy: NULL → no-op
  default (Postgres semantics), negative → "LIMIT/OFFSET must not be
  negative", non-integer → rejected. bindParams funnels all arms into
  one `out` + single `b.err` check. `noteSelectVals` counts the two
  bare-Param positions (the only value positions not held as ExLit).
- `describe.go` (`describeSelect`): counts note as `bytdb.TInt`, so
  pgx (which encodes by described OID) sends int8 — this is what
  makes the church queries work over the wire.
- `pgwire/errors.go`: negative bound counts map to SQLSTATE `2201W`
  (LIMIT) / `2201X` (OFFSET) instead of falling to XX000.
- Prepared statements re-bind per execution; nested selects (scalar
  subqueries, UNION arms) resolve through the same walk.

## Pre-existing quirks found, deliberately not fixed

- The `IN (SELECT ...)` (= ANY) evaluation path ignores the
  subquery's LIMIT **even as a literal**.
- Scalar subqueries ignore `ORDER BY ... DESC`.
Both noted in `limit_param_test.go`; upstream candidates for later.

## Verification

- New `sql/between_test.go` (WHERE forms, SYMMETRIC, precedence,
  params in bounds, CHECK column/table/ALTER-ADD round-trips) and
  `sql/limit_param_test.go` (binding, re-execution, shared $n, NULL,
  negative/non-integer rejections, Describe types, nested selects).
- Two stale pins rewritten: `semantic_gaps_test.go`
  (TestLimitOffsetCorners) and `parser_test.go` (TestParseParams) had
  asserted the old rejections.
- Wire-level via the `verify` skill: scratch `bytdbd` on :5439 + pgx.
  BETWEEN CHECKs reject as 23514, `LIMIT $1 OFFSET $2` with Go ints
  returns the right window, NULL limit = all rows, `-1` → 2201W.
- Full suites green with `-count=1`: root, sql, pgwire, replicate.

## Release (tags pushed)

Same nested-module ordering as v0.6.1:

1. `91778f1` committed → tagged `v0.6.2` → pushed.
2. pgwire pin `v0.6.1` → `v0.6.2`, tests green → `526aee8` →
   tagged `pgwire/v0.6.2` → pushed.

Consumers: `go get github.com/rohanthewiz/bytdb/pgwire@v0.6.2`.

## Church repo next step (other repo)

Bump its dep to v0.6.2, then revert the two workarounds: restore
BETWEEN in the recurrence CHECK migrations and put `$n` placeholders
back in the chat ×2 / prayerwall ×1 LIMIT/OFFSET queries.
