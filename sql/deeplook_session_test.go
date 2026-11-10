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
