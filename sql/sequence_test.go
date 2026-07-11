package sql

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// Standalone sequences: CREATE/DROP/ALTER SEQUENCE plus nextval,
// setval, and the session readbacks. nextval inside a SELECT runs the
// statement in a writable transaction under the covers, so plain
// `SELECT nextval('s')` works as it does in Postgres.

func TestSequenceBasics(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create sequence s`)

	for want := int64(1); want <= 3; want++ {
		res := exec(t, d, `select nextval('s')`)
		if !reflect.DeepEqual(res.Rows, [][]any{{want}}) {
			t.Fatalf("nextval draw %d: %v", want, res.Rows)
		}
	}

	// Session readbacks track the draws; setval repositions both the
	// sequence and currval.
	if res := exec(t, d, `select currval('s'), lastval()`); !reflect.DeepEqual(res.Rows, [][]any{{int64(3), int64(3)}}) {
		t.Fatalf("currval/lastval: %v", res.Rows)
	}
	if res := exec(t, d, `select setval('s', 42)`); !reflect.DeepEqual(res.Rows, [][]any{{int64(42)}}) {
		t.Fatalf("setval: %v", res.Rows)
	}
	if res := exec(t, d, `select nextval('s')`); !reflect.DeepEqual(res.Rows, [][]any{{int64(43)}}) {
		t.Fatalf("nextval after setval: %v", res.Rows)
	}
	// 3-arg setval with is_called=false: the value comes back itself.
	exec(t, d, `select setval('s', 100, false)`)
	if res := exec(t, d, `select nextval('s')`); !reflect.DeepEqual(res.Rows, [][]any{{int64(100)}}) {
		t.Fatalf("nextval after setval not-called: %v", res.Rows)
	}

	// The regclass spelling drivers use resolves through the catalog.
	if res := exec(t, d, `select nextval('s'::regclass)`); !reflect.DeepEqual(res.Rows, [][]any{{int64(101)}}) {
		t.Fatalf("nextval regclass: %v", res.Rows)
	}

	// A sequence reads as a one-row state relation, as in Postgres.
	res := exec(t, d, `select last_value, is_called from s`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(101), true}}) {
		t.Fatalf("state relation: %v", res.Rows)
	}

	// The draw-then-insert allocation pattern drivers use still works
	// (nextval directly in VALUES is covered in insert_expr_test.go).
	exec(t, d, `create table seqt (id int primary key, v text)`)
	id := exec(t, d, `select nextval('s')`).Rows[0][0].(int64)
	if _, err := d.Exec(`insert into seqt values ($1, 'a')`, id); err != nil {
		t.Fatalf("insert drawn key: %v", err)
	}
	res = exec(t, d, `select id from seqt`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(102)}}) {
		t.Fatalf("drawn key: %v", res.Rows)
	}
}

func TestSequenceOptions(t *testing.T) {
	d := openDB(t)

	// START/INCREMENT/MAXVALUE without CYCLE: exhaustion errors with
	// Postgres' wording.
	exec(t, d, `create sequence bounded start with 10 increment by 5 maxvalue 20`)
	res := exec(t, d, `select nextval('bounded'), nextval('bounded'), nextval('bounded')`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(10), int64(15), int64(20)}}) {
		t.Fatalf("bounded draws: %v", res.Rows)
	}
	if _, err := d.Exec(`select nextval('bounded')`); err == nil ||
		!strings.Contains(err.Error(), `reached maximum value of sequence "bounded" (20)`) {
		t.Fatalf("exhaustion: %v", err)
	}

	// CYCLE wraps to MINVALUE.
	exec(t, d, `create sequence wheel minvalue 1 maxvalue 3 cycle`)
	res = exec(t, d, `select nextval('wheel'), nextval('wheel'), nextval('wheel'), nextval('wheel')`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1), int64(2), int64(3), int64(1)}}) {
		t.Fatalf("cycle draws: %v", res.Rows)
	}

	// Descending defaults: start at -1, walk down.
	exec(t, d, `create sequence down increment by -1`)
	res = exec(t, d, `select nextval('down'), nextval('down')`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(-1), int64(-2)}}) {
		t.Fatalf("descending draws: %v", res.Rows)
	}

	// The declared type bounds the declarable range.
	exec(t, d, `create sequence small as smallint`)
	if _, err := d.Exec(`create sequence toobig as smallint maxvalue 40000`); err == nil ||
		!strings.Contains(err.Error(), "out of range for sequence data type smallint") {
		t.Fatalf("type range: %v", err)
	}

	// Option validation carries Postgres' wording.
	for _, c := range []struct{ q, want string }{
		{`create sequence z increment by 0`, "INCREMENT must not be zero"},
		{`create sequence z minvalue 10 maxvalue 5`, "MINVALUE (10) must be less than MAXVALUE (5)"},
		{`create sequence z start with 0`, "START value (0) cannot be less than MINVALUE (1)"},
		{`create sequence z cache 0`, "CACHE (0) must be greater than zero"},
		{`select setval('wheel', 9)`, `setval: value 9 is out of bounds for sequence "wheel" (1..3)`},
		{`select nextval('ghost')`, `relation "ghost" does not exist`},
		{`drop sequence ghost`, `sequence "ghost" does not exist`},
	} {
		if _, err := d.Exec(c.q); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want containing %q", c.q, err, c.want)
		}
	}
}

func TestSequenceDDL(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create sequence s`)

	// IF NOT EXISTS / IF EXISTS report notices instead of errors.
	if res := exec(t, d, `create sequence if not exists s`); !strings.Contains(res.Notice, "already exists, skipping") {
		t.Fatalf("if not exists notice: %q", res.Notice)
	}
	if _, err := d.Exec(`create sequence s`); err == nil ||
		!strings.Contains(err.Error(), `relation "s" already exists`) {
		t.Fatalf("duplicate: %v", err)
	}

	// Sequences and tables share the relation namespace.
	if _, err := d.Exec(`create table s (id int primary key)`); err == nil ||
		!strings.Contains(err.Error(), `already exists`) {
		t.Fatalf("table over sequence: %v", err)
	}
	exec(t, d, `create table t (id int primary key)`)
	if _, err := d.Exec(`create sequence t`); err == nil ||
		!strings.Contains(err.Error(), `relation "t" already exists`) {
		t.Fatalf("sequence over table: %v", err)
	}

	// ALTER: restart, retarget, and the option validations.
	exec(t, d, `select nextval('s'), nextval('s')`)
	exec(t, d, `alter sequence s restart`)
	if res := exec(t, d, `select nextval('s')`); !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}}) {
		t.Fatalf("after restart: %v", res.Rows)
	}
	exec(t, d, `alter sequence s restart with 50`)
	if res := exec(t, d, `select nextval('s')`); !reflect.DeepEqual(res.Rows, [][]any{{int64(50)}}) {
		t.Fatalf("after restart with: %v", res.Rows)
	}
	exec(t, d, `alter sequence s increment by 10`)
	if res := exec(t, d, `select nextval('s')`); !reflect.DeepEqual(res.Rows, [][]any{{int64(60)}}) {
		t.Fatalf("after increment change: %v", res.Rows)
	}
	if _, err := d.Exec(`alter sequence s`); err == nil ||
		!strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("alter without options: %v", err)
	}
	if _, err := d.Exec(`alter sequence ghost restart`); err == nil ||
		!strings.Contains(err.Error(), `sequence "ghost" does not exist`) {
		t.Fatalf("alter missing: %v", err)
	}

	// DROP.
	exec(t, d, `drop sequence s`)
	if res := exec(t, d, `drop sequence if exists s`); !strings.Contains(res.Notice, "does not exist, skipping") {
		t.Fatalf("drop if exists notice: %q", res.Notice)
	}
	if _, err := d.Exec(`select nextval('s')`); err == nil {
		t.Fatal("nextval after drop should fail")
	}
}

func TestSequenceCatalog(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create sequence s start with 5 increment by 2 minvalue 5 maxvalue 99 cycle`)

	// pg_class lists it as relkind 'S'.
	res := exec(t, d, `select relname, relkind from pg_class where relkind = 'S'`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"s", "S"}}) {
		t.Fatalf("pg_class: %v", res.Rows)
	}

	// pg_sequence carries the options, joined by relid.
	res = exec(t, d, `select s.seqstart, s.seqincrement, s.seqmin, s.seqmax, s.seqcycle, s.seqtypid
		from pg_sequence s join pg_class c on c.oid = s.seqrelid where c.relname = 's'`)
	want := [][]any{{int64(5), int64(2), int64(5), int64(99), true, int64(20)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("pg_sequence: %v", res.Rows)
	}

	// information_schema.sequences renders the standard shape (numeric
	// fields as text).
	res = exec(t, d, `select sequence_name, data_type, start_value, increment, cycle_option
		from information_schema.sequences`)
	want = [][]any{{"s", "bigint", "5", "2", "YES"}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("information_schema.sequences: %v", res.Rows)
	}

	// 'name'::regclass resolves sequences too.
	oid := exec(t, d, `select 's'::regclass`).Rows[0][0]
	res = exec(t, d, `select relname from pg_class where oid = 's'::regclass`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"s"}}) {
		t.Fatalf("regclass %v: %v", oid, res.Rows)
	}
}

func TestSequenceSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	e, err := bytdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d := New(e)
	exec(t, d, `create sequence s increment by 3`)
	exec(t, d, `select nextval('s')`) // 1
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	if e, err = bytdb.Open(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	d = New(e)
	// Options and position both survive: the next step is 1+3.
	if res := exec(t, d, `select nextval('s')`); !reflect.DeepEqual(res.Rows, [][]any{{int64(4)}}) {
		t.Fatalf("after reopen: %v", res.Rows)
	}
}
