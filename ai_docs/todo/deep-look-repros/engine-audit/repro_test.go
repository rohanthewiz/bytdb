package engineaudit

import (
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/rohanthewiz/bytdb"
)

func open(t testing.TB, occ bool) *bytdb.Engine {
	t.Helper()
	opts := []any{}
	_ = opts
	var e *bytdb.Engine
	var err error
	p := filepath.Join(t.TempDir(), "db")
	if occ {
		e, err = bytdb.Open(p, bytdb.WithSyncNever(), bytdb.WithConcurrentWrites())
	} else {
		e, err = bytdb.Open(p, bytdb.WithSyncNever())
	}
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// R1: a failed Insert's duplicate probe is an untracked read. Two
// SERIALIZABLE transactions each "observe" a row through a failed
// insert, then delete the other's row. No serial order produces "both
// rows gone": whichever ran second would have had its insert succeed.
func TestR1_SerializableDupProbeUntracked(t *testing.T) {
	e := open(t, true)
	defer e.Close()
	for _, n := range []string{"a", "b"} {
		_, err := e.CreateTable(n, []bytdb.Column{{Name: "id", Type: bytdb.TInt}}, "id")
		must(t, err)
		must(t, e.Insert(n, 1))
	}
	t1, err := e.BeginSerializable()
	must(t, err)
	t2, err := e.BeginSerializable()
	must(t, err)

	// T1: "ensure a(1) exists" via insert; it does, so retire b(1).
	if err := t1.Insert("a", 1); err == nil {
		t.Fatal("expected duplicate")
	}
	_, err = t1.Delete("b", 1)
	must(t, err)
	// T2: mirror image.
	if err := t2.Insert("b", 1); err == nil {
		t.Fatal("expected duplicate")
	}
	_, err = t2.Delete("a", 1)
	must(t, err)

	err1 := t1.Commit()
	err2 := t2.Commit()
	_, aOK, _ := e.Get("a", 1)
	_, bOK, _ := e.Get("b", 1)
	t.Logf("commit1=%v commit2=%v a(1) present=%v b(1) present=%v", err1, err2, aOK, bOK)
	if err1 == nil && err2 == nil {
		t.Errorf("WRITE SKEW under SERIALIZABLE: both committed; a=%v b=%v", aOK, bOK)
	}
}

// R1 control: same shape with Get instead of a failed Insert conflicts.
func TestR1_ControlWithGet(t *testing.T) {
	e := open(t, true)
	defer e.Close()
	for _, n := range []string{"a", "b"} {
		_, err := e.CreateTable(n, []bytdb.Column{{Name: "id", Type: bytdb.TInt}}, "id")
		must(t, err)
		must(t, e.Insert(n, 1))
	}
	t1, _ := e.BeginSerializable()
	t2, _ := e.BeginSerializable()
	t1.Get("a", 1)
	t1.Delete("b", 1)
	t2.Get("b", 1)
	t2.Delete("a", 1)
	err1 := t1.Commit()
	err2 := t2.Commit()
	t.Logf("control: commit1=%v commit2=%v", err1, err2)
	if err1 == nil && err2 == nil {
		t.Errorf("control also skewed")
	}
}

// R1b: same hole through a unique-index probe in Update.
func TestR1b_SerializableUniqueProbeUntracked(t *testing.T) {
	e := open(t, true)
	defer e.Close()
	_, err := e.CreateTable("u", []bytdb.Column{{Name: "id", Type: bytdb.TInt}, {Name: "email", Type: bytdb.TString}}, "id")
	must(t, err)
	_, err = e.CreateIndex("u", "u_email", true, "email")
	must(t, err)
	_, err = e.CreateTable("log", []bytdb.Column{{Name: "id", Type: bytdb.TInt}}, "id")
	must(t, err)
	must(t, e.Insert("u", 1, "x@x"))
	must(t, e.Insert("log", 1))

	t1, _ := e.BeginSerializable()
	t2, _ := e.BeginSerializable()
	// T1 learns "x@x is taken" from a unique violation, then deletes log(1).
	if err := t1.Insert("u", 2, "x@x"); err == nil {
		t.Fatal("expected unique violation")
	}
	t1.Delete("log", 1)
	// T2 learns log(1) exists from a dup, then frees x@x.
	if err := t2.Insert("log", 1); err == nil {
		t.Fatal("expected dup")
	}
	t2.Delete("u", 1)
	err1, err2 := t1.Commit(), t2.Commit()
	t.Logf("commit1=%v commit2=%v", err1, err2)
	if err1 == nil && err2 == nil {
		t.Errorf("WRITE SKEW under SERIALIZABLE via unique probe")
	}
}

// runWithTimeout reports whether fn finished within d.
func runWithTimeout(d time.Duration, fn func()) bool {
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// R2: a DDL callback (AddCheck validate / AlterColumnType Using) that
// issues an engine write deadlocks; the reentrancy guard is not armed
// for DDL closures.
func TestR2_DDLCallbackReentrancyDefaultMode(t *testing.T) {
	e := open(t, false)
	_, err := e.CreateTable("t", []bytdb.Column{{Name: "id", Type: bytdb.TInt}, {Name: "v", Type: bytdb.TInt}}, "id")
	must(t, err)
	must(t, e.Insert("t", 1, 1))
	var inner error
	ok := runWithTimeout(3*time.Second, func() {
		err = e.AddCheck("t", bytdb.CheckDesc{Name: "c", Expr: "v>0"}, func(r bytdb.Row) error {
			_, inner = e.NextSeq("audit")
			return inner
		})
	})
	if !ok {
		t.Errorf("DEADLOCK: AddCheck validate calling e.NextSeq hung (default mode); guard did not fire")
		return
	}
	t.Logf("returned: err=%v inner=%v", err, inner)
}

func TestR2_DDLCallbackReentrancyOCC(t *testing.T) {
	e := open(t, true)
	_, err := e.CreateTable("t", []bytdb.Column{{Name: "id", Type: bytdb.TInt}, {Name: "v", Type: bytdb.TString}}, "id")
	must(t, err)
	for i := 0; i < 200; i++ {
		must(t, e.Insert("t", i, "x"))
	}
	ok := runWithTimeout(5*time.Second, func() {
		err = e.AlterColumnType("t", "v", bytdb.ColumnTypeChange{
			Type: bytdb.TInt,
			Using: func(old bytdb.Row) (any, error) {
				v, err := e.NextSeq("renumber")
				return int64(v), err
			},
		})
	})
	if !ok {
		t.Errorf("DEADLOCK: AlterColumnType Using calling e.NextSeq hung (OCC mode)")
		return
	}
	t.Logf("returned: err=%v", err)
}

// R3: reads through a finished Txn silently report "not found".
func TestR3_UseAfterCommitReadsEmpty(t *testing.T) {
	e := open(t, false)
	defer e.Close()
	_, err := e.CreateTable("t", []bytdb.Column{{Name: "id", Type: bytdb.TInt}}, "id")
	must(t, err)
	must(t, e.Insert("t", 1))
	tx, err := e.Begin(false)
	must(t, err)
	_, ok, err := tx.Get("t", 1)
	t.Logf("before close: ok=%v err=%v", ok, err)
	tx.Rollback()
	_, ok, err = tx.Get("t", 1)
	n := 0
	var serr error
	for _, err := range tx.Scan("t") {
		if err != nil {
			serr = err
			break
		}
		n++
	}
	t.Logf("after close: Get ok=%v err=%v; Scan rows=%d err=%v", ok, err, n, serr)
	if !ok && err == nil {
		t.Errorf("Get on a closed Txn returned (not found, nil) for an existing row")
	}
}

// R4: NaN cannot be stored in a (non-key) float column.
func TestR4_NaN(t *testing.T) {
	e := open(t, false)
	defer e.Close()
	_, err := e.CreateTable("t", []bytdb.Column{{Name: "id", Type: bytdb.TInt}, {Name: "f", Type: bytdb.TFloat}}, "id")
	must(t, err)
	err = e.Insert("t", 1, math.NaN())
	t.Logf("insert NaN: %v", err)
	err = e.Insert("t", 2, math.Inf(1))
	t.Logf("insert +Inf: %v", err)
	r, _ := e.InsertReturning("t", 3, math.Copysign(0, -1))
	g, _, _ := e.Get("t", 3)
	t.Logf("-0: returning signbit=%v stored signbit=%v", math.Signbit(r.Vals[1].(float64)), math.Signbit(g.Vals[1].(float64)))
}

// R7: integer kinds coerce accepts.
func TestR7_Coerce(t *testing.T) {
	e := open(t, false)
	defer e.Close()
	_, err := e.CreateTable("t", []bytdb.Column{{Name: "id", Type: bytdb.TInt}, {Name: "f", Type: bytdb.TFloat}, {Name: "ts", Type: bytdb.TTimestamp}}, "id")
	must(t, err)
	t.Logf("uint id:    %v", e.Insert("t", uint(1), 1.0, nil))
	t.Logf("uint64 id:  %v", e.Insert("t", uint64(2), 1.0, nil))
	t.Logf("int32->flt: %v", e.Insert("t", 3, int32(1), nil))
	t.Logf("int->ts:    %v", e.Insert("t", 4, 1.0, 12345))
	_, _, err = e.Get("t", uint64(2))
	t.Logf("Get(uint64): %v", err)
}

// R11: UPDATE of an identity column does not bump the counter.
func TestR11_UpdateIdentityNoBump(t *testing.T) {
	e := open(t, false)
	defer e.Close()
	_, err := e.CreateTable("t", []bytdb.Column{{Name: "id", Type: bytdb.TInt, Identity: true}, {Name: "v", Type: bytdb.TInt}}, "id")
	must(t, err)
	must(t, e.Insert("t", nil, 0)) // id 1
	_, err = e.Update("t", []any{1}, map[string]any{"id": 3})
	must(t, err)
	var last error
	for i := 0; i < 3; i++ {
		_, last = e.InsertReturning("t", nil, 0)
		if last != nil {
			t.Logf("draw %d failed: %v", i+2, last)
		}
	}
}

// R25: writing through the Txn while ranging over its own Scan.
func TestR25_MutateDuringTxnScan(t *testing.T) {
	for _, mode := range []string{"update-nonkey", "delete-current", "move-key-forward", "insert-ahead"} {
		t.Run(mode, func(t *testing.T) {
			e := open(t, false)
			defer e.Close()
			_, err := e.CreateTable("t", []bytdb.Column{{Name: "id", Type: bytdb.TInt}, {Name: "v", Type: bytdb.TInt}}, "id")
			must(t, err)
			const n = 5000
			must(t, e.WriteTxn(func(tx *bytdb.Txn) error {
				for i := 0; i < n; i++ {
					if err := tx.Insert("t", i, 0); err != nil {
						return err
					}
				}
				return nil
			}))
			seen := 0
			dups := map[int64]int{}
			var panicked any
			err = func() (err error) {
				defer func() {
					if r := recover(); r != nil {
						panicked = r
					}
				}()
				return e.WriteTxn(func(tx *bytdb.Txn) error {
					for row, err := range tx.Scan("t") {
						if err != nil {
							return err
						}
						id := row.Vals[0].(int64)
						seen++
						dups[id]++
						if seen > 10*n {
							return errors.New("runaway scan")
						}
						switch mode {
						case "update-nonkey":
							if _, err := tx.Update("t", []any{id}, map[string]any{"v": 1}); err != nil {
								return err
							}
						case "delete-current":
							if _, err := tx.Delete("t", id); err != nil {
								return err
							}
						case "move-key-forward":
							if id < n {
								if _, err := tx.Update("t", []any{id}, map[string]any{"id": id + 1_000_000}); err != nil {
									return err
								}
							}
						case "insert-ahead":
							if id < n {
								if err := tx.Insert("t", id+1_000_000, 0); err != nil {
									return err
								}
							}
						}
					}
					return nil
				})
			}()
			multi := 0
			for _, c := range dups {
				if c > 1 {
					multi++
				}
			}
			left := 0
			upd := 0
			for row, _ := range e.Scan("t") {
				left++
				if row.Vals[1].(int64) == 1 {
					upd++
				}
			}
			t.Logf("mode=%s rows yielded=%d (table had %d) distinct=%d yielded-twice=%d err=%v panic=%v; table now %d rows, %d with v=1",
				mode, seen, n, len(dups), multi, err, panicked, left, upd)
		})
	}
}

// R34: Engine.Table hands out the shared cached descriptor.
func TestR34_SharedDescMutation(t *testing.T) {
	e := open(t, false)
	_, err := e.CreateTable("t", []bytdb.Column{{Name: "id", Type: bytdb.TInt}, {Name: "v", Type: bytdb.TString}}, "id")
	must(t, err)
	must(t, e.Insert("t", 1, "hello"))
	d := e.Table("t")
	d.Columns[1].Type = bytdb.TInt // caller "normalizes" its copy
	// Any later descriptor rewrite clones the polluted cache entry and persists it.
	must(t, e.AddColumn("t", bytdb.Column{Name: "w", Type: bytdb.TInt}))
	t.Logf("after AddColumn: column v type = %s", e.Table("t").Columns[1].Type)
	err = e.Insert("t", 2, "world", nil)
	t.Logf("insert string into v: %v", err)
	if e.Table("t").Columns[1].Type != bytdb.TString {
		t.Errorf("mutating the *TableDesc from Engine.Table corrupted the persisted schema")
	}
	e.Close()
}

// Row.Col on the zero Row a miss returns.
func TestRowColZero(t *testing.T) {
	e := open(t, false)
	defer e.Close()
	_, err := e.CreateTable("t", []bytdb.Column{{Name: "id", Type: bytdb.TInt}}, "id")
	must(t, err)
	row, ok, err := e.Get("t", 42)
	t.Logf("ok=%v err=%v", ok, err)
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Row.Col on a not-found Row panicked: %v", r)
		}
	}()
	_ = row.Col("id")
}

// R40: Insert through a read-only Txn in OCC mode burns an identity value.
func TestR40_ReadOnlyInsertBurnsIdentity(t *testing.T) {
	e := open(t, true)
	defer e.Close()
	_, err := e.CreateTable("t", []bytdb.Column{{Name: "id", Type: bytdb.TInt, Identity: true}}, "id")
	must(t, err)
	before, _, _ := e.PeekIdentity("t", "id")
	err = e.ReadTxn(func(tx *bytdb.Txn) error { return tx.Insert("t", nil) })
	after, ok, _ := e.PeekIdentity("t", "id")
	t.Logf("read-only insert err=%v; identity next before=%d after=%d (exists=%v)", err, before, after, ok)
}
