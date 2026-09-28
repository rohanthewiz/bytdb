# On-demand compaction: Engine.Compact and SQL VACUUM, v0.19.0

- **Session:** `3238ce20-68a9-4c89-97e4-29ae8d2d1502`
- **Date:** 2026-09-28
- **Scope:** The user asked whether "the bytdb engine's public API doesn't
  expose a compact call" was true. It was. btypedb has `DB.Compact()`, but
  `Engine` kept its handle in the unexported `kv` field and had no
  passthrough. Only auto-compaction was reachable, tuned through the
  `btypedb.Option`s that `bytdb.Open` forwards. This session added
  `Engine.Compact`, then a SQL `VACUUM` statement on top of it, and released
  both as v0.19.0.

## 1. Engine.Compact (`54fa334`)

- **`engine.go`**: `func (e *Engine) Compact() error` sits beside `Backup`.
  It calls `e.kv.Compact()` and wraps any error with `serr` (`op: engine
  compact`), following `Backup`'s pattern. The doc comment covers:
  - **When to call it:** after a bulk delete or dropping a large table,
    before a `Backup` (to ship the smallest file), or with auto-compaction
    turned off.
  - **Concurrency and crash safety:** writers pause twice, briefly, and a
    crash leaves either the old log or the new one.
  - **Replication cost:** each call bumps `LogEpoch`, so the replicator rolls
    to a new generation and re-ships the whole compacted file.
- **`compact_test.go`** (new):
  - `TestEngineCompact` opens with `WithAutoCompactDisabled()` so no
    background pass can race the size and epoch checks. It also uses
    `WithSyncNever()`, because the default fsync per write made the 1000-op
    churn take 4.5s. The test inserts 500 rows and deletes 498, compacts,
    then checks that `LogBytes` shrank and `LogEpoch` rose. It then writes
    one more row, closes, reopens and checks that all 3 rows came back.
  - `TestEngineCompactClosed` checks that `Compact` on a closed engine
    returns an error.

## 2. SQL VACUUM (`54fa334`)

- **Grammar** (`sql/parser.go`, `vacuumStmt`): both of Postgres's option
  spellings are accepted.
  - Legacy form: `[FULL] [FREEZE] [VERBOSE] [ANALYZE]`. Sequential `acceptKw`
    calls enforce Postgres's fixed keyword order. A keyword out of order
    falls through to the table list and fails there.
  - Parenthesized form: `(option [value], ...)`. Each value is skipped as a
    single token, so options Postgres adds later still parse without a
    parser change.
  - The options are discarded, since they control Postgres's heap and
    visibility-map machinery, which bytdb doesn't have.
  - Column lists (`VACUUM ANALYZE t (a, b)`) are deliberately rejected. They
    only scope ANALYZE statistics, which bytdb doesn't gather.
- **AST** (`sql/ast.go`): `Vacuum{Tables []string}`.
- **Execution** (`sql/exec.go`, `execVacuum`):
  - bytdb has one log file, so compaction is always whole-file.
  - A named user table or system catalog is vacuumable.
  - A view or sequence gets Postgres's `skipping "x" --- cannot vacuum
    non-tables or special system tables` warning, joined into
    `Result.Notice`.
  - An unknown name returns `no such table` (42P01) before any work starts.
  - A list made up only of skipped relations compacts nothing.
  - An already-canceled `ctx` stops the compaction from starting, because it
    can't be interrupted once it begins.
  - It refuses to run when `d.tx != nil`, as a guard for any caller that
    bypasses the Session check.
- **Session** (`sql/session.go`): inside a block, `VACUUM` fails with
  Postgres's exact message, `VACUUM cannot run inside a transaction block`,
  and aborts the block. The existing pgwire mapping turns that into SQLSTATE
  `25001`. VACUUM isn't DDL, so it has its own check just before `isDDL`.
- **Command tag** (`sql/describe.go`): `VACUUM`. **Dispatch**:
  `sql/sql.go`.
- **Tests** (`sql/vacuum_test.go`): the parser accepts and rejects the
  expected inputs; compaction is shown by the epoch bump, the log shrinking
  and the rows surviving; name resolution covers tables, pg_catalog, view
  and sequence skips and unknown names (no compaction on failure); VACUUM
  inside a block is rejected. There is no `generate_series`, so the test
  builds its 500-row INSERT in Go.
- **Wire check** (`verify` skill: `bytdbd` plus a pgx client): the plain
  `VACUUM` tag was `VACUUM` and the file went from 252,290 to 479 bytes
  after 2000 inserts and 1998 deletes. `VACUUM (VERBOSE) t` over the
  extended protocol succeeded. `VACUUM v` sent the skip notice.
  `VACUUM nosuch` returned `42P01`. VACUUM inside `BEGIN` returned `25001`.

## 3. Release

- Root **`v0.19.0`** on `54fa334`. Minor version: it adds new API
  (`Engine.Compact`) and new SQL (`VACUUM`). The proxy resolved it.
- `4e02f2c` **pgwire, bench: bump bytdb to v0.19.0** (pgwire pin; `bench`
  tidied to match). Both modules built with and without the workspace, and
  pgwire's tests passed.
- **`pgwire/v0.19.0`** on `4e02f2c`. The proxy resolved it.
- Nothing on the next-list was waiting on this release.

## 4. Docs

After the release, `README.md` (statement grammar), `docs/features.md` (the
section became "TRUNCATE, VACUUM, SET, SHOW", with semantics), and
`docs/gotchas.md` were updated. The gotchas entry had told readers to
"call `Compact()` yourself", which wasn't possible through bytdb before this
session. It now names `btypedb.WithAutoCompact` /
`WithAutoCompactDisabled` and `Engine.Compact()` / `VACUUM`. The
`bytdb-fast-memory-based-db` skill's feature list was updated too. These doc
changes are committed after the tags, so the v0.19.0 README on pkg.go.dev
doesn't show the VACUUM grammar line yet.

## Files touched

- `engine.go`, `compact_test.go` (new).
- `sql/ast.go`, `sql/parser.go`, `sql/exec.go`, `sql/sql.go`,
  `sql/describe.go`, `sql/session.go`, `sql/vacuum_test.go` (new).
- `pgwire/go.mod`, `bench/go.mod`.
- `README.md`, `docs/features.md`, `docs/gotchas.md`,
  `.claude/skills/bytdb-fast-memory-based-db/SKILL.md`.

## Next

Closed: None. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
