package pgwire

// panic_test.go: the connection recover fence. A panic reachable from
// one connection's statement must cost only that connection — the
// server keeps serving every other client and accepting new ones.
// In-package (unlike server_test.go) to reach the testPanicQuery hook.

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

func TestConnPanicIsolatesConnection(t *testing.T) {
	// Arm the hook before the server goroutine starts.
	testPanicQuery.Store("select panic_now")
	defer testPanicQuery.Store("")

	e, err := bytdb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(bsql.New(e))
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		e.Close()
	})
	connString := fmt.Sprintf("postgres://test@%s/test", ln.Addr())

	ctx := context.Background()
	victim, err := pgx.Connect(ctx, connString)
	if err != nil {
		t.Fatal(err)
	}
	defer victim.Close(ctx)
	survivor, err := pgx.Connect(ctx, connString)
	if err != nil {
		t.Fatal(err)
	}
	defer survivor.Close(ctx)
	if _, err := survivor.Exec(ctx, `create table t (id int primary key)`); err != nil {
		t.Fatal(err)
	}

	// No-args Exec goes over the simple protocol, which is where the
	// hook fires. The victim connection must die with an error...
	if _, err := victim.Exec(ctx, "select panic_now"); err == nil {
		t.Fatal("panicking statement: expected an error")
	}

	// ...while an already-open connection still works...
	if _, err := survivor.Exec(ctx, `insert into t values (1)`); err != nil {
		t.Fatalf("existing connection broken after another conn's panic: %v", err)
	}

	// ...and brand-new connections are still accepted.
	fresh, err := pgx.Connect(ctx, connString)
	if err != nil {
		t.Fatalf("new connection refused after a panic: %v", err)
	}
	defer fresh.Close(ctx)
	var n int64
	if err := fresh.QueryRow(ctx, `select count(*) from t`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("count on fresh connection: n=%d err=%v", n, err)
	}
}
