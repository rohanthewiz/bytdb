package bytdb

import (
	"path/filepath"
	"slices"
	"testing"
)

// TestScanRangeRev walks the primary index backward, with and without
// bounds, mirroring ScanRange's [from, to) region in descending order.
func TestScanRangeRev(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e,
		[]any{1, "grace", 45, "g@x"},
		[]any{2, "ada", 36, "a@x"},
		[]any{3, "edsger", 51, "e@x"},
		[]any{4, "alan", 29, "n@x"},
	)

	got := names(t, e.ScanRangeRev("people", nil, nil))
	if want := []string{"alan", "edsger", "ada", "grace"}; !slices.Equal(got, want) {
		t.Fatalf("full reverse scan = %v; want %v", got, want)
	}

	// Same [from, to) region as ScanRange, just descending.
	got = names(t, e.ScanRangeRev("people", []any{2}, []any{4}))
	if want := []string{"edsger", "ada"}; !slices.Equal(got, want) {
		t.Fatalf("bounded reverse scan = %v; want %v", got, want)
	}

	// A reverse scan is the forward scan read back to front.
	fwd := names(t, e.ScanRange("people", nil, nil))
	slices.Reverse(fwd)
	if rev := names(t, e.ScanRangeRev("people", nil, nil)); !slices.Equal(fwd, rev) {
		t.Fatalf("reverse != forward reversed: %v vs %v", rev, fwd)
	}
}

// TestScanIndexRev walks a secondary index backward.
func TestScanIndexRev(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e,
		[]any{1, "grace", 45, "g@x"},
		[]any{2, "ada", 36, "a@x"},
		[]any{3, "edsger", 51, "e@x"},
		[]any{4, "alan", 29, "n@x"},
	)
	if _, err := e.CreateIndex("people", "by-age", false, "age"); err != nil {
		t.Fatal(err)
	}

	// Ascending index read backward = descending age.
	got := names(t, e.ScanIndexRev("people", "by-age", nil, nil))
	if want := []string{"edsger", "grace", "ada", "alan"}; !slices.Equal(got, want) {
		t.Fatalf("reverse index scan = %v; want %v", got, want)
	}

	// Reverse of a DESC index yields ascending age.
	if _, err := e.CreateIndexCols("people", "by-age-desc", false,
		[]IndexCol{{Name: "age", Desc: true}}); err != nil {
		t.Fatal(err)
	}
	got = names(t, e.ScanIndexRev("people", "by-age-desc", nil, nil))
	if want := []string{"alan", "ada", "grace", "edsger"}; !slices.Equal(got, want) {
		t.Fatalf("reverse desc-index scan = %v; want %v", got, want)
	}

	fwd := names(t, e.ScanIndex("people", "by-age", nil, nil))
	slices.Reverse(fwd)
	if rev := names(t, e.ScanIndexRev("people", "by-age", nil, nil)); !slices.Equal(fwd, rev) {
		t.Fatalf("reverse index != forward reversed: %v vs %v", rev, fwd)
	}
}

// TestScanRevInTxn confirms the transaction view exposes the same
// reverse walks, including its own uncommitted writes.
func TestScanRevInTxn(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e,
		[]any{1, "grace", 45, "g@x"},
		[]any{3, "edsger", 51, "e@x"},
	)
	if err := e.WriteTxn(func(tx *Txn) error {
		if err := tx.Insert("people", 2, "ada", 36, "a@x"); err != nil {
			return err
		}
		got := names(t, tx.ScanRangeRev("people", nil, nil))
		if want := []string{"edsger", "ada", "grace"}; !slices.Equal(got, want) {
			t.Fatalf("txn reverse scan = %v; want %v", got, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
