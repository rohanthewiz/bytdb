# Next-list rebuild, and draining the backlog it exposed

- **Date:** 2026-09-07
- **Session:** https://claude.ai/code/session_01DmK8b5a4wxeXmcHCGJPoLH
- **Scope:** `/next-list 30` over the session-doc history, then a plan
  (`ai_docs/plans/2026-0907-next-list-backlog.md`) and its execution across
  `.claude/skills/`, `ddl.go`, `engine.go`, `sql/`, `bench/`, docs.

## Ask

`/next-list 30` — rebuild the open follow-up list from the last 30 session
docs rather than carrying it forward, then "create a plan from the Next list
items in ai_docs/plans/ and begin execution".

## What the rebuild found

Two structural problems, both worth more than any single item on the list.

### The list leaks

**No session doc in the 30-doc window carries a `## Next` heading.** Items
live under a different name every time — `Follow-ups`, `Notes for next
time`, `Known remaining (deliberate)`, `State / next steps`, `Open items /
follow-ups`, `Still absent after v0.11.0`. An item survives only as long as
the next author happens to re-read the previous doc's differently-named
tail. Four open items (the encryption deferred set, the `IF NOT EXISTS`
edge case, the untested identity shape, the lint sweep) had fallen out of
the record entirely while still open.

### The carry also goes stale in the other direction

Three items — btypedb's `Update` recover, pgwire DoS hardening, and the
replicate restore-completeness marker — were **finished at
`2026-0722-0838`** and released, yet were restated as pending at
`2026-0730-2036` and again at `2026-0731-2358` before vanishing. Verified
done in code:

| item | evidence |
|---|---|
| `Update` recover | `btypedb/tx.go:157`, inside tag `v0.7.0`, which `go.mod:6` pins |
| pgwire DoS | `pgwire/pgwire.go:86-105,156` (`DefaultMaxConns`, `DefaultIOTimeout`, `WriteTimeout`) |
| restore-completeness | `replicate/restore.go:22-28,80-126` (manifest marker + `ErrIncompleteReplica`) |

The project **memory index** was the mechanism keeping the dead premise
alive — its one-line hook still read "pending btypedb.Update recover +
pgwire DoS + replicate restore-completeness", re-injecting it into every
new session. Corrected this session (the memory *body* was already right;
only the index line was stale).

Two more premises were dead on inspection:

- **The church-repo item (age ≥ 29) is fully closed.** `church/church`
  is on `bytdb v0.9.1`, `BETWEEN` is back in the recurrence CHECK
  constraints, and `$n` placeholders are back in the chat ×2
  (`resource/chat/queries.go:47,52`) and prayerwall ×1
  (`resource/prayerwall/prayerwall.go:97`) LIMIT/OFFSET queries.
- **GoNotes** was recorded as "needs a bump to v0.10.0, pinned at v0.6.4";
  it is already on `bytdb v0.11.0`.

## The rebuilt list

13 open items, sorted by age. One `high` (the stale skill), one `medium`
(the backfill guard), eleven `low`. Two of the `low` ones were marked as
deletion candidates rather than a ninth carry.

## What was implemented

### Phase 1 — the skill was denying a shipped feature (item 9, high)

`.claude/skills/bytdb-fast-memory-based-db/SKILL.md` line 228-229 read
*"**ALTER TABLE ADD COLUMN with DEFAULT / NOT NULL** requires an empty
table (no backfill machinery, by design)"* — false since v0.10.0. The skill
is the first thing a future session reads, which is what made this the
highest-value item on a list where everything else was `low`.

Replaced with the accurate rule (O(1) without a DEFAULT; O(rows) with one;
`NOT NULL` alone still needs an empty table), plus the large-table batched
recipe, the narrow DEFAULT vocabulary `default.go` can evaluate, and the
DDL forms the summary omitted.

Drafting the grammar line caught an overclaim in my own first version:
there is **no `CREATE VIEW IF NOT EXISTS`** — views take `CREATE OR
REPLACE`. Checked against `sql/ast.go`, the real set is `IF NOT EXISTS` on
CREATE TABLE/INDEX/SEQUENCE and `IF EXISTS` on DROP
TABLE/INDEX/SEQUENCE/VIEW/CONSTRAINT.

This subsumed the `2026-0807-0910` item about README/skill grammar — the
README half was already done (`README.md:298-299`).

### Phase 2 — a guard on the enormous backfill (item 13, medium)

Nothing warned when `ADD COLUMN ... DEFAULT` was about to rewrite a huge
table. The cost is **live** memory held to the commit (~+0.7-1 KB per
~60-byte row, ~71 GB at 100M rows), so `GOMEMLIMIT` cannot cap it: the
failure mode is an OOM kill part-way through a migration, not a slow
statement.

- `DefaultBackfillLimit = 1_000_000` (~0.7-1 GB by the measured figures).
- `Engine.backfillLimit atomic.Int64` + `SetBackfillLimit` /
  `BackfillLimit`. `0` or less disables — the same "negative means no
  limit" idiom pgwire's `MaxConns` uses.
- `countRowsUpTo(tx, tableID, limit)` — early-exit counter; `hasRows` now
  delegates to it with `limit == 1`, so there is one scan implementation.
- Checked in `AddColumn`'s `alterDesc` closure **only when `fill != nil`**,
  counting to `limit+1` so "exactly at the cap" stays allowed and the
  guard costs a bounded scan even on a huge table.
- `backfillTooLargeErr` names the batched alternative, not just the
  refusal.

Docs: `docs/gotchas.md` and `docs/features.md` gained the cap and the knob.

### Phase 3 — the `IF NOT EXISTS` guards were narrower than the namespace

The engine enforces **one relation namespace** across tables, sequences and
views — `CreateTable`, `CreateSequence` and `CreateView` each reject a name
held by any of the three. But the SQL-layer guards each consulted only
their own kind, so `CREATE TABLE IF NOT EXISTS s` over an existing sequence
`s` still failed with "already exists" — precisely the error the clause
promises to swallow.

Added `DB.relationExists` (tables + sequences + views; indexes
deliberately excluded, since bytdb scopes index names to their table) and
widened the CREATE TABLE and CREATE SEQUENCE guards together.

`sql/ifexists_test.go` `TestIfNotExistsCrossRelationKind` covers both
directions across all three kinds. **Verified it fails with the pre-fix
guard restored** — it is not a vacuous test.

### Phase 3b — the untested identity shape went differently than planned

`dml.go` reclassifies a draw collision as `ErrTxConflict` in two places:
the PK probe (`:123`) and the unique-index probe (`:143`). Only the first
had a test.

The first race test I wrote **reached the branch zero times** — found by
instrumenting it with a counter, not by reading it. It would have shipped
as coverage covering nothing. Worth keeping up as a habit.

A deterministic test turned out to be impossible: an explicitly inserted
value bumps the *shared* allocator (`seqalloc.go` `bumpCounterAllocTo`
sets `a.next = next` when `next < a.watermark`), so a later draw skips past
it rather than colliding. Only a true race with
`TRUNCATE ... RESTART IDENTITY` — whose cache invalidation is deferred to
commit — re-issues a live value.

Shipped `TestOCCTruncateInsertRaceUniqueIndex`: 8 writers × 200 inserts
against 60 truncates, PK caller-supplied and distinct per writer so any
collision observed can only be the index's. Instrumentation showed it
reaching the branch on most runs but not all, so its assertions are
invariants (no bare uniqueness error escapes, no duplicate survives) rather
than a demand that it fire — which is also why it cannot flake.

### Phase 4 — housekeeping

- `bench/go.mod` pin v0.8.0 → v0.11.0. The recorded premise was partly
  wrong: `replace => ../` (lines 67-69) means the pin is cosmetic and the
  build already resolved from the tree.
- Lint sweep: `WriteString` concatenation in `sql/window.go` (6 sites),
  `sql/explain.go` (3), `sql/agg.go` (1); `slices.ContainsFunc` in
  `sql/sequence.go`; `slices.Contains` for `TableDesc.isPK` in
  `engine.go`; six `wg.Add(1)`/`go func` pairs → `wg.Go` in
  `seq_occ_test.go`.
- Two recorded sites had drifted: `maps.Copy` in `exec.go` no longer
  reports, and `ContainsFunc` was in `sequence.go`, not `parser.go`. The
  `WriteString` hint also fires in four test files — left alone, since the
  item named production files and test readability is worth more there.

### Phase 5 — closing the leak

`~/.claude/commands/sess-save.md` now requires a section headed exactly
`## Next`, carried forward minus what the session finished, with deliberate
non-goals marked rather than dropped. **This is a global command file** —
the convention applies to every repo, not just bytdb.

## Verification

- `go build ./...`, `go vet ./...`, `gofmt -l .` clean.
- Full suite green: bytdb, sql, replicate, replicate/s3, stdlib, tuple.
- `-race` green on the two packages that changed behavior (bytdb, sql).
- New tests: `TestAddColumnBackfillLimit`,
  `TestDefaultBackfillLimitIsSet` (`default_backfill_test.go`),
  `TestIfNotExistsCrossRelationKind` (`sql/ifexists_test.go`),
  `TestOCCTruncateInsertRaceUniqueIndex` (`seq_occ_test.go`).
- 14 files changed, +410/−60.

## Files touched

`.claude/skills/bytdb-fast-memory-based-db/SKILL.md`, `bench/go.mod`,
`ddl.go`, `default_backfill_test.go`, `docs/features.md`,
`docs/gotchas.md`, `engine.go`, `seq_occ_test.go`, `sql/agg.go`,
`sql/explain.go`, `sql/ifexists_test.go`, `sql/sequence.go`, `sql/sql.go`,
`sql/window.go`, and new `ai_docs/plans/2026-0907-next-list-backlog.md`.
Outside the repo: `~/.claude/commands/sess-save.md`, and the project memory
index.

## Next

- **Release phases 2 and 3.** Both change behavior (the backfill guard
  refuses statements that used to run; `IF NOT EXISTS` now skips where it
  used to error), so they want a version bump: tag root first, then
  pgwire, so pgwire's pin resolves. Nothing is tagged.
- **btypedb encryption deferred set** (first written `2026-0722-1303`,
  restated `2026-0730-1924`): online key rotation, plaintext↔encrypted
  migration helper, key+value scope, ChaCha20-Poly1305. Verified still
  absent — `btypedb/encrypt.go:41` reserves flag bits 1..4,
  `compact.go:139` names the re-encrypt seam. Rotation needs a v3
  wrapped-DEK header first, because `Compact`'s raw tail-copy is invalid
  across keys. **Low until a deployment needs to rotate a key** — that is
  the trigger to watch for, not a date.
- **`ALTER TABLE ADD PRIMARY KEY`** — rejected at `sql/parser.go:1397`.
  Rows are physically keyed by the PK tuple, so adding one post-hoc
  rewrites every row key and index entry. Contingent on an app migration
  actually needing it.
- **`ALTER COLUMN TYPE` / `SET STORAGE`, and `ADD COLUMN` of an identity
  column** — `sql/parser.go:1462,1474` accept only the four shipped
  sub-clauses; `ddl.go:196` refuses identity columns explicitly. Nobody
  is blocked on either.
- **Decide whether Phase 5's `## Next` convention should be global.** It
  currently lives in `~/.claude/commands/sess-save.md` and applies to
  every project; scoping it to bytdb is a one-file move if that is not
  wanted.

### Deliberate non-goals (declined, not dropped)

- **Unindexed FK columns scan the child table per check.** Planner-driven
  FK enforcement with no index requirement; documented in the skill's
  gotchas with the workaround ("index the child FK columns"). A design
  property, not a task.
- **Embedded Engine one-shot writes surface `ErrTxConflict` raw.** The
  caller writes the retry loop, same contract as `WriteTxn`; documented
  in `docs/concurrency.md`.
- **Correlated ON conjuncts and function-wrapped correlated predicates
  evaluate per row.** Full Postgres-style decorrelation was considered
  and rejected at `2026-0805-1603`; rewrite as JOIN for large outer sets.
- **`WriteString` lint hints in test files.** Test readability is worth
  more than the allocation.
