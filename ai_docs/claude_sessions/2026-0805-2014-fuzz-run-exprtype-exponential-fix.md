# Fuzz campaign: 10-minute runs on all targets; exprType exponential blowup found and fixed

Session: 75c2ba9c-6d1f-4b64-8d11-209efc6befee
Date: 2026-08-05

## What happened

Ran every fuzz target for 10 minutes (concurrently, in background).
FuzzExec surfaced a real bug; the other four were clean.

| Target                    | Execs | Result |
|---------------------------|-------|--------|
| sql FuzzExec              | 9.8M  | worker "hung" at 2m43s — real finding |
| sql FuzzParse             | 17.9M | clean |
| pgwire FuzzMessageParse   | 10M   | clean |
| tuple FuzzTupleRoundTrip  | 48M   | clean |
| tuple FuzzTupleOrder      | 48M   | clean |

## The bug: exponential type derivation on minus chains

Failing input: `SELECT-0-0-00-0-...-5-0-$1-$1` (~30 terms). No panic —
the query took 3.6s, so the (oversubscribed) fuzz worker was killed as
hung while minimizing. Profiling put 94% of CPU in `exprType`
(sql/expr.go): the `*ExArith` "-" arm called `exprType` on both
operands for the JSONB check, then FELL THROUGH and called both again
for the float check → T(n) = 2·T(n-1) → 2^depth on a left-nested
minus chain. Measured: n=20 → 141ms, n=26 → 3.5s.

Severity: `d.Exec` has no deadline of its own, so a ~60-char hostile
query could pin a CPU indefinitely — a DoS surface reachable through
pgwire. (The fuzz harness only survived because its ExecCtx carries a
3s deadline.)

## Fix (sql/expr.go, exprType *ExArith arm)

Compute `lt, rt := exprType(sc, n.L), exprType(sc, n.R)` exactly once
after the pure-JSONB operators return, then branch on the saved
values. n=26: 3.5s → 0.7ms; n=1000 chain: 5ms.

Checked the other recursive type paths for the same shape —
`staticJSONB`, `ExWindow.resultType`, describe.go's
columnType/itemType — all single-eval.

## Regression guards

- Minimized crasher kept as seed corpus:
  sql/testdata/fuzz/FuzzExec/416d7daa00af8c28 (replayed by plain
  `go test`).
- New `TestTrickyDeepArithChain` (sql/tricky_test.go): executes a
  500-term minus chain ending in `$1` — instant when derivation is
  linear, effectively infinite (2^500) on regression, so the ordinary
  test timeout is the detector; no wall-clock assertion needed.

## Verification

- `go test ./...` green in root module; pgwire module green.
- FuzzExec re-run 2m post-fix: 11.2M execs (throughput roughly doubled
  vs the buggy run), no findings.

## Files touched

- sql/expr.go — exprType single-eval fix
- sql/tricky_test.go — TestTrickyDeepArithChain
- sql/testdata/fuzz/FuzzExec/416d7daa00af8c28 — new corpus entry
