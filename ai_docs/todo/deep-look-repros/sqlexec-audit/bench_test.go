package sqlexecaudit

import (
	"fmt"
	"strings"
	"sync"
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

var (
	benchOnce sync.Once
	benchDB   *bsql.DB
)

// load fills a table with n rows using 1000-row INSERT batches.
func load(tb testing.TB, d *bsql.DB, table string, n int, row func(i int) string) {
	for i := 0; i < n; i += 1000 {
		var sb strings.Builder
		sb.WriteString("INSERT INTO " + table + " VALUES ")
		for j := i; j < i+1000 && j < n; j++ {
			if j > i {
				sb.WriteByte(',')
			}
			sb.WriteString(row(j))
		}
		if _, err := d.Exec(sb.String()); err != nil {
			tb.Fatal(err)
		}
	}
}

// bdb builds the shared benchmark dataset once per process.
func bdb(b *testing.B) *bsql.DB {
	benchOnce.Do(func() {
		e, err := bytdb.Open(filepath.Join(scratchTmp, "bench.db"))
		if err != nil {
			b.Fatal(err)
		}
		d := bsql.New(e)
		benchEngine = e
		must(b, d,
			`CREATE TABLE orders (id int primary key, cust int NOT NULL, amount float NOT NULL, status text NOT NULL, note text)`,
			`CREATE TABLE cust (id int primary key, name text NOT NULL, region int NOT NULL)`,
			`CREATE TABLE cust_noidx (cid int NOT NULL, name text NOT NULL, region int NOT NULL, pk int primary key)`,
		)
		load(b, d, "orders", 100000, func(i int) string {
			return fmt.Sprintf("(%d, %d, %d.25, 'st%d', 'note number %d')", i, i%10000, i%977, i%5, i)
		})
		load(b, d, "cust", 10000, func(i int) string { return fmt.Sprintf("(%d, 'name%d', %d)", i, i, i%20) })
		load(b, d, "cust_noidx", 10000, func(i int) string { return fmt.Sprintf("(%d, 'name%d', %d, %d)", i, i, i%20, i+5000000) })
		benchDB = d
	})
	return benchDB
}

func run(b *testing.B, q string, args ...any) {
	d := bdb(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Exec(q, args...); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCountSum(b *testing.B)      { run(b, `SELECT count(*), sum(amount) FROM orders`) }
func BenchmarkCountStar(b *testing.B)     { run(b, `SELECT count(*) FROM orders`) }
func BenchmarkCountSumWhere(b *testing.B) { run(b, `SELECT count(*), sum(amount) FROM orders WHERE cust < 5000`) }
func BenchmarkGroupBy(b *testing.B)       { run(b, `SELECT status, count(*), sum(amount) FROM orders GROUP BY status`) }
func BenchmarkPointSelect(b *testing.B)   { run(b, `SELECT amount FROM orders WHERE id = 54321`) }
func BenchmarkPointSelectParam(b *testing.B) {
	run(b, `SELECT amount FROM orders WHERE id = $1`, int64(54321))
}
func BenchmarkPointSelectPrepared(b *testing.B) {
	d := bdb(b)
	st, err := d.Prepare(`SELECT amount FROM orders WHERE id = $1`)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.Exec(int64(54321)); err != nil {
			b.Fatal(err)
		}
	}
}
func BenchmarkParseOnly(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := bsql.Parse(`SELECT amount FROM orders WHERE id = 54321`); err != nil {
			b.Fatal(err)
		}
	}
}
func BenchmarkTopNUnindexed(b *testing.B) {
	run(b, `SELECT id, amount FROM orders ORDER BY amount DESC, id LIMIT 10`)
}
func BenchmarkScanAllNoSort(b *testing.B) { run(b, `SELECT id, amount FROM orders WHERE amount < 0`) }
func BenchmarkHashJoin(b *testing.B) {
	run(b, `SELECT count(*) FROM orders o JOIN cust_noidx c ON c.cid = o.cust`)
}
func BenchmarkIndexNLJoin(b *testing.B) {
	run(b, `SELECT count(*) FROM orders o JOIN cust c ON c.id = o.cust`)
}
func inList(n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprint(&sb, i*7)
	}
	return sb.String()
}
func BenchmarkInList10PK(b *testing.B) {
	run(b, `SELECT id FROM orders WHERE id IN (`+inList(10)+`)`)
}
func BenchmarkInList100PK(b *testing.B) {
	run(b, `SELECT id FROM orders WHERE id IN (`+inList(100)+`)`)
}
func BenchmarkAnyArrayPK(b *testing.B) {
	run(b, `SELECT id FROM orders WHERE id = ANY($1)`, "{0,7,14,21,28,35,42,49,56,63}")
}
func BenchmarkOrPK(b *testing.B) {
	run(b, `SELECT id FROM orders WHERE id = 5 OR id = 77`)
}
