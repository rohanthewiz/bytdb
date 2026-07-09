package sql

// default_test.go: constant DEFAULT column values — declaration,
// application on column-list inserts, the DEFAULT keyword in VALUES,
// DEFAULT VALUES, catalog reporting, and the constant-only rejections.

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func TestDefaultValues(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (
		id int primary key,
		n int default 0,
		f float default 1.5,
		s text default 'none',
		b bool default true,
		free text)`)

	// A column-list insert fills omitted columns with their defaults;
	// a defaultless column still inserts NULL.
	exec(t, d, `insert into t (id) values (1)`)
	res := exec(t, d, `select * from t`)
	want := [][]any{{int64(1), int64(0), 1.5, "none", true, nil}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("defaults: got %v, want %v", res.Rows, want)
	}

	// Explicit values — including explicit NULL — win over defaults.
	exec(t, d, `insert into t (id, n, s) values (2, 7, null)`)
	res = exec(t, d, `select n, s from t where id = 2`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(7), nil}}) {
		t.Fatalf("explicit over default: %v", res.Rows)
	}

	// The DEFAULT keyword slots the default into a full-arity VALUES
	// row; on a defaultless column it means NULL.
	exec(t, d, `insert into t values (3, default, default, default, default, default)`)
	res = exec(t, d, `select n, f, s, b, free from t where id = 3`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(0), 1.5, "none", true, nil}}) {
		t.Fatalf("DEFAULT keyword: %v", res.Rows)
	}

	// A quoted literal adapts to the column type at declaration.
	exec(t, d, `create table q (id int primary key, n int default '5')`)
	exec(t, d, `insert into q (id) values (1)`)
	res = exec(t, d, `select n from q`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(5)}}) {
		t.Fatalf("adapted literal default: %v", res.Rows)
	}
}

func TestDefaultValuesStatement(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id serial primary key, v text default 'x', n int)`)

	// INSERT ... DEFAULT VALUES: identity draws, defaults fill, the
	// rest are NULL — and RETURNING sees the final row.
	res := exec(t, d, `insert into t default values returning id, v, n`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1), "x", nil}}) {
		t.Fatalf("default values: %v", res.Rows)
	}

	// A NOT NULL column without a default rejects DEFAULT VALUES (the
	// identity PK draws, so the not-null column is what trips).
	exec(t, d, `create table strict (id serial primary key, v text not null)`)
	if _, err := d.Exec(`insert into strict default values`); err == nil ||
		!strings.Contains(err.Error(), "not-null") {
		t.Fatalf("strict default values: %v", err)
	}
}

func TestDefaultInteractions(t *testing.T) {
	d := openDB(t)

	// Defaults participate in CHECK evaluation and upsert probing.
	exec(t, d, `create table c (id int primary key, n int default -1 check (n < 100))`)
	exec(t, d, `insert into c (id) values (1)`) // default -1 passes the check
	exec(t, d, `create table u (k text primary key, v int default 10)`)
	exec(t, d, `insert into u (k) values ('a')`)
	res := exec(t, d, `insert into u (k) values ('a')
		on conflict (k) do update set v = u.v + excluded.v returning v`)
	// excluded.v carries the default (10), the existing row has 10.
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(20)}}) {
		t.Fatalf("default in excluded: %v", res.Rows)
	}

	// information_schema reports the stored literal.
	res = exec(t, d, `select column_default from information_schema.columns
		where table_name = 'u' and column_name = 'v'`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"10"}}) {
		t.Fatalf("column_default: %v", res.Rows)
	}

	// ADD COLUMN with DEFAULT works on an empty table only (no
	// backfill story), same rule as NOT NULL.
	exec(t, d, `create table a (id int primary key)`)
	exec(t, d, `alter table a add column v int default 3`)
	exec(t, d, `insert into a (id) values (1)`)
	res = exec(t, d, `select v from a`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(3)}}) {
		t.Fatalf("added column default: %v", res.Rows)
	}
	if _, err := d.Exec(`alter table a add column w int default 9`); err == nil ||
		!strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("add default to non-empty: %v", err)
	}
}

func TestDefaultRejections(t *testing.T) {
	d := openDB(t)
	for _, tc := range []struct{ q, want string }{
		{`create table t (id int primary key, ts int default now())`,
			"no date/time types"},
		{`create table t (id int primary key, ts int default current_timestamp)`,
			"no date/time types"},
		{`create table t (id int primary key, n int default 1 + 2)`,
			""}, // trailing tokens: a parse error is enough
		{`create table t (id int primary key, n int default upper('x'))`,
			"DEFAULT must be a constant"},
		{`create table t (id int primary key, n int default $1)`,
			"placeholders are not allowed in DEFAULT"},
		{`create table t (id serial default 5 primary key)`,
			"conflicting DEFAULT for identity column"},
		{`create table t (id int default 5 generated by default as identity primary key)`,
			"conflicting DEFAULT for identity column"},
		{`create table t (id int primary key, n int default 'abc')`,
			"invalid input syntax"},
	} {
		if _, err := d.Exec(tc.q); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: got %v, want %q", tc.q, err, tc.want)
		}
	}

	// DEFAULT NULL is legal and means "no default".
	exec(t, d, `create table ok (id int primary key, v text default null)`)
	res := exec(t, d, `select column_default from information_schema.columns
		where table_name = 'ok' and column_name = 'v'`)
	if !reflect.DeepEqual(res.Rows, [][]any{{nil}}) {
		t.Fatalf("DEFAULT NULL: %v", res.Rows)
	}
}

func TestDefaultSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := bytdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d := New(e)
	exec(t, d, `create table t (id int primary key, v text default 'd''quote')`)
	exec(t, d, `insert into t (id) values (1)`)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	// The default round-trips through the stored descriptor — quote
	// escaping included — across a reopen.
	if e, err = bytdb.Open(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	d = New(e)
	exec(t, d, `insert into t (id) values (2)`)
	res := exec(t, d, `select v from t order by id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"d'quote"}, {"d'quote"}}) {
		t.Fatalf("after reopen: %v", res.Rows)
	}
}
