# Session: Catalog into the kv keyspace (+ SUM overflow, UPDATE SET expressions)

- **Session ID**: `da3e7a49-b312-47b5-8f0d-100b85420201`
- **Date**: 2026-07-08
- **Prior context**: loaded `2026-0707-2011-tier3-test-coverage.md`; executed its next-step items 2 and 3, then item 1 (the architectural milestone).
- **State at end**: all work committed and pushed. `go test -race -count=1 ./...` green in both modules; `go vet` and `gofmt` clean; 45s `FuzzParse` (5.9M execs) clean over the parser change.

## Part 1 — SUM overflow + UPDATE SET expressions (commit `1ceb50d`)

### Checked integer SUM (`sql/agg.go`)
- `accum.add`'s int64 branch bound-checks the running total (same test as scalar `+` from T1.4) and raises `bigint out of range` instead of wrapping. Guarded to `fn == AggSum && intSum` — AVG reads only the float accumulator (Postgres: `avg(bigint)` is numeric), so big-int AVG still works. Window `SUM(...) OVER` shares `accum.add`, so it's covered for free.

### Expressions in UPDATE SET
- `SET age = age + 1`, CASE, functions, `||`, casts, scalar subqueries — evaluated per matched row against **pre-update** values (so `SET a = b, b = a` swaps). Design: `Update.Set` (literals/`$n`, coerce-once fast path, unchanged) + new `Update.SetEx map[string]Expr` for everything else; a column is in at most one map. Parser folds bare `ExLit` back into `Set`.
- Wired through: `parser.go` (rejects aggregates/windows in SET via `findAgg`/new `hasWindowExpr`), `params.go` (numParams + bindExpr over SetEx), `describe.go` (a `$n` inside a SET expression infers as the **target column's type** — without this pgx encodes the Bind arg as text and arith fails; note() keeps first inference so cross-referenced params are unaffected), `exec.go` (per-row `evalEx` + `coerceLit`, merged map to `tx.Update`; CHECK judges evaluated values).
- Tests: `sql/update_expr_test.go` (incl. swap semantics, index-self-mutation with a real `age = age + 10`, CHECK/error paths, session param rebinds, SUM overflow both directions + exact-boundary + window path), `pgwire/update_expr_test.go` (pgx extended protocol, rerun for cached-statement rebind), Describe cases.

## Part 2 — Catalog into the kv keyspace (this commit)

**The insight**: descriptors were *already persisted* in kv (`descKey(name) → JSON`); the milestone was making kv the **authoritative read source**. Every transaction now resolves descriptors lazily from *its own snapshot*, so schema view ≡ data view by construction. ~100 net lines deleted.

### What was removed (engine.go)
- `e.tables` in-memory catalog, `e.mu`, `ddlMu`, the whole seqlock (`catVer`, `catStable`, `publishDesc`, `publishDescPending`, `stabilizeCatalog`, `restoreDesc`, `catalogSnapshot`, `stableCatalogSnapshot`, `catalogVersion`) and `ReadTxn`/`readSnapshot`'s version-check retry loops.

### What replaced it
- **Parse cache** (`descCache map[name]{blob, desc}` under `cacheMu`): a hit counts only when the snapshot's stored bytes equal the cached ones, so stale entries can't leak across DDL — at worst a reader over an old snapshot re-parses. Name-keyed (bounded by table count) rather than blob-keyed (unbounded across DDL versions). `loadCatalog` still validates everything at Open (collecting all bad descriptors into one error) and warms the cache; new `parseDesc` funnels unmarshal + future-version refusal.
- **`tableFromView(v, name)`** (nil,nil when absent) / **`descFromView`** (errors "no such table") resolve from any kv view.
- **`Txn`**: `{tx, e, releaseW, descs}` — `descs` memoizes per-transaction (including nil for absent; the snapshot can't change underneath). `Txn.Table` → memo → `tableFromView`. `releaseW` replaced the `e != nil` writable-marker since `e` is now always set.
- **DDL**: every schema change resolves + validates + mutates the descriptor *inside its own kv transaction* via new **`alterDesc(table, mutate)`** (mutate returning nil,nil = no-op commit, used by DropCheck's absent case). `CreateTable`'s existence check moved into the txn (`tx.Contains(descKey)`); `writeDescIn` is now just marshal+Set — commit IS the publish. `updateDDL` keeps the reentrancy check + `testCommitErr` harness (run closure, roll back, return the injected error — which now faithfully leaves *nothing* to restore).
- **One-shot reads** (`Get`/`ScanRange`/`ScanRangeRev`) now open a snapshot (`kv.View`/`readSnapshot`) instead of `e.desc` + live `e.kv` — the same consistency ScanIndex already had. `Engine.Table`/`Tables()` read via `kv.View`; `Tables()` relies on tuple key order = name order (no re-sort).

### Why concurrent DDL stays correct without ddlMu
kv's writer lock serializes DDL; because each DDL *resolves the descriptor inside its transaction*, each ALTER builds on the committed descriptor before it. The old code resolved outside the txn and would lose updates without ddlMu. Pinned by `TestConcurrentAlterNoLostUpdate` (16 racing AddColumns, distinct IDs) and `TestConcurrentCreateTableSameName` (exactly one winner) in new `catalog_kv_test.go`.

### What this closes
- **T1.3 residual window** — gone entirely: a rolled-back DDL leaves nothing to un-publish; no instant where a write can resolve a phantom descriptor (`TestFailedDDLLeavesNoPhantomForWrites`).
- **T2.2** — subsumed: consistency is structural, not seqlock-patched. `snapshot_test.go` (4 readers × 500 iters vs create/drop churn) kept as the regression guard; header comments updated in it and `ddl_failure_test.go`.
- For *real* (non-simulated) commit failures, btypedb's visible-before-fsync semantics still apply — but catalog and data are now visible-or-not **together**, so "durability unknown" (already documented on WriteTxn/Insert) is the only remaining caveat; there is no catalog/data divergence mode left.

## Facts learned (worth keeping)

- btypedb read `Begin(false)`/`View` takes only `db.mu` for an O(1) state copy — **never blocks on writerMu** — so opening a snapshot (e.g. `Engine.Table`) is safe from any context, including a goroutine holding the write txn (`sql/expr.go` regclass lookup does this).
- Parsed `*TableDesc` values are immutable (DDL clones), so one pointer can be shared across transactions; nothing compares descriptor pointers.
- `sql/expr.go:689` (regclass cast) resolves `e.Table` against *current committed* state, not the enclosing txn's snapshot — pre-existing wrinkle, unchanged.
- Test placement gotcha: running `go test .` from `pgwire/` silently matches zero root-module tests ("no tests to run" + PASS) — cd matters with the two-module layout.

## Next steps

1. Optional cleanups now unlocked: `UPDATE SET` expressions could simplify `TestUpdateViaIndexItMutates` (still uses literals); tier-3 session doc's "UPDATE SET expressions unsupported" note is stale.
2. Remaining plan follow-on: periodic longer fuzz runs (`-fuzztime=10m` × 4 targets) / CI wiring.
3. Possible future: `Engine.Tables()`+`Table(name)` pairs in syscat take two snapshots; a single-snapshot listing API would make `pg_catalog` views fully consistent (cosmetic — nil-checked today).
