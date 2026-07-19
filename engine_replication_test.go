package bytdb

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestEngineBackupTo verifies the streaming backup: the bytes written
// to an arbitrary io.Writer must themselves be an openable database
// with the pre-backup rows.
func TestEngineBackupTo(t *testing.T) {
	dir := t.TempDir()
	e := openEngine(t, filepath.Join(dir, "test.db"))
	defer e.Close()
	usersTable(t, e)
	if err := e.Insert("users", 1, "ada", 8.25, true, nil); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	n, err := e.BackupTo(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(buf.Len()) {
		t.Fatalf("reported %d bytes, wrote %d", n, buf.Len())
	}

	restorePath := filepath.Join(dir, "streamed.db")
	if err := os.WriteFile(restorePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	restored := openEngine(t, restorePath)
	defer restored.Close()
	row, ok, err := restored.Get("users", 1)
	if err != nil || !ok {
		t.Fatalf("restored row: ok=%v err=%v", ok, err)
	}
	if row.Col("name") != "ada" {
		t.Fatalf("restored name = %v", row.Col("name"))
	}
}

// TestEngineLogRange verifies the replication passthroughs: shipping
// the engine's whole log via LogState + ReadLogRange reproduces the
// database, SQL catalog included.
func TestEngineLogRange(t *testing.T) {
	dir := t.TempDir()
	e := openEngine(t, filepath.Join(dir, "test.db"))
	defer e.Close()
	usersTable(t, e)
	if err := e.Insert("users", 7, "grace", 9.5, false, nil); err != nil {
		t.Fatal(err)
	}

	epoch, size, err := e.LogState()
	if err != nil {
		t.Fatal(err)
	}
	var replica bytes.Buffer
	if _, err := e.ReadLogRange(epoch, 0, size, &replica); err != nil {
		t.Fatal(err)
	}

	restorePath := filepath.Join(dir, "replica.db")
	if err := os.WriteFile(restorePath, replica.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	restored := openEngine(t, restorePath)
	defer restored.Close()
	row, ok, err := restored.Get("users", 7)
	if err != nil || !ok {
		t.Fatalf("replica row: ok=%v err=%v", ok, err)
	}
	if row.Col("name") != "grace" {
		t.Fatalf("replica name = %v", row.Col("name"))
	}
}
