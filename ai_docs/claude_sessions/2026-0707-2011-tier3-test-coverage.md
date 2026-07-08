# Session: Tier 3 test coverage (from the reliability-hardening plan)

- **Session ID**: `5cc9bf04-6f3f-48dc-9be2-47684bbf2abd`
- **Date**: 2026-07-07
- **Plan**: `ai_docs/plans/2026-0707-reliability-hardening.md` (executed Tier 3 in full; Tiers 1-2 were the prior sessions)
- **State at end**: all Tier-3 work committed and pushed. `go test -race -count=1 ./...` green in both modules; `go vet` and `gofmt` clean.

## Bugs found by the new tests (both fixed this session)

### B1 — Remote OOM-kill via `$n` parameter numbers (found by FuzzMessageParse in ~30s)
- `select $100000000000` parses (int is 64-bit, `strconv.Atoi` accepts it); the statement's parameter count is the highest `$n`, and `Describe`/`describeStmt` do `make([]ColType, n)` → a ~terabyte allocation → fatal OOM, killing the whole server. The original fuzz run reported "fuzzing process hung or terminated unexpectedly: exit status 2" (worker OOM); the minimized crasher re-ran green in isolation because a single exec with free memory can survive — don't be fooled by that.
- **Fix**: `maxParamNumber = 65535` cap at the parser's single `tParam` site in `literal()` (`sql/parser.go`) — 65535 is the wire protocol's own ceiling (Bind carries the param count as int16). Regression: `TestParamNumberLimit` (`sql/parser_test.go`); the crasher input stays in `pgwire/testdata/fuzz/FuzzMessageParse/` as permanent corpus.

### B2 — Text column vs typed value silently parsed the stored text (plan item 19)
- `where text_col < 5` matched rows by whether each row's *stored value* parsed as a number (`'2'` matched, `'abc'` silently didn't) — `checkPred`'s symmetric adaptation coerced the column value to the literal's kind. Postgres errors.
- **Fix**: in `checkPred` (`sql/exec.go`), a string **column value** vs a non-string kind is now `operator does not exist: text <op> <kind>`. The other direction — string **literal** adapting to the column (quoted-literal semantics, load-bearing for pgx simple protocol and IN lists) — is deliberately kept. Full suite passed unchanged, so nothing depended on the old behavior. Covered in `TestCrossTypeComparison`.

## What was added

### Fuzz targets (plan items 1-3)
- `tuple/fuzz_test.go` — `FuzzTupleRoundTrip` (Decode of arbitrary bytes never panics; successfully decoded values re-encode canonically and byte-stably; crafted bytes can decode to NaN/-0.0, which the target accounts for) and `FuzzTupleOrder` (a `genTuple` bytecode maps fuzz bytes to encodable tuples incl. a boundary-value table; byte order ≡ `Compare`, desc reversed, desc round-trips). ~31M execs clean.
- `sql/fuzz_test.go` — `FuzzParse`: error-or-AST, never panic/stack-overflow; seeds cover the whole grammar plus malformed fragments and 2000-deep parens. Note: `go test -fuzz` can FAIL with "context deadline exceeded" right at fuzztime expiry — a harness flake, not a finding; rerun before diagnosing.
- `pgwire/fuzz_test.go` — `FuzzMessageParse`: drives the same per-message dispatch as `conn.run` but **below the recover() fence** (the fence would swallow panics and hide them from the fuzzer). Shared engine + per-exec session over a discard-backed writer. Found B1. 4.2M execs clean post-fix.

### Property tests (items 4-5)
- `scan_property_test.go` — `TestScanRevProperty`: rev ≡ reverse(fwd) over 300 random typed bounds × three key spaces (composite PK, asc index, mixed asc/desc index). The `toIncl` variant is modeled purely from forward scans: `fwd(from,to)` ∪ the leading equal-prefix run of `fwd(to,nil)`, filtered through `fwd(from,nil)` to keep order and exclude group rows below `from`. Element equality via `tuple.Compare` is direction-agnostic (desc inversion is a bijection), which is what makes one model serve all three spaces.
- `sql/plan_property_test.go` — `TestPlannerEquivalenceProperty`: twin tables (`ti` indexed with single/composite-desc/desc-leading indexes, `ts` bare), 400 random WHERE+ORDER BY queries; multiset equality + semantic ORDER BY check (ties are common, so never compare ordered results byte-for-byte), plus a floor (currently 107/400) on EXPLAIN-verified index paths so the test can't degrade into seq-vs-seq.

### Crash/fault tests (items 6-8; item 9 was already covered by `ddl_failure_test.go`)
- `crash_test.go` — kill is simulated by copying the live db file (btypedb takes **no file lock**; a quiescent-point copy is exactly the crash state). `TestKillWithoutClose` (SyncNever, 50 rows, table+index survive), `TestGarbageTailRecovery`, and `TestCrashPointAtomicity`: truncate the log at **every byte offset** past the schema region; at each cut Open must succeed, survivors must be an insertion-order prefix, and both secondary indexes must agree with the table exactly. Per-byte sweep ~8s; `-short` samples with step 7.

### Concurrency (item 13) and protocol corners (item 21)
- `pgwire/concurrent_test.go` — `TestConcurrentDDLandDML`: 3 writers (disjoint ids) + create/drop-index churn + 2 readers through real pgx conns under `-race`; settle checks index-vs-seq agreement.
- `pgwire/protocol_test.go` — empty/whitespace/`;` query buffers → EmptyQueryResponse, conn stays usable; explicit `Prepare`/Describe returns inferred ParamOIDs (int8=20, bool=16, text=25; float8=701 in Fields); cached-statement rebinds with new values → NULL → values.
- `pgwire/rowlimit_test.go` — raw-frame P/B/E(maxRows=2)/S through the internal dispatch, backend frames parsed from a buffer: all 5 rows, CommandComplete, never PortalSuspended. Pins "row limit ignored" as documented.

### Semantic gaps (items 14-20, 22) — `sql/semantic_gaps_test.go`
- UPDATE scanning the secondary index it mutates, both directions (rows moving ahead of and behind the scan window), with `explainsAs` asserting the Index Scan path (DML EXPLAIN puts the scan on a child line under "Update on t"/"Delete on t" — search all lines, not just the title).
- Three-valued `IN (1, NULL)` / `NOT IN (1, NULL)` / NULL probe / `<> ALL` over a NULL-bearing subquery.
- Bulk DELETE/UPDATE via index ranges with index-cleanliness checks (count via index == count via PK; re-insert into vacated range).
- LIMIT 0, OFFSET past end, window past end; `LIMIT $1`/`OFFSET $1` pinned as "placeholders are not supported" parse errors.
- Statement atomicity: multi-row UPDATE failing on a later row (unique collision with a value the same statement just wrote) rolls back entirely; multi-row INSERT likewise (unique and PK-dup variants).
- Cross-type matrix (int=2.0, f=2, i>f col-vs-col, text-vs-number errors per B2, quoted-literal direction still adapts).
- Chained LEFT JOIN NULL propagation + `count(col)` over padded rows; correlated-EXISTS DELETE; ADD COLUMN over an existing index (old rows NULL, new rows indexed).
- Item 22's DROP COLUMN×index and RIGHT/FULL-join rejection were already covered (`alter_test.go`, `parser_test.go`) — not duplicated.

## Facts learned (worth keeping)

- **UPDATE `SET col = expr` is unsupported** — the parser's SET clause takes `p.literal()` only. `SET val = val + 1` is a syntax error. Tests use literal SET or `$n`; a future milestone could add expressions.
- `execUpdate`/`execDelete` plan via `planScan` (so they DO take index paths) and **materialize matching rows before mutating** — that's what makes item 14 pass. Each statement runs inside one `d.write` closure → statement-level atomicity comes from the kv transaction rollback.
- `checkPred` is the single comparison funnel for both `Pred` leaves and `ExCmp` expressions — one fix site for B2.
- Go fuzzing: workers are separate processes; per-worker OOM shows as "hung or terminated unexpectedly: exit status 2"; the saved crasher may pass in isolation (see B1). Piping fuzz output through `tail` loses the progress lines — capture to a file.
- The fuzz corpus files under `testdata/fuzz/<Target>/` are regression tests forever — commit them.

## Next steps (plan's remaining items)

1. Longer-term architectural: move the catalog into the kv keyspace (closes T1.3's residual window, retires the seqlock).
2. Aggregate SUM overflow (noted in Tier 1, still open).
3. Feature gap: expressions in UPDATE SET (`SET age = age + 1`), surfaced this session.
4. Optional: periodic longer fuzz runs (`-fuzztime=10m`) for the four targets; consider a CI job.
