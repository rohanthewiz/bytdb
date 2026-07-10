package sql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// TestWindowInContainers exercises rewriteWindows across the expression
// containers it mirrors from bindExpr: a window call nested inside CASE,
// arithmetic, casts, function args, IN lists, boolean logic, ANY, and
// ARRAY must all rewrite into exWinRef placeholders and evaluate.
func TestWindowInContainers(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	res := exec(t, d, `select name,
		row_number() over (order by age) + 100 as arith,
		case when rank() over (order by age) = 1 then 'first' else 'rest' end as cs,
		row_number() over (order by age) :: text as cst,
		coalesce(row_number() over (order by age), 0) as fn,
		row_number() over (order by age) in (1, 3) as inx,
		row_number() over (order by age) = any(array[row_number() over (order by age)]) as anyx,
		row_number() over (order by age) is null as isn,
		not (row_number() over (order by age) = 1) as notx,
		array[row_number() over (order by age)] as arr
		from users where id = 5`)
	want := [][]any{{"barbara", int64(101), "first", "1", int64(1), true, true, false, false, "{1}"}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}

	// AND/OR containers (nested under CASE WHEN so the boolean shapes
	// stay in expression form), and a window in ORDER BY.
	res = exec(t, d, `select name,
		case when row_number() over (order by age) = 1 and rank() over (order by age) = 1
			then 'one' when row_number() over (order by age) = 2 or 1 = 2 then 'two'
			else 'more' end as tag
		from users order by row_number() over (order by age desc)`)
	if len(res.Rows) != 5 || res.Rows[0][0] != "grace" || res.Rows[4][0] != "barbara" {
		t.Fatalf("order by window: %v", res.Rows)
	}
	if res.Rows[4][1] != "one" || res.Rows[3][1] != "two" || res.Rows[0][1] != "more" {
		t.Fatalf("case tags: %v", res.Rows)
	}
}

// TestWindowResultTypes locks in resultType across the aggregate window
// family: COUNT -> int, AVG -> float, MIN/MAX/SUM -> the argument's type.
func TestWindowResultTypes(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	res := exec(t, d, `select avg(age) over (partition by city) as a,
		count(age) over () as c,
		min(name) over (partition by city) as mn,
		max(age) over () as mx
		from users order by id`)
	wantTypes := []bytdb.ColType{bytdb.TFloat, bytdb.TInt, bytdb.TString, bytdb.TInt}
	if !reflect.DeepEqual(res.Types, wantTypes) {
		t.Fatalf("types %v", res.Types)
	}
	// Row 1 is ada (london): ages 36, 41 -> avg 38.5, min name "ada";
	// whole set: 5 rows, max age 45.
	if len(res.Rows) != 5 || !reflect.DeepEqual(res.Rows[0], []any{38.5, int64(5), "ada", int64(45)}) {
		t.Fatalf("got %v", res.Rows)
	}

	// Describe types a window query without executing it: exprType and
	// exprName take the ExWindow branches there.
	info := describe(t, d, `select rank() over (order by age), avg(age) over () from users`)
	if !reflect.DeepEqual(info.Cols, []string{"rank", "avg"}) ||
		!reflect.DeepEqual(info.Types, []bytdb.ColType{bytdb.TInt, bytdb.TFloat}) {
		t.Fatalf("describe window: %v %v", info.Cols, info.Types)
	}
}

// TestWindowNullPeers covers equalVals' NULL handling: NULL ORDER BY
// keys are peers of each other and of nothing else.
func TestWindowNullPeers(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table s (id int primary key, grp text, v int)`)
	exec(t, d, `insert into s values (1,'a',null),(2,'a',null),(3,'a',5),(4,'a',5),(5,'a',9)`)

	res := exec(t, d, `select id, rank() over (order by v) as r,
		sum(id) over (order by v) as run
		from s order by r, id`)
	// Ascending NULLs sort last: v=5 peers rank 1, v=9 rank 3, NULL peers rank 4.
	want := [][]any{
		{int64(3), int64(1), int64(7)}, {int64(4), int64(1), int64(7)},
		{int64(5), int64(3), int64(12)},
		{int64(1), int64(4), int64(15)}, {int64(2), int64(4), int64(15)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("null peers: %v", res.Rows)
	}
}

func TestWindowErrors(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	for _, c := range []struct{ q, want string }{
		// A window anywhere but the select list / ORDER BY errors at eval.
		{`select name from users where row_number() over (order by age) = 1`,
			"only allowed in the SELECT list and ORDER BY"},
		// Errors around the window phase surface: FROM resolution, the
		// row-materializing scan, projection, sort keys, and the final
		// per-row expression evaluation.
		{`select row_number() over () from missing`, "no such table"},
		{`select row_number() over () from users where age/0 = 1`, "division by zero"},
		{`select row_number() over (), nope from users`, "no such column"},
		{`select row_number() over () from users order by nope`, "no such column"},
		{`select row_number() over (), age/0 from users`, "division by zero"},
		// A window inside an array subscript rewrites (ExIndex container),
		// then the evaluator rejects the subscript itself.
		{`select (array[1,2])[row_number() over ()] from users`, "array subscripts are not supported"},
		{`select count(distinct age) over () from users`, "DISTINCT is not supported in a window function"},
		{`select sum(age) over (order by age, name range 1 preceding) from users`,
			"RANGE with offset PRECEDING/FOLLOWING requires exactly one ORDER BY column"},
		// Errors inside PARTITION BY / ORDER BY evaluation surface.
		{`select rank() over (partition by age/0 order by age) from users`, "division by zero"},
		{`select rank() over (order by age/0) from users`, "division by zero"},
		{`select sum(age) over (order by name/0) from users`, "arithmetic requires numeric operands"},
	} {
		if _, err := d.Exec(c.q); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want containing %q", c.q, err, c.want)
		}
	}
}

// TestWindowExplain covers windowText's rendering of every OVER clause
// shape: *, an argument, PARTITION BY, ORDER BY, and DESC.
func TestWindowExplain(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	res := exec(t, d, `explain select count(*) over (partition by city),
		sum(age) over (order by age desc),
		rank() over (partition by city order by age, name desc),
		row_number() over ()
		from users order by row_number() over ()`)
	var plan strings.Builder
	for _, r := range res.Rows {
		plan.WriteString(r[0].(string) + "\n")
	}
	for _, want := range []string{
		"WindowAgg",
		"Window: count(*) OVER (PARTITION BY city)",
		"Window: sum(age) OVER (ORDER BY age DESC)",
		"Window: rank() OVER (PARTITION BY city ORDER BY age, name DESC)",
		"Window: row_number() OVER ()",
	} {
		if !strings.Contains(plan.String(), want) {
			t.Errorf("plan missing %q:\n%s", want, plan.String())
		}
	}
}
