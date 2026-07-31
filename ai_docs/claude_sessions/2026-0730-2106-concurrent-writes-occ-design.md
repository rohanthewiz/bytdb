# Session: Concurrent Writes OCC Design

- **Session ID:** `b2bb2b25-e109-4edf-8e8e-8dfa2c515208`
- **Date:** 2026-07-30, 21:06
- **Repo state:** main @ `7e16a2a` (docs refresh) + new design doc, uncommitted at session start

## What happened

Design-only session. Started from the open question "thinking outside the box,
what would it take to enable concurrent writes?" and ended with a full staged
design doc: **`ai_docs/plans/2026-0730-concurrent-writes-occ.md`**. No code
changes; prototype of Stage 1 deferred to a future branch by user choice.

## Analysis that drove the design

Read the real write path before proposing anything:

- `btypedb/tx.go` — `Begin(true)` takes `writerMu` at tx.go:46 and holds it
  for the transaction's entire lifetime; all bytdb SQL work (FK checks,
  unique checks, encoding, index maintenance) runs under the process-wide
  writer lock.
- `btypedb/state.go` — `dbState` is persistent COW trees; snapshot copy is
  O(1); commit is a pointer swap. The LMDB shape: publish is the only thing
  that must serialize.
- **Key discovery that shrank the design:** `Tx` already buffers its framed
  WAL records in `tx.pending` (with `nops`), and savepoints already work by
  truncating that buffer (`savepoint.go`). Write-set capture is mostly free —
  OCC needs only a parallel *logical* op list plus version bookkeeping.
- **Hidden read dependencies in "write" ops:** `Delete` returns `existed`
  from the snapshot; `DeleteRange` scans the range. Resolved in the doc:
  the `existed` phantom is standard SI (Postgres REPEATABLE READ behavior);
  range deletes are made deliberately *stricter* than Postgres SI (concurrent
  insert into a deleted range conflicts) because `Truncate`/`DropTable` ride
  on range deletes and an insert surviving a truncate is unacceptable.
- `btypedb/groupcommit.go` — group commit already sequences appends and
  coalesces fsyncs with no DB locks held during `waitDurable`; concurrent
  committers are the scenario it was built for.
- `bytdb/seq.go` — `nextFromCounter` is a transactional read-modify-write of
  a counter key in the same kv tx as the insert → under OCC every pair of
  concurrent identity inserts would conflict. Making sequences
  non-transactional is a hard prerequisite, not an optimization.
- `bytdb/engine.go` — the `writerGID` reentrancy guard (goroutine-ID stack
  parse) exists only because the lock outlives the call; becomes removable
  once the lock is commit-scoped. Side benefit: a hung user closure in
  `WriteTxn` can no longer wedge writes process-wide.

## The design (4 stages, summarized)

1. **btypedb — commit-time OCC:** `Begin(true)` stops taking `writerMu`;
   `Tx` gains `baseVersion` + `ops []logicalOp` (deadlines already absolute,
   so replay is wall-clock-independent); `DB` gains `commitVersion` + a
   bounded `recentCommits` ring of changed keys/ranges. Commit under a short
   `writerMu`: fast-path swap if head unmoved, validate-then-replay-onto-head
   if no key/range overlap, else `ErrTxConflict`. WAL append stays in commit
   order → replay/backup/replication (`LogState`/`ReadLogRange`/epochs)
   untouched. Gives snapshot isolation. Gated behind opt-in
   `ConcurrentWrites` flag for one release.
2. **bytdb — sequences/identity go non-transactional:** atomic counters with
   WAL high-watermark records (log `n + cacheSize` ahead), Postgres gap
   semantics; counter keys leave the transactional keyspace entirely.
3. **bytdb — opt-in SERIALIZABLE:** read-set tracking at the bytdb layer
   (which knows its Get/FK/unique/scan reads semantically, unlike the kv
   layer); closes write skew. DDL stays fully serialized via a coarse
   `schemaVersion` validated at every commit.
4. **pgwire:** `ErrTxConflict` → SQLSTATE `40001` (`serialization_failure`);
   bounded auto-retry for auto-commit statements only (explicit BEGIN blocks
   never auto-retry — client saw intermediate results); docs page.

Alternatives considered and rejected in the doc: pipelining (can't express
interactive BEGIN…COMMIT; OCC's fast path subsumes it), per-table locks
(lock ordering hazards, still one commit funnel), fine-grained latching
(abandons the COW design everything is built on), git-style tree merge
(fun, but semantic conflicts need SQL-level knowledge).

Risks logged: hot-key retry storms (mitigation ladder ending in opt-in
per-key locks, not built speculatively), ring sizing for bulk transactions
(spill to conflicts-with-everything), replay cost for large txns. Test plan
includes a write-skew probe that must FAIL under SI and PASS under Stage 3 —
a tripwire for the isolation level actually shipped.

## State / next steps

- Design doc committed this session; **no implementation exists**.
- Memory (`btypedb-local-workspace.md`) updated with a design summary.
- Next step when user is ready: prototype Stage 1 in a branch — it is
  self-contained in btypedb and testable purely at the kv layer.
- `.cats-todo/` remains untracked by design (user's local todo tooling).
