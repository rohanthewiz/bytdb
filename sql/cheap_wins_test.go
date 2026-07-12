package sql

// Tests for the operability batch: TRUNCATE, VARCHAR(n) enforcement,
// UNIQUE constraint sugar, and SHOW.

import (
	"strings"
	"testing"
)

func TestTruncate(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table a (id serial primary key, v text)`)
	exec(t, d, `create table b (id int primary key, a_v text)`)
	exec(t, d, `create index a_v_idx on a (v)`)
	exec(t, d, `insert into a (v) values ('x'), ('y')`)
	exec(t, d, `insert into b values (1, 'x')`)

	// Multi-table truncate is one statement, indexes emptied too.
	res := exec(t, d, `truncate a, b`)
	if res.RowsAffected != 0 || len(res.Rows) != 0 {
		t.Fatalf("truncate result: %+v", res)
	}
	for _, q := range []string{
		`select count(*) from a`,
		`select count(*) from b`,
		`select count(*) from a where v = 'x'`, // through the index
	} {
		if res := exec(t, d, q); res.Rows[0][0].(int64) != 0 {
			t.Fatalf("%s = %v after truncate", q, res.Rows[0][0])
		}
	}

	// CONTINUE IDENTITY (the default): the counter keeps counting.
	exec(t, d, `insert into a (v) values ('z')`)
	if res := exec(t, d, `select id from a`); res.Rows[0][0].(int64) != 3 {
		t.Fatalf("identity after plain truncate: %v", res.Rows[0][0])
	}

	// RESTART IDENTITY resets the counter.
	exec(t, d, `truncate table a restart identity`)
	exec(t, d, `insert into a (v) values ('w')`)
	if res := exec(t, d, `select id from a`); res.Rows[0][0].(int64) != 1 {
		t.Fatalf("identity after restart: %v", res.Rows[0][0])
	}

	// CASCADE is refused; the system catalog is protected.
	if _, err := d.Exec(`truncate a cascade`); err == nil {
		t.Fatal("TRUNCATE CASCADE accepted")
	}
	if _, err := d.Exec(`truncate pg_catalog.pg_class`); err == nil ||
		!strings.Contains(err.Error(), "read-only") {
		t.Fatalf("truncate catalog: %v", err)
	}
	if _, err := d.Exec(`truncate nope`); err == nil {
		t.Fatal("truncate of a missing table succeeded")
	}
}

// TestTruncateInBlock: TRUNCATE is DML here as in Postgres — it runs
// inside a transaction block and rolls back with it.
func TestTruncateInBlock(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table tt (id int primary key)`)
	exec(t, d, `insert into tt values (1), (2)`)
	s := d.NewSession()
	mustSess := func(q string) {
		t.Helper()
		if _, err := s.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustSess(`begin`)
	mustSess(`truncate tt`)
	res, err := s.Exec(`select count(*) from tt`)
	if err != nil || res.Rows[0][0].(int64) != 0 {
		t.Fatalf("inside block after truncate: %v %v", err, res)
	}
	mustSess(`rollback`)
	if res := exec(t, d, `select count(*) from tt`); res.Rows[0][0].(int64) != 2 {
		t.Fatalf("rows after rolled-back truncate: %v", res.Rows[0][0])
	}
}

func TestVarcharLimit(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table v (id int primary key, s varchar(5), u character varying(3))`)

	// Within the limit; multi-byte text counts characters, not bytes.
	exec(t, d, `insert into v values (1, 'héllo', 'äöü')`)

	// Overflow errors with Postgres's wording...
	if _, err := d.Exec(`insert into v values (2, 'toolong', 'ab')`); err == nil ||
		!strings.Contains(err.Error(), "value too long for type character varying(5)") {
		t.Fatalf("overflow: %v", err)
	}
	// ...on UPDATE too...
	if _, err := d.Exec(`update v set u = 'wxyz' where id = 1`); err == nil ||
		!strings.Contains(err.Error(), "character varying(3)") {
		t.Fatalf("update overflow: %v", err)
	}
	// ...but all-space overflow silently truncates, per the standard.
	exec(t, d, `insert into v values (3, 'abc   ', 'ab')`)
	res := exec(t, d, `select s from v where id = 3`)
	if got := res.Rows[0][0].(string); got != "abc  " {
		t.Fatalf("space overflow stored %q", got)
	}

	// The limit surfaces in information_schema.
	res = exec(t, d, `select character_maximum_length from information_schema.columns
		where table_name = 'v' and column_name = 's'`)
	if got := res.Rows[0][0].(int64); got != 5 {
		t.Fatalf("character_maximum_length = %v", got)
	}

	// varchar(0) is rejected at parse; char(n) points at varchar.
	if _, err := d.Exec(`create table w (a varchar(0) primary key)`); err == nil {
		t.Fatal("varchar(0) accepted")
	}
	if _, err := d.Exec(`create table w (a char(3) primary key)`); err == nil ||
		!strings.Contains(err.Error(), "varchar") {
		t.Fatalf("char(n): %v", err)
	}
}

func TestUniqueConstraintSugar(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table uu (
		id int primary key,
		email text unique,
		a int, b int,
		unique (a, b)
	)`)
	exec(t, d, `insert into uu values (1, 'x@y', 1, 1)`)

	if _, err := d.Exec(`insert into uu values (2, 'x@y', 2, 2)`); err == nil ||
		!strings.Contains(err.Error(), "unique") {
		t.Fatalf("column unique: %v", err)
	}
	if _, err := d.Exec(`insert into uu values (2, 'z@y', 1, 1)`); err == nil ||
		!strings.Contains(err.Error(), "unique") {
		t.Fatalf("table unique: %v", err)
	}
	// NULLs never conflict, per SQL unique-index semantics.
	exec(t, d, `insert into uu values (3, null, 3, 3)`)
	exec(t, d, `insert into uu values (4, null, 4, 4)`)

	// The sugar is a real index with the conventional name: usable as
	// an ON CONFLICT target and droppable.
	exec(t, d, `insert into uu values (1, 'x@y', 9, 9)
		on conflict (id) do nothing`)
	exec(t, d, `drop index uu_email_key`)
	exec(t, d, `insert into uu values (5, 'x@y', 5, 5)`)

	// A bad unique column un-creates the whole table.
	if _, err := d.Exec(`create table bad (id int primary key, unique (nope))`); err == nil {
		t.Fatal("unique over a missing column accepted")
	}
	if _, err := d.Exec(`create table bad (id int primary key)`); err != nil {
		t.Fatalf("table half-created after failed unique: %v", err)
	}
}

func TestShow(t *testing.T) {
	d := openDB(t)
	res := exec(t, d, `show server_version`)
	if res.Cols[0] != "server_version" || res.Rows[0][0].(string) != ServerVersion {
		t.Fatalf("show server_version: %+v", res)
	}
	if res := exec(t, d, `show transaction isolation level`); res.Rows[0][0].(string) != "serializable" {
		t.Fatalf("isolation: %+v", res.Rows[0])
	}
	if _, err := d.Exec(`show no_such_thing`); err == nil ||
		!strings.Contains(err.Error(), "unrecognized configuration parameter") {
		t.Fatalf("unknown parameter: %v", err)
	}

	// A session's SET values overlay the defaults; SHOW ALL includes them.
	s := d.NewSession()
	if _, err := s.Exec(`set application_name = 'myapp'`); err != nil {
		t.Fatal(err)
	}
	res, err := s.Exec(`show application_name`)
	if err != nil || res.Rows[0][0].(string) != "myapp" {
		t.Fatalf("show after set: %v %+v", err, res)
	}
	if _, err := s.Exec(`set statement_timeout = '2s'`); err != nil {
		t.Fatal(err)
	}
	res, _ = s.Exec(`show statement_timeout`)
	if res.Rows[0][0].(string) != "2000ms" {
		t.Fatalf("statement_timeout: %+v", res.Rows[0])
	}
	res, err = s.Exec(`show all`)
	if err != nil || len(res.Cols) != 3 || len(res.Rows) < 10 {
		t.Fatalf("show all: %v %d rows", err, len(res.Rows))
	}
	found := false
	for _, r := range res.Rows {
		if r[0] == "application_name" && r[1] == "myapp" {
			found = true
		}
	}
	if !found {
		t.Fatal("show all missed a SET parameter")
	}
}
