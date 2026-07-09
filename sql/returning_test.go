package sql

// returning_test.go: the RETURNING clause of INSERT, UPDATE, and
// DELETE — the piece ORMs key on once IDs are server-generated. The
// contract under test: INSERT and UPDATE report each row as the engine
// stored it (identity values drawn inside the engine, values coerced,
// SET applied), DELETE reports the row as it was, and the clause is a
// full select list (expressions, aliases, *, t.*) minus aggregates and
// window functions.

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// TestInsertReturningSerial: the flagship — the client learns the
// server-drawn identity value without a second round trip.
func TestInsertReturningSerial(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table users (id serial primary key, name text)`)

	res := exec(t, d, `insert into users (name) values ('ada'), ('grace') returning id`)
	if !reflect.DeepEqual(res.Cols, []string{"id"}) {
		t.Fatalf("cols: got %v", res.Cols)
	}
	if !reflect.DeepEqual(res.Types, []bytdb.ColType{bytdb.TInt}) {
		t.Fatalf("types: got %v", res.Types)
	}
	// Rows come back in insertion order, one per VALUES row.
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}, {int64(2)}}) {
		t.Fatalf("rows: got %v", res.Rows)
	}
	if res.RowsAffected != 2 {
		t.Fatalf("affected: got %d, want 2", res.RowsAffected)
	}

	// An explicit id echoes back as given (and bumps the counter, so
	// the next draw continues past it).
	res = exec(t, d, `insert into users values (10, 'alan') returning id, name`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(10), "alan"}}) {
		t.Fatalf("explicit id: got %v", res.Rows)
	}
	res = exec(t, d, `insert into users (name) values ('edsger') returning id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(11)}}) {
		t.Fatalf("post-bump draw: got %v", res.Rows)
	}
}

// TestInsertReturningStar: * expands to the full stored row — identity
// filled, unnamed columns NULL.
func TestInsertReturningStar(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id serial primary key, name text, age int)`)

	res := exec(t, d, `insert into t (name) values ('ada') returning *`)
	if !reflect.DeepEqual(res.Cols, []string{"id", "name", "age"}) {
		t.Fatalf("cols: got %v", res.Cols)
	}
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1), "ada", nil}}) {
		t.Fatalf("rows: got %v", res.Rows)
	}

	// t.* is the same expansion through the qualified form.
	res = exec(t, d, `insert into t (name, age) values ('grace', 45) returning t.*`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(2), "grace", int64(45)}}) {
		t.Fatalf("t.*: got %v", res.Rows)
	}
}

// TestInsertReturningExpr: the clause is a select list — expressions,
// aliases, and literal coercion all apply.
func TestInsertReturningExpr(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id serial primary key, name text)`)

	res := exec(t, d, `insert into t (name) values ('ada') returning id * 2 as double, upper(name)`)
	if !reflect.DeepEqual(res.Cols, []string{"double", "upper"}) {
		t.Fatalf("cols: got %v", res.Cols)
	}
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(2), "ADA"}}) {
		t.Fatalf("rows: got %v", res.Rows)
	}
}

// TestUpdateReturning: reports the post-update row, including values
// the engine computed (SET expressions read the pre-update row; the
// stored result is what comes back).
func TestUpdateReturning(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	res := exec(t, d, `update users set age = age + 1 where city = 'london' returning id, age`)
	if res.RowsAffected != 2 {
		t.Fatalf("affected: got %d, want 2", res.RowsAffected)
	}
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1), int64(37)}, {int64(3), int64(42)}}) {
		t.Fatalf("rows: got %v", res.Rows)
	}

	// No matches: zero rows, but the shape is still announced (a wire
	// client sends RowDescription before it knows the row count).
	res = exec(t, d, `update users set age = 0 where id = 999 returning id`)
	if len(res.Rows) != 0 || res.RowsAffected != 0 {
		t.Fatalf("no-match: got rows %v, affected %d", res.Rows, res.RowsAffected)
	}
	if !reflect.DeepEqual(res.Cols, []string{"id"}) {
		t.Fatalf("no-match cols: got %v", res.Cols)
	}
}

// TestUpdateReturningEngineCoercion: an int assigned to a float column
// stores (and returns) as float — RETURNING reports engine truth, not
// the statement's literal.
func TestUpdateReturningEngineCoercion(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table m (id int primary key, price float)`)
	exec(t, d, `insert into m values (1, 2.5)`)

	res := exec(t, d, `update m set price = 3 where id = 1 returning price`)
	if !reflect.DeepEqual(res.Rows, [][]any{{float64(3)}}) {
		t.Fatalf("got %v (%T), want [[3.0]]", res.Rows, res.Rows[0][0])
	}
}

// TestDeleteReturning: reports each row as it was before its removal.
func TestDeleteReturning(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	res := exec(t, d, `delete from users where city = 'nyc' returning id, name`)
	if res.RowsAffected != 2 {
		t.Fatalf("affected: got %d, want 2", res.RowsAffected)
	}
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(2), "grace"}, {int64(5), "barbara"}}) {
		t.Fatalf("rows: got %v", res.Rows)
	}
	// And the rows are really gone.
	res = exec(t, d, `select count(*) from users`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(3)}}) {
		t.Fatalf("remaining: got %v", res.Rows)
	}
}

// TestReturningParams: placeholders bind inside RETURNING expressions
// and count toward the statement's parameter arity.
func TestReturningParams(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id serial primary key, name text)`)

	st, err := d.Prepare(`insert into t (name) values ($1) returning id + $2`)
	if err != nil {
		t.Fatal(err)
	}
	if st.NumParams() != 2 {
		t.Fatalf("NumParams: got %d, want 2", st.NumParams())
	}
	res, err := st.Exec("ada", int64(100))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(101)}}) {
		t.Fatalf("rows: got %v", res.Rows)
	}
}

// TestReturningDescribe: the extended protocol learns a DML
// statement's output shape at Describe time, before execution.
func TestReturningDescribe(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id serial primary key, name text)`)

	st, err := d.Prepare(`insert into t (name) values ($1) returning id, name`)
	if err != nil {
		t.Fatal(err)
	}
	info, err := st.Describe()
	if err != nil {
		t.Fatal(err)
	}
	if info.Command != "INSERT" {
		t.Fatalf("command: got %q", info.Command)
	}
	if !reflect.DeepEqual(info.Cols, []string{"id", "name"}) {
		t.Fatalf("cols: got %v", info.Cols)
	}
	if !reflect.DeepEqual(info.Types, []bytdb.ColType{bytdb.TInt, bytdb.TString}) {
		t.Fatalf("types: got %v", info.Types)
	}

	// Without the clause the statement keeps its rowless shape.
	st, err = d.Prepare(`insert into t (name) values ($1)`)
	if err != nil {
		t.Fatal(err)
	}
	if info, err = st.Describe(); err != nil {
		t.Fatal(err)
	}
	if len(info.Cols) != 0 {
		t.Fatalf("plain insert cols: got %v, want none", info.Cols)
	}
}

// TestReturningRejects: aggregates and window functions have no row
// set to fold — rejected at parse, as in Postgres. Unknown columns
// fail the whole statement, and atomically: nothing is inserted.
func TestReturningRejects(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, v text)`)

	for _, q := range []string{
		`insert into t values (1, 'a') returning count(*)`,
		`insert into t values (1, 'a') returning sum(id)`,
		`update t set v = 'b' returning max(id)`,
		`delete from t returning count(*)`,
	} {
		if _, err := d.Exec(q); err == nil || !strings.Contains(err.Error(), "aggregate") {
			t.Fatalf("%s: got %v, want an aggregate rejection", q, err)
		}
	}
	if _, err := d.Exec(`insert into t values (1, 'a') returning row_number() over ()`); err == nil ||
		!strings.Contains(err.Error(), "window") {
		t.Fatalf("window: got %v, want a window rejection", err)
	}

	// An unknown column in RETURNING fails before (and rolls back) the
	// statement's writes.
	if _, err := d.Exec(`insert into t values (1, 'a') returning nope`); err == nil {
		t.Fatal("unknown column: want an error")
	}
	res := exec(t, d, `select count(*) from t`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(0)}}) {
		t.Fatalf("atomicity: table has %v rows, want 0", res.Rows[0][0])
	}
}

// TestReturningInTransaction: RETURNING works inside a BEGIN block
// (the write path runs on the session's open transaction), and the
// returned values roll back with it.
func TestReturningInTransaction(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id serial primary key, v text)`)
	s := d.NewSession()
	defer s.Close()

	mustS := func(q string) *Result {
		t.Helper()
		res, err := s.Exec(q)
		if err != nil {
			t.Fatalf("Exec(%q): %v", q, err)
		}
		return res
	}
	mustS(`begin`)
	res := mustS(`insert into t (v) values ('a') returning id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}}) {
		t.Fatalf("in-txn insert: got %v", res.Rows)
	}
	mustS(`rollback`)

	// The rolled-back draw returns to the counter (single-writer
	// engine: nothing else could have drawn meanwhile).
	res = mustS(`insert into t (v) values ('b') returning id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}}) {
		t.Fatalf("post-rollback insert: got %v", res.Rows)
	}
}
