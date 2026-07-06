package sql

import (
	"reflect"
	"strings"
	"testing"
)

// rows1 flattens a single-column result.
func rows1(res *Result) []any {
	out := make([]any, len(res.Rows))
	for i, r := range res.Rows {
		out[i] = r[0]
	}
	return out
}

func TestExprCase(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	// Searched form, with alias and ORDER BY on the alias.
	res := exec(t, d, `select name, case when age >= 45 then 'senior'
		when age >= 36 then 'mid' else 'young' end as band
		from users order by band, name`)
	want := [][]any{
		{"ada", "mid"}, {"alan", "mid"}, {"edsger", "mid"},
		{"grace", "senior"}, {"barbara", "young"},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}
	if res.Cols[1] != "band" {
		t.Fatalf("cols %v", res.Cols)
	}

	// Operand form; no ELSE means NULL.
	res = exec(t, d, `select case city when 'nyc' then 'east' when 'london' then 'uk' end
		from users order by name`)
	if !reflect.DeepEqual(rows1(res), []any{"uk", "uk", "east", nil, "east"}) {
		t.Fatalf("got %v", res.Rows)
	}
	if res.Cols[0] != "case" {
		t.Fatalf("cols %v", res.Cols)
	}
}

func TestExprInAndRegex(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	res := exec(t, d, `select name from users where id in (1, 3, 99) order by 1`)
	if !reflect.DeepEqual(rows1(res), []any{"ada", "alan"}) {
		t.Fatalf("got %v", res.Rows)
	}
	res = exec(t, d, `select name from users where city not in ('nyc') order by 1`)
	if !reflect.DeepEqual(rows1(res), []any{"ada", "alan", "edsger"}) {
		t.Fatalf("got %v", res.Rows)
	}
	// IN adapts quoted literals to the column's type.
	res = exec(t, d, `select name from users where id in ('2') order by 1`)
	if !reflect.DeepEqual(rows1(res), []any{"grace"}) {
		t.Fatalf("got %v", res.Rows)
	}

	res = exec(t, d, `select name from users where name ~ '^a' order by 1`)
	if !reflect.DeepEqual(rows1(res), []any{"ada", "alan"}) {
		t.Fatalf("got %v", res.Rows)
	}
	res = exec(t, d, `select name from users where name !~ '^a' and name ~* 'GRACE' order by 1`)
	if !reflect.DeepEqual(rows1(res), []any{"grace"}) {
		t.Fatalf("got %v", res.Rows)
	}
	// OPERATOR(pg_catalog.~) reads as the operator it names.
	res = exec(t, d, `select name from users where name operator(pg_catalog.~) '^b'`)
	if !reflect.DeepEqual(rows1(res), []any{"barbara"}) {
		t.Fatalf("got %v", res.Rows)
	}
	if _, err := d.Exec(`select 1 from users where name ~ '('`); err == nil {
		t.Fatal("expected regex compile error")
	}
}

func TestExprAnyAll(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	// = ANY over an ARRAY[...] constructor — the IN equivalent.
	res := exec(t, d, `select name from users where id = any(array[1, 3, 99]) order by 1`)
	if !reflect.DeepEqual(rows1(res), []any{"ada", "alan"}) {
		t.Fatalf("= any array: %v", res.Rows)
	}
	// A non-equality operator: ANY is true if it holds for some element.
	res = exec(t, d, `select name from users where age > any(array[40, 45]) order by 1`)
	if !reflect.DeepEqual(rows1(res), []any{"alan", "grace"}) {
		t.Fatalf("> any: %v", res.Rows)
	}
	// <> ALL is NOT IN.
	res = exec(t, d, `select name from users where city <> all(array['nyc']) order by 1`)
	if !reflect.DeepEqual(rows1(res), []any{"ada", "alan", "edsger"}) {
		t.Fatalf("<> all: %v", res.Rows)
	}
	// >= ALL selects the maxima.
	res = exec(t, d, `select name from users where age >= all(array[28, 45]) order by 1`)
	if !reflect.DeepEqual(rows1(res), []any{"grace"}) {
		t.Fatalf(">= all: %v", res.Rows)
	}
	// A Postgres array-literal string coerces its text elements.
	res = exec(t, d, `select name from users where id = any('{2}') order by 1`)
	if !reflect.DeepEqual(rows1(res), []any{"grace"}) {
		t.Fatalf("= any literal: %v", res.Rows)
	}
	// The subquery (row-set) form.
	res = exec(t, d, `select name from users
		where id = any(select id from users where city = 'london') order by 1`)
	if !reflect.DeepEqual(rows1(res), []any{"ada", "alan"}) {
		t.Fatalf("= any subquery: %v", res.Rows)
	}
	// Empty array: ANY→false (no rows), ALL→true (all rows).
	res = exec(t, d, `select name from users where id = any(array[]::int[])`)
	if len(res.Rows) != 0 {
		t.Fatalf("= any empty: %v", res.Rows)
	}
	res = exec(t, d, `select count(*) from users where id <> all(array[]::int[])`)
	if res.Rows[0][0] != int64(5) {
		t.Fatalf("<> all empty: %v", res.Rows)
	}
	// The constructor is also a value: SELECT ARRAY[...] renders {..}.
	res = exec(t, d, `select array[1, 2, 3]`)
	if res.Rows[0][0] != "{1,2,3}" {
		t.Fatalf("array literal render: %v", res.Rows)
	}
}

func TestExprArithCastsFuncs(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	res := exec(t, d, `select upper(name) || '/' || (age + 1) as tag from users where id = 1`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"ADA/37"}}) || res.Cols[0] != "tag" {
		t.Fatalf("got %v %v", res.Rows, res.Cols)
	}
	res = exec(t, d, `select '41'::int + 1, 7 / 2, 7 % 2, 7.0 / 2, age::text from users where id = 3`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(42), int64(3), int64(1), 3.5, "41"}}) {
		t.Fatalf("got %v", res.Rows)
	}
	if _, err := d.Exec(`select 1 / 0`); err == nil || !strings.Contains(err.Error(), "division by zero") {
		t.Fatalf("err %v", err)
	}
	// NULL is strict through arithmetic and concatenation.
	res = exec(t, d, `select null || 'x', 1 + null`)
	if !reflect.DeepEqual(res.Rows, [][]any{{nil, nil}}) {
		t.Fatalf("got %v", res.Rows)
	}

	res = exec(t, d, `select coalesce(null, 'x'), nullif('a', 'a'), nullif('a', 'b'), length(name)
		from users where id = 2`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"x", nil, "a", int64(5)}}) {
		t.Fatalf("got %v", res.Rows)
	}

	// 'users'::regclass resolves through the catalog to the table oid.
	res = exec(t, d, `select 'users'::regclass = c.oid from pg_class c where c.relname = 'users'`)
	if !reflect.DeepEqual(res.Rows, [][]any{{true}}) {
		t.Fatalf("got %v", res.Rows)
	}

	// WHERE takes bare boolean expressions and literal comparisons.
	res = exec(t, d, `select count(*) from users where 1 = 1`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(5)}}) {
		t.Fatalf("got %v", res.Rows)
	}
	res = exec(t, d, `select 'x' as v where 1 = 2`)
	if len(res.Rows) != 0 {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestExprEStrings(t *testing.T) {
	d := openDB(t)
	res := exec(t, d, `select E'a\nb' || E'\t' || e'c''d'`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"a\nb\tc'd"}}) {
		t.Fatalf("got %q", res.Rows)
	}
}

func TestScalarSubqueries(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	exec(t, d, `create table pets (id int primary key, owner_id int, kind text)`)
	exec(t, d, `insert into pets values (1,1,'cat'),(2,1,'dog'),(3,3,'iguana')`)

	// Correlated aggregate.
	res := exec(t, d, `select name, (select count(*) from pets where owner_id = users.id) as pets
		from users where id <= 3 order by 1`)
	want := [][]any{{"ada", int64(2)}, {"alan", int64(1)}, {"grace", int64(0)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}

	// Correlated value; zero rows read as NULL.
	res = exec(t, d, `select (select kind from pets where owner_id = users.id and kind = 'cat')
		from users where id in (1, 2) order by name`)
	if !reflect.DeepEqual(rows1(res), []any{"cat", nil}) {
		t.Fatalf("got %v", res.Rows)
	}

	// More than one row is an error.
	if _, err := d.Exec(`select (select kind from pets) from users`); err == nil ||
		!strings.Contains(err.Error(), "more than one row") {
		t.Fatalf("err %v", err)
	}

	// EXISTS and its negation.
	res = exec(t, d, `select name from users
		where exists (select 1 from pets where owner_id = users.id) order by 1`)
	if !reflect.DeepEqual(rows1(res), []any{"ada", "alan"}) {
		t.Fatalf("got %v", res.Rows)
	}
	res = exec(t, d, `select count(*) from users
		where not exists (select 1 from pets where owner_id = users.id)`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(3)}}) {
		t.Fatalf("got %v", res.Rows)
	}

	// ARRAY(SELECT ...) renders the Postgres array text form.
	res = exec(t, d, `select array(select kind from pets where owner_id = users.id order by 1)
		from users where id in (1, 2) order by name`)
	if !reflect.DeepEqual(rows1(res), []any{"{cat,dog}", "{}"}) {
		t.Fatalf("got %v", res.Rows)
	}

	// A subquery may sit in WHERE too.
	res = exec(t, d, `select name from users
		where (select count(*) from pets where owner_id = users.id) > 1`)
	if !reflect.DeepEqual(rows1(res), []any{"ada"}) {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestUnion(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	exec(t, d, `create table pets (id int primary key, owner_id int, kind text)`)
	exec(t, d, `insert into pets values (1,1,'cat'),(2,1,'ada')`) // 'ada' collides with a user

	res := exec(t, d, `select name from users where city = 'london'
		union select kind from pets order by 1`)
	if !reflect.DeepEqual(rows1(res), []any{"ada", "alan", "cat"}) {
		t.Fatalf("got %v", res.Rows)
	}

	res = exec(t, d, `select name from users where id = 1
		union all select kind from pets order by 1 desc limit 2`)
	if !reflect.DeepEqual(rows1(res), []any{"cat", "ada"}) {
		t.Fatalf("got %v", res.Rows)
	}

	// ORDER BY may name an output column; the first arm names them.
	res = exec(t, d, `select name as who from users where id = 2
		union select kind from pets where kind = 'cat' order by who desc`)
	if !reflect.DeepEqual(rows1(res), []any{"grace", "cat"}) {
		t.Fatalf("got %v", res.Rows)
	}

	// Aggregate arms combine like any others.
	res = exec(t, d, `select count(*) from users union select count(*) from pets order by 1`)
	if !reflect.DeepEqual(rows1(res), []any{int64(2), int64(5)}) {
		t.Fatalf("got %v", res.Rows)
	}

	if _, err := d.Exec(`select name, age from users union select kind from pets`); err == nil ||
		!strings.Contains(err.Error(), "same number of columns") {
		t.Fatalf("err %v", err)
	}
}

func TestExprInWrites(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	res := exec(t, d, `update users set city = 'metro' where id in (1, 2)`)
	if res.RowsAffected != 2 {
		t.Fatalf("affected %d", res.RowsAffected)
	}
	res = exec(t, d, `delete from users where name ~ '^e' or id in (5)`)
	if res.RowsAffected != 2 {
		t.Fatalf("affected %d", res.RowsAffected)
	}
	res = exec(t, d, `select count(*) from users`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(3)}}) {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestExprParams(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	// Params bind inside expressions and in the select list.
	res, err := d.Exec(`select $1, name from users where case when $2 then id = 1 else id = 2 end`,
		"tag", true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Rows, [][]any{{"tag", "ada"}}) {
		t.Fatalf("got %v", res.Rows)
	}

	st, err := d.Prepare(`select name from users where id in ($1, $2) order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	if st.NumParams() != 2 {
		t.Fatalf("params %d", st.NumParams())
	}
	res, err = st.Exec(int64(1), int64(4))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rows1(res), []any{"ada", "edsger"}) {
		t.Fatalf("got %v", res.Rows)
	}
}
