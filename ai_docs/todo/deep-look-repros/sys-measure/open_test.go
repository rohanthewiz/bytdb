package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

// BenchmarkOpen isolates startup replay so a CPU profile shows where a
// restart of a 500k-row (1M-key) database spends its time.
func BenchmarkOpen(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "m.db")
	e, _ := bytdb.Open(path, bytdb.WithSyncNever())
	db := bsql.New(e)
	db.Exec(`CREATE TABLE t (id INT PRIMARY KEY, name TEXT, amount FLOAT, ts TIMESTAMP)`)
	db.Exec(`CREATE INDEX t_name ON t (name)`)
	n := 500000
	for i := 0; i < n; i += 1000 {
		var sb strings.Builder
		sb.WriteString("INSERT INTO t VALUES ")
		for j := 0; j < 1000; j++ {
			if j > 0 {
				sb.WriteByte(',')
			}
			id := i + j
			fmt.Fprintf(&sb, "(%d,'name-%08d',%d.5,'2026-01-01 00:00:00')", id, id*7919%n, id)
		}
		if _, err := db.Exec(sb.String()); err != nil {
			b.Fatal(err)
		}
	}
	e.Compact()
	e.Close()
	fi, _ := os.Stat(path)
	b.SetBytes(fi.Size())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e, err := bytdb.Open(path, bytdb.WithSyncNever())
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		e.Close()
		b.StartTimer()
	}
}
