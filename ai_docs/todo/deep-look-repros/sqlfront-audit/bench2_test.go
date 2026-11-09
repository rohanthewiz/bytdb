package sqlfrontaudit

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

// Engine.Get on the same table shape: the floor a point SELECT could approach.
func BenchmarkPointEngineGetFloor(b *testing.B) {
	e, err := bytdb.Open(filepath.Join(b.TempDir(), "t.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close()
	d := bsql.New(e)
	must(b, d, `create table users (id int primary key, name text, email text, age int, score float, city text, created timestamp)`)
	for i := 0; i < 10000; i += 500 {
		q := "insert into users values "
		for j := i; j < i+500; j++ {
			if j > i {
				q += ","
			}
			q += fmt.Sprintf("(%d, 'name%d', 'u%d@example.com', %d, %d.5, 'city%d', '2024-01-01 00:00:00')", j, j, j, j%90, j%100, j%50)
		}
		must(b, d, q)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := e.Get("users", int64(i%10000)); err != nil {
			b.Fatal(err)
		}
	}
}
