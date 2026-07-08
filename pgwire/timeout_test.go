package pgwire_test

// timeout_test.go: the idle-in-transaction timeout. A writable
// transaction block holds the engine's single global writer lock, so
// a client that sends BEGIN and then goes silent would otherwise
// stall every other connection's writes forever — the server must
// terminate it (FATAL 25P03, as Postgres does) so the rollback frees
// the lock.

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"

	"github.com/rohanthewiz/bytdb/pgwire"
)

// startServerIdleTx is startServer with a configured
// idle-in-transaction timeout.
func startServerIdleTx(t *testing.T, timeout time.Duration) string {
	t.Helper()
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := pgwire.NewServer(bsql.New(e))
	srv.IdleTxTimeout = timeout
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		e.Close()
	})
	return fmt.Sprintf("postgres://test@%s/test", ln.Addr())
}

// TestIdleInTransactionTimeout is the DoS scenario end to end: conn A
// opens a block (taking the writer lock) and goes silent; conn B's
// write must not block past the timeout, and conn A must find its
// connection terminated.
func TestIdleInTransactionTimeout(t *testing.T) {
	ctx := context.Background()
	url := startServerIdleTx(t, 300*time.Millisecond)
	a := connect(t, url)
	b := connect(t, url)

	mustExec(t, a, `create table t (id int primary key, v int)`)
	// Simple protocol (no args) keeps the whole block on one round trip.
	mustExec(t, a, `begin`)
	mustExec(t, a, `insert into t values (1, 1)`)
	// A now sits idle inside the block, holding the writer lock.

	bctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := b.Exec(bctx, `insert into t values (2, 2)`); err != nil {
		t.Fatalf("conn B's insert should be released by the timeout, got: %v", err)
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("conn B waited %v; the idle timeout should have freed the lock far sooner", waited)
	}

	// Conn A was terminated; its next statement fails and its
	// half-done block is gone (rolled back).
	if _, err := a.Exec(ctx, `commit`); err == nil {
		t.Fatal("conn A should have been terminated by the idle-in-transaction timeout")
	}
	var one int
	if err := b.QueryRow(ctx, `select count(*) from t where id = 1`).Scan(&one); err != nil {
		t.Fatal(err)
	}
	if one != 0 {
		t.Fatalf("conn A's uncommitted insert survived its termination: count=%d", one)
	}
}

// TestIdleOutsideTransactionNotTerminated pins the negative space: the
// deadline only arms inside a block. A connection idle between
// statements outlives the timeout untouched, and one that keeps
// talking inside a block re-arms the deadline with each message.
func TestIdleOutsideTransactionNotTerminated(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServerIdleTx(t, 200*time.Millisecond))
	mustExec(t, c, `create table t (id int primary key)`)

	time.Sleep(600 * time.Millisecond) // idle, but not in a transaction
	mustExec(t, c, `insert into t values (1)`)

	// Active inside a block: each statement lands before the deadline.
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 4; i++ {
		time.Sleep(100 * time.Millisecond)
		if _, err := tx.Exec(ctx, `insert into t values ($1)`, i); err != nil {
			t.Fatalf("statement %d inside an active block: %v", i, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := c.QueryRow(ctx, `select count(*) from t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("want 4 rows, got %d", n)
	}
}
