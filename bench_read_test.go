package bytdb

import (
	"path/filepath"
	"sync/atomic"
	"testing"
)

// openReadBench opens a sync-never engine holding n rows in a two-column
// table, so the read benchmarks below measure lookup and snapshot cost,
// not the fsync of setup writes.
func openReadBench(b *testing.B, n int) *Engine {
	b.Helper()
	e, err := Open(filepath.Join(b.TempDir(), "r.db"), WithSyncNever())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { e.Close() })
	if _, err := e.CreateTable("t", []Column{{Name: "id", Type: TInt}, {Name: "v", Type: TInt}}, "id"); err != nil {
		b.Fatal(err)
	}
	if err := e.WriteTxn(func(tx *Txn) error {
		for i := range n {
			if err := tx.Insert("t", i, i); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	return e
}

const readBenchRows = 10_000

// BenchmarkDLEngineGet is the one-shot point read on one goroutine:
// open a read snapshot, decode the descriptor, look up and decode the
// row, release the snapshot (deep-look D-46).
func BenchmarkDLEngineGet(b *testing.B) {
	e := openReadBench(b, readBenchRows)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if _, ok, err := e.Get("t", i%readBenchRows); err != nil || !ok {
			b.Fatal(ok, err)
		}
	}
}

// BenchmarkDLEngineGetParallel is the same one-shot read from
// GOMAXPROCS goroutines. Before btypedb shared read snapshots
// (N-046) every call took the store's mutex exclusively twice, so the
// per-call time did not fall with more cores; with them, a read
// snapshot is opened once per commit and each call only bumps its
// reference count.
func BenchmarkDLEngineGetParallel(b *testing.B) {
	e := openReadBench(b, readBenchRows)
	var seed atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := int(seed.Add(7919))
		for pb.Next() {
			i++
			if _, ok, err := e.Get("t", i%readBenchRows); err != nil || !ok {
				b.Error(ok, err)
				return
			}
		}
	})
}

// BenchmarkDLReadTxnGetParallel is the documented workaround from
// before N-046: one ReadTxn per goroutine around the whole loop. It
// stays the floor that one-shot Get is measured against.
func BenchmarkDLReadTxnGetParallel(b *testing.B) {
	e := openReadBench(b, readBenchRows)
	var seed atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := int(seed.Add(7919))
		err := e.ReadTxn(func(tx *Txn) error {
			for pb.Next() {
				i++
				if _, ok, err := tx.Get("t", i%readBenchRows); err != nil || !ok {
					b.Error(ok, err)
					return nil
				}
			}
			return nil
		})
		if err != nil {
			b.Error(err)
		}
	})
}
