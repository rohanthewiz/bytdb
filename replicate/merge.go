package replicate

// merge.go: size-tiered merging of the current generation's chunks
// (deep-look D-34).
//
// Each ship uploads what appended since the last one as a new object, so
// a database written a little at a time grows one object per active
// tick until the next compaction rolls the generation — up to ~17,000 a
// day at the default 5 s interval — and restore downloads every one.
// Merging folds runs of small chunks into one object, LSM-style:
//
//	tier 0:  [a][b][c][d][e][f][g][h]       MergeChunks (8) adjacent tier-0 chunks
//	              │ re-read [a.start, h.end) from the log, PUT as one object
//	              ▼
//	tier 1:  [a ──────────────────── h]     the 8 small objects are deleted
//	                                        after a grace period
//	tier 1 × 8  ─►  tier 2,  and so on, while the merged object stays
//	                within MaxChunkBytes
//
// A byte is re-uploaded once per tier it climbs, and tier k spans 8^k
// ticks, so a day of steady small writes is ~5 tiers of at most 7 chunks
// each plus the full-size chunks — instead of thousands of objects.
//
// Why this is safe without coordinating with restore:
//
//   - The merged object covers bytes the small ones already hold, and a
//     generation is one log epoch, append-only, so the bytes are
//     identical. Restore's chain builder takes the furthest-reaching
//     chunk at each offset and ignores the ones it covers (D-05), so a
//     merged chunk and its not-yet-deleted parts coexist harmlessly.
//   - The merged object is stored before any part is deleted, and parts
//     are deleted only after deleteGrace, so a restore that listed the
//     store before the merge still finds the parts it chose. (Restore
//     also re-lists once if a chunk vanishes mid-download.)
//   - The manifest's certified size is about coverage from offset zero,
//     which merging never reduces.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/serr"
)

// mergeDeleteGrace is how long merged-away chunks outlive the merge. It
// only needs to exceed the gap between a restore listing the store and
// downloading what it chose.
const mergeDeleteGrace = 10 * time.Minute

// maxDeleteAttempts bounds retries of a merged-away chunk's delete; a
// store that keeps refusing (a policy without s3:DeleteObject, say)
// would otherwise grow the pending list forever. A chunk given up on is
// redundant, not harmful, and goes when its generation is pruned.
const maxDeleteAttempts = 3

// shippedChunk is one live chunk object of the current generation.
type shippedChunk struct {
	start, end int64
	// tier counts the merges that built the chunk: 0 for a shipped
	// chunk, k+1 for a merge of tier-k chunks.
	tier int
	// full chunks are never merged again: they reached MaxChunkBytes,
	// or opened a run whose merge would have exceeded it.
	full bool
}

// pendingDelete is a batch of merged-away chunk keys awaiting deletion.
type pendingDelete struct {
	keys     []string
	after    time.Time // not before this
	attempts int
}

// maybeMerge merges every run of MergeChunks adjacent mergeable chunks
// of one tier, cascading as merges complete higher-tier runs. A failed
// merge is logged and left for a later ship; nothing was deleted, so
// the generation's chain is unchanged. Callers hold shipMu.
func (r *Replicator) maybeMerge(ctx context.Context) {
	fanIn := r.opt.MergeChunks
	if fanIn < 2 {
		return
	}
	for {
		i := r.mergeableRun(fanIn)
		if i < 0 {
			return
		}
		run := r.chunks[i : i+fanIn]
		start, end := run[0].start, run[fanIn-1].end
		if end-start > r.opt.MaxChunkBytes {
			// Too big to merge as one object. Pinning the run's first chunk
			// lets the rest pair up with later arrivals; pinning the whole
			// run would also strand its small chunks, which a later run
			// can still absorb.
			run[0].full = true
			continue
		}
		if err := r.mergeRange(ctx, start, end); err != nil {
			if !errors.Is(err, btypedb.ErrEpochChanged) { // compaction: the generation is rolling anyway
				r.opt.Logf("replicate: chunk merge failed (will retry): %v", err)
			}
			return
		}
		keys := make([]string, fanIn)
		for k, c := range run {
			keys[k] = r.chunkKey(r.gen, c.start, c.end)
		}
		merged := shippedChunk{start: start, end: end, tier: run[0].tier + 1,
			full: end-start >= r.opt.MaxChunkBytes}
		r.chunks = slices.Replace(r.chunks, i, i+fanIn, merged)
		r.pendingDeletes = append(r.pendingDeletes,
			pendingDelete{keys: keys, after: time.Now().Add(r.deleteGrace)})
	}
}

// mergeableRun returns the index of the first run of fanIn adjacent,
// non-full chunks of one tier, or -1. Runs normally form at the tail
// (new chunks arrive there, and a merge's output lands where its parts
// were), but a merge that failed leaves its run behind newer chunks, so
// the whole list is scanned; it is short — a few tiers of fewer than
// fanIn chunks each, plus full ones.
func (r *Replicator) mergeableRun(fanIn int) int {
	n := 0 // length of the run ending at i
	for i, c := range r.chunks {
		switch {
		case c.full:
			n = 0
			continue
		case n > 0 && r.chunks[i-1].tier == c.tier:
			n++
		default:
			n = 1
		}
		if n == fanIn {
			return i - fanIn + 1
		}
	}
	return -1
}

// mergeRange re-reads [start, end) of the current epoch's log and stores
// it as one chunk. The range is below the watermark, so it is immutable
// for as long as the epoch lasts; a compaction surfaces as
// btypedb.ErrEpochChanged.
func (r *Replicator) mergeRange(ctx context.Context, start, end int64) error {
	r.buf.Reset()
	n, err := r.src.ReadLogRange(r.epoch, start, end-start, &r.buf)
	if err != nil {
		return err
	}
	if n != end-start {
		return serr.New("short log read for merge",
			"from", fmt.Sprint(start), "want", fmt.Sprint(end-start), "got", fmt.Sprint(n))
	}
	key := r.chunkKey(r.gen, start, end)
	opCtx, cancel := r.opCtx(ctx, n)
	defer cancel()
	if err := r.store.Put(opCtx, key, r.buf.Bytes()); err != nil {
		return serr.Wrap(err, "op", "put merged chunk", "key", key)
	}
	return nil
}

// runPendingDeletes deletes merged-away chunks whose grace period has
// passed. Failures stay pending for a later ship, up to
// maxDeleteAttempts. Callers hold shipMu.
func (r *Replicator) runPendingDeletes(ctx context.Context) {
	if len(r.pendingDeletes) == 0 {
		return
	}
	now := time.Now()
	keep := r.pendingDeletes[:0] // in-place filter: writes never pass the read index
	var lastErr error
	for _, p := range r.pendingDeletes {
		if now.Before(p.after) {
			keep = append(keep, p)
			continue
		}
		var left []string
		for _, k := range p.keys {
			if err := r.deleteKey(ctx, k); err != nil {
				left = append(left, k)
				lastErr = err
			}
		}
		switch {
		case len(left) == 0:
		case p.attempts+1 < maxDeleteAttempts:
			keep = append(keep, pendingDelete{keys: left, after: p.after, attempts: p.attempts + 1})
		default:
			r.opt.Logf("replicate: giving up deleting %d merged chunk(s); they are redundant and go with their generation", len(left))
		}
	}
	clear(r.pendingDeletes[len(keep):])
	r.pendingDeletes = keep
	if lastErr != nil {
		r.opt.Logf("replicate: deleting merged chunks failed (will retry): %v", lastErr)
	}
}
