package bytdb

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/rohanthewiz/serr"
)

// ErrLocked is returned by Open when another engine already holds the
// database — in another process, or earlier in this one without a Close.
// Two engines over one file would each replay the WAL into their own
// memory and then append to it independently, so each would silently
// miss the other's writes and interleave records the next replay has
// to make sense of. Share a database across processes through the wire
// server (pgwire / bytdbd); within a process, share one *Engine (the
// stdlib driver already does this per path).
var ErrLocked = errors.New("database is locked: already open by another engine")

// lockSuffix names the sidecar lock file next to the database file.
//
// The lock lives on a sidecar rather than on the database file because
// btypedb's compaction writes a fresh file and renames it over the
// original path. A lock held on the old file follows the old inode, so
// after the first compaction a second process opening the path would
// find the new, unlocked inode and get in. The sidecar is never renamed
// or replaced, so its lock stays meaningful for the engine's lifetime.
//
// The sidecar is deliberately left on disk after Close. Deleting it
// would reopen the same race in a new form: a process that opened the
// old sidecar just before the unlink could lock that orphaned inode
// while a third process creates and locks a new file at the path —
// two holders, each believing it is alone.
const lockSuffix = ".lock"

// errLockHeld is the platform layer's signal that the lock is taken;
// acquireFileLock turns it into ErrLocked with context.
var errLockHeld = errors.New("lock held")

// fileLock is an exclusive, advisory, process-lifetime lock on a
// database's sidecar file. The OS drops it when the holding process
// exits for any reason (crash and kill -9 included), so a stale sidecar
// left by a dead process never blocks a later Open — the file's content
// is only a diagnostic, not the lock itself.
type fileLock struct {
	path string
	f    *os.File
	once sync.Once
	err  error
}

// lockPathFor returns the sidecar path guarding dbPath. An existing
// database is resolved through symlinks first so every spelling of it —
// a symlinked file or directory, a relative path — maps to one sidecar;
// otherwise two names for the same file would take two different locks
// and exclude nothing. A path that does not exist yet (first Open) has
// nothing to resolve and is used as given.
func lockPathFor(dbPath string) string {
	if real, err := filepath.EvalSymlinks(dbPath); err == nil {
		dbPath = real
	}
	return dbPath + lockSuffix
}

// acquireFileLock takes the lock guarding dbPath without waiting: if it
// is held, Open should fail fast with an explanation rather than hang
// behind a TUI someone left running in another terminal.
func acquireFileLock(dbPath string) (*fileLock, error) {
	lp := lockPathFor(dbPath)
	f, err := tryLockFile(lp)
	if err != nil {
		if errors.Is(err, errLockHeld) {
			flds := []string{"path", dbPath, "lock_file", lp}
			if pid := holderPID(lp); pid != "" {
				flds = append(flds, "holder_pid", pid)
			}
			return nil, serr.Wrap(ErrLocked, flds...)
		}
		return nil, serr.Wrap(err, "op", "lock database", "path", dbPath, "lock_file", lp)
	}

	// Record the holder's PID so the next contender's error can name
	// it. Best effort: the lock is the flock/share-mode state, not these
	// bytes, so a failed write costs only the diagnostic.
	if err := f.Truncate(0); err == nil {
		f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &fileLock{path: lp, f: f}, nil
}

// holderPID reads the PID the current holder recorded, or "" if it is
// unreadable (Windows' share mode may refuse the read; the holder may
// not have written it yet).
func holderPID(lockPath string) string {
	b, err := os.ReadFile(lockPath)
	if err != nil {
		return ""
	}
	pid := strings.TrimSpace(string(b))
	if _, err := strconv.Atoi(pid); err != nil {
		return ""
	}
	return pid
}

// release drops the lock by closing the file (closing releases both a
// flock and a Windows share-mode handle). Idempotent, because Engine.Close
// is: a second Close must not fail on an already-closed file.
func (l *fileLock) release() error {
	l.once.Do(func() {
		if err := l.f.Close(); err != nil {
			l.err = serr.Wrap(err, "op", "release database lock", "lock_file", l.path)
		}
	})
	return l.err
}
