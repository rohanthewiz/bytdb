package sqlfrontaudit

import (
	"fmt"
	"testing"
	"time"

	bsql "github.com/rohanthewiz/bytdb/sql"
)

// Each subtest asserts the Postgres answer (stated from knowledge; Postgres
// was not run). A failing subtest is a confirmed divergence.

func rows(t *testing.T, d *bsql.DB, q string, args ...any) string {
	t.Helper()
	res, err := d.Exec(q, args...)
	if err != nil {
		return "ERR: " + err.Error()
	}
	return fmt.Sprint(res.Rows)
}

func want(t *testing.T, d *bsql.DB, expect, q string, args ...any) {
	t.Helper()
	if got := rows(t, d, q, args...); got != expect {
		t.Errorf("BUG  %s %v\n      got  %s\n      want %s", q, args, got, expect)
	}
}

func setup(t *testing.T) *bsql.DB {
	d := openDB(t)
	must(t, d, `create table t (id int primary key, i int, s text, ts timestamp, d date, u uuid, j jsonb)`)
	must(t, d, `insert into t values (1, 10, 'a', '2024-02-29 12:00:00', '2024-02-29', 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', '{"a":1}')`)
	return d
}

// R1: a quoted literal (or text-bound parameter) compared to a
// timestamp/date/uuid/jsonb value anywhere outside a bare `col op literal`
// predicate silently never matches.
func TestR1_UntypedLiteralInExpressionLeaves(t *testing.T) {
	d := setup(t)
	want(t, d, "[[1]]", `select id from t where u in ('a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11')`)
	want(t, d, "[[1]]", `select id from t where u in ($1, $2)`, "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", "00000000-0000-0000-0000-000000000000")
	want(t, d, "[[1]]", `select id from t where u = any($1)`, "{a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11}")
	want(t, d, "[[1]]", `select id from t where ts in ('2024-02-29 12:00:00')`)
	want(t, d, "[[1]]", `select id from t where d in ('2024-02-29', '2024-03-01')`)
	want(t, d, "[[1]]", `select id from t where d in ($1)`, time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC))
	want(t, d, "[[1]]", `select id from t where j in ('{"a": 1}')`)
	want(t, d, "[[1]]", `select id from t where coalesce(ts, '2000-01-01') > '2024-01-01'`)
	want(t, d, "[[true]]", `select ts > '2024-01-01' from t`)
	want(t, d, "[[true]]", `select now() > '2020-01-01'`)
}

// R2: a CHECK constraint comparing a date/timestamp/uuid column with a quoted
// literal evaluates to unknown, which passes: the constraint is never enforced.
func TestR2_CheckWithDateLiteralNotEnforced(t *testing.T) {
	d := openDB(t)
	must(t, d, `create table ev (id int primary key, day date check (day >= '2000-01-01'), at timestamp check (at >= '2000-01-01 00:00:00'))`)
	if _, err := d.Exec(`insert into ev values (1, '1999-01-01', null)`); err == nil {
		t.Errorf("BUG  date CHECK (day >= '2000-01-01') accepted '1999-01-01'")
	}
	if _, err := d.Exec(`insert into ev values (2, null, '1990-01-01 00:00:00')`); err == nil {
		t.Errorf("BUG  timestamp CHECK (at >= '2000-01-01 00:00:00') accepted '1990-01-01'")
	}
}

// R3: casts act on the runtime Go kind, not the SQL type.
func TestR3_CastsIgnoreSourceType(t *testing.T) {
	d := setup(t)
	want(t, d, "[[19782]]", `select ts::date from t`)                 // days since epoch for 2024-02-29
	want(t, d, "[[1709164800000000]]", `select d::timestamp from t`)  // micros for 2024-02-29 00:00:00
	want(t, d, "[[1]]", `select id from t where ts::date = '2024-02-29'`)
	want(t, d, "[[true]]", `select now()::date = current_date`)
	want(t, d, "[[2024-02-29 12:00:00+00]]", `select ts::text from t`)
	want(t, d, "[[2024-02-29]]", `select d::text from t`)
	want(t, d, "[[a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11]]", `select u::text from t`)
	want(t, d, "[[x2024-02-29]]", `select 'x' || d from t`)
	want(t, d, "[[true]]", `select 1::bool`)
	want(t, d, "[[1]]", `select true::int`)
	want(t, d, "[[1000000]]", `select 1000000.0::text`)
	// ALTER ... USING with the idiomatic cast stores garbage.
	must(t, d, `alter table t alter column ts type date using ts::date`)
	want(t, d, "[[19782]]", `select ts from t`)
}

// R4: date/timestamp arithmetic and cross-type comparison use raw storage units.
func TestR4_DateArithmetic(t *testing.T) {
	d := setup(t)
	res := must(t, d, `select d + 1 from t`)
	if res.Types[0] != "date" {
		t.Errorf("BUG  d + 1 is typed %q (Postgres: date)", res.Types[0])
	}
	must(t, d, `insert into t (id, ts, d) values (2, '2024-02-28 00:00:00', '2024-03-05')`)
	want(t, d, "[[true]]", `select d > ts from t where id = 2`) // 2024-03-05 > 2024-02-28
}

// R5: COALESCE evaluates every argument (Postgres stops at the first non-NULL).
func TestR5_CoalesceEager(t *testing.T) {
	d := setup(t)
	must(t, d, `create sequence sq`)
	want(t, d, "[[1]]", `select coalesce(1, 1/0)`)
	must(t, d, `select coalesce(1, nextval('sq'))`)
	want(t, d, "[[1]]", `select nextval('sq')`)
}

// R6: parser / lexer wrong answers and rejections.
func TestR6_ParserLexer(t *testing.T) {
	d := setup(t)
	want(t, d, "[[-1]]", `select -1::int`)
	want(t, d, "[[1]]", `select - - 1`)
	want(t, d, "[[-9223372036854775808]]", `select -9223372036854775808`)
	want(t, d, "[[31]]", `select 0x1F`)   // Postgres 16
	want(t, d, "[[1000]]", `select 1_000`) // Postgres 16
	want(t, d, "[[A]]", `select E'\x41'`)
	want(t, d, "[[A]]", `select E'\101'`)
	want(t, d, "[[27]]", `select 2 || 3 + 4`) // + binds tighter than ||
	want(t, d, "[[]]", `select id from t where i = null`)
	want(t, d, "[[<nil>]]", `select 1 = any(null)`)
	want(t, d, "[[2]]", `select '1' + 1`)
}

// R7: SQL-reachable nil dereference.
func TestR7_PanicNextvalInRowExpr(t *testing.T) {
	d := setup(t)
	must(t, d, `create sequence sq`)
	for _, q := range []string{
		`alter table t alter column i type int using nextval('sq')`,
		`alter table t add constraint ck check (nextval('sq') > 0)`,
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("BUG  PANIC from %q: %v", q, r)
				}
			}()
			d.Exec(q)
		}()
	}
}

// R8: a statement that fails to parse inside a transaction block does not
// abort the block (Postgres: 25P02 until ROLLBACK; COMMIT reports ROLLBACK).
func TestR8_ParseErrorDoesNotAbortBlock(t *testing.T) {
	d := setup(t)
	s := d.NewSession()
	defer s.Close()
	s.Exec(`begin`)
	s.Exec(`insert into t (id) values (100)`)
	if _, err := s.Exec(`insrt into t (id) values (101)`); err == nil {
		t.Fatal("expected a syntax error")
	}
	if s.Status() != bsql.TxFailed {
		t.Errorf("BUG  status after syntax error in block = %c, want E", s.Status())
	}
	res, _ := s.Exec(`commit`)
	if res != nil && res.Tag != "ROLLBACK" {
		t.Errorf("BUG  COMMIT after a failed statement committed (tag %q)", res.Tag)
	}
	want(t, d, "[[0]]", `select count(*) from t where id = 100`)
}

// R9: static result types for common expressions fall back to text.
func TestR9_ResultTypes(t *testing.T) {
	d := setup(t)
	for q, wantT := range map[string]string{
		`select coalesce(i, 0) from t`:                      "int",
		`select coalesce(sum(i), 0) from t`:                 "int",
		`select nullif(i, 0) from t`:                        "int",
		`select (select max(id) from t)`:                    "int",
		`select exists(select 1 from t)`:                    "bool",
		`select case when id = 9 then null else i end from t`: "int",
		`select coalesce(ts, now()) from t`:                 "timestamp",
		`select upper(s) from t`:                            "string",
	} {
		res, err := d.Exec(q)
		if err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		if string(res.Types[0]) != wantT {
			t.Errorf("BUG  %s is typed %q, want %q (value %#v)", q, res.Types[0], wantT, res.Rows[0][0])
		}
	}
}
