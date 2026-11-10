package main

// Deep-look D-58: the daemon's replication pieces — location parsing,
// restore-if-missing, the health endpoint and the replication gauges —
// against an in-memory store standing in for the bucket.

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/replicate"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

// memStore is a replicate.Storage in a map; failPut makes every upload
// fail, for the unhealthy paths.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	failPut bool
}

func newMemStore() *memStore { return &memStore{objects: map[string][]byte{}} }

func (m *memStore) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failPut {
		return errors.New("bucket unreachable")
	}
	m.objects[key] = append([]byte(nil), data...)
	return nil
}

func (m *memStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.objects[key]
	if !ok {
		return nil, errors.New("no such key " + key)
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

func TestParseS3URL(t *testing.T) {
	for in, want := range map[string]s3Location{
		"s3://bkt":          {bucket: "bkt"},
		"s3://bkt/sites/a":  {bucket: "bkt", prefix: "sites/a"},
		"s3://bkt/sites/a/": {bucket: "bkt", prefix: "sites/a/"},
	} {
		got, err := parseS3URL(in)
		if err != nil || got != want {
			t.Errorf("parseS3URL(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "bkt/x", "https://bkt/x", "s3:///x"} {
		if _, err := parseS3URL(bad); err == nil {
			t.Errorf("parseS3URL(%q) accepted", bad)
		}
	}
}

func TestS3CredentialsFromEnv(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	if _, err := (s3Settings{}).client(s3Location{bucket: "b"}); err == nil {
		t.Error("client built without credentials")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "AKID")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	if _, err := (s3Settings{region: "eu-west-1"}).client(s3Location{bucket: "b"}); err != nil {
		t.Errorf("client with env credentials: %v", err)
	}
}

func TestRestoreIfMissing(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	e, err := bytdb.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	d := bsql.New(e)
	if _, err := d.Exec(`create table t (id int primary key, v text)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`insert into t values (1, 'replicated')`); err != nil {
		t.Fatal(err)
	}
	r := replicate.New(e, store, replicate.Options{Prefix: "db1"})
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	r.Close()
	e.Close()

	dst := filepath.Join(dir, "dst.db")
	ran, info, err := restoreIfMissing(ctx, store, "db1", dst)
	if err != nil || !ran || info.Bytes == 0 {
		t.Fatalf("restore: ran=%v info=%+v err=%v", ran, info, err)
	}
	e2, err := bytdb.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	res, err := bsql.New(e2).Exec(`select v from t`)
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != "replicated" {
		t.Fatalf("restored rows: %v %v", res, err)
	}
	e2.Close()
	// An existing file is left alone.
	if ran, _, err := restoreIfMissing(ctx, store, "db1", dst); err != nil || ran {
		t.Errorf("restore over an existing file: ran=%v err=%v", ran, err)
	}
}

func TestHealthAndReplicationMetrics(t *testing.T) {
	ctx := context.Background()
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	get := func(rp replication) (int, string) {
		rec := httptest.NewRecorder()
		healthHandler(e, rp).ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
		return rec.Code, rec.Body.String()
	}
	if code, body := get(replication{}); code != 200 || !strings.Contains(body, `"status":"ok"`) || strings.Contains(body, "replication") {
		t.Errorf("no replication: %d %s", code, body)
	}

	store := newMemStore()
	r := replicate.New(e, store, replicate.Options{Interval: time.Hour})
	defer r.Close()
	if err := r.ShipNow(ctx); err != nil {
		t.Fatal(err)
	}
	rp := replication{r: r, interval: 10 * time.Millisecond, started: time.Now()}
	if code, body := get(rp); code != 200 || !strings.Contains(body, `"sealed":true`) {
		t.Errorf("healthy replication: %d %s", code, body)
	}
	var m bytes.Buffer
	rp.writeMetrics(&m)
	for _, g := range []string{"bytdb_replication_lag_bytes 0", "bytdb_replication_sealed 1", "bytdb_replication_failing 0"} {
		if !strings.Contains(m.String(), g) {
			t.Errorf("metrics missing %q:\n%s", g, m.String())
		}
	}

	// A bucket that stops accepting uploads turns health once ships have
	// failed for longer than three intervals; one failed tick does not.
	store.mu.Lock()
	store.failPut = true
	store.mu.Unlock()
	bsql.New(e).Exec(`create table more (id int primary key)`) // something to ship
	if err := r.ShipNow(ctx); err == nil {
		t.Fatal("ship to a failing bucket succeeded")
	}
	st, _ := rp.status()
	if rp.failing(st, st.LastShipTime.Add(rp.interval)) {
		t.Error("one interval of failure already unhealthy")
	}
	time.Sleep(4 * rp.interval)
	if code, body := get(rp); code != 503 || !strings.Contains(body, "last_error") {
		t.Errorf("failing replication: %d %s", code, body)
	}
	m.Reset()
	rp.writeMetrics(&m)
	if !strings.Contains(m.String(), "bytdb_replication_failing 1") {
		t.Errorf("metrics do not report the failure:\n%s", m.String())
	}
}
