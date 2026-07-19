package replicate

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb"
)

// memStore is an in-memory Storage honoring the interface contract:
// atomic puts (a full []byte swap under lock) and lexicographically
// ordered listings.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newMemStore() *memStore { return &memStore{objects: map[string][]byte{}} }

func (m *memStore) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = append([]byte(nil), data...)
	return nil
}

func (m *memStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("memStore: no such key %q", key)
	}
	return append([]byte(nil), data...), nil
}

func (m *memStore) List(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (m *memStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *memStore) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objects)
}

// quietOpts returns Options tuned for tests: tiny chunks so multi-chunk
// paths are exercised, and a Logf that routes into the test log.
func quietOpts(t *testing.T) Options {
	t.Helper()
	return Options{
		MaxChunkBytes: 64, // force many chunks even for small datasets
		Logf:          func(format string, args ...any) { t.Logf(format, args...) },
	}
}

// TestEngineShipAndRestore is the end-to-end path the church-platform
// deployment will run: a relational engine takes writes, the replicator
// ships to object storage, and Restore + bytdb.Open on a fresh path
// yields the same rows.
func TestEngineShipAndRestore(t *testing.T) {
	dir := t.TempDir()
	e, err := bytdb.Open(filepath.Join(dir, "src.db"))
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
	if err := e.Insert("members", 1, "ada"); err != nil {
		t.Fatal(err)
	}

	store := newMemStore()
	r := New(e, store, quietOpts(t))
	ctx := context.Background()
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}

	// Post-ship writes must ship incrementally on the next call.
	if err := e.Insert("members", 2, "grace"); err != nil {
		t.Fatal(err)
	}
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}

	st := r.Status()
	if st.Generation == "" || st.Watermark == 0 || st.LastError != nil {
		t.Fatalf("bad status after ship: %+v", st)
	}

	restorePath := filepath.Join(dir, "restored.db")
	info, err := Restore(ctx, store, "", restorePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Chunks < 2 {
		t.Fatalf("expected multi-chunk restore with 64-byte chunks, got %d", info.Chunks)
	}
	if info.Bytes != st.Watermark {
		t.Fatalf("restored %d bytes, shipped watermark %d", info.Bytes, st.Watermark)
	}

	re, err := bytdb.Open(restorePath)
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	for id, wantName := range map[int]string{1: "ada", 2: "grace"} {
		row, ok, err := re.Get("members", id)
		if err != nil || !ok {
			t.Fatalf("restored row %d: ok=%v err=%v", id, ok, err)
		}
		if got := row.Col("name"); got != wantName {
			t.Fatalf("restored row %d name = %v, want %q", id, got, wantName)
		}
	}
}

// TestShipNowIdempotent verifies a ship with nothing new appended
// uploads nothing (object count stays flat).
func TestShipNowIdempotent(t *testing.T) {
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
	before := store.count()
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	if store.count() != before {
		t.Fatalf("idle ship changed object count: %d -> %d", before, store.count())
	}
}

// TestCompactionRollsGeneration drives a compaction between ships and
// verifies the replicator starts a new generation, that restore prefers
// it, and that pruning eventually drops old generations.
func TestCompactionRollsGeneration(t *testing.T) {
	dir := t.TempDir()
	db, err := btypedb.Open(filepath.Join(dir, "src.db"),
		btypedb.StringCodec, btypedb.StringCodec, btypedb.WithAutoCompactDisabled())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store := newMemStore()
	opts := quietOpts(t)
	opts.RetainGenerations = 2
	r := New(db, store, opts)
	ctx := context.Background()

	if err := db.Set("a", "1"); err != nil {
		t.Fatal(err)
	}
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	gen0 := r.Status().Generation

	if err := db.Set("b", "2"); err != nil {
		t.Fatal(err)
	}
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	gen1 := r.Status().Generation
	if gen1 == gen0 {
		t.Fatal("generation did not roll after compaction")
	}
	if gen1 < gen0 {
		t.Fatalf("generation IDs not chronological: %s then %s", gen0, gen1)
	}

	restorePath := filepath.Join(dir, "restored.db")
	info, err := Restore(ctx, store, "", restorePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Generation != gen1 {
		t.Fatalf("restore used generation %s, want newest %s", info.Generation, gen1)
	}
	rdb, err := btypedb.Open(restorePath, btypedb.StringCodec, btypedb.StringCodec)
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	if v, ok := rdb.Get("b"); !ok || v != "2" {
		t.Fatalf("restored[b] = %q,%v", v, ok)
	}

	// A second compaction rolls a third generation; with
	// RetainGenerations=2, gen0 must be pruned once the roll happens.
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	keys, err := store.List(ctx, "gen/"+gen0+"/")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("generation %s should have been pruned, still has %d objects", gen0, len(keys))
	}
}

// TestRestorePrefixIsolation verifies two databases sharing a bucket
// under different prefixes restore independently.
func TestRestorePrefixIsolation(t *testing.T) {
	dir := t.TempDir()
	store := newMemStore()
	ctx := context.Background()

	for _, site := range []string{"alpha", "beta"} {
		db, err := btypedb.Open(filepath.Join(dir, site+".db"), btypedb.StringCodec, btypedb.StringCodec)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Set("site", site); err != nil {
			t.Fatal(err)
		}
		opts := quietOpts(t)
		opts.Prefix = "sites/" + site // no trailing slash on purpose; must be normalized
		r := New(db, store, opts)
		if err := r.ShipNow(ctx); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}

	for _, site := range []string{"alpha", "beta"} {
		restorePath := filepath.Join(dir, site+"-restored.db")
		if _, err := Restore(ctx, store, "sites/"+site+"/", restorePath); err != nil {
			t.Fatal(err)
		}
		rdb, err := btypedb.Open(restorePath, btypedb.StringCodec, btypedb.StringCodec)
		if err != nil {
			t.Fatal(err)
		}
		if v, ok := rdb.Get("site"); !ok || v != site {
			t.Fatalf("prefix %s restored wrong data: %q,%v", site, v, ok)
		}
		rdb.Close()
	}
}

// TestRestoreSkipsEmptyGeneration verifies restore falls back past a
// generation that never got a chunk at offset zero (source died before
// its first upload).
func TestRestoreSkipsEmptyGeneration(t *testing.T) {
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
	goodGen := r.Status().Generation

	// A later, broken generation: its chain starts at a nonzero offset,
	// as if the first PUT never landed. "zz..." sorts after any
	// timestamp ID, so restore sees it first.
	brokenKey := fmt.Sprintf("gen/zzzz-broken/%016x-%016x.wlog", 64, 128)
	if err := store.Put(ctx, brokenKey, make([]byte, 64)); err != nil {
		t.Fatal(err)
	}

	restorePath := filepath.Join(dir, "restored.db")
	info, err := Restore(ctx, store, "", restorePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Generation != goodGen {
		t.Fatalf("restore picked %s, want fallback to %s", info.Generation, goodGen)
	}
}

// TestStartCloseLoop exercises the background loop: writes committed
// between ticks must be in the store after Close's final flush, with no
// explicit ShipNow call anywhere.
func TestStartCloseLoop(t *testing.T) {
	dir := t.TempDir()
	db, err := btypedb.Open(filepath.Join(dir, "src.db"), btypedb.StringCodec, btypedb.StringCodec)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store := newMemStore()
	opts := quietOpts(t)
	opts.Interval = time.Hour // only Close's final flush can possibly ship
	r := New(db, store, opts)
	r.Start()

	if err := db.Set("committed-before-close", "yes"); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil { // Close is idempotent
		t.Fatal(err)
	}

	restorePath := filepath.Join(dir, "restored.db")
	if _, err := Restore(context.Background(), store, "", restorePath); err != nil {
		t.Fatal(err)
	}
	rdb, err := btypedb.Open(restorePath, btypedb.StringCodec, btypedb.StringCodec)
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	if v, ok := rdb.Get("committed-before-close"); !ok || v != "yes" {
		t.Fatalf("final flush lost data: %q,%v", v, ok)
	}
}

// TestRestoreNoReplica pins the empty-store behavior.
func TestRestoreNoReplica(t *testing.T) {
	_, err := Restore(context.Background(), newMemStore(), "", filepath.Join(t.TempDir(), "x.db"))
	if !errors.Is(err, ErrNoReplica) {
		t.Fatalf("err = %v, want ErrNoReplica", err)
	}
}

// TestRestoreChunkSizeMismatch verifies a corrupted chunk (size differs
// from its name's declared range) fails the restore loudly instead of
// silently splicing shifted bytes.
func TestRestoreChunkSizeMismatch(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()
	key := fmt.Sprintf("gen/20990101t000000.000000000-dead/%016x-%016x.wlog", 0, 100)
	if err := store.Put(ctx, key, make([]byte, 60)); err != nil { // 40 bytes short
		t.Fatal(err)
	}
	_, err := Restore(ctx, store, "", filepath.Join(t.TempDir(), "x.db"))
	if err == nil || !strings.Contains(err.Error(), "chunk size mismatch") {
		t.Fatalf("err = %v, want chunk size mismatch", err)
	}
}
