package bytdb

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// TestStats checks the store-side figures track inserts and that the
// runtime figures are populated (non-zero) — the exact heap numbers
// are the runtime's business, not ours.
func TestStats(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()

	before := e.Stats()
	if before.Tables != 0 {
		t.Fatalf("fresh engine: tables = %d, want 0", before.Tables)
	}

	usersTable(t, e)
	for i := range 50 {
		if err := e.Insert("users", int64(i), "n", 1.0, true, nil); err != nil {
			t.Fatal(err)
		}
	}

	after := e.Stats()
	if after.Tables != 1 {
		t.Errorf("tables = %d, want 1", after.Tables)
	}
	// 50 rows plus the descriptor; secondary indexes (if usersTable
	// declares any) only add to it, so >= is the honest bound.
	if after.Keys < before.Keys+51 {
		t.Errorf("keys = %d, want >= %d", after.Keys, before.Keys+51)
	}
	if after.LogBytes <= before.LogBytes {
		t.Errorf("log bytes did not grow: %d -> %d", before.LogBytes, after.LogBytes)
	}
	if after.HeapLive == 0 || after.HeapGoal == 0 || after.RuntimeTotal == 0 || after.Goroutines == 0 {
		t.Errorf("runtime figures not populated: %+v", after)
	}
}

// TestStatsMemLimit pins the "no limit reads as zero" normalization
// and the fraction that depends on it.
func TestStatsMemLimit(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()

	prev := debug.SetMemoryLimit(-1) // read without changing
	defer debug.SetMemoryLimit(prev)

	debug.SetMemoryLimit(1 << 40) // 1 TiB: never reached, but set
	s := e.Stats()
	if s.MemLimit != 1<<40 {
		t.Errorf("mem limit = %d, want %d", s.MemLimit, 1<<40)
	}
	if f := s.HeapFraction(); f <= 0 || f >= 1 {
		t.Errorf("heap fraction = %g, want in (0,1)", f)
	}

	debug.SetMemoryLimit(1<<63 - 1) // the runtime's "unlimited"
	s = e.Stats()
	if s.MemLimit != 0 {
		t.Errorf("unlimited: mem limit = %d, want 0", s.MemLimit)
	}
	if f := s.HeapFraction(); f != 0 {
		t.Errorf("unlimited: heap fraction = %g, want 0", f)
	}
}

// TestMetricsHandler covers both response formats and the extra hook.
func TestMetricsHandler(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	usersTable(t, e)

	h := MetricsHandler(e, func(w io.Writer) { io.WriteString(w, "extra_gauge 7\n") })

	// Prometheus text by default.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != PrometheusContentType {
		t.Errorf("content type %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE bytdb_tables gauge\nbytdb_tables 1\n",
		"# TYPE bytdb_gc_cycles_total counter\n",
		"bytdb_heap_live_bytes ",
		"extra_gauge 7\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("prometheus body missing %q:\n%s", want, body)
		}
	}

	// JSON on request.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Accept", "application/json")
	h.ServeHTTP(rec, req)
	var s Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("json: %v\n%s", err, rec.Body.String())
	}
	if s.Tables != 1 || s.HeapLive == 0 {
		t.Errorf("json stats = %+v", s)
	}

	// Anything but GET/HEAD is refused.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status %d, want 405", rec.Code)
	}
}
