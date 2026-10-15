# Next-list review; N-029 parked on the Roadmap

- **Session:** `dc3e7c32-ec24-46ec-a4ef-0a340af785be` (continues
  `2026-1015-1621-v0.22.0-release-memguard-race-fix` in the same session)
- **Date:** 2026-10-15
- **Scope:** Housekeeping. `.ced/` added to `.gitignore`, a `/next-list`
  review in living-list mode, and N-029 moved from Open to Roadmap at the
  user's request. No code changes.

## 1. `.gitignore`

`.ced/` holds the editor's local history store (`history.bytdb`, its lock
and its own `.gitignore`), dated 2026-10-06. It had been untracked across
this session's commits and is now ignored.

## 2. `/next-list` (living-list mode)

- **Window:** 88 session docs. The newest before this one was
  `2026-1015-1621`.
- **Lapses:** none. Every ID that ever appeared in the file's history
  (N-001 to N-029) is present in some section. The only other matches
  were the Next ID line (N-030) and dbc's own N-128 and N-131, which are
  cited in N-026 and N-027. Next ID N-030 is above the highest ID. In the
  15-doc transition check, only `2026-0916-1611` still had an old-style
  full list, and all 13 of its items match file entries.
- **N-029 premise:** holds for UPDATE. `execUpdate`'s scan callback
  charges `Txn.CheckWrites` but never `budget.check`. The item asked
  whether `execDelete` has the same gap. `collectPKs` is unbudgeted too,
  but charging the row cap there would refuse a `DELETE FROM big` of more
  than 10M rows. That runs against DELETE's deliberate exemptions:
  `DB.freeing` skips the ceiling, and the write cap doesn't count
  deletes. The item text was narrowed to UPDATE only. It now notes that,
  with both caps at 10M by default, the real gain is the `CheckMemory`
  poll while the set grows. Value stays low.
- **N-001 (Roadmap), light pass:** btypedb (`2383665`) still has only the
  reserved cipher flag bits in `encrypt.go`, and no rotation or ChaCha.
  The trigger (a deployment needs key rotation) hasn't arrived, so it
  stays parked.

## 3. N-029 → Roadmap

At the user's request, N-029 moved from Open to Roadmap, in ID order
after N-001, with its header line unchanged. Open is now empty.

## Next

Closed: None. Declined: None. Raised: None.
Deferred: N-029. Promoted: None. Moved: None.
Updated: N-029 (scope narrowed to UPDATE; DELETE left exempt, with the
reason). Full list: `ai_docs/todo/next-list.md`.
