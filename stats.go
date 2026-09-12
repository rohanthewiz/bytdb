package bytdb

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime/metrics"
	"strings"
)

// Stats is a point-in-time snapshot of the engine's size and of the Go
// runtime memory it lives in. bytdb is in-process and holds every row
// on the Go heap, so "is the database running low on RAM" is really
// "is this process's heap approaching what the host can give it" —
// which is why runtime figures sit alongside the store's own counts.
//
// The memory fields come from runtime/metrics, not
// runtime.ReadMemStats: the latter stops the world, the former does
// not, so Stats is cheap enough to poll every few seconds from a
// health check.
//
// How to read it as a low-memory signal:
//
//	HeapLive / MemLimit  > 0.8   -> approaching the ceiling
//	GCCPUSeconds rising fast     -> the collector is thrashing to stay
//	                                under MemLimit; an OOM kill is next
//	HeapLive / Keys              -> bytes per key; multiply by expected
//	                                growth to plan capacity
//
// MemLimit is zero when GOMEMLIMIT is unset (Go reports MaxInt64 for
// "no limit"; Stats folds that to zero so the ratio above is not a
// silent division by a huge number). Compare HeapLive against the
// container or host limit yourself in that case.
type Stats struct {
	// Keys is the number of entries in the underlying key-value store:
	// rows, secondary-index entries, and catalog descriptors together.
	// It is not a row count — a table with two secondary indexes
	// contributes three keys per row — but it is O(1) to read and
	// tracks the dataset's growth faithfully.
	Keys int `json:"keys"`

	// Tables is the number of user tables in the catalog.
	Tables int `json:"tables"`

	// LogEpoch and LogBytes describe the write-ahead log on disk: the
	// current file generation (bumped by compaction) and its size.
	// LogBytes grows with every write and is what a restart must
	// replay into memory — a useful proxy for recovery time.
	LogEpoch uint64 `json:"log_epoch"`
	LogBytes int64  `json:"log_bytes"`

	// HeapLive is the bytes occupied by live (reachable or not yet
	// swept) objects on the Go heap — the closest thing to "how much
	// memory the dataset takes".
	HeapLive uint64 `json:"heap_live_bytes"`

	// HeapGoal is the heap size at which the next GC cycle triggers. It
	// converges toward MemLimit under memory pressure, so a goal that
	// stops growing while HeapLive keeps rising means the limit is
	// doing the steering.
	HeapGoal uint64 `json:"heap_goal_bytes"`

	// RuntimeTotal is all memory mapped by the Go runtime: heap
	// (including freed-but-retained spans), stacks, and metadata. This
	// is roughly what the OS sees as the process's resident size,
	// minus page cache for the log file.
	RuntimeTotal uint64 `json:"runtime_total_bytes"`

	// MemLimit is the soft memory limit (GOMEMLIMIT or
	// debug.SetMemoryLimit), or zero when none is set.
	MemLimit uint64 `json:"mem_limit_bytes"`

	// GCCycles counts completed garbage collections since process
	// start; GCCPUSeconds is the CPU time the collector has consumed.
	// Both are cumulative, so sample twice and diff for a rate.
	GCCycles     uint64  `json:"gc_cycles"`
	GCCPUSeconds float64 `json:"gc_cpu_seconds"`

	// Goroutines is the live goroutine count — a leak in a caller's
	// connection handling shows up here before it shows up in memory.
	Goroutines int `json:"goroutines"`
}

// HeapFraction is HeapLive as a share of MemLimit, or zero when no
// limit is set. A value above ~0.8 is the practical "low on RAM"
// threshold: the runtime is already collecting aggressively to stay
// under the limit, and reachable memory (rows, an open transaction's
// writes) cannot be freed no matter how hard it tries.
func (s Stats) HeapFraction() float64 {
	if s.MemLimit == 0 {
		return 0
	}
	return float64(s.HeapLive) / float64(s.MemLimit)
}

// runtimeSamples lists the runtime/metrics keys Stats reads. The slice
// is built once per call rather than shared because metrics.Read
// writes into it, and Stats may be called from several goroutines.
func runtimeSamples() []metrics.Sample {
	return []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/gc/heap/goal:bytes"},
		{Name: "/memory/classes/total:bytes"},
		{Name: "/gc/gomemlimit:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
		{Name: "/cpu/classes/gc/total:cpu-seconds"},
		{Name: "/sched/goroutines:goroutines"},
	}
}

// sampleUint64 returns a metric's value, or zero if the runtime does
// not publish that metric (a KindBad sample). Every key above exists
// in the Go version bytdb requires, so zero only appears if a future
// runtime renames one — better a missing figure than a panic in a
// health endpoint.
func sampleUint64(s metrics.Sample) uint64 {
	if s.Value.Kind() == metrics.KindUint64 {
		return s.Value.Uint64()
	}
	return 0
}

func sampleFloat64(s metrics.Sample) float64 {
	if s.Value.Kind() == metrics.KindFloat64 {
		return s.Value.Float64()
	}
	return 0
}

// Stats returns a snapshot of the store's size and the process's
// memory state. The store figures are read under the kv read lock,
// so they are consistent with each other; the runtime figures are
// sampled immediately after and may be a GC cycle apart. Safe to call
// concurrently with any other engine operation, including from inside
// a transaction.
//
// If the engine is closed, the store fields read as zero and the
// runtime fields are still populated.
func (e *Engine) Stats() Stats {
	var s Stats
	s.Keys = e.kv.Len()
	s.Tables = len(e.Tables())
	// A closed store returns ErrClosed here; the zero values are the
	// honest answer, so the error is deliberately dropped.
	s.LogEpoch, s.LogBytes, _ = e.kv.LogState()

	samples := runtimeSamples()
	metrics.Read(samples)
	s.HeapLive = sampleUint64(samples[0])
	s.HeapGoal = sampleUint64(samples[1])
	s.RuntimeTotal = sampleUint64(samples[2])
	// The runtime reports "no limit" as math.MaxInt64; see the type
	// comment for why that is normalized to zero.
	if lim := sampleUint64(samples[3]); lim < 1<<62 {
		s.MemLimit = lim
	}
	s.GCCycles = sampleUint64(samples[4])
	s.GCCPUSeconds = sampleFloat64(samples[5])
	s.Goroutines = int(sampleUint64(samples[6]))
	return s
}

// promMetric is one line-set of the Prometheus text exposition format:
// a HELP line, a TYPE line, and the sample.
type promMetric struct {
	name, help, kind string
	value            string
}

// promMetrics renders the snapshot as Prometheus samples. Names carry
// the bytdb_ prefix and the conventional unit suffix so they slot into
// a scrape alongside process_* and go_* collectors without collision.
// Cumulative figures are counters, everything else a gauge.
func (s Stats) promMetrics() []promMetric {
	u := func(v uint64) string { return fmt.Sprint(v) }
	return []promMetric{
		{"bytdb_keys", "Entries in the key-value store (rows, index entries, catalog).", "gauge", fmt.Sprint(s.Keys)},
		{"bytdb_tables", "User tables in the catalog.", "gauge", fmt.Sprint(s.Tables)},
		{"bytdb_log_epoch", "Write-ahead log file generation; bumped by compaction.", "gauge", u(s.LogEpoch)},
		{"bytdb_log_bytes", "Write-ahead log size on disk.", "gauge", fmt.Sprint(s.LogBytes)},
		{"bytdb_heap_live_bytes", "Live Go heap; the dataset's memory footprint.", "gauge", u(s.HeapLive)},
		{"bytdb_heap_goal_bytes", "Heap size that triggers the next GC cycle.", "gauge", u(s.HeapGoal)},
		{"bytdb_runtime_total_bytes", "All memory mapped by the Go runtime.", "gauge", u(s.RuntimeTotal)},
		{"bytdb_mem_limit_bytes", "GOMEMLIMIT soft limit; 0 when unset.", "gauge", u(s.MemLimit)},
		{"bytdb_heap_fraction", "heap_live / mem_limit; 0 when no limit is set.", "gauge", fmt.Sprintf("%g", s.HeapFraction())},
		{"bytdb_gc_cycles_total", "Completed garbage collections.", "counter", u(s.GCCycles)},
		{"bytdb_gc_cpu_seconds_total", "CPU time spent in the garbage collector.", "counter", fmt.Sprintf("%g", s.GCCPUSeconds)},
		{"bytdb_goroutines", "Live goroutines.", "gauge", fmt.Sprint(s.Goroutines)},
	}
}

// WriteTo renders the snapshot in the Prometheus text exposition
// format (version 0.0.4), one HELP/TYPE/sample triple per field. It
// satisfies io.WriterTo so a handler can stream it straight to the
// response; MetricsHandler does exactly that.
func (s Stats) WriteTo(w io.Writer) (int64, error) {
	var b strings.Builder
	for _, m := range s.promMetrics() {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n%s %s\n", m.name, m.help, m.name, m.kind, m.name, m.value)
	}
	n, err := io.WriteString(w, b.String())
	return int64(n), err
}

// PrometheusContentType is the Content-Type MetricsHandler answers
// with — the text exposition format every Prometheus-compatible
// scraper accepts.
const PrometheusContentType = "text/plain; version=0.0.4; charset=utf-8"

// MetricsHandler serves the engine's Stats over HTTP for scraping.
// The default response is Prometheus text; a request whose Accept
// header names application/json gets the Stats struct as JSON instead,
// so the same URL serves a scraper and a curl-wielding operator.
//
// extra, if non-nil, is appended to the Prometheus output on every
// request — the hook a host uses to add gauges the engine cannot know
// about, such as its own connection count. It receives the writer
// after the engine's own metrics have been written.
//
// The handler does no authentication: bind it to a loopback or
// private address, as bytdbd's -metrics-addr does by default.
func MetricsHandler(e *Engine, extra func(w io.Writer)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s := e.Stats()
		if strings.Contains(r.Header.Get("Accept"), "application/json") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(s)
			return
		}
		w.Header().Set("Content-Type", PrometheusContentType)
		s.WriteTo(w)
		if extra != nil {
			extra(w)
		}
	})
}
