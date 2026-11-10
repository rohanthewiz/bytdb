package sql

// Session regression tests from the deep-look remediation plan.

import (
	"strings"
	"testing"
)

// TestSessionParseErrorFailsBlock (D-07): a statement that failed to
// parse returned before the block's bookkeeping, so the block stayed
// healthy and COMMIT persisted the writes before the typo. Postgres
// fails the block on any error, parse errors included, and answers the
// COMMIT with ROLLBACK.
func TestSessionParseErrorFailsBlock(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table b (id int primary key)`)
	s := d.NewSession()
	defer s.Close()

	sessExec(t, s, `begin`)
	sessExec(t, s, `insert into b values (100)`)
	if _, err := s.Exec(`insrt into b values (101)`); err == nil {
		t.Fatal("typo parsed")
	}
	if s.Status() != TxFailed {
		t.Fatalf("status after a parse error in the block = %c; want E", s.Status())
	}
	if _, err := s.Exec(`select 1`); err == nil || !strings.Contains(err.Error(), "current transaction is aborted") {
		t.Fatalf("statement in the failed block: err = %v", err)
	}
	// BEGIN is refused in a failed block too (25P02), not just warned.
	if _, err := s.Exec(`begin`); err == nil || !strings.Contains(err.Error(), "current transaction is aborted") {
		t.Fatalf("BEGIN in the failed block: err = %v", err)
	}
	if res := sessExec(t, s, `commit`); res.Tag != "ROLLBACK" {
		t.Fatalf("COMMIT of a failed block: tag %q; want ROLLBACK", res.Tag)
	}
	if res := exec(t, d, `select count(*) from b`); res.Rows[0][0] != int64(0) {
		t.Fatalf("the failed block's insert was committed: %v", res.Rows)
	}

	// Outside a block a parse error changes nothing.
	if _, err := s.Exec(`insrt into b values (1)`); err == nil {
		t.Fatal("typo parsed")
	}
	if s.Status() != TxIdle {
		t.Fatalf("status after an idle parse error = %c; want I", s.Status())
	}
	// A healthy block still only warns on a nested BEGIN.
	sessExec(t, s, `begin`)
	if res := sessExec(t, s, `begin`); res.Notice == "" {
		t.Fatal("nested BEGIN in a healthy block: no warning")
	}
	sessExec(t, s, `rollback`)
}

// TestSubqueryLimitInterimReject (D-09): the subquery runners ignore
// some ORDER BY / LIMIT / OFFSET combinations, which returned wrong rows
// silently. Until subqueries run through the real SELECT path (D-27)
// those shapes are refused; the shapes the runners get right still run.
func TestSubqueryLimitInterimReject(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table o (id int primary key, amt int)`)
	exec(t, d, `insert into o values (1, 10), (2, 50), (3, 30), (4, 5), (5, 99)`)
	for _, q := range []string{
		`select (select amt from o order by amt desc limit 1)`,
		`select (select amt from o order by amt offset 1 limit 1)`,
		`select id from o where id in (select id from o order by amt desc limit 2)`,
		`select id from o where id = any (select id from o limit 2)`,
		`select exists (select 1 from o limit 0)`,
		`select exists (select 1 from o offset 10)`,
		`select array(select amt from o limit 2)`,
		`select array(select amt from o order by amt desc)`,
		`select array(select amt from o order by id)`,
	} {
		if _, err := d.Exec(q); err == nil || !strings.Contains(err.Error(), "not supported yet") {
			t.Errorf("%s: err = %v, want the interim refusal", q, err)
		}
	}
	for q, want := range map[string]any{
		`select exists (select 1 from o where amt > 90 limit 1)`:               true,
		`select (select amt from o limit 1)`:                                   int64(10),
		`select (select max(amt) from o)`:                                      int64(99),
		`select array(select amt from o order by amt)`:                         "{5,10,30,50,99}",
		`select array(select amt from o order by 1)`:                           "{5,10,30,50,99}",
		`select count(*) from o where id in (select id from o where amt > 20)`: int64(3),
	} {
		res, err := d.Exec(q)
		if err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		if got := res.Rows[0][0]; got != want {
			t.Errorf("%s = %v (%T), want %v", q, got, got, want)
		}
	}
}

// TestUpsertChangingReferencedKey (D-10): ON CONFLICT DO UPDATE that
// changes a key other rows reference skipped the parent-side check, so
// the child was left dangling. UPDATE always checked; the two now share
// one code path.
func TestUpsertChangingReferencedKey(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table p (id int primary key, name text)`)
	exec(t, d, `create table ch (id int primary key, pid int references p(id))`)
	exec(t, d, `insert into p values (1, 'a'), (2, 'b')`)
	exec(t, d, `insert into ch values (1, 1)`)

	msg := execErr(t, d, `insert into p values (1, 'x') on conflict (id) do update set id = 100`)
	if !strings.Contains(msg, "violates foreign key constraint") {
		t.Fatalf("upsert moving a referenced key: %s", msg)
	}
	if res := exec(t, d, `select id from p order by id`); len(res.Rows) != 2 || res.Rows[0][0] != int64(1) {
		t.Fatalf("parent rows after the refused upsert: %v", res.Rows)
	}
	// An unreferenced key may move; a non-key update of a referenced row
	// is fine.
	exec(t, d, `insert into p values (2, 'x') on conflict (id) do update set id = 200`)
	exec(t, d, `insert into p values (1, 'x') on conflict (id) do update set name = excluded.name`)
	if res := exec(t, d, `select id, name from p order by id`); len(res.Rows) != 2 || res.Rows[1][0] != int64(200) || res.Rows[0][1] != "x" {
		t.Fatalf("parent rows: %v", res.Rows)
	}
}

// TestSequenceFunctionsInRowExpressions (D-18): nextval/setval in an
// ALTER ... USING or a CHECK expression dereferenced a nil statement
// transaction and panicked. They are now refused when the expression is
// validated.
func TestSequenceFunctionsInRowExpressions(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create sequence sq`)
	exec(t, d, `create table t (id int primary key, i int)`)
	exec(t, d, `insert into t values (1, 1)`)
	for _, q := range []string{
		`alter table t alter column i type int using nextval('sq')`,
		`alter table t add constraint ck check (nextval('sq') > 0)`,
		`alter table t add constraint ck check (setval('sq', 5) > 0)`,
		`create table u (id int primary key, v int check (v < nextval('sq')))`,
	} {
		msg := execErr(t, d, q)
		if !strings.Contains(msg, "sequence functions are not allowed") {
			t.Errorf("%s: %s", q, msg)
		}
	}
	// The sequence was never drawn from.
	if res := exec(t, d, `select nextval('sq')`); res.Rows[0][0] != int64(1) {
		t.Fatalf("nextval after the refusals = %v; want 1", res.Rows[0][0])
	}
}

// TestSmallSQLCorrectness (D-21): the plan's grab-bag of smaller items.
func TestSmallSQLCorrectness(t *testing.T) {
	d := openDB(t)

	// COALESCE short-circuits: unused arguments are not evaluated.
	exec(t, d, `create sequence s`)
	if res := exec(t, d, `select coalesce(1, 1/0), coalesce(2, nextval('s'))`); res.Rows[0][0] != int64(1) || res.Rows[0][1] != int64(2) {
		t.Fatalf("coalesce = %v", res.Rows[0])
	}
	if res := exec(t, d, `select nextval('s')`); res.Rows[0][0] != int64(1) {
		t.Fatalf("an unused coalesce argument drew from the sequence: nextval = %v", res.Rows[0][0])
	}

	// A CHECK over an identity column judges the drawn value.
	exec(t, d, `create table idc (id serial primary key check (id > 5), v int)`)
	if msg := execErr(t, d, `insert into idc (v) values (1)`); !strings.Contains(msg, "violates check constraint") {
		t.Fatalf("drawn identity 1 vs CHECK (id > 5): %s", msg)
	}
	if res := exec(t, d, `select count(*) from idc`); res.Rows[0][0] != int64(0) {
		t.Fatal("the failing row was stored")
	}
	exec(t, d, `insert into idc values (10, 1)`) // explicit values are checked before the insert, as ever

	// count(DISTINCT x) is charged against the row cap.
	exec(t, d, `create table dv (id int primary key)`)
	exec(t, d, `insert into dv values (1), (2), (3), (4), (5)`)
	d.e.SetMaxResultRows(3)
	if msg := execErr(t, d, `select count(distinct id) from dv`); !strings.Contains(msg, "result row limit") {
		t.Fatalf("count(DISTINCT) over the row cap: %s", msg)
	}
	d.e.SetMaxResultRows(0)

	// A view body cannot use placeholders.
	if _, err := d.Exec(`create view pv as select $1 as x`); err == nil || !strings.Contains(err.Error(), "there is no parameter") {
		t.Fatalf("view with a placeholder: %v", err)
	}

	// statement_timeout is range-checked.
	s := d.NewSession()
	defer s.Close()
	if _, err := s.Exec(`set statement_timeout = 99999999999999999999`); err == nil {
		t.Fatal("an overflowing statement_timeout was accepted")
	}
	sessExec(t, s, `set statement_timeout = 2147483647`)

	// Duplicate output names in a derived table are ambiguous.
	exec(t, d, `create table dd (id int primary key)`)
	exec(t, d, `insert into dd values (1), (2)`)
	if msg := execErr(t, d, `select x.id from (select a.id, b.id from dd a, dd b) x`); !strings.Contains(msg, "ambiguous") {
		t.Fatalf("duplicate derived column: %s", msg)
	}
	if res := exec(t, d, `select count(*) from (select a.id, b.id from dd a, dd b) x`); res.Rows[0][0] != int64(4) {
		t.Fatal("an unreferenced duplicate name must stay legal")
	}

	// An ungrouped aggregate yields one row, so EXISTS over it is true.
	if res := exec(t, d, `select exists (select count(*) from dd where false)`); res.Rows[0][0] != true {
		t.Fatalf("EXISTS (SELECT count(*) ... WHERE false) = %v", res.Rows[0][0])
	}

	// A join probe whose outer value changes Go type mid-stream re-plans.
	res := exec(t, d, `with x as (select 1 as v union all select 2.0) select dd.id from x join dd on dd.id = x.v`)
	if len(res.Rows) != 2 {
		t.Fatalf("join probe across a type change: %v", res.Rows)
	}
}

// TestLexerParserDivergences (D-22): literal forms, escapes, signs and
// precedence that silently differed from Postgres 16. The difftest
// corpus checks the same lines against a real Postgres.
func TestLexerParserDivergences(t *testing.T) {
	d := openDB(t)
	for q, want := range map[string]any{
		`select 0x1F`:                 int64(31),
		`select 0o17`:                 int64(15),
		`select 0b101`:                int64(5),
		`select 1_000`:                int64(1000),
		`select 0x_ff`:                int64(255),
		`select 1_000.000_5`:          1000.0005,
		`select -0x10`:                int64(-16),
		`select E'\x41'`:              "A",
		`select E'\101'`:              "A",
		`select E'café'`:              "café",
		`select E'\U0001F600'`:        "😀",
		`select -1::int`:              int64(-1),
		`select - - 1`:                int64(1),
		`select 1+-+1`:                int64(0),
		`select -9223372036854775808`: int64(-9223372036854775808),
		`select 'a' || 3 + 4`:         "a7",
		`select 1 = any(null::int[])`: nil,
		`select 1 = any('{}'::int[])`: false,
		`select count(*) from (select 1 as v) s where v = null`: int64(0),
	} {
		res, err := d.Exec(q)
		if err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		if got := res.Rows[0][0]; got != want {
			t.Errorf("%s = %#v, want %#v", q, got, want)
		}
	}
	for _, q := range []string{
		`select 1a`, `select 0x`, `select 1__000`, `select 1_`, `select 0b2`,
		`select E'\xff'`, `select E'\u12'`,
	} {
		if _, err := d.Exec(q); err == nil {
			t.Errorf("%s: accepted", q)
		}
	}
}
