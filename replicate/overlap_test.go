package replicate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// ambiguousStore is a memStore whose next n chunk PUTs behave like an S3
// PUT whose response was lost: the object IS stored, but the caller sees
// an error (a timeout or reset after the server committed).
type ambiguousStore struct {
	*memStore
	ambiguous int
}

func (s *ambiguousStore) Put(ctx context.Context, key string, data []byte) error {
	if err := s.memStore.Put(ctx, key, data); err != nil {
		return err
	}
	if s.ambiguous > 0 && strings.HasSuffix(key, ".wlog") {
		s.ambiguous--
		return errors.New("simulated: response lost after the store committed the object")
	}
	return nil
}

// TestAmbiguousPutThenGrowthRestores (D-05): a chunk PUT that is stored
// but reported failed leaves the watermark in place; when the log grows
// before the retry, the retry ships [0,S2) beside the stored [0,S1).
// The chain used to stop at the second chunk with the same start, so
// the generation fell short of its manifest and Restore silently
// returned an older state (here: only row 1) while Status reported no
// error. Every shipped row must come back.
func TestAmbiguousPutThenGrowthRestores(t *testing.T) {
	for _, maxChunk := range []int64{0, 4096} { // default (one chunk) and multi-chunk retries
		t.Run(fmt.Sprintf("maxChunk=%d", maxChunk), func(t *testing.T) {
			dir := t.TempDir()
			ctx := context.Background()
			e, err := bytdb.Open(filepath.Join(dir, "src.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			if _, err := e.CreateTable("members", []bytdb.Column{
				{Name: "id", Type: bytdb.TInt}, {Name: "name", Type: bytdb.TString},
			}, "id"); err != nil {
				t.Fatal(err)
			}
			if err := e.Insert("members", 1, "ada"); err != nil {
				t.Fatal(err)
			}

			store := &ambiguousStore{memStore: newMemStore(), ambiguous: 1}
			opt := quietOpts(t)
			opt.MaxChunkBytes = maxChunk
			r := New(e, store, opt)
			if err := r.ShipNow(ctx); err == nil {
				t.Fatal("first ship should report the lost PUT response")
			}
			for id := 2; id <= 40; id++ {
				if err := e.Insert("members", id, strings.Repeat("x", 100)); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.ShipNow(ctx); err != nil {
				t.Fatalf("second ship: %v", err)
			}
			st := r.Status()

			restorePath := filepath.Join(dir, "restored.db")
			info, err := Restore(ctx, store, "", restorePath)
			if err != nil {
				t.Fatalf("restore: %v", err)
			}
			if info.Bytes != st.Watermark {
				t.Fatalf("restored %d bytes, shipped %d", info.Bytes, st.Watermark)
			}
			re, err := bytdb.Open(restorePath)
			if err != nil {
				t.Fatal(err)
			}
			defer re.Close()
			for id := 1; id <= 40; id++ {
				if _, ok, err := re.Get("members", id); err != nil || !ok {
					t.Fatalf("restored row %d: ok=%v err=%v", id, ok, err)
				}
			}
		})
	}
}

// TestContiguousChainOverlaps pins the chain rule directly, including a
// partial overlap (skip > 0), which the replicator does not produce
// today but which assemble must splice correctly if it ever appears.
func TestContiguousChainOverlaps(t *testing.T) {
	key := func(start, end int64) string {
		return fmt.Sprintf("gen/g/%016x-%016x.wlog", start, end)
	}
	type want struct{ start, end, skip int64 }
	cases := []struct {
		name string
		keys []string
		want []want
	}{
		{"disjoint", []string{key(0, 10), key(10, 30)}, []want{{0, 10, 0}, {10, 30, 0}}},
		{"same start, longer wins", []string{key(0, 10), key(0, 25), key(25, 40)}, []want{{0, 25, 0}, {25, 40, 0}}},
		{"redundant inner chunk", []string{key(0, 30), key(10, 20), key(30, 35)}, []want{{0, 30, 0}, {30, 35, 0}}},
		{"partial overlap", []string{key(0, 10), key(5, 20)}, []want{{0, 10, 0}, {5, 20, 5}}},
		{"gap truncates", []string{key(0, 10), key(12, 20)}, []want{{0, 10, 0}}},
		{"no zero start", []string{key(5, 10)}, nil},
	}
	for _, c := range cases {
		chain, ok := contiguousChain(c.keys)
		if ok != (len(c.want) > 0) || len(chain) != len(c.want) {
			t.Fatalf("%s: chain = %+v ok=%v, want %+v", c.name, chain, ok, c.want)
		}
		for i, w := range c.want {
			if g := chain[i]; g.start != w.start || g.end != w.end || g.skip != w.skip {
				t.Fatalf("%s: chain[%d] = %+v, want %+v", c.name, i, g, w)
			}
		}
	}

	// assemble splices a partial overlap by position.
	src := []byte("0123456789abcdefghij")
	store := newMemStore()
	ctx := context.Background()
	store.Put(ctx, key(0, 10), src[0:10])
	store.Put(ctx, key(5, 20), src[5:20])
	chain, _ := contiguousChain([]string{key(0, 10), key(5, 20)})
	dest := filepath.Join(t.TempDir(), "out")
	info, err := assemble(ctx, store, "g", chain, dest)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, src) || info.Bytes != int64(len(src)) {
		t.Fatalf("assembled %q (%d bytes), want %q", got, info.Bytes, src)
	}
}
