package sql

import (
	"reflect"
	"strings"
	"testing"
)

// TestTableIfExistsDDL exercises CREATE TABLE IF NOT EXISTS and
// DROP TABLE IF EXISTS end to end: the guarded forms turn the
// name-collision (or absence) error into a Postgres-style notice,
// while the plain forms keep erroring.
func TestTableIfExistsDDL(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, name text)`)
	exec(t, d, `insert into t values (1, 'ada')`)

	// IF NOT EXISTS on a taken name: notice, no error, and the
	// existing table survives untouched — the clause checks the name
	// only, never comparing the requested columns to the stored ones.
	res := exec(t, d, `create table if not exists t (other int primary key)`)
	if !strings.Contains(res.Notice, `relation "t" already exists, skipping`) {
		t.Fatalf("create notice: %q", res.Notice)
	}
	if res := exec(t, d, `select name from t where id = 1`); !reflect.DeepEqual(res.Rows, [][]any{{"ada"}}) {
		t.Fatalf("existing table disturbed: %v", res.Rows)
	}

	// The unguarded duplicate still errors.
	if _, err := d.Exec(`create table t (id int primary key)`); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate create: %v", err)
	}

	// IF NOT EXISTS on a free name creates normally (no notice).
	if res := exec(t, d, `create table if not exists u (id int primary key)`); res.Notice != "" {
		t.Fatalf("fresh create: unexpected notice %q", res.Notice)
	}
	exec(t, d, `insert into u values (1)`)

	// DROP TABLE IF EXISTS: notice when the table is missing, a real
	// drop when it is there.
	res = exec(t, d, `drop table if exists ghost`)
	if !strings.Contains(res.Notice, `table "ghost" does not exist, skipping`) {
		t.Fatalf("drop notice: %q", res.Notice)
	}
	if _, err := d.Exec(`drop table ghost`); err == nil {
		t.Fatal("unguarded drop of a missing table should error")
	}
	if res := exec(t, d, `drop table if exists u`); res.Notice != "" {
		t.Fatalf("drop existing: unexpected notice %q", res.Notice)
	}
	if _, err := d.Exec(`select * from u`); err == nil {
		t.Fatal("u should be gone after DROP TABLE IF EXISTS")
	}

	// A dropped name is free again for the guarded create.
	exec(t, d, `create table if not exists u (id int primary key, v text)`)
	exec(t, d, `insert into u values (2, 'x')`)
}
