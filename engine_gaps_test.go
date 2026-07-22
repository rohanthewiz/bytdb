package bytdb

// Coverage for two engine-API entry points the rest of the suite reaches
// only indirectly: the WithSyncNever WAL-policy option, and the
// transaction-scoped Sequence resolver (as opposed to Engine.Sequence,
// which reads committed state).

import (
	"math"
	"path/filepath"
	"testing"
)

func TestWithSyncNeverEngineOperates(t *testing.T) {
	// WithSyncNever leaves WAL fsyncs to the OS — faster, with a
	// power-loss durability trade-off — but the engine must otherwise
	// open and read/write exactly as usual.
	e, err := Open(filepath.Join(t.TempDir(), "sync.db"), WithSyncNever())
	if err != nil {
		t.Fatalf("Open(WithSyncNever): %v", err)
	}
	defer e.Close()

	if _, err := e.CreateTable("t", []Column{{Name: "id", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("t", int64(7)); err != nil {
		t.Fatalf("insert under SyncNever: %v", err)
	}
	if _, ok, err := e.Get("t", int64(7)); err != nil || !ok {
		t.Fatalf("readback under SyncNever: ok=%v err=%v", ok, err)
	}
}

func TestTxnSequenceResolvesFromSnapshot(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "seq.db"))
	defer e.Close()

	if err := e.CreateSequence(&SeqDesc{
		Name: "s", Start: 1, Increment: 1, Min: 1, Max: math.MaxInt64, Cache: 1,
	}); err != nil {
		t.Fatalf("CreateSequence: %v", err)
	}

	// Txn.Sequence reads the transaction's snapshot; Engine.Sequence
	// reads committed state. Both must find the sequence and agree it is
	// absent for an unknown name.
	if err := e.ReadTxn(func(tx *Txn) error {
		if d := tx.Sequence("s"); d == nil || d.Name != "s" {
			t.Fatalf("Txn.Sequence(s) = %v, want a descriptor named s", d)
		}
		if d := tx.Sequence("missing"); d != nil {
			t.Fatalf("Txn.Sequence(missing) = %v, want nil", d)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if d := e.Sequence("s"); d == nil || d.Name != "s" {
		t.Fatalf("Engine.Sequence(s) = %v, want a descriptor named s", d)
	}
}
