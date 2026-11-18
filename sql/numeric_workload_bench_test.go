package sql

// A decimal-heavy analytical workload (N-050): TPC-H's lineitem shape
// with its two arithmetic-heavy queries, Q1 (pricing summary: products
// of numeric columns summed per group) and Q6 (forecast revenue: range
// filters on numeric columns, then a summed product). The same table in
// float8 measures what exact numeric costs over binary floating point,
// and a 10k-row fetch rendered to text measures the output path a
// Postgres-wire client exercises.
//
//	li    100,000 rows   numeric(15,2) quantity/price/discount/tax
//	lif   100,000 rows   the same values as float8
//
// Measured 2026-11-15 (Apple M-series, -count 6, means), before and
// after the decimal package's small-coefficient form (N-050):
//
//	                  before     after    float8   allocs/row before → after (float8)
//	Q1                120 ms     69 ms    52 ms    50 → 17 (17)
//	Q6                 37 ms     27 ms    19 ms    13 → 6.0 (5.9)
//	fetch 10k + text  7.1 ms    3.7 ms   4.2 ms    24 → 7.0 (6.8)
//
// What remains on Q1 is mostly boxing: each arithmetic result is a
// 24-byte Decimal stored in an interface, one allocation per operator
// per row (float8 boxes 8 bytes the same way).

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/decimal"
)

var (
	liOnce sync.Once
	liDB   *DB
	liErr  error
	liDir  string
)

func lineitemBenchDB(b *testing.B) *DB {
	liOnce.Do(func() {
		// Outlives any one benchmark, like sharedBenchDB's.
		dir, err := os.MkdirTemp("", "bytdb-libench-")
		if err != nil {
			liErr = err
			return
		}
		liDir = dir
		e, err := bytdb.Open(filepath.Join(dir, "li.db"), bytdb.WithSyncNever())
		if err != nil {
			liErr = err
			return
		}
		d := New(e)
		for _, q := range []string{
			`CREATE TABLE li (id int primary key, quantity numeric(15,2) NOT NULL, price numeric(15,2) NOT NULL,
				discount numeric(15,2) NOT NULL, tax numeric(15,2) NOT NULL, rflag text NOT NULL, lstatus text NOT NULL, shipdate date NOT NULL)`,
			`CREATE TABLE lif (id int primary key, quantity float8 NOT NULL, price float8 NOT NULL,
				discount float8 NOT NULL, tax float8 NOT NULL, rflag text NOT NULL, lstatus text NOT NULL, shipdate date NOT NULL)`,
		} {
			if _, err := d.Exec(q); err != nil {
				liErr = err
				return
			}
		}
		// Values in TPC-H's ranges: quantity 1-50, price ~900-105000,
		// discount 0.00-0.10, tax 0.00-0.08, three return flags, two
		// statuses, ship dates over six years.
		row := func(i int) string {
			qty := 1 + i%50
			price := 90000 + (i*7919)%10400000 // cents
			return fmt.Sprintf("(%d, %d.00, %d.%02d, 0.%02d, 0.%02d, '%c', '%c', date '1992-01-01' + %d)",
				i, qty, price/100, price%100, i%11, i%9, "ARN"[i%3], "OF"[i%2], i%2400)
		}
		for _, t := range []string{"li", "lif"} {
			if liErr = benchLoad(d, t, 100_000, row); liErr != nil {
				return
			}
		}
		liDB = d
	})
	if liErr != nil {
		b.Fatal(liErr)
	}
	return liDB
}

func q1(t string) string {
	return `SELECT rflag, lstatus, sum(quantity), sum(price), sum(price * (1 - discount)),
		sum(price * (1 - discount) * (1 + tax)), avg(quantity), avg(price), avg(discount), count(*)
		FROM ` + t + ` WHERE shipdate <= date '1998-09-02' GROUP BY rflag, lstatus ORDER BY rflag, lstatus`
}

func q6(t string) string {
	return `SELECT sum(price * discount) FROM ` + t + `
		WHERE shipdate >= date '1994-01-01' AND shipdate < date '1995-01-01'
		AND discount BETWEEN 0.05 AND 0.07 AND quantity < 24`
}

func benchLI(b *testing.B, q string) {
	d := lineitemBenchDB(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := d.Exec(q); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNumericTPCHQ1(b *testing.B)      { benchLI(b, q1("li")) }
func BenchmarkNumericTPCHQ1Float(b *testing.B) { benchLI(b, q1("lif")) }
func BenchmarkNumericTPCHQ6(b *testing.B)      { benchLI(b, q6("li")) }
func BenchmarkNumericTPCHQ6Float(b *testing.B) { benchLI(b, q6("lif")) }

// Fetch 10k rows and render every numeric to text, as a text-format
// wire response does.
func benchFetch(b *testing.B, t string) {
	d := lineitemBenchDB(b)
	b.ReportAllocs()
	b.ResetTimer()
	var buf []byte
	for b.Loop() {
		res, err := d.Exec(`SELECT quantity, price, discount, tax FROM ` + t + ` WHERE id < 10000`)
		if err != nil {
			b.Fatal(err)
		}
		// pgwire's text encoders for the two types.
		for _, r := range res.Rows {
			for _, v := range r {
				switch x := v.(type) {
				case decimal.Decimal:
					buf = x.AppendText(buf[:0])
				case float64:
					buf = bytdb.AppendFloat(buf[:0], x)
				}
			}
		}
	}
	_ = buf
}

func BenchmarkNumericFetch10k(b *testing.B)      { benchFetch(b, "li") }
func BenchmarkNumericFetch10kFloat(b *testing.B) { benchFetch(b, "lif") }
