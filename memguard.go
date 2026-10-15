package bytdb

// memguard.go: two admission controls that turn "the process is about
// to be OOM-killed" into an ordinary, retryable statement error.
//
//   - The memory ceiling (CheckMemory) refuses work that grows the heap
//     — row inserts and updates, index builds, table rewrites, and the
//     SQL layer's result materializations — once the live heap is over
//     a ceiling. DELETE, DROP and TRUNCATE are never refused: they are
//     how an operator gets back under it.
//   - The result row cap (MaxResultRows) bounds how many rows any one
//     query may materialize. The SQL layer enforces it; the engine only
//     holds the knob, so every frontend sharing the engine (sql.DB,
//     Sessions, database/sql, pgwire) is bound by the one setting.
//
// Neither control can make the process safe on its own: GOMEMLIMIT
// keeps garbage from piling up, the ceiling keeps reachable data from
// growing past the point where GC can help, and the row cap keeps one
// query from allocating gigabytes between two ceiling checks.

import (
	"errors"
	"math"
	"runtime"
	"runtime/metrics"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/rohanthewiz/serr"
)

// ErrOutOfMemory is the root of every memory-ceiling refusal. Match it
// with errors.Is; the wrapped error carries the live heap and ceiling
// as fields. pgwire reports it as SQLSTATE 53200 (out_of_memory), the
// code Postgres uses when an allocation fails, so clients and pools
// treat it as a resource error rather than a bug in the statement.
var ErrOutOfMemory = errors.New("out of memory")

// DefaultMemoryCeilingFraction is the share of the Go soft memory limit
// (GOMEMLIMIT / debug.SetMemoryLimit) that the automatic ceiling sits
// at. The runtime itself starts collecting continuously as the heap
// nears the limit; refusing growth at 90% of it leaves the last tenth
// for the in-flight statements that already passed the check, the
// copy-on-write node copies their commits make, and the collector's
// own working room.
const DefaultMemoryCeilingFraction = 0.9

// DefaultMaxResultRows is the cap on rows one query may materialize
// (see SetMaxResultRows).
//
// Result rows live as [][]any: a slice header plus a boxed interface
// value per column, roughly 150-300 bytes for a typical 5-column row.
// Ten million of them is 1.5-3 GB of transient heap for one statement,
// far beyond anything an interactive or paginated query returns, yet
// small enough that a forgotten WHERE clause or an accidental cross
// join is refused before it takes the process down. Bulk exports that
// genuinely need more should paginate (keyset on the primary key), or
// raise the cap deliberately.
const DefaultMaxResultRows = 10_000_000

// memSampleInterval is how long one heap reading is trusted. The check
// sits on per-row write paths (every insert and update), where reading
// runtime/metrics on each call would be a measurable tax on bulk
// loads; reusing a reading for 2 ms costs one atomic load and one
// monotonic clock read per row instead. Two milliseconds is shorter
// than any GC cycle that could move the figure meaningfully, and short
// enough that a burst of inserts cannot outrun the guard by more than
// a couple of milliseconds' worth of rows.
const memSampleInterval = 2 * time.Millisecond

// memForceGCInterval rate-limits the collections the guard forces
// while it is refusing work (see memGuard.resample).
const memForceGCInterval = time.Second

// memEpoch anchors the guard's monotonic clock: time.Since reads the
// monotonic reading, so wall-clock jumps cannot freeze or expire the
// cached verdict early.
var memEpoch = time.Now()

func monoNow() int64 { return int64(time.Since(memEpoch)) }

// memGuard is the engine's memory-ceiling state.
//
// The configuration is one int64 (see SetMemoryCeiling for its three
// regimes). The verdict is an immutable snapshot swapped in whole
// through an atomic pointer, so concurrent checkers never see a torn
// live/ceiling pair, and resampling needs no lock: when two goroutines
// race past an expired verdict, both sample and the last store wins —
// both readings are equally fresh.
type memGuard struct {
	ceiling atomic.Int64
	verdict atomic.Pointer[memVerdict]

	// gcRunning / lastForcedGC rate-limit forced collections.
	gcRunning    atomic.Bool
	lastForcedGC atomic.Int64

	// sample reads (live heap after the last GC, GOMEMLIMIT). nil means
	// the runtime; tests substitute a fake to drive the guard without
	// actually exhausting memory or setting a tiny GOMEMLIMIT, which
	// would make the whole test binary thrash.
	sample func() (live, gomemlimit uint64)
}

// memVerdict is one heap reading and the ceiling in force when it was
// taken. ceiling 0 means the guard was inactive (automatic mode with
// no GOMEMLIMIT set).
type memVerdict struct {
	at      int64 // monoNow() at sampling
	live    uint64
	ceiling uint64
}

func (v *memVerdict) over() bool { return v.ceiling > 0 && v.live > v.ceiling }

// runtimeMemSample reads the two figures the guard needs.
//
// The live heap figure is /gc/heap/live:bytes — the bytes the last GC
// cycle marked reachable — not Stats.HeapLive (/memory/classes/heap/
// objects:bytes). The latter includes garbage not yet collected: with
// GOGC=100 it swings between 1x and 2x the reachable set over every GC
// cycle, so a ceiling compared against it would refuse writes half the
// time on a database using only half its budget. The marked figure
// moves only at GC boundaries, but that is exactly when it matters:
// near GOMEMLIMIT the runtime collects continuously, so the reading is
// fresh precisely when the heap is close to the edge.
func runtimeMemSample() (live, gomemlimit uint64) {
	s := []metrics.Sample{
		{Name: "/gc/heap/live:bytes"},
		{Name: "/gc/gomemlimit:bytes"},
	}
	metrics.Read(s)
	return sampleUint64(s[0]), sampleUint64(s[1])
}

// effectiveCeiling resolves the configured value against the runtime's
// soft limit: an explicit byte count wins; automatic mode derives
// DefaultMemoryCeilingFraction of GOMEMLIMIT, or 0 (inactive) when
// GOMEMLIMIT is unset — the runtime reports "unset" as MaxInt64, which
// the 1<<62 bound folds away exactly as Stats does.
func effectiveCeiling(cfg int64, gomemlimit uint64) uint64 {
	switch {
	case cfg > 0:
		return uint64(cfg)
	case cfg < 0:
		return 0
	case gomemlimit == 0 || gomemlimit >= 1<<62:
		return 0
	}
	return uint64(math.Floor(float64(gomemlimit) * DefaultMemoryCeilingFraction))
}

// resample takes a fresh reading and publishes it.
//
// When the reading is over the ceiling, it also starts a background GC
// (at most one at a time, at most once per memForceGCInterval). The
// live figure only changes when a collection completes, and a refusing
// guard stops the very allocations that would trigger one: after an
// operator DELETEs their way back under the ceiling, nothing else
// would allocate, no GC would run, and the stale "over" reading would
// refuse writes indefinitely. The forced cycle is what lets the guard
// notice the recovery. It runs off the caller's goroutine because
// runtime.GC blocks until the cycle finishes, and a refused statement
// should fail fast rather than wait out a full collection.
func (g *memGuard) resample(now int64) *memVerdict {
	sample := g.sample
	if sample == nil {
		sample = runtimeMemSample
	}
	live, gml := sample()
	v := &memVerdict{at: now, live: live, ceiling: effectiveCeiling(g.ceiling.Load(), gml)}
	g.verdict.Store(v)
	if v.over() && now-g.lastForcedGC.Load() >= int64(memForceGCInterval) &&
		g.gcRunning.CompareAndSwap(false, true) {
		g.lastForcedGC.Store(now)
		go func() {
			defer g.gcRunning.Store(false)
			runtime.GC()
			// Publish the post-GC reading right away instead of waiting
			// for the next caller to find the verdict stale.
			g.resample(monoNow())
		}()
	}
	return v
}

// current returns a verdict no older than memSampleInterval.
func (g *memGuard) current() *memVerdict {
	now := monoNow()
	if v := g.verdict.Load(); v != nil && now-v.at < int64(memSampleInterval) {
		return v
	}
	return g.resample(now)
}

// SetMemoryCeiling sets the live-heap level above which the engine
// refuses memory-growing work with ErrOutOfMemory:
//
//	n > 0   an explicit ceiling, in bytes of live heap
//	n == 0  automatic (the default): DefaultMemoryCeilingFraction of
//	        GOMEMLIMIT, re-derived on every reading so a later
//	        debug.SetMemoryLimit is honored; inactive while no
//	        GOMEMLIMIT is set
//	n < 0   disabled
//
// Zero means automatic here, unlike SetBackfillLimit and
// SetMaxResultRows where zero disables: the right ceiling depends on a
// limit that lives outside the engine, so the default has to follow
// it rather than be a constant.
//
// The ceiling is compared against the heap the last GC found
// reachable (see Stats.HeapMarked), not the instantaneous heap, so an
// explicit ceiling should leave room for the garbage the runtime lets
// accumulate between cycles — with the default GOGC=100 and no
// GOMEMLIMIT, the process can briefly hold up to twice the marked
// heap. Setting GOMEMLIMIT and leaving this automatic avoids that
// arithmetic.
//
// Safe to call at any time; the next check uses the new value.
func (e *Engine) SetMemoryCeiling(n int64) {
	e.mem.ceiling.Store(n)
	// Drop the cached verdict: it was computed against the old ceiling,
	// and a caller lowering it (or a test) expects the very next check
	// to apply the new one.
	e.mem.verdict.Store(nil)
}

// MemoryCeiling reports the configured ceiling as passed to
// SetMemoryCeiling (0 = automatic, negative = disabled). Stats.MemCeiling
// reports the effective byte value.
func (e *Engine) MemoryCeiling() int64 { return e.mem.ceiling.Load() }

// CheckMemory returns an ErrOutOfMemory error when the live heap is
// over the memory ceiling, and nil otherwise (including when the guard
// is inactive or disabled).
//
// The engine calls it before every row insert and update, index
// build, and table rewrite; the SQL layer calls it while materializing
// query results. Embedders running their own memory-hungry work (a
// bulk import that buffers, an in-process cache keyed off query
// results) can call it the same way. It is cheap — an atomic load and
// a clock read, plus a runtime/metrics read at most once per
// memSampleInterval — so per-row use is fine.
func (e *Engine) CheckMemory() error {
	if e.mem.ceiling.Load() < 0 {
		return nil
	}
	v := e.mem.current()
	if !v.over() {
		return nil
	}
	return serr.Wrap(ErrOutOfMemory,
		"heapMarked", strconv.FormatUint(v.live, 10),
		"memCeiling", strconv.FormatUint(v.ceiling, 10),
		"hint", "free memory (DELETE, DROP, TRUNCATE) or raise the ceiling: "+
			"GOMEMLIMIT, Engine.SetMemoryCeiling, or bytdbd -memory-ceiling")
}

// SetMaxResultRows changes the cap on rows a single query may
// materialize — its result, and each intermediate row set the SQL
// executor builds on the way there (view and CTE materializations,
// subquery value sets, hash-join build tables, GROUP BY groups, window
// inputs, sort buffers, RETURNING lists). A query over the cap fails
// with SQLSTATE 54000 (program_limit_exceeded) instead of allocating
// without bound. A value of 0 or less disables the cap, the same
// idiom as SetBackfillLimit.
//
// Note that ORDER BY without a usable index sorts the whole matching
// set before LIMIT applies, so "ORDER BY x LIMIT 10" over more rows
// than the cap is refused too — that query really does hold every
// matching row in memory. An index on x avoids both the sort and the
// refusal.
//
// Safe to call at any time; a statement reads the cap once at each
// materialization's start.
func (e *Engine) SetMaxResultRows(n int64) { e.maxResultRows.Store(n) }

// MaxResultRows reports the current result row cap (0 or less: none).
func (e *Engine) MaxResultRows() int64 { return e.maxResultRows.Load() }
