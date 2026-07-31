package main

import (
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// benchBytdb mirrors bytdb's own BenchmarkIdentityInsert* benches:
// engine API (no SQL parse), identity column drawing ids. In occ mode
// the write keys are always fresh identity values and the read set is
// never written, so WriteTxn cannot conflict; errors are real failures.
func benchBytdb(b *testing.B, occ, heavy bool) {
	path := filepath.Join(b.TempDir(), "bench.db")
	var e *bytdb.Engine
	var err error
	if occ {
		e, err = bytdb.Open(path, bytdb.WithSyncNever(), bytdb.WithConcurrentWrites())
	} else {
		e, err = bytdb.Open(path, bytdb.WithSyncNever())
	}
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close()

	if _, err := e.CreateTable("events", []bytdb.Column{
		{Name: "id", Type: bytdb.TInt, Identity: true},
		{Name: "note", Type: bytdb.TString},
	}, "id"); err != nil {
		b.Fatal(err)
	}
	for range seedRows {
		if err := e.Insert("events", nil, "seed"); err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			err := e.WriteTxn(func(tx *bytdb.Txn) error {
				if heavy {
					for pk := int64(1); pk <= readsPer; pk++ {
						if _, _, err := tx.Get("events", pk); err != nil {
							return err
						}
					}
				}
				return tx.Insert("events", nil, "bench")
			})
			if err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkInsert_bytdb_single_writer(b *testing.B) { benchBytdb(b, false, false) }
func BenchmarkInsert_bytdb_occ(b *testing.B)           { benchBytdb(b, true, false) }
func BenchmarkHeavy_bytdb_single_writer(b *testing.B)  { benchBytdb(b, false, true) }
func BenchmarkHeavy_bytdb_occ(b *testing.B)            { benchBytdb(b, true, true) }
