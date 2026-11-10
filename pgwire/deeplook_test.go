package pgwire_test

// Wire-level regression tests from the deep-look remediation plan.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
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
