package bytdb

// Deep-look D-39: Txn.ScanWith decodes only the masked columns, on every
// scan form, and decodeRowCols round-trips every value kind.

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestScanWithColumnMask(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "db"))
	defer e.Close()
	usersTable(t, e)
	if _, err := e.CreateIndex("users", "users_score", false, "score"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		if err := e.Insert("users", i, fmt.Sprintf("n%d\x00x", i), float64(i)/2, i%2 == 0, []byte{0, byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	mask := []bool{false, false, true, false, true} // score and blob; id is the key
	err := e.ReadTxn(func(tx *Txn) error {
		for _, o := range []ScanOptions{
			{Cols: mask},
			{Cols: mask, Reverse: true},
			{Cols: mask, From: []any{2}, To: []any{4}},
			{Cols: mask, Index: "users_score"},
			{Cols: mask, Index: "users_score", Reverse: true, To: []any{2.0}, ToIncl: true},
		} {
			n := 0
			for row, err := range tx.ScanWith("users", o) {
				if err != nil {
					return err
				}
				n++
				id := row.Vals[0].(int64)
				if row.Vals[1] != nil || row.Vals[3] != nil {
					t.Errorf("%+v: unmasked columns decoded: %v", o, row.Vals)
				}
				if row.Vals[2] != float64(id)/2 || fmt.Sprint(row.Vals[4]) != fmt.Sprint([]byte{0, byte(id)}) {
					t.Errorf("%+v: masked columns wrong: %v", o, row.Vals)
				}
			}
			if n == 0 {
				t.Errorf("%+v: no rows", o)
			}
		}
		// A mask of the wrong length (built for another version of the
		// table) is ignored: every column decodes.
		for row, err := range tx.ScanWith("users", ScanOptions{Cols: []bool{true}}) {
			if err != nil {
				return err
			}
			if row.Vals[1] == nil || row.Vals[3] == nil {
				t.Errorf("short mask applied: %v", row.Vals)
			}
			break
		}
		// No mask: identical to Scan.
		var a, b []string
		for row, err := range tx.ScanWith("users", ScanOptions{}) {
			if err != nil {
				return err
			}
			a = append(a, fmt.Sprint(row.Vals))
		}
		for row, err := range tx.Scan("users") {
			if err != nil {
				return err
			}
			b = append(b, fmt.Sprint(row.Vals))
		}
		if fmt.Sprint(a) != fmt.Sprint(b) {
			t.Errorf("ScanWith{} = %v\nScan      = %v", a, b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Deep-look D-44: a wide parsed descriptor resolves names through an
// index; ColIndexUnique reports duplicates on synthesized descriptors.
func TestColIndexUnique(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "db"))
	defer e.Close()
	cols := []Column{{Name: "id", Type: TInt}}
	for i := range 40 {
		cols = append(cols, Column{Name: fmt.Sprintf("c%02d", i), Type: TInt})
	}
	if _, err := e.CreateTable("wide", cols, "id"); err != nil {
		t.Fatal(err)
	}
	d := e.TableShared("wide")
	if d.ordByName == nil {
		t.Fatal("wide parsed descriptor has no name index")
	}
	for i, c := range cols {
		if ord, unique := d.ColIndexUnique(c.Name); ord != i || !unique || d.ColIndex(c.Name) != i {
			t.Fatalf("%s -> %d, %v", c.Name, ord, unique)
		}
	}
	if ord, _ := d.ColIndexUnique("nope"); ord != -1 || d.ColIndex("nope") != -1 {
		t.Fatal("absent name resolved")
	}
	if c := d.Clone(); c.ordByName != nil {
		t.Fatal("clone kept the name index it may desynchronize")
	}
	synth := &TableDesc{Columns: []Column{{Name: "id"}, {Name: "x"}, {Name: "id"}}}
	if ord, unique := synth.ColIndexUnique("id"); ord != 0 || unique {
		t.Fatalf("duplicate id -> %d, %v", ord, unique)
	}
	if ord, unique := synth.ColIndexUnique("x"); ord != 1 || !unique {
		t.Fatalf("x -> %d, %v", ord, unique)
	}
}
