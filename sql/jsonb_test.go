package sql

// jsonb at the SQL layer: DDL (including the json alias), literal
// canonicalization on writes and comparisons, the ::jsonb cast,
// constant DEFAULTs, malformed-input rejection, and what the catalog
// reports.

import (
	"reflect"
	"strings"
	"testing"
)

func TestJSONBColumns(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table docs (
		id int primary key,
		body jsonb,
		meta jsonb default '{"v": 1}')`)

	// Writes canonicalize: whitespace goes, keys sort, and the read
	// returns the one canonical spelling.
	exec(t, d, `insert into docs (id, body) values (1, '{"b": 2, "a": 1}')`)
	res := exec(t, d, `select body, meta from docs`)
	want := [][]any{{`{"a":1,"b":2}`, `{"v":1}`}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("canonicalized read: %v", res.Rows)
	}

	// Comparisons canonicalize the literal side too, so any spelling
	// of the same document matches.
	res = exec(t, d, `select id from docs where body = '{ "b":2, "a": 1 }'`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}}) {
		t.Fatalf("document equality: %v", res.Rows)
	}

	// The ::jsonb cast validates and canonicalizes an expression.
	res = exec(t, d, `select '{"z": 1, "a": {"y": 2, "x": 3}}'::jsonb`)
	if !reflect.DeepEqual(res.Rows, [][]any{{`{"a":{"x":3,"y":2},"z":1}`}}) {
		t.Fatalf("::jsonb cast: %v", res.Rows)
	}

	// Malformed JSON is refused on write and in casts.
	if _, err := d.Exec(`insert into docs (id, body) values (9, '{nope')`); err == nil ||
		!strings.Contains(err.Error(), "invalid input syntax for type json") {
		t.Fatalf("malformed insert: %v", err)
	}
	if _, err := d.Exec(`select '[1,'::jsonb`); err == nil {
		t.Fatal("malformed cast accepted")
	}

	// json is an alias for jsonb (documented: jsonb semantics).
	exec(t, d, `create table j (id int primary key, v json)`)
	exec(t, d, `insert into j values (1, '  [1, 2]  ')`)
	res = exec(t, d, `select v from j`)
	if !reflect.DeepEqual(res.Rows, [][]any{{`[1,2]`}}) {
		t.Fatalf("json alias: %v", res.Rows)
	}

	// jsonb[] does not exist.
	if _, err := d.Exec(`create table a (id int primary key, v jsonb[])`); err == nil {
		t.Fatal("jsonb[] accepted")
	}
}

func TestJSONBCatalog(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table docs (id int primary key, body jsonb)`)

	res := exec(t, d, `select data_type, udt_name from information_schema.columns
		where table_name = 'docs' and column_name = 'body'`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"jsonb", "jsonb"}}) {
		t.Fatalf("information_schema: %v", res.Rows)
	}

	res = exec(t, d, `select typname from pg_catalog.pg_type where oid = 3802`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"jsonb"}}) {
		t.Fatalf("pg_type: %v", res.Rows)
	}
}
