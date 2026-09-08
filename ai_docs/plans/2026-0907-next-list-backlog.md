# Next-list backlog: draining the accumulated follow-ups

**Status:** phases 1-5 implemented, unreleased
**Scope:** `.claude/skills/`, `ddl.go`, `sql/`, `bench/`, docs
**Date:** 2026-09-07
**Source:** `/next-list 30` over `2026-0719-1746` … `2026-0827-1518`

## Why this plan exists

The rebuild of the Next list over the last 30 session docs turned up two
things worth more than any individual item.

**The list leaks.** No session doc in the window carries a `## Next`
heading. Follow-ups live under a different name every time — `Follow-ups`,
`Notes for next time`, `Known remaining (deliberate)`, `State / next
steps`, `Open items / follow-ups`, `Still absent after v0.11.0`. An item
survives only as long as the next author happens to re-read the previous
doc's differently-named tail, so four items (1, 6, 7, 8 below) fell out
of the record entirely while still open.

**The carry goes stale in the other direction too.** Three items —
btypedb's `Update` recover, pgwire DoS hardening, and the replicate
restore-completeness marker — were *finished* at `2026-0722-0838` and
released, yet were restated as pending at `2026-0730-2036` and again at
`2026-0731-2358` before vanishing. The project memory index repeated the
same dead premise into every new session until it was corrected on
2026-09-07. Verified done in code: `btypedb/tx.go:157` (inside tag
`v0.7.0`, which `go.mod:6` pins), `pgwire/pgwire.go:86-105,156`,
`replicate/restore.go:22-28,80-126`.

So this plan does two jobs: drain the real backlog, and leave behind a
fixed heading so the next rebuild is a two-minute job.

## The work, in execution order

Items keep their `/next-list` numbering (`age` = session docs since first
written, from the newest; `value` = payoff, not effort) so this plan and
the list read against each other.

### Phase 1 — Make the skill tell the truth (item 9, age 1, value high) — DONE

`.claude/skills/bytdb-fast-memory-based-db/SKILL.md` is the first thing a
future session reads, and it currently **denies a shipped feature**:

- Line 228-229 states *"**ALTER TABLE ADD COLUMN with DEFAULT / NOT NULL**
  requires an empty table (no backfill machinery, by design)"*. False
  since v0.10.0 — `ddl.go:189` `AddColumn` backfills inside the
  descriptor-publishing transaction, and `ddl.go:467`/`:501` added
  `SetColumnNotNull`/`DropColumnNotNull` in v0.11.0.
- The DDL summary (lines 92-96) omits `ALTER COLUMN SET/DROP DEFAULT`,
  `SET/DROP NOT NULL`, and the index `IF [NOT] EXISTS` forms shipped in
  v0.9.1.

This subsumes the `2026-0807-0910` item "next release should mention both
clauses in README/skill grammar" — **the README half is already done**
(`README.md:298-299`); only the skill half is outstanding.

Changes:

1. Replace the false gotcha with the accurate rule: `ADD COLUMN` is O(1)
   and rows read NULL; a non-NULL `DEFAULT` backfills at O(rows) and makes
   `NOT NULL DEFAULT x` legal on a non-empty table; `NOT NULL` *alone*
   still needs an empty table.
2. Add a gotcha for the large-table path, mirroring `docs/gotchas.md:156-162`
   — the one-shot form holds every rewritten row live to the commit
   (~+0.7-1 KB transient heap per ~60-byte row), so prefer `ADD COLUMN` +
   `SET DEFAULT` + batched `UPDATE` + `SET NOT NULL`, whose validating scan
   rewrites nothing.
3. Extend the DDL sentence with the ALTER COLUMN forms and `IF [NOT] EXISTS`.
4. Note the narrow DEFAULT vocabulary the engine can evaluate (`default.go`):
   constants, `now()`, `current_date` — anything else *fails the ALTER*
   rather than silently leaving old rows NULL.

**Acceptance:** every DDL statement the parser accepts appears in the skill,
and no sentence in it contradicts `docs/features.md` or `docs/gotchas.md`.

### Phase 2 — Guard the enormous backfill (item 13, age 0, value medium) — DONE

Nothing warns when `ADD COLUMN ... DEFAULT` is about to rewrite a huge
table. The measured cost is ~70 s and **+71 GB live heap** at 100M rows,
all of it retained to the commit and therefore not capped by `GOMEMLIMIT`
— the failure mode is an OOM kill mid-migration, not a slow statement.
The batched recipe that avoids it already exists in
`docs/features.md:95-101` and `docs/gotchas.md:156-162`, so the error has
something concrete to point at.

Design:

- `countRowsUpTo(tx, tableID, limit) int` — an early-exit counter beside
  the existing `hasRows` (`ddl.go:281`), which is the same scan with
  `limit == 1`. Stops at `limit`, so the guard never costs a full scan.
- A threshold on `Engine` (default a package-level constant; **0 disables**,
  matching the `MaxConns`-style idiom already used in pgwire, and settable
  by an option so a caller who means it can proceed).
- Checked in `AddColumn`'s `alterDesc` closure **only when `fill != nil`**
  — the defaultless path stays O(1) and untouched.
- The error names the row count, the threshold, and the batched recipe.

Settled in execution: `DefaultBackfillLimit = 1_000_000`. 1M rows is
~0.7-1 GB of transient heap by the measured figures — high enough that no
ordinary migration trips it, low enough to catch the case the analysis was
written about.

**Acceptance:** a table over the threshold fails the one-shot form with a
message naming the alternative; under it, and with the guard disabled,
behavior is byte-identical to today. Tests for both sides plus the
disabled path.

### Phase 3 — Small consistency fixes (items 6 and 7, age 7 and 6, value low) — DONE

Cheap, real, and both `lapsed` — each fell out of the record while still open.

**Item 6 — `CREATE TABLE IF NOT EXISTS` is table-only.** `sql/sql.go:723`
guards on `d.e.Table(s.Table) != nil`, so `CREATE TABLE IF NOT EXISTS s`
over an existing *sequence* `s` still errors "already exists" from the
engine instead of skipping. Postgres skips on any relation in the
namespace. `Engine.Sequence` (`seqobj.go:178`) and `Engine.View`
(`view.go:73`) already exist, so this is a widened check plus the matching
widening of the sequence and view guards for symmetry.

*Note the trade-off recorded at `2026-0805-1837`:* the current narrowness
was deliberate, chosen to match the sequence guard's sequence-only check.
Widening all three together is what makes it consistent rather than just
differently inconsistent — that is the reason to do it now, and the reason
it must be one change rather than three.

**Item 7 — untested identity shape.** The `2026-0805-1854` conflict
reclassification covers unique *secondary* indexes over identity columns,
but nothing exercises that shape. One targeted test.

### Phase 4 — Housekeeping (items 5 and 8, age 8 and 6, value low) — DONE

**Item 5 — `bench/go.mod` pins `bytdb v0.8.0`** against a repo at v0.11.0.
*The premise recorded at `2026-0805-1820` is partly wrong:* it warned that
`run.sh`'s `go run` step would fail under `set -e` after a version bump,
but `bench/go.mod:67-69` carries `replace => ../` for both bytdb and
pgwire, so the pin is cosmetic and the build already resolves from the
tree. Tidy for tidiness; do not carry the "will break" wording forward.

**Item 8 — lint suggestions, merged.** Raised as two separate notes
(`WaitGroup.Go` at `2026-0805-1854`, the rest at `2026-0807-0910`); they
land together as one sweep: `WriteString` concatenation in `sql/window.go`
(10), `sql/explain.go` (5), `sql/agg.go` (1); `slices.ContainsFunc` in
`sql/parser.go`; `maps.Copy` in `sql/exec.go`; Go 1.25 `WaitGroup.Go` for
the OCC test's `wg.Add(1)`/`go func` pairs. All verified still unapplied.

### Phase 5 — Close the leak — DONE

Add a `## Next` heading to the session-doc convention (`/sess-save`
skill), so follow-ups land in one predictable place instead of six
differently-named tails. This is the item that prevents the next backlog.

## Not scheduled, and why

Kept here rather than dropped, so the next rebuild does not rediscover
them as if new.

- **Item 1 — btypedb encryption deferred set** (age 25): online key
  rotation, plaintext↔encrypted migration helper, key+value scope,
  ChaCha20-Poly1305. Verified still absent (`btypedb/encrypt.go:41`
  reserves flag bits 1..4; `compact.go:139` names the re-encrypt seam).
  Rotation needs a v3 wrapped-DEK header first, because `Compact`'s raw
  tail-copy is invalid across keys. **Becomes `medium` the day a
  deployment needs to rotate a key** — that is the trigger to watch for,
  not a date.
- **Item 10 — `ALTER TABLE ADD PRIMARY KEY`** (age 1): rejected at
  `sql/parser.go:1397`. Rows are physically keyed by the PK tuple, so
  adding one post-hoc rewrites every row key and index entry. Contingent
  on an app migration actually needing it.
- **Items 11, 12 — `ALTER COLUMN TYPE`/`SET STORAGE`, `ADD COLUMN` of an
  identity column** (age 0): `sql/parser.go:1462,1474` accept only the
  four shipped sub-clauses; `ddl.go:196` refuses identity columns
  explicitly. Nobody is blocked on either.

## Delete rather than carry a ninth time

Both are documented design properties with stated workarounds, not work
anyone intends to do. Recording the deletion here is the point — the leak
this plan exists to close is items vanishing *silently*.

- **Item 2 — unindexed FK columns scan the child table per check**
  (age 23). Planner-driven FK enforcement with no index requirement;
  already in the skill's gotchas with the workaround ("index the child FK
  columns").
- **Item 3 — embedded Engine one-shot writes surface `ErrTxConflict` raw**
  (age 18). The caller writes the retry loop, same contract as `WriteTxn`;
  documented in `docs/concurrency.md`.

## Release

Phases 2 and 3 change behavior, so they want a version bump and the
established chain: tag root first, then pgwire, so pgwire's pin resolves.
Phase 1, 4 and 5 are docs/tests/tooling and need no tag.

## Execution notes (2026-09-07)

What the work turned up that the plan did not predict.

**Item 7 needed a different test than the one asked for.** The follow-up
asked for "a targeted test" of the unique-secondary-index
reclassification, implying a deterministic one. There isn't one to write:
an explicitly inserted value bumps the shared in-memory allocator
(`seqalloc.go` `bumpCounterAllocTo`), so a later draw skips past it rather
than colliding — the deterministic route the plan assumed does not exist.
The branch is reachable only through a true race with
`TRUNCATE ... RESTART IDENTITY`, whose cache invalidation is deferred to
commit. Instrumenting the branch showed a first attempt at the test reached
it **zero** times; the shipped version, at 8 writers x 200 inserts against
60 truncates, reaches it on most runs but not all. Its assertions are
therefore invariants (no bare uniqueness error escapes, no duplicate
survives) rather than a demand that the branch fire, which is also why it
cannot flake.

**Every new test was checked against the bug it claims to catch.** The
cross-relation-kind test fails with the pre-fix guard restored; the
backfill-limit test pins the boundary in both directions. Worth keeping up
as a habit — the first version of the item-7 test passed with the
reclassification disabled, which is exactly the test that would have been
committed as coverage while covering nothing.

**Item 8's inventory was larger than recorded.** The `WriteString`
suggestion also fires in four test files
(`window_value_test.go`, `window_frame_test.go`, `window_cover_test.go`,
`misc_cover_test.go`). Left alone: the item named production files, and
test readability is worth more there than the allocation. The `maps.Copy`
site recorded in `exec.go` no longer reports; `slices.ContainsFunc` turned
out to be in `sequence.go`, not `parser.go`, and a `slices.Contains` site
in `engine.go` (`TableDesc.isPK`) surfaced alongside it.

**Phase 5 edited a global file.** `~/.claude/commands/sess-save.md` is
shared across all projects, not scoped to bytdb — the `## Next` convention
now applies to every repo the command is used in. Scope it down if that is
not wanted.
