package pgwire

import "testing"

// TestRbufByte covers rbuf.byte's short-read latch: the first read
// past the end sets bad and returns 0, and once latched every later
// read — even with bytes available — stays bad, so a caller can parse
// a whole message and check the flag once at the end.
func TestRbufByte(t *testing.T) {
	r := &rbuf{b: []byte{0xAB}}
	if v := r.byte(); v != 0xAB || r.bad {
		t.Fatalf("first byte: got 0x%02x bad=%v", v, r.bad)
	}
	// Buffer exhausted: the read fails and latches bad.
	if v := r.byte(); v != 0 || !r.bad {
		t.Fatalf("short read: got 0x%02x bad=%v; want 0, true", v, r.bad)
	}
	// Latched: bytes appearing later must not un-stick the flag.
	r.b = []byte{0xCD}
	if v := r.byte(); v != 0 || !r.bad {
		t.Fatalf("latched read: got 0x%02x bad=%v; want 0, true", v, r.bad)
	}
}
