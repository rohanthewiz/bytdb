package sql

import (
	"reflect"
	"strings"
	"testing"
)

// TestLiteralCoercion: quoted literals adapt to the column type they
// meet, as untyped literals do in Postgres — clients like pgx's
// simple protocol render every argument quoted and depend on it.
func TestLiteralCoercion(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, score float, active bool, blob bytea, name text)`)
	exec(t, d, `insert into t values ('1', '2.5', 'true', '\x00ff', 'ada')`)
	exec(t, d, `insert into t (id, name) values ('2', 'grace')`)

	res := exec(t, d, `select score, active, blob from t where id = '1'`)
	if !reflect.DeepEqual(res.Rows, [][]any{{2.5, true, []byte{0x00, 0xff}}}) {
		t.Fatalf("got %v", res.Rows)
	}
	// Ranges, ON, and HAVING coerce too.
	if res := exec(t, d, `select id from t where score > '1'`); len(res.Rows) != 1 {
		t.Fatalf("range: %v", res.Rows)
	}
	if res := exec(t, d, `select a.id from t a join t b on a.id = b.id and b.id >= '2'`); len(res.Rows) != 1 {
		t.Fatalf("on: %v", res.Rows)
	}
	if res := exec(t, d, `select count(*) from t group by active having count(*) = '1'`); len(res.Rows) != 2 {
		t.Fatalf("having: %v", res.Rows)
	}
	if res := exec(t, d, `update t set score = '9', active = 'f' where id = '1'`); res.RowsAffected != 1 {
		t.Fatalf("update: %d", res.RowsAffected)
	}
	if res := exec(t, d, `select score, active from t where id = 1`); !reflect.DeepEqual(res.Rows, [][]any{{9.0, false}}) {
		t.Fatalf("after update: %v", res.Rows)
	}
	if res := exec(t, d, `delete from t where id = '2'`); res.RowsAffected != 1 {
		t.Fatalf("delete: %d", res.RowsAffected)
	}

	// Coercion feeds the planner: a quoted PK literal is still a
	// point get.
	st, err := Parse(`select * from t where id = '1'`)
	if err != nil {
		t.Fatal(err)
	}
	cst, err := d.coerceLiterals(st)
	if err != nil {
		t.Fatal(err)
	}
	sel := cst.(*Select)
	pl, err := planScan(d.e.Table("t"), "t", sel.Where)
	if err != nil {
		t.Fatal(err)
	}
	if pl.get == nil {
		t.Fatal("quoted pk literal did not plan a point get")
	}

	// A string column keeps string comparison; a text literal never
	// silently converts.
	if res := exec(t, d, `select id from t where name = '1'`); len(res.Rows) != 0 {
		t.Fatalf("text col: %v", res.Rows)
	}

	// Malformed literals error like Postgres instead of matching
	// nothing.
	for _, q := range []string{
		`select * from t where id = 'abc'`,
		`insert into t (id) values ('abc')`,
		`update t set score = 'abc' where id = 1`,
	} {
		if _, err := d.Exec(q); err == nil || !strings.Contains(err.Error(), "invalid input syntax") {
			t.Errorf("Exec(%q): err %v, want invalid input syntax", q, err)
		}
	}

	// The prepared statement's parsed tree must stay untouched by
	// per-execution coercion.
	ps, err := d.Prepare(`select id from t where id = '1'`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if res, err := ps.Exec(); err != nil || len(res.Rows) != 1 {
			t.Fatalf("prepared: %v", err)
		}
	}
	if v := ps.st.(*Select).Where.(*Pred).Val; v != "1" {
		t.Fatalf("prepared tree mutated: %v", v)
	}
}
