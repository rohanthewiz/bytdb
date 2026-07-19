package s3

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
)

const (
	testRegion = "test-region-1"
	testBucket = "unit-bucket"
	testAccess = "AKIDEXAMPLE"
	testSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
)

// fakeS3 is an in-memory, signature-verifying S3 endpoint. Every
// request has its SigV4 signature re-derived from the raw request and
// compared to the Authorization header, so a client encoding bug (path,
// query, or payload hash) fails tests here rather than against a real
// store. Listing paginates at two keys per page to force the
// continuation-token path.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	t       *testing.T
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !f.verifySignature(r, body) {
		http.Error(w, "SignatureDoesNotMatch", http.StatusForbidden)
		return
	}
	bucketPrefix := "/" + testBucket
	if !strings.HasPrefix(r.URL.EscapedPath(), bucketPrefix) {
		http.Error(w, "NoSuchBucket", http.StatusNotFound)
		return
	}
	key, _ := url.PathUnescape(strings.TrimPrefix(strings.TrimPrefix(r.URL.EscapedPath(), bucketPrefix), "/"))

	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPut:
		f.objects[key] = body
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && key == "":
		f.serveList(w, r)
	case r.Method == http.MethodGet:
		data, ok := f.objects[key]
		if !ok {
			http.Error(w, "NoSuchKey", http.StatusNotFound)
			return
		}
		w.Write(data)
	case r.Method == http.MethodDelete:
		delete(f.objects, key) // deleting a missing key is 204 too, per S3
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "MethodNotAllowed", http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) serveList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("list-type") != "2" {
		http.Error(w, "only ListObjectsV2 supported", http.StatusBadRequest)
		return
	}
	prefix, token := q.Get("prefix"), q.Get("continuation-token")

	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) && (token == "" || k > token) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	const pageSize = 2
	truncated := len(keys) > pageSize
	if truncated {
		keys = keys[:pageSize]
	}
	fmt.Fprint(w, `<?xml version="1.0"?><ListBucketResult>`)
	fmt.Fprintf(w, "<IsTruncated>%v</IsTruncated>", truncated)
	if truncated {
		fmt.Fprintf(w, "<NextContinuationToken>%s</NextContinuationToken>", keys[len(keys)-1])
	}
	for _, k := range keys {
		fmt.Fprintf(w, "<Contents><Key>%s</Key></Contents>", k)
	}
	fmt.Fprint(w, "</ListBucketResult>")
}

// verifySignature re-derives the SigV4 signature from the request as
// received and compares it to the one the client sent.
func (f *fakeS3) verifySignature(r *http.Request, body []byte) bool {
	auth := r.Header.Get("Authorization")
	sigIdx := strings.LastIndex(auth, "Signature=")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential="+testAccess+"/") || sigIdx < 0 {
		f.t.Errorf("malformed Authorization header: %q", auth)
		return false
	}
	gotSig := auth[sigIdx+len("Signature="):]

	amzDate := r.Header.Get("X-Amz-Date")
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if want := hex.EncodeToString(sha256Sum(body)); payloadHash != want {
		f.t.Errorf("payload hash header %s, computed %s", payloadHash, want)
		return false
	}

	canonicalRequest := strings.Join([]string{
		r.Method,
		r.URL.EscapedPath(),
		canonicalQueryString(r.URL.Query()),
		"host:" + r.Host + "\nx-amz-content-sha256:" + payloadHash + "\nx-amz-date:" + amzDate + "\n",
		"host;x-amz-content-sha256;x-amz-date",
		payloadHash,
	}, "\n")
	scope := amzDate[:8] + "/" + testRegion + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" +
		hex.EncodeToString(sha256Sum([]byte(canonicalRequest)))

	kDate := hmacSHA256([]byte("AWS4"+testSecret), amzDate[:8])
	kRegion := hmacSHA256(kDate, testRegion)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	wantSig := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	if gotSig != wantSig {
		f.t.Errorf("signature mismatch\ncanonical request:\n%s", canonicalRequest)
		return false
	}
	return true
}

func newTestClient(t *testing.T) (*Client, *fakeS3) {
	t.Helper()
	fake := &fakeS3{objects: map[string][]byte{}, t: t}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c, err := New(Config{
		Endpoint:  srv.URL,
		Region:    testRegion,
		Bucket:    testBucket,
		AccessKey: testAccess,
		SecretKey: testSecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, fake
}

// TestPutGetDelete round-trips an object, including a key with
// characters that exercise the URI encoder.
func TestPutGetDelete(t *testing.T) {
	c, _ := newTestClient(t)
	ctx := context.Background()
	key := "gen/2026 test+id/0000000000000000-0000000000000040.wlog"
	payload := []byte("hello replication")

	if err := c.Put(ctx, key, payload); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}
	if err := c.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, key); err == nil {
		t.Fatal("get after delete succeeded")
	}
	if err := c.Delete(ctx, key); err != nil {
		t.Fatalf("deleting a missing key must be idempotent: %v", err)
	}
}

// TestListPagination stores enough keys to force several pages (the
// fake paginates at two) and checks order, completeness, and prefix
// filtering — with a prefix containing a space, the exact case where
// query encoding and signing can disagree.
func TestListPagination(t *testing.T) {
	c, _ := newTestClient(t)
	ctx := context.Background()
	var want []string
	for i := range 7 {
		key := fmt.Sprintf("gen/site a/%016x.wlog", i*100)
		want = append(want, key)
		if err := c.Put(ctx, key, []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Put(ctx, "other/stray", []byte("x")); err != nil {
		t.Fatal(err)
	}

	got, err := c.List(ctx, "gen/site a/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("listed %d keys, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("key[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestConfigValidation pins the fail-fast checks in New.
func TestConfigValidation(t *testing.T) {
	bad := []Config{
		{},
		{Endpoint: "https://x", Bucket: "b", Region: "r"},                               // no creds
		{Endpoint: "ftp://x", Bucket: "b", Region: "r", AccessKey: "a", SecretKey: "s"}, // bad scheme
		{Endpoint: "https://x", Region: "r", AccessKey: "a", SecretKey: "s"},            // no bucket
	}
	for i, cfg := range bad {
		if _, err := New(cfg); err == nil {
			t.Errorf("config %d accepted: %+v", i, cfg)
		}
	}
	if _, err := New(Config{Endpoint: "https://x", Bucket: "b", Region: "r", AccessKey: "a", SecretKey: "s"}); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

// TestVirtualHostAddressing checks the URL and canonical path shapes
// for both addressing modes without a network round trip.
func TestVirtualHostAddressing(t *testing.T) {
	pathStyle, err := New(Config{Endpoint: "https://obj.example.com", Region: "r", Bucket: "b", AccessKey: "a", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	u, canonical := pathStyle.objectURL("k1/k 2")
	if u.Host != "obj.example.com" || canonical != "/b/k1/k%202" {
		t.Fatalf("path-style: host=%q canonical=%q", u.Host, canonical)
	}

	vhost, err := New(Config{Endpoint: "https://obj.example.com", Region: "r", Bucket: "b", AccessKey: "a", SecretKey: "s", VirtualHost: true})
	if err != nil {
		t.Fatal(err)
	}
	u, canonical = vhost.objectURL("k1")
	if u.Host != "b.obj.example.com" || canonical != "/k1" {
		t.Fatalf("virtual-host: host=%q canonical=%q", u.Host, canonical)
	}
}
