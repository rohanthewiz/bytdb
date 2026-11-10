package sql

// Benchmarks behind the deep-look plan's performance items (D-04). They
// are the baseline each Phase 7 change is measured against with
// benchstat; CI only runs them once (-benchtime=1x) to prove they still
// build and run. Numbers in the plan came from an Apple M3 / M1 Pro.
//
// All of them share one dataset, built once per test binary:
//
//	orders      100,000 rows  id int pk, cust int, amount float, status text, note text
//	cust         10,000 rows  id int pk (indexed join target)
//	cust_noidx   10,000 rows  join column not indexed (forces the hash join)
//	ev           10,000 rows  id uuid pk (D-37: uuid point reads)

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

var (
	benchOnce sync.Once
	benchDB   *DB
	benchErr  error
	benchDir  string
)

// TestMain only exists to remove the shared benchmark dataset's
// directory, which must outlive any one benchmark.
func TestMain(m *testing.M) {
	code := m.Run()
	if benchDir != "" {
		os.RemoveAll(benchDir)
	}
	os.Exit(code)
}

// benchLoad inserts n rows in 1,000-row statements.
func benchLoad(d *DB, table string, n int, row func(i int) string) error {
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
			return err
		}
	}
	return nil
}

func benchUUID(i int) string {
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i)
}

func sharedBenchDB(b *testing.B) *DB {
	benchOnce.Do(func() {
		dir, err := os.MkdirTemp("", "bytdb-sqlbench-")
		if err != nil {
			benchErr = err
			return
		}
		benchDir = dir
		e, err := bytdb.Open(filepath.Join(dir, "bench.db"), bytdb.WithSyncNever())
		if err != nil {
			benchErr = err
			return
		}
		d := New(e)
		for _, q := range []string{
			`CREATE TABLE orders (id int primary key, cust int NOT NULL, amount float NOT NULL, status text NOT NULL, note text)`,
			`CREATE TABLE cust (id int primary key, name text NOT NULL, region int NOT NULL)`,
			`CREATE TABLE cust_noidx (cid int NOT NULL, name text NOT NULL, region int NOT NULL, pk int primary key)`,
			`CREATE TABLE ev (id uuid primary key, n int)`,
		} {
			if _, err := d.Exec(q); err != nil {
				benchErr = err
				return
			}
		}
		loads := []struct {
			table string
			n     int
			row   func(int) string
		}{
			{"orders", 100_000, func(i int) string {
				return fmt.Sprintf("(%d, %d, %d.25, 'st%d', 'note number %d')", i, i%10000, i%977, i%5, i)
			}},
			{"cust", 10_000, func(i int) string { return fmt.Sprintf("(%d, 'name%d', %d)", i, i, i%20) }},
			{"cust_noidx", 10_000, func(i int) string { return fmt.Sprintf("(%d, 'name%d', %d, %d)", i, i, i%20, i+5000000) }},
			{"ev", 10_000, func(i int) string { return fmt.Sprintf("('%s', %d)", benchUUID(i), i) }},
		}
		for _, l := range loads {
			if benchErr = benchLoad(d, l.table, l.n, l.row); benchErr != nil {
				return
			}
		}
		benchDB = d
	})
	if benchErr != nil {
		b.Fatal(benchErr)
	}
	return benchDB
}

func benchQuery(b *testing.B, q string, args ...any) {
	d := sharedBenchDB(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Exec(q, args...); err != nil {
			b.Fatal(err)
		}
	}
}

// D-39: a full scan with two aggregates is dominated by row decoding.
func BenchmarkDLCountSum100k(b *testing.B) { benchQuery(b, `SELECT count(*), sum(amount) FROM orders`) }

// D-40: point reads, unprepared and prepared.
func BenchmarkDLPointSelect(b *testing.B) {
	benchQuery(b, `SELECT amount FROM orders WHERE id = $1`, int64(54321))
}

func BenchmarkDLPointSelectPrepared(b *testing.B) {
	d := sharedBenchDB(b)
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

// D-38: IN-lists and = ANY on the primary key (full scans today).
func BenchmarkDLInList10PK(b *testing.B) {
	benchQuery(b, `SELECT id FROM orders WHERE id IN (0,7,14,21,28,35,42,49,56,63)`)
}

func BenchmarkDLAnyArrayPK(b *testing.B) {
	benchQuery(b, `SELECT id FROM orders WHERE id = ANY($1)`, "{0,7,14,21,28,35,42,49,56,63}")
}

// D-37: a uuid primary-key lookup (no pushdown today).
func BenchmarkDLUUIDPointSelect(b *testing.B) {
	benchQuery(b, `SELECT n FROM ev WHERE id = '`+benchUUID(4321)+`'`)
}

// D-43: unindexed ORDER BY ... LIMIT sorts every row today.
func BenchmarkDLTopNUnindexed(b *testing.B) {
	benchQuery(b, `SELECT id, amount FROM orders ORDER BY amount DESC, id LIMIT 10`)
}

// The hash join (no index on the join column) and its indexed twin.
func BenchmarkDLHashJoin(b *testing.B) {
	benchQuery(b, `SELECT count(*) FROM orders o JOIN cust_noidx c ON c.cid = o.cust`)
}

func BenchmarkDLIndexNLJoin(b *testing.B) {
	benchQuery(b, `SELECT count(*) FROM orders o JOIN cust c ON c.id = o.cust`)
}
