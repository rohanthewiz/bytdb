package pgwire

import (
	"bytes"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/sql"
)

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
		// Resource refusals: the engine's result row cap and memory
		// ceiling (bytdb.SetMaxResultRows / ErrOutOfMemory).
		{"query would hold more rows in memory than the result row limit allows; narrow it", false, "54000"},
		{"out of memory", false, "53200"},

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

		// Literal parse failures, whatever the type.
		{"invalid input syntax for type int", false, "22P02"},
		{"invalid input syntax for type uuid", false, "22P02"},

		// Bind-parameter decoding: text vs binary representation.
		{"bad integer parameter", false, "22P02"},
		{"bad bytea parameter", false, "22P02"},
		{"bad binary int8 parameter", false, "22P03"},
		{"bad binary array parameter", false, "22P03"},
		// A format code other than text/binary is a malformed Bind.
		{"bad parameter format code", false, "08P01"},

		// Every primary-key invariant refusal.
		{`multiple primary keys for table "t" are not allowed`, false, "42P16"},
		{"multiple primary keys", false, "42P16"},
		{"cannot add a primary key column", false, "42P16"},
		{`cannot drop constraint "t_pkey" of relation "t"`, false, "42P16"},
		{"cannot drop a primary key column", false, "42P16"},
		{"a primary key is required", false, "42P16"},
		{`column "id" is in a primary key`, false, "42P16"},

		// Naming a column that isn't there, or naming one twice.
		{"primary key column not declared", false, "42703"},
		{`column "zz" of relation "t" does not exist`, false, "42703"},
		{"duplicate primary key column", false, "42701"},
		{`column "a" appears twice in primary key constraint`, false, "42701"},
		{`column "b" of relation "t" already exists`, false, "42701"},
		// A constraint's "does not exist" stays 42704, not 42703.
		{`constraint "c" of relation "t" does not exist`, false, "42704"},

		{"wrong number of parameters", false, "08P01"},
		{"current transaction is aborted, commands ignored", false, "25P02"},
		{"cannot execute INSERT in a read-only transaction", false, "25006"},
		{"CREATE INDEX CONCURRENTLY cannot run inside a transaction block", false, "25001"},
		{"SET TRANSACTION ISOLATION LEVEL must be called before any query", false, "25001"},
		{"SAVEPOINT can only be used in transaction blocks", false, "25P01"},
		{"savepoint sp1 does not exist", false, "3B001"},

		// Every dependency refusal shares one code, whichever object
		// (FK, CHECK, index) is in the way.
		{`cannot drop table "p" because other objects depend on it`, false, "2BP01"},
		{`cannot drop column "a" of table "t" because other objects depend on it`, false, "2BP01"},
		{`cannot rename column "a" of table "t" because other objects depend on it`, false, "2BP01"},
		{"cannot drop the unique index a foreign key depends on", false, "2BP01"},
		{"cannot drop the primary key a foreign key depends on", false, "2BP01"},
		{"cannot drop an indexed column; drop the index first", false, "2BP01"},
		{"cannot drop a foreign key column; drop the constraint first", false, "2BP01"},
		{"cannot drop a column referenced by a foreign key", false, "2BP01"},
		{"cannot rename a table referenced by a foreign key", false, "2BP01"},
		{"cannot rename a column referenced by a foreign key", false, "2BP01"},
		{"cannot change the type of a foreign key column; drop the constraint first", false, "2BP01"},
		{"cannot change the type of a column referenced by a foreign key; drop the constraint first", false, "2BP01"},
		// Postgres's own code for TRUNCATE of a referenced table stays.
		{"cannot truncate a table referenced in a foreign key constraint", false, "0A000"},

		// Anything unrecognized is internal_error.
		{"disk exploded", false, "XX000"},
	}
	for _, c := range cases {
		if got := sqlstate(c.msg, c.hasPos); got != c.want {
			t.Errorf("sqlstate(%q, %v) = %s; want %s", c.msg, c.hasPos, got, c.want)
		}
	}
}

// TestResourceErrorsEndToEnd renders the real errors, wrapped the way
// the engine and executor wrap them, so a reworded message or an
// extra wrapping layer cannot silently drop them to XX000.
func TestResourceErrorsEndToEnd(t *testing.T) {
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	d := sql.New(e)
	if _, err := d.Exec(`create table t (id int primary key)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`insert into t values (1), (2), (3)`); err != nil {
		t.Fatal(err)
	}

	e.SetMaxResultRows(2)
	_, err = d.Exec(`select * from t`)
	if got := codeOf(errorBody(err, "", 0)); got != "54000" {
		t.Errorf("row cap: SQLSTATE %q (err %v), want 54000", got, err)
	}
	e.SetMaxResultRows(0)

	runtime.GC() // populate the marked-heap figure the ceiling reads
	e.SetMemoryCeiling(1)
	_, err = d.Exec(`insert into t values (4)`)
	if got := codeOf(errorBody(err, "", 0)); got != "53200" {
		t.Errorf("memory ceiling: SQLSTATE %q (err %v), want 53200", got, err)
	}
}

// codeOf extracts the 'C' (SQLSTATE) field from an ErrorResponse body.
func codeOf(b wbuf) string {
	fields := bytes.Split(b, []byte{0})
	for _, f := range fields {
		if len(f) > 1 && f[0] == 'C' {
			return string(f[1:])
		}
	}
	return ""
}
