package sql

// returning_test.go: the RETURNING clause of INSERT, UPDATE, and
// DELETE — the stored/post-update/pre-delete row images, star and
// expression items, placeholders, Describe, and the rejections.

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func TestInsertReturning(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table users (id serial primary key, name text)`)

	// The headline: server-generated ids come back, one row per
	// inserted row, in insert order.
	res := exec(t, d, `insert into users (name) values ('ada'), ('grace') returning id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}, {int64(2)}}) {
		t.Fatalf("ids: %v", res.Rows)
	}
	if res.RowsAffected != 2 {
		t.Fatalf("rows affected: %d", res.RowsAffected)
	}
	if !reflect.DeepEqual(res.Cols, []string{"id"}) ||
		!reflect.DeepEqual(res.Types, []bytdb.ColType{bytdb.TInt}) {
		t.Fatalf("shape: %v %v", res.Cols, res.Types)
	}

	// RETURNING * yields the whole stored row in declared column
	// order — identity filled even when the insert wrote NULL there.
	res = exec(t, d, `insert into users values (null, 'alan') returning *`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(3), "alan"}}) {
		t.Fatalf("star: %v", res.Rows)
	}
	if !reflect.DeepEqual(res.Cols, []string{"id", "name"}) {
		t.Fatalf("star cols: %v", res.Cols)
	}

	// t.* and expression items with aliases, evaluated against the
	// stored row.
	res = exec(t, d, `insert into users (name) values ('edsger')
		returning users.*, id * 2 as twice, upper(name)`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(4), "edsger", int64(8), "EDSGER"}}) {
		t.Fatalf("exprs: %v", res.Rows)
	}
	if !reflect.DeepEqual(res.Cols, []string{"id", "name", "twice", "upper"}) {
		t.Fatalf("expr cols: %v", res.Cols)
	}
}

func TestUpdateDeleteReturning(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	// UPDATE returns the post-update image, only for matched rows.
	res := exec(t, d, `update users set age = age + 1 where city = 'london' returning id, age`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1), int64(37)}, {int64(3), int64(42)}}) {
		t.Fatalf("update returning: %v", res.Rows)
	}
	if res.RowsAffected != 2 {
		t.Fatalf("rows affected: %d", res.RowsAffected)
	}

	// DELETE returns each row as it was.
	res = exec(t, d, `delete from users where city = 'nyc' returning *`)
	if !reflect.DeepEqual(res.Rows, [][]any{
		{int64(2), "grace", int64(45), "nyc"},
		{int64(5), "barbara", int64(28), "nyc"},
	}) {
		t.Fatalf("delete returning: %v", res.Rows)
	}

	// No matches: the shape is still announced, with zero rows — wire
	// clients expect a RowDescription either way.
	res = exec(t, d, `delete from users where id = 99 returning id`)
	if len(res.Rows) != 0 || !reflect.DeepEqual(res.Cols, []string{"id"}) {
		t.Fatalf("empty returning: rows=%v cols=%v", res.Rows, res.Cols)
	}
}

func TestReturningParams(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id serial primary key, v text)`)

	// Placeholders bind inside RETURNING expressions like anywhere
	// else, and count toward the statement's arity.
	res, err := d.Exec(`insert into t (v) values ($1) returning id, v || $2`, "a", "!")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1), "a!"}}) {
		t.Fatalf("bound returning: %v", res.Rows)
	}

	st, err := d.Prepare(`update t set v = $1 where id = $2 returning id, v`)
	if err != nil {
		t.Fatal(err)
	}
	if st.NumParams() != 2 {
		t.Fatalf("params: %d", st.NumParams())
	}
	res, err = st.Exec("b", int64(1))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1), "b"}}) {
		t.Fatalf("prepared returning: %v", res.Rows)
	}
	// A prepared statement re-binds cleanly on each execution.
	if res, err = st.Exec("c", int64(1)); err != nil || res.Rows[0][1] != "c" {
		t.Fatalf("re-exec: %v %v", err, res.Rows)
	}
}

func TestReturningDescribe(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id serial primary key, v text)`)

	// Describe announces the output shape without executing — what an
	// ORM's Parse/Describe round asks before the first insert.
	st, err := d.Prepare(`insert into t (v) values ($1) returning id, v as name`)
	if err != nil {
		t.Fatal(err)
	}
	info, err := st.Describe()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(info.Cols, []string{"id", "name"}) ||
		!reflect.DeepEqual(info.Types, []bytdb.ColType{bytdb.TInt, bytdb.TString}) {
		t.Fatalf("describe: %v %v", info.Cols, info.Types)
	}
	if info.Command != "INSERT" {
		t.Fatalf("command: %q", info.Command)
	}

	// Without RETURNING the statement still describes as row-less.
	st, err = d.Prepare(`insert into t (v) values ($1)`)
	if err != nil {
		t.Fatal(err)
	}
	if info, err = st.Describe(); err != nil || len(info.Cols) != 0 {
		t.Fatalf("no returning: %v %v", err, info.Cols)
	}
}

func TestReturningRejections(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, v text)`)

	for _, tc := range []struct{ q, want string }{
		{`insert into t values (1, 'a') returning count(*)`,
			"aggregate functions are not allowed in RETURNING"},
		{`update t set v = 'b' returning sum(id)`,
			"aggregate functions are not allowed in RETURNING"},
		{`delete from t returning row_number() over ()`,
			"window functions are not allowed in RETURNING"},
		{`insert into t values (1, 'a') returning nope`,
			"column"},
		{`insert into t values (1, 'a') returning`,
			""}, // trailing RETURNING with no items is a parse error
	} {
		if _, err := d.Exec(tc.q); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: got %v, want %q", tc.q, err, tc.want)
		}
	}
}

func TestReturningInTransaction(t *testing.T) {
	// RETURNING runs inside the statement's transaction: in a block,
	// rolled-back rows were still reported at insert time (their ids
	// burn, as in Postgres), and the data is gone.
	d := openDB(t)
	exec(t, d, `create table t (id serial primary key, v text)`)
	s := d.NewSession()
	mustSess := func(q string) *Result {
		t.Helper()
		res, err := s.Exec(q)
		if err != nil {
			t.Fatalf("session %q: %v", q, err)
		}
		return res
	}
	mustSess(`begin`)
	res := mustSess(`insert into t (v) values ('x') returning id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}}) {
		t.Fatalf("in-block returning: %v", res.Rows)
	}
	mustSess(`rollback`)
	if res = mustSess(`select count(*) from t`); res.Rows[0][0] != int64(0) {
		t.Fatalf("rolled-back rows remain: %v", res.Rows)
	}
}
