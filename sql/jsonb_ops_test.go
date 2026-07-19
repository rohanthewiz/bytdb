package sql

// The jsonb operator family: accessors (-> ->> #> #>>), predicates
// (@> <@ ? ?| ?&), concatenation (||) and deletion (-), plus the
// static-type dispatch that keeps text || and numeric - working.

import (
	"reflect"
	"strings"
	"testing"
)

func seedJSONB(t *testing.T, d *DB) {
	t.Helper()
	exec(t, d, `create table docs (id int primary key, body jsonb)`)
	exec(t, d, `insert into docs values
		(1, '{"name": "ana", "n": 1.0, "tags": ["go", "db"],
		      "addr": {"city": "Austin", "geo": {"lat": 30}}}'),
		(2, '{"name": "bo", "tags": []}'),
		(3, '[1, 2, [1, 3]]')`)
}

// one runs a query expected to yield a single row and column.
func one(t *testing.T, d *DB, q string) any {
	t.Helper()
	res := exec(t, d, q)
	if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
		t.Fatalf("%s: want one value, got %v", q, res.Rows)
	}
	return res.Rows[0][0]
}

func TestJSONBAccessors(t *testing.T) {
	d := openDB(t)
	seedJSONB(t, d)

	cases := []struct {
		q    string
		want any // nil means SQL NULL
	}{
		// -> yields jsonb (canonical text, strings keep their quotes);
		// ->> yields text (strings unquoted).
		{`select body -> 'name' from docs where id = 1`, `"ana"`},
		{`select body ->> 'name' from docs where id = 1`, `ana`},
		// Chained accessors and array indexes, including negative
		// (from the end) and out-of-range (NULL).
		{`select body -> 'tags' -> 0 from docs where id = 1`, `"go"`},
		{`select body -> 'tags' ->> -1 from docs where id = 1`, `db`},
		{`select body -> 'tags' -> 7 from docs where id = 1`, nil},
		// A miss is NULL, and NULL-strictness carries it through chains.
		{`select body -> 'missing' from docs where id = 1`, nil},
		{`select body -> 'missing' ->> 'x' from docs where id = 1`, nil},
		// Kind mismatches miss instead of erroring: integer against an
		// object, text key against an array.
		{`select body -> 0 from docs where id = 1`, nil},
		{`select body -> 'x' from docs where id = 3`, nil},
		// ->> of a container is its canonical text; of JSON null, SQL NULL.
		{`select body ->> 'geo' -> 'x' from docs where id = 1`, nil},
		{`select body -> 'addr' ->> 'geo' from docs where id = 1`, `{"lat":30}`},
		{`select 'null'::jsonb ->> 0 from docs where id = 3`, nil},
		// #> / #>> walk text[] paths, mixing keys and array indexes.
		{`select body #> '{addr,geo,lat}' from docs where id = 1`, `30`},
		{`select body #>> '{addr,geo}' from docs where id = 1`, `{"lat":30}`},
		{`select body #>> '{tags,1}' from docs where id = 1`, `db`},
		{`select body #> '{addr,nope}' from docs where id = 1`, nil},
		{`select body #> '{tags,x}' from docs where id = 1`, nil},
	}
	for _, c := range cases {
		if got := one(t, d, c.q); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %#v, want %#v", c.q, got, c.want)
		}
	}

	// Accessors compose with the rest of the layer: WHERE and ORDER BY.
	res := exec(t, d, `select id from docs where body ->> 'name' = 'ana'`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}}) {
		t.Fatalf("WHERE on ->>: %v", res.Rows)
	}
	res = exec(t, d, `select id from docs where body ? 'name' order by body ->> 'name' desc`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(2)}, {int64(1)}}) {
		t.Fatalf("ORDER BY on ->>: %v", res.Rows)
	}
}

func TestJSONBPredicates(t *testing.T) {
	d := openDB(t)
	seedJSONB(t, d)

	cases := []struct {
		where string
		want  []int64
	}{
		// Containment recurses through object values...
		{`body @> '{"addr": {"city": "Austin"}}'`, []int64{1}},
		// ...compares numbers numerically (stored 1.0 contains 1)...
		{`body @> '{"n": 1}'`, []int64{1}},
		// ...matches array elements in any order, and never crosses
		// kinds: [1,2,[1,3]] contains [[1,3]] but not [1,3]'s elements
		// unwrapped past one level.
		{`body @> '[2, 1]'`, []int64{3}},
		{`body @> '[[1, 3]]'`, []int64{3}},
		{`body @> '[3]'`, nil},
		// The top-level scalar-in-array exception.
		{`body @> '2'`, []int64{3}},
		// <@ is the mirror.
		{`'{"tags": ["go", "db"], "name": "bo", "x": 1}' <@ body`, nil},
		{`body <@ '{"name": "bo", "tags": [], "extra": true}'`, []int64{2}},
		// ? on objects (key), arrays (string element), via accessors too.
		{`body ? 'addr'`, []int64{1}},
		{`body -> 'tags' ? 'go'`, []int64{1}},
		{`body -> 'name' ? 'bo'`, []int64{2}},
		// ?| and ?& over both spellings of a key list.
		{`body ?| '{zzz,addr}'`, []int64{1}},
		{`body ?| array['zzz', 'yyy']`, nil},
		{`body ?& '{name,tags}'`, []int64{1, 2}},
		{`body ?& array['name', 'addr']`, []int64{1}},
	}
	for _, c := range cases {
		res := exec(t, d, `select id from docs where `+c.where+` order by id`)
		var got []int64
		for _, r := range res.Rows {
			got = append(got, r[0].(int64))
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("WHERE %s: got %v, want %v", c.where, got, c.want)
		}
	}

	// A malformed containment operand is an input-syntax error, caught
	// by the static coercion pass before any row is read.
	if _, err := d.Exec(`select id from docs where body @> '{bad'`); err == nil ||
		!strings.Contains(err.Error(), "invalid input syntax for type json") {
		t.Fatalf("malformed @> operand: %v", err)
	}
	// A NULL operand leaves the predicate unknown: no row, no error.
	res := exec(t, d, `select id from docs where body @> null::jsonb`)
	if len(res.Rows) != 0 {
		t.Fatalf("NULL containment operand: %v", res.Rows)
	}
}

func TestJSONBConcatDelete(t *testing.T) {
	d := openDB(t)
	seedJSONB(t, d)

	cases := []struct {
		q    string
		want any
	}{
		// Object merge, right side winning collisions; result canonical.
		{`select body || '{"z": 9, "name": "ann"}' from docs where id = 2`,
			`{"name":"ann","tags":[],"z":9}`},
		// Array concat, and the wrap-as-array rules for mixed kinds.
		{`select body -> 'tags' || '["sql"]' from docs where id = 1`,
			`["go","db","sql"]`},
		{`select '{"a": 1}'::jsonb || '2' from docs where id = 1`,
			`[{"a":1},2]`},
		{`select '1'::jsonb || '[2]' from docs where id = 1`,
			`[1,2]`},
		// - text drops an object key (absent key: no-op) or every equal
		// string element of an array.
		{`select body - 'addr' - 'n' from docs where id = 1`,
			`{"name":"ana","tags":["go","db"]}`},
		{`select body - 'nope' from docs where id = 2`,
			`{"name":"bo","tags":[]}`},
		{`select body -> 'tags' - 'go' from docs where id = 1`,
			`["db"]`},
		// - integer drops by index, negative from the end, out of range
		// is a no-op.
		{`select body -> 'tags' - 0 from docs where id = 1`, `["db"]`},
		{`select body - -1 from docs where id = 3`, `[1,2]`},
		{`select body -> 'tags' - 5 from docs where id = 1`, `["go","db"]`},
		// The shared spellings keep their old meanings when nothing is
		// statically jsonb.
		{`select 'a' || 'b' from docs where id = 1`, `ab`},
		{`select 7 - 2 from docs where id = 1`, int64(5)},
	}
	for _, c := range cases {
		if got := one(t, d, c.q); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %#v, want %#v", c.q, got, c.want)
		}
	}

	// Deleting from the wrong container kind errors, as in Postgres.
	if _, err := d.Exec(`select '1'::jsonb - 'a' from docs`); err == nil ||
		!strings.Contains(err.Error(), "cannot delete from scalar") {
		t.Fatalf("delete from scalar: %v", err)
	}
	if _, err := d.Exec(`select '{"a": 1}'::jsonb - 1 from docs`); err == nil ||
		!strings.Contains(err.Error(), "cannot delete from object using integer index") {
		t.Fatalf("integer delete from object: %v", err)
	}

	// The read-modify-write shape the operators exist for: UPDATE
	// through || stores the merged, canonical document.
	exec(t, d, `update docs set body = body || '{"seen": true}' where id = 2`)
	got := one(t, d, `select body from docs where id = 2`)
	if got != `{"name":"bo","seen":true,"tags":[]}` {
		t.Fatalf("UPDATE via ||: %#v", got)
	}
}
