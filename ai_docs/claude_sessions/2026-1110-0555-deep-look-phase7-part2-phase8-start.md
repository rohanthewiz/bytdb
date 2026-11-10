# Deep-look plan: Phase 7 part 2, start of Phase 8

Session: `2c5b1603-fa95-4c81-b037-0bcdc19533ac` (continues
`2026-1110-0512-deep-look-phase7-part1`).

## What landed

- **D-43, top-N heap** (`c837ac2`). `ORDER BY ... LIMIT` without a serving
  index keeps the best OFFSET+LIMIT rows in a max-heap whose entries carry
  their arrival number (stable ties). Only the heap counts against the row
  cap. 38.9 -> 13.6 ms.
- **Q6, D-49, D-48 doc** (`e72676b`). `bytdbd -max-result-rows` defaults to
  1M; docs/gotchas.md gives ~280 B of heap per small row and the restart
  cost.
- **D-44, per-row overheads** (`25dc12e`). First-table rows pass through
  `stepNext`; `evalPreds` reuses the caller's env; patterns compile through
  a `sync.Map` keyed by (pattern, LIKE?, ci); `ExArith.Kind` decided by the
  typing pass; wide descriptors carry a name index and
  `TableDesc.ColIndexUnique`; `LIKE 'prefix%'` bounds text-key scans.
- **D-45, write path** (`92691b0`). Keys built in one buffer
  (`tuple.AppendInt`), untouched indexes skipped by UPDATE,
  `Txn.ReferencingFKs` memoized, no goroutine-ID parse under OCC, index
  backfill decodes with the original descriptor.
- **D-46** (`7e044e4`): documented `ReadTxn` for read-heavy loops.
- **D-47 first half** (`83f1a6e`): typing, Describe and scope checks resolve
  catalog tables by shape (`DB.shapeLookup`). -42% / -16%.
- **D-48 in btypedb** (btypedb `ad77fd8`, pushed to main, not tagged): reused
  record buffer for codecs marked `detached`, CRC via `crc32.Update`,
  `PushBack` for in-order keys. Open of 500k rows + index 389 -> 104 ms.
  bytdb docs updated (`10c4bdc`).
- **D-58/D-56 quick fixes** (`fb6ec5f`): `bytdbd` timeout, concurrent-writes
  and compaction flags; `-max-conns` help text; stale "no recover" comments.
- **D-55, input forms** (`67d49a6`): timestamps without seconds, zone names,
  special words (infinity stored as the extreme instants, also on the wire's
  binary formats); `bytdb.ParseBool`; braced uuids. difftest 72.
- **D-54, migration DDL** (`7be18fa`): `sql.Ignored` (CREATE SCHEMA /
  EXTENSION, COMMENT, ANALYZE, LOCK), unnamed indexes, ALTER TABLE IF EXISTS
  / ONLY, DROP COLUMN IF EXISTS, multi-table DROP, CASCADE/RESTRICT as
  RESTRICT, NOT VALID, cast/paren defaults, per-row `DEFAULT
  gen_random_uuid()`, wider `castType`; CTAS refused with a hint.

## Decisions

- **Top-N ties** break by arrival, so the heap's answer equals the stable
  full sort's slice; a 300-shape property test checks it.
- **D-47 second half deferred**: some catalog tables are live
  (`pg_stat_activity`, sequence state), so a per-version row memo needs an
  exclusion list; carried as N-047.
- **D-48 went to btypedb main without a tag.** Codec gained an unexported
  `detached` flag so only the built-in codecs share the replay buffer; a
  custom codec that aliases its input keeps fresh buffers (test proves the
  guard is needed). bytdb needs a btypedb tag + go.mod bump to benefit —
  added to N-044's release decision.
- **NOT VALID still validates** existing rows; **CASCADE runs as
  RESTRICT**; **LOCK TABLE is a no-op** (documented as a gap under
  concurrent writes); **cast modifiers parse but do not apply**.
- **Infinity timestamps/dates** are the type's extreme values, matching
  Postgres's binary wire encoding.

## Measurements

Apple M3, benchstat n=6 unless noted: DLTopNUnindexed 38.9 -> 13.6 ms;
after D-44, DLCountSum100k 15.1 -> 12.8 ms, DLHashJoin -7%; D-45:
DLUpdateUnindexedCol 8.52 -> 7.77 µs (64 -> 38 allocs); D-47:
DLCatalogColumns200 328 -> 190 µs; D-48: DLOpen500k 389 -> 104 ms.

## Next

Closed: N-042. Declined: None. Raised: N-045, N-046, N-047. Deferred: None.
Promoted: None. Moved: None. Updated: N-044.
Full list: `ai_docs/todo/next-list.md`.
