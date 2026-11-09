package edgeaudit

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/pgwire"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

// startServer serves a fresh database on loopback; tune lets a test set
// Server fields before Serve.
func startServer(t testing.TB, tune func(*pgwire.Server), opts ...btypedb.Option) (addr string, srv *pgwire.Server, e *bytdb.Engine) {
	t.Helper()
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "test.db"), opts...)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv = pgwire.NewServer(bsql.New(e))
	if tune != nil {
		tune(srv)
	}
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		e.Close()
	})
	return ln.Addr().String(), srv, e
}

func dsn(addr string) string { return fmt.Sprintf("postgres://test@%s/test?sslmode=disable", addr) }

func pgxConn(t testing.TB, addr string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), dsn(addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}
