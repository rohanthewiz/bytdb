package edgeaudit

import (
	"context"
	"testing"
	"time"

	"github.com/rohanthewiz/bytdb/pgwire"
)

// A statement that WAITS (here: behind another connection's writer lock)
// longer than WriteTimeout: is its reply still deliverable?
func TestWriteDeadlineCoversExecution(t *testing.T) { runWD(t, 300*time.Millisecond) }
func TestWriteDeadlineControl(t *testing.T) { runWD(t, -1) }
func runWD(t *testing.T, wt time.Duration) {
	addr, _, _ := startServer(t, func(s *pgwire.Server) { s.WriteTimeout = wt })
	ctx := context.Background()
	a := pgxConn(t, addr)
	b := pgxConn(t, addr)
	if _, err := a.Exec(ctx, "create table t (id int primary key, v text)"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(ctx, "begin"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(ctx, "insert into t values (1,'a')"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := b.Exec(ctx, "insert into t values (2,'b')")
		done <- err
	}()
	time.Sleep(900 * time.Millisecond) // b waits on the writer lock for 3x WriteTimeout
	if _, err := a.Exec(ctx, "commit"); err != nil {
		t.Fatal(err)
	}
	err := <-done
	t.Logf("b's INSERT result as seen by client: %v", err)
	var n int
	if e2 := a.QueryRow(ctx, "select count(*) from t where id=2").Scan(&n); e2 != nil {
		t.Fatal(e2)
	}
	t.Logf("row 2 present in table: %v (count=%d)", n == 1, n)
	if err != nil && n == 1 {
		t.Errorf("BUG: client got an error/connection loss for a statement that COMMITTED")
	}
}
