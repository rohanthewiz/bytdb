package sqlexecaudit

import (
	"testing"

	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

var benchEngine *bytdb.Engine

// BenchmarkEngineScan measures the engine-level full scan (decode every
// row) with no SQL layer on top — the floor the SQL aggregate sits on.
func BenchmarkEngineScan(b *testing.B) {
	bdb(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		err := benchEngine.ReadTxn(func(tx *bytdb.Txn) error {
			for _, err := range tx.ScanRange("orders", nil, nil) {
				if err != nil {
					return err
				}
				n++
			}
			return nil
		})
		if err != nil || n != 100000 {
			b.Fatal(err, n)
		}
	}
}

// BenchmarkEngineGet is the engine point read under the SQL point SELECT.
func BenchmarkEngineGet(b *testing.B) {
	bdb(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := benchEngine.ReadTxn(func(tx *bytdb.Txn) error {
			_, ok, err := tx.Get("orders", int64(54321))
			if !ok {
				b.Fatal("missing")
			}
			return err
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseAgg(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := bsql.Parse(`SELECT amount FROM orders WHERE id = 54321`); err != nil {
			b.Fatal(err)
		}
	}
}
