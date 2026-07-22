package replicate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/rohanthewiz/serr"
)

// ErrNoReplica is returned by Restore when the store holds no
// restorable generation under the given prefix. Test with errors.Is.
var ErrNoReplica = errors.New("replicate: no restorable generation found")

// ErrIncompleteReplica is returned by Restore when the store holds
// generations that were certified complete (they carry a manifest) but
// every one is now missing chunks below its certified size — the store
// has lost data. Restore refuses to silently roll back to a truncated
// fragment in that case; the operator must investigate the missing
// objects. Test with errors.Is.
var ErrIncompleteReplica = errors.New("replicate: all complete generations are missing chunks")

// RestoreInfo describes what a Restore produced.
type RestoreInfo struct {
	Generation string // generation the file was assembled from
	Bytes      int64  // size of the restored database file
	Chunks     int    // number of objects concatenated
}

// Restore assembles the newest complete generation under prefix into a
// database file at destPath, atomically (temp file + fsync + rename).
// Opening the result with bytdb.Open / btypedb.Open yields every
// transaction that had been shipped when the source last uploaded —
// the replay machinery treats the file exactly like a crash-recovered
// local one.
//
// "Complete" is authoritative, not inferred: the replicator writes a
// per-generation manifest the first time a generation is shipped in full,
// and refreshes it as the generation grows. Restore prefers the newest
// generation that carries a manifest and whose chunk chain still reaches
// its certified size, downloading nothing until the choice is made. This
// is what stops a barely-started generation from a fresh compaction — its
// chain begins at zero and so looks restorable — from silently shadowing
// a complete older one (a roll-backward).
//
// Selection, newest-first:
//
//   - A manifested generation whose contiguous-from-zero chain reaches at
//     least the certified size is restored (its full chain, which may
//     extend past the certified point with a valid torn tail).
//   - A manifested generation whose chain falls short of its certified
//     size has lost chunks; it is skipped for an older complete one.
//   - If manifests exist but every one is short, Restore fails with
//     ErrIncompleteReplica rather than restore a truncated fragment.
//   - If no generation carries a manifest at all (a store written before
//     manifests, or one where nothing has caught up yet), Restore falls
//     back to the legacy best-effort rule: the newest contiguous-from-zero
//     chain, which offers no completeness guarantee.
func Restore(ctx context.Context, store Storage, prefix, destPath string) (*RestoreInfo, error) {
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	byGen, err := listGenerations(ctx, store, prefix)
	if err != nil {
		return nil, err
	}
	gens := make([]string, 0, len(byGen))
	for g := range byGen {
		gens = append(gens, g)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(gens)))

	// Pass 1: the newest complete (manifested) generation.
	sawManifest := false
	for _, gen := range gens {
		keys := byGen[gen]
		if !slices.Contains(keys, manifestKey(prefix, gen)) {
			continue // never certified complete; considered only in pass 2
		}
		sawManifest = true
		man, err := loadManifest(ctx, store, prefix, gen)
		if err != nil {
			// The listing showed a manifest, so a Get failure is a real
			// store error, not an absent object — surface it.
			return nil, serr.Wrap(err, "generation", gen)
		}
		chain, ok := contiguousChain(keys)
		if !ok {
			continue // manifested but the chain no longer starts at zero
		}
		if end := chain[len(chain)-1].end; end < man.Size {
			// Certified to man.Size but the chain is short: chunks below the
			// certified point are gone. Prefer an older complete generation.
			continue
		}
		info, err := assemble(ctx, store, gen, chain, destPath)
		if err != nil {
			return nil, serr.Wrap(err, "generation", gen)
		}
		return info, nil
	}

	// Pass 2: fallback only when nothing was ever certified complete.
	// With manifests present but all short, refuse rather than roll back.
	if !sawManifest {
		for _, gen := range gens {
			chain, ok := contiguousChain(byGen[gen])
			if !ok {
				continue
			}
			info, err := assemble(ctx, store, gen, chain, destPath)
			if err != nil {
				return nil, serr.Wrap(err, "generation", gen)
			}
			return info, nil
		}
		return nil, serr.Wrap(ErrNoReplica, "prefix", prefix, "generationsSeen", strconv.Itoa(len(gens)))
	}
	return nil, serr.Wrap(ErrIncompleteReplica, "prefix", prefix, "generationsSeen", strconv.Itoa(len(gens)))
}

// loadManifest downloads and parses a generation's completeness marker.
// Only called for generations whose listing already showed the manifest
// key, so a Get error here is a genuine store failure.
func loadManifest(ctx context.Context, store Storage, prefix, gen string) (*generationManifest, error) {
	data, err := store.Get(ctx, manifestKey(prefix, gen))
	if err != nil {
		return nil, serr.Wrap(err, "op", "get manifest")
	}
	var man generationManifest
	if err := json.Unmarshal(data, &man); err != nil {
		return nil, serr.Wrap(err, "op", "parse manifest")
	}
	return &man, nil
}

// chunkRef is one parsed chunk key.
type chunkRef struct {
	key        string
	start, end int64
}

// contiguousChain parses the chunk keys of one generation and returns
// them ordered by offset, verifying the chain starts at zero and each
// chunk begins exactly where the previous ended. A missing first chunk
// means the generation never became restorable (ok=false); a gap later
// in the chain truncates it there — everything before the gap is still
// a valid database prefix, exactly like a torn tail.
func contiguousChain(keys []string) (chain []chunkRef, ok bool) {
	refs := make([]chunkRef, 0, len(keys))
	for _, k := range keys {
		base := k[strings.LastIndexByte(k, '/')+1:]
		name, isChunk := strings.CutSuffix(base, ".wlog")
		if !isChunk {
			continue
		}
		startHex, endHex, found := strings.Cut(name, "-")
		if !found {
			continue
		}
		start, err1 := strconv.ParseInt(startHex, 16, 64)
		end, err2 := strconv.ParseInt(endHex, 16, 64)
		if err1 != nil || err2 != nil || end <= start {
			continue
		}
		refs = append(refs, chunkRef{key: k, start: start, end: end})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].start < refs[j].start })

	var next int64
	for _, ref := range refs {
		if ref.start != next {
			break // gap (or duplicate); the chain ends at the last good chunk
		}
		chain = append(chain, ref)
		next = ref.end
	}
	return chain, len(chain) > 0
}

// assemble downloads a validated chain into destPath via a temp file,
// fsyncing before the rename so a crash mid-restore never leaves a
// plausible-looking partial database — the same dance btypedb's own
// Backup and Compact do.
func assemble(ctx context.Context, store Storage, gen string, chain []chunkRef, destPath string) (*RestoreInfo, error) {
	tmpPath := destPath + ".restore-tmp"
	tmp, err := os.Create(tmpPath)
	if err != nil {
		return nil, serr.Wrap(err, "path", tmpPath)
	}
	discard := func() {
		tmp.Close()
		os.Remove(tmpPath)
	}

	var total int64
	for _, ref := range chain {
		data, err := store.Get(ctx, ref.key)
		if err != nil {
			discard()
			return nil, serr.Wrap(err, "op", "get chunk", "key", ref.key)
		}
		// The chunk's own name declares its size; a mismatch means the
		// object was corrupted or tampered with in the store, and
		// splicing it in would shift every later offset. Fail loudly
		// rather than silently restoring a fork.
		if got, want := int64(len(data)), ref.end-ref.start; got != want {
			discard()
			return nil, serr.New("chunk size mismatch",
				"key", ref.key, "declared", fmt.Sprint(want), "actual", fmt.Sprint(got))
		}
		if _, err := tmp.Write(data); err != nil {
			discard()
			return nil, serr.Wrap(err, "op", "write chunk", "key", ref.key)
		}
		total += int64(len(data))
	}

	if err := tmp.Sync(); err != nil {
		discard()
		return nil, serr.Wrap(err, "op", "sync restored file")
	}
	if err := tmp.Close(); err != nil {
		discard()
		return nil, serr.Wrap(err, "op", "close restored file")
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return nil, serr.Wrap(err, "op", "move restored file into place")
	}
	// Persist the directory entry too, so the rename survives a power
	// cut — best-effort, as some filesystems refuse directory fsync.
	if d, err := os.Open(filepath.Dir(destPath)); err == nil {
		d.Sync()
		d.Close()
	}
	return &RestoreInfo{Generation: gen, Bytes: total, Chunks: len(chain)}, nil
}
