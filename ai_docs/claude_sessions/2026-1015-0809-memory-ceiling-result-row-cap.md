# Memory ceiling and result row cap: refuse work instead of OOM

- **Session:** `b88fca3d-3722-4e10-81a1-3a44e67df04c`
- **Date:** 2026-10-15
- **Scope:** The user asked how to make sure regular use doesn't run the
  process out of memory. An analysis of the existing guardrails found
  three gaps. The user chose to build two of them (1: a result size cap;
  3: refusing work when the heap is high) and to record the other (2: a
  cap on write-transaction size) on the next-list as N-028. Feature
  commit `f62d362`. **Not released:** the latest tags are still
  v0.21.2 / pgwire/v0.21.2.

## 1. The analysis (what already existed)

- `Engine.Stats()` and `/metrics` report HeapLive, HeapGoal, MemLimit,
  HeapFraction and GC CPU time. `DefaultBackfillLimit` (1M rows) refuses
  ADD COLUMN DEFAULT and table rewrites. pgwire has `MaxConns` (100),
  per-connection caps on prepared statements and portals (54000), a
  64 MiB frame cap, `statement_timeout` (scans poll ctx every 256 rows)
  and `-idle-tx-timeout`.
- **The gaps:**
  1. Every SELECT result is fully materialized (`sql.Result.Rows
     [][]any`); pgwire doesn't stream, and nothing caps the size.
  2. Outside ALTER, nothing caps how much one write transaction stages.
  3. No admission control looks at the heap.

## 2. Memory ceiling (`memguard.go`)

- **API:** `Engine.SetMemoryCeiling(n)`: `n > 0` is an explicit byte
  ceiling, `0` is automatic (the default; 90% of GOMEMLIMIT, re-derived on
  every reading, inactive without GOMEMLIMIT), and `n < 0` disables it.
  Also `MemoryCeiling()` and `CheckMemory()` (public, so embedders can use
  it), plus the sentinel `ErrOutOfMemory`, which pgwire maps to 53200.
  Zero meaning *automatic* departs from `SetBackfillLimit`'s "0 disables".
  That is deliberate and documented, because the right ceiling depends on
  a limit that lives outside the engine.
- **Metric:** `/gc/heap/live:bytes` (the heap the last GC marked
  reachable), not HeapLive (`/memory/classes/heap/objects:bytes`).
  HeapLive includes uncollected garbage and swings between 1x and 2x the
  live data under GOGC=100, so a ceiling on it would refuse writes on a
  half-empty budget. The marked figure updates only at GC boundaries, but
  near GOMEMLIMIT the runtime collects continuously.
- **Cost:** the verdict is an immutable snapshot behind an
  `atomic.Pointer`, trusted for 2 ms on a monotonic clock. A check costs
  about 20 ns. A bare in-transaction insert went from about 737 to about
  745 ns/op (1–3%).
- **Recovery:** a refusing guard stops the allocations that would trigger
  a GC, so the stale "over" reading could stick after the operator
  deletes data. When the guard is over, it starts a background
  `runtime.GC()` (at most one at a time, at most once a second) and then
  resamples.
- **Where it's checked:** `insertRow` (every insert path), `Engine.Update`,
  `Txn.UpdateReturning`, `CreateIndexCols`, `AddColumn` (only with a
  DEFAULT or identity; a bare ADD COLUMN is allowed), `AlterColumnType`,
  `SetPrimaryKey`. The SQL row budget also polls it every 1024
  accumulated rows.
- **Never refused:** DELETE, DROP and TRUNCATE. In the SQL layer,
  `DB.freeing` is set by dispatch on a copy for `*Delete`, so the
  DELETE's views, subquery key sets and RETURNING rows skip the ceiling
  poll but keep the row cap. Reads and streaming aggregates never reach a
  poll.
- **Stats:** gains `HeapMarked` and `MemCeiling` (the effective value),
  plus the gauges `bytdb_heap_marked_bytes` and `bytdb_mem_ceiling_bytes`.

## 3. Result row cap (`sql/rowbudget.go`)

- **API:** `Engine.SetMaxResultRows(n)` / `MaxResultRows()`, default
  `DefaultMaxResultRows = 10_000_000` (about 1.5–3 GB of `[][]any` for
  typical rows); `0` or less disables it. The knob lives on the Engine so
  every frontend sharing it is bound by one setting.
- `rowBudget.check(len)` is per materialization, not per statement. The
  memory ceiling bounds the sum. Charged sites:
  - the `execSelect` collector (also covers CTE, view and derived-table
    materializations)
  - the window base rows (`execSelectWindow`)
  - new groups in `aggScanGroups` (groups, not scanned rows, so COUNT(*)
    over any number of rows streams)
  - hash-join build tables (scan branch only; vtab rows were charged when
    materialized)
  - IN / ANY and `ARRAY(subquery)` value sets (`expr.go`)
  - UNION concatenation
  - RETURNING in INSERT, ON CONFLICT, UPDATE and DELETE
- **Not charged:** UPDATE's pre-write row materialization and DELETE's
  pk list. Those belong to N-028.
- **Error:** "…than the result row limit allows; narrow it with WHERE,
  add a LIMIT, or paginate — or raise the limit (Engine.SetMaxResultRows;
  bytdbd -max-result-rows)". pgwire maps it to 54000 on the phrase
  "result row limit".
- **Known edge (documented):** an unindexed `ORDER BY x LIMIT n` sorts
  every matching row, so it counts all of them against the cap.

## 4. bytdbd, pgwire, docs

- **Flags:** `-max-result-rows N` and `-memory-ceiling auto|off|<size>`.
  Sizes use GOMEMLIMIT's suffixes (B/KiB/MiB/GiB/TiB) and are parsed
  before the engine opens (`parseMemCeiling`). "6GB" is rejected on
  purpose.
- **`pgwire/errors.go` sqlstate:** "result row limit" maps to 54000 and
  "out of memory" to 53200.
- **Docs:** the README's Monitoring memory section (both guards and a
  recommended production setup), the README's bytdbd paragraph,
  `docs/gotchas.md` (the Sizing edges), and `docs/security.md`'s flag
  table.

## 5. Verification

- **Tests:** `memguard_test.go` fakes heap readings through
  `memGuard.sample`, because a real tiny GOMEMLIMIT would make the test
  binary thrash. It covers refusals, exemptions, automatic mode, recovery
  once the cache expires, and Stats. `sql/rowbudget_test.go` covers each
  charged site with queries whose outer result stays under the cap, so
  each case isolates its own site. The hash case was confirmed by
  EXPLAIN, and a mutation (removing the hash-build charge) was caught.
  The SQL test also covers a 1-byte ceiling with the DELETE exemption.
  `pgwire/errors_state_test.go` adds a mapping table and an end-to-end
  render of the real errors. `cmd/bytdbd/memceiling_test.go` covers the
  flag parser.
- **Suites:** `go vet` and the full suites pass for both modules.
- **/verify (bytdbd + pgx, simple and extended protocol):**
  - With the cap at 5: 10 rows → 54000, LIMIT 5 and count(*) are fine,
    a 6-row INSERT … RETURNING → 54000, and the connection stays usable.
  - With `-memory-ceiling 1B`: a large SELECT, INSERT, UPDATE and an
    INSERT inside a BEGIN block → 53200; a point read and count(*) are
    fine; DELETE of 20,000 rows with RETURNING succeeds.
- **Workspace note:** `go build` through `go.work` fails because the
  local `../btypedb` clone is behind the pinned v0.9.0 (missing
  `ErrLocked`, `LogStats`, `AcquireLock`). This predates the session.
  Everything was run with `GOWORK=off`. Pulling `../btypedb` fixes it.

## Next

Closed: None. Declined: None. Raised: N-028.
Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
