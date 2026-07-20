package sql

import (
	"reflect"
	"strings"
	"testing"
)

// TestBetweenWhere covers the scalar [NOT] BETWEEN [SYMMETRIC] forms
// in WHERE — the desugared comparisons must behave exactly as their
// spelled-out equivalents, three-valued logic included.
func TestBetweenWhere(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, n int, s text)`)
	exec(t, d, `insert into t values
		(1, 5, 'apple'), (2, 10, 'banana'), (3, 15, 'cherry'),
		(4, 20, 'date'), (5, null, null)`)

	if got := ids(t, d, `select id from t where n between 10 and 20 order by id`); !reflect.DeepEqual(got, []int64{2, 3, 4}) {
		t.Fatalf("BETWEEN: %v", got)
	}
	// Bounds are inclusive on both ends; NOT BETWEEN excludes them and
	// — per three-valued logic — never matches the NULL row.
	if got := ids(t, d, `select id from t where n not between 10 and 20 order by id`); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("NOT BETWEEN: %v", got)
	}
	// Text operands compare lexicographically like any comparison.
	if got := ids(t, d, `select id from t where s between 'b' and 'd' order by id`); !reflect.DeepEqual(got, []int64{2, 3}) {
		t.Fatalf("text BETWEEN: %v", got)
	}
	// SYMMETRIC accepts the bounds in either order; the plain form,
	// per SQL, matches nothing when lo > hi.
	if got := ids(t, d, `select id from t where n between 20 and 10`); len(got) != 0 {
		t.Fatalf("reversed plain BETWEEN matched: %v", got)
	}
	if got := ids(t, d, `select id from t where n between symmetric 20 and 10 order by id`); !reflect.DeepEqual(got, []int64{2, 3, 4}) {
		t.Fatalf("BETWEEN SYMMETRIC: %v", got)
	}
	if got := ids(t, d, `select id from t where n not between symmetric 20 and 10 order by id`); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("NOT BETWEEN SYMMETRIC: %v", got)
	}
	// ASYMMETRIC is the explicit spelling of the default.
	if got := ids(t, d, `select id from t where n between asymmetric 10 and 20 order by id`); !reflect.DeepEqual(got, []int64{2, 3, 4}) {
		t.Fatalf("BETWEEN ASYMMETRIC: %v", got)
	}

	// Precedence: the separating AND belongs to BETWEEN; the next AND
	// is boolean, and arithmetic bounds parse at the additive level.
	if got := ids(t, d, `select id from t where n between 5 and 10 and id > 1`); !reflect.DeepEqual(got, []int64{2}) {
		t.Fatalf("BETWEEN then boolean AND: %v", got)
	}
	if got := ids(t, d, `select id from t where n between 2 + 3 and 2 * 5 order by id`); !reflect.DeepEqual(got, []int64{1, 2}) {
		t.Fatalf("arithmetic bounds: %v", got)
	}

	// Placeholders work in both bounds, as in any comparison.
	if got := ids(t, d, `select id from t where n between $1 and $2 order by id`, 10, 20); !reflect.DeepEqual(got, []int64{2, 3, 4}) {
		t.Fatalf("BETWEEN with params: %v", got)
	}

	// A missing AND is a syntax error, not a silent misread.
	if _, err := d.Exec(`select id from t where n between 10`); err == nil {
		t.Fatal("BETWEEN without AND parsed")
	}
}

// TestBetweenCheck is the shape that motivated the feature: BETWEEN
// inside CHECK constraints (pg_dump-style DDL), both column-level and
// added by ALTER TABLE.
func TestBetweenCheck(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table ev (id int primary key,
		freq int check (freq between 1 and 52),
		day int,
		constraint day_range check (day between 0 and 6))`)

	exec(t, d, `insert into ev values (1, 1, 0), (2, 52, 6)`)
	// NULLs pass a check, per SQL: only definitely-false rejects.
	exec(t, d, `insert into ev values (3, null, null)`)

	for _, q := range []string{
		`insert into ev values (4, 0, 3)`,  // below the column check
		`insert into ev values (4, 53, 3)`, // above it
		`insert into ev values (4, 10, 7)`, // violates the named table check
	} {
		if _, err := d.Exec(q); err == nil || !strings.Contains(err.Error(), "check constraint") {
			t.Fatalf("Exec(%q): err %v; want check violation", q, err)
		}
	}
	if _, err := d.Exec(`update ev set day = -1 where id = 1`); err == nil {
		t.Fatal("update past NOT BETWEEN-range check succeeded")
	}

	// ALTER TABLE ADD CHECK re-parses the stored text on every write;
	// BETWEEN must round-trip through that text form too.
	exec(t, d, `create table s (id int primary key, pct int)`)
	exec(t, d, `insert into s values (1, 50)`)
	exec(t, d, `alter table s add constraint pct_range check (pct between 0 and 100)`)
	if _, err := d.Exec(`insert into s values (2, 101)`); err == nil {
		t.Fatal("added BETWEEN check not enforced")
	}
	exec(t, d, `insert into s values (2, 100)`)

	// Installing a check existing rows already violate must fail.
	if _, err := d.Exec(`alter table s add check (pct not between 40 and 60)`); err == nil {
		t.Fatal("ADD CHECK over violating rows succeeded")
	}
}
