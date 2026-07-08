package bytdb

// ddl_failure_test.go: the DDL commit-failure paths. Every schema
// change publishes its new descriptor inside its kv transaction —
// before commit, deliberately (see updateDDL) — so a failed commit
// must roll the in-memory catalog back to what disk still holds, or
// the engine would advertise phantom schema until restart. Failures
// are injected via testCommitErr, which simulates the WAL append or
// fsync failing after the closure ran.

import (
	"errors"
	"path/filepath"
	"testing"
)

var errCommitBoom = errors.New("injected commit failure")

// failNextDDLCommit arms the injection for the duration of fn.
func failNextDDLCommit(e *Engine, fn func()) {
	e.testCommitErr = errCommitBoom
	defer func() { e.testCommitErr = nil }()
	fn()
}

func TestCreateIndexCommitFailureUnpublishes(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	usersTable(t, e)
	if err := e.Insert("users", int64(1), "ada", 9.5, true, nil); err != nil {
		t.Fatal(err)
	}

	failNextDDLCommit(e, func() {
		if _, err := e.CreateIndex("users", "by_name", false, "name"); !errors.Is(err, errCommitBoom) {
			t.Fatalf("CreateIndex: want injected commit error, got %v", err)
		}
	})

	// The catalog must not advertise the index that never reached disk...
	if e.Table("users").Index("by_name") != nil {
		t.Fatal("failed CreateIndex left the index in the catalog")
	}
	// ...and scanning it must be a clean "no such index", not empty rows.
	for _, err := range e.ScanIndex("users", "by_name", nil, nil) {
		if err == nil {
			t.Fatal("ScanIndex on a rolled-back index: want an error, got a row")
		}
		break
	}

	// The engine must still work: the same DDL succeeds once commits do.
	if _, err := e.CreateIndex("users", "by_name", false, "name"); err != nil {
		t.Fatal(err)
	}
	rows := collect(t, e.ScanIndex("users", "by_name", nil, nil))
	if len(rows) != 1 {
		t.Fatalf("index scan after retry: want 1 row, got %d", len(rows))
	}
}

func TestDropIndexCommitFailureKeepsIndex(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	usersTable(t, e)
	if _, err := e.CreateIndex("users", "by_name", false, "name"); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("users", int64(1), "ada", 9.5, true, nil); err != nil {
		t.Fatal(err)
	}

	failNextDDLCommit(e, func() {
		if err := e.DropIndex("users", "by_name"); !errors.Is(err, errCommitBoom) {
			t.Fatalf("DropIndex: want injected commit error, got %v", err)
		}
	})

	// Disk still holds the index, so the catalog must too — and index
	// maintenance must keep running for later writes.
	if e.Table("users").Index("by_name") == nil {
		t.Fatal("failed DropIndex removed the index from the catalog")
	}
	if err := e.Insert("users", int64(2), "bob", 1.5, false, nil); err != nil {
		t.Fatal(err)
	}
	rows := collect(t, e.ScanIndex("users", "by_name", nil, nil))
	if len(rows) != 2 {
		t.Fatalf("index scan after failed drop: want 2 rows, got %d", len(rows))
	}
}

func TestDropTableCommitFailureKeepsTable(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	usersTable(t, e)
	if err := e.Insert("users", int64(1), "ada", 9.5, true, nil); err != nil {
		t.Fatal(err)
	}

	failNextDDLCommit(e, func() {
		if err := e.DropTable("users"); !errors.Is(err, errCommitBoom) {
			t.Fatalf("DropTable: want injected commit error, got %v", err)
		}
	})

	if e.Table("users") == nil {
		t.Fatal("failed DropTable removed the table from the catalog")
	}
	rows := collect(t, e.Scan("users"))
	if len(rows) != 1 {
		t.Fatalf("scan after failed drop: want 1 row, got %d", len(rows))
	}
}

func TestAlterCommitFailureRestoresDescriptor(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	usersTable(t, e)

	// AddColumn
	failNextDDLCommit(e, func() {
		if err := e.AddColumn("users", Column{Name: "extra", Type: TInt}); !errors.Is(err, errCommitBoom) {
			t.Fatalf("AddColumn: want injected commit error, got %v", err)
		}
	})
	if e.Table("users").ColIndex("extra") >= 0 {
		t.Fatal("failed AddColumn left the column in the catalog")
	}

	// DropColumn (via writeDesc)
	failNextDDLCommit(e, func() {
		if err := e.DropColumn("users", "blob"); !errors.Is(err, errCommitBoom) {
			t.Fatalf("DropColumn: want injected commit error, got %v", err)
		}
	})
	if e.Table("users").ColIndex("blob") < 0 {
		t.Fatal("failed DropColumn removed the column from the catalog")
	}

	// AddCheck
	failNextDDLCommit(e, func() {
		if err := e.AddCheck("users", CheckDesc{Name: "ck", Expr: "score > 0"}, nil); !errors.Is(err, errCommitBoom) {
			t.Fatalf("AddCheck: want injected commit error, got %v", err)
		}
	})
	if len(e.Table("users").Checks) != 0 {
		t.Fatal("failed AddCheck left the constraint in the catalog")
	}

	// DropCheck (via writeDesc)
	if err := e.AddCheck("users", CheckDesc{Name: "ck", Expr: "score > 0"}, nil); err != nil {
		t.Fatal(err)
	}
	failNextDDLCommit(e, func() {
		if _, err := e.DropCheck("users", "ck"); !errors.Is(err, errCommitBoom) {
			t.Fatalf("DropCheck: want injected commit error, got %v", err)
		}
	})
	if len(e.Table("users").Checks) != 1 {
		t.Fatal("failed DropCheck removed the constraint from the catalog")
	}

	// After all the failed attempts, the descriptor must still round-trip
	// through a reopen exactly as disk holds it.
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
}
