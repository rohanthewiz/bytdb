package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3" // driver "sqlite3" (cgo, real C SQLite)
	_ "modernc.org/sqlite"          // driver "sqlite" (pure-Go transpile)
)

// benchSQLite drives SQLite through database/sql the way a Go app
// would: WAL journal, synchronous=OFF (the SyncNever equivalent),
// prepared statements, and _txlock=immediate so heavy transactions
// take the write lock up front instead of failing on a mid-transaction
// read→write upgrade (SQLITE_BUSY_SNAPSHOT is not retried by
// busy_timeout). Ids are plain rowid (INTEGER PRIMARY KEY) — SQLite's
// native cheapest identity.
func benchSQLite(b *testing.B, driver string, heavy bool) {
	path := filepath.Join(b.TempDir(), "bench.db")
	var dsn string
	switch driver {
	case "sqlite3": // mattn option spellings
		dsn = "file:" + path + "?_journal_mode=WAL&_synchronous=OFF&_busy_timeout=10000&_txlock=immediate"
	case "sqlite": // modernc option spellings
		dsn = "file:" + path + "?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=synchronous(OFF)&_pragma=busy_timeout(10000)"
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(8)

	if _, err := db.Exec("CREATE TABLE events (id INTEGER PRIMARY KEY, note TEXT)"); err != nil {
		b.Fatal(err)
	}
	ins, err := db.Prepare("INSERT INTO events(note) VALUES(?)")
	if err != nil {
		b.Fatal(err)
	}
	for range seedRows {
		if _, err := ins.Exec("seed"); err != nil {
			b.Fatal(err)
		}
	}
	sel, err := db.Prepare("SELECT note FROM events WHERE id = ?")
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if !heavy {
				if _, err := ins.Exec("bench"); err != nil {
					b.Error(err)
					return
				}
				continue
			}
			tx, err := db.Begin()
			if err != nil {
				b.Error(err)
				return
			}
			tsel, tins := tx.Stmt(sel), tx.Stmt(ins)
			var note string
			for id := 1; id <= readsPer; id++ {
				if err := tsel.QueryRow(id).Scan(&note); err != nil {
					b.Error(err)
					tx.Rollback()
					return
				}
			}
			if _, err := tins.Exec("bench"); err != nil {
				b.Error(err)
				tx.Rollback()
				return
			}
			if err := tx.Commit(); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkInsert_sqlite_mattn(b *testing.B)   { benchSQLite(b, "sqlite3", false) }
func BenchmarkInsert_sqlite_modernc(b *testing.B) { benchSQLite(b, "sqlite", false) }
func BenchmarkHeavy_sqlite_mattn(b *testing.B)    { benchSQLite(b, "sqlite3", true) }
func BenchmarkHeavy_sqlite_modernc(b *testing.B)  { benchSQLite(b, "sqlite", true) }
