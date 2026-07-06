package sql

import (
	"reflect"
	"testing"
)

// ids runs a query whose first column is an int and returns the values
// in result order.
func ids(t *testing.T, d *DB, q string) []int64 {
	t.Helper()
	res := exec(t, d, q)
	out := make([]int64, len(res.Rows))
	for i, r := range res.Rows {
		out[i] = r[0].(int64)
	}
	return out
}

func wantIDs(t *testing.T, d *DB, q string, want ...int64) {
	t.Helper()
	if got := ids(t, d, q); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: got %v want %v", q, got, want)
	}
}

// TestOrderPrimaryScan: ORDER BY the primary key reads the primary
// index directly — ascending forward, descending backward — and drops
// the sort. A LIMIT then bounds the scan.
func TestOrderPrimaryScan(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	wantPlan(t, d, `explain select id from users order by id`,
		`Index Scan using users_pkey on users`)
	wantPlan(t, d, `explain select id from users order by id limit 3`,
		`Limit`,
		`  ->  Index Scan using users_pkey on users`)
	wantPlan(t, d, `explain select id from users order by id desc limit 3`,
		`Limit`,
		`  ->  Index Scan Backward using users_pkey on users`)

	wantIDs(t, d, `select id from users order by id`, 1, 2, 3, 4, 5)
	wantIDs(t, d, `select id from users order by id limit 3`, 1, 2, 3)
	wantIDs(t, d, `select id from users order by id desc`, 5, 4, 3, 2, 1)
	wantIDs(t, d, `select id from users order by id desc limit 3`, 5, 4, 3)
	wantIDs(t, d, `select id from users order by id desc offset 1 limit 2`, 4, 3)

	// A forward range bound is compatible with ascending order; the
	// pushed lower bound and the ordered walk coexist, sort dropped.
	wantPlan(t, d, `explain select id from users where id > 2 order by id`,
		`Index Scan using users_pkey on users`,
		`  Index Cond: (id > 2)`)
	wantIDs(t, d, `select id from users where id > 2 order by id`, 3, 4, 5)

	// A bounded scan cannot be run backward (deferred), so ORDER BY id
	// DESC with a lower bound still sorts — but the answer is the same.
	wantPlan(t, d, `explain select id from users where id > 2 order by id desc`,
		`Sort`,
		`  Sort Key: id DESC`,
		`  ->  Index Scan using users_pkey on users`,
		`        Index Cond: (id > 2)`)
	wantIDs(t, d, `select id from users where id > 2 order by id desc`, 5, 4, 3)
}

// TestOrderIndexScan: with no WHERE and a LIMIT, ORDER BY a NOT NULL
// indexed column is served by an ordered index walk instead of a full
// scan plus sort. A DESC index serves ORDER BY ... DESC forward; an
// ASC index serves it backward.
func TestOrderIndexScan(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, a int not null, b int not null, note text)`)
	exec(t, d, `insert into t values
		(1, 20, 0, 'x'), (2, 10, 0, 'y'), (3, 30, 0, 'z'), (4, 40, 0, 'w'), (5, 5, 0, 'q')`)
	exec(t, d, `create index by_a on t (a)`)

	wantPlan(t, d, `explain select id from t order by a limit 3`,
		`Limit`,
		`  ->  Index Scan using by_a on t`)
	wantPlan(t, d, `explain select id from t order by a desc limit 3`,
		`Limit`,
		`  ->  Index Scan Backward using by_a on t`)

	// a: id5=5,id2=10,id1=20,id3=30,id4=40.
	wantIDs(t, d, `select id from t order by a limit 3`, 5, 2, 1)
	wantIDs(t, d, `select id from t order by a desc limit 3`, 4, 3, 1)

	// Without a LIMIT the full scan + sort is kept (no ordered-walk win).
	wantPlan(t, d, `explain select id from t order by a`,
		`Sort`,
		`  Sort Key: a`,
		`  ->  Seq Scan on t`)
	wantIDs(t, d, `select id from t order by a`, 5, 2, 1, 3, 4)
}

// TestOrderCompositeIndex: an equality prefix pins the leading index
// column, so ORDER BY the next column is already in order — the sort
// drops even though the ORDER BY names a different column than the
// index leader.
func TestOrderCompositeIndex(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, a int not null, b int not null)`)
	exec(t, d, `insert into t values
		(1, 2, 30), (2, 1, 10), (3, 2, 20), (4, 1, 40), (5, 2, 25)`)
	exec(t, d, `create index by_ab on t (a, b)`)

	wantPlan(t, d, `explain select id from t where a = 2 order by b limit 5`,
		`Limit`,
		`  ->  Index Scan using by_ab on t`,
		`        Index Cond: (a = 2)`)
	wantIDs(t, d, `select id from t where a = 2 order by b`, 3, 5, 1)         // b: 20,25,30
	wantIDs(t, d, `select id from t where a = 2 order by a, b`, 3, 5, 1)      // leading eq col ignored
	wantIDs(t, d, `select id from t where a = 2 order by b desc`, 1, 5, 3)   // bounded reverse: sorts, still right
}

// TestOrderDescIndex: a DESC index serves ORDER BY ... DESC with a
// plain forward walk (its keys are stored inverted).
func TestOrderDescIndex(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, a int not null)`)
	exec(t, d, `insert into t values (1, 20), (2, 10), (3, 30), (4, 5), (5, 25)`)
	exec(t, d, `create index by_a_desc on t (a desc)`)

	wantPlan(t, d, `explain select id from t order by a desc limit 3`,
		`Limit`,
		`  ->  Index Scan using by_a_desc on t`)
	wantIDs(t, d, `select id from t order by a desc limit 3`, 3, 5, 1) // 30,25,20
	// ORDER BY a ASC over a DESC index needs the walk reversed.
	wantPlan(t, d, `explain select id from t order by a limit 3`,
		`Limit`,
		`  ->  Index Scan Backward using by_a_desc on t`)
	wantIDs(t, d, `select id from t order by a limit 3`, 4, 2, 1) // 5,10,20
}

// TestOrderNullableNotOptimized: a nullable column's index orders NULLs
// opposite to ORDER BY's NULLS LAST/FIRST, so it is never used to serve
// the order — the sort stays and NULLs land correctly.
func TestOrderNullableNotOptimized(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, a int)`) // a is nullable
	exec(t, d, `insert into t values (1, 2), (2, null), (3, 1), (4, null), (5, 3)`)
	exec(t, d, `create index by_a on t (a)`)

	// Not eligible: still a seq scan + sort.
	wantPlan(t, d, `explain select id from t order by a limit 10`,
		`Limit`,
		`  ->  Sort`,
		`        Sort Key: a`,
		`        ->  Seq Scan on t`)
	// NULLS LAST for ascending, NULLS FIRST for descending.
	wantIDs(t, d, `select id from t order by a`, 3, 1, 5, 2, 4)
	wantIDs(t, d, `select id from t order by a desc`, 2, 4, 5, 1, 3)
}

// TestOrderExprNotOptimized: ORDER BY an expression (not a base column)
// is not served from the scan.
func TestOrderExprNotOptimized(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	wantPlan(t, d, `explain select id from users order by id + 1 limit 3`,
		`Limit`,
		`  ->  Sort`,
		`        Sort Key: (id + 1)`,
		`        ->  Seq Scan on users`)
	wantIDs(t, d, `select id from users order by id + 1 desc limit 2`, 5, 4)
}

// TestOrderJoinNotOptimized: with a join the combined order is not the
// scan order, so the sort is kept.
func TestOrderJoinNotOptimized(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	exec(t, d, `create table orders (id int primary key, user_id int, total float)`)
	exec(t, d, `insert into orders values (1, 1, 5.0), (2, 2, 9.0)`)
	got := explainLines(t, d, `explain select u.id from users u join orders o on o.user_id = u.id order by u.id`)
	if got[0] != "Sort" {
		t.Fatalf("join ORDER BY should sort, got: %v", got)
	}
}
