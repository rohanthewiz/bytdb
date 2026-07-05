package bytdb

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rohanthewiz/btypedb"
)

func TestUpdateRow(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "a@x"}, []any{2, "grace", 45, "g@x"})
	if _, err := e.CreateIndex("people", "by-age", false, "age"); err != nil {
		t.Fatal(err)
	}

	// Plain column update: value changes, index entry moves.
	updated, err := e.Update("people", []any{1}, map[string]any{"age": 50, "name": "ada l."})
	if err != nil || !updated {
		t.Fatalf("Update: %v, %v", updated, err)
	}
	row, _, _ := e.Get("people", 1)
	if row.Col("age") != int64(50) || row.Col("name") != "ada l." || row.Col("email") != "a@x" {
		t.Fatalf("updated row = %v", row.Vals)
	}
	got := names(t, e.ScanIndex("people", "by-age", nil, nil))
	if want := []string{"grace", "ada l."}; !slices.Equal(got, want) {
		t.Fatalf("index after update = %v; want %v", got, want)
	}
	// Exactly one entry per row: the old age-36 entry must be gone.
	if got := names(t, e.ScanIndex("people", "by-age", []any{30}, []any{40})); got != nil {
		t.Fatalf("stale index entry survived update: %v", got)
	}

	// Updating a missing row reports false without error.
	if updated, err := e.Update("people", []any{99}, map[string]any{"age": 1}); err != nil || updated {
		t.Fatalf("update of missing row: %v, %v", updated, err)
	}
}

func TestUpdatePKMove(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "a@x"}, []any{2, "grace", 45, "g@x"})
	if _, err := e.CreateIndex("people", "by-email", true, "email"); err != nil {
		t.Fatal(err)
	}

	// Changing the primary key moves the row; index entries embed the
	// pk, so they must move too.
	if _, err := e.Update("people", []any{1}, map[string]any{"id": 10}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.Get("people", 1); ok {
		t.Fatal("row still reachable by old pk")
	}
	row, ok, _ := e.Get("people", 10)
	if !ok || row.Col("name") != "ada" {
		t.Fatalf("row at new pk = %v, %v", row.Vals, ok)
	}
	got := names(t, e.ScanIndex("people", "by-email", nil, nil))
	if want := []string{"ada", "grace"}; !slices.Equal(got, want) {
		t.Fatalf("index after pk move = %v; want %v", got, want)
	}

	// Moving onto an existing pk must fail cleanly.
	if _, err := e.Update("people", []any{10}, map[string]any{"id": 2}); err == nil ||
		!strings.Contains(err.Error(), "duplicate primary key") {
		t.Fatalf("pk collision: %v", err)
	}
	if row, ok, _ := e.Get("people", 10); !ok || row.Col("name") != "ada" {
		t.Fatal("row damaged by failed pk move")
	}

	// Setting a pk column to NULL is rejected.
	if _, err := e.Update("people", []any{10}, map[string]any{"id": nil}); err == nil {
		t.Fatal("NULL pk accepted")
	}
	// Unknown column and bad type are rejected.
	if _, err := e.Update("people", []any{10}, map[string]any{"nope": 1}); err == nil {
		t.Fatal("unknown column accepted")
	}
	if _, err := e.Update("people", []any{10}, map[string]any{"age": "old"}); err == nil {
		t.Fatal("mistyped value accepted")
	}
}

func TestUpdateUniqueIndex(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "a@x"}, []any{2, "grace", 45, "g@x"})
	if _, err := e.CreateIndex("people", "by-email", true, "email"); err != nil {
		t.Fatal(err)
	}

	// Updating into a taken unique value fails; nothing may change.
	if _, err := e.Update("people", []any{1}, map[string]any{"email": "g@x"}); err == nil ||
		!strings.Contains(err.Error(), "unique index violation") {
		t.Fatalf("unique collision: %v", err)
	}
	row, _, _ := e.Get("people", 1)
	if row.Col("email") != "a@x" {
		t.Fatal("row changed by failed update")
	}

	// Updating a row's unique value to itself is a no-op, not a
	// self-collision.
	if _, err := e.Update("people", []any{1}, map[string]any{"email": "a@x"}); err != nil {
		t.Fatalf("self-update: %v", err)
	}

	// Moving to NULL frees the value; another row can take it.
	if _, err := e.Update("people", []any{1}, map[string]any{"email": nil}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Update("people", []any{2}, map[string]any{"email": "a@x"}); err != nil {
		t.Fatalf("freed unique value not reusable: %v", err)
	}
	// Two NULLs never conflict.
	if _, err := e.Update("people", []any{2}, map[string]any{"email": nil}); err != nil {
		t.Fatal(err)
	}
	got := names(t, e.ScanIndex("people", "by-email", nil, nil))
	if want := []string{"ada", "grace"}; !slices.Equal(got, want) { // both NULL, pk order
		t.Fatalf("index after NULL updates = %v; want %v", got, want)
	}
}

func TestWriteTxnAtomicity(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	if _, err := e.CreateIndex("people", "by-age", false, "age"); err != nil {
		t.Fatal(err)
	}

	// Error return rolls everything back.
	boom := errors.New("boom")
	err := e.WriteTxn(func(tx *Txn) error {
		if err := tx.Insert("people", 1, "ada", 36, "a@x"); err != nil {
			return err
		}
		if err := tx.Insert("people", 2, "grace", 45, "g@x"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WriteTxn error = %v", err)
	}
	if rows := collect(t, e.Scan("people")); rows != nil {
		t.Fatalf("rolled-back rows visible: %v", rows)
	}
	if got := names(t, e.ScanIndex("people", "by-age", nil, nil)); got != nil {
		t.Fatalf("rolled-back index entries visible: %v", got)
	}

	// Success commits everything at once; own writes visible inside,
	// nothing visible outside until commit.
	err = e.WriteTxn(func(tx *Txn) error {
		if err := tx.Insert("people", 1, "ada", 36, "a@x"); err != nil {
			return err
		}
		if _, err := tx.Update("people", []any{1}, map[string]any{"age": 37}); err != nil {
			return err
		}
		row, ok, err := tx.Get("people", 1)
		if err != nil || !ok || row.Col("age") != int64(37) {
			t.Fatalf("own writes not visible in txn: %v %v %v", row.Vals, ok, err)
		}
		if got := names(t, tx.ScanIndex("people", "by-age", nil, nil)); !slices.Equal(got, []string{"ada"}) {
			t.Fatalf("own index writes not visible in txn: %v", got)
		}
		// The engine's view is still pre-transaction.
		if _, ok, _ := e.Get("people", 1); ok {
			t.Fatal("uncommitted write visible outside the transaction")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	row, ok, _ := e.Get("people", 1)
	if !ok || row.Col("age") != int64(37) {
		t.Fatalf("committed row = %v, %v", row.Vals, ok)
	}
}

func TestWriteTxnHandledErrorStaysClean(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "a@x"}, []any{2, "grace", 45, "g@x"})
	if _, err := e.CreateIndex("people", "by-email", true, "email"); err != nil {
		t.Fatal(err)
	}

	// A unique violation inside a transaction stages no writes, so the
	// caller may handle the error and still commit sound state.
	err := e.WriteTxn(func(tx *Txn) error {
		if _, err := tx.Update("people", []any{1}, map[string]any{"email": "g@x"}); err == nil {
			t.Fatal("expected unique violation")
		}
		if err := tx.Insert("people", 3, "lin", 30, "l@x"); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got := names(t, e.ScanIndex("people", "by-email", nil, nil))
	if want := []string{"ada", "grace", "lin"}; !slices.Equal(got, want) {
		t.Fatalf("index after handled violation = %v; want %v", got, want)
	}
	if row, _, _ := e.Get("people", 1); row.Col("email") != "a@x" {
		t.Fatal("failed update leaked writes into the committed transaction")
	}
}

func TestReadTxnSnapshot(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "a@x"})

	err := e.ReadTxn(func(tx *Txn) error {
		// A write committed after the snapshot began...
		insertPeople(t, e, []any{2, "grace", 45, "g@x"})
		// ...is invisible inside it, visible outside.
		if rows := collect(t, tx.Scan("people")); len(rows) != 1 {
			t.Fatalf("snapshot sees %d rows; want 1", len(rows))
		}
		if _, ok, _ := tx.Get("people", 2); ok {
			t.Fatal("snapshot sees later write")
		}
		if _, ok, _ := e.Get("people", 2); !ok {
			t.Fatal("committed write invisible outside snapshot")
		}
		// Writes through a read transaction are refused.
		if err := tx.Insert("people", 3, "x", 1, nil); !errors.Is(err, btypedb.ErrTxNotWritable) {
			t.Fatalf("write through read txn: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTxnRangeScans(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	if _, err := e.CreateIndex("people", "by-age", false, "age"); err != nil {
		t.Fatal(err)
	}
	err := e.WriteTxn(func(tx *Txn) error {
		for i, age := range []int{50, 30, 40} {
			if err := tx.Insert("people", i, "p", age, nil); err != nil {
				return err
			}
		}
		rows := collect(t, tx.ScanRange("people", []any{1}, []any{3}))
		if len(rows) != 2 {
			t.Fatalf("txn pk range = %d rows; want 2", len(rows))
		}
		var ages []int64
		for row, err := range tx.ScanIndex("people", "by-age", []any{35}, nil) {
			if err != nil {
				return err
			}
			ages = append(ages, row.Col("age").(int64))
		}
		if !slices.Equal(ages, []int64{40, 50}) {
			t.Fatalf("txn index range = %v; want [40 50]", ages)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestManualTxn(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)

	// Commit publishes atomically.
	tx, err := e.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Insert("people", 1, "ada", 36, "a@x"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Insert("people", 2, "grace", 45, "g@x"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.Get("people", 2); !ok {
		t.Fatal("committed row missing")
	}

	// Rollback discards; the writer lock is released either way.
	tx, err = e.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Insert("people", 3, "alan", 41, "al@x"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.Get("people", 3); ok {
		t.Fatal("rolled-back row visible")
	}

	// A read-only transaction is a stable snapshot and refuses writes.
	ro, err := e.Begin(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("people", 4, "edsger", 39, "e@x"); err != nil {
		t.Fatal(err) // read txn holds no writer lock, so this proceeds
	}
	if _, ok, _ := ro.Get("people", 4); ok {
		t.Fatal("read snapshot sees later commit")
	}
	if err := ro.Insert("people", 5, "x", 1, "x@x"); !errors.Is(err, btypedb.ErrTxNotWritable) {
		t.Fatalf("write through read txn = %v; want ErrTxNotWritable", err)
	}
	if err := ro.Rollback(); err != nil {
		t.Fatal(err)
	}
}
