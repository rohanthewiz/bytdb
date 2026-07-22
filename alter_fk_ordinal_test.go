package bytdb

// Regression: DropColumn must renumber foreign-key child ordinals the
// same way it renumbers primary-key and index ordinals. The guard only
// refuses dropping a column that IS an FK child column; dropping an
// unrelated column with a LOWER ordinal is allowed, and before the fix
// the FK's Cols kept their old (now too-high) values — so the constraint
// silently enforced against the wrong column, or panicked with an
// index-out-of-range when the child column had been the last ordinal.

import (
	"path/filepath"
	"testing"
)

// fkChildCol returns the name of the column the table's sole FK points
// at — the descriptor read that would panic if an ordinal went stale.
func fkChildCol(t *testing.T, e *Engine, table string) string {
	t.Helper()
	desc := e.Table(table)
	if desc == nil {
		t.Fatalf("table %q not found", table)
	}
	if len(desc.ForeignKeys) != 1 {
		t.Fatalf("table %q: want 1 FK, have %d", table, len(desc.ForeignKeys))
	}
	fk := desc.ForeignKeys[0]
	if len(fk.Cols) != 1 {
		t.Fatalf("FK %q: want 1 child column, have %d", fk.Name, len(fk.Cols))
	}
	ord := fk.Cols[0]
	if ord < 0 || ord >= len(desc.Columns) {
		t.Fatalf("FK %q child ordinal %d out of range (table has %d columns) — ordinal not shifted on drop",
			fk.Name, ord, len(desc.Columns))
	}
	return desc.Columns[ord].Name
}

func TestDropColumnShiftsForeignKeyOrdinals(t *testing.T) {
	setup := func(t *testing.T, childCols []Column, dropCol string) *Engine {
		e := openEngine(t, filepath.Join(t.TempDir(), "fk.db"))
		if _, err := e.CreateTable("p", []Column{{Name: "id", Type: TInt}}, "id"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.CreateTable("c", childCols, "id"); err != nil {
			t.Fatal(err)
		}
		if err := e.AddForeignKey("c", FKDesc{
			Name: "c_ref_fk", Cols: []int{colOrd(childCols, "ref")},
			RefTable: "p", RefCols: []string{"id"},
		}, false); err != nil {
			t.Fatal(err)
		}
		if err := e.DropColumn("c", dropCol); err != nil {
			t.Fatalf("DropColumn(%q): %v", dropCol, err)
		}
		return e
	}

	// FK child column in the MIDDLE: dropping a lower-ordinal column must
	// leave the FK naming "ref", not the column that slides into its old
	// ordinal.
	t.Run("middle", func(t *testing.T) {
		e := setup(t, []Column{
			{Name: "filler", Type: TInt}, {Name: "tag", Type: TString},
			{Name: "ref", Type: TInt}, {Name: "id", Type: TInt},
		}, "filler")
		defer e.Close()
		if got := fkChildCol(t, e, "c"); got != "ref" {
			t.Errorf("FK child column = %q after drop, want %q", got, "ref")
		}
	})

	// FK child column LAST: the pre-fix stale ordinal equals the new
	// column count, so the descriptor read is an out-of-range panic —
	// fkChildCol's bounds check turns that into a clear failure.
	t.Run("last-ordinal", func(t *testing.T) {
		e := setup(t, []Column{
			{Name: "filler", Type: TInt}, {Name: "id", Type: TInt},
			{Name: "ref", Type: TInt},
		}, "filler")
		defer e.Close()
		if got := fkChildCol(t, e, "c"); got != "ref" {
			t.Errorf("FK child column = %q after drop, want %q", got, "ref")
		}
	})
}

func colOrd(cols []Column, name string) int {
	for i, c := range cols {
		if c.Name == name {
			return i
		}
	}
	return -1
}
