package replicate

// manifest_test.go: the generation-completeness marker and the restore
// selection it drives. The core guarantee is that a barely-started
// generation from a fresh compaction cannot silently shadow a complete
// older one (a roll-backward).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/btypedb"
)

// TestManifestWrittenOnCatchUp: a ship that drains the log writes a
// manifest certifying the generation complete, and an idle re-ship writes
// nothing further.
func TestManifestWrittenOnCatchUp(t *testing.T) {
	dir := t.TempDir()
	db, err := btypedb.Open(filepath.Join(dir, "src.db"), btypedb.StringCodec, btypedb.StringCodec)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Set("k", "v"); err != nil {
		t.Fatal(err)
	}

	store := newMemStore()
	r := New(db, store, quietOpts(t))
	ctx := context.Background()
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	st := r.Status()

	data, err := store.Get(ctx, manifestKey("", st.Generation))
	if err != nil {
		t.Fatalf("no manifest after catch-up: %v", err)
	}
	var man generationManifest
	if err := json.Unmarshal(data, &man); err != nil {
		t.Fatal(err)
	}
	if man.Generation != st.Generation || man.Size != st.Watermark || man.Epoch != st.Epoch {
		t.Fatalf("manifest %+v; want gen %s size %d epoch %d", man, st.Generation, st.Watermark, st.Epoch)
	}

	// An idle ship (nothing new appended, size unchanged) must not re-PUT
	// the manifest.
	before := store.count()
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	if store.count() != before {
		t.Fatalf("idle ship wrote objects: %d -> %d", before, store.count())
	}
}

// TestRestorePrefersCompleteOverPartialNewer is the roll-backward
// regression: a complete (manifested) generation must win over a newer
// generation that shipped only its first chunk and never caught up — even
// though that partial generation's chain starts at zero and so looks
// restorable to the legacy rule.
func TestRestorePrefersCompleteOverPartialNewer(t *testing.T) {
	dir := t.TempDir()
	db, err := btypedb.Open(filepath.Join(dir, "src.db"), btypedb.StringCodec, btypedb.StringCodec)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Set("complete", "yes"); err != nil {
		t.Fatal(err)
	}

	store := newMemStore()
	r := New(db, store, quietOpts(t))
	ctx := context.Background()
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	completeGen := r.Status().Generation

	// A newer generation that never caught up: one chunk from offset zero,
	// no manifest. "zzzz-..." sorts after any timestamp-based generation.
	partialGen := "zzzz-partial"
	partialKey := fmt.Sprintf("gen/%s/%016x-%016x.wlog", partialGen, 0, 64)
	if err := store.Put(ctx, partialKey, make([]byte, 64)); err != nil {
		t.Fatal(err)
	}

	restorePath := filepath.Join(dir, "restored.db")
	info, err := Restore(ctx, store, "", restorePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Generation != completeGen {
		t.Fatalf("restore rolled back to partial %s; want complete %s", info.Generation, completeGen)
	}
	rdb, err := btypedb.Open(restorePath, btypedb.StringCodec, btypedb.StringCodec)
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	if v, ok := rdb.Get("complete"); !ok || v != "yes" {
		t.Fatalf("restored wrong data: %q,%v", v, ok)
	}
}

// TestRestoreSkipsDamagedManifestedGeneration: a newer generation
// certified complete but now missing chunks below its certified size is
// skipped for an older complete one, rather than restored as a fragment.
func TestRestoreSkipsDamagedManifestedGeneration(t *testing.T) {
	dir := t.TempDir()
	db, err := btypedb.Open(filepath.Join(dir, "src.db"), btypedb.StringCodec, btypedb.StringCodec)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Set("complete", "yes"); err != nil {
		t.Fatal(err)
	}

	store := newMemStore()
	r := New(db, store, quietOpts(t))
	ctx := context.Background()
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	completeGen := r.Status().Generation

	// A newer generation certified to size 200 whose chain reaches only 64.
	damagedGen := "zzzz-damaged"
	if err := store.Put(ctx, fmt.Sprintf("gen/%s/%016x-%016x.wlog", damagedGen, 0, 64), make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	man, _ := json.Marshal(generationManifest{Generation: damagedGen, Epoch: 9, Size: 200})
	if err := store.Put(ctx, manifestKey("", damagedGen), man); err != nil {
		t.Fatal(err)
	}

	info, err := Restore(ctx, store, "", filepath.Join(dir, "restored.db"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Generation != completeGen {
		t.Fatalf("restore used damaged %s; want complete %s", info.Generation, completeGen)
	}
}

// TestRestoreIncompleteReplica: when the only certified generations are
// short, Restore refuses (ErrIncompleteReplica) rather than silently
// restore a truncated fragment.
func TestRestoreIncompleteReplica(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()
	gen := "20990101t000000.000000000-dead"
	if err := store.Put(ctx, fmt.Sprintf("gen/%s/%016x-%016x.wlog", gen, 0, 64), make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	man, _ := json.Marshal(generationManifest{Generation: gen, Size: 200})
	if err := store.Put(ctx, manifestKey("", gen), man); err != nil {
		t.Fatal(err)
	}
	_, err := Restore(ctx, store, "", filepath.Join(t.TempDir(), "x.db"))
	if !errors.Is(err, ErrIncompleteReplica) {
		t.Fatalf("err = %v, want ErrIncompleteReplica", err)
	}
}
