package bytdb

// memguard_test.go: the memory ceiling's admission decisions. The heap
// reading is faked through memGuard.sample — driving the real runtime
// over a ceiling would mean exhausting memory or setting a tiny
// GOMEMLIMIT, and either would make the whole test binary thrash.

import (
	"errors"
	"math"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHeap installs a fixed heap reading and returns a pointer the test
// can move. The cached verdict is dropped so the next check samples.
func fakeHeap(e *Engine, live, gomemlimit uint64) *atomic.Uint64 {
	var l atomic.Uint64
	l.Store(live)
	e.mem.sample = func() (uint64, uint64) { return l.Load(), gomemlimit }
	e.mem.verdict.Store(nil)
	return &l
}

// TestMemoryCeilingRefusesGrowth pins which operations the ceiling
// refuses (inserts, updates, index builds, backfills) and which it
// never does (deletes, bare ADD COLUMN, reads).
func TestMemoryCeilingRefusesGrowth(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	usersTable(t, e)
	for i := range 3 {
		if err := e.Insert("users", int64(i), "n", 1.0, true, nil); err != nil {
			t.Fatal(err)
		}
	}

	fakeHeap(e, 1000, 0)
	e.SetMemoryCeiling(500)

	isOOM := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrOutOfMemory) {
			t.Errorf("%s: err = %v, want ErrOutOfMemory", what, err)
		}
	}
	isOOM("insert", e.Insert("users", int64(10), "n", 1.0, true, nil))
	_, err := e.Update("users", []any{int64(1)}, map[string]any{"name": "x"})
	isOOM("update", err)
	_, err = e.CreateIndex("users", "users_name", false, "name")
	isOOM("create index", err)
	isOOM("add column with default", e.AddColumn("users", Column{Name: "d", Type: TInt, Default: "7"}))
	isOOM("txn insert", e.WriteTxn(func(tx *Txn) error {
		return tx.Insert("users", int64(11), "n", 1.0, true, nil)
	}))

	// Freeing and descriptor-only work stays available: this is how an
	// operator gets back under the ceiling.
	if ok, err := e.Delete("users", int64(0)); err != nil || !ok {
		t.Errorf("delete: ok=%v err=%v, want a successful delete", ok, err)
	}
	if err := e.AddColumn("users", Column{Name: "plain", Type: TInt}); err != nil {
		t.Errorf("bare add column: %v", err)
	}
	if _, ok, err := e.Get("users", int64(1)); err != nil || !ok {
		t.Errorf("get: ok=%v err=%v", ok, err)
	}

	// Raising the ceiling, or disabling the guard, lets growth through
	// immediately: SetMemoryCeiling drops the cached verdict.
	e.SetMemoryCeiling(2000)
	if err := e.Insert("users", int64(20), "n", 1.0, true, nil, nil); err != nil {
		t.Errorf("insert under a raised ceiling: %v", err)
	}
	e.SetMemoryCeiling(-1)
	fakeHeap(e, math.MaxUint64/2, 0)
	if err := e.Insert("users", int64(21), "n", 1.0, true, nil, nil); err != nil {
		t.Errorf("insert with the guard disabled: %v", err)
	}
}

// TestMemoryCeilingAuto pins the automatic mode: a share of GOMEMLIMIT
// when one is set, inactive when none is (the runtime's MaxInt64).
func TestMemoryCeilingAuto(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	usersTable(t, e)
	if e.MemoryCeiling() != 0 {
		t.Fatalf("default MemoryCeiling = %d, want 0 (automatic)", e.MemoryCeiling())
	}

	// 95 of a 100-byte limit is over the 90% automatic ceiling.
	fakeHeap(e, 95, 100)
	if err := e.CheckMemory(); !errors.Is(err, ErrOutOfMemory) {
		t.Errorf("95/100 live: err = %v, want ErrOutOfMemory", err)
	}
	fakeHeap(e, 85, 100)
	if err := e.CheckMemory(); err != nil {
		t.Errorf("85/100 live: err = %v, want nil", err)
	}
	// No GOMEMLIMIT: the automatic guard has nothing to derive from.
	fakeHeap(e, math.MaxUint64/2, math.MaxInt64)
	if err := e.CheckMemory(); err != nil {
		t.Errorf("no GOMEMLIMIT: err = %v, want nil (inactive)", err)
	}

	if got := effectiveCeiling(0, 1000); got != 900 {
		t.Errorf("effectiveCeiling(auto, 1000) = %d, want 900", got)
	}
	if got := effectiveCeiling(123, 1000); got != 123 {
		t.Errorf("effectiveCeiling(123, 1000) = %d, want the explicit 123", got)
	}
	if got := effectiveCeiling(-1, 1000); got != 0 {
		t.Errorf("effectiveCeiling(off, 1000) = %d, want 0", got)
	}
}

// TestMemoryCeilingRecovers checks a refusing guard notices the heap
// dropping once its cached reading expires — the path an operator's
// cleanup depends on.
func TestMemoryCeilingRecovers(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	e.SetMemoryCeiling(500)
	live := fakeHeap(e, 1000, 0)
	if err := e.CheckMemory(); !errors.Is(err, ErrOutOfMemory) {
		t.Fatalf("over: err = %v, want ErrOutOfMemory", err)
	}
	live.Store(100)
	deadline := time.Now().Add(time.Second)
	for e.CheckMemory() != nil {
		if time.Now().After(deadline) {
			t.Fatal("guard still refusing a second after the heap dropped")
		}
		time.Sleep(memSampleInterval)
	}
}

// TestStatsMemCeiling checks Stats reports the effective ceiling and
// the marked heap it is compared against.
func TestStatsMemCeiling(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	e.SetMemoryCeiling(12345)
	s := e.Stats()
	if s.MemCeiling != 12345 {
		t.Errorf("MemCeiling = %d, want 12345", s.MemCeiling)
	}
	if s.HeapMarked == 0 {
		// Any GC since process start sets it; the test binary has
		// allocated plenty by now, but force one to be certain.
		forceGC()
		if s = e.Stats(); s.HeapMarked == 0 {
			t.Errorf("HeapMarked = 0 after a GC")
		}
	}
	e.SetMemoryCeiling(-1)
	if s = e.Stats(); s.MemCeiling != 0 {
		t.Errorf("disabled: MemCeiling = %d, want 0", s.MemCeiling)
	}
}

// TestMaxResultRowsDefault pins the default cap and the knob's
// round-trip; enforcement is the SQL layer's and tested there.
func TestMaxResultRowsDefault(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	if e.MaxResultRows() != DefaultMaxResultRows {
		t.Errorf("default = %d, want %d", e.MaxResultRows(), DefaultMaxResultRows)
	}
	e.SetMaxResultRows(0)
	if e.MaxResultRows() != 0 {
		t.Errorf("after SetMaxResultRows(0) = %d", e.MaxResultRows())
	}
}

func forceGC() { runtime.GC() }
