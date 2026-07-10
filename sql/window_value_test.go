package sql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// seedSeries builds a small two-partition table for the value-function
// tests: partition 'a' is 1,2,3 by ord; partition 'b' is 10,20. The v
// values are distinct within a partition so expected rows are exact.
func seedSeries(t *testing.T, d *DB) {
	t.Helper()
	exec(t, d, `create table s (id int primary key, grp text, ord int, v int)`)
	exec(t, d, `insert into s values
		(1,'a',1,100),(2,'a',2,200),(3,'a',3,300),
		(4,'b',1,10),(5,'b',2,20)`)
}

// TestLagLead covers the core LAG/LEAD semantics: implicit offset 1,
// partition edges yielding NULL, and the explicit default fallback.
func TestLagLead(t *testing.T) {
	d := openDB(t)
	seedSeries(t, d)

	res := exec(t, d, `select id,
		lag(v) over (partition by grp order by ord) as lg,
		lead(v) over (partition by grp order by ord) as ld,
		lag(v, 1, -1) over (partition by grp order by ord) as lgd
		from s order by id`)
	want := [][]any{
		{int64(1), nil, int64(200), int64(-1)},
		{int64(2), int64(100), int64(300), int64(100)},
		{int64(3), int64(200), nil, int64(200)},
		{int64(4), nil, int64(20), int64(-1)},
		{int64(5), int64(10), nil, int64(10)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("lag/lead: %v", res.Rows)
	}
}

// TestLagLeadOffsets covers explicit, negative (direction-flipping, as
// in Postgres), oversized, and NULL offsets, plus a per-row offset
// expression.
func TestLagLeadOffsets(t *testing.T) {
	d := openDB(t)
	seedSeries(t, d)

	res := exec(t, d, `select id,
		lag(v, 2) over (partition by grp order by ord) as lg2,
		lag(v, -1) over (partition by grp order by ord) as lgneg,
		lead(v, 100) over (partition by grp order by ord) as far,
		lag(v, null) over (partition by grp order by ord) as lgnull,
		lag(v, ord - 1) over (partition by grp order by ord) as first
		from s where grp = 'a' order by id`)
	want := [][]any{
		// lag -1 == lead 1; lag(v, ord-1) walks back to the first row.
		{int64(1), nil, int64(200), nil, nil, int64(100)},
		{int64(2), nil, int64(300), nil, nil, int64(100)},
		{int64(3), int64(100), nil, nil, nil, int64(100)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("offsets: %v", res.Rows)
	}
}

// TestFirstLastNthValue locks in default-frame semantics: FIRST_VALUE
// is the partition head; LAST_VALUE ends at the current row's last
// ORDER BY peer (the Postgres surprise), reaching the partition end
// only on the final peer group or when there is no ORDER BY.
func TestFirstLastNthValue(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table p (id int primary key, k int, v text)`)
	// Peer groups by k: {1,2} then {3} — ids 1,2 share k=1.
	exec(t, d, `insert into p values (1,1,'x'),(2,1,'y'),(3,2,'z')`)

	res := exec(t, d, `select id,
		first_value(v) over (order by k) as fv,
		last_value(v) over (order by k) as lv,
		last_value(v) over () as lvall,
		nth_value(v, 2) over (order by k) as nv2,
		nth_value(v, 3) over (order by k) as nv3
		from p order by id`)
	want := [][]any{
		// Rows 1,2 are peers: their frame is rows 1..2, so last_value
		// is 'y' and nth_value(3) is still out of frame.
		{int64(1), "x", "y", "z", "y", nil},
		{int64(2), "x", "y", "z", "y", nil},
		{int64(3), "x", "z", "z", "y", "z"},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("first/last/nth: %v", res.Rows)
	}
}

// TestValueWindowTypesAndExplain: result columns take the argument's
// type, default names come from the function, and EXPLAIN renders the
// multi-argument call.
func TestValueWindowTypesAndExplain(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	info := describe(t, d, `select lag(name) over (order by age),
		lead(age, 2) over (order by age),
		first_value(name) over (order by age) from users`)
	if !reflect.DeepEqual(info.Cols, []string{"lag", "lead", "first_value"}) ||
		!reflect.DeepEqual(info.Types, []bytdb.ColType{bytdb.TString, bytdb.TInt, bytdb.TString}) {
		t.Fatalf("describe: %v %v", info.Cols, info.Types)
	}

	res := exec(t, d, `explain select lag(age, 1, 0) over (partition by city order by age),
		nth_value(name, 2) over (order by age) from users`)
	var plan strings.Builder
	for _, r := range res.Rows {
		plan.WriteString(r[0].(string) + "\n")
	}
	for _, want := range []string{
		"Window: lag(age, 1, 0) OVER (PARTITION BY city ORDER BY age)",
		"Window: nth_value(name, 2) OVER (ORDER BY age)",
	} {
		if !strings.Contains(plan.String(), want) {
			t.Errorf("plan missing %q:\n%s", want, plan.String())
		}
	}
}

// TestValueWindowParams: $n placeholders bind inside a value window's
// offset and default arguments.
func TestValueWindowParams(t *testing.T) {
	d := openDB(t)
	seedSeries(t, d)

	res, err := d.Exec(`select lag(v, $1, $2) over (partition by grp order by ord) from s where id = 1`,
		1, -7)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(-7) {
		t.Fatalf("bound lag default: %v", res.Rows)
	}

	// Describe infers the offset as int and the default as the value
	// argument's type — wire drivers encode bindings by these.
	info := describe(t, d, `select lag(grp, $1, $2) over (order by ord) from s`)
	if !reflect.DeepEqual(info.Params, []bytdb.ColType{bytdb.TInt, bytdb.TString}) {
		t.Fatalf("inferred params: %v", info.Params)
	}
}

func TestValueWindowErrors(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	for _, c := range []struct{ q, want string }{
		{`select lag(age) from users`, "requires an OVER clause"},
		{`select first_value() over (order by age) from users`, "wrong number of arguments"},
		{`select lag(age, 1, 0, 9) over (order by age) from users`, "wrong number of arguments"},
		{`select nth_value(age) over (order by age) from users`, "wrong number of arguments"},
		{`select row_number(age) over () from users`, "wrong number of arguments"},
		{`select lag(row_number() over ()) over () from users`, "cannot be nested"},
		{`select lag(age, 'x') over (order by age) from users`, "offset must be an integer"},
		{`select nth_value(age, 0) over (order by age) from users`, "greater than zero"},
		{`select nth_value(age, name) over (order by age) from users`, "must be an integer"},
	} {
		if _, err := d.Exec(c.q); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want containing %q", c.q, err, c.want)
		}
	}
}
