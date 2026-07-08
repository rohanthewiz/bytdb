package bytdb

// snapshot_test.go: catalog/data snapshot consistency. A reader must
// never hold a descriptor advertising an index whose backfill its
// data snapshot predates (ScanIndex over the empty range would
// silently drop every row), nor one still naming an index whose
// entries the snapshot has already lost to DropIndex. With the
// catalog in the kv keyspace the invariant holds by construction —
// descriptors resolve from the same snapshot as the data — but these
// tests, written against an earlier two-snapshot design, stay as the
// regression guard for whatever scheme provides it; run under -race.

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// snapshotEngine builds a table with enough rows that an index scan
// missing them all is unambiguous.
func snapshotEngine(t *testing.T) (*Engine, int) {
	t.Helper()
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	t.Cleanup(func() { e.Close() })
	if _, err := e.CreateTable("t", []Column{
		{Name: "id", Type: TInt}, {Name: "v", Type: TInt},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	const rows = 50
	for i := range rows {
		if err := e.Insert("t", i, i*10); err != nil {
			t.Fatal(err)
		}
	}
	return e, rows
}

// countIndex counts the rows an open transaction's index scan yields.
func countIndex(t *testing.T, tx *Txn) (int, error) {
	t.Helper()
	n := 0
	for _, err := range tx.ScanIndex("t", "v_idx", nil, nil) {
		if err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// TestReadTxnSnapshotConsistency races read transactions against a
// CreateIndex/DropIndex churn loop. The invariant: whenever a read
// transaction's own catalog shows the index, scanning it yields every
// row — all-or-nothing, never a torn view.
func TestReadTxnSnapshotConsistency(t *testing.T) {
	e, rows := snapshotEngine(t)

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // DDL churn: the commit window under attack
		defer wg.Done()
		for !stop.Load() {
			if _, err := e.CreateIndex("t", "v_idx", false, "v"); err != nil {
				t.Error(err)
				return
			}
			if err := e.DropIndex("t", "v_idx"); err != nil {
				t.Error(err)
				return
			}
		}
	}()

	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 500 {
				err := e.ReadTxn(func(tx *Txn) error {
					if tx.Table("t").Index("v_idx") == nil {
						return nil // snapshot predates the index; nothing to check
					}
					n, err := countIndex(t, tx)
					if err != nil {
						return err
					}
					if n != rows {
						t.Errorf("read txn sees the index but only %d/%d rows through it", n, rows)
					}
					return nil
				})
				if err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	// Readers finish first, so the churn loop covers their whole run.
	readers.Wait()
	stop.Store(true)
	wg.Wait()
}

// TestScanIndexSnapshotConsistency covers the one-shot Engine.ScanIndex
// path, which resolves its descriptor and opens its snapshot on its
// own: it must either error "no such index" or return the full table.
func TestScanIndexSnapshotConsistency(t *testing.T) {
	e, rows := snapshotEngine(t)

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			if _, err := e.CreateIndex("t", "v_idx", false, "v"); err != nil {
				t.Error(err)
				return
			}
			if err := e.DropIndex("t", "v_idx"); err != nil {
				t.Error(err)
				return
			}
		}
	}()

	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 500 {
				n := 0
				sawErr := false
				for _, err := range e.ScanIndex("t", "v_idx", nil, nil) {
					if err != nil {
						sawErr = true // "no such index": a valid outcome
						break
					}
					n++
				}
				if !sawErr && n != rows {
					t.Errorf("ScanIndex returned %d/%d rows with no error", n, rows)
				}
			}
		}()
	}
	readers.Wait()
	stop.Store(true)
	wg.Wait()
}
