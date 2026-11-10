package replicate

// Deep-look D-34: generation ordering by sequence number, Status.Sealed
// and LagBytes, per-operation store timeouts, chunk merging, and
// parallel restore downloads.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rohanthewiz/btypedb"
)

// newerGen names a generation one sequence number after gen — newer by
// the ordering restore and pruning use — with label in place of the
// timestamp and random suffix.
func newerGen(gen, label string) string {
	return fmt.Sprintf("g%016d-%s", generationSeq(gen)+1, label)
}

func openKV(t *testing.T, path string, opts ...btypedb.Option) *btypedb.DB[string, string] {
	t.Helper()
	db, err := btypedb.Open(path, btypedb.StringCodec, btypedb.StringCodec, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestGenerationSeqOrdering(t *testing.T) {
	cases := []struct {
		gen string
		seq uint64
	}{
		{"g0000000000000042-20261109t150405.000000000-8f2a1c40", 42},
		{newGenerationID(7), 7},
		{"20261109t150405.000000000-8f2a1c40", 0}, // legacy timestamp name
		{"g", 0},
		{"gx-1", 0},
		{"2026a", 0},
	}
	for _, c := range cases {
		if got := generationSeq(c.gen); got != c.seq {
			t.Errorf("generationSeq(%q) = %d, want %d", c.gen, got, c.seq)
		}
	}
	// Sequence beats timestamp, and every sequenced name is newer than
	// every legacy one, however far in the future its timestamp.
	gens := []string{
		"g0000000000000003-20200101t000000.000000000-aa",
		"29990101t000000.000000000-ff", // legacy, far future
		"g0000000000000002-29990101t000000.000000000-bb",
		"20200101t000000.000000000-00", // legacy
	}
	sortGenerations(gens)
	want := []string{
		"20200101t000000.000000000-00",
		"29990101t000000.000000000-ff",
		"g0000000000000002-29990101t000000.000000000-bb",
		"g0000000000000003-20200101t000000.000000000-aa",
	}
	for i := range want {
		if gens[i] != want[i] {
			t.Fatalf("order = %v\nwant    %v", gens, want)
		}
	}
}

// seedCompleteGen stores a complete-looking generation (one chunk from
// zero plus a manifest certifying it) under name gen.
func seedCompleteGen(t *testing.T, store Storage, gen string) {
	t.Helper()
	ctx := context.Background()
	if err := store.Put(ctx, fmt.Sprintf("gen/%s/%016x-%016x.wlog", gen, 0, 64), make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	man, _ := json.Marshal(generationManifest{Generation: gen, Size: 64})
	if err := store.Put(ctx, manifestKey("", gen), man); err != nil {
		t.Fatal(err)
	}
}

// A replacement host whose clock lags (or a clock stepped backwards)
// writes generation names with older timestamps than the stale ones in
// the bucket. Ordering by name made restore pick the stale generation
// and pruning delete the live one; the sequence number orders them by
// creation regardless of the clock.
func TestGenerationOrderIgnoresClock(t *testing.T) {
	dir := t.TempDir()
	db := openKV(t, filepath.Join(dir, "src.db"))
	if err := db.Set("live", "yes"); err != nil {
		t.Fatal(err)
	}
	store := newMemStore()
	ctx := context.Background()

	// Stale generations from "another host" whose clock ran ahead: one
	// legacy name and one sequenced name, both dated 2999.
	legacy := "29990101t000000.000000000-dead"
	stale := "g0000000000000041-29990101t000000.000000000-beef"
	seedCompleteGen(t, store, legacy)
	seedCompleteGen(t, store, stale)

	opts := quietOpts(t)
	opts.RetainGenerations = 2
	r := New(db, store, opts)
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	live := r.Status().Generation
	if got := generationSeq(live); got != 42 {
		t.Fatalf("live generation %s has sequence %d, want 42 (one past the store's largest)", live, got)
	}

	info, err := Restore(ctx, store, "", filepath.Join(dir, "restored.db"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Generation != live {
		t.Fatalf("restore picked %s, want the live generation %s", info.Generation, live)
	}

	// Retention 2 keeps the live generation and one prior: the stale
	// sequenced one, which is newer than the legacy one. The legacy one
	// is pruned, not the live one.
	if keys, _ := store.List(ctx, "gen/"+legacy+"/"); len(keys) != 0 {
		t.Fatalf("legacy generation not pruned: %v", keys)
	}
	if keys, _ := store.List(ctx, "gen/"+live+"/"); len(keys) == 0 {
		t.Fatal("live generation was pruned")
	}

	// A compaction rolls to 43 without consulting the clock.
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	if got := generationSeq(r.Status().Generation); got != 43 {
		t.Fatalf("after compaction sequence = %d, want 43", got)
	}
	// The manifest records the sequence for operators.
	data, err := store.Get(ctx, manifestKey("", r.Status().Generation))
	if err != nil {
		t.Fatal(err)
	}
	var man generationManifest
	if err := json.Unmarshal(data, &man); err != nil || man.Seq != 43 {
		t.Fatalf("manifest seq = %d (%v), want 43", man.Seq, err)
	}
}

// flakyStore wraps a Storage with switchable failures and stalls.
type flakyStore struct {
	Storage
	failPuts  atomic.Bool
	failLists atomic.Bool
	stallPuts atomic.Bool // block until the operation's context ends
	puts      atomic.Int64
}

func (f *flakyStore) Put(ctx context.Context, key string, data []byte) error {
	f.puts.Add(1)
	if f.stallPuts.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.failPuts.Load() {
		return errors.New("flaky: put refused")
	}
	return f.Storage.Put(ctx, key, data)
}

func (f *flakyStore) List(ctx context.Context, prefix string) ([]string, error) {
	if f.failLists.Load() {
		return nil, errors.New("flaky: list refused")
	}
	return f.Storage.List(ctx, prefix)
}

// Status.Sealed and LagBytes make the post-roll exposure visible: after
// a restart or compaction nothing new is protected until the whole log
// re-uploads, and Status used to report a healthy replicator throughout.
func TestStatusSealedAndLag(t *testing.T) {
	dir := t.TempDir()
	db := openKV(t, filepath.Join(dir, "src.db"), btypedb.WithAutoCompactDisabled())
	for i := range 20 {
		if err := db.Set(fmt.Sprintf("k%02d", i), strings.Repeat("v", 50)); err != nil {
			t.Fatal(err)
		}
	}
	store := &flakyStore{Storage: newMemStore()}
	r := New(db, store, quietOpts(t))
	ctx := context.Background()

	if st := r.Status(); st.Sealed {
		t.Fatalf("sealed before any ship: %+v", st)
	}
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	if st := r.Status(); !st.Sealed || st.LagBytes != 0 {
		t.Fatalf("after a full ship: Sealed=%v LagBytes=%d, want true, 0", st.Sealed, st.LagBytes)
	}

	// Compaction rolls the generation; with uploads failing, the new
	// generation stays unsealed and the whole log is lag.
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	store.failPuts.Store(true)
	if err := r.ShipNow(ctx); err == nil {
		t.Fatal("ship succeeded with puts refused")
	}
	_, size, _ := db.LogState()
	st := r.Status()
	if st.Sealed || st.LagBytes != size || st.Watermark != 0 {
		t.Fatalf("after a failed post-roll ship: Sealed=%v LagBytes=%d Watermark=%d, want false, %d, 0",
			st.Sealed, st.LagBytes, st.Watermark, size)
	}

	store.failPuts.Store(false)
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	if st := r.Status(); !st.Sealed || st.LagBytes != 0 {
		t.Fatalf("after catching up: Sealed=%v LagBytes=%d", st.Sealed, st.LagBytes)
	}
}

// A first roll needs the store's largest sequence number, so a listing
// failure fails the ship (retried next tick) rather than guessing; a
// later roll falls back to the process's own last sequence.
func TestRollListingFailure(t *testing.T) {
	dir := t.TempDir()
	db := openKV(t, filepath.Join(dir, "src.db"), btypedb.WithAutoCompactDisabled())
	if err := db.Set("k", "v"); err != nil {
		t.Fatal(err)
	}
	store := &flakyStore{Storage: newMemStore()}
	r := New(db, store, quietOpts(t))
	ctx := context.Background()

	store.failLists.Store(true)
	if err := r.ShipNow(ctx); err == nil {
		t.Fatal("first roll shipped without knowing the store's sequence numbers")
	}
	if store.puts.Load() != 0 {
		t.Fatalf("%d puts before a generation was chosen", store.puts.Load())
	}
	store.failLists.Store(false)
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	first := generationSeq(r.Status().Generation)

	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	store.failLists.Store(true)
	if err := r.ShipNow(ctx); err != nil {
		t.Fatalf("a later roll with listing down: %v", err)
	}
	if got := generationSeq(r.Status().Generation); got != first+1 {
		t.Fatalf("sequence after fallback roll = %d, want %d", got, first+1)
	}
}

// A PUT whose connection is black-holed mid-body blocks until TCP gives
// up, holding the ship lock all the while. The per-operation timeout
// bounds it.
func TestOpTimeoutBoundsStalledPut(t *testing.T) {
	dir := t.TempDir()
	db := openKV(t, filepath.Join(dir, "src.db"))
	if err := db.Set("k", "v"); err != nil {
		t.Fatal(err)
	}
	store := &flakyStore{Storage: newMemStore()}
	store.stallPuts.Store(true)
	opts := quietOpts(t)
	opts.OpTimeout = 50 * time.Millisecond
	r := New(db, store, opts)

	start := time.Now()
	err := r.ShipNow(context.Background()) // no deadline of its own
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("stalled put held the ship for %v", el)
	}
}

func TestOpContextScalesWithSize(t *testing.T) {
	ctx := context.Background()
	deadline := func(base time.Duration, rate, n int64) time.Duration {
		c, cancel := opContext(ctx, base, rate, n)
		defer cancel()
		d, ok := c.Deadline()
		if !ok {
			return -1
		}
		return time.Until(d)
	}
	small := deadline(30*time.Second, 64<<10, 0)
	big := deadline(30*time.Second, 64<<10, 8<<20) // 8 MiB at 64 KiB/s = 128 s more
	if small > 30*time.Second || small < 29*time.Second {
		t.Fatalf("empty payload deadline %v, want ~30s", small)
	}
	if big < 157*time.Second || big > 158*time.Second {
		t.Fatalf("8 MiB deadline %v, want ~158s", big)
	}
	if d := deadline(-1, 0, 1<<20); d != -1 {
		t.Fatalf("negative base still set a deadline (%v)", d)
	}
}

// genObjects counts a generation's chunk objects.
func genObjects(t *testing.T, store Storage, gen string) int {
	t.Helper()
	keys, err := store.List(context.Background(), "gen/"+gen+"/")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, k := range keys {
		if strings.HasSuffix(k, ".wlog") {
			n++
		}
	}
	return n
}

// A database written a little at a time used to accumulate one object
// per ship until the next compaction. Merging keeps the count
// logarithmic, and the restore is still byte-for-byte the log.
func TestMergeBoundsChunkCount(t *testing.T) {
	dir := t.TempDir()
	db := openKV(t, filepath.Join(dir, "src.db"), btypedb.WithAutoCompactDisabled())
	store := newMemStore()
	opts := quietOpts(t)
	opts.MaxChunkBytes = 1 << 20 // small writes never fill a chunk
	opts.MergeChunks = 4
	r := New(db, store, opts)
	r.deleteGrace = 0
	ctx := context.Background()

	const ships = 300
	for i := range ships {
		if err := db.Set(fmt.Sprintf("k%04d", i), fmt.Sprintf("v%d", i)); err != nil {
			t.Fatal(err)
		}
		if err := r.ShipNow(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.ShipNow(ctx); err != nil { // runs the last grace-expired deletes
		t.Fatal(err)
	}
	gen := r.Status().Generation
	// 300 = 1·256 + 0·64 + 2·16 + 3·4 → base-4 digits 1,0,2,3,0: 6 live
	// chunks. Allow slack for the first ship (the bulk of the log, which
	// may have different tier history) but not a count anywhere near 300.
	if n := genObjects(t, store, gen); n > 12 {
		t.Fatalf("%d chunk objects after %d ships with merging; want O(log)", n, ships)
	}
	if n := len(r.chunks); n > 12 {
		t.Fatalf("replicator tracks %d live chunks", n)
	}

	info, err := Restore(ctx, store, "", filepath.Join(dir, "restored.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, size, _ := db.LogState()
	if info.Bytes != size {
		t.Fatalf("restored %d bytes, log is %d", info.Bytes, size)
	}
	rdb := openKV(t, filepath.Join(dir, "restored.db"))
	for i := range ships {
		if v, ok := rdb.Get(fmt.Sprintf("k%04d", i)); !ok || v != fmt.Sprintf("v%d", i) {
			t.Fatalf("restored k%04d = %q,%v", i, v, ok)
		}
	}
}

// Merged-away chunks outlive the merge by the grace period, so a restore
// that listed before the merge still finds them; restore meanwhile uses
// the merged chunk (the furthest-reaching) and ignores the parts.
func TestMergeKeepsPartsDuringGrace(t *testing.T) {
	dir := t.TempDir()
	db := openKV(t, filepath.Join(dir, "src.db"), btypedb.WithAutoCompactDisabled())
	store := newMemStore()
	opts := quietOpts(t)
	opts.MaxChunkBytes = 1 << 20
	opts.MergeChunks = 4
	r := New(db, store, opts)
	r.deleteGrace = time.Hour
	ctx := context.Background()

	for i := range 16 {
		if err := db.Set(fmt.Sprintf("k%02d", i), "v"); err != nil {
			t.Fatal(err)
		}
		if err := r.ShipNow(ctx); err != nil {
			t.Fatal(err)
		}
	}
	gen := r.Status().Generation
	if len(r.chunks) != 1 {
		t.Fatalf("16 ships at fan-in 4 left %d live chunks, want 1 (two tiers merged)", len(r.chunks))
	}
	// 16 shipped + 4 tier-1 merges + 1 tier-2 merge, none deleted yet.
	if n := genObjects(t, store, gen); n != 21 {
		t.Fatalf("%d objects during grace, want 21", n)
	}
	info, err := Restore(ctx, store, "", filepath.Join(dir, "restored.db"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Chunks != 1 {
		t.Fatalf("restore assembled %d chunks, want the 1 merged chunk", info.Chunks)
	}

	// Once the grace passes the parts go.
	r.shipMu.Lock()
	for i := range r.pendingDeletes {
		r.pendingDeletes[i].after = time.Time{}
	}
	r.shipMu.Unlock()
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	if n := genObjects(t, store, gen); n != 1 {
		t.Fatalf("%d objects after the grace period, want 1", n)
	}
}

// A run whose merge would exceed MaxChunkBytes is not merged: its first
// chunk is pinned, and no object ever exceeds the limit.
func TestMergeRespectsMaxChunkBytes(t *testing.T) {
	dir := t.TempDir()
	db := openKV(t, filepath.Join(dir, "src.db"), btypedb.WithAutoCompactDisabled())
	store := newMemStore()
	opts := quietOpts(t)
	opts.MaxChunkBytes = 300
	opts.MergeChunks = 3
	r := New(db, store, opts)
	r.deleteGrace = 0
	ctx := context.Background()
	for i := range 40 {
		if err := db.Set(fmt.Sprintf("k%02d", i), strings.Repeat("x", 40)); err != nil {
			t.Fatal(err)
		}
		if err := r.ShipNow(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range r.chunks {
		if c.end-c.start > opts.MaxChunkBytes {
			t.Fatalf("chunk [%d,%d) exceeds MaxChunkBytes %d", c.start, c.end, opts.MaxChunkBytes)
		}
	}
	keys, _ := store.List(ctx, "gen/")
	for _, k := range keys {
		if !strings.HasSuffix(k, ".wlog") {
			continue
		}
		data, _ := store.Get(ctx, k)
		if int64(len(data)) > opts.MaxChunkBytes {
			t.Fatalf("object %s is %d bytes, over MaxChunkBytes", k, len(data))
		}
	}
	if _, err := Restore(ctx, store, "", filepath.Join(dir, "restored.db")); err != nil {
		t.Fatal(err)
	}
	rdb := openKV(t, filepath.Join(dir, "restored.db"))
	if v, ok := rdb.Get("k39"); !ok || v != strings.Repeat("x", 40) {
		t.Fatalf("restored k39 = %q,%v", v, ok)
	}
}

// jitterStore delays every Get by a random amount, so parallel restore
// downloads complete out of order.
type jitterStore struct {
	Storage
	inFlight, peak atomic.Int64
}

func (j *jitterStore) Get(ctx context.Context, key string) ([]byte, error) {
	n := j.inFlight.Add(1)
	defer j.inFlight.Add(-1)
	for {
		p := j.peak.Load()
		if n <= p || j.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(time.Duration(rand.IntN(3)) * time.Millisecond)
	return j.Storage.Get(ctx, key)
}

// Restore downloads chunks in parallel but writes them in order, with at
// most restoreWorkers in flight.
func TestRestoreParallelInOrder(t *testing.T) {
	dir := t.TempDir()
	db := openKV(t, filepath.Join(dir, "src.db"))
	for i := range 200 {
		if err := db.Set(fmt.Sprintf("k%03d", i), fmt.Sprintf("value-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	mem := newMemStore()
	r := New(db, mem, quietOpts(t)) // 64-byte chunks: many objects
	ctx := context.Background()
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	store := &jitterStore{Storage: mem}
	info, err := Restore(ctx, store, "", filepath.Join(dir, "restored.db"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Chunks < 50 {
		t.Fatalf("only %d chunks; the test wants many", info.Chunks)
	}
	if p := store.peak.Load(); p < 2 || p > restoreWorkers {
		t.Fatalf("peak concurrent downloads %d, want 2..%d", p, restoreWorkers)
	}
	rdb := openKV(t, filepath.Join(dir, "restored.db"))
	for i := range 200 {
		if v, ok := rdb.Get(fmt.Sprintf("k%03d", i)); !ok || v != fmt.Sprintf("value-%d", i) {
			t.Fatalf("restored k%03d = %q,%v", i, v, ok)
		}
	}
}

// vanishStore deletes a whole generation the first time one of its
// chunks is fetched — what a live replicator's pruning does to a restore
// running beside it.
type vanishStore struct {
	*memStore
	gen  string
	once sync.Once
}

func (v *vanishStore) Get(ctx context.Context, key string) ([]byte, error) {
	if strings.Contains(key, "/"+v.gen+"/") && strings.HasSuffix(key, ".wlog") {
		gone := false
		v.once.Do(func() {
			keys, _ := v.memStore.List(ctx, "gen/"+v.gen+"/")
			for _, k := range keys {
				v.memStore.Delete(ctx, k)
			}
			gone = true
		})
		if gone {
			return nil, errors.New("NoSuchKey")
		}
	}
	return v.memStore.Get(ctx, key)
}

// A chunk that disappears mid-restore means the listing went stale (the
// replicator pruned or merged); Restore lists again once and restores
// from what is there now.
func TestRestoreRelistsWhenChunkVanishes(t *testing.T) {
	dir := t.TempDir()
	db := openKV(t, filepath.Join(dir, "src.db"), btypedb.WithAutoCompactDisabled())
	if err := db.Set("k", "v"); err != nil {
		t.Fatal(err)
	}
	mem := newMemStore()
	r := New(db, mem, quietOpts(t))
	ctx := context.Background()
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	older := r.Status().Generation
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	newer := r.Status().Generation

	store := &vanishStore{memStore: mem, gen: newer}
	info, err := Restore(ctx, store, "", filepath.Join(dir, "restored.db"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Generation != older {
		t.Fatalf("restored %s, want %s after %s vanished", info.Generation, older, newer)
	}
}
