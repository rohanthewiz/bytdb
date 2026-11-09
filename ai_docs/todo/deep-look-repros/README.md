# Deep-look audit repros

The runnable evidence behind
[`../2026-1109-deep-look-remediation-plan.md`](../2026-1109-deep-look-remediation-plan.md),
copied from the audit's scratch directory on 2026-11-09 against bytdb
`6ccf3cc` and btypedb `2383665`. They are reference material for plan item
D-01 (port each repro into its owning package as a skipped failing test) and
can be deleted once that is done.

| Directory | Covers | Plan items |
|---|---|---|
| `engine-audit/` | Root engine package: serializable probe, DDL-callback deadlock, sequence spin, closed-Txn reads, shared descriptor, identity bump, allocator leaks; plus the model-based index-drift and OCC stress tests that passed | D-11 to D-13, D-19, D-20, D-36, D-45, D-46 |
| `sqlexec-audit/` | Executor and planner: subquery ORDER BY/LIMIT, temporal comparisons, UPDATE own-writes, snapshots, FK timing, pushdown gaps; the differential and oracle tests that passed; benchmarks | D-09, D-10, D-21, D-23, D-27 to D-31, D-37 to D-39, D-43 |
| `sqlfront-audit/` | Lexer, parser, expressions, casts, params, session, syscat; `deep/` is the stack-overflow case; `wire/` (own module) drives pgx end to end | D-06, D-07, D-18, D-21 to D-25, D-40, D-44, D-47, D-51 to D-56 |
| `edge-audit/` | pgwire, stdlib, replicate: ambiguous-PUT restore, pgx ping, write deadline, lock waits, stale prepared statements, batch atomicity, accept errors; wire benchmarks | D-05, D-08, D-14 to D-17, D-32, D-33, D-41, D-42, D-57 |
| `sys-measure/` | Load, heap per row, startup replay and compaction at 200k and 1M rows; `verify/` and `verify2/` are the main session's independent re-checks of the headline SQL findings | D-48, D-49 |

## Running

Each directory is its own Go module, outside `go.work` on purpose so that
`go test ./...` at the repository root never picks these up. The `replace`
directives are relative and expect btypedb cloned beside bytdb
(`../btypedb` from the repository root).

```sh
cd engine-audit
GOWORK=off GOFLAGS=-mod=mod go test -count=1 -v -run TestR1_ .
```

`edge-audit/` and `sqlfront-audit/wire/` need pgx and lib/pq from the module
cache or the network.

## Reading the results

- **A test that FAILS is demonstrating a bug.** Several log their
  observations instead of asserting, so read them with `-v`.
- Some hang by design (the deadlock and spin repros) and report after a
  timeout of a few seconds.
- `sqlfront-audit/deep` kills its own process with a stack overflow when
  run with `DEEP_KIND=plus DEEP_N=1000000`; that is the finding.
- "Postgres gives" comments are from knowledge. Nobody ran Postgres.
- Benchmark numbers quoted in the plan came from an Apple M3 (reviewers) and
  an M1 Pro (`sys-measure/`).
