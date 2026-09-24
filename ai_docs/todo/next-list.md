# Next list

The project's single list of open follow-ups. Sessions **edit this file in
place**. They don't copy a list forward into each session doc. A session doc's
`## Next` becomes a short summary instead: `Closed: N-… · Raised: N-…`.

Seeded 2026-09-24 by `/next-list seed` from the 15 session docs
`2026-0805-1603` → `2026-0916-1611`, with each item's `raised` traced back
through all 74 docs.

## Conventions

- **IDs are permanent** (`N-001`, `N-002`, …) and never reused, even after an
  item closes.
- **`raised`** is the stem of the session doc where the item first appeared
  (`ai_docs/claude_sessions/<stem>.md`).
- **Age is computed, never stored**: the number of session docs written after
  `raised`. `/next-list` works it out from the directory listing.
- **Value** measures payoff, not effort:
  - **high**: something is being worked around today, or a second independent
    consumer has arrived.
  - **medium**: it blocks one named thing, or it is a visible defect that
    nobody has to route around yet.
  - **low**: a gap nobody has hit, or it depends on something that doesn't
    exist yet.
- **Open** holds what we plan to pick up next. **Roadmap** holds what we want
  to do later, not soon. **Non-goals** holds what we probably won't do.
- **Nothing leaves Open or Roadmap without a line in another section.** A
  finished item moves to Closed with its date and the evidence. A declined
  item moves to Non-goals with the reason. Merged items go to Closed as
  `merged into N-xxx`. Moving an item between Open and Roadmap is fine.
- Open and Roadmap stay in ID order.

**Next ID:** N-018

## Open

- **N-003** · raised `2026-0916-1557-sqlstate-gaps-constraint-catalog` · value low
  **`pg_constraint` omits primary keys and unique constraints.** It lists only
  CHECK (`c`) and FK (`f`) rows (`sql/syscat.go:545-590`). Keys show up through
  `pg_index` instead. `information_schema.table_constraints` lists all four
  kinds. Tools that read `contype IN ('p','u')` would miss keys. Nobody has
  reported this.
- **N-004** · raised `2026-0916-1557-sqlstate-gaps-constraint-catalog` · value low
  **`bad parameter format code` maps to XX000.** Postgres treats an invalid Bind
  format code as a protocol violation (08P01). The error comes from
  `pgwire/values.go:244`, and no case in `pgwire/errors.go` matches it (the
  comment at `errors.go:54` says it is kept out of the `bad <type> parameter`
  rule on purpose). This predates the SQLSTATE work.

## Roadmap

Wanted, but not soon. Promote an item to Open when the thing it waits for
arrives.

- **N-001** · raised `2026-0722-1303-wal-encryption-at-rest` · value low
  **btypedb encryption deferred set:** online key rotation, a
  plaintext↔encrypted migration helper, key+value scope, and
  ChaCha20-Poly1305. None of these exist yet. `btypedb/encrypt.go:40-41`
  reserves flag bits 1..4, and `compact.go:138-139` names the re-encrypt seam.
  Rotation first needs a v3 wrapped-DEK header, because `Compact`'s raw
  tail-copy is invalid across keys. Restated at `2026-0730-1924`,
  `2026-0907-2325`, `2026-0907-2339`, `2026-0916-1511`, `2026-0916-1557` and
  `2026-0916-1611`, and kept open by decision each time. **Trigger:** a
  deployment needs to rotate a key.

## Non-goals

- **N-005** · declined `2026-0730-2350-occ-stage2-sequences`: **Embedded
  Engine one-shot writes surface `ErrTxConflict` raw.** The caller writes the
  retry loop, the same contract as `WriteTxn`. Documented in
  `docs/concurrency.md`.
- **N-006** · declined `2026-0805-1603-correlated-subquery-index-pushdown`:
  **Correlated ON conjuncts and function-wrapped correlated predicates
  evaluate per row.** Full Postgres-style decorrelation was considered and
  rejected. For large outer sets, rewrite as a JOIN.
- **N-007** · declined `2026-0805-1603-correlated-subquery-index-pushdown`:
  **Statement paths that never seed a `subMemo` re-prepare correlated
  subqueries on each invocation.** This errs on the safe side, and they still
  get pushdown within one invocation. It was recorded as "known remaining
  (deliberate)" and never appeared in a Next list. Filed here so it stays
  visibly declined.
- **N-008** · declined `2026-0907-2325-next-list-rebuild-backlog-drain`:
  **Unindexed FK columns scan the child table on each check.** FK enforcement
  is planner-driven and doesn't require an index. The skill's gotchas document
  the workaround ("index the child FK columns").
- **N-009** · declined `2026-0907-2325-next-list-rebuild-backlog-drain`:
  **`WriteString` lint hints in test files.** Test readability is worth more
  than the allocation.
- **N-010** · declined `2026-0916-1511-next-list-rebuild-and-drain`:
  **General multi-action `ALTER TABLE`.** Only the `DROP CONSTRAINT t_pkey,
  ADD PRIMARY KEY` pairing is accepted, because it is the one pairing that must
  be atomic in bytdb. Other actions can run as separate statements.
- **N-011** · declined `2026-0916-1511-next-list-rebuild-and-drain`:
  **`ALTER COLUMN TYPE` on foreign-key columns.** Both sides would have to
  change together. Instead, drop the constraint, alter both columns, and re-add
  it.
- **N-012** · declined `2026-0916-1511-next-list-rebuild-and-drain`:
  **pgwire `v0.10.0` / `v0.11.0` tags stay unbackfilled.** The lockstep tag
  line resumes at `v0.12.0`.
- **N-013** · declined `2026-0916-1557-sqlstate-gaps-constraint-catalog`:
  **Unique indexes vs UNIQUE constraints in `table_constraints`.** bytdb can't
  tell them apart, so every unique index reports as UNIQUE.
- **N-014** · declined `2026-0916-1557-sqlstate-gaps-constraint-catalog`:
  **Synthetic NOT NULL CHECK rows in `table_constraints`.** Nullability is
  already in `information_schema.columns`.
- **N-015** · declined `2026-0916-1611-license-and-release-v0.15.0`:
  **License file in `bench/`.** It is an internal harness module and nothing
  imports it.
## Closed

- **N-002** · raised `2026-0805-1820-benchmark-rerun-m1pro-doc-refresh` ·
  closed 2026-09-24. **`bench/go.mod` pin goes stale on every release.**
  Tidied from v0.14.0 to v0.16.0, and bench now builds with and without
  `GOWORK=off`. The lasting fix is the new `/release` skill
  (`.claude/skills/release/SKILL.md`). It is the first written release
  routine, and step 5 tidies bench right after pgwire's pin bump, so the pin
  follows every release.
- **N-017** · raised in dbc, `2026-09-24` (no bytdb session doc) · closed
  2026-09-24. **Views have no columns in the catalog.** Fixed:
  `pg_attribute` and `information_schema.columns` now list each view's
  output columns, and `information_schema.tables` lists views as `VIEW`. The
  columns come from describing the stored query with `staticView`, the same
  path EXPLAIN and wire Describe use, so nothing executes
  (`DB.viewColumns`, `sql/syscat.go`). A `catalogShapes` flag stops the
  recursion a view over `pg_attribute` would otherwise cause. Tests:
  `TestSystemCatalogViews` and `TestSystemCatalogBrokenView`. Checked over
  the wire with dbc's own column and table queries. dbc's bytdb-specific
  table query (`dbc/db/catalog.go:49`) can now fall back to the generic
  `information_schema.tables` one if wanted.
- **N-016** · raised `2026-0805-1854-occ-truncate-insert-race-flake-fix` ·
  closed 2026-09-24. **Use Go 1.25's `WaitGroup.Go` for the
  `wg.Add(1)`/`go func` pairs in `seq_occ_test.go`.** gopls suggested it, and
  the item never reached a Next list. The rebuild found it already done:
  `seq_occ_test.go` has six `wg.Go(` calls and no `wg.Add(1)` left. They came in with
  commit `6145d14` (the `2026-0907-2325` backlog drain).

Closures before 2026-09-24 are recorded in the session docs' `## Next`
sections, for example
`2026-0916-1611` (release `v0.15.0`) and `2026-0916-1557` (SQLSTATE gaps,
`table_constraints`, bench tidy).
