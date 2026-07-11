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

- **One writer at a time.** A writable transaction holds the engine-wide writer
  lock from `BEGIN` to `COMMIT`/`ROLLBACK`; other writers queue behind it.
  Readers never wait — they run on lock-free snapshots.
- **Keep write transactions short.** A long-running write block stalls every
  other writer in the process (and every other wire connection).
- **Reentrancy trap (guarded):** calling a one-shot write, DDL, or a second
  writable transaction *from the goroutine that already holds the open write
  transaction* would deadlock the engine forever, because the writer lock is
  not reentrant. The engine detects this and returns an error telling you to
  use the `Txn` methods instead (`engine.go:253-261`). Structure code so
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

## SQL that is deliberately not there

Parse-time rejections with pointed errors:

| Not supported | Use instead / note |
|---|---|
| `DEFAULT now()` / expression defaults | Constant defaults work; store epoch ints, set in the app |
| `UNIQUE` column constraint | `CREATE UNIQUE INDEX` |
| `REFERENCES` / foreign keys | Enforce in application code |
| `ALTER TABLE ... ADD PRIMARY KEY / ADD UNIQUE` | Declare PK at create; unique via index |
| `RIGHT` / `FULL` / `NATURAL` joins | Rewrite as `LEFT`/`INNER` |
| CTEs (`WITH`), `TRUNCATE`, views | — |
| `ON CONFLICT ON CONSTRAINT name`, index predicates in the conflict target | Name the columns: `ON CONFLICT (col, ...)` |
| Date/time, decimal, uuid, json, array column types | Store as `INT` (epoch), `TEXT`, or `BYTEA` |
| `$n` placeholders in `LIMIT`/`OFFSET` | Literal counts only |
| `EXPLAIN ANALYZE` | `EXPLAIN` only — execution is not instrumented |
| Aggregates, subqueries, or placeholders inside `CHECK` | — |
| `nextval(...)` as a column `DEFAULT` | Constant defaults only; use an identity column (`SERIAL`) or put `nextval('s')` in the VALUES list |
| `SELECT DISTINCT ON (...)` | Plain `DISTINCT` works; keep-first-per-group needs application code |
| `SELECT DISTINCT ... ORDER BY <expression>` | Order by output column names or positions — a sort key the projection dropped would decide which duplicate survives |
| `DROP SEQUENCE ... CASCADE`, `OWNED BY table.column` | Nothing can depend on a sequence yet; `OWNED BY NONE` parses |
| `COPY`, out-of-band query cancellation, SSL on the wire | — |

Two deliberate Postgres *divergences* around sequences and windows:

- `DISTINCT` inside a window aggregate (`COUNT(DISTINCT x) OVER (...)`)
  **works** here — Postgres rejects it ("DISTINCT is not implemented for
  window functions"); DuckDB supports it the same way.
- Sequence allocation is **transactional**: a `nextval` in a rolled-back
  transaction is not consumed, so the value is handed out again later.
  Postgres burns it. Code that relies on rolled-back values staying burned
  (rare, but it exists) will see reuse.

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

## Schema-change edges

- `ADD COLUMN ... NOT NULL` is allowed **only on an empty table** (there is no
  `DEFAULT` to backfill with).
- `DROP COLUMN` cannot drop a primary-key or indexed column (drop the index
  first).
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

## Operational notes

- **The WAL grows until compaction.** Auto-compaction triggers at ≥32 MB *and*
  ≥100% growth since the last compaction (both tunable, or disable and call
  `Compact()` yourself). Startup replays the whole file; a huge uncompacted log
  means a slow open.
- **One process per file.** There is no file locking for multi-process access;
  the wire server is the intended way to share a database.
- **`server_version` is advertised as `16.0 (bytdb)`** — version-sniffing
  clients will believe they talk to Postgres 16. Features they then assume may
  not exist (see the table above).
- **Auth is trust** on the wire server; bind it to loopback or a trusted
  network. SSL is declined.
