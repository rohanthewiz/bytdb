package sql

// Deep-look D-38: IN-lists, = ANY(constant array) and OR-of-equalities
// on a key column run as key probes instead of a full scan.

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

func TestInListProbes(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table o (id int primary key, uid int, tag text, ref uuid)`)
	exec(t, d, `create index o_uid on o (uid)`)
	for i := 0; i < 60; i++ {
		exec(t, d, fmt.Sprintf(`insert into o values (%d, %d, 't%d', '%s')`, i, i%7, i%3, uuidN(i)))
	}
	cases := []struct{ q, plan, want string }{
		{`select id from o where id in (7, 3, 3, null, 99) order by id`,
			"Index Cond: (id = ANY (ARRAY[3, 7, 99]))", "[[3] [7]]"},
		{`select id from o where id = any('{14,7,3}') order by id`,
			"Index Cond: (id = ANY (ARRAY[3, 7, 14]))", "[[3] [7] [14]]"},
		{`select id from o where id = any(array[8, 2]) order by id`,
			"Index Scan using o_pkey on o", "[[2] [8]]"},
		{`select id from o where id = 5 or id = 1 order by id`,
			"Index Cond: (id = ANY (ARRAY[1, 5]))", "[[1] [5]]"},
		{`select id from o where uid in (6, 5) and id < 20 order by id`,
			"Index Scan using o_uid on o", "[[5] [6] [12] [13] [19]]"},
		{`select id from o where id in (null)`, "Index Scan using o_pkey on o", "[]"},
		{`select id from o where id = any('{}')`, "Index Scan using o_pkey on o", "[]"},
		// NOT IN never probes.
		{`select count(*) from o where id not in (1, 2, 3)`, "Seq Scan on o", "[[57]]"},
		// A column already pinned by an equality: the IN only filters.
		{`select id from o where id = 4 and id in (4, 5)`, "Point Get on o", "[[4]]"},
	}
	for _, c := range cases {
		if plan := explainText(t, d, c.q); !strings.Contains(plan, c.plan) {
			t.Errorf("%s\nplan:\n%s\nwant it to contain %q", c.q, plan, c.plan)
		}
		if got := fmt.Sprint(exec(t, d, c.q).Rows); got != c.want {
			t.Errorf("%s = %s, want %s", c.q, got, c.want)
		}
	}
	// An IN on an unindexed column (ref) cannot probe; it stays a filter
	// and still answers.
	q := `select id from o where ref in ('` + uuidN(4) + `', '` + uuidN(40) + `') order by id`
	if plan := explainText(t, d, q); strings.Contains(plan, "= ANY") {
		t.Errorf("unindexed IN probed:\n%s", plan)
	}
	if got := fmt.Sprint(exec(t, d, q).Rows); got != "[[4] [40]]" {
		t.Errorf("uuid IN filter = %s", got)
	}

	// LIMIT stops between probes.
	if got := fmt.Sprint(exec(t, d, `select id from o where id in (9, 1, 5) limit 2`).Rows); got != "[[1] [5]]" {
		t.Errorf("limited probes = %s", got)
	}
	// Parameters: an array bound as text, and one IN element per parameter.
	r, err := d.Exec(`select id from o where id = any($1) order by id`, "{11,10}")
	if err != nil || fmt.Sprint(r.Rows) != "[[10] [11]]" {
		t.Fatalf("= ANY($1) = %v, %v", r, err)
	}
	r, err = d.Exec(`select id from o where id in ($1, $2) order by id`, 21, 20)
	if err != nil || fmt.Sprint(r.Rows) != "[[20] [21]]" {
		t.Fatalf("IN ($1, $2) = %v, %v", r, err)
	}

	// UPDATE and DELETE plan through the same path.
	exec(t, d, `update o set tag = 'hit' where id in (30, 31, 99)`)
	if got := fmt.Sprint(exec(t, d, `select id from o where tag = 'hit' order by id`).Rows); got != "[[30] [31]]" {
		t.Errorf("update by IN = %s", got)
	}
	exec(t, d, `delete from o where id = any('{30,32}')`)
	if got := fmt.Sprint(exec(t, d, `select count(*) from o`).Rows); got != "[[58]]" {
		t.Errorf("delete by ANY left %s rows", got)
	}
}

// Probes run in the probed column's scan order, so a path that serves
// ORDER BY still does — forward, backward, and over a DESC index column
// — and a sort key the probes do not order by still gets its Sort.
func TestInListProbeOrder(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, a int not null, b int not null)`)
	exec(t, d, `create index t_ab on t (a, b)`)
	exec(t, d, `create index t_bd on t (b desc, id)`)
	exec(t, d, `create table bare (id int primary key, a int not null, b int not null)`)
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 200; i++ {
		row := fmt.Sprintf("(%d, %d, %d)", i, rng.Intn(10), rng.Intn(10))
		exec(t, d, "insert into t values "+row)
		exec(t, d, "insert into bare values "+row)
	}
	for _, c := range []struct {
		q      string
		sorted bool  // the plan serves the order (no Sort node)
		keys   []int // result columns the ORDER BY names; rows tied on them may come in any order
	}{
		{`select id, a, b from %s where a in (7, 2, 5) order by a, b`, true, []int{1, 2}},
		{`select id, a, b from %s where a in (7, 2, 5) order by a desc, b desc`, true, []int{1, 2}},
		{`select id, a, b from %s where a = any('{3,1}') and b > 4 order by a, b`, true, []int{1, 2}},
		{`select id, a, b from %s where b in (1, 8, 4) order by b desc, id`, true, []int{2, 0}},
		{`select id, a, b from %s where b in (1, 8, 4) order by b, id desc`, true, []int{2, 0}},
		// Ordering by b alone is NOT what probing a visits (a varies).
		{`select id, a, b from %s where a in (7, 2) order by b, id`, false, []int{2, 0}},
	} {
		qt, qb := fmt.Sprintf(c.q, "t"), fmt.Sprintf(c.q, "bare")
		plan := explainText(t, d, qt)
		if hasSort := strings.Contains(plan, "Sort"); hasSort == c.sorted {
			t.Errorf("%s: plan sorted=%v, want sorted=%v\n%s", qt, !hasSort, c.sorted, plan)
		}
		if !strings.Contains(plan, "= ANY") {
			t.Errorf("%s does not probe:\n%s", qt, plan)
		}
		// Same rows, and the same sequence of ORDER BY key values: SQL
		// leaves the order of rows tied on every key unspecified, and the
		// index walk and the bare table's stable sort break ties
		// differently.
		got, want := exec(t, d, qt).Rows, exec(t, d, qb).Rows
		if !slices.Equal(multiset(got), multiset(want)) {
			t.Errorf("%s: row sets differ\n got  %v\n want %v", qt, got, want)
			continue
		}
		keySeq := func(rows [][]any) string {
			var b strings.Builder
			for _, r := range rows {
				for _, k := range c.keys {
					fmt.Fprint(&b, r[k], ",")
				}
				b.WriteByte(';')
			}
			return b.String()
		}
		if keySeq(got) != keySeq(want) {
			t.Errorf("%s: order differs\n got  %v\n want %v", qt, got, want)
		}
	}
}

// A join step probes with an IN on its own table; one on another table
// probes that table instead, and LEFT JOIN's nullable side does not
// take WHERE probes.
func TestInListProbeJoin(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table u (id int primary key, name text)`)
	exec(t, d, `create table p (id int primary key, uid int, title text)`)
	exec(t, d, `create index p_uid on p (uid)`)
	for i := 1; i <= 30; i++ {
		exec(t, d, fmt.Sprintf(`insert into u values (%d, 'u%d')`, i, i))
	}
	for i := 1; i <= 90; i++ {
		exec(t, d, fmt.Sprintf(`insert into p values (%d, %d, 'p%d')`, i, 1+i%30, i))
	}
	q := `select u.name, p.title from u join p on p.uid = u.id where u.id in (3, 2) order by p.id`
	plan := explainText(t, d, q)
	if !strings.Contains(plan, "Index Cond: (id = ANY (ARRAY[2, 3]))") || strings.Contains(plan, "Filter: (u.id IN") {
		t.Errorf("outer IN not probed (or repeated as a filter):\n%s", plan)
	}
	if got := fmt.Sprint(exec(t, d, q).Rows); got != "[[u2 p1] [u3 p2] [u2 p31] [u3 p32] [u2 p61] [u3 p62]]" {
		t.Errorf("join = %s", got)
	}
	q = `select count(*) from u join p on p.uid = u.id where p.id in (5, 6, 7)`
	if plan := explainText(t, d, q); !strings.Contains(plan, "(id = ANY (ARRAY[5, 6, 7]))") {
		t.Errorf("inner IN not probed:\n%s", plan)
	}
	if got := fmt.Sprint(exec(t, d, q).Rows); got != "[[3]]" {
		t.Errorf("inner IN join = %s", got)
	}
	q = `select u.id, p.id from u left join p on p.uid = u.id and p.id in (4, 5) where u.id in (5, 6, 7) order by u.id`
	if got := fmt.Sprint(exec(t, d, q).Rows); got != "[[5 4] [6 5] [7 <nil>]]" {
		t.Errorf("left join with ON probe = %s", got)
	}
}

// The residual check of a constant IN list uses a hash set; its
// three-valued answers must match the list walk's.
func TestInListSetThreeValued(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table v (id int primary key, x int, s text)`)
	exec(t, d, `insert into v values (1, 1, 'a'), (2, 5, 'b'), (3, null, 'c'), (4, 9, 'd')`)
	for _, c := range []struct{ q, want string }{
		{`select id from v where x in (1, 2, 3, 4, 5) order by id`, "[[1] [2]]"},
		{`select id from v where x in (1, 2, 3, 4, null) order by id`, "[[1]]"},
		{`select id from v where x not in (1, 2, 3, 4, 6) order by id`, "[[2] [4]]"},
		{`select id from v where x not in (1, 2, 3, 4, null) order by id`, "[]"},
		{`select id, x in (1, 2, 3, 4, null) from v order by id`, "[[1 true] [2 <nil>] [3 <nil>] [4 <nil>]]"},
		{`select id from v where s in ('a', 'b', 'q', 'r', 'z') order by id`, "[[1] [2]]"},
		// Mixed kinds fall back to the walk's coercion.
		{`select id from v where x in (1, '5', 7, 8, 10) order by id`, "[[1] [2]]"},
		{`select id from v where x in (1.0, 5.0, 7, 8, 10) order by id`, "[[1] [2]]"},
	} {
		if got := fmt.Sprint(exec(t, d, c.q).Rows); got != c.want {
			t.Errorf("%s = %s, want %s", c.q, got, c.want)
		}
	}
}

func TestInKeysCanonical(t *testing.T) {
	keys, ok := inKeys([]any{int64(3), nil, int64(1), int64(3), "2"}, "int")
	if !ok || !slices.Equal(keys, []any{int64(1), int64(2), int64(3)}) {
		t.Fatalf("int keys = %v, %v", keys, ok)
	}
	keys, ok = inKeys([]any{int64(2), 1.5, int64(2)}, "float")
	if !ok || !slices.Equal(keys, []any{1.5, 2.0}) {
		t.Fatalf("float keys = %v, %v", keys, ok)
	}
	if _, ok := inKeys([]any{"x"}, "int"); ok {
		t.Fatal("an unconvertible element must not probe")
	}
}
