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
	"os"
	"os/signal"
	"syscall"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/pgwire"
	"github.com/rohanthewiz/bytdb/sql"
)

func main() {
	dbPath := flag.String("db", "", "database file (created if absent)")
	addr := flag.String("addr", "127.0.0.1:5433", "listen address")
	idleTx := flag.Duration("idle-tx-timeout", 0,
		"terminate connections idle in a transaction this long (0 = server default, negative = disabled)")
	flag.Parse()
	if *dbPath == "" {
		log.Fatal("bytdbd: -db is required")
	}
	e, err := bytdb.Open(*dbPath)
	if err != nil {
		log.Fatalf("bytdbd: %s", bytdb.ErrText(err))
	}
	// Closed on the way out of main — which the signal path below
	// reaches too — so the engine's final flush and fsync always run.
	// (log.Fatalf skips defers, hence the explicit error returns above
	// happen before this point.)
	defer e.Close()

	srv := pgwire.NewServer(sql.New(e))
	srv.IdleTxTimeout = *idleTx

	// A signal must not kill the process outright: under a relaxed
	// sync policy the engine buffers acknowledged writes, and only
	// Close guarantees they reach disk. Closing the server makes
	// ListenAndServe return nil, letting main unwind through the
	// deferred e.Close.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-sig
		fmt.Printf("bytdbd: %s received, shutting down\n", s)
		srv.Close()
	}()

	fmt.Printf("bytdbd: serving %s on %s\n", *dbPath, *addr)
	if err := srv.ListenAndServe(*addr); err != nil {
		// Not Fatalf: the engine still needs its deferred Close.
		fmt.Fprintf(os.Stderr, "bytdbd: %v\n", err)
	}
}
