package engineaudit

import (
	"runtime"
	"testing"
	"time"

	"github.com/rohanthewiz/bytdb"
)

// What curGID costs: it runs once per WriteTxn / Begin(true).
func BenchmarkGoroutineIDParse(b *testing.B) {
	var buf [40]byte
	for i := 0; i < b.N; i++ {
		runtime.Stack(buf[:], false)
	}
}

func BenchmarkWriteTxnOneUpdate(b *testing.B) {
	e := benchEngine(b, 0)
	defer e.Close()
	set := map[string]any{"score": 2.5}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		must(b, e.WriteTxn(func(tx *bytdb.Txn) error {
			_, err := tx.Update("t", []any{i % 10000}, set)
			return err
		}))
	}
}

func BenchmarkOneShotUpdate(b *testing.B) {
	e := benchEngine(b, 0)
	defer e.Close()
	set := map[string]any{"score": 2.5}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.Update("t", []any{i % 10000}, set); err != nil {
			b.Fatal(err)
		}
	}
}

// CACHE is walked one step at a time inside reserveLocked.
func TestSeqHugeCacheSpins(t *testing.T) {
	e := open(t, true)
	mk := func(name string, cache int64) {
		must(t, e.CreateSequence(&bytdb.SeqDesc{Name: name, Start: 1, Increment: 1, Min: 1, Max: 1<<63 - 1, Cache: cache}))
	}
	mk("s_small", 50_000_000)
	start := time.Now()
	must(t, e.WriteTxn(func(tx *bytdb.Txn) error { _, err := tx.NextVal("s_small"); return err }))
	t.Logf("first nextval with CACHE 50,000,000 took %v", time.Since(start))

	mk("s_huge", 1<<50)
	ok := runWithTimeout(5*time.Second, func() {
		e.WriteTxn(func(tx *bytdb.Txn) error { _, err := tx.NextVal("s_huge"); return err })
	})
	if !ok {
		t.Errorf("nextval on a sequence with CACHE 2^50 did not return within 5s (CPU spin in reserveLocked)")
	}
}

// AddCheck does not look at foreign-key names; AddForeignKey looks at both.
func TestConstraintNameCollision(t *testing.T) {
	e := open(t, false)
	defer e.Close()
	_, err := e.CreateTable("p", []bytdb.Column{{Name: "id", Type: bytdb.TInt}}, "id")
	must(t, err)
	_, err = e.CreateTable("c", []bytdb.Column{{Name: "id", Type: bytdb.TInt}, {Name: "pid", Type: bytdb.TInt}}, "id")
	must(t, err)
	must(t, e.AddForeignKey("c", bytdb.FKDesc{Name: "k", Cols: []int{1}, RefTable: "p"}, true))
	err = e.AddCheck("c", bytdb.CheckDesc{Name: "k", Expr: "pid > 0"}, nil)
	t.Logf("AddCheck with an existing FK's name: err=%v", err)
	d := e.Table("c")
	t.Logf("constraints named k: checks=%d fks=%d", len(d.Checks), len(d.ForeignKeys))
	if err == nil {
		t.Errorf("two constraints share the name %q on one table", "k")
	}
}

// Update/Delete through a finished writable Txn report "no such row".
func TestClosedTxnUpdateDelete(t *testing.T) {
	e := open(t, false)
	defer e.Close()
	_, err := e.CreateTable("t", []bytdb.Column{{Name: "id", Type: bytdb.TInt}, {Name: "v", Type: bytdb.TInt}}, "id")
	must(t, err)
	must(t, e.Insert("t", 1, 1))
	tx, err := e.Begin(true)
	must(t, err)
	tx.Table("t")
	must(t, tx.Commit())
	upd, uerr := tx.Update("t", []any{1}, map[string]any{"v": 2})
	del, derr := tx.Delete("t", 1)
	ierr := tx.Insert("t", 2, 2)
	t.Logf("after Commit: Update=(%v,%v) Delete=(%v,%v) Insert err=%v", upd, uerr, del, derr, ierr)
	if uerr == nil || derr == nil {
		t.Errorf("Update/Delete on a committed Txn returned (false, nil) for an existing row")
	}
}
