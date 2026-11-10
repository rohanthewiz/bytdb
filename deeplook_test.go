package bytdb

// Regression tests for the engine items of the deep-look remediation
// plan (ai_docs/todo/2026-1109-deep-look-remediation-plan.md). Each test
// names its plan item; the runnable originals were the audit repros in
// ai_docs/todo/deep-look-repros/engine-audit/.

import (
	"errors"
	"fmt"
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

// TestDDLCallbackWriteIsRefused (D-12): updateDDL never marked its
// goroutine, so a DDL callback that issued an engine write waited on
// the writer lock its own DDL held — forever, in default mode; under
// concurrent writes the nested commit forced the UpdateExclusive
// escalation, which then waited on the nested commit. The write must
// come back as an error, promptly, and the DDL must fail with it.
func TestDDLCallbackWriteIsRefused(t *testing.T) {
	for _, occ := range []bool{false, true} {
		name := "default"
		if occ {
			name = "occ"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			var e *Engine
			if occ {
				e = openOCCEngine(t, path)
			} else {
				e = openEngine(t, path)
			}
			defer e.Close()
			if _, err := e.CreateTable("t", []Column{{Name: "id", Type: TInt}, {Name: "v", Type: TString}}, "id"); err != nil {
				t.Fatal(err)
			}
			for i := range 50 {
				if err := e.Insert("t", i, "x"); err != nil {
					t.Fatal(err)
				}
			}
			// Each callback runs inside a DDL transaction and writes
			// through the engine; each must be refused.
			cases := map[string]func() error{
				"AddCheck validate → NextSeq": func() error {
					var inner error
					err := e.AddCheck("t", CheckDesc{Name: "c", Expr: "v <> ''"}, func(Row) error {
						_, inner = e.NextSeq("audit")
						return inner
					})
					if inner == nil {
						return nil
					}
					return err
				},
				"AlterColumnType Using → NextSeq": func() error {
					return e.AlterColumnType("t", "v", ColumnTypeChange{
						Type: TInt,
						Using: func(Row) (any, error) {
							v, err := e.NextSeq("renumber")
							return int64(v), err
						},
					})
				},
				"AddCheck validate → Insert": func() error {
					return e.AddCheck("t", CheckDesc{Name: "c2", Expr: "v <> ''"}, func(Row) error {
						return e.Insert("t", 1000, "y")
					})
				},
				"AddCheck validate → WriteTxn": func() error {
					return e.AddCheck("t", CheckDesc{Name: "c3", Expr: "v <> ''"}, func(Row) error {
						return e.WriteTxn(func(*Txn) error { return nil })
					})
				},
			}
			for what, run := range cases {
				var err error
				if !finishesWithin(5*time.Second, func() { err = run() }) {
					t.Fatalf("%s: DDL hung (callback write deadlocked)", what)
				}
				if err == nil || !strings.Contains(err.Error(), "would deadlock") {
					t.Fatalf("%s: err = %v, want the reentrant-write refusal", what, err)
				}
			}
			// The guard must not outlive the DDL: ordinary writes from
			// this goroutine work afterwards, in both modes.
			if err := e.Insert("t", 2000, "z"); err != nil {
				t.Fatalf("insert after the refused DDLs: %v", err)
			}
			if _, err := e.NextSeq("after"); err != nil {
				t.Fatalf("NextSeq after the refused DDLs: %v", err)
			}
			if d := e.Table("t"); len(d.Checks) != 0 || d.Columns[1].Type != TString {
				t.Fatalf("a refused DDL left schema behind: %+v", d)
			}
		})
	}
}

// TestClosedTxnRefusesEverything (D-19): a finished transaction used to
// answer reads as misses — btypedb's Get on a done transaction reports
// "absent" — so Get, Scan, Update and Delete said "no such row" with a
// nil error for rows that exist. Every method now returns ErrTxClosed.
func TestClosedTxnRefusesEverything(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "db"))
	defer e.Close()
	if _, err := e.CreateTable("t", []Column{{Name: "id", Type: TInt}, {Name: "v", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("t", 1, 1); err != nil {
		t.Fatal(err)
	}
	var leaked *Txn
	if err := e.ReadTxn(func(tx *Txn) error { leaked = tx; return nil }); err != nil {
		t.Fatal(err)
	}
	committed, err := e.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	if err := committed.Commit(); err != nil {
		t.Fatal(err)
	}
	rolled, err := e.Begin(false)
	if err != nil {
		t.Fatal(err)
	}
	rolled.Rollback()
	var fromWriteTxn *Txn
	if err := e.WriteTxn(func(tx *Txn) error { fromWriteTxn = tx; return nil }); err != nil {
		t.Fatal(err)
	}

	for name, tx := range map[string]*Txn{"ReadTxn": leaked, "Commit": committed, "Rollback": rolled, "WriteTxn": fromWriteTxn} {
		check := func(op string, err error) {
			t.Helper()
			if !errors.Is(err, ErrTxClosed) {
				t.Errorf("%s: %s on a finished Txn: err = %v, want ErrTxClosed", name, op, err)
			}
		}
		_, _, err := tx.Get("t", 1)
		check("Get", err)
		var scanErr error
		for _, err := range tx.Scan("t") {
			scanErr = err
			break
		}
		check("Scan", scanErr)
		_, err = tx.Update("t", []any{1}, map[string]any{"v": 2})
		check("Update", err)
		_, err = tx.Delete("t", 1)
		check("Delete", err)
		check("Insert", tx.Insert("t", 2, 2))
		_, err = tx.Savepoint()
		check("Savepoint", err)
		_, err = tx.ReferencingFKs("t")
		check("ReferencingFKs", err)
		if tx.Table("t") != nil {
			t.Errorf("%s: Table on a finished Txn returned a descriptor", name)
		}
	}
}

// TestDescriptorAccessorsReturnCopies (D-19): Engine.Table, Txn.Table
// and ReferencingFKs handed out the parse cache's shared descriptor, so
// a caller that modified "its" copy rewrote the schema every
// transaction reads, and the next DDL persisted it. Row.Desc stays
// shared (copying per row is too costly), so DDL now builds on the
// stored bytes and a mutation through it cannot be persisted either.
func TestDescriptorAccessorsReturnCopies(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "db"))
	defer e.Close()
	if _, err := e.CreateTable("p", []Column{{Name: "id", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTable("t", []Column{{Name: "id", Type: TInt}, {Name: "v", Type: TString}, {Name: "pid", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateIndex("t", "t_v", false, "v"); err != nil {
		t.Fatal(err)
	}
	if err := e.AddForeignKey("t", FKDesc{Name: "t_p", Cols: []int{2}, RefTable: "p"}, true); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("t", 1, "hello", nil); err != nil {
		t.Fatal(err)
	}

	// Mutate every copy a caller can get, at every depth.
	d := e.Table("t")
	d.Columns[1].Type = TInt
	d.Indexes[0].Cols[0] = 0
	if err := e.ReadTxn(func(tx *Txn) error {
		td := tx.Table("t")
		td.Columns[1].Name = "renamed"
		refs, err := tx.ReferencingFKs("p")
		if err != nil {
			return err
		}
		refs[0].Child.Columns[1].Type = TBool
		refs[0].FK.Cols[0] = 0
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Row.Desc is shared, but a mutation through it must not survive a
	// DDL: the DDL builds on the stored bytes.
	row, _, err := e.Get("t", 1)
	if err != nil {
		t.Fatal(err)
	}
	saved := row.Desc.Columns[1].Type
	row.Desc.Columns[1].Type = TBytes
	if err := e.AddColumn("t", Column{Name: "w", Type: TInt}); err != nil {
		t.Fatal(err)
	}
	row.Desc.Columns[1].Type = saved // put the shared descriptor back

	got := e.Table("t")
	if got.Columns[1].Type != TString || got.Columns[1].Name != "v" || got.Indexes[0].Cols[0] != 1 || got.ForeignKeys[0].Cols[0] != 2 {
		t.Fatalf("schema changed through a returned descriptor: %+v", got)
	}
	if err := e.Insert("t", 2, "world", nil, nil); err != nil {
		t.Fatalf("insert after the mutations: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestIdentityUpdateBumpsCounter (D-19): setting an identity column to
// an explicit value by UPDATE now bumps its counter past it, as an
// explicit value on INSERT always did, so later draws cannot collide.
func TestIdentityUpdateBumpsCounter(t *testing.T) {
	for _, occ := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "db")
		e := openEngine(t, path)
		if occ {
			e.Close()
			e = openOCCEngine(t, path)
		}
		if _, err := e.CreateTable("t", []Column{{Name: "id", Type: TInt, Identity: true}, {Name: "v", Type: TInt}}, "id"); err != nil {
			t.Fatal(err)
		}
		if err := e.Insert("t", nil, 0); err != nil { // id 1
			t.Fatal(err)
		}
		if _, err := e.Update("t", []any{1}, map[string]any{"id": 3}); err != nil {
			t.Fatal(err)
		}
		for i := range 3 {
			r, err := e.InsertReturning("t", nil, 0)
			if err != nil {
				t.Fatalf("occ=%v: draw %d after UPDATE id=3: %v", occ, i, err)
			}
			if id := r.Vals[0].(int64); id <= 3 {
				t.Fatalf("occ=%v: drew %d, want > 3", occ, id)
			}
		}
		e.Close()
	}
}

// TestReadOnlyInsertBurnsNothing (D-19): under concurrent writes an
// identity draw is a durable write of its own, so an insert through a
// read-only transaction used to burn a value (and write the log) before
// failing.
func TestReadOnlyInsertBurnsNothing(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "db"))
	defer e.Close()
	if _, err := e.CreateTable("t", []Column{{Name: "id", Type: TInt, Identity: true}}, "id"); err != nil {
		t.Fatal(err)
	}
	before, _, _ := e.PeekIdentity("t", "id")
	err := e.ReadTxn(func(tx *Txn) error { return tx.Insert("t", nil) })
	if err == nil {
		t.Fatal("insert through a read-only transaction succeeded")
	}
	after, _, _ := e.PeekIdentity("t", "id")
	if after != before {
		t.Fatalf("identity next moved %d -> %d on a refused read-only insert", before, after)
	}
}

// TestConstraintNamesShareANamespace (D-19): AddCheck accepted a name an
// existing foreign key on the table already used.
func TestConstraintNamesShareANamespace(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "db"))
	defer e.Close()
	if _, err := e.CreateTable("p", []Column{{Name: "id", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTable("c", []Column{{Name: "id", Type: TInt}, {Name: "pid", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.AddForeignKey("c", FKDesc{Name: "k", Cols: []int{1}, RefTable: "p"}, true); err != nil {
		t.Fatal(err)
	}
	err := e.AddCheck("c", CheckDesc{Name: "k", Expr: "pid > 0"}, nil)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("AddCheck reusing an FK's name: err = %v, want already exists", err)
	}
}

// TestZeroRowCol (D-19 / D-56): Row.Col on the zero Row a lookup miss
// returns used to dereference a nil descriptor.
func TestZeroRowCol(t *testing.T) {
	var r Row
	if v := r.Col("id"); v != nil {
		t.Fatalf("zero Row Col = %v, want nil", v)
	}
}

// TestOCCAllocatorLeaks (D-20): nextval on a missing sequence left one
// allocator in the engine's map per distinct name, and an identity
// insert that lost to DROP TABLE re-created the dropped table's counter
// key — an orphan nothing deletes.
func TestOCCAllocatorLeaks(t *testing.T) {
	e := openOCCEngine(t, filepath.Join(t.TempDir(), "db"))
	defer e.Close()
	for i := range 100 {
		if _, err := e.allocSeqObj(fmt.Sprintf("missing_%d", i)); err == nil {
			t.Fatal("nextval on a missing sequence succeeded")
		}
	}
	e.seqMu.Lock()
	n := len(e.seqObjAllocs)
	e.seqMu.Unlock()
	if n != 0 {
		t.Fatalf("%d sequence allocators left behind by failed nextvals", n)
	}

	if _, err := e.CreateTable("t", []Column{{Name: "id", Type: TInt, Identity: true}}, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("t", nil); err != nil {
		t.Fatal(err)
	}
	tx, err := e.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Table("t") == nil { // resolve and memoize in this snapshot
		t.Fatal("no table")
	}
	if err := e.DropTable("t"); err != nil {
		t.Fatal(err)
	}
	keysAfterDrop := e.Stats().Keys
	_ = tx.Insert("t", nil)
	_ = tx.Commit()
	if k := e.Stats().Keys; k != keysAfterDrop {
		t.Fatalf("a key outlived DROP TABLE (orphan identity counter): %d -> %d", keysAfterDrop, k)
	}
}

// TestCatalogVersion (D-16, D-40 step 2): the DDL counter moves on every
// committed schema change and on nothing else.
func TestCatalogVersion(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "db"))
	defer e.Close()
	v0 := e.CatalogVersion()
	if _, err := e.CreateTable("t", []Column{{Name: "id", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	v1 := e.CatalogVersion()
	if v1 <= v0 {
		t.Fatalf("CREATE TABLE left the version at %d", v1)
	}
	if err := e.Insert("t", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.NextSeq("s"); err != nil {
		t.Fatal(err)
	}
	if v := e.CatalogVersion(); v != v1 {
		t.Fatalf("DML moved the version %d -> %d", v1, v)
	}
	if _, err := e.CreateTable("t", []Column{{Name: "id", Type: TInt}}, "id"); err == nil {
		t.Fatal("duplicate CREATE TABLE succeeded")
	}
	if v := e.CatalogVersion(); v != v1 {
		t.Fatalf("a failed DDL moved the version %d -> %d", v1, v)
	}
	if err := e.AddColumn("t", Column{Name: "v", Type: TInt}); err != nil {
		t.Fatal(err)
	}
	if v := e.CatalogVersion(); v <= v1 {
		t.Fatalf("ADD COLUMN left the version at %d", v)
	}
}
