# CREATE TABLE IF NOT EXISTS / DROP TABLE IF EXISTS

Session: 00387645-91d0-4881-bb15-cd5601b121ee
Date: 2026-08-05

## What happened

Another agent noticed bytdb did not parse `CREATE TABLE IF NOT
EXISTS`. Confirmed: only `CREATE SEQUENCE` / `DROP SEQUENCE` /
`DROP VIEW` / `ALTER TABLE ... DROP CONSTRAINT` carried the guard
clauses — tables were the odd one out, and `DROP TABLE IF EXISTS` was
missing too. Implemented both, mirroring the sequence implementation
(the closest precedent, `sql/sequence.go` execCreateSequence /
execDropSequence).

## Changes

- `sql/ast.go` — `CreateTable` gains `IfNotExists bool`; `DropTable`
  goes from `struct{ Table string }` to `{Table string; IfExists
  bool}`.
- `sql/parser.go` — `createTable()` accepts `IF NOT EXISTS` before
  `tableName()` (same accept/expect chain as `createSequence`); the
  `DROP TABLE` branch of `statement()` accepts `IF EXISTS`, same
  shape as the `DROP VIEW` branch below it.
- `sql/sql.go` executor —
  - `*CreateTable`: early `&Result{Notice: 'relation "t" already
    exists, skipping'}` when guarded and `d.e.Table()` hits. Name
    check only, as in Postgres: the existing table's schema is never
    compared to the statement's column list.
  - `*DropTable`: guarded + `d.e.Table() == nil` → `&Result{Notice:
    'table "t" does not exist, skipping'}`. Resolved in the SQL layer,
    not the engine, so a guarded drop of a *present* table still
    surfaces real engine errors (e.g. a dependent foreign key).
- Docs: grammar listings updated in the `sql` package doc comment and
  README "What's supported".

## Tests / verification

- `TestParseTableIfExists` (sql/parser_test.go): flags set on the
  guarded forms, off on the plain forms; half-written clauses
  (`create table if exists ...`, `drop table if t`) are syntax
  errors, not a table named `if`.
- `TestTableIfExistsDDL` (sql/ifexists_test.go, new): end-to-end —
  guarded create on a taken name notices and leaves existing rows
  untouched; unguarded duplicate still errors; guarded drop notices
  on a missing table, really drops a present one; a dropped name is
  free again for the guarded create.
- Wire-level (/verify skill): bytdbd + pgx client on :5439 — both
  notices arrive as NoticeResponse (pgwire/conn.go already forwards
  `Result.Notice` on both the simple and extended paths, so tables
  got notice delivery for free), extended protocol Describe/bind
  works against a guarded-created table.
- Full `go test ./...` green.

## Notes / known remaining

- `TestOCCTruncateInsertRace` (engine package, seq_occ_test.go:582
  "duplicate primary key") failed once during the first full-suite
  run, before passing 5/5 in isolation and on suite re-run.
  Pre-existing flake, unrelated (this session only touched sql/ and
  docs) — worth a look in a future session.
- The guarded create's existence check is table-only (`d.e.Table`),
  matching the sequence guard's sequence-only check: `CREATE TABLE IF
  NOT EXISTS s` over an existing *sequence* `s` still errors
  "already exists" from the engine rather than skipping. Postgres
  skips on any relation; edge case, deliberately left consistent
  with the existing sequence behavior.
- A table literally named `if` now needs quoting (`create table "if"
  ...`) — same trade-off the sequence forms already made.
