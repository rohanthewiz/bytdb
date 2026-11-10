package bytdb

// Regression tests for the engine items of the deep-look remediation
// plan (ai_docs/todo/2026-1109-deep-look-remediation-plan.md). Each test
// names its plan item; the runnable originals were the audit repros in
// ai_docs/todo/deep-look-repros/engine-audit/.

import (
	"math"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// finishesWithin runs fn on its own goroutine and reports whether it
// returned within d. The repros for hangs and deadlocks use it so a
// regression is a test failure, never a stuck run. A hung fn's
// goroutine is leaked on purpose: there is nothing safe to cancel.
func finishesWithin(d time.Duration, fn func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// TestAdvanceSeqMatchesStepping (D-13) pins advanceSeq to the
// one-draw-at-a-time stepSeq it replaces, over random descriptors that
// cover both directions, bounds near the int64 limits, CYCLE, and a
// not-yet-called sequence.
func TestAdvanceSeqMatchesStepping(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	pick := func() int64 {
		switch rng.IntN(4) {
		case 0:
			return rng.Int64N(41) - 20
		case 1:
			return math.MaxInt64 - rng.Int64N(20)
		case 2:
			return math.MinInt64 + rng.Int64N(20)
		}
		return rng.Int64()
	}
	for i := 0; i < 20000; i++ {
		lo, hi := pick(), pick()
		if lo > hi {
			lo, hi = hi, lo
		}
		inc := rng.Int64N(7) - 3
		if rng.IntN(5) == 0 {
			inc = pick()
		}
		if inc == 0 {
			inc = 1
		}
		// Last uniformly in [lo, hi]; the span is computed in uint64
		// because hi-lo can exceed MaxInt64.
		last := int64(rng.Uint64())
		if span := uint64(hi) - uint64(lo); span < math.MaxUint64 {
			last = int64(uint64(lo) + rng.Uint64N(span+1))
		}
		d := SeqDesc{Min: lo, Max: hi, Increment: inc, Last: last,
			Cycle: rng.IntN(2) == 0, Called: rng.IntN(4) != 0}
		n := rng.Int64N(50) + 1

		stepped := d
		var want int64
		var wantErr error
		for want < n {
			if _, err := stepSeq(&stepped, "s"); err != nil {
				wantErr = err
				break
			}
			want++
		}
		jumped := d
		got, gotErr := advanceSeq(&jumped, "s", n)
		if got != want || (gotErr == nil) != (wantErr == nil) || jumped != stepped {
			t.Fatalf("desc %+v n=%d: advance=(%d, %v, %+v), step=(%d, %v, %+v)",
				d, n, got, gotErr, jumped, want, wantErr, stepped)
		}
	}
}

// TestSeqHugeCacheDoesNotSpin (D-13): under concurrent writes the
// first nextval reserved its CACHE batch by stepping the sequence one
// draw at a time while holding the allocator mutex, so CACHE 2^50 spun
// at 100% CPU forever. CREATE now caps CACHE, and a descriptor stored
// with a larger one (before the cap) reserves in O(1).
func TestSeqHugeCacheDoesNotSpin(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "db"))
	defer e.Close()

	err := e.CreateSequence(&SeqDesc{Name: "s_huge", Start: 1, Increment: 1, Min: 1, Max: math.MaxInt64, Cache: 1 << 50})
	if err == nil || !strings.Contains(err.Error(), "CACHE") {
		t.Fatalf("CREATE with CACHE 2^50: err = %v, want the CACHE cap", err)
	}

	// Simulate a descriptor written before the cap: create at the cap,
	// then raise its stored CACHE behind the validation.
	if err := e.CreateSequence(&SeqDesc{Name: "s", Start: 1, Increment: 1, Min: 1, Max: math.MaxInt64, Cache: MaxSeqCache}); err != nil {
		t.Fatal(err)
	}
	if err := e.WriteTxn(func(tx *Txn) error {
		d, err := seqDescIn(tx.tx, "s")
		if err != nil {
			return err
		}
		d.Cache = 1 << 50
		return writeSeqDescIn(tx.tx, d)
	}); err != nil {
		t.Fatal(err)
	}
	var v1, v2 int64
	ok := finishesWithin(5*time.Second, func() {
		_ = e.WriteTxn(func(tx *Txn) error {
			var err error
			if v1, err = tx.NextVal("s"); err != nil {
				return err
			}
			v2, err = tx.NextVal("s")
			return err
		})
	})
	if !ok {
		t.Fatal("nextval on a sequence with CACHE 2^50 did not return within 5s")
	}
	if v1 != 1 || v2 != 2 {
		t.Fatalf("draws = %d, %d; want 1, 2", v1, v2)
	}

	// A tiny cycling range under the maximum cache walks one cycle per
	// jump; it must still answer promptly and keep cycling correctly.
	if err := e.CreateSequence(&SeqDesc{Name: "c", Start: 1, Increment: 1, Min: 1, Max: 3, Cycle: true, Cache: MaxSeqCache}); err != nil {
		t.Fatal(err)
	}
	var got []int64
	ok = finishesWithin(5*time.Second, func() {
		_ = e.WriteTxn(func(tx *Txn) error {
			for range 5 {
				v, err := tx.NextVal("c")
				if err != nil {
					return err
				}
				got = append(got, v)
			}
			return nil
		})
	})
	if !ok {
		t.Fatal("nextval on a cycling sequence with the maximum CACHE did not return within 5s")
	}
	if want := []int64{1, 2, 3, 1, 2}; !equalInt64s(got, want) {
		t.Fatalf("cycling draws = %v, want %v", got, want)
	}
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSerializableFailedProbeIsARead (D-11): a failed duplicate or
// unique probe leaves the transaction committable, so "this key is
// taken" is a read. Two SERIALIZABLE transactions each learn of the
// other's row through a failed insert (or update), then delete it. No
// serial order produces "both rows gone" — whichever ran second would
// have seen its probe succeed — so one of them must fail to commit.
func TestSerializableFailedProbeIsARead(t *testing.T) {
	setup := func(t *testing.T) *Engine {
		e := openOCCEngine(t, filepath.Join(t.TempDir(), "db"))
		t.Cleanup(func() { e.Close() })
		for _, n := range []string{"a", "b"} {
			if _, err := e.CreateTable(n, []Column{{Name: "id", Type: TInt}, {Name: "email", Type: TString}}, "id"); err != nil {
				t.Fatal(err)
			}
			if _, err := e.CreateIndex(n, n+"_email", true, "email"); err != nil {
				t.Fatal(err)
			}
			if err := e.Insert(n, 1, n+"@x"); err != nil {
				t.Fatal(err)
			}
		}
		return e
	}
	// Each probe makes tx learn that table `seen`'s row 1 exists, by a
	// failing write against `seen`; the transaction then deletes the
	// other table's row.
	probes := map[string]func(tx *Txn, seen string) error{
		"insert duplicate primary key": func(tx *Txn, seen string) error {
			return tx.Insert(seen, 1, "fresh@x")
		},
		"insert unique violation": func(tx *Txn, seen string) error {
			return tx.Insert(seen, 2, seen+"@x")
		},
		"update onto an existing key": func(tx *Txn, seen string) error {
			if err := tx.Insert(seen, 3, "three@x"); err != nil {
				return nil // setup inside the txn failed; report "no error" so the test fails loudly
			}
			_, err := tx.Update(seen, []any{3}, map[string]any{"id": 1})
			return err
		},
		"update unique violation": func(tx *Txn, seen string) error {
			if err := tx.Insert(seen, 3, "three@x"); err != nil {
				return nil
			}
			_, err := tx.Update(seen, []any{3}, map[string]any{"email": seen + "@x"})
			return err
		},
	}
	for name, probe := range probes {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			t1, err := e.BeginSerializable()
			if err != nil {
				t.Fatal(err)
			}
			t2, err := e.BeginSerializable()
			if err != nil {
				t.Fatal(err)
			}
			if err := probe(t1, "a"); err == nil {
				t.Fatal("t1's probe unexpectedly succeeded")
			}
			if _, err := t1.Delete("b", 1); err != nil {
				t.Fatal(err)
			}
			if err := probe(t2, "b"); err == nil {
				t.Fatal("t2's probe unexpectedly succeeded")
			}
			if _, err := t2.Delete("a", 1); err != nil {
				t.Fatal(err)
			}
			err1, err2 := t1.Commit(), t2.Commit()
			if err1 == nil && err2 == nil {
				t.Fatal("write skew under SERIALIZABLE: both transactions committed")
			}
		})
	}
}
