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

// TestIndexIfExistsDDL exercises CREATE [UNIQUE] INDEX IF NOT EXISTS
// and DROP INDEX IF EXISTS: the guarded forms turn the name collision
// (or absence) into a notice, the plain forms keep erroring, and the
// existing index is never disturbed by a skipped create.
func TestIndexIfExistsDDL(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, a int, b int)`)
	exec(t, d, `create index ix on t (a)`)

	// IF NOT EXISTS on a taken name: notice, no error — even when the
	// requested definition differs (columns, uniqueness), because the
	// clause checks the name only.
	res := exec(t, d, `create index if not exists ix on t (b)`)
	if !strings.Contains(res.Notice, `relation "ix" already exists, skipping`) {
		t.Fatalf("create notice: %q", res.Notice)
	}
	res = exec(t, d, `create unique index if not exists ix on t (b)`)
	if !strings.Contains(res.Notice, `already exists, skipping`) {
		t.Fatalf("unique create notice: %q", res.Notice)
	}

	// The unguarded duplicate still errors.
	if _, err := d.Exec(`create index ix on t (b)`); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate create: %v", err)
	}

	// IF NOT EXISTS on a free name creates normally (no notice), and
	// the resulting index is real — a unique one enforces uniqueness.
	if res := exec(t, d, `create unique index if not exists ub on t (b)`); res.Notice != "" {
		t.Fatalf("fresh create: unexpected notice %q", res.Notice)
	}
	exec(t, d, `insert into t values (1, 10, 100)`)
	if _, err := d.Exec(`insert into t values (2, 20, 100)`); err == nil {
		t.Fatal("unique index from guarded create should reject the duplicate")
	}

	// The name check is per table: bytdb scopes index names to their
	// table, so the same name on another table creates normally.
	exec(t, d, `create table s (id int primary key, a int)`)
	if res := exec(t, d, `create index if not exists ix on s (a)`); res.Notice != "" {
		t.Fatalf("same name, other table: unexpected notice %q", res.Notice)
	}

	// DROP INDEX IF EXISTS: notice when the index (or, with ON, the
	// table) is missing; a real drop when it is there. "ix" is now
	// ambiguous across t and s, which IF EXISTS does not paper over.
	res = exec(t, d, `drop index if exists ghost on t`)
	if !strings.Contains(res.Notice, `index "ghost" does not exist, skipping`) {
		t.Fatalf("drop notice: %q", res.Notice)
	}
	if res := exec(t, d, `drop index if exists ghost`); !strings.Contains(res.Notice, "skipping") {
		t.Fatalf("drop notice (no ON): %q", res.Notice)
	}
	if res := exec(t, d, `drop index if exists nope on missing_table`); !strings.Contains(res.Notice, "skipping") {
		t.Fatalf("drop notice (missing table): %q", res.Notice)
	}
	if _, err := d.Exec(`drop index ghost on t`); err == nil {
		t.Fatal("unguarded drop of a missing index should error")
	}
	if _, err := d.Exec(`drop index if exists ix`); err == nil ||
		!strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous name should still error under IF EXISTS: %v", err)
	}
	if res := exec(t, d, `drop index if exists ix on s`); res.Notice != "" {
		t.Fatalf("drop existing: unexpected notice %q", res.Notice)
	}
	if res := exec(t, d, `drop index if exists ix on s`); !strings.Contains(res.Notice, "skipping") {
		t.Fatalf("second drop should notice: %q", res.Notice)
	}
}
