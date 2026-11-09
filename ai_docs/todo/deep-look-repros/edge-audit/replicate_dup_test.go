package edgeaudit

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/replicate"
)

// memStore is an in-memory replicate.Storage. ambiguousPuts makes the next
// N chunk Puts behave like an S3 PUT whose response was lost: the object
// IS stored, but the caller sees an error (timeout / connection reset
// after the server committed).
type memStore struct {
	mu            sync.Mutex
	objects       map[string][]byte
	ambiguousPuts int
}

func newMemStore() *memStore { return &memStore{objects: map[string][]byte{}} }

func (m *memStore) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = append([]byte(nil), data...)
	if m.ambiguousPuts > 0 && strings.HasSuffix(key, ".wlog") {
		m.ambiguousPuts--
		return errors.New("simulated: response lost after the store committed the object")
	}
	return nil
}
func (m *memStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("no such key %q", key)
	}
	return append([]byte(nil), d...), nil
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

func openMembers(t *testing.T, path string) *bytdb.Engine {
	t.Helper()
	e, err := bytdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTable("members", []bytdb.Column{
		{Name: "id", Type: bytdb.TInt}, {Name: "name", Type: bytdb.TString},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	return e
}

func rowsIn(t *testing.T, path string) []int {
	t.Helper()
	re, err := bytdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	var got []int
	for id := 1; id <= 5; id++ {
		if _, ok, _ := re.Get("members", id); ok {
			got = append(got, id)
		}
	}
	return got
}

func opts(t *testing.T) replicate.Options {
	return replicate.Options{Logf: func(f string, a ...any) { t.Logf(f, a...) }}
}

// One lost PUT response, then the log grows before the retry: the retry
// uploads [0,S2) next to the already-stored [0,S1).
func TestAmbiguousPutThenGrowth_Incomplete(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	e := openMembers(t, filepath.Join(dir, "src.db"))
	defer e.Close()
	e.Insert("members", 1, "ada")

	store := newMemStore()
	r := replicate.New(e, store, opts(t))
	store.ambiguousPuts = 1
	t.Logf("ship 1: %v", r.ShipNow(ctx))
	e.Insert("members", 2, "grace")
	if err := r.ShipNow(ctx); err != nil {
		t.Fatalf("ship 2: %v", err)
	}
	t.Logf("status after successful ship: %+v", r.Status())
	keys, _ := store.List(ctx, "")
	for _, k := range keys {
		t.Logf("  object %s", k)
	}
	_, err := replicate.Restore(ctx, store, "", filepath.Join(dir, "restored.db"))
	if err != nil {
		t.Errorf("BUG: fully shipped, manifested generation is unrestorable: %v", err)
	}
}

// Same, but an older complete generation exists: Restore silently returns it.
func TestAmbiguousPutThenGrowth_SilentRollback(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	e := openMembers(t, filepath.Join(dir, "src.db"))
	defer e.Close()
	e.Insert("members", 1, "ada")

	store := newMemStore()
	// "Yesterday's process": generation A, complete with row 1.
	if err := replicate.New(e, store, opts(t)).ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	// "Today's process": a new Replicator rolls generation B.
	r := replicate.New(e, store, opts(t))
	e.Insert("members", 2, "grace")
	store.ambiguousPuts = 1
	t.Logf("ship 1 (gen B): %v", r.ShipNow(ctx))
	e.Insert("members", 3, "linus")
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	e.Insert("members", 4, "ken")
	if err := r.ShipNow(ctx); err != nil { // LastError is nil, status looks healthy
		t.Fatal(err)
	}
	st := r.Status()
	t.Logf("replicator status: gen=%s watermark=%d lastErr=%v", st.Generation, st.Watermark, st.LastError)

	dst := filepath.Join(dir, "restored.db")
	info, err := replicate.Restore(ctx, store, "", dst)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	got := rowsIn(t, dst)
	t.Logf("restored generation %s (%d bytes); current generation %s (%d bytes); rows restored: %v",
		info.Generation, info.Bytes, st.Generation, st.Watermark, got)
	if info.Generation != st.Generation || len(got) != 4 {
		t.Errorf("BUG: Restore silently rolled back to an older generation; rows 2-4 were shipped and acknowledged (LastError=nil) but are missing")
	}
}
