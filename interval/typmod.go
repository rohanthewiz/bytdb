package interval

// typmod.go: interval type modifiers — the field qualifier and
// fractional-second precision of `interval day to second(3)`.
//
// A typmod packs both, exactly as Postgres's INTERVAL_TYPMOD does, so
// the value bytdb stores is the one pg_attribute.atttypmod reports:
//
//	bits 31..16  range      the allowed fields, a mask of Postgres's
//	                        field bits (YEAR 1<<2, MONTH 1<<1, DAY 1<<3,
//	                        HOUR 1<<10, MINUTE 1<<11, SECOND 1<<12);
//	                        0x7FFF for "all fields"
//	bits 15..0   precision  0..6 fractional-second digits; 0xFFFF for
//	                        "unspecified"
//
//	interval                    -1 (NoTypmod)
//	interval(3)                 0x7FFF<<16 | 3          = 2147418115
//	interval day to second(3)   (DAY|HOUR|MINUTE|SECOND)<<16 | 3 = 470286339
//	interval year               YEAR<<16 | 0xFFFF       = 327679
//
// A valid typmod is never 0 (a range of 0 names no field), which is what
// lets bytdb's column descriptor use 0 for "no modifier".
//
// The modifier does two things (AdjustIntervalForTypmod): fields to the
// right of the last allowed one are zeroed — interval day drops the
// clock, interval hour drops minutes and seconds — and the clock is
// rounded to the precision. Fields to the left are kept: interval day
// does not reject '1 mon 2 days'. It also decides what a bare number
// means on input ('5' is 5 minutes for interval minute).

import (
	"fmt"
	"strings"
)

// NoTypmod is the typmod of a plain interval: all fields, full precision.
const NoTypmod int32 = -1

// Field range bits (INTERVAL_MASK of Postgres's field numbers).
const (
	rangeMonth  int32 = 1 << 1
	rangeYear   int32 = 1 << 2
	rangeDay    int32 = 1 << 3
	rangeHour   int32 = 1 << 10
	rangeMinute int32 = 1 << 11
	rangeSecond int32 = 1 << 12
	fullRange   int32 = 0x7FFF
	fullPrec    int32 = 0xFFFF
	// MaxPrecision is the most fractional-second digits a modifier can
	// keep (microseconds).
	MaxPrecision = 6
)

func typmodRange(t int32) int32 { return (t >> 16) & 0x7FFF }
func typmodPrec(t int32) int32  { return t & 0xFFFF }

// qualifiers maps each field qualifier, as SQL spells it, to its range.
var qualifiers = []struct {
	name string
	rng  int32
}{
	{"year", rangeYear},
	{"month", rangeMonth},
	{"day", rangeDay},
	{"hour", rangeHour},
	{"minute", rangeMinute},
	{"second", rangeSecond},
	{"year to month", rangeYear | rangeMonth},
	{"day to hour", rangeDay | rangeHour},
	{"day to minute", rangeDay | rangeHour | rangeMinute},
	{"day to second", rangeDay | rangeHour | rangeMinute | rangeSecond},
	{"hour to minute", rangeHour | rangeMinute},
	{"hour to second", rangeHour | rangeMinute | rangeSecond},
	{"minute to second", rangeMinute | rangeSecond},
}

// Typmod builds a type modifier from a field qualifier ("" for all
// fields, else one of "year", "day to second", ... in lowercase with
// single spaces) and a precision (-1 for unspecified). A precision above
// 6 is reduced to 6, as Postgres does (it warns); a negative one other
// than -1 is an error. A precision is only allowed with all fields or a
// qualifier ending in SECOND, which the SQL grammar enforces by where
// it accepts one.
func Typmod(qualifier string, precision int) (int32, error) {
	rng := fullRange
	if qualifier != "" {
		found := false
		for _, q := range qualifiers {
			if q.name == qualifier {
				rng, found = q.rng, true
				break
			}
		}
		if !found {
			return 0, fmt.Errorf("invalid INTERVAL type modifier")
		}
	}
	prec := fullPrec
	switch {
	case precision == -1:
	case precision < 0:
		return 0, fmt.Errorf("INTERVAL(%d) precision must not be negative", precision)
	case precision > MaxPrecision:
		prec = MaxPrecision
	default:
		prec = int32(precision)
	}
	if rng == fullRange && prec == fullPrec {
		return NoTypmod, nil
	}
	return rng<<16 | prec, nil
}

// FormatTypmod is the modifier's text as format_type appends it to
// "interval": " day to second(3)", "(2)", " year", or "" for NoTypmod
// (intervaltypmodout).
func FormatTypmod(t int32) string {
	if t < 0 {
		return ""
	}
	var b strings.Builder
	if rng := typmodRange(t); rng != fullRange {
		for _, q := range qualifiers {
			if q.rng == rng {
				b.WriteString(" " + q.name)
				break
			}
		}
	}
	if p := typmodPrec(t); p != fullPrec {
		fmt.Fprintf(&b, "(%d)", p)
	}
	return b.String()
}

// intervalScales and intervalOffsets round the clock to a precision:
// add half the dropped unit away from zero, then truncate.
var (
	intervalScales  = [MaxPrecision + 1]int64{1000000, 100000, 10000, 1000, 100, 10, 1}
	intervalOffsets = [MaxPrecision + 1]int64{500000, 50000, 5000, 500, 50, 5, 0}
)

// Adjust applies a type modifier to a value (AdjustIntervalForTypmod):
// zero the fields right of the qualifier's last field, then round the
// clock to the precision, half away from zero.
func Adjust(iv Interval, t int32) (Interval, error) {
	if t < 0 {
		return iv, nil
	}
	rng, prec := typmodRange(t), typmodPrec(t)
	switch rng {
	case fullRange, rangeSecond, rangeDay | rangeHour | rangeMinute | rangeSecond,
		rangeHour | rangeMinute | rangeSecond, rangeMinute | rangeSecond:
		// Nothing to zero; the precision below rounds the seconds.
	case rangeYear:
		iv.Months = (iv.Months / MonthsPerYear) * MonthsPerYear
		iv.Days, iv.Micros = 0, 0
	case rangeMonth, rangeYear | rangeMonth:
		iv.Days, iv.Micros = 0, 0
	case rangeDay:
		iv.Micros = 0
	case rangeHour, rangeDay | rangeHour:
		iv.Micros = (iv.Micros / MicrosPerHour) * MicrosPerHour
	case rangeMinute, rangeDay | rangeHour | rangeMinute, rangeHour | rangeMinute:
		iv.Micros = (iv.Micros / MicrosPerMinute) * MicrosPerMinute
	default:
		return Interval{}, fmt.Errorf("unrecognized interval typmod: %d", t)
	}
	if prec != fullPrec {
		if prec < 0 || prec > MaxPrecision {
			return Interval{}, fmt.Errorf("interval(%d) precision must be between 0 and %d", prec, MaxPrecision)
		}
		var ok bool
		if iv.Micros >= 0 {
			iv.Micros, ok = add64(iv.Micros, intervalOffsets[prec])
		} else {
			iv.Micros, ok = sub64(iv.Micros, intervalOffsets[prec])
		}
		if !ok {
			return Interval{}, ErrOutOfRange
		}
		iv.Micros -= iv.Micros % intervalScales[prec]
	}
	return iv, nil
}
