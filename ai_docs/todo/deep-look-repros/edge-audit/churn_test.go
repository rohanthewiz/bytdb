package edgeaudit

import (
	"context"
	"database/sql"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/pgwire"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

type countLn struct {
	net.Listener
	n atomic.Int64
}

func (l *countLn) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.n.Add(1)
	}
	return c, err
}

// pgx's liveness ping is the simple query "-- ping". database/sql+pgx
// (what GORM uses) and pgxpool both ping a connection that has been idle
// for more than a second before handing it out, and discard it when the
// ping fails. How many TCP connections do 5 queries, 1.2 s apart, cost?
func TestIdlePingChurn(t *testing.T) {
	e, _ := bytdb.Open(filepath.Join(t.TempDir(), "x.db"))
	defer e.Close()
	base, _ := net.Listen("tcp", "127.0.0.1:0")
	ln := &countLn{Listener: base}
	srv := pgwire.NewServer(bsql.New(e))
	go srv.Serve(ln)
	defer srv.Close()
	ctx := context.Background()

	db, _ := sql.Open("pgx", dsn(base.Addr().String()))
	defer db.Close()
	for i := 0; i < 5; i++ {
		var one int
		if err := db.QueryRowContext(ctx, "select 1").Scan(&one); err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
		time.Sleep(1200 * time.Millisecond)
	}
	t.Logf("database/sql + pgx stdlib: 5 sequential queries used %d server connections (expected 1)", ln.n.Load())
	if ln.n.Load() > 1 {
		t.Errorf("idle-ping failure makes database/sql discard and redial the connection on every idle>1s checkout")
	}

	before := ln.n.Load()
	pool, err := pgxpool.New(ctx, dsn(base.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for i := 0; i < 5; i++ {
		var one int
		if err := pool.QueryRow(ctx, "select 1").Scan(&one); err != nil {
			t.Fatalf("pool query %d: %v", i, err)
		}
		time.Sleep(1200 * time.Millisecond)
	}
	t.Logf("pgxpool: 5 sequential queries used %d server connections (expected 1); pool.Ping: %v", ln.n.Load()-before, pool.Ping(ctx))
}
