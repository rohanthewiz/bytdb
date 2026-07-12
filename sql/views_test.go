package sql

// Tests for derived tables, WITH (CTEs), and views.

import (
	"strings"
	"testing"
)

func TestDerivedTables(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	// A derived table joins, filters, and projects like any table.
	res := exec(t, d, `select x.name from (select name, age from users where age > 38) x
		where x.age < 42 order by x.name`)
	if len(res.Rows) != 2 || res.Rows[0][0] != "alan" || res.Rows[1][0] != "edsger" {
		t.Fatalf("derived rows: %v", res.Rows)
	}

	// Aliased output columns; expression items carry their alias in.
	res = exec(t, d, `select big.n from (select count(*) as n from users) as big`)
	if res.Rows[0][0].(int64) != 5 {
		t.Fatalf("derived aggregate: %v", res.Rows)
	}

	// Derived tables join with real tables (and each other).
	res = exec(t, d, `select u.name, g.cnt
		from users u
		join (select city, count(*) as cnt from users group by city) g
		  on u.city = g.city
		where u.id = 1`)
	if len(res.Rows) != 1 || res.Rows[0][1].(int64) != 2 {
		t.Fatalf("derived join: %v", res.Rows)
	}

	// The alias is mandatory, as in Postgres.
	if _, err := d.Exec(`select * from (select 1)`); err == nil ||
		!strings.Contains(err.Error(), "must have an alias") {
		t.Fatalf("missing alias: %v", err)
	}
	// Derived tables outside SELECT are rejected clearly.
	if _, err := d.Exec(`delete from users where id in (select id from (select 1 as id) q)`); err == nil ||
		!strings.Contains(err.Error(), "only supported in SELECT") {
		t.Fatalf("derived in DML: %v", err)
	}
}

func TestWithCTE(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	// A CTE materializes once and is visible in the body and later CTEs.
	res := exec(t, d, `with
		locals as (select * from users where city = 'london'),
		named as (select name from locals where age > 37)
		select * from named order by name`)
	if len(res.Rows) != 1 || res.Rows[0][0] != "alan" {
		t.Fatalf("cte chain: %v", res.Rows)
	}

	// WITH x(a, b) renames the output columns.
	res = exec(t, d, `with pairs(who, years) as (select name, age from users)
		select who from pairs where years = 45`)
	if len(res.Rows) != 1 || res.Rows[0][0] != "grace" {
		t.Fatalf("cte renames: %v", res.Rows)
	}

	// A CTE shadows a real table of the same name.
	res = exec(t, d, `with users as (select 1 as one) select one from users`)
	if len(res.Rows) != 1 || res.Rows[0][0].(int64) != 1 {
		t.Fatalf("cte shadowing: %v", res.Rows)
	}

	// CTEs feed scalar subqueries and union arms too.
	res = exec(t, d, `with l as (select age from users where city = 'london')
		select name from users where age = (select max(age) from l)`)
	if len(res.Rows) != 1 || res.Rows[0][0] != "alan" {
		t.Fatalf("cte in subquery: %v", res.Rows)
	}

	for q, want := range map[string]string{
		`with recursive r as (select 1) select * from r`: "WITH RECURSIVE is not supported",
		`with a as (select 1), a as (select 2) select 1`: "specified more than once",
		`with p(a, b) as (select 1) select * from p`:     "does not match",
	} {
		if _, err := d.Exec(q); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestViews(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	exec(t, d, `create view londoners as select id, name, age from users where city = 'london'`)
	res := exec(t, d, `select name from londoners order by age`)
	if len(res.Rows) != 2 || res.Rows[0][0] != "ada" {
		t.Fatalf("view select: %v", res.Rows)
	}

	// Views reflect base-table changes at each use (unmaterialized).
	exec(t, d, `insert into users values (6, 'tommy', 20, 'london')`)
	if res := exec(t, d, `select count(*) from londoners`); res.Rows[0][0].(int64) != 3 {
		t.Fatalf("view after insert: %v", res.Rows[0][0])
	}

	// Views join with tables and appear in subqueries.
	res = exec(t, d, `select u.city from users u
		where u.id in (select id from londoners) and u.age > 40`)
	if len(res.Rows) != 1 || res.Rows[0][0] != "london" {
		t.Fatalf("view in subquery: %v", res.Rows)
	}

	// A view over a view.
	exec(t, d, `create view young_londoners as select * from londoners where age < 30`)
	if res := exec(t, d, `select count(*) from young_londoners`); res.Rows[0][0].(int64) != 1 {
		t.Fatalf("view over view: %v", res.Rows[0][0])
	}

	// CREATE VIEW validates the body up front.
	if _, err := d.Exec(`create view broken as select nope from users`); err == nil {
		t.Fatal("invalid view body accepted")
	}
	// The relation namespace is shared.
	if _, err := d.Exec(`create view users as select 1`); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("view over table name: %v", err)
	}
	if _, err := d.Exec(`create table londoners (id int primary key)`); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("table over view name: %v", err)
	}

	// OR REPLACE swaps the definition; plain CREATE refuses.
	if _, err := d.Exec(`create view londoners as select 1`); err == nil {
		t.Fatal("duplicate view accepted")
	}
	exec(t, d, `create or replace view londoners as select name from users where city = 'nyc'`)
	if res := exec(t, d, `select count(*) from londoners`); res.Rows[0][0].(int64) != 2 {
		t.Fatalf("replaced view: %v", res.Rows[0][0])
	}
	// young_londoners depended on the age column the replacement lost.
	if _, err := d.Exec(`select * from young_londoners`); err == nil {
		t.Fatal("view over replaced view still resolves a dropped column")
	}

	// DROP VIEW; IF EXISTS is a notice.
	exec(t, d, `drop view young_londoners`)
	exec(t, d, `drop view londoners`)
	if _, err := d.Exec(`drop view londoners`); err == nil {
		t.Fatal("dropping a dropped view succeeded")
	}
	if res := exec(t, d, `drop view if exists londoners`); res.Notice == "" {
		t.Fatal("expected a notice")
	}
	// The name is free again.
	exec(t, d, `create table londoners (id int primary key)`)
}

func TestViewsAndCTEsPrepared(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	exec(t, d, `create view adults as select id, name, age from users where age >= 30`)

	// Describe (the wire Parse path) resolves views and CTEs without
	// executing them.
	st, err := d.Prepare(`with old as (select * from adults where age > $1)
		select name, age from old order by age`)
	if err != nil {
		t.Fatal(err)
	}
	info, err := st.Describe()
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Cols) != 2 || info.Cols[0] != "name" || info.Types[1] != "int" {
		t.Fatalf("described shape: %+v", info)
	}
	res, err := st.Exec(int64(40))
	if err != nil || len(res.Rows) != 2 {
		t.Fatalf("prepared cte over view: %v %v", err, res)
	}

	// EXPLAIN doesn't execute but still resolves everything.
	if _, err := d.Exec(`explain with x as (select * from adults) select * from x`); err != nil {
		t.Fatalf("explain with cte+view: %v", err)
	}
}
