# N-033 Linux pty run; N-049 GORM / golang-migrate / atlas capture and fixes

Session: `0df97cd7-6d83-43d0-a135-f68ec255d4d6`. Started from `/clear`,
then `/work-next-list` over the whole Open + Validate sections. This
doc covers the first batch (N-033, N-049). The doc stem was fixed at
16:10 so the list's `raised`/`closed` fields could name it in the N-049
commit; the doc itself was written a few minutes later.

## Triage

| ID | Call |
|---|---|
| N-033 (Validate) | doable for Linux (docker pty); Windows stays |
| N-049 | doable |
| N-052 | doable (exact numeric transcendental fns, `round(int)` type) |
| N-050 | skipped: "measure a real workload before starting" and there is none |
| N-051 | skipped: trigger is a dump or client that writes NaN/Infinity |
| N-053 | skipped: "no consumer has asked"; `money` output format is a product call |

## N-033: the shell through a pty on Linux

- `pgwire/cmd/bytdb/testdata/ptydrive.py` (new, committed) types at the
  interactive shell key by key and checks 23 things: arrow/Ctrl-E/Backspace
  editing, the continuation prompt, history Up and the history file,
  Ctrl-C at the prompt, Tab completion, Ctrl-C cancelling a 4-way
  100M-row cross join, `.shell` and `.edit` children seeing cooked mode
  (the `.edit` "editor" does a canonical-mode `read`, which only completes
  if the handoff really happened), editing after each, Ctrl-D exit 0,
  `stty -g` equal before/after, and box output fitting 40 columns.
- Ran in docker: `golang:1.26.1` (linux/arm64, native) and
  `python:3.13-slim --platform linux/amd64` (emulated): 23/23 on both.
- Driver lessons, recorded in its header:
  - liner redraws the line as `ESC[1G` + prompt + text on every key, so a
    "fresh prompt" must be matched after a newline.
  - The wrapper needs job control (`bash -c 'set -m; …'`). Under plain
    `sh -c`, the mid-query Ctrl-C signals the shared process group, and
    dash exits on the pending SIGINT as soon as bytdb exits. That looked
    like an exit hang at first; it was the harness.
  - `generate_series` in FROM is not supported; the long query is a
    100-row table cross-joined four ways (the table needs a primary key).
- The macOS driver from `2026-1104-1337` was never saved, which is why
  this one is now checked in.
- N-033 stays in Validate for Windows (Windows Terminal and `cmd.exe`).
  Commit `ed56ea5`.

## N-049: capture instead of copy

The item asked for statements copied from the tools' sources. Running the
tools gives the rendered SQL and real argument types. So:

- A scratch module (own `GOMODCACHE` in the scratchpad) pulled gorm
  v1.31.2 + driver/postgres v1.6.3, golang-migrate v4.20.1 and atlas
  v1.3.0, and drove each against a fresh `bytdbd -log-queries`. A pgx
  `QueryTracer` recorded every statement, its args (`%T:%v`) and the
  server's error.
  - lib/pq doesn't go through pgx, so its statements came from the
    server log.
- GORM scenario: AutoMigrate ×4 (create; re-run; model v2 with a wider
  varchar, new columns, a composite index and a field gaining `unique`;
  re-run), CRUD (single and batch create with RETURNING, First/Where,
  Update/Updates, Preload, Joins, Count, Pluck with LIMIT/OFFSET,
  OnConflict upsert, a nested Transaction → SAVEPOINT, soft and hard
  delete, Save of a live row and of a deleted row → id upsert), and the
  Migrator API (HasTable/Column/Index/Constraint, Rename/DropColumn,
  Rename/DropIndex, Drop/CreateConstraint, GetTables, ColumnTypes,
  GetIndexes, DropTable).
- golang-migrate: Up, Version, Steps(-1), Up, Force, Down, Drop on both
  drivers. **Passed untouched.** lib/pq opens transactions with
  `BEGIN READ WRITE`.
- atlas: Open (params probe), InspectRealm (search_path swap, then
  `pg_depend` fails), Lock. Every inspect query was then probed
  individually through the `bytdb` CLI. All fail, each on different
  catalog surface. Recorded as N-055.

### What GORM hit, and the fixes

AutoMigrate on an **existing** table had never worked: the second
AutoMigrate of any GORM app failed.

1. **`information_schema.columns.identity_increment` missing** (42703).
   Added Postgres's identity family in its order (`identity_start`,
   `_increment`, `_maximum`, `_minimum`, `_cycle`) and
   `generation_expression`. Serial-style columns read NULL with
   `identity_cycle` NO, as PG16 does for a bigserial (checked).
2. **`SELECT * FROM t LIMIT $1` in pgx's simple protocol**: pgx 5.13
   inlines it as `LIMIT  '1'`, which bytdb's parser refused.
   `nonNegInt` now takes a string token, trims it and parses it as bigint.
   Errors follow PG: `invalid input syntax for type bigint` (22P02) and
   `LIMIT/OFFSET must not be negative` (2201W/X). PG16 confirmed both.
3. **Unique index vs UNIQUE constraint** (reverses the N-013 non-goal).
   GORM's MigrateColumnUnique saw `uniqueIndex`'s index listed as a
   UNIQUE constraint and issued `DROP CONSTRAINT "uni_users_email"`,
   which doesn't exist → AutoMigrate failed.
   - `IndexDesc.IndexOnly` (`json:"index_only,omitempty"`). Only `CREATE
     UNIQUE INDEX` sets it, through the new `Engine.CreateIndexSpec` /
     `IndexSpec`. The positional `CreateIndex*` forms keep it false.
   - Old descriptors decode as false, so their unique indexes keep reading
     as constraints. That is safe for existing data.
   - `pg_constraint`, `table_constraints`, `key_column_usage`,
     `constraint_column_usage` and `pg_get_constraintdef` skip IndexOnly.
     GORM's own query joins two of those views, so one view's filter could
     hide a gap in another. `sql/orm_ddl_test.go` pins each view on its
     own, and each filter was mutation-checked.
   - `DROP CONSTRAINT` of a bare unique index is still accepted (PG
     refuses with 42704). That keeps `TestDropConstraintUnique`'s
     deliberate behavior. A strict version was tried and backed out.
4. **`ALTER TABLE … ADD [CONSTRAINT n] UNIQUE (cols)`** (GORM's
   CreateConstraint for `unique`). New `AddUnique` statement. Unnamed, it
   takes `t_cols_key`, numbered past a taken name (`freeIndexName`, shared
   with `generatedIndexName`). PG16 gives `n49_c_key1` for the same case.
5. **CREATE TABLE's `CONSTRAINT name UNIQUE`** now keeps the name. It used
   to be dropped for `t_cols_key`. `CreateTable.Uniques` changed from
   `[][]string` to `[]UniqueDef`.
6. **`ALTER INDEX [IF EXISTS] name [ON t] RENAME TO new`**
   (`Engine.RenameIndex`).
   - Descriptor-only, since entries are keyed by index ID.
   - Missing → `relation "x" does not exist` (42P01), as PG; IF EXISTS →
     notice.
   - `t_pkey` → 0A000 "a primary key index cannot be renamed", because
     its name is derived from the table's.
   - A collision → 42P07.
   - `public.`-qualified index names are accepted here and in DROP INDEX
     (`parser.indexName`).
7. **`DROP INDEX` of a missing index**: PG's `index "x" does not exist`
   (42704), previously "no such index" (XX000). The engine's "no such
   index" also maps to 42704.
8. **`attnum = ANY(i.indkey)`** (GetIndexes) failed "malformed array
   literal". `anyElems` now accepts the int2vector text form via
   `parseVectorText`: all-digit, space-separated tokens only. The accepted
   leniency is that `2 = ANY('1 2')` answers true, where PG16 rejects it
   (checked).
9. **Identity `USING`**: GORM re-sends `ALTER COLUMN "age" TYPE smallint
   USING "age"::smallint` on every run, because bytdb stores smallint as
   int8.
   - With USING present, the engine skipped its no-op shortcut and
     rewrote the table and its indexes, so every app start was O(rows).
   - `usingIsIdentity` (sql/alter.go) drops a USING that is `c` or
     `c::T`, where T and the TYPE clause both name the column's current
     type with the same modifiers.
   - A text column needs a text spelling, because `castColType` defaults
     unknown names to text. A `numeric(p,s)` cast must match the column's
     own (p,s).
   - Proven by WAL growth: 0 bytes vs 2240 for a real USING over the
     wire, and the test's 2000-row table. Mutation-checked.
10. CLI `.dump`: a UNIQUE constraint is written as `ALTER TABLE … ADD
    CONSTRAINT … UNIQUE`, and a bare one as `CREATE UNIQUE INDEX`. DESC
    keys stay an index. The round-trip test's dump-equality check shows
    the kind survives.

Seen but left alone:
- GORM re-sends `COMMENT ON COLUMN` each run, because bytdb ignores
  comments and pg_description is empty. Harmless.
- `DropColumn` of an indexed column and parent-first `DropTable … CASCADE`
  are refused (D-54 RESTRICT-only), where PG cascades. That's the user's
  call → N-056.

### Tests

- `pgwire/orm_verbatim_test.go`: `TestGormVerbatim` (the whole life
  above, asserting the fixed points), `TestGolangMigrateVerbatim`,
  `TestAtlasVerbatim` (connect + lock only). Header comment rewritten.
- `sql/orm_ddl_test.go` (new): each catalog view, UNIQUE naming and ADD
  UNIQUE, ALTER INDEX (every refusal), quoted counts, int2vector, the
  identity family, no-rewrite USING.
- `index_test.go` `TestRenameIndexAndIndexOnly`: rename, refusals, and
  IndexOnly plus the renamed index surviving a reopen.
- `pgwire/errors_state_test.go`: 7 new SQLSTATE rows.
- `pgwire/cmd/bytdb/shell_test.go`: the dump round trip includes both
  unique kinds.
- Updated for the new behavior: `TestSQLAddDropConstraint` (ADD UNIQUE no
  longer refused), `TestDropConstraintUnique`, `TestInfoSchemaConstraints`.

### Verification

- `go test ./...` (root and pgwire), `go vet` (both), staticcheck v0.8.2
  (both; installed into the scratchpad because the local one was built
  with go1.23); difftest and bench modules vet clean.
- `/verify` (run after the N-049 commit, which it should have preceded):
  pgx v5.10.0 against a scratch bytdbd, both protocols, 19/19, then a
  restart and 3/3 persistence checks (renamed index, IndexOnly, enforcement).
- difftest against PG16 (`bytdb-numeric-pg` on 55432): 18 passes.
- PG16 checked directly for every behavior the code or docs claim: quoted
  LIMIT, the identity family, table_constraints/pg_constraint for a bare
  unique index, ALTER/DROP INDEX errors and the IF EXISTS notice, ADD
  UNIQUE naming, DROP CONSTRAINT of a bare index (42704), `ANY('1 2')`.

### Also fixed

CI on main had been red since `5c0eca5` (N-047's session doc commit):
staticcheck S1011 in `sql/catmemo_test.go`. Fixed in its own commit,
`3514eae`. The N-033 push had inherited the red.

Commits: `ed56ea5` (N-033), `3514eae` (lint), `ee5df4b` (N-049).

## Docs

`docs/features.md` covers UNIQUE naming and ADD UNIQUE, the catalog
distinction, ALTER INDEX, the identity family, the identity-USING no-op,
and the migration-tool coverage. `docs/gotchas.md`: the ADD UNIQUE row
is replaced by the unsupported ALTER INDEX forms and `UNIQUE USING INDEX`
/ `NULLS NOT DISTINCT`. `docs/cli.md`: dump step 4.

## Gotchas met

- The LSP shows stale "undefined" diagnostics across the workspace after
  edits. `go build` is the truth.
- `p.unexpected` keeps the expected-token text in a field, not in
  `err.Error()`, so tests match `syntax error at or near "x"`.
- A Go shell loop splitting `expr|file` on `|` breaks on a sed pattern
  containing `||`.
- pgwire changes that need new root API make the GOWORK=off
  `pgwire-released-pin` CI job fail until the next release. That job is
  `continue-on-error`.

## Next

Closed: N-049, N-013 (reversed from Non-goals). Declined: None.
Raised: N-055, N-056. Deferred: None. Promoted: None. Moved: None.
Updated: N-033 (Linux done; Windows remains in Validate).
Full list: `ai_docs/todo/next-list.md`.
