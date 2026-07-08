package bytdb

// seq_test.go: named sequences — allocation from 1, per-name
// independence, durability across reopen, transactional rollback,
// SetSeq/PeekSeq/DeleteSeq, corruption, exhaustion, and distinctness
// under concurrent allocation.

import (
	"errors"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestNextSeqBasics(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()

	for want := uint64(1); want <= 3; want++ {
		got, err := e.NextSeq("orders")
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("NextSeq(orders) = %d, want %d", got, want)
		}
	}
	// A second sequence counts independently.
	got, err := e.NextSeq("users")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("NextSeq(users) = %d, want 1", got)
	}

	if _, err := e.NextSeq(""); err == nil {
		t.Fatal("NextSeq with an empty name must fail")
	}
}

func TestSeqSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	e := openEngine(t, path)
	for range 5 {
		if _, err := e.NextSeq("s"); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	got, err := e2.NextSeq("s")
	if err != nil {
		t.Fatal(err)
	}
	if got != 6 {
		t.Fatalf("NextSeq after reopen = %d, want 6", got)
	}
}

// TestTxnSeqRollback: a bump inside a rolled-back transaction is not a
// bump — the next allocation re-issues the value.
func TestTxnSeqRollback(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()

	boom := errors.New("boom")
	err := e.WriteTxn(func(tx *Txn) error {
		v, err := tx.NextSeq("s")
		if err != nil {
			return err
		}
		if v != 1 {
			t.Fatalf("NextSeq in txn = %d, want 1", v)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WriteTxn error = %v, want boom", err)
	}

	got, err := e.NextSeq("s")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("NextSeq after rollback = %d, want 1 (value returned to the sequence)", got)
	}
}

func TestSetPeekDeleteSeq(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()

	// Peek on an absent sequence.
	if _, ok, err := e.PeekSeq("s"); err != nil || ok {
		t.Fatalf("PeekSeq(absent) = ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	// SetSeq creates; NextSeq continues from there.
	if err := e.SetSeq("s", 100); err != nil {
		t.Fatal(err)
	}
	if next, ok, err := e.PeekSeq("s"); err != nil || !ok || next != 100 {
		t.Fatalf("PeekSeq after SetSeq(100) = %d ok=%v err=%v, want 100 true nil", next, ok, err)
	}
	got, err := e.NextSeq("s")
	if err != nil {
		t.Fatal(err)
	}
	if got != 100 {
		t.Fatalf("NextSeq after SetSeq(100) = %d, want 100", got)
	}

	// Peek does not allocate.
	if next, _, err := e.PeekSeq("s"); err != nil || next != 101 {
		t.Fatalf("PeekSeq = %d err=%v, want 101 nil", next, err)
	}
	if got, err := e.NextSeq("s"); err != nil || got != 101 {
		t.Fatalf("NextSeq = %d err=%v, want 101 nil", got, err)
	}

	// Delete resets: the sequence restarts at 1 on next use.
	existed, err := e.DeleteSeq("s")
	if err != nil || !existed {
		t.Fatalf("DeleteSeq = existed=%v err=%v, want true nil", existed, err)
	}
	existed, err = e.DeleteSeq("s")
	if err != nil || existed {
		t.Fatalf("DeleteSeq(absent) = existed=%v err=%v, want false nil", existed, err)
	}
	if got, err := e.NextSeq("s"); err != nil || got != 1 {
		t.Fatalf("NextSeq after delete = %d err=%v, want 1 nil", got, err)
	}
}

func TestSeqCorruptValue(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	if _, err := e.NextSeq("s"); err != nil {
		t.Fatal(err)
	}
	setRaw(t, e, seqNameKey("s"), []byte{1, 2, 3}) // not 8 bytes

	if _, err := e.NextSeq("s"); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("NextSeq over a corrupt value: got %v, want a corruption error", err)
	}
	if _, _, err := e.PeekSeq("s"); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("PeekSeq over a corrupt value: got %v, want a corruption error", err)
	}
}

func TestSeqExhaustion(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	if err := e.SetSeq("s", math.MaxUint64); err != nil {
		t.Fatal(err)
	}
	if _, err := e.NextSeq("s"); err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("NextSeq at MaxUint64: got %v, want an exhaustion error", err)
	}
}

// TestSeqNamespaceSeparateFromTables: a sequence and a table may share
// a name; the sequence's key lives in its own system table.
func TestSeqNamespaceSeparateFromTables(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	if _, err := e.CreateTable("t", []Column{{Name: "id", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	if got, err := e.NextSeq("t"); err != nil || got != 1 {
		t.Fatalf("NextSeq(t) = %d err=%v, want 1 nil", got, err)
	}
	// Neither disturbed the other: the table still resolves, and a
	// fresh table gets a fresh ID (the table-id counter is untouched).
	if e.Table("t") == nil {
		t.Fatal("table t vanished after NextSeq(t)")
	}
	d1 := e.Table("t")
	d2, err := e.CreateTable("t2", []Column{{Name: "id", Type: TInt}}, "id")
	if err != nil {
		t.Fatal(err)
	}
	if d2.ID != d1.ID+1 {
		t.Fatalf("table IDs %d then %d, want consecutive", d1.ID, d2.ID)
	}
}

// TestConcurrentNextSeq: racing allocations must produce distinct
// values — the counter read-modify-write runs under the writer lock.
func TestConcurrentNextSeq(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()

	const workers, perWorker = 8, 25
	vals := make([][]uint64, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				v, err := e.NextSeq("s")
				if err != nil {
					t.Error(err)
					return
				}
				vals[i] = append(vals[i], v)
			}
		}()
	}
	wg.Wait()

	seen := map[uint64]bool{}
	for _, vs := range vals {
		for _, v := range vs {
			if seen[v] {
				t.Fatalf("value %d allocated twice", v)
			}
			seen[v] = true
		}
	}
	if len(seen) != workers*perWorker {
		t.Fatalf("%d distinct values, want %d", len(seen), workers*perWorker)
	}
}

// TestNextSeqReentrantWrite: a one-shot NextSeq from the goroutine
// holding the open write transaction must error, not deadlock.
func TestNextSeqReentrantWrite(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	err := e.WriteTxn(func(tx *Txn) error {
		_, err := e.NextSeq("s")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "deadlock") {
		t.Fatalf("one-shot NextSeq inside WriteTxn: got %v, want a reentrancy error", err)
	}
}
