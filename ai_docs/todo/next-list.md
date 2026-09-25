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

**Next ID:** N-023

## Open

Nothing open.

## Closed

- **N-022** · raised `2026-0925-1542-file-lock-release-v0.17.0` · closed
  2026-09-25, bytdb `100aafa`, btypedb `49d0e36`. **The file lock covers only
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
