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

// --- text arrays ---
//
// TTextArray rides on a string runtime representation: the canonical
// Postgres one-dimensional array literal ('{a,"b c",NULL}'), which is
// also the exact text lib/pq and pgx exchange for text[]. Storing the
// wire text itself means the pgwire layer round-trips arrays for free,
// and the tuple encoding needs no new element kind. The cost is that
// value equality is text equality — which is why every write path
// canonicalizes through CanonTextArray, so '{a, b}' and '{a,b}' store
// (and therefore compare) identically.
//
// Elements are string or nil (a SQL NULL element). Only one dimension
// exists; '{{a},{b}}' is rejected rather than silently flattened.

// ParseTextArray reads a Postgres one-dimensional array literal into
// its elements. Quoted elements ("a \"b\"") unescape backslash
// sequences; unquoted elements trim surrounding whitespace, may not be
// empty, and read as NULL when they spell null (any case), exactly the
// rules Postgres's array_in applies.
func ParseTextArray(s string) ([]any, error) {
	malformed := func() error {
		return serr.New(`malformed array literal: "` + s + `"`)
	}
	t := strings.TrimSpace(s)
	if len(t) < 2 || t[0] != '{' || t[len(t)-1] != '}' {
		return nil, malformed()
	}
	body := t[1 : len(t)-1]
	if strings.TrimSpace(body) == "" {
		return []any{}, nil
	}
	var elems []any
	i := 0
	for {
		// Each loop iteration consumes one element and the comma after
		// it. Whitespace around elements is decoration, not content.
		for i < len(body) && (body[i] == ' ' || body[i] == '\t') {
			i++
		}
		if i < len(body) && body[i] == '{' {
			return nil, serr.New("multidimensional arrays are not supported", "value", s)
		}
		if i < len(body) && body[i] == '"' {
			// Quoted element: content runs to the closing quote, with
			// backslash escaping the next byte (covers \" and \\).
			var b strings.Builder
			i++
			closed := false
			for i < len(body) {
				c := body[i]
				if c == '\\' && i+1 < len(body) {
					b.WriteByte(body[i+1])
					i += 2
					continue
				}
				if c == '"' {
					closed = true
					i++
					break
				}
				b.WriteByte(c)
				i++
			}
			if !closed {
				return nil, malformed()
			}
			elems = append(elems, b.String())
		} else {
			// Unquoted element: runs to the next comma. Braces and
			// quotes inside are structural characters gone wrong.
			start := i
			for i < len(body) && body[i] != ',' {
				if body[i] == '{' || body[i] == '}' || body[i] == '"' {
					return nil, malformed()
				}
				i++
			}
			tok := strings.TrimSpace(body[start:i])
			if tok == "" {
				return nil, malformed()
			}
			if strings.EqualFold(tok, "null") {
				elems = append(elems, nil)
			} else {
				elems = append(elems, tok)
			}
		}
		// After an element: optional whitespace, then a comma (another
		// element follows) or the end of the body.
		for i < len(body) && (body[i] == ' ' || body[i] == '\t') {
			i++
		}
		if i >= len(body) {
			return elems, nil
		}
		if body[i] != ',' {
			return nil, malformed()
		}
		i++
	}
}

// FormatTextArray renders elements (string or nil) as the canonical
// array literal. Quoting matches Postgres's array_out: an element is
// quoted when leaving it bare would be ambiguous — it is empty, spells
// NULL, or contains a structural character ({ } , " \) or whitespace —
// and inside quotes, backslash escapes itself and the double quote.
func FormatTextArray(elems []any) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, e := range elems {
		if i > 0 {
			b.WriteByte(',')
		}
		if e == nil {
			b.WriteString("NULL")
			continue
		}
		s := fmt.Sprint(e) // string in practice; Sprint keeps corrupt values visible
		if !textArrayNeedsQuotes(s) {
			b.WriteString(s)
			continue
		}
		b.WriteByte('"')
		for j := 0; j < len(s); j++ {
			if s[j] == '"' || s[j] == '\\' {
				b.WriteByte('\\')
			}
			b.WriteByte(s[j])
		}
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func textArrayNeedsQuotes(s string) bool {
	if s == "" || strings.EqualFold(s, "null") {
		return true
	}
	return strings.ContainsAny(s, "{},\"\\ \t\n\r")
}

// CanonTextArray parses and re-renders an array literal, producing the
// one spelling every write path stores. Canonical text is what lets
// '=' on text[] columns behave as element-wise equality despite the
// underlying comparison being plain string comparison.
func CanonTextArray(s string) (string, error) {
	elems, err := ParseTextArray(s)
	if err != nil {
		return "", err
	}
	return FormatTextArray(elems), nil
}
