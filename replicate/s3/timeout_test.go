package s3

// Regression: the default HTTP client must bound the phases a
// black-holed endpoint stalls in (dial, TLS handshake, wait for response
// headers). http.DefaultClient sets none of these, so a dead endpoint
// would block a ship — and, via the replicator's shipMu, all replication
// — indefinitely. A blanket http.Client.Timeout is deliberately NOT used
// because it would cap total body transfer and break a large restore
// Get, so the guarantee is expressed at the transport level.

import (
	"net/http"
	"testing"
)

func TestDefaultClientHasBoundedTransport(t *testing.T) {
	c, err := New(Config{
		Endpoint: "https://s3.example.com", Bucket: "b", Region: "r",
		AccessKey: "a", SecretKey: "s",
		// HTTPClient left nil so New installs the default.
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.http == http.DefaultClient {
		t.Fatal("New used http.DefaultClient (no timeouts) for the default client")
	}
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default client transport is %T, want *http.Transport", c.http.Transport)
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("ResponseHeaderTimeout not set — a silent endpoint would hang forever")
	}
	if tr.TLSHandshakeTimeout <= 0 {
		t.Error("TLSHandshakeTimeout not set")
	}
	if tr.DialContext == nil {
		t.Error("DialContext (with a dial timeout) not set")
	}
	// A blanket total-request timeout would break large restores; assert
	// we did not set one.
	if c.http.Timeout != 0 {
		t.Errorf("http.Client.Timeout = %v; want 0 (caps body transfer, breaks large Get)", c.http.Timeout)
	}
}
