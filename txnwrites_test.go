package bytdb

// txnwrites_test.go: the transaction write cap (SetMaxTxnWrites) —
// which writes count, which are exempt, how savepoint rollbacks give
// writes back, and that a refusal leaves the transaction committable.

import (
	"errors"
	"path/filepath"
	"testing"
)

func capEngine(t *testing.T) *Engine {
	t.Helper()
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	t.Cleanup(func() { e.Close() })
	usersTable(t, e)
	return e
}

func insertUser(tx *Txn, id int64) error {
	return tx.Insert("users", id, "n", 1.0, true, nil)
}

func TestTxnWriteCapDefault(t *testing.T) {
	e := capEngine(t)
	if got := e.MaxTxnWrites(); got != DefaultMaxTxnWrites {
		t.Fatalf("MaxTxnWrites on open = %d, want %d", got, DefaultMaxTxnWrites)
	}
}

// TestTxnWriteCapCounts pins what is charged: inserts and updates that
// stage a write. Failed inserts, updates that match nothing, and
// deletes are not, and a refused write leaves the transaction
// committable with everything it staged before the refusal.
func TestTxnWriteCapCounts(t *testing.T) {
	e := capEngine(t)
	for i := range 5 {
		if err := e.Insert("users", int64(100+i), "seed", 1.0, true, nil); err != nil {
			t.Fatal(err)
		}
	}
	e.SetMaxTxnWrites(3)

	tx, err := e.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := insertUser(tx, 1); err != nil {
		t.Fatal(err)
	}
	// A duplicate key stages nothing, so it must not be charged.
	if err := insertUser(tx, 1); err == nil {
		t.Fatal("duplicate insert succeeded")
	}
	// Nor an update of a missing row.
	if ok, err := tx.Update("users", []any{int64(999)}, map[string]any{"name": "x"}); err != nil || ok {
		t.Fatalf("update of missing row: ok=%v err=%v", ok, err)
	}
	if _, err := tx.Update("users", []any{int64(100)}, map[string]any{"name": "u"}); err != nil {
		t.Fatal(err)
	}
	// Deletes are exempt however many there are.
	for i := range 5 {
		if _, err := tx.Delete("users", int64(100+i)); err != nil {
			t.Fatal(err)
		}
	}
	if got := tx.Writes(); got != 2 {
		t.Fatalf("Writes() = %d after one insert and one update, want 2", got)
	}
	if err := insertUser(tx, 2); err != nil {
		t.Fatal(err)
	}

	// At the cap: the next insert and the next update are both refused.
	if err := insertUser(tx, 3); !errors.Is(err, ErrTxnTooLarge) {
		t.Fatalf("insert past the cap: err = %v, want ErrTxnTooLarge", err)
	}
	if _, err := tx.Update("users", []any{int64(1)}, map[string]any{"name": "v"}); !errors.Is(err, ErrTxnTooLarge) {
		t.Fatalf("update past the cap: err = %v, want ErrTxnTooLarge", err)
	}
	if got := tx.Writes(); got != 3 {
		t.Fatalf("Writes() = %d after refusals, want 3", got)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit after a refusal: %v", err)
	}
	n := 0
	for _, err := range e.Scan("users") {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("rows after commit = %d, want 2 (ids 1 and 2; seeds deleted)", n)
	}
}

// TestTxnWriteCapSavepoints: rolling back to a savepoint gives back the
// writes it discards; releasing one keeps them.
func TestTxnWriteCapSavepoints(t *testing.T) {
	e := capEngine(t)
	e.SetMaxTxnWrites(4)
	tx, err := e.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	mustInsert := func(id int64) {
		t.Helper()
		if err := insertUser(tx, id); err != nil {
			t.Fatalf("insert %d: %v", id, err)
		}
	}

	mustInsert(1)
	a, err := tx.Savepoint()
	if err != nil {
		t.Fatal(err)
	}
	mustInsert(2)
	b, err := tx.Savepoint()
	if err != nil {
		t.Fatal(err)
	}
	mustInsert(3)
	mustInsert(4)
	if err := insertUser(tx, 5); !errors.Is(err, ErrTxnTooLarge) {
		t.Fatalf("insert past the cap: err = %v, want ErrTxnTooLarge", err)
	}

	// Rolling back to a, the earlier mark, also destroys b.
	if err := tx.RollbackTo(a); err != nil {
		t.Fatal(err)
	}
	if got := tx.Writes(); got != 1 {
		t.Fatalf("Writes() after ROLLBACK TO a = %d, want 1", got)
	}
	if err := tx.RollbackTo(b); err == nil {
		t.Fatal("rollback to a destroyed savepoint succeeded")
	}
	mustInsert(12)
	mustInsert(13)

	// a stays valid after a rollback to it, so it can be used again.
	if err := tx.RollbackTo(a); err != nil {
		t.Fatal(err)
	}
	if got := tx.Writes(); got != 1 {
		t.Fatalf("Writes() after second ROLLBACK TO a = %d, want 1", got)
	}

	// Release keeps the writes made since the mark.
	mustInsert(22)
	if err := tx.Release(a); err != nil {
		t.Fatal(err)
	}
	if got := tx.Writes(); got != 2 {
		t.Fatalf("Writes() after RELEASE a = %d, want 2", got)
	}
	if len(tx.saveWrites) != 0 {
		t.Fatalf("savepoint mirror not emptied by release: %d marks left", len(tx.saveWrites))
	}
	mustInsert(23)
	mustInsert(24)
	if err := insertUser(tx, 25); !errors.Is(err, ErrTxnTooLarge) {
		t.Fatalf("insert past the cap after release: err = %v, want ErrTxnTooLarge", err)
	}
}

// TestTxnWriteCapScope: WriteTxn is capped like Begin; one-shot writes
// and a disabled cap are not; CheckWrites answers for a batch.
func TestTxnWriteCapScope(t *testing.T) {
	e := capEngine(t)
	e.SetMaxTxnWrites(2)

	err := e.WriteTxn(func(tx *Txn) error {
		for i := range 3 {
			if err := insertUser(tx, int64(i)); err != nil {
				return err
			}
		}
		return nil
	})
	if !errors.Is(err, ErrTxnTooLarge) {
		t.Fatalf("WriteTxn past the cap: err = %v, want ErrTxnTooLarge", err)
	}
	if _, found, _ := e.Get("users", int64(0)); found {
		t.Fatal("refused WriteTxn left rows behind")
	}

	// One-shot writes are their own one-row transactions.
	for i := range 5 {
		if err := e.Insert("users", int64(i), "n", 1.0, true, nil); err != nil {
			t.Fatalf("one-shot insert %d under cap 2: %v", i, err)
		}
	}

	err = e.WriteTxn(func(tx *Txn) error {
		if err := tx.CheckWrites(2); err != nil {
			t.Errorf("CheckWrites(2) under cap 2: %v", err)
		}
		if err := tx.CheckWrites(3); !errors.Is(err, ErrTxnTooLarge) {
			t.Errorf("CheckWrites(3) under cap 2: err = %v, want ErrTxnTooLarge", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	e.SetMaxTxnWrites(0)
	err = e.WriteTxn(func(tx *Txn) error {
		for i := range 10 {
			if err := insertUser(tx, int64(100+i)); err != nil {
				return err
			}
		}
		return tx.CheckWrites(1 << 40)
	})
	if err != nil {
		t.Fatalf("disabled cap refused: %v", err)
	}
}
