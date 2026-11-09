// Independent re-check of the SQL front-end reviewer's headline findings.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

func main() {
	dir, _ := os.MkdirTemp("", "v")
	defer os.RemoveAll(dir)
	e, _ := bytdb.Open(filepath.Join(dir, "v.db"), bytdb.WithSyncNever())
	defer e.Close()
	db := bsql.New(e)
	q := func(s string) {
		r, err := db.Exec(s)
		if err != nil {
			fmt.Printf("%-80s => ERR %v\n", s, err)
			return
		}
		fmt.Printf("%-80s => %v\n", s, r.Rows)
	}
	q(`CREATE TABLE c (id INT PRIMARY KEY, day DATE CHECK (day >= '2000-01-01'))`)
	q(`INSERT INTO c VALUES (1, '1999-01-01')`)
	q(`SELECT count(*) FROM c`)
	q(`SELECT id FROM c WHERE id IN (SELECT id FROM c WHERE day = '1999-01-01')`)
	q(`SELECT coalesce(1, 1/0)`)
	q(`SELECT 0x1F`)
	q(`SELECT -1::int`)
	q(`SELECT E'\x41'`)

	s := db.NewSession()
	s.Exec(`CREATE TABLE b (id INT PRIMARY KEY)`)
	s.Exec(`BEGIN`)
	s.Exec(`INSERT INTO b VALUES (100)`)
	_, err := s.Exec(`insrt typo`)
	_, cerr := s.Exec(`COMMIT`)
	fmt.Println("in-block syntax error:", err, "| commit err:", cerr)
	q(`SELECT count(*) FROM b`)

	if len(os.Args) > 1 {
		big := "select 1" + strings.Repeat("+1", 1000000)
		_, err := db.Exec(big)
		fmt.Println("1M-term chain:", err)
	}
}
