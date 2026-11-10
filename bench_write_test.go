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
