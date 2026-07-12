package sql

// Hash-join tests: plan selection (EXPLAIN), equivalence with the
// nested loop on every join shape, NULL keys, cross-numeric keys, and
// a scale check that would time out under a quadratic nested loop.

import (
	"fmt"
	"strings"
	"testing"
)

// explainText renders EXPLAIN output as one string.
func explainText(t *testing.T, d *DB, q string) string {
	t.Helper()
	res := exec(t, d, "explain "+q)
	var b strings.Builder
	for _, r := range res.Rows {
		b.WriteString(r[0].(string))
		b.WriteByte('\n')
	}
	return b.String()
}

func TestHashJoinSelection(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table a (id int primary key, x int)`)
	exec(t, d, `create table b (id int primary key, y int)`)

	// Unindexed equijoin: hash join.
	plan := explainText(t, d, `select * from a join b on a.x = b.y`)
	if !strings.Contains(plan, "Hash Join") || !strings.Contains(plan, "Hash Cond: (b.y = a.x)") {
		t.Fatalf("unindexed equijoin plan:\n%s", plan)
	}

	// Join on the inner primary key: the nested loop's point get wins.
	plan = explainText(t, d, `select * from a join b on a.x = b.id`)
	if strings.Contains(plan, "Hash Join") || !strings.Contains(plan, "Nested Loop") {
		t.Fatalf("indexed equijoin plan:\n%s", plan)
	}

	// An index on the join column flips it back to the nested loop.
	exec(t, d, `create index b_y on b (y)`)
	plan = explainText(t, d, `select * from a join b on a.x = b.y`)
	if strings.Contains(plan, "Hash Join") {
		t.Fatalf("indexed join still hashed:\n%s", plan)
	}

	// Non-equality joins stay nested loops.
	plan = explainText(t, d, `select * from a join b on a.x < b.id`)
	if strings.Contains(plan, "Hash Join") {
		t.Fatalf("range join hashed:\n%s", plan)
	}
}

func TestHashJoinCorrectness(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table dept (id int primary key, code int, name text)`)
	exec(t, d, `create table emp (id int primary key, dcode int, who text)`)
	exec(t, d, `insert into dept values (1, 10, 'eng'), (2, 20, 'ops'), (3, null, 'limbo')`)
	exec(t, d, `insert into emp values
		(1, 10, 'ada'), (2, 10, 'grace'), (3, 20, 'alan'),
		(4, 99, 'orphan'), (5, null, 'floating')`)

	// Inner equijoin (dcode/code are unindexed → hash path; verified in
	// TestHashJoinSelection).
	res := exec(t, d, `select e.who, d.name from emp e join dept d on e.dcode = d.code
		order by e.who`)
	if len(res.Rows) != 3 || res.Rows[0][0] != "ada" || res.Rows[1][1] != "ops" {
		t.Fatalf("inner hash join: %v", res.Rows)
	}

	// NULL keys never match (either side): 'floating' and 'limbo' pair
	// with nothing.
	// LEFT JOIN NULL-extends the unmatched left rows.
	res = exec(t, d, `select e.who, d.name from emp e left join dept d on e.dcode = d.code
		order by e.who`)
	if len(res.Rows) != 5 {
		t.Fatalf("left hash join rows: %v", res.Rows)
	}
	byWho := map[string]any{}
	for _, r := range res.Rows {
		byWho[r[0].(string)] = r[1]
	}
	if byWho["orphan"] != nil || byWho["floating"] != nil || byWho["ada"] != "eng" {
		t.Fatalf("left hash join extension: %v", byWho)
	}

	// Extra ON conditions beyond the equality still filter.
	res = exec(t, d, `select e.who from emp e join dept d on e.dcode = d.code and d.name != 'eng'`)
	if len(res.Rows) != 1 || res.Rows[0][0] != "alan" {
		t.Fatalf("hash join with residual ON: %v", res.Rows)
	}

	// WHERE over the joined rows still applies.
	res = exec(t, d, `select e.who from emp e join dept d on e.dcode = d.code
		where d.name = 'eng' order by e.who`)
	if len(res.Rows) != 2 {
		t.Fatalf("hash join with WHERE: %v", res.Rows)
	}
}

func TestHashJoinCrossNumeric(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table ints (id int primary key, v int)`)
	exec(t, d, `create table floats (id int primary key, v float)`)
	exec(t, d, `insert into ints values (1, 1), (2, 2)`)
	exec(t, d, `insert into floats values (1, 1.0), (2, 2.5)`)

	// int = float joins land in the same bucket (1 = 1.0).
	res := exec(t, d, `select i.id from ints i join floats f on i.v = f.v`)
	if len(res.Rows) != 1 || res.Rows[0][0].(int64) != 1 {
		t.Fatalf("cross-numeric hash join: %v", res.Rows)
	}
}

// TestHashJoinDerived: virtual tables (derived tables, CTEs, views)
// have no indexes, so their equijoins always hash.
func TestHashJoinDerived(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	res := exec(t, d, `with counts as (select city, count(*) as n from users group by city)
		select u.name, c.n from users u join counts c on u.city = c.city
		where u.id = 2`)
	if len(res.Rows) != 1 || res.Rows[0][1].(int64) != 2 {
		t.Fatalf("cte hash join: %v", res.Rows)
	}
}

// TestHashJoinScale joins two unindexed 3000-row tables — nine
// million nested-loop probes if quadratic, two scans and one build if
// hashed. The Go test timeout is the real assertion; the count just
// proves the join matched.
func TestHashJoinScale(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table l (id int primary key, k int)`)
	exec(t, d, `create table r (id int primary key, k int)`)
	const n = 3000
	for lo := 0; lo < n; lo += 500 {
		var lv, rv []string
		for i := lo; i < lo+500; i++ {
			lv = append(lv, fmt.Sprintf("(%d, %d)", i, i))
			rv = append(rv, fmt.Sprintf("(%d, %d)", i, n-1-i))
		}
		exec(t, d, "insert into l values "+strings.Join(lv, ","))
		exec(t, d, "insert into r values "+strings.Join(rv, ","))
	}
	res := exec(t, d, `select count(*) from l join r on l.k = r.k`)
	if res.Rows[0][0].(int64) != n {
		t.Fatalf("scale join count: %v", res.Rows[0][0])
	}
}
