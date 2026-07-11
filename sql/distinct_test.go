package sql

import (
	"reflect"
	"strings"
	"testing"
)

// SELECT DISTINCT: projected rows dedup before ORDER BY, OFFSET, and
// LIMIT apply. ORDER BY is restricted to output columns and positions
// (Postgres's rule — a dropped sort key would decide which duplicate
// survives). DISTINCT composes with aggregates, windows, UNION arms,
// and the subquery evaluators.

func distinctDB(t *testing.T) *DB {
	t.Helper()
	d := openDB(t)
	exec(t, d, `create table emp (id int primary key, dept text, sal int)`)
	exec(t, d, `insert into emp values
		(1, 'eng', 100), (2, 'eng', 200), (3, 'ops', 100),
		(4, 'ops', 100), (5, 'sales', 300), (6, null, 100), (7, null, 200)`)
	return d
}

func TestSelectDistinct(t *testing.T) {
	d := distinctDB(t)

	// Single column: duplicates collapse, NULLs collapse to one NULL
	// (dedup treats NULL = NULL, as in Postgres).
	res := exec(t, d, `select distinct dept from emp order by dept`)
	want := [][]any{{"eng"}, {"ops"}, {"sales"}, {nil}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("distinct dept: %v", res.Rows)
	}

	// Multi-column: the combination is what dedups.
	res = exec(t, d, `select distinct dept, sal from emp order by dept, sal`)
	want = [][]any{{"eng", int64(100)}, {"eng", int64(200)}, {"ops", int64(100)},
		{"sales", int64(300)}, {nil, int64(100)}, {nil, int64(200)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("distinct dept, sal: %v", res.Rows)
	}

	// Expression items dedup on their computed value; ORDER BY takes
	// the output alias and positions, ASC and DESC.
	res = exec(t, d, `select distinct sal / 100 as bucket from emp order by bucket desc`)
	want = [][]any{{int64(3)}, {int64(2)}, {int64(1)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("distinct expression: %v", res.Rows)
	}
	res = exec(t, d, `select distinct sal from emp order by 1 limit 2 offset 1`)
	want = [][]any{{int64(200)}, {int64(300)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("order/offset/limit after dedup: %v", res.Rows)
	}

	// Without ORDER BY, first occurrence survives in scan order, and
	// LIMIT counts distinct rows, not input rows.
	res = exec(t, d, `select distinct sal from emp limit 2`)
	want = [][]any{{int64(100)}, {int64(200)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("limit counts distinct rows: %v", res.Rows)
	}

	// SELECT ALL is the explicit spelling of the default.
	res = exec(t, d, `select all sal from emp where dept = 'ops'`)
	if len(res.Rows) != 2 {
		t.Fatalf("select all: %v", res.Rows)
	}
}

func TestSelectDistinctShapes(t *testing.T) {
	d := distinctDB(t)

	// Over an aggregate query: equal group results collapse.
	res := exec(t, d, `select distinct max(sal) from emp group by dept order by 1`)
	want := [][]any{{int64(100)}, {int64(200)}, {int64(300)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("distinct over groups: %v", res.Rows)
	}

	// Over a window query: one value per partition collapses to the
	// distinct set.
	res = exec(t, d, `select distinct count(*) over (partition by dept) from emp order by 1`)
	want = [][]any{{int64(1)}, {int64(2)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("distinct over window: %v", res.Rows)
	}

	// A DISTINCT arm of a UNION ALL dedups itself; the other arm's
	// duplicates survive.
	res = exec(t, d, `select distinct sal from emp where dept = 'ops'
		union all select sal from emp where dept = 'eng' order by sal`)
	want = [][]any{{int64(100)}, {int64(100)}, {int64(200)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("distinct union arm: %v", res.Rows)
	}

	// A scalar subquery: duplicates collapse before the one-row rule.
	res = exec(t, d, `select (select distinct sal from emp where dept = 'ops')`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(100)}}) {
		t.Fatalf("distinct scalar subquery: %v", res.Rows)
	}
	// ...and two distinct values still error, as they should.
	if _, err := d.Exec(`select (select distinct sal from emp where dept = 'eng')`); err == nil ||
		!strings.Contains(err.Error(), "more than one row") {
		t.Fatalf("distinct scalar subquery, two values: %v", err)
	}

	// ANY over a DISTINCT subquery matches what the plain form matches.
	res = exec(t, d, `select id from emp where sal = any(select distinct sal from emp where dept = 'ops') order by id`)
	want = [][]any{{int64(1)}, {int64(3)}, {int64(4)}, {int64(6)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("any distinct: %v", res.Rows)
	}

	// ARRAY(SELECT DISTINCT ...) dedups its elements.
	res = exec(t, d, `select array(select distinct dept from emp where dept is not null order by 1)`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"{eng,ops,sales}"}}) {
		t.Fatalf("array distinct: %v", res.Rows)
	}

	// No FROM: trivially one row.
	res = exec(t, d, `select distinct 1`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}}) {
		t.Fatalf("distinct literal: %v", res.Rows)
	}
}

func TestSelectDistinctErrors(t *testing.T) {
	d := distinctDB(t)

	// ORDER BY must name an output column or position — a FROM column
	// the projection dropped cannot order a deduped result.
	for _, q := range []string{
		`select distinct dept from emp order by sal`,
		`select distinct dept from emp order by sal + 1`,
		`select distinct dept from emp order by emp.dept`,
	} {
		if _, err := d.Exec(q); err == nil ||
			!strings.Contains(err.Error(), "ORDER BY expressions must appear in select list") {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := d.Exec(`select distinct dept from emp order by 3`); err == nil ||
		!strings.Contains(err.Error(), "ORDER BY position is not in the select list") {
		t.Fatalf("bad position: %v", err)
	}
	// DISTINCT ON is a different feature (keep-first-per-group); it is
	// rejected outright rather than misread.
	if _, err := d.Exec(`select distinct on (dept) dept, sal from emp`); err == nil ||
		!strings.Contains(err.Error(), "SELECT DISTINCT ON is not supported") {
		t.Fatalf("distinct on: %v", err)
	}
}

func TestSelectDistinctExplain(t *testing.T) {
	d := distinctDB(t)

	// The Unique node sits between the Sort (post-dedup) and the scan.
	res := exec(t, d, `explain select distinct dept from emp order by dept limit 2`)
	var lines []string
	for _, r := range res.Rows {
		lines = append(lines, r[0].(string))
	}
	plan := strings.Join(lines, "\n")
	for _, node := range []string{"Limit", "Sort", "Unique", "Seq Scan on emp"} {
		if !strings.Contains(plan, node) {
			t.Fatalf("plan missing %s:\n%s", node, plan)
		}
	}
	if strings.Index(plan, "Sort") > strings.Index(plan, "Unique") {
		t.Fatalf("Sort should sit above Unique:\n%s", plan)
	}
}
