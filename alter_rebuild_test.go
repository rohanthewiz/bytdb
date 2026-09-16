package bytdb

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// alter_rebuild_test.go: ADD COLUMN of an identity column, ALTER COLUMN
// TYPE, and SetPrimaryKey — the DDL that numbers or re-derives existing
// rows. Each test pins the table's state after a FAILED change too,
// since atomicity is the property the rebuild design exists for.

// scanCol collects one column across a full table scan, in scan order.
func scanCol(t *testing.T, e *Engine, table, col string) []any {
	t.Helper()
	var out []any
	for row, err := range e.Scan(table) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, row.Col(col))
	}
	return out
}

func wantErr(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Fatalf("err = %v; want containing %q", err, substr)
	}
}

func itemsTable(t *testing.T, e *Engine) {
	t.Helper()
	if _, err := e.CreateTable("items", []Column{
		{Name: "id", Type: TInt}, {Name: "code", Type: TString}, {Name: "qty", Type: TFloat},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	for _, r := range [][]any{{30, "c", 1.5}, {10, "a", 2.5}, {20, "b", 3.4}} {
		if err := e.Insert("items", r...); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAddIdentityColumnNumbersRows(t *testing.T) {
	for _, mode := range []string{"serialized", "occ"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "t.db")
			e := openEngine(t, path)
			if mode == "occ" {
				e.Close()
				e = openOCCEngine(t, path)
			}
			defer e.Close()
			itemsTable(t, e)

			if err := e.AddColumn("items", Column{Name: "seq", Type: TInt, Identity: true}); err != nil {
				t.Fatal(err)
			}
			// Numbered in primary-key order: ids 10, 20, 30.
			if got := scanCol(t, e, "items", "seq"); !slices.Equal(got, []any{int64(1), int64(2), int64(3)}) {
				t.Fatalf("seq = %v", got)
			}
			// The counter continues past the backfill.
			row, err := e.InsertReturning("items", 40, "d", 1.0, nil)
			if err != nil {
				t.Fatal(err)
			}
			if row.Col("seq") != int64(4) {
				t.Fatalf("next draw = %v; want 4", row.Col("seq"))
			}
		})
	}
}

func TestAddIdentityColumnEmptyAndRefusals(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "t.db"))
	defer e.Close()
	if _, err := e.CreateTable("empty", []Column{{Name: "id", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.AddColumn("empty", Column{Name: "n", Type: TInt, Identity: true}); err != nil {
		t.Fatal(err)
	}
	row, err := e.InsertReturning("empty", 1, nil)
	if err != nil || row.Col("n") != int64(1) {
		t.Fatalf("first draw on empty table = %v, %v; want 1", row.Col("n"), err)
	}

	wantErr(t, e.AddColumn("empty", Column{Name: "s", Type: TString, Identity: true}), "must be an int column")
	wantErr(t, e.AddColumn("empty", Column{Name: "d", Type: TInt, Identity: true, Default: "5"}), "conflicting DEFAULT")

	itemsTable(t, e)
	e.SetBackfillLimit(2)
	wantErr(t, e.AddColumn("items", Column{Name: "seq", Type: TInt, Identity: true}), "backfill limit")
	if e.Table("items").ColIndex("seq") >= 0 {
		t.Fatal("refused identity column was published")
	}
}

func TestAlterColumnTypeAutomaticCasts(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "t.db"))
	defer e.Close()
	itemsTable(t, e)
	if _, err := e.CreateIndex("items", "items_qty", false, "qty"); err != nil {
		t.Fatal(err)
	}

	// float -> int rounds half to even: 1.5 -> 2, 2.5 -> 2, 3.4 -> 3.
	if err := e.AlterColumnType("items", "qty", ColumnTypeChange{Type: TInt}); err != nil {
		t.Fatal(err)
	}
	if got := scanCol(t, e, "items", "qty"); !slices.Equal(got, []any{int64(2), int64(3), int64(2)}) {
		t.Fatalf("qty = %v", got)
	}
	// The index was rebuilt under the new type: an int bound finds rows.
	var ids []any
	for row, err := range e.ScanIndex("items", "items_qty", []any{2}, []any{3}) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, row.Col("id"))
	}
	if !slices.Equal(ids, []any{int64(10), int64(30)}) {
		t.Fatalf("index scan qty in [2,3) = %v; want [10 30]", ids)
	}

	// int -> text on the KEY column re-keys the rows: text orders "10" < "20" < "30"
	// as before, but a new id "9" now sorts last.
	if err := e.AlterColumnType("items", "id", ColumnTypeChange{Type: TString}); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("items", "9", "z", 1); err != nil {
		t.Fatal(err)
	}
	if got := scanCol(t, e, "items", "id"); !slices.Equal(got, []any{"10", "20", "30", "9"}) {
		t.Fatalf("ids after int->text = %v", got)
	}
	if _, ok, _ := e.Get("items", "20"); !ok {
		t.Fatal("Get by the text key missed")
	}
}

func TestAlterColumnTypeAtomicFailures(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "t.db"))
	defer e.Close()
	itemsTable(t, e)
	if _, err := e.CreateIndex("items", "items_qty_u", true, "qty"); err != nil {
		t.Fatal(err)
	}
	before := scanCol(t, e, "items", "qty")

	// 1.5 and 2.5 both round to 2 — the unique index collides.
	wantErr(t, e.AlterColumnType("items", "qty", ColumnTypeChange{Type: TInt}), `could not create unique index "items_qty_u"`)
	if got := scanCol(t, e, "items", "qty"); !slices.Equal(got, before) {
		t.Fatalf("failed change modified rows: %v", got)
	}
	if e.Table("items").Columns[2].Type != TFloat {
		t.Fatal("failed change published the new type")
	}

	// No automatic cast from text to int; USING is the way out.
	wantErr(t, e.AlterColumnType("items", "code", ColumnTypeChange{Type: TInt}), "cannot be cast automatically")

	// A Validate error aborts the whole change.
	err := e.AlterColumnType("items", "code", ColumnTypeChange{Type: TString, MaxLen: 5,
		Validate: func(r Row) error {
			if r.Col("code") == "b" {
				return errString("rejected b")
			}
			return nil
		}})
	wantErr(t, err, "rejected b")
	if e.Table("items").Columns[1].MaxLen != 0 {
		t.Fatal("aborted change published MaxLen")
	}

	// Shrinking a varchar below a stored value fails like an insert would.
	if err := e.Insert("items", 40, "toolong", 9.0); err != nil {
		t.Fatal(err)
	}
	wantErr(t, e.AlterColumnType("items", "code", ColumnTypeChange{Type: TString, MaxLen: 3}), "value too long")

	// Identity columns stay int.
	if err := e.AddColumn("items", Column{Name: "seq", Type: TInt, Identity: true}); err != nil {
		t.Fatal(err)
	}
	wantErr(t, e.AlterColumnType("items", "seq", ColumnTypeChange{Type: TFloat}), "must be an int column")
}

type errString string

func (e errString) Error() string { return string(e) }

func TestAlterColumnTypeUsing(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "t.db"))
	defer e.Close()
	if _, err := e.CreateTable("t", []Column{{Name: "id", Type: TInt}, {Name: "n", Type: TString}}, "id"); err != nil {
		t.Fatal(err)
	}
	for _, r := range [][]any{{1, "7"}, {2, nil}} {
		if err := e.Insert("t", r...); err != nil {
			t.Fatal(err)
		}
	}
	// Using sees the whole old row and may fill a NULL.
	err := e.AlterColumnType("t", "n", ColumnTypeChange{Type: TInt, Using: func(r Row) (any, error) {
		if r.Col("n") == nil {
			return r.Col("id").(int64) * 100, nil
		}
		return int64(len(r.Col("n").(string))) + 40, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := scanCol(t, e, "t", "n"); !slices.Equal(got, []any{int64(41), int64(200)}) {
		t.Fatalf("n = %v", got)
	}
}

func TestAlterColumnTypeDefaultsAndFKs(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "t.db"))
	defer e.Close()
	if _, err := e.CreateTable("t", []Column{
		{Name: "id", Type: TInt},
		{Name: "n", Type: TInt, Default: "5"},
		{Name: "at", Type: TTimestamp, Default: "now()"},
		{Name: "b", Type: TBytes},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.AlterColumnType("t", "n", ColumnTypeChange{Type: TString}); err != nil {
		t.Fatal(err)
	}
	if d := e.Table("t").Columns[1].Default; d != "'5'" {
		t.Fatalf("int default 5 cast to text = %q; want '5'", d)
	}
	if err := e.AlterColumnType("t", "at", ColumnTypeChange{Type: TDate}); err != nil {
		t.Fatal(err)
	}
	if d := e.Table("t").Columns[2].Default; d != "now()" {
		t.Fatalf("now() default across timestamp->date = %q", d)
	}
	wantErr(t, e.AlterColumnType("t", "at", ColumnTypeChange{Type: TString}), "default for column")

	// Either side of a foreign key is refused.
	if _, err := e.CreateTable("c", []Column{{Name: "id", Type: TInt}, {Name: "tid", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.AddForeignKey("c", FKDesc{Name: "c_t", Cols: []int{1}, RefTable: "t", RefCols: []string{"id"}}, true); err != nil {
		t.Fatal(err)
	}
	wantErr(t, e.AlterColumnType("c", "tid", ColumnTypeChange{Type: TFloat}), "foreign key column")
	wantErr(t, e.AlterColumnType("t", "id", ColumnTypeChange{Type: TFloat}), "referenced by a foreign key")

	// Same type, same length, no USING: a no-op that does not error.
	if err := e.AlterColumnType("t", "b", ColumnTypeChange{Type: TBytes}); err != nil {
		t.Fatal(err)
	}
}

func TestSetPrimaryKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	e := openEngine(t, path)
	itemsTable(t, e)
	if _, err := e.CreateIndex("items", "items_qty", false, "qty"); err != nil {
		t.Fatal(err)
	}
	if err := e.SetPrimaryKey("items", "code"); err != nil {
		t.Fatal(err)
	}
	desc := e.Table("items")
	if !slices.Equal(desc.PKCols, []int{1}) {
		t.Fatalf("PKCols = %v", desc.PKCols)
	}
	if !desc.Columns[0].NotNull || !desc.Columns[1].NotNull {
		t.Fatal("old and new key columns should both be NOT NULL")
	}
	// Scan order follows the new key; Get uses it.
	if got := scanCol(t, e, "items", "code"); !slices.Equal(got, []any{"a", "b", "c"}) {
		t.Fatalf("codes = %v", got)
	}
	if row, ok, err := e.Get("items", "c"); err != nil || !ok || row.Col("id") != int64(30) {
		t.Fatalf("Get(c) = %v %v %v", row, ok, err)
	}
	// The index entries carry the new key and resolve to rows.
	n := 0
	for _, err := range e.ScanIndex("items", "items_qty", nil, nil) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 3 {
		t.Fatalf("index scan saw %d rows; want 3", n)
	}
	// The new key enforces uniqueness; the old one no longer does.
	wantErr(t, e.Insert("items", 99, "a", 1.0), "duplicate primary key")
	if err := e.Insert("items", 10, "d", 1.0); err != nil {
		t.Fatalf("old key value reused: %v", err)
	}
	// Survives reopen.
	e.Close()
	e = openEngine(t, path)
	defer e.Close()
	if got := scanCol(t, e, "items", "code"); !slices.Equal(got, []any{"a", "b", "c", "d"}) {
		t.Fatalf("codes after reopen = %v", got)
	}

	// Composite key, then back — same key is a no-op.
	if err := e.SetPrimaryKey("items", "id", "code"); err != nil {
		t.Fatal(err)
	}
	if err := e.SetPrimaryKey("items", "id", "code"); err != nil {
		t.Fatal(err)
	}
	if got := scanCol(t, e, "items", "code"); !slices.Equal(got, []any{"a", "d", "b", "c"}) {
		t.Fatalf("codes under (id, code) = %v", got)
	}
}

func TestSetPrimaryKeyRefusals(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "t.db"))
	defer e.Close()
	itemsTable(t, e)
	if err := e.Insert("items", 40, "a", nil); err != nil {
		t.Fatal(err)
	}
	before := scanCol(t, e, "items", "id")

	wantErr(t, e.SetPrimaryKey("items", "code"), `could not create unique index "items_pkey"`)
	wantErr(t, e.SetPrimaryKey("items", "qty"), `column "qty" of relation "items" contains null values`)
	wantErr(t, e.SetPrimaryKey("items", "nope"), "does not exist")
	wantErr(t, e.SetPrimaryKey("items", "id", "id"), "appears twice")
	if got := scanCol(t, e, "items", "id"); !slices.Equal(got, before) {
		t.Fatalf("failed changes modified rows: %v", got)
	}
	if !slices.Equal(e.Table("items").PKCols, []int{0}) {
		t.Fatal("failed change published a key")
	}

	// A foreign key referencing the old key blocks the change, until a
	// unique index keeps the referenced columns a unique key.
	if _, err := e.Delete("items", 40); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTable("c", []Column{{Name: "id", Type: TInt}, {Name: "item", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.AddForeignKey("c", FKDesc{Name: "c_item", Cols: []int{1}, RefTable: "items", RefCols: []string{"id"}}, true); err != nil {
		t.Fatal(err)
	}
	wantErr(t, e.SetPrimaryKey("items", "code"), "foreign key depends on")
	if _, err := e.CreateIndex("items", "items_id_u", true, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.SetPrimaryKey("items", "code"); err != nil {
		t.Fatal(err)
	}
}

func TestRebuildRespectsBackfillLimit(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "t.db"))
	defer e.Close()
	itemsTable(t, e)
	e.SetBackfillLimit(2)
	wantErr(t, e.SetPrimaryKey("items", "code"), "backfill limit")
	wantErr(t, e.AlterColumnType("items", "qty", ColumnTypeChange{Type: TInt}), "backfill limit")
	e.SetBackfillLimit(3) // exactly at the cap is allowed
	if err := e.SetPrimaryKey("items", "code"); err != nil {
		t.Fatal(err)
	}
}
