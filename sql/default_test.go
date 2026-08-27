package sql

// default_test.go: DEFAULT column values — constant declaration,
// application on column-list inserts, the DEFAULT keyword in VALUES,
// DEFAULT VALUES, the evaluated clock defaults (now(), current_date),
// catalog reporting, and the expression rejections.

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

	// ADD COLUMN with DEFAULT on an empty table: later inserts get it.
	exec(t, d, `create table a (id int primary key)`)
	exec(t, d, `alter table a add column v int default 3`)
	exec(t, d, `insert into a (id) values (1)`)
	res = exec(t, d, `select v from a`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(3)}}) {
		t.Fatalf("added column default: %v", res.Rows)
	}
	// ...and on a non-empty table the existing rows are backfilled, so
	// the column reads the default everywhere, as in Postgres.
	exec(t, d, `alter table a add column w int default 9`)
	res = exec(t, d, `select v, w from a`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(3), int64(9)}}) {
		t.Fatalf("backfilled column default: %v", res.Rows)
	}
}

func TestDefaultRejections(t *testing.T) {
	d := openDB(t)
	for _, tc := range []struct{ q, want string }{
		{`create table t (id int primary key, ts int default now())`,
			"requires a timestamp or date column"},
		{`create table t (id int primary key, s text default current_timestamp)`,
			"requires a timestamp or date column"},
		{`create table t (id int primary key, ts timestamp default current_time)`,
			"no time-of-day type"},
		{`create table t (id int primary key, ts timestamp default now)`,
			""}, // now without parens is a column reference: parse error
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

func TestDefaultNow(t *testing.T) {
	d := openDB(t)
	// Every timestamp spelling normalizes to now(); current_date is
	// its own marker. Both work on timestamp and date columns.
	exec(t, d, `create table t (
		id int primary key,
		created timestamptz default now(),
		stamped timestamp default current_timestamp,
		local timestamp default localtimestamp,
		txts timestamp default transaction_timestamp(),
		d date default current_date,
		dts timestamp default current_date,
		dnow date default now())`)

	before := time.Now().Add(-time.Minute).UnixMicro()
	res := exec(t, d, `insert into t (id) values (1), (2)
		returning created, stamped, local, txts, d, dts, dnow`)
	after := time.Now().Add(time.Minute).UnixMicro()

	for i, row := range res.Rows {
		for j := range 4 { // the four timestamp-instant columns
			ts, ok := row[j].(int64)
			if !ok || ts < before || ts > after {
				t.Fatalf("row %d col %d: %v not a current timestamp", i, j, row[j])
			}
		}
		today := time.Now().UTC().Unix() / 86400
		if days := row[4].(int64); days < today-1 || days > today+1 {
			t.Fatalf("current_date: %v, today is day %d", row[4], today)
		}
		// current_date on a timestamp column lands as midnight UTC.
		if micros := row[5].(int64); micros%(86400*1e6) != 0 {
			t.Fatalf("current_date on timestamp not midnight: %v", row[5])
		}
		// now() on a date column truncates to days.
		if days := row[6].(int64); days < today-1 || days > today+1 {
			t.Fatalf("now() on date: %v", row[6])
		}
	}

	// One statement, one instant: both rows share every clock value.
	if !reflect.DeepEqual(res.Rows[0], res.Rows[1]) {
		t.Fatalf("rows of one INSERT differ: %v vs %v", res.Rows[0], res.Rows[1])
	}

	// The catalog reports the normalized marker text, unquoted.
	res = exec(t, d, `select column_name, column_default from information_schema.columns
		where table_name = 't' and column_name in ('created', 'stamped', 'd')
		order by column_name`)
	want := [][]any{{"created", "now()"}, {"d", "current_date"}, {"stamped", "now()"}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("column_default: %v", res.Rows)
	}

	// A string default that spells now() stays a constant — the quoted
	// descriptor text never collides with the marker.
	exec(t, d, `create table s (id int primary key, v text default 'now()')`)
	exec(t, d, `insert into s (id) values (1)`)
	res = exec(t, d, `select v from s`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"now()"}}) {
		t.Fatalf("literal 'now()': %v", res.Rows)
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

// TestAlterColumnDefault covers ALTER TABLE ... ALTER COLUMN
// SET/DROP DEFAULT: a descriptor-only change that steers later
// inserts and leaves stored rows alone.
func TestAlterColumnDefault(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, n int, ts timestamp)`)
	exec(t, d, `insert into t (id, n) values (1, 5)`)

	exec(t, d, `alter table t alter column n set default 42`)
	exec(t, d, `insert into t (id) values (2)`)
	res := exec(t, d, `select id, n from t order by id`)
	// Row 1 keeps its value: SET DEFAULT never rewrites rows.
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1), int64(5)}, {int64(2), int64(42)}}) {
		t.Fatalf("after set default: %v", res.Rows)
	}
	// information_schema reports the new literal.
	res = exec(t, d, `select column_default from information_schema.columns
		where table_name = 't' and column_name = 'n'`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"42"}}) {
		t.Fatalf("column_default: %v", res.Rows)
	}

	// The optional COLUMN keyword, and the clock markers.
	exec(t, d, `alter table t alter ts set default now()`)
	exec(t, d, `insert into t (id) values (3)`)
	res = exec(t, d, `select count(*) from t where ts is not null`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}}) {
		t.Fatalf("now() default: %v", res.Rows)
	}

	// DROP DEFAULT, and SET DEFAULT NULL as its synonym.
	exec(t, d, `alter table t alter column n drop default`)
	exec(t, d, `insert into t (id) values (4)`)
	res = exec(t, d, `select n from t where id = 4`)
	if !reflect.DeepEqual(res.Rows, [][]any{{nil}}) {
		t.Fatalf("after drop default: %v", res.Rows)
	}
	exec(t, d, `alter table t alter column ts set default null`)
	res = exec(t, d, `select column_default from information_schema.columns
		where table_name = 't' and column_name = 'ts'`)
	if !reflect.DeepEqual(res.Rows, [][]any{{nil}}) {
		t.Fatalf("column_default after set default null: %v", res.Rows)
	}
	// DROP DEFAULT on a column that has none is a no-op, not an error.
	exec(t, d, `alter table t alter column ts drop default`)

	for _, tc := range []struct{ q, want string }{
		{`alter table t alter column n set default 'abc'`, "invalid input syntax"},
		{`alter table t alter column n set default now()`, "requires a timestamp or date column"},
		{`alter table t alter column nope set default 1`, "does not exist"},
		{`alter table ghosts alter column n set default 1`, "no such table"},
		{`alter table t alter column n set not null`, "only SET DEFAULT is supported"},
		{`alter table t alter column n drop not null`, "only DROP DEFAULT is supported"},
		{`alter table t alter column n type text`, ""}, // parse error is enough
	} {
		err := execErr(t, d, tc.q)
		if tc.want != "" && !strings.Contains(err, tc.want) {
			t.Fatalf("%s: %v (want %q)", tc.q, err, tc.want)
		}
	}
}
