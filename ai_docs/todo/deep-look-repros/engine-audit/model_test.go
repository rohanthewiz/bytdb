package engineaudit

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// Model-based check: random DML + schema changes against an in-memory
// model; after each phase every index must list exactly the model's
// rows, and a reopen (WAL replay) must reproduce them.
func TestModelIndexDrift(t *testing.T) {
	for seed := int64(1); seed <= 6; seed++ {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) { runModel(t, seed) })
	}
}

type mrow struct {
	id   int64
	a    any // int64 or nil
	b    any // string or nil
	junk any
}

func runModel(t *testing.T, seed int64) {
	rng := rand.New(rand.NewSource(seed))
	path := filepath.Join(t.TempDir(), "db")
	e, err := bytdb.Open(path, bytdb.WithSyncNever())
	must(t, err)
	_, err = e.CreateTable("t", []bytdb.Column{
		{Name: "junk", Type: bytdb.TInt}, // dropped later: shifts every ordinal
		{Name: "id", Type: bytdb.TInt},
		{Name: "a", Type: bytdb.TInt},
		{Name: "b", Type: bytdb.TString},
	}, "id")
	must(t, err)
	_, err = e.CreateIndexCols("t", "ix_a_desc", false, []bytdb.IndexCol{{Name: "a", Desc: true}})
	must(t, err)
	_, err = e.CreateIndexCols("t", "ux_ab", true, []bytdb.IndexCol{{Name: "a"}, {Name: "b", Desc: true}})
	must(t, err)
	_, err = e.CreateIndex("t", "ux_b", true, "b")
	must(t, err)

	model := map[int64]*mrow{}
	hasJunk := true
	uniqueOK := func(self int64, a, b any) bool {
		for id, r := range model {
			if id == self {
				continue
			}
			if b != nil && r.b == b {
				return false // ux_b
			}
			if a != nil && b != nil && r.a == a && r.b == b {
				return false // ux_ab
			}
		}
		return true
	}
	randA := func() any {
		if rng.Intn(4) == 0 {
			return nil
		}
		return int64(rng.Intn(8))
	}
	randB := func() any {
		if rng.Intn(4) == 0 {
			return nil
		}
		return fmt.Sprintf("s%d", rng.Intn(60))
	}
	vals := func(id int64, a, b any) []any {
		if hasJunk {
			return []any{nil, id, a, b}
		}
		return []any{id, a, b}
	}

	verify := func(stage string) {
		t.Helper()
		got := map[int64][]any{}
		for r, err := range e.Scan("t") {
			if err != nil {
				t.Fatalf("%s: scan: %v", stage, err)
			}
			got[r.Col("id").(int64)] = []any{r.Col("a"), r.Col("b")}
		}
		if len(got) != len(model) {
			t.Fatalf("%s: table has %d rows, model %d", stage, len(got), len(model))
		}
		for id, r := range model {
			g, ok := got[id]
			if !ok || g[0] != r.a || g[1] != r.b {
				t.Fatalf("%s: row %d = %v, model (%v,%v)", stage, id, g, r.a, r.b)
			}
		}
		for _, ix := range e.Table("t").Indexes {
			seen := map[int64]bool{}
			var prev []any
			for r, err := range e.ScanIndex("t", ix.Name, nil, nil) {
				if err != nil {
					t.Fatalf("%s: index %s: %v", stage, ix.Name, err)
				}
				id := r.Col("id").(int64)
				if seen[id] {
					t.Fatalf("%s: index %s lists row %d twice", stage, ix.Name, id)
				}
				seen[id] = true
				m := model[id]
				if m == nil || r.Col("a") != m.a || r.Col("b") != m.b {
					t.Fatalf("%s: index %s yields stale row %d", stage, ix.Name, id)
				}
				_ = prev
			}
			if len(seen) != len(model) {
				t.Fatalf("%s: index %s has %d entries, model %d rows", stage, ix.Name, len(seen), len(model))
			}
			// reverse scan must agree in count
			n := 0
			for _, err := range e.ScanIndexRev("t", ix.Name, nil, nil, false) {
				if err != nil {
					t.Fatalf("%s: index %s rev: %v", stage, ix.Name, err)
				}
				n++
			}
			if n != len(model) {
				t.Fatalf("%s: index %s reverse scan %d entries, model %d", stage, ix.Name, n, len(model))
			}
		}
	}

	dml := func(n int) {
		for i := 0; i < n; i++ {
			id := int64(rng.Intn(120))
			switch rng.Intn(6) {
			case 0, 1: // insert
				a, b := randA(), randB()
				err := e.Insert("t", vals(id, a, b)...)
				want := model[id] == nil && uniqueOK(-1, a, b)
				if (err == nil) != want {
					t.Fatalf("insert(%d,%v,%v): err=%v, model expects ok=%v", id, a, b, err, want)
				}
				if err == nil {
					model[id] = &mrow{id: id, a: a, b: b}
				}
			case 2, 3: // update a/b
				a, b := randA(), randB()
				ok, err := e.Update("t", []any{id}, map[string]any{"a": a, "b": b})
				exists := model[id] != nil
				want := !exists || uniqueOK(id, a, b)
				if (err == nil) != want {
					t.Fatalf("update(%d,%v,%v): err=%v, model expects ok=%v", id, a, b, err, want)
				}
				if err == nil && ok != exists {
					t.Fatalf("update(%d) reported %v, model %v", id, ok, exists)
				}
				if err == nil && exists {
					model[id].a, model[id].b = a, b
				}
			case 4: // move pk inside a txn with a savepoint that is sometimes rolled back
				nid := int64(rng.Intn(120))
				rollback := rng.Intn(3) == 0
				var moved bool
				err := e.WriteTxn(func(tx *bytdb.Txn) error {
					sp, err := tx.Savepoint()
					if err != nil {
						return err
					}
					ok, err := tx.Update("t", []any{id}, map[string]any{"id": nid})
					if err != nil {
						return tx.RollbackTo(sp)
					}
					moved = ok
					if rollback {
						moved = false
						return tx.RollbackTo(sp)
					}
					return nil
				})
				must(t, err)
				exists := model[id] != nil
				want := exists && (nid == id || model[nid] == nil) && !rollback
				if moved != want {
					t.Fatalf("pk move %d->%d: moved=%v want=%v", id, nid, moved, want)
				}
				if moved && nid != id {
					r := model[id]
					delete(model, id)
					r.id = nid
					model[nid] = r
				}
			case 5:
				ok, err := e.Delete("t", id)
				must(t, err)
				if ok != (model[id] != nil) {
					t.Fatalf("delete(%d)=%v", id, ok)
				}
				delete(model, id)
			}
		}
	}

	dml(600)
	verify("initial")

	must(t, e.DropColumn("t", "junk")) // ordinal shift under three indexes
	hasJunk = false
	dml(600)
	verify("after drop column")

	must(t, e.AddColumn("t", bytdb.Column{Name: "c", Type: bytdb.TInt, Default: "5", NotNull: true}))
	vals = func(id int64, a, b any) []any { return []any{id, a, b, 5} }
	dml(400)
	verify("after add column default")

	// a: int -> float (indexed, descending in one index, part of a unique one)
	must(t, e.AlterColumnType("t", "a", bytdb.ColumnTypeChange{Type: bytdb.TFloat}))
	for _, r := range model {
		if r.a != nil {
			r.a = float64(r.a.(int64))
		}
	}
	randA = func() any {
		if rng.Intn(4) == 0 {
			return nil
		}
		return float64(rng.Intn(8))
	}
	dml(400)
	verify("after alter type")

	must(t, e.RenameColumn("t", "b", "bb"))
	must(t, e.RenameColumn("t", "bb", "b"))
	must(t, e.RenameTable("t", "t2"))
	must(t, e.RenameTable("t2", "t"))
	dml(300)
	verify("after renames")

	// Reopen: replay must reproduce the same state.
	must(t, e.Close())
	e, err = bytdb.Open(path, bytdb.WithSyncNever())
	must(t, err)
	defer e.Close()
	verify("after reopen")
	ids := make([]int64, 0, len(model))
	for id := range model {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	i := 0
	for r, err := range e.Scan("t") {
		must(t, err)
		if r.Col("id").(int64) != ids[i] {
			t.Fatalf("scan order broken at %d", i)
		}
		i++
	}
}

// nextval on names that do not exist: each leaves an allocator in the
// engine map (OCC mode).
func TestSeqObjAllocLeakOnMissingName(t *testing.T) {
	e := open(t, true)
	defer e.Close()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	const n = 200_000
	must(t, e.WriteTxn(func(tx *bytdb.Txn) error {
		for i := 0; i < n; i++ {
			if _, err := tx.NextVal(fmt.Sprintf("no_such_sequence_%d", i)); err == nil {
				t.Fatal("expected error")
			}
		}
		return nil
	}))
	runtime.GC()
	runtime.ReadMemStats(&after)
	t.Logf("%d failed nextval calls on distinct missing names: live heap %d KB -> %d KB (+%d B/call)",
		n, before.HeapAlloc/1024, after.HeapAlloc/1024, (int64(after.HeapAlloc)-int64(before.HeapAlloc))/n)
}
