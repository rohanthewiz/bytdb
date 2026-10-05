package bytdb

import (
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/btypedb"
)

// TestEngineCompact checks the on-demand compaction wrapper end to end:
// after churning rows the log must shrink, the epoch must bump (the
// replication signal), and the surviving catalog and rows must read back
// identically both live and after a reopen of the compacted file.
// Auto compaction is disabled so the only rewrite is the explicit one —
// otherwise a background pass could land first and make the size and
// epoch assertions racy. SyncNever keeps the 1000-op churn fast; Close
// still flushes, which the reopen below relies on.
func TestEngineCompact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	e, err := Open(path, btypedb.WithAutoCompactDisabled(), WithSyncNever())
	if err != nil {
		t.Fatal(err)
	}
	usersTable(t, e)

	// Insert then delete most rows so the log is dominated by dead
	// records that a compaction can drop.
	const n = 500
	for i := 1; i <= n; i++ {
		if err := e.Insert("users", i, "user", float64(i), i%2 == 0, nil); err != nil {
			t.Fatal(err)
		}
	}
	for i := 3; i <= n; i++ {
		if _, err := e.Delete("users", i); err != nil {
			t.Fatal(err)
		}
	}

	before := e.Stats()
	if err := e.Compact(); err != nil {
		t.Fatal(err)
	}
	after := e.Stats()

	if after.LogBytes >= before.LogBytes {
		t.Fatalf("log did not shrink: %d -> %d bytes", before.LogBytes, after.LogBytes)
	}
	if after.LogEpoch == before.LogEpoch {
		t.Fatalf("log epoch not bumped by compaction: still %d", after.LogEpoch)
	}
	// The churn was all growth past the base taken at Open; the
	// compaction resets the base to the compacted file, with no writers
	// running to put anything past it.
	if before.LogBytes-before.LogBaseBytes <= 0 {
		t.Fatalf("before compaction: log %d, base %d; want growth past the base",
			before.LogBytes, before.LogBaseBytes)
	}
	if after.LogBaseBytes != after.LogBytes {
		t.Fatalf("after compaction: log %d, base %d; want equal", after.LogBytes, after.LogBaseBytes)
	}

	if rows := collect(t, e.Scan("users")); len(rows) != 2 {
		t.Fatalf("live engine has %d rows after compact, want 2", len(rows))
	}

	// Writes after compaction must append to the new log and survive.
	if err := e.Insert("users", 1000, "post", 1.0, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openEngine(t, path)
	defer reopened.Close()
	if reopened.Table("users") == nil {
		t.Fatal("users table missing after reopening compacted file")
	}
	rows := collect(t, reopened.Scan("users"))
	if len(rows) != 3 {
		t.Fatalf("reopened compacted db has %d rows, want 3", len(rows))
	}
	if rows[0].Col("id") != int64(1) || rows[2].Col("id") != int64(1000) {
		t.Fatalf("reopened rows wrong: %v", rows)
	}
}

// TestEngineCompactClosed confirms Compact on a closed engine reports
// an error instead of touching the file.
func TestEngineCompactClosed(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Compact(); err == nil {
		t.Fatal("Compact on closed engine: want error, got nil")
	}
}
