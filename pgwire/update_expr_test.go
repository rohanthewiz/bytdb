package pgwire_test

// End-to-end coverage for expressions in UPDATE SET over the extended
// query protocol: pgx Describes the statement, so the $n inside the
// SET expression must infer a usable type (the target column's) for
// the client to encode its argument at Bind.

import (
	"context"
	"testing"
)

func TestUpdateSetExpressionWire(t *testing.T) {
	c := connect(t, startServer(t))
	ctx := context.Background()

	mustExec(t, c, `create table t (id int primary key, age int, name text)`)
	mustExec(t, c, `insert into t values (1, 30, 'ada'), (2, 40, 'grace')`)

	// Extended protocol with a placeholder inside the SET expression;
	// re-run to prove the cached prepared statement re-binds.
	for _, want := range []int64{35, 40} {
		tag := mustExec(t, c, `update t set age = age + $1 where id = $2`, int64(5), int64(1))
		if tag.RowsAffected() != 1 {
			t.Fatalf("affected %d; want 1", tag.RowsAffected())
		}
		var age int64
		if err := c.QueryRow(ctx, `select age from t where id = 1`).Scan(&age); err != nil {
			t.Fatal(err)
		}
		if age != want {
			t.Fatalf("age = %d; want %d", age, want)
		}
	}

	// Self-referential string expression, no placeholders (simple
	// protocol path inside an Exec with no args).
	mustExec(t, c, `update t set name = name || '!' where id = 2`)
	var name string
	if err := c.QueryRow(ctx, `select name from t where id = 2`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "grace!" {
		t.Fatalf("name = %q; want %q", name, "grace!")
	}
}
