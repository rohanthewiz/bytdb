package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/marcboeker/go-duckdb/v2" // driver "duckdb" (cgo, bundled static lib)
)

// benchDuckDB: the OLAP point of reference. File-backed database (one
// shared instance across the pool's connections), id BIGINT PRIMARY
// KEY (ART index, so the 400 point reads are indexed lookups, not
// scans), ids from a native sequence. DuckDB uses optimistic
// concurrency; concurrent transaction conflicts are retried as
// workload, matching how the other OCC engines are driven. Point
// lookups and single-row writes are DuckDB's documented weak spot —
// it is built for vectorized scans — so these numbers are expected to
// be unflattering and are included for niche contrast, not as a
// criticism of DuckDB.
func benchDuckDB(b *testing.B, heavy bool) {
	db, err := sql.Open("duckdb", filepath.Join(b.TempDir(), "bench.duckdb"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(8)

	for _, ddl := range []string{
		"CREATE TABLE events (id BIGINT PRIMARY KEY, note TEXT)",
		"CREATE SEQUENCE ids START 501",
	} {
		if _, err := db.Exec(ddl); err != nil {
			b.Fatal(err)
		}
	}
	seed, err := db.Prepare("INSERT INTO events VALUES (?, 'seed')")
	if err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= seedRows; i++ {
		if _, err := seed.Exec(i); err != nil {
			b.Fatal(err)
		}
	}
	ins, err := db.Prepare("INSERT INTO events VALUES (nextval('ids'), ?)")
	if err != nil {
		b.Fatal(err)
	}
	sel, err := db.Prepare("SELECT note FROM events WHERE id = ?")
	if err != nil {
		b.Fatal(err)
	}

	// DuckDB aborts the losing side of concurrent writes to one table
	// with a transaction-conflict error; that is workload under OCC,
	// not failure, so retry it exactly like ErrTxConflict elsewhere.
	isConflict := func(err error) bool {
		return err != nil && strings.Contains(err.Error(), "onflict")
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			for {
				var err error
				if !heavy {
					_, err = ins.Exec("bench")
				} else {
					err = func() error {
						tx, err := db.Begin()
						if err != nil {
							return err
						}
						defer tx.Rollback()
						tsel := tx.Stmt(sel)
						var note string
						for id := 1; id <= readsPer; id++ {
							if err := tsel.QueryRow(id).Scan(&note); err != nil {
								return err
							}
						}
						if _, err := tx.Stmt(ins).Exec("bench"); err != nil {
							return err
						}
						return tx.Commit()
					}()
				}
				if isConflict(err) {
					continue
				}
				if err != nil {
					b.Error(err)
				}
				break
			}
		}
	})
}

func BenchmarkInsert_duckdb(b *testing.B) { benchDuckDB(b, false) }
func BenchmarkHeavy_duckdb(b *testing.B)  { benchDuckDB(b, true) }
