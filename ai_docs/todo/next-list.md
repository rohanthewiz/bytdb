# Next list

The project's single list of open follow-ups. Sessions **edit this file in
place**. They don't copy a list forward into each session doc. A session doc's
`## Next` becomes a short summary instead: `Closed: N-… · Raised: N-…`.

Seeded 2026-09-24 by `/next-list seed` from the 15 session docs
`2026-0805-1603` → `2026-0916-1611`, with each item's `raised` traced back
through all 74 docs.

## Conventions

- **IDs are permanent** (`N-001`, `N-002`, …) and never reused, even after an
  item closes.
- **`raised`** is the stem of the session doc where the item first appeared
  (`ai_docs/claude_sessions/<stem>.md`).
- **Age is computed, never stored**: the number of session docs written after
  `raised`. `/next-list` works it out from the directory listing.
- **Value** measures payoff, not effort:
  - **high**: something is being worked around today, or a second independent
    consumer has arrived.
  - **medium**: it blocks one named thing, or it is a visible defect that
    nobody has to route around yet.
  - **low**: a gap nobody has hit, or it depends on something that doesn't
    exist yet.
- **Open** holds what we plan to pick up next. **Roadmap** holds what we want
  to do later, not soon. **Non-goals** holds what we probably won't do.
- **Nothing leaves Open or Roadmap without a line in another section.** A
  finished item moves to Closed with its date and the evidence. A declined
  item moves to Non-goals with the reason. Merged items go to Closed as
  `merged into N-xxx`. Moving an item between Open and Roadmap is fine.
- Open and Roadmap stay in ID order.

**Next ID:** N-028

## Open

*Empty since 2026-10-05: N-023, N-024 and N-025 closed in
`2026-1005-1107-compaction-stall-vacuum-verbose-notices`.*

## Roadmap

Wanted, but not soon. Promote an item to Open when the thing it waits for
arrives.

- **N-001** · raised `2026-0722-1303-wal-encryption-at-rest` · value low
  **btypedb encryption deferred set:** online key rotation, a
  plaintext↔encrypted migration helper, key+value scope, and
  ChaCha20-Poly1305. None of these exist yet. `btypedb/encrypt.go:40-41`
  reserves flag bits 1..4, and `compact.go:138-139` names the re-encrypt seam.
  Rotation first needs a v3 wrapped-DEK header, because `Compact`'s raw
  tail-copy is invalid across keys. Restated at `2026-0730-1924`,
  `2026-0907-2325`, `2026-0907-2339`, `2026-0916-1511`, `2026-0916-1557` and
  `2026-0916-1611`, and kept open by decision each time. **Trigger:** a
  deployment needs to rotate a key. *Restored 2026-10-03 by `/next-list`:
  commit `dd7a25d` (closing N-022) deleted the whole Roadmap and Non-goals
  sections with no closing line. Premise re-checked against btypedb then:
  still absent, and both citations still hold.*

## Non-goals

*Restored 2026-10-03 by `/next-list` after commit `dd7a25d` deleted this
section. The entries below are the text as of `3b603e9`, unchanged.*

- **N-005** · declined `2026-0730-2350-occ-stage2-sequences`: **Embedded
  Engine one-shot writes surface `ErrTxConflict` raw.** The caller writes the
  retry loop, the same contract as `WriteTxn`. Documented in
  `docs/concurrency.md`.
- **N-006** · declined `2026-0805-1603-correlated-subquery-index-pushdown`:
  **Correlated ON conjuncts and function-wrapped correlated predicates
  evaluate per row.** Full Postgres-style decorrelation was considered and
  rejected. For large outer sets, rewrite as a JOIN.
- **N-007** · declined `2026-0805-1603-correlated-subquery-index-pushdown`:
  **Statement paths that never seed a `subMemo` re-prepare correlated
  subqueries on each invocation.** This errs on the safe side, and they still
  get pushdown within one invocation. It was recorded as "known remaining
  (deliberate)" and never appeared in a Next list. Filed here so it stays
  visibly declined.
- **N-008** · declined `2026-0907-2325-next-list-rebuild-backlog-drain`:
  **Unindexed FK columns scan the child table on each check.** FK enforcement
  is planner-driven and doesn't require an index. The skill's gotchas document
  the workaround ("index the child FK columns").
- **N-009** · declined `2026-0907-2325-next-list-rebuild-backlog-drain`:
  **`WriteString` lint hints in test files.** Test readability is worth more
  than the allocation.
- **N-010** · declined `2026-0916-1511-next-list-rebuild-and-drain`:
  **General multi-action `ALTER TABLE`.** Only the `DROP CONSTRAINT t_pkey,
  ADD PRIMARY KEY` pairing is accepted, because it is the one pairing that must
  be atomic in bytdb. Other actions can run as separate statements.
- **N-011** · declined `2026-0916-1511-next-list-rebuild-and-drain`:
  **`ALTER COLUMN TYPE` on foreign-key columns.** Both sides would have to
  change together. Instead, drop the constraint, alter both columns, and re-add
  it.
- **N-012** · declined `2026-0916-1511-next-list-rebuild-and-drain`:
  **pgwire `v0.10.0` / `v0.11.0` tags stay unbackfilled.** The lockstep tag
  line resumes at `v0.12.0`.
- **N-013** · declined `2026-0916-1557-sqlstate-gaps-constraint-catalog`:
  **Unique indexes vs UNIQUE constraints in `table_constraints`.** bytdb can't
  tell them apart, so every unique index reports as UNIQUE.
- **N-014** · declined `2026-0916-1557-sqlstate-gaps-constraint-catalog`:
  **Synthetic NOT NULL CHECK rows in `table_constraints`.** Nullability is
  already in `information_schema.columns`.
- **N-015** · declined `2026-0916-1611-license-and-release-v0.15.0`:
  **License file in `bench/`.** It is an internal harness module and nothing
  imports it.

## Closed

- **N-027** · raised in dbc as its N-131 (dbc doc `2026-1006-1855-scripts-dir-resolution`) · closed
  2026-10-06, `2026-1006-1907-int-into-bool-coerce-v0.21.2`. **An integer 0/1 could not be stored in a bool column.** SQLite
  has no boolean type and returns a BOOLEAN column as int64 0/1, so dbc's
  `s.Copy("demo-sqlite", "demo-bytdb", "cats", …)` failed on `adopted` with
  "value does not fit column type" (`coerce` in `dml.go`). Fixed in `coerce`,
  so the Go API, `database/sql` and pgwire all accept it. An integer of any
  width that is 0 or 1 becomes false/true, the same rule as `database/sql`'s
  `driver.Bool`. Any other integer is refused with "integer for a bool column
  must be 0 or 1". Side effect: the SQL literal `INSERT … VALUES (1)` into a
  bool column is now accepted, where Postgres refuses it. Bound parameters and
  literals look the same by the time they reach `coerce`. Tests:
  `stdlib.TestIntParameterIntoBool` (INSERT and UPDATE via `$n`, plus the 2
  rejection) and new cases in `TestCoerceWidthsAndMismatches`. Both fail on
  the old `coerce`. `docs/features.md` notes the rule on the `BOOL` row. Released in `v0.21.2` (`38e193a`) and `pgwire/v0.21.2`
  (`b958b26`). dbc still has to bump to v0.21.2 to close its N-131.

- **N-026** · raised in dbc as its N-128 (dbc doc `2026-1006-1430-headless-dbc-copy`) · closed
  2026-10-06, `2026-1006-1520-date-input-accepts-timestamp-text`. **A `time.Time` could not be bound to a date column.**
  `stdlib.CheckNamedValue` sends every `time.Time` as `'2006-01-02
  15:04:05…'` UTC text, and `ParseDate` took only `YYYY-MM-DD`, so the insert
  failed with "invalid input syntax for type date". That blocked dbc's
  `dbc copy` / `s.Copy` of any date column into bytdb, since pgx reads
  Postgres dates as midnight-UTC `time.Time`. Fixed in `ParseDate` (`types.go`),
  not the driver. It now accepts every `ParseTimestamp` form, and keeps the date
  as written while dropping the time and zone, as Postgres does (checked
  against Postgres 17: `'2024-01-02 23:30:00-05'::date` is 2024-01-02, and
  `'2024-01-02 25:00:00'` is still an error). A bound `time.Time` therefore
  lands on its UTC day, the same day the Go API (`coerce` in `dml.go`)
  truncates one to. Tests: `TestValueTextRoundTrip/date_from_timestamp_text`,
  and `stdlib.TestTimeParameterIntoDate`, which fails on the old `ParseDate`.
  Released in `v0.21.1` and `pgwire/v0.21.1`. dbc still has to bump to
  v0.21.1 to close its N-128.

- **N-025** · raised `2026-1005-1022-add-column-if-not-exists-v0.20.0` ·
  closed 2026-10-05, `2026-1005-1107-compaction-stall-vacuum-verbose-notices`. **Skip notices go out at
  WARNING severity with SQLSTATE 01000.** Fixed: `noticeBody`
  (`pgwire/errors.go`) now sends:
  - `... already exists, skipping` as NOTICE 42P07, or 42701 for a column;
  - `... does not exist, skipping` as NOTICE 00000;
  - VACUUM VERBOSE's report as INFO 00000.

  VACUUM's `skipping "v" --- cannot vacuum non-tables` stays WARNING
  01000, because it is a WARNING in Postgres too. `sendNotice`
  (`pgwire/conn.go`) now sends a multi-line `Result.Notice` as one
  NoticeResponse per line. Before, VACUUM's joined skip warnings went out
  as a single message. Tests: `TestNoticeSeverities`
  (`pgwire/notice_test.go`) runs 13 statements over pgx and checks
  severity, code and message for each. Checked over the wire with
  bytdbd. Commit `5722154`.
- **N-024** · raised `2026-1003-0815-next-list-restore-sess-save-guard` ·
  closed 2026-10-05, `2026-1005-1107-compaction-stall-vacuum-verbose-notices`. **Nothing tells you whether a
  VACUUM is worth running, or what it reclaimed.** Fixed with the cheap
  version the item described:
  - btypedb v0.9.0 adds `LogStats()`, which returns epoch, size and
    `BaseSize` read under one lock.
  - bytdb's `Stats` gains `LogBaseBytes`, and `/metrics` gains
    `bytdb_log_base_bytes`.
  - VERBOSE is now kept rather than discarded (`(VERBOSE false)` turns it
    off), and `VACUUM VERBOSE` appends an INFO line:
    `compacted the storage log: B bytes before, A after (R reclaimed)`.

  `LogBytes - LogBaseBytes` is the growth auto-compaction measures. After
  a restart, the base is the whole file as found, so the growth
  understates the garbage until the first compaction. The `Stats` doc
  says so. `Engine.Compact` still returns only an error: VACUUM reads
  `LogState` on either side of it. Tests: `TestParseVacuum` (verbose
  cases), `TestVacuumVerbose`, `TestEngineCompact` (base assertions),
  `TestMetricsHandler`, and btypedb's `TestLogStats`. Commit `b841789`.
- **N-023** · raised `2026-1003-0815-next-list-restore-sess-save-guard` ·
  closed 2026-10-05, `2026-1005-1107-compaction-stall-vacuum-verbose-notices`. **Compaction's writer pause grows
  with database size.** The proposed fix (an early snapshot sync) wasn't
  enough on its own. Measured with a fast writer (`SyncNever`, 1 KB values),
  phase B's copy of the *tail* also grew with the database: ~400 MB of
  tail at 900 MB, because the tail is every append made while the snapshot
  streamed. With only the early sync, the worst stall got *worse*.
  btypedb v0.9.0 (`902e317`) runs an unlocked catch-up loop instead. It
  copies the tail so far and fsyncs, up to 4 rounds, until at most 1 MiB
  is left. Phase B then splices and syncs only that residue, and skips
  the sync when there is none. Measured phase-B lock hold at ~900 MB:
  v0.8.0 340–540 ms, v0.9.0 16–32 ms. Two follow-on fixes keep the
  auto-compaction policy unchanged:
  - `baseSize` excludes appends made after the snapshot streamed.
  - A successful run re-checks the policy, because a burst that ends
    during the catch-up has no later append to trigger it.

  Writers can still stall outside the lock when they outrun the disk (a
  `SyncNever` writer at memory speed saw 100–450 ms), probably from the
  kernel throttling writes. Under `SyncAlways`, each write pays its own
  F_FULLFSYNC, and old and new behave about the same. Tests: btypedb
  `TestCompactSyncsSnapshotBeforePausingWriters` (no tail, small tail,
  tail over the slack) and `TestAutoCompact` (now waits for chained runs).
  bytdb bumped to v0.9.0, `Engine.Compact`'s doc corrected (`d9631d3`).
- **N-022** · raised `2026-0925-1542-file-lock-release-v0.17.0` · closed
  2026-09-25, `2026-0925-1607-lock-in-btypedb-v0.18.0`. **The file lock covers only
  `bytdb.Open`.** Closed ahead of its trigger. The lock moved into btypedb
  v0.8.0 (`lock.go` there): `btypedb.Open` takes it, so raw opens are covered
  for every btypedb user. It sits on a new `fsys.Lock` seam, not
  `realFS.OpenFile` as first suggested, because locking the database file
  itself stops working after the first compaction rename. `Backup` locks its
  destination, and the new `btypedb.AcquireLock` guards `replicate.Restore`'s
  `destPath` through the rename. Both refuse a live file with `ErrLocked`.
  bytdb dropped its own copy (the lock is not reentrant) and aliases
  `ErrLocked = btypedb.ErrLocked`. Released in btypedb `v0.8.0` and bytdb /
  pgwire `v0.18.0`. Other btypedb consumers (cats, gonotes, grmob, …) get
  `ErrLocked` on a double open once they bump. Tests: btypedb `lock_test.go`,
  bytdb `TestLockRawKVOpen` / `TestLockBackupDestination`,
  `replicate/restore_lock_test.go`.

- **N-021** · raised in dbc, `2026-09-25` (no bytdb session doc) · closed
  2026-09-25, `2026-0925-1542-file-lock-release-v0.17.0`. **bytdb takes no file lock.** Two dbc processes
  (two TUIs, or a TUI and `dbc web`) opened the same `demo.bytdb` and both
  wrote its WAL without a word. Fixed: `Open` takes a non-blocking
  exclusive lock on a `<path>.lock` sidecar before btypedb touches the
  file, and fails with the new `bytdb.ErrLocked` (with `holder_pid`); `Close`
  releases it after the final fsync (`lock.go`, `lock_flock.go`,
  `lock_windows.go`, `lock_other.go`). Released in `v0.17.0`. dbc still has
  to bump to v0.17.0 and match `errors.Is(err, bytdb.ErrLocked)`. Tests:
  `lock_test.go`, including a real second process and a `kill -9` holder.
- **N-020** · raised `2026-0924-1717-drop-unique-constraint-bind-format-sqlstate` ·
  closed 2026-09-24, `2026-0924-1717-drop-unique-constraint-bind-format-sqlstate`.
  **`cannot drop a primary key column` maps to XX000.** Fixed, with the rest
  of the primary-key DDL errors. A wire probe found seven more that were
  mis-mapped:
  - **42P16** (the one-key invariant) now also covers a key column's
    DROP NOT NULL, ADD COLUMN ... PRIMARY KEY, CREATE TABLE with no key,
    and the parser's short "multiple primary keys".
  - **42703** covers a key naming an undeclared column
    (`primary key column not declared`) and Postgres's
    `column "x" of relation "t" does not exist` (ADD PRIMARY KEY, ALTER
    COLUMN).
  - **42701** covers a column named twice in a key and a RENAME onto a
    taken name. `duplicate primary key column` used to come back as 23505,
    because it contains the data-conflict wording "duplicate primary key".
    A client would have read a DDL typo as a unique violation. The 42701
    case now comes before 23505.

  Tests: new `TestSQLStateMapping` rows, including one pinning that a
  constraint's "does not exist" stays 42704. `TestPrimaryKeyDDLErrors`
  (`pgwire/server_test.go`) runs 11 statements over pgx.
- **N-019** · raised `2026-0924-1717-drop-unique-constraint-bind-format-sqlstate` ·
  closed 2026-09-24, `2026-0924-1717-drop-unique-constraint-bind-format-sqlstate`.
  **Dependency refusals map to XX000.** Fixed: `sqlstate`
  (`pgwire/errors.go`) maps every dependency refusal to 2BP01. When filed,
  the item named four. The code has eleven: the two "because other objects
  depend on it" wordings (DROP TABLE, DROP/RENAME COLUMN blocked by a
  CHECK), the two "a foreign key depends on" ones (unique index, PK
  replace), dropping an indexed column or an FK column, and the "referenced
  by a foreign key" refusals (drop/rename column, rename table, alter type).
  All eleven get 2BP01, even where Postgres would allow the change, because
  the remedy is the same in every case: remove the dependent object, then
  retry. TRUNCATE of a referenced table keeps Postgres's 0A000. Tests: 13
  new `TestSQLStateMapping` rows, and `TestDependencyRefusals`
  (`pgwire/server_test.go`), which triggers the refusals from real SQL over
  pgx. "Drop a column referenced by a foreign key" (`ddl.go:438`) is covered
  by a mapping row only. From SQL, an earlier check always fires first: a
  referenced column is always in the primary key ("cannot drop a primary key
  column") or in a unique index ("cannot drop an indexed column").
- **N-018** · raised `2026-0924-1559-pg-constraint-key-rows` · closed
  2026-09-24, `2026-0924-1717-drop-unique-constraint-bind-format-sqlstate`.
  **`DROP CONSTRAINT` refuses a unique constraint's name.** Fixed:
  `execDropConstraint` (`sql/check.go`) now tries unique indexes after
  checks and FKs, and drops a match through `Engine.DropIndex`, so it keeps
  DROP INDEX's refusal when an FK depends on the index. A plain index is not
  a constraint and still gets "does not exist". Test:
  `TestDropConstraintUnique`.
- **N-004** · raised `2026-0916-1557-sqlstate-gaps-constraint-catalog` ·
  closed 2026-09-24, `2026-0924-1717-drop-unique-constraint-bind-format-sqlstate`.
  **`bad parameter format code` maps to XX000.** Fixed: `sqlstate`
  (`pgwire/errors.go`) now maps it to 08P01 next to "wrong number of
  parameters". Tests: the `TestSQLStateMapping` row now expects 08P01, and a
  new case 3 in `TestBindFormatCountMismatchIsProtocolError` sends a Bind with
  format code 2 over the wire and checks the ErrorResponse's `C` field.
- **N-003** · raised `2026-0916-1557-sqlstate-gaps-constraint-catalog` ·
  closed 2026-09-24, `2026-0924-1559-pg-constraint-key-rows`. **`pg_constraint` omits primary
  keys and unique constraints.** Fixed: one `p` row per table and one `u` row
  per unique index, each with `conindid` set to the backing index and its oid
  reused as the constraint oid (`sql/syscat.go`). `conkey` and `confkey`
  (FK rows) are now filled as `{1,2}` literals; before, they were NULL on
  every row. `pg_get_constraintdef` renders `PRIMARY KEY (...)` and
  `UNIQUE (...)`. psql's `\d` index listing now joins the pkey to its
  constraint. Tests: `TestSystemCatalogKeyConstraints`, plus a new assertion
  in `TestPsqlDescribeTable`.
- **N-002** · raised `2026-0805-1820-benchmark-rerun-m1pro-doc-refresh` ·
  closed 2026-09-24, `2026-0924-1522-next-list-seed-view-catalog-v0.16.0`. **`bench/go.mod` pin goes stale on every release.**
  Tidied from v0.14.0 to v0.16.0, and bench now builds with and without
  `GOWORK=off`. The lasting fix is the new `/release` skill
  (`.claude/skills/release/SKILL.md`). It is the first written release
  routine, and step 5 tidies bench right after pgwire's pin bump, so the pin
  follows every release.
- **N-017** · raised in dbc, `2026-09-24` (no bytdb session doc) · closed
  2026-09-24, `2026-0924-1522-next-list-seed-view-catalog-v0.16.0`. **Views have no columns in the catalog.** Fixed:
  `pg_attribute` and `information_schema.columns` now list each view's
  output columns, and `information_schema.tables` lists views as `VIEW`. The
  columns come from describing the stored query with `staticView`, the same
  path EXPLAIN and wire Describe use, so nothing executes
  (`DB.viewColumns`, `sql/syscat.go`). A `catalogShapes` flag stops the
  recursion a view over `pg_attribute` would otherwise cause. Tests:
  `TestSystemCatalogViews` and `TestSystemCatalogBrokenView`. Checked over
  the wire with dbc's own column and table queries. dbc's bytdb-specific
  table query (`dbc/db/catalog.go:49`) can now fall back to the generic
  `information_schema.tables` one if wanted.
- **N-016** · raised `2026-0805-1854-occ-truncate-insert-race-flake-fix` ·
  closed 2026-09-24, `2026-0924-1522-next-list-seed-view-catalog-v0.16.0`. **Use Go 1.25's `WaitGroup.Go` for the
  `wg.Add(1)`/`go func` pairs in `seq_occ_test.go`.** gopls suggested it, and
  the item never reached a Next list. The rebuild found it already done:
  `seq_occ_test.go` has six `wg.Go(` calls and no `wg.Add(1)` left. They came in with
  commit `6145d14` (the `2026-0907-2325` backlog drain).

Closures before 2026-09-24 are recorded in the session docs' `## Next`
sections, for example
`2026-0916-1611` (release `v0.15.0`) and `2026-0916-1557` (SQLSTATE gaps,
`table_constraints`, bench tidy).
