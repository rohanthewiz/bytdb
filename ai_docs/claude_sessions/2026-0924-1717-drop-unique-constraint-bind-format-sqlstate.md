# DROP CONSTRAINT for unique constraints, and SQLSTATE fixes (N-018, N-004, N-019, N-020)

- **Session:** `71cb07ba-8fb8-41ec-9032-b6cd1a6c4d5e`
- **Date:** 2026-09-24
- **Scope:** `/sl` loaded the previous session doc and the living list, and
  both Open items were re-checked and still valid. Then four items were done
  in order: N-018 and N-004 (the Open list), N-019 (raised while doing
  N-018), and N-020 (raised while doing N-019). The Open list ends empty.

## 1. N-018: `DROP CONSTRAINT` accepts a unique constraint's name

**The gap:** since N-003, `pg_constraint` and `table_constraints` list every
unique index as a UNIQUE constraint. But `execDropConstraint` only knew
checks, FKs and the pkey. `ALTER TABLE t DROP CONSTRAINT t_name_key` failed
with "does not exist", and only `DROP INDEX` worked. A migration tool that
diffs `pg_constraint` would emit the failing form.

**The change** (`sql/check.go`, `execDropConstraint`):

- The lookup order is now check → FK → unique index → pkey hint.
- A unique-index match drops through `Engine.DropIndex`. It therefore keeps
  DROP INDEX's refusal when an FK depends on the index's uniqueness
  (`uniqueKeyCovers` in `index.go`); nothing was duplicated.
- A plain index still gets "does not exist" (a notice with IF EXISTS). The
  catalogs give it no constraint row, and Postgres refuses it the same way.
- Each kind drops in its own engine transaction. Only the first match runs,
  so no statement touches two kinds.

**Test:** `TestDropConstraintUnique` (`sql/sql_test.go`) covers:

- column-level `UNIQUE`, table-level `UNIQUE (a, b)` and
  `CREATE UNIQUE INDEX`, each dropped by name
- the `u` rows gone, the indexes gone from the descriptor, and duplicates
  now accepted
- a plain index refused and left in place, and `IF EXISTS` giving a notice
- the refusal when an FK depends on the index

**Docs:** `docs/features.md` (the UNIQUE sugar bullet, the ALTER bullet and
the schema guards) and the `sql/sql.go` package comment.

## 2. N-004: a bad Bind format code is 08P01

`pgwire/values.go:244` raises "bad parameter format code". Nothing in
`sqlstate` matched it, so it came back as XX000. It now shares the
08P01 (protocol_violation) case with "wrong number of parameters", as
Postgres's "unsupported format code" does.

**Tests:**

- The `TestSQLStateMapping` row now expects 08P01.
- A new case 3 in `TestBindFormatCountMismatchIsProtocolError` sends a raw
  Bind with format code 2. A new helper, `errorCodeUntilReady`, parses the
  ErrorResponse's `C` field. This proves the code end to end, not just the
  mapping function.

## 3. N-019: dependency refusals are 2BP01

N-019 was raised while writing N-018's test: the FK-dependent refusal came
back as XX000. It was filed naming four messages, but a grep found eleven
dependency refusals, and none were mapped:

| Wording | Origin |
|---|---|
| `... because other objects depend on it` | DROP TABLE (`ddl.go`), DROP/RENAME COLUMN a CHECK mentions (`sql/check.go`) |
| `... a foreign key depends on` | DROP INDEX / DROP CONSTRAINT on a unique index (`index.go`), PK replace (`alter.go`) |
| `cannot drop an indexed column` | DROP COLUMN (`ddl.go`) |
| `cannot drop / change the type of a foreign key column` | `ddl.go`, `alter.go` (`refuseFKColumn`) |
| `... referenced by a foreign key` | DROP/RENAME COLUMN, RENAME TABLE, ALTER TYPE |

**Decision: 2BP01 for all eleven**, even where Postgres would allow the
change (Postgres tracks dependencies by oid, bytdb by name). The remedy is
the same everywhere: remove the dependent object, then retry. 0A000 would
tell a client the change can never work. TRUNCATE of a referenced table
says "referenced *in*" and keeps Postgres's own 0A000. The reasoning is in
the comment on the rule in `pgwire/errors.go`.

**Tests:**

- 13 new `TestSQLStateMapping` rows.
- `TestDependencyRefusals` (`pgwire/server_test.go`) triggers 12 refusals
  from real SQL over pgx. It checks both the code and a message fragment,
  so a reordered engine check can't route one refusal through another's
  text.
- One refusal can't be reached from SQL, and has a mapping row only:
  "drop a column referenced by a foreign key" (`ddl.go:438`). A referenced
  column is always a PK or unique-index column, and those checks fire
  first.
- A first attempt used `alter column pid type bigint`. It succeeded as a
  no-op, because int and bigint are the same type in bytdb; the test uses
  `type text` instead.

## 4. N-020: primary-key DDL errors

N-020 was raised while checking N-019's reachability: "cannot drop a
primary key column" was XX000. A pgx probe of every PK-related DDL error
found eight mis-mapped. Postgres's codes were followed:

- **42P16** (invalid_table_definition, the one-key invariant) now also
  covers:
  - dropping a key column
  - DROP NOT NULL on a key column
  - `ADD COLUMN ... PRIMARY KEY`
  - CREATE TABLE with no key
  - the parser's short "multiple primary keys" (the old rule required
    "... for table")
- **42703** (undefined_column) covers `primary key column not declared` and
  `column "x" of relation "t" does not exist` (ADD PRIMARY KEY, ALTER
  COLUMN). The `column "` prefix keeps a constraint's "does not exist" at
  42704.
- **42701** (duplicate_column) covers a column named twice in a key and a
  RENAME onto a taken name.

**A real misroute was fixed:** `create table d (a int, primary key (a, a))`
returned **23505** unique_violation. "duplicate primary key column"
contains the data-conflict wording "duplicate primary key", so a client
would have read a DDL typo as a duplicate row. The 42701 case now comes
before the 23505 case.

**Tests:**

- New `TestSQLStateMapping` rows, including one pinning that a constraint's
  "does not exist" stays 42704.
- `TestPrimaryKeyDDLErrors` (`pgwire/server_test.go`) runs 11 statements
  over pgx, checking code and message.

**Docs:** `docs/features.md` has a 2BP01 note on the schema guards, and
08P01 and 2BP01 were added to the SQLSTATE list.

## 5. Results

`gofmt` and `go vet` are clean. `go test ./...` passes in root and
`pgwire/`. No separate `/verify` run was done: the new pgx tests
(`TestDependencyRefusals`, `TestPrimaryKeyDDLErrors`, and the raw-Bind
case) already drive a real server over the wire.

## Files touched

`sql/check.go`, `sql/sql.go`, `sql/sql_test.go`, `pgwire/errors.go`,
`pgwire/errors_state_test.go`, `pgwire/bind_formats_test.go`,
`pgwire/server_test.go`, `docs/features.md`, `ai_docs/todo/next-list.md`.

## Next

Closed: N-004, N-018, N-019, N-020. Declined: None. Raised: N-019, N-020.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
