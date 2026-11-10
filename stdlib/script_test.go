package stdlib

// Deep-look D-56: Exec with several statements runs them as a script —
// what migration runners do with a whole .sql file — and refuses
// arguments, as Postgres does.

import (
	"errors"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func TestExecScript(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	res, err := db.Exec(`
		create table m (id int primary key, v text);
		insert into m values (1, 'a; b'), (2, $$c;d$$);
		update m set v = 'x' where id = 2;`)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Errorf("RowsAffected = %d, want the last statement's 1", n)
	}
	var v string
	if err := db.QueryRow(`select v from m where id = 1`).Scan(&v); err != nil || v != "a; b" {
		t.Errorf("v = %q %v", v, err)
	}
	// One unit: a failure undoes the writes before it.
	_, err = db.Exec(`update m set v = 'gone' where id = 1; insert into m values (2, 'dup')`)
	if !errors.Is(err, bytdb.ErrUniqueViolation) {
		t.Fatalf("err = %v", err)
	}
	if err := db.QueryRow(`select v from m where id = 1`).Scan(&v); err != nil || v != "a; b" {
		t.Errorf("after the failed script v = %q %v", v, err)
	}
	if _, err := db.Exec(`select 1; select $1`, 1); err == nil || !strings.Contains(err.Error(), "multiple commands") {
		t.Errorf("script with arguments: %v", err)
	}
}
