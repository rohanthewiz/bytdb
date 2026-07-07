# Session: Tier 1 reliability fixes (from the reliability-hardening plan)

- **Session ID**: `95877d66-7cfd-4bd7-9cd8-eb5f9476e153`
- **Date**: 2026-07-07
- **Plan**: `ai_docs/plans/2026-0707-reliability-hardening.md` (executed Tier 1 in full)
- **State at end**: all changes in the working tree, uncommitted; `go test -race -count=1 ./...` green in both modules (root + pgwire); `go vet` clean. 322 insertions / 11 deletions across 10 files + 2 new test files.

## What was done

Executed all five Tier-1 items from the reliability plan, each with regression tests.

### T1.1 — Parser stack-overflow DoS (CRITICAL) — `sql/parser.go`
- Added `maxParseDepth = 1000`, a `depth` field on `parser`, and `enter()`/`leave()` helpers.
- Guard placed in `expression()` — every nesting construct (parens, subqueries, CASE, function/list args, ANY/IN) re-enters through it — **plus** `exprNot()`, which self-recurses on `NOT` chains without passing through `expression()`.
- Bounding parse depth also bounds AST depth, protecting the later recursive walks (`lowerBool`, `walkExpr`, eval, planner).
- Test: `TestParseDepthLimit` (parser_test.go) — 100k levels of parens / `(select` / `not` / `case` / `-(` / `abs(` all return normal errors; 500 levels still parse.

### T1.2 — recover() per connection (CRITICAL) — `pgwire/conn.go`
- `conn.run()` is now fenced: panic → stack trace to stderr (`fmt`/`os`/`runtime/debug`, per shared-package convention), best-effort `ErrorResponse` XX000 ("internal error"), named-return error. The deferred `sess.Close()`/`nc.Close()` in `Serve` still roll back the transaction, so the global writer lock cannot leak.
- Test hook: `testPanicQuery atomic.Value` (atomic because connection goroutines read it; armed *before* the server goroutine starts for a happens-before edge under `-race`). Fires in `simpleQuery` only.
- Test: `pgwire/panic_test.go` `TestConnPanicIsolatesConnection` — in-package (the hook is unexported); real pgx over TCP; victim conn dies with an error, pre-existing conn keeps working, fresh conns accepted.

### T1.3 — DDL catalog rollback on commit failure (HIGH) — `engine.go`, `ddl.go`, `index.go`
- **Kept** the deliberate publish-inside-the-closure (the plan's warning: moving publish after commit re-opens the concurrent-insert stale-descriptor race).
- New `Engine.updateDDL(fn)` wraps `kv.Update` for all descriptor-writing DDL; new `Engine.restoreDesc(name, old)` rolls `e.tables` back under `e.mu` on any error (nil `old` deletes — not currently needed since `CreateTable` publishes after commit).
- Wired into: `CreateIndexCols`, `DropIndex`, `DropTable` (restores the deleted entry), `AddColumn`, `AddCheck`, and `writeDesc` (covers `DropColumn`, `DropCheck`). Restore is correct in both failure shapes (closure-error before publish = no-op; commit-error after publish = un-publish).
- **Fault injection**: `Engine.testCommitErr` field. When set, `updateDDL` runs the closure in a real writable tx then **rolls it back** — faithful simulation of a failed WAL append: the catalog publish happens, disk is untouched. (First attempt returned the error after a *successful* commit; data-survival assertions caught the infidelity.)
- Known residual window (documented on `restoreDesc`): a one-shot write beginning between the failed commit returning and the restore can still resolve the phantom descriptor. Full fix = catalog in the kv keyspace or a btypedb commit hook (plan Tier 2 / longer-term; would also fix T2.2).
- Tests: `ddl_failure_test.go` — 4 tests covering create-index (incl. "ScanIndex errors 'no such index', retry succeeds"), drop-index (index survives + later inserts maintain it), drop-table (rows survive), and all four ALTER paths.

### T1.4 — Checked int64 arithmetic (MEDIUM) — `sql/expr.go`
- `arith` integer branch: precondition checks for `+`/`-` (compare against `MaxInt64-ri` / `MinInt64-ri`), divide-back check for `*` with the explicit `-1 * MinInt64` case (division itself wraps there and cannot witness it), and `MinInt64 / -1` for `/`. All raise `bigint out of range` (Postgres message, SQLSTATE 22003 territory) via `errIntRange()`.
- Test: `TestExprIntOverflow` (expr_test.go) — 7 overflowing queries error; the exact extremes (`MaxInt64`, `MinInt64`, `MinInt64 % -1`, near-sqrt product) still compute.

### T1.5 — UPDATE CHECK vs uncoerced SET values (MEDIUM) — `sql/exec.go`
- **Honest finding**: the plan's repro is NOT reachable through the public API today. Verified empirically before fixing: `run()` binds params *first*, then `coerceLiterals` coerces `Update.Set` (literals AND bound params) before `execUpdate` runs. Quoted literals, string params, violation direction, and NULL all already behaved correctly.
- What's real: a hidden coupling — `execUpdate` trusted that earlier coercion, which resolves the *engine's* catalog (`d.lookup(d.e.Table)`) while the executor resolves the *transaction's* catalog (`tx.Table`).
- Fix (defense-in-depth per plan): `execUpdate` coerces SET values via `coerceLit` (idempotent) against the txn descriptor before building the check row; `tx.Update` still receives `s.Set` unchanged.
- Test: `TestUpdateCheckCoercion` (sql_test.go) — literal/param pass + violation directions, `SET price = NULL` passes `CHECK (price > 0)` (unknown ≠ violation), `'abc'` is "invalid input syntax" not a check violation.

## Key architecture facts learned (worth keeping)

- Statement flow: `Parse` → `run(st, args)` = `bindParams` → `coerceLiterals` (uses **engine** catalog) → exec (uses **txn** catalog via `tx.Table`). Params are literal-coerced because binding precedes coercion.
- Engine `coerce` (dml.go) REJECTS string→int/float/bool (no parsing); `coerceLit` (sql/coerce.go) does the Postgres-literal parsing. The SQL layer must coerce before the engine sees strings.
- Recursion cycles in the parser all pass through `expression()` except `exprNot` (self-recursive). `exprUnary` does NOT self-recurse (`- -1` is an error; signed numbers fold to literals). FROM has no subqueries (`tableRef` only parses table-valued function args).
- `checkRow` rejects only `triFalse` — NULL/unknown passes, per SQL. Was already correct.
- btypedb public API has no fault-injection option (`WithSyncPolicy`, `WithAutoCompact*`, `WithSweepInterval` only) — hence the `testCommitErr` + run-then-rollback simulation pattern.
- Test packages: `pgwire/server_test.go` is external (`pgwire_test`); in-package tests are needed to reach unexported hooks. `Engine.Scan(table)` takes no bounds args; `sql.New(engine)` constructs the DB.
- pgx v5: `Exec` with **no args** uses the simple protocol (that's where the panic hook fires).

## Next steps (plan's execution order)

1. **Fuzz targets** (Tier 3, items 1-3): `FuzzTupleRoundTrip` + `FuzzTupleOrder` (tuple codec), `FuzzParse` (would have caught T1.1), `FuzzMessageParse` (pgwire frames).
2. Property tests: planner equivalence (index scan ≡ seq-scan+filter+sort), `ScanRangeRev ≡ reverse(ScanRange)`.
3. Crash/fault tests at bytdb level (SyncNever + reopen-without-Close, torn tail, multi-index atomicity).
4. Concurrency tests under `-race` (read-Txn + CreateIndex for T2.2, idle-BEGIN timeout for T2.1).
5. Tier 2 robustness: idle-in-transaction timeout, atomic dual snapshot (T2.2), engine reentrancy guard, smaller items (Offset+Limit overflow, SIGINT handler, seq-key length guard, descriptor versioning).

Also unaddressed (noticed, out of Tier-1 scope): aggregate SUM accumulation may still wrap (only `arith` was checked); T1.3's residual window above.
