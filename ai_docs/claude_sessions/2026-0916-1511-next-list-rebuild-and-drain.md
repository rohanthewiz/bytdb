# Next-list rebuild over the v0.12.0/v0.13.0 window, then drain it

- **Date:** 2026-09-16
- **Scope:** `/next-list 30` over the session-doc history, then draining the
  list: two decisions, and three DDL features (`alter.go`, `ddl.go`, `sql/`,
  `pgwire/errors.go`, docs). This doc was seeded with the rebuilt list and
  updated in place as items were completed.

## Ask

`/next-list 30`, then "seed a new session doc with this Next list and update
it as we complete items", then "start working what we can from the Next
list". Decisions taken: keep the `## Next` rule global, leave the pgwire tag
gaps, keep carrying the encryption item, and build all three engine items.

## What the rebuild found

- **Window:** 30 docs, `2026-0722-0751-production-comb-fuzz-fixes` →
  `2026-0907-2339-release-v0.12.0`. 28 of them were already scanned by the
  `2026-0907-2325` rebuild, so only the last two docs are new ground.
- **Age floored by the window:** none.
- **Lapsed items:** none. All 13 items from the 09-07 rebuild were finished,
  carried, or declined. "Release phases 2 and 3" left the list between the
  last two docs because it was done (`v0.12.0`).
- **Premises:** all five open items still hold. One reference drifted:
  the identity-column refusal moved from `ddl.go:196` to `ddl.go:200`.
- **Record gap:** commits `d967066` and `d4bfe29` (Engine.Stats, `/metrics`,
  `bytdbd -metrics-addr`, tags `v0.13.0` / `pgwire/v0.13.0`, 2026-09-12) have
  no session doc, so that session never wrote a `## Next`. Its commit
  message names no follow-ups; the memory index is the only other record.

## Work completed

### Decided, no code (items 4 and 5)

- **`## Next` convention stays global.** The list-leak problem is not
  bytdb-specific; `~/.claude/commands/sess-save.md` is left as is.
- **pgwire `v0.10.0` / `v0.11.0` tags are not backfilled.** Nobody is
  blocked and pushed tags are public; recorded under non-goals below.
- **Encryption deferred set stays open** (item 1) — kept on the list rather
  than moved to non-goals.

### Item 2's premise was wrong — built "replace the primary key" instead

The item read "`ALTER TABLE ADD PRIMARY KEY` — contingent on an app
migration needing it", as though a table could lack one. It cannot:
`CreateTable` refuses a keyless table (`ddl.go:36`). In Postgres, `ADD
PRIMARY KEY` on a table that has one fails with "multiple primary keys", so
the useful feature is **replacing** the key, which Postgres spells as a
two-action ALTER. Built that:

- `Engine.SetPrimaryKey(table, cols...)` (`alter.go`). New and old key
  columns become NOT NULL, as Postgres leaves them. Refuses a NULL in a new
  key column (23502), duplicate new keys (`could not create unique index
  "t_pkey"`, detail `Key (org)=(10) is duplicated.`, 23505), and an inbound
  FK whose referenced columns would stop being a unique key (judged with
  `uniqueKeyCovers` against the next descriptor, same rule as `DropIndex`).
- SQL: `DROP CONSTRAINT t_pkey, ADD [CONSTRAINT n] PRIMARY KEY (cols)` —
  the only multi-action ALTER TABLE accepted (`sql/parser.go`
  `replacePrimaryKey`). A lone `ADD PRIMARY KEY` now gives Postgres's
  `multiple primary keys for table "t" are not allowed` (42P16) with a hint
  naming the two-action form; `DROP CONSTRAINT t_pkey` alone keeps refusing,
  with the same hint.
- `bytdb.PKConstraintName` names `<table>_pkey` in one place.

### Item 3 — ALTER COLUMN TYPE, SET STORAGE/STATISTICS, identity ADD COLUMN

- **`rebuildTable`** (`alter.go`) is the shared core for the type change
  and the key replacement: decode every row under the old descriptor,
  transform, `coerceRow` under the new one, validate — then one
  `DeleteRange` over the table's whole space and a rewrite of rows and all
  index entries. Collisions show up as occupied keys during the write
  phase. Chosen over in-place patching because a type change can move a
  value's bytes in the row value, the row key, and index key positions,
  and a key change moves every index entry; re-deriving under the new
  descriptor reuses the write path's own `coerceRow`/`rowKey`/
  `encodeRowValue`/`indexEntry` and cannot miss a structure.
- **`Engine.AlterColumnType(table, col, ColumnTypeChange)`**. Automatic
  (assignment) casts only when `Using` is nil: same type/varchar length,
  int↔float (float→int is round-half-even, range-checked), date↔timestamp,
  anything→text. Otherwise Postgres's `cannot be cast automatically`
  (42804) with a `USING c::type` hint. The DEFAULT is cast with the
  automatic cast (Postgres does not apply USING to defaults) and re-probed
  with `columnDefaultValue`; `now()`/`current_date` survive only onto
  timestamp/date. Refused: FK columns on either side, identity → non-int.
- **SQL**: `ALTER [COLUMN] c [SET DATA] TYPE t [USING expr]`. USING is
  validated with the CHECK rules (`validateRowExpr`, generalized from
  `validateCheckExpr`) and evaluated per row against the descriptor the
  engine resolved inside the DDL transaction (`rowEnvFor` rebuilds scope
  when the row's `Desc` changes). CHECK constraints are re-evaluated on the
  converted rows. **Deliberate leniency:** a USING result coerces like an
  inserted literal, so a text result parses into the new type where
  Postgres would demand an explicit cast inside USING.
- **`SET STORAGE` / `SET COMPRESSION` / `SET STATISTICS`** parse to
  `AlterColumnNoop` — accepted like `OWNER TO`, but the column must exist.
- **Identity ADD COLUMN** (`ddl.go` `backfillIdentity`): rows numbered 1..n
  in PK order by appending a `(colID, n)` pair, counter bumped to n+1 in the
  DDL transaction. Correct under concurrent writes too, because the counter
  key uses a fresh column ID, so no in-memory allocator can already hold it.
- All three respect `DefaultBackfillLimit` (`checkRewriteLimit`).
- pgwire SQLSTATEs: `could not create unique index` → 23505,
  `cannot be cast automatically` → 42804, `multiple primary keys` → 42P16.

### Verification

- `go build`, `go vet`, `gofmt -l` clean (root, sql, pgwire).
- Full suites green: bytdb, sql, replicate, replicate/s3, stdlib, tuple,
  pgwire; `-race` green on bytdb and sql.
- New tests: `alter_rebuild_test.go` (identity numbering in serialized and
  OCC modes, automatic casts incl. re-keying an int PK to text, atomic
  failures, USING, defaults and FK refusals, SetPrimaryKey incl. reopen and
  composite keys, refusals, backfill cap) and `sql/alter_test.go`.
- **Mutation-checked:** disabling the index-entry writes in `rebuildTable`
  fails three tests; bumping the identity counter to 1 instead of n+1 fails
  both identity modes.
- **Wire check** (`/verify`, bytdbd + pgx): every form above over simple and
  extended protocol; SQLSTATEs, hints and details arrive (serr fields ride in
  Detail, pgwire's existing convention).
- Updated pinned tests: `identity_test.go` (ADD identity no longer
  refused), `sql/sql_test.go` and `sql/default_test.go` (new messages).

### Docs

`README.md` grammar, `docs/features.md` (three new bullets), `docs/gotchas.md`
(rejection table + whole-table rewrite note), and the bytdb skill.

## Files touched

`alter.go` (new), `alter_rebuild_test.go` (new), `ddl.go`,
`identity_test.go`, `sql/alter.go` (new), `sql/alter_test.go` (new),
`sql/ast.go`, `sql/check.go`, `sql/default_test.go`, `sql/describe.go`,
`sql/parser.go`, `sql/session.go`, `sql/sql.go`, `sql/sql_test.go`,
`sql/syscat.go`, `pgwire/errors.go`, `README.md`, `docs/features.md`,
`docs/gotchas.md`, `.claude/skills/bytdb-fast-memory-based-db/SKILL.md`.
Nothing committed or tagged.

## Next

*Legend: **age** = session docs since the item was first written down,
counted from this doc (0 = raised here). **value** = payoff, not effort:
**high** means something is worked around today; **medium** means it blocks
one named thing; **low** means nobody has run into it yet or it depends on
something that does not exist.*

1. **btypedb encryption deferred set** **(age 28 · value low)**: online key
   rotation, plaintext↔encrypted migration helper, key+value scope,
   ChaCha20-Poly1305. First written `2026-0722-1303`, restated
   `2026-0730-1924`, `2026-0907-2325`, `2026-0907-2339`, and here. Still
   absent — `btypedb/encrypt.go:41` reserves flag bits 1..4,
   `compact.go:139` names the re-encrypt seam. Rotation needs a v3
   wrapped-DEK header first, because `Compact`'s raw tail-copy is invalid
   across keys. **Low until a deployment needs to rotate a key.** Kept open
   by decision this session.
2. **Release this session's DDL** **(age 0 · value medium)**: new SQL
   surface plus changed error text and SQLSTATEs (a lone `ADD PRIMARY KEY`,
   `DROP CONSTRAINT t_pkey`, and the ALTER COLUMN messages all changed), so
   it needs a minor bump — `v0.14.0`, tag root first, then pgwire in
   lockstep. Blocks any consumer using the features outside the workspace.
3. **pgwire SQLSTATE gaps seen during the wire check** **(age 0 · value
   low)**: `invalid input syntax for type ...` maps to XX000 (Postgres:
   22P02) and `cannot drop constraint "t_pkey"` to XX000 (Postgres: 42P16).
   Both predate this session.
4. **`information_schema.table_constraints` does not exist** **(age 0 ·
   value low)**: the wire check's query returned `no such table` (42P01).
   ORMs that introspect constraints this way would miss them; nobody has
   reported one.

Read by value instead: **high** none · **medium** 2 · **low** 1, 3, 4.

### Deliberate non-goals (declined, not dropped)

- **General multi-action `ALTER TABLE`.** Only the `DROP CONSTRAINT t_pkey,
  ADD PRIMARY KEY` pairing is accepted, because it is the one pairing that
  must be atomic in bytdb. Other comma-joined actions can run as separate
  statements.
- **`ALTER COLUMN TYPE` on foreign-key columns.** Both sides would have to
  change together; drop the constraint, alter both, re-add it.
- **pgwire `v0.10.0` / `v0.11.0` tags stay unbackfilled.** Decided this
  session; the lockstep tag line resumes at `v0.12.0`.
- **Unindexed FK columns scan the child table per check.** Planner-driven FK
  enforcement with no index requirement; documented in the skill's gotchas
  with the workaround ("index the child FK columns").
- **Embedded Engine one-shot writes surface `ErrTxConflict` raw.** The
  caller writes the retry loop, same contract as `WriteTxn`; documented in
  `docs/concurrency.md`.
- **Correlated ON conjuncts and function-wrapped correlated predicates
  evaluate per row.** Full decorrelation was rejected at `2026-0805-1603`;
  rewrite as JOIN for large outer sets.
- **`WriteString` lint hints in test files.** Test readability is worth more
  than the allocation.
