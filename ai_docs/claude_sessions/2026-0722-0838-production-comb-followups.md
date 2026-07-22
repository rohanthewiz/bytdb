# bytdb production-comb follow-ups: btypedb recover, pgwire DoS, restore manifest

- **Date:** 2026-07-22
- **Scope:** Close the three design-decision follow-ups the previous
  production comb (session `2026-0722-0751`) deliberately left open, each
  with regression tests. Commit after each.

## Goal

"Do all 3 [follow-ups], committing after each one." (Plus: save a session
doc at the end.) The three were: (1) btypedb `db.Update` recover, (2)
pgwire DoS hardening, (3) replicate restore correctness marker.

## 1. btypedb `Update` recover — commit `94adbd1` (btypedb repo)

`db.Begin(true)` holds the single-writer lock (`writerMu`) for the txn's
lifetime, released only by Commit/Rollback. A **panic** in the `fn`
closure of `DB.Update` unwound past it with neither running → writer lock
held forever → every future write process-wide wedged (reads keep working,
masking it). The prior session's bytdb `WriteTxn` fix wrapped this from the
*outside*, but the one-shot `Insert`/`Update`/`Delete`/DDL paths call
`btypedb.Update` directly and were still exposed.

Fix (`btypedb/tx.go`): `defer recover → tx.Rollback() → re-panic` inside
`Update`, right after `Begin` succeeds. Rollback is idempotent on `tx.done`,
so it's a no-op on the normal commit/return paths and composes safely with
`WriteTxn`'s own outer recover (both call Rollback; the second sees
`done==true`). Fixes all write paths at the source. Test:
`btypedb/tx_panic_test.go` — panic value propagates, next write does not
hang, panicked write rolled back.

**Release note:** btypedb is linked via go.work (local fix active
immediately). It is committed but NOT tagged/pushed; bytdb's go.mod still
pins btypedb v0.6.0. Non-workspace consumers need a btypedb tag + a bytdb
pin-bump to receive it. Deferred (no release requested).

## 2. pgwire DoS hardening — commit `9a70873` (bytdb repo)

Six changes across `pgwire/{proto,pgwire,conn,errors}.go`, tests in
`dos_test.go` (in-package unit) + `dos_wire_test.go` (end-to-end socket):

- **Write deadline** (`Server.WriteTimeout`, default 60s via
  `DefaultIOTimeout`, negative disables). The headline fix: a client that
  opens a writable txn block (taking the engine writer lock), issues a
  large query, then stops reading left the server blocked in `Flush` with
  the lock held — the read-side idle-in-tx deadline can't fire (stuck
  *writing*, not reading), stalling all writes forever. Armed per-message
  in `run()` and across `startup()`; a timeout latches the bufio writer's
  sticky error → `wErr()` → connection ends → deferred rollback frees the
  lock. `TestWriteDeadlineFreesWriterLock` proves a 2nd client's write is
  freed in ~WriteTimeout (raw non-reading client A with a 4 KiB SO_RCVBUF
  + pgx client B).
- **Bounded body reads** (`readBody` now reads through a `bytes.Buffer`
  that grows as bytes land, vs. `make([]byte, n)` up to 64 MiB up front;
  short reads normalized to `ErrUnexpectedEOF` to match old `io.ReadFull`).
  Paired with **ReadTimeout** (default 60s, `armBodyDeadline` after the
  first byte) for a "slow body" peer. Split the `run()` read into
  type-byte (idle wait) + body (bounded).
- **MaxConns default** — 0 now means `DefaultMaxConns` (100), matching PG
  and the sibling IdleTxTimeout idiom; **negative = unlimited**.
  ⚠ BEHAVIOR CHANGE: `MaxConns==0` was formerly unlimited.
- **Per-conn stmt/portal ceilings** (16384 each) → 54000 on overflow.
- **Duplicate named Parse → 42P05** (only unnamed "" is replaceable).
- **Opt-in idle-session timeout** (`Server.IdleTimeout`, default OFF so
  pools are unaffected) → FATAL 57P05. In-txn case still always covered by
  IdleTxTimeout.

New SQLSTATEs added to `errors.go` `sqlstate()`: 42P05, 54000.
Full pgwire suite green under `-race`.

## 3. replicate generation-complete manifest — commit `a50e50e` (bytdb repo)

`Restore` chose the newest generation whose chain started at offset 0. A
generation freshly rolled by a compaction has exactly that shape after its
first partial ship → restore could pick an 8 MB fragment of the new epoch
over a complete older generation = **silent roll-backward**.

Fix: authoritative completeness marker. The first ship that drains the log
(`watermark == size`) writes `gen/<id>/manifest.json`
(`generationManifest{Generation,Epoch,Size,SealedUnixNano}`) recording the
certified-complete size; refreshed when the complete size grows, skipped on
idle ticks (tracked via `Replicator.sealed`, reset on roll). A generation
that never caught up has no manifest.

`Restore` selection (newest-first): pick the newest *manifested* generation
whose contiguous chain still reaches its certified `Size` (restore the full
chain — tail past the certified point is a valid torn tail). A manifested
generation short of its size lost chunks → skip for an older complete one.
If every manifested generation is short → new `ErrIncompleteReplica`
(refuse, don't restore a fragment). If NO generation carries a manifest
(legacy store / nothing caught up yet) → fall back to legacy
newest-contiguous-chain (existing replicas keep working, gain the guarantee
within one catch-up cycle).

Manifest lives in the generation's key space (listGenerations groups it,
prune removes it, `.json` ≠ `.wlog` so contiguousChain ignores it). Tests
`manifest_test.go`: manifest-on-catch-up + no idle re-PUT;
complete-over-partial-newer (the regression); damaged-manifested skipped;
all-short → ErrIncompleteReplica. README replication section + storage.go
package doc updated (the "newest complete generation" wording is now true).

## Verification

`go vet` clean (all modules). Full suites green: root (`bytdb`, `sql`,
`tuple`, `replicate`, `replicate/s3`), pgwire, btypedb. pgwire also green
under `-race`. gofmt clean.

## Commits (all local-only, not pushed)

- btypedb `94adbd1` — Update recover
- bytdb `9a70873` — pgwire DoS hardening
- bytdb `a50e50e` — replicate manifest

## Follow-ups / notes for next time

- **btypedb release**: tag btypedb (past `v0.6.0`) + bump bytdb go.mod pin
  so non-workspace consumers get the `Update` recover. Not done (no release
  requested this session).
- **MaxConns behavior change** is a semver-minor surprise: any deployment
  relying on `MaxConns==0 == unlimited` must switch to a negative value.
- The pgwire write/read/idle timeouts are godoc-documented on the `Server`
  fields; README does not enumerate `Server` knobs (left as-is).
