package pgwire_test

// Deep-look D-51: a startup asking for protocol 3.2 (libpq 18 does) or
// carrying _pq_. protocol options gets NegotiateProtocolVersion and a
// 3.0 session, instead of a silently closed socket.

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// startupFrame frames a startup packet for version with params.
func startupFrame(version uint32, params ...string) []byte {
	body := binary.BigEndian.AppendUint32(nil, version)
	for _, p := range params {
		body = append(body, p...)
		body = append(body, 0)
	}
	body = append(body, 0)
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(body)+4)), body...)
}

// readFrame reads one backend frame.
func readFrame(t *testing.T, r *bufio.Reader) (byte, []byte) {
	t.Helper()
	typ, err := r.ReadByte()
	if err != nil {
		t.Fatalf("read frame type: %v", err)
	}
	var lb [4]byte
	if _, err := io.ReadFull(r, lb[:]); err != nil {
		t.Fatalf("read frame length: %v", err)
	}
	body := make([]byte, int(binary.BigEndian.Uint32(lb[:]))-4)
	if _, err := io.ReadFull(r, body); err != nil {
		t.Fatalf("read frame body: %v", err)
	}
	return typ, body
}

func TestNegotiateProtocolVersion(t *testing.T) {
	url := startServer(t)
	addr := strings.TrimPrefix(strings.TrimSuffix(url, "/test"), "postgres://test@")
	for _, tc := range []struct {
		name      string
		version   uint32
		params    []string
		negotiate bool
		opts      []string
	}{
		{"3.2 with an option", 3<<16 | 2, []string{"user", "test", "_pq_.test_opt", "x"}, true, []string{"_pq_.test_opt"}},
		{"3.2 alone", 3<<16 | 2, []string{"user", "test"}, true, nil},
		{"3.0 with an option", 3 << 16, []string{"user", "test", "_pq_.a", "1", "_pq_.b", "2"}, true, []string{"_pq_.a", "_pq_.b"}},
		{"3.0 plain", 3 << 16, []string{"user", "test"}, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nc, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer nc.Close()
			nc.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := nc.Write(startupFrame(tc.version, tc.params...)); err != nil {
				t.Fatal(err)
			}
			r := bufio.NewReader(nc)
			typ, body := readFrame(t, r)
			if tc.negotiate {
				if typ != 'v' {
					t.Fatalf("first frame %q, want NegotiateProtocolVersion", typ)
				}
				if minor := binary.BigEndian.Uint32(body); minor != 0 {
					t.Errorf("newest minor = %d, want 0", minor)
				}
				n := int(binary.BigEndian.Uint32(body[4:]))
				var got []string
				for _, s := range strings.Split(string(body[8:]), "\x00") {
					if s != "" {
						got = append(got, s)
					}
				}
				if n != len(tc.opts) || strings.Join(got, ",") != strings.Join(tc.opts, ",") {
					t.Errorf("options %d %q, want %q", n, got, tc.opts)
				}
				typ, _ = readFrame(t, r)
			}
			if typ != 'R' {
				t.Fatalf("frame %q, want AuthenticationOk", typ)
			}
			pgReadUntilReady(t, r)
			// The session works, and the option did not become a setting.
			if _, err := nc.Write(pgSimpleQuery(`select 1`)); err != nil {
				t.Fatal(err)
			}
			pgReadUntilReady(t, r)
		})
	}
}
