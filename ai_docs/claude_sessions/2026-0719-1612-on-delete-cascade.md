# Session: ON DELETE CASCADE + README/skill refresh

- Session ID: `a3346eb2-5d8f-4adb-85a8-e80b7c7619a4`
- Date: 2026-07-19
- Branch: `main` (started clean at `74f66d9`, jsonb operators already in)

## What was asked

Continue from the jsonb-operators session: assess, then implement, ON
DELETE CASCADE — the last known engine-side app-migration gap. Then
update the README with everything shipped since milestone 29 and
create a project skill `bytdb-fast-memory-based-db`.

## ON DELETE CASCADE design

- **Descriptor** (`engine.go`): `FKDesc.OnDelete` — `""` means NO
  ACTION/RESTRICT (the pre-action meaning, so old descriptors read
  unchanged), `bytdb.FKCascade` (`"cascade"`) is the one action.
  Persisted as `on_delete,omitempty`; deliberately **no
  descFormatVersion bump** — an older reader enforces the stricter
  refuse semantics rather than cascading wrongly. The engine's
  `AddForeignKey` refuses unknown action strings so nothing
  unrecognized is ever persisted (it would later misread as NO
  ACTION). There is no ON UPDATE action: parent key updates always
  refuse while referenced (`fkVerifyReferenced` on the update path is
  untouched and checks cascade constraints too — correct Postgres
  behavior for ON DELETE CASCADE + ON UPDATE NO ACTION).
- **Parser** (`sql/parser.go` `referencesClause`): `ON DELETE
  CASCADE` sets `FKDef.OnDelete`; `ON UPDATE CASCADE` and `SET
  NULL/DEFAULT` stay rejected with targeted messages. `resolveFK`
  carries the action onto the engine descriptor.
- **Execution** (`sql/fk.go` `fkAfterDelete`, called from
  `execDelete` in place of the old inline verify loop): a worklist of
  (table, inbound refs, deleted old-images). For each cascade
  constraint, referencing child rows are found through the ordinary
  planner (`fkDeleteMatching` — equality conjuncts, so an index on
  the child FK columns makes each probe a point get), deleted, and
  the child table re-queued — cascades are transitive. After the
  worklist drains, the NO ACTION/RESTRICT constraints of **every**
  deleted row-set — cascaded ones included — are verified, keeping
  the end-of-statement semantics: a NO ACTION grandchild blocks a
  cascade through its parent; deleting a row plus everything
  referencing it in one statement stays legal. Cascade constraints
  need no final check — the deletion is the enforcement.
- **Termination** through FK cycles and self-references is natural: a
  table re-queues only when rows were actually deleted, rows are
  finite, and revisited scans run against the transaction's own view
  (already-deleted rows are simply not found). Diamond shapes
  (a row reached via two cascade paths) are tolerated the same way.
- **Statement accounting**: RowsAffected and RETURNING report only
  directly targeted rows, matching Postgres — falls out of execDelete
  feeding only top-level rows to those paths.
- **Catalog**: `pg_constraint` gained `confupdtype`/`confdeltype`
  (`'a'`/`'c'`; `' '` on non-FK rows as in Postgres);
  `pg_get_constraintdef` appends ` ON DELETE CASCADE`.
- Shared helpers extracted: `fkRefTuple` (NULL-aware referenced-key
  extraction, now also used by `fkVerifyReferenced`) and
  `fkChildCols`.

## Tests / verification

- `sql/fk_cascade_test.go` (new): basic cascade (RowsAffected/
  RETURNING exclude cascaded rows, NULL-FK child survives), 3-level
  chain, NO-ACTION-grandchild blocks with nothing deleted, mixed
  actions on one parent, self-referencing tree (+ row that is its own
  parent), two-table mutual-cascade cycle (built via nullable FK +
  UPDATE), diamond double-reach, multi-column FK with partial-NULL
  survivor, parent-key UPDATE still refused, parse rejections,
  pg_constraint/constraintdef rows, close/reopen persistence,
  engine-level unknown-action rejection.
- All suites green (root, sql, tuple, pgwire); gofmt + vet clean —
  also fixed pre-existing gofmt drift in `jsonb_test.go`/
  `textarray_test.go` (comment alignment only).
- `/verify` end-to-end: bytdbd on :5439, pgx v5.10.0 client —
  17 checks over simple + extended protocols (cascade via `$1`
  params, RETURNING, SQLSTATE 23503 with connection survival,
  ALTER-added cascade, catalog queries), then a server restart on the
  same db file with 3 more checks (REOPEN PASS: persisted action
  still cascades, confdeltype intact). 20/20.

## README + skill

- `README.md` refreshed: Status now "Milestones 1–29 plus a
  production-readiness sweep" naming FKs/CASCADE, the
  timestamp/date/uuid/jsonb/text[] types, jsonb operators, CTEs/
  views/derived tables, hash joins, LIKE/ILIKE, TRUNCATE, SET/SHOW,
  RENAME, SCRAM. Statement block synced with `sql/sql.go`'s list;
  fixed the stale "now() defaults are rejected" claim; new
  foreign-keys paragraph; FROM paragraph covers CTEs/views/hash
  joins; catalog paragraph lists the real-row tables (pg_constraint,
  pg_attrdef, pg_sequence, pg_stat_activity); pgwire section
  documents SCRAM-SHA-256(-PLUS) over TLS (verified against
  `pgwire/auth.go` before claiming) and bytdbd's `-max-conns`/
  `-sync`; roadmap gained a "Beyond the milestones" checklist.
- `.claude/skills/bytdb-fast-memory-based-db/SKILL.md` (new):
  usage-guide skill with name/description frontmatter — the three
  entry points (engine API, `sql.New`, pgwire) with code, the SQL
  surface and type map, gotchas (single writer, DDL outside blocks,
  empty-table ALTER restriction, index-the-FK-columns advice,
  statement-frozen now(), serr handling), pointer to `/verify`.

## State / next steps

- Modified: `engine.go`, `fk.go`, `sql/{ast,parser,fk,exec,expr,
  syscat,sql}.go`, `README.md`, gofmt on two test files; new:
  `sql/fk_cascade_test.go`, the skill. Committed with this doc.
- The known engine-side app-migration gaps are now all closed.
- Deferred (unchanged): ON UPDATE CASCADE / SET NULL, `#-` and jsonb
  functions, jsonb indexing, MVCC, NUMERIC, non-text arrays.
