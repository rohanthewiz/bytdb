# Database file lock (N-021) and release v0.17.0

- **Session:** `b36b5a5a-9b6d-40eb-a98c-1612a0c8a0c3`
- **Date:** 2026-09-25
- **Scope:** an issue raised in dbc. bytdb v0.16.0 took no file lock, so
  two dbc processes (two TUIs, or a TUI and `dbc web`) could open the same
  `demo.bytdb` and both write its WAL. The lock was added in bytdb itself,
  then released as v0.17.0 with `/release`.

## 1. The lock

**Where:** `Engine.Open` / `Engine.Close` (`engine.go`), with the
mechanism in `lock.go` and three platform files. btypedb (v0.7.0) has no
lock either. The fix sits in bytdb, as the issue asked. Raw btypedb opens
stay unprotected (N-022).

**Design choices:**

- **A sidecar, `<path>.lock`, not the database file.** btypedb's
  compaction writes a fresh file and renames it over the path. A lock on
  the database file would stay with the old inode, and after the first
  compaction a second process would get in.
- **The sidecar is never deleted.** Unlinking it on Close would let a
  process that opened the old inode just before the unlink lock it while
  a third process creates and locks a new file: two holders.
- **The lock comes before `btypedb.Open`.** That open is not read-only. It
  removes a leftover `.compact` temp file (another engine's in-flight
  compaction) and truncates what it takes for a torn tail (another
  engine's half-written append).
- **Non-blocking.** A second Open fails fast with `ErrLocked`, carrying
  `path`, `lock_file` and `holder_pid`. The holder writes its PID into
  the sidecar after locking. This is diagnostic only; the lock is the OS
  state.
- **Unlock order.** Close runs `kv.Close()` (the final fsync) before
  releasing the lock, and joins both errors. Release uses `sync.Once`, so
  Close stays idempotent.
- **Symlinks** are resolved with `EvalSymlinks` when the file exists, so
  every spelling of one database maps to one sidecar.

**Platforms:**

- `lock_flock.go` (darwin, linux, the BSDs) uses `flock(LOCK_EX|LOCK_NB)`,
  retrying on EINTR. flock was chosen over fcntl because flock locks
  belong to the open file description: a second Open in the same process
  also conflicts, and closing some other fd on the file cannot drop it.
- `lock_windows.go` uses `CreateFile` with `FILE_SHARE_READ` only, so a
  second writer gets ERROR_SHARING_VIOLATION (defined locally as errno
  32, since `syscall` doesn't name it). This avoids `golang.org/x/sys`.
- `lock_other.go` (solaris, illumos, aix, plan9, js, wasip1) creates the
  sidecar but takes no lock, the pre-v0.17 behavior.
- Cross-built with `GOOS=windows|linux|freebsd go vet`,
  `GOOS=solaris GOARCH=amd64` and `GOOS=js GOARCH=wasm go build`.

## 2. Tests (`lock_test.go`)

- `TestLockSameProcess`: a second Open fails with ErrLocked naming this
  PID, still fails after `e.kv.Compact()`, double Close is fine, and a
  reopen after Close sees the data.
- `TestLockSymlink`: opening through a symlink is refused.
- `TestLockReleasedOnFailedOpen`: a keyed Open of a plaintext db fails
  with ErrNotEncrypted and gives the lock back.
- `TestLockCrossProcess` and `TestLockReleasedOnKill` re-exec the test
  binary into `TestLockHelperProcess` (driven by
  `BYTDB_LOCK_HELPER_DB`). The child is refused with the parent's PID.
  After a helper holding the lock is killed, the parent's Open succeeds.

**The lock found a real double-open:**
`pgwire/encrypt_e2e_test.go` checked a keyless `bytdb.Open` while its
second server still held the engine. The check moved to the window
after `stop1()`, where no engine is open.

## 3. Docs and comments

- `docs/gotchas.md`: "One process per file" became "One engine per
  file, enforced". It covers the sidecar, `ErrLocked`/`holder_pid`, no
  stale locks, and the advisory/NFS caveats.
- `docs/stdlib.md`: the engine-per-file section now names the lock.
- Stale comments fixed: `stdlib/stdlib.go` (it claimed "btypedb's file
  lock", which didn't exist) and `crash_test.go`.
- `.gitignore` gains `*.db.lock`.

## 4. Release v0.17.0

It's a minor version: new API (`ErrLocked`) and a behavior change, since
double opens now fail.

- `056a9cf` "Lock the database file: a second Open fails with ErrLocked",
  tagged **`v0.17.0`**. The tag message also covers the pg_constraint
  key rows, DROP CONSTRAINT for unique constraints, and the SQLSTATE
  fixes since v0.16.0.
- `5a6225f` "pgwire, bench: bump bytdb to v0.17.0", tagged
  **`pgwire/v0.17.0`**.
- Both versions resolve on proxy.golang.org. pgwire and bench build with
  and without `GOWORK=off`, and `go vet` / `go test ./...` passed in root
  and `pgwire/` before tagging.

**For dbc:** bump to `github.com/rohanthewiz/bytdb v0.17.0` and turn
`errors.Is(err, bytdb.ErrLocked)` into a message naming the holder PID
(`serr` field `holder_pid`).

## Files touched

`lock.go`, `lock_flock.go`, `lock_windows.go`, `lock_other.go`,
`lock_test.go`, `engine.go`, `pgwire/encrypt_e2e_test.go`,
`stdlib/stdlib.go`, `crash_test.go`, `docs/gotchas.md`, `docs/stdlib.md`,
`.gitignore`, `pgwire/go.mod`, `bench/go.mod`,
`ai_docs/todo/next-list.md`.

## Next

Closed: N-021. Declined: None. Raised: N-021, N-022.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
