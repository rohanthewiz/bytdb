package bytdb

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/btypedb"
)

// Crash and fault tests at the bytdb level. btypedb fault-tests its own
// WAL (torn tails, power loss, mid-batch truncation); these tests pin
// the *relational* guarantees on top of it: committed rows and their
// index entries survive a kill, and every possible crash point leaves
// each row all-present-or-all-absent across the table and all of its
// indexes.
//
// A "kill" is simulated by copying the database file while the engine
// is open (or after writes, before Close): the copy is exactly the
// on-disk state a process death would leave — no final Close fsync, no
// clean shutdown. btypedb takes no file lock, so reading the live file
// is safe at a quiescent point.

// snapshotFile copies the live database file into a fresh path,
// simulating a kill at this instant.
func snapshotFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// openBytes opens an engine over a byte image written to its own file.
func openBytes(t *testing.T, dir, name string, image []byte) (*Engine, error) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, image, 0o644); err != nil {
		t.Fatal(err)
	}
	return Open(p)
}

// TestKillWithoutClose: under SyncNever — the policy with no fsync
// safety net at all — rows whose Insert returned must survive a kill
// (the writes are in the file, just not synced; a snapshot of the file
// is the crash state). Every row must come back through the table scan
// AND the secondary index: replay rebuilds both from the same batches.
func TestKillWithoutClose(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.db")
	e, err := Open(live, btypedb.WithSyncPolicy(btypedb.SyncNever))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close() // resource cleanup only; the snapshot is taken before
	if _, err := e.CreateTable("t", []Column{
		{Name: "id", Type: TInt}, {Name: "x", Type: TInt},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateIndex("t", "by_x", false, "x"); err != nil {
		t.Fatal(err)
	}
	const n = 50
	for i := range n {
		if err := e.Insert("t", i, i*10); err != nil {
			t.Fatal(err)
		}
	}

	// Kill: reopen the file image as it stands, no Close in between.
	killed, err := openBytes(t, dir, "killed.db", snapshotFile(t, live))
	if err != nil {
		t.Fatalf("open after kill: %v", err)
	}
	defer killed.Close()
	if got := len(collect(t, killed.Scan("t"))); got != n {
		t.Fatalf("table scan after kill: %d rows; want %d", got, n)
	}
	if got := len(collect(t, killed.ScanIndex("t", "by_x", nil, nil))); got != n {
		t.Fatalf("index scan after kill: %d rows; want %d", got, n)
	}
}

// TestGarbageTailRecovery: a crash can leave trailing bytes that were
// never a complete record (a torn append). Whatever junk follows the
// last good record, Open must recover every previously committed row
// and not error.
func TestGarbageTailRecovery(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.db")
	e, err := Open(live)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTable("t", []Column{
		{Name: "id", Type: TInt}, {Name: "x", Type: TInt},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		if err := e.Insert("t", i, i); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	image := snapshotFile(t, live)

	for _, junk := range [][]byte{
		{0x00},
		{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		append([]byte{0x01, 0x02, 0x03}, make([]byte, 512)...),
	} {
		re, err := openBytes(t, dir, fmt.Sprintf("junk%d.db", len(junk)), append(append([]byte{}, image...), junk...))
		if err != nil {
			t.Fatalf("open with %d junk bytes: %v", len(junk), err)
		}
		if got := len(collect(t, re.Scan("t"))); got != 10 {
			t.Fatalf("with %d junk bytes: %d rows; want 10", len(junk), got)
		}
		re.Close()
	}
}

// TestCrashPointAtomicity is the exhaustive fault sweep: a table with
// two secondary indexes, killed at *every byte offset* a crash could
// truncate the log to. At each crash point the database must open, the
// schema must be intact, and the surviving rows must be a prefix of
// the insertion order with their index entries exactly matching — a
// row half-applied (present in the table but missing from an index, or
// vice versa) at any offset means multi-key batch replay is broken.
func TestCrashPointAtomicity(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.db")
	e, err := Open(live)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTable("t", []Column{
		{Name: "id", Type: TInt}, {Name: "x", Type: TInt}, {Name: "y", Type: TString},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateIndex("t", "by_x", false, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateIndex("t", "by_y", false, "y"); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	// The schema boundary: cuts are taken only past this point, so the
	// catalog itself is always whole and recovery questions are about
	// row batches alone. (Cutting into the schema region legitimately
	// loses tables — WAL semantics — so it proves nothing here.)
	schemaImage := snapshotFile(t, live)

	e, err = Open(live)
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	for i := range n {
		// One Insert = one commit batch: row + by_x entry + by_y entry.
		if err := e.Insert("t", i, i*10, fmt.Sprintf("s%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	image := snapshotFile(t, live)
	if len(image) <= len(schemaImage) {
		t.Fatalf("no bytes appended by inserts? schema=%d full=%d", len(schemaImage), len(image))
	}

	// Exhaustive by default (~8s of repeated Opens); -short samples
	// crash points instead, still crossing every batch boundary region.
	step := 1
	if testing.Short() {
		step = 7
	}
	for cut := len(schemaImage); cut <= len(image); cut += step {
		re, err := openBytes(t, dir, "cut.db", image[:cut])
		if err != nil {
			t.Fatalf("cut %d/%d: open failed: %v", cut, len(image), err)
		}
		rows := collect(t, re.Scan("t"))
		// Survivors are a prefix of insertion order: batches append in
		// commit order and replay stops at the torn one.
		for i, r := range rows {
			if r.Col("id") != int64(i) {
				t.Fatalf("cut %d: rows are not an insertion-order prefix: row %d has id %v", cut, i, r.Col("id"))
			}
		}
		// Each index must agree with the table exactly.
		for _, idx := range []string{"by_x", "by_y"} {
			ir := collect(t, re.ScanIndex("t", idx, nil, nil))
			if len(ir) != len(rows) {
				t.Fatalf("cut %d: table has %d rows but %s has %d entries", cut, len(rows), idx, len(ir))
			}
		}
		re.Close()
	}
}
