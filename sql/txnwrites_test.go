package sql

// txnwrites_test.go: the engine's transaction write cap
// (Engine.SetMaxTxnWrites) as SQL statements meet it. UPDATE and plain
// multi-row INSERT are refused before staging anything. ON CONFLICT is
// charged per row actually written. DELETE is exempt. A BEGIN block
// accumulates across statements, and ROLLBACK TO gives writes back.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func wantCapErr(t *testing.T, q string, err error) {
	t.Helper()
	if !errors.Is(err, bytdb.ErrTxnTooLarge) {
		t.Fatalf("%s: err = %v, want the transaction write limit refusal", q, err)
	}
}

func TestTxnWriteCapStatements(t *testing.T) {
	d := openDB(t)
	seedNums(t, d, 10)
	d.e.SetMaxTxnWrites(5)

	// A whole-table UPDATE over the cap fails during its scan, and none
	// of its rows change.
	q := `update nums set v = -1`
	_, err := d.Exec(q)
	wantCapErr(t, q, err)
	if res := exec(t, d, `select count(*) from nums where v = -1`); res.Rows[0][0].(int64) != 0 {
		t.Fatalf("refused UPDATE changed %v rows", res.Rows[0][0])
	}
	// One under the cap goes through.
	exec(t, d, `update nums set v = -1 where id < 5`)

	q = `insert into nums values (100, 0, 0), (101, 0, 0), (102, 0, 0), (103, 0, 0), (104, 0, 0), (105, 0, 0)`
	_, err = d.Exec(q)
	wantCapErr(t, q, err)
	if res := exec(t, d, `select count(*) from nums where id >= 100`); res.Rows[0][0].(int64) != 0 {
		t.Fatalf("refused INSERT left %v rows", res.Rows[0][0])
	}

	// ON CONFLICT DO NOTHING: six proposed rows, but five collide, so
	// one write. The row count alone must not refuse it.
	exec(t, d, `insert into nums values (0, 0, 0), (1, 0, 0), (2, 0, 0), (3, 0, 0), (4, 0, 0), (200, 0, 0) on conflict do nothing`)

	// DELETE is exempt: eleven rows in one statement under a cap of 5.
	exec(t, d, `delete from nums`)
	if res := exec(t, d, `select count(*) from nums`); res.Rows[0][0].(int64) != 0 {
		t.Fatalf("rows after DELETE: %v", res.Rows[0][0])
	}

	d.e.SetMaxTxnWrites(0)
	seedNumsMore(t, d, 20)
	exec(t, d, `update nums set v = 0`)
}

// seedNumsMore inserts ids 1000.. into the existing nums table.
func seedNumsMore(t *testing.T, d *DB, n int) {
	t.Helper()
	var b strings.Builder
	b.WriteString(`insert into nums values `)
	for i := range n {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "(%d, 0, 0)", 1000+i)
	}
	exec(t, d, b.String())
}

// TestTxnWriteCapBlock: the count spans a BEGIN block's statements, a
// refusal aborts the block, and ROLLBACK TO gives writes back.
func TestTxnWriteCapBlock(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table w (id int primary key)`)
	d.e.SetMaxTxnWrites(4)
	s := d.NewSession()
	must := func(q string) {
		t.Helper()
		if _, err := s.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	must(`begin`)
	must(`insert into w values (1), (2)`)
	must(`insert into w values (3)`)
	q := `insert into w values (4), (5)` // 3 + 2 > 4
	_, err := s.Exec(q)
	wantCapErr(t, q, err)
	if _, err := s.Exec(`insert into w values (4)`); err == nil ||
		!strings.Contains(err.Error(), "current transaction is aborted") {
		t.Fatalf("block not aborted after the refusal: %v", err)
	}
	must(`rollback`)

	must(`begin`)
	must(`insert into w values (1), (2)`)
	must(`savepoint a`)
	must(`insert into w values (3), (4)`)
	must(`rollback to a`)
	// Without the give-back this would be 2 + 2 + 2 = 6 > 4.
	must(`insert into w values (13), (14)`)
	must(`commit`)
	if res := exec(t, d, `select count(*) from w`); res.Rows[0][0].(int64) != 4 {
		t.Fatalf("rows after commit = %v, want 4", res.Rows[0][0])
	}
}
