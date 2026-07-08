package sql

// Fixed-example tests for semantic corners the suite did not cover:
// DML that scans the index it mutates, three-valued IN/NOT IN, bulk
// DML over index ranges, LIMIT/OFFSET corners, statement-level
// atomicity, cross-type comparison, NULL propagation through chained
// LEFT JOINs, and ALTER × index interactions.

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// explainsAs asserts the plan contains the given node (DML plans put
// the scan on a child line under "Update on t"/"Delete on t"), so a
// test exercising an index-path hazard cannot silently degrade into a
// seq scan.
func explainsAs(t *testing.T, d *DB, q, wantNode string) {
	t.Helper()
	res := exec(t, d, "explain "+q)
	var lines []string
	for _, r := range res.Rows {
		if s, ok := r[0].(string); ok {
			if strings.Contains(s, wantNode) {
				return
			}
			lines = append(lines, s)
		}
	}
	t.Fatalf("explain %q = %q; want a %s node", q, lines, wantNode)
}

// TestUpdateViaIndexItMutates: an UPDATE whose WHERE is served by the
// same secondary index its SET mutates. The executor must materialize
// the matching rows before writing; a live scan would revisit rows
// that move forward in the index (skip/duplicate/never-terminate) or
// miss rows that move backward.
func TestUpdateViaIndexItMutates(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, age int)`)
	exec(t, d, `create index by_age on t (age)`)
	exec(t, d, `insert into t values (1, 25), (2, 31), (3, 32), (4, 33), (5, 34), (6, 35)`)

	// Forward move: matching rows relocate *ahead* of the scan window
	// (31..35 -> 99, still > 30). A live index scan would meet each
	// moved row again.
	q := `update t set age = 99 where age > 30`
	explainsAs(t, d, q, "Index Scan")
	if res := exec(t, d, q); res.RowsAffected != 5 {
		t.Fatalf("forward move affected %d; want 5", res.RowsAffected)
	}
	res := exec(t, d, `select count(*) from t where age = 99`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(5)}}) {
		t.Fatalf("after forward move: %v", res.Rows)
	}

	// Backward move: rows relocate behind the scan (99 -> 20). A live
	// scan under-counts or leaves stale entries.
	if res := exec(t, d, `update t set age = 20 where age > 30`); res.RowsAffected != 5 {
		t.Fatalf("backward move affected %d; want 5", res.RowsAffected)
	}
	// The index must agree with the table afterward: every row scanned
	// through the index exactly once, no stale 99 entries.
	explainsAs(t, d, `select id from t where age >= 0`, "Index Scan")
	res = exec(t, d, `select count(*) from t where age >= 0`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(6)}}) {
		t.Fatalf("index disagrees with table: %v", res.Rows)
	}
	if res := exec(t, d, `select count(*) from t where age = 99`); res.Rows[0][0] != int64(0) {
		t.Fatalf("stale index entries at 99: %v", res.Rows)
	}
}

// TestInNotInWithNull pins the three-valued IN semantics: a NULL in
// the list can never make NOT IN true (x <> NULL is unknown), and a
// NULL probe value matches nothing in either direction.
func TestInNotInWithNull(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, x int)`)
	exec(t, d, `insert into t values (1, 1), (2, 2), (3, null)`)

	// IN with a NULL element: only definite matches qualify.
	res := exec(t, d, `select id from t where x in (1, null)`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}}) {
		t.Fatalf("IN (1, NULL): %v", res.Rows)
	}
	// NOT IN with a NULL element is never true: x=2 gives
	// (2<>1)=t AND (2<>NULL)=unknown -> unknown.
	if res := exec(t, d, `select id from t where x not in (1, null)`); len(res.Rows) != 0 {
		t.Fatalf("NOT IN (1, NULL) returned rows: %v", res.Rows)
	}
	// A NULL probe is unknown for both.
	if res := exec(t, d, `select id from t where x in (1, 2) and id = 3`); len(res.Rows) != 0 {
		t.Fatalf("NULL IN list returned rows: %v", res.Rows)
	}
	if res := exec(t, d, `select id from t where x not in (7, 8) and id = 3`); len(res.Rows) != 0 {
		t.Fatalf("NULL NOT IN list returned rows: %v", res.Rows)
	}
	// The subquery spelling of NOT IN — <> ALL — with a NULL row in
	// the subquery result: same three-valued collapse to zero rows.
	exec(t, d, `create table u (id int primary key, y int)`)
	exec(t, d, `insert into u values (1, 7), (2, null)`)
	if res := exec(t, d, `select id from t where x <> all (select y from u)`); len(res.Rows) != 0 {
		t.Fatalf("<> ALL over NULL-bearing subquery returned rows: %v", res.Rows)
	}
}

// TestBulkDMLViaIndexRange: multi-row UPDATE and DELETE planned as a
// secondary-index range scan (all prior DML WHERE tests were full-scan
// or PK point-get), with the index provably cleaned afterward.
func TestBulkDMLViaIndexRange(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, age int)`)
	exec(t, d, `create index by_age on t (age)`)
	for i := 1; i <= 10; i++ {
		exec(t, d, fmt.Sprintf(`insert into t values (%d, %d)`, i, 20+i))
	}

	q := `delete from t where age >= 23 and age < 26` // ages 23,24,25
	explainsAs(t, d, q, "Index Scan")
	if res := exec(t, d, q); res.RowsAffected != 3 {
		t.Fatalf("index-range delete affected %d; want 3", res.RowsAffected)
	}

	// Count through the index and through the primary key: a stale
	// index entry (delete that missed the index) breaks the agreement.
	explainsAs(t, d, `select id from t where age >= 0`, "Index Scan")
	viaIndex := exec(t, d, `select count(*) from t where age >= 0`).Rows[0][0]
	viaPK := exec(t, d, `select count(*) from t`).Rows[0][0]
	if viaIndex != int64(7) || viaPK != int64(7) {
		t.Fatalf("after delete: via index %v, via pk %v; want 7", viaIndex, viaPK)
	}

	// Re-inserting into the vacated range must land in the index once.
	exec(t, d, `insert into t values (11, 24)`)
	res := exec(t, d, `select id from t where age >= 23 and age < 26`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(11)}}) {
		t.Fatalf("vacated range after reinsert: %v", res.Rows)
	}

	// Multi-row UPDATE through the same index path.
	qu := `update t set age = 50 where age >= 27`
	explainsAs(t, d, qu, "Index Scan")
	if res := exec(t, d, qu); res.RowsAffected != 4 { // ages 27..30
		t.Fatalf("index-range update affected %d; want 4", res.RowsAffected)
	}
	if n := exec(t, d, `select count(*) from t where age = 50`).Rows[0][0]; n != int64(4) {
		t.Fatalf("moved rows via index: %v", n)
	}
}

// TestLimitOffsetCorners pins LIMIT 0, OFFSET past the end, and the
// dialect's stance on parameterized LIMIT/OFFSET (unsupported: the AST
// holds plain integers — a clear parse error, not a silent misread).
func TestLimitOffsetCorners(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key)`)
	exec(t, d, `insert into t values (1), (2), (3)`)

	if res := exec(t, d, `select id from t limit 0`); len(res.Rows) != 0 {
		t.Fatalf("LIMIT 0: %v", res.Rows)
	}
	if res := exec(t, d, `select id from t offset 99`); len(res.Rows) != 0 {
		t.Fatalf("OFFSET past end: %v", res.Rows)
	}
	if res := exec(t, d, `select id from t order by id limit 2 offset 2`); !reflect.DeepEqual(res.Rows, [][]any{{int64(3)}}) {
		t.Fatalf("window past-the-end: %v", res.Rows)
	}
	for _, q := range []string{
		`select id from t limit $1`,
		`select id from t offset $1`,
	} {
		if _, err := d.Exec(q); err == nil ||
			!strings.Contains(err.Error(), "placeholders are not supported") {
			t.Fatalf("Exec(%q): err %v; want placeholder rejection", q, err)
		}
	}
}

// TestStatementAtomicity: a multi-row statement that fails on a later
// row must undo the rows it already wrote — per-statement all-or-
// nothing, even outside a transaction block.
func TestStatementAtomicity(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, u int)`)
	exec(t, d, `create unique index by_u on t (u)`)
	exec(t, d, `insert into t values (1, 10), (2, 20), (3, 30), (4, 40), (5, 50)`)

	// UPDATE: row 4 -> u=60 succeeds, then row 5 -> u=60 collides with
	// row 4's fresh value. Row 4 must roll back.
	if _, err := d.Exec(`update t set u = 60 where id >= 4`); err == nil {
		t.Fatal("conflicting multi-row update succeeded")
	}
	res := exec(t, d, `select u from t where id >= 4 order by id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(40)}, {int64(50)}}) {
		t.Fatalf("update not atomic: %v", res.Rows)
	}

	// INSERT: second VALUES row collides on the unique index; the
	// first must not survive.
	if _, err := d.Exec(`insert into t values (6, 70), (7, 70)`); err == nil {
		t.Fatal("conflicting multi-row insert succeeded")
	}
	if res := exec(t, d, `select count(*) from t where id in (6, 7)`); res.Rows[0][0] != int64(0) {
		t.Fatalf("insert not atomic: %v", res.Rows)
	}
	// Same for a duplicate primary key within one statement.
	if _, err := d.Exec(`insert into t values (8, 80), (8, 81)`); err == nil {
		t.Fatal("duplicate-pk multi-row insert succeeded")
	}
	if res := exec(t, d, `select count(*) from t where id = 8`); res.Rows[0][0] != int64(0) {
		t.Fatalf("pk-dup insert not atomic: %v", res.Rows)
	}
}

// TestCrossTypeComparison pins eval-time comparison across numeric
// types and the text/number wall: int and float compare by value;
// text never silently compares to a number.
func TestCrossTypeComparison(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, i int, f float, s text)`)
	exec(t, d, `insert into t values (1, 2, 2.0, '2'), (2, 3, 2.5, '10')`)

	// int column vs float literal: numeric equality and ordering.
	if res := exec(t, d, `select id from t where i = 2.0`); !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}}) {
		t.Fatalf("i = 2.0: %v", res.Rows)
	}
	if res := exec(t, d, `select id from t where i = 2.5`); len(res.Rows) != 0 {
		t.Fatalf("i = 2.5 matched: %v", res.Rows)
	}
	if res := exec(t, d, `select id from t where i < 2.5`); !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}}) {
		t.Fatalf("i < 2.5: %v", res.Rows)
	}
	// float column vs int literal.
	if res := exec(t, d, `select id from t where f = 2`); !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}}) {
		t.Fatalf("f = 2: %v", res.Rows)
	}
	// int column vs float column, both directions of magnitude.
	if res := exec(t, d, `select id from t where i > f order by id`); !reflect.DeepEqual(res.Rows, [][]any{{int64(2)}}) {
		t.Fatalf("i > f: %v", res.Rows)
	}
	// text vs number errors, as in Postgres. (Parsing the stored text
	// per row would give data-dependent semantics: s='2' would match
	// s < 5 while s='abc' silently wouldn't.)
	for _, q := range []string{
		`select id from t where s < 5`,
		`select id from t where s = 5`,
		`select id from t where s = true`,
		`select id from t where s < i`, // text column vs int column
	} {
		if _, err := d.Exec(q); err == nil ||
			!strings.Contains(err.Error(), "operator does not exist") {
			t.Fatalf("Exec(%q): err %v; want operator-does-not-exist", q, err)
		}
	}
	// The quoted-literal direction still adapts (untyped literal to
	// the column): these are unaffected by the text-column rule.
	if res := exec(t, d, `select id from t where i = '2'`); !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}}) {
		t.Fatalf("i = '2': %v", res.Rows)
	}
}

// TestChainedLeftJoinNulls: two chained LEFT JOINs where the middle
// table misses — the NULLs must propagate through the second join's ON
// (NULL = anything is unknown, so the third table contributes NULLs
// too, not an error and not a dropped row).
func TestChainedLeftJoinNulls(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table a (id int primary key, name text)`)
	exec(t, d, `create table b (id int primary key, a_id int, tag text)`)
	exec(t, d, `create table c (id int primary key, b_id int, note text)`)
	exec(t, d, `insert into a values (1, 'with'), (2, 'without')`)
	exec(t, d, `insert into b values (10, 1, 'b1')`)
	exec(t, d, `insert into c values (100, 10, 'c1')`)

	res := exec(t, d, `select a.name, b.tag, c.note from a
		left join b on b.a_id = a.id
		left join c on c.b_id = b.id
		order by a.id`)
	want := [][]any{
		{"with", "b1", "c1"},
		{"without", nil, nil},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("chained left joins: %v", res.Rows)
	}

	// Aggregates over the padded rows count real values only.
	res = exec(t, d, `select count(c.note) from a
		left join b on b.a_id = a.id
		left join c on c.b_id = b.id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}}) {
		t.Fatalf("count over padded rows: %v", res.Rows)
	}
}

// TestCorrelatedSubqueryDelete: EXISTS over a correlated subquery
// drives row selection for DELETE.
func TestCorrelatedSubqueryDelete(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key)`)
	exec(t, d, `create table dead (t_id int primary key)`)
	exec(t, d, `insert into t values (1), (2), (3), (4)`)
	exec(t, d, `insert into dead values (2), (4)`)

	res := exec(t, d, `delete from t where exists (select 1 from dead where dead.t_id = t.id)`)
	if res.RowsAffected != 2 {
		t.Fatalf("correlated delete affected %d; want 2", res.RowsAffected)
	}
	left := exec(t, d, `select id from t order by id`)
	if !reflect.DeepEqual(left.Rows, [][]any{{int64(1)}, {int64(3)}}) {
		t.Fatalf("survivors: %v", left.Rows)
	}
}

// TestAddColumnWithExistingIndex: ADD COLUMN on a table that already
// has a secondary index — existing entries stay valid, and rows
// inserted after the ALTER flow through both old and new shapes.
func TestAddColumnWithExistingIndex(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, age int)`)
	exec(t, d, `create index by_age on t (age)`)
	exec(t, d, `insert into t values (1, 30), (2, 40)`)

	exec(t, d, `alter table t add column note text`)
	exec(t, d, `insert into t values (3, 35, 'new-shape')`)

	explainsAs(t, d, `select id from t where age >= 30`, "Index Scan")
	res := exec(t, d, `select id, note from t where age >= 30 order by id`)
	want := [][]any{{int64(1), nil}, {int64(2), nil}, {int64(3), "new-shape"}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("index over altered table: %v", res.Rows)
	}
}
