# ALTER TABLE ADD COLUMN IF NOT EXISTS, v0.20.0

- **Session:** `7756dc71-29d5-4c96-9dd8-fbeafa58c449`
- **Date:** 2026-10-05
- **Scope:** The user asked whether "bytdb has plain ADD COLUMN but no IF
  NOT EXISTS" was true. It was: `AddColumn` had no flag, and the parser went
  straight from the optional `COLUMN` keyword to `colDef`, so
  `ADD COLUMN IF NOT EXISTS c int` read `if` as the column name and `not` as
  its type, then failed with `unknown column type`. CREATE TABLE, CREATE
  INDEX and CREATE SEQUENCE already took `IF NOT EXISTS`. This session added
  the clause, fixed the duplicate-column SQLSTATE that verification turned
  up, and released both as v0.20.0.

## 1. ADD [COLUMN] IF NOT EXISTS (`cc8ffaa`)

- **`sql/ast.go`**: `AddColumn` gains `IfNotExists bool`.
- **`sql/parser.go`** (`alterTable`, after `p.acceptKw("column")`): the
  clause is claimed only when the current token is unquoted `if` **and**
  `p.tokAt(1)` is unquoted `not`, then `exists` is required. With two tokens
  of lookahead, a column literally named `if` (`ADD COLUMN if int`) still
  parses as a plain add. A real column definition can't have `NOT` where
  the type goes, so nothing valid becomes ambiguous. `IF NOT` without
  `EXISTS` is a syntax error (42601).
- **`sql/sql.go`** (dispatch `case *AddColumn`): when `IfNotExists` is set,
  the executor checks `d.e.Table(s.Table)` and its `ColIndex(name)`. If the
  column exists, it returns
  `Result{Notice: column "c" of relation "t" already exists, skipping}`.
  - **Name check only, as in Postgres.** The existing column's type,
    default and nullability are never compared, and no DEFAULT backfill
    runs.
  - **A missing table falls through** to the engine's `no such table`
    (42P01). The clause guards the column, not the table.
  - **Committed state is correct here.** `isDDL` keeps DDL out of
    transaction blocks, so `Engine.Table` (committed view) is what the add
    would see anyway.
  - **Race window:** a concurrent add of the same name between the check
    and `Engine.AddColumn` comes back as the plain duplicate error. CREATE
    TABLE IF NOT EXISTS has the same window, and the comment says so.
- The executor holds the check, not the engine. That's the same split
  CREATE TABLE and CREATE INDEX use: the parser records the flag and
  dispatch resolves it.

## 2. Duplicate ADD COLUMN reports 42701 (same commit)

- `/verify` over pgx showed a plain duplicate `ADD COLUMN` coming back as
  `ERROR: column already exists (SQLSTATE XX000)`. pgwire's `errors.go`
  maps 42701 (`duplicate_column`) from messages shaped
  `column "…" … already exists`, and RENAME COLUMN already used that wording.
- **`ddl.go`** (`Engine.AddColumn`): the message is now Postgres's
  `column "c" of relation "t" already exists`, with the same serr context
  fields as before. This matters for this feature: a migration that catches
  the duplicate (the pre-IF-NOT-EXISTS workaround, or the race above)
  checks for 42701.

## 3. Tests and docs

- **`sql/ifexists_test.go`**: `TestAddColumnIfNotExists` covers:
  - a taken name with a different type and a DEFAULT (notice; the stored
    value is unchanged);
  - the form without `COLUMN`;
  - the unguarded duplicate still erroring;
  - a free name adding normally, with the DEFAULT backfill;
  - a missing table erroring;
  - a column named `if`;
  - `IF NOT` without `EXISTS` being rejected.
- **`pgwire/server_test.go`** (`TestPrimaryKeyDDLErrors`): new case
  `alter table t add column a text` → 42701 with Postgres's message.
- Docs: the syntax list in `sql/sql.go`, the `README.md` grammar line, a
  bullet in `docs/features.md`, and the IF NOT EXISTS list in the bytdb
  skill's `SKILL.md`.
- `go vet` and `go test ./...` are green at the root and in `pgwire/`.

## 4. Verification (`/verify`)

`bytdbd` was built from the workspace (go.work points pgwire at `../`) and
served a scratch db on `127.0.0.1:5439`. A pgx v5.10.0 client with
`OnNotice` produced:

| statement | result |
|---|---|
| `add column if not exists name int default 7` (name exists) | `ALTER TABLE`, notice `column "name" of relation "t" already exists, skipping` |
| `add if not exists name text` | same notice |
| `add column name text` | `42701 column "name" of relation "t" already exists` (was XX000 before §2) |
| `add column if not exists score int default 5` | `ALTER TABLE`, no notice; row 1 reads `score = 5` |
| `alter table ghost add column if not exists c int` | `42P01 no such table` |
| `add column if int` | `ALTER TABLE`; `"if"` listed as a column |
| `add column if not c int` | `42601 syntax error` |

One more mismatch showed up: the notice arrives at **WARNING** severity.
pgwire's `noticeBody` sends every statement notice as `WARNING`/`01000`,
which is older than this session and also applies to the CREATE TABLE/INDEX/SEQUENCE
skips. It's left as-is and raised as N-025.

## 5. Release v0.20.0

Minor bump because the release adds SQL. Followed the `release` skill:

- Root tag **`v0.20.0`** on `cc8ffaa`. Message: "ALTER TABLE ADD COLUMN IF
  NOT EXISTS; duplicate ADD COLUMN reports SQLSTATE 42701". Pushed, and it
  resolves on proxy.golang.org.
- `pgwire/go.mod` pin `v0.19.0` → `v0.20.0`, then `bench` `go mod tidy`
  raised its pin to `v0.20.0`. `go build ./...` passed with and without
  `GOWORK=off` in both modules, and pgwire tests passed. Committed as
  `f2fd380` "pgwire, bench: bump bytdb to v0.20.0".
- pgwire tag **`pgwire/v0.20.0`** on `f2fd380`. Pushed, and it resolves on
  the proxy.
- No next-list item was waiting on this release.

## Files touched

- `ddl.go`.
- `sql/ast.go`, `sql/parser.go`, `sql/sql.go`, `sql/ifexists_test.go`.
- `pgwire/server_test.go`, `pgwire/go.mod`, `bench/go.mod`.
- `README.md`, `docs/features.md`,
  `.claude/skills/bytdb-fast-memory-based-db/SKILL.md`.
- `ai_docs/todo/next-list.md` (N-025).

## Next

Closed: None. Declined: None. Raised: N-025.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
