package sql

import (
	"strings"
	"testing"
)

func sessExec(t *testing.T, s *Session, q string, args ...any) *Result {
	t.Helper()
	res, err := s.Exec(q, args...)
	if err != nil {
		t.Fatalf("Session.Exec(%q): %v", q, err)
	}
	return res
}

func TestSessionCommitAndRollback(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, v text)`)
	s := d.NewSession()
	defer s.Close()

	if s.Status() != TxIdle {
		t.Fatalf("status = %c; want I", s.Status())
	}
	sessExec(t, s, `begin`)
	if s.Status() != TxActive {
		t.Fatalf("status = %c; want T", s.Status())
	}
	sessExec(t, s, `insert into t values (1, 'a'), (2, 'b')`)
	sessExec(t, s, `update t set v = 'a2' where id = 1`)

	// The block sees its own writes...
	res := sessExec(t, s, `select v from t order by id`)
	if len(res.Rows) != 2 || res.Rows[0][0] != "a2" {
		t.Fatalf("in-txn rows = %v", res.Rows)
	}
	// ...but autocommit readers on fresh snapshots do not.
	if res := exec(t, d, `select * from t`); len(res.Rows) != 0 {
		t.Fatalf("uncommitted rows leaked: %v", res.Rows)
	}

	sessExec(t, s, `commit`)
	if s.Status() != TxIdle {
		t.Fatalf("status after commit = %c; want I", s.Status())
	}
	if res := exec(t, d, `select * from t`); len(res.Rows) != 2 {
		t.Fatalf("committed rows = %v", res.Rows)
	}

	// ROLLBACK discards everything staged since BEGIN.
	sessExec(t, s, `begin`)
	sessExec(t, s, `delete from t`)
	sessExec(t, s, `insert into t values (9, 'z')`)
	sessExec(t, s, `rollback`)
	res = exec(t, d, `select id from t order by id`)
	if len(res.Rows) != 2 || res.Rows[0][0] != int64(1) {
		t.Fatalf("rows after rollback = %v", res.Rows)
	}
}

func TestSessionAbortedBlock(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, v text)`)
	exec(t, d, `insert into t values (1, 'a')`)
	s := d.NewSession()
	defer s.Close()

	sessExec(t, s, `begin`)
	sessExec(t, s, `insert into t values (2, 'b')`)
	// Duplicate key fails the statement and the block.
	if _, err := s.Exec(`insert into t values (1, 'dup')`); err == nil {
		t.Fatal("duplicate insert succeeded")
	}
	if s.Status() != TxFailed {
		t.Fatalf("status = %c; want E", s.Status())
	}
	// Everything but COMMIT/ROLLBACK is refused now — even reads.
	if _, err := s.Exec(`select * from t`); err == nil ||
		!strings.Contains(err.Error(), "current transaction is aborted") {
		t.Fatalf("in-failed-block select err = %v", err)
	}
	// COMMIT of a failed block rolls back and says so.
	res := sessExec(t, s, `commit`)
	if res.Tag != "ROLLBACK" {
		t.Fatalf("commit tag = %q; want ROLLBACK", res.Tag)
	}
	if s.Status() != TxIdle {
		t.Fatalf("status = %c; want I", s.Status())
	}
	// The pre-error staged insert (id 2) must be gone with the block.
	res = sessExec(t, s, `select id from t`)
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(1) {
		t.Fatalf("rows after failed block = %v", res.Rows)
	}

	// A failed statement mid-multi-row-INSERT can never leak its
	// earlier rows: the block is failed, so only rollback remains.
	sessExec(t, s, `begin`)
	if _, err := s.Exec(`insert into t values (3, 'c'), (1, 'dup'), (4, 'd')`); err == nil {
		t.Fatal("partially-failing insert succeeded")
	}
	sessExec(t, s, `rollback`)
	res = sessExec(t, s, `select count(*) from t`)
	if res.Rows[0][0] != int64(1) {
		t.Fatalf("count after partial-failure block = %v", res.Rows[0][0])
	}
}

func TestSessionNoticesAndRedundantControl(t *testing.T) {
	d := openDB(t)
	s := d.NewSession()
	defer s.Close()

	if res := sessExec(t, s, `commit`); res.Notice == "" || res.Tag != "" {
		t.Fatalf("stray commit: notice=%q tag=%q", res.Notice, res.Tag)
	}
	if res := sessExec(t, s, `rollback`); res.Notice == "" {
		t.Fatalf("stray rollback: notice=%q", res.Notice)
	}
	sessExec(t, s, `begin`)
	if res := sessExec(t, s, `begin`); !strings.Contains(res.Notice, "already a transaction") {
		t.Fatalf("nested begin notice = %q", res.Notice)
	}
	if s.Status() != TxActive { // the redundant BEGIN changed nothing
		t.Fatalf("status = %c; want T", s.Status())
	}
	sessExec(t, s, `rollback`)
}

func TestSessionReadOnly(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key)`)
	exec(t, d, `insert into t values (1)`)
	s := d.NewSession()
	defer s.Close()

	sessExec(t, s, `begin read only`)
	if res := sessExec(t, s, `select * from t`); len(res.Rows) != 1 {
		t.Fatalf("read-only select = %v", res.Rows)
	}
	// A read-only block takes no writer lock: autocommit writes from
	// elsewhere proceed, and the block's snapshot does not see them.
	exec(t, d, `insert into t values (2)`)
	if res := sessExec(t, s, `select count(*) from t`); res.Rows[0][0] != int64(1) {
		t.Fatalf("snapshot count = %v; want 1", res.Rows[0][0])
	}
	if _, err := s.Exec(`insert into t values (3)`); err == nil ||
		!strings.Contains(err.Error(), "read-only transaction") {
		t.Fatalf("read-only insert err = %v", err)
	}
	if s.Status() != TxFailed {
		t.Fatalf("status = %c; want E", s.Status())
	}
	sessExec(t, s, `rollback`)
	if res := sessExec(t, s, `select count(*) from t`); res.Rows[0][0] != int64(2) {
		t.Fatalf("post-block count = %v; want 2", res.Rows[0][0])
	}
}

func TestSessionRejectsDDLInBlock(t *testing.T) {
	d := openDB(t)
	s := d.NewSession()
	defer s.Close()

	sessExec(t, s, `begin`)
	if _, err := s.Exec(`create table t (id int primary key)`); err == nil ||
		!strings.Contains(err.Error(), "cannot run inside a transaction block") {
		t.Fatalf("DDL in block err = %v", err)
	}
	if s.Status() != TxFailed {
		t.Fatalf("status = %c; want E", s.Status())
	}
	sessExec(t, s, `rollback`)
	sessExec(t, s, `create table t (id int primary key)`) // fine outside
}

func TestSessionParamsAndPrepared(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, v text)`)
	s := d.NewSession()
	defer s.Close()

	ins, err := d.Prepare(`insert into t values ($1, $2)`)
	if err != nil {
		t.Fatal(err)
	}
	sessExec(t, s, `begin`)
	if _, err := s.ExecStmt(ins, int64(1), "a"); err != nil {
		t.Fatal(err)
	}
	sessExec(t, s, `insert into t values ($1, $2)`, int64(2), "b")
	sessExec(t, s, `commit`)
	if res := exec(t, d, `select count(*) from t`); res.Rows[0][0] != int64(2) {
		t.Fatalf("count = %v", res.Rows[0][0])
	}
}

func TestTxnControlParsing(t *testing.T) {
	for q, want := range map[string]string{
		`begin`:             "BEGIN",
		`BEGIN WORK`:        "BEGIN",
		`begin transaction`: "BEGIN",
		`begin isolation level repeatable read read only, not deferrable`: "BEGIN",
		`start transaction isolation level serializable read write`:       "START TRANSACTION",
		`commit`:               "COMMIT",
		`commit work`:          "COMMIT",
		`end`:                  "COMMIT",
		`end transaction`:      "COMMIT",
		`commit and no chain`:  "COMMIT",
		`rollback`:             "ROLLBACK",
		`abort`:                "ROLLBACK",
		`rollback transaction`: "ROLLBACK",
	} {
		st, err := Parse(q)
		if err != nil {
			t.Fatalf("Parse(%q): %v", q, err)
		}
		tc, ok := st.(*TxnControl)
		if !ok || tc.Tag != want {
			t.Fatalf("Parse(%q) = %#v; want tag %q", q, st, want)
		}
	}
	if st, _ := Parse(`begin read only`); !st.(*TxnControl).ReadOnly {
		t.Fatal("READ ONLY not honored")
	}
	if st, _ := Parse(`begin read only read write`); st.(*TxnControl).ReadOnly {
		t.Fatal("later READ WRITE should win")
	}
	for _, q := range []string{
		`commit and chain`, // unsupported
		`begin isolation level bogus`,
		`rollback to savepoint sp1`, // savepoints don't exist
		`start work`,
	} {
		if _, err := Parse(q); err == nil {
			t.Fatalf("Parse(%q) succeeded; want error", q)
		}
	}

	// Transaction control needs session state; a bare DB refuses it.
	d := openDB(t)
	if _, err := d.Exec(`begin`); err == nil ||
		!strings.Contains(err.Error(), "require a Session") {
		t.Fatalf("bare-DB begin err = %v", err)
	}
}
