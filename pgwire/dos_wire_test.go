package pgwire_test

// dos_wire_test.go: the DoS-hardening timeouts exercised end to end over
// a real socket — the opt-in idle-session timeout, and the write deadline
// that frees the engine's writer lock when a client stops reading mid
// result inside a transaction block.

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"

	"github.com/rohanthewiz/bytdb/pgwire"
)

// TestIdleSessionTimeout: with IdleTimeout set, a connection that sits
// idle between statements past the deadline is terminated (FATAL 57P05),
// while the server stays healthy for fresh connections.
func TestIdleSessionTimeout(t *testing.T) {
	addr := startConfiguredServer(t, func(s *pgwire.Server) {
		s.IdleTimeout = 300 * time.Millisecond
	})
	url := fmt.Sprintf("postgres://test@%s/test?sslmode=disable", addr)
	ctx := context.Background()

	c := connect(t, url)
	mustExec(t, c, `create table t (id int primary key)`) // session now idle
	time.Sleep(700 * time.Millisecond)                    // idle past the timeout

	if _, err := c.Exec(ctx, `insert into t values (1)`); err == nil {
		t.Fatal("idle session past the timeout should have been terminated")
	}

	// The server itself is unaffected: a new connection works.
	c2 := connect(t, url)
	mustExec(t, c2, `insert into t values (2)`)
}

// startWriteTimeoutServer serves a table pre-seeded with ~4 MB of rows
// and a short WriteTimeout, returning the listen address.
func startWriteTimeoutServer(t *testing.T, wt time.Duration) string {
	t.Helper()
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	db := bsql.New(e)

	// Seed enough result data that streaming it to a client that never
	// reads will fill the socket buffers and block the server mid-write.
	sess := db.NewSession()
	create, err := db.Prepare(`create table big (id int primary key, s text)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.ExecStmt(create); err != nil {
		t.Fatal(err)
	}
	ins, err := db.Prepare(`insert into big values ($1, $2)`)
	if err != nil {
		t.Fatal(err)
	}
	// One transaction around the whole seed keeps it to a single fsync
	// instead of 2000, so the test is not dominated by seeding.
	exec := func(sql string) {
		st, err := db.Prepare(sql)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sess.ExecStmt(st); err != nil {
			t.Fatal(err)
		}
	}
	exec(`begin`)
	wide := strings.Repeat("x", 2000)
	for i := range 2000 {
		if _, err := sess.ExecStmt(ins, int64(i), wide); err != nil {
			t.Fatal(err)
		}
	}
	exec(`commit`)
	sess.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := pgwire.NewServer(db)
	srv.WriteTimeout = wt
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		e.Close()
	})
	return ln.Addr().String()
}

// pgReadUntilReady consumes backend frames until ReadyForQuery ('Z').
func pgReadUntilReady(t *testing.T, r *bufio.Reader) {
	t.Helper()
	for {
		typ, err := r.ReadByte()
		if err != nil {
			t.Fatalf("read frame type: %v", err)
		}
		var lb [4]byte
		if _, err := io.ReadFull(r, lb[:]); err != nil {
			t.Fatalf("read frame length: %v", err)
		}
		n := int(binary.BigEndian.Uint32(lb[:])) - 4
		if _, err := io.ReadFull(r, make([]byte, n)); err != nil {
			t.Fatalf("read frame body: %v", err)
		}
		if typ == 'Z' { // ReadyForQuery
			return
		}
	}
}

// pgSimpleQuery frames one 'Q' (simple query) message.
func pgSimpleQuery(sql string) []byte {
	body := append([]byte(sql), 0)
	out := []byte{'Q'}
	out = binary.BigEndian.AppendUint32(out, uint32(len(body)+4))
	return append(out, body...)
}

// TestWriteDeadlineFreesWriterLock is the write-side counterpart of the
// idle-in-transaction test: a client opens a writable block (taking the
// engine's writer lock), issues a large query, and then stops reading.
// The server blocks writing the result with the lock held — a stall the
// read-side idle deadline cannot catch — so only the write deadline can
// free the lock. A second client's write must be released promptly.
func TestWriteDeadlineFreesWriterLock(t *testing.T) {
	addr := startWriteTimeoutServer(t, 500*time.Millisecond)

	// Client A: a raw socket with a tiny receive buffer, so the server's
	// send blocks after only a little data even on loopback.
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if tc, ok := nc.(*net.TCPConn); ok {
		tc.SetReadBuffer(4096)
	}

	// Startup: protocol 3.0, user + database, empty terminator.
	var body []byte
	body = binary.BigEndian.AppendUint32(body, 196608)
	for _, kv := range [][2]string{{"user", "test"}, {"database", "test"}} {
		body = append(append(body, kv[0]...), 0)
		body = append(append(body, kv[1]...), 0)
	}
	body = append(body, 0)
	pkt := binary.BigEndian.AppendUint32(nil, uint32(len(body)+4))
	pkt = append(pkt, body...)
	if _, err := nc.Write(pkt); err != nil {
		t.Fatal(err)
	}
	ar := bufio.NewReader(nc)
	pgReadUntilReady(t, ar) // startup complete

	// Take the writer lock: open a block and write a row.
	if _, err := nc.Write(pgSimpleQuery(`begin`)); err != nil {
		t.Fatal(err)
	}
	pgReadUntilReady(t, ar)
	if _, err := nc.Write(pgSimpleQuery(`insert into big values (999999, 'sentinel')`)); err != nil {
		t.Fatal(err)
	}
	pgReadUntilReady(t, ar)

	// Now issue the large query and never read the response. The server
	// starts streaming, fills the buffers, and blocks in Flush — with the
	// writer lock still held.
	if _, err := nc.Write(pgSimpleQuery(`select s from big`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // let the server begin (and block on) the write

	// Client B: its write must be freed once A's write deadline fires,
	// rolls back A's block, and releases the lock. If the lock had leaked,
	// this blocks until the context deadline and the test fails.
	b := connect(t, fmt.Sprintf("postgres://test@%s/test?sslmode=disable", addr))
	bctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := b.Exec(bctx, `insert into big values (1000000, 'ok')`); err != nil {
		t.Fatalf("client B's write should have been freed by A's write deadline: %v", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("client B waited %v; the write deadline should have freed the lock far sooner", waited)
	}
}
