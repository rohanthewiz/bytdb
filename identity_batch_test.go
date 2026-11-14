package bytdb

// identity_batch_test.go: serialized-mode identity counters held in
// the Txn between draws (idCounters, deep-look D-45). Each test pins
// one rule from idCounters' comment: the counter reaches the kv store
// at commit and at savepoints, a savepoint rollback rewinds it, and
// every other in-transaction access (explicit values, UPDATE, RESTART,
// TRUNCATE ... RESTART IDENTITY, PeekIdentity) agrees with the cache.

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/btypedb"
)

// drawIn inserts a row with a NULL identity in tx and returns the
// value it drew.
func drawIn(t *testing.T, tx *Txn) int64 {
	t.Helper()
	row, err := tx.InsertReturning("t", nil, "x")
	if err != nil {
		t.Fatal(err)
	}
	return row.Vals[0].(int64)
}

// drawOne draws one value in its own WriteTxn.
func drawOne(t *testing.T, e *Engine) int64 {
	t.Helper()
	var v int64
	if err := e.WriteTxn(func(tx *Txn) error { v = drawIn(t, tx); return nil }); err != nil {
		t.Fatal(err)
	}
	return v
}

// expectDraws asserts a run of draws in tx yields want, in order.
func expectDraws(t *testing.T, tx *Txn, want ...int64) {
	t.Helper()
	for _, w := range want {
		if got := drawIn(t, tx); got != w {
			t.Fatalf("draw = %d, want %d", got, w)
		}
	}
}

// Many draws in one transaction commit the final counter: the next
// transaction, and the next process, continue after the last value.
// Both commit paths flush: WriteTxn's closure return and Txn.Commit.
func TestIdentityBatchedDrawsCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	e := openEngine(t, path)
	idTable(t, e)

	if err := e.WriteTxn(func(tx *Txn) error {
		expectDraws(t, tx, 1, 2, 3, 4, 5)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := e.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	expectDraws(t, tx, 6, 7, 8)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if next, ok, err := e.PeekIdentity("t", "id"); err != nil || !ok || next != 9 {
		t.Fatalf("stored counter after two batches: next=%d ok=%v err=%v, want 9", next, ok, err)
	}
	e.Close()

	e = openEngine(t, path)
	defer e.Close()
	if got := drawOne(t, e); got != 9 {
		t.Fatalf("draw after reopen = %d, want 9", got)
	}
}

// A transaction that rolls back takes its draws with it, as the
// per-row counter writes did: serialized-mode identity is gapless.
func TestIdentityBatchedDrawsRollBack(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	idTable(t, e)

	boom := errors.New("boom")
	err := e.WriteTxn(func(tx *Txn) error {
		expectDraws(t, tx, 1, 2, 3)
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WriteTxn error = %v, want boom", err)
	}
	tx, err := e.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	expectDraws(t, tx, 1, 2)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := drawOne(t, e); got != 1 {
		t.Fatalf("draw after two rolled-back transactions = %d, want 1", got)
	}
}

// Savepoints flush the counter into their mark, and ROLLBACK TO
// rewinds it with the rows, through nesting and RELEASE.
func TestIdentityBatchedSavepoints(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	idTable(t, e)

	tx, err := e.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	// A mark before any draw: rolling back to it forgets the counter
	// was ever loaded.
	sp0, err := tx.Savepoint()
	if err != nil {
		t.Fatal(err)
	}
	expectDraws(t, tx, 1, 2)
	if err := tx.RollbackTo(sp0); err != nil {
		t.Fatal(err)
	}
	expectDraws(t, tx, 1, 2, 3)

	spA, err := tx.Savepoint() // counter at 4
	if err != nil {
		t.Fatal(err)
	}
	expectDraws(t, tx, 4, 5)
	spB, err := tx.Savepoint() // counter at 6
	if err != nil {
		t.Fatal(err)
	}
	expectDraws(t, tx, 6)
	if err := tx.RollbackTo(spB); err != nil {
		t.Fatal(err)
	}
	expectDraws(t, tx, 6, 7)
	if err := tx.RollbackTo(spA); err != nil {
		t.Fatal(err)
	}
	expectDraws(t, tx, 4)

	// RELEASE keeps everything drawn under the mark.
	spC, err := tx.Savepoint()
	if err != nil {
		t.Fatal(err)
	}
	expectDraws(t, tx, 5, 6)
	if err := tx.Release(spC); err != nil {
		t.Fatal(err)
	}
	expectDraws(t, tx, 7)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Rows 1..7 are in the table, each once.
	n, err := e.Count("t")
	if err != nil || n != 7 {
		t.Fatalf("rows after commit: %d (err %v), want 7", n, err)
	}
	if got := drawOne(t, e); got != 8 {
		t.Fatalf("draw after commit = %d, want 8", got)
	}
}

// Explicit values and UPDATEs that set the identity column bump the
// cached counter, so draws after them in the same transaction (and
// after its commit) land past the values already used.
func TestIdentityBatchedBumps(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	idTable(t, e)

	if err := e.WriteTxn(func(tx *Txn) error {
		expectDraws(t, tx, 1, 2)
		if err := tx.Insert("t", 10, "explicit"); err != nil {
			return err
		}
		expectDraws(t, tx, 11)
		// An explicit value below the counter does not move it back.
		if err := tx.Insert("t", 5, "low"); err != nil {
			return err
		}
		expectDraws(t, tx, 12)
		if _, err := tx.Update("t", []any{1}, map[string]any{"id": 50}); err != nil {
			return err
		}
		expectDraws(t, tx, 51)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := drawOne(t, e); got != 52 {
		t.Fatalf("draw after commit = %d, want 52", got)
	}
}

// RestartIdentity and TRUNCATE ... RESTART IDENTITY write the counter
// record directly. The cache must yield to them, before the commit's
// write-back and across a savepoint rollback.
func TestIdentityBatchedRestarts(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	idTable(t, e)

	if err := e.WriteTxn(func(tx *Txn) error {
		expectDraws(t, tx, 1, 2, 3)
		if err := tx.RestartIdentity("t", "id", 100); err != nil {
			return err
		}
		expectDraws(t, tx, 100, 101)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := drawOne(t, e); got != 102 {
		t.Fatalf("draw after RESTART WITH 100 and two draws = %d, want 102", got)
	}

	if err := e.WriteTxn(func(tx *Txn) error {
		expectDraws(t, tx, 103)
		sp, err := tx.Savepoint()
		if err != nil {
			return err
		}
		if err := tx.RestartIdentity("t", "id", 500); err != nil {
			return err
		}
		expectDraws(t, tx, 500)
		if err := tx.RollbackTo(sp); err != nil {
			return err
		}
		expectDraws(t, tx, 104)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := e.WriteTxn(func(tx *Txn) error {
		expectDraws(t, tx, 105, 106)
		if err := tx.Truncate("t", true); err != nil {
			return err
		}
		expectDraws(t, tx, 1, 2)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := drawOne(t, e); got != 3 {
		t.Fatalf("draw after TRUNCATE ... RESTART IDENTITY and two draws = %d, want 3", got)
	}

	// Without RESTART IDENTITY the counter is untouched, cache included.
	if err := e.WriteTxn(func(tx *Txn) error {
		expectDraws(t, tx, 4)
		if err := tx.Truncate("t", false); err != nil {
			return err
		}
		expectDraws(t, tx, 5)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// PeekIdentity inside the transaction sees draws not yet written back.
func TestIdentityBatchedPeek(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	idTable(t, e)

	if err := e.WriteTxn(func(tx *Txn) error {
		if _, ok, err := tx.PeekIdentity("t", "id"); err != nil || ok {
			t.Fatalf("peek before any draw: ok=%v err=%v, want absent", ok, err)
		}
		expectDraws(t, tx, 1, 2, 3)
		next, ok, err := tx.PeekIdentity("t", "id")
		if err != nil || !ok || next != 4 {
			t.Fatalf("peek after three draws: next=%d ok=%v err=%v, want 4", next, ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A read-only transaction refuses an identity draw at the insert and
// an identity bump at the update, and committing it afterwards has
// nothing to write back.
func TestIdentityBatchedReadOnly(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	idTable(t, e)
	if got := drawOne(t, e); got != 1 {
		t.Fatalf("seed draw = %d, want 1", got)
	}

	tx, err := e.Begin(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Insert("t", nil, "x"); !errors.Is(err, btypedb.ErrTxNotWritable) {
		t.Fatalf("insert in a read-only transaction: %v, want ErrTxNotWritable", err)
	}
	// Row 1 exists, so the update gets as far as bumping the counter
	// past 9 before its row write would fail.
	if _, err := tx.Update("t", []any{1}, map[string]any{"id": 9}); !errors.Is(err, btypedb.ErrTxNotWritable) {
		t.Fatalf("update in a read-only transaction: %v, want ErrTxNotWritable", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit of a read-only transaction: %v", err)
	}
	if got := drawOne(t, e); got != 2 {
		t.Fatalf("draw after the read-only transaction = %d, want 2", got)
	}
}

// The point of the cache: a batch of draws writes the counter once,
// so the log grows by what the rows take plus one counter record, the
// same as a table whose key the caller supplies. Before, every row
// also logged its own counter record (deep-look D-45 measured 116 WAL
// bytes per identity row against 59 without).
func TestIdentityBatchLogsCounterOnce(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	idTable(t, e)
	if _, err := e.CreateTable("plain",
		[]Column{{Name: "id", Type: TInt}, {Name: "name", Type: TString}}, "id"); err != nil {
		t.Fatal(err)
	}
	logBytes := func() int64 { return e.Stats().LogBytes }
	const n = 500
	grow := func(table string, id func(i int) any) int64 {
		before := logBytes()
		if err := e.WriteTxn(func(tx *Txn) error {
			for i := range n {
				if err := tx.Insert(table, id(i), "x"); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return logBytes() - before
	}
	plain := grow("plain", func(i int) any { return i + 1 })
	drawn := grow("t", func(int) any { return nil })
	// Same row count and values; the identity batch may add one counter
	// record (well under 100 bytes), never one per row.
	if drawn-plain > 100 {
		t.Fatalf("identity batch of %d rows logged %d bytes, plain batch %d: "+
			"%d extra bytes, about %d per row", n, drawn, plain, drawn-plain, (drawn-plain)/n)
	}
}
