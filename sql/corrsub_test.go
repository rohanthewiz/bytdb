package sql

// Correlated-subquery pushdown tests (corrsub.go): result equivalence
// across every shape the template extraction touches — including the
// shapes it must refuse (OR-nested, LEFT-joined, type-mismatched) —
// plus a scale check that would take tens of seconds under the old
// full-scan-per-outer-row execution.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// corrDB: outer (5 rows, k points at inner ids, one NULL, one dangling)
// and inner (4 rows, id is the primary key the templates push into).
func corrDB(t *testing.T) *DB {
	t.Helper()
	d := openDB(t)
	exec(t, d, `create table outer_t (id int primary key, k int, f float)`)
	exec(t, d, `create table inner_t (id int primary key, v int)`)
	exec(t, d, `insert into inner_t values (10, 1), (20, 2), (30, 3), (40, 2)`)
	exec(t, d, `insert into outer_t values
		(1, 10, 10.0),
		(2, 30, 30.5),
		(3, null, null),
		(4, 99, 99.0),
		(5, 20, 20.0)`)
	return d
}

// one column of int64/NULL results, in outer id order.
func col0(t *testing.T, d *DB, q string) []any {
	t.Helper()
	res := exec(t, d, q)
	out := make([]any, len(res.Rows))
	for i, r := range res.Rows {
		out[i] = r[0]
	}
	return out
}

func wantVals(t *testing.T, got []any, want ...any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("row count: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d: got %v want %v (all: %v)", i, got[i], want[i], want)
		}
	}
}

func TestCorrelatedScalarAggPushdown(t *testing.T) {
	d := corrDB(t)

	// Point lookup on the inner primary key; NULL and dangling outer
	// values still count zero rows.
	got := col0(t, d, `select (select count(*) from inner_t i where i.id = o.k)
		from outer_t o order by o.id`)
	wantVals(t, got, int64(1), int64(1), int64(0), int64(0), int64(1))

	// min over a pushed range; zero matching rows read as NULL.
	got = col0(t, d, `select (select min(i.v) from inner_t i where i.id < o.k)
		from outer_t o order by o.id`)
	wantVals(t, got, nil, int64(1), nil, int64(1), int64(1))

	// Outer reference on the left of the comparison: the template is
	// the flipped predicate, same results.
	got = col0(t, d, `select (select count(*) from inner_t i where o.k = i.id)
		from outer_t o order by o.id`)
	wantVals(t, got, int64(1), int64(1), int64(0), int64(0), int64(1))
}

func TestCorrelatedScalarPlainAndDistinct(t *testing.T) {
	d := corrDB(t)

	// Non-aggregate scalar subquery: the single matching row's value,
	// NULL when nothing matches.
	got := col0(t, d, `select (select i.v from inner_t i where i.id = o.k)
		from outer_t o order by o.id`)
	wantVals(t, got, int64(1), int64(3), nil, nil, int64(2))

	// DISTINCT path with a correlated equality alongside a local one.
	got = col0(t, d, `select (select distinct i.v from inner_t i
		where i.id = o.k and i.v > 0) from outer_t o order by o.id`)
	wantVals(t, got, int64(1), int64(3), nil, nil, int64(2))
}

func TestCorrelatedExistsAnyArray(t *testing.T) {
	d := corrDB(t)

	got := col0(t, d, `select exists (select 1 from inner_t i where i.id = o.k)
		from outer_t o order by o.id`)
	wantVals(t, got, true, true, false, false, true)

	// ANY over the correlated column set: v of the row i.id = o.k.
	got = col0(t, d, `select 2 = any (select i.v from inner_t i where i.id = o.k)
		from outer_t o order by o.id`)
	wantVals(t, got, false, false, false, false, true)

	got = col0(t, d, `select array(select i.v from inner_t i where i.id = o.k)
		from outer_t o order by o.id`)
	wantVals(t, got, "{1}", "{3}", "{}", "{}", "{2}")
}

// Shapes the extraction must refuse — results must match plain
// per-row Cond evaluation exactly.
func TestCorrelatedPushdownRefusals(t *testing.T) {
	d := corrDB(t)

	// Under OR the correlated predicate is not individually required:
	// no template, and both disjuncts' matches survive.
	got := col0(t, d, `select (select count(*) from inner_t i
		where i.id = o.k or i.v = 2) from outer_t o order by o.id`)
	wantVals(t, got, int64(3), int64(3), int64(2), int64(2), int64(2))

	// Go-type mismatch (float outer vs int primary key): litFits
	// refuses the push, the residual comparison still coerces. 30.5
	// matches nothing; the .0 floats match their rows.
	got = col0(t, d, `select (select count(*) from inner_t i where i.id = o.f)
		from outer_t o order by o.id`)
	wantVals(t, got, int64(1), int64(0), int64(0), int64(0), int64(1))
}

// A correlated WHERE predicate on the NULL-extended side of a LEFT
// JOIN must not narrow that table's scan (prepareFrom's own rule);
// the post-join filter handles it, including the NULL-extended rows.
func TestCorrelatedLeftJoinSubquery(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table pa (id int primary key)`)
	exec(t, d, `create table ch (id int primary key, pid int, tag int)`)
	exec(t, d, `create table oo (id int primary key, want int)`)
	exec(t, d, `insert into pa values (1), (2)`)
	exec(t, d, `insert into ch values (100, 1, 7), (101, 1, 8)`)
	exec(t, d, `insert into oo values (1, 7), (2, 9)`)

	// Per outer row: parents whose left-joined child carries oo.want.
	got := col0(t, d, `select (select count(*) from pa left join ch on ch.pid = pa.id
		where ch.tag = o.want) from oo o order by o.id`)
	wantVals(t, got, int64(1), int64(0))

	// Correlated predicate on the non-LEFT side still pushes; the LEFT
	// join NULL-extends around it.
	got = col0(t, d, `select (select count(*) from pa left join ch on ch.pid = pa.id
		where pa.id = o.id) from oo o order by o.id`)
	wantVals(t, got, int64(2), int64(1))
}

// Correlated joins inside the subquery: the correlated conjunct binds
// to its owning step while the join equality keeps its own machinery
// (index nested loop or hash join).
func TestCorrelatedSubqueryWithJoin(t *testing.T) {
	d := corrDB(t)
	exec(t, d, `create table tags (id int primary key, iid int, label text)`)
	exec(t, d, `insert into tags values (1, 10, 'a'), (2, 30, 'b'), (3, 30, 'c')`)

	got := col0(t, d, `select (select count(*) from inner_t i join tags g on g.iid = i.id
		where i.id = o.k) from outer_t o order by o.id`)
	wantVals(t, got, int64(1), int64(2), int64(0), int64(0), int64(0))
}

// TestCorrelatedSubqueryScale is the headline regression: 3000 outer
// rows each running a correlated count against a 3000-row inner table
// keyed by primary key. The old execution full-scanned the inner
// table per outer row (9M row visits, tens of seconds); the template
// pushdown makes each invocation one point lookup. The elapsed-time
// bound carries ~100x headroom over the fixed behavior while sitting
// far under the old one.
func TestCorrelatedSubqueryScale(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table lo (id int primary key, k int)`)
	exec(t, d, `create table ri (id int primary key, v int)`)
	const n = 3000
	for base := 0; base < n; base += 500 {
		var lv, rv []string
		for i := base; i < base+500; i++ {
			lv = append(lv, fmt.Sprintf("(%d, %d)", i, i))
			rv = append(rv, fmt.Sprintf("(%d, %d)", i, i%7))
		}
		exec(t, d, "insert into lo values "+strings.Join(lv, ","))
		exec(t, d, "insert into ri values "+strings.Join(rv, ","))
	}
	start := time.Now()
	res := exec(t, d, `select count(*) from lo
		where (select count(*) from ri where ri.id = lo.k) = 1`)
	elapsed := time.Since(start)
	if res.Rows[0][0].(int64) != n {
		t.Fatalf("scale count: %v", res.Rows[0][0])
	}
	if elapsed > 5*time.Second {
		t.Fatalf("correlated scale query took %v; pushdown regressed", elapsed)
	}
}
