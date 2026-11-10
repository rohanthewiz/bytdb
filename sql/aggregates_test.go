package sql

// Deep-look D-53 part 2: the aggregate pieces the Postgres difftest
// corpus (76_aggregates) cannot reach — bound parameters inside the new
// clauses, the parse cache's shared trees, Describe's types, EXPLAIN
// text, the row budget on collected items, and the scalar-subquery
// fast path.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func seedAggs(t *testing.T, d *DB) {
	t.Helper()
	exec(t, d, `create table a (id int primary key, g text, s text, b bool, n int, ts timestamp)`)
	exec(t, d, `insert into a values
		(1, 'p', 'x', true, 3, '2024-01-01'),
		(2, 'p', 'y', false, 1, '2024-01-02 12:30'),
		(3, 'q', null, true, 2, null),
		(4, 'q', 'z', null, null, '2024-01-03')`)
}

// Placeholders in the delimiter, the FILTER condition and the ORDER BY
// bind like any expression's, and a cached statement re-run with other
// arguments sees the new values: binding copies the call instead of
// writing into the tree the parse cache shares.
func TestAggregateParams(t *testing.T) {
	d := openDB(t)
	seedAggs(t, d)
	q := `select string_agg(s, $1 order by id) filter (where n > $2), count(*) filter (where g = $3) from a`
	for _, c := range []struct {
		args []any
		want string
	}{
		{[]any{",", int64(0), "p"}, "[[x,y 2]]"},
		{[]any{" / ", int64(1), "q"}, "[[x 2]]"}, // n > 1: x and a NULL s, which string_agg skips
		{[]any{"-", int64(2), "none"}, "[[x 0]]"},
	} {
		r, err := d.Exec(q, c.args...)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if got := fmt.Sprint(r.Rows); got != c.want {
			t.Errorf("%v: got %s, want %s", c.args, r.Rows, c.want)
		}
	}
	// Describe types the placeholders from their positions and the
	// outputs from the aggregates' fixed result types.
	st, err := d.Prepare(`select string_agg(s, $1), array_agg(n) filter (where id > $2), bool_or(b) from a`)
	if err != nil {
		t.Fatal(err)
	}
	info, err := st.Describe()
	if err != nil {
		t.Fatal(err)
	}
	if want := []bytdb.ColType{bytdb.TString, bytdb.TTextArray, bytdb.TBool}; fmt.Sprint(info.Types) != fmt.Sprint(want) {
		t.Errorf("Describe types = %v, want %v", info.Types, want)
	}
	if info.Params[1] != bytdb.TInt {
		t.Errorf("FILTER placeholder typed %q, want int", info.Params[1])
	}
}

// Identical calls share one accumulator; calls that differ only in a
// D-53 part do not.
func TestAggregateAccumulatorIdentity(t *testing.T) {
	d := openDB(t)
	seedAggs(t, d)
	r := exec(t, d, `select count(*), count(*) filter (where b), count(*) filter (where not b),
		string_agg(s, ',' order by id), string_agg(s, ',' order by id desc), string_agg(s, ';' order by id) from a`)
	if got, want := fmt.Sprint(r.Rows), "[[4 2 1 x,y,z z,y,x x;y;z]]"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	// HAVING and the select list name the same call: one accumulator,
	// one answer.
	r = exec(t, d, `select g, count(*) filter (where b) from a group by g having count(*) filter (where b) > 0 order by g`)
	if got, want := fmt.Sprint(r.Rows), "[[p 1] [q 1]]"; got != want {
		t.Errorf("HAVING: got %s, want %s", got, want)
	}
}

// array_agg renders elements by their static type: a timestamp column
// holds int64 microseconds at runtime, and a uuid holds 16 bytes.
func TestArrayAggElementText(t *testing.T) {
	d := openDB(t)
	seedAggs(t, d)
	want := `{"2024-01-01 00:00:00+00","2024-01-02 12:30:00+00",NULL,"2024-01-03 00:00:00+00"}`
	for _, q := range []string{
		`select array_agg(ts order by id) from a`,
		`select (select array_agg(ts) from a)`, // the scalar-subquery fast path
	} {
		if r := exec(t, d, q); r.Rows[0][0] != want {
			t.Errorf("%s = %v", q, r.Rows[0][0])
		}
	}
	exec(t, d, `create table u (id uuid primary key)`)
	exec(t, d, `insert into u values ('a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11')`)
	if r := exec(t, d, `select array_agg(id) from u`); r.Rows[0][0] != "{a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11}" {
		t.Errorf("uuid array_agg = %v", r.Rows[0][0])
	}
}

func TestAggregateExplain(t *testing.T) {
	d := openDB(t)
	seedAggs(t, d)
	got := exprText(mustSelectItem(t, d, `select string_agg(distinct s, ',' order by s desc) filter (where b) from a`))
	if want := "string_agg(DISTINCT s, ',' ORDER BY s DESC) FILTER (WHERE b)"; got != want {
		t.Errorf("exprText = %s, want %s", got, want)
	}
}

// mustSelectItem parses q and returns its first select item as an
// expression.
func mustSelectItem(t *testing.T, d *DB, q string) Expr {
	t.Helper()
	st, err := Parse(q)
	if err != nil {
		t.Fatal(err)
	}
	return itemToExpr(st.(*Select).Items[0])
}

// string_agg and array_agg hold their inputs until the group closes, so
// the held items count against the result row cap like a DISTINCT set.
func TestAggregateCollectBudget(t *testing.T) {
	d := openDB(t)
	seedNums(t, d, 10)
	d.e.SetMaxResultRows(5)
	defer d.e.SetMaxResultRows(0)
	for _, q := range []string{
		`select string_agg(id::text, ',') from nums`,
		`select array_agg(id) from nums`,
	} {
		if _, err := d.Exec(q); err == nil || !strings.Contains(err.Error(), "result row limit") {
			t.Errorf("%s: err = %v, want the row limit refusal", q, err)
		}
	}
	// Filtered down below the cap, the same calls succeed.
	exec(t, d, `select string_agg(id::text, ',') filter (where id < 4) from nums`)
}

func TestAggregateErrors(t *testing.T) {
	d := openDB(t)
	seedAggs(t, d)
	for q, want := range map[string]string{
		`select string_agg(n, ',') from a`:                                   "function string_agg(bigint, unknown) does not exist",
		`select string_agg(s, 1) from a`:                                     "function string_agg(text, bigint) does not exist",
		`select bool_and(n) from a`:                                          "function bool_and(bigint) does not exist",
		`select array_agg(distinct g order by n) from a`:                     "must appear in argument list",
		`select count(*) filter (where n) from a`:                            "argument of FILTER must be type boolean",
		`select count(*) filter (where count(*) > 1) from a`:                 "not allowed in FILTER",
		`select string_agg(s, ',' order by id) over () from a`:               "not implemented for window functions",
		`select bool_or(b) over () from a`:                                   "not supported as a window function",
		`select count(*) filter (where b) over () from a`:                    "not supported as a window function",
		`select string_agg(string_agg(s, ','), ',') from a`:                  "cannot be nested",
		`select array_agg(n order by count(*)) from a`:                       "cannot be nested",
		`select array_agg(x) from (select '{a}'::text[] as x) t`:             "one-dimensional",
		`select array_agg(n) filter (where row_number() over () > 1) from a`: "window function",
	} {
		_, err := d.Exec(q)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", q, err, want)
		}
	}
	// ORDER BY in an order-insensitive aggregate is accepted, as in
	// Postgres, and changes nothing.
	if r := exec(t, d, `select sum(n order by n desc), count(* ) from a`); fmt.Sprint(r.Rows) != "[[6 4]]" {
		t.Errorf("sum(... order by) = %v", r.Rows)
	}
}
