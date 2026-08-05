package bytdb

// Regression: WriteTxn's doc promises the transaction is "rolled back on
// error or panic." The underlying btypedb Update rolls back on an error
// return but has no recover, so a panic in the closure used to unwind
// with the single-writer lock still held — wedging every future write
// process-wide (reads keep working, which hides it). WriteTxn now
// recovers, rolls back to release the lock, and re-panics.

import (
	"path/filepath"
	"testing"
	"time"
)

func TestWriteTxnPanicReleasesWriterLock(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "panic.db"))
	defer e.Close()
	if _, err := e.CreateTable("t", []Column{{Name: "id", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}

	// A closure that writes and then panics. The panic must propagate
	// (WriteTxn re-panics), which we catch here.
	panicked := func() (caught bool) {
		defer func() { caught = recover() != nil }()
		_ = e.WriteTxn(func(tx *Txn) error {
			if err := tx.Insert("t", int64(99)); err != nil {
				t.Fatalf("txn insert: %v", err)
			}
			panic("boom")
		})
		return
	}()
	if !panicked {
		t.Fatal("WriteTxn swallowed the panic instead of propagating it")
	}

	// The whole point: the next write must not block on a leaked lock.
	// Run it off-goroutine with a deadline so a leak fails the test
	// instead of hanging the suite forever.
	done := make(chan error, 1)
	go func() { done <- e.Insert("t", int64(1)) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("insert after panicked WriteTxn: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("insert blocked after a panicked WriteTxn — the writer lock leaked")
	}

	// And the panicked transaction's write must have been rolled back:
	// id=99 must not be present, id=1 must be.
	if _, ok, err := e.Get("t", int64(99)); err != nil || ok {
		t.Fatalf("row from panicked txn survived (ok=%v err=%v) — not rolled back", ok, err)
	}
	if _, ok, err := e.Get("t", int64(1)); err != nil || !ok {
		t.Fatalf("post-panic insert missing (ok=%v err=%v)", ok, err)
	}
}

// Regression: DDL closures can run caller-supplied callbacks (AddCheck's
// per-row validate, AlterSequence's mutate) inside the kv transaction.
// updateDDL used to pass them to kv.Update unguarded, so a panic there
// unwound with the writer lock held — the same process-wide write wedge
// WriteTxn already guards against.
func TestDDLPanicReleasesWriterLock(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "ddlpanic.db"))
	defer e.Close()
	if _, err := e.CreateTable("t", []Column{{Name: "id", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("t", int64(1)); err != nil {
		t.Fatal(err)
	}

	panicked := func() (caught bool) {
		defer func() { caught = recover() != nil }()
		_ = e.AddCheck("t", CheckDesc{Name: "ck", Expr: "id > 0"}, func(Row) error {
			panic("validate boom")
		})
		return
	}()
	if !panicked {
		t.Fatal("AddCheck swallowed the validate panic instead of propagating it")
	}

	// The next write must not block on a leaked writer lock.
	done := make(chan error, 1)
	go func() { done <- e.Insert("t", int64(2)) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("insert after panicked DDL: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("insert blocked after a panicked DDL — the writer lock leaked")
	}

	// And the half-added constraint must have been rolled back.
	desc := e.Table("t")
	if desc == nil {
		t.Fatal("table t missing after panicked DDL")
	}
	if len(desc.Checks) != 0 {
		t.Fatalf("check from panicked DDL survived: %+v", desc.Checks)
	}
}
