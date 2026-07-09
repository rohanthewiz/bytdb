package sql

// upsert_test.go: INSERT ... ON CONFLICT — DO NOTHING and DO UPDATE
// against the primary key and unique indexes, excluded references,
// the DO UPDATE WHERE filter, RETURNING over the mix, counts, and
// the rejections and cardinality errors Postgres raises.

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func TestUpsertDoNothing(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, v text)`)
	exec(t, d, `insert into t values (1, 'a')`)

	// With a target: the conflicting row is skipped, the fresh row
	// inserts, and only the insert counts.
	res := exec(t, d, `insert into t values (1, 'dup'), (2, 'b') on conflict (id) do nothing`)
	if res.RowsAffected != 1 {
		t.Fatalf("affected: got %d, want 1", res.RowsAffected)
	}
	res = exec(t, d, `select * from t order by id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1), "a"}, {int64(2), "b"}}) {
		t.Fatalf("rows: %v", res.Rows)
	}

	// Without a target, any uniqueness violation is absorbed — here a
	// unique index, not the PK.
	exec(t, d, `create unique index by_v on t (v)`)
	res = exec(t, d, `insert into t values (9, 'a') on conflict do nothing`)
	if res.RowsAffected != 0 {
		t.Fatalf("unique-index conflict: affected %d, want 0", res.RowsAffected)
	}

	// A conflict with a row inserted earlier in the same statement.
	res = exec(t, d, `insert into t values (5, 'e'), (5, 'e2') on conflict (id) do nothing`)
	if res.RowsAffected != 1 {
		t.Fatalf("self-conflict: affected %d, want 1", res.RowsAffected)
	}
}

func TestUpsertDoUpdate(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table kv (k text primary key, v int, hits int not null)`)

	// The idiomatic counter upsert: insert path first.
	q := `insert into kv values ($1, $2, 1)
		on conflict (k) do update set v = excluded.v, hits = kv.hits + 1
		returning k, v, hits`
	res, err := d.Exec(q, "a", int64(1))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Rows, [][]any{{"a", int64(1), int64(1)}}) || res.RowsAffected != 1 {
		t.Fatalf("insert path: %v affected %d", res.Rows, res.RowsAffected)
	}
	// Conflict path: excluded carries the proposed values, bare (and
	// qualified) names read the existing row.
	res, err = d.Exec(q, "a", int64(42))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Rows, [][]any{{"a", int64(42), int64(2)}}) || res.RowsAffected != 1 {
		t.Fatalf("update path: %v affected %d", res.Rows, res.RowsAffected)
	}

	// Unqualified references in SET expressions read the existing row
	// (Postgres semantics), qualified excluded reads the proposal.
	res = exec(t, d, `insert into kv values ('a', 0, 0)
		on conflict (k) do update set v = v + excluded.v returning v`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(42)}}) {
		t.Fatalf("bare-name resolution: %v", res.Rows)
	}

	// Mixed statement: one insert, one update, both counted, RETURNING
	// in statement order.
	res = exec(t, d, `insert into kv values ('b', 7, 1), ('a', 5, 1)
		on conflict (k) do update set v = excluded.v returning k, v`)
	if res.RowsAffected != 2 ||
		!reflect.DeepEqual(res.Rows, [][]any{{"b", int64(7)}, {"a", int64(5)}}) {
		t.Fatalf("mixed: affected %d rows %v", res.RowsAffected, res.Rows)
	}
}

func TestUpsertUniqueIndexArbiter(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table users (id serial primary key, email text, visits int)`)
	exec(t, d, `create unique index by_email on users (email)`)
	exec(t, d, `insert into users (email, visits) values ('x@y.z', 1)`)

	// The target names the unique index's columns; the existing row is
	// found through the index and updated in place — the identity draw
	// for the conflicting proposal never sticks.
	res := exec(t, d, `insert into users (email, visits) values ('x@y.z', 0)
		on conflict (email) do update set visits = visits + 1
		returning id, visits`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1), int64(2)}}) {
		t.Fatalf("index arbiter: %v", res.Rows)
	}
	res = exec(t, d, `select count(*) from users`)
	if res.Rows[0][0] != int64(1) {
		t.Fatalf("row count: %v", res.Rows)
	}

	// NULL in the key never conflicts: both insert.
	exec(t, d, `insert into users (email) values (null) on conflict (email) do nothing`)
	exec(t, d, `insert into users (email) values (null) on conflict (email) do nothing`)
	res = exec(t, d, `select count(*) from users where email is null`)
	if res.Rows[0][0] != int64(2) {
		t.Fatalf("NULL keys: %v", res.Rows)
	}
}

func TestUpsertWhere(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, v int)`)
	exec(t, d, `insert into t values (1, 10), (2, 99)`)

	// Only conflicting pairs satisfying the WHERE update; the filtered
	// row neither counts nor returns.
	res := exec(t, d, `insert into t values (1, 100), (2, 200), (3, 300)
		on conflict (id) do update set v = excluded.v where t.v < 50
		returning id, v`)
	if res.RowsAffected != 2 {
		t.Fatalf("affected: got %d, want 2 (one update, one insert)", res.RowsAffected)
	}
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1), int64(100)}, {int64(3), int64(300)}}) {
		t.Fatalf("rows: %v", res.Rows)
	}
	res = exec(t, d, `select v from t where id = 2`)
	if res.Rows[0][0] != int64(99) {
		t.Fatalf("filtered row changed: %v", res.Rows)
	}

	// The WHERE may read excluded too.
	res = exec(t, d, `insert into t values (2, 5)
		on conflict (id) do update set v = excluded.v where excluded.v < t.v`)
	if res.RowsAffected != 1 {
		t.Fatalf("excluded in WHERE: affected %d", res.RowsAffected)
	}
}

func TestUpsertChecksAndErrors(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, v int check (v >= 0), w text)`)
	exec(t, d, `create unique index by_w on t (w)`)
	exec(t, d, `insert into t values (1, 1, 'a')`)

	// CHECK constraints judge the post-update row on the conflict path.
	if _, err := d.Exec(`insert into t values (1, 0, 'x') on conflict (id) do update set v = -5`); err == nil ||
		!strings.Contains(err.Error(), "check constraint") {
		t.Fatalf("check on upsert: %v", err)
	}

	// DO UPDATE handles only the named target: a collision on a
	// different unique constraint stays an error.
	exec(t, d, `insert into t values (2, 2, 'b')`)
	if _, err := d.Exec(`insert into t values (9, 9, 'b') on conflict (id) do update set v = 0`); err == nil ||
		!strings.Contains(err.Error(), "unique") {
		t.Fatalf("other-constraint collision: %v", err)
	}

	// Postgres's cardinality rule: two VALUES rows may not update the
	// same existing row.
	if _, err := d.Exec(`insert into t values (1, 7, 'p'), (1, 8, 'q') on conflict (id) do update set v = excluded.v`); err == nil ||
		!strings.Contains(err.Error(), "second time") {
		t.Fatalf("double update: %v", err)
	}
	// Nor may a row inserted by the statement be updated by it.
	if _, err := d.Exec(`insert into t values (5, 5, 'r'), (5, 6, 's') on conflict (id) do update set v = excluded.v`); err == nil ||
		!strings.Contains(err.Error(), "second time") {
		t.Fatalf("update of own insert: %v", err)
	}

	// Target that matches no uniqueness constraint.
	if _, err := d.Exec(`insert into t values (3, 3, 'c') on conflict (v) do update set w = 'x'`); err == nil ||
		!strings.Contains(err.Error(), "no unique or exclusion constraint") {
		t.Fatalf("bad target: %v", err)
	}

	// Parse-time rejections.
	for _, tc := range []struct{ q, want string }{
		{`insert into t values (1, 1, 'z') on conflict do update set v = 0`,
			"requires the conflict target"},
		{`insert into t values (1, 1, 'z') on conflict on constraint c do nothing`,
			"ON CONSTRAINT is not supported"},
		{`insert into t values (1, 1, 'z') on conflict (id) where v > 0 do nothing`,
			"index predicate is not supported"},
		{`insert into t values (1, 1, 'z') on conflict (id) do update set v = max(v)`,
			"aggregate functions are not allowed"},
	} {
		if _, err := d.Exec(tc.q); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: got %v, want %q", tc.q, err, tc.want)
		}
	}
}

func TestUpsertDescribe(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table kv (k text primary key, v int)`)

	// Parameters infer through the conflict clause: $2 appears only in
	// DO UPDATE SET and types as the column it assigns.
	st, err := d.Prepare(`insert into kv values ($1, 0) on conflict (k) do update set v = $2 returning v`)
	if err != nil {
		t.Fatal(err)
	}
	if st.NumParams() != 2 {
		t.Fatalf("params: %d", st.NumParams())
	}
	info, err := st.Describe()
	if err != nil {
		t.Fatal(err)
	}
	if info.Params[0] != bytdb.TString || info.Params[1] != bytdb.TInt {
		t.Fatalf("param types: %v", info.Params)
	}
	if !reflect.DeepEqual(info.Cols, []string{"v"}) {
		t.Fatalf("cols: %v", info.Cols)
	}

	// And the prepared statement executes both paths.
	if res, err := st.Exec("a", int64(1)); err != nil || res.Rows[0][0] != int64(0) {
		t.Fatalf("insert path: %v %v", err, res)
	}
	if res, err := st.Exec("a", int64(9)); err != nil || res.Rows[0][0] != int64(9) {
		t.Fatalf("update path: %v %v", err, res)
	}
}
