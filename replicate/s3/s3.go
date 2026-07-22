// Package s3 is a minimal, dependency-free S3-compatible object store
// client implementing replicate.Storage: Put, Get, List (V2), Delete,
// with AWS Signature Version 4 signing over net/http.
//
// It deliberately covers only what replication needs — no multipart
// uploads, ACLs, or bucket management — which keeps it a few hundred
// lines of stdlib and auditable end to end. It works against AWS S3
// and the S3-compatible stores this feature targets (Linode Object
// Storage, IDrive e2, MinIO, Backblaze B2, ...).
package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"
)

// defaultHTTPClient bounds how long a request may stall while still
// allowing arbitrarily large bodies. A blanket http.Client.Timeout
// cannot be used: it caps the WHOLE request including the response body,
// which would break a multi-GB restore Get. Instead the transport bounds
// only the phases a black-holed or half-open endpoint stalls in — the
// TCP dial, the TLS handshake, and the wait for response headers — so a
// dead endpoint fails a ship/restore in seconds (letting the next tick
// retry) instead of blocking the replicator, and via shipMu all of
// replication, indefinitely. http.DefaultClient sets none of these.
func defaultHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          100,
		},
	}
}

// Config identifies the bucket and credentials.
type Config struct {
	// Endpoint is the store's base URL, e.g.
	// "https://us-east-1.linodeobjects.com" or "http://localhost:9000".
	Endpoint string
	// Region for SigV4 scoping, e.g. "us-east-1". S3-compatibles often
	// accept anything here but AWS requires the real one.
	Region string
	// Bucket must already exist; this client never creates buckets.
	Bucket string

	AccessKey string
	SecretKey string

	// VirtualHost switches addressing from path-style
	// (endpoint/bucket/key — the default, and what most S3-compatibles
	// prefer) to virtual-hosted style (bucket.endpoint/key — AWS's
	// standard).
	VirtualHost bool

	// HTTPClient overrides http.DefaultClient, e.g. to set timeouts.
	HTTPClient *http.Client
}

// Client implements replicate.Storage against one bucket.
type Client struct {
	cfg  Config
	base *url.URL // scheme + host to hit (bucket folded in when VirtualHost)
	http *http.Client
}

// New validates cfg and returns a ready client. No network call is
// made; a bad endpoint or credential surfaces on first use.
func New(cfg Config) (*Client, error) {
	switch {
	case cfg.Endpoint == "":
		return nil, serr.New("s3: endpoint is required")
	case cfg.Bucket == "":
		return nil, serr.New("s3: bucket is required")
	case cfg.Region == "":
		return nil, serr.New("s3: region is required")
	case cfg.AccessKey == "" || cfg.SecretKey == "":
		return nil, serr.New("s3: credentials are required")
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, serr.Wrap(err, "op", "parse endpoint", "endpoint", cfg.Endpoint)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, serr.New("s3: endpoint must be http(s)", "endpoint", cfg.Endpoint)
	}
	if cfg.VirtualHost {
		u.Host = cfg.Bucket + "." + u.Host
	}
	c := &Client{cfg: cfg, base: u, http: cfg.HTTPClient}
	if c.http == nil {
		c.http = defaultHTTPClient()
	}
	return c, nil
}

// objectURL returns the request URL and the canonical URI path for key
// ("" addresses the bucket itself, for List).
func (c *Client) objectURL(key string) (*url.URL, string) {
	path := "/"
	if !c.cfg.VirtualHost {
		path += c.cfg.Bucket
		if key != "" {
			path += "/"
		}
	}
	path += uriEncode(key, true)
	u := *c.base
	// RawPath preserves our canonical encoding on the wire, so the
	// bytes signed are the bytes sent.
	u.Path, u.RawPath = mustPathUnescape(path), path
	return &u, path
}

// Put implements replicate.Storage. S3's PUT is atomic: the object
// appears complete or not at all, which is exactly the property the
// replicator's chunk protocol relies on.
func (c *Client) Put(ctx context.Context, key string, data []byte) error {
	resp, err := c.do(ctx, http.MethodPut, key, nil, data)
	if err != nil {
		return err
	}
	return drainAndCheck(resp, "put", key, http.StatusOK)
}

// Get implements replicate.Storage.
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr(resp, "get", key)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, serr.Wrap(err, "op", "get body", "key", key)
	}
	return data, nil
}

// Delete implements replicate.Storage. S3 returns 204 for missing keys
// too, giving the idempotency the interface asks for.
func (c *Client) Delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, key, nil, nil)
	if err != nil {
		return err
	}
	return drainAndCheck(resp, "delete", key, http.StatusNoContent, http.StatusOK)
}

// listResult mirrors the ListObjectsV2 response envelope, keys only.
type listResult struct {
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Keys                  []string `xml:"Contents>Key"`
}

// List implements replicate.Storage, paging ListObjectsV2 until the
// full ascending key set is collected.
func (c *Client) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, http.MethodGet, "", q, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, statusErr(resp, "list", prefix)
		}
		var page listResult
		err = xml.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, serr.Wrap(err, "op", "decode list response", "prefix", prefix)
		}
		keys = append(keys, page.Keys...)
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return keys, nil
		}
		token = page.NextContinuationToken
	}
}

// do builds, signs, and sends one request. body is nil for GET/DELETE.
func (c *Client) do(ctx context.Context, method, key string, query url.Values, body []byte) (*http.Response, error) {
	u, canonicalPath := c.objectURL(key)
	// The wire query is built with the same strict encoder the
	// signature uses (url.Values.Encode would differ on spaces: '+'
	// versus the canonical %20), so the bytes signed are exactly the
	// bytes sent and no server-side re-canonicalization can disagree.
	canonicalQuery := canonicalQueryString(query)
	u.RawQuery = canonicalQuery
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, serr.Wrap(err, "op", "build request", "key", key)
	}
	req.ContentLength = int64(len(body))
	c.sign(req, canonicalPath, canonicalQuery, body, time.Now().UTC())
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, serr.Wrap(err, "op", strings.ToLower(method), "key", key)
	}
	return resp, nil
}

// sign applies AWS Signature Version 4 with the payload hash carried in
// x-amz-content-sha256 (the S3 variant of SigV4). The payload is always
// fully in memory here, so real hashing costs little and avoids the
// UNSIGNED-PAYLOAD escape hatch some stores reject over plain http.
func (c *Client) sign(req *http.Request, canonicalPath, canonicalQuery string, body []byte, now time.Time) {
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadHash := hex.EncodeToString(sha256Sum(body))

	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	canonicalHeaders := "host:" + req.URL.Host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	const signedHeaders = "host;x-amz-content-sha256;x-amz-date"

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalPath,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := dateStamp + "/" + c.cfg.Region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(sha256Sum([]byte(canonicalRequest))),
	}, "\n")

	// Signing key: a four-stage HMAC chain rooted in the secret.
	kDate := hmacSHA256([]byte("AWS4"+c.cfg.SecretKey), dateStamp)
	kRegion := hmacSHA256(kDate, c.cfg.Region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.cfg.AccessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)
}

// canonicalQueryString renders query per SigV4: parameters sorted,
// keys and values strictly URI-encoded, joined with '&'.
func canonicalQueryString(query url.Values) string {
	var parts []string
	for k, vs := range query {
		for _, v := range vs {
			parts = append(parts, uriEncode(k, false)+"="+uriEncode(v, false))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// uriEncode is AWS's URI encoding: unreserved characters
// (A-Za-z0-9-._~) pass through, everything else becomes uppercase
// %XX per byte; '/' passes through only in paths.
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~',
			keepSlash && c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// mustPathUnescape decodes an encoded path for url.URL.Path; our
// encoder only ever produces valid escapes, so failure is impossible.
func mustPathUnescape(p string) string {
	out, err := url.PathUnescape(p)
	if err != nil {
		return p
	}
	return out
}

// drainAndCheck consumes and closes the response, accepting any of the
// given status codes.
func drainAndCheck(resp *http.Response, op, key string, okCodes ...int) error {
	defer resp.Body.Close()
	if slices.Contains(okCodes, resp.StatusCode) {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return statusErr(resp, op, key)
}

// statusErr wraps an unexpected response, keeping a snippet of the
// body — S3 errors carry their reason as XML text.
func statusErr(resp *http.Response, op, key string) error {
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return serr.New("s3: unexpected response",
		"op", op, "key", key, "status", resp.Status, "body", string(snippet))
}
