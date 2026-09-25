package bytdb

import "github.com/rohanthewiz/btypedb"

// ErrLocked is returned by Open when another engine — or any other
// btypedb.DB — already holds the database, in another process or
// earlier in this one without a Close, and by Backup when its
// destination is a live database. Two engines over one file would each
// replay the WAL into their own memory and then append to it
// independently, so each would silently miss the other's writes and
// interleave records the next replay has to make sense of. Share a
// database across processes through the wire server (pgwire / bytdbd);
// within a process, share one *Engine (the stdlib driver already does
// this per path).
//
// The lock itself lives in btypedb (its lock.go), not here. It was
// first taken in bytdb, which left a raw btypedb.Open of the same file,
// and btypedb's Backup onto a live path, outside it. Taken inside
// btypedb.Open it covers every opener, Backup checks its destination,
// and tools that write a file without opening it (replicate.Restore)
// take it through btypedb.AcquireLock. This is an alias rather than a
// wrapper so errors.Is matches either name: callers that test for
// bytdb.ErrLocked keep working, and so do those testing
// btypedb.ErrLocked.
var ErrLocked = btypedb.ErrLocked
