# Session: CREATE INDEX IF NOT EXISTS + DROP INDEX IF EXISTS

- Session ID: `057e9ee6-bd66-4c0f-884a-1cb3d3e355db`
- Date: 2026-08-07
- Branch: `main` (started clean at `e848804`, the v0.9.0 pgwire pin bump)

## Trigger

An agent working in another repo reported against v0.9.0:

> "v0.9.0 does add CREATE TABLE IF NOT EXISTS, but I probed the engine
> directly and CREATE INDEX IF NOT EXISTS is still a syntax error, and
> ALTER TABLE ADD PRIMARY KEY is still unsupported."

Both claims verified true against the code. One was a genuine gap (fixed
this session); the other is a deliberate engine constraint (left as-is,
documented below).

## What was done

### 1. `CREATE [UNIQUE] INDEX IF NOT EXISTS` — implemented

Followed the exact split v0.9.0 established for `CREATE TABLE IF NOT
EXISTS`: the parser only records the flag; the executor resolves it by
turning a name collision into a Postgres-style notice.

- `sql/ast.go` — `CreateIndex` gained `IfNotExists bool`.
- `sql/parser.go` `createIndex()` — accepts `IF NOT EXISTS` before the
  index name (same accept-if / expect-not / expect-exists shape as
  `createTable` and `createSequence`).
- `sql/sql.go` `case *CreateIndex` — when the flag is set and the target
  table already has an index of that name, returns
  `relation "<name>" already exists, skipping` as a notice, no error.

Semantics notes (mirroring Postgres where bytdb's model allows):
- Name check ONLY — the requested columns/uniqueness are never compared
  to the existing index's, exactly like the table variant.
- The check is scoped to the target table because bytdb names indexes
  per table, unlike Postgres's schema-wide relation namespace. The same
  name on another table would not have collided anyway, so it creates
  normally.
- A missing table still errors (the engine call reports it), matching
  Postgres — IF NOT EXISTS guards the index name, not the table.

### 2. `DROP INDEX IF EXISTS` — implemented (symmetric gap, also missing)

- `sql/ast.go` — `DropIndex` gained `IfExists bool` (struct expanded
  from the one-liner).
- `sql/parser.go` `dropIndex()` — accepts `IF EXISTS` before the name.
- `sql/exec.go` `execDropIndex()` — notices
  `index "<name>" does not exist, skipping` when:
  - bare form: no table anywhere has the index;
  - `ON table` form: the table is missing OR the table lacks the index.

  Deliberate carve-out: an ambiguous bare name (same index name on two
  tables) STILL errors under IF EXISTS — the clause papers over absence,
  not conflicts it cannot resolve.

### 3. `ALTER TABLE ADD PRIMARY KEY` — confirmed deliberate, not fixed

The parser rejects it explicitly (`sql/parser.go` `alterTable()`, the
`case p.acceptKw("primary")` arm) with "ADD PRIMARY KEY is not
supported". This is structural, not an oversight: the engine physically
keys every row by its encoded primary-key tuple (`engine.go` —
`TableDesc.PKCols`, rows stored under `tablePrefix(id) + pk tuple`).
Adding a PK post-hoc would mean rewriting every row's storage key plus
all secondary index entries — a full table rebuild. If ever wanted, it
is a feature to design (atomic rebuild inside `alterDesc`, FK
implications, uniqueness backfill validation), not a parse fix.

Workarounds stand: declare the PK at `CREATE TABLE`, or
`CREATE UNIQUE INDEX` when uniqueness is all that's needed.

## Tests

- New `TestIndexIfExistsDDL` in `sql/ifexists_test.go` (alongside the
  table variant's test, same style): guarded create on taken name
  notices (even with different columns/uniqueness), unguarded duplicate
  still errors, guarded create on free name is real (unique index from
  a guarded create rejects a duplicate row), per-table name scoping,
  drop notices for missing index / missing ON-table / bare form,
  unguarded drop still errors, ambiguous bare name still errors under
  IF EXISTS, and drop-then-redrop notices.
- Full suite green: `go test ./...` — bytdb, replicate, replicate/s3,
  sql, stdlib, tuple all ok.

## Files touched

| File | Change |
|---|---|
| `sql/ast.go` | `CreateIndex.IfNotExists`, `DropIndex.IfExists` + doc comments |
| `sql/parser.go` | IF NOT EXISTS in `createIndex()`, IF EXISTS in `dropIndex()` |
| `sql/sql.go` | skip-with-notice guard in `case *CreateIndex` |
| `sql/exec.go` | IF EXISTS resolution in `execDropIndex()` |
| `sql/ifexists_test.go` | `TestIndexIfExistsDDL` |

## Open items / follow-ups

- Not tagged: this is unreleased on top of v0.9.0. Next release should
  mention both clauses in README/skill grammar (README grammar section
  not yet updated this session).
- `ALTER TABLE ADD PRIMARY KEY` remains a candidate feature if an app
  migration ever truly needs it (full-rebuild design).
- Pre-existing lint suggestions surfaced by diagnostics (WriteString
  concatenation in window/explain/agg, `slices.ContainsFunc` in
  parser.go, `maps.Copy` in exec.go) — untouched, unrelated.
