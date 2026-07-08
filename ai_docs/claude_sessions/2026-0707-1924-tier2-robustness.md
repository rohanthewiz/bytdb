# Session: Tier 2 robustness (from the reliability-hardening plan)

- **Session ID**: `95a54f2e-e64b-4758-b8cd-7a99d268e083`
- **Date**: 2026-07-07
- **Plan**: `ai_docs/plans/2026-0707-reliability-hardening.md` (executed Tier 2 in full; Tier 1 was the prior session)
- **State at end**: committed as `468fd9e` "Reliability hardening: Tier 1 fixes and Tier 2 robustness" (one commit covering both tiers — the uncommitted Tier-1 work shared hunks in engine.go/ddl.go/index.go/sql/exec.go, so splitting was not practical). `go test -race -count=1 ./...` green in both modules; `go vet` clean. SIGTERM smoke-tested against a real bytdbd binary.

## What was done

### T2.2 — Atomic catalog+data snapshot for readers (MED) — `engine.go`, `txn.go`, `index.go`, `ddl.go`
- **Plan deviation (important)**: the plan's suggested fix — hold `e.mu` across the DDL commit — deadlocks: writable `Begin` takes btypedb's writerMu *then* reads the catalog under `e.mu.RLock`, while such a DDL would hold `e.mu` and wait on writerMu (AB-BA). Used a **seqlock-style catalog version** instead.
- Every mutation of `e.tables` now funnels through `publishDesc` (stable, post-commit; version += 2, stays even) or `publishDescPending` (in-closure, pre-commit; version goes **odd**). `updateDDL` stabilizes (odd→even) after a successful commit; `restoreDesc` un-publishes and stabilizes on failure. `catStable` channel: readers wait (not spin) while a DDL commit/fsync is in flight.
- Readers — `ReadTxn`, `Begin(false)` (new `readSnapshot`), and one-shot `ScanIndex`/`ScanIndexRev` (which had the same race via their own `e.desc` + `kv.Begin(false)`) — take the catalog at an even version, open the kv snapshot, re-check the version, retry on mismatch.
- **First attempt failed its own test**: a plain version-equality check only catches publishes *between* the two snapshot acquisitions. Because DDL publishes before commit, the catalog is ahead of disk for the *entire* commit window — a reader starting wholly inside it passed the check and `ScanIndex` returned 0/50 rows. The odd/even (unstable-marker) scheme is what closes it.
- Writers need none of this: `Begin(true)`/`WriteTxn` hold writerMu, which every DDL holds through publish+commit.
- Tests: `snapshot_test.go` — create/drop-index churn vs 4 reader goroutines × 500 iterations, for both `ReadTxn` (invariant: catalog shows index ⇒ index scan yields all rows) and one-shot `ScanIndex` (either "no such index" or the full table).

### T2.1 — Idle-in-transaction timeout (MED-HIGH) — `pgwire/pgwire.go`, `pgwire/conn.go`, `pgwire/errors.go`, `cmd/bytdbd`
- `Server.IdleTxTimeout`: **default 5 minutes** (0 = default, negative = disabled) — deliberately stricter than Postgres's off-by-default because a silent read-write BEGIN holds bytdb's *global* writer lock.
- `conn` carries the raw `net.Conn`; `armIdleDeadline` sets a read deadline before each message read while `sess.Status() != TxIdle` (both 'T' and failed 'E'), clears it when idle. On expiry: `fatalBody` → **FATAL 25P03** "terminating connection due to idle-in-transaction timeout", connection returns, deferred `sess.Close()` rolls back and frees the lock.
- `bytdbd -idle-tx-timeout` flag wired through.
- Test: `pgwire/timeout_test.go` — conn A BEGIN+INSERT then silent (300ms timeout); conn B's INSERT is released well under bound; conn A's next statement fails; A's uncommitted row is gone. Negative space: idle *outside* a block survives 3× the timeout; an active block re-arms per message.

### T2.3 — Write-reentrancy guard (LOW-MED) — `engine.go`, `txn.go`, `dml.go`, `ddl.go`
- `Engine.writerGID atomic.Uint64` records the goroutine holding the open write transaction: set inside `WriteTxn`'s closure (deferred clear) and at `Begin(true)` (cleared by `Txn.Commit`/`Rollback` via new `Txn.e` field + `releaseWriter`).
- `checkReentrantWrite(op)` at the top of `Insert`/`Update`/`Delete`, `WriteTxn`, `Begin(true)`, `updateDDL`, and `CreateTableWithChecks`: same-goroutine re-entry returns a "would deadlock" error; other goroutines block normally.
- Goroutine ID via `runtime.Stack` header parse (`curGID`) — the standard hack; only evaluated while a write txn is actually open (atomic load short-circuits the common path).
- Tests: `reentrancy_test.go` — every entry point errors inside `WriteTxn`; marker clears after Commit AND Rollback; a different goroutine's insert blocks then succeeds after commit (no false positive).

### T2.4 — Smaller items
- **Offset+Limit overflow** (`sql/exec.go`): collection cutoff uses new `satAdd` (saturates at MaxInt64). Test: `LIMIT max OFFSET 1` returns all-but-first on both the sorted and early-cutoff (unsorted) paths — `sql/limits_test.go`.
- **NaN ordering** (`sql/exec.go`): new `cmpFloat` — NaN greater than everything incl. +Inf, equal to itself (Postgres) — used by all numeric branches of `compareVals` (flows into `orderCmp`, MIN/MAX, sorts). Unit-tested directly (NaN is hard to produce through SQL; the store rejects it in keys).
- **bytdbd signals** (`cmd/bytdbd/main.go`): SIGINT/SIGTERM → `srv.Close()` → `ListenAndServe` returns nil → main unwinds through deferred `e.Close()` (final fsync). Listen errors print instead of `log.Fatalf` so the defer still runs. Smoke-tested with a real SIGTERM (clean exit).
- **Seq-key guard** (`ddl.go`): a non-8-byte table-id sequence value is a corruption *error* — falling back to the default would reissue an existing table's ID and interleave key spaces.
- **Descriptor versioning** (`engine.go`): `TableDesc.FormatVersion` (`descFormatVersion = 1`), stamped by new single-funnel `marshalDesc` (replaced 4 inline `json.Marshal` sites). `loadCatalog` collects ALL bad descriptors (undecodable or future-versioned) and reports them in one message — still fails Open (skipping would hide rows and risk ID reuse). Old version-0 descriptors read as current.
- **Docs**: `WriteTxn`/`Txn.Commit`/`Engine.Insert` now state that a commit error means "durability unknown" (btypedb makes commits visible before fsync).
- Tests: `catalog_test.go` (corrupt seq key; Open naming both a mangled and a future descriptor in one error; format-version round-trip incl. index).

## Key architecture facts learned (worth keeping)

- **Lock order**: btypedb writerMu → `e.mu` (writers snapshot the catalog *after* acquiring writerMu). Anything that holds `e.mu`-or-equivalent while waiting on writerMu deadlocks. This killed the plan's suggested T2.2 fix.
- The DDL publish→commit window is *long* (contains the WAL fsync). Any reader-consistency scheme must treat the whole window as unstable, not just the publish instant — hence seqlock, not plain version-compare. The failing intermediate test (`ScanIndex returned 0/50 rows`) is the reproducer if this ever regresses.
- `serr` structured fields do NOT appear in `err.Error()` — operator-facing detail (like the bad-descriptor list) must go in the message string itself.
- pgx: a plain `Exec` with no args uses the simple protocol; `startServer` helper in `pgwire/server_test.go` is the template for server tests with options (external package `pgwire_test`).
- Engine one-shot writes resolve descriptors inside their own `kv.Update` (not via the catalog version) — that's what preserves the T1.3 residual window (phantom descriptor between failed commit and restore). Catalog-in-kv remains the full fix and would subsume the seqlock.
- `SetReadDeadline` on the raw conn works fine under a `bufio.Reader`.

## Next steps (plan's execution order)

1. **Fuzz targets** (Tier 3, items 1-3): `FuzzTupleRoundTrip` + `FuzzTupleOrder`, `FuzzParse` (would have caught T1.1), `FuzzMessageParse`.
2. Property tests: planner equivalence (index scan ≡ seq-scan+filter+sort), `ScanRangeRev ≡ reverse(ScanRange)`.
3. Crash/fault tests at the bytdb level (SyncNever + reopen-without-Close, torn tail, multi-index atomicity).
4. Remaining semantic-gap tests (plan items 14-22: UPDATE scanning the index it mutates, NOT IN with NULL, LIMIT $1, statement-level atomicity, ALTER × index interactions, ...).
5. Longer-term: catalog into the kv keyspace (closes T1.3's residual window, retires the seqlock); aggregate SUM overflow (noted in Tier 1, still open).
