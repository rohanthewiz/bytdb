package bytdb

import (
	"path/filepath"
	"reflect"
	"testing"
)

// returning_test.go: InsertReturning and UpdateReturning — the row as
// stored (identity filled, values coerced), engine one-shot and Txn.

func TestInsertReturning(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "t.db"))
	defer e.Close()
	if _, err := e.CreateTable("users", []Column{
		{Name: "id", Type: TInt, Identity: true},
		{Name: "name", Type: TString},
	}, "id"); err != nil {
		t.Fatal(err)
	}

	// The one-shot returns the stored row: identity drawn, int widths
	// coerced to int64.
	row, err := e.InsertReturning("users", nil, "ada")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(row.Vals, []any{int64(1), "ada"}) {
		t.Fatalf("stored row: %v", row.Vals)
	}
	if row.Col("id") != int64(1) {
		t.Fatalf("Col(id): %v", row.Col("id"))
	}

	// Same through a transaction, with an explicit int (not int64) id.
	err = e.WriteTxn(func(tx *Txn) error {
		row, err := tx.InsertReturning("users", 10, "grace")
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(row.Vals, []any{int64(10), "grace"}) {
			t.Fatalf("txn stored row: %v", row.Vals)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A failed insert returns no row.
	if _, err := e.InsertReturning("users", 10, "dup"); err == nil {
		t.Fatal("duplicate key insert succeeded")
	}
}

func TestUpdateReturning(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "t.db"))
	defer e.Close()
	if _, err := e.CreateTable("users", []Column{
		{Name: "id", Type: TInt},
		{Name: "name", Type: TString},
		{Name: "age", Type: TInt},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("users", 1, "ada", 36); err != nil {
		t.Fatal(err)
	}

	err := e.WriteTxn(func(tx *Txn) error {
		// The returned row is the post-update image: touched and
		// untouched columns alike, coerced.
		row, ok, err := tx.UpdateReturning("users", []any{1}, map[string]any{"age": 37})
		if err != nil || !ok {
			t.Fatalf("update: %v ok=%v", err, ok)
		}
		if !reflect.DeepEqual(row.Vals, []any{int64(1), "ada", int64(37)}) {
			t.Fatalf("post-update row: %v", row.Vals)
		}
		// A missing row reports ok=false with a zero Row.
		row, ok, err = tx.UpdateReturning("users", []any{99}, map[string]any{"age": 1})
		if err != nil || ok || row.Vals != nil {
			t.Fatalf("missing row: %v ok=%v row=%v", err, ok, row.Vals)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
