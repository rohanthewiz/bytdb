package sql

import (
	"reflect"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// seedShop: users(1 ada, 2 grace, 3 alan), orders referencing them;
// user 3 has no orders, order 40 has a NULL user_id.
func seedShop(t *testing.T, d *DB) {
	t.Helper()
	exec(t, d, `create table users (id int primary key, name text, city text)`)
	exec(t, d, `insert into users values
		(1, 'ada', 'london'), (2, 'grace', 'nyc'), (3, 'alan', 'london')`)
	exec(t, d, `create table orders (id int primary key, user_id int, total int)`)
	exec(t, d, `insert into orders values
		(10, 1, 250), (20, 1, 75), (30, 2, 120), (40, null, 999)`)
}

func TestSQLInnerJoin(t *testing.T) {
	d := openDB(t)
	seedShop(t, d)

	res := exec(t, d, `select u.name, o.total from users u
		join orders o on u.id = o.user_id
		order by o.total desc`)
	want := [][]any{{"ada", int64(250)}, {"grace", int64(120)}, {"ada", int64(75)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}
	if !reflect.DeepEqual(res.Cols, []string{"name", "total"}) {
		t.Fatalf("cols: %v", res.Cols)
	}

	// The NULL user_id never matches (three-valued ON).
	res = exec(t, d, `select count(*) from users u join orders o on u.id = o.user_id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(3)}}) {
		t.Fatalf("got %v", res.Rows)
	}

	// WHERE conjuncts on either side apply; ON plus WHERE compose.
	res = exec(t, d, `select o.id from users u join orders o on u.id = o.user_id
		where u.city = 'london' and o.total > 100`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(10)}}) {
		t.Fatalf("got %v", res.Rows)
	}

	// SELECT * over a join concatenates all columns in FROM order.
	res = exec(t, d, `select * from users u join orders o on u.id = o.user_id limit 1`)
	if !reflect.DeepEqual(res.Cols, []string{"id", "name", "city", "id", "user_id", "total"}) {
		t.Fatalf("cols: %v", res.Cols)
	}
	if !reflect.DeepEqual(res.Types, []bytdb.ColType{
		bytdb.TInt, bytdb.TString, bytdb.TString, bytdb.TInt, bytdb.TInt, bytdb.TInt,
	}) {
		t.Fatalf("types: %v", res.Types)
	}

	// t.* projects one table's columns.
	res = exec(t, d, `select o.*, u.name from users u join orders o on u.id = o.user_id
		where o.id = 30`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(30), int64(2), int64(120), "grace"}}) {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestSQLLeftJoin(t *testing.T) {
	d := openDB(t)
	seedShop(t, d)

	// alan has no orders: NULL-extended.
	res := exec(t, d, `select u.name, o.id from users u
		left join orders o on u.id = o.user_id
		order by u.name, o.id`)
	want := [][]any{
		{"ada", int64(10)}, {"ada", int64(20)}, {"alan", nil}, {"grace", int64(30)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}

	// The anti-join: WHERE on the right side runs after NULL extension.
	res = exec(t, d, `select u.name from users u
		left join orders o on u.id = o.user_id
		where o.id is null`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"alan"}}) {
		t.Fatalf("got %v", res.Rows)
	}

	// An extra ON conjunct restricts matches, not left rows.
	res = exec(t, d, `select u.name, o.id from users u
		left join orders o on u.id = o.user_id and o.total > 100
		order by u.name, o.id`)
	want = [][]any{{"ada", int64(10)}, {"alan", nil}, {"grace", int64(30)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}

	// A WHERE conjunct on the left table still narrows the outer scan.
	res = exec(t, d, `select u.name, o.id from users u
		left join orders o on u.id = o.user_id
		where u.id = 3`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"alan", nil}}) {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestSQLCrossAndCommaJoin(t *testing.T) {
	d := openDB(t)
	seedShop(t, d)

	res := exec(t, d, `select count(*) from users cross join orders`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(12)}}) {
		t.Fatalf("got %v", res.Rows)
	}
	// A comma join with an equi-WHERE behaves as an inner join.
	res = exec(t, d, `select u.name, o.id from users u, orders o
		where u.id = o.user_id order by o.id`)
	want := [][]any{{"ada", int64(10)}, {"ada", int64(20)}, {"grace", int64(30)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestSQLSelfJoin(t *testing.T) {
	d := openDB(t)
	seedShop(t, d)

	// Pairs of distinct users in the same city.
	res := exec(t, d, `select a.name, b.name from users a
		join users b on a.city = b.city and a.id < b.id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"ada", "alan"}}) {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestSQLThreeTableJoin(t *testing.T) {
	d := openDB(t)
	seedShop(t, d)
	exec(t, d, `create table cities (name text primary key, country text)`)
	exec(t, d, `insert into cities values ('london', 'uk'), ('nyc', 'us')`)

	res := exec(t, d, `select distinct_marker.name, c.country from users distinct_marker
		join orders o on distinct_marker.id = o.user_id
		join cities c on c.name = distinct_marker.city
		where o.total > 100 order by distinct_marker.name`)
	want := [][]any{{"ada", "uk"}, {"grace", "us"}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestSQLJoinAggregates(t *testing.T) {
	d := openDB(t)
	seedShop(t, d)

	// Per-user order stats over a LEFT JOIN: COUNT(o.id) ignores the
	// NULL extension, COUNT(*) does not.
	res := exec(t, d, `select u.name, count(o.id), sum(o.total) from users u
		left join orders o on u.id = o.user_id
		group by u.name order by u.name`)
	want := [][]any{
		{"ada", int64(2), int64(325)},
		{"alan", int64(0), nil},
		{"grace", int64(1), int64(120)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}

	// HAVING can compare two aggregates.
	res = exec(t, d, `select u.city, count(*) from users u
		join orders o on u.id = o.user_id
		group by u.city having count(o.id) = count(*) order by u.city`)
	want = [][]any{{"london", int64(2)}, {"nyc", int64(1)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestSQLColVsCol(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table p (id int primary key, lo int, hi int)`)
	exec(t, d, `insert into p values (1, 3, 5), (2, 7, 7), (3, 9, 4), (4, null, 1)`)

	res := exec(t, d, `select id from p where lo < hi`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}}) {
		t.Fatalf("got %v", res.Rows)
	}
	// NULL against a column is unknown, and NOT keeps it unknown.
	res = exec(t, d, `select id from p where not lo <= hi order by id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(3)}}) {
		t.Fatalf("got %v", res.Rows)
	}
}

// TestJoinPushdown pins the nested-loop planning: ON equality binds
// the inner table to the outer row (index nested-loop), WHERE
// conjuncts push into their own table's scan — except into the
// NULL-extended side of a LEFT JOIN, where they must stay post-join.
func TestJoinPushdown(t *testing.T) {
	d := openDB(t)
	seedShop(t, d)
	prep := func(q string) *fromPlan {
		t.Helper()
		s := mustParse(t, q).(*Select)
		var fp *fromPlan
		if err := d.e.ReadTxn(func(tx *bytdb.Txn) error {
			var err error
			fp, err = prepareFrom(d.lookup(tx.Table), s.From, s.Where)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return fp
	}

	fp := prep(`select u.name from users u join orders o on u.id = o.user_id
		where u.city = 'london' and o.total > 100`)
	if len(fp.steps[0].static) != 1 || len(fp.steps[0].tmpls) != 0 {
		t.Fatalf("step 0: %+v", fp.steps[0])
	}
	if len(fp.steps[1].static) != 1 { // o.total > 100
		t.Fatalf("step 1 static: %+v", fp.steps[1].static)
	}
	if len(fp.steps[1].tmpls) != 1 || fp.steps[1].tmpls[0].op != OpEQ ||
		fp.steps[1].tmpls[0].srcOrd != 0 || fp.steps[1].tmpls[0].item.Col.Name != "user_id" {
		t.Fatalf("step 1 tmpls: %+v", fp.steps[1].tmpls)
	}

	// LEFT JOIN: the ON template still binds, but the WHERE conjunct
	// on orders must not enter the scan.
	fp = prep(`select u.name from users u left join orders o on u.id = o.user_id
		where o.total is null`)
	if len(fp.steps[1].static) != 0 || len(fp.steps[1].tmpls) != 1 {
		t.Fatalf("left step 1: %+v", fp.steps[1])
	}
}

func TestSQLJoinErrors(t *testing.T) {
	d := openDB(t)
	seedShop(t, d)
	for _, q := range []string{
		`select id from users u join orders o on u.id = o.user_id`,                                  // ambiguous id
		`select x.name from users u join orders o on u.id = o.user_id`,                              // unknown alias
		`select u.nope from users u join orders o on u.id = o.user_id`,                              // unknown column
		`select * from users join users on users.id = users.id`,                                     // dup table, no alias
		`select * from users u join orders o on u.id = x.id`,                                        // unknown alias in ON
		`select u.name from users u join orders o on u.id = c.user_id join orders c on c.id = o.id`, // ON sees later table
		`select o.* from users u join orders o on u.id = o.user_id group by o.id`,                   // t.* in aggregate
	} {
		if _, err := d.Exec(q); err == nil {
			t.Errorf("Exec(%q): expected error", q)
		}
	}
}
