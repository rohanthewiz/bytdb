# bytdb production fine-tooth comb: bug fixes, fuzzing, doc sync

- **Session ID:** `1627e76e-61a6-42d6-af4a-612d99e7c675`
- **Date:** 2026-07-22
- **Scope:** Production-readiness review of the whole bytdb tree; close testing
  gaps; add fuzz targets; bring README + skill up to date.

## Goal

"Go over bytdb with a fine-tooth comb in preparation for production. Ensure
there are no testing gaps except where unfeasible. Add fuzz testing for extra
protection. Ensure the README and skill are fully up-to-date."

## Method

Baseline was already strong: root **89.6%** (cross-package), sql 87.2%, pgwire
87.1%, replicate 81.7%, s3 89.3%, tuple 97.3%; `go vet` clean; four existing
fuzz targets (sql `FuzzParse`, pgwire `FuzzMessageParse`, tuple
`FuzzTupleRoundTrip`/`FuzzTupleOrder`).

Five read-only review agents combed the subsystems in parallel — storage core,
SQL executor, parser/planner/params, pgwire, replication — each hunting panics,
resource/DoS issues, concurrency, durability, and testing gaps, with
`file:line` + concrete failure scenarios. Findings were then verified against
the actual code before any fix.

## Bugs found and FIXED (each with a regression test)

1. **Parser stack-overflow DoS (CRITICAL)** — `sql/parser.go`. The
   `maxParseDepth` guard lived only in `expression()`/`exprNot()`, never in the
   `selectStmt → selectCore → fromClause → tableRef → selectStmt` cycle. A
   deeply nested FROM subquery (~27 MB, under pgwire's 64 MiB message cap) drove
   an **uncatchable `fatal error: stack overflow`** that kills the whole
   server. Fix: `enter()/defer leave()` at the top of `selectStmt()` — every
   nested-SELECT path (derived table, scalar/IN/EXISTS subquery, CTE body,
   UNION arm) funnels through it. Test: `TestParseDepthLimit` gains a
   `fromDeep(100_000)` case; `FuzzParse` gains a FROM-nesting seed. (Verified
   live-reproduced by the review agent at n=1.5M pre-fix.)

2. **`SUM(expr)` silently drops float rows (HIGH, silent wrong answer)** —
   `sql/agg.go`. `intSum` is a *static* property (arg's declared type is int),
   but an expression arg can be float at runtime — e.g.
   `SUM(CASE WHEN c THEN 1 ELSE 1.5 END)`, whose type is the first branch.
   Float rows went to `sumF` only while `value()` returned the int-only `sumI`,
   so `sum(CASE…)` returned `1` instead of `7.0`. AVG (reads `sumF`) stayed
   right, which hid it. Fix: a runtime `sawFloat` flag; `value()` returns `sumF`
   (which includes the int rows too) whenever a float arrived, else the exact
   overflow-checked `sumI`. The window path reuses the same `accum`, so it is
   fixed for free. Tests: `sum_mixed_test.go` (plain / DISTINCT / OVER() / pure-int).

3. **`DropColumn` corrupts FK child ordinals (HIGH)** — `ddl.go`. Drop shifted
   `PKCols` and `Indexes[].Cols` for ordinals above the removed column but not
   `ForeignKeys[].Cols`. Dropping an unrelated *lower*-ordinal column left the
   FK naming the wrong column — or, when the FK child was the last ordinal, a
   stale ordinal == `len(Columns)` → index-out-of-range panic. Fix: shift
   `ForeignKeys[].Cols` with the same rule. Test:
   `alter_fk_ordinal_test.go` (middle + last-ordinal cases).

4. **pgwire binary `text[]` OOM → whole-server crash (HIGH)** —
   `pgwire/values.go`. `decodeTextArrayBinary` used the wire-controlled element
   count `n` (checked only `< 0`) directly as `make([]any, 0, n)`. A 20-byte
   param declaring `n = 0x7FFFFFFF` allocates ~34 GB → runtime OOM throw, which
   is **fatal and bypasses `run()`'s recover fence** — one hostile Bind downs
   the whole server. Reachable: `parse()` stores client-declared param OIDs
   verbatim, so a client forces OID 1009 + binary format. Fix: clamp the
   preallocation to `(len(raw)-20)/4`; the element loop already bounds every
   read. Test: `pgwire/array_bomb_test.go`.

5. **S3 client has no timeouts → replication hangs forever (HIGH)** —
   `replicate/s3/s3.go`. Defaulted to `http.DefaultClient` (no timeouts). A
   black-holed endpoint blocks a ship and, via `shipMu`, all replication,
   indefinitely — silently (stuck, not erroring). Fix: default to a client
   whose transport bounds dial / TLS-handshake / response-header time (NOT a
   blanket `Client.Timeout`, which would cap a large restore's body transfer).
   `Config.HTTPClient` still overrides. Test:
   `replicate/s3/timeout_test.go`.

6. **`WriteTxn` leaks the writer lock on panic (HIGH, liveness)** — `txn.go`.
   btypedb's `Update` rolls back on an error *return* but has no recover, so a
   panic in the closure unwound with the single-writer lock still held —
   wedging every future write process-wide (reads keep working, masking it).
   The doc promised "rolled back on error or panic," which was false. Fix:
   `WriteTxn` recovers, rolls back to release the lock, and re-panics
   (preserving the original failure). Test:
   `writetxn_panic_test.go` (asserts the next write does not hang, and the
   panicked txn's write was rolled back).

## Testing gaps closed (no bug — pure coverage)

- `types_format_test.go` — the untested `FormatTimestamp`/`FormatDate`/
  `FormatUUID` (incl. pre-epoch fraction wrap, non-16-byte UUID escape) + text
  round-trip identities.
- `engine_gaps_test.go` — `WithSyncNever` engine operates; `Txn.Sequence`
  snapshot resolver vs `Engine.Sequence`.
- `sql/ctx_activity_test.go` — `Session.ExecStmtCtx`, `Stmt.ExecCtx`
  (incl. cancelled-context error), `SetActivityProvider` feeding
  `pg_stat_activity`, and the `viewOID` branch of `pg_class` synthesis.

Root cross-package coverage 89.6% → **90.2%**; only `WithSyncNever` +
`Txn.Sequence` had been genuinely untested and are now covered. The remaining
sql 0%-functions are `boolExpr()/expr()/stmt()` interface-marker no-ops (not
meaningfully testable).

## New fuzz targets (all ran clean)

- **`sql.FuzzExec`** (`sql/exec_fuzz_test.go`) — the executor counterpart to
  `FuzzParse`: Parse→bind→coerce→plan→eval on any query the parser *accepts*
  (an executor panic escapes the SQL layer entirely = process fault). Tables
  seeded with **one row** so an N-way cross join stays 1 row (no combinatorial
  OOM) while every join/agg/window/order/distinct operator still runs; driven
  through `ExecCtx` with a deadline. **3.9M execs, 0 crashes.**
- Root `types_fuzz_test.go` — `FuzzTextArrayCanon` (hand-rolled `text[]`
  parser: no panic + idempotent canon), `FuzzJSONBCanon`, `FuzzTimestampRoundTrip`
  (bounded micros ↔ text), `FuzzUUIDRoundTrip`. All clean at ~12s each.
- Existing fuzzers re-run to confirm the fixes: `FuzzParse` 2.2M execs (FROM
  nesting no longer overflows), `FuzzMessageParse` 0.76M, tuple pair clean.

## Docs

- **README**: added `ALTER TABLE … OWNER TO` (no-op) to the DDL grammar block
  and the Statements roadmap line; added `BETWEEN` + `$n` in LIMIT/OFFSET to
  Statements; documented the S3 client's default bounded timeouts.
- **skill**: added `BETWEEN` and the `$n`-placeholder positions (incl.
  LIMIT/OFFSET/BETWEEN) to the dialect-coverage paragraph.

## Findings surfaced but NOT changed (need a design/behavior decision)

These are real but involve runtime-behavior changes, an on-disk-format change,
or a dependency edit — deliberately left for the maintainer:

- **btypedb `db.Update` root cause** — the WriteTxn fix is in-repo, but the
  one-shot `Insert`/`Update`/`Delete`/DDL paths call `btypedb.Update` directly,
  which still lacks a recover. A 3-line `defer recover→Rollback→re-panic` in
  btypedb's `Update` would fix all write paths at the source (go.work already
  links the local clone). Recommended.
- **pgwire DoS hardening** — no write deadline (a stalled reader inside a txn
  pins the writer lock); no idle read deadline *between* statements (only inside
  a txn) + `readBody` pre-allocates up to 64 MiB before reading + default
  `MaxConns=0` (unlimited) → slowloris; unbounded `stmts`/`portals` maps;
  duplicate named Parse silently overwrites (PG raises `42P05`).
- **replicate restore correctness** — `Restore` commits to the newest
  generation whose chain starts at 0 even if it is only partially shipped, over
  a complete older one → silent roll-backward after a compaction. There is no
  generation-complete marker (a manifest / sealed object would fix it). The
  README's "newest *complete* generation" wording is aspirational until then.
  Also: normal-tick ships use `context.Background()` (no deadline); prune can
  delete the last complete gen under rapid compaction; `Start`/`Close`
  lifecycle is unsynchronized.
- **executor completeness (not crashes)** — scalar subqueries with an
  expression/DISTINCT aggregate (`(SELECT sum(a*b) …)`, `(SELECT count(distinct
  c) …)`) are rejected; hash join / aggregation / ORDER BY / window buffering
  are unbounded (no spill — inherent to in-memory, worth a documented ceiling).
- **parser minor** — placeholders in FROM set-returning-function args and a
  bare `ORDER BY $n` are neither counted nor bound; empty-quoted-alias error
  swallowed.
- **storage minor** — catalog accessors (`Tables`/`Table`/`Sequences`/`Views`)
  swallow snapshot/decode errors (silent "none" vs. real failure);
  index-backfill / FK-validation materialize the whole table transiently.

## Verification

`go vet ./...` clean (both modules). Full suites green: root, sql, replicate,
replicate/s3, tuple, pgwire. `gofmt` clean. Fuzz sweep clean across all targets.

## Follow-ups

1. Decide on the btypedb `Update` recover (covers the non-WriteTxn write paths).
2. Decide the pgwire DoS-hardening defaults (deadlines, MaxConns, map caps).
3. Decide the replication generation-complete marker (fixes silent restore
   roll-backward; then the README wording becomes literally true).
