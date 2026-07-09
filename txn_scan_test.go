package bytdb

// txn_scan_test.go: the Txn-level data paths the one-shot tests miss —
// Delete inside a transaction, reverse index scans over uncommitted
// writes, scans of unknown tables and indexes, transactions over a
// corrupted descriptor, and every scan flavor on a closed engine.

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// expectScanErr drains a row sequence expecting a yielded error that
// mentions want. Matches the helper style of names/collect but for the
// error path, where those would t.Fatal.
func expectScanErr(t *testing.T, seq func(func(Row, error) bool), want string) {
	t.Helper()
	var got error
	for _, err := range seq {
		if err != nil {
			got = err
			break
		}
	}
	if got == nil || !strings.Contains(got.Error(), want) {
		t.Fatalf("scan error = %v; want mention of %q", got, want)
	}
}

func TestTxnDelete(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "a@x"}, []any{2, "grace", 45, "g@x"})
	if _, err := e.CreateIndex("people", "by-age", false, "age"); err != nil {
		t.Fatal(err)
	}

	err := e.WriteTxn(func(tx *Txn) error {
		existed, err := tx.Delete("people", 1)
		if err != nil || !existed {
			t.Fatalf("txn Delete = existed=%v err=%v, want true nil", existed, err)
		}
		// The row's index entry is gone in the transaction's own view.
		got := names(t, tx.ScanIndex("people", "by-age", nil, nil))
		if want := []string{"grace"}; !slices.Equal(got, want) {
			t.Fatalf("index in txn after delete = %v; want %v", got, want)
		}
		if existed, err := tx.Delete("people", 1); err != nil || existed {
			t.Fatalf("txn Delete(missing) = existed=%v err=%v, want false nil", existed, err)
		}
		// Bad pk arity and unknown table error without staging writes.
		if _, err := tx.Delete("people"); err == nil {
			t.Fatal("txn Delete with wrong pk arity accepted")
		}
		if _, err := tx.Delete("ghosts", 1); err == nil {
			t.Fatal("txn Delete on unknown table accepted")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The delete committed; nothing outside the transaction still sees it.
	if _, ok, _ := e.Get("people", 1); ok {
		t.Fatal("committed delete not visible outside the transaction")
	}
}

// TestTxnScanIndexRev: reverse index scans inside a transaction see
// uncommitted writes and honor bounds, toIncl included.
func TestTxnScanIndexRev(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e,
		[]any{1, "grace", 45, "g@x"},
		[]any{3, "edsger", 51, "e@x"},
	)
	if _, err := e.CreateIndex("people", "by-age", false, "age"); err != nil {
		t.Fatal(err)
	}
	err := e.WriteTxn(func(tx *Txn) error {
		if err := tx.Insert("people", 2, "ada", 36, "a@x"); err != nil {
			return err
		}
		got := names(t, tx.ScanIndexRev("people", "by-age", nil, nil, false))
		if want := []string{"edsger", "grace", "ada"}; !slices.Equal(got, want) {
			t.Fatalf("txn reverse index scan = %v; want %v", got, want)
		}
		// Bounded: 36 <= age < 51, then the upper bound closed over 51.
		got = names(t, tx.ScanIndexRev("people", "by-age", []any{36}, []any{51}, false))
		if want := []string{"grace", "ada"}; !slices.Equal(got, want) {
			t.Fatalf("bounded txn reverse index scan = %v; want %v", got, want)
		}
		got = names(t, tx.ScanIndexRev("people", "by-age", []any{36}, []any{51}, true))
		if want := []string{"edsger", "grace", "ada"}; !slices.Equal(got, want) {
			t.Fatalf("inclusive-bounded txn reverse index scan = %v; want %v", got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestTxnScanUnknownTargets: every Txn scan flavor surfaces "no such
// table" / "no such index" as a yielded error, not a silent empty scan.
func TestTxnScanUnknownTargets(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)

	err := e.ReadTxn(func(tx *Txn) error {
		expectScanErr(t, tx.ScanRange("ghosts", nil, nil), "no such table")
		expectScanErr(t, tx.ScanRangeRev("ghosts", nil, nil, false), "no such table")
		expectScanErr(t, tx.ScanIndex("ghosts", "i", nil, nil), "no such table")
		expectScanErr(t, tx.ScanIndexRev("ghosts", "i", nil, nil, false), "no such table")
		expectScanErr(t, tx.ScanIndex("people", "nope", nil, nil), "no such index")
		expectScanErr(t, tx.ScanIndexRev("people", "nope", nil, nil, false), "no such index")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestTxnCorruptDescriptor: a stored descriptor that no longer parses
// surfaces as an error from transactional reads (the desc/table error
// path, distinct from the "no such table" miss).
func TestTxnCorruptDescriptor(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	setRaw(t, e, descKey("people"), []byte("{not json"))

	err := e.ReadTxn(func(tx *Txn) error {
		if _, _, err := tx.Get("people", 1); err == nil {
			t.Fatal("Get over a corrupt descriptor must fail")
		}
		expectScanErr(t, tx.Scan("people"), "invalid character")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestScanAfterClose: on a closed engine, every scan flavor yields the
// begin-scan error instead of panicking, and Begin refuses a snapshot.
func TestScanAfterClose(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	peopleTable(t, e)
	if _, err := e.CreateIndex("people", "by-age", false, "age"); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	expectScanErr(t, e.ScanRange("people", nil, nil), "closed")
	expectScanErr(t, e.ScanRangeRev("people", nil, nil, false), "closed")
	expectScanErr(t, e.ScanIndex("people", "by-age", nil, nil), "closed")
	expectScanErr(t, e.ScanIndexRev("people", "by-age", nil, nil, false), "closed")
	if _, err := e.Begin(false); err == nil {
		t.Fatal("Begin(false) on a closed engine must fail")
	}
}
