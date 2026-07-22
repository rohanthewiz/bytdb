package pgwire

// Regression: a binary text[] parameter carries a 4-byte element-count
// field that the wire controls independently of the body size. It was
// used verbatim as a make() capacity, so a 20-byte parameter declaring
// ~2.1 billion elements forced a ~34 GB allocation. That is a runtime
// out-of-memory throw — fatal, and it bypasses the per-connection
// recover fence, so one hostile Bind would take down the whole server.
// The decoder now clamps the preallocation to what the body can hold.

import (
	"encoding/binary"
	"testing"
)

func TestDecodeTextArrayBinaryHostileCount(t *testing.T) {
	// A well-formed 20-byte header: ndim=1, has-nulls=0, elem OID=text,
	// then a dimension whose declared element count is INT32_MAX. The
	// body carries no element bytes, so a correct decoder must reject it
	// (not try to allocate two billion slots).
	raw := make([]byte, 20)
	binary.BigEndian.PutUint32(raw[0:4], 1)         // ndim
	binary.BigEndian.PutUint32(raw[4:8], 0)         // has-nulls flag
	binary.BigEndian.PutUint32(raw[8:12], oidText)  // element OID
	binary.BigEndian.PutUint32(raw[12:16], 1<<31-1) // hostile element count
	// raw[16:20] is the dimension lower bound; value irrelevant.

	// Pre-fix this OOM-kills the test binary; post-fix it returns a clean
	// error because the body has no element bytes to back the count.
	if _, err := decodeTextArrayBinary(raw); err == nil {
		t.Fatal("decodeTextArrayBinary accepted a body with no elements for a 2.1e9 count")
	}

	// The same path reached through the public decodeParam dispatch.
	if _, err := decodeParam(raw, fmtBinary, oidTextArray); err == nil {
		t.Fatal("decodeParam(binary text[]) accepted a hostile element count")
	}

	// Sanity: a genuine small binary array still decodes, so the clamp
	// did not break the happy path. One element, the text "hi".
	ok := make([]byte, 0, 30)
	ok = binary.BigEndian.AppendUint32(ok, 1)       // ndim
	ok = binary.BigEndian.AppendUint32(ok, 0)       // has-nulls
	ok = binary.BigEndian.AppendUint32(ok, oidText) // elem OID
	ok = binary.BigEndian.AppendUint32(ok, 1)       // element count
	ok = binary.BigEndian.AppendUint32(ok, 1)       // lower bound
	ok = binary.BigEndian.AppendUint32(ok, 2)       // element length
	ok = append(ok, 'h', 'i')
	got, err := decodeTextArrayBinary(ok)
	if err != nil {
		t.Fatalf("valid array rejected: %v", err)
	}
	if got != "{hi}" {
		t.Fatalf("decoded %q, want {hi}", got)
	}
}
