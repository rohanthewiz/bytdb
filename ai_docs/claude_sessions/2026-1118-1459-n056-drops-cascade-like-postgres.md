# N-056: drops cascade to dependents the way Postgres's do

Session: `0eeafa70-0183-4ec1-b691-998f153fc63f`. The user pasted N-056
from the Next list. The decision was already made (2026-11-14, user): do
what Postgres does, where it works for bytdb, reversing deep-look D-54's
RESTRICT-only drops. The item asked for each case to be checked against
PG16 first. Saved and committed with `/sw`.

## PG16 first

Three probe scripts ran in the running `bytdb-numeric-pg` container
(postgres:16 on 55432, `docker exec -i … psql -U postgres`). What PG16
did:

| Case | PG16 |
|---|---|
| `DROP COLUMN a`, with indexes on `(a)`, `(b, a)` and CHECKs `a > 0`, `a < b` | all four dropped silently |
| drop an FK's own (child) column, single or multi-column FK | FK dropped silently |
| drop a column a column's own FK references (`a unique references t(a)`) | silent: the auto dependency wins |
| drop a referenced parent column (unique) | 2BP01; CASCADE drops the FK and the unique index, one NOTICE |
| drop a self-referenced column (`pc references t(code)`, drop `code`) | 2BP01 too; CASCADE notice `… on table t` |
| drop a PK column, nothing referencing it | silent; the PK goes |
| `DROP TABLE p` with 2 FKs and views `v2` (on p), `v3` (on v2) | 2BP01 listing all four; CASCADE drops FKs and both views, tables kept |
| `DROP TABLE p, c` (c references p), no CASCADE | silent |
| `DROP TABLE a, p` where p has a dependent | `cannot drop desired object(s) because other objects depend on them` |
| `DROP TABLE a, a` | fine, one drop |
| view alone blocking `DROP TABLE`; view on view blocking `DROP VIEW` | 2BP01; CASCADE drops |
| `DROP COLUMN z` read by a view | 2BP01 (column-level view dependency) |
| `DROP INDEX` / `DROP CONSTRAINT` of the unique an FK uses | 2BP01; CASCADE drops the FK |
| `DROP TABLE IF EXISTS nope, p4 CASCADE` | skip notice first, then the cascade notice |

Message shapes: relation names are quoted only when needed (reserved
words included: `view "user"`), while constraint and column names stay bare
(`constraint Fk 5 on table "C5"`, `column Code X of table "P"`). One
dependent gets `NOTICE: drop cascades to constraint x on table y`.
Several get one `drop cascades to N other objects` NOTICE with the list in
DETAIL. The refusal is `cannot drop <obj> because other objects depend on
it`, with DETAIL lines `<dep> depends on <obj>` and the HINT `Use DROP ...
CASCADE to drop the dependent objects too.`

Side findings: PG16 names a two-column FK `ch_x_y_fkey`, bytdb names it
`ch_x_fkey`, which was raised as N-060. bytdb's parser doesn't accept
WITH inside a subquery.

## Design

Postgres has two dependency strengths, and the new engine file
`drop.go` is built around them:

- **auto**: part of the target, gone CASCADE or not. A column's indexes
  and CHECKs, the table's own FKs over it.
- **normal**: a separate object using the target. Other FKs referencing
  a dropped table, column or unique index, and views reading a dropped
  table or view, transitively. RESTRICT refuses; CASCADE drops.

Engine API:

- `DropOptions{Cascade, ViewReads, CheckReads}`. Views and CHECKs are SQL
  text the engine does not parse, so the SQL layer passes callbacks. They
  run inside the DDL transaction, so the dependency check and the drop see
  the same catalog. A nil callback means the engine ignores views or
  CHECKs, as before.
- `DropTables(names, opts)`, `DropColumnWith`, `DropIndexWith` and
  `DropViewWith` each run in one `updateDDL`. Outputs are reset at the top
  of the closure, since it retries under concurrent writes. They return
  `[]Dependent{Kind, Name, Table, On}`, and `Dependent.String()` renders
  Postgres's object description.
- The old `DropTable`, `DropColumn`, `DropIndex` and `DropView` wrap the new
  calls with `DropOptions{}`. So `DropColumn` of an indexed column now
  succeeds through the Go API too.
- `ErrDependentObjects` sentinel; `errDependents` builds Postgres's
  message + `detail` + `hint` fields (multi-table drop: the "desired
  object(s)" wording).
- `dropFKDependents(tx, deps, self)` removes FK dependents from their
  tables. A dependent on the table already being rewritten (a
  self-reference) is edited in that same descriptor, since writing a
  fresh copy would be overwritten by the caller's write. A mutation
  check confirmed that.
- `viewDependents` reads every view once through the callback, then does
  a breadth-first search from the roots. A view's `On` is the first
  relation it reads in the previous level, in name order.
- `QuoteIdent` and the reserved-word list moved from `sql/sessinfo.go` to
  the root package (`ident.go`), so engine messages quote like Postgres.
  `sql.quoteIdent` now delegates to it.

SQL layer (`sql/drop.go`, new):

- `dropOpts(cascade)`, `cascadeNotices`, `viewReads` (FROM names at any
  depth, minus every CTE name the query binds: broader than scoping, in
  the direction that never over-reports), `checkReads` (an unparsable
  CHECK fails the drop rather than survive naming a missing column),
  `eachSelect` (a walker over every nested SELECT).
- `execDropTables` moved here and now handles single and multi-table
  DROP alike: IF EXISTS filter, then one `DropTables` call. A missing
  table without IF EXISTS is `table "x" does not exist` with the
  `ErrNoSuchTable` sentinel (42P01).
- Parser: `dropBehavior()` returns bool. `Cascade` fields on
  `DropTable`, `DropView`, `DropColumn`, `DropIndex`, and
  `DropConstraint`, which now also parses a trailing CASCADE/RESTRICT.
- `checkDropColumn` was removed (dead once the CHECK drops with the
  column). `checkColumnUnmentioned` still guards RENAME COLUMN.

pgwire:

- `ErrDependentObjects` maps to 2BP01 by sentinel. The text case widened
  to `because other objects depend on ` (covers "… on them"). The dead
  `an indexed column` text case was removed.
- `noticeBody`: `drop cascades to …` sends NOTICE/00000.
- `errorBody`: a `hint` field now goes out as the HINT (`H`) field. It
  used to be folded into DETAIL as `hint: …`, or dropped beside an
  explicit detail. This changes the rendering of the existing hints too
  (memguard, numeric NaN, ALTER TYPE USING), which is more correct.

## Kept apart from Postgres

- PK column drop stays refused: a bytdb table always has a primary key.
- No column-level view dependencies. bytdb binds a view's columns at run
  time, so a view reading a dropped column fails when queried.
- One NOTICE per cascaded object rather than Postgres's single "N other
  objects" NOTICE with a DETAIL, because a `Result.Notice` is lines.

## Tests

- `drop_test.go` (engine): index key ranges deleted, FK ordinal shift,
  the sentinel, CASCADE return values, reopen, view count, the
  in-set-reference rule, `testCommitErr` atomicity for all four drops, a
  redundant unique index, and `Dependent.String` quoting.
- `sql/drop_cascade_test.go`: every PG16 scenario above with its exact
  notice/message, plus `viewReads` and `checkReads` tables.
- Updated: `TestDropColumnRestrictions`, `TestFKSchemaGuards`,
  `TestDropForms`, `TestSQLCheckConstraints`,
  `TestSQLAddDropConstraint`, `TestDropConstraintUnique` (now also
  CASCADE).
- pgwire: `TestDependencyRefusals` rewritten around the refusals that
  remain, plus an exact DETAIL/HINT check. `TestNoticeSeverities` gained
  the cascade rows. `TestSQLStateMapping` rows were updated.
  `TestGormVerbatim` lost both work-arounds (index-first before
  `DropColumn`, child-first before `DropTable`) and now asserts the index
  and FK are gone.
- Mutations: nine, one per rule (same-column self-FK exclusion,
  transitive views, drop-set FK skip, CTE subtraction, CHECK reader,
  self-descriptor FK removal, index auto-drop, view deps in DropTables,
  the cascade flag). Every one is caught.
- `go vet` clean on both modules. staticcheck v0.8.2 (CI's version,
  installed into the scratchpad because the local one is built with
  go1.23) is clean on both modules. Every module's tests pass.
- `/verify`: `bytdbd` on a scratch file, a pgx client (simple and
  extended protocol) ran every scenario with 0 failures, then a
  restart confirmed the descriptors persisted.

## Docs

`docs/features.md` has a new "Drops and dependencies" section with the
table. The DROP COLUMN, migration-DDL, schema-guard and GORM paragraphs
were rewritten. `docs/gotchas.md` drops the "cannot drop indexed/FK
column" line and adds the three kept differences. README and
`sql/sql.go` grammar now show `[CASCADE | RESTRICT]`.
`pgwire/errors.go`'s 2BP01 comment and the parser's `dropBehavior`
comment were updated.

## Notes

- CI's `pgwire-released-pin` job (GOWORK=off, continue-on-error) will
  fail until the next release, since pgwire now uses
  `bytdb.ErrDependentObjects`. The release bumps the pin, still the
  planned MINOR v0.25.0.
- The local `~/bin/staticcheck` is too old for go1.26 modules. Use
  `go install honnef.co/go/tools/cmd/staticcheck@v0.8.2` into a temp
  GOBIN.

## Next

Closed: N-056. Declined: None. Raised: N-060.
Deferred: None. Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
