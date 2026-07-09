package sql

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

// TestBindArgKinds covers bindArg's normalization of every accepted Go
// parameter type to the engine's value kinds, plus its rejections.
func TestBindArgKinds(t *testing.T) {
	d := openDB(t)

	cases := []struct {
		arg  any
		want any
	}{
		{nil, nil},
		{int64(7), int64(7)},
		{3.5, 3.5},
		{"s", "s"},
		{true, true},
		{[]byte{0xab}, `\xab`}, // rendered through ::text below
		{int(5), int64(5)},
		{int8(-8), int64(-8)},
		{int16(16), int64(16)},
		{int32(-32), int64(-32)},
		{uint8(8), int64(8)},
		{uint16(160), int64(160)},
		{uint32(320), int64(320)},
		{uint(64), int64(64)},
		{uint64(640), int64(640)},
		{float32(1.5), 1.5},
	}
	for _, c := range cases {
		q := `select $1`
		if _, isBytes := c.arg.([]byte); isBytes {
			q = `select $1::text` // []byte compares awkwardly; its text form is stable
		}
		res, err := d.Exec(q, c.arg)
		if err != nil {
			t.Errorf("%T: %v", c.arg, err)
			continue
		}
		if !reflect.DeepEqual(res.Rows[0][0], c.want) {
			t.Errorf("%T: got %#v, want %#v", c.arg, res.Rows[0][0], c.want)
		}
	}

	// Unrepresentable and unsupported arguments.
	for _, c := range []struct {
		arg  any
		want string
	}{
		{uint64(math.MaxUint64), "overflows int64"},
		{uint(math.MaxUint64), "overflows int64"},
		{struct{}{}, "unsupported parameter type"},
		{complex(1, 2), "unsupported parameter type"},
	} {
		if _, err := d.Exec(`select $1`, c.arg); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%T: err %v, want containing %q", c.arg, err, c.want)
		}
	}
}

// TestBindExprPositions binds placeholders across the expression node
// kinds bindExpr clones: CASE (both forms), functions, casts, arrays,
// IN, ANY, IS NULL, NOT, AND/OR, arithmetic, aggregates, windows, and
// subqueries.
func TestBindExprPositions(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	res, err := d.Exec(`select
		case when age > $1 and (age < $2 or age = $3) then upper($4) else 'lo' || name end as cs,
		case $5 when 1 then 'one' else 'other' end as op,
		coalesce($6, name) as fn,
		age + $7 as ar,
		$8::int as ct,
		array[$9, 'x'] as arr,
		age in ($10, 99) as inx,
		age = any(array[$11]) as anyx,
		$12 is null as isn,
		not (age = $13) as notx
		from users where id = 1`,
		30, 40, 99, "hi", 1, nil, 4, "12", "a", 36, 36, nil, 99)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]any{{"HI", "one", "ada", int64(40), int64(12), "{a,x}",
		true, true, true, true}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}

	// Aggregates: an expression argument binds; a plain-column aggregate
	// inside an expression passes through untouched.
	res, err = d.Exec(`select sum(age + $1), sum(age) + $2 from users`, 1, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(194), int64(1189)}}) {
		t.Fatalf("agg params: %v", res.Rows)
	}

	// Windows: params in the argument, PARTITION BY, and ORDER BY.
	res, err = d.Exec(`select name, sum(age + $1) over (partition by city = $2 order by age + $3)
		from users where city = 'london' order by age`, 0, "london", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Rows, [][]any{{"ada", int64(36)}, {"alan", int64(77)}}) {
		t.Fatalf("window params: %v", res.Rows)
	}

	// A param inside an array subscript clones through bindExpr's
	// ExIndex arm before the evaluator rejects the subscript.
	if _, err = d.Exec(`select (array[1])[$1]`, 1); err == nil ||
		!strings.Contains(err.Error(), "array subscripts are not supported") {
		t.Fatalf("index param: %v", err)
	}

	// A subquery on the right of a comparison binds through bindSelect.
	res, err = d.Exec(`select name from users
		where age > (select age from users where name = $1) order by 1`, "alan")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rows1(res), []any{"grace"}) {
		t.Fatalf("subquery param: %v", res.Rows)
	}

	// UPDATE SET expressions clone with bound params too.
	if res, err = d.Exec(`update users set age = age + $1 where id = $2`, 10, 5); err != nil ||
		res.RowsAffected != 1 {
		t.Fatalf("update setex: %v %v", res, err)
	}
	res = exec(t, d, `select age from users where id = 5`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(38)}}) {
		t.Fatalf("updated age: %v", res.Rows)
	}
}

func TestBindParamCountErrors(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	// Too few, too many, and args to a parameterless statement.
	for _, c := range []struct {
		q    string
		args []any
	}{
		{`select name from users where id = $1`, nil},
		{`select name from users where id = $1`, []any{1, 2}},
		{`select name from users`, []any{1}},
		// $2 alone still demands two values (numbering, not counting).
		{`select name from users where id = $2`, []any{1}},
	} {
		if _, err := d.Exec(c.q, c.args...); err == nil ||
			!strings.Contains(err.Error(), "wrong number of parameters") {
			t.Errorf("%s with %d args: err %v", c.q, len(c.args), err)
		}
	}

	// The same check guards a prepared statement's Exec.
	st, err := d.Prepare(`delete from users where id = $1`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Exec(); err == nil || !strings.Contains(err.Error(), "wrong number of parameters") {
		t.Fatalf("prepared underbind: %v", err)
	}

	// A bad argument still fails cleanly through the binding wrapper.
	if _, err := d.Exec(`select $1`, struct{}{}); err == nil ||
		!strings.Contains(err.Error(), "unsupported parameter type") {
		t.Fatalf("bad arg: %v", err)
	}
}
