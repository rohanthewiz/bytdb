package bytdb

// scan_bounds_test.go: the encoding edges of the one-shot read paths —
// malformed scan bounds on every scan flavor, scans of unknown tables,
// primary-key validation on Get, Row.Col misses, and the full matrix
// of coerce's accepted widths and rejected mismatches.

import (
	"path/filepath"
	"testing"
)

// TestScanUnknownTable: engine-level scans of an absent table (and a
// reverse scan of an absent index) yield the error rather than an
// empty sequence.
func TestScanUnknownTable(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)

	expectScanErr(t, e.ScanRange("ghosts", nil, nil), "no such table")
	expectScanErr(t, e.ScanRangeRev("ghosts", nil, nil, false), "no such table")
	expectScanErr(t, e.ScanIndex("ghosts", "i", nil, nil), "no such table")
	expectScanErr(t, e.ScanIndexRev("ghosts", "i", nil, nil, false), "no such table")
	expectScanErr(t, e.ScanIndexRev("people", "nope", nil, nil, false), "no such index")
}

// TestScanBoundErrors: bad bounds — too many values, a mistyped value,
// or NULL in a pk bound — are yielded as errors from both ends of all
// four scan flavors.
func TestScanBoundErrors(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "a@x"})
	if _, err := e.CreateIndex("people", "by-age", false, "age"); err != nil {
		t.Fatal(err)
	}

	// Primary-key bounds, forward and reverse, from- and to-side.
	expectScanErr(t, e.ScanRange("people", []any{1, 2}, nil), "too many primary key values")
	expectScanErr(t, e.ScanRange("people", nil, []any{"x"}), "does not fit column type")
	expectScanErr(t, e.ScanRangeRev("people", []any{1, 2}, nil, false), "too many primary key values")
	expectScanErr(t, e.ScanRangeRev("people", nil, []any{"x"}, false), "does not fit column type")
	expectScanErr(t, e.ScanRange("people", []any{nil}, nil), "may not be NULL")

	// Index bounds likewise.
	expectScanErr(t, e.ScanIndex("people", "by-age", []any{1, 2}, nil), "too many values in index scan bound")
	expectScanErr(t, e.ScanIndex("people", "by-age", nil, []any{"x"}), "does not fit column type")
	expectScanErr(t, e.ScanIndexRev("people", "by-age", []any{1, 2}, nil, false), "too many values in index scan bound")
	expectScanErr(t, e.ScanIndexRev("people", "by-age", nil, []any{"x"}, false), "does not fit column type")
}

func TestGetPKValidation(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	usersTable(t, e)

	if _, _, err := e.Get("users", 1, 2); err == nil {
		t.Fatal("Get with too many pk values accepted")
	}
	if _, _, err := e.Get("users", "not-an-int"); err == nil {
		t.Fatal("Get with a mistyped pk value accepted")
	}
	if _, _, err := e.Get("users", nil); err == nil {
		t.Fatal("Get with a NULL pk value accepted")
	}
}

func TestRowColUnknownColumn(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	usersTable(t, e)
	if err := e.Insert("users", 1, "ada", 1.0, true, nil); err != nil {
		t.Fatal(err)
	}
	row, ok, err := e.Get("users", 1)
	if err != nil || !ok {
		t.Fatalf("Get: %v, %v", ok, err)
	}
	if v := row.Col("nope"); v != nil {
		t.Fatalf("Col(unknown) = %v; want nil", v)
	}
}

// TestCoerceWidthsAndMismatches: every accepted Go width lands in the
// column's canonical type, and each column type rejects a foreign value.
func TestCoerceWidthsAndMismatches(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	usersTable(t, e) // id int, name string, score float, active bool, blob bytes

	// All int widths coerce into a TInt key column.
	intWidths := []any{int8(1), int16(2), int32(3), int64(4), uint8(5), uint16(6), uint32(7)}
	for _, id := range intWidths {
		if err := e.Insert("users", id, "w", 0.0, true, nil); err != nil {
			t.Fatalf("Insert(id=%T): %v", id, err)
		}
	}
	for want := int64(1); want <= 7; want++ {
		if _, ok, err := e.Get("users", want); err != nil || !ok {
			t.Fatalf("Get(%d) after width coercion = %v, %v", want, ok, err)
		}
	}

	// TFloat accepts float32, int, and int64, all read back as float64.
	floatIns := []struct {
		id    int
		score any
		want  float64
	}{
		{10, float32(1.5), 1.5},
		{11, 2, 2.0},
		{12, int64(3), 3.0},
	}
	for _, c := range floatIns {
		if err := e.Insert("users", c.id, "f", c.score, true, nil); err != nil {
			t.Fatalf("Insert(score=%T): %v", c.score, err)
		}
		row, ok, err := e.Get("users", c.id)
		if err != nil || !ok || row.Col("score") != c.want {
			t.Fatalf("Get(%d).score = %v (%v, %v); want %v", c.id, row.Col("score"), ok, err, c.want)
		}
	}

	// Each column type rejects a value of the wrong Go type.
	mismatches := []struct {
		row   []any
		descr string
	}{
		{[]any{20, "a", 1.0, "yes", nil}, "string into bool"},
		{[]any{21, 5, 1.0, true, nil}, "int into string"},
		{[]any{22, "a", "high", true, nil}, "string into float"},
		{[]any{23, "a", 1.0, true, "blob"}, "string into bytes"},
		{[]any{uint64(24), "a", 1.0, true, nil}, "uint64 into int"},
	}
	for _, c := range mismatches {
		if err := e.Insert("users", c.row...); err == nil {
			t.Fatalf("Insert accepted %s", c.descr)
		}
	}
}
