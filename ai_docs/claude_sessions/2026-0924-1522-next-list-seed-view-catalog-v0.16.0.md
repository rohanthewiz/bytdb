# Seeding the living next list, catalog columns for views, release v0.16.0

- **Session:** `2e49d96d-5877-407e-901e-061160e6fc76`
- **Date:** 2026-09-24
- **Scope:** `/next-list seed` created `ai_docs/todo/next-list.md`. Then
  N-017 (views have no columns in the catalog) was fixed and released as
  `v0.16.0` / `pgwire/v0.16.0`. Last, bench was tidied and the release routine
  written down as a `/release` project skill (closes N-002).
- **Commits:** `4945218`, `bd95153`, `dedec4e`, `1eacf88` (all pushed).

## 1. Seeding the living list

History mode ran over the last 15 of 74 session docs (`2026-0805-1603` →
`2026-0916-1611`). Each item's `raised` was traced through all 74 docs, so
no age is floored by the window.

- **Open:** N-002 (bench pin), N-003 (`pg_constraint` omits keys),
  N-004 (`bad parameter format code` → XX000). All three premises were
  re-checked against the code and still held.
- **Roadmap:** N-001, the btypedb encryption deferred set (age 30). It is
  seeded there because the docs call it "deferred" and it waits on a
  deployment that needs key rotation.
- **Non-goals:** the ten already-declined items got IDs, each with the doc
  that declined it. One lapsed item is also filed here: N-007 ("statement
  paths that never seed a `subMemo` re-prepare per invocation"). It was
  recorded as deliberate at `2026-0805-1603` but never appeared in a Next
  list.
- **Closed:** N-016, the lapsed gopls hint to use `WaitGroup.Go` in
  `seq_occ_test.go`. It was already done in `6145d14`.

Between turns the user added **N-017** by hand, found while working on
dbc: views have no columns in the catalog.

## 2. N-017: views list their columns (`4945218`)

**Symptom:** dbc's AI schema lookup sent bytdb views to the model with no
columns. `pg_class` listed views under synthetic oids (`viewOID`), but
`pg_attribute` and `information_schema.columns` iterated `userDescs()`
only.

**Change (`sql/syscat.go`):**

- `DB.viewColumns()` returns each view's `{name, cols}` in `Views()` order
  (index i ↔ `viewOID(i)`). The names come back with the columns, so callers
  never read `Views()` twice and risk a shifted list.
- The shapes come from `staticView`, the describe path EXPLAIN and wire
  Describe use, so nothing executes. It works on a private DB copy with
  `vtabs` cleared, so the reading statement's CTEs can't shadow a view's
  base tables. The copy is threaded through the loop, so each dependency
  is described once.
- `pg_attribute` gets one row per view column (`attnotnull` false, as in
  Postgres). `information_schema.columns` lists view columns as nullable
  with no default. `information_schema.tables` now lists views as `VIEW`.
  This is the same gap: dbc had worked around it at
  `dbc/db/catalog.go:49`. The GORM `HasTable` probe filters on
  `BASE TABLE`, so it is unaffected.
- A view whose base table was dropped gets nil columns. It still lists in
  `pg_class`, and the rest of the catalog is unaffected.

**Two problems found while building it:**

1. **Infinite recursion.** Catalog lookups build rows eagerly
   (`syscat.go:59`), so describing a view over `pg_attribute` from inside
   `pg_attribute`'s row builder recursed until the stack overflowed. Fix:
   a new `DB.catalogShapes` field (`sql/sql.go`). On the shapes copy,
   system tables resolve to descriptors with no rows.
2. **Initialization cycle.** The compiler rejected the `sysTables`
   literal, which reached itself through `viewColumns` → `staticView` →
   `lookup` → `sysLookup`. Fix: `viewColumnsFn` is bound to
   `(*DB).viewColumnsImpl` in `init()`, with a comment explaining why.

**Tests:** `TestSystemCatalogViews` covers:

- a view over a table, an aggregate view, and a view over a view
- `information_schema.columns` and `information_schema.tables`
- CTE shadowing
- a view over `pg_attribute`, both listed in the catalog and read

`TestSystemCatalogBrokenView` covers a dropped base table.

A mutation check proved both guards are load-bearing. Dropping the
`catalogShapes` check overflows the stack. Keeping `vtabs` makes the
CTE-shadowing case return `[]`.

**Wire check (`/verify`):** bytdbd on a scratch db, driven by pgx. dbc's
exact bytdb table query, its generic Postgres table query, and its
`pg_attribute` + `format_type` column query all return the view and its
columns. The extended protocol with `$1` works too. Note that
`varchar(40)` shows as `text` in `format_type`. That comes from
`format_type` itself, not from the view work.

**Docs:** `README.md` (catalog list), `docs/features.md` (a new paragraph
on views in the catalog), and the `sql/sql.go` package comment.

## 3. Release v0.16.0

Minor bump, because this adds catalog behavior.

1. `go vet` plus `go test ./...` in root and `pgwire/`: all green.
2. Committed the feature (`4945218`) and the seeded list (`bd95153`).
   Annotated tag `v0.16.0` on `bd95153`, pushed. The proxy resolved it
   right away.
3. `pgwire/go.mod` bytdb `v0.15.0` → `v0.16.0`. Builds with and without
   `GOWORK=off` and tests pass. Committed as `dedec4e`, tagged
   `pgwire/v0.16.0`, pushed. The proxy resolved it.

## 4. Bench tidy and the `/release` skill (`1eacf88`)

- `bench/go.mod`: `go mod tidy` moved the pin v0.14.0 → v0.16.0. Before,
  `GOWORK=off go build` failed with "updates to go.mod needed". Now it
  builds both ways.
- There was no written release routine; it lived only in session docs.
  `.claude/skills/release/SKILL.md` now records it:
  1. choose minor or patch
  2. start clean and current
  3. test root and pgwire
  4. tag root, push, and check the proxy
  5. bump pgwire's pin
  6. tidy bench
  7. build pgwire and bench both ways
  8. one `pgwire, bench: bump bytdb to vX.Y.Z` commit, tag
     `pgwire/vX.Y.Z`, push
  9. record the release

  The bench tidy must come **after** pgwire's bump. Tidy lifts bench's pin
  only because bench requires pgwire, which now requires the new version
  (minimum version selection).

## Files touched

`sql/syscat.go`, `sql/sql.go`, `sql/syscat_test.go`, `README.md`,
`docs/features.md`, `pgwire/go.mod`, `bench/go.mod`,
`.claude/skills/release/SKILL.md` (new), `ai_docs/todo/next-list.md` (new).

## Next

Seeded: N-001–N-016 (this session created the list). Closed: N-002, N-016,
N-017. Declined: None. Raised: N-017 (added by the user from dbc).
Deferred: N-001 (seeded into Roadmap). Promoted: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
