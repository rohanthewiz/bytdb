package bytdb

// identity_test.go: identity (auto-increment) columns at the engine
// level — NULL draws the next value, explicit values bump the counter
// past themselves, identity implies NOT NULL, counters survive reopen
// and are cleaned up with their column or table.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/btypedb"
)

func idTable(t *testing.T, e *Engine) *TableDesc {
	t.Helper()
	desc, err := e.CreateTable("t",
		[]Column{{Name: "id", Type: TInt, Identity: true}, {Name: "name", Type: TString}}, "id")
	if err != nil {
		t.Fatal(err)
	}
	return desc
}

func TestIdentityInsertDrawsValues(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	idTable(t, e)

	for _, name := range []string{"ada", "grace", "alan"} {
		if err := e.Insert("t", nil, name); err != nil {
			t.Fatal(err)
		}
	}
	row, ok, err := e.Get("t", 2)
	if err != nil || !ok {
		t.Fatalf("Get(t, 2): ok=%v err=%v", ok, err)
	}
	if row.Vals[1] != "grace" {
		t.Fatalf("row 2 = %v, want grace", row.Vals)
	}
}

func TestIdentityExplicitValueBumpsCounter(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	idTable(t, e)

	// An explicit value must push the counter past itself...
	if err := e.Insert("t", 10, "ada"); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("t", nil, "grace"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.Get("t", 11); !ok {
		t.Fatal("insert after explicit 10 did not draw 11")
	}
	// ...but an explicit negative value never touches it.
	if err := e.Insert("t", -5, "alan"); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("t", nil, "edsger"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.Get("t", 12); !ok {
		t.Fatal("negative explicit value moved the counter")
	}
	// An explicit value below the counter does not move it backward.
	if err := e.Insert("t", 3, "barbara"); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("t", nil, "mary"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.Get("t", 13); !ok {
		t.Fatal("low explicit value moved the counter backward")
	}
}

func TestIdentityCounterSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	e := openEngine(t, path)
	idTable(t, e)
	if err := e.Insert("t", nil, "ada"); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	if err := e2.Insert("t", nil, "grace"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e2.Get("t", 2); !ok {
		t.Fatal("counter restarted after reopen")
	}
}

// TestIdentityImpliesNotNull: a non-key identity column rejects an
// UPDATE to NULL (inserts never see NULL — the engine fills it).
func TestIdentityImpliesNotNull(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	_, err := e.CreateTable("t", []Column{
		{Name: "id", Type: TInt},
		{Name: "n", Type: TInt, Identity: true},
	}, "id")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("t", 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.Get("t", 1); !ok {
		t.Fatal("row missing")
	}
	if _, err := e.Update("t", []any{1}, map[string]any{"n": nil}); err == nil ||
		!strings.Contains(err.Error(), "not-null") {
		t.Fatalf("update identity to NULL: got %v, want a not-null violation", err)
	}
}

func TestIdentityValidation(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()

	_, err := e.CreateTable("t", []Column{
		{Name: "id", Type: TString, Identity: true}}, "id")
	if err == nil || !strings.Contains(err.Error(), "identity column must be an int") {
		t.Fatalf("string identity column: got %v, want a type error", err)
	}

	idTable(t, e)
	err = e.AddColumn("t", Column{Name: "n2", Type: TInt, Identity: true})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("ADD identity COLUMN: got %v, want unsupported", err)
	}
}

// counterExists reports whether an identity counter key is present in
// the raw keyspace — how the cleanup tests observe DDL side effects.
func counterExists(t *testing.T, e *Engine, tableID uint64, colID uint32) bool {
	t.Helper()
	found := false
	err := e.kv.View(func(tx *btypedb.Tx[string, []byte]) error {
		found = tx.Contains(identitySeqKey(tableID, colID))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestIdentityCounterCleanup(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()

	// DropColumn removes the dropped column's counter.
	desc, err := e.CreateTable("t", []Column{
		{Name: "id", Type: TInt},
		{Name: "n", Type: TInt, Identity: true},
	}, "id")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("t", 1, nil); err != nil {
		t.Fatal(err)
	}
	colID := desc.Columns[1].ID
	if !counterExists(t, e, desc.ID, colID) {
		t.Fatal("counter missing after first draw")
	}
	if err := e.DropColumn("t", "n"); err != nil {
		t.Fatal(err)
	}
	if counterExists(t, e, desc.ID, colID) {
		t.Fatal("counter survives DropColumn")
	}

	// DropTable removes every identity counter of the table.
	if err := e.DropTable("t"); err != nil {
		t.Fatal(err)
	}
	desc2 := idTable(t, e)
	if err := e.Insert("t", nil, "ada"); err != nil {
		t.Fatal(err)
	}
	if err := e.DropTable("t"); err != nil {
		t.Fatal(err)
	}
	if counterExists(t, e, desc2.ID, desc2.Columns[0].ID) {
		t.Fatal("counter survives DropTable")
	}
}

// TestIdentityFailedInsertKeepsMonotonicity: a duplicate-key failure
// after the draw may burn a value (a gap), but never repeats one.
func TestIdentityFailedInsertKeepsMonotonicity(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	idTable(t, e)
	if err := e.Insert("t", nil, "ada"); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("t", 1, "dup"); err == nil {
		t.Fatal("duplicate key insert succeeded")
	}
	if err := e.Insert("t", nil, "grace"); err != nil {
		t.Fatal(err)
	}
	rows := 0
	for _, err := range e.Scan("t") {
		if err != nil {
			t.Fatal(err)
		}
		rows++
	}
	if rows != 2 {
		t.Fatalf("%d rows, want 2", rows)
	}
}
