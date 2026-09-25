# Database lock moved into btypedb (N-022), btypedb v0.8.0, bytdb v0.18.0

- **Session:** `88d4569b-a36c-44ac-8b00-68531e279bd4`
- **Date:** 2026-09-25
- **Scope:** next-list N-022. The N-021 lock lived in bytdb, so it did not
  cover a raw `btypedb.Open` of the same file, `Engine.Backup` aimed at a
  live file, or a `replicate.Restore` written over one. The lock moved down
  into btypedb, and both backup and restore check their destination. Closed
  ahead of its trigger (no second consumer had appeared yet), by the user's
  choice. Released as btypedb v0.8.0, then bytdb and pgwire v0.18.0.

## 1. btypedb: the lock moves down (`49d0e36`, tag `v0.8.0`)

- **Ported, not redesigned.** bytdb's `lock.go`, `lock_flock.go`,
  `lock_windows.go` and `lock_other.go` moved to btypedb under the same
  design: a `<path>.lock` sidecar, symlinks resolved, `flock`
  (share-mode `CreateFile` on Windows, no-op elsewhere), non-blocking, PID
  recorded for `holder_pid`, and the sidecar left on disk after Close.
  `fileLock.release` became `Close`, so it satisfies `io.Closer`.
- **On an `fsys.Lock` seam, not `realFS.OpenFile`.** N-022 suggested
  `realFS.OpenFile`, but that locks the database file itself, and the first
  compaction renames a new inode over the path, leaving the lock on the old
  one. `fsys` gained `Lock(path) (io.Closer, error)`. `realFS` takes the
  sidecar lock. `powerFS` (the power-loss test FS, whose paths are only
  entries in a map) returns `nopLock`.
- **`Open`** takes the lock before anything destructive (the compaction
  temp removal, torn-tail truncation). It switched to a named `err` result
  so one deferred check releases the lock on every failed return. **`Close`**
  releases it last, after the final sync and file close.
- **`Backup(destPath)`** locks the destination first and holds it through
  the rename. That refuses both another DB's live file and the backing-up
  DB's own path. Before, renaming over a live file left its holder appending
  to the unlinked inode, and every later write was lost at the next open.
- **New API:** `ErrLocked` and `AcquireLock(path) (io.Closer, error)`, for
  code that writes a database file without opening it.
- README gained "One DB per file, enforced", and `.gitignore` gained
  `*.db.lock`.
- Tests (`lock_test.go`): same-process (including after `Compact`), symlink,
  release after a failed keyed open, backup onto a live or own path (the
  destination keeps later writes), `AcquireLock` in both directions, and
  cross-process plus `kill` via a re-exec helper.

## 2. bytdb (`100aafa`)

- `lock.go` is now `var ErrLocked = btypedb.ErrLocked`. It is an alias, so
  `errors.Is` matches either name. The three platform files were deleted,
  and the engine's `lock` field and its acquire/release calls were removed.
  This was required, not tidying: the lock is not reentrant, so the engine
  taking it before `btypedb.Open` would make every open fail against itself.
- `Engine.Backup` inherits the destination check; its doc comment says so.
- `replicate.Restore` takes `btypedb.AcquireLock(destPath)` before listing
  or downloading, and holds it until `assemble`'s rename.
- New tests: `TestLockRawKVOpen`, `TestLockBackupDestination`
  (`lock_test.go`), and `replicate/restore_lock_test.go` (restore onto the
  live source, and onto another live DB whose later writes survive).
- Docs: `docs/gotchas.md` (the one-engine note, online backup) and
  `docs/replication.md` (a "Never over a live database" guarantee). The
  `crash_test.go` comment now points at btypedb's `lock.go`.
- Development ran against the local btypedb through a scratchpad `go.work`
  (`GOWORK=`), so no `replace` directives were committed.

## 3. Pre-existing flake, not from this change

`btypedb`'s `TestAutoCompact` fails intermittently: 2 of 100 runs on the
untouched base, 5 of 100 with the change (noise). The cause is a race
between `Close` and an in-flight auto-compaction, which aborts with
`ErrClosed` and leaves the log above the test's 16 KB bound. Not tracked;
worth a look if it gets noisier.

## 4. Release

- btypedb `v0.8.0` on `49d0e36`: a minor release, with new API and a
  behavior change (a double open now fails).
- bytdb `v0.18.0` on `100aafa`: minor, via `/release`.
- pgwire `v0.18.0` on `431447c` ("pgwire, bench: bump bytdb to v0.18.0").
  **This departs from the skill's step 4.** pgwire and bench each list
  btypedb directly, so both moved v0.7.0 to v0.8.0 as well, and pgwire's
  `go.sum` changed (`GOWORK=off go mod tidy`), not only `go.mod`. Both
  build in the workspace and with `GOWORK=off`, and all three tags resolve
  on proxy.golang.org.
- **Downstream:** cats, gonotes, grmob and other direct btypedb users will
  get `ErrLocked` on a double open once they bump to v0.8.0. dbc still
  needs v0.17.0 or later for the lock at all.

## Files touched

- btypedb: `lock.go`, `lock_flock.go`, `lock_windows.go`, `lock_other.go`,
  `lock_test.go` (new); `db.go`, `fs.go`, `backup.go`,
  `powerfailfs_test.go`, `README.md`, `.gitignore`.
- bytdb: `lock.go`, `engine.go`, `lock_test.go`, `crash_test.go`,
  `replicate/restore.go`, `replicate/restore_lock_test.go` (new),
  `docs/gotchas.md`, `docs/replication.md`, `go.mod`/`go.sum`; the three
  `lock_*.go` platform files were deleted. `pgwire/go.mod`/`go.sum` and
  `bench/go.mod`/`go.sum`. `ai_docs/todo/next-list.md`.

## Next

Closed: N-022. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
