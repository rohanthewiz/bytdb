package sql

import (
	"reflect"
	"strings"
	"testing"
)

// Expression values in INSERT: VALUES entries parse through the full
// expression grammar and evaluate at execution — arithmetic, casts,
// function calls, scalar subqueries, and (the driving case) sequence
// draws via nextval. Plain literals keep their historical fast path.

func TestInsertExprValues(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, name text, score float)`)

	// Arithmetic, concatenation, and a function call, all in one row.
	exec(t, d, `insert into t values (40 + 2, 'by'||upper('t')||'db', 1.5 * 2)`)
	res := exec(t, d, `select id, name, score from t`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(42), "byTdb", 3.0}}) {
		t.Fatalf("expression values: %v", res.Rows)
	}

	// A CASE and a cast; multi-row VALUES mixes expressions, literals,
	// and DEFAULT in one statement.
	exec(t, d, `alter table t add column tag text`)
	exec(t, d, `insert into t values
		(case when 1 < 2 then 100 else 200 end, 'c', 0.5, 'x'),
		('7'::int * 10, 'd', 0.25, default)`)
	res = exec(t, d, `select id, tag from t where id >= 70 order by id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(70), nil}, {int64(100), "x"}}) {
		t.Fatalf("case/cast/default rows: %v", res.Rows)
	}

	// RETURNING sees the evaluated (and engine-coerced) values.
	res = exec(t, d, `insert into t values (2 * 500, 'e', 1.0/4, lower('E')) returning id, score, tag`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1000), 0.25, "e"}}) {
		t.Fatalf("returning evaluated values: %v", res.Rows)
	}
}

func TestInsertExprNextval(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create sequence ids start with 10`)
	exec(t, d, `create table u (id int primary key, v text)`)

	// The m28 gotcha this milestone closes: nextval directly in VALUES.
	exec(t, d, `insert into u values (nextval('ids'), 'a')`)
	exec(t, d, `insert into u values (nextval('ids'), 'b'), (nextval('ids'), 'c')`)
	res := exec(t, d, `select id, v from u order by id`)
	want := [][]any{{int64(10), "a"}, {int64(11), "b"}, {int64(12), "c"}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("nextval draws: %v", res.Rows)
	}

	// The draws registered with the session readbacks, as in Postgres.
	res = exec(t, d, `select currval('ids'), lastval()`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(12), int64(12)}}) {
		t.Fatalf("currval/lastval after insert: %v", res.Rows)
	}

	// Re-executing the same statement draws fresh values: evaluation
	// must not fold results back into the parsed AST.
	if _, err := d.Exec(`insert into u values (nextval('ids'), 'd')`); err != nil {
		t.Fatalf("re-insert: %v", err)
	}
	res = exec(t, d, `select max(id) from u`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(13)}}) {
		t.Fatalf("fresh draw on re-execution: %v", res.Rows)
	}
}

func TestInsertExprSubqueryAndParams(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table src (id int primary key)`)
	exec(t, d, `insert into src values (5), (9)`)
	exec(t, d, `create table dst (id int primary key, v int)`)

	// A scalar subquery as a VALUES entry.
	exec(t, d, `insert into dst values ((select max(id) from src), 1)`)
	res := exec(t, d, `select id from dst`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(9)}}) {
		t.Fatalf("subquery value: %v", res.Rows)
	}

	// Placeholders inside expressions bind per execution, so the same
	// prepared shape inserts different rows.
	for i, base := range []int64{100, 200} {
		if _, err := d.Exec(`insert into dst values ($1 + 1, $2)`, base, int64(i)); err != nil {
			t.Fatalf("param expr insert %d: %v", i, err)
		}
	}
	res = exec(t, d, `select id, v from dst where id > 50 order by id`)
	want := [][]any{{int64(101), int64(0)}, {int64(201), int64(1)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("param expr rows: %v", res.Rows)
	}

	// The ON CONFLICT probe sees the evaluated value, not the tree.
	exec(t, d, `insert into dst values (100 + 1, 99) on conflict (id) do nothing`)
	res = exec(t, d, `select v from dst where id = 101`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(0)}}) {
		t.Fatalf("conflict probe on evaluated value: %v", res.Rows)
	}
}

func TestInsertExprErrors(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, v int)`)

	// VALUES has no input row: column references fail, as in Postgres.
	if _, err := d.Exec(`insert into t values (id + 1, 2)`); err == nil ||
		!strings.Contains(err.Error(), "no such column") {
		t.Fatalf("column ref in VALUES: %v", err)
	}
	// Aggregates and windows have no row set to compute over.
	if _, err := d.Exec(`insert into t values (sum(1), 2)`); err == nil ||
		!strings.Contains(err.Error(), "aggregate functions are not allowed in VALUES") {
		t.Fatalf("aggregate in VALUES: %v", err)
	}
	if _, err := d.Exec(`insert into t values (row_number() over (), 2)`); err == nil ||
		!strings.Contains(err.Error(), "window functions are not allowed in VALUES") {
		t.Fatalf("window in VALUES: %v", err)
	}
	// An expression evaluating to the wrong kind still hits the
	// engine's coercion error, same as a literal would.
	if _, err := d.Exec(`insert into t values ('a'||'b', 2)`); err == nil {
		t.Fatal("string expression into int column should error")
	}
}
