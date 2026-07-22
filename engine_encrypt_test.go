package bytdb

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/btypedb"
)

// encKey is a fixed 32-byte master for the engine-level encryption tests.
func encKey() []byte { return bytes.Repeat([]byte{0x3C}, 32) }

// TestEngineEncryptionRoundTrip drives the encrypted WAL through the full
// relational layer: create a table, insert rows, reopen with the key, and
// confirm both the catalog and the rows survive — while the plaintext string
// column never appears on disk.
func TestEngineEncryptionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "enc.db")

	e, err := Open(path, WithEncryptionKey(encKey()))
	if err != nil {
		t.Fatal(err)
	}
	usersTable(t, e)
	if err := e.Insert("users", 1, "ada-SECRET", 8.25, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("users", 2, "grace-SECRET", 9.5, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	// The plaintext column value must not be readable in the on-disk log.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("ada-SECRET")) || bytes.Contains(raw, []byte("grace-SECRET")) {
		t.Fatalf("plaintext row value found on disk")
	}

	// Reopen with the key: catalog + rows intact.
	e2, err := Open(path, WithEncryptionKey(encKey()))
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	if e2.Table("users") == nil {
		t.Fatalf("users table lost after reopen")
	}
	rows := collect(t, e2.Scan("users"))
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].Col("name") != "ada-SECRET" || rows[1].Col("name") != "grace-SECRET" {
		t.Fatalf("rows wrong after encrypted reopen: %v", rows)
	}
}

// TestEngineEncryptionWrongKey: the wrong key is rejected at Open, surfaced
// through the engine layer as btypedb.ErrWrongKey.
func TestEngineEncryptionWrongKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "enc.db")

	e, err := Open(path, WithEncryptionKey(encKey()))
	if err != nil {
		t.Fatal(err)
	}
	usersTable(t, e)
	if err := e.Insert("users", 1, "ada", 8.25, true, nil); err != nil {
		t.Fatal(err)
	}
	e.Close()

	wrong := bytes.Repeat([]byte{0x11}, 32)
	if _, err := Open(path, WithEncryptionKey(wrong)); !errors.Is(err, btypedb.ErrWrongKey) {
		t.Fatalf("got %v, want btypedb.ErrWrongKey", err)
	}
	if _, err := Open(path); !errors.Is(err, btypedb.ErrKeyRequired) {
		t.Fatalf("no-key open: got %v, want btypedb.ErrKeyRequired", err)
	}
}

// TestEngineEncryptionBackup: an online backup of an encrypted engine is
// itself ciphertext (raw-byte copy) and opens only with the key.
func TestEngineEncryptionBackup(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	bak := filepath.Join(dir, "src.backup")

	e, err := Open(src, WithEncryptionKey(encKey()))
	if err != nil {
		t.Fatal(err)
	}
	usersTable(t, e)
	if err := e.Insert("users", 1, "ada-SECRET", 8.25, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.Backup(bak); err != nil {
		t.Fatal(err)
	}
	e.Close()

	raw, err := os.ReadFile(bak)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("ada-SECRET")) {
		t.Fatalf("plaintext value found in encrypted backup")
	}
	if _, err := Open(bak); !errors.Is(err, btypedb.ErrKeyRequired) {
		t.Fatalf("backup opened without key: got %v, want ErrKeyRequired", err)
	}

	restored, err := Open(bak, WithEncryptionKey(encKey()))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	rows := collect(t, restored.Scan("users"))
	if len(rows) != 1 || rows[0].Col("name") != "ada-SECRET" {
		t.Fatalf("restored backup rows wrong: %v", rows)
	}
}
