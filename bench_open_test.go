package bytdb

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkDLOpen500k measures startup replay (deep-look D-48): Open of
// a compacted file holding 500,000 rows plus one secondary index, so
// about a million keys. The plan's figure is ~0.93 s per million rows
// on an M1 Pro; a profile of this benchmark is where D-48's replay work
// starts. The file is built with the engine API in one transaction, so
// the setup stays a few seconds even in CI's one-iteration smoke run.
func BenchmarkDLOpen500k(b *testing.B) {
	const n = 500_000
	path := filepath.Join(b.TempDir(), "open.db")
	e, err := Open(path, WithSyncNever())
	if err != nil {
		b.Fatal(err)
	}
	if _, err := e.CreateTable("t", []Column{
		{Name: "id", Type: TInt}, {Name: "name", Type: TString},
		{Name: "amount", Type: TFloat}, {Name: "ts", Type: TTimestamp},
	}, "id"); err != nil {
		b.Fatal(err)
	}
	if _, err := e.CreateIndex("t", "t_name", false, "name"); err != nil {
		b.Fatal(err)
	}
	e.SetMaxTxnWrites(0)
	if err := e.WriteTxn(func(tx *Txn) error {
		for i := range n {
			if err := tx.Insert("t", i, fmt.Sprintf("name-%08d", i*7919%n), float64(i)+0.5, int64(1767225600000000)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	if err := e.Compact(); err != nil {
		b.Fatal(err)
	}
	e.Close()
	fi, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(fi.Size())
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		e, err := Open(path, WithSyncNever())
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		e.Close()
		b.StartTimer()
	}
}
