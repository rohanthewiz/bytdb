package sql

// Baselines for the numeric type (N-048): aggregation and filtering over
// 100k numeric(10,2) rows, with the same aggregate over float8 for
// scale. Measured 2026-11-11 (M-series, -benchtime 10x): sum+avg 18 ms
// numeric vs 11.7 ms float8; a cross-scale filter 18 ms. The next lever
// is a small-coefficient fast path in package decimal (plan Q2).

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func numericBenchDB(b *testing.B) *DB {
	e, err := bytdb.Open(filepath.Join(b.TempDir(), "b.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { e.Close() })
	d := New(e)
	if _, err := d.Exec(`create table m (id int primary key, a numeric(10,2), f float8)`); err != nil {
		b.Fatal(err)
	}
	for start := 0; start < 100000; start += 1000 {
		var sb strings.Builder
		sb.WriteString("insert into m values ")
		for i := start; i < start+1000; i++ {
			if i > start {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, "(%d, %d.%02d, %d.%02d)", i, i%5000, i%100, i%5000, i%100)
		}
		if _, err := d.Exec(sb.String()); err != nil {
			b.Fatal(err)
		}
	}
	return d
}

func BenchmarkNumericSumAvg(b *testing.B) {
	d := numericBenchDB(b)
	b.ResetTimer()
	for b.Loop() {
		if _, err := d.Exec(`select sum(a), avg(a) from m`); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNumericSumAvgFloatBaseline(b *testing.B) {
	d := numericBenchDB(b)
	b.ResetTimer()
	for b.Loop() {
		if _, err := d.Exec(`select sum(f), avg(f) from m`); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNumericFilter(b *testing.B) {
	d := numericBenchDB(b)
	b.ResetTimer()
	for b.Loop() {
		if _, err := d.Exec(`select count(*) from m where a > 2500.5`); err != nil {
			b.Fatal(err)
		}
	}
}
