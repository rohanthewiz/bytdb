package sql

// N-048: the numeric type through the SQL layer, hermetically (the
// difftest corpus 77_numeric.sql checks the same ground against a real
// Postgres when one is configured). Expected values are Postgres 16's
// answers to the same statements.

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// decv is an expected numeric value, as a parsed Decimal: reflect.
// DeepEqual compares it with a result digit for digit and scale for
// scale, so 1.5 and 1.50 differ, which is the point.
func decv(s string) bytdb.Decimal {
	d, err := bytdb.ParseDecimal(s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestNumericSQL(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table n (id int primary key, a numeric(6,2) default 0.00, b numeric)`)
	exec(t, d, `create unique index n_b on n (b)`)
	exec(t, d, `insert into n values (1, 1.005, 1.0), (2, -2, 2.50), (3, default, null)`)

	// Literals are numeric with their written scale; division keeps 16
	// significant digits; int ∘ numeric is numeric, numeric ∘ float float.
	res := exec(t, d, `select 0.1 + 0.2, 1/3.0, 2 * 1.50, 1.5 + 1::float8, 2.5::int, '7.125'::numeric(4,2)`)
	want := []any{decv("0.3"), decv("0.33333333333333333333"), decv("3.00"), 2.5, int64(3), decv("7.13")}
	if !reflect.DeepEqual(res.Rows[0], want) {
		t.Errorf("literals = %v, want %v", res.Rows[0], want)
	}
	if !reflect.DeepEqual(res.Types, []bytdb.ColType{bytdb.TNumeric, bytdb.TNumeric, bytdb.TNumeric,
		bytdb.TFloat, bytdb.TInt, bytdb.TNumeric}) {
		t.Errorf("literal types = %v", res.Types)
	}

	// The typmod rounds on write; DEFAULT keeps its scale.
	wantRows(t, d, `select id, a, b from n order by id`, [][]any{
		{int64(1), decv("1.01"), decv("1.0")},
		{int64(2), decv("-2.00"), decv("2.50")},
		{int64(3), decv("0.00"), nil},
	})
	if _, err := d.Exec(`insert into n (id, a) values (4, 12345)`); err == nil ||
		!strings.Contains(err.Error(), "numeric field overflow") {
		t.Errorf("overflow insert: %v", err)
	}
	// 1.00 equals 1.0: the unique index refuses it, = finds it, GROUP BY
	// folds it.
	if _, err := d.Exec(`insert into n (id, b) values (5, 1.00)`); err == nil ||
		!strings.Contains(err.Error(), "duplicate key value") {
		t.Errorf("unique across scales: %v", err)
	}
	wantRows(t, d, `select id from n where b = 1`, [][]any{{int64(1)}})
	wantRows(t, d, `select count(*) from (select b from n union select 1.000) u`, [][]any{{int64(3)}})

	// Aggregates: sum keeps numeric, avg is numeric over integers too.
	res = exec(t, d, `select sum(a), avg(a), avg(id), min(b), max(b) from n`)
	want = []any{decv("-0.99"), decv("-0.33000000000000000000"), decv("2.0000000000000000"), decv("1.0"), decv("2.50")}
	if !reflect.DeepEqual(res.Rows[0], want) {
		t.Errorf("aggregates = %v, want %v", res.Rows[0], want)
	}

	// Numeric functions are exact and keep Postgres's scales.
	res = exec(t, d, `select round(2.345, 2), abs(-2.50), ceil(2.10), mod(-7, 2.5), trunc(5, 1), scale(1.500), trim_scale(1.500)`)
	want = []any{decv("2.35"), decv("2.50"), decv("3"), decv("-2.0"), decv("5.0"), int64(3), decv("1.5")}
	if !reflect.DeepEqual(res.Rows[0], want) {
		t.Errorf("functions = %v, want %v", res.Rows[0], want)
	}

	// Errors in Postgres's words.
	for q, msg := range map[string]string{
		`select 1 / 0.0`:            "division by zero",
		`select 'x'::numeric`:       `invalid input syntax for type numeric: "x"`,
		`select 1234.5::numeric(3)`: "numeric field overflow",
		`select 'NaN'::numeric`:     "numeric NaN and Infinity are not supported",
	} {
		if _, err := d.Exec(q); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s: %v, want %q", q, err, msg)
		}
	}
}
