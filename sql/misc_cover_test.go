package sql

import (
	"reflect"
	"strings"
	"testing"
)

// TestStmtCommand covers Stmt.Command for every statement kind: the
// tag comes from the parse alone, without touching the catalog (the
// named tables need not exist).
func TestStmtCommand(t *testing.T) {
	d := openDB(t)

	for _, c := range []struct{ q, want string }{
		{`select 1`, "SELECT"},
		{`insert into t values (1)`, "INSERT"},
		{`update t set a = 1`, "UPDATE"},
		{`delete from t`, "DELETE"},
		{`explain select 1`, "EXPLAIN"},
		{`create table t (a int primary key)`, "CREATE TABLE"},
		{`drop table t`, "DROP TABLE"},
		{`alter table t add column b int`, "ALTER TABLE"},
		{`alter table t drop column b`, "ALTER TABLE"},
		{`create index ix on t (a)`, "CREATE INDEX"},
		{`drop index ix`, "DROP INDEX"},
		{`begin`, "BEGIN"},
		{`start transaction`, "START TRANSACTION"},
		{`commit`, "COMMIT"},
		{`rollback`, "ROLLBACK"},
	} {
		st, err := d.Prepare(c.q)
		if err != nil {
			t.Fatalf("Prepare(%q): %v", c.q, err)
		}
		if got := st.Command(); got != c.want {
			t.Errorf("Command(%q) = %q, want %q", c.q, got, c.want)
		}
	}
}

func TestExplainInsertAndExprText(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	res := exec(t, d, `explain insert into users values (9, 'x', 1, 'y')`)
	if len(res.Rows) != 2 || res.Rows[0][0] != "Insert on users" ||
		!strings.Contains(res.Rows[1][0].(string), `Values Scan on "*VALUES*"`) {
		t.Fatalf("insert plan: %v", res.Rows)
	}
	if _, err := d.Exec(`explain insert into missing values (1)`); err == nil ||
		!strings.Contains(err.Error(), "no such table") {
		t.Fatalf("insert missing: %v", err)
	}

	// Expression-level AND/OR render through joinExprs in plan filters.
	res = exec(t, d, `explain select name from users
		where case when age > 30 and age < 50 then true else (age = 28 or age = 99) end`)
	var plan strings.Builder
	for _, r := range res.Rows {
		plan.WriteString(r[0].(string) + "\n")
	}
	for _, want := range []string{"((age > 30) AND (age < 50))", "((age = 28) OR (age = 99))"} {
		if !strings.Contains(plan.String(), want) {
			t.Errorf("plan missing %q:\n%s", want, plan.String())
		}
	}
}

// TestAggRewriteContainers exercises aggQuery.rewrite across the
// composite expression kinds: group keys and accumulator references
// nested in IS NULL, IN, NOT, CASE, casts, arrays, ANY, AND/OR, and
// arithmetic, plus expression-valued GROUP BY keys.
func TestAggRewriteContainers(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	res := exec(t, d, `select city is null as isn,
		city in ('nyc', 'austin') as inx,
		not (city = 'nyc') as notx,
		case when count(*) > 1 then 'many' else 'few' end as sz,
		count(*) + 1 as cp,
		city::text as ct,
		array[city] as arr,
		city = any(array['nyc']) as anyx,
		case when city > 'a' and (city < 'm' or city = 'nyc') then 1 else 0 end as bl,
		(select max(age) from users) as sub
		from users group by city order by city`)
	want := [][]any{
		{false, true, true, "few", int64(2), "austin", "{austin}", false, int64(1), int64(45)},
		{false, false, true, "many", int64(3), "london", "{london}", false, int64(1), int64(45)},
		{false, true, false, "many", int64(3), "nyc", "{nyc}", true, int64(1), int64(45)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}

	// An expression GROUP BY key matches the same expression in the
	// select list (ExFunc through exprKey).
	res = exec(t, d, `select upper(city) as u, count(*) from users group by upper(city) order by 1`)
	want = [][]any{{"AUSTIN", int64(1)}, {"LONDON", int64(2)}, {"NYC", int64(2)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("expr key: %v", res.Rows)
	}

	// Ungrouped and unknown columns report the classic errors.
	if _, err := d.Exec(`select name from users group by city`); err == nil ||
		!strings.Contains(err.Error(), "must appear in the GROUP BY clause") {
		t.Fatalf("ungrouped: %v", err)
	}
	if _, err := d.Exec(`select nope from users group by city`); err == nil ||
		!strings.Contains(err.Error(), "no such column") {
		t.Fatalf("unknown col: %v", err)
	}
}

// TestAggRewriteErrors covers rewrite's error propagation: an
// ungrouped column nested inside each composite expression kind must
// surface the GROUP BY error, and other failure points report theirs.
func TestAggRewriteErrors(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	// name is never grouped; each query nests it in a different container.
	groupErr := "must appear in the GROUP BY clause"
	for _, q := range []string{
		`select case when name = 'a' and city = 'b' then 1 end from users group by city`,
		`select case when name = 'a' or city = 'b' then 1 end from users group by city`,
		`select not (name = 'a') from users group by city`,
		`select name is null from users group by city`,
		`select name in ('a') from users group by city`,
		`select city in (name) from users group by city`,
		`select upper(name) from users group by city`,
		`select name::text from users group by city`,
		`select array[name] from users group by city`,
		`select name = any(array['a']) from users group by city`,
		`select name || 'x' from users group by city`,
		`select name[1] from users group by city`,
		`select count(*) from users group by city having name = 'x'`,
		// An aggregate anywhere in ORDER BY makes the query aggregate,
		// so an ungrouped select item errors the same way.
		`select name from users order by count(*)`,
	} {
		if _, err := d.Exec(q); err == nil || !strings.Contains(err.Error(), groupErr) {
			t.Errorf("%s: err %v, want containing %q", q, err, groupErr)
		}
	}

	for _, c := range []struct{ q, want string }{
		{`select sum(nope + 1) from users`, "no such column"},
		{`select count(*) from missing`, "no such table"},
		{`select sum(age/0) from users`, "division by zero"},
		{`select count(*) from users group by age/0`, "division by zero"},
		{`select count(*) from users order by 1.5`, "non-integer constant in ORDER BY"},
	} {
		if _, err := d.Exec(c.q); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want containing %q", c.q, err, c.want)
		}
	}
}

// TestHavingShapes covers havingToExpr's tree kinds: NOT, OR, IS
// [NOT] NULL over aggregates, aggregate-to-aggregate comparisons, and
// the Cond leaf a non-legacy HAVING shape lowers to.
func TestHavingShapes(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	res := exec(t, d, `select city from users group by city
		having not (count(*) > 2) order by 1`)
	if !reflect.DeepEqual(rows1(res), []any{"austin", "london", "nyc"}) {
		t.Fatalf("not: %v", res.Rows)
	}
	res = exec(t, d, `select city from users group by city
		having count(*) = 1 or count(*) = 3 order by 1`)
	if !reflect.DeepEqual(rows1(res), []any{"austin"}) {
		t.Fatalf("or: %v", res.Rows)
	}
	res = exec(t, d, `select city from users group by city
		having min(name) is not null and max(age) is null order by 1`)
	if len(res.Rows) != 0 {
		t.Fatalf("is null: %v", res.Rows)
	}
	// Aggregate on both sides of the comparison (RItem).
	res = exec(t, d, `select city from users group by city
		having sum(age) > count(*) order by 1`)
	if !reflect.DeepEqual(rows1(res), []any{"austin", "london", "nyc"}) {
		t.Fatalf("agg cmp: %v", res.Rows)
	}
	// A non-legacy shape lowers to a Cond and evaluates per group.
	res = exec(t, d, `select city from users group by city
		having count(*) + 1 > 2 order by 1`)
	if !reflect.DeepEqual(rows1(res), []any{"london", "nyc"}) {
		t.Fatalf("cond: %v", res.Rows)
	}
}

// TestSyscatColumnTypes covers the type-mapping helpers (typeOID,
// typeLen, sqlTypeName, udtName) across all five column types via the
// catalog views that render them.
func TestSyscatColumnTypes(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table alltypes (id int primary key, f float, b bool, bs bytea, s text)`)

	res := exec(t, d, `select attname, atttypid, attlen from pg_attribute
		where attrelid = 'alltypes'::regclass order by attnum`)
	want := [][]any{
		{"id", int64(20), int64(8)},
		{"f", int64(701), int64(8)},
		{"b", int64(16), int64(1)},
		{"bs", int64(17), int64(-1)},
		{"s", int64(25), int64(-1)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("pg_attribute: %v", res.Rows)
	}

	res = exec(t, d, `select column_name, data_type, udt_name from information_schema.columns
		where table_name = 'alltypes' order by ordinal_position`)
	want = [][]any{
		{"id", "bigint", "int8"},
		{"f", "double precision", "float8"},
		{"b", "boolean", "bool"},
		{"bs", "bytea", "bytea"},
		{"s", "text", "text"},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("information_schema: %v", res.Rows)
	}
}

// TestEStringEscapes covers scanEString's escape table and its
// unterminated-literal error.
func TestEStringEscapes(t *testing.T) {
	d := openDB(t)

	res := exec(t, d, `select E'a\rb\bc\fd\\e\'f\qg''h'`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"a\rb\bc\fd\\e'fqg'h"}}) {
		t.Fatalf("got %q", res.Rows)
	}
	if _, err := d.Exec(`select E'abc`); err == nil ||
		!strings.Contains(err.Error(), "unterminated string literal") {
		t.Fatalf("unterminated: %v", err)
	}
}
