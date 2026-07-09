package bytdb

// seq_ops_test.go: sequence operations through the Txn API —
// SetSeq/PeekSeq/DeleteSeq inside transactions, rollback of a SetSeq,
// name validation on every entry point, and reentrancy guards on the
// one-shot forms.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestTxnSeqOps(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()

	err := e.WriteTxn(func(tx *Txn) error {
		if err := tx.SetSeq("s", 50); err != nil {
			return err
		}
		next, ok, err := tx.PeekSeq("s")
		if err != nil || !ok || next != 50 {
			t.Fatalf("txn PeekSeq after SetSeq(50) = %d ok=%v err=%v, want 50 true nil", next, ok, err)
		}
		v, err := tx.NextSeq("s")
		if err != nil || v != 50 {
			t.Fatalf("txn NextSeq after SetSeq(50) = %d err=%v, want 50 nil", v, err)
		}
		existed, err := tx.DeleteSeq("s")
		if err != nil || !existed {
			t.Fatalf("txn DeleteSeq = existed=%v err=%v, want true nil", existed, err)
		}
		existed, err = tx.DeleteSeq("s")
		if err != nil || existed {
			t.Fatalf("txn DeleteSeq(absent) = existed=%v err=%v, want false nil", existed, err)
		}
		if _, ok, err := tx.PeekSeq("s"); err != nil || ok {
			t.Fatalf("txn PeekSeq after delete = ok=%v err=%v, want false nil", ok, err)
		}
		// Recreate so the commit publishes a final state we can verify.
		return tx.SetSeq("s", 7)
	})
	if err != nil {
		t.Fatal(err)
	}
	if next, ok, err := e.PeekSeq("s"); err != nil || !ok || next != 7 {
		t.Fatalf("PeekSeq after commit = %d ok=%v err=%v, want 7 true nil", next, ok, err)
	}
}

// TestTxnSetSeqRollback: a SetSeq inside a rolled-back transaction
// leaves the sequence untouched (here: still absent).
func TestTxnSetSeqRollback(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()

	boom := errors.New("boom")
	err := e.WriteTxn(func(tx *Txn) error {
		if err := tx.SetSeq("s", 99); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WriteTxn error = %v, want boom", err)
	}
	if _, ok, err := e.PeekSeq("s"); err != nil || ok {
		t.Fatalf("PeekSeq after rollback = ok=%v err=%v, want false nil (SetSeq rolled back)", ok, err)
	}
}

// TestSeqNameValidation: every entry point — one-shot and
// transactional — rejects the empty sequence name.
func TestSeqNameValidation(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()

	if err := e.SetSeq("", 1); err == nil {
		t.Fatal("SetSeq with an empty name must fail")
	}
	if _, _, err := e.PeekSeq(""); err == nil {
		t.Fatal("PeekSeq with an empty name must fail")
	}
	if _, err := e.DeleteSeq(""); err == nil {
		t.Fatal("DeleteSeq with an empty name must fail")
	}
	err := e.WriteTxn(func(tx *Txn) error {
		if err := tx.SetSeq("", 1); err == nil {
			t.Fatal("txn SetSeq with an empty name must fail")
		}
		if _, _, err := tx.PeekSeq(""); err == nil {
			t.Fatal("txn PeekSeq with an empty name must fail")
		}
		if _, err := tx.DeleteSeq(""); err == nil {
			t.Fatal("txn DeleteSeq with an empty name must fail")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestSeqWriteReentrancy: the one-shot write forms must error (not
// deadlock) when called from the goroutine holding a write transaction.
func TestSeqWriteReentrancy(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	err := e.WriteTxn(func(tx *Txn) error {
		if err := e.SetSeq("s", 1); err == nil || !strings.Contains(err.Error(), "deadlock") {
			t.Fatalf("one-shot SetSeq inside WriteTxn: got %v, want a reentrancy error", err)
		}
		if _, err := e.DeleteSeq("s"); err == nil || !strings.Contains(err.Error(), "deadlock") {
			t.Fatalf("one-shot DeleteSeq inside WriteTxn: got %v, want a reentrancy error", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
