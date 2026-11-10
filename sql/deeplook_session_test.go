package sql

// Session regression tests from the deep-look remediation plan.

import (
	"strings"
	"testing"
)

// TestSessionParseErrorFailsBlock (D-07): a statement that failed to
// parse returned before the block's bookkeeping, so the block stayed
// healthy and COMMIT persisted the writes before the typo. Postgres
// fails the block on any error, parse errors included, and answers the
// COMMIT with ROLLBACK.
func TestSessionParseErrorFailsBlock(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table b (id int primary key)`)
	s := d.NewSession()
	defer s.Close()

	sessExec(t, s, `begin`)
	sessExec(t, s, `insert into b values (100)`)
	if _, err := s.Exec(`insrt into b values (101)`); err == nil {
		t.Fatal("typo parsed")
	}
	if s.Status() != TxFailed {
		t.Fatalf("status after a parse error in the block = %c; want E", s.Status())
	}
	if _, err := s.Exec(`select 1`); err == nil || !strings.Contains(err.Error(), "current transaction is aborted") {
		t.Fatalf("statement in the failed block: err = %v", err)
	}
	// BEGIN is refused in a failed block too (25P02), not just warned.
	if _, err := s.Exec(`begin`); err == nil || !strings.Contains(err.Error(), "current transaction is aborted") {
		t.Fatalf("BEGIN in the failed block: err = %v", err)
	}
	if res := sessExec(t, s, `commit`); res.Tag != "ROLLBACK" {
		t.Fatalf("COMMIT of a failed block: tag %q; want ROLLBACK", res.Tag)
	}
	if res := exec(t, d, `select count(*) from b`); res.Rows[0][0] != int64(0) {
		t.Fatalf("the failed block's insert was committed: %v", res.Rows)
	}

	// Outside a block a parse error changes nothing.
	if _, err := s.Exec(`insrt into b values (1)`); err == nil {
		t.Fatal("typo parsed")
	}
	if s.Status() != TxIdle {
		t.Fatalf("status after an idle parse error = %c; want I", s.Status())
	}
	// A healthy block still only warns on a nested BEGIN.
	sessExec(t, s, `begin`)
	if res := sessExec(t, s, `begin`); res.Notice == "" {
		t.Fatal("nested BEGIN in a healthy block: no warning")
	}
	sessExec(t, s, `rollback`)
}

// TestSubqueryLimitInterimReject (D-09): the subquery runners ignore
// some ORDER BY / LIMIT / OFFSET combinations, which returned wrong rows
// silently. Until subqueries run through the real SELECT path (D-27)
// those shapes are refused; the shapes the runners get right still run.
func TestSubqueryLimitInterimReject(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table o (id int primary key, amt int)`)
	exec(t, d, `insert into o values (1, 10), (2, 50), (3, 30), (4, 5), (5, 99)`)
	for _, q := range []string{
		`select (select amt from o order by amt desc limit 1)`,
		`select (select amt from o order by amt offset 1 limit 1)`,
		`select id from o where id in (select id from o order by amt desc limit 2)`,
		`select id from o where id = any (select id from o limit 2)`,
		`select exists (select 1 from o limit 0)`,
		`select exists (select 1 from o offset 10)`,
		`select array(select amt from o limit 2)`,
		`select array(select amt from o order by amt desc)`,
		`select array(select amt from o order by id)`,
	} {
		if _, err := d.Exec(q); err == nil || !strings.Contains(err.Error(), "not supported yet") {
			t.Errorf("%s: err = %v, want the interim refusal", q, err)
		}
	}
	for q, want := range map[string]any{
		`select exists (select 1 from o where amt > 90 limit 1)`: true,
		`select (select amt from o limit 1)`:                      int64(10),
		`select (select max(amt) from o)`:                        int64(99),
		`select array(select amt from o order by amt)`:           "{5,10,30,50,99}",
		`select array(select amt from o order by 1)`:             "{5,10,30,50,99}",
		`select count(*) from o where id in (select id from o where amt > 20)`: int64(3),
	} {
		res, err := d.Exec(q)
		if err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		if got := res.Rows[0][0]; got != want {
			t.Errorf("%s = %v (%T), want %v", q, got, got, want)
		}
	}
}
