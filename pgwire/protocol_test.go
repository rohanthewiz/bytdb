package pgwire_test

// Protocol-corner tests (plan item 21): the empty query string, an
// explicit Describe of a prepared statement with inferred parameter
// OIDs, and rebinding a cached prepared statement with fresh values
// including NULL. The row-limited Execute corner lives in
// rowlimit_test.go (it needs raw frames; no driver sends a row limit
// by default).

import (
	"context"
	"testing"
)

func TestEmptyQueryString(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))

	// An empty (or whitespace/;-only) simple query gets
	// EmptyQueryResponse, not an error, and leaves the connection
	// usable.
	for _, q := range []string{"", "   ", ";", " ; ; "} {
		res, err := c.PgConn().Exec(ctx, q).ReadAll()
		if err != nil {
			t.Fatalf("empty query %q: %v", q, err)
		}
		for _, r := range res {
			if r.Err != nil {
				t.Fatalf("empty query %q: %v", q, r.Err)
			}
		}
	}
	var one int64
	if err := c.QueryRow(ctx, `select 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("connection unusable after empty queries: %v %d", err, one)
	}
}

// TestDescribeStatementOIDs: an explicit Describe('S') must report one
// inferred parameter OID per placeholder — from the columns each $n
// touches — plus the result-row shape.
func TestDescribeStatementOIDs(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create table users (id int primary key, name text, active bool, score float)`)

	sd, err := c.PgConn().Prepare(ctx, "ps_infer",
		`select name, score from users where id = $1 and active = $2 and name = $3`, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantParams := []uint32{20, 16, 25} // int8, bool, text
	if len(sd.ParamOIDs) != len(wantParams) {
		t.Fatalf("param OIDs: %v; want %v", sd.ParamOIDs, wantParams)
	}
	for i, oid := range wantParams {
		if sd.ParamOIDs[i] != oid {
			t.Fatalf("param $%d OID = %d; want %d", i+1, sd.ParamOIDs[i], oid)
		}
	}
	if len(sd.Fields) != 2 || sd.Fields[0].DataTypeOID != 25 || sd.Fields[1].DataTypeOID != 701 {
		t.Fatalf("result fields: %+v", sd.Fields)
	}
}

// TestRebindCachedStatement: pgx caches the statement after the first
// Parse and re-Binds it on later executions — fresh values, NULL, and
// back must all flow through the cached plan.
func TestRebindCachedStatement(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create table users (id int primary key, age int)`)
	mustExec(t, c, `insert into users values (1, 30), (2, 40), (3, null)`)

	q := `select count(*) from users where age > $1`
	count := func(arg any) int64 {
		t.Helper()
		var n int64
		if err := c.QueryRow(ctx, q, arg).Scan(&n); err != nil {
			t.Fatalf("count(%v): %v", arg, err)
		}
		return n
	}
	if n := count(int64(25)); n != 2 {
		t.Fatalf("age > 25: %d", n)
	}
	if n := count(int64(35)); n != 1 { // same cached statement, new value
		t.Fatalf("age > 35: %d", n)
	}
	// NULL parameter: age > NULL is unknown for every row.
	if n := count((*int64)(nil)); n != 0 {
		t.Fatalf("age > NULL: %d", n)
	}
	if n := count(int64(25)); n != 2 { // and back to a value
		t.Fatalf("age > 25 after NULL: %d", n)
	}
}
