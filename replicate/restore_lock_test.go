package replicate

// restore_lock_test.go: Restore refuses to write over a live database.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/rohanthewiz/btypedb"
)

// TestRestoreRefusesLiveDestination: restoring onto the path of an open
// database — the source itself, or another live DB — fails with
// ErrLocked before anything is written, and the live DB keeps working;
// once that DB closes, the same restore goes through.
func TestRestoreRefusesLiveDestination(t *testing.T) {
	switch runtime.GOOS {
	case "darwin", "dragonfly", "freebsd", "linux", "netbsd", "openbsd", "windows":
	default:
		t.Skip("no file locking on " + runtime.GOOS)
	}
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	src, err := btypedb.Open(srcPath, btypedb.StringCodec, btypedb.StringCodec)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := src.Set("from", "src"); err != nil {
		t.Fatal(err)
	}
	store := newMemStore()
	ctx := context.Background()
	if err := New(src, store, quietOpts(t)).ShipNow(ctx); err != nil {
		t.Fatal(err)
	}

	// The operator's classic slip: restoring over the very database that
	// is still running.
	if _, err := Restore(ctx, store, "", srcPath); !errors.Is(err, btypedb.ErrLocked) {
		t.Fatalf("Restore onto the live source: want ErrLocked, got %v", err)
	}

	destPath := filepath.Join(dir, "dest.db")
	dest, err := btypedb.Open(destPath, btypedb.StringCodec, btypedb.StringCodec)
	if err != nil {
		t.Fatal(err)
	}
	if err := dest.Set("from", "dest"); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, store, "", destPath); !errors.Is(err, btypedb.ErrLocked) {
		t.Fatalf("Restore onto a live db: want ErrLocked, got %v", err)
	}
	if _, err := os.Stat(destPath + ".restore-tmp"); !os.IsNotExist(err) {
		t.Fatalf("refused Restore left a temp file behind (stat err %v)", err)
	}
	// A write after the refused attempt must survive a reopen, which it
	// would not had the restore swapped the file under dest.
	if err := dest.Set("after", "attempt"); err != nil {
		t.Fatal(err)
	}
	if err := dest.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := btypedb.Open(destPath, btypedb.StringCodec, btypedb.StringCodec)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := reopened.Get("from"); v != "dest" {
		t.Fatalf("dest after refused Restore: from = %q, want dest", v)
	}
	if _, ok := reopened.Get("after"); !ok {
		t.Fatal("dest lost a write made after the refused Restore")
	}
	reopened.Close()

	// Closed, the path is free, and the restore replaces it.
	if _, err := Restore(ctx, store, "", destPath); err != nil {
		t.Fatalf("Restore onto a closed db: %v", err)
	}
	restored, err := btypedb.Open(destPath, btypedb.StringCodec, btypedb.StringCodec)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if v, _ := restored.Get("from"); v != "src" {
		t.Fatalf("restored: from = %q, want src", v)
	}
}
