package bytdb

// serializable_test.go: Stage 3 of the concurrent-writes plan —
// opt-in SERIALIZABLE isolation via read-set validation, and DDL that
// stays fully serialized against every concurrent transaction. The
// snapshot-isolation anomalies (write skew, phantoms, update-miss)
// are each demonstrated REAL under WriteTxn first where practical, so
// the serializable assertions prove the upgrade rather than an
// accident of timing.

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rohanthewiz/btypedb"
)

// serialTable creates a plain (non-identity) two-column int table so
// tests control their own keys.
func serialTable(t *testing.T, e *Engine, name string) {
	t.Helper()
	_, err := e.CreateTable(name, []Column{
		{Name: "id", Type: TInt},
		{Name: "val", Type: TInt},
	}, "id")
	if err != nil {
		t.Fatal(err)
	}
}

// mustInsert seeds rows outside the transactions under test.
func mustInsert(t *testing.T, e *Engine, table string, rows ...[]any) {
	t.Helper()
	for _, r := range rows {
		if err := e.Insert(table, r...); err != nil {
			t.Fatal(err)
		}
	}
}

// The classic write-skew shape at the SQL-engine level: each
// transaction reads the OTHER row and updates its own. Snapshot
// isolation commits both (the anomaly); serializable must abort the
// second committer.
func TestSerializableWriteSkew(t *testing.T) {
	run := func(serializable bool) []error {
		e := openOCCEngine(t, filepath.Join(t.TempDir(), "skew.db"))
		defer e.Close()
		serialTable(t, e, "oncall")
		mustInsert(t, e, "oncall", []any{1, 1}, []any{2, 1})

		begin := e.Begin
		if serializable {
			begin = func(bool) (*Txn, error) { return e.BeginSerializable() }
		}
		t1, err := begin(true)
		if err != nil {
			t.Fatal(err)
		}
		t2, err := begin(true)
		if err != nil {
			t.Fatal(err)
		}
		// T1: "the other doctor (2) is on call, I can go off."
		if _, ok, err := t1.Get("oncall", 2); err != nil || !ok {
			t.Fatalf("t1 read: %v %v", ok, err)
		}
		if _, err := t1.Update("oncall", []any{1}, map[string]any{"val": 0}); err != nil {
			t.Fatal(err)
		}
		// T2: symmetric.
		if _, ok, err := t2.Get("oncall", 1); err != nil || !ok {
			t.Fatalf("t2 read: %v %v", ok, err)
		}
		if _, err := t2.Update("oncall", []any{2}, map[string]any{"val": 0}); err != nil {
			t.Fatal(err)
		}
		return []error{t1.Commit(), t2.Commit()}
	}

	// Snapshot isolation: both commit — the skew is real.
	for i, err := range run(false) {
		if err != nil {
			t.Fatalf("SI commit %d: %v; want both to commit (write skew is the SI baseline)", i+1, err)
		}
	}
	// Serializable: first wins, second conflicts.
	errs := run(true)
	if errs[0] != nil {
		t.Fatalf("serializable first committer: %v", errs[0])
	}
	if !errors.Is(errs[1], btypedb.ErrTxConflict) {
		t.Fatalf("serializable second committer: %v; want ErrTxConflict", errs[1])
	}
}

// A serializable scan claims its whole range: a row inserted into the
// scanned interval by a concurrent committer is a phantom and must
// conflict. A row beyond the interval must not.
func TestSerializablePhantomScan(t *testing.T) {
	for _, tc := range []struct {
		insertID int
		conflict bool
	}{
		{50, true},   // inside the scanned [1, 100)
		{500, false}, // beyond it
	} {
		t.Run(fmt.Sprint(tc.insertID), func(t *testing.T) {
			e := openOCCEngine(t, filepath.Join(t.TempDir(), "phantom.db"))
			defer e.Close()
			serialTable(t, e, "items")
			serialTable(t, e, "totals")
			mustInsert(t, e, "items", []any{10, 1}, []any{20, 2})

			tx, err := e.BeginSerializable()
			if err != nil {
				t.Fatal(err)
			}
			sum := int64(0)
			for row, err := range tx.ScanRange("items", []any{1}, []any{100}) {
				if err != nil {
					t.Fatal(err)
				}
				sum += row.Vals[1].(int64)
			}
			if err := tx.Insert("totals", 1, sum); err != nil {
				t.Fatal(err)
			}

			// A committed insert relative to the scanned interval.
			mustInsert(t, e, "items", []any{tc.insertID, 100})

			err = tx.Commit()
			if tc.conflict && !errors.Is(err, btypedb.ErrTxConflict) {
				t.Fatalf("phantom at id=%d: commit = %v; want ErrTxConflict", tc.insertID, err)
			}
			if !tc.conflict && err != nil {
				t.Fatalf("insert beyond scan at id=%d: commit = %v; want nil", tc.insertID, err)
			}
		})
	}
}

// The FK shape that motivated Stage 3: a child insert probes the
// parent's existence but writes only child keys, so snapshot isolation
// lets a concurrent parent delete slip through. Serializable catches
// it in both commit orders.
func TestSerializableParentDeleteVsChildInsert(t *testing.T) {
	setup := func(t *testing.T, path string) *Engine {
		e := openOCCEngine(t, path)
		serialTable(t, e, "parent")
		serialTable(t, e, "child")
		mustInsert(t, e, "parent", []any{1, 0})
		return e
	}

	// Order A: the parent delete commits first; the child insert that
	// verified the parent must then conflict.
	t.Run("delete-first", func(t *testing.T) {
		e := setup(t, filepath.Join(t.TempDir(), "fk-a.db"))
		defer e.Close()
		child, err := e.BeginSerializable()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, err := child.Get("parent", 1); err != nil || !ok {
			t.Fatalf("parent probe: %v %v", ok, err)
		}
		if err := child.Insert("child", 10, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Delete("parent", 1); err != nil {
			t.Fatal(err)
		}
		if err := child.Commit(); !errors.Is(err, btypedb.ErrTxConflict) {
			t.Fatalf("child insert after parent delete: %v; want ErrTxConflict", err)
		}
	})

	// Order B: the child insert commits first; the serializable parent
	// delete that scanned for children must then conflict.
	t.Run("insert-first", func(t *testing.T) {
		e := setup(t, filepath.Join(t.TempDir(), "fk-b.db"))
		defer e.Close()
		del, err := e.BeginSerializable()
		if err != nil {
			t.Fatal(err)
		}
		// "No children reference parent 1" — a full scan of child.
		for _, err := range del.Scan("child") {
			if err != nil {
				t.Fatal(err)
			}
			t.Fatal("child table should be empty")
		}
		if _, err := del.Delete("parent", 1); err != nil {
			t.Fatal(err)
		}
		if err := e.Insert("child", 10, 1); err != nil {
			t.Fatal(err)
		}
		if err := del.Commit(); !errors.Is(err, btypedb.ErrTxConflict) {
			t.Fatalf("parent delete after child insert: %v; want ErrTxConflict", err)
		}
	})
}

// Rows fetched THROUGH an index are reads of the row keys themselves:
// a concurrent update to a non-indexed column never touches the index
// range, and must still conflict with a serializable index scan.
func TestSerializableIndexScanRowRead(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "idx.db"))
	defer e.Close()
	_, err := e.CreateTable("users", []Column{
		{Name: "id", Type: TInt},
		{Name: "team", Type: TString},
		{Name: "score", Type: TInt},
	}, "id")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateIndex("users", "by_team", false, "team"); err != nil {
		t.Fatal(err)
	}
	mustInsert(t, e, "users", []any{1, "red", 10}, []any{2, "blue", 20})

	tx, err := e.BeginSerializable()
	if err != nil {
		t.Fatal(err)
	}
	for row, err := range tx.ScanIndex("users", "by_team", []any{"red"}, nil) {
		if err != nil {
			t.Fatal(err)
		}
		_ = row
	}
	if err := tx.Insert("users", 3, "green", 0); err != nil {
		t.Fatal(err)
	}

	// score is not indexed: this write moves no index entry, only the
	// row key the scan above fetched.
	if _, err := e.Update("users", []any{1}, map[string]any{"score": 99}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); !errors.Is(err, btypedb.ErrTxConflict) {
		t.Fatalf("index-scanned row rewritten underneath: %v; want ErrTxConflict", err)
	}
}

// An UPDATE that found no row made a decision ("nothing to change")
// that a concurrent insert of that key must invalidate.
func TestSerializableUpdateMissThenInsert(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "miss.db"))
	defer e.Close()
	serialTable(t, e, "cfg")
	serialTable(t, e, "log")

	tx, err := e.BeginSerializable()
	if err != nil {
		t.Fatal(err)
	}
	updated, err := tx.Update("cfg", []any{7}, map[string]any{"val": 1})
	if err != nil {
		t.Fatal(err)
	}
	if updated {
		t.Fatal("row 7 should not exist yet")
	}
	if err := tx.Insert("log", 1, 0); err != nil { // acted on the miss
		t.Fatal(err)
	}
	mustInsert(t, e, "cfg", []any{7, 42})
	if err := tx.Commit(); !errors.Is(err, btypedb.ErrTxConflict) {
		t.Fatalf("commit after missed row appeared: %v; want ErrTxConflict", err)
	}
}

// Any DDL landing mid-flight aborts every overlapping write
// transaction — even a snapshot-isolation one touching a different
// table: its planning may have depended on the old catalog.
func TestSerializableDDLAbortsInflightWriter(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "ddl.db"))
	defer e.Close()
	serialTable(t, e, "t1")
	serialTable(t, e, "t2")

	tx, err := e.Begin(true) // plain SI writer
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Insert("t1", 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := e.AddColumn("t2", Column{Name: "extra", Type: TString}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); !errors.Is(err, btypedb.ErrTxConflict) {
		t.Fatalf("writer overlapping a DDL commit: %v; want ErrTxConflict", err)
	}
	// The retry — now planned against the new catalog — succeeds.
	if err := e.WriteTxn(func(tx *Txn) error { return tx.Insert("t1", 1, 1) }); err != nil {
		t.Fatalf("retry after DDL: %v", err)
	}
}

// CreateIndex under sustained concurrent inserts: the escalation to
// the exclusive (head-frozen) run guarantees the DDL completes, and
// the wildcard publish guarantees no insert slips between the
// backfill's snapshot and its commit — the index is complete.
func TestSerializableCreateIndexUnderLoad(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "ddl-load.db"))
	defer e.Close()
	occIDTable(t, e, "events") // id identity PK + note string

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var inserted atomic.Int64
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				err := e.WriteTxn(func(tx *Txn) error {
					return tx.Insert("events", nil, "x")
				})
				if err == nil {
					inserted.Add(1)
				} else if !errors.Is(err, btypedb.ErrTxConflict) {
					t.Error(err)
					return
				}
			}
		})
	}

	// Let the writers get going, then index the busy table.
	for inserted.Load() < 200 {
		runtime.Gosched()
	}
	if _, err := e.CreateIndex("events", "by_note", false, "note"); err != nil {
		t.Fatalf("CreateIndex under write load: %v", err)
	}
	close(stop)
	wg.Wait()

	// Every row must be reachable through the index — the backfill
	// missed nothing, and post-DDL inserts maintained it.
	rows, viaIndex := 0, 0
	for _, err := range e.Scan("events") {
		if err != nil {
			t.Fatal(err)
		}
		rows++
	}
	for _, err := range e.ScanIndex("events", "by_note", nil, nil) {
		if err != nil {
			t.Fatal(err)
		}
		viaIndex++
	}
	if rows == 0 || rows != viaIndex {
		t.Fatalf("table has %d rows but index reaches %d", rows, viaIndex)
	}
}

// Serializable transactions over disjoint data must not conflict:
// read validation adds no false positives.
func TestSerializableDisjointCommit(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "disjoint.db"))
	defer e.Close()
	serialTable(t, e, "a")
	serialTable(t, e, "b")
	mustInsert(t, e, "a", []any{1, 1})
	mustInsert(t, e, "b", []any{1, 1})

	t1, err := e.BeginSerializable()
	if err != nil {
		t.Fatal(err)
	}
	t2, err := e.BeginSerializable()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := t1.Get("a", 1); err != nil {
		t.Fatal(err)
	}
	if err := t1.Insert("a", 2, 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := t2.Get("b", 1); err != nil {
		t.Fatal(err)
	}
	if err := t2.Insert("b", 2, 2); err != nil {
		t.Fatal(err)
	}
	if err := t1.Commit(); err != nil {
		t.Fatalf("t1: %v", err)
	}
	if err := t2.Commit(); err != nil {
		t.Fatalf("t2 (fully disjoint): %v", err)
	}
}

// Without WithConcurrentWrites the serializable surface degrades to
// the plain (already serializable) write path.
func TestSerializableDefaultModeDegrades(t *testing.T) {
	e, err := Open(filepath.Join(t.TempDir(), "plain.db"), WithSyncNever())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	serialTable(t, e, "t")

	if err := e.WriteTxnSerializable(func(tx *Txn) error {
		return tx.Insert("t", 1, 1)
	}); err != nil {
		t.Fatalf("WriteTxnSerializable in default mode: %v", err)
	}
	tx, err := e.BeginSerializable()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Insert("t", 2, 2); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, err := range e.Scan("t") {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("rows = %d; want 2", n)
	}
}

// The invariant SI famously breaks: every withdrawal re-checks that
// the two accounts together stay non-negative. Under serializable
// (with conflict retries) the invariant must hold at every step and
// at the end.
func TestSerializableInvariantStress(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "invariant.db"))
	defer e.Close()
	serialTable(t, e, "acct")
	mustInsert(t, e, "acct", []any{1, 100}, []any{2, 100})

	const workers, tries = 8, 30
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			from := int64(1 + w%2) // half the workers draw from each account
			for range tries {
				for { // retry loop on conflict
					err := e.WriteTxnSerializable(func(tx *Txn) error {
						r1, ok, err := tx.Get("acct", 1)
						if err != nil || !ok {
							return fmt.Errorf("acct 1: %v %v", ok, err)
						}
						r2, ok, err := tx.Get("acct", 2)
						if err != nil || !ok {
							return fmt.Errorf("acct 2: %v %v", ok, err)
						}
						total := r1.Vals[1].(int64) + r2.Vals[1].(int64)
						if total < 10 {
							return nil // constraint says no: withdraw nothing
						}
						bal := r1.Vals[1].(int64)
						if from == 2 {
							bal = r2.Vals[1].(int64)
						}
						_, err = tx.Update("acct", []any{from}, map[string]any{"val": bal - 10})
						return err
					})
					if err == nil {
						break
					}
					if !errors.Is(err, btypedb.ErrTxConflict) {
						t.Error(err)
						return
					}
				}
			}
		})
	}
	wg.Wait()

	r1, _, err := e.Get("acct", 1)
	if err != nil {
		t.Fatal(err)
	}
	r2, _, err := e.Get("acct", 2)
	if err != nil {
		t.Fatal(err)
	}
	total := r1.Vals[1].(int64) + r2.Vals[1].(int64)
	// Withdrawals stop when total < 10 and each takes exactly 10, so a
	// serial history can never drive the total negative.
	if total < 0 {
		t.Fatalf("combined balance %d < 0: write skew under serializable", total)
	}
}

// A serializable transaction that only reads always commits: with no
// writes there is nothing to publish, and a read-only snapshot is
// trivially serializable at its snapshot point.
func TestSerializableReadOnlyCommit(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "ro.db"))
	defer e.Close()
	serialTable(t, e, "t")
	mustInsert(t, e, "t", []any{1, 1})

	tx, err := e.BeginSerializable()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := tx.Get("t", 1); err != nil || !ok {
		t.Fatalf("read: %v %v", ok, err)
	}
	mustInsert(t, e, "t", []any{2, 2}) // head moves, touching read... nothing we read
	if err := tx.Commit(); err != nil {
		t.Fatalf("read-only serializable commit: %v", err)
	}
}

// Serializable read-tracking overhead on exactly the Stage 2
// heavy-transaction shape (500 seed rows, 400 point reads, one
// identity insert, parallel writers) — the only variable against
// BenchmarkIdentityInsertHeavyTxn/occ in seq_occ_test.go is
// WriteTxnSerializable vs WriteTxn.
func BenchmarkSerializableHeavyTxn(b *testing.B) {
	e, err := Open(filepath.Join(b.TempDir(), "bench.db"), WithConcurrentWrites(), WithSyncNever())
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close()
	if _, err := e.CreateTable("events", []Column{
		{Name: "id", Type: TInt, Identity: true},
		{Name: "note", Type: TString},
	}, "id"); err != nil {
		b.Fatal(err)
	}
	for range 500 {
		if err := e.Insert("events", nil, "seed"); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			err := e.WriteTxnSerializable(func(tx *Txn) error {
				for pk := int64(1); pk <= 400; pk++ {
					if _, _, err := tx.Get("events", pk); err != nil {
						return err
					}
				}
				return tx.Insert("events", nil, "bench")
			})
			// The seed rows are read-only, so reads never overlap the
			// identity-keyed inserts — conflicts only under unlucky
			// ring pressure; count them as workload, not failure.
			if err != nil && !errors.Is(err, btypedb.ErrTxConflict) {
				b.Error(err)
				return
			}
		}
	})
}
