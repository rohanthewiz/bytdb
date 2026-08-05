package bytdb

import (
	"encoding/binary"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rohanthewiz/btypedb"
)

// seq_occ_test.go: Stage 2 of the concurrent-writes plan — sequences
// and identity leave the OCC conflict scope. These tests open engines
// with WithConcurrentWrites and pin the non-transactional, Postgres-
// style allocation semantics: concurrent draws never conflict, draws
// survive rollback (gaps, not reuse), watermarks make crash recovery
// resume past everything possibly handed out, and DDL/TRUNCATE
// invalidate the in-memory caches.

func openOCCEngine(t *testing.T, path string) *Engine {
	t.Helper()
	e, err := Open(path, WithConcurrentWrites(), WithSyncNever())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func occIDTable(t *testing.T, e *Engine, name string) {
	t.Helper()
	_, err := e.CreateTable(name, []Column{
		{Name: "id", Type: TInt, Identity: true},
		{Name: "note", Type: TString},
	}, "id")
	if err != nil {
		t.Fatal(err)
	}
}

// The headline property: concurrent inserts into an identity table
// must not conflict on the counter — under transactional counters
// every pair would. Identity is the PK, so distinct draws also mean
// disjoint row keys: no transaction should see ErrTxConflict at all.
func TestOCCConcurrentIdentityInserts(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	occIDTable(t, e, "events")

	const writers, perWriter = 8, 50
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWriter {
				err := e.WriteTxn(func(tx *Txn) error {
					return tx.Insert("events", nil, "w")
				})
				if err != nil {
					errs <- err
					return
				}
			}
			_ = w
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if errors.Is(err, btypedb.ErrTxConflict) {
			t.Fatalf("identity insert conflicted — counter still in the write set: %v", err)
		}
		t.Fatal(err)
	}

	seen := map[int64]bool{}
	rows := collect(t, e.Scan("events"))
	for _, r := range rows {
		id := r.Col("id").(int64)
		if seen[id] {
			t.Fatalf("duplicate identity value %d", id)
		}
		seen[id] = true
	}
	if len(rows) != writers*perWriter {
		t.Fatalf("rows = %d, want %d", len(rows), writers*perWriter)
	}
}

// Reopening must resume past the watermark: values handed out before a
// crash (simulated by Close/Open) can never be handed out again, at
// the price of a bounded gap.
func TestOCCSeqWatermarkReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	e := openOCCEngine(t, path)

	var last uint64
	for range 5 {
		v, err := e.NextSeq("s")
		if err != nil {
			t.Fatal(err)
		}
		last = v
	}
	if last != 5 {
		t.Fatalf("last draw = %d, want 5", last)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2 := openOCCEngine(t, path)
	defer e2.Close()
	v, err := e2.NextSeq("s")
	if err != nil {
		t.Fatal(err)
	}
	// The stored watermark was 1+seqAllocBatch: draws resume there. The
	// exact value is an implementation detail; the guarantees are no
	// reuse and a bounded gap.
	if v <= last {
		t.Fatalf("draw after reopen = %d, must exceed %d", v, last)
	}
	if v > last+seqAllocBatch {
		t.Fatalf("draw after reopen = %d, gap exceeds one batch (%d)", v, seqAllocBatch)
	}
}

// Draws are non-transactional: a rolled-back transaction burns its
// values (Postgres sequence semantics).
func TestOCCSeqRollbackBurns(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()

	boom := errors.New("boom")
	var drawn uint64
	err := e.WriteTxn(func(tx *Txn) error {
		var err error
		drawn, err = tx.NextSeq("s")
		if err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	v, err := e.NextSeq("s")
	if err != nil {
		t.Fatal(err)
	}
	if v <= drawn {
		t.Fatalf("draw after rollback = %d, want > %d (burned, not reused)", v, drawn)
	}
}

// SetSeq through a transaction takes effect immediately in OCC mode
// and survives the rollback, like Postgres setval.
func TestOCCSetSeqImmediate(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()

	boom := errors.New("boom")
	err := e.WriteTxn(func(tx *Txn) error {
		if err := tx.SetSeq("s", 100); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	v, err := e.NextSeq("s")
	if err != nil {
		t.Fatal(err)
	}
	if v != 100 {
		t.Fatalf("draw after rolled-back SetSeq = %d, want 100 (setval is not transactional)", v)
	}
}

// PeekSeq must report the next draw exactly, even while the allocator
// holds an unhanded cache (the stored watermark runs ahead).
func TestOCCPeekSeq(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()

	if _, ok, err := e.PeekSeq("s"); err != nil || ok {
		t.Fatalf("peek of absent sequence = ok %v err %v, want !ok nil", ok, err)
	}
	for i := uint64(1); i <= 3; i++ {
		v, err := e.NextSeq("s")
		if err != nil {
			t.Fatal(err)
		}
		if v != i {
			t.Fatalf("draw = %d, want %d", v, i)
		}
		next, ok, err := e.PeekSeq("s")
		if err != nil || !ok {
			t.Fatalf("peek = ok %v err %v", ok, err)
		}
		if next != i+1 {
			t.Fatalf("peek = %d, want %d", next, i+1)
		}
	}
}

// DeleteSeq drops allocator state with the stored key: a later draw
// recreates the sequence at 1.
func TestOCCDeleteSeqRestartsAtOne(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()

	for range 4 {
		if _, err := e.NextSeq("s"); err != nil {
			t.Fatal(err)
		}
	}
	existed, err := e.DeleteSeq("s")
	if err != nil || !existed {
		t.Fatalf("delete = existed %v err %v", existed, err)
	}
	v, err := e.NextSeq("s")
	if err != nil {
		t.Fatal(err)
	}
	if v != 1 {
		t.Fatalf("draw after delete = %d, want 1", v)
	}
}

// An explicit identity value bumps the counter past itself so later
// NULL draws cannot collide — and the bump, being non-transactional,
// survives the inserting transaction's rollback.
func TestOCCIdentityExplicitBump(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	occIDTable(t, e, "events")

	boom := errors.New("boom")
	err := e.WriteTxn(func(tx *Txn) error {
		if err := tx.Insert("events", 100, "explicit"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	row, err := e.InsertReturning("events", nil, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if id := row.Col("id").(int64); id <= 100 {
		t.Fatalf("draw after rolled-back explicit insert = %d, want > 100", id)
	}
}

// Sequence objects: concurrent nextval with CACHE batches must hand
// out globally distinct values, one durable write per batch.
func TestOCCNextValConcurrent(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	if err := e.CreateSequence(&SeqDesc{
		Name: "counter", Start: 1, Increment: 1,
		Min: 1, Max: 1 << 40, Cache: 5,
	}); err != nil {
		t.Fatal(err)
	}

	const workers, perWorker = 6, 40
	var mu sync.Mutex
	seen := map[int64]bool{}
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				var v int64
				err := e.WriteTxn(func(tx *Txn) error {
					var err error
					v, err = tx.NextVal("counter")
					return err
				})
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				if seen[v] {
					mu.Unlock()
					errs <- errors.New("duplicate nextval value")
					return
				}
				seen[v] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(seen) != workers*perWorker {
		t.Fatalf("distinct values = %d, want %d", len(seen), workers*perWorker)
	}
}

// Bound and cycle behavior must survive the move to batched
// reservations: a non-cycling sequence still errors with Postgres'
// wording at its bound (even when the bound falls mid-batch), a
// cycling one wraps.
func TestOCCNextValBoundsAndCycle(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()

	// CACHE 10 with MAX 3: the first reservation must clip to the 3
	// existing draws, then error.
	if err := e.CreateSequence(&SeqDesc{
		Name: "bounded", Start: 1, Increment: 1, Min: 1, Max: 3, Cache: 10,
	}); err != nil {
		t.Fatal(err)
	}
	for want := int64(1); want <= 3; want++ {
		var v int64
		err := e.WriteTxn(func(tx *Txn) error {
			var err error
			v, err = tx.NextVal("bounded")
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if v != want {
			t.Fatalf("draw = %d, want %d", v, want)
		}
	}
	err := e.WriteTxn(func(tx *Txn) error {
		_, err := tx.NextVal("bounded")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "reached maximum value") {
		t.Fatalf("draw past bound = %v, want Postgres bound error", err)
	}

	if err := e.CreateSequence(&SeqDesc{
		Name: "wheel", Start: 1, Increment: 1, Min: 1, Max: 2, Cycle: true, Cache: 3,
	}); err != nil {
		t.Fatal(err)
	}
	want := []int64{1, 2, 1, 2, 1}
	for i, w := range want {
		var v int64
		err := e.WriteTxn(func(tx *Txn) error {
			var err error
			v, err = tx.NextVal("wheel")
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if v != w {
			t.Fatalf("cycle draw %d = %d, want %d", i, v, w)
		}
	}
}

// setval repositions immediately, discarding the allocator's cache,
// and survives rollback.
func TestOCCSetValImmediate(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	if err := e.CreateSequence(&SeqDesc{
		Name: "counter", Start: 1, Increment: 1, Min: 1, Max: 1 << 40, Cache: 8,
	}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	err := e.WriteTxn(func(tx *Txn) error {
		if _, err := tx.NextVal("counter"); err != nil { // load a cache
			return err
		}
		if err := tx.SetVal("counter", 500, true); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	var v int64
	if err := e.WriteTxn(func(tx *Txn) error {
		var err error
		v, err = tx.NextVal("counter")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if v != 501 {
		t.Fatalf("draw after rolled-back setval = %d, want 501", v)
	}
}

// ALTER SEQUENCE (RESTART-style state edits) must apply from the very
// next draw, not after a stale cache drains.
func TestOCCAlterSequenceDropsCache(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	if err := e.CreateSequence(&SeqDesc{
		Name: "counter", Start: 1, Increment: 1, Min: 1, Max: 1 << 40, Cache: 8,
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.WriteTxn(func(tx *Txn) error {
		_, err := tx.NextVal("counter") // cache 8 draws, hand out 1
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.AlterSequence("counter", func(d *SeqDesc) error {
		d.Last, d.Called = 1000, true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var v int64
	if err := e.WriteTxn(func(tx *Txn) error {
		var err error
		v, err = tx.NextVal("counter")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if v != 1001 {
		t.Fatalf("draw after restart = %d, want 1001 (stale cache served)", v)
	}
}

// TRUNCATE ... RESTART IDENTITY: committed, the next insert draws 1
// again; rolled back, the counter keeps counting.
func TestOCCTruncateRestartIdentity(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	occIDTable(t, e, "events")
	for range 3 {
		if err := e.Insert("events", nil, "x"); err != nil {
			t.Fatal(err)
		}
	}

	// Rolled back: allocator cache must survive untouched.
	boom := errors.New("boom")
	err := e.WriteTxn(func(tx *Txn) error {
		if err := tx.Truncate("events", true); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	row, err := e.InsertReturning("events", nil, "after-rollback")
	if err != nil {
		t.Fatal(err)
	}
	if id := row.Col("id").(int64); id != 4 {
		t.Fatalf("draw after rolled-back truncate = %d, want 4", id)
	}

	// Committed: counter restarts at 1.
	if err := e.WriteTxn(func(tx *Txn) error {
		return tx.Truncate("events", true)
	}); err != nil {
		t.Fatal(err)
	}
	row, err = e.InsertReturning("events", nil, "after-truncate")
	if err != nil {
		t.Fatal(err)
	}
	if id := row.Col("id").(int64); id != 1 {
		t.Fatalf("draw after committed truncate = %d, want 1 (allocator cache not invalidated)", id)
	}
}

// DROP TABLE then re-CREATE with the same name: the new table's
// identity must start fresh — its counters are new keys (new table
// ID), but a stale allocator writing old keys back would leak orphans.
func TestOCCDropTableIdentityFresh(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	occIDTable(t, e, "events")
	for range 3 {
		if err := e.Insert("events", nil, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.DropTable("events"); err != nil {
		t.Fatal(err)
	}
	occIDTable(t, e, "events")
	row, err := e.InsertReturning("events", nil, "fresh")
	if err != nil {
		t.Fatal(err)
	}
	if id := row.Col("id").(int64); id != 1 {
		t.Fatalf("draw in recreated table = %d, want 1", id)
	}
}

// Sequence ops on a read-only transaction must refuse with
// ErrTxNotWritable in OCC mode too — routing around the transaction
// must not route around its read-only guard.
func TestOCCSeqReadOnlyGuard(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	err := e.ReadTxn(func(tx *Txn) error {
		_, err := tx.NextSeq("s")
		return err
	})
	if !errors.Is(err, btypedb.ErrTxNotWritable) {
		t.Fatalf("NextSeq in ReadTxn = %v, want ErrTxNotWritable", err)
	}
	if err := e.ReadTxn(func(tx *Txn) error {
		return tx.SetSeq("s", 5)
	}); !errors.Is(err, btypedb.ErrTxNotWritable) {
		t.Fatal("SetSeq in ReadTxn should refuse")
	}
}

// A concurrent mix: writers hammer identity inserts while another
// goroutine truncates with restart. Nothing may violate PK uniqueness
// at any point, whatever the interleaving — inserts may lose to the
// truncate's range claim, or draw a value the restarted counter
// re-issues to someone else; both surface as ErrTxConflict and retry.
func TestOCCTruncateInsertRace(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	occIDTable(t, e, "events")

	const writers = 4
	var wg sync.WaitGroup
	errs := make(chan error, writers+1)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 30 {
				err := e.WriteTxn(func(tx *Txn) error {
					return tx.Insert("events", nil, "w")
				})
				if err != nil && !errors.Is(err, btypedb.ErrTxConflict) {
					// Duplicate PK here would mean a counter reset raced
					// wrongly; conflicts are the expected loss mode.
					errs <- err
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 10 {
			err := e.WriteTxn(func(tx *Txn) error {
				return tx.Truncate("events", true)
			})
			if err != nil && !errors.Is(err, btypedb.ErrTxConflict) {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// Whatever survived, ids must be unique.
	seen := map[int64]bool{}
	for _, r := range collect(t, e.Scan("events")) {
		id := r.Col("id").(int64)
		if seen[id] {
			t.Fatalf("duplicate identity value %d after truncate race", id)
		}
		seen[id] = true
	}
}

// BenchmarkIdentityInsertParallel is Stage 2's reason to exist,
// measured: parallel single-row inserts into an identity table. In the
// default mode they serialize on the writer lock; in OCC mode (without
// this file's allocators) they would all conflict on the counter key;
// with them they overlap cleanly. Run with -cpu to vary writers.
func BenchmarkIdentityInsertParallel(b *testing.B) {
	for _, mode := range []struct {
		name string
		opts []btypedb.Option
	}{
		{"serialized", []btypedb.Option{WithSyncNever()}},
		{"occ", []btypedb.Option{WithSyncNever(), WithConcurrentWrites()}},
	} {
		b.Run(mode.name, func(b *testing.B) {
			e, err := Open(filepath.Join(b.TempDir(), "bench.db"), mode.opts...)
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
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					err := e.WriteTxn(func(tx *Txn) error {
						return tx.Insert("events", nil, "bench")
					})
					if err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}

// BenchmarkIdentityInsertHeavyTxn adds per-transaction read work (the
// shape of FK/unique-check-heavy SQL writes). Serialized mode does all
// of it under the writer lock; OCC overlaps it, which is where the
// mode wins.
func BenchmarkIdentityInsertHeavyTxn(b *testing.B) {
	for _, mode := range []struct {
		name string
		opts []btypedb.Option
	}{
		{"serialized", []btypedb.Option{WithSyncNever()}},
		{"occ", []btypedb.Option{WithSyncNever(), WithConcurrentWrites()}},
	} {
		b.Run(mode.name, func(b *testing.B) {
			e, err := Open(filepath.Join(b.TempDir(), "bench.db"), mode.opts...)
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
					err := e.WriteTxn(func(tx *Txn) error {
						for pk := int64(1); pk <= 400; pk++ {
							if _, _, err := tx.Get("events", pk); err != nil {
								return err
							}
						}
						return tx.Insert("events", nil, "bench")
					})
					if err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}

// Regression: a watermark extension used to take max(stale in-memory
// position, stored value), so a SetSeq that committed a BACKWARD move
// mid-extension (its post-commit invalidation blocked on the allocator
// mutex the extension holds) was durably overwritten by the stale
// position — the committed set had zero effect, an outcome no serial
// order of the two operations produces. The extension must instead
// treat stored < anchored-watermark as an external reset and re-anchor
// on the stored value.
func TestOCCBackwardSetDuringExtensionReanchors(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()

	// Exhaust the first reserved batch so the next draw must extend.
	for i := uint64(1); i <= seqAllocBatch; i++ {
		v, err := e.NextSeq("s")
		if err != nil {
			t.Fatal(err)
		}
		if v != i {
			t.Fatalf("draw = %d, want %d", v, i)
		}
	}

	// Land the durable backward write exactly as a concurrent SetSeq
	// does, WITHOUT the allocator invalidation that sequential SetSeq
	// would run afterward — in the real interleaving that invalidation
	// has not happened yet when the extension's transaction reads.
	err := e.seqPut(func(tx *btypedb.Tx[string, []byte]) error {
		return tx.Set(seqNameKey("s"), binary.BigEndian.AppendUint64(nil, 5))
	})
	if err != nil {
		t.Fatal(err)
	}

	// The extension must observe the backward move and re-anchor at 5,
	// not resurrect its stale position (which would draw seqAllocBatch+1).
	v, err := e.NextSeq("s")
	if err != nil {
		t.Fatal(err)
	}
	if v != 5 {
		t.Fatalf("draw after backward set = %d, want 5 (stale watermark resurrected the old position)", v)
	}
}
