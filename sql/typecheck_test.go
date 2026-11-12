package sql

// Regression tests for the typed bind pass (deep-look D-23), the casts
// it informs (D-24) and the result types it fixes (D-25). The difftest
// corpus (sql/difftest/corpus/20_typing.sql) checks the same shapes
// against a real Postgres; these keep them covered without one.

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func typedDB(t *testing.T) *DB {
	t.Helper()
	d := openDB(t)
	exec(t, d, `create table ev (id int primary key, due date, at timestamp, u uuid, n int, f float)`)
	exec(t, d, `insert into ev values (1, '2099-01-01', '2024-02-29 12:00:00', 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', 1, 1.0)`)
	exec(t, d, `insert into ev values (2, '2024-02-29', '2019-06-01 00:00:00', 'b0eebc99-9c0b-4ef8-bb6d-6bb9bd380a12', 2, 2.5)`)
	return d
}

// TestTypedLiteralsEverywhere: a quoted literal takes its context's type
// wherever it sits, not only in a top-level WHERE.
func TestTypedLiteralsEverywhere(t *testing.T) {
	d := typedDB(t)
	exec(t, d, `create view v as select id, due from ev where due = '2024-02-29'`)
	for q, want := range map[string][]int64{
		`select id from ev where due > now() order by id`:                            {1},
		`select id from ev where at::date = '2024-02-29'`:                            {1},
		`select id from ev where id in (select id from ev where due = '2024-02-29')`: {2},
		`with x as (select id from ev where due = '2024-02-29') select id from x`:    {2},
		`select id from (select id, due from ev) s where s.due = '2024-02-29'`:       {2},
		`select id from v`: {2},
		`select id from ev where u in ('a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11')`:                  {1},
		`select id from ev where at in ('2024-02-29 12:00:00')`:                                  {1},
		`select id from ev where coalesce(at, now()) < '2025-01-01' order by id`:                 {1, 2},
		`select id from ev where (select max(at) from ev) > '2020-01-01' order by id`:            {1, 2},
		`select id from ev where due > at order by id`:                                           {1, 2},
		`select id from ev where due between '2024-01-01' and '2024-12-31'`:                      {2},
		`select id from ev where u = any(array['a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11']::uuid[])`: {1},
	} {
		if got := ids(t, d, q); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", q, got, want)
		}
	}
	res := exec(t, d, `select case when at > '2020-01-01' then 'recent' else 'old' end, at > '2024-01-01' from ev order by id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"recent", true}, {"old", false}}) {
		t.Errorf("select-list comparisons: %v", res.Rows)
	}
}

// TestTypedCheckIsEnforced: a CHECK comparing a date column with a
// literal was never violated, because the literal stayed a string.
func TestTypedCheckIsEnforced(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table ck (id int primary key, day date check (day >= '2000-01-01'))`)
	if msg := execErr(t, d, `insert into ck values (1, '1999-01-01')`); !strings.Contains(msg, "violates check constraint") {
		t.Fatalf("CHECK on a date: %s", msg)
	}
	exec(t, d, `insert into ck values (2, '2001-01-01')`)
	if msg := execErr(t, d, `update ck set day = '1990-05-05'`); !strings.Contains(msg, "violates check constraint") {
		t.Fatalf("CHECK on update: %s", msg)
	}
	// A CHECK Postgres has no operator for is refused at CREATE.
	if msg := execErr(t, d, `create table bad (id int primary key, n int check (n = true))`); !strings.Contains(msg, "operator does not exist") {
		t.Fatalf("CHECK (int = bool): %s", msg)
	}
}

// TestTypedRefusals: comparisons and operators Postgres has no operator
// for are refused at bind instead of yielding NULL, and an untyped
// literal that cannot take its context's type is invalid input.
func TestTypedRefusals(t *testing.T) {
	d := typedDB(t)
	for q, want := range map[string]string{
		`select 1 < 'a'`:                   "invalid input syntax",
		`select n + 'x' from ev`:           "invalid input syntax",
		`select 1 = true`:                  "operator does not exist: bigint = boolean",
		`select id from ev where n = true`: "operator does not exist",
		`select 1 < 2 < 3`:                 "operator does not exist",
		`select at + 1 from ev`:            "operator does not exist: timestamp with time zone + bigint",
		`select 2 || 3`:                    "operator does not exist: bigint || bigint",
		`select current_date::int`:         "cannot cast type date to bigint",
	} {
		if msg := execErr(t, d, q); !strings.Contains(msg, want) {
			t.Errorf("%s: %s, want %q", q, msg, want)
		}
	}
}

// TestTypedCasts (D-24): casts convert by what the operand is.
func TestTypedCasts(t *testing.T) {
	d := typedDB(t)
	res := exec(t, d, `select at::date, u::text, 'x' || due, at::date::text, due::timestamp from ev where id = 2`)
	want := []any{int64(18048), "b0eebc99-9c0b-4ef8-bb6d-6bb9bd380a12", "x2024-02-29", "2019-06-01", int64(19782) * microsPerDay}
	if !reflect.DeepEqual(res.Rows[0], want) {
		t.Errorf("typed casts = %#v, want %#v", res.Rows[0], want)
	}
	if !reflect.DeepEqual(res.Types[:1], []bytdb.ColType{bytdb.TDate}) {
		t.Errorf("ts::date described as %v", res.Types[0])
	}
	res = exec(t, d, `select 1000000.0::text, 1e15::text, 0.00001::text, 1::bool, 0::bool, true::int, false::int`)
	// Decimal literals are numeric (N-048), and numeric's text keeps the
	// written scale and never uses an exponent — Postgres's answers.
	want = []any{"1000000.0", "1000000000000000", "0.00001", true, false, int64(1), int64(0)}
	if !reflect.DeepEqual(res.Rows[0], want) {
		t.Errorf("casts = %#v, want %#v", res.Rows[0], want)
	}
	// ALTER COLUMN TYPE ... USING ts::date converts instead of storing
	// the microsecond count as a day number.
	exec(t, d, `alter table ev alter column at type date using at::date`)
	if res := exec(t, d, `select at::text from ev where id = 1`); res.Rows[0][0] != "2024-02-29" {
		t.Errorf("after ALTER ... USING at::date: %v", res.Rows)
	}
}

// TestTypedResultDescriptions (D-25): result types for the expressions
// that used to fall back to text.
func TestTypedResultDescriptions(t *testing.T) {
	d := typedDB(t)
	res := exec(t, d, `select coalesce(n, 0), nullif(n, 2), (select max(n) from ev),
		exists (select 1 from ev), case when n = 1 then null else n end, due + 1, due - due,
		coalesce(sum(f), 0) from ev group by n, due order by n`)
	want := []bytdb.ColType{bytdb.TInt, bytdb.TInt, bytdb.TInt, bytdb.TBool, bytdb.TInt,
		bytdb.TDate, bytdb.TInt, bytdb.TFloat}
	if !reflect.DeepEqual(res.Types, want) {
		t.Errorf("types = %v, want %v", res.Types, want)
	}
	// Describe gives placeholders their context's type.
	st, err := d.Prepare(`select id from ev where id in ($1, $2) and n + $3 > 0 and due = $4 and $5::int = 1 and id in (select id from ev where n = $6)`)
	if err != nil {
		t.Fatal(err)
	}
	info, err := st.Describe()
	if err != nil {
		t.Fatal(err)
	}
	wantP := []bytdb.ColType{bytdb.TInt, bytdb.TInt, bytdb.TInt, bytdb.TDate, bytdb.TInt, bytdb.TInt}
	if !reflect.DeepEqual(info.Params, wantP) {
		t.Errorf("params = %v, want %v", info.Params, wantP)
	}
	// A prepared statement's shared tree is never modified by typing.
	before := st.st.(*Select).Where
	if _, err := st.Exec(int64(1), int64(2), int64(0), "2024-02-29", int64(1), int64(2)); err != nil {
		t.Fatal(err)
	}
	if st.st.(*Select).Where != before {
		t.Error("execution rewrote the prepared statement's tree")
	}
}

// TestHashKeysBySQLEquality (D-26): 1 and 1.0 are equal in SQL, so they
// collapse under UNION, DISTINCT, GROUP BY and count(DISTINCT); NaN
// groups with NaN instead of failing the statement.
func TestHashKeysBySQLEquality(t *testing.T) {
	d := openDB(t)
	if res := exec(t, d, `select 1 union select 1.0`); len(res.Rows) != 1 {
		t.Errorf("1 UNION 1.0: %v", res.Rows)
	}
	if res := exec(t, d, `select count(*) from (select distinct x from (select 1 as x union all select 1.0) s) s2`); res.Rows[0][0] != int64(1) {
		t.Errorf("DISTINCT over 1 and 1.0: %v", res.Rows)
	}
	// NaN cannot be stored yet (D-36), so the values are derived.
	const vals = `(select 1.0::float8 as x union all select 1::float8 union all
		select 'NaN'::float8 union all select 'NaN'::float8 union all select 2.5::float8) f`
	res := exec(t, d, `select x, count(*) from `+vals+` group by x order by x`)
	if len(res.Rows) != 3 || res.Rows[0][1] != int64(2) || res.Rows[2][1] != int64(2) {
		t.Errorf("GROUP BY over 1.0, 1, NaN, NaN, 2.5: %v", res.Rows)
	}
	if res := exec(t, d, `select count(distinct x) from `+vals); res.Rows[0][0] != int64(3) {
		t.Errorf("count(DISTINCT x) = %v, want 3", res.Rows[0][0])
	}
}

// TestVerifyChecks (release note for D-23): rows admitted while a CHECK
// over a date was inert are found by VACUUM (VERIFY) / DB.VerifyChecks,
// without changing anything and without failing the statement.
func TestVerifyChecks(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table ck (id int primary key, day date check (day >= '2000-01-01'))`)
	exec(t, d, `insert into ck values (1, '2001-01-01')`)
	// The engine API bypasses CHECKs (they belong to the SQL layer),
	// which stands in for a row written before the check was enforced.
	old, err := bytdb.ParseDate("1999-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.e.Insert("ck", 2, old); err != nil {
		t.Fatal(err)
	}
	found, err := d.VerifyChecks()
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Table != "ck" || found[0].Key[0] != int64(2) {
		t.Fatalf("VerifyChecks = %+v", found)
	}
	res := exec(t, d, `vacuum (verify) ck`)
	if !strings.Contains(res.Notice, `row [2] of relation "ck" violates check constraint "ck_day_check"`) ||
		!strings.Contains(res.Notice, "found 1 check constraint violation") {
		t.Fatalf("VACUUM (VERIFY) notice: %q", res.Notice)
	}
	if res := exec(t, d, `select count(*) from ck`); res.Rows[0][0] != int64(2) {
		t.Fatal("VERIFY changed the table")
	}
}
