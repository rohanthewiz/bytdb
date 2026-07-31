package sql

// isolation_test.go: Stage 4 of the concurrent-writes plan — the SQL
// surface of isolation levels. BEGIN ISOLATION LEVEL SERIALIZABLE and
// SET TRANSACTION must actually change behavior under an engine with
// concurrent writes (write skew conflicts instead of committing), SHOW
// must describe the engine's real mode, and autocommit statements must
// absorb bounded optimistic-conflict retries that transaction blocks
// never get.

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// openOCC opens a DB over an engine in concurrent-writes mode — the
// only mode where isolation levels are more than labels.
func openOCC(t *testing.T) *DB {
	t.Helper()
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "occ.db"),
		bytdb.WithConcurrentWrites())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return New(e)
}

// skewSetup creates the classic write-skew fixture: two rows whose
// values a pair of transactions cross-read and cross-write.
func skewSetup(t *testing.T, d *DB) {
	t.Helper()
	exec(t, d, `create table skew (id int primary key, v int)`)
	exec(t, d, `insert into skew values (1, 1), (2, 1)`)
}

// crossUpdate runs one arm of the write-skew pair on an open block:
// read the OTHER row, then write our own based on it.
func crossUpdate(t *testing.T, s *Session, readID, writeID int) {
	t.Helper()
	res := sessExec(t, s, `select v from skew where id = $1`, readID)
	if len(res.Rows) != 1 {
		t.Fatalf("read id=%d: %v", readID, res.Rows)
	}
	sessExec(t, s, `update skew set v = 0 where id = $1`, writeID)
}

// Plain BEGIN under concurrent writes is snapshot isolation: the
// write-skew pair both commit (the anomaly SI admits by design). This
// is the baseline that makes the serializable test meaningful.
func TestBeginDefaultOCCAllowsWriteSkew(t *testing.T) {
	d := openOCC(t)
	skewSetup(t, d)
	a, b := d.NewSession(), d.NewSession()
	defer a.Close()
	defer b.Close()

	sessExec(t, a, `begin`)
	sessExec(t, b, `begin`)
	crossUpdate(t, a, 2, 1)
	crossUpdate(t, b, 1, 2)
	sessExec(t, a, `commit`)
	sessExec(t, b, `commit`) // SI: disjoint write sets, both land

	res := exec(t, d, `select sum(v) from skew`)
	if res.Rows[0][0] != int64(0) {
		t.Fatalf("sum = %v; want 0 (write skew is expected under SI)", res.Rows[0][0])
	}
}

// BEGIN ISOLATION LEVEL SERIALIZABLE closes the skew: the second
// committer's read of the first's written row conflicts.
func TestBeginSerializablePreventsWriteSkew(t *testing.T) {
	d := openOCC(t)
	skewSetup(t, d)
	a, b := d.NewSession(), d.NewSession()
	defer a.Close()
	defer b.Close()

	sessExec(t, a, `begin isolation level serializable`)
	sessExec(t, b, `begin isolation level serializable`)
	crossUpdate(t, a, 2, 1)
	crossUpdate(t, b, 1, 2)
	sessExec(t, a, `commit`)
	if _, err := b.Exec(`commit`); !errors.Is(err, bytdb.ErrTxConflict) {
		t.Fatalf("second serializable commit: %v; want ErrTxConflict", err)
	}
	// The failed COMMIT ended the block; the session must be idle, and
	// exactly one arm's write must have survived.
	if b.Status() != TxIdle {
		t.Fatalf("status after failed commit = %c; want I", b.Status())
	}
	res := exec(t, d, `select sum(v) from skew`)
	if res.Rows[0][0] != int64(1) {
		t.Fatalf("sum = %v; want 1 (one arm committed)", res.Rows[0][0])
	}
}

// SET TRANSACTION ISOLATION LEVEL SERIALIZABLE as the block's first
// statement is equivalent to asking at BEGIN — read tracking starts
// before any read exists to miss.
func TestSetTransactionSerializable(t *testing.T) {
	d := openOCC(t)
	skewSetup(t, d)
	a, b := d.NewSession(), d.NewSession()
	defer a.Close()
	defer b.Close()

	sessExec(t, a, `begin`)
	sessExec(t, a, `set transaction isolation level serializable`)
	if res := sessExec(t, a, `show transaction isolation level`); res.Rows[0][0] != "serializable" {
		t.Fatalf("show in upgraded block: %v", res.Rows[0])
	}
	sessExec(t, b, `begin`)
	sessExec(t, b, `set transaction isolation level serializable`)
	crossUpdate(t, a, 2, 1)
	crossUpdate(t, b, 1, 2)
	sessExec(t, a, `commit`)
	if _, err := b.Exec(`commit`); !errors.Is(err, bytdb.ErrTxConflict) {
		t.Fatalf("second commit after SET TRANSACTION: %v; want ErrTxConflict", err)
	}
}

// After the block's first query the upgrade window is closed: reads
// already taken were not tracked, so honoring a late SET would fake a
// guarantee. Postgres refuses it with the same wording.
func TestSetTransactionAfterQueryRefused(t *testing.T) {
	d := openOCC(t)
	skewSetup(t, d)
	s := d.NewSession()
	defer s.Close()

	sessExec(t, s, `begin`)
	sessExec(t, s, `select v from skew where id = 1`)
	_, err := s.Exec(`set transaction isolation level serializable`)
	if err == nil || !strings.Contains(err.Error(), "must be called before any query") {
		t.Fatalf("late SET TRANSACTION: %v", err)
	}
	if s.Status() != TxFailed { // an error fails the block, like any other
		t.Fatalf("status = %c; want E", s.Status())
	}
	sessExec(t, s, `rollback`)

	// Outside a block it is a warning, not an error, since drivers
	// issue it unconditionally.
	res := sessExec(t, s, `set transaction isolation level serializable`)
	if !strings.Contains(res.Notice, "can only be used in transaction blocks") {
		t.Fatalf("stray SET TRANSACTION notice = %q", res.Notice)
	}
}

// SET SESSION CHARACTERISTICS (JDBC's spelling) and SET
// default_transaction_isolation both set the session default, which a
// bare BEGIN then inherits.
func TestDefaultIsolationSessionCharacteristics(t *testing.T) {
	d := openOCC(t)
	skewSetup(t, d)
	a, b := d.NewSession(), d.NewSession()
	defer a.Close()
	defer b.Close()

	for s, form := range map[*Session]string{
		a: `set session characteristics as transaction isolation level serializable`,
		b: `set default_transaction_isolation = 'SERIALIZABLE'`,
	} {
		sessExec(t, s, form)
		if res := sessExec(t, s, `show default_transaction_isolation`); res.Rows[0][0] != "serializable" {
			t.Fatalf("default after %q: %v", form, res.Rows[0])
		}
	}

	// Bare BEGINs now open serializable blocks: the skew pair conflicts.
	sessExec(t, a, `begin`)
	sessExec(t, b, `begin`)
	crossUpdate(t, a, 2, 1)
	crossUpdate(t, b, 1, 2)
	sessExec(t, a, `commit`)
	if _, err := b.Exec(`commit`); !errors.Is(err, bytdb.ErrTxConflict) {
		t.Fatalf("commit under serializable default: %v; want ErrTxConflict", err)
	}

	// RESET ALL restores the mode default.
	sessExec(t, a, `reset all`)
	if res := sessExec(t, a, `show default_transaction_isolation`); res.Rows[0][0] != "repeatable read" {
		t.Fatalf("default after RESET ALL: %v", res.Rows[0])
	}

	if _, err := a.Exec(`set default_transaction_isolation = 'bogus'`); err == nil ||
		!strings.Contains(err.Error(), "invalid value") {
		t.Fatalf("bogus level: %v", err)
	}
}

// SHOW describes the engine's real mode: snapshot isolation reports
// as repeatable read (Postgres's name for it), the single-writer
// default as serializable — for sessions and bare DBs alike.
func TestShowIsolationByMode(t *testing.T) {
	occ := openOCC(t)
	if res := exec(t, occ, `show transaction isolation level`); res.Rows[0][0] != "repeatable read" {
		t.Fatalf("occ bare DB: %v", res.Rows[0])
	}
	s := occ.NewSession()
	defer s.Close()
	if res := sessExec(t, s, `show transaction isolation level`); res.Rows[0][0] != "repeatable read" {
		t.Fatalf("occ session: %v", res.Rows[0])
	}
	sessExec(t, s, `begin isolation level serializable`)
	if res := sessExec(t, s, `show transaction isolation level`); res.Rows[0][0] != "serializable" {
		t.Fatalf("occ serializable block: %v", res.Rows[0])
	}
	sessExec(t, s, `rollback`)

	plain := openDB(t)
	if res := exec(t, plain, `show transaction isolation level`); res.Rows[0][0] != "serializable" {
		t.Fatalf("single-writer mode: %v", res.Rows[0])
	}
}

// In the single-writer default the serializable forms are accepted
// no-ops — every transaction is serializable anyway — and nothing
// about block behavior changes.
func TestSerializableFormsDefaultMode(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, v int)`)
	s := d.NewSession()
	defer s.Close()

	sessExec(t, s, `set session characteristics as transaction isolation level serializable`)
	sessExec(t, s, `begin isolation level serializable`)
	sessExec(t, s, `insert into t values (1, 10)`)
	if res := sessExec(t, s, `show transaction isolation level`); res.Rows[0][0] != "serializable" {
		t.Fatalf("show: %v", res.Rows[0])
	}
	sessExec(t, s, `commit`)
	if res := exec(t, d, `select v from t where id = 1`); res.Rows[0][0] != int64(10) {
		t.Fatalf("committed row: %v", res.Rows)
	}
}

// The autocommit retry loop, exercised deterministically: injected
// conflicts within the budget are absorbed; past it, the conflict
// surfaces. The statement must be idempotent — injection happens
// after real effects apply, so retries re-apply them.
func TestAutocommitConflictRetry(t *testing.T) {
	d := openOCC(t)
	exec(t, d, `create table t (id int primary key, v int)`)
	exec(t, d, `insert into t values (1, 0)`)
	defer testInjectConflicts.Store(0)

	testInjectConflicts.Store(autocommitRetries) // exactly exhausts the budget
	exec(t, d, `update t set v = 5 where id = 1`)
	if res := exec(t, d, `select v from t where id = 1`); res.Rows[0][0] != int64(5) {
		t.Fatalf("v = %v; want 5", res.Rows[0][0])
	}

	testInjectConflicts.Store(autocommitRetries + 1) // one loss too many
	if _, err := d.Exec(`update t set v = 7 where id = 1`); !errors.Is(err, bytdb.ErrTxConflict) {
		t.Fatalf("exhausted retries: %v; want ErrTxConflict", err)
	}
	testInjectConflicts.Store(0)

	// A block statement is never retried: the same injection that an
	// autocommit statement absorbs passes through a session block
	// untouched (injection skips d.tx != nil, so the block path's
	// exemption is structural — pin it stays that way).
	s := d.NewSession()
	defer s.Close()
	testInjectConflicts.Store(1)
	sessExec(t, s, `begin`)
	sessExec(t, s, `update t set v = 9 where id = 1`)
	sessExec(t, s, `commit`)
	if got := testInjectConflicts.Load(); got != 1 {
		t.Fatalf("block statement consumed an injected conflict (left %d); blocks must not retry", got)
	}
}

// Concurrent autocommit increments through sessions: whatever mix of
// internal retries and surfaced conflicts occurs, every increment
// that reported success must be present exactly once — no lost
// updates, no double-applies.
func TestAutocommitConcurrentIncrements(t *testing.T) {
	d := openOCC(t)
	exec(t, d, `create table c (id int primary key, v int)`)
	exec(t, d, `insert into c values (1, 0)`)

	const workers, perWorker = 6, 25
	var wg sync.WaitGroup
	succ := make([]int, workers)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := d.NewSession()
			defer s.Close()
			for done := 0; done < perWorker; {
				_, err := s.Exec(`update c set v = v + 1 where id = 1`)
				switch {
				case err == nil:
					succ[w]++
					done++
				case errors.Is(err, bytdb.ErrTxConflict):
					// Internal retries exhausted under contention; the
					// statement did not apply. Re-run at "client" level,
					// exactly what a wire client would do on 40001.
				default:
					t.Errorf("worker %d: %v", w, err)
					return
				}
			}
		}()
	}
	wg.Wait()

	total := 0
	for _, n := range succ {
		total += n
	}
	res := exec(t, d, `select v from c where id = 1`)
	if res.Rows[0][0] != int64(total) || total != workers*perWorker {
		t.Fatalf("v = %v; successes = %d; want both %d", res.Rows[0][0], total, workers*perWorker)
	}
}
