package pgwire

// dos_test.go: unit-level coverage for the DoS-hardening knobs that are
// awkward to reach through a live pgx client — duplicate-Parse rejection,
// the per-connection statement/portal ceilings, the incremental body
// reader, and the timeout/limit resolvers. The wire-level timeout
// behaviors (write deadline, idle-session) are exercised end to end in
// dos_wire_test.go.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/sql"
)

// newDriveConn builds a conn whose backend output lands in the returned
// buffer, driving the extended-protocol handlers directly (below run's
// recover fence and without a socket), the way rowlimit_test does.
func newDriveConn(t *testing.T) (*conn, *bytes.Buffer) {
	t.Helper()
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	db := sql.New(e)
	setup := db.NewSession()
	st, err := db.Prepare("CREATE TABLE t (id INT PRIMARY KEY)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.ExecStmt(st); err != nil {
		t.Fatal(err)
	}
	setup.Close()

	out := &bytes.Buffer{}
	c := &conn{
		srv:     NewServer(db),
		w:       bufio.NewWriter(out),
		sess:    db.NewSession(),
		stmts:   map[string]*prepared{},
		portals: map[string]*portal{},
	}
	t.Cleanup(func() { c.sess.Close() })
	return c, out
}

// flushErrState flushes the writer and returns the SQLSTATE carried in
// the last ErrorResponse the handler emitted, or "" if none.
func flushErrState(t *testing.T, c *conn, out *bytes.Buffer) string {
	t.Helper()
	c.w.Flush()
	code := ""
	b := out.Bytes()
	for len(b) >= 5 {
		typ := b[0]
		n := int(binary.BigEndian.Uint32(b[1:5])) - 4
		if n < 0 || 5+n > len(b) {
			t.Fatalf("malformed backend frame %q", typ)
		}
		if typ == msgErrorResponse {
			// Walk the field list for the 'C' (SQLSTATE) field.
			for f := b[5 : 5+n]; len(f) > 0 && f[0] != 0; {
				key := f[0]
				f = f[1:]
				z := bytes.IndexByte(f, 0)
				if z < 0 {
					break
				}
				if key == 'C' {
					code = string(f[:z])
				}
				f = f[z+1:]
			}
		}
		b = b[5+n:]
	}
	out.Reset()
	return code
}

func parseFrame(name, query string) *rbuf {
	var b wbuf
	b.cstr(name)
	b.cstr(query)
	b.int16(0) // no declared param OIDs
	return &rbuf{b: []byte(b)}
}

// TestParseDuplicateName: a second Parse under an existing *named*
// statement is refused with 42P05, as Postgres does, while the unnamed
// statement may be re-Parsed freely.
func TestParseDuplicateName(t *testing.T) {
	c, out := newDriveConn(t)

	c.parse(parseFrame("s1", "select id from t"))
	if code := flushErrState(t, c, out); code != "" {
		t.Fatalf("first Parse errored: %s", code)
	}
	c.inErr = false
	c.parse(parseFrame("s1", "select id from t"))
	if code := flushErrState(t, c, out); code != "42P05" {
		t.Fatalf("duplicate named Parse: got %q, want 42P05", code)
	}
	if !c.inErr {
		t.Fatal("duplicate Parse should enter discard-until-Sync state")
	}

	// The unnamed statement is replaceable — re-Parsing it is not an error.
	c.inErr = false
	c.parse(parseFrame("", "select id from t"))
	c.parse(parseFrame("", "select id from t"))
	if code := flushErrState(t, c, out); code != "" {
		t.Fatalf("unnamed re-Parse errored: %s", code)
	}
}

// TestPreparedStatementCap: a new named statement past the ceiling is
// refused with 54000 rather than growing the map unbounded.
func TestPreparedStatementCap(t *testing.T) {
	c, out := newDriveConn(t)
	for i := range maxPreparedStmts {
		c.stmts[fmt.Sprintf("s%d", i)] = &prepared{}
	}
	c.parse(parseFrame("one-too-many", "select id from t"))
	if code := flushErrState(t, c, out); code != "54000" {
		t.Fatalf("over-cap Parse: got %q, want 54000", code)
	}
}

// TestPortalCap: a new named portal past the ceiling is refused with
// 54000 before the statement is even looked up.
func TestPortalCap(t *testing.T) {
	c, out := newDriveConn(t)
	for i := range maxPortals {
		c.portals[fmt.Sprintf("p%d", i)] = &portal{}
	}
	var b wbuf
	b.cstr("one-too-many") // portal name
	b.cstr("s1")           // statement name
	b.int16(0)             // param format codes
	b.int16(0)             // param values
	b.int16(0)             // result format codes
	c.bind(&rbuf{b: []byte(b)})
	if code := flushErrState(t, c, out); code != "54000" {
		t.Fatalf("over-cap Bind: got %q, want 54000", code)
	}
}

// TestReadBodyBounds: a full body round-trips; a truncated stream that
// declared a large length fails cleanly (and, by construction, the
// allocation grows only as bytes arrive — see readBodyPrealloc); an
// over-cap length and a zero length are handled at the edges.
func TestReadBodyBounds(t *testing.T) {
	frameOf := func(declaredLen int, payload []byte) *bufio.Reader {
		var raw []byte
		raw = binary.BigEndian.AppendUint32(raw, uint32(declaredLen+4))
		raw = append(raw, payload...)
		return bufio.NewReader(bytes.NewReader(raw))
	}

	// A body well past the pre-allocation threshold round-trips intact.
	big := bytes.Repeat([]byte{0xAB}, 4*readBodyPrealloc)
	got, err := readBody(frameOf(len(big), big))
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("full body: err=%v len(got)=%d", err, len(got))
	}

	// A huge declared length with only a few bytes present must fail as a
	// short read, not hang or allocate the declared size.
	if _, err := readBody(frameOf(1<<25, []byte{1, 2, 3})); err != io.ErrUnexpectedEOF {
		t.Fatalf("truncated huge claim: err=%v, want ErrUnexpectedEOF", err)
	}

	// Over the hard frame cap is rejected before any read of the body.
	if _, err := readBody(frameOf(maxMsgLen+1, nil)); err == nil {
		t.Fatal("over-cap length accepted")
	}

	// A zero-length body is nil, no error.
	if got, err := readBody(frameOf(0, nil)); err != nil || got != nil {
		t.Fatalf("zero-length body: err=%v got=%v", err, got)
	}
}

// TestLimitResolvers pins the zero/negative/positive policy of the
// server's tunables, since the accept loop and per-conn deadlines depend
// on it: zero means "safe default", negative means "off/unlimited".
func TestLimitResolvers(t *testing.T) {
	// MaxConns: 0 -> default, negative -> unlimited (0 to the accept loop).
	if got := (&Server{}).maxConns(); got != DefaultMaxConns {
		t.Fatalf("maxConns() zero = %d, want %d", got, DefaultMaxConns)
	}
	if got := (&Server{MaxConns: -1}).maxConns(); got != 0 {
		t.Fatalf("maxConns() negative = %d, want 0 (unlimited)", got)
	}
	if got := (&Server{MaxConns: 7}).maxConns(); got != 7 {
		t.Fatalf("maxConns() explicit = %d, want 7", got)
	}

	// WriteTimeout / ReadTimeout: 0 -> DefaultIOTimeout, negative -> off.
	for _, tc := range []struct {
		name string
		get  func(*Server) time.Duration
		set  func(*Server, time.Duration)
	}{
		{"writeTimeout", (*Server).writeTimeout, func(s *Server, d time.Duration) { s.WriteTimeout = d }},
		{"readTimeout", (*Server).readTimeout, func(s *Server, d time.Duration) { s.ReadTimeout = d }},
	} {
		if got := tc.get(&Server{}); got != DefaultIOTimeout {
			t.Fatalf("%s zero = %v, want %v", tc.name, got, DefaultIOTimeout)
		}
		off := &Server{}
		tc.set(off, -1)
		if got := tc.get(off); got != 0 {
			t.Fatalf("%s negative = %v, want 0 (off)", tc.name, got)
		}
		set := &Server{}
		tc.set(set, 2*time.Second)
		if got := tc.get(set); got != 2*time.Second {
			t.Fatalf("%s explicit = %v, want 2s", tc.name, got)
		}
	}

	// IdleTimeout: opt-in — only a positive value enables it.
	if got := (&Server{}).idleTimeout(); got != 0 {
		t.Fatalf("idleTimeout() zero = %v, want 0 (off)", got)
	}
	if got := (&Server{IdleTimeout: -1}).idleTimeout(); got != 0 {
		t.Fatalf("idleTimeout() negative = %v, want 0 (off)", got)
	}
	if got := (&Server{IdleTimeout: time.Second}).idleTimeout(); got != time.Second {
		t.Fatalf("idleTimeout() explicit = %v, want 1s", got)
	}
}
