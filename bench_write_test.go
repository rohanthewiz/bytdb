package bytdb

import (
	"path/filepath"
	"testing"
)

// BenchmarkDLWriteTxnOneInsert is the uncontended single-writer path:
// one WriteTxn per inserted row, sync disabled so the engine's own cost
// shows. It guards D-33's write slot, which every default-mode writer
// now takes before btypedb's writer lock (the plan's risk note: no
// measurable regression on this path).
func BenchmarkDLWriteTxnOneInsert(b *testing.B) {
	e, err := Open(filepath.Join(b.TempDir(), "w.db"), WithSyncNever())
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close()
	if _, err := e.CreateTable("t", []Column{{Name: "id", Type: TInt}, {Name: "v", Type: TInt}}, "id"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if err := e.WriteTxn(func(tx *Txn) error { return tx.Insert("t", i, i) }); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDLUpdateUnindexedCol updates a column no index covers on a
// table with two secondary indexes (deep-look D-45: index maintenance
// for an untouched index used to cost two key encodings per index).
func BenchmarkDLUpdateUnindexedCol(b *testing.B) {
	e, err := Open(filepath.Join(b.TempDir(), "u.db"), WithSyncNever())
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close()
	if _, err := e.CreateTable("t", []Column{{Name: "id", Type: TInt}, {Name: "a", Type: TInt},
		{Name: "s", Type: TString}, {Name: "v", Type: TInt}}, "id"); err != nil {
		b.Fatal(err)
	}
	for _, ix := range [][2]string{{"t_a", "a"}, {"t_s", "s"}} {
		if _, err := e.CreateIndex("t", ix[0], false, ix[1]); err != nil {
			b.Fatal(err)
		}
	}
	for i := range 1000 {
		if err := e.Insert("t", i, i, "name", 0); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if err := e.WriteTxn(func(tx *Txn) error {
			_, err := tx.Update("t", []any{i % 1000}, map[string]any{"v": i})
			return err
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDLInsertIndexed inserts into a table with two secondary
// indexes, one row per transaction (D-45's key building).
func BenchmarkDLInsertIndexed(b *testing.B) {
	e, err := Open(filepath.Join(b.TempDir(), "i.db"), WithSyncNever())
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close()
	if _, err := e.CreateTable("t", []Column{{Name: "id", Type: TInt}, {Name: "a", Type: TInt},
		{Name: "s", Type: TString}}, "id"); err != nil {
		b.Fatal(err)
	}
	for _, ix := range [][2]string{{"t_a", "a"}, {"t_s", "s"}} {
		if _, err := e.CreateIndex("t", ix[0], false, ix[1]); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if err := e.WriteTxn(func(tx *Txn) error { return tx.Insert("t", i, i%97, "name") }); err != nil {
			b.Fatal(err)
		}
	}
}
