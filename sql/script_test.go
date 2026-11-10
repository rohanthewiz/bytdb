package sql

// Deep-look D-56 part 2: ExecScript (one unit, DDL carve-out, stop at
// the first error), SplitScript's dollar quoting, and SQL parameter
// binding of named Go types, pointers, driver.Valuer and []string.

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

func TestExecScript(t *testing.T) {
	d := openDB(t)
	results, err := d.ExecScript(`
		-- a migration file
		create table acct (id int primary key, bal int, note text);
		insert into acct values (1, 100, $$it's; fine$$), (2, 50, 'x');
		update acct set bal = bal - 10 where id = 1;
		/* trailing comment */`)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || results[1].RowsAffected != 2 {
		t.Fatalf("results %d, rows %v", len(results), results)
	}
	if r := exec(t, d, `select note from acct where id = 1`); r.Rows[0][0] != "it's; fine" {
		t.Errorf("dollar-quoted text = %v", r.Rows[0][0])
	}

	// A failure rolls back the unit's writes before it and names the
	// failing statement.
	_, err = d.ExecScript(`update acct set bal = 0 where id = 2; insert into acct values (1, 0, 'dup')`)
	if !errors.Is(err, bytdb.ErrUniqueViolation) {
		t.Fatalf("err = %v", err)
	}
	if f := serr.SErrFromErr(err).FieldsMap(); f["statement"] != "2" {
		t.Errorf("fields %v", f)
	}
	if r := exec(t, d, `select bal from acct where id = 2`); r.Rows[0][0] != int64(50) {
		t.Errorf("failed script's first write survived: bal = %v", r.Rows[0][0])
	}

	// In a session's explicit block, the script joins the block.
	s := d.NewSession()
	defer s.Close()
	sexec(t, s, `begin`)
	if _, err := s.ExecScript(`update acct set bal = 1 where id = 2; update acct set bal = 2 where id = 1`); err != nil {
		t.Fatal(err)
	}
	sexec(t, s, `rollback`)
	if r := exec(t, d, `select bal from acct order by id`); fmt.Sprint(r.Rows) != "[[90] [50]]" {
		t.Errorf("block-joined script not rolled back: %v", r.Rows)
	}
	if res, err := d.ExecScript(" ; -- nothing\n"); err != nil || len(res) != 0 {
		t.Errorf("empty script: %v %v", res, err)
	}
}

func TestSplitScriptDollarQuotes(t *testing.T) {
	for q, want := range map[string]int{
		`select $$a;b$$; select 2`:          2,
		`select $t$ ; $$ ; $t$; select 2`:   2,
		`select $1; select a$b; select 3`:   3,
		`select 'a;b'; select E'\';'; x`:    3,
		`select 1 /* ; /* ; */ ; */; sel 2`: 2,
	} {
		if got := len(SplitScript(q)); got != want {
			t.Errorf("SplitScript(%q) = %d parts, want %d: %v", q, got, want, SplitScript(q))
		}
	}
}

type tenantID int64
type label string

func TestBindGoShapes(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, name text, tags text[], score float8)`)
	name := "ada"
	if _, err := d.Exec(`insert into t values ($1, $2, $3, $4)`, tenantID(1), &name, []string{"a", "b c"}, int32(7)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`insert into t values ($1, $2, $3, $4)`, uint64(2), sql.NullString{}, nil, uint8(1)); err != nil {
		t.Fatal(err)
	}
	r, err := d.Exec(`select name, tags, score from t where id = $1 and name = $2`, tenantID(1), label("ada"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(r.Rows); got != `[[ada {a,"b c"} 7]]` {
		t.Errorf("rows %s", got)
	}
	if r := exec(t, d, `select name is null from t where id = 2`); r.Rows[0][0] != true {
		t.Error("an invalid NullString did not bind NULL")
	}
	if _, err := d.Exec(`select $1`, struct{}{}); err == nil || !strings.Contains(err.Error(), "unsupported parameter type") {
		t.Errorf("struct parameter: %v", err)
	}
}
