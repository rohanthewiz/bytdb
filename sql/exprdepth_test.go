package sql

// Tests for the expression-depth bound (deep-look D-06).

import (
	"fmt"
	"strings"
	"testing"
)

// TestExprChainDepthRefused: chains the parser handles iteratively —
// arithmetic, ||, casts, subscripts, comparisons, IS NULL — used to build
// a tree as deep as the chain, and a million terms overflowed the Go
// stack in the first recursive walk: a fatal error that killed the
// process. Each must now be refused with "statement too complex", well
// below any stack limit, and nesting must not let chains add up past the
// bound either.
func TestExprChainDepthRefused(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, a text)`)
	cases := map[string]string{
		"plus":        "select 1" + strings.Repeat("+1", 1_000_000),
		"times":       "select 1" + strings.Repeat("*1", 50_000),
		"concat":      "select 'a'" + strings.Repeat("||'a'", 50_000),
		"cast":        "select 1" + strings.Repeat("::int", 50_000),
		"is null":     "select 1" + strings.Repeat(" is null", 50_000),
		"comparisons": "select 1" + strings.Repeat(" = 1", 50_000),
		"unary minus": "select " + strings.Repeat("-(", 900) + "1" + strings.Repeat(" +1", 20_000) + strings.Repeat(")", 900),
		// 900 nested parens (under maxParseDepth), each level the left
		// operand of a 20-term chain: no single loop is long, but the
		// left spine through all of them is 18,000 deep.
		"nested chains": "select " + strings.Repeat("(", 900) + "1" + strings.Repeat(")"+strings.Repeat("+1", 20), 900),
		"in where":      "select id from t where id = 1" + strings.Repeat("+1", 50_000),
		"function args": "select coalesce(1" + strings.Repeat("+1", 50_000) + ")",
	}
	for name, q := range cases {
		_, err := d.Exec(q)
		if err == nil || !strings.Contains(err.Error(), "statement too complex") {
			t.Errorf("%s: err = %v, want statement too complex", name, err)
		}
	}
}

// TestExprDepthBoundSparesWideStatements: the bound is on depth, not on
// how many operators a statement holds. Long OR/AND lists are n-ary
// nodes and stay shallow; a big multi-row INSERT with a cast in every
// cell builds many shallow trees; a long IN list is one node. None may
// trip it.
func TestExprDepthBoundSparesWideStatements(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, v int)`)

	var rows []string
	for i := range 20_000 {
		rows = append(rows, fmt.Sprintf("('%d'::int, %d::int + 1)", i, i))
	}
	if res := exec(t, d, "insert into t values "+strings.Join(rows, ", ")); res.RowsAffected != 20_000 {
		t.Fatalf("bulk insert affected %d", res.RowsAffected)
	}

	// The long lists run against a small table: what is under test is
	// that they parse, and evaluating 20,000 terms per row over 20,000
	// rows would only measure the (unindexed) evaluator.
	exec(t, d, `create table s (id int primary key, v int)`)
	exec(t, d, "insert into s values "+strings.Join(rows[:100], ", "))
	var ors []string
	for i := range 20_000 {
		ors = append(ors, fmt.Sprintf("id = %d", i))
	}
	res := exec(t, d, "select count(*) from s where "+strings.Join(ors, " or "))
	if res.Rows[0][0] != int64(100) {
		t.Fatalf("OR list count = %v", res.Rows[0][0])
	}
	ands := "select count(*) from s where id >= 0" + strings.Repeat(" and v > 0", 20_000)
	if res := exec(t, d, ands); res.Rows[0][0] != int64(100) {
		t.Fatalf("AND list count = %v", res.Rows[0][0])
	}
	var in []string
	for i := range 30_000 {
		in = append(in, fmt.Sprintf("%d::int", i))
	}
	if res := exec(t, d, "select count(*) from s where id in ("+strings.Join(in, ",")+")"); res.Rows[0][0] != int64(100) {
		t.Fatalf("IN list count = %v", res.Rows[0][0])
	}
	// A chain just under the bound still works end to end.
	if res := exec(t, d, "select 0"+strings.Repeat("+1", 9_000)); res.Rows[0][0] != int64(9_000) {
		t.Fatalf("9,000-term chain = %v", res.Rows[0][0])
	}
}
