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
		`select 1 / 0.0`:             "division by zero",
		`select 'x'::numeric`:        `invalid input syntax for type numeric: "x"`,
		`select 1234.5::numeric(3)`:  "numeric field overflow",
		`select 'NaN'::numeric::int`: "cannot convert NaN to integer", // N-051: NaN itself is a value
	} {
		if _, err := d.Exec(q); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s: %v, want %q", q, err, msg)
		}
	}
}

// specialText renders a result's rows as Postgres's text, "|" between
// columns and NULL as <null>: NaN compares unequal to itself as a
// float64, so the special-value checks compare text.
func specialText(res *Result) []string {
	out := make([]string, len(res.Rows))
	for i, row := range res.Rows {
		cells := make([]string, len(row))
		for j, v := range row {
			if v == nil {
				cells[j] = "<null>"
				continue
			}
			cells[j] = valText(v)
		}
		out[i] = strings.Join(cells, "|")
	}
	return out
}

func wantText(t *testing.T, d *DB, q string, want ...string) {
	t.Helper()
	if got := specialText(exec(t, d, q)); !reflect.DeepEqual(got, want) {
		t.Errorf("%s\n got: %q\nwant: %q", q, got, want)
	}
}

// TestNumericSpecialSQL (N-051): numeric NaN and ±Infinity through SQL.
// Every expected value is Postgres 16.9's answer to the same statement
// (captured with psql); the corpus file 80_numeric_special.sql runs the
// same ground against a live Postgres.
func TestNumericSpecialSQL(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, x numeric, y numeric(5,2))`)
	exec(t, d, `create index t_x on t (x)`)
	exec(t, d, `insert into t values (1, 'NaN', 'NaN'), (2, 'Infinity', 1.5), (3, '-Infinity', 2),
		(4, 1.25, null), (5, 'NaN', null), (6, 0, 3)`)

	// Input spellings and output; NaN fits numeric(5,2), an infinity
	// does not.
	wantText(t, d, `select 'NaN'::numeric, ' nan '::numeric, 'Infinity'::numeric, '-inf'::numeric, '+INF'::numeric, 'NaN'::numeric(5,2)`,
		"NaN|NaN|Infinity|-Infinity|Infinity|NaN")
	for q, msg := range map[string]string{
		`select '-NaN'::numeric`:                     `invalid input syntax for type numeric: "-NaN"`,
		`select 'Infinity'::numeric(5,2)`:            "numeric field overflow",
		`insert into t values (7, 1, 'Infinity')`:    "numeric field overflow",
		`select 'Infinity'::numeric / 0`:             "division by zero",
		`select mod('-Infinity'::numeric, 0)`:        "division by zero",
		`select 'NaN'::numeric::bigint`:              "cannot convert NaN to bigint",
		`select 'Infinity'::numeric::int8`:           "cannot convert infinity to bigint",
		`select '-Infinity'::numeric::smallint`:      "cannot convert infinity to smallint",
		`select sqrt('-Infinity'::numeric)`:          "cannot take square root of a negative number",
		`select ln('-Infinity'::numeric)`:            "cannot take logarithm of a negative number",
		`select power('-Infinity'::numeric, 0.5)`:    "a negative number raised to a non-integer power",
		`select to_timestamp('NaN'::numeric)`:        "timestamp cannot be NaN",
		`insert into t values (8, 'NaN', null)`:      "",
		`select x from t where id = 8 and x = 'NaN'`: "",
	} {
		_, err := d.Exec(q)
		switch {
		case msg == "" && err != nil:
			t.Errorf("%s: %v", q, err)
		case msg != "" && (err == nil || !strings.Contains(err.Error(), msg)):
			t.Errorf("%s: %v, want %q", q, err, msg)
		}
	}
	exec(t, d, `delete from t where id = 8`)

	// Arithmetic and functions.
	wantText(t, d, `select 'NaN'::numeric + 1, 'Infinity'::numeric + 1, 'Infinity'::numeric + '-Infinity'::numeric,
		1 - 'Infinity'::numeric, 'Infinity'::numeric * 0, '-Infinity'::numeric * -2.5, 1.5 / 'Infinity'::numeric,
		'NaN'::numeric / 0, 5.25 % 'Infinity'::numeric, 'Infinity'::numeric % 2`,
		"NaN|Infinity|NaN|-Infinity|NaN|Infinity|0|NaN|5.25|NaN")
	wantText(t, d, `select -('Infinity'::numeric), abs('-Infinity'::numeric), sign('NaN'::numeric), sign('-Infinity'::numeric),
		round('NaN'::numeric, 2), trunc('Infinity'::numeric), ceil('-Infinity'::numeric), floor('NaN'::numeric),
		scale('NaN'::numeric), min_scale('Infinity'::numeric), trim_scale('-Infinity'::numeric)`,
		"-Infinity|Infinity|NaN|-1|NaN|Infinity|-Infinity|NaN|<null>|<null>|-Infinity")
	wantText(t, d, `select sqrt('Infinity'::numeric), exp('-Infinity'::numeric), ln('Infinity'::numeric), log('NaN'::numeric),
		log(2, 'Infinity'::numeric), log('Infinity'::numeric, 2), power('NaN'::numeric, 0), power(2, 'Infinity'::numeric),
		power('-Infinity'::numeric, 3), power(0.5, 'Infinity'::numeric)`,
		"Infinity|0|Infinity|NaN|Infinity|0|1|Infinity|-Infinity|0")

	// Casts to and from float8 and text.
	wantText(t, d, `select 'NaN'::numeric::float8, '-Infinity'::numeric::float8, 'NaN'::float8::numeric,
		'Infinity'::float8::numeric, 'Infinity'::numeric::text, ('NaN'::numeric)::varchar`,
		"NaN|-Infinity|NaN|Infinity|Infinity|NaN")

	// Comparisons: NaN = NaN and above everything; -Infinity below every
	// number; numeric meets float8 as float8.
	wantText(t, d, `select 'NaN'::numeric = 'NaN'::numeric, 'NaN'::numeric > 'Infinity'::numeric,
		'-Infinity'::numeric < -1e100, 'NaN'::numeric = 'NaN'::float8, 'Infinity'::numeric = 'Infinity'::float8,
		'NaN'::numeric in (1, 'NaN'), 'Infinity'::numeric between 1 and 'Infinity'`,
		"true|true|true|true|true|true|true")
	wantText(t, d, `select greatest(1, 'NaN'::numeric), least('-Infinity'::numeric, 1)`, "NaN|-Infinity")

	// Ordering, grouping and DISTINCT treat each special value as one
	// value, in Postgres's order; the index range scan agrees.
	wantText(t, d, `select x from t order by x desc`, "NaN", "NaN", "Infinity", "1.25", "0", "-Infinity")
	wantText(t, d, `select x, count(*) from t group by x order by x`,
		"-Infinity|1", "0|1", "1.25|1", "Infinity|1", "NaN|2")
	wantText(t, d, `select distinct x from t order by 1`, "-Infinity", "0", "1.25", "Infinity", "NaN")
	wantText(t, d, `select count(*) from t where x > 1e300`, "3")
	wantText(t, d, `select count(*) from t where x = 'NaN'`, "2")
	wantText(t, d, `select id from t where x >= 1 order by x, id`, "4", "2", "1", "5")
	wantText(t, d, `select x from t union select 'NaN'::float8 order by 1`, "-Infinity", "0", "1.25", "Infinity", "NaN")

	// Aggregates: any NaN, or both infinities, make sum and avg NaN; one
	// infinity wins; min/max order NaN last.
	wantText(t, d, `select sum(x), avg(x), min(x), max(x) from t`, "NaN|NaN|-Infinity|NaN")
	wantText(t, d, `select sum(x), avg(x) from t where x < 0 or x = 0`, "-Infinity|-Infinity")
	wantText(t, d, `select sum(x) from t where id in (2, 4)`, "Infinity")
	wantText(t, d, `select sum(x), avg(x) from t where id in (2, 3)`, "NaN|NaN")
	wantText(t, d, `select sum(y), avg(y), max(y) from t`, "NaN|NaN|NaN")

	// A unique key holds one NaN.
	exec(t, d, `create table u (x numeric primary key)`)
	exec(t, d, `insert into u values ('NaN'), ('Infinity'), ('-Infinity'), (5)`)
	if _, err := d.Exec(`insert into u values ('nan')`); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("second NaN key: %v", err)
	}
	wantText(t, d, `select * from u where x >= 5 order by x`, "5", "Infinity", "NaN")
	wantText(t, d, `select * from u where x < 5 order by x desc`, "-Infinity")

	// DEFAULT 'NaN' on numeric and float8 columns reaches every INSERT
	// that leaves the column out (the float8 one failed to parse its
	// stored default before N-051's renderLit fix).
	exec(t, d, `create table w (id int primary key, f float8 default 'NaN', n numeric default 'Infinity', m numeric default 'NaN'::numeric)`)
	exec(t, d, `insert into w (id) values (1)`)
	wantText(t, d, `select f, n, m from w`, "NaN|Infinity|NaN")
}
