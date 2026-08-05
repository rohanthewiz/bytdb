# Considerations & Gotchas

bytdb makes deliberate trade-offs. This page is the honest list — what to know
before choosing it, and the sharp edges to avoid once you have.

## Sizing: the dataset lives in memory

The entire keyspace is memory-resident (copy-on-write B-trees); the file on
disk is a log, not a paging store. That is where the microsecond reads come
from, and it means **your working set must fit in RAM**. There is no buffer
pool, no spill-to-disk. Budget memory for the data plus a transient second
copy of hot tree nodes during writes/snapshots.

## Concurrency model

- **One writer at a time — by default.** Out of the box, a writable transaction
  holds the engine-wide writer lock from `BEGIN` to `COMMIT`/`ROLLBACK`; other
  writers queue behind it. Readers never wait — they run on lock-free
  snapshots. The upside of the queue: every transaction is serializable for
  free.
- **Concurrent writers are opt-in.** Opening with
  `bytdb.WithConcurrentWrites()` switches the engine to optimistic
  concurrency: writers build transactions in parallel against COW snapshots
  and validate at commit, and a losing commit surfaces as
  `bytdb.ErrTxConflict` (SQLSTATE **40001** on the wire). Isolation levels,
  retry responsibilities, and sequence semantics all change with it — read
  [Concurrency & Isolation](concurrency.md) before turning it on.
- **Keep write transactions short.** In the default mode a long-running write
  block stalls every other writer in the process (and every other wire
  connection) — which is why the wire server ships with a 5-minute
  idle-in-transaction timeout *on by default*. Under concurrent writes a
  long block doesn't stall anyone, but it widens its own conflict window.
- **Reentrancy trap (guarded):** calling a one-shot write, DDL, or a second
  writable transaction *from the goroutine that already holds the open write
  transaction* would deadlock the engine forever, because the writer lock is
  not reentrant. The engine detects this and returns an error telling you to
  use the `Txn` methods instead (`engine.go`). Structure code so
  everything inside `WriteTxn` goes through the `tx` it is given.
- **DDL cannot run inside a transaction block** — every schema change gets its
  own transaction.
- **Don't write from inside DB-level iteration.** `DB.Ascend`/`All`/etc. hold a
  read lock for the whole loop; a write inside the loop deadlocks. Transaction
  iterators (`Tx.Ascend`, engine `Scan`) are snapshot-based and lock-free —
  prefer them.

## Durability nuances

- **Visibility slightly precedes durability.** A commit becomes visible to new
  snapshots before its WAL fsync completes; the *writer* is not acknowledged
  until the bytes are on disk. A reader can therefore briefly observe a write
  that a power cut would lose. Consequence: a commit error means "durability
  unknown," not "not applied" — replay drops it on restart.
- **`SyncEverySecond` loses up to ~1 s** of acknowledged writes on power loss.
  `SyncAlways` (the default) never loses an acknowledged write, at the cost of
  a (group-committed) fsync per commit — on macOS that is `F_FULLFSYNC`,
  measured at ~4 ms; on Linux server hardware typically far cheaper. See
  [Benchmarks](benchmarks.md).
- **Sticky write errors.** After a failed WAL append the store refuses further
  writes (reads keep working) — fail-stop beats silently diverging from disk.
  Background sync/compaction/sweep errors surface on `Close`; check its error.
- **Replication is asynchronous.** The ship interval is the data-loss window
  for a lost disk; call `ShipNow` after critical commits if that matters. See
  [Replication & Backup](replication.md).

## SQL that is deliberately not there

Parse-time rejections with pointed errors:

| Not supported | Use instead / note |
|---|---|
| `ALTER TABLE ... ADD PRIMARY KEY / ADD UNIQUE` | Declare PK at create; unique via `CREATE UNIQUE INDEX` or the `UNIQUE` constraint sugar at create |
| `RIGHT` / `FULL` / `NATURAL` joins | Rewrite as `LEFT`/`INNER` |
| `WITH RECURSIVE`; `WITH` on INSERT/UPDATE/DELETE; data-modifying CTEs | CTEs are SELECT-only and non-recursive |
| Writable or materialized views; `WITH CHECK OPTION` | Views are read-only, materialized per statement |
| FK `MATCH FULL/PARTIAL`, `ON UPDATE CASCADE`, `ON DELETE/UPDATE SET NULL/DEFAULT` | `MATCH SIMPLE` with `NO ACTION`/`RESTRICT`/`ON DELETE CASCADE` only — rejected rather than silently weakened |
| `TRUNCATE ... CASCADE` | Name every referencing table: `TRUNCATE parent, child` (the error's HINT lists them) |
| `ON CONFLICT ON CONSTRAINT name`, index predicates in the conflict target | Name the columns: `ON CONFLICT (col, ...)` |
| Decimal/`NUMERIC` storage, time-of-day, `CHAR(n)`, interval | `numeric` casts to float; store money as int cents; `TIME` → use `TIMESTAMP` |
| Array types other than `TEXT[]`; multi-dimensional arrays; array `@>`/`&&`/`unnest`/subscripts | One-dimensional text arrays with `= ANY(col)`, `array_to_string`, `array_length` |
| `jsonb_set`, `jsonb_build_*`, `jsonb_agg`, `to_jsonb`, jsonpath (`@?` `@@`), `#-`, jsonb indexes | Read-modify-write with `\|\|` and `-`; the operator family covers reads |
| Expression column defaults beyond the clock markers | Constants plus `DEFAULT now()` / `current_date` only; `nextval(...)` as DEFAULT → use `SERIAL` or put `nextval('s')` in VALUES |
| `NULLS FIRST/LAST` in `CREATE INDEX` | NULL placement follows the key encoding: ascending columns put NULLs first, descending last |
| `EXPLAIN ANALYZE` | `EXPLAIN` only — execution is not instrumented |
| Aggregates, subqueries, or placeholders inside `CHECK` | — |
| `SELECT DISTINCT ON (...)` | Plain `DISTINCT` works; keep-first-per-group needs application code |
| `SELECT DISTINCT ... ORDER BY <expression>` | Order by output column names or positions — a sort key the projection dropped would decide which duplicate survives |
| `DROP SEQUENCE ... CASCADE`, `OWNED BY table.column` | Nothing can depend on a sequence yet; `OWNED BY NONE` parses |
| `COPY`, portal suspension | — |

Out-of-band query cancellation **works**: the server issues real
BackendKeyData secrets, honors `CancelRequest` (SQLSTATE 57014), and
`SET statement_timeout` bounds every statement — a runaway query no
longer wedges the global writer lock. `SET`/`RESET` parse; parameters
other than `statement_timeout`, `search_path`, and `time zone` are accepted
and ignored, and `SET LOCAL` degrades to session scope.

Deliberate Postgres *divergences* to know about:

- **`TIMESTAMP` and `TIMESTAMPTZ` are the same type.** Both store UTC
  instants (int64 microseconds) and present as `timestamptz` on the wire;
  the `WITH/WITHOUT TIME ZONE` distinction parses and folds away. Zone-less
  input text reads as UTC.
- **`JSON` is an alias for `JSONB`.** Documents canonicalize on write
  (compact, keys sorted) — Postgres's verbatim-text `json` behavior (key
  order, duplicate keys, whitespace) is not preserved.
- `DISTINCT` inside a window aggregate (`COUNT(DISTINCT x) OVER (...)`)
  **works** here — Postgres rejects it ("DISTINCT is not implemented for
  window functions"); DuckDB supports it the same way.
- Sequence allocation is **transactional** in the default single-writer mode:
  a `nextval` in a rolled-back transaction is not consumed, so the value is
  handed out again later. Postgres burns it. Code that relies on rolled-back
  values staying burned (rare, but it exists) will see reuse. Under
  `WithConcurrentWrites` allocation switches to Postgres's non-transactional
  behavior — a rollback leaves a gap
  ([Concurrency & Isolation](concurrency.md)).
- Identity columns use **MySQL's collision rule**: an explicit insert bumps
  the counter past itself, so draws after a restore never collide.
- `LIKE`'s `ESCAPE` accepts only the default backslash — any other escape
  character is rejected rather than silently misapplied.

Three semantic notes that surprise people (all Postgres-faithful):

- `x NOT IN (...)` with a NULL in the list matches **zero rows** — three-valued
  logic, same as Postgres.
- Text-vs-number comparison errors with `operator does not exist` rather than
  coercing; a *quoted untyped literal* against a typed column still adapts
  (`'42'` works where `42` does).
- `LAST_VALUE(v) OVER (ORDER BY k)` returns the current row's last *peer*,
  not the partition's last row — the default frame ends at the current row's
  peer group, exactly as in Postgres. The fix is Postgres' usual one, an
  explicit frame: `OVER (ORDER BY k RANGE BETWEEN UNBOUNDED PRECEDING AND
  UNBOUNDED FOLLOWING)`. `NTH_VALUE` is frame-limited the same way.

## Performance edges

- **Index your foreign-key columns.** FK enforcement goes through the
  ordinary planner: unindexed child FK columns turn every parent
  DELETE/UPDATE check into a child-table scan. Same advice as Postgres.
- **Views and CTEs materialize per statement.** The whole result set is
  computed and held in memory each time a statement names them — there is no
  predicate pushdown into a view and no index on a view. Joins against them
  are hash joins (linear), but a view over a huge table is still a huge
  materialization.
- **Hash joins need hash-compatible types.** An equijoin whose operands need
  dynamic coercion (text vs non-text) falls back to a nested loop; so do
  non-equality joins.

## Schema-change edges

- `ADD COLUMN ... NOT NULL` is allowed **only on an empty table** (there is no
  backfill).
- `DROP COLUMN` cannot drop a primary-key, indexed, or foreign-key column
  (drop the index/constraint first).
- Dropped-column data is not rewritten out of existing rows — it lingers under
  a retired column ID (invisible, but occupying space) until rows are updated
  or compaction-adjacent rewrites happen. Re-adding the same name gets a fresh
  ID; old data can never resurface.

## Two different "index" features

Easy to confuse:

- **Engine/SQL indexes** (`CREATE INDEX`, `e.CreateIndex`) are persisted in the
  catalog and maintained transactionally. These are the ones you want.
- **btypedb runtime indexes** (`kv.CreateIndex(name, compareFn)`) take arbitrary
  Go comparator functions and therefore **cannot be persisted** — they must be
  re-registered after every `Open`, and creation does a full O(n log n) build
  with writers paused.

## TTL behavior

- Expiry is exact at read time (an expired key reads as absent immediately) but
  **reclamation is lazy**: the sweeper runs every 500 ms and removes ≤512 keys
  per pass, so memory and log space trail expiry under heavy TTL churn.
- `Len` counts expired-but-unswept keys; `LiveLen` excludes them (and costs
  O(expired) to answer).

## Encryption caveats

- **Value-only scope**: the tuple-encoded primary key stays cleartext on
  disk. Aimed at surrogate-ID/UUID primary keys — PK column values are not
  protected. See [Encryption & Security](security.md).
- **No online key rotation or in-place conversion** — migrate by copying rows
  into a fresh database opened with the new key.
- **Lose the key, lose everything**: replicas and backups are ciphertext
  under the same key.

## Operational notes

- **The WAL grows until compaction.** Auto-compaction triggers at ≥32 MB *and*
  ≥100% growth since the last compaction (both tunable, or disable and call
  `Compact()` yourself). Startup replays the whole file; a huge uncompacted log
  means a slow open. Compaction also rolls the replication generation — see
  [Replication & Backup](replication.md).
- **One process per file.** There is no file locking for multi-process access;
  the wire server is the intended way to share a database across processes.
  Within one process, sharing is free: every `*sql.DB` the
  [`stdlib` driver](stdlib.md) opens on a path reuses the same engine.
- **Online backup**: `Engine.Backup(destPath)` writes a consistent
  point-in-time copy without blocking readers or writers;
  `Engine.BackupTo(w)` streams the same bytes. Restoring is just `Open` on
  the copy. Never copy the live file by hand while the process runs — a raw
  copy can catch a torn tail mid-append.
- **The log refuses to open past mid-file corruption.** A torn tail
  (crash mid-append) is repaired silently, as always; but if an intact
  record survives *after* a corrupt one (bitrot), `Open` fails with
  `ErrCorrupt` instead of silently discarding everything past the
  damage. `WithTruncateAtCorruption()` is the explicit salvage
  override. The file begins with a magic + format-version header;
  pre-header files still open and are upgraded on their next
  compaction.
- **`server_version` is advertised as `16.0 (bytdb)`** — version-sniffing
  clients will believe they talk to Postgres 16. Features they then assume may
  not exist (see the table above).
- **Auth is trust by default** — bind to loopback or a trusted network, or
  configure SCRAM-SHA-256 and TLS (both supported; see
  [Encryption & Security](security.md)).
