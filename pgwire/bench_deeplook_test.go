package pgwire_test

// Wire benchmarks behind deep-look D-41 (row encoding) and D-42 (the
// transport): a 100,000-row scan in text and binary result format, and
// a point read over TCP loopback. CI runs them once as a smoke test;
// D-41/D-42 changes carry a benchstat comparison against them.

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/pgwire"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

const wireBenchRows = 100_000

// wireBenchServer serves a fresh database holding wireBenchRows rows of
// five columns and returns a pgx connection to it.
func wireBenchServer(b *testing.B) *pgx.Conn {
	b.Helper()
	e, err := bytdb.Open(filepath.Join(b.TempDir(), "w.db"), bytdb.WithSyncNever())
	if err != nil {
		b.Fatal(err)
	}
	if _, err := e.CreateTable("kv", []bytdb.Column{
		{Name: "id", Type: bytdb.TInt}, {Name: "name", Type: bytdb.TString},
		{Name: "n", Type: bytdb.TInt}, {Name: "f", Type: bytdb.TFloat},
		{Name: "ts", Type: bytdb.TTimestamp},
	}, "id"); err != nil {
		b.Fatal(err)
	}
	if err := e.WriteTxn(func(tx *bytdb.Txn) error {
		for i := range wireBenchRows {
			if err := tx.Insert("kv", i, "name-of-row", i*7, float64(i)*1.5, int64(1700000000000000+i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	srv := pgwire.NewServer(bsql.New(e))
	go srv.Serve(ln)
	b.Cleanup(func() {
		srv.Close()
		e.Close()
	})
	c, err := pgx.Connect(context.Background(), fmt.Sprintf("postgres://bench@%s/bench", ln.Addr()))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// benchScan drains the whole table per iteration in the given mode:
// the simple protocol returns text; pgx's default extended protocol
// asks for binary.
func benchScan(b *testing.B, mode pgx.QueryExecMode) {
	c := wireBenchServer(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		rows, err := c.Query(ctx, `select id, name, n, f, ts from kv`, mode)
		if err != nil {
			b.Fatal(err)
		}
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil || n != wireBenchRows {
			b.Fatalf("scanned %d rows: %v", n, err)
		}
	}
}

func BenchmarkDLWireScan100kText(b *testing.B) {
	benchScan(b, pgx.QueryExecModeSimpleProtocol)
}

func BenchmarkDLWireScan100kBinary(b *testing.B) {
	benchScan(b, pgx.QueryExecModeCacheStatement)
}

func BenchmarkDLWirePointRead(b *testing.B) {
	c := wireBenchServer(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		var name string
		if err := c.QueryRow(ctx, `select name from kv where id = $1`, int64(i%wireBenchRows)).Scan(&name); err != nil {
			b.Fatal(err)
		}
	}
}
