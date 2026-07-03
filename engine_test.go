package bytdb

import (
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func openEngine(t *testing.T, path string) *Engine {
	t.Helper()
	e, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func usersTable(t *testing.T, e *Engine) *TableDesc {
	t.Helper()
	desc, err := e.CreateTable("users", []Column{
		{Name: "id", Type: TInt}, {Name: "name", Type: TString}, {Name: "score", Type: TFloat}, {Name: "active", Type: TBool}, {Name: "blob", Type: TBytes},
	}, "id")
	if err != nil {
		t.Fatal(err)
	}
	return desc
}

func collect(t *testing.T, seq func(func(Row, error) bool)) []Row {
	t.Helper()
	var rows []Row
	for row, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestCreateInsertScan(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	usersTable(t, e)

	// Insert out of PK order, with negative keys and a NULL non-key column.
	ins := [][]any{
		{7, "grace", 9.5, true, []byte{1, 2}},
		{-3, "ada", 8.25, false, []byte{}},
		{0, "alan", nil, true, nil},
	}
	for _, row := range ins {
		if err := e.Insert("users", row...); err != nil {
			t.Fatal(err)
		}
	}

	rows := collect(t, e.Scan("users"))
	if len(rows) != 3 {
		t.Fatalf("scanned %d rows; want 3", len(rows))
	}
	// PK order: -3, 0, 7 — and full round trip of every column.
	wantIDs := []int64{-3, 0, 7}
	for i, r := range rows {
		if r.Col("id") != wantIDs[i] {
			t.Fatalf("row %d id = %v; want %d", i, r.Col("id"), wantIDs[i])
		}
	}
	if !reflect.DeepEqual(rows[0].Vals, []any{int64(-3), "ada", 8.25, false, []byte{}}) {
		t.Fatalf("row values = %#v", rows[0].Vals)
	}
	if rows[1].Col("score") != nil || rows[1].Col("blob") != nil {
		t.Fatalf("NULL columns did not round trip: %#v", rows[1].Vals)
	}

	// Point lookup, with int width coercion on the way in.
	row, ok, err := e.Get("users", int32(7))
	if err != nil || !ok {
		t.Fatalf("Get: %v, %v", ok, err)
	}
	if row.Col("name") != "grace" {
		t.Fatalf("Get(7).name = %v", row.Col("name"))
	}
	if _, ok, _ := e.Get("users", 99); ok {
		t.Fatal("Get(99) found a row")
	}
}

func TestInsertValidation(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	usersTable(t, e)

	if err := e.Insert("users", 1, "a", 1.0, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("users", 1, "dup", 2.0, false, nil); err == nil ||
		!strings.Contains(err.Error(), "duplicate primary key") {
		t.Fatalf("duplicate insert: %v", err)
	}
	if err := e.Insert("users", 2, "short"); err == nil {
		t.Fatal("arity mismatch accepted")
	}
	if err := e.Insert("users", "not-an-int", "a", 1.0, true, nil); err == nil {
		t.Fatal("type mismatch accepted")
	}
	if err := e.Insert("users", nil, "a", 1.0, true, nil); err == nil {
		t.Fatal("NULL primary key accepted")
	}
	if err := e.Insert("ghosts", 1); err == nil {
		t.Fatal("insert into unknown table accepted")
	}
}

func TestCompositePKAndRange(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	_, err := e.CreateTable("events", []Column{
		{Name: "org", Type: TString}, {Name: "seq", Type: TInt}, {Name: "note", Type: TString},
	}, "org", "seq")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range [][]any{
		{"beta", 1, "b1"}, {"acme", 2, "a2"}, {"acme", 10, "a10"},
		{"acme", 1, "a1"}, {"citro", 1, "c1"},
	} {
		if err := e.Insert("events", r...); err != nil {
			t.Fatal(err)
		}
	}

	notes := func(rows []Row) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.Col("note").(string))
		}
		return out
	}

	// Full scan: org asc, then seq asc numerically (10 after 2).
	got := notes(collect(t, e.Scan("events")))
	want := []string{"a1", "a2", "a10", "b1", "c1"}
	if !slices.Equal(got, want) {
		t.Fatalf("scan order = %v; want %v", got, want)
	}

	// Partial-prefix bounds: everything for one org.
	got = notes(collect(t, e.ScanRange("events", []any{"acme"}, []any{"beta"})))
	if !slices.Equal(got, []string{"a1", "a2", "a10"}) {
		t.Fatalf("org range = %v", got)
	}

	// Full-tuple bounds, half-open: [("acme",2), ("citro",1)).
	got = notes(collect(t, e.ScanRange("events", []any{"acme", 2}, []any{"citro", 1})))
	if !slices.Equal(got, []string{"a2", "a10", "b1"}) {
		t.Fatalf("tuple range = %v", got)
	}

	// Early break must not wedge the engine (lock release).
	for range e.Scan("events") {
		break
	}
	if err := e.Insert("events", "dora", 1, "d1"); err != nil {
		t.Fatal(err)
	}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	e := openEngine(t, path)
	usersTable(t, e)
	if err := e.Insert("users", 5, "eve", 1.25, true, []byte{9}); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2 := openEngine(t, path)
	defer e2.Close()
	if !slices.Equal(e2.Tables(), []string{"users"}) {
		t.Fatalf("catalog after reopen: %v", e2.Tables())
	}
	desc := e2.Table("users")
	if desc == nil || len(desc.Columns) != 5 || desc.ID < firstUserTableID {
		t.Fatalf("descriptor after reopen: %+v", desc)
	}
	row, ok, err := e2.Get("users", 5)
	if err != nil || !ok || row.Col("name") != "eve" {
		t.Fatalf("row after reopen: %v %v %v", row, ok, err)
	}

	// New tables after reopen must not reuse IDs.
	d2, err := e2.CreateTable("orgs", []Column{{Name: "id", Type: TInt}}, "id")
	if err != nil {
		t.Fatal(err)
	}
	if d2.ID <= desc.ID {
		t.Fatalf("table ID reused: %d after %d", d2.ID, desc.ID)
	}
}

func TestDeleteAndDrop(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	usersTable(t, e)
	for i := range 5 {
		if err := e.Insert("users", i, "u", 0.0, false, nil); err != nil {
			t.Fatal(err)
		}
	}

	existed, err := e.Delete("users", 3)
	if err != nil || !existed {
		t.Fatalf("Delete(3): %v, %v", existed, err)
	}
	if existed, _ = e.Delete("users", 3); existed {
		t.Fatal("Delete(3) twice reported existed")
	}
	if rows := collect(t, e.Scan("users")); len(rows) != 4 {
		t.Fatalf("%d rows after delete; want 4", len(rows))
	}

	// A second table must be untouched by dropping the first.
	if _, err := e.CreateTable("keep", []Column{{Name: "k", Type: TString}}, "k"); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("keep", "x"); err != nil {
		t.Fatal(err)
	}
	if err := e.DropTable("users"); err != nil {
		t.Fatal(err)
	}
	if e.Table("users") != nil {
		t.Fatal("descriptor survived drop")
	}
	if err := e.Insert("users", 1, "a", 0.0, true, nil); err == nil {
		t.Fatal("insert into dropped table accepted")
	}
	if rows := collect(t, e.Scan("keep")); len(rows) != 1 || rows[0].Col("k") != "x" {
		t.Fatalf("neighbor table damaged by drop: %v", rows)
	}
	if err := e.DropTable("users"); err == nil {
		t.Fatal("double drop accepted")
	}
}

func TestCreateTableValidation(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	cols := []Column{{Name: "id", Type: TInt}}

	for _, c := range []struct {
		name  string
		cols  []Column
		pk    []string
		descr string
	}{
		{"", cols, []string{"id"}, "empty name"},
		{"t", nil, []string{"id"}, "no columns"},
		{"t", cols, nil, "no pk"},
		{"t", cols, []string{"nope"}, "unknown pk column"},
		{"t", cols, []string{"id", "id"}, "duplicate pk column"},
		{"t", []Column{{Name: "a", Type: TInt}, {Name: "a", Type: TInt}}, []string{"a"}, "duplicate column"},
		{"t", []Column{{Name: "a", Type: "jsonb"}}, []string{"a"}, "unknown type"},
	} {
		if _, err := e.CreateTable(c.name, c.cols, c.pk...); err == nil {
			t.Fatalf("CreateTable accepted %s", c.descr)
		}
	}

	if _, err := e.CreateTable("t", cols, "id"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTable("t", cols, "id"); err == nil {
		t.Fatal("duplicate table accepted")
	}
}
