package bytdb

import (
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestAddColumn(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "a@x"})

	if err := e.AddColumn("people", Column{Name: "city", Type: TString}); err != nil {
		t.Fatal(err)
	}

	// Existing rows read the new column as NULL — no rewrite happened.
	row, ok, err := e.Get("people", 1)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if row.Col("city") != nil {
		t.Fatalf("old row's new column = %v; want NULL", row.Col("city"))
	}

	// New inserts must supply the new arity; the column round-trips.
	if err := e.Insert("people", 1, "x", 1, "y"); err == nil {
		t.Fatal("old arity accepted after AddColumn")
	}
	if err := e.Insert("people", 2, "grace", 45, "g@x", "nyc"); err != nil {
		t.Fatal(err)
	}
	row, _, _ = e.Get("people", 2)
	if row.Col("city") != "nyc" {
		t.Fatalf("new column = %v; want nyc", row.Col("city"))
	}

	// Update can set it on an old row.
	if _, err := e.Update("people", []any{1}, map[string]any{"city": "london"}); err != nil {
		t.Fatal(err)
	}
	row, _, _ = e.Get("people", 1)
	if row.Col("city") != "london" {
		t.Fatalf("updated new column = %v", row.Col("city"))
	}

	// An index on the added column works, old NULL rows included.
	if _, err := e.CreateIndex("people", "by-city", false, "city"); err != nil {
		t.Fatal(err)
	}
	got := names(t, e.ScanIndex("people", "by-city", nil, nil))
	if want := []string{"ada", "grace"}; !slices.Equal(got, want) { // london < nyc
		t.Fatalf("index on added column = %v; want %v", got, want)
	}
}

func TestAddColumnValidation(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)

	if err := e.AddColumn("ghosts", Column{Name: "c", Type: TInt}); err == nil {
		t.Fatal("unknown table accepted")
	}
	if err := e.AddColumn("people", Column{Name: "", Type: TInt}); err == nil {
		t.Fatal("empty column name accepted")
	}
	if err := e.AddColumn("people", Column{Name: "age", Type: TInt}); err == nil {
		t.Fatal("duplicate column accepted")
	}
	if err := e.AddColumn("people", Column{Name: "c", Type: "hstore"}); err == nil {
		t.Fatal("unknown type accepted")
	}
}

func TestDropColumnAndIDNeverReused(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "a@x"})

	if err := e.DropColumn("people", "email"); err != nil {
		t.Fatal(err)
	}
	if e.Table("people").ColIndex("email") >= 0 {
		t.Fatal("dropped column still declared")
	}
	// Inserts use the new arity; the old row still decodes.
	insertPeople(t, e, []any{2, "grace", 45})
	row, _, _ := e.Get("people", 1)
	if row.Col("name") != "ada" || row.Col("age") != int64(36) {
		t.Fatalf("old row after drop = %v", row.Vals)
	}

	// The critical property: re-adding the same name gets a fresh ID,
	// so ada's old email data must NOT resurface.
	if err := e.AddColumn("people", Column{Name: "email", Type: TString}); err != nil {
		t.Fatal(err)
	}
	row, _, _ = e.Get("people", 1)
	if row.Col("email") != nil {
		t.Fatalf("dropped column's data resurrected: %v", row.Col("email"))
	}
}

func TestDropColumnRestrictions(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	if _, err := e.CreateIndex("people", "by-age", false, "age"); err != nil {
		t.Fatal(err)
	}

	if err := e.DropColumn("people", "id"); err == nil ||
		!strings.Contains(err.Error(), "primary key") {
		t.Fatalf("pk column drop: %v", err)
	}
	if err := e.DropColumn("people", "age"); err == nil ||
		!strings.Contains(err.Error(), "drop the index first") {
		t.Fatalf("indexed column drop: %v", err)
	}
	if err := e.DropColumn("people", "nope"); err == nil {
		t.Fatal("unknown column drop accepted")
	}
	// After dropping the index, the column may go.
	if err := e.DropIndex("people", "by-age"); err != nil {
		t.Fatal(err)
	}
	if err := e.DropColumn("people", "age"); err != nil {
		t.Fatal(err)
	}
}

// TestDropColumnOrdinalShift drops a column that sits before the
// primary-key and indexed columns, which renumbers every ordinal
// reference in the descriptor. Everything must keep working — across
// a reopen, which reloads the renumbered descriptor from disk.
func TestDropColumnOrdinalShift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	e := openEngine(t, path)
	_, err := e.CreateTable("wide", []Column{
		{Name: "junk", Type: TString}, // will be dropped: every later ordinal shifts
		{Name: "key", Type: TInt},
		{Name: "val", Type: TString},
		{Name: "rank", Type: TInt},
	}, "key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateIndex("wide", "by-rank", false, "rank"); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("wide", "x", 1, "one", 30); err != nil {
		t.Fatal(err)
	}
	if err := e.DropColumn("wide", "junk"); err != nil {
		t.Fatal(err)
	}

	// New-arity insert, pk lookup, and index scan all use shifted ordinals.
	if err := e.Insert("wide", 2, "two", 10); err != nil {
		t.Fatal(err)
	}
	row, ok, err := e.Get("wide", 2)
	if err != nil || !ok || row.Col("val") != "two" {
		t.Fatalf("Get after shift: %v %v %v", row.Vals, ok, err)
	}
	var vals []string
	for row, err := range e.ScanIndex("wide", "by-rank", nil, nil) {
		if err != nil {
			t.Fatal(err)
		}
		vals = append(vals, row.Col("val").(string))
	}
	if want := []string{"two", "one"}; !slices.Equal(vals, want) { // rank 10, 30
		t.Fatalf("index after shift = %v; want %v", vals, want)
	}

	// The renumbered descriptor persists.
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e2 := openEngine(t, path)
	defer e2.Close()
	row, ok, err = e2.Get("wide", 1)
	if err != nil || !ok || row.Col("val") != "one" || row.Col("rank") != int64(30) {
		t.Fatalf("after reopen: %v %v %v", row.Vals, ok, err)
	}
	if err := e2.Insert("wide", 3, "three", 20); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.Update("wide", []any{3}, map[string]any{"rank": 40}); err != nil {
		t.Fatal(err)
	}
}

func TestNotNullColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	e := openEngine(t, path)
	defer e.Close()
	if _, err := e.CreateTable("t", []Column{
		{Name: "id", Type: TInt}, {Name: "name", Type: TString, NotNull: true},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("t", 1, nil); err == nil ||
		!strings.Contains(err.Error(), `null value in column "name" of relation "t" violates not-null constraint`) {
		t.Fatalf("NULL insert into NOT NULL column: %v", err)
	}
	if err := e.Insert("t", 1, "ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Update("t", []any{1}, map[string]any{"name": nil}); err == nil ||
		!strings.Contains(err.Error(), "violates not-null constraint") {
		t.Fatalf("NULL update of NOT NULL column: %v", err)
	}

	// Adding a NOT NULL column: only while the table is empty.
	if err := e.AddColumn("t", Column{Name: "email", Type: TString, NotNull: true}); err == nil ||
		!strings.Contains(err.Error(), `column "email" of relation "t" contains null values`) {
		t.Fatalf("NOT NULL add on non-empty table: %v", err)
	}
	if _, err := e.Delete("t", 1); err != nil {
		t.Fatal(err)
	}
	if err := e.AddColumn("t", Column{Name: "email", Type: TString, NotNull: true}); err != nil {
		t.Fatalf("NOT NULL add on empty table: %v", err)
	}
	if err := e.Insert("t", 2, "grace", nil); err == nil ||
		!strings.Contains(err.Error(), "violates not-null constraint") {
		t.Fatalf("added NOT NULL column not enforced: %v", err)
	}

	// The flag persists.
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e2 := openEngine(t, path)
	defer e2.Close()
	if d := e2.Table("t"); !d.Columns[1].NotNull || !d.Columns[2].NotNull {
		t.Fatalf("NotNull not persisted: %+v", d.Columns)
	}
}

func TestCheckDescStorage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	e := openEngine(t, path)
	defer e.Close()
	checks := []CheckDesc{{Name: "t_a_check", Expr: "a > 0"}, {Name: "t_check", Expr: "a < b"}}
	if _, err := e.CreateTableWithChecks("t", []Column{
		{Name: "a", Type: TInt}, {Name: "b", Type: TInt},
	}, checks, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTableWithChecks("u", []Column{{Name: "a", Type: TInt}},
		[]CheckDesc{{Name: "x", Expr: "a > 0"}, {Name: "x", Expr: "a < 9"}}, "a"); err == nil {
		t.Fatal("duplicate check names accepted")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e2 := openEngine(t, path)
	defer e2.Close()
	if got := e2.Table("t").Checks; !reflect.DeepEqual(got, checks) {
		t.Fatalf("checks not persisted: %+v", got)
	}
}

func TestAddDropCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	e := openEngine(t, path)
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "a@x"}, []any{2, "grace", 45, "g@x"})

	// validate sees every existing row; its error aborts the add.
	var seen []any
	err := e.AddCheck("people", CheckDesc{Name: "adults", Expr: "age >= 18"}, func(r Row) error {
		seen = append(seen, r.Col("id"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seen, []any{int64(1), int64(2)}) {
		t.Fatalf("validate saw %v", seen)
	}
	if err := e.AddCheck("people", CheckDesc{Name: "x", Expr: "e"}, func(Row) error {
		return errors.New("row rejected")
	}); err == nil || !strings.Contains(err.Error(), "row rejected") {
		t.Fatalf("validate error not surfaced: %v", err)
	}
	if e.Table("people").Checks[0].Name != "adults" || len(e.Table("people").Checks) != 1 {
		t.Fatalf("checks after failed add: %+v", e.Table("people").Checks)
	}

	// Names must be present and unused.
	if err := e.AddCheck("people", CheckDesc{Name: "adults", Expr: "age >= 21"}, nil); err == nil ||
		!strings.Contains(err.Error(), `constraint "adults" for relation "people" already exists`) {
		t.Fatalf("duplicate name: %v", err)
	}
	if err := e.AddCheck("people", CheckDesc{Expr: "age >= 21"}, nil); err == nil {
		t.Fatal("empty name accepted")
	}
	if err := e.AddCheck("nope", CheckDesc{Name: "c", Expr: "1"}, nil); err == nil {
		t.Fatal("missing table accepted")
	}

	if existed, err := e.DropCheck("people", "nope"); err != nil || existed {
		t.Fatalf("DropCheck missing = %v, %v", existed, err)
	}
	if existed, err := e.DropCheck("people", "adults"); err != nil || !existed {
		t.Fatalf("DropCheck = %v, %v", existed, err)
	}
	if len(e.Table("people").Checks) != 0 {
		t.Fatalf("checks after drop: %+v", e.Table("people").Checks)
	}
}

func TestAlterPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	e := openEngine(t, path)
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "a@x"})
	if err := e.AddColumn("people", Column{Name: "city", Type: TString}); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2 := openEngine(t, path)
	defer e2.Close()
	desc := e2.Table("people")
	if desc.ColIndex("city") != 4 || desc.NextColID != 6 {
		t.Fatalf("descriptor after reopen: %+v", desc)
	}
	row, ok, _ := e2.Get("people", 1)
	if !ok || row.Col("city") != nil || row.Col("email") != "a@x" {
		t.Fatalf("row after reopen: %v", row.Vals)
	}
	if err := e2.Insert("people", 2, "grace", 45, "g@x", "nyc"); err != nil {
		t.Fatal(err)
	}
}
