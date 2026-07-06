package pgwire_test

// Transaction blocks over the wire: BEGIN/COMMIT/ROLLBACK through
// pgx and database/sql, ReadyForQuery transaction status, the failed
// block state, and the notices Postgres sends for redundant control
// statements.

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestTransactionBlocks(t *testing.T) {
	ctx := context.Background()
	cs := startServer(t)
	c := connect(t, cs)

	mustExec(t, c, `create table t (id int primary key, v text)`)

	// Commit path, through pgx.Tx.
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st := c.PgConn().TxStatus(); st != 'T' {
		t.Fatalf("TxStatus after begin = %c; want T", st)
	}
	if _, err := tx.Exec(ctx, `insert into t values (1, 'a')`); err != nil {
		t.Fatal(err)
	}
	// A second connection must not see the uncommitted row (and must
	// not block: it reads a snapshot).
	c2 := connect(t, cs)
	var n int64
	if err := c2.QueryRow(ctx, `select count(*) from t`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("uncommitted visible elsewhere: n=%d err=%v", n, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if st := c.PgConn().TxStatus(); st != 'I' {
		t.Fatalf("TxStatus after commit = %c; want I", st)
	}
	if err := c2.QueryRow(ctx, `select count(*) from t`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("committed row not visible: n=%d err=%v", n, err)
	}

	// Rollback path.
	tx, err = c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `insert into t values (2, 'b')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(ctx, `select count(*) from t`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rollback leaked: n=%d err=%v", n, err)
	}
}

func TestTransactionFailedBlock(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create table t (id int primary key)`)
	mustExec(t, c, `insert into t values (1)`)

	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `insert into t values (2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `insert into t values (1)`); err == nil {
		t.Fatal("duplicate insert succeeded")
	}
	if st := c.PgConn().TxStatus(); st != 'E' {
		t.Fatalf("TxStatus after error = %c; want E", st)
	}
	// Further statements are refused with 25P02.
	var pgErr *pgconn.PgError
	if _, err := tx.Exec(ctx, `select 1`); !errors.As(err, &pgErr) || pgErr.Code != "25P02" {
		t.Fatalf("in-failed-block err = %v; want SQLSTATE 25P02", err)
	}
	// COMMIT resolves to ROLLBACK; pgx reports that distinctly.
	if err := tx.Commit(ctx); !errors.Is(err, pgx.ErrTxCommitRollback) {
		t.Fatalf("commit of failed block = %v; want ErrTxCommitRollback", err)
	}
	if st := c.PgConn().TxStatus(); st != 'I' {
		t.Fatalf("TxStatus after resolving = %c; want I", st)
	}
	// Nothing from the block survived, and the connection still works.
	var n int64
	if err := c.QueryRow(ctx, `select count(*) from t`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("failed block leaked: n=%d err=%v", n, err)
	}
}

func TestTransactionNotices(t *testing.T) {
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(startServer(t))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var notices []string
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		mu.Lock()
		notices = append(notices, n.Severity+" "+n.Code+" "+n.Message)
		mu.Unlock()
	}
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)

	// COMMIT outside a block warns but succeeds with its normal tag.
	tag, err := c.Exec(ctx, `commit`)
	if err != nil || tag.String() != "COMMIT" {
		t.Fatalf("stray commit: tag=%q err=%v", tag, err)
	}
	mustExec(t, c, `begin`)
	mustExec(t, c, `begin`) // redundant: warns, stays in the block
	if st := c.PgConn().TxStatus(); st != 'T' {
		t.Fatalf("TxStatus = %c; want T", st)
	}
	mustExec(t, c, `rollback`)

	mu.Lock()
	defer mu.Unlock()
	if len(notices) != 2 ||
		notices[0] != "WARNING 25P01 there is no transaction in progress" ||
		notices[1] != "WARNING 25001 there is already a transaction in progress" {
		t.Fatalf("notices = %q", notices)
	}
}

func TestTransactionDatabaseSQL(t *testing.T) {
	db, err := sql.Open("pgx", startServer(t)+"?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`create table t (id int primary key, v text)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`insert into t values (1, 'a')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`insert into t values (2, 'b')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`select count(*) from t`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("count = %d err = %v; want 1", n, err)
	}
}

// TestTransactionDisconnectReleases: a client that drops mid-block
// must not wedge the engine — its rollback frees the writer lock.
func TestTransactionDisconnectReleases(t *testing.T) {
	ctx := context.Background()
	cs := startServer(t)
	c := connect(t, cs)
	mustExec(t, c, `create table t (id int primary key)`)

	c2 := connect(t, cs)
	mustExec(t, c2, `begin`)
	mustExec(t, c2, `insert into t values (1)`)
	c2.Close(ctx) // Terminate with the block open

	// The held writer lock must release, letting this write proceed.
	mustExec(t, c, `insert into t values (2)`)
	var n int64
	if err := c.QueryRow(ctx, `select count(*) from t`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d err = %v; want only the post-disconnect row", n, err)
	}
}

func TestTransactionSavepoints(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create table t (id int primary key)`)
	mustExec(t, c, `insert into t values (1)`)

	// Plain savepoint flow with the Postgres command tags.
	mustExec(t, c, `begin`)
	if tag, err := c.Exec(ctx, `savepoint sp`); err != nil || tag.String() != "SAVEPOINT" {
		t.Fatalf("savepoint: tag=%q err=%v", tag, err)
	}
	mustExec(t, c, `insert into t values (2)`)
	if tag, err := c.Exec(ctx, `rollback to savepoint sp`); err != nil || tag.String() != "ROLLBACK" {
		t.Fatalf("rollback to: tag=%q err=%v", tag, err)
	}
	if st := c.PgConn().TxStatus(); st != 'T' {
		t.Fatalf("TxStatus after rollback to = %c; want T", st)
	}
	if tag, err := c.Exec(ctx, `release savepoint sp`); err != nil || tag.String() != "RELEASE" {
		t.Fatalf("release: tag=%q err=%v", tag, err)
	}
	mustExec(t, c, `commit`)
	var n int64
	if err := c.QueryRow(ctx, `select count(*) from t`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("count = %d err = %v; want 1", n, err)
	}

	// ROLLBACK TO recovers a failed block: 'E' back to 'T', and the
	// block commits what preceded the mark.
	mustExec(t, c, `begin`)
	mustExec(t, c, `insert into t values (3)`)
	mustExec(t, c, `savepoint sp`)
	if _, err := c.Exec(ctx, `insert into t values (1)`); err == nil {
		t.Fatal("duplicate insert succeeded")
	}
	if st := c.PgConn().TxStatus(); st != 'E' {
		t.Fatalf("TxStatus after error = %c; want E", st)
	}
	mustExec(t, c, `rollback to savepoint sp`)
	if st := c.PgConn().TxStatus(); st != 'T' {
		t.Fatalf("TxStatus after recovery = %c; want T", st)
	}
	mustExec(t, c, `commit`)
	if err := c.QueryRow(ctx, `select count(*) from t`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("count after recovered block = %d err = %v; want 2", n, err)
	}

	// Savepoint statements outside a block are errors (25P01), and an
	// unknown name is 3B001.
	var pgErr *pgconn.PgError
	if _, err := c.Exec(ctx, `savepoint sp`); !errors.As(err, &pgErr) || pgErr.Code != "25P01" {
		t.Fatalf("savepoint outside block = %v; want SQLSTATE 25P01", err)
	}
	mustExec(t, c, `begin`)
	if _, err := c.Exec(ctx, `rollback to savepoint nope`); !errors.As(err, &pgErr) || pgErr.Code != "3B001" {
		t.Fatalf("unknown savepoint = %v; want SQLSTATE 3B001", err)
	}
	mustExec(t, c, `rollback`)
}

// TestTransactionNestedPgx: pgx models nested Begin calls as
// savepoints — the inner "transaction" must roll back independently
// while the outer one commits.
func TestTransactionNestedPgx(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create table t (id int primary key)`)

	outer, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := outer.Exec(ctx, `insert into t values (1)`); err != nil {
		t.Fatal(err)
	}
	inner, err := outer.Begin(ctx) // SAVEPOINT under the hood
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inner.Exec(ctx, `insert into t values (2)`); err != nil {
		t.Fatal(err)
	}
	if err := inner.Rollback(ctx); err != nil { // ROLLBACK TO SAVEPOINT
		t.Fatal(err)
	}
	if err := outer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := c.QueryRow(ctx, `select count(*) from t`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("count = %d err = %v; want only the outer row", n, err)
	}

	// And the commit path: an inner commit is RELEASE SAVEPOINT.
	outer, err = c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inner, err = outer.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inner.Exec(ctx, `insert into t values (3)`); err != nil {
		t.Fatal(err)
	}
	if err := inner.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		t.Fatal(err)
	}
	if err := outer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(ctx, `select count(*) from t`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("count = %d err = %v; want 2", n, err)
	}
}
