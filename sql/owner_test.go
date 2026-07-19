package sql

// ALTER TABLE ... OWNER TO tests. bytdb has no roles, so the statement
// is parse-and-ignore: it must succeed without touching anything, and —
// unlike real bytdb DDL — it must be allowed inside a transaction
// block, because migration tools (goose) wrap each migration in one and
// pg_dump-derived DDL carries OWNER TO lines throughout.

import (
	"strings"
	"testing"
)

func TestAlterOwnerIsNoOp(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, v text)`)
	exec(t, d, `insert into t values (1, 'a')`)

	// Unquoted and quoted role names, and Postgres pseudo-roles, all
	// accept; none change anything observable. The command tag is what
	// the wire layer reports, so it must read ALTER TABLE, not blank.
	for _, q := range []string{
		`alter table t owner to devuser`,
		`alter table t owner to "devuser"`,
		`alter table t owner to current_user`,
	} {
		ps, err := d.Prepare(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if got := ps.Command(); got != "ALTER TABLE" {
			t.Fatalf("%s: command = %q; want ALTER TABLE", q, got)
		}
		exec(t, d, q)
	}
	if res := exec(t, d, `select v from t where id = 1`); len(res.Rows) != 1 || res.Rows[0][0] != "a" {
		t.Fatalf("rows after OWNER TO: %v", res.Rows)
	}

	// Malformed forms still fail like any other syntax error.
	if _, err := d.Exec(`alter table t owner devuser`); err == nil {
		t.Fatal("OWNER without TO accepted")
	}
	if _, err := d.Exec(`alter table t owner to`); err == nil {
		t.Fatal("OWNER TO without a role accepted")
	}
}

func TestAlterOwnerInsideTransaction(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key)`)
	s := d.NewSession()
	defer s.Close()

	// The goose shape: BEGIN, schema statements, COMMIT. Real DDL is
	// refused inside a block, but the OWNER no-op must pass through or
	// unmodified pg_dump migrations abort here.
	sessExec(t, s, `begin`)
	sessExec(t, s, `insert into t values (1)`)
	sessExec(t, s, `alter table t owner to "devuser"`)
	sessExec(t, s, `commit`)
	if res := exec(t, d, `select * from t`); len(res.Rows) != 1 {
		t.Fatalf("rows after block: %v", res.Rows)
	}

	// Sanity: the DDL-in-block guard still holds for real ALTERs, so
	// the OWNER carve-out did not widen it.
	sessExec(t, s, `begin`)
	if _, err := s.Exec(`alter table t add column v text`); err == nil ||
		!strings.Contains(err.Error(), "transaction block") {
		t.Fatalf("real DDL in block: %v", err)
	}
	sessExec(t, s, `rollback`)
}
