package edgeaudit

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Postgres runs (a) a multi-statement simple Query and (b) an extended
// pipeline up to Sync (pgx.Batch) as ONE implicit transaction: a failure
// rolls back the statements before it. What does bytdb do?
func TestImplicitTransactionAtomicity(t *testing.T) {
	addr, _, _ := startServer(t, nil)
	ctx := context.Background()
	c := pgxConn(t, addr)
	if _, err := c.Exec(ctx, `create table acct (id int primary key, bal int)`); err != nil {
		t.Fatal(err)
	}
	c.Exec(ctx, `insert into acct values (1, 100), (2, 0)`)

	// (a) simple protocol, one Query message.
	_, err := c.PgConn().Exec(ctx, `update acct set bal = bal - 50 where id = 1; insert into acct values (2, 999)`).ReadAll()
	t.Logf("simple multi-statement error: %v", err)
	var bal int
	c.QueryRow(ctx, `select bal from acct where id = 1`).Scan(&bal)
	t.Logf("(a) after failed simple-query batch: acct 1 balance = %d (Postgres: 100)", bal)
	if bal != 100 {
		t.Errorf("simple multi-statement Query is not atomic: debit committed although the batch failed")
	}

	// (b) pgx.Batch -> Parse/Bind/Execute x2 + one Sync.
	c.Exec(ctx, `update acct set bal = 100 where id = 1`)
	b := &pgx.Batch{}
	b.Queue(`update acct set bal = bal - 50 where id = $1`, 1)
	b.Queue(`insert into acct values ($1, 999)`, 2) // duplicate key
	err = c.SendBatch(ctx, b).Close()
	t.Logf("batch error: %v", err)
	c.QueryRow(ctx, `select bal from acct where id = 1`).Scan(&bal)
	t.Logf("(b) after failed pgx.Batch: acct 1 balance = %d (Postgres: 100)", bal)
	if bal != 100 {
		t.Errorf("pgx.Batch (pipeline to Sync) is not atomic: debit committed although the batch failed")
	}
}
