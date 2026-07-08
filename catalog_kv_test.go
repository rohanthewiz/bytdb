package bytdb

// catalog_kv_test.go: behaviors specific to the catalog living in the
// kv keyspace. DDL no longer serializes on a dedicated mutex — each
// schema change resolves the descriptor it mutates inside its own kv
// transaction, under the writer lock — so concurrent ALTERs must
// compose instead of losing updates, and a transaction's schema view
// must be exactly its snapshot's.

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestConcurrentAlterNoLostUpdate races many AddColumn calls on one
// table. Each must build on the committed descriptor before it; if any
// resolved a stale descriptor outside its transaction, one column
// would silently vanish under another's write (last-writer-wins).
func TestConcurrentAlterNoLostUpdate(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	if _, err := e.CreateTable("t", []Column{{Name: "id", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}

	const n = 16
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.AddColumn("t", Column{Name: fmt.Sprintf("c%02d", i), Type: TInt}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	desc := e.Table("t")
	if got := len(desc.Columns); got != n+1 {
		t.Fatalf("lost update: %d columns survive of %d added (+ id)", got-1, n)
	}
	// Column IDs must be distinct — NextColID also composes.
	seen := map[uint32]bool{}
	for _, c := range desc.Columns {
		if seen[c.ID] {
			t.Fatalf("column ID %d assigned twice", c.ID)
		}
		seen[c.ID] = true
	}
}

// TestConcurrentCreateTableSameName: exactly one racing CreateTable
// wins; the rest get "table already exists" (the check runs inside the
// transaction, against committed state).
func TestConcurrentCreateTableSameName(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.CreateTable("t", []Column{{Name: "id", Type: TInt}}, "id")
		}()
	}
	wg.Wait()

	won := 0
	for _, err := range errs {
		if err == nil {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d CreateTables succeeded; want exactly 1", won)
	}
	// The losers must not have burned the winner's key space: the
	// table works, and only one descriptor exists.
	if err := e.Insert("t", int64(1)); err != nil {
		t.Fatal(err)
	}
	if names := e.Tables(); len(names) != 1 || names[0] != "t" {
		t.Fatalf("Tables() = %v; want [t]", names)
	}
}

// TestTxnSchemaIsSnapshotSchema: a read transaction's Table() answers
// from its snapshot, not from the live catalog — a table created after
// the snapshot is invisible, a table dropped after it remains usable.
func TestTxnSchemaIsSnapshotSchema(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	usersTable(t, e)
	if err := e.Insert("users", int64(1), "ada", 9.5, true, nil); err != nil {
		t.Fatal(err)
	}

	tx, err := e.Begin(false)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	if _, err := e.CreateTable("later", []Column{{Name: "id", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.DropTable("users"); err != nil {
		t.Fatal(err)
	}

	if tx.Table("later") != nil {
		t.Fatal("read txn sees a table created after its snapshot")
	}
	if tx.Table("users") == nil {
		t.Fatal("read txn lost a table dropped after its snapshot")
	}
	rows := collect(t, tx.Scan("users"))
	if len(rows) != 1 || rows[0].Col("name") != "ada" {
		t.Fatalf("read txn cannot scan its snapshot of a dropped table: %v", rows)
	}
	// Live views disagree, correctly.
	if e.Table("later") == nil || e.Table("users") != nil {
		t.Fatal("live catalog does not reflect the DDL")
	}
}

// TestFailedDDLLeavesNoPhantomForWrites pins the closed T1.3 window:
// after a DDL whose commit failed, a one-shot write resolves its
// descriptor from committed state and maintains exactly the schema on
// disk — there is no instant at which it could see the phantom index.
func TestFailedDDLLeavesNoPhantomForWrites(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	usersTable(t, e)

	failNextDDLCommit(e, func() {
		if _, err := e.CreateIndex("users", "by_name", false, "name"); !errors.Is(err, errCommitBoom) {
			t.Fatalf("want injected commit error, got %v", err)
		}
	})
	// A write immediately after the failed DDL: it must not write
	// entries for the never-committed index.
	if err := e.Insert("users", int64(1), "ada", 9.5, true, nil); err != nil {
		t.Fatal(err)
	}
	// Creating the index for real must backfill that row exactly once
	// (a phantom-maintained entry would collide or duplicate).
	if _, err := e.CreateIndex("users", "by_name", true, "name"); err != nil {
		t.Fatal(err)
	}
	rows := collect(t, e.ScanIndex("users", "by_name", nil, nil))
	if len(rows) != 1 {
		t.Fatalf("index holds %d rows; want 1", len(rows))
	}
}
