# Session: OCC v0.7.0 released (full chain) + committed head-to-head harness; README numbers authentic

- **Session ID:** `819fa68d-114d-4da4-ac84-31a285a6439b`
- **Date:** 2026-07-31, 12:49
- **Deliverables:** release chain **btypedb v0.7.0 → bytdb v0.7.0 →
  pgwire/v0.7.0** (all merged to main, tagged, pushed); committed
  head-to-head concurrency harness in `bench/` with README numbers
  regenerated from it (`3982818`).
- **Prior doc (same session, earlier wrap):**
  `2026-0731-0846-benchmarks-head-to-head-readme.md` — benchmark
  re-run, README Concurrent writes section, scratchpad head-to-head.
- **This doc covers what happened after that wrap.**

## Part 1: the v0.7.0 release (closes the 4-stage OCC plan)

Order (root-before-dependents, as always):

1. **btypedb**: `occ-stage1` fast-forwarded into main (`f67e2b6..3692710`,
   3 commits: Stage 1 prototype / Stage 2 accessors / Stage 3 read-set
   validation + exclusive txns). Full suite plain + `-race` green.
   Annotated tag **`v0.7.0`**, pushed with main.
2. **bytdb root**: `occ-stage2` fast-forwarded into main
   (`7ea422e..259d855`, 7 commits — all four OCC stages' bytdb side,
   docs, session docs, README benchmark work). Pin bump btypedb
   v0.6.2→v0.7.0 (`GOWORK=off GOPRIVATE=github.com/rohanthewiz
   GOPROXY=direct go get`, then tidy) = commit `14185cc`, tagged
   **`v0.7.0`**, pushed.
3. **pgwire**: pins btypedb→v0.7.0 + bytdb v0.6.4→v0.7.0 (module still
   carries `replace => ../`, per precedent) = commit `bf5cc31`, tagged
   **`pgwire/v0.7.0`**, pushed. Suite green against real tags.

**Flake note (unresolved, watch for it):** the very first cold
`GOWORK=off go test ./...` on bytdb root FAILed; 8 subsequent runs —
including 3×plain + 3×race running *concurrently* as stress — were all
green, and the failing test's name was not captured (output had been
piped to `tail`). Happened while the toolchain compiled the whole dep
tree from scratch, so likely a timing-sensitive test under CPU
starvation.

> **Retired 2026-07-31 (later session):** deliberate cold repro —
> `go clean -cache` then `GOWORK=off go test ./...` with output fully
> captured via `tee` — all 6 packages green (root 15.4s, sql 9.6s),
> exit 0, no FAIL/panic. Did not reproduce under the same cold-compile
> conditions; consistent with the CPU-starvation guess. If it ever
> recurs, capture the full log (don't pipe to `tail`).

`occ-stage1` (btypedb) and `occ-stage2` (bytdb) are fully merged and
deletable; left in place.

## Part 2: reproducible head-to-head (user: "make it validatable, re-run so it's authentic")

Discovery: the repo **already had** a committed cross-DB harness —
`bench/` (July 8, commit `642c764`): single-client sequential latency
runner (`main.go`, bytdb embedded/pgwire vs Postgres/DuckDB/Redis)
feeding `docs/benchmarks.md`, with a **nested `go.work`**
(`use . .. ../pgwire`) so it benches the local checkout while the repo
root go.work is shadowed. Extended that module rather than adding a
second harness:

- **New files**: `bench/head2head_test.go` (doc + shared consts/k8),
  `bench/h2h_{bytdb,sqlite,bolt,badger,redis,duckdb}_test.go`
  (package main test files — `go test` compiles alongside main.go),
  `bench/head2head.sh` (starts/stops throwaway redis on **63799**,
  run.sh's port convention; runs
  `go test -run '^$' -bench 'Insert_|Heavy_' -cpu 8 -count 3`),
  `bench/head2head-results.txt` (committed raw output = provenance,
  mirroring the committed `results.json` precedent).
- **Deps**: badger v4.9.5, bbolt v1.5.0, mattn go-sqlite3 v1.14.49,
  modernc sqlite v1.55.0 added; go-duckdb v2.4.3 / go-redis v9.21.0
  upgraded to latest (fairness: competitors at current releases).
  btypedb resolves to published v0.7.0 (nested go.work has no
  ../btypedb entry); bytdb resolves to `..` — cloners bench exactly
  the code they cloned.
- **README**: validation block in the Concurrent writes section
  (`./bench/head2head.sh`, or the direct go test line; Redis rows
  skip cleanly without a server). `docs/benchmarks.md` cross-refs the
  concurrency comparison; `docs/concurrency.md` numbers refreshed.

### Authentic numbers (this run IS the README table; Apple M3, medians of 3)

Head-to-head (from the committed harness via `head2head.sh`):
Badger 2.7µs / 134µs · SQLite-mattn 5.1µs / 712µs · SQLite-modernc
5.9µs / 1,600µs · **bytdb OCC 11.1µs / 49µs** · bytdb single-writer
11.3µs / 154µs · Bolt 27.4µs / 82µs · Redis 36.8µs / 145µs · DuckDB
56.5µs / 8,368µs. (columns: single-row insert / 400 reads + insert)

Mode table re-run same day for internal consistency: insert 11.4 vs
11.0µs; heavy 146µs single-writer / **61µs SI (2.4×)** / 79µs
serializable (1.9×); btypedb light 18.3 vs 26.0µs (0.7×).

### Honesty adjustments baked into the README

- **btypedb contended-random-write row is bimodal**: 8 samples ranged
  126–268µs (conflict-retry thrash over 5 random keys in a 100k
  keyspace); README now reports the **median 231µs = 2.2×** with a
  footnote giving the spread and the ~4× good-run figure — the earlier
  3.6× claim was not reliably reproducible today and is gone.
- **±20% run-to-run spread on OCC-heavy shapes disclosed**: same shape
  measured 49µs (head-to-head) and 61µs (repo-root bench) the same
  day; README notes this next to the table instead of hiding it.

Commit `3982818` (15 files, +844/−84) pushed to main.

## State after this session

- Latest tags: **btypedb v0.7.0, bytdb v0.7.0, pgwire/v0.7.0** — the
  OCC concurrent-writes arc is fully released.
- bytdb main at `3982818`; btypedb main at `3692710` (= v0.7.0).
- Harness: `bench/head2head.sh` reproduces the README's competitive
  claims; `bench/run.sh` still reproduces docs/benchmarks.md (latency,
  needs Docker Postgres).
- Open threads: the uncaptured cold-run root-test flake; go-learn's
  pins still pre-OCC (bump + `yaegi extract` re-run only when a track
  needs the new API); occ branches deletable; `.cats-todo/` untracked
  by design.
