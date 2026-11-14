# N-046 shared read snapshots in btypedb; release v0.24.0 (+ btypedb v0.11.0)

Session: `bba65599-6d66-431b-9a7c-a5dd342b1d2c`. Started from `/clear`.
The user pasted N-046. It was built and measured, the user asked to
commit, push, wrap and release, and the session cut btypedb v0.11.0 and
bytdb/pgwire v0.24.0.

## Where the fix went: btypedb, not the engine

N-046 said `Engine.Get` opens and releases a btypedb snapshot per call,
both under btypedb's mutex. Reading btypedb (`tx.go`) confirmed that
`begin(false)` took `db.mu.Lock()` to `state.copy()`, and Rollback took
it again to `release()`. That was two exclusive sections per one-shot
read, plus about five allocations for the copied tree headers.

The item's precondition was that btypedb would need to guarantee
concurrent reads of one snapshot. It already relied on that: `DB.Get`,
`DB.All` and the rest read the head tree from many goroutines under
`mu.RLock`. btype source (v0.3.0, pinned) confirmed that the non-Mut read
paths pass `mut=false` and `cowRoot`/`cowChild` short-circuit on it, so
reads only load. Copy needs `mu` because `CopyInto` writes
`t.copied = true` on the source tree header; refcounts are atomic.

So the change lives in btypedb, which helps Engine.Get, ReadTxn and every
SQL read with no engine code change.

### Design (`btypedb/readsnap.go`, commit `7c06d99`)

- `DB.readSnap atomic.Pointer[sharedRead]` caches one snapshot of the
  head. `sharedRead.refs` counts the cache slot plus each open read-only
  Tx.
- Fast path of `Begin(false)`: load the pointer, `acquire()` (one
  `Add(1)`), and allocate the Tx. No lock, no copy.
- Slow path: under `mu`, copy the head and install it with refs = 2.
- `dropReadSnap()` runs under `mu` before every head mutation: the commit
  swap, direct Set/SetTTL/Delete, CreateIndex, DropIndex and Close. Doing
  it *before* the mutation means that, with no readers, the snapshot is
  released first and direct writes still mutate in place instead of
  copying their path.
- Release protocol: `unref()` is `Add(-1) == 0 && CAS(0, deadRefs)`, and
  `acquire()` is `Add(1) > 0`, else undo. An acquire that lands between a
  releaser's 0 and its CAS keeps the snapshot, and the CAS then fails, so
  exactly one goroutine releases it. Only that last holder takes `mu`
  (`releaseState`), which keeps the "Copy and Release under mu"
  invariant.
- Read-only savepoints take no copy (`Savepoint.state == nil`).
  RollbackTo, Release and releaseSaves handle nil. Copying the shared
  state would write its tree headers.

### Measurements (this M-series machine, 8 cores)

| | before | after |
|---|---|---|
| btypedb View+Get, 1 goroutine | 275 ns, 6 allocs | 105 ns, 1 alloc |
| btypedb View+Get, 8 goroutines | 407 ns/op | 98 ns/op |
| bytdb Engine.Get, 1 goroutine | 690 ns | 500 ns |
| bytdb Engine.Get, 8 goroutines | 565 ns/op | 185–215 ns/op |
| bytdb ReadTxn loop, 8 goroutines | ~70 ns/op | ~70 ns/op |

Two experiments were tried and reverted:
- **Refcounting stubbed out.** btypedb's parallel benchmark went from 98
  to 48 ns, but Engine.Get only from about 214 to about 165 ns. A
  `-cpuprofile` of Engine.Get at 8 goroutines was dominated by the runtime
  (`madvise`, `lock2`, `allocSpan`), which is allocation pressure from 12
  allocs/op. `acquire`/`unref` were about 3%, so striped refcounts were
  not worth it.
- **Engine.Get with Begin/Rollback instead of View** (to avoid closure
  captures). Still 12 allocs; the compiler already kept the closure off
  the heap. Reverted, keeping only the new doc comment.

## Bug found on the way: OCC index changes lost by an open Tx's commit

The hammer test's concurrent-writes variant failed with "index already
exists" after a DropIndex. Reproduced on unmodified v0.10.0: in OCC mode,
`tx := Begin(true)`; `CreateIndex("ix")`; `tx.Commit()` left `Indexes()`
empty. The Tx carried a copy of the idx map from Begin, and with no
intervening commit (`commitVersion == baseVersion`) Commit swapped it in
whole. DropIndex had the mirror-image effect and resurrected the index.

Fix (`indexChanged`): in OCC mode, CreateIndex and DropIndex push an
empty ring entry. It conflicts with nothing but bumps `commitVersion`,
so such commits take the existing rebase path (replay ops onto the head,
which maintains the new index). Default mode is unaffected because
writable Txs hold `writerMu` for life. bytdb never uses btypedb-native
indexes, so it was not affected. Regression test:
`TestOCCIndexChangeSurvivesOpenTxCommit`.

## Verification

- btypedb `readsnap_test.go`:
  - snapshot shared until a write; refcount lifecycle down to `deadRefs`;
  - freshness after each mutation kind, in default and OCC modes;
  - TTL expiry seen through a cached snapshot;
  - Begin after Close returns ErrClosed;
  - read-only savepoints;
  - an acquire/unref protocol hammer;
  - a race hammer (one-shot Views, scans, savepoints against Update with
    read-after-commit checks, direct writes, TTLs, index churn and
    Compact), 20 runs under `-race`.
- Full btypedb suite under `-race`; staticcheck v0.8.2 via `go run`. The
  installed binary is built with go1.23 and refuses go1.26 modules.
- bytdb full suite under `-race` in the workspace and with `GOWORK=off` on
  the v0.11.0 pin; pgwire suite.
- `/verify` over the wire (bytdbd built against the local btypedb, pgx
  client):
  - 300 UPDATEs, each seen by the next SELECT on another connection, over
    both simple and extended protocols;
  - 8 readers × 300 count and point SELECTs under 500 inserts, with
    monotonic counts;
  - a READ ONLY transaction kept its snapshot across a concurrent commit
    and SAVEPOINT / ROLLBACK TO / RELEASE, and refused a write with 25006.
- CI green on btypedb `7c06d99`, and on bytdb `5d8bba9` and `c938ac3`
  (every job, including `pgwire-released-pin` after the bump).

## Release

| tag | commit | notes |
|---|---|---|
| btypedb `v0.11.0` | `7c06d99` | shared read snapshots; OCC index fix |
| bytdb `v0.24.0` | `5d8bba9` | N-048 numeric, N-054 42P07, N-046, typer copy trim |
| bytdb `pgwire/v0.24.0` | `c938ac3` | tracks v0.24.0; still no `replace` |

- The bytdb root pin was bumped to btypedb v0.11.0 in `5d8bba9`, which
  was pushed and passed CI before the root tag.
- `c938ac3` bumped pgwire, bench and difftest. pgwire now pins bytdb
  v0.24.0 and btypedb v0.11.0.
- All three tags resolve on proxy.golang.org.
- `GOWORK=off go run …/pgwire/cmd/bytdbd@v0.24.0 -h` works.

## Files

- btypedb: `readsnap.go` (new), `readsnap_test.go` (new), `tx.go`,
  `db.go`, `savepoint.go`, `index.go` (+ `indexChanged`),
  `index_test.go`, `README.md`, and `pin_test.go` (a third relied-on
  btype property: read paths are write-free).
- bytdb: `bench_read_test.go` (new: `BenchmarkDLEngineGet`,
  `…GetParallel`, `BenchmarkDLReadTxnGetParallel`), the Engine.Get doc
  comment, `docs/concurrency.md`, the go.mod/go.sum pins, and
  `ai_docs/todo/next-list.md`.

## Gotchas

- gopls diagnostics for `Decimal`/`TNumeric` being undefined were stale
  index noise; `go build` was clean throughout.
- The installed `staticcheck` is too old for go1.26.1. Use
  `go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...`.
- To compare benchmarks against the pre-change btypedb, stash the
  modified files and move the new files out of the package. A
  benchmark-only `readsnap_test.go` and the HEAD `index_test.go` are
  needed so the package still compiles.

## Next

Closed: N-046. Declined: None. Raised: None. Deferred: None.
Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
