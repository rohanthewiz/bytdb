package bytdb

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func peopleTable(t *testing.T, e *Engine) {
	t.Helper()
	_, err := e.CreateTable("people", []Column{
		{Name: "id", Type: TInt}, {Name: "name", Type: TString}, {Name: "age", Type: TInt}, {Name: "email", Type: TString},
	}, "id")
	if err != nil {
		t.Fatal(err)
	}
}

func insertPeople(t *testing.T, e *Engine, rows ...[]any) {
	t.Helper()
	for _, r := range rows {
		if err := e.Insert("people", r...); err != nil {
			t.Fatal(err)
		}
	}
}

func names(t *testing.T, seq func(func(Row, error) bool)) []string {
	t.Helper()
	var out []string
	for row, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, row.Col("name").(string))
	}
	return out
}

func TestDescIndex(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e,
		[]any{1, "grace", 45, "g@x"},
		[]any{2, "ada", 36, "a@x"},
		[]any{3, "edsger", 51, "e@x"},
		[]any{4, "alan", 29, "n@x"},
	)
	if _, err := e.CreateIndexCols("people", "by-age-desc", false,
		[]IndexCol{{Name: "age", Desc: true}}); err != nil {
		t.Fatal(err)
	}
	got := names(t, e.ScanIndex("people", "by-age-desc", nil, nil))
	if want := []string{"edsger", "grace", "ada", "alan"}; !slices.Equal(got, want) {
		t.Fatalf("desc index scan = %v; want %v", got, want)
	}

	// Bounds are semantic values; the engine encodes them in key
	// direction, so from/to follow the index's (descending) order.
	got = names(t, e.ScanIndex("people", "by-age-desc", []any{51}, []any{36}))
	if want := []string{"edsger", "grace"}; !slices.Equal(got, want) {
		t.Fatalf("bounded desc scan = %v; want %v", got, want)
	}

	// Maintenance on insert and delete.
	if err := e.Insert("people", 5, "barbara", 48, "b@x"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Delete("people", 2); err != nil {
		t.Fatal(err)
	}
	got = names(t, e.ScanIndex("people", "by-age-desc", nil, nil))
	if want := []string{"edsger", "barbara", "grace", "alan"}; !slices.Equal(got, want) {
		t.Fatalf("desc index after writes = %v; want %v", got, want)
	}

	// NULLs sort after values in a descending column.
	if err := e.Insert("people", 6, "unknown", nil, "u@x"); err != nil {
		t.Fatal(err)
	}
	got = names(t, e.ScanIndex("people", "by-age-desc", nil, nil))
	if want := []string{"edsger", "barbara", "grace", "alan", "unknown"}; !slices.Equal(got, want) {
		t.Fatalf("desc index with NULL = %v; want %v", got, want)
	}
}

func TestDescIndexUniqueAndMixed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	e := openEngine(t, path)
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e,
		[]any{1, "grace", 45, "g@x"},
		[]any{2, "ada", 36, "a@x"},
	)
	if _, err := e.CreateIndexCols("people", "by-email", true,
		[]IndexCol{{Name: "email", Desc: true}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("people", 3, "dupe", 20, "g@x"); err == nil ||
		!strings.Contains(err.Error(), "unique index violation") {
		t.Fatalf("descending unique index not enforced: %v", err)
	}

	// Mixed directions: name ascending, age descending.
	if _, err := e.CreateIndexCols("people", "name-agedesc", false,
		[]IndexCol{{Name: "name"}, {Name: "age", Desc: true}}); err != nil {
		t.Fatal(err)
	}
	insertPeople(t, e,
		[]any{4, "ada", 50, "a2@x"},
		[]any{5, "ada", 10, "a3@x"},
	)
	var ages []int64
	for row, err := range e.ScanIndex("people", "name-agedesc", []any{"ada"}, []any{"adaz"}) {
		if err != nil {
			t.Fatal(err)
		}
		ages = append(ages, row.Col("age").(int64))
	}
	if want := []int64{50, 36, 10}; !slices.Equal(ages, want) {
		t.Fatalf("mixed-direction scan ages = %v; want %v", ages, want)
	}

	// Reopen: the direction round-trips through the descriptor.
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e2 := openEngine(t, path)
	defer e2.Close()
	got := names(t, e2.ScanIndex("people", "name-agedesc", nil, nil))
	if want := []string{"ada", "ada", "ada", "grace"}; !slices.Equal(got, want) {
		t.Fatalf("after reopen = %v; want %v", got, want)
	}
}

func TestCreateIndexBackfillAndScan(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e,
		[]any{1, "grace", 45, "g@x"},
		[]any{2, "ada", 36, "a@x"},
		[]any{3, "edsger", 51, "e@x"},
		[]any{4, "alan", 29, "n@x"},
	)

	// Index created after data exists must backfill.
	if _, err := e.CreateIndex("people", "by-age", false, "age"); err != nil {
		t.Fatal(err)
	}
	got := names(t, e.ScanIndex("people", "by-age", nil, nil))
	if want := []string{"alan", "ada", "grace", "edsger"}; !slices.Equal(got, want) {
		t.Fatalf("index scan = %v; want %v", got, want)
	}

	// Range bounds on the indexed column: 30 <= age < 50.
	got = names(t, e.ScanIndex("people", "by-age", []any{30}, []any{50}))
	if want := []string{"ada", "grace"}; !slices.Equal(got, want) {
		t.Fatalf("bounded index scan = %v; want %v", got, want)
	}

	// Maintained by later writes: insert and delete both update the index.
	insertPeople(t, e, []any{5, "barbara", 40, "b@x"})
	if _, err := e.Delete("people", 1); err != nil { // grace, 45
		t.Fatal(err)
	}
	got = names(t, e.ScanIndex("people", "by-age", nil, nil))
	if want := []string{"alan", "ada", "barbara", "edsger"}; !slices.Equal(got, want) {
		t.Fatalf("index after writes = %v; want %v", got, want)
	}

	// Unknown index errors rather than yielding nothing.
	for _, err := range e.ScanIndex("people", "nope", nil, nil) {
		if err == nil || !strings.Contains(err.Error(), "no such index") {
			t.Fatalf("unknown index scan error = %v", err)
		}
	}
}

func TestIndexDuplicatesAndTies(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	// Same age: entries must coexist and order by primary key.
	insertPeople(t, e,
		[]any{9, "zoe", 30, "z@x"},
		[]any{4, "amy", 30, "y@x"},
		[]any{7, "bob", 25, "b@x"},
	)
	if _, err := e.CreateIndex("people", "by-age", false, "age"); err != nil {
		t.Fatal(err)
	}
	got := names(t, e.ScanIndex("people", "by-age", nil, nil))
	if want := []string{"bob", "amy", "zoe"}; !slices.Equal(got, want) {
		t.Fatalf("dup-value order = %v; want %v", got, want)
	}
}

func TestUniqueIndex(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "ada@x"})

	if _, err := e.CreateIndex("people", "by-email", true, "email"); err != nil {
		t.Fatal(err)
	}

	// Violation on insert; the row must not be half-inserted.
	err := e.Insert("people", 2, "impostor", 99, "ada@x")
	if err == nil || !strings.Contains(err.Error(), "unique index violation") {
		t.Fatalf("duplicate email insert: %v", err)
	}
	if _, ok, _ := e.Get("people", 2); ok {
		t.Fatal("violating row was inserted")
	}
	if got := names(t, e.ScanIndex("people", "by-email", nil, nil)); !slices.Equal(got, []string{"ada"}) {
		t.Fatalf("index polluted by failed insert: %v", got)
	}

	// A different value is fine; deleting frees the value for reuse.
	insertPeople(t, e, []any{3, "grace", 45, "grace@x"})
	if _, err := e.Delete("people", 1); err != nil {
		t.Fatal(err)
	}
	insertPeople(t, e, []any{4, "ada2", 37, "ada@x"})

	// NULLs never conflict in a unique index.
	insertPeople(t, e, []any{5, "anon1", 20, nil}, []any{6, "anon2", 21, nil})
	got := names(t, e.ScanIndex("people", "by-email", nil, nil))
	if want := []string{"anon1", "anon2", "ada2", "grace"}; !slices.Equal(got, want) {
		t.Fatalf("unique index scan = %v; want %v", got, want)
	}
}

func TestUniqueBackfillViolation(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "a", 1, "same@x"}, []any{2, "b", 2, "same@x"})

	_, err := e.CreateIndex("people", "by-email", true, "email")
	if err == nil || !strings.Contains(err.Error(), "violate unique index") {
		t.Fatalf("backfill over duplicates: %v", err)
	}
	// The failed creation must leave no trace: not in the catalog, and
	// recreatable as non-unique with a clean backfill.
	if e.Table("people").Index("by-email") != nil {
		t.Fatal("failed index left in catalog")
	}
	if _, err := e.CreateIndex("people", "by-email", false, "email"); err != nil {
		t.Fatal(err)
	}
	if got := names(t, e.ScanIndex("people", "by-email", nil, nil)); len(got) != 2 {
		t.Fatalf("recreated index rows = %v", got)
	}
}

func TestIndexPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	e := openEngine(t, path)
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "a@x"})
	if _, err := e.CreateIndex("people", "by-age", false, "age"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateIndex("people", "by-email", true, "email"); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2 := openEngine(t, path)
	defer e2.Close()
	desc := e2.Table("people")
	if desc.Index("by-age") == nil || !desc.Index("by-email").Unique {
		t.Fatalf("index descriptors after reopen: %+v", desc.Indexes)
	}
	// Entries survived and maintenance still works, including
	// uniqueness enforcement.
	insertPeople(t, e2, []any{2, "grace", 45, "g@x"})
	got := names(t, e2.ScanIndex("people", "by-age", nil, nil))
	if want := []string{"ada", "grace"}; !slices.Equal(got, want) {
		t.Fatalf("index after reopen = %v; want %v", got, want)
	}
	if err := e2.Insert("people", 3, "dup", 1, "a@x"); err == nil {
		t.Fatal("unique index not enforced after reopen")
	}
}

func TestDropIndex(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "a@x"})
	if _, err := e.CreateIndex("people", "by-age", false, "age"); err != nil {
		t.Fatal(err)
	}
	if err := e.DropIndex("people", "by-age"); err != nil {
		t.Fatal(err)
	}
	if e.Table("people").Index("by-age") != nil {
		t.Fatal("dropped index still in catalog")
	}
	if err := e.DropIndex("people", "by-age"); err == nil {
		t.Fatal("double drop accepted")
	}
	// Re-creating (which may reuse the index ID) must backfill from a
	// clean key range — stale entries would break the arity checks or
	// resurrect rows.
	insertPeople(t, e, []any{2, "grace", 45, "g@x"})
	if _, err := e.CreateIndex("people", "by-age", false, "age"); err != nil {
		t.Fatal(err)
	}
	got := names(t, e.ScanIndex("people", "by-age", nil, nil))
	if want := []string{"ada", "grace"}; !slices.Equal(got, want) {
		t.Fatalf("recreated index = %v; want %v", got, want)
	}
}

func TestDropTableRemovesIndexSpace(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "a@x"})
	if _, err := e.CreateIndex("people", "by-age", false, "age"); err != nil {
		t.Fatal(err)
	}
	if err := e.DropTable("people"); err != nil {
		t.Fatal(err)
	}
	// Nothing of the table — rows, index entries, or descriptor — may
	// remain in the underlying store; only the table-ID sequence key.
	if n := e.kv.Len(); n != 1 {
		t.Fatalf("kv keys after drop = %d; want only the sequence key", n)
	}
}

func TestCreateIndexValidation(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)

	for _, c := range []struct {
		table, name string
		cols        []string
		descr       string
	}{
		{"ghosts", "i", []string{"age"}, "unknown table"},
		{"people", "", []string{"age"}, "empty name"},
		{"people", "i", nil, "no columns"},
		{"people", "i", []string{"nope"}, "unknown column"},
		{"people", "i", []string{"age", "age"}, "duplicate column"},
	} {
		if _, err := e.CreateIndex(c.table, c.name, false, c.cols...); err == nil {
			t.Fatalf("CreateIndex accepted %s", c.descr)
		}
	}
	if _, err := e.CreateIndex("people", "by-age", false, "age"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateIndex("people", "by-age", false, "name"); err == nil {
		t.Fatal("duplicate index name accepted")
	}
}

// TestConcurrentInsertsDuringCreateIndex races writes against index
// creation: every row must land in the index exactly once, whether it
// was caught by the backfill (serialized before) or written by an
// insert that resolved the descriptor after the index committed.
func TestConcurrentInsertsDuringCreateIndex(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	for i := range 20 {
		insertPeople(t, e, []any{i, fmt.Sprintf("u%03d", i), i, nil})
	}

	done := make(chan error, 1)
	go func() {
		for i := 20; i < 60; i++ {
			if err := e.Insert("people", i, fmt.Sprintf("u%03d", i), i, nil); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	if _, err := e.CreateIndex("people", "by-age", false, "age"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	got := names(t, e.ScanIndex("people", "by-age", nil, nil))
	if len(got) != 60 || !slices.IsSorted(got) {
		t.Fatalf("index has %d entries (sorted=%v); want all 60 rows exactly once",
			len(got), slices.IsSorted(got))
	}
}

func TestCompositeIndex(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e,
		[]any{1, "ada", 36, "x"},
		[]any{2, "bob", 36, "a"},
		[]any{3, "cat", 29, "z"},
	)
	if _, err := e.CreateIndex("people", "age-email", false, "age", "email"); err != nil {
		t.Fatal(err)
	}
	got := names(t, e.ScanIndex("people", "age-email", nil, nil))
	if want := []string{"cat", "bob", "ada"}; !slices.Equal(got, want) {
		t.Fatalf("composite index order = %v; want %v", got, want)
	}
	// Partial-prefix bound: age 36 only.
	got = names(t, e.ScanIndex("people", "age-email", []any{36}, []any{37}))
	if want := []string{"bob", "ada"}; !slices.Equal(got, want) {
		t.Fatalf("partial bound = %v; want %v", got, want)
	}
}
