package pgwire_test

// Wire-level regression tests from the deep-look remediation plan.

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/pgwire"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

// TestParseErrorFailsBlock (D-07): a Parse (or Bind/Describe) error never
// reached the session, so the transaction block stayed healthy and COMMIT
// persisted everything before the typo. Postgres fails the block on any
// error. Covered in both protocols: pgx's tx.Exec goes through Parse; the
// simple protocol parses inside its Query loop.
func TestParseErrorFailsBlock(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create table b (id int primary key)`)

	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `insert into b values (100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `insrt into b values (101)`); err == nil {
		t.Fatal("typo parsed")
	}
	if err := tx.Commit(ctx); !errors.Is(err, pgx.ErrTxCommitRollback) {
		t.Fatalf("commit after a Parse error in the block: err = %v; want ErrTxCommitRollback", err)
	}

	mustExec(t, c, `begin`)
	mustExec(t, c, `insert into b values (200)`)
	if _, err := c.Exec(ctx, `insrt into b values (201)`, pgx.QueryExecModeSimpleProtocol); err == nil {
		t.Fatal("typo parsed")
	}
	if tag := mustExec(t, c, `commit`); tag.String() != "ROLLBACK" {
		t.Fatalf("simple-protocol COMMIT of a failed block: %q; want ROLLBACK", tag)
	}

	var n int64
	if err := c.QueryRow(ctx, `select count(*) from b`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d rows committed from failed blocks", n)
	}
}

// TestCommentOnlyQueries (D-08): pgx's liveness check sends "-- ping",
// which bytdb parsed as a syntax error, so every database/sql Ping
// failed with "driver: bad connection" and pools threw away healthy
// connections. A query of only comments is an empty statement in both
// protocols, and a trailing comment after the last statement is
// ignored rather than failing the batch after it ran.
func TestCommentOnlyQueries(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("pgx Ping: %v", err)
	}
	for _, mode := range []pgx.QueryExecMode{pgx.QueryExecModeSimpleProtocol, pgx.QueryExecModeExec, pgx.QueryExecModeDescribeExec} {
		for _, q := range []string{"-- ping", "/* nothing /* nested */ here */", "  ;  -- x"} {
			if _, err := c.Exec(ctx, q, mode); err != nil {
				t.Fatalf("mode %v: Exec(%q): %v", mode, q, err)
			}
		}
	}
	mustExec(t, c, `create table t (id int primary key)`)
	if _, err := c.Exec(ctx, "insert into t values (1); -- trailing", pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("statement with a trailing comment: %v", err)
	}
	var n int64
	if err := c.QueryRow(ctx, `select count(*) from t`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("count = %d, err = %v", n, err)
	}
}

// TestWriteDeadlineExcludesExecution (D-15a): the write deadline was
// armed when a message arrived, before the statement ran. A write that
// waited 900 ms for the writer lock under a 300 ms WriteTimeout then
// committed, and its reply hit the long-expired deadline: the client
// saw a dropped connection for a write that had committed.
func TestWriteDeadlineExcludesExecution(t *testing.T) {
	ctx := context.Background()
	addr := startConfiguredServer(t, func(s *pgwire.Server) { s.WriteTimeout = 300 * time.Millisecond })
	url := fmt.Sprintf("postgres://test@%s/test?sslmode=disable", addr)
	a, b := connect(t, url), connect(t, url)
	mustExec(t, a, `create table t (id int primary key)`)
	mustExec(t, a, `begin`)
	mustExec(t, a, `insert into t values (1)`)
	done := make(chan error, 1)
	go func() {
		_, err := b.Exec(ctx, `insert into t values (2)`)
		done <- err
	}()
	time.Sleep(900 * time.Millisecond) // b waits on the writer lock for 3x WriteTimeout
	mustExec(t, a, `commit`)
	if err := <-done; err != nil {
		t.Fatalf("a write that waited for the lock and committed reported: %v", err)
	}
	var n int64
	if err := b.QueryRow(ctx, `select count(*) from t`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("count = %d, err = %v", n, err)
	}
}

// TestWriteDeadlineBoundsStallNotTransfer (D-15b): one deadline covered
// a whole reply, so a client reading a large result steadily was cut
// off once the transfer outran WriteTimeout. The deadline now bounds a
// stall: here ~4 MB arrives at ~3 MB/s against a 300 ms WriteTimeout.
// (TestWriteDeadlineFreesWriterLock covers a reader that stops.)
func TestWriteDeadlineBoundsStallNotTransfer(t *testing.T) {
	addr := startWriteTimeoutServer(t, 300*time.Millisecond)
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	var body []byte
	body = binary.BigEndian.AppendUint32(body, 196608)
	body = append(body, "user\x00test\x00\x00"...)
	if _, err := nc.Write(append(binary.BigEndian.AppendUint32(nil, uint32(len(body)+4)), body...)); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReaderSize(nc, 64<<10)
	pgReadUntilReady(t, r)
	if _, err := nc.Write(pgSimpleQuery(`select s from big`)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	total := 0
	buf := make([]byte, 64<<10)
	var tail []byte
	for {
		n, err := r.Read(buf)
		total += n
		tail = append(tail, buf[:n]...)
		if len(tail) > 6 {
			tail = tail[len(tail)-6:]
		}
		if string(tail) == "Z\x00\x00\x00\x05I" {
			break // ReadyForQuery: the whole result arrived
		}
		if err != nil {
			t.Fatalf("stream cut after %v and %d bytes: %v", time.Since(start), total, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if took := time.Since(start); took < 300*time.Millisecond {
		t.Fatalf("transfer took %v; the test needs it to outlast WriteTimeout", took)
	}
}

// flakyListener returns a temporary error from its first n Accepts.
type flakyListener struct {
	net.Listener
	failures atomic.Int32
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.failures.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}
	}
	return l.Listener.Accept()
}

// TestServeRetriesTemporaryAcceptErrors (D-14): Serve returned on the
// first accept error of any kind, so a transient EMFILE stopped bytdbd
// from accepting ever again while the process kept running.
func TestServeRetriesTemporaryAcceptErrors(t *testing.T) {
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fl := &flakyListener{Listener: ln}
	fl.failures.Store(3)
	srv := pgwire.NewServer(bsql.New(e))
	served := make(chan error, 1)
	go func() { served <- srv.Serve(fl) }()
	t.Cleanup(func() {
		srv.Close()
		e.Close()
	})
	c := connect(t, fmt.Sprintf("postgres://test@%s/test", ln.Addr()))
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("server stopped accepting after temporary errors: %v", err)
	}
	select {
	case err := <-served:
		t.Fatalf("Serve returned early: %v", err)
	default:
	}
}

// TestHardConnectionCeiling (D-14): connections over MaxConns are
// accepted so they can be refused with 53300, which left file
// descriptors unbounded under a connect flood. Past twice MaxConns they
// are now closed at once, before any handshake.
func TestHardConnectionCeiling(t *testing.T) {
	addr := startConfiguredServer(t, func(s *pgwire.Server) { s.MaxConns = 2 })
	// Four silent connections fill MaxConns (2) and the refusal band (2
	// more, held open in their startup handshake).
	var held []net.Conn
	for range 4 {
		nc, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, nc)
	}
	defer func() {
		for _, nc := range held {
			nc.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond) // let the server register them
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := nc.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("connection past the hard ceiling: read err = %v, want EOF (closed at once)", err)
	}
}

// TestPreparedStatementAfterSchemaChange (D-16): a prepared statement's
// result description was fixed at Parse. After another connection's
// ALTER COLUMN n TYPE text the client decoded text bytes as int8, and
// after ADD COLUMN a SELECT * indexed past the described types and the
// connection died in the panic fence. Execute now refuses with 0A000
// "cached plan must not change result type", which pgx answers by
// re-preparing, so the retry sees the new shape.
func TestPreparedStatementAfterSchemaChange(t *testing.T) {
	ctx := context.Background()
	url := startServer(t)
	a, b := connect(t, url), connect(t, url)
	mustExec(t, a, `create table t (id int primary key, n int)`)
	mustExec(t, a, `insert into t values (1, 4702111234474983745)`)

	var n int64
	if err := a.QueryRow(ctx, `select n from t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	mustExec(t, b, `alter table t alter column n type text using n::text`)

	// The first run after the change may fail with 0A000 (pgx then drops
	// its cached statement); it must never deliver misdecoded bytes.
	var s string
	err := a.QueryRow(ctx, `select n from t`).Scan(&s)
	if err != nil {
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "0A000" {
			t.Fatalf("after ALTER TYPE: %v; want success or 0A000", err)
		}
		if err := a.QueryRow(ctx, `select n from t`).Scan(&s); err != nil {
			t.Fatalf("retry after 0A000: %v", err)
		}
	}
	if s != "4702111234474983745" {
		t.Fatalf("after ALTER TYPE: n = %q", s)
	}

	// SELECT * across ADD COLUMN.
	countCols := func() (int, error) {
		rows, err := a.Query(ctx, `select * from t`)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		cols := len(rows.FieldDescriptions())
		for rows.Next() {
		}
		return cols, rows.Err()
	}
	if _, err := countCols(); err != nil {
		t.Fatal(err)
	}
	mustExec(t, b, `alter table t add column extra int default 5`)
	cols, err := countCols()
	if err != nil {
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "0A000" {
			t.Fatalf("after ADD COLUMN: %v; want success or 0A000", err)
		}
		cols, err = countCols()
	}
	if err != nil || cols != 3 {
		t.Fatalf("after ADD COLUMN: %d columns, err %v; want 3", cols, err)
	}
	if a.IsClosed() {
		t.Fatal("the schema change on another connection closed this one")
	}
}

// TestTypedParamsAndResults (D-23, D-25): pgx encodes arguments by the
// parameter types the server describes and scans results by the column
// types it describes. Placeholders in IN lists, under casts, in
// arithmetic and subqueries were described as text, so pgx refused to
// encode an int into them ("unable to encode 7 into text format");
// coalesce/nullif/CASE/scalar subqueries/exists were described as text,
// so pgx could not scan them into numbers or booleans; and temporal
// values compared or cast inside expressions misbehaved.
func TestTypedParamsAndResults(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create table t (id int primary key, i int, u uuid, ts timestamp, d date, s text)`)
	mustExec(t, c, `insert into t values (1, 10, 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', '2024-02-29 12:00:00', '2024-02-29', 'x')`)
	mustExec(t, c, `insert into t values (2, null, null, null, null, null)`)

	ints := []struct {
		q    string
		args []any
		want int64
	}{
		{`select count(*) from t where id in ($1, $2)`, []any{1, 2}, 2},
		{`select count(*) from t where u in ($1)`, []any{"a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"}, 1},
		{`select count(*) from t where ts in ($1)`, []any{time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC)}, 1},
		{`select count(*) from t where d in ($1)`, []any{time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)}, 1},
		{`select count(*) from t where id in (select id from t where i = $1)`, []any{10}, 1},
		{`select coalesce(i, 0) from t where id = 2`, nil, 0},
		{`select coalesce(sum(i), 0) from t`, nil, 10},
		{`select (select max(id) from t)`, nil, 2},
		{`select case when id = 9 then null else i end from t where id = 1`, nil, 10},
		{`select nullif(i, 0) from t where id = 1`, nil, 10},
		{`select $1::int + 1`, []any{5}, 6},
		{`select i + $1 from t where id = 1`, []any{5}, 15},
		{`select d - d from t where id = 1`, nil, 0},
	}
	for _, c2 := range ints {
		var n int64
		if err := c.QueryRow(ctx, c2.q, c2.args...).Scan(&n); err != nil || n != c2.want {
			t.Errorf("%s %v: n=%d err=%v; want %d", c2.q, c2.args, n, err, c2.want)
		}
	}
	var b bool
	for _, q := range []string{`select exists(select 1 from t where id = 1)`, `select 1::bool`} {
		if err := c.QueryRow(ctx, q).Scan(&b); err != nil || !b {
			t.Errorf("%s: %v %v", q, b, err)
		}
	}
	var tm time.Time
	for q, want := range map[string]time.Time{
		`select ts::date from t where id = 1`: time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC),
		`select d + 1 from t where id = 1`:    time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
	} {
		if err := c.QueryRow(ctx, q).Scan(&tm); err != nil || !tm.Equal(want) {
			t.Errorf("%s: %v %v; want %v", q, tm, err, want)
		}
	}
	var s string
	for q, want := range map[string]string{
		`select u::text from t where id = 1`:   "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11",
		`select 'd=' || d from t where id = 1`: "d=2024-02-29",
	} {
		if err := c.QueryRow(ctx, q).Scan(&s); err != nil || s != want {
			t.Errorf("%s: %q %v; want %q", q, s, err, want)
		}
	}
	var f float64
	if err := c.QueryRow(ctx, `select 1000000.0::text::float`).Scan(&f); err != nil || f != 1e6 {
		t.Errorf("1000000.0::text::float = %v, %v", f, err)
	}
	// A placeholder with no context is described as unknown (OID 0), so
	// pgx sends it as text instead of refusing to encode it.
	if err := c.QueryRow(ctx, `select $1`, 7).Scan(&s); err != nil || s != "7" {
		t.Errorf("select $1 with an int argument: %q %v", s, err)
	}
}

// TestImplicitTransactions (D-32): several statements in one simple
// Query, or one pgx.Batch pipeline, run as one implicit transaction: a
// failure rolls back the writes before it. Each statement used to
// autocommit, so "update ...; insert <duplicate>" kept the update.
func TestImplicitTransactions(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create table acct (id int primary key, bal int)`)
	mustExec(t, c, `insert into acct values (1, 100), (2, 0)`)
	balance := func(id int) int64 {
		t.Helper()
		var b int64
		if err := c.QueryRow(ctx, `select bal from acct where id = $1`, id).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}

	// Simple protocol: the failing INSERT rolls the UPDATE back.
	if _, err := c.Exec(ctx, `update acct set bal = bal - 10 where id = 1; insert into acct values (2, 0)`,
		pgx.QueryExecModeSimpleProtocol); err == nil {
		t.Fatal("duplicate insert succeeded")
	}
	if b := balance(1); b != 100 {
		t.Fatalf("simple query: balance %d after a failed batch; want 100", b)
	}

	// pgx.Batch: the same through the extended protocol.
	b := &pgx.Batch{}
	b.Queue(`update acct set bal = bal - 10 where id = 1`)
	b.Queue(`update acct set bal = bal + 10 where id = 2`)
	b.Queue(`insert into acct values (1, 0)`)
	if err := c.SendBatch(ctx, b).Close(); err == nil {
		t.Fatal("batch with a duplicate insert succeeded")
	}
	if b1, b2 := balance(1), balance(2); b1 != 100 || b2 != 0 {
		t.Fatalf("batch: balances %d, %d after a failed batch; want 100, 0", b1, b2)
	}

	// A successful batch commits as a whole.
	b = &pgx.Batch{}
	b.Queue(`update acct set bal = bal - 10 where id = 1`)
	b.Queue(`update acct set bal = bal + 10 where id = 2`)
	if err := c.SendBatch(ctx, b).Close(); err != nil {
		t.Fatal(err)
	}
	if b1, b2 := balance(1), balance(2); b1 != 90 || b2 != 10 {
		t.Fatalf("batch: balances %d, %d; want 90, 10", b1, b2)
	}

	// COMMIT inside a multi-statement query ends the implicit block: the
	// write before it stays, the one after the failure does not.
	if _, err := c.Exec(ctx, `update acct set bal = 1 where id = 1; commit; update acct set bal = 2 where id = 2; select 1/0`,
		pgx.QueryExecModeSimpleProtocol); err == nil {
		t.Fatal("division by zero succeeded")
	}
	if b1, b2 := balance(1), balance(2); b1 != 1 || b2 != 10 {
		t.Fatalf("after COMMIT mid-query: balances %d, %d; want 1, 10", b1, b2)
	}

	// DDL cannot join a block: it commits the writes before it and runs
	// alone (the documented carve-out), and the query still succeeds.
	if _, err := c.Exec(ctx, `update acct set bal = 5 where id = 1; create table t2 (id int primary key); insert into t2 values (1)`,
		pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("query mixing DDL: %v", err)
	}
	var n int64
	if err := c.QueryRow(ctx, `select count(*) from t2`).Scan(&n); err != nil || n != 1 || balance(1) != 5 {
		t.Fatalf("after the DDL batch: t2 rows %d (%v), balance %d", n, err, balance(1))
	}

	// The connection is idle again after every unit.
	if tag := mustExec(t, c, `select 1`); tag.String() != "SELECT 1" {
		t.Fatalf("follow-up statement: %q", tag)
	}
	if st := c.PgConn().TxStatus(); st != 'I' {
		t.Fatalf("transaction status after the units: %c; want I", st)
	}
}

// startShutdownServer serves a database with a table big enough that a
// cross join of it runs for a while, and returns the server and address.
func startShutdownServer(t *testing.T, rows int) (*pgwire.Server, string) {
	t.Helper()
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "test.db"), bytdb.WithSyncNever())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTable("big", []bytdb.Column{{Name: "id", Type: bytdb.TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.WriteTxn(func(tx *bytdb.Txn) error {
		for i := range rows {
			if err := tx.Insert("big", i); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := pgwire.NewServer(bsql.New(e))
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		e.Close()
	})
	return srv, fmt.Sprintf("postgres://test@%s/test", ln.Addr())
}

// TestGracefulShutdown (D-35): Close dropped every socket and returned
// at once, so bytdbd closed the engine under running statements.
// Shutdown ends idle sessions with FATAL 57P01, lets a running statement
// finish and deliver its result, and returns only when every connection
// is gone.
func TestGracefulShutdown(t *testing.T) {
	ctx := context.Background()
	srv, url := startShutdownServer(t, 1500)
	busy, idle := connect(t, url), connect(t, url)
	if err := idle.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	type result struct {
		n   int64
		err error
	}
	got := make(chan result, 1)
	go func() {
		var n int64
		err := busy.QueryRow(ctx, `select count(*) from big a, big b`).Scan(&n)
		got <- result{n, err}
	}()
	time.Sleep(100 * time.Millisecond) // let the join start
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	r := <-got
	if r.err != nil || r.n != 1500*1500 {
		t.Fatalf("the running statement: n=%d err=%v; want its full result", r.n, r.err)
	}
	var pe *pgconn.PgError
	if err := idle.Ping(ctx); err == nil {
		t.Fatal("the idle session survived Shutdown")
	} else if errors.As(err, &pe) && pe.Code != "57P01" {
		t.Fatalf("idle session error %v; want 57P01", err)
	}
	if n := srv.ConnCount(); n != 0 {
		t.Fatalf("%d connections left after Shutdown", n)
	}
}

// TestShutdownGraceExpires (D-35): when the grace period ends first, a
// running statement is canceled and Shutdown still waits for it, then
// reports the deadline.
func TestShutdownGraceExpires(t *testing.T) {
	ctx := context.Background()
	srv, url := startShutdownServer(t, 4000)
	busy := connect(t, url)
	got := make(chan error, 1)
	go func() {
		var n int64
		got <- busy.QueryRow(ctx, `select count(*) from big a, big b`).Scan(&n)
	}()
	time.Sleep(100 * time.Millisecond)
	sctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := srv.Shutdown(sctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown: %v; want the deadline", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("Shutdown took %v after its grace period", took)
	}
	if err := <-got; err == nil {
		t.Fatal("the canceled statement reported success")
	}
	if n := srv.ConnCount(); n != 0 {
		t.Fatalf("%d connections left after Shutdown", n)
	}
}
