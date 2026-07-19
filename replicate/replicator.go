package replicate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
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
	return out
}

// Status is a point-in-time snapshot of replication progress, suitable
// for a health endpoint.
type Status struct {
	Generation   string    // current generation ID ("" before the first ship)
	Epoch        uint64    // log epoch the generation tracks
	Watermark    int64     // bytes shipped in this generation
	LastShipTime time.Time // completion time of the last successful ship
	LastError    error     // most recent ship error, cleared on success
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
	buf       bytes.Buffer // reused chunk staging buffer

	statusMu sync.Mutex
	status   Status

	cancel context.CancelFunc // set by Start
	done   chan struct{}      // closed when Run returns
}

// New builds a Replicator over src and store. Nothing ships until Run,
// Start, or ShipNow is called.
func New(src Source, store Storage, opt Options) *Replicator {
	return &Replicator{src: src, store: store, opt: opt.withDefaults()}
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
		r.gen = newGenerationID()
		r.epoch = epoch
		r.watermark = 0
		r.statusMu.Lock()
		r.status.Generation, r.status.Epoch, r.status.Watermark = r.gen, epoch, 0
		r.statusMu.Unlock()
		// Prune before shipping, not after: the store holds complete
		// prior generations right now, so this is the safe moment to
		// thin them. Best-effort — a prune failure never blocks
		// shipping.
		if err := r.pruneGenerations(ctx); err != nil {
			r.opt.Logf("replicate: prune failed: %v", err)
		}
	}

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
		if err := r.store.Put(ctx, key, r.buf.Bytes()); err != nil {
			// Watermark deliberately not advanced: the next ship
			// re-reads and re-uploads the same immutable range, and an
			// atomic PUT means a half-uploaded object can never exist.
			return serr.Wrap(err, "op", "put chunk", "key", key)
		}
		r.watermark += n
		r.statusMu.Lock()
		r.status.Watermark = r.watermark
		r.statusMu.Unlock()
	}
	return nil
}

// pruneGenerations deletes all but the newest RetainGenerations-1 prior
// generations (the current one, being brand new and empty, is the
// +1). Callers hold shipMu.
func (r *Replicator) pruneGenerations(ctx context.Context) error {
	byGen, err := listGenerations(ctx, r.store, r.opt.Prefix)
	if err != nil {
		return err
	}
	gens := make([]string, 0, len(byGen))
	for g := range byGen {
		if g != r.gen {
			gens = append(gens, g)
		}
	}
	sort.Strings(gens)
	keep := r.opt.RetainGenerations - 1
	if len(gens) <= keep {
		return nil
	}
	var errs []error
	for _, g := range gens[:len(gens)-keep] {
		for _, key := range byGen[g] {
			if err := r.store.Delete(ctx, key); err != nil {
				errs = append(errs, serr.Wrap(err, "key", key))
			}
		}
		if len(errs) == 0 {
			r.opt.Logf("replicate: pruned generation %s (%d objects)", g, len(byGen[g]))
		}
	}
	return errors.Join(errs...)
}

// chunkKey names one shipped range. Offsets are zero-padded hex so the
// store's lexicographic listing is numeric order, and both ends are in
// the name so restore verifies contiguity from the listing alone.
func (r *Replicator) chunkKey(gen string, start, end int64) string {
	return fmt.Sprintf("%sgen/%s/%016x-%016x.wlog", r.opt.Prefix, gen, start, end)
}

// newGenerationID returns a fresh generation name: a UTC timestamp to
// the nanosecond (lexicographic order == chronological order, which is
// how restore finds "latest") plus random bytes so two processes
// starting in the same instant cannot collide.
func newGenerationID() string {
	var suffix [4]byte
	rand.Read(suffix[:]) // never fails per crypto/rand contract
	return time.Now().UTC().Format("20060102t150405.000000000") + "-" + hex.EncodeToString(suffix[:])
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
