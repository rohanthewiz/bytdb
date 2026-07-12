package bytdb

import (
	"path/filepath"
	"testing"
)

// TestEngineBackup exercises the engine-level online backup: the copy
// must be an openable database holding exactly the catalog and rows
// committed before the backup was taken — later writes excluded.
func TestEngineBackup(t *testing.T) {
	dir := t.TempDir()
	e := openEngine(t, filepath.Join(dir, "test.db"))
	defer e.Close()
	usersTable(t, e)

	if err := e.Insert("users", 1, "ada", 8.25, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("users", 2, "grace", 9.5, false, nil); err != nil {
		t.Fatal(err)
	}

	bak := filepath.Join(dir, "test.backup")
	if err := e.Backup(bak); err != nil {
		t.Fatal(err)
	}

	// Post-backup changes — a new row and a whole new table — must not
	// leak into the copy.
	if err := e.Insert("users", 3, "alan", 7.0, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTable("later", []Column{{Name: "id", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}

	restored := openEngine(t, bak)
	defer restored.Close()

	rows := collect(t, restored.Scan("users"))
	if len(rows) != 2 {
		t.Fatalf("restored backup has %d rows, want 2", len(rows))
	}
	if rows[0].Col("name") != "ada" || rows[1].Col("name") != "grace" {
		t.Fatalf("restored rows wrong: %v", rows)
	}
	if restored.Table("later") != nil {
		t.Fatal("post-backup table leaked into the backup")
	}
}
