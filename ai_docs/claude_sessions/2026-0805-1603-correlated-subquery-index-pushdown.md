# Correlated subquery index pushdown (outer-value binding before planning)

Session: 355bee9f-8640-4065-9908-239c88a5e7dc
Date: 2026-08-05
Follows: 2026-0805-1537-perf-robustness-pass-six-packages.md ("Known
remaining" item #1)

## What happened

Closed the last documented perf hole from the six-package pass:
correlated subqueries ran their correlated predicate as a post-scan
Cond filter, full-scanning the inner table per outer row. Now a
correlated conjunct of the form `inner_col op outer_ref` binds the
outer value before planning and pushes into the inner scan's index,
exactly like the nested-loop join's predTmpl/rebind machinery from
the previous session's fix #7.

Measured on the 3000x3000 correlated point-lookup
(`WHERE (SELECT count(*) FROM ri WHERE ri.id = lo.k) = 1`):
**15.66s (HEAD baseline, measured in a worktree) → 9.9ms (~1600x)**.
Same query over the pgwire surface: 9.2ms.

## Design (sql/corrsub.go, new)

- `decorrelateCollect` walks the subquery WHERE at top-level AND
  positions only and extracts pushable correlated conjuncts (strict
  ops `= < <= > >=`, both sides plain columns, one resolving in the
  inner scope, the other resolving somewhere up the enclosing env
  chain). Each becomes a template `*Pred` appended to the owning
  joinStep's `static` list; the original Cond leaf stays in the WHERE
  tree untouched, so the final row filter is unchanged and the
  template only narrows what the scan visits.
- `prepareSubFrom` is the shared front half of all four subquery
  runners (scalar/agg, ANY/ALL column, ARRAY, EXISTS) — replaced their
  four duplicated buildScope/decorrelate/prepareFrom preambles in
  sql/expr.go. Prepared plans cache per statement in the subMemo
  (new `plans map[*Select]*subPrep` field), same lifetime rules as the
  uncorrelated result memo from the previous session's fix #6.
- Per invocation, `bind` resolves each outer ref with `env.lookupVal`
  — the identical climb the Cond's ExCol performs after failing inner
  resolution, and both template and Cond evaluate through `checkPred`
  (ExCmp delegates to it), so agreement is by construction, not
  parallel reimplementation. `plan.rebind` then refreshes the pushed
  bounds without re-planning.

## Safety valves

- NULL outer value → `empty` short-circuit: strict comparison matches
  nothing and templates only sit on non-LEFT steps, so the invocation
  skips runJoin entirely (aggregates still report zero-row values:
  count 0, min NULL).
- Bound Go type change (possible from untyped virtual-table sources)
  → drop the step's cached tmplPlan and re-plan; rebind's
  fixed-type assumption is never trusted across a type flip.
- Outer ref not evaluable yet (partial row during ON eval) → fall
  back to untemplated preparation, preserving lazy name resolution
  (queries whose exotic branches scan zero rows still succeed).
- No templates for: conjuncts under OR/NOT (not individually
  required), LEFT-joined steps (prepareFrom's own WHERE rule),
  virtual tables (no scan to push into), correlated ON clauses
  (kept out of scope), non-Pred (function-wrapped) conditions.
- Cache guard: `envSC` (enclosing scope pointer) revalidated on
  reuse; mismatch re-prepares. Chain drift is otherwise harmless
  because bind re-resolves per invocation — a shifted chain yields
  the same value the Cond would see.

## Verification

- 7 new tests in sql/corrsub_test.go: all four runner shapes, flipped
  operands, ranges, NULL/dangling outers, refusal cases (OR-nested,
  float-vs-int litFits fallback, LEFT-join, joined subqueries), and a
  scale test bounding the 3000x3000 query at 5s (~100x headroom over
  the fixed ~10ms, ~3x under the old 15.7s — it genuinely fails on
  pre-change code).
- `go test -race ./...` green across all six modules; vet + gofmt
  clean.
- End-to-end via the verify skill (bytdbd on :5439 + pgx): simple and
  extended protocols correct, including prepared-statement
  re-execution with different $n args (fresh AST per execution, no
  cross-execution plan-cache leakage). Note: booleans arrive as t/f
  text over the simple protocol — expected rendering, not a bug.

## Known remaining (deliberate)

- Correlated ON conjuncts and function-wrapped correlated predicates
  still evaluate per row without pushdown — rewrite as JOIN for large
  outer sets (skill gotcha updated to say exactly this).
- Statement paths that never seed a subMemo re-prepare per invocation
  (safe direction; they still get within-invocation pushdown).
- Full decorrelation-to-join rewriting (Postgres-style pull-up) was
  considered and rejected: much bigger surface for marginal gain over
  the indexed nested loop.

## Housekeeping

- Skill SKILL.md correlated-subquery gotcha rewritten: pushable
  shapes are now cheap; only the refusal shapes need JOIN rewrites.
- `decorrelate` doc comment points at decorrelateCollect/corrsub.go.
