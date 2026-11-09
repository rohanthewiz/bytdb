package engineaudit

import (
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// Each goroutine holds one ReadTxn and issues many Gets through it.
func BenchmarkTxnGetParallel(b *testing.B) {
	e := benchEngine(b, 0)
	defer e.Close()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		e.ReadTxn(func(tx *bytdb.Txn) error {
			i := 0
			for pb.Next() {
				i++
				if _, ok, err := tx.Get("t", i%10000); !ok || err != nil {
					b.Fatal(err)
				}
			}
			return nil
		})
	})
}
