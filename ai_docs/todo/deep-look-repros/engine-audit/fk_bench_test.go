package engineaudit

import (
	"fmt"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// Cost of Txn.ReferencingFKs (called by the SQL layer once per
// DELETE/UPDATE statement) as the catalog grows.
func benchRefFKs(b *testing.B, tables int) {
	e := open(b, false)
	defer e.Close()
	for i := 0; i < tables; i++ {
		_, err := e.CreateTable(fmt.Sprintf("tbl_%04d", i), []bytdb.Column{
			{Name: "id", Type: bytdb.TInt}, {Name: "name", Type: bytdb.TString},
			{Name: "created_at", Type: bytdb.TTimestamp}, {Name: "owner_id", Type: bytdb.TInt},
			{Name: "status", Type: bytdb.TString}, {Name: "payload", Type: bytdb.TJSONB},
		}, "id")
		must(b, err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.ReadTxn(func(tx *bytdb.Txn) error {
			_, err := tx.ReferencingFKs("tbl_0000")
			return err
		})
	}
}
func BenchmarkReferencingFKs10Tables(b *testing.B)  { benchRefFKs(b, 10) }
func BenchmarkReferencingFKs500Tables(b *testing.B) { benchRefFKs(b, 500) }
