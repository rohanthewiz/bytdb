package sql

// N-052: sqrt, exp, ln, log and power pick Postgres's numeric version
// for numeric input. decimal/math_test.go and the difftest corpus
// (79_numeric_math.sql) pin the digits against Postgres 16. This pins
// the SQL layer: which version a call resolves to, what a placeholder
// describes as, and the errors.

import (
	"reflect"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func TestNumericMathVersions(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table nm (id int primary key, n numeric(10,2), i int, f float8)`)
	exec(t, d, `insert into nm values (1, 16.00, 16, 16), (2, null, null, null)`)

	// pg_typeof of each call, as Postgres 16 reports it.
	got := exec(t, d, `select pg_typeof(sqrt(2)), pg_typeof(sqrt(2.0)), pg_typeof(sqrt('2')),
		pg_typeof(exp(n)), pg_typeof(ln(i)), pg_typeof(log(f)), pg_typeof(log(8.0)),
		pg_typeof(log(2, 8)), pg_typeof(log(i, n)), pg_typeof(log(f, f)), pg_typeof(log10(n)),
		pg_typeof(power(2, 3)), pg_typeof(power(i, n)), pg_typeof(power(n, f)), pg_typeof(cbrt(n))
		from nm where id = 1`).Rows[0]
	want := []any{"double precision", "numeric", "double precision",
		"numeric", "double precision", "double precision", "numeric",
		"numeric", "numeric", "double precision", "numeric",
		"double precision", "numeric", "double precision", "double precision"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("versions:\n got %v\nwant %v", got, want)
	}

	// The numeric results carry Postgres's scales; the float ones are
	// floats; NULL in, NULL out.
	got = exec(t, d, `select sqrt(n), sqrt(i), log(2, 8)::text, power(n, 0.5)::text, ln(null::numeric)
		from nm where id = 1`).Rows[0]
	if got[0].(bytdb.Decimal).String() != "4.000000000000000" || got[1] != 4.0 ||
		got[2] != "3.0000000000000000" || got[3] != "4.0000000000000000" || got[4] != nil {
		t.Fatalf("values: %v", got)
	}
	if got := exec(t, d, `select sqrt(n) from nm where id = 2`).Rows[0][0]; got != nil {
		t.Fatalf("sqrt(NULL) = %v", got)
	}

	// Placeholders take the version's type: an untyped $1 alone resolves
	// to float8, beside a numeric to numeric, and two-argument log's to
	// numeric.
	for q, params := range map[string][]bytdb.ColType{
		`select sqrt($1)`:             {bytdb.TFloat},
		`select power(2.0, $1)`:       {bytdb.TNumeric},
		`select log($1, $2)`:          {bytdb.TNumeric, bytdb.TNumeric},
		`select exp($1::numeric)`:     {bytdb.TNumeric},
		`select power($1, f) from nm`: {bytdb.TFloat},
	} {
		if info := describe(t, d, q); !reflect.DeepEqual(info.Params, params) {
			t.Errorf("%s: params %v, want %v", q, info.Params, params)
		}
	}

	// Postgres's errors, the float versions included (they returned
	// Inf/NaN or one combined message before).
	for q, msg := range map[string]string{
		`select sqrt(-1.0)`:             "cannot take square root of a negative number",
		`select sqrt(-1::float8)`:       "cannot take square root of a negative number",
		`select ln(0.0)`:                "cannot take logarithm of zero",
		`select ln(0::float8)`:          "cannot take logarithm of zero",
		`select log(-1::float8)`:        "cannot take logarithm of a negative number",
		`select log(1, 10)`:             "division by zero",
		`select power(0.0, -1)`:         "zero raised to a negative power is undefined",
		`select power(0::float8, -1)`:   "zero raised to a negative power is undefined",
		`select power(-8.0, 0.5)`:       "a negative number raised to a non-integer power yields a complex result",
		`select power(-8::float8, 0.5)`: "a negative number raised to a non-integer power yields a complex result",
		`select exp(6000.0)`:            "value overflows numeric format",
		`select log10(1, 2)`:            "function log10(bigint, bigint) does not exist",
		`select sqrt('x'::text)`:        "function sqrt(text) does not exist",
	} {
		wantErr(t, d, q, msg)
	}
}
