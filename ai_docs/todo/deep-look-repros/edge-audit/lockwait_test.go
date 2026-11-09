package edgeaudit

import (
	"context"
	"testing"
	"time"
)

// A writer queued behind another connection's open transaction block:
// can statement_timeout, a CancelRequest (pgx ctx cancel), or the queue
// holder's own disappearance get it out?
func TestLockWaitNotCancellable(t *testing.T) {
	addr, _, _ := startServer(t, nil)
	ctx := context.Background()
	a := pgxConn(t, addr)
	b := pgxConn(t, addr)
	a.Exec(ctx, "create table t (id int primary key)")
	a.Exec(ctx, "begin")
	a.Exec(ctx, "insert into t values (1)")

	if _, err := b.Exec(ctx, "set statement_timeout = 200"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	done := make(chan error, 1)
	cctx, cancel := context.WithTimeout(ctx, 600*time.Millisecond) // pgx sends CancelRequest at expiry
	defer cancel()
	go func() { _, err := b.Exec(cctx, "insert into t values (2)"); done <- err }()
	select {
	case err := <-done:
		t.Logf("returned after %v: %v", time.Since(start).Round(time.Millisecond), err)
	case <-time.After(1500 * time.Millisecond):
		t.Logf("after 1.5s: statement still waiting (statement_timeout=200ms and client ctx 600ms both expired)")
	}
	// Did the server actually stop the statement, or only the client give up?
	time.Sleep(200 * time.Millisecond)
	a.Exec(ctx, "commit")
	time.Sleep(300 * time.Millisecond)
	var n int
	a.QueryRow(ctx, "select count(*) from t where id = 2").Scan(&n)
	t.Logf("row 2 present after the holder committed: %v", n == 1)
	if n == 1 {
		t.Errorf("BUG: statement that the client was told timed out/canceled (or whose connection was dropped) executed anyway once the lock freed")
	}
}
