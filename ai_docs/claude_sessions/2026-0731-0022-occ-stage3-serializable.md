# Session: OCC Stage 3 — opt-in SERIALIZABLE via read sets; DDL fully serialized

- **Session ID:** `0bf61a6c-6153-4801-a08d-893cd8a86d6e`
- **Date:** 2026-07-31, 00:22
- **Deliverables:**
  - btypedb branch **`occ-stage1`** (on top of Stage 2's `0a09c79`): read-set
    validation + exclusive transactions
  - bytdb branch **`occ-stage2`** (on top of `1d497a6`): serializable API,
    read marking, exclusive DDL
- **Design:** `ai_docs/plans/2026-0730-concurrent-writes-occ.md` (Stage 3 section)
- **Prior session:** `2026-0730-2350-occ-stage2-sequences.md`

## What was built

Stage 3 of the concurrent-writes plan: under `WithConcurrentWrites()`,
a transaction can opt up from snapshot isolation to **SERIALIZABLE** —
its reads are validated at commit alongside its writes, closing write
skew, phantoms, and the FK parent-delete-vs-child-insert hole. DDL now
serializes fully against every concurrent transaction.

### btypedb: read-set validation (occ.go, tx.go)

- `Tx.TrackReads()` opts in; `MarkRead(key)` / `MarkReadRange(min, max)`
  record semantic reads. `occReadConflicts` = classic backward
  validation: any intervening commit's keys/ranges vs our read
  keys/ranges, all four combinations. Committed transactions' reads
  need no checking (they serialized before us).
- Scans record their **bounds**, not the keys yielded — the interval
  claim is what catches phantoms (keys a scan did NOT see). This is why
  tracking lives above the kv iterators.
- Reads deliberately **survive savepoint rollback**: results produced
  before a RollbackTo may already have escaped to the application, so
  Commit keeps vouching for them.
- Empty read set = plain SI at zero cost; no-ops in default mode and on
  read-only transactions.

### btypedb: exclusive transactions (the DDL contract)

- `Tx.MarkExclusive()`: commit succeeds only if NO other commit landed
  since Begin (no validation attempted — work like an index backfill
  depends on every key of its snapshot staying put, which per-key write
  sets cannot express), and its ring entry is a **wildcard**, so every
  overlapping-in-time transaction conflicts and retries.
- `DB.UpdateExclusive(fn)`: escalation that cannot lose — takes
  writerMu at Begin even in occ mode (freezing the head; `begin(writable,
  exclusive)` refactor), still pushes the wildcard on publish (in-flight
  transactions that began before the lock must still abort).
- In default mode both are no-ops / exactly `Update`.

### bytdb: DDL fully serialized (engine.go updateDDL)

- occ mode: 2 optimistic `MarkExclusive` attempts
  (`ddlOptimisticTries`), then one `UpdateExclusive` — guaranteed
  progress under any write load. Replaces the retry-×10 loop.
- **Strictly stronger than the design doc's `schemaVersion` idea**, and
  deliberately so: key-overlap validation alone let a CreateIndex
  backfill miss rows committed mid-DDL (real Stage-1 index-corruption
  hole — backfill's write set never touches the new row's key). The
  unmoved-head requirement closes the DDL-commits-second direction; the
  wildcard closes writer-commits-after-DDL. Pinned by
  `TestSerializableCreateIndexUnderLoad` (index built under 4 hammering
  insert goroutines comes out complete).

### bytdb: serializable API + read marking

- `Engine.WriteTxnSerializable(fn)` / `Engine.BeginSerializable()` —
  per-transaction opt-in, as in Postgres; mixed SI/serializable gives
  no cross guarantee (documented, matches Postgres). Default mode:
  degrade to the plain (already serializable) path.
- `readMarker` interface + `markRead`/`markReadRange` helpers
  (engine.go, next to kvView): type-asserted so raw snapshots and
  untracked transactions no-op.
- Marked sites: `Txn.Get` (hit or miss — absence is information),
  `updateRow` oldKey (miss = the statement's result; hit = every new
  value derives from it), `deleteRow` key (miss branch), scanRows/Rev +
  scanIndexRows/Rev full [start, end) claims (over-claim under LIMIT is
  conservative, documented), `rowFromIndexEntry` row fetch (update to a
  non-indexed column rewrites the row key without entering the index
  range).
- Deliberately UNmarked (each with an explaining comment): insertRow's
  PK/unique probes and updateRow's newKey/unique probes (write-paired —
  write-write validation covers them; keeps bulk loads' read sets
  lean), catalog/descriptor reads (exclusive DDL subsumes them),
  sequence/identity draws (non-transactional by Stage 2 design).

### Bug found by the new tests

`checkReentrantWrite` (writerGID guard) fired spuriously under OCC:
same-goroutine one-shot writes while a `Begin(true)` transaction is
open were rejected, but the deadlock it guards against cannot happen
there (Begin holds no lock; the marker is a single slot and occ allows
many open writers). Now bypassed when `e.occ` — the Stage-1 design doc
had already predicted the guard becomes dead weight under OCC.

## Semantics pinned / documented

- Serializable = backward validation: their writes ∩ (our reads ∪ our
  writes) = ∅ over the (base, head] window, then replay. Read-only
  serializable transactions always commit (nops==0 early return).
- Ring eviction and wildcard entries conflict read validation exactly
  as they do write validation.
- DDL under sustained writes costs writers one stall + a wildcard
  retry, never the DDL its progress.

## Tests (all green, plain + `-race`)

- btypedb `occ_serial_test.go` (10): point-read conflict (+SI control),
  absent-read conflict, range phantom in/out, reads vs range delete
  (point + range), write-skew (real under SI, prevented tracked),
  savepoint-rollback read retention, exclusive aborts on any commit,
  wildcard aborts overlappers, UpdateExclusive under load ×20,
  default-mode no-ops, disjoint tracked commit.
- bytdb `serializable_test.go` (10 + bench): write skew (SI baseline
  demonstrated first), phantom scan in/out, parent-delete vs
  child-insert both commit orders, index-scan row-read conflict,
  update-miss-then-insert, DDL aborts in-flight SI writer (+retry
  succeeds), CreateIndex under load (completeness), disjoint
  serializable commit, default-mode degradation, invariant stress
  (8 workers, conserved constraint), read-only commit.
- Full suites green both repos: bytdb + sql + pgwire + tuple +
  replicate; btypedb complete.

## Benchmarks (M3, 8 writers, SyncNever, heavy txn = 400 reads + identity insert)

| mode | ns/op |
|---|---|
| serialized (default) | 150µs |
| occ snapshot isolation | 53.6µs |
| occ serializable | **66µs** (~23% tracking overhead; still 2.3× vs serialized) |

`BenchmarkSerializableHeavyTxn` mirrors the Stage-2 bench shape exactly
so the only variable is WriteTxnSerializable vs WriteTxn.

## State / next steps

- Changes on btypedb `occ-stage1` and bytdb `occ-stage2` (this commit);
  bytdb main and all tags untouched.
- Not started: Stage 4 — pgwire SQLSTATE 40001 mapping, bounded
  auto-commit retry, `SET TRANSACTION ISOLATION LEVEL SERIALIZABLE` in
  the SQL session layer, mkdocs Concurrency page.
- Open question carried forward: Engine one-shot writes surface
  `ErrTxConflict` raw; auto-retry belongs with the pgwire auto-commit
  work.
