package replicate

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb"
)

// TestEncryptedShipAndRestore proves the at-rest encryption flows through
// replication unchanged: because the replicator ships raw log bytes, the
// object store receives ciphertext (the plaintext column value never appears
// in any shipped chunk), a Restore reconstructs a byte-identical encrypted
// file, and that file needs the same key to Open — without it, Open fails with
// ErrKeyRequired.
func TestEncryptedShipAndRestore(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{0x7E}, 32)

	e, err := bytdb.Open(filepath.Join(dir, "src.db"), bytdb.WithEncryptionKey(key))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	if _, err := e.CreateTable("members", []bytdb.Column{
		{Name: "id", Type: bytdb.TInt},
		{Name: "name", Type: bytdb.TString},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("members", 1, "ada-SECRET"); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("members", 2, "grace-SECRET"); err != nil {
		t.Fatal(err)
	}

	store := newMemStore()
	r := New(e, store, quietOpts(t))
	ctx := context.Background()
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}

	// Every shipped object must be ciphertext: the plaintext column value is
	// absent across all chunks.
	keys, err := store.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		data, err := store.Get(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("SECRET")) {
			t.Fatalf("plaintext value found in shipped object %q", k)
		}
	}

	// Restore reassembles the encrypted file; it needs the key to Open.
	restorePath := filepath.Join(dir, "restored.db")
	if _, err := Restore(ctx, store, "", restorePath); err != nil {
		t.Fatal(err)
	}

	if _, err := bytdb.Open(restorePath); !errors.Is(err, btypedb.ErrKeyRequired) {
		t.Fatalf("restore opened without key: got %v, want ErrKeyRequired", err)
	}

	re, err := bytdb.Open(restorePath, bytdb.WithEncryptionKey(key))
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	for id, wantName := range map[int]string{1: "ada-SECRET", 2: "grace-SECRET"} {
		row, ok, err := re.Get("members", id)
		if err != nil || !ok {
			t.Fatalf("restored row %d: ok=%v err=%v", id, ok, err)
		}
		if got := row.Col("name"); got != wantName {
			t.Fatalf("restored row %d name = %v, want %q", id, got, wantName)
		}
	}
}
