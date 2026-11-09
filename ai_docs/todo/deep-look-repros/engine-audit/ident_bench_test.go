package engineaudit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// Default (single-writer) mode: each identity draw stages its own
// counter write. Compare a bulk load with and without an identity key.
func benchBulk(b *testing.B, identity bool) {
	dir := b.TempDir()
	p := filepath.Join(dir, "db")
	e, err := bytdb.Open(p, bytdb.WithSyncNever())
	must(b, err)
	defer e.Close()
	_, err = e.CreateTable("t", []bytdb.Column{
		{Name: "id", Type: bytdb.TInt, Identity: identity},
		{Name: "v", Type: bytdb.TString},
	}, "id")
	must(b, err)
	e.SetMaxTxnWrites(0)
	b.ReportAllocs()
	b.ResetTimer()
	must(b, e.WriteTxn(func(tx *bytdb.Txn) error {
		for i := 0; i < b.N; i++ {
			var id any = i + 1
			if identity {
				id = nil
			}
			if err := tx.Insert("t", id, "payload"); err != nil {
				return err
			}
		}
		return nil
	}))
	b.StopTimer()
	if st, err := os.Stat(p); err == nil {
		b.ReportMetric(float64(st.Size())/float64(b.N), "walB/row")
	}
}
func BenchmarkBulkInsertPlainKey(b *testing.B)    { benchBulk(b, false) }
func BenchmarkBulkInsertIdentityKey(b *testing.B) { benchBulk(b, true) }
