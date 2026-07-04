// Command bytdbd serves a bytdb database file over the PostgreSQL
// wire protocol:
//
//	bytdbd -db app.bytdb -addr 127.0.0.1:5433
//	psql "postgres://any@127.0.0.1:5433/any?sslmode=disable"
package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/pgwire"
	"github.com/rohanthewiz/bytdb/sql"
)

func main() {
	dbPath := flag.String("db", "", "database file (created if absent)")
	addr := flag.String("addr", "127.0.0.1:5433", "listen address")
	flag.Parse()
	if *dbPath == "" {
		log.Fatal("bytdbd: -db is required")
	}
	e, err := bytdb.Open(*dbPath)
	if err != nil {
		log.Fatalf("bytdbd: %s", bytdb.ErrText(err))
	}
	defer e.Close()
	fmt.Printf("bytdbd: serving %s on %s\n", *dbPath, *addr)
	if err := pgwire.NewServer(sql.New(e)).ListenAndServe(*addr); err != nil {
		log.Fatalf("bytdbd: %v", err)
	}
}
