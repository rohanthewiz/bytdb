# Session: Audit remediation complete — channel binding through pg_stat_activity

- **Session ID**: `82f11035-5a97-4bab-9a07-5814f87992e7`
- **Date**: 2026-07-11 → 2026-07-12
- **Prior context**: continues the auth/TLS session (`f309deb`). User
  asked: "Continue all the remaining audit items" — everything left on
  `ai_docs/production-readiness-audit.md`'s remediation lists.
- **State at end**: all items implemented, tested (every suite green
  incl. `-race` on sql and pgwire), and committed — 9 commits
  `952e49e`..`0e80c26` plus the audit-doc status commit. The audit's
  "suggested order", "cheap wins", and "reasonable, high-value" lists
  are now fully done; only the assessed rewrites remain (MVCC,
  replication, larger-than-RAM, external sort, cost model, triggers,
  JSON/arrays, NUMERIC).

## 1. `952e49e` — SCRAM-SHA-256-PLUS channel binding

- `Server.channelBinding()` (pgwire/auth.go) derives RFC 5929
  tls-server-end-point data — hash of the DER cert by its signature
  algorithm's hash (MD5/SHA-1→SHA-256), mirroring pgx's client-side
  `getTLSCertificateHash` exactly. Only derivable when cert selection
  is unambiguous: exactly one static certificate, no
  GetCertificate/GetConfigForClient (SNI would make "the served cert"
  unknowable server-side). Cached via `bindOnce` on Server.
- Advertised ahead of SCRAM-SHA-256 when `c.tlsOn && binding != nil`.
- gs2 parsing reworked: `p=tls-server-end-point,` required iff PLUS
  chosen; **"y" with PLUS advertised = downgrade → fail** (RFC 5802
  §6); `c=` now verifies base64(gs2header ‖ cbind-data).
- Tests: pgx `channel_binding=require` (works over TLS, refused
  client-side over plaintext); hand-rolled raw client sends "y,," to
  assert the downgrade refusal (pgx will never emit that shape).

## 2. `ebd0078` — operability batch

- **TRUNCATE** `[TABLE] t[, ...] [RESTART|CONTINUE IDENTITY]
  [RESTRICT]`: engine `Txn.Truncate` = one DeleteRange over
  `tableSpace(id)` (covers rows + all indexes) (+ identity counters on
  RESTART). DML, not DDL: runs and rolls back inside blocks; all
  tables in one txn; CASCADE rejected; sysWriteGuard per table.
- **VARCHAR(n)** / CHARACTER VARYING(n): limit stored as
  `Column.MaxLen`, enforced in engine coerceRow/updateRow
  (`enforceMaxLen`): rune counting, "value too long for type character
  varying(n)" (22001 via pgwire), all-space overflow truncates
  silently per the SQL standard. Reported in
  information_schema.columns.character_maximum_length. char(n)
  rejected with a pointer to varchar.
- **UNIQUE sugar**: column and table-level UNIQUE lower to unique
  indexes named `t_cols_key`; failed index creation un-creates the
  table (all-or-nothing CREATE TABLE).
- **SHOW** name / ALL / TIME ZONE / TRANSACTION ISOLATION LEVEL
  (JDBC probes the last). Session SET values overlay `showDefaults`
  (sql/vars.go); `sql.ServerVersion` ("16.0 (bytdb)") is now the one
  source for the wire ParameterStatus, SHOW, and version().
- **pgwire**: `Server.MaxConns` — over-limit connections get FATAL
  53300 "sorry, too many clients already" after protocol negotiation
  (so it arrives through TLS); CancelRequest exempt, decision made
  under the same lock that registers the conn. `Server.QueryLog(q,
  dur, err)` called per executed statement (simple + extended).
- **bytdbd**: `-sync always|never` (via new `bytdb.WithSyncNever()`),
  `-max-conns`, `-log-queries`.

## 3. `ea8da92` — TIMESTAMP/TIMESTAMPTZ/DATE/UUID

- Representations (the audit's premise: tuple already orders int64):
  TTimestamp = int64 **Unix-epoch microseconds, UTC**; TDate = int64
  days; TUUID = 16-byte []byte. No tuple-format change; keys/indexes
  order chronologically/bytewise for free.
- Shared text codecs in **bytdb/types.go**: Parse/FormatTimestamp
  (Postgres text shapes incl. 'T', zones, bare date; renders
  `2024-01-02 03:04:05.123456+00`), Parse/FormatDate, Parse/FormatUUID
  (dashed + undashed in, canonical out).
- SQL: typeName accepts timestamp[(p)] [with|without time zone],
  timestamptz, date, uuid ("time" rejected); parenthesis-free
  CURRENT_TIMESTAMP/CURRENT_DATE lower to now()/current_date;
  coerceLit adapts text literals; casts ::timestamp[tz]/::date/::uuid;
  evalFunc: now (+ transaction/statement/clock_timestamp aliases,
  per-call not txn-frozen — documented), current_date,
  gen_random_uuid (v4). Bound `time.Time` stays itself through
  bindArg and converts where the column type is known (coerceLit +
  engine coerce + checkPred dynamic mirror) — micros vs days is
  column-dependent. `[16]byte` binds as uuid.
- Wire: OIDs 1184/1082/2950 (timestamp presents as timestamptz —
  stored instants are UTC); binary formats convert at the PG-2000
  epoch offsets; **encodeValue now takes the declared ColType** and
  sendDataRows threads Result.Types — an int64 is a count or an
  instant only by declaration. decodeParam handles text+binary for
  all three.
- Catalog: typeOID/typeLen/sqlTypeName/udtName + pg_type rows.
- DEFAULT now() still rejected (defaults are constants) — message
  re-worded; stale "no date/time types" docs updated.

## 4. `83a6fc8` — foreign keys (NO ACTION/RESTRICT, MATCH SIMPLE)

- **Engine** (bytdb/fk.go): `FKDesc{Name, Cols []int (child ords),
  RefTable, RefCols []string}` on the child descriptor.
  `AddForeignKey(table, fk, validateRows)`: parent must exist; RefCols
  default to parent PK; must be PK or a unique index's column set
  (Postgres wording when not); types must match; optional existing-row
  validation = one scan of each table via a tuple-encoded parent key
  set (not O(n·m)). `DropForeignKey`. `Txn.ReferencingFKs(table)`
  exposes inbound refs (`FKRef{Child, FK}`).
- **Schema guards**: DropTable refuses while referenced (self-refs
  exempt); DropColumn refuses FK columns on either side; DropIndex
  refuses the unique index a constraint depends on unless another
  unique key still covers the RefCols.
- **SQL enforcement** (sql/fk.go): child INSERT/UPDATE (and upsert
  DO UPDATE) verify each fully non-NULL FK tuple exists in the parent
  — via `fkRowExists` through planScan, so a parent key/index makes it
  a point get. Parent DELETE/UPDATE: inbound refs collected once;
  old-row images re-checked **after** the statement's writes (NO
  ACTION end-of-statement), so deleting a parent with its children —
  or a self-referencing row — in one statement is legal. UPDATE only
  re-checks rows whose referenced columns changed. TRUNCATE of a
  referenced table requires the children in the same list.
- Grammar: column `REFERENCES parent[(col)]`, table-level
  `[CONSTRAINT n] FOREIGN KEY (cols) REFERENCES ...`, `ALTER TABLE ADD
  FOREIGN KEY` (validates rows), DROP CONSTRAINT covers checks + FKs.
  CASCADE / SET NULL/DEFAULT / MATCH FULL/PARTIAL rejected at parse.
- Wire: "violates foreign key constraint" → 23503; truncate-refused →
  0A000. pg_constraint contype 'f' rows + pg_get_constraintdef.

## 5. `bc7f719` — derived tables, CTEs, views, IN (SELECT)

- **One mechanism**: a subquery materializes once into a
  per-statement virtual table (`DB.vtabs map[string]vtab`, layered
  first in `d.lookup`) — the same `scopeTable.rows` path that serves
  the system catalog, so joins/filters need nothing new.
- **Derived tables** `(SELECT ...) alias` (alias mandatory) lower at
  parse to synthetic CTEs named `*derived*N` ('*' is unspellable in
  idents), hoisted with written CTEs into the top-level Select.With in
  completion order = dependency order. Non-SELECT statements with
  hoisted CTEs get a clear parse error.
- **WITH** name[(cols)] AS (...): sequential visibility, column
  renames, shadows real tables, visible in scalar subqueries and
  union arms. RECURSIVE rejected. Parameters bind inside bodies
  (numParams/bindSelect now walk With — was a real bug).
- **Views**: engine keyspace `sysViewTableID=3` stores
  `ViewDesc{Name, Query}`; shared relation namespace (CreateTable/
  CreateSequence/CreateView all cross-check). CREATE [OR REPLACE]
  VIEW validates by running the body once; statements referencing a
  view materialize it (withViews → materializeView, recursive,
  depth-capped 32 for OR-REPLACE-created cycles). pg_class relkind
  'v' rows (synthetic oids) + pg_get_viewdef.
- **Static paths**: Describe (wire Parse) and EXPLAIN resolve CTEs and
  views without executing — staticWith/staticViews register
  empty-row vtabs shaped by describeSelect. Execution of
  CTE/view-bearing selects wraps in one ReadTxn (WriteTxn when
  sequences are written) so materializations and body share a
  snapshot.
- **IN (SELECT ...)** now parses, lowered to `= ANY` (NOT IN →
  NOT(= ANY)) — it had never been supported.

## 6. `284bc47` — hash join

- `chooseHashJoin` (prepareFrom): a step hash-joins when it has
  equality templates, no PK/index starts with any of the equijoin
  columns (virtual tables never have one), and the sides hash
  compatibly (same type, or both numeric-class — int/float/
  timestamp/date). Build once per step (statics pushed into the build
  scan), probe per outer row; LEFT JOIN NULL-extension preserved.
- Keys: tuple-encode with int64→float64 normalization so 1 = 1.0
  buckets together; buckets only need to be **supersets** — every
  candidate re-passes the full ON/WHERE evaluation, which is also why
  non-equality templates can be dropped from the probe safely.
- EXPLAIN: "Hash Join"/"Hash Left Join" + "Hash Cond: (b.y = a.x)";
  eq templates claimed by the join node instead of the inner scan.
- `TestHashJoinScale`: 3000×3000 unindexed equijoin (would be 9M
  probes quadratic) as the complexity guard.

## 7. `0e80c26` — ALTER RENAME + pg_stat_activity

- **RenameTable**: descriptor is keyed by name → delete old key, write
  under new; rows never move (table-ID keyspace). Refused while
  referenced by another table's FK (RefTable is a name); self-refs
  rename along. **RenameColumn**: descriptor-only (rows tagged by
  column ID); refused for columns in CHECK text (SQL-layer guard,
  generalized from checkDropColumn) or referenced by another table's
  FK; own inbound self-reference RefCols rename along. Views aren't
  tracked — stale text fails at next use (documented).
- **pg_stat_activity**: `sql.Activity` + `DB.SetActivityProvider`
  (copied into sessions with the DB); `pgwire.NewServer` installs a
  provider over the backends registry. Each conn tracks
  user/app_name/client_addr/state/query under its own `statMu`
  (provider reads cross-goroutine); state transitions:
  statConnected → statActive(q) → statIdle (reads sess.Status() on
  the conn's own goroutine: idle / idle in transaction / (aborted)).

## Gotchas hit

- pgconn (pgx v5.10) sends gs2 "n,," when channel_binding=disable —
  no downgrade false-positive; "y,," only when PLUS wasn't offered.
- `IN (subquery)` had silently never parsed; found by the CTE tests.
- numParams/bindSelect missing With traversal made prepared CTE
  statements report 0 params.
- `time.Time` args can't normalize at bind time (micros vs days
  depends on the column) — converted at every point where the column
  type is known instead.
- coerceLiterals' buildScope failure is non-fatal by design, so CTE
  names needed no changes there (dynamic checkPred coercion covers).
- explain's stepScan synthesizes template preds — hash steps must
  claim eq templates without synthesizing, or they'd render as index
  conds on the inner scan.

## Files (high-level)

- **bytdb**: engine.go (MaxLen, new ColTypes, FKDesc, view table ID,
  WithSyncNever), dml.go (enforceMaxLen, datetime/uuid coerce),
  ddl.go (validMaxLen, FK/rename guards, RenameTable/RenameColumn),
  txn.go (Truncate, ReferencingFKs), fk.go (new), view.go (new),
  types.go (new), index.go (FK-backed unique index guard).
- **sql**: parser.go (+~500: truncate/show/with/views/references/
  rename/in-subquery/types), ast.go, sql.go (run rewiring, vtabs),
  exec.go (truncate, FK hooks), fk.go (new), views.go (new), vars.go
  (new), join.go (hash join), explain.go, coerce.go, expr.go,
  describe.go, session.go, syscat.go (pg_stat_activity, pg_type,
  pg_constraint 'f', columns.character_maximum_length), params.go.
- **pgwire**: auth.go (PLUS), conn.go (MaxConns/QueryLog/stat/
  typed encoding), pgwire.go (fields, activity provider), values.go
  (new OIDs, typed encode/decode), errors.go (23503, 0A000),
  cmd/bytdbd (flags). Tests throughout (~10 new test files).

## Follow-ups

- Commits are **local-only**: 9 unpushed on main (`git log
  origin/main..main`). Wrap step pushes them.
- Deferred by assessment (rewrites): MVCC writers, replication/HA,
  larger-than-RAM, streaming/external sort, cost model, triggers,
  JSON/arrays, NUMERIC(p,s).
- Minor known edges (documented in code): now() is per-call, not
  transaction-frozen; view text can go stale across RENAME; autocommit
  DML materializes views on a separate snapshot from its write txn;
  EXPLAIN of CTEs shows scans over empty virtual rows.
