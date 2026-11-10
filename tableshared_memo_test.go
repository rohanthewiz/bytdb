package bytdb

// Deep-look D-40: Engine.TableShared memoizes per catalog version. Every
// kind of DDL must make the next lookup see the new catalog.

import (
	"path/filepath"
	"sync"
	"testing"
)

func TestTableSharedMemoFollowsDDL(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "db"))
	defer e.Close()
	if e.TableShared("m") != nil {
		t.Fatal("absent table resolved")
	}
	if _, err := e.CreateTable("m", []Column{{Name: "id", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	d := e.TableShared("m")
	if d == nil || len(d.Columns) != 1 {
		t.Fatalf("after CREATE: %+v", d)
	}
	if e.TableShared("m") != d {
		t.Fatal("second lookup at the same catalog version was not memoized")
	}
	if err := e.AddColumn("m", Column{Name: "x", Type: TString}); err != nil {
		t.Fatal(err)
	}
	if d := e.TableShared("m"); d == nil || len(d.Columns) != 2 {
		t.Fatalf("after ADD COLUMN: %+v", d)
	}
	if err := e.RenameTable("m", "m2"); err != nil {
		t.Fatal(err)
	}
	if e.TableShared("m") != nil || e.TableShared("m2") == nil {
		t.Fatal("rename not visible")
	}
	if err := e.DropTable("m2"); err != nil {
		t.Fatal(err)
	}
	if e.TableShared("m2") != nil {
		t.Fatal("dropped table still resolves")
	}
}

// Lookups racing DDL never see a catalog older than the last DDL that
// returned before the lookup began.
func TestTableSharedMemoRace(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "db"))
	defer e.Close()
	if _, err := e.CreateTable("r", []Column{{Name: "id", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					e.TableShared("r")
					e.TableShared("nope")
				}
			}
		}()
	}
	for i := range 30 {
		name := "c" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if err := e.AddColumn("r", Column{Name: name, Type: TInt}); err != nil {
			t.Fatal(err)
		}
		if d := e.TableShared("r"); d == nil || len(d.Columns) != i+2 {
			t.Fatalf("after ADD COLUMN %d: %d columns", i, len(d.Columns))
		}
	}
	close(stop)
	wg.Wait()
}
