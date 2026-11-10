package sql

// Deep-look D-52: the parse-level pieces the Postgres difftest corpus
// (73_everyday_sql) cannot cover — error paths, FOR UPDATE under
// concurrent writes, schema-qualified names.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func TestEverydaySQLErrors(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, a int)`)
	exec(t, d, `create table u (id int primary key, a int)`)
	exec(t, d, `create table v (id int primary key)`)
	exec(t, d, `insert into t values (1, 1)`)
	for q, want := range map[string]string{
		`select (1, 2)`:                                          "row constructor",
		`select * from t where (id, a) < (1, 1)`:                 "only = and <>",
		`select * from t where (id, a) = (1, 1, 1)`:              "unequal number",
		`select * from t where (id, a) in (select id, a from t)`: "not supported",
		`values (1), (1, 2)`:                                     "same length",
		`select $x$abc`:                                          "unterminated dollar-quoted",
		`select * from t join u using (nope)`:                    "does not exist in right table",
		`select * from t join v using (a)`:                       "does not exist in right table",
		`select * from v join t using (a)`:                       "does not exist in left table",
		`select * from t, u join v using (id)`:                   "appears more than once in left table",
		`select x.t.a from t`:                                    `schema "x" does not exist`,
		`select a from t fetch first 1 rows with ties`:           "WITH TIES",
		`select a from t order by a nulls middle`:                "syntax error",
	} {
		_, err := d.Exec(q)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", q, err, want)
		}
	}
	// Three-part names resolve through public.
	if r := exec(t, d, `select public.t.a from t`); fmt.Sprint(r.Rows) != "[[1]]" {
		t.Errorf("public.t.a = %v", r.Rows)
	}
	// A string continued on a new line is one constant; on one line, an error.
	if r := exec(t, d, "select 'ab'\n  'cd'"); r.Rows[0][0] != "abcd" {
		t.Errorf("continued string = %v", r.Rows[0][0])
	}
	if _, err := d.Exec(`select 'ab' 'cd'`); err == nil {
		t.Error("same-line adjacent strings accepted")
	}
	// EXPLAIN names the set operation.
	if plan := explainText(t, d, `select a from t intersect select a from u`); !strings.Contains(plan, "HashSetOp Intersect") {
		t.Errorf("EXPLAIN INTERSECT:\n%s", plan)
	}
}

// FOR UPDATE under concurrent writes (plan Q3): the rows a block reads
// FOR UPDATE become serializable reads, so a concurrent writer of them
// makes the block's commit fail with a conflict instead of both
// proceeding on stale data. Without FOR UPDATE the same interleaving
// commits (snapshot isolation).
func TestForUpdateUnderConcurrentWrites(t *testing.T) {
	for _, forUpdate := range []bool{true, false} {
		d := openOCC(t)
		exec(t, d, `create table acct (id int primary key, bal int)`)
		exec(t, d, `insert into acct values (1, 100), (2, 100)`)
		a, b := d.NewSession(), d.NewSession()
		if _, err := a.Exec(`begin`); err != nil {
			t.Fatal(err)
		}
		q := `select bal from acct where id = 1`
		if forUpdate {
			q += ` for update`
		}
		if _, err := a.Exec(q); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Exec(`update acct set bal = 50 where id = 1`); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Exec(`update acct set bal = bal + 10 where id = 2`); err != nil {
			t.Fatal(err)
		}
		_, err := a.Exec(`commit`)
		switch {
		case forUpdate && !errors.Is(err, bytdb.ErrTxConflict):
			t.Errorf("FOR UPDATE: commit err = %v, want a conflict", err)
		case !forUpdate && err != nil:
			t.Errorf("plain SELECT: commit err = %v, want success", err)
		}
	}
}
