package bytdb

// types.go: the date/time and UUID value representations. bytdb keeps
// its storage layer small by giving these types integer and byte
// runtime representations — the same kinds the tuple encoding already
// orders correctly — and defining the text syntax here, once, for the
// SQL layer and wire server to share:
//
//	TTimestamp  int64  microseconds since the Unix epoch, always UTC
//	TDate       int64  days since the Unix epoch
//	TUUID       []byte 16 bytes, ordered bytewise (RFC 4122 order)
//
// Microseconds match Postgres's own timestamp resolution, and int64
// micros cover the years 290,000 BCE..CE — no range gymnastics needed.

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"
)

// tsLayouts are the timestamp text forms accepted, most specific
// first: ISO date-time with optional fraction and zone (space or 'T'
// separated), then a bare date, which reads as its midnight UTC.
var tsLayouts = []string{
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999Z0700",
	"2006-01-02 15:04:05.999999999-07",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05.999999999Z0700",
	"2006-01-02T15:04:05.999999999-07",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02",
}

// ParseTimestamp reads Postgres-style timestamp text ('2024-01-02
// 03:04:05.123456+00', T-separated ISO 8601, or a bare date) to
// microseconds since the Unix epoch. Text without a zone is read as
// UTC — bytdb stores instants, not local wall clocks.
func ParseTimestamp(s string) (int64, error) {
	s = strings.TrimSpace(s)
	for _, layout := range tsLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMicro(), nil
		}
	}
	return 0, serr.New("invalid input syntax for type timestamp", "value", s)
}

// FormatTimestamp renders micros-since-epoch the way Postgres renders
// a UTC timestamptz — '2024-01-02 03:04:05.123456+00', fraction
// omitted when zero — which is the exact form client libraries parse.
func FormatTimestamp(micros int64) string {
	t := time.UnixMicro(micros).UTC()
	out := t.Format("2006-01-02 15:04:05")
	if frac := ((micros % 1e6) + 1e6) % 1e6; frac != 0 {
		out += strings.TrimRight(fmt.Sprintf(".%06d", frac), "0")
	}
	return out + "+00"
}

// ParseDate reads 'YYYY-MM-DD' to days since the Unix epoch.
func ParseDate(s string) (int64, error) {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(s))
	if err != nil {
		return 0, serr.New("invalid input syntax for type date", "value", s)
	}
	// Midnight UTC is exactly divisible, so this holds pre-1970 too.
	return t.Unix() / 86400, nil
}

// FormatDate renders days-since-epoch as 'YYYY-MM-DD'.
func FormatDate(days int64) string {
	return time.Unix(days*86400, 0).UTC().Format("2006-01-02")
}

// ParseUUID reads the canonical 8-4-4-4-12 form (case-insensitive;
// the undashed 32-hex form is also accepted) to its 16 bytes.
func ParseUUID(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	bad := func() ([]byte, error) {
		return nil, serr.New("invalid input syntax for type uuid", "value", s)
	}
	if len(s) == 36 {
		if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
			return bad()
		}
		s = s[:8] + s[9:13] + s[14:18] + s[19:23] + s[24:]
	}
	if len(s) != 32 {
		return bad()
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return bad()
	}
	return b, nil
}

// FormatUUID renders 16 bytes in the canonical lowercase dashed form.
// Anything but 16 bytes is a programming error upstream; it renders
// hex-escaped rather than panicking so a corrupt value stays visible.
func FormatUUID(b []byte) string {
	if len(b) != 16 {
		return fmt.Sprintf("\\x%x", b)
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
