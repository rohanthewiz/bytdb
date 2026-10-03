# Next list: restore 12 deleted items, guard `/sess-save` against it

- **Session:** `57376956-2b8b-4e8f-99c1-7d068103bfe2`
- **Date:** 2026-10-03
- **Scope:** No code change. The user ran `/next-list seed`, which was refused
  because `ai_docs/todo/next-list.md` already exists. They then ran
  `/next-list 30` (living-list mode, with the transition check over the last
  30 session docs). That run found 12 items deleted from the list, and the
  user then asked for a check in `/sess-save` so it can't recur. With Open
  empty, the user then asked for gaps in v0.19.0's Compact/VACUUM; two were
  filed (section 4).

## 1. The lapse: `dd7a25d` deleted Roadmap and Non-goals

The L3 history scan collected every ID that ever appeared in
`next-list.md` and checked each against the current file. N-001 and
N-005–N-015 were missing. Walking the file's eight commits showed where:

| Commit | IDs | Sections |
|---|---|---|
| `3b603e9` (file lock, v0.17.0) | N-001–N-022 | Open, Roadmap, Non-goals, Closed |
| `dd7a25d` (close N-022) | N-002–4, N-016–22 | Open, Closed |

`dd7a25d` was meant to move N-022 from Open to Closed. It rewrote the file
instead (17 insertions, 69 deletions), and the Roadmap and Non-goals sections
went with it, with no closing line for any of the 12 items. `a1dfd3b` and the
`2026-0928-1404` Compact/VACUUM doc then carried the damaged file forward, so
three sessions read "nothing pending" where one item was parked and eleven
were declined.

**Restored** (working tree, committed with this doc):

- **N-001** (btypedb encryption deferred set) back into **Roadmap**, with the
  Roadmap intro. The skill's rule is "restore to Open", but `2026-0924-1522`
  parked it in Roadmap by decision, and promotion is the user's call, so it
  went back where it was. Its text gained a note naming `dd7a25d`.
- **N-005–N-015** back into **Non-goals**, verbatim as of `3b603e9`, under a
  one-line note naming `dd7a25d`.

After the restore: 22 distinct IDs, no duplicates, **Next ID** N-023 above
the highest.

## 2. Premise checks

- **N-001** (Roadmap, light pass): still not started. `btypedb/encrypt.go:41`
  reserves flag bits 1..4, `compact.go:139` names the re-encrypt seam, and
  no rotation or ChaCha20 code exists. Its trigger (a deployment needing key
  rotation) hasn't arrived, so it isn't ready for promotion.
- **Transition check, docs 51–80:** docs 51–69 predate the `## Next` heading
  and keep follow-ups under other names ("Known remaining", "Notes for next
  time", "Open items / follow-ups", "Follow-ups / notes"), so those sections
  were read too. Every candidate in them was already done:
  - `CREATE TABLE IF NOT EXISTS` over a sequence (`0805-1837`):
    `relationExists` now checks the whole relation namespace
    (`sql/sql.go:757`).
  - Unique index over an identity column, untested (`0805-1854`):
    `TestOCCTruncateInsertRaceUniqueIndex` (`seq_occ_test.go:744`).
  - README grammar for the index `IF [NOT] EXISTS` clauses (`0807-0910`):
    `README.md:334-335`.
  - Non-test lint sites (`0807-0910`): drained in `2026-0907-2325`.
  - GoNotes bump past v0.10.0 (`0827-1518`): its `go.mod` pins v0.11.0.
  - ALTER COLUMN TYPE, SET STORAGE, identity ADD COLUMN, ADD PRIMARY KEY
    (`0907-2325`): shipped by v0.14.0. SET STORAGE parses as a no-op
    (`sql/parser.go:1473`).
  - Whether the `## Next` rule stays global (`0907-2325`): decided in
    `2026-0916-1511`.
- **Not filed:** the "fast default" alternative to the O(rows) `ADD COLUMN
  ... DEFAULT` backfill (`0827-1518`). `2026-0907-2325` met the same problem
  with `DefaultBackfillLimit` instead, so it was resolved by another remedy.
  It's recorded nowhere now; offered to the user as a Roadmap candidate.
- Session-doc summaries cross-check clean: every Raised ID exists, every
  Closed one is in Closed.

## 3. `/sess-save` ID-loss guard

`~/.claude/commands/sess-save.md` gained a pre-commit check for projects with
a living list (skipped when the file isn't in `HEAD` yet):

    f=ai_docs/todo/next-list.md
    ids() { grep -oE '\*\*N-[0-9]{3}\*\*' | tr -d '*' | sort -u; }
    comm -23 <(git show HEAD:$f | ids) <(ids < $f)                 # IDs lost
    diff <(git show HEAD:$f | grep '^## ') <(grep '^## ' $f)       # headings
    grep -oE '\*\*N-[0-9]{3}\*\*' $f | sort | uniq -d               # duplicates

IDs may move between sections but never disappear; no section heading may be
removed, even an empty one; no duplicates; **Next ID** above the highest. On a
failure it restores from `HEAD`, reruns, reports, and doesn't commit until
clean. `/sess-wrap` and `/sw` inherit it through `/sess-save`.

Verified by replay: run against `3b603e9` → `dd7a25d`, it prints all 12 lost
IDs and the two removed headings. Run against today's working tree, it prints
no lost IDs and only the two restored headings as additions.

The file also ends with `<!-- Last modified: 2026-10-03 ... -->`. It went at
the bottom because the command has no frontmatter and its menu description
comes from the first line. This session's own `/sess-save` ran the new check
on itself before committing.

## 4. Gaps in Compact/VACUUM (after the first commit)

The list was empty, and the user asked for gaps in v0.19.0's Compact and
VACUUM. Filed two:

- **N-023 (medium):** compaction's writer pause grows with database size,
  because btypedb fsyncs the whole snapshot inside phase B under `db.mu`.
  Measured with a scratch probe (scratchpad only, not committed): worst
  writer stall 18 ms at 58 MB, 38 ms at 220 MB, 117–185 ms at ~920 MB.
  Syncing the snapshot before phase B brought the last one to 56 ms.
- **N-024 (low):** nothing says whether a VACUUM is worth running or what it
  reclaimed: no garbage estimate in `Stats`, and `VACUUM VERBOSE` is
  discarded.

Checked and not filed:

- VACUUM in a `BEGIN` block or a read-only transaction is already refused
  (25001).
- Concurrent compactions, and compaction against `Backup`, are serialized
  by btypedb's `compactMu`.
- A failed compaction removes its temp file, and `Open` removes one left by
  a crash (`db.go:276`).
- `VACUUM; SELECT 1` as one simple Query runs the VACUUM, where Postgres
  refuses it in an implicit transaction. bytdb has no implicit transaction
  for multi-statement queries at all, so this is that broader, deliberate
  behavior, not a VACUUM gap.
- Compaction can't be interrupted once started (documented).
- On a replicated database each VACUUM re-ships the whole file
  (documented).
- The v0.19.0 README on pkg.go.dev lacks the VACUUM grammar line. The next
  tag fixes that.

## Files touched

- `ai_docs/todo/next-list.md`: Roadmap (N-001) and Non-goals (N-005–N-015)
  restored; N-023 and N-024 added to Open.
- `~/.claude/commands/sess-save.md`: ID-loss guard and modification date.
  Outside this repo, not committed here.
- This doc.

## Next

Closed: None. Declined: None. Raised: N-023, N-024.
Deferred: None. Promoted: None.
Updated: N-001 (restore note). Restored after `dd7a25d` deleted them: N-001
(Roadmap), N-005–N-015 (Non-goals). Full list: `ai_docs/todo/next-list.md`.
