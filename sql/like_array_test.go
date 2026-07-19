package sql

// Tests for the LIKE/ILIKE operator family and the text[] column type
// (canonical-array-literal string representation; see bytdb/types.go),
// including the array functions and the array_to_string + ILIKE shape
// text search rewrites use.

import (
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func seedLike(t *testing.T, d *DB) {
	t.Helper()
	exec(t, d, `create table docs (id int primary key, title text, body text)`)
	exec(t, d, `insert into docs values
		(1, 'Amazing Grace', 'a hymn'),
		(2, 'grace notes', 'music theory'),
		(3, 'Path_to/glory', '100% effort'),
		(4, null, 'untitled')`)
}

func TestLikeOperators(t *testing.T) {
	d := openDB(t)
	seedLike(t, d)

	// %: any run. LIKE is case-sensitive, ILIKE is not.
	res := exec(t, d, `select id from docs where title like '%grace%' order by id`)
	if len(res.Rows) != 1 || res.Rows[0][0].(int64) != 2 {
		t.Fatalf("LIKE: %v", res.Rows)
	}
	res = exec(t, d, `select id from docs where title ilike '%grace%' order by id`)
	if len(res.Rows) != 2 || res.Rows[0][0].(int64) != 1 || res.Rows[1][0].(int64) != 2 {
		t.Fatalf("ILIKE: %v", res.Rows)
	}

	// _: exactly one character; the pattern anchors at both ends.
	res = exec(t, d, `select id from docs where title like 'grace note_'`)
	if len(res.Rows) != 1 || res.Rows[0][0].(int64) != 2 {
		t.Fatalf("underscore: %v", res.Rows)
	}
	res = exec(t, d, `select id from docs where title like 'grace'`)
	if len(res.Rows) != 0 {
		t.Fatalf("anchoring: %v", res.Rows)
	}

	// Backslash escapes make % and _ literal; regex metacharacters in
	// the pattern are literal on their own ('/' and '.' need no care).
	res = exec(t, d, `select id from docs where title like 'Path\_to/glory'`)
	if len(res.Rows) != 1 || res.Rows[0][0].(int64) != 3 {
		t.Fatalf("escaped underscore: %v", res.Rows)
	}
	res = exec(t, d, `select id from docs where body like '100\% effort'`)
	if len(res.Rows) != 1 || res.Rows[0][0].(int64) != 3 {
		t.Fatalf("escaped percent: %v", res.Rows)
	}

	// NOT LIKE / NOT ILIKE: NULL title is unknown, so row 4 never
	// appears — three-valued logic, as Postgres has it.
	res = exec(t, d, `select id from docs where title not ilike '%grace%' order by id`)
	if len(res.Rows) != 1 || res.Rows[0][0].(int64) != 3 {
		t.Fatalf("NOT ILIKE: %v", res.Rows)
	}

	// The pattern can be a composed expression and a parameter — the
	// '%' || $1 || '%' contains-search idiom.
	res, err := d.Exec(`select id from docs where title ilike '%' || $1 || '%'`, "AMAZ")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0][0].(int64) != 1 {
		t.Fatalf("param pattern: %v", res.Rows)
	}

	// The default backslash ESCAPE clause is accepted; any other
	// escape character is rejected, not silently misinterpreted.
	exec(t, d, `select id from docs where title like '%\%%' escape '\'`)
	if msg := execErr(t, d, `select id from docs where title like '%x%' escape '#'`); !strings.Contains(msg, "backslash") {
		t.Fatalf("non-default escape error: %q", msg)
	}
	// A trailing escape character has nothing to escape (Postgres's
	// error, surfaced at evaluation).
	if msg := execErr(t, d, `select id from docs where title like 'abc\'`); !strings.Contains(msg, "escape character") {
		t.Fatalf("trailing escape error: %q", msg)
	}

	// Pattern ops don't coerce their pattern to the column type: LIKE
	// on a non-text column reports a type error, not a parse error.
	exec(t, d, `create table nums (id int primary key)`)
	exec(t, d, `insert into nums values (1)`)
	if msg := execErr(t, d, `select id from nums where id like '1%'`); !strings.Contains(msg, "text operands") {
		t.Fatalf("non-text operand error: %q", msg)
	}
}

func TestTextArrayColumns(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table sermons (
		id int primary key,
		title text,
		scripture_refs text[],
		categories varchar[]
	)`)

	// Literals canonicalize on write: spacing collapses, quoting is
	// re-derived, so equality comparisons hit regardless of spelling.
	exec(t, d, `insert into sermons values
		(1, 'On Grace', '{ "John 3:16" , "Rom 5"}', '{grace,love}'),
		(2, 'On Hope', '{"Heb 11:1"}', '{hope}'),
		(3, 'Untagged', '{}', null)`)

	res := exec(t, d, `select scripture_refs, categories from sermons where id = 1`)
	if got := res.Rows[0][0].(string); got != `{"John 3:16","Rom 5"}` {
		t.Fatalf("canonical refs = %q", got)
	}
	if got := res.Rows[0][1].(string); got != `{grace,love}` {
		t.Fatalf("canonical categories = %q", got)
	}
	if res.Types[0] != bytdb.TTextArray {
		t.Fatalf("type = %v", res.Types[0])
	}

	// Equality against a differently-spelled literal works because both
	// sides canonicalize.
	res = exec(t, d, `select id from sermons where categories = '{grace, love}'`)
	if len(res.Rows) != 1 || res.Rows[0][0].(int64) != 1 {
		t.Fatalf("array equality: %v", res.Rows)
	}

	// x = ANY(array_column): membership over the stored literal.
	res = exec(t, d, `select id from sermons where 'hope' = any(categories)`)
	if len(res.Rows) != 1 || res.Rows[0][0].(int64) != 2 {
		t.Fatalf("ANY over column: %v", res.Rows)
	}

	// array_to_string joins; 2-arg skips NULL elements, 3-arg renders
	// them. array_length of an empty array is NULL, as in Postgres.
	res = exec(t, d, `select array_to_string(scripture_refs, ', ') from sermons where id = 1`)
	if got := res.Rows[0][0].(string); got != "John 3:16, Rom 5" {
		t.Fatalf("array_to_string = %q", got)
	}
	res = exec(t, d, `select array_to_string('{a,NULL,c}', '-'), array_to_string('{a,NULL,c}', '-', '?')`)
	if res.Rows[0][0].(string) != "a-c" || res.Rows[0][1].(string) != "a-?-c" {
		t.Fatalf("null handling: %v", res.Rows[0])
	}
	res = exec(t, d, `select array_length(scripture_refs, 1) from sermons order by id`)
	if res.Rows[0][0].(int64) != 2 || res.Rows[1][0].(int64) != 1 || res.Rows[2][0] != nil {
		t.Fatalf("array_length: %v", res.Rows)
	}

	// The sermon-search shape: ILIKE over the joined elements.
	res = exec(t, d, `select id from sermons where array_to_string(scripture_refs, ' ') ilike '%rom%'`)
	if len(res.Rows) != 1 || res.Rows[0][0].(int64) != 1 {
		t.Fatalf("array search: %v", res.Rows)
	}

	// ARRAY[...] constructor and ::text[] cast produce canonical
	// literals (note the quoting of the spaced element).
	res = exec(t, d, `select array['a b', 'c'], '{x, "y z"}'::text[]`)
	if res.Rows[0][0].(string) != `{"a b",c}` || res.Rows[0][1].(string) != `{x,"y z"}` {
		t.Fatalf("constructor/cast: %v", res.Rows[0])
	}

	// UPDATE writes canonicalize too.
	exec(t, d, `update sermons set categories = '{ mercy }' where id = 3`)
	res = exec(t, d, `select categories from sermons where id = 3`)
	if res.Rows[0][0].(string) != `{mercy}` {
		t.Fatalf("update canonicalization: %v", res.Rows[0][0])
	}

	// Malformed literals and unsupported shapes error clearly.
	if msg := execErr(t, d, `insert into sermons values (9, 'x', '{a', null)`); !strings.Contains(msg, "malformed array literal") {
		t.Fatalf("malformed literal error: %q", msg)
	}
	if msg := execErr(t, d, `insert into sermons values (9, 'x', '{{a},{b}}', null)`); !strings.Contains(msg, "multidimensional") {
		t.Fatalf("multidim error: %q", msg)
	}
	if msg := execErr(t, d, `create table bad (id int primary key, ns int[])`); !strings.Contains(msg, "only text[] arrays") {
		t.Fatalf("int[] error: %q", msg)
	}
	if msg := execErr(t, d, `create table bad (id int primary key, ns varchar(5)[])`); !strings.Contains(msg, "varchar(n)[]") {
		t.Fatalf("varchar(n)[] error: %q", msg)
	}
}
