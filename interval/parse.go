package interval

// parse.go: interval input, Postgres 16's interval_in.
//
// Postgres reads an interval in two passes, and so does Parse:
//
//	text ─► tokenize (ParseDateTime) ─► fields ─► decode (DecodeInterval) ─┬─► Interval
//	                                                                       │ bad format
//	                                                                       ▼
//	text ─────────────────────────────────────────► decodeISO8601 ─────────┴─► Interval
//
// The tokenizer splits the text into typed fields: numbers ("1",
// "1.5"), strings ("day", "ago"), times ("01:02:03"), "dates" ("1-2",
// the year-month form) and signed fields ("+1", "-2:00"). decode then
// reads the fields right to left, so a unit word is seen before the
// number it applies to:
//
//	'1 year 2 mons 3 days 04:05:06'
//	  ◄── read order ───────────────
//	  04:05:06  → clock time; the next bare number is days
//	  days      → unit for the number to its left
//	  3         → 3 days
//	  mons, 2   → 2 months
//	  year, 1   → 1 year
//
// A number with no unit takes the unit of the rightmost field the type
// modifier allows (seconds for a plain interval), and each field kind
// may appear once (a bitmask catches '1 day 2 days'). Fractions cascade
// downward: '1.5 mon' is 1 month 15 days, '0.5 day' is 12 hours.
// Anything that tokenizes but does not decode is retried as ISO 8601
// ('P1Y2M3DT4H5M6S', or the alternative 'P0001-02-03T04:05:06').
//
// The quirks are Postgres's and are kept: "ago" negates everything and
// may repeat, a clock field overwrites (rather than adds to) clock time
// that a fractional day put there ('01:00 1.5 days' is 1 day 01:00:00),
// and unit names are matched on their first 10 characters
// ('2 millisecondsxx' is 2 ms).

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// ErrSyntax and ErrFieldOverflow classify an *InputError (errors.Is).
var (
	ErrSyntax        = errors.New("invalid input syntax for type interval")
	ErrFieldOverflow = errors.New("interval field value out of range")
)

// InputError is a rejected interval text, worded as Postgres words it:
// `invalid input syntax for type interval: "x"` (22007) or
// `interval field value out of range: "x"` (22015).
type InputError struct {
	Kind  error // ErrSyntax or ErrFieldOverflow
	Input string
}

func (e *InputError) Error() string { return e.Kind.Error() + `: "` + e.Input + `"` }
func (e *InputError) Unwrap() error { return e.Kind }

// Parse reads interval text with no type modifier ('1 day 02:00',
// '-1 years +2 mons', '@ 3 hours ago', 'P1DT2H', '1-6' ...).
func Parse(s string) (Interval, error) { return ParseTypmod(s, NoTypmod) }

// ParseTypmod reads interval text for a column or cast with type
// modifier typmod (see Typmod). The modifier decides what a bare number
// means ('5' is 5 minutes for interval minute) and then truncates or
// rounds the result to the modifier's fields and precision.
func ParseTypmod(s string, typmod int32) (Interval, error) {
	rng := fullRange
	if typmod != NoTypmod {
		rng = typmodRange(typmod)
	}
	var itm itmIn
	err := errBadFormat
	if fields, ok := tokenize(s); ok {
		err = decode(fields, rng, &itm)
	}
	if err == errBadFormat {
		itm = itmIn{}
		err = decodeISO8601(s, &itm)
	}
	switch err {
	case nil:
	case errFieldOverflow:
		return Interval{}, &InputError{Kind: ErrFieldOverflow, Input: s}
	default:
		return Interval{}, &InputError{Kind: ErrSyntax, Input: s}
	}
	iv, ok := itm.interval()
	if !ok {
		return Interval{}, ErrOutOfRange
	}
	if typmod != NoTypmod {
		return Adjust(iv, typmod)
	}
	return iv, nil
}

// Internal decode outcomes (DTERR_BAD_FORMAT, DTERR_FIELD_OVERFLOW).
var (
	errBadFormat     = errors.New("bad format")
	errFieldOverflow = errors.New("field overflow")
)

// itmIn is pg_itm_in: the fields as decoding accumulates them, years
// apart from months so each can overflow on its own as in Postgres.
type itmIn struct {
	usec  int64
	mday  int32
	mon   int32
	years int32
}

// interval is itmin2interval: years fold into months.
func (t itmIn) interval() (Interval, bool) {
	total := int64(t.years)*MonthsPerYear + int64(t.mon)
	if total > math.MaxInt32 || total < math.MinInt32 {
		return Interval{}, false
	}
	return Interval{Months: int32(total), Days: t.mday, Micros: t.usec}, true
}

// --- tokenizer (ParseDateTime) ---

type fieldType uint8

const (
	ftNumber  fieldType = iota // digits, maybe with a fraction ("1", "1.5", ".5")
	ftString                   // a word ("day", "ago")
	ftDate                     // digits with '-' or '/' ("1-2"), or a word glued to digits
	ftTime                     // digits with ':' ("01:02:03.5")
	ftTZ                       // a sign and digits ("+1", "-2:00", "-1-2")
	ftSpecial                  // a sign and a word ("-infinity")
)

type field struct {
	s string
	t fieldType
}

// maxFields and maxFieldBytes are Postgres's MAXDATEFIELDS and its
// interval_in work buffer: input with more fields, or more field text,
// is bad format there too.
const (
	maxFields     = 25
	maxFieldBytes = 256
)

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isSpace(c byte) bool { return c == ' ' || (c >= '\t' && c <= '\r') }
func isPunct(c byte) bool { return c > ' ' && c < 0x7f && !isDigit(c) && !isAlpha(c) }
func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// tokenize splits s into fields as Postgres's ParseDateTime does. Words
// are lowercased. ok is false for text it rejects outright (a byte that
// is neither space, digit, letter nor ASCII punctuation; a sign followed
// by neither digit nor letter; too many fields).
func tokenize(s string) (fields []field, ok bool) {
	used := 0
	i := 0
	for i < len(s) {
		c := s[i]
		if isSpace(c) {
			i++
			continue
		}
		// The field limit is checked as each non-space character starts
		// a field, before punctuation is skipped, as ParseDateTime does.
		if len(fields) >= maxFields {
			return nil, false
		}
		var b []byte
		var t fieldType
		switch {
		case isDigit(c):
			for i < len(s) && isDigit(s[i]) {
				b = append(b, s[i])
				i++
			}
			switch {
			case i < len(s) && s[i] == ':':
				t = ftTime
				for i < len(s) && (isDigit(s[i]) || s[i] == ':' || s[i] == '.') {
					b = append(b, s[i])
					i++
				}
			case i < len(s) && (s[i] == '-' || s[i] == '/' || s[i] == '.'):
				delim := s[i]
				b = append(b, delim)
				i++
				if i < len(s) && isDigit(s[i]) {
					// "1.5" is a number; "1-2" and "1/2" are dates, and a
					// second matching delimiter makes any of them a date.
					t = ftDate
					if delim == '.' {
						t = ftNumber
					}
					for i < len(s) && isDigit(s[i]) {
						b = append(b, s[i])
						i++
					}
					if i < len(s) && s[i] == delim {
						t = ftDate
						for i < len(s) && (isDigit(s[i]) || s[i] == delim) {
							b = append(b, s[i])
							i++
						}
					}
				} else {
					t = ftDate
					for i < len(s) && (isDigit(s[i]) || isAlpha(s[i]) || s[i] == delim) {
						b = append(b, lower(s[i]))
						i++
					}
				}
			default:
				t = ftNumber
			}
		case c == '.':
			b = append(b, c)
			i++
			for i < len(s) && isDigit(s[i]) {
				b = append(b, s[i])
				i++
			}
			t = ftNumber
		case isAlpha(c):
			t = ftString
			for i < len(s) && isAlpha(s[i]) {
				b = append(b, lower(s[i]))
				i++
			}
			// A word glued to '-', '/' or '.', or to a digit or '+' when
			// the word is not a known keyword, is a date-like field
			// ("p1y2m" — which decode rejects, sending the text on to
			// the ISO 8601 reader).
			isDate := false
			if i < len(s) && (s[i] == '-' || s[i] == '/' || s[i] == '.') {
				isDate = true
			} else if i < len(s) && (s[i] == '+' || isDigit(s[i])) {
				isDate = !dateKeyword(string(b))
			}
			if isDate {
				t = ftDate
				for {
					b = append(b, lower(s[i]))
					i++
					if i >= len(s) {
						break
					}
					c := s[i]
					if !(c == '+' || c == '-' || c == '/' || c == '_' || c == '.' || c == ':' || isDigit(c) || isAlpha(c)) {
						break
					}
				}
			}
		case c == '+' || c == '-':
			b = append(b, c)
			i++
			for i < len(s) && isSpace(s[i]) {
				i++
			}
			switch {
			case i < len(s) && isDigit(s[i]):
				t = ftTZ
				b = append(b, s[i])
				i++
				for i < len(s) && (isDigit(s[i]) || s[i] == ':' || s[i] == '.' || s[i] == '-') {
					b = append(b, s[i])
					i++
				}
			case i < len(s) && isAlpha(s[i]):
				t = ftSpecial
				for i < len(s) && isAlpha(s[i]) {
					b = append(b, lower(s[i]))
					i++
				}
			default:
				return nil, false
			}
		case isPunct(c):
			i++ // other punctuation ('@', ',') only separates fields
			continue
		default:
			return nil, false
		}
		used += len(b) + 1 // each field and its terminator in the work buffer
		if used > maxFieldBytes {
			return nil, false
		}
		fields = append(fields, field{s: string(b), t: t})
	}
	return fields, true
}

// dateKeyword reports whether a word is in Postgres's core date token
// table (datetktbl): such a word followed by a digit stays a word
// ('ago5' is not a date-like field only if 'ago' were there; it is not).
// Only the membership matters, for the tokenizer's date-or-word choice.
func dateKeyword(w string) bool {
	if len(w) > tokMaxLen {
		w = w[:tokMaxLen]
	}
	return dateTokens[w]
}

// tokMaxLen is TOKMAXLEN: Postgres compares keywords on their first 10
// characters.
const tokMaxLen = 10

var dateTokens = map[string]bool{
	"-infinity": true, "ad": true, "allballs": true, "am": true, "apr": true,
	"april": true, "at": true, "aug": true, "august": true, "bc": true,
	"d": true, "dec": true, "december": true, "dow": true, "doy": true,
	"dst": true, "epoch": true, "feb": true, "february": true, "fri": true,
	"friday": true, "h": true, "infinity": true, "isodow": true,
	"isoyear": true, "j": true, "jan": true, "january": true, "jd": true,
	"jul": true, "julian": true, "july": true, "jun": true, "june": true,
	"m": true, "mar": true, "march": true, "may": true, "mm": true,
	"mon": true, "monday": true, "nov": true, "november": true, "now": true,
	"oct": true, "october": true, "on": true, "pm": true, "s": true,
	"sat": true, "saturday": true, "sep": true, "sept": true,
	"september": true, "sun": true, "sunday": true, "t": true, "thu": true,
	"thur": true, "thurs": true, "thursday": true, "today": true,
	"tomorrow": true, "tue": true, "tues": true, "tuesday": true,
	"wed": true, "wednesday": true, "weds": true, "y": true,
	"yesterday": true,
}

// --- decoder (DecodeInterval) ---

// Unit kinds a word can name, and the field bits that keep a kind from
// appearing twice (Postgres's DTK_* units and DTK_M masks).
type unit uint8

const (
	uNone unit = iota // no unit seen yet: a bare number takes the typmod's rightmost field
	uAgo              // after "ago": a bare number there is bad format
	uMicro
	uMilli
	uSecond
	uMinute
	uHour
	uDay
	uWeek
	uMonth
	uYear
	uDecade
	uCentury
	uMillennium
	uQuarter  // a unit for extract and date_trunc, not for interval input
	uTimezone // likewise ("timezone", "timezone_h", "timezone_m"): never valid for interval
)

// Field mask bits, Postgres's DTK_M(field) values.
const (
	mMonth       = 1 << 1
	mYear        = 1 << 2
	mDay         = 1 << 3
	mHour        = 1 << 10
	mMinute      = 1 << 11
	mSecond      = 1 << 12
	mMillisecond = 1 << 13
	mMicrosecond = 1 << 14
	mWeek        = 1 << 24
	mDecade      = 1 << 25
	mCentury     = 1 << 26
	mMillennium  = 1 << 27
	mAllSecs     = mSecond | mMillisecond | mMicrosecond
	mTime        = mHour | mMinute | mAllSecs
)

// unitWords is Postgres's deltatktbl, the words DecodeInterval knows.
// Keys are at most tokMaxLen long ("microsecon" matches "microseconds").
var unitWords = map[string]unit{
	"ago": uAgo,
	"c":   uCentury, "cent": uCentury, "centuries": uCentury, "century": uCentury,
	"d": uDay, "day": uDay, "days": uDay,
	"dec": uDecade, "decade": uDecade, "decades": uDecade, "decs": uDecade,
	"h": uHour, "hour": uHour, "hours": uHour, "hr": uHour, "hrs": uHour,
	"m": uMinute, "min": uMinute, "mins": uMinute, "minute": uMinute, "minutes": uMinute,
	"microsecon": uMicro, "us": uMicro, "usec": uMicro, "useconds": uMicro, "usecs": uMicro,
	"microsecond": uMicro, // truncated to "microsecon" before lookup; kept for clarity
	"mil":         uMillennium, "millennia": uMillennium, "millennium": uMillennium, "mils": uMillennium,
	"millisecon": uMilli, "ms": uMilli, "msec": uMilli, "mseconds": uMilli, "msecs": uMilli,
	"mon": uMonth, "mons": uMonth, "month": uMonth, "months": uMonth,
	"qtr": uQuarter, "quarter": uQuarter,
	"s": uSecond, "sec": uSecond, "second": uSecond, "seconds": uSecond, "secs": uSecond,
	"timezone": uTimezone, "timezone_h": uTimezone, "timezone_m": uTimezone,
	"w": uWeek, "week": uWeek, "weeks": uWeek,
	"y": uYear, "year": uYear, "years": uYear, "yr": uYear, "yrs": uYear,
}

func lookupUnit(w string) (unit, bool) {
	if len(w) > tokMaxLen {
		w = w[:tokMaxLen]
	}
	u, ok := unitWords[w]
	return u, ok
}

// decode is DecodeInterval for a non-SQL-standard IntervalStyle: fields
// right to left, each number scaled by the unit to its right.
func decode(fields []field, rng int32, itm *itmIn) error {
	isBefore := false
	fmask := 0
	typ := uNone
	for i := len(fields) - 1; i >= 0; i-- {
		f := fields[i]
		tmask := 0
		switch f.t {
		case ftTime:
			m, err := decodeTimeForInterval(f.s, rng, itm)
			if err != nil {
				return err
			}
			tmask = m
			typ = uDay // a bare number left of a clock is days: '3 04:05'
		case ftTZ, ftDate, ftNumber:
			if f.t == ftTZ && strings.IndexByte(f.s[1:], ':') >= 0 {
				// A signed clock ('-02:00'): as ftTime, then the sign.
				saved := itm.usec
				if m, err := decodeTimeForInterval(f.s[1:], rng, itm); err == nil {
					if f.s[0] == '-' {
						if itm.usec == math.MinInt64 {
							return errFieldOverflow
						}
						itm.usec = -itm.usec
					}
					tmask = m
					typ = uDay
					break
				}
				itm.usec = saved
			}
			if typ == uNone {
				typ = rangeUnit(rng)
			}
			m, err := decodeNumber(f.s, &typ, itm)
			if err != nil {
				return err
			}
			tmask = m
		case ftString, ftSpecial:
			// Postgres 16 keeps no state between unit words: a later
			// (leftward) word simply replaces an unused one ('1 hours
			// days' is one hour), and a word with no number is ignored
			// ('hours 1 day' is one day). Postgres 17 rejects both.
			u, ok := lookupUnit(f.s)
			if !ok {
				return errBadFormat
			}
			if u == uAgo {
				isBefore = true
				typ = uAgo
			} else {
				typ = u
			}
		default:
			return errBadFormat
		}
		if tmask&fmask != 0 {
			return errBadFormat
		}
		fmask |= tmask
	}
	if fmask == 0 {
		return errBadFormat // no number or clock at all: 'day', 'ago'
	}
	if isBefore {
		if itm.usec == math.MinInt64 || itm.mday == math.MinInt32 ||
			itm.mon == math.MinInt32 || itm.years == math.MinInt32 {
			return errFieldOverflow
		}
		itm.usec, itm.mday, itm.mon, itm.years = -itm.usec, -itm.mday, -itm.mon, -itm.years
	}
	return nil
}

// rangeUnit is the unit of a bare number when no unit word applies: the
// rightmost field the type modifier allows.
func rangeUnit(rng int32) unit {
	switch rng {
	case rangeYear:
		return uYear
	case rangeMonth, rangeYear | rangeMonth:
		return uMonth
	case rangeDay:
		return uDay
	case rangeHour, rangeDay | rangeHour:
		return uHour
	case rangeMinute, rangeHour | rangeMinute, rangeDay | rangeHour | rangeMinute:
		return uMinute
	}
	return uSecond
}

// decodeNumber applies one numeric field (with any fraction, or the
// "years-months" form) in unit *typ, and returns the field mask it
// claims. An hour moves *typ on to days for the next field ('1 2
// hours' is 1 day 2 hours).
func decodeNumber(s string, typ *unit, itm *itmIn) (int, error) {
	val, rest, err := strtoi64(s)
	if err != nil {
		return 0, err
	}
	var fval float64
	switch {
	case len(rest) > 0 && rest[0] == '-':
		// SQL "years-months": '1-2' is 1 year 2 months.
		v2, rest2, err := strtoi32(rest[1:])
		if err != nil || v2 < 0 || v2 >= MonthsPerYear {
			return 0, errFieldOverflow
		}
		if rest2 != "" {
			return 0, errBadFormat
		}
		*typ = uMonth
		if s[0] == '-' {
			v2 = -v2
		}
		m, ok := mul64ok(val, MonthsPerYear)
		if !ok {
			return 0, errFieldOverflow
		}
		if val, ok = add64(m, int64(v2)); !ok {
			return 0, errFieldOverflow
		}
	case len(rest) > 0 && rest[0] == '.':
		f, err := parseFraction(rest)
		if err != nil {
			return 0, err
		}
		fval = f
		if s[0] == '-' {
			fval = -fval
		}
	case rest == "":
	default:
		return 0, errBadFormat
	}
	ok := true
	var tmask int
	switch *typ {
	case uMicro:
		ok = adjustMicros(val, fval, 1, itm)
		tmask = mMicrosecond
	case uMilli:
		ok = adjustMicros(val, fval, 1000, itm)
		tmask = mMillisecond
	case uSecond:
		ok = adjustMicros(val, fval, MicrosPerSecond, itm)
		tmask = mSecond
		if fval != 0 {
			tmask = mAllSecs // a fractional second claims ms and µs too
		}
	case uMinute:
		ok = adjustMicros(val, fval, MicrosPerMinute, itm)
		tmask = mMinute
	case uHour:
		ok = adjustMicros(val, fval, MicrosPerHour, itm)
		tmask = mHour
		*typ = uDay
	case uDay:
		ok = adjustDays(val, 1, itm) && adjustFractMicros(fval, MicrosPerDay, itm)
		tmask = mDay
	case uWeek:
		ok = adjustDays(val, 7, itm) && adjustFractDays(fval, 7, itm)
		tmask = mWeek
	case uMonth:
		ok = adjustMonths(val, itm) && adjustFractDays(fval, DaysPerMonth, itm)
		tmask = mMonth
	case uYear:
		ok = adjustYears(val, 1, itm) && adjustFractYears(fval, 1, itm)
		tmask = mYear
	case uDecade:
		ok = adjustYears(val, 10, itm) && adjustFractYears(fval, 10, itm)
		tmask = mDecade
	case uCentury:
		ok = adjustYears(val, 100, itm) && adjustFractYears(fval, 100, itm)
		tmask = mCentury
	case uMillennium:
		ok = adjustYears(val, 1000, itm) && adjustFractYears(fval, 1000, itm)
		tmask = mMillennium
	default:
		return 0, errBadFormat // "ago" right after a number, or a word with no interval unit
	}
	if !ok {
		return 0, errFieldOverflow
	}
	return tmask, nil
}

// strtoi64 is strtoi64(s, &end, 10): an optional sign and leading
// digits; rest is what follows. No digits at all reads as 0 with s
// unconsumed, as C's strtol leaves it.
func strtoi64(s string) (int64, string, error) {
	j := 0
	if j < len(s) && (s[j] == '+' || s[j] == '-') {
		j++
	}
	k := j
	for k < len(s) && isDigit(s[k]) {
		k++
	}
	if k == j {
		return 0, s, nil
	}
	v, err := strconv.ParseInt(s[:k], 10, 64)
	if err != nil {
		return 0, "", errFieldOverflow
	}
	return v, s[k:], nil
}

// strtoi32 is strtoint: strtoi64 limited to int32.
func strtoi32(s string) (int32, string, error) {
	v, rest, err := strtoi64(s)
	if err != nil || v > math.MaxInt32 || v < math.MinInt32 {
		return 0, "", errFieldOverflow
	}
	return int32(v), rest, nil
}

// parseFraction is ParseFraction: s is "." then digits (or just ".");
// anything after the digits is bad format.
func parseFraction(s string) (float64, error) {
	if s == "." {
		return 0, nil
	}
	for i := 1; i < len(s); i++ {
		if !isDigit(s[i]) {
			return 0, errBadFormat
		}
	}
	f, err := strconv.ParseFloat("0"+s, 64)
	if err != nil {
		return 0, errBadFormat
	}
	return f, nil
}

// decodeTimeForInterval reads a clock field ('h:m', 'h:m:s[.f]',
// 'm:s.f') into itm.usec, replacing what was there (Postgres assigns,
// it does not add), and returns the clock's field mask.
func decodeTimeForInterval(s string, rng int32, itm *itmIn) (int, error) {
	hour, rest, err := strtoi64(s)
	if err != nil {
		return 0, err
	}
	if rest == "" || rest[0] != ':' {
		return 0, errBadFormat
	}
	min, rest, err := strtoi32(rest[1:])
	if err != nil {
		return 0, err
	}
	var sec int32
	var usec int64
	switch {
	case rest == "":
		// h:m — or m:s for an interval minute to second.
		if rng == rangeMinute|rangeSecond {
			if hour > math.MaxInt32 || hour < math.MinInt32 {
				return 0, errFieldOverflow
			}
			sec, min, hour = min, int32(hour), 0
		}
	case rest[0] == '.':
		// m:s.f is always minutes and seconds.
		if usec, err = parseFractionalSecond(rest); err != nil {
			return 0, err
		}
		if hour > math.MaxInt32 || hour < math.MinInt32 {
			return 0, errFieldOverflow
		}
		sec, min, hour = min, int32(hour), 0
	case rest[0] == ':':
		if sec, rest, err = strtoi32(rest[1:]); err != nil {
			return 0, err
		}
		switch {
		case rest == "":
		case rest[0] == '.':
			if usec, err = parseFractionalSecond(rest); err != nil {
				return 0, err
			}
		default:
			return 0, errBadFormat
		}
	default:
		return 0, errBadFormat
	}
	if hour < 0 || min < 0 || min > 59 || sec < 0 || sec > 60 || usec < 0 || usec > MicrosPerSecond {
		return 0, errFieldOverflow
	}
	total := usec
	for _, p := range [][2]int64{{hour, MicrosPerHour}, {int64(min), MicrosPerMinute}, {int64(sec), MicrosPerSecond}} {
		m, ok := mul64ok(p[0], p[1])
		if !ok {
			return 0, errFieldOverflow
		}
		if total, ok = add64(total, m); !ok {
			return 0, errFieldOverflow
		}
	}
	itm.usec = total
	return mTime, nil
}

// parseFractionalSecond is ParseFractionalSecond: the fraction rounded
// to microseconds (half to even, C's rint).
func parseFractionalSecond(s string) (int64, error) {
	f, err := parseFraction(s)
	if err != nil {
		return 0, err
	}
	return int64(math.RoundToEven(float64(f * 1e6))), nil
}

// --- the Adjust* helpers: add a scaled value to one itmIn field ---

func adjustFractMicros(frac float64, scale int64, itm *itmIn) bool {
	if frac == 0 {
		return true
	}
	frac = float64(frac * float64(scale))
	us := int64(frac)
	frac -= float64(us)
	if frac > 0.5 {
		us++
	} else if frac < -0.5 {
		us--
	}
	var ok bool
	itm.usec, ok = add64(itm.usec, us)
	return ok
}

func adjustFractDays(frac float64, scale int64, itm *itmIn) bool {
	if frac == 0 {
		return true
	}
	frac = float64(frac * float64(scale))
	extra := int32(frac)
	d, ok := add32(itm.mday, extra)
	if !ok {
		return false
	}
	itm.mday = d
	frac -= float64(extra)
	return adjustFractMicros(frac, MicrosPerDay, itm)
}

func adjustFractYears(frac float64, scale int64, itm *itmIn) bool {
	extra := int32(math.RoundToEven(float64(frac * float64(scale) * MonthsPerYear)))
	m, ok := add32(itm.mon, extra)
	itm.mon = m
	return ok
}

func adjustMicros(val int64, fval float64, scale int64, itm *itmIn) bool {
	v, ok := mul64ok(val, scale)
	if !ok {
		return false
	}
	if itm.usec, ok = add64(itm.usec, v); !ok {
		return false
	}
	return adjustFractMicros(fval, scale, itm)
}

func adjustDays(val int64, scale int32, itm *itmIn) bool {
	if val < math.MinInt32 || val > math.MaxInt32 {
		return false
	}
	p := int64(val) * int64(scale)
	if p < math.MinInt32 || p > math.MaxInt32 {
		return false
	}
	d, ok := add32(itm.mday, int32(p))
	itm.mday = d
	return ok
}

func adjustMonths(val int64, itm *itmIn) bool {
	if val < math.MinInt32 || val > math.MaxInt32 {
		return false
	}
	m, ok := add32(itm.mon, int32(val))
	itm.mon = m
	return ok
}

func adjustYears(val int64, scale int32, itm *itmIn) bool {
	if val < math.MinInt32 || val > math.MaxInt32 {
		return false
	}
	p := val * int64(scale)
	if p < math.MinInt32 || p > math.MaxInt32 {
		return false
	}
	y, ok := add32(itm.years, int32(p))
	itm.years = y
	return ok
}

// --- ISO 8601 (DecodeISO8601Interval) ---

// decodeISO8601 reads 'P[nY][nM][nW][nD][T[nH][nM][nS]]' with optional
// fractions, and the alternative forms 'PYYYY-MM-DDThh:mm:ss' and
// 'PYYYYMMDDThhmmss'. The text must start with an uppercase P; units
// are uppercase too.
func decodeISO8601(s string, itm *itmIn) error {
	if len(s) < 2 || s[0] != 'P' {
		return errBadFormat
	}
	s = s[1:]
	datePart, haveField := true, false
	for len(s) > 0 {
		if s[0] == 'T' {
			datePart, haveField = false, false
			s = s[1:]
			continue
		}
		fieldStart := s
		val, fval, rest, err := parseISO8601Number(s)
		if err != nil {
			return err
		}
		s = rest
		var u byte // the unit letter, 0 at the end of the text
		if len(s) > 0 {
			u, s = s[0], s[1:]
		}
		if datePart {
			switch u {
			case 'Y':
				if !adjustYears(val, 1, itm) || !adjustFractYears(fval, 1, itm) {
					return errFieldOverflow
				}
			case 'M':
				if !adjustMonths(val, itm) || !adjustFractDays(fval, DaysPerMonth, itm) {
					return errFieldOverflow
				}
			case 'W':
				if !adjustDays(val, 7, itm) || !adjustFractDays(fval, 7, itm) {
					return errFieldOverflow
				}
			case 'D':
				if !adjustDays(val, 1, itm) || !adjustFractMicros(fval, MicrosPerDay, itm) {
					return errFieldOverflow
				}
			case 'T', 0, '-':
				if u != '-' && isoIntegerWidth(fieldStart) == 8 && !haveField {
					// Basic alternative format: YYYYMMDD.
					if !adjustYears(val/10000, 1, itm) || !adjustMonths((val/100)%100, itm) ||
						!adjustDays(val%100, 1, itm) || !adjustFractMicros(fval, MicrosPerDay, itm) {
						return errFieldOverflow
					}
					if u == 0 {
						return nil
					}
					datePart, haveField = false, false
					continue
				}
				// Extended alternative format: YYYY-MM-DD.
				if haveField {
					return errBadFormat
				}
				if !adjustYears(val, 1, itm) || !adjustFractYears(fval, 1, itm) {
					return errFieldOverflow
				}
				if u == 0 {
					return nil
				}
				if u == 'T' {
					datePart, haveField = false, false
					continue
				}
				if val, fval, s, err = parseISO8601Number(s); err != nil {
					return err
				}
				if !adjustMonths(val, itm) || !adjustFractDays(fval, DaysPerMonth, itm) {
					return errFieldOverflow
				}
				if s == "" {
					return nil
				}
				if s[0] == 'T' {
					datePart, haveField = false, false
					s = s[1:]
					continue
				}
				if s[0] != '-' {
					return errBadFormat
				}
				if val, fval, s, err = parseISO8601Number(s[1:]); err != nil {
					return err
				}
				if !adjustDays(val, 1, itm) || !adjustFractMicros(fval, MicrosPerDay, itm) {
					return errFieldOverflow
				}
				if s == "" {
					return nil
				}
				if s[0] == 'T' {
					datePart, haveField = false, false
					s = s[1:]
					continue
				}
				return errBadFormat
			default:
				return errBadFormat
			}
		} else {
			switch u {
			case 'H':
				if !adjustMicros(val, fval, MicrosPerHour, itm) {
					return errFieldOverflow
				}
			case 'M':
				if !adjustMicros(val, fval, MicrosPerMinute, itm) {
					return errFieldOverflow
				}
			case 'S':
				if !adjustMicros(val, fval, MicrosPerSecond, itm) {
					return errFieldOverflow
				}
			case 0, ':':
				if u == 0 && isoIntegerWidth(fieldStart) == 6 && !haveField {
					// Basic alternative format: hhmmss.
					if !adjustMicros(val/10000, 0, MicrosPerHour, itm) ||
						!adjustMicros((val/100)%100, 0, MicrosPerMinute, itm) ||
						!adjustMicros(val%100, 0, MicrosPerSecond, itm) ||
						!adjustFractMicros(fval, 1, itm) {
						return errFieldOverflow
					}
					return nil
				}
				// Extended alternative format: hh:mm:ss.
				if haveField {
					return errBadFormat
				}
				if !adjustMicros(val, fval, MicrosPerHour, itm) {
					return errFieldOverflow
				}
				if u == 0 {
					return nil
				}
				if val, fval, s, err = parseISO8601Number(s); err != nil {
					return err
				}
				if !adjustMicros(val, fval, MicrosPerMinute, itm) {
					return errFieldOverflow
				}
				if s == "" {
					return nil
				}
				if s[0] != ':' {
					return errBadFormat
				}
				if val, fval, s, err = parseISO8601Number(s[1:]); err != nil {
					return err
				}
				if !adjustMicros(val, fval, MicrosPerSecond, itm) {
					return errFieldOverflow
				}
				if s == "" {
					return nil
				}
				return errBadFormat
			default:
				return errBadFormat
			}
		}
		haveField = true
	}
	return nil
}

// parseISO8601Number is ParseISO8601Number: a strtod-style number
// (digits, fraction, exponent; '-' or '.' may lead, '+' may not) split
// into its integer part, truncated toward zero, and the fraction. |v|
// beyond 1e15 is a field overflow, so the integer part is exact.
// C's strtod also reads hex floats and inf/nan words; the words are
// handled (and rejected, as Postgres rejects them), hex is not read.
func parseISO8601Number(s string) (ipart int64, fpart float64, rest string, err error) {
	if s == "" || !(isDigit(s[0]) || s[0] == '-' || s[0] == '.') {
		return 0, 0, s, errBadFormat
	}
	n := strtodPrefix(s)
	if n == 0 {
		return 0, 0, s, errBadFormat
	}
	v, perr := strconv.ParseFloat(s[:n], 64)
	if perr != nil && !errors.Is(perr, strconv.ErrRange) {
		return 0, 0, s, errBadFormat
	}
	if perr != nil || math.IsNaN(v) || v < -1e15 || v > 1e15 {
		return 0, 0, s, errFieldOverflow
	}
	if v >= 0 {
		ipart = int64(math.Floor(v))
	} else {
		ipart = -int64(math.Floor(-v))
	}
	return ipart, v - float64(ipart), s[n:], nil
}

// strtodPrefix is the length of the longest prefix of s that C's strtod
// would read as a decimal number: [sign] (digits [. digits] | . digits)
// [e [sign] digits], or [sign] inf / infinity / nan.
func strtodPrefix(s string) int {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	lw := strings.ToLower(s[i:])
	for _, w := range []string{"infinity", "inf", "nan"} {
		if strings.HasPrefix(lw, w) {
			return i + len(w)
		}
	}
	digits := 0
	for i < len(s) && isDigit(s[i]) {
		i++
		digits++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && isDigit(s[i]) {
			i++
			digits++
		}
	}
	if digits == 0 {
		return 0
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < len(s) && isDigit(s[j]) {
			for j < len(s) && isDigit(s[j]) {
				j++
			}
			i = j
		}
	}
	return i
}

// isoIntegerWidth is ISO8601IntegerWidth: the count of leading digits,
// after an optional '-'.
func isoIntegerWidth(s string) int {
	if s != "" && s[0] == '-' {
		s = s[1:]
	}
	n := 0
	for n < len(s) && isDigit(s[n]) {
		n++
	}
	return n
}
