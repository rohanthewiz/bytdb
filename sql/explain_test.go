package sql

import (
	"strings"
	"testing"
)

// explainLines runs EXPLAIN and returns the plan lines.
func explainLines(t *testing.T, d *DB, q string) []string {
	t.Helper()
	res := exec(t, d, q)
	if len(res.Cols) != 1 || res.Cols[0] != "QUERY PLAN" {
		t.Fatalf("EXPLAIN shape: %v", res.Cols)
	}
	out := make([]string, len(res.Rows))
	for i, r := range res.Rows {
		out[i] = r[0].(string)
	}
	return out
}

func wantPlan(t *testing.T, d *DB, q string, want ...string) {
	t.Helper()
	got := explainLines(t, d, q)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("EXPLAIN %s:\ngot:\n%s\nwant:\n%s", q,
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestExplainScans(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	exec(t, d, `create index by_city on users (city)`)

	wantPlan(t, d, `explain select * from users`,
		`Seq Scan on users`)
	wantPlan(t, d, `explain select * from users where id = 5`,
		`Point Get on users`,
		`  Key: (id = 5)`)
	wantPlan(t, d, `explain select name from users where city = 'london' and age > 30`,
		`Index Scan using by_city on users`,
		`  Index Cond: (city = 'london')`,
		`  Filter: (age > 30)`)
	wantPlan(t, d, `explain select name from users where id >= 2 and id < 4`,
		`Index Scan using users_pkey on users`,
		`  Index Cond: ((id >= 2) AND (id < 4))`)
	wantPlan(t, d, `explain delete from users where city = 'x' or age > 99`,
		`Delete on users`,
		`  ->  Seq Scan on users`,
		`        Filter: ((city = 'x') OR (age > 99))`)
	wantPlan(t, d, `explain update users set age = 1 where id = 3`,
		`Update on users`,
		`  ->  Point Get on users`,
		`        Key: (id = 3)`)
	wantPlan(t, d, `explain insert into users values (9, 'x', 1, 'y')`,
		`Insert on users`,
		`  ->  Values Scan on "*VALUES*"`)
}

func TestExplainJoinAggSort(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	exec(t, d, `create table orders (id int primary key, user_id int, total float)`)
	exec(t, d, `create index by_user on orders (user_id)`)

	// The inner scan shows the per-outer-row index nested loop.
	wantPlan(t, d, `explain select u.name, o.total from users u join orders o on o.user_id = u.id where u.age > 30 order by o.total desc limit 5`,
		`Limit`,
		`  ->  Sort`,
		`        Sort Key: o.total DESC`,
		`        ->  Nested Loop`,
		`              ->  Seq Scan on users u`,
		`                    Filter: (u.age > 30)`,
		`              ->  Index Scan using by_user on orders o`,
		`                    Index Cond: (o.user_id = u.id)`)

	// A WHERE predicate on a LEFT JOIN's right table waits for the
	// post-join filter, so it shows on the join, not the scan.
	wantPlan(t, d, `explain select name from users u left join orders o on o.user_id = u.id where o.id is null`,
		`Nested Loop Left Join`,
		`  Filter: (o.id IS NULL)`,
		`  ->  Seq Scan on users u`,
		`  ->  Index Scan using by_user on orders o`,
		`        Index Cond: (o.user_id = u.id)`)

	wantPlan(t, d, `explain select city, count(*) as n from users group by city having count(*) > 1 order by n desc`,
		`Sort`,
		`  Sort Key: n DESC`,
		`  ->  HashAggregate`,
		`        Group Key: city`,
		`        Filter: (count(*) > 1)`,
		`        ->  Seq Scan on users`)
	wantPlan(t, d, `explain select count(*) from users`,
		`Aggregate`,
		`  ->  Seq Scan on users`)
	wantPlan(t, d, `explain select city from users group by city having count(distinct age) > 1`,
		`HashAggregate`,
		`  Group Key: city`,
		`  Filter: (count(DISTINCT age) > 1)`,
		`  ->  Seq Scan on users`)

	wantPlan(t, d, `explain select name from users union all select name from users`,
		`Append`,
		`  ->  Seq Scan on users`,
		`  ->  Seq Scan on users`)
	wantPlan(t, d, `explain select 1`,
		`Result`)
}

func TestExplainErrorsAndOptions(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	// Options parse; ANALYZE and non-text formats are rejected.
	wantPlan(t, d, `explain (costs off, verbose, format text) select * from users`,
		`Seq Scan on users`)
	wantPlan(t, d, `explain verbose select * from users`,
		`Seq Scan on users`)
	for q, want := range map[string]string{
		`explain analyze select * from users`:          "EXPLAIN ANALYZE is not supported",
		`explain (analyze) select * from users`:        "EXPLAIN ANALYZE is not supported",
		`explain (format json) select * from users`:    "only EXPLAIN (FORMAT TEXT) is supported",
		`explain create table t (a int primary key)`:   "EXPLAIN supports SELECT, INSERT, UPDATE, and DELETE",
		`explain select * from nosuch`:                 "no such table",
		`explain select nocol from users`:              "no such column",
		`explain select name from users group by city`: "must appear in the GROUP BY clause",
	} {
		if _, err := d.Exec(q); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s\n  got: %v\n  want: %s", q, err, want)
		}
	}
	// (ANALYZE FALSE) explicitly off is fine.
	wantPlan(t, d, `explain (analyze false) select * from users`,
		`Seq Scan on users`)

	// Placeholders bind before planning.
	st, err := d.Prepare(`explain select * from users where id = $1`)
	if err != nil {
		t.Fatal(err)
	}
	res, err := st.Exec(int64(7))
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[1][0] != "  Key: (id = 7)" {
		t.Fatalf("bound explain: %v", res.Rows)
	}
	info, err := st.Describe()
	if err != nil {
		t.Fatal(err)
	}
	if info.Command != "EXPLAIN" || len(info.Cols) != 1 || info.Cols[0] != "QUERY PLAN" ||
		len(info.Params) != 1 || info.Params[0] != "int" {
		t.Fatalf("describe explain: %+v", info)
	}
}
