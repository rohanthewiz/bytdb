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
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
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
	// Without seconds, as Postgres also accepts ('2024-02-29 12:00').
	"2006-01-02 15:04Z07:00",
	"2006-01-02 15:04-07",
	"2006-01-02 15:04",
	"2006-01-02T15:04Z07:00",
	"2006-01-02T15:04",
	"2006-01-02",
	// Unpadded month and day ('2024-2-9'), and the compact ISO date.
	"2006-1-2 15:04:05.999999999",
	"2006-1-2",
	"20060102",
}

// Timestamp sentinels for Postgres's special values. 'infinity' and
// '-infinity' are the extreme instants, so they order after and before
// every finite timestamp exactly as Postgres orders them, and render
// back as the words; the wire's binary format uses the same two int64
// values for them.
const (
	TimestampInfinity    = math.MaxInt64
	TimestampNegInfinity = math.MinInt64
)

// specialTime reads Postgres's special date/time input words, relative
// to now. ok is false for anything else.
func specialTime(s string, now time.Time) (micros int64, ok bool) {
	day := func(offset int) int64 {
		u := now.UTC()
		return time.Date(u.Year(), u.Month(), u.Day()+offset, 0, 0, 0, 0, time.UTC).UnixMicro()
	}
	switch strings.ToLower(s) {
	case "epoch":
		return 0, true
	case "infinity", "+infinity":
		return TimestampInfinity, true
	case "-infinity":
		return TimestampNegInfinity, true
	case "now":
		return now.UnixMicro(), true
	case "today":
		return day(0), true
	case "tomorrow":
		return day(1), true
	case "yesterday":
		return day(-1), true
	}
	return 0, false
}

// splitZoneName splits a trailing zone name off a timestamp: ' UTC',
// ' GMT', ' Z', or an IANA name (' America/New_York'). loc is nil when
// there is none, or the trailing word is not a zone.
func splitZoneName(s string) (rest string, loc *time.Location) {
	i := strings.LastIndexByte(s, ' ')
	if i < 0 {
		return s, nil
	}
	name := s[i+1:]
	switch strings.ToUpper(name) {
	case "UTC", "GMT", "Z", "ZULU":
		return strings.TrimSpace(s[:i]), time.UTC
	}
	if strings.Contains(name, "/") {
		if l, err := time.LoadLocation(name); err == nil {
			return strings.TrimSpace(s[:i]), l
		}
	}
	return s, nil
}

// ParseTimestamp reads Postgres-style timestamp text ('2024-01-02
// 03:04:05.123456+00', T-separated ISO 8601, or a bare date) to
// microseconds since the Unix epoch. Text without a zone is read as
// UTC — bytdb stores instants, not local wall clocks.
//
// Beyond those (deep-look D-55): the time without seconds, unpadded
// dates, a trailing zone name (UTC, GMT, Z, or an IANA name such as
// America/New_York, which places a zone-less time in that zone), and the
// special words epoch, infinity, -infinity, now, today, tomorrow and
// yesterday.
func ParseTimestamp(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if v, ok := specialTime(s, time.Now()); ok {
		return v, nil
	}
	in := time.UTC
	if rest, loc := splitZoneName(s); loc != nil {
		s, in = rest, loc
	}
	for _, layout := range tsLayouts {
		if t, err := time.ParseInLocation(layout, s, in); err == nil {
			return t.UnixMicro(), nil
		}
	}
	return 0, serr.New("invalid input syntax for type timestamp", "value", s)
}

// ParseBool reads Postgres's boolean input forms, case-insensitively and
// ignoring surrounding space: true/false, yes/no, on/off, 1/0, and any
// unique prefix of true, false, yes or no ('t', 'fa', 'y'), which is
// what Postgres's boolin accepts. strconv.ParseBool, used before,
// refused yes/no/on/off (deep-look D-55).
func ParseBool(s string) (bool, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	switch t {
	case "1", "on":
		return true, nil
	case "0", "off":
		return false, nil
	}
	if t != "" {
		for _, w := range []struct {
			word string
			val  bool
		}{{"true", true}, {"false", false}, {"yes", true}, {"no", false}} {
			if strings.HasPrefix(w.word, t) {
				return w.val, nil
			}
		}
	}
	return false, serr.New("invalid input syntax for type boolean", "value", s)
}

// FormatFloat renders a float64 the way Postgres's float8 output does
// (extra_float_digits = 1, its default since Postgres 12): the shortest
// digits that round-trip, positional for decimal exponents from -4 to
// 14 and scientific outside that ("1e+15", "1e-05"), and NaN / Infinity
// / -Infinity spelled out. Go's %g switches to scientific after six
// digits, so 1000000 printed as "1e+06" where Postgres prints 1000000,
// on the wire and in ::text (deep-look D-24, D-55).
func FormatFloat(f float64) string {
	return string(AppendFloat(nil, f))
}

// AppendFloat appends FormatFloat's rendering of f to dst. The Append
// forms of the formatters let the wire server encode a result row into
// one reused buffer instead of allocating a string per value (deep-look
// D-41); the Format forms wrap them.
func AppendFloat(dst []byte, f float64) []byte {
	switch {
	case math.IsNaN(f):
		return append(dst, "NaN"...)
	case math.IsInf(f, 1):
		return append(dst, "Infinity"...)
	case math.IsInf(f, -1):
		return append(dst, "-Infinity"...)
	}
	start := len(dst)
	dst = strconv.AppendFloat(dst, f, 'e', -1, 64) // shortest digits, e.g. "1.25e+08"
	if f == 0 {
		return bytes.TrimSuffix(dst, []byte("e+00")) // "0" or "-0", as Postgres prints
	}
	// The exponent: everything after the 'e', a sign and two or three
	// digits.
	e, neg := 0, false
	for i := len(dst) - 1; i > start; i-- {
		if dst[i] == 'e' {
			neg = dst[i+1] == '-'
			for _, c := range dst[i+2:] {
				e = e*10 + int(c-'0')
			}
			break
		}
	}
	if neg {
		e = -e
	}
	if e < -4 || e >= 15 {
		return dst // Go pads the exponent to two digits, as Postgres does
	}
	return strconv.AppendFloat(dst[:start], f, 'f', -1, 64)
}

// FormatTimestamp renders micros-since-epoch the way Postgres renders
// a UTC timestamptz — '2024-01-02 03:04:05.123456+00', fraction
// omitted when zero — which is the exact form client libraries parse.
func FormatTimestamp(micros int64) string {
	return string(AppendTimestamp(nil, micros))
}

// AppendTimestamp appends FormatTimestamp's rendering to dst.
func AppendTimestamp(dst []byte, micros int64) []byte {
	switch micros {
	case TimestampInfinity:
		return append(dst, "infinity"...)
	case TimestampNegInfinity:
		return append(dst, "-infinity"...)
	}
	dst = time.UnixMicro(micros).UTC().AppendFormat(dst, "2006-01-02 15:04:05")
	if frac := ((micros % 1e6) + 1e6) % 1e6; frac != 0 {
		// ".%06d" with its trailing zeros trimmed: .5 for 500000µs.
		var digits [7]byte
		digits[0] = '.'
		for i := 6; i >= 1; i-- {
			digits[i] = byte('0' + frac%10)
			frac /= 10
		}
		n := 7
		for digits[n-1] == '0' {
			n--
		}
		dst = append(dst, digits[:n]...)
	}
	return append(dst, "+00"...)
}

// ParseDate reads 'YYYY-MM-DD' to days since the Unix epoch. Any
// timestamp form ParseTimestamp accepts is a date too, as in Postgres
// ('2024-01-02 23:30:00-05'::date is 2024-01-02): the date is the one
// written, and the time and zone are checked for syntax, then dropped —
// never applied, which could move the date across midnight. This is what
// lets the database/sql driver bind a time.Time to a date column: it
// sends every time.Time as '2006-01-02 15:04:05…' text in UTC, which
// lands on the UTC day, the same day the Go API truncates a time.Time to.
func ParseDate(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if v, ok := specialTime(s, time.Now()); ok {
		switch v {
		case TimestampInfinity:
			return DateInfinity, nil
		case TimestampNegInfinity:
			return DateNegInfinity, nil
		}
		return floorDiv(v, 86400e6), nil
	}
	// The bare date first: by far the common input, and otherwise it
	// would be tried last, after every timestamp layout had failed.
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		parsed := false
		for _, layout := range tsLayouts {
			if t, err = time.Parse(layout, s); err == nil {
				parsed = true
				break
			}
		}
		if !parsed {
			return 0, serr.New("invalid input syntax for type date", "value", s)
		}
	}
	// t.Date() reads the fields in t's own (parsed, fixed-offset)
	// location: the date as written, not the UTC date of that instant.
	// Rebuilding midnight UTC from them keeps the division exact, so
	// pre-1970 dates come out right too.
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400, nil
}

// Date sentinels for 'infinity' and '-infinity', as for timestamps;
// the wire's binary date format uses int32's extremes for them.
const (
	DateInfinity    = math.MaxInt32
	DateNegInfinity = math.MinInt32
)

// floorDiv is integer division rounding toward negative infinity.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// FormatDate renders days-since-epoch as 'YYYY-MM-DD'.
func FormatDate(days int64) string {
	return string(AppendDate(nil, days))
}

// AppendDate appends FormatDate's rendering to dst.
func AppendDate(dst []byte, days int64) []byte {
	switch days {
	case DateInfinity:
		return append(dst, "infinity"...)
	case DateNegInfinity:
		return append(dst, "-infinity"...)
	}
	return time.Unix(days*86400, 0).UTC().AppendFormat(dst, "2006-01-02")
}

// ParseUUID reads the canonical 8-4-4-4-12 form (case-insensitive;
// the undashed 32-hex form is also accepted) to its 16 bytes.
func ParseUUID(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	// Postgres also accepts the braced form, '{a0eebc99-...}'.
	if len(s) >= 2 && s[0] == '{' && s[len(s)-1] == '}' {
		s = s[1 : len(s)-1]
	}
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
	return string(AppendUUID(nil, b))
}

// AppendUUID appends FormatUUID's rendering to dst.
func AppendUUID(dst []byte, b []byte) []byte {
	if len(b) != 16 {
		return hex.AppendEncode(append(dst, `\x`...), b)
	}
	dst = hex.AppendEncode(dst, b[0:4])
	dst = hex.AppendEncode(append(dst, '-'), b[4:6])
	dst = hex.AppendEncode(append(dst, '-'), b[6:8])
	dst = hex.AppendEncode(append(dst, '-'), b[8:10])
	return hex.AppendEncode(append(dst, '-'), b[10:16])
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

// arraySpace is the whitespace alphabet of array literals — the six
// ASCII bytes Postgres's array_isspace accepts, used for every trim and
// skip in this codec AND for the formatter's quoting decision. The two
// sides sharing one definition is a correctness invariant, not a style
// choice: any byte the parser trims but the formatter leaves unquoted
// is silent corruption (canon("{\f0}") would drop the \f on re-parse).
// Deliberately narrower than unicode.IsSpace — a non-breaking space or
// other Unicode space is element content, exactly as in Postgres.
const arraySpace = " \t\n\r\v\f"

func isArraySpace(c byte) bool { return strings.IndexByte(arraySpace, c) >= 0 }

// ParseTextArray reads a Postgres one-dimensional array literal into
// its elements. Quoted elements ("a \"b\"") unescape backslash
// sequences; unquoted elements trim surrounding whitespace, may not be
// empty, and read as NULL when they spell null (any case), exactly the
// rules Postgres's array_in applies.
func ParseTextArray(s string) ([]any, error) {
	malformed := func() error {
		return serr.New(`malformed array literal: "` + s + `"`)
	}
	t := strings.Trim(s, arraySpace)
	if len(t) < 2 || t[0] != '{' || t[len(t)-1] != '}' {
		return nil, malformed()
	}
	body := t[1 : len(t)-1]
	if strings.Trim(body, arraySpace) == "" {
		return []any{}, nil
	}
	var elems []any
	i := 0
	for {
		// Each loop iteration consumes one element and the comma after
		// it. Whitespace around elements is decoration, not content.
		for i < len(body) && isArraySpace(body[i]) {
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
			tok := strings.Trim(body[start:i], arraySpace)
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
		for i < len(body) && isArraySpace(body[i]) {
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
	return strings.ContainsAny(s, `{},"\`+arraySpace)
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

// --- jsonb ---
//
// TJSONB follows the TTextArray playbook: the runtime value is a
// string, the canonical rendering of the JSON document, and every
// write path canonicalizes through CanonJSONB so plain string
// comparison implements jsonb document equality — whitespace and
// object key order vanish, the last duplicate key wins.
//
// Canonical form is Go's encoding/json compact rendering with object
// keys sorted lexically. Postgres renders jsonb with spaces and sorts
// keys length-first, so the two systems' texts differ cosmetically —
// which no JSON-parsing client can observe; what matters is that this
// engine has exactly one spelling per document.

// CanonJSONB validates JSON text and re-renders it canonically.
// Numbers pass through as their source text (json.Number), not
// float64 — 20-digit integers and deliberate decimals survive
// untouched, the closest text-representation analog of jsonb's
// arbitrary-precision numerics.
func CanonJSONB(s string) (string, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", serr.New("invalid input syntax for type json", "value", s)
	}
	// One document only: anything after it (beyond whitespace, which
	// Token skips) is malformed, same as Postgres's jsonb_in.
	if _, err := dec.Token(); err != io.EOF {
		return "", serr.New("invalid input syntax for type json", "value", s)
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	// Postgres does not HTML-escape <, > and & inside jsonb text, and
	// neither should we — the stored form is data, not markup.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", serr.Wrap(err, "op", "canonicalize jsonb")
	}
	// Encode terminates the stream with a newline; the canonical value
	// must not carry it (it would leak into '=' comparisons).
	return strings.TrimSuffix(buf.String(), "\n"), nil
}
