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

func TestSQLBoolExprs(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	res := exec(t, d, `select id from users where city = 'austin' or age > 42 order by id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(2)}, {int64(4)}}) {
		t.Fatalf("got %v", res.Rows)
	}
	// AND binds tighter than OR.
	res = exec(t, d, `select id from users where city = 'nyc' and age > 40 or id = 1 order by id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}, {int64(2)}}) {
		t.Fatalf("got %v", res.Rows)
	}
	// Parens override.
	res = exec(t, d, `select id from users where city = 'nyc' and (age > 40 or id = 5) order by id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(2)}, {int64(5)}}) {
		t.Fatalf("got %v", res.Rows)
	}
	// NOT.
	res = exec(t, d, `select id from users where not (city = 'london' or city = 'nyc') order by id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(4)}}) {
		t.Fatalf("got %v", res.Rows)
	}
	// A pushable conjunct beside an OR narrows the scan but keeps OR
	// semantics.
	res = exec(t, d, `select id from users where id >= 3 and (city = 'nyc' or age = 41) order by id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(3)}, {int64(5)}}) {
		t.Fatalf("got %v", res.Rows)
	}
	// UPDATE/DELETE take boolean expressions too.
	if res := exec(t, d, `update users set city = 'x' where id = 1 or id = 4`); res.RowsAffected != 2 {
		t.Fatalf("update affected %d", res.RowsAffected)
	}
	if res := exec(t, d, `delete from users where not city = 'x'`); res.RowsAffected != 3 {
		t.Fatalf("delete affected %d", res.RowsAffected)
	}
}

func TestSQLThreeValuedLogic(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, v int)`)
	exec(t, d, `insert into t (id, v) values (1, 1), (2, 2), (3, null)`)

	// NOT over an unknown comparison stays unknown: the NULL row never
	// matches, in either direction.
	if res := exec(t, d, `select id from t where not v = 1 order by id`); !reflect.DeepEqual(res.Rows, [][]any{{int64(2)}}) {
		t.Fatalf("got %v", res.Rows)
	}
	// The classic: v = 1 OR v != 1 still excludes NULL.
	if res := exec(t, d, `select id from t where v = 1 or v != 1 order by id`); len(res.Rows) != 2 {
		t.Fatalf("got %v", res.Rows)
	}
	// Unknown OR true is true.
	if res := exec(t, d, `select id from t where v = 99 or id = 3`); !reflect.DeepEqual(res.Rows, [][]any{{int64(3)}}) {
		t.Fatalf("got %v", res.Rows)
	}
	// NOT (v IS NULL) is definite, not unknown.
	if res := exec(t, d, `select id from t where not v is null order by id`); len(res.Rows) != 2 {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestSQLAggregates(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	res := exec(t, d, `select count(*), min(age), max(age), sum(age), avg(age) from users`)
	if !reflect.DeepEqual(res.Cols, []string{"count(*)", "min(age)", "max(age)", "sum(age)", "avg(age)"}) {
		t.Fatalf("cols: %v", res.Cols)
	}
	if !reflect.DeepEqual(res.Types, []bytdb.ColType{bytdb.TInt, bytdb.TInt, bytdb.TInt, bytdb.TInt, bytdb.TFloat}) {
		t.Fatalf("types: %v", res.Types)
	}
	want := [][]any{{int64(5), int64(28), int64(45), int64(189), 37.8}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}

	// GROUP BY: groups come back in ascending group-column order.
	res = exec(t, d, `select city, count(*), max(age) from users group by city`)
	want = [][]any{
		{"austin", int64(1), int64(39)},
		{"london", int64(2), int64(41)},
		{"nyc", int64(2), int64(45)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}

	// HAVING filters groups; ORDER BY an aggregate.
	res = exec(t, d, `select city from users group by city having count(*) >= 2 order by count(*) desc, city`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"london"}, {"nyc"}}) {
		t.Fatalf("got %v", res.Rows)
	}

	// HAVING takes boolean expressions: OR over aggregates and group
	// columns.
	res = exec(t, d, `select city from users group by city having count(*) >= 2 or city = 'austin' order by city`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"austin"}, {"london"}, {"nyc"}}) {
		t.Fatalf("got %v", res.Rows)
	}
	res = exec(t, d, `select city from users group by city having not count(*) >= 2 order by city`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"austin"}}) {
		t.Fatalf("got %v", res.Rows)
	}

	// WHERE runs before grouping.
	res = exec(t, d, `select city, count(*) from users where age > 36 group by city`)
	want = [][]any{{"austin", int64(1)}, {"london", int64(1)}, {"nyc", int64(1)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestSQLAggregateNulls(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table m (id int primary key, grp text, v int)`)
	exec(t, d, `insert into m values (1, 'a', 10), (2, 'a', null), (3, null, 30), (4, null, null)`)

	// Aggregates ignore NULL inputs; COUNT(*) does not. NULL group
	// values form one group, sorted first (key order).
	res := exec(t, d, `select grp, count(*), count(v), sum(v) from m group by grp`)
	want := [][]any{
		{nil, int64(2), int64(1), int64(30)},
		{"a", int64(2), int64(1), int64(10)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}

	// Zero rows, no GROUP BY: one row; COUNT 0, the rest NULL.
	res = exec(t, d, `select count(*), sum(v), avg(v), min(v) from m where id > 100`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(0), nil, nil, nil}}) {
		t.Fatalf("got %v", res.Rows)
	}
	// Zero rows with GROUP BY: zero groups.
	if res := exec(t, d, `select grp, count(*) from m where id > 100 group by grp`); len(res.Rows) != 0 {
		t.Fatalf("got %v", res.Rows)
	}
	// HAVING without GROUP BY filters the single group.
	if res := exec(t, d, `select count(*) from m having count(*) > 100`); len(res.Rows) != 0 {
		t.Fatalf("got %v", res.Rows)
	}

	// Float aggregation and AVG over ints.
	exec(t, d, `create table f (id int primary key, x float)`)
	exec(t, d, `insert into f values (1, 1.5), (2, 2.5)`)
	res = exec(t, d, `select sum(x), avg(x), min(x) from f`)
	if !reflect.DeepEqual(res.Rows, [][]any{{4.0, 2.0, 1.5}}) {
		t.Fatalf("got %v", res.Rows)
	}
	// MIN/MAX work on strings too.
	res = exec(t, d, `select min(grp), max(grp) from m`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"a", "a"}}) {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestSQLAggregateErrors(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	for _, q := range []string{
		`select name from users group by city`,                 // name not grouped
		`select * from users group by city`,                    // star in aggregate query
		`select sum(name) from users`,                          // non-numeric SUM
		`select city from users group by city order by age`,    // ungrouped sort column
		`select count(*) from users having name = 'x'`,         // ungrouped HAVING column
		`select count(nope) from users`,                        // unknown column
		`select city, count(*) from users group by city, city`, // duplicate group col
	} {
		if _, err := d.Exec(q); err == nil {
			t.Errorf("Exec(%q): expected error", q)
		}
	}
}

func TestSQLParams(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	// Go integers normalize to int64 before comparison and storage.
	res, err := d.Exec(`select name from users where city = $1 and age > $2 order by name`, "london", 30)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Rows, [][]any{{"ada"}, {"alan"}}) {
		t.Fatalf("got %v", res.Rows)
	}

	// The same parameter may be used more than once.
	res, err = d.Exec(`select id from users where age = $1 or id = $1`, 41)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(3)}}) {
		t.Fatalf("got %v", res.Rows)
	}

	// INSERT, UPDATE, and DELETE take parameters; nil is SQL NULL.
	if res, err = d.Exec(`insert into users values ($1, $2, $3, $4)`, 6, "linus", 55, nil); err != nil || res.RowsAffected != 1 {
		t.Fatalf("insert: %v %v", res, err)
	}
	res, err = d.Exec(`select name from users where id = $1 and city is null`, 6)
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != "linus" {
		t.Fatalf("got %v, %v", res, err)
	}
	if res, err = d.Exec(`update users set city = $1 where id = $2`, "helsinki", 6); err != nil || res.RowsAffected != 1 {
		t.Fatalf("update: %v %v", res, err)
	}
	if res, err = d.Exec(`delete from users where id = $1`, 6); err != nil || res.RowsAffected != 1 {
		t.Fatalf("delete: %v %v", res, err)
	}

	// A nil parameter in a comparison is NULL: unknown, never a match.
	res, err = d.Exec(`select id from users where city = $1`, nil)
	if err != nil || len(res.Rows) != 0 {
		t.Fatalf("got %v, %v", res, err)
	}

	// Parameters bind in join ON conditions and in HAVING.
	exec(t, d, `create table orders (id int primary key, user_id int, total int)`)
	exec(t, d, `insert into orders values (1, 1, 40), (2, 1, 60), (3, 2, 90)`)
	res, err = d.Exec(`select u.name, o.total from users u join orders o
		on u.id = o.user_id and o.total > $1 order by o.total`, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Rows, [][]any{{"ada", int64(60)}, {"grace", int64(90)}}) {
		t.Fatalf("got %v", res.Rows)
	}
	res, err = d.Exec(`select city, count(*) from users group by city having count(*) >= $1 order by city`, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Rows, [][]any{{"london", int64(2)}, {"nyc", int64(2)}}) {
		t.Fatalf("got %v", res.Rows)
	}

	// Arity and argument types are checked.
	for _, bad := range []func() (*Result, error){
		func() (*Result, error) { return d.Exec(`select * from users where id = $1`) },
		func() (*Result, error) { return d.Exec(`select * from users where id = $1`, 1, 2) },
		func() (*Result, error) { return d.Exec(`select * from users`, 1) },
		func() (*Result, error) { return d.Exec(`select * from users where id = $1`, struct{}{}) },
	} {
		if _, err := bad(); err == nil {
			t.Error("expected error")
		}
	}
}

func TestSQLPrepare(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	st, err := d.Prepare(`select name from users where city = $1 order by name`)
	if err != nil {
		t.Fatal(err)
	}
	if st.NumParams() != 1 {
		t.Fatalf("NumParams: got %d", st.NumParams())
	}
	london := [][]any{{"ada"}, {"alan"}}
	res, err := st.Exec("london")
	if err != nil || !reflect.DeepEqual(res.Rows, london) {
		t.Fatalf("got %v, %v", res, err)
	}
	res, err = st.Exec("nyc")
	if err != nil || !reflect.DeepEqual(res.Rows, [][]any{{"barbara"}, {"grace"}}) {
		t.Fatalf("got %v, %v", res, err)
	}
	// Re-binding starts from the parsed form: earlier values don't stick.
	res, err = st.Exec("london")
	if err != nil || !reflect.DeepEqual(res.Rows, london) {
		t.Fatalf("rerun got %v, %v", res, err)
	}
	if _, err := st.Exec(); err == nil {
		t.Error("expected arity error")
	}

	ins, err := d.Prepare(`insert into users values ($1, $2, $3, $4)`)
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"ken", "dennis"} {
		if res, err := ins.Exec(10+i, name, 70, "murray hill"); err != nil || res.RowsAffected != 1 {
			t.Fatalf("insert %s: %v %v", name, res, err)
		}
	}
	res = exec(t, d, `select count(*) from users where city = 'murray hill'`)
	if res.Rows[0][0] != int64(2) {
		t.Fatalf("got %v", res.Rows)
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
