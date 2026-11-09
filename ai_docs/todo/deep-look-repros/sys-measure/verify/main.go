// Independent re-check of the SQL reviewer's headline findings.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

func main() {
	dir, _ := os.MkdirTemp("", "v")
	defer os.RemoveAll(dir)
	e, _ := bytdb.Open(filepath.Join(dir, "v.db"), bytdb.WithSyncNever())
	defer e.Close()
	db := bsql.New(e)
	q := func(s string, args ...any) {
		r, err := db.Exec(s, args...)
		if err != nil {
			fmt.Printf("%-95s => ERR %v\n", s, err)
			return
		}
		fmt.Printf("%-95s => %v\n", s, r.Rows)
	}
	q(`CREATE TABLE o (id INT PRIMARY KEY, cust INT, amt INT)`)
	q(`INSERT INTO o VALUES (1,1,10),(2,1,50),(3,2,30),(4,2,5),(5,2,99)`)
	q(`SELECT (SELECT amt FROM o ORDER BY amt DESC LIMIT 1)`)
	q(`SELECT id FROM o WHERE id IN (SELECT id FROM o ORDER BY amt DESC LIMIT 2)`)
	q(`SELECT EXISTS (SELECT 1 FROM o LIMIT 0)`)
	q(`SELECT (SELECT coalesce(sum(amt),0) FROM o WHERE cust = 9)`)
	q(`EXPLAIN SELECT * FROM o WHERE id IN (1,2,3)`)

	q(`CREATE TABLE ev (id UUID PRIMARY KEY, due DATE, at TIMESTAMP)`)
	q(`CREATE INDEX ev_at ON ev (at)`)
	q(`INSERT INTO ev VALUES ('11111111-1111-1111-1111-111111111111','2099-01-01','2024-01-01 10:00:00')`)
	q(`SELECT count(*) FROM ev WHERE due > now()`)
	q(`SELECT at::date FROM ev`)
	q(`SELECT CASE WHEN at > '2020-01-01' THEN 'recent' ELSE 'old' END FROM ev`)
	q(`EXPLAIN SELECT * FROM ev WHERE id = '11111111-1111-1111-1111-111111111111'`)
	q(`EXPLAIN SELECT * FROM ev WHERE at >= '2024-01-01'`)

	q(`CREATE TABLE p (id INT PRIMARY KEY, v TEXT)`)
	q(`CREATE TABLE ch (id INT PRIMARY KEY, pid INT REFERENCES p(id))`)
	q(`INSERT INTO p VALUES (1,'a')`)
	q(`INSERT INTO ch VALUES (1,1)`)
	q(`INSERT INTO p VALUES (1,'x') ON CONFLICT (id) DO UPDATE SET id = 100`)
	q(`SELECT ch.pid, p.id FROM ch LEFT JOIN p ON p.id = ch.pid`)
	q(`SELECT abs(-1)`)
	q(`SELECT CAST('1' AS INT)`)
	q(`CREATE TABLE o2 (id INT PRIMARY KEY, cust INT, amt INT)`)
	q(`INSERT INTO o2 SELECT * FROM o`)
}
