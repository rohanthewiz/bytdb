package sql

// seqfuncs_test.go: lastval() and currval() — fed by identity draws,
// session-scoped, Postgres-worded before the first draw.

import (
	"reflect"
	"strings"
	"testing"
)

func TestLastvalCurrval(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table users (id serial primary key, name text)`)

	// Before any draw: Postgres's exact wording.
	if _, err := d.Exec(`select lastval()`); err == nil ||
		!strings.Contains(err.Error(), "lastval is not yet defined in this session") {
		t.Fatalf("lastval before draw: %v", err)
	}
	if _, err := d.Exec(`select currval('users_id_seq')`); err == nil ||
		!strings.Contains(err.Error(), `currval of sequence "users_id_seq" is not yet defined`) {
		t.Fatalf("currval before draw: %v", err)
	}

	// A draw defines both; multi-row inserts leave the last value.
	exec(t, d, `insert into users (name) values ('ada'), ('grace')`)
	res := exec(t, d, `select lastval(), currval('users_id_seq')`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(2), int64(2)}}) {
		t.Fatalf("after draw: %v", res.Rows)
	}

	// An explicit id is not a draw: lastval stays at the last draw.
	exec(t, d, `insert into users values (10, 'alan')`)
	res = exec(t, d, `select lastval()`)
	if res.Rows[0][0] != int64(2) {
		t.Fatalf("explicit insert moved lastval: %v", res.Rows)
	}

	// currval is per implied sequence; lastval follows the most recent
	// draw across tables.
	exec(t, d, `create table posts (id serial primary key, title text)`)
	exec(t, d, `insert into posts (title) values ('t1')`)
	res = exec(t, d, `select lastval(), currval('posts_id_seq'), currval('users_id_seq')`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1), int64(1), int64(2)}}) {
		t.Fatalf("cross-table: %v", res.Rows)
	}

	// Usable inside expressions and predicates.
	res = exec(t, d, `select name from users where id = currval('users_id_seq')`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"grace"}}) {
		t.Fatalf("currval in WHERE: %v", res.Rows)
	}
}

func TestLastvalSessionScope(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id serial primary key, v text)`)

	// Draws in one session do not define lastval in another.
	s1, s2 := d.NewSession(), d.NewSession()
	if _, err := s1.Exec(`insert into t (v) values ('a')`); err != nil {
		t.Fatal(err)
	}
	res, err := s1.Exec(`select lastval()`)
	if err != nil || res.Rows[0][0] != int64(1) {
		t.Fatalf("s1 lastval: %v %v", err, res)
	}
	if _, err := s2.Exec(`select lastval()`); err == nil ||
		!strings.Contains(err.Error(), "not yet defined") {
		t.Fatalf("s2 sees s1's draw: %v", err)
	}

	// As in Postgres, lastval survives a rolled-back block: sequence
	// state is session-local, not transactional.
	if _, err := s2.Exec(`begin`); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Exec(`insert into t (v) values ('b')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Exec(`rollback`); err != nil {
		t.Fatal(err)
	}
	res, err = s2.Exec(`select lastval()`)
	if err != nil || res.Rows[0][0] != int64(2) {
		t.Fatalf("lastval after rollback: %v %v", err, res)
	}
}
