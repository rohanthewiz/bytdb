package engineaudit

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// Orphan identity counter: an insert planned against a table that a
// concurrent DROP TABLE removed re-creates the table's counter key.
func TestOrphanIdentityCounterAfterDrop(t *testing.T) {
	e := open(t, true)
	defer e.Close()
	_, err := e.CreateTable("t", []bytdb.Column{{Name: "id", Type: bytdb.TInt, Identity: true}}, "id")
	must(t, err)
	must(t, e.Insert("t", nil))

	tx, err := e.Begin(true)
	must(t, err)
	if tx.Table("t") == nil { // resolve + memoize the descriptor in this snapshot
		t.Fatal("no table")
	}
	must(t, e.DropTable("t"))
	keysAfterDrop := e.Stats().Keys
	insErr := tx.Insert("t", nil)
	commitErr := tx.Commit()
	keysAfter := e.Stats().Keys
	t.Logf("insert err=%v commit err=%v; kv keys after drop=%d, after the losing insert=%d",
		insErr, commitErr, keysAfterDrop, keysAfter)
	if keysAfter != keysAfterDrop {
		t.Errorf("a key outlived DROP TABLE (orphan identity counter): %d -> %d", keysAfterDrop, keysAfter)
	}
}

// nextval on a sequence that does not exist leaves an allocator behind.
// (Observable only as memory; here we just show the call shape works and
// errors, the leak is by code reading: seqObjAllocFor inserts before the
// existence check in reserveLocked.)

// Stress: concurrent OCC writers + DDL; afterwards every index must
// agree with the primary rows.
func TestOCCStressIndexConsistency(t *testing.T) {
	e := open(t, true)
	defer e.Close()
	_, err := e.CreateTable("t", []bytdb.Column{
		{Name: "id", Type: bytdb.TInt},
		{Name: "a", Type: bytdb.TInt},
		{Name: "u", Type: bytdb.TInt},
		{Name: "s", Type: bytdb.TString},
	}, "id")
	must(t, err)
	_, err = e.CreateIndex("t", "t_a", false, "a")
	must(t, err)
	_, err = e.CreateIndexCols("t", "t_u", true, []bytdb.IndexCol{{Name: "u", Desc: true}})
	must(t, err)

	var wg sync.WaitGroup
	var conflicts, oks, other atomic.Int64
	stop := make(chan struct{})
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w)))
			for {
				select {
				case <-stop:
					return
				default:
				}
				err := e.WriteTxn(func(tx *bytdb.Txn) error {
					for i := 0; i < 3; i++ {
						id := rng.Intn(300)
						var u any = rng.Intn(400)
						if rng.Intn(5) == 0 {
							u = nil
						}
						switch rng.Intn(5) {
						case 0, 1:
							if err := tx.Insert("t", id, rng.Intn(10), u, "x"); err != nil {
								continue // dup: handled, keep going
							}
						case 2:
							if _, err := tx.Update("t", []any{id}, map[string]any{"a": rng.Intn(10), "u": u}); err != nil {
								continue
							}
						case 3:
							if _, err := tx.Update("t", []any{id}, map[string]any{"id": rng.Intn(300)}); err != nil {
								continue
							}
						case 4:
							if _, err := tx.Delete("t", id); err != nil {
								return err
							}
						}
					}
					return nil
				})
				switch {
				case err == nil:
					oks.Add(1)
				case errors.Is(err, bytdb.ErrTxConflict):
					conflicts.Add(1)
				default:
					other.Add(1)
				}
			}
		}(w)
	}
	// DDL churn alongside.
	for i := 0; i < 30; i++ {
		name := fmt.Sprintf("x%d", i)
		if _, err := e.CreateIndex("t", name, false, "s", "a"); err != nil {
			t.Errorf("create index: %v", err)
		}
		if i%3 == 0 {
			if err := e.AddColumn("t", bytdb.Column{Name: "c" + name, Type: bytdb.TInt, Default: "7"}); err != nil {
				t.Errorf("add column: %v", err)
			}
			if err := e.DropColumn("t", "c"+name); err != nil {
				t.Errorf("drop column: %v", err)
			}
		}
		if err := e.DropIndex("t", name); err != nil {
			t.Errorf("drop index: %v", err)
		}
	}
	close(stop)
	wg.Wait()
	t.Logf("commits=%d conflicts=%d other-errors=%d", oks.Load(), conflicts.Load(), other.Load())

	rows := map[int64][]any{}
	for r, err := range e.Scan("t") {
		must(t, err)
		rows[r.Vals[0].(int64)] = r.Vals
	}
	for _, idx := range []string{"t_a", "t_u"} {
		n := 0
		seenU := map[int64]bool{}
		for r, err := range e.ScanIndex("t", idx, nil, nil) {
			if err != nil {
				t.Fatalf("index %s: %v", idx, err)
			}
			n++
			if idx == "t_u" && r.Vals[2] != nil {
				u := r.Vals[2].(int64)
				if seenU[u] {
					t.Errorf("unique index holds duplicate u=%d", u)
				}
				seenU[u] = true
			}
		}
		if n != len(rows) {
			t.Errorf("index %s has %d entries, table has %d rows", idx, n, len(rows))
		}
	}
	// Uniqueness in the rows themselves.
	us := map[int64]int64{}
	for id, v := range rows {
		if v[2] == nil {
			continue
		}
		u := v[2].(int64)
		if other, dup := us[u]; dup {
			t.Errorf("rows %d and %d share unique u=%d", other, id, u)
		}
		us[u] = id
	}
	t.Logf("final rows=%d", len(rows))
}

// ---- allocation profile of the hot paths ----

func benchEngine(b *testing.B, indexes int) *bytdb.Engine {
	e := open(b, false)
	_, err := e.CreateTable("t", []bytdb.Column{
		{Name: "id", Type: bytdb.TInt},
		{Name: "name", Type: bytdb.TString},
		{Name: "age", Type: bytdb.TInt},
		{Name: "email", Type: bytdb.TString},
		{Name: "score", Type: bytdb.TFloat},
	}, "id")
	must(b, err)
	if indexes >= 1 {
		_, err = e.CreateIndex("t", "t_age", false, "age")
		must(b, err)
	}
	if indexes >= 2 {
		_, err = e.CreateIndex("t", "t_email", true, "email")
		must(b, err)
	}
	must(b, e.WriteTxn(func(tx *bytdb.Txn) error {
		for i := 0; i < 10000; i++ {
			if err := tx.Insert("t", i, "name-of-row", i%90, fmt.Sprintf("u%d@example.com", i), 1.5); err != nil {
				return err
			}
		}
		return nil
	}))
	return e
}

func BenchmarkEngineGet(b *testing.B) {
	e := benchEngine(b, 0)
	defer e.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok, err := e.Get("t", i%10000); !ok || err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTxnGet(b *testing.B) {
	e := benchEngine(b, 0)
	defer e.Close()
	b.ReportAllocs()
	b.ResetTimer()
	e.ReadTxn(func(tx *bytdb.Txn) error {
		for i := 0; i < b.N; i++ {
			if _, ok, err := tx.Get("t", i%10000); !ok || err != nil {
				b.Fatal(err)
			}
		}
		return nil
	})
}

func BenchmarkScanPerRow(b *testing.B) {
	e := benchEngine(b, 0)
	defer e.Close()
	b.ReportAllocs()
	b.ResetTimer()
	n := 0
	for n < b.N {
		for _, err := range e.Scan("t") {
			if err != nil {
				b.Fatal(err)
			}
			n++
			if n >= b.N {
				break
			}
		}
	}
}

func BenchmarkScanIndexPerRow(b *testing.B) {
	e := benchEngine(b, 1)
	defer e.Close()
	b.ReportAllocs()
	b.ResetTimer()
	n := 0
	for n < b.N {
		for _, err := range e.ScanIndex("t", "t_age", nil, nil) {
			if err != nil {
				b.Fatal(err)
			}
			n++
			if n >= b.N {
				break
			}
		}
	}
}

func benchInsert(b *testing.B, indexes int) {
	e := benchEngine(b, indexes)
	defer e.Close()
	e.SetMaxTxnWrites(0)
	b.ReportAllocs()
	b.ResetTimer()
	must(b, e.WriteTxn(func(tx *bytdb.Txn) error {
		for i := 0; i < b.N; i++ {
			id := 10000 + i
			if err := tx.Insert("t", id, "name-of-row", id%90, "v"+fmt.Sprint(id), 1.5); err != nil {
				return err
			}
		}
		return nil
	}))
}
func BenchmarkTxnInsert0Idx(b *testing.B) { benchInsert(b, 0) }
func BenchmarkTxnInsert2Idx(b *testing.B) { benchInsert(b, 2) }

func benchUpdate(b *testing.B, indexes int) {
	e := benchEngine(b, indexes)
	defer e.Close()
	e.SetMaxTxnWrites(0)
	set := map[string]any{"score": 2.5} // not indexed
	b.ReportAllocs()
	b.ResetTimer()
	must(b, e.WriteTxn(func(tx *bytdb.Txn) error {
		for i := 0; i < b.N; i++ {
			if _, err := tx.Update("t", []any{i % 10000}, set); err != nil {
				return err
			}
		}
		return nil
	}))
}
func BenchmarkTxnUpdateNonIndexed0Idx(b *testing.B) { benchUpdate(b, 0) }
func BenchmarkTxnUpdateNonIndexed2Idx(b *testing.B) { benchUpdate(b, 2) }

// Parallel one-shot Gets: the descriptor cache's RWMutex and per-call
// blob copy sit on this path.
func BenchmarkEngineGetParallel(b *testing.B) {
	e := benchEngine(b, 0)
	defer e.Close()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			i++
			if _, ok, err := e.Get("t", i%10000); !ok || err != nil {
				b.Fatal(err)
			}
		}
	})
}

// Wide descriptor: cost of the per-lookup blob copy grows with schema size.
func BenchmarkEngineGetWideDesc(b *testing.B) {
	e := open(b, false)
	defer e.Close()
	cols := []bytdb.Column{{Name: "id", Type: bytdb.TInt}}
	for i := 0; i < 100; i++ {
		cols = append(cols, bytdb.Column{Name: fmt.Sprintf("column_number_%d", i), Type: bytdb.TString})
	}
	_, err := e.CreateTable("w", cols, "id")
	must(b, err)
	vals := make([]any, len(cols))
	vals[0] = 1
	must(b, e.Insert("w", vals...))
	b.Logf("descriptor JSON is ~%d columns", len(cols))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok, err := e.Get("w", 1); !ok || err != nil {
			b.Fatal(err)
		}
	}
}
