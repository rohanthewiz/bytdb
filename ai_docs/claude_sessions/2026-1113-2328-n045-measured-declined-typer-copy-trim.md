# N-045 measured and declined; typer copies a Select only on change

Session: `10e25b82-be09-499d-8c4c-c13f4acd0ec2`, the same session as
`2026-1111-1808-duplicate-relation-42p07-n054`. Picked up again after
an `/sw` with nothing to save (the tree was clean). The user pasted
N-045. The item asked to measure before starting. The measurement argued
against the cache, and the user chose to decline it and keep a small
trim the profile turned up.

## Measurement

`BenchmarkDLPointSelectPrepared` (`sql/bench_deeplook_test.go`,
`SELECT amount FROM orders WHERE id = $1`, 100k rows), 6 runs:

| | ns/op | B/op | allocs/op |
|---|---|---|---|
| D-40 plan (2026-11-09) | 3,900–4,900 | — | 117–128 |
| Baseline this session | 2,266 (mean) | 4,680 | 75 |
| After the trim | ~2,240 (median; within noise) | 4,232 | 73 |

CPU profile focused on `Stmt.Exec` (4.14 s of samples):
- `typeStatement` is 0.27 s, about 6.5% (~145 ns per execution).
- `prepareFrom` is 1.10 s, of which `buildScope` is 0.68 s.
- The `Txn.Get` fetch is 0.71 s, `bindParams` 0.16 s.

Allocation profile (`-memprofilerate=1`): 72 allocations per prepared
`Exec`. Typing is 9 of them:
- the typer struct;
- `shapeLookup`'s `sd := *d` (a 104-byte DB copy) and two closures;
- one `Select` copy each in `sel` and `core`;
- `&tscope`;
- a two-allocation `buildScope` built only for typing (execution builds
  its own again);
- the item-types slice.

## Decision: N-045 declined (Non-goals)

A typed-tree cache saves at most ~145 ns in-process, and under 1% over
the wire (a point read is 11–27 µs, D-42). To be correct it must key on
the Go types of the bound arguments as well as the catalog version: a
string `$1` is untyped and takes its column's type, an `int64` is int,
and refusals such as `1 = true` depend on that. It must also replay
every conversion and error exactly, which is correctness risk in the
D-23 pass. Options offered: decline (recommended), Roadmap with a
trigger, or build anyway. The user chose decline.

## The trim (`0e02458`)

`typer.sel` and `typer.core` (`sql/typecheck.go`) began with `c := *s`.
Escape analysis (`-gcflags=-m`: "moved to heap: c") put the 224-byte
`Select` on the heap twice per typed SELECT, even on the common path
where nothing changes and `s` itself is returned. The rebuilt parts now
live in locals (`newWith`, `newUnion`; in core `from`, `groupBy`,
`orderBy` plus the already-local `where`, `items`, `having`), and `*s`
is copied only inside the changed branch. That saves 2 allocations and
448 B per execution.

Left alone on purpose: `insert`, `update`, `delete` and `caseExpr` have
the same up-front copy, but the write path's cost dwarfs one struct
copy. The remaining 7 typing allocations are structural (typer, lookup
closures, scope).

## Verification

- `go test -race ./sql/...` green; `TestParseCacheTreesStayPristine`
  (the copy-on-write contract) and the `TestTyped*` / `TestDescribe*`
  tests pass.
- `sql/difftest` against Postgres 16 (`bytdb-numeric-pg`, 55432) green.
- staticcheck v0.8.2 and gofmt clean.
- `/verify`: a differential pgx client ran 14 SELECT shapes on bytdbd and
  on PG16 and compared rows:
  - shapes: untyped literals against uuid and timestamp columns (the
    changed branch), plain filters (the unchanged branch), WITH, UNION,
    JOIN ON with a literal, GROUP BY/HAVING/ORDER BY by expression, and
    parameters;
  - each over the simple, extended and prepared protocols, with prepared
    run 3 times;
  - 70 checks, 0 diffs.

## Gotchas

- `go run golang.org/x/perf/cmd/benchstat@latest` was denied by the auto
  mode classifier (network fetch). Compare benchmark means with awk.
- A CPU profile of a `sharedBenchDB` benchmark is dominated by the
  dataset load. Use `-focus='sql\.\(\*Stmt\)\.Exec'`.

## Next

Closed: None. Declined: N-045. Raised: None. Deferred: None.
Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
