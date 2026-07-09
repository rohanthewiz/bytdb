package pgwire

import "testing"

// TestSQLStateMapping pins every message-text -> SQLSTATE branch.
// bytdb errors carry no code, so sqlstate keys off stable message
// substrings; this table is the contract that clients (which switch on
// SQLSTATE, not text) depend on.
func TestSQLStateMapping(t *testing.T) {
	cases := []struct {
		msg    string
		hasPos bool
		want   string
	}{
		// A position always means the parser rejected the text,
		// regardless of the message wording.
		{"unexpected token", true, "42601"},

		{"no such table users", false, "42P01"},
		{"no such column age", false, "42703"},
		{"column ref is ambiguous", false, "42702"},

		// Both uniqueness wordings share one code.
		{"duplicate primary key", false, "23505"},
		{"unique index violation on idx_users_email", false, "23505"},

		// The three not-null wordings share one code.
		{"value violates not-null constraint", false, "23502"},
		{"primary key column may not be NULL", false, "23502"},
		{"column age contains null values", false, "23502"},

		// Both check wordings share one code.
		{"new row violates check constraint chk_age", false, "23514"},
		{"check constraint chk_age is violated by some row", false, "23514"},

		{"constraint chk_age already exists", false, "42710"},
		{"constraint chk_age does not exist", false, "42704"},

		{"wrong number of parameters", false, "08P01"},
		{"current transaction is aborted, commands ignored", false, "25P02"},
		{"cannot execute INSERT in a read-only transaction", false, "25006"},
		{"CREATE INDEX CONCURRENTLY cannot run inside a transaction block", false, "25001"},
		{"SAVEPOINT can only be used in transaction blocks", false, "25P01"},
		{"savepoint sp1 does not exist", false, "3B001"},

		// Anything unrecognized is internal_error.
		{"disk exploded", false, "XX000"},
	}
	for _, c := range cases {
		if got := sqlstate(c.msg, c.hasPos); got != c.want {
			t.Errorf("sqlstate(%q, %v) = %s; want %s", c.msg, c.hasPos, got, c.want)
		}
	}
}
