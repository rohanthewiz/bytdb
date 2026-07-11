package sql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// Window functions with GROUP BY / aggregates: windows evaluate after
// grouping and HAVING, so their inputs are the surviving groups and
// their arguments may reference group keys and aggregate results.
// seedUsers groups by city: austin sum(age)=39 count=1, london 77/2,
// nyc 73/2.

// TestWindowOverGroups covers the core shapes: ranking groups by an
// aggregate, running aggregates of aggregates, LAG across groups, and
// a window keyed on a group key.
func TestWindowOverGroups(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	res := exec(t, d, `select city, sum(age),
		rank() over (order by sum(age) desc) as r
		from users group by city order by r`)
	want := [][]any{
		{"london", int64(77), int64(1)},
		{"nyc", int64(73), int64(2)},
		{"austin", int64(39), int64(3)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("rank over sums: %v", res.Rows)
	}
	if !reflect.DeepEqual(res.Types, []bytdb.ColType{bytdb.TString, bytdb.TInt, bytdb.TInt}) {
		t.Fatalf("types: %v", res.Types)
	}

	// SUM(SUM(x)) OVER: the inner SUM aggregates each group, the outer
	// accumulates across groups (the default frame runs city-ascending).
	res = exec(t, d, `select city, sum(sum(age)) over (order by city)
		from users group by city order by city`)
	want = [][]any{
		{"austin", int64(39)},
		{"london", int64(116)},
		{"nyc", int64(189)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("running sum of sums: %v", res.Rows)
	}

	// LAG reads the previous group; the first group falls off the edge.
	res = exec(t, d, `select city, lag(sum(age)) over (order by city)
		from users group by city order by city`)
	want = [][]any{
		{"austin", nil},
		{"london", int64(39)},
		{"nyc", int64(77)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("lag over groups: %v", res.Rows)
	}

	// PARTITION BY an aggregate: groups partition by their row counts
	// (austin alone at 1; london and nyc share 2).
	res = exec(t, d, `select city, count(*) over (partition by count(*))
		from users group by city order by city`)
	want = [][]any{
		{"austin", int64(1)},
		{"london", int64(2)},
		{"nyc", int64(2)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("partition by aggregate: %v", res.Rows)
	}

	// Explicit frames compose: a sliding ROWS frame over the groups.
	res = exec(t, d, `select city, sum(sum(age)) over (order by city
		rows between 1 preceding and current row)
		from users group by city order by city`)
	want = [][]any{
		{"austin", int64(39)},
		{"london", int64(116)},
		{"nyc", int64(150)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("sliding frame over groups: %v", res.Rows)
	}

	// Value functions take aggregate arguments too.
	res = exec(t, d, `select city, first_value(sum(age)) over (order by city)
		from users group by city order by city`)
	want = [][]any{
		{"austin", int64(39)},
		{"london", int64(39)},
		{"nyc", int64(39)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("first_value of sums: %v", res.Rows)
	}
}

// TestWindowOverGroupsShapes covers HAVING running before window
// assignment, the implicit single group, windows living only in ORDER
// BY, and EXPLAIN's node order.
func TestWindowOverGroupsShapes(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	// HAVING drops austin before ranking, so the surviving groups
	// rank 1..2 — not 1 and 2 of 3.
	res := exec(t, d, `select city, rank() over (order by sum(age) desc)
		from users group by city having count(*) > 1 order by 2`)
	want := [][]any{
		{"london", int64(1)},
		{"nyc", int64(2)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("having before windows: %v", res.Rows)
	}

	// An aggregate without GROUP BY is one group; the window sees one
	// row.
	res = exec(t, d, `select count(*), rank() over (order by sum(age)) from users`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(5), int64(1)}}) {
		t.Fatalf("single group: %v", res.Rows)
	}

	// A window only in ORDER BY still forces the post-group pass.
	res = exec(t, d, `select city from users group by city
		order by rank() over (order by sum(age) desc)`)
	want = [][]any{{"london"}, {"nyc"}, {"austin"}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("window in ORDER BY only: %v", res.Rows)
	}

	// EXPLAIN: WindowAgg sits above the aggregation, as execution runs.
	lines := explainLines(t, d, `explain select city,
		rank() over (order by sum(age) desc) from users group by city`)
	txt := strings.Join(lines, "\n")
	wi, ai := strings.Index(txt, "WindowAgg"), strings.Index(txt, "HashAggregate")
	if wi < 0 || ai < 0 || wi > ai {
		t.Fatalf("explain node order:\n%s", txt)
	}
}

// TestWindowOverGroupsErrors: ungrouped columns inside a window's
// expressions get the classic GROUP BY error, and aggregates cannot
// consume window results.
func TestWindowOverGroupsErrors(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	for _, c := range []struct{ q, want string }{
		{`select city, rank() over (order by age) from users group by city`,
			`column "age" must appear in the GROUP BY clause`},
		{`select rank() over (partition by name) from users group by city`,
			`column "name" must appear in the GROUP BY clause`},
		{`select sum(age), avg(age) over () from users group by city`,
			`column "age" must appear in the GROUP BY clause`},
		{`select sum(rank() over ()) from users group by city`,
			"aggregate function calls cannot contain window function calls"},
	} {
		if _, err := d.Exec(c.q); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want containing %q", c.q, err, c.want)
		}
	}
}

// DISTINCT in aggregate windows (a bytdb extension — Postgres does not
// implement it) dedups within each row's frame. seedFrames' k column
// repeats (1,1,2,3), so distinct and plain counts differ.

func TestWindowDistinct(t *testing.T) {
	d := openDB(t)
	seedFrames(t, d)

	res := exec(t, d, `select id,
		count(distinct k) over () as total,
		count(distinct k) over (order by id) as run,
		sum(distinct k) over (order by id) as sums,
		count(distinct k) over (order by id
			rows between unbounded preceding and unbounded following
			exclude current row) as excl
		from f order by id`)
	want := [][]any{
		{int64(1), int64(3), int64(1), int64(1), int64(3)},
		{int64(2), int64(3), int64(1), int64(1), int64(3)},
		{int64(3), int64(3), int64(2), int64(3), int64(2)},
		{int64(4), int64(3), int64(3), int64(6), int64(2)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("distinct windows: %v", res.Rows)
	}

	// AVG(DISTINCT) averages the deduped values: (1+2+3)/3.
	res = exec(t, d, `select avg(distinct k) over () from f limit 1`)
	if !reflect.DeepEqual(res.Rows, [][]any{{float64(2)}}) {
		t.Fatalf("avg distinct: %v", res.Rows)
	}

	// DISTINCT composes with the grouped-window path: group row counts
	// are 2,1,1 (k=1 holds two rows), so the distinct count of counts
	// is 2 for every group.
	res = exec(t, d, `select k, count(distinct count(*)) over ()
		from f group by k order by k`)
	want = [][]any{
		{int64(1), int64(2)},
		{int64(2), int64(2)},
		{int64(3), int64(2)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("distinct over groups: %v", res.Rows)
	}

	// EXPLAIN renders the DISTINCT.
	lines := explainLines(t, d, `explain select count(distinct k) over () from f`)
	if txt := strings.Join(lines, "\n"); !strings.Contains(txt, "count(DISTINCT k) OVER ()") {
		t.Fatalf("explain distinct:\n%s", txt)
	}
}
