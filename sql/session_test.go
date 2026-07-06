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
		`commit`:                                "COMMIT",
		`commit work`:                           "COMMIT",
		`end`:                                   "COMMIT",
		`end transaction`:                       "COMMIT",
		`commit and no chain`:                   "COMMIT",
		`rollback`:                              "ROLLBACK",
		`abort`:                                 "ROLLBACK",
		`rollback transaction`:                  "ROLLBACK",
		`savepoint sp1`:                         "SAVEPOINT",
		`release sp1`:                           "RELEASE",
		`release savepoint sp1`:                 "RELEASE",
		`rollback to sp1`:                       "ROLLBACK",
		`rollback to savepoint sp1`:             "ROLLBACK",
		`rollback work to savepoint sp1`:        "ROLLBACK",
		`rollback transaction to savepoint sp1`: "ROLLBACK",
	} {
		st, err := Parse(q)
		if err != nil {
			t.Fatalf("Parse(%q): %v", q, err)
		}
		tc, ok := st.(*TxnControl)
		if !ok || tc.Tag != want {
			t.Fatalf("Parse(%q) = %#v; want tag %q", q, st, want)
		}
		if strings.Contains(q, "sp1") && tc.Name != "sp1" {
			t.Fatalf("Parse(%q) name = %q; want sp1", q, tc.Name)
		}
	}
	if st, _ := Parse(`rollback to savepoint sp1`); st.(*TxnControl).Kind != TxnRollbackTo {
		t.Fatal("ROLLBACK TO parsed as plain rollback")
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
		`start work`,
		`savepoint`,             // missing name
		`release`,               // missing name
		`rollback to`,           // missing name
		`commit to savepoint x`, // TO is ROLLBACK-only
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

func TestSessionSavepoints(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, v text)`)
	s := d.NewSession()
	defer s.Close()

	// Rewind discards writes after the mark; the block stays open and
	// commits everything else.
	sessExec(t, s, `begin`)
	sessExec(t, s, `insert into t values (1, 'a')`)
	sessExec(t, s, `savepoint sp`)
	sessExec(t, s, `insert into t values (2, 'b')`)
	sessExec(t, s, `update t set v = 'a2' where id = 1`)
	sessExec(t, s, `rollback to savepoint sp`)
	if s.Status() != TxActive {
		t.Fatalf("status after ROLLBACK TO = %c; want T", s.Status())
	}
	sessExec(t, s, `insert into t values (3, 'c')`)
	sessExec(t, s, `commit`)
	res := exec(t, d, `select id, v from t order by id`)
	if len(res.Rows) != 2 || res.Rows[0][1] != "a" || res.Rows[1][0] != int64(3) {
		t.Fatalf("rows after savepoint commit = %v", res.Rows)
	}

	// ROLLBACK TO recovers a failed block: the failed statement's
	// partial writes and everything after the mark are gone, and the
	// block commits cleanly.
	sessExec(t, s, `begin`)
	sessExec(t, s, `savepoint sp`)
	sessExec(t, s, `insert into t values (4, 'd')`)
	if _, err := s.Exec(`insert into t values (5, 'e'), (1, 'dup')`); err == nil {
		t.Fatal("duplicate insert succeeded")
	}
	if s.Status() != TxFailed {
		t.Fatalf("status = %c; want E", s.Status())
	}
	// SAVEPOINT and RELEASE are refused in a failed block.
	if _, err := s.Exec(`savepoint other`); err == nil ||
		!strings.Contains(err.Error(), "current transaction is aborted") {
		t.Fatalf("savepoint in failed block err = %v", err)
	}
	if _, err := s.Exec(`release sp`); err == nil ||
		!strings.Contains(err.Error(), "current transaction is aborted") {
		t.Fatalf("release in failed block err = %v", err)
	}
	sessExec(t, s, `rollback to sp`)
	if s.Status() != TxActive {
		t.Fatalf("status after recovery = %c; want T", s.Status())
	}
	sessExec(t, s, `insert into t values (6, 'f')`)
	sessExec(t, s, `commit`)
	res = exec(t, d, `select id from t order by id`)
	if len(res.Rows) != 3 || res.Rows[2][0] != int64(6) {
		t.Fatalf("rows after recovered block = %v", res.Rows)
	}

	// RELEASE keeps changes and destroys the name (and later marks).
	sessExec(t, s, `begin`)
	sessExec(t, s, `savepoint a`)
	sessExec(t, s, `insert into t values (7, 'g')`)
	sessExec(t, s, `savepoint b`)
	sessExec(t, s, `release savepoint a`)
	if _, err := s.Exec(`rollback to b`); err == nil ||
		!strings.Contains(err.Error(), `savepoint "b" does not exist`) {
		t.Fatalf("rollback to released-past savepoint err = %v", err)
	}
	sessExec(t, s, `rollback`)

	// A repeated name shadows; RELEASE unshadows the earlier one.
	sessExec(t, s, `begin`)
	sessExec(t, s, `savepoint a`)
	sessExec(t, s, `insert into t values (8, 'h')`)
	sessExec(t, s, `savepoint a`)
	sessExec(t, s, `insert into t values (9, 'i')`)
	sessExec(t, s, `rollback to a`) // the later mark: only id 9 unwinds
	res = sessExec(t, s, `select count(*) from t where id in (8, 9)`)
	if res.Rows[0][0] != int64(1) {
		t.Fatalf("after rollback to shadowing mark: %v", res.Rows[0][0])
	}
	sessExec(t, s, `release a`)     // destroys the later mark...
	sessExec(t, s, `rollback to a`) // ...resolving to the earlier one
	res = sessExec(t, s, `select count(*) from t where id in (8, 9)`)
	if res.Rows[0][0] != int64(0) {
		t.Fatalf("after rollback to earlier mark: %v", res.Rows[0][0])
	}
	sessExec(t, s, `rollback`)
}

func TestSessionSavepointErrors(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key)`)
	s := d.NewSession()
	defer s.Close()

	// Outside a block, savepoint statements are errors, not warnings.
	for q, want := range map[string]string{
		`savepoint sp`:            "SAVEPOINT can only be used in transaction blocks",
		`release savepoint sp`:    "RELEASE SAVEPOINT can only be used in transaction blocks",
		`rollback to savepoint x`: "ROLLBACK TO SAVEPOINT can only be used in transaction blocks",
	} {
		if _, err := s.Exec(q); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q err = %v; want %q", q, err, want)
		}
	}

	// Unknown names, in healthy and failed blocks alike.
	sessExec(t, s, `begin`)
	if _, err := s.Exec(`rollback to nope`); err == nil ||
		!strings.Contains(err.Error(), `savepoint "nope" does not exist`) {
		t.Fatalf("unknown savepoint err = %v", err)
	}
	// That error itself failed the block; ROLLBACK TO with a bad name
	// while failed still reports the name, and the block stays failed.
	if s.Status() != TxFailed {
		t.Fatalf("status = %c; want E", s.Status())
	}
	if _, err := s.Exec(`rollback to nope`); err == nil ||
		!strings.Contains(err.Error(), `savepoint "nope" does not exist`) {
		t.Fatalf("unknown savepoint in failed block err = %v", err)
	}
	if s.Status() != TxFailed {
		t.Fatalf("status = %c; want E", s.Status())
	}
	sessExec(t, s, `rollback`)

	// Savepoints work in read-only blocks.
	sessExec(t, s, `begin read only`)
	sessExec(t, s, `savepoint sp`)
	sessExec(t, s, `rollback to sp`)
	sessExec(t, s, `release sp`)
	sessExec(t, s, `commit`)
}
