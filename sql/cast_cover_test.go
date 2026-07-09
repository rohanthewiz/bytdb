package sql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// TestCastValPairs exercises castVal across the type families: the
// integer/oid family, regclass name resolution, the other reg* types,
// text rendering, bool, and float.
func TestCastValPairs(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	res := exec(t, d, `select ' 42 '::int, 3.9::bigint, null::smallint, 7::int,
		'  123 '::regclass, 5::regclass, null::regclass,
		'99'::regtype, 42::regproc, null::regrole,
		1.5::text, true::text, null::text,
		'true'::bool, 'f'::boolean, null::bool,
		'2.5'::numeric, 1::float8, 2.25::real, null::decimal`)
	want := [][]any{{int64(42), int64(3), nil, int64(7),
		int64(123), int64(5), nil,
		int64(99), int64(42), nil,
		"1.5", "true", nil,
		true, false, nil,
		2.5, 1.0, 2.25, nil}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}

	// regclass resolves a (possibly schema-qualified) table name to its oid.
	res = exec(t, d, `select 'public.users'::regclass = 'users'::regclass`)
	if !reflect.DeepEqual(res.Rows, [][]any{{true}}) {
		t.Fatalf("regclass name: %v", res.Rows)
	}

	// A bytea value renders in \x form through ::text.
	res, err := d.Exec(`select $1::text`, []byte{0x01, 0xff})
	if err != nil || !reflect.DeepEqual(res.Rows, [][]any{{`\x01ff`}}) {
		t.Fatalf("bytea text: %v %v", res, err)
	}
}

func TestCastValErrors(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	for _, c := range []struct{ q, want string }{
		{`select 'abc'::int`, "invalid input syntax for type int"},
		{`select true::int`, "unsupported cast"},
		{`select 'missing'::regclass`, "relation does not exist"},
		// reg* types other than regclass do not resolve names.
		{`select 'pg_type'::regtype`, "unsupported cast"},
		{`select 'yes'::bool`, "invalid input syntax for type bool"},
		{`select 'abc'::float8`, "invalid input syntax for type float"},
		{`select '{1}'::int[]`, "array casts are not supported"},
		{`select 1::json`, "unsupported cast"},
	} {
		if _, err := d.Exec(c.q); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want containing %q", c.q, err, c.want)
		}
	}
}

// TestFormatType covers pg_catalog.format_type across its known oids,
// the varchar typmod arithmetic, and its NULL/unknown fallbacks.
func TestFormatType(t *testing.T) {
	d := openDB(t)

	res := exec(t, d, `select format_type(16, null), format_type(17, null),
		format_type(19, null), format_type(20, null), format_type(21, null),
		format_type(23, null), format_type(25, null), format_type(26, null),
		format_type(700, null), format_type(701, null),
		format_type(1043, 36), format_type(1043, null),
		format_type(9999, null), format_type(null, null)`)
	want := [][]any{{"boolean", "bytea", "name", "bigint", "smallint",
		"integer", "text", "oid", "real", "double precision",
		"character varying(32)", "character varying", "???", nil}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}
}

// TestExprStaticTypesAndNames covers exprType/castColType/exprName
// through Describe, which types a select list without executing it.
func TestExprStaticTypesAndNames(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	info := describe(t, d, `select name || 'x', age + 1.5, age + 1,
		length(name), case when true then 1.5 else 2.0 end,
		age::float4, age::bool, age::regclass, name::varchar,
		age in (1, 2), age = any(array[1]), not (age = 1), name is null,
		age::text, case when age > 1 then 'a' end, 1 + 1
		from users`)
	wantTypes := []bytdb.ColType{
		bytdb.TString, bytdb.TFloat, bytdb.TInt,
		bytdb.TInt, bytdb.TFloat,
		bytdb.TFloat, bytdb.TBool, bytdb.TInt, bytdb.TString,
		bytdb.TBool, bytdb.TBool, bytdb.TBool, bytdb.TBool,
		bytdb.TString, bytdb.TString, bytdb.TInt,
	}
	if !reflect.DeepEqual(info.Types, wantTypes) {
		t.Fatalf("types %v", info.Types)
	}
	// Unaliased names: a cast keeps its operand's name, CASE names
	// "case", other synthesized shapes fall back to ?column?.
	if info.Cols[13] != "age" || info.Cols[14] != "case" || info.Cols[15] != "?column?" ||
		info.Cols[3] != "length" {
		t.Fatalf("cols %v", info.Cols)
	}

	// A folded system function keeps its name even through parens.
	res := exec(t, d, `select (version())`)
	if len(res.Cols) != 1 || res.Cols[0] != "version" {
		t.Fatalf("version cols %v", res.Cols)
	}
}

// TestPredExprCorrelated covers predExpr's IS NULL / IS NOT NULL
// branches: a correlated NULL test re-lowers into an expression the
// subquery evaluates against the outer row.
func TestPredExprCorrelated(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	exec(t, d, `create table pets (id int primary key, owner_id int, kind text)`)
	exec(t, d, `insert into pets values (1,1,'cat'),(2,3,'dog')`)
	exec(t, d, `update users set city = null where id = 3`)

	res := exec(t, d, `select name,
		(select count(*) from pets p where p.owner_id = users.id and users.city is not null) as c1,
		(select count(*) from pets p where p.owner_id = users.id and users.city is null) as c2
		from users where id in (1, 3) order by 1`)
	want := [][]any{{"ada", int64(1), int64(0)}, {"alan", int64(0), int64(1)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}

	// A correlated comparison against a literal re-lowers with the
	// literal side intact (predExpr's ExLit branch).
	res = exec(t, d, `select name,
		(select count(*) from pets p where p.owner_id = users.id and users.city = 'london') as c
		from users where id in (1, 3) order by 1`)
	want = [][]any{{"ada", int64(1)}, {"alan", int64(0)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("literal pred: %v", res.Rows)
	}
}
