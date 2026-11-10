package sql

// Deep-look D-52 part 2: INSERT ... SELECT, UPDATE ... FROM, DELETE ...
// USING, target aliases, SET col = DEFAULT. The Postgres comparison is
// difftest corpus 74_dml_forms; these cover what it cannot.

import (
	"fmt"
	"strings"
	"testing"
)

func TestInsertSelect(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table src (id int primary key, a int, b text)`)
	exec(t, d, `create table dst (id int primary key, a int, b text default 'd')`)
	exec(t, d, `insert into src values (1, 1, 'x'), (2, 2, 'y'), (3, 3, 'z')`)

	r := exec(t, d, `insert into dst (id, a) select id, a * 10 from src where id < 3 returning id, a, b`)
	if got := fmt.Sprint(r.Rows); got != "[[1 10 d] [2 20 d]]" {
		t.Fatalf("returning = %s", got)
	}
	// A derived table inside the query (its CTE attaches to the query).
	exec(t, d, `insert into dst select v.x + 100, v.x, 'v' from (select id as x from src) v`)
	// Parameters inside the query bind like any statement's.
	if _, err := d.Exec(`insert into dst select id + $1, a, b from src where id = $2`, 200, 3); err != nil {
		t.Fatal(err)
	}
	// The query reads the statement's starting snapshot, never the rows
	// it is inserting: doubling a table adds exactly its old row count.
	exec(t, d, `insert into src select id + 10, a, b from src`)
	if got := fmt.Sprint(exec(t, d, `select count(*) from src`).Rows); got != "[[6]]" {
		t.Fatalf("self insert left %s rows", got)
	}
	exec(t, d, `insert into dst select id, a, b from src on conflict (id) do nothing`)
	if got := fmt.Sprint(exec(t, d, `select count(*) from dst`).Rows); got != "[[10]]" {
		t.Fatalf("dst has %s rows", got)
	}
	for q, want := range map[string]string{
		`insert into dst select id, a, b, b from src`:   "more expressions than target columns",
		`insert into dst (id, a, b) select id from src`: "more target columns than expressions",
	} {
		if _, err := d.Exec(q); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", q, err, want)
		}
	}
}

func TestUpdateFromDeleteUsing(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table acct (id int primary key, bal int, tag text, k uuid default gen_random_uuid())`)
	exec(t, d, `create table adj (id int primary key, acct int, amt int)`)
	exec(t, d, `insert into acct (id, bal, tag) values (1, 100, 'a'), (2, 200, 'b'), (3, 300, 'c')`)
	// Two adjustments match account 1: it is updated once.
	exec(t, d, `insert into adj values (1, 1, 5), (2, 1, 7), (3, 2, 9)`)
	r := exec(t, d, `update acct a set bal = a.bal + j.amt from adj j where j.acct = a.id returning a.id`)
	if r.RowsAffected != 2 {
		t.Fatalf("UPDATE FROM touched %d rows, want 2 (one per target row)", r.RowsAffected)
	}
	got := fmt.Sprint(exec(t, d, `select id, bal from acct order by id`).Rows)
	if got != "[[1 105] [2 209] [3 300]]" && got != "[[1 107] [2 209] [3 300]]" {
		t.Fatalf("balances = %s", got)
	}
	// SET col = DEFAULT with a per-row default draws a fresh value per row.
	exec(t, d, `update acct set k = default`)
	if got := fmt.Sprint(exec(t, d, `select count(distinct k) from acct`).Rows); got != "[[3]]" {
		t.Fatalf("distinct uuids after SET k = DEFAULT: %s", got)
	}
	// RETURNING reads the target only.
	if _, err := d.Exec(`update acct set bal = 1 from adj where adj.acct = acct.id returning adj.amt`); err == nil {
		t.Error("RETURNING of a FROM table's column accepted")
	}
	r = exec(t, d, `delete from acct x using adj j where j.acct = x.id and j.amt > 8 returning x.id`)
	if got := fmt.Sprint(r.Rows); got != "[[2]]" {
		t.Fatalf("DELETE USING returned %s", got)
	}
	if got := fmt.Sprint(exec(t, d, `select id from acct order by id`).Rows); got != "[[1] [3]]" {
		t.Fatalf("after DELETE USING: %s", got)
	}
}
