# N-047: catalog rows memoized per catalog version; identity counters batched in the Txn

Session: `04ffb06f-c5f4-4ef1-8fa8-c7a9dae66e84`. Started from `/clear`.
The user pasted N-047, the last deferred Phase 7 piece of the deep-look
plan (`ai_docs/todo/2026-1109-deep-look-remediation-plan.md`, D-47 second
half and D-45's identity batching). Both were built, measured, tested,
mutation-checked and verified over the wire, then wrapped with `/sw`.
No release was cut.

## Catalog: D-47 second half

### Premise check

Every system-table builder was read against what it depends on.

- **Catalog-only.** Most builders read only `d.userDescs()`
  (`Engine.TableShared`), `d.e.Views()`, `d.e.Sequences()` option
  fields, and `viewColumns` (derived from those). All of that changes only
  through DDL.
- **DDL bumps the version.** DDL is engine-only (no Txn DDL) and runs
  through `updateDDL`, which bumps `catalogVer` after the commit. That
  includes CREATE/ALTER/DROP SEQUENCE and views.
- **Three live tables.** `pg_stat_activity` reads the serving layer's
  provider. `pg_settings` reads the session's SET variables.
  `pg_sequences.last_value` moves with `nextval`, which is not DDL.
  `pg_sequence` and `information_schema.sequences` show only options, so
  they are cacheable. The bare-sequence virtual table
  (`SELECT last_value FROM seq`) does not go through `sysTables` at all.

### Memo (`sql/catmemo.go`)

- `catalogMemo{ver, rows map[*sysTableDef][][]any}` hangs off the DB from
  `New` (`DB.cat`) and rides every copy, so all sessions share it.
- `DB.sysRows` reads `CatalogVersion()` before building, the protocol
  `Engine.catalogVer` documents. Rows stored under v are at least as new
  as v, never older.
- `put` discards a build for an older version, so a slow build cannot
  roll the memo back. `get` drops a superseded version's map, so stale
  rows free on the first lookup after DDL.
- Opt-in per table: `sysTableDef.byVersion`. 17 catalog-derived tables
  are marked. The live three and the cheap constant tables stay unmarked.
  Opt-in was chosen because a forgotten flag only costs a rebuild, while
  a wrong one would serve stale rows.

### Executor follow-ups (`sql/join.go`)

The memo alone took `DLCatalogColumns200` from 194 to 88 µs. A profile
then showed 98% of the remaining allocation was `stepNext` copying every
virtual row before the WHERE. GC dominated CPU.

- **First-step pass-through.** `stepNext` now passes a virtual row
  through uncopied at `k == 0`, as D-44 already did for stored rows.
  - The `k == 0` bound matters: a zero-width first table also gives
    `len(partial) == 0` at k=1.
  - `joinRun.borrowed` marks a single-step plan over a virtual table, and
    `rec` clones a surviving row at `yield`.
  - That clone is load-bearing in principle. `execSelect`'s yield does
    `vals = append(vals, v)` for expression columns, which would write
    into a shared row that has spare capacity.
  - Today every builder caps its rows (`slab[start:len:len]` and
    literals), which is why the SQL-level mutation test could not fail.
    `TestBorrowedVirtualRowCopiedAtYield` pins the mechanism directly
    with a capacity-4 row.
- **Static-conjunct prefilter (`joinRun.virtualRows`).** `prepareFrom`
  always computed `step.static` for virtual steps, but execution ignored
  it.
  - Rows are now filtered once per join run, before the hash build or
    nested loop.
  - Each candidate is evaluated in a scratch row of width
    `off+len(cols)`, because binds use combined-row ordinals.
  - `kept` is allocated only at the first rejection, so an all-pass
    filter such as `attnum > 0` copies nothing.
  - The prefilter is skipped when `borrowed`, where the WHERE right after
    applies the same conjuncts.
  - It is safe because statics are conjuncts the WHERE (or a LEFT JOIN
    step's ON) applies anyway. WHERE conjuncts never become statics of a
    LEFT JOIN step (`avail` excludes them).
  - This replaced the item's other option, "push `table_name = 'x'` into
    the builders". It is general: CTEs, views and derived tables benefit
    too.

## Identity: D-45 batching

btypedb's `Tx.Set` appends a sealed WAL record to `tx.pending` per call
(read in `btypedb/tx.go`), so every default-mode draw logged a counter
record. A 500-row batch logged 28.5 KB of dead counter records, 57 bytes
per row.

### `idCounters` (`identity.go`) and `Txn.ids`

- Keyed by `(tableID, colID)`, so no key encoding per draw. Each entry
  holds `{key, next, dirty}`.
- `draw` and `bump` mirror `nextFromCounter` and `bumpCounterTo` exactly:
  - An absent counter loads as 1. A bump to 1 or below stays a no-op.
  - The error texts are the same: corrupt, "sequence exhausted".
  - `ErrTxNotWritable` comes at the same point. `draw` always checks;
    `bump` checks only when it would write.

### Lifecycle (each rule mutation-checked; removing it fails a test)

- **Flush points.** The cache is flushed in `WriteTxn` after `fn`
  returns nil (inside the kv txn), in `Txn.Commit` before the commit, and
  in `Savepoint` before the mark. A flush failure in Commit rolls back:
  committing rows without their counter would let later draws collide.
- **Savepoint rollback.** `RollbackTo` resets the cache after the kv
  rewind. The next draw reloads from the mark, which the Savepoint flush
  filled.
- **Direct counter writes.** `RestartIdentity` calls `forget` before its
  direct Set, and `Truncate(restart)` calls `forgetTable` before its
  DeleteRange. A stale entry flushed later would undo the restart.
- **Peek.** `PeekIdentity` returns a dirty entry before reading the
  stored record.
- **Unchanged paths.** One-shot `Engine.Insert/Update` pass `ids == nil`
  and keep the direct path (one draw per transaction, nothing to batch).
  OCC is unchanged, since `fillIdentity` checks `e.occ` first.

`fillIdentity`'s three-way split moved into `bumpIdentity` for bumps.
`identityName` (the `table.column` error text) is built only on the
paths that need it: the error path, and OCC, which threads it through
its allocator.

The SQL layer takes no per-statement savepoints (only explicit
`SAVEPOINT`), so a whole BEGIN…COMMIT block batches.

## Measurements

Apple M3. The baseline was captured before any edit, n=6, and medians
were computed with awk; fetching benchstat via `go run …@latest` was
denied by auto mode.

| Benchmark | Before | After |
|---|---|---|
| `DLCatalogColumns200` | 200 µs, 723 KB, 6685 allocs | 21 µs, 5 KB, 60 allocs |
| `DLCatalogAttributes200` | 674 µs, 1.9 MB, 17390 allocs | 133 µs, 85 KB, 6233 allocs |
| `DLInsertIdentity` (new, 100 rows/txn, per row) | 1248 ns, 1174 B, 20 allocs | 871 ns, 729 B, 12 allocs |
| `DLWriteTxnOneInsert` | 7.9 µs | 7.5 µs (noise) |

What remains in the attributes query is mostly `hashJoinKey`'s
`tuple.Encode` per probe.

## Tests and verification

- **`sql/catmemo_test.go`:**
  - memo reuse and invalidation (pointer identity)
  - each DDL kind reflected
  - live tables stay live (`pg_sequences`, `pg_settings` per session,
    `pg_stat_activity`)
  - caller mutation of results cannot reach the memo
  - the yield clone with a spare-capacity row
  - prefilter semantics: hash vs nested loop, LEFT JOIN ON static vs the
    same conjunct in WHERE, a filter that rejects everything
  - 4 sessions reading during 20 CREATE TABLEs under `-race`
- **`identity_batch_test.go`:**
  - commit through both paths, plus reopen
  - rollback and gaplessness
  - nested savepoints and RELEASE
  - explicit and UPDATE bumps
  - restarts, including across a savepoint rollback, and truncate with
    and without restart
  - peek
  - read-only draws and bumps
  - `TestIdentityBatchLogsCounterOnce` uses `Stats().LogBytes`; the
    old-behavior mutant fails with 57 extra bytes per row
- **Suites:**
  - `go test ./...` passes (root and pgwire modules), and the root and
    `sql` packages pass under `-race`.
  - difftest passes: 16 corpus files against Postgres 16 in the existing
    `bytdb-numeric-pg` container on 55432 (password `pg`, trust auth).
- **`/verify` over pgwire with pgx v5.10.0, 18 checks:**
  - introspection over the extended and simple protocols
  - DDL on one connection seen by another's next introspection
  - `pg_sequences` live
  - multi-row identity INSERT in a block with SAVEPOINT / ROLLBACK TO
    (ids 4,5 redrawn)
  - rolled-back block
  - `setval('users_id_seq', 100)` in a block, then draws 101, 102
  - after a server restart, the next draw was 103

## Gotchas met

- bytdb refuses `GENERATED ALWAYS AS IDENTITY` (only BY DEFAULT); the
  verify fixture had to use BY DEFAULT.
- `Engine.Stats()` returns a single value, not `(Stats, error)`.
- Starting the catalog baseline while editing `sql` would have
  compiled the edits into the baseline. The baseline run finished before
  any `sql` edit.

## Next

Closed: N-047. Declined: None. Raised: None. Deferred: None.
Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
