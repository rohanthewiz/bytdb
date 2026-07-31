package pgwire_test

// serializable_test.go: the concurrent-writes surface over the wire
// (Stage 4 of the OCC plan). An engine opened WithConcurrentWrites
// lets transaction blocks on different connections overlap; a losing
// commit must reach the client as SQLSTATE 40001 with Postgres's
// message and retry hint, SHOW must describe the real isolation, and
// autocommit statements absorb the server's internal retries.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"

	"github.com/rohanthewiz/bytdb/pgwire"
)

// startServerOCC is startServer over an engine in concurrent-writes
// mode — the only mode in which commits can lose and 40001 can occur.
func startServerOCC(t *testing.T) string {
	t.Helper()
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "occ.db"),
		bytdb.WithConcurrentWrites())
	if err != nil {
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
	return fmt.Sprintf("postgres://test@%s/test", ln.Addr())
}

// wantConflict asserts err is the canonical serialization failure:
// code, message, and hint are all part of the contract — pools and
// retry middleware match on each.
func wantConflict(t *testing.T, err error) {
	t.Helper()
	var pge *pgconn.PgError
	if !errors.As(err, &pge) {
		t.Fatalf("want PgError, got %v", err)
	}
	if pge.Code != "40001" {
		t.Fatalf("SQLSTATE = %s (%s); want 40001", pge.Code, pge.Message)
	}
	if pge.Message != "could not serialize access due to concurrent update" {
		t.Fatalf("message = %q", pge.Message)
	}
	if pge.Hint != "The transaction might succeed if retried." {
		t.Fatalf("hint = %q", pge.Hint)
	}
}

// Two blocks writing the same row: the second COMMIT loses validation
// and the client sees 40001. Explicit blocks are never retried
// server-side — the failing COMMIT surfaces immediately.
func TestWireWriteConflict40001(t *testing.T) {
	ctx := context.Background()
	cs := startServerOCC(t)
	c1, c2 := connect(t, cs), connect(t, cs)

	mustExec(t, c1, `create table t (id int primary key, v int)`)
	mustExec(t, c1, `insert into t values (1, 0)`)

	// Raw control statements rather than pgx.Begin: both blocks must be
	// open at once, which only an OCC engine allows without blocking.
	mustExec(t, c1, `begin`)
	mustExec(t, c1, `update t set v = 1 where id = 1`)
	mustExec(t, c2, `begin`)
	mustExec(t, c2, `update t set v = 2 where id = 1`)
	mustExec(t, c1, `commit`)
	_, err := c2.Exec(ctx, `commit`)
	wantConflict(t, err)
	// The failed COMMIT ended the block: back to idle, and c2 can win
	// the retry it was hinted to make.
	if st := c2.PgConn().TxStatus(); st != 'I' {
		t.Fatalf("TxStatus after failed commit = %c; want I", st)
	}
	mustExec(t, c2, `begin`)
	mustExec(t, c2, `update t set v = 2 where id = 1`)
	mustExec(t, c2, `commit`)
	var v int64
	if err := c1.QueryRow(ctx, `select v from t where id = 1`).Scan(&v); err != nil || v != 2 {
		t.Fatalf("v = %d err = %v; want 2", v, err)
	}
}

// The full serializable story over the wire: plain blocks write-skew
// (snapshot isolation), BEGIN ISOLATION LEVEL SERIALIZABLE conflicts
// instead.
func TestWireSerializableWriteSkew(t *testing.T) {
	ctx := context.Background()
	cs := startServerOCC(t)
	c1, c2 := connect(t, cs), connect(t, cs)

	mustExec(t, c1, `create table skew (id int primary key, v int)`)
	mustExec(t, c1, `insert into skew values (1, 1), (2, 1)`)

	arm := func(c *pgx.Conn, begin string, readID, writeID int) error {
		if _, err := c.Exec(ctx, begin); err != nil {
			return err
		}
		var v int64
		if err := c.QueryRow(ctx,
			fmt.Sprintf(`select v from skew where id = %d`, readID)).Scan(&v); err != nil {
			return err
		}
		_, err := c.Exec(ctx, fmt.Sprintf(`update skew set v = 0 where id = %d`, writeID))
		return err
	}

	// Snapshot-isolation baseline: both commit; the invariant breaks.
	if err := arm(c1, `begin`, 2, 1); err != nil {
		t.Fatal(err)
	}
	if err := arm(c2, `begin`, 1, 2); err != nil {
		t.Fatal(err)
	}
	mustExec(t, c1, `commit`)
	mustExec(t, c2, `commit`)
	var sum int64
	if err := c1.QueryRow(ctx, `select sum(v) from skew`).Scan(&sum); err != nil || sum != 0 {
		t.Fatalf("SI sum = %d err = %v; want 0 (skew expected)", sum, err)
	}

	// Serializable: the second committer's read set catches the skew.
	mustExec(t, c1, `update skew set v = 1 where id = 1`)
	mustExec(t, c1, `update skew set v = 1 where id = 2`)
	if err := arm(c1, `begin isolation level serializable`, 2, 1); err != nil {
		t.Fatal(err)
	}
	if err := arm(c2, `begin isolation level serializable`, 1, 2); err != nil {
		t.Fatal(err)
	}
	mustExec(t, c1, `commit`)
	_, err := c2.Exec(ctx, `commit`)
	wantConflict(t, err)
	if err := c1.QueryRow(ctx, `select sum(v) from skew`).Scan(&sum); err != nil || sum != 1 {
		t.Fatalf("serializable sum = %d err = %v; want 1", sum, err)
	}
}

// SHOW reports the engine's real levels over the wire, and a late SET
// TRANSACTION gets Postgres's 25001.
func TestWireIsolationReporting(t *testing.T) {
	ctx := context.Background()
	cs := startServerOCC(t)
	c := connect(t, cs)

	mustExec(t, c, `create table t (id int primary key)`)

	var lvl string
	if err := c.QueryRow(ctx, `show transaction isolation level`).Scan(&lvl); err != nil ||
		lvl != "repeatable read" {
		t.Fatalf("occ default level = %q err = %v", lvl, err)
	}

	mustExec(t, c, `set session characteristics as transaction isolation level serializable`)
	if err := c.QueryRow(ctx, `show default_transaction_isolation`).Scan(&lvl); err != nil ||
		lvl != "serializable" {
		t.Fatalf("default after characteristics = %q err = %v", lvl, err)
	}

	// SET TRANSACTION after the block's first query: 25001, and the
	// block is failed until rollback.
	mustExec(t, c, `begin`)
	if _, err := c.Exec(ctx, `select count(*) from t`); err != nil {
		t.Fatal(err)
	}
	_, err := c.Exec(ctx, `set transaction isolation level serializable`)
	var pge *pgconn.PgError
	if !errors.As(err, &pge) || pge.Code != "25001" {
		t.Fatalf("late SET TRANSACTION: %v; want 25001", err)
	}
	mustExec(t, c, `rollback`)
}

// Concurrent autocommit increments through the wire: the server's
// internal retries plus client re-runs on 40001 must conserve every
// increment exactly once.
func TestWireAutocommitContention(t *testing.T) {
	ctx := context.Background()
	cs := startServerOCC(t)
	c0 := connect(t, cs)
	mustExec(t, c0, `create table c (id int primary key, v int)`)
	mustExec(t, c0, `insert into c values (1, 0)`)

	const workers, perWorker = 4, 20
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		c := connect(t, cs)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for done := 0; done < perWorker; {
				_, err := c.Exec(ctx, `update c set v = v + 1 where id = 1`)
				if err == nil {
					done++
					continue
				}
				var pge *pgconn.PgError
				if errors.As(err, &pge) && pge.Code == "40001" {
					continue // the hinted client-side retry
				}
				errs <- err
				return
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var v int64
	if err := c0.QueryRow(ctx, `select v from c where id = 1`).Scan(&v); err != nil ||
		v != workers*perWorker {
		t.Fatalf("v = %d err = %v; want %d", v, err, workers*perWorker)
	}
}
