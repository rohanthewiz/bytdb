package edgeaudit

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/pgwire"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

// A driver caches a prepared statement (pgx does by default); another
// connection then changes the table. Postgres answers the next Execute
// with 0A000 "cached plan must not change result type", which drivers
// handle by re-preparing. What does bytdb send?
func TestCachedStatementAfterDDL(t *testing.T) {
	addr, _, _ := startServer(t, nil)
	ctx := context.Background()
	a := pgxConn(t, addr)
	b := pgxConn(t, addr)
	a.Exec(ctx, `create table t (id int primary key, n int, s text)`)
	a.Exec(ctx, `insert into t values (1, 4702111234474983745, 'x')`)

	read := func(label string) {
		rows, err := a.Query(ctx, `select * from t`)
		if err != nil {
			t.Logf("%s: query error: %v", label, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			v, err := rows.Values()
			t.Logf("%s: row=%v err=%v", label, v, err)
		}
		if rows.Err() != nil {
			t.Logf("%s: rows error: %v", label, rows.Err())
		}
	}
	read("before DDL")

	// (1) type change under a cached statement.
	if _, err := b.Exec(ctx, `alter table t alter column n type text using n::text`); err != nil {
		t.Logf("alter type: %v", err)
	}
	read("after ALTER COLUMN n TYPE text (client still believes int8)")

	// (2) column added under a cached statement.
	if _, err := b.Exec(ctx, `alter table t add column extra int`); err != nil {
		t.Fatal(err)
	}
	read("after ADD COLUMN")
	t.Logf("connection a closed by server: %v", a.IsClosed())
	if a.IsClosed() {
		t.Errorf("BUG: a schema change on another connection killed this connection (server panic fence) instead of a clean 0A000")
	}
}

// One transient Accept failure (EMFILE under an fd spike is the classic).
type flakyLn struct {
	net.Listener
	failed bool
}

func (l *flakyLn) Accept() (net.Conn, error) {
	if !l.failed {
		l.failed = true
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}
	}
	return l.Listener.Accept()
}

func TestServeSurvivesTransientAcceptError(t *testing.T) {
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := pgwire.NewServer(bsql.New(e))
	defer srv.Close()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(&flakyLn{Listener: ln}) }()
	select {
	case err := <-done:
		var ne net.Error
		t.Errorf("BUG: Serve returned on a transient accept error (bytdbd then exits): %v (temporary=%v)", err, errors.As(err, &ne) && ne.Temporary())
	case <-time.After(500 * time.Millisecond):
		c, err := pgx.Connect(context.Background(), dsn(ln.Addr().String()))
		if err != nil {
			t.Fatal(err)
		}
		c.Close(context.Background())
	}
}
