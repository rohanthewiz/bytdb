package pgwire_test

// cancel_test.go: out-of-band query cancellation and the statement
// timeout, end to end through pgx. A context canceled client-side
// makes pgx send a real CancelRequest on a second connection — the
// server must find the target backend by PID+secret, cancel the
// running statement, and answer it with SQLSTATE 57014 so pgx can
// recover the connection instead of tearing it down.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// seedHeavy creates table n with enough rows that a triple self cross
// join (rows³ combinations) cannot finish before any sane timeout.
func seedHeavy(t *testing.T, c *pgx.Conn, rows int) {
	t.Helper()
	mustExec(t, c, `create table n (id int primary key, v int)`)
	var b strings.Builder
	b.WriteString("insert into n values ")
	for i := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "(%d, %d)", i, i%7)
	}
	mustExec(t, c, b.String())
}

const heavyQuery = `select count(*) from n a, n b, n c where a.v + b.v + c.v = 100`

func TestCancelRequest(t *testing.T) {
	url := startServer(t)
	c := connect(t, url)
	seedHeavy(t, c, 400)

	// Run the heavy query with an uncancellable client context, so the
	// only thing that can stop it is the server honoring the
	// out-of-band CancelRequest sent below on a second connection.
	errCh := make(chan error, 1)
	go func() {
		_, err := c.Exec(context.Background(), heavyQuery)
		errCh <- err
	}()

	time.Sleep(50 * time.Millisecond) // let the query reach the executor
	if err := c.PgConn().CancelRequest(context.Background()); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errCh:
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
			t.Fatalf("got %v, want SQLSTATE 57014", err)
		}
		if !strings.Contains(pgErr.Message, "user request") {
			t.Fatalf("message %q does not name the user request", pgErr.Message)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CancelRequest did not stop the query")
	}

	// Only the statement died: the connection must still serve queries.
	var count int
	if err := c.QueryRow(context.Background(), `select count(*) from n`).Scan(&count); err != nil {
		t.Fatalf("connection unusable after cancel: %v", err)
	}
	if count != 400 {
		t.Fatalf("got %d rows, want 400", count)
	}
}

func TestStatementTimeoutOverWire(t *testing.T) {
	url := startServer(t)
	c := connect(t, url)
	seedHeavy(t, c, 400)

	mustExec(t, c, `set statement_timeout = '50ms'`)
	_, err := c.Exec(context.Background(), heavyQuery)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("got %v, want SQLSTATE 57014", err)
	}
	if !strings.Contains(pgErr.Message, "statement timeout") {
		t.Fatalf("message %q does not name the statement timeout", pgErr.Message)
	}

	// The session survives, and clearing the timeout restores unbounded
	// execution for fast statements.
	mustExec(t, c, `set statement_timeout = 0`)
	var count int
	if err := c.QueryRow(context.Background(), `select count(*) from n where v = 3`).Scan(&count); err != nil {
		t.Fatal(err)
	}
}

func TestCancelWrongSecretIgnored(t *testing.T) {
	url := startServer(t)
	c := connect(t, url)
	seedHeavy(t, c, 30)

	// Forge a CancelRequest with the right PID but the wrong secret,
	// by hand over a plain socket: it must change nothing (and produce
	// no response, per the protocol).
	pid := c.PgConn().PID()
	addr := url[strings.Index(url, "@")+1:] // postgres://test@host:port/test
	addr = addr[:strings.Index(addr, "/")]
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	frame := make([]byte, 16)
	be := func(off int, v uint32) {
		frame[off] = byte(v >> 24)
		frame[off+1] = byte(v >> 16)
		frame[off+2] = byte(v >> 8)
		frame[off+3] = byte(v)
	}
	be(0, 16)
	be(4, 80877102) // CancelRequest code
	be(8, pid)
	be(12, 0xdeadbeef) // wrong secret
	if _, err := nc.Write(frame); err != nil {
		t.Fatal(err)
	}

	// The connection keeps working: nothing was canceled.
	time.Sleep(50 * time.Millisecond)
	var count int
	if err := c.QueryRow(context.Background(), `select count(*) from n`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 30 {
		t.Fatalf("got %d rows, want 30", count)
	}
}
