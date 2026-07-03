package sql

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func openDB(t *testing.T) *DB {
	t.Helper()
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return New(e)
}

func exec(t *testing.T, d *DB, q string) *Result {
	t.Helper()
	res, err := d.Exec(q)
	if err != nil {
		t.Fatalf("Exec(%q): %v", q, err)
	}
	return res
}

func seedUsers(t *testing.T, d *DB) {
	t.Helper()
	exec(t, d, `create table users (id int primary key, name text, age int, city text)`)
	exec(t, d, `insert into users values
		(1, 'ada', 36, 'london'),
		(2, 'grace', 45, 'nyc'),
		(3, 'alan', 41, 'london'),
		(4, 'edsger', 39, 'austin'),
		(5, 'barbara', 28, 'nyc')`)
}

func TestSQLCrud(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	res := exec(t, d, `SELECT name, age FROM users WHERE city = 'london' ORDER BY age DESC`)
	want := [][]any{{"alan", int64(41)}, {"ada", int64(36)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}
	if !reflect.DeepEqual(res.Cols, []string{"name", "age"}) ||
		!reflect.DeepEqual(res.Types, []bytdb.ColType{bytdb.TString, bytdb.TInt}) {
		t.Fatalf("cols %v types %v", res.Cols, res.Types)
	}

	if res := exec(t, d, `update users set city = 'sf', age = 29 where id = 5`); res.RowsAffected != 1 {
		t.Fatalf("update affected %d", res.RowsAffected)
	}
	res = exec(t, d, `select city, age from users where id = 5`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"sf", int64(29)}}) {
		t.Fatalf("got %v", res.Rows)
	}

	// A key-moving update through SQL.
	exec(t, d, `update users set id = 50 where id = 5`)
	if res := exec(t, d, `select id from users where id = 50`); len(res.Rows) != 1 {
		t.Fatal("moved row not found")
	}

	if res := exec(t, d, `delete from users where age >= 40`); res.RowsAffected != 2 {
		t.Fatalf("delete affected %d", res.RowsAffected)
	}
	if res := exec(t, d, `select * from users`); len(res.Rows) != 3 {
		t.Fatalf("%d rows left", len(res.Rows))
	}
}

func TestSQLPointGetAndMisses(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	if res := exec(t, d, `select name from users where id = 3`); !reflect.DeepEqual(res.Rows, [][]any{{"alan"}}) {
		t.Fatalf("got %v", res.Rows)
	}
	if res := exec(t, d, `select name from users where id = 99`); len(res.Rows) != 0 {
		t.Fatalf("got %v", res.Rows)
	}
	// Point get with an extra false residual predicate.
	if res := exec(t, d, `select name from users where id = 3 and age > 100`); len(res.Rows) != 0 {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestSQLIndexScan(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	exec(t, d, `create index by_city on users (city)`)

	res := exec(t, d, `select id from users where city = 'nyc' order by id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(2)}, {int64(5)}}) {
		t.Fatalf("got %v", res.Rows)
	}
	// Range over the indexed column.
	res = exec(t, d, `select id from users where city >= 'l' and city < 'm' order by id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}, {int64(3)}}) {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestSQLIndexUpperBoundSkipsNullGroup(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, v int)`)
	exec(t, d, `create index by_v on t (v)`)
	exec(t, d, `insert into t (id, v) values (1, null), (2, 5), (3, 20), (4, null), (5, 10)`)
	// Only an upper bound: the index scan enters at the NULL group,
	// which sorts before every value and must be skipped, not treated
	// as the end of the range.
	res := exec(t, d, `select id from t where v <= 10 order by id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(2)}, {int64(5)}}) {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestSQLCompositePKRange(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table ev (a int, b int, v text, primary key (a, b))`)
	exec(t, d, `insert into ev values (1,1,'a'),(1,2,'b'),(1,3,'c'),(2,1,'d'),(2,2,'e')`)

	res := exec(t, d, `select v from ev where a = 1 and b >= 2`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"b"}, {"c"}}) {
		t.Fatalf("got %v", res.Rows)
	}
	res = exec(t, d, `select v from ev where a = 1 and b > 1 and b <= 2`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"b"}}) {
		t.Fatalf("got %v", res.Rows)
	}
	res = exec(t, d, `select v from ev where a = 2`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"d"}, {"e"}}) {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestSQLNullsAndOrdering(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, v int)`)
	exec(t, d, `insert into t (id, v) values (1, 3), (2, 1), (3, null), (4, 2)`)

	res := exec(t, d, `select id from t where v is null`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(3)}}) {
		t.Fatalf("got %v", res.Rows)
	}
	if res := exec(t, d, `select id from t where v is not null`); len(res.Rows) != 3 {
		t.Fatalf("got %v", res.Rows)
	}
	// NULL never matches a comparison.
	if res := exec(t, d, `select id from t where v != 1`); len(res.Rows) != 2 {
		t.Fatalf("got %v", res.Rows)
	}
	// NULLS LAST ascending, first descending.
	res = exec(t, d, `select id from t order by v`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(2)}, {int64(4)}, {int64(1)}, {int64(3)}}) {
		t.Fatalf("asc: got %v", res.Rows)
	}
	res = exec(t, d, `select id from t order by v desc`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(3)}, {int64(1)}, {int64(4)}, {int64(2)}}) {
		t.Fatalf("desc: got %v", res.Rows)
	}
	// LIMIT/OFFSET after ordering.
	res = exec(t, d, `select id from t order by v limit 2 offset 1`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(4)}, {int64(1)}}) {
		t.Fatalf("got %v", res.Rows)
	}
	// LIMIT without ORDER BY stops the scan early but stays correct.
	if res := exec(t, d, `select id from t limit 2`); len(res.Rows) != 2 {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestSQLInsertAtomicity(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, email text)`)
	exec(t, d, `create unique index by_email on t (email)`)
	exec(t, d, `insert into t values (1, 'a@x')`)

	// Second row collides on the unique index: the whole INSERT rolls back.
	if _, err := d.Exec(`insert into t values (2, 'b@x'), (3, 'a@x')`); err == nil {
		t.Fatal("expected unique violation")
	}
	if res := exec(t, d, `select * from t`); len(res.Rows) != 1 {
		t.Fatalf("partial insert leaked: %v", res.Rows)
	}

	// Same for UPDATE: an error mid-statement stages nothing.
	exec(t, d, `insert into t values (2, 'b@x')`)
	if _, err := d.Exec(`update t set email = 'a@x' where id = 2`); err == nil {
		t.Fatal("expected unique violation")
	}
	if res := exec(t, d, `select email from t where id = 2`); !reflect.DeepEqual(res.Rows, [][]any{{"b@x"}}) {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestSQLInsertColumnList(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, a text, b int)`)
	exec(t, d, `insert into t (b, id) values (7, 1)`) // a omitted -> NULL
	res := exec(t, d, `select a, b from t where id = 1`)
	if !reflect.DeepEqual(res.Rows, [][]any{{nil, int64(7)}}) {
		t.Fatalf("got %v", res.Rows)
	}
	// Omitting a pk column is an engine-level error.
	if _, err := d.Exec(`insert into t (a) values ('x')`); err == nil {
		t.Fatal("expected NULL-pk rejection")
	}
}

func TestSQLAlter(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, a text)`)
	exec(t, d, `insert into t values (1, 'x')`)
	exec(t, d, `alter table t add column score float`)
	// Old rows read the new column as NULL.
	res := exec(t, d, `select id from t where score is null`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}}) {
		t.Fatalf("got %v", res.Rows)
	}
	exec(t, d, `insert into t values (2, 'y', 9.5)`)
	res = exec(t, d, `select score from t where id = 2`)
	if !reflect.DeepEqual(res.Rows, [][]any{{9.5}}) {
		t.Fatalf("got %v", res.Rows)
	}
	exec(t, d, `alter table t drop column a`)
	res = exec(t, d, `select * from t where id = 1`)
	if !reflect.DeepEqual(res.Cols, []string{"id", "score"}) {
		t.Fatalf("cols: %v", res.Cols)
	}
}

func TestSQLDropIndexResolve(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table a (id int primary key, v int)`)
	exec(t, d, `create table b (id int primary key, v int)`)
	exec(t, d, `create index by_v on a (v)`)
	exec(t, d, `create index by_v on b (v)`)

	if _, err := d.Exec(`drop index by_v`); err == nil {
		t.Fatal("expected ambiguity error")
	}
	exec(t, d, `drop index by_v on a`)
	exec(t, d, `drop index by_v`) // now unambiguous
	if _, err := d.Exec(`drop index by_v`); err == nil {
		t.Fatal("expected no-such-index error")
	}
}

func TestSQLDDLRoundTrip(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table "MyTable" ("Weird Col" int primary key, v text)`)
	exec(t, d, `insert into "MyTable" values (1, 'x')`)
	res := exec(t, d, `select "Weird Col" from "MyTable" where "Weird Col" = 1`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}}) {
		t.Fatalf("got %v", res.Rows)
	}
	exec(t, d, `drop table "MyTable"`)
	if _, err := d.Exec(`select * from "MyTable"`); err == nil {
		t.Fatal("expected no-such-table")
	}
}

func TestSQLErrors(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	for _, q := range []string{
		`select * from nope`,
		`select nope from users`,
		`select * from users where nope = 1`,
		`select * from users order by nope`,
		`insert into users values (1, 'dup', 1, 'x')`, // duplicate pk
		`update users set nope = 1 where id = 1`,
		`create table users (id int primary key)`, // already exists
	} {
		if _, err := d.Exec(q); err == nil {
			t.Errorf("Exec(%q): expected error", q)
		}
	}
}
