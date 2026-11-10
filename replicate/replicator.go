package replicate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/serr"
)

// Source is the engine-side surface the replicator ships from. Both
// *bytdb.Engine and *btypedb.DB satisfy it, so the replicator neither
// knows nor cares whether it is shipping a relational database or a
// bare key-value store — it moves log bytes.
type Source interface {
	// LogState returns the current log file epoch and byte size; the
	// bytes [0, size) of an epoch are immutable.
	LogState() (epoch uint64, size int64, err error)
	// ReadLogRange copies up to max log bytes from offset from into w,
	// failing with btypedb.ErrEpochChanged if the file was replaced.
	ReadLogRange(epoch uint64, from, max int64, w io.Writer) (int64, error)
}

// Options tunes a Replicator. The zero value is usable: 5s ship
// interval, 8 MB chunks, 3 retained generations, no key prefix.
type Options struct {
	// Interval is how often the log is polled and shipped. It is the
	// upper bound on the replica's data-loss window (plus upload time).
	Interval time.Duration

	// MaxChunkBytes caps the size of one uploaded object. Larger chunks
	// mean fewer PUT requests; smaller chunks mean less memory and a
	// shorter compaction hold during each range read.
	MaxChunkBytes int64

	// RetainGenerations is how many generations (current included) are
	// kept in the store; older ones are pruned when a new generation
	// starts. Minimum effective value is 2 — pruning down to just the
	// brand-new, still-empty generation would delete the only
	// restorable copy.
	RetainGenerations int

	// Prefix namespaces every key, letting many databases share one
	// bucket ("sites/stjohns/", say). A trailing slash is added if
	// missing.
	Prefix string

	// Logf receives operational messages (ship failures being retried,
	// pruning results). Defaults to log.Printf; set to a no-op to
	// silence.
	Logf func(format string, args ...any)

	// OpTimeout bounds each store operation, scaled by its payload: an
	// operation moving n bytes gets OpTimeout + n/MinTransferRate before
	// its context is canceled. Without it a PUT whose connection is
	// black-holed mid-body waits for TCP to give up (many minutes), all
	// the while holding the ship lock, so ShipNow callers and the final
	// flush queue behind it. Default 30s; negative disables (the
	// caller's context and the store client's own timeouts still apply).
	OpTimeout time.Duration

	// MinTransferRate is the slowest transfer, in bytes per second, the
	// per-operation timeout tolerates before treating an upload as
	// stalled. Default 64 KiB/s, which gives an 8 MB chunk 30s + 128s.
	// Set it below the uplink's real worst case: a timeout shorter than
	// an honest upload fails every attempt and replication never
	// progresses.
	MinTransferRate int64

	// MergeChunks is how many adjacent small chunks of the current
	// generation are merged into one object once they accumulate. A
	// database written a little at a time ships one object per active
	// tick (up to ~17,000 a day at the default interval) until the next
	// compaction, and restore fetches each one. Merging re-reads the
	// covered log range (immutable within a generation) and uploads it
	// as one chunk; the merged-away objects are deleted after a grace
	// period. Merges are size-tiered, like an LSM tree: MergeChunks
	// chunks of one tier become one chunk of the next, so a byte is
	// re-uploaded once per tier (logarithmically often), and a merge
	// never builds an object larger than MaxChunkBytes. Default 8;
	// negative disables merging.
	MergeChunks int
}

func (o *Options) withDefaults() Options {
	out := *o
	if out.Interval <= 0 {
		out.Interval = 5 * time.Second
	}
	if out.MaxChunkBytes <= 0 {
		out.MaxChunkBytes = 8 << 20
	}
	if out.RetainGenerations == 0 {
		out.RetainGenerations = 3
	} else if out.RetainGenerations < 2 {
		out.RetainGenerations = 2
	}
	if out.Prefix != "" && !strings.HasSuffix(out.Prefix, "/") {
		out.Prefix += "/"
	}
	if out.Logf == nil {
		out.Logf = log.Printf
	}
	if out.OpTimeout == 0 {
		out.OpTimeout = defaultOpTimeout
	}
	if out.MinTransferRate <= 0 {
		out.MinTransferRate = defaultMinTransferRate
	}
	if out.MergeChunks == 0 {
		out.MergeChunks = 8
	}
	return out
}

// Defaults for the per-operation store timeout (Options.OpTimeout,
// Options.MinTransferRate). Restore, which takes no Options, uses them
// as they are.
const (
	defaultOpTimeout       = 30 * time.Second
	defaultMinTransferRate = 64 << 10
)

// opContext derives the context for one store operation moving n bytes:
// base plus the time n bytes take at rate, so a large chunk is not cut
// off by a timeout sized for a small one. A negative base disables the
// bound. The arithmetic is in float64 because n*time.Second overflows
// int64 for objects past ~9 GB.
func opContext(ctx context.Context, base time.Duration, rate, n int64) (context.Context, context.CancelFunc) {
	if base < 0 {
		return ctx, func() {}
	}
	if rate <= 0 {
		rate = defaultMinTransferRate
	}
	d := base + time.Duration(float64(n)/float64(rate)*float64(time.Second))
	return context.WithTimeout(ctx, d)
}

// opCtx is opContext with the replicator's configured bounds.
func (r *Replicator) opCtx(ctx context.Context, n int64) (context.Context, context.CancelFunc) {
	return opContext(ctx, r.opt.OpTimeout, r.opt.MinTransferRate, n)
}

// Status is a point-in-time snapshot of replication progress, suitable
// for a health endpoint.
type Status struct {
	Generation   string    // current generation ID ("" before the first ship)
	Epoch        uint64    // log epoch the generation tracks
	Watermark    int64     // bytes shipped in this generation
	LastShipTime time.Time // completion time of the last successful ship
	LastError    error     // most recent ship error, cleared on success

	// Sealed reports whether the current generation has been certified
	// complete (its manifest is written). A generation starts unsealed
	// at every process start and every compaction, and stays so until
	// the whole log has been uploaded once. While it is unsealed,
	// Restore returns the previous complete generation, so no write
	// made since the roll is protected yet — on a large database that
	// window lasts as long as the full re-upload takes.
	Sealed bool

	// LagBytes is how many bytes of the log were not yet shipped as of
	// the last ship attempt: the log size minus the watermark. After a
	// roll it starts at the whole log size and falls as chunks upload.
	LagBytes int64
}

// Replicator asynchronously ships a Source's log to a Storage. Create
// with New, drive with Run (blocking) or Start/Close, and force a
// synchronous ship with ShipNow.
type Replicator struct {
	src   Source
	store Storage
	opt   Options

	// shipMu serializes ship attempts (the Run loop vs. explicit
	// ShipNow calls); the fields below it are the shipper's cursor and
	// are only touched with shipMu held.
	shipMu    sync.Mutex
	gen       string
	epoch     uint64
	watermark int64
	// sealed is the size last certified in this generation's manifest.
	// A generation earns a manifest once a ship catches up to the log
	// tip (watermark == size); restore trusts a manifested generation as
	// a complete snapshot. Tracking the last-sealed size lets a catch-up
	// re-write the manifest only when the complete size actually grows,
	// so idle ticks emit no PUT. Reset to 0 when the generation rolls.
	sealed int64
	buf    bytes.Buffer // reused chunk staging buffer

	// seq is the current generation's sequence number: one more than the
	// largest any generation in the store had when it rolled (see
	// newGenerationID). Kept across rolls so a roll whose listing fails
	// can still pick a number above everything this process has seen.
	seq uint64

	// chunks are the current generation's live chunk objects, in log
	// order, for merging (see maybeMerge). Every generation is created
	// by this process, so the list is complete — except for chunks an
	// ambiguous PUT stored without the replicator knowing, which are
	// redundant and harmless (restore trims overlap).
	chunks []shippedChunk
	// pendingDeletes are merged-away chunk keys waiting out deleteGrace
	// before deletion; deleteGrace defaults to mergeDeleteGrace (a field
	// so tests can shorten it).
	pendingDeletes []pendingDelete
	deleteGrace    time.Duration

	statusMu sync.Mutex
	status   Status

	cancel context.CancelFunc // set by Start
	done   chan struct{}      // closed when Run returns
}

// New builds a Replicator over src and store. Nothing ships until Run,
// Start, or ShipNow is called.
func New(src Source, store Storage, opt Options) *Replicator {
	return &Replicator{src: src, store: store, opt: opt.withDefaults(), deleteGrace: mergeDeleteGrace}
}

// Run ships on the configured interval until ctx is canceled, then
// performs one final ship (with a short grace timeout) so a clean
// shutdown loses nothing, and returns ctx's cause. Ship failures are
// logged and retried on the next tick — the interval loop is the retry
// policy — so Run only returns via ctx or when the source closes.
func (r *Replicator) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.opt.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Final flush on a fresh context: ctx is already dead, but
			// the bytes committed since the last tick deserve one
			// best-effort upload before the process exits.
			flushCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := r.ShipNow(flushCtx); err != nil && !errors.Is(err, btypedb.ErrClosed) {
				r.opt.Logf("replicate: final flush failed: %v", err)
			}
			cancel()
			return ctx.Err()
		case <-ticker.C:
			if err := r.ShipNow(ctx); err != nil {
				if errors.Is(err, btypedb.ErrClosed) {
					return err // source is gone; nothing further can ever ship
				}
				r.opt.Logf("replicate: ship failed (will retry in %v): %v", r.opt.Interval, err)
			}
		}
	}
}

// Start launches Run on a background goroutine. Close stops it.
func (r *Replicator) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		r.Run(ctx)
	}()
}

// Close stops a Start-ed replicator, waiting for its final flush.
// Close the Replicator before closing the underlying database, so the
// final flush still has a live source to read from.
func (r *Replicator) Close() error {
	if r.cancel == nil {
		return nil
	}
	r.cancel()
	<-r.done
	r.cancel = nil
	return nil
}

// Status returns a snapshot of replication progress.
func (r *Replicator) Status() Status {
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	return r.status
}

// ShipNow synchronously ships everything appended since the last ship,
// rolling to a new generation first if the log epoch moved (compaction)
// or nothing has shipped yet this process. It is what the Run loop
// calls each tick, exposed for tests and for callers that want a
// durable replica right now (say, after a critical transaction).
func (r *Replicator) ShipNow(ctx context.Context) error {
	r.shipMu.Lock()
	defer r.shipMu.Unlock()
	err := r.shipLocked(ctx)
	r.statusMu.Lock()
	if err != nil {
		r.status.LastError = err
	} else {
		r.status.LastError = nil
		r.status.LastShipTime = time.Now()
	}
	r.statusMu.Unlock()
	return err
}

func (r *Replicator) shipLocked(ctx context.Context) error {
	epoch, size, err := r.src.LogState()
	if err != nil {
		return serr.Wrap(err, "op", "log state")
	}

	// A new process (gen == "") and a compaction (epoch moved) are the
	// same event from the replica's viewpoint: prior offsets are void,
	// start a fresh generation from zero. See btypedb's replication
	// docs for why restart-from-zero is the correct crash-safe answer.
	if r.gen == "" || epoch != r.epoch {
		if err := r.rollGeneration(ctx, epoch); err != nil {
			return err
		}
	}
	r.setLag(size)

	// Merged-away chunks whose grace period has passed. Before the ship
	// loop rather than after, so a ship that fails part-way still makes
	// this progress.
	r.runPendingDeletes(ctx)

	for r.watermark < size {
		chunk := min(size-r.watermark, r.opt.MaxChunkBytes)
		r.buf.Reset()
		n, err := r.src.ReadLogRange(r.epoch, r.watermark, chunk, &r.buf)
		if errors.Is(err, btypedb.ErrEpochChanged) {
			return nil // compaction won the race; next ship rolls the generation
		}
		if err != nil {
			return serr.Wrap(err, "op", "read log range", "from", fmt.Sprint(r.watermark))
		}
		if n == 0 {
			return nil // log ended early inside the range; nothing more to do
		}
		key := r.chunkKey(r.gen, r.watermark, r.watermark+n)
		opCtx, cancel := r.opCtx(ctx, n)
		err = r.store.Put(opCtx, key, r.buf.Bytes())
		cancel()
		if err != nil {
			// Watermark deliberately not advanced: the next ship
			// re-reads and re-uploads the same immutable range, and an
			// atomic PUT means a half-uploaded object can never exist.
			return serr.Wrap(err, "op", "put chunk", "key", key)
		}
		r.chunks = append(r.chunks, shippedChunk{start: r.watermark, end: r.watermark + n,
			full: n >= r.opt.MaxChunkBytes})
		r.watermark += n
		r.setLag(size)
	}

	// Reaching here means the loop drained the log without returning
	// early, so the generation is fully shipped as of this size. Certify
	// it with a manifest (or refresh an existing one to the new complete
	// size) so restore can prefer this generation over any older complete
	// one, and can detect a chain that later loses a chunk below its
	// certified size. The manifest is metadata, not durability: a failed
	// write is logged and retried on the next catch-up (r.sealed stays
	// put), never failing the ship whose chunks are already stored.
	if r.watermark > 0 && r.watermark != r.sealed {
		if err := r.writeManifest(ctx); err != nil {
			r.opt.Logf("replicate: manifest write failed (will retry): %v", err)
		} else {
			r.sealed = r.watermark
			r.statusMu.Lock()
			r.status.Sealed = true
			r.statusMu.Unlock()
		}
	}

	// Merging comes last: the new bytes are already stored and certified,
	// so a slow or failed merge delays nothing that matters for
	// durability. A merge failure is logged and retried on a later ship.
	r.maybeMerge(ctx)
	return nil
}

// rollGeneration starts a fresh generation for log epoch at offset zero.
// Callers hold shipMu.
//
// The new generation's sequence number is one more than the largest in
// the store (or than this process's own last one), which is what orders
// generations: wall-clock names used to, and a clock stepped backwards,
// or a replacement host whose clock lags, made the live generation sort
// older than a stale one — restore then picked the stale one, and pruning
// deleted the live one (deep-look D-34). The listing that finds the
// largest number also feeds pruning.
//
// If the listing fails, a process that has rolled before uses its own
// last number plus one; that is still above everything in the store,
// because one replicator owns a prefix. A process that has never listed
// cannot know, so the ship fails and the next tick retries.
func (r *Replicator) rollGeneration(ctx context.Context, epoch uint64) error {
	lctx, cancel := r.opCtx(ctx, 0)
	byGen, err := listGenerations(lctx, r.store, r.opt.Prefix)
	cancel()
	seq := r.seq
	switch {
	case err == nil:
		for g := range byGen {
			seq = max(seq, generationSeq(g))
		}
	case r.seq == 0:
		return serr.Wrap(err, "op", "roll generation")
	default:
		r.opt.Logf("replicate: listing at generation roll failed, continuing from sequence %d: %v", r.seq, err)
	}
	seq++

	r.seq = seq
	r.gen = newGenerationID(seq)
	r.epoch = epoch
	r.watermark = 0
	r.sealed = 0
	r.chunks = nil
	r.statusMu.Lock()
	r.status.Generation, r.status.Epoch, r.status.Watermark = r.gen, epoch, 0
	r.status.Sealed = false
	r.statusMu.Unlock()

	// Prune before shipping, not after: the store holds complete prior
	// generations right now, so this is the safe moment to thin them.
	// Best-effort — a prune failure never blocks shipping.
	if err == nil {
		if err := r.pruneListed(ctx, byGen); err != nil {
			r.opt.Logf("replicate: prune failed: %v", err)
		}
	}
	return nil
}

// setLag records the unshipped byte count against log size size.
func (r *Replicator) setLag(size int64) {
	r.statusMu.Lock()
	r.status.Watermark = r.watermark
	r.status.LagBytes = max(size-r.watermark, 0)
	r.statusMu.Unlock()
}

// generationManifest certifies that a generation was shipped in full at
// least once: its chunks cover [0, Size) contiguously and form a
// complete, self-consistent database snapshot. Restore prefers the newest
// manifested generation and, seeing a manifest, treats a chain shorter
// than Size as damage (missing chunks) rather than a valid torn tail — the
// distinction that stops a barely-started post-compaction generation from
// silently shadowing a complete older one.
type generationManifest struct {
	Generation     string `json:"generation"`
	Epoch          uint64 `json:"epoch"`
	Size           int64  `json:"size"`
	SealedUnixNano int64  `json:"sealedUnixNano"`
	// Seq is the generation's sequence number, the same one its name
	// leads with (newGenerationID). Ordering reads the name, which a
	// listing alone provides; this copy is for operators reading the
	// manifest. Absent (0) in generations written before sequencing.
	Seq uint64 `json:"seq,omitempty"`
}

// manifestKey names a generation's completeness marker. It lives under
// the generation's own key space so listGenerations groups it with the
// generation and pruning removes it alongside the chunks; the ".json"
// suffix keeps contiguousChain from mistaking it for a ".wlog" chunk.
func manifestKey(prefix, gen string) string {
	return fmt.Sprintf("%sgen/%s/manifest.json", prefix, gen)
}

// writeManifest publishes the current generation's completeness marker at
// the caught-up watermark. Callers hold shipMu.
func (r *Replicator) writeManifest(ctx context.Context) error {
	data, err := json.Marshal(generationManifest{
		Generation:     r.gen,
		Epoch:          r.epoch,
		Size:           r.watermark,
		SealedUnixNano: time.Now().UnixNano(),
		Seq:            r.seq,
	})
	if err != nil {
		return serr.Wrap(err, "op", "marshal manifest")
	}
	opCtx, cancel := r.opCtx(ctx, int64(len(data)))
	defer cancel()
	if err := r.store.Put(opCtx, manifestKey(r.opt.Prefix, r.gen), data); err != nil {
		return serr.Wrap(err, "op", "put manifest", "generation", r.gen)
	}
	return nil
}

// pruneGenerations deletes all but the newest RetainGenerations-1 prior
// generations (the current one, being brand new and empty, is the
// +1). Callers hold shipMu.
//
// The newest MANIFESTED generation is never pruned, wherever it falls
// in the retention window. Retention counts generations by position
// alone, and restarts or compactions during slow shipping roll
// incomplete (unmanifested) generations that count equally — enough of
// them in a row would push the only complete, restorable generation
// past the horizon and delete it, leaving Restore nothing but partial
// prefixes (the exact roll-backward the manifest machinery exists to
// prevent). Keeping it can hold one generation beyond RetainGenerations
// until a newer one completes; that is the right trade.
func (r *Replicator) pruneGenerations(ctx context.Context) error {
	lctx, cancel := r.opCtx(ctx, 0)
	byGen, err := listGenerations(lctx, r.store, r.opt.Prefix)
	cancel()
	if err != nil {
		return err
	}
	return r.pruneListed(ctx, byGen)
}

// pruneListed is pruneGenerations over a listing already in hand.
// Generations are ordered by sequence number (sortGenerations), not by
// name, so a clock that stepped backwards cannot make pruning delete the
// live end of the history.
func (r *Replicator) pruneListed(ctx context.Context, byGen map[string][]string) error {
	gens := make([]string, 0, len(byGen))
	for g := range byGen {
		if g != r.gen {
			gens = append(gens, g)
		}
	}
	sortGenerations(gens)
	newestManifested := ""
	for _, g := range gens { // ascending: the last manifested one wins
		for _, key := range byGen[g] {
			if strings.HasSuffix(key, "/manifest.json") {
				newestManifested = g
				break
			}
		}
	}
	keep := r.opt.RetainGenerations - 1
	if len(gens) <= keep {
		return nil
	}
	var errs []error
	for _, g := range gens[:len(gens)-keep] {
		if g == newestManifested {
			continue
		}
		for _, key := range byGen[g] {
			if err := r.deleteKey(ctx, key); err != nil {
				errs = append(errs, serr.Wrap(err, "key", key))
			}
		}
		if len(errs) == 0 {
			r.opt.Logf("replicate: pruned generation %s (%d objects)", g, len(byGen[g]))
		}
	}
	return errors.Join(errs...)
}

// deleteKey deletes one object under the per-operation timeout.
func (r *Replicator) deleteKey(ctx context.Context, key string) error {
	opCtx, cancel := r.opCtx(ctx, 0)
	defer cancel()
	return r.store.Delete(opCtx, key)
}

// chunkKey names one shipped range. Offsets are zero-padded hex so the
// store's lexicographic listing is numeric order, and both ends are in
// the name so restore verifies contiguity from the listing alone.
func (r *Replicator) chunkKey(gen string, start, end int64) string {
	return fmt.Sprintf("%sgen/%s/%016x-%016x.wlog", r.opt.Prefix, gen, start, end)
}

// newGenerationID returns a fresh generation name:
//
//	g<seq, 16 decimal digits>-<UTC timestamp to the ns>-<4 random bytes, hex>
//	g0000000000000042-20261109t150405.000000000-8f2a1c40
//
// The sequence number orders generations (generationLess); the
// timestamp is for the humans reading a bucket listing, and the random
// suffix keeps two misconfigured replicators sharing a prefix from
// writing into one generation. Zero-padding keeps a plain listing in
// sequence order too, and the "g" keeps every sequenced name sorting
// after the legacy names, which began with the year.
func newGenerationID(seq uint64) string {
	var suffix [4]byte
	rand.Read(suffix[:]) // never fails per crypto/rand contract
	return fmt.Sprintf("g%016d-%s-%s", seq,
		time.Now().UTC().Format("20060102t150405.000000000"), hex.EncodeToString(suffix[:]))
}

// generationSeq is the sequence number a generation name leads with, or
// 0 for a name without one: a legacy timestamp-only name written before
// sequencing, or a foreign directory.
func generationSeq(gen string) uint64 {
	rest, ok := strings.CutPrefix(gen, "g")
	if !ok {
		return 0
	}
	digits, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// generationLess orders generations oldest first: by sequence number,
// then by name. Legacy names all have sequence 0, so among themselves
// they keep the timestamp order they were written with, and all of them
// are older than any sequenced generation.
func generationLess(a, b string) bool {
	sa, sb := generationSeq(a), generationSeq(b)
	if sa != sb {
		return sa < sb
	}
	return a < b
}

// sortGenerations sorts generation names oldest first.
func sortGenerations(gens []string) {
	sort.Slice(gens, func(i, j int) bool { return generationLess(gens[i], gens[j]) })
}

// listGenerations groups every chunk key in the store by generation ID.
func listGenerations(ctx context.Context, store Storage, prefix string) (map[string][]string, error) {
	keys, err := store.List(ctx, prefix+"gen/")
	if err != nil {
		return nil, serr.Wrap(err, "op", "list generations")
	}
	byGen := map[string][]string{}
	for _, k := range keys {
		rest := strings.TrimPrefix(k, prefix+"gen/")
		gen, _, ok := strings.Cut(rest, "/")
		if !ok || gen == "" {
			continue // stray object; not ours to manage
		}
		byGen[gen] = append(byGen[gen], k)
	}
	return byGen, nil
}
