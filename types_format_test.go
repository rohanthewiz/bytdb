package bytdb

// Direct coverage for the value FORMATTERS — the render half of the
// parse/format pairs the SQL layer and wire server share. The parsers
// are exercised heavily through query execution; the formatters are
// reached mostly on the read path, so they get explicit round-trip and
// edge-case tests here (pre-epoch instants, zero vs. non-zero
// fractions, and the deliberately non-panicking corrupt-UUID path).

import (
	"strings"
	"testing"
)

func TestFormatTimestamp(t *testing.T) {
	cases := []struct {
		micros int64
		want   string
	}{
		{0, "1970-01-01 00:00:00+00"},                // the epoch, no fraction
		{2_000_000, "1970-01-01 00:00:02+00"},        // whole seconds stay bare
		{1_500_000, "1970-01-01 00:00:01.5+00"},      // trailing zeros trimmed
		{1_234_567, "1970-01-01 00:00:01.234567+00"}, // full micro precision
		{-1, "1969-12-31 23:59:59.999999+00"},        // pre-epoch: fraction wraps positive
		{-2_000_000, "1969-12-31 23:59:58+00"},       // pre-epoch whole second
		{1_704_164_645_123456, "2024-01-02 03:04:05.123456+00"},
	}
	for _, c := range cases {
		if got := FormatTimestamp(c.micros); got != c.want {
			t.Errorf("FormatTimestamp(%d) = %q, want %q", c.micros, got, c.want)
		}
	}
}

func TestFormatDate(t *testing.T) {
	cases := []struct {
		days int64
		want string
	}{
		{0, "1970-01-01"},
		{-1, "1969-12-31"}, // midnight UTC divides evenly, so pre-1970 holds
		{19724, "2024-01-02"},
	}
	for _, c := range cases {
		if got := FormatDate(c.days); got != c.want {
			t.Errorf("FormatDate(%d) = %q, want %q", c.days, got, c.want)
		}
	}
}

func TestFormatUUID(t *testing.T) {
	// A 16-byte value renders canonical lowercase dashed.
	b := []byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0,
		0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}
	if got, want := FormatUUID(b), "12345678-9abc-def0-1122-334455667788"; got != want {
		t.Errorf("FormatUUID(canonical) = %q, want %q", got, want)
	}
	// A wrong-length value must stay visible, not panic. It renders as a
	// bytea-style hex escape so a corrupt column is diagnosable.
	if got, want := FormatUUID([]byte{0xde, 0xad}), `\xdead`; got != want {
		t.Errorf("FormatUUID(short) = %q, want %q", got, want)
	}
	if got, want := FormatUUID(nil), `\x`; got != want {
		t.Errorf("FormatUUID(nil) = %q, want %q", got, want)
	}
}

// TestValueTextRoundTrip locks the parse∘format identity that the wire
// server relies on: whatever a formatter emits, the matching parser
// reads back to the same runtime value.
func TestValueTextRoundTrip(t *testing.T) {
	t.Run("timestamp", func(t *testing.T) {
		for _, micros := range []int64{0, -1, 1_500_000, 1_704_164_645_123456, -62_135_596_800_000000} {
			got, err := ParseTimestamp(FormatTimestamp(micros))
			if err != nil {
				t.Fatalf("ParseTimestamp(FormatTimestamp(%d)): %v", micros, err)
			}
			if got != micros {
				t.Errorf("timestamp round trip: %d -> %q -> %d", micros, FormatTimestamp(micros), got)
			}
		}
	})
	t.Run("date", func(t *testing.T) {
		for _, days := range []int64{0, -1, 19724, -25567} {
			got, err := ParseDate(FormatDate(days))
			if err != nil {
				t.Fatalf("ParseDate(FormatDate(%d)): %v", days, err)
			}
			if got != days {
				t.Errorf("date round trip: %d -> %q -> %d", days, FormatDate(days), got)
			}
		}
	})
	t.Run("uuid", func(t *testing.T) {
		b := []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
			0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
		got, err := ParseUUID(FormatUUID(b))
		if err != nil {
			t.Fatalf("ParseUUID(FormatUUID): %v", err)
		}
		if !strings.EqualFold(FormatUUID(got), FormatUUID(b)) {
			t.Errorf("uuid round trip changed value: %x -> %x", b, got)
		}
	})
}
