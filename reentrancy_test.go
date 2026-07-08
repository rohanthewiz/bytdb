package bytdb

// reentrancy_test.go: the write-reentrancy guard. The kv writer lock
// is not reentrant, so an engine one-shot write or DDL issued from the
// goroutine that already holds an open writable transaction would
// deadlock the whole engine forever. The guard must turn exactly that
// case into an error — and only that case: other goroutines blocking
// behind the transaction is normal.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reentrancyEngine(t *testing.T) *Engine {
	t.Helper()
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	t.Cleanup(func() { e.Close() })
	if _, err := e.CreateTable("t", []Column{
		{Name: "id", Type: TInt}, {Name: "v", Type: TInt},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	return e
}

func wantDeadlockErr(t *testing.T, err error, op string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s inside a write transaction: want a deadlock error, got nil", op)
	}
	if !strings.Contains(err.Error(), "deadlock") {
		t.Fatalf("%s inside a write transaction: got %q, want a deadlock error", op, err)
	}
}

func TestReentrantWriteInsideWriteTxn(t *testing.T) {
	e := reentrancyEngine(t)
	err := e.WriteTxn(func(tx *Txn) error {
		wantDeadlockErr(t, e.Insert("t", 1, 1), "Insert")
		_, err := e.Update("t", []any{1}, map[string]any{"v": 2})
		wantDeadlockErr(t, err, "Update")
		_, err = e.Delete("t", 1)
		wantDeadlockErr(t, err, "Delete")
		wantDeadlockErr(t, e.WriteTxn(func(*Txn) error { return nil }), "WriteTxn")
		_, err = e.Begin(true)
		wantDeadlockErr(t, err, "Begin(true)")
		_, err = e.CreateIndex("t", "v_idx", false, "v")
		wantDeadlockErr(t, err, "CreateIndex")
		_, err = e.CreateTable("t2", []Column{{Name: "id", Type: TInt}}, "id")
		wantDeadlockErr(t, err, "CreateTable")
		wantDeadlockErr(t, e.DropTable("t"), "DropTable")
		// The transaction itself must still be usable and committable.
		return tx.Insert("t", 1, 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := e.Get("t", 1); err != nil || !ok {
		t.Fatalf("row from the fenced transaction missing: ok=%v err=%v", ok, err)
	}
}

func TestReentrantWriteInsideBeginTxn(t *testing.T) {
	e := reentrancyEngine(t)
	tx, err := e.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	wantDeadlockErr(t, e.Insert("t", 1, 1), "Insert")
	if err := tx.Insert("t", 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// The marker must clear with the transaction: one-shot writes work
	// again on this same goroutine.
	if err := e.Insert("t", 2, 2); err != nil {
		t.Fatalf("insert after commit: %v", err)
	}

	// Rollback must clear it too.
	tx, err = e.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("t", 3, 3); err != nil {
		t.Fatalf("insert after rollback: %v", err)
	}
}

// TestOtherGoroutineWriteBlocksNormally pins the guard's negative
// space: a write from a different goroutine is not reentrancy — it
// must block behind the open transaction and succeed once it commits.
func TestOtherGoroutineWriteBlocksNormally(t *testing.T) {
	e := reentrancyEngine(t)
	tx, err := e.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.Insert("t", 1, 1) }()
	select {
	case err := <-done:
		// The insert must not finish (or spuriously error) while the
		// transaction is open.
		t.Fatalf("concurrent insert finished under an open write txn: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("concurrent insert after commit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent insert still blocked after commit")
	}
}
