# Production-Readiness Audit — bytdb / btypedb

Date: 2026-07-11 (post Milestone 29). Four parallel audits: durability & recovery,
concurrency & transactions, pgwire security/operability, SQL layer & testing.
btypedb audited at v0.4.0.

## Verdict

bytdb is well built for its stated niche (embedded, in-process, data fits in RAM,
trusted callers — the SQLite niche) with genuinely strong correctness testing:
83–91% coverage per package, four fuzz targets, property tests, exhaustive
crash-point atomicity tests, and real fsync-modeling power-loss simulation at the
KV layer. For *mission-critical* production use it is missing whole categories of
capability, and a few gaps are silent-data-loss hazards rather than absent features.

## Hard blockers

### 1. No backup, restore, PITR, replication, or HA
No backup/export/dump API anywhere in bytdb or btypedb. The only durable artifact
is the single WAL file; the only "checkpoint" is in-place `Compact()`. No hot
backup, no WAL archiving, no second node, no failover. Recovery story for a lost
disk is "you lose the database."

### 2. Mid-file corruption silently discards everything after it
btypedb `replayLog` (`wal.go:131-133`): a CRC failure on *any* record is treated
as end-of-log. Correct for a torn tail; but one bit-rotted record mid-file
silently truncates every later record on next open, no error surfaced. No file
magic / format-version header either, so a wrong or incompatible file can't be
detected. This is a latent bug worth fixing regardless of production ambitions.

### 3. Hard RAM ceiling, ungraceful failure
Entire dataset in memory (disk is durability only); no spill/paging/eviction —
exceeding RAM is an OOM kill. Recovery is full log replay into memory on every
open (startup scales with uncompacted log size). Long-lived read snapshots pin
old COW btree versions with only refcount reclamation — memory grows with churn
under a long reader.

### 4. Zero write concurrency; no way to interrupt a writer
One global writer lock held BEGIN→COMMIT; concurrent writers block indefinitely
(no lock timeout, no conflict-abort). No `context.Context` anywhere in the SQL
execution path; pgwire `CancelRequest` unimplemented (BackendKeyData secret is
hardcoded 0); no `statement_timeout`. The idle-in-transaction timeout (5m
default) fires only between statements — a runaway *write* query wedges every
writer with no remedy but killing the process.

### 5. No security surface on the wire
pgwire is trust-all (`conn.go:186-211` sends AuthenticationOk unconditionally),
declines SSL/TLS, has no roles/GRANT/privileges, no connection cap, and fully
materializes result sets (portal row-limits read and ignored). Localhost-only
posture; cannot face a network.

### 6. Durability-visibility anomaly
A committed write is visible to readers *before* its fsync completes (btypedb
`db.go:44-48`); a commit error means "durability unknown," not "not applied."
A reader can act on data a power cut then erases.

## Significant gaps by domain

- **Data types**: no DATE/TIMESTAMP (epoch INTs advised), no DECIMAL/NUMERIC
  (float64 only — unsafe for money), no UUID/JSON/arrays; `VARCHAR(n)` length
  unenforced; NaN unstorable (tuple encoder rejects it).
- **Integrity**: FOREIGN KEY rejected at parse; no triggers; CHECK/DEFAULT
  enforced only in the SQL layer (bypassable via engine API); UNIQUE only via
  `CREATE UNIQUE INDEX`.
- **SQL**: no CTEs, views, derived-table subqueries (FROM subqueries), RIGHT/FULL
  joins, TRUNCATE, COPY, ALTER COLUMN / RENAME.
- **Planner/execution**: heuristic score only (`plan.go` `pathScore`, no
  statistics/cost model); index nested-loop is the only join algorithm; ORDER BY,
  aggregates, windows, UNION all materialize fully in memory; no external sort,
  no streaming.
- **Operability**: no metrics, no query/connection logging, no
  `pg_stat_activity`, single database per server, shutdown hard-closes
  connections without draining, `bytdbd` hardcodes SyncAlways with no durability
  flag, and `server_version` advertises `16.0` so clients assume Postgres
  features that aren't there.
- **Testing**: no load/soak or perf-regression gates, no executor-level fuzzing
  over generated schemas/data, little multi-client contention stress; `bench/`
  is a standalone comparison harness (with a 67 MB checked-in binary), not a CI
  gate.

## What is already solid

- Per-commit fsync default with group commit (leader-fsync coalescing);
  CRC-framed WAL; batch-atomic recovery with torn-tail truncation; crash-safe
  atomic compaction (temp file + fsync + rename + SyncDir).
- Snapshot-isolation lock-free reads; serialized writes are trivially
  serializable; reentrancy self-deadlock guard (`engine.go:266-297`).
- Per-connection panic fence in pgwire + protocol fuzzing; structured SQLSTATE
  errors; idle-in-transaction timeout protecting the global writer lock.
- Honest docs: `docs/gotchas.md` lists deliberate omissions; MVCC explicitly
  deferred; btypedb self-labels experimental.

## Remediation order (highest leverage first)

1. **Replay fix + file header** (btypedb): distinguish torn tail from mid-file
   corruption; add magic + format version, backward compatible with headerless
   files.
2. **Online backup API** (btypedb): consistent hot copy of the append-only log
   (capture durable length, block compaction during copy).
3. **Cancellation & timeouts** (bytdb + pgwire): context through the executor,
   `statement_timeout`, real BackendKeyData secrets + CancelRequest.
4. **Auth/TLS on pgwire**: password/SCRAM + TLS for any network exposure.

Replication/HA and larger-than-RAM datasets are architectural rewrites, not
features — out of scope until the niche changes.

---

## Remediation status (updated 2026-07-12)

Everything in the "suggested order" and "cheap wins" lists is done:

- **Auth/TLS**: SCRAM-SHA-256 + TLS (`f309deb`), SCRAM-SHA-256-PLUS
  channel binding (`952e49e`).
- **Operability batch** (`ebd0078`): TRUNCATE, VARCHAR(n) enforcement,
  UNIQUE constraint sugar, SHOW / honest server_version,
  bytdbd -sync/-max-conns/-log-queries, pgwire MaxConns + QueryLog.
- **Types** (`ea8da92`): TIMESTAMP[TZ]/DATE (int64 micros/days, UTC),
  UUID; text+binary wire formats; now()/current_date/gen_random_uuid.
- **Foreign keys** (`83a6fc8`): NO ACTION/RESTRICT, MATCH SIMPLE;
  full schema guards; SQLSTATE 23503.
- **Derived tables → CTEs → views** (`bc7f719`): all via the
  virtual-table machinery; IN (SELECT ...) as = ANY.
- **Hash join** (`284bc47`): unindexed equijoins are linear.
- **ALTER RENAME + pg_stat_activity** (`0e80c26`).

Still deferred (rewrites, as assessed): MVCC concurrent writers,
replication/HA, larger-than-RAM, streaming/external sort, cost-based
optimization, triggers, JSON/arrays, NUMERIC(p,s).
