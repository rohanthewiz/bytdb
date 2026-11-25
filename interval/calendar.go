package interval

// calendar.go: intervals meeting timestamps — timestamp ± interval,
// timestamp − timestamp, age(), and the interval forms of extract and
// date_trunc.
//
// Timestamps here are bytdb's: int64 microseconds since 1970-01-01
// 00:00 UTC, with math.MaxInt64 and math.MinInt64 standing for
// 'infinity' and '-infinity' (bytdb.TimestampInfinity and
// TimestampNegInfinity). bytdb keeps every timestamp in UTC, so the
// calendar is the proleptic Gregorian one with no time zone or DST;
// Postgres's timestamptz arithmetic in a UTC session gives the same
// answers.
//
// Adding an interval applies its fields largest first, each on the
// calendar (timestamp_pl_interval):
//
//	months  move the month, keeping the day of month but clamping it to
//	        the new month's length (Jan 31 + 1 mon = Feb 28/29)
//	days    move the date by whole days (the clock is kept)
//	micros  move the instant
//
// so 2024-01-31 + '1 mon' is Feb 29 (the day clamped to February's
// length), while + '30 days' and + '720 hours' are both Mar 1. The three
// intervals are equal as values yet move a timestamp differently, which
// is why the fields are kept apart.

import (
	"math"
	"math/big"
	"strings"

	"github.com/rohanthewiz/bytdb/decimal"
)

// Timestamp sentinels, equal to bytdb.TimestampInfinity and
// bytdb.TimestampNegInfinity (this package cannot import bytdb).
const (
	tsInfinity    = math.MaxInt64
	tsNegInfinity = math.MinInt64
)

// minTimestamp is Postgres's MIN_TIMESTAMP (4714-11-24 00:00:00 BC, the
// start of the Julian day count) in microseconds since 1970. Postgres's
// upper bound (294277 AD) lies beyond int64 microseconds since 1970, so
// overflow is the upper check; MaxInt64 itself is 'infinity'.
const minTimestamp int64 = -210866803200000000

func finiteTS(us int64) bool { return us != tsInfinity && us != tsNegInfinity }

func validTS(us int64) bool { return us >= minTimestamp && us < tsInfinity }

// splitTS breaks a timestamp into whole days since 1970 (floored) and
// microseconds into that day (0 ≤ tod < a day).
func splitTS(us int64) (days, tod int64) {
	days = us / MicrosPerDay
	tod = us % MicrosPerDay
	if tod < 0 {
		days--
		tod += MicrosPerDay
	}
	return days, tod
}

// civil converts days since 1970-01-01 to a proleptic Gregorian date
// (year 0 is 1 BC, as in Postgres's internal year numbering). Howard
// Hinnant's civil_from_days: exact over the whole int64-days range.
func civil(days int64) (y int64, m, d int) {
	z := days + 719468
	era := z / 146097
	if z < 0 && z%146097 != 0 {
		era--
	}
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y = yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d = int(doy - (153*mp+2)/5 + 1)
	if mp < 10 {
		m = int(mp + 3)
	} else {
		m = int(mp - 9)
	}
	if m <= 2 {
		y++
	}
	return y, m, d
}

// daysFrom is civil's inverse (days_from_civil).
func daysFrom(y int64, m, d int) int64 {
	if m <= 2 {
		y--
	}
	era := y / 400
	if y < 0 && y%400 != 0 {
		era--
	}
	yoe := y - era*400
	mp := int64(m + 9)
	if m > 2 {
		mp = int64(m - 3)
	}
	doy := (153*mp+2)/5 + int64(d) - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

func isLeap(y int64) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }

var monthDays = [2][12]int{
	{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31},
	{31, 29, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31},
}

func daysIn(y int64, m int) int {
	if isLeap(y) {
		return monthDays[1][m-1]
	}
	return monthDays[0][m-1]
}

// AddTo is timestamp + interval. Infinite timestamps stay infinite.
func AddTo(ts int64, iv Interval) (int64, error) {
	if !finiteTS(ts) {
		return ts, nil
	}
	if iv.Months != 0 {
		days, tod := splitTS(ts)
		y, m, d := civil(days)
		// Month arithmetic on a zero-based month count, then back.
		total := y*MonthsPerYear + int64(m-1) + int64(iv.Months)
		ny := total / MonthsPerYear
		nm := total % MonthsPerYear
		if nm < 0 {
			nm += MonthsPerYear
			ny--
		}
		m = int(nm) + 1
		if dm := daysIn(ny, m); d > dm {
			d = dm
		}
		var ok bool
		if ts, ok = fromCivil(ny, m, d, tod); !ok {
			return 0, ErrTimestampOutOfRange
		}
	}
	if iv.Days != 0 {
		days, tod := splitTS(ts)
		days += int64(iv.Days)
		t, ok := mul64ok(days, MicrosPerDay)
		if ok {
			t, ok = add64(t, tod)
		}
		if !ok || !validTS(t) {
			return 0, ErrTimestampOutOfRange
		}
		ts = t
	}
	t, ok := add64(ts, iv.Micros)
	if !ok || !validTS(t) {
		return 0, ErrTimestampOutOfRange
	}
	return t, nil
}

// SubFrom is timestamp - interval: the timestamp plus the negated
// fields (timestamp_mi_interval). The negation is unchecked, as in
// Postgres 16: a field at its minimum stays there (two's-complement
// wrap), and an infinite timestamp is returned before it matters.
func SubFrom(ts int64, iv Interval) (int64, error) {
	return AddTo(ts, Interval{Months: -iv.Months, Days: -iv.Days, Micros: -iv.Micros})
}

// fromCivil builds a timestamp from a date and a time of day, failing
// outside the timestamp range.
func fromCivil(y int64, m, d int, tod int64) (int64, bool) {
	if y > 300000 || y < -5000 {
		return 0, false // far outside the range; keeps daysFrom's math small
	}
	t, ok := mul64ok(daysFrom(y, m, d), MicrosPerDay)
	if ok {
		t, ok = add64(t, tod)
	}
	return t, ok && validTS(t)
}

// Between is timestamp - timestamp (timestamp_mi): the difference as
// days and clock time (justify_hours), never months, since the
// difference's month count is not well defined. Infinite operands fail.
func Between(t1, t2 int64) (Interval, error) {
	if !finiteTS(t1) || !finiteTS(t2) {
		return Interval{}, errInfiniteSub
	}
	d, ok := sub64(t1, t2)
	if !ok {
		return Interval{}, ErrOutOfRange
	}
	return Interval{Micros: d}.JustifyHours()
}

// errInfiniteSub is Postgres's wording for timestamp - timestamp with
// an infinite side (22008).
var errInfiniteSub = errorString("cannot subtract infinite timestamps")

type errorString string

func (e errorString) Error() string { return string(e) }

// tm is a broken-down timestamp (pg_tm plus fsec).
type tm struct {
	year           int64
	mon, mday      int
	hour, min, sec int
	usec           int64
}

// pgEpochDays is the days from 1970-01-01 to 2000-01-01, Postgres's
// timestamp epoch.
const pgEpochDays = 10957

func breakDown(ts int64) tm {
	days, tod := splitTS(ts)
	if ts == tsInfinity {
		// Postgres 16's age() breaks 'infinity' down as the raw int64 it
		// stores, MaxInt64 microseconds after 2000-01-01 (a date in
		// 294277 AD), rather than refusing it. bytdb's sentinel is
		// MaxInt64 after 1970, so reinterpret it on Postgres's epoch.
		days, tod = splitTS(ts)
		days += pgEpochDays
	}
	y, m, d := civil(days)
	return tm{
		year: y, mon: m, mday: d,
		hour: int(tod / MicrosPerHour),
		min:  int(tod % MicrosPerHour / MicrosPerMinute),
		sec:  int(tod % MicrosPerMinute / MicrosPerSecond),
		usec: tod % MicrosPerSecond,
	}
}

// Age is age(t1, t2): the difference as years, months, days and clock,
// field by field like a hand subtraction with borrows, so
// age('2024-03-31', '2024-02-29') is '1 mon 2 days' rather than a day
// count (timestamp_age). -infinity is out of range; +infinity is read
// the way Postgres 16 reads it (see breakDown).
func Age(t1, t2 int64) (Interval, error) {
	// -infinity has no broken-down form in Postgres (its Julian day is
	// negative); +infinity does, see breakDown.
	if t1 == tsNegInfinity || t2 == tsNegInfinity {
		return Interval{}, ErrTimestampOutOfRange
	}
	a, b := breakDown(t1), breakDown(t2)
	usec := a.usec - b.usec
	sec := int64(a.sec - b.sec)
	min := int64(a.min - b.min)
	hour := int64(a.hour - b.hour)
	mday := int64(a.mday - b.mday)
	mon := int64(a.mon - b.mon)
	year := a.year - b.year
	flip := t1 < t2
	if flip {
		usec, sec, min, hour, mday, mon, year = -usec, -sec, -min, -hour, -mday, -mon, -year
	}
	for usec < 0 {
		usec += MicrosPerSecond
		sec--
	}
	for sec < 0 {
		sec += 60
		min--
	}
	for min < 0 {
		min += 60
		hour--
	}
	for hour < 0 {
		hour += 24
		mday--
	}
	for mday < 0 {
		// Borrow a month: the length of the earlier timestamp's month.
		if flip {
			mday += int64(daysIn(a.year, a.mon))
		} else {
			mday += int64(daysIn(b.year, b.mon))
		}
		mon--
	}
	for mon < 0 {
		mon += MonthsPerYear
		year--
	}
	if flip {
		usec, sec, min, hour, mday, mon, year = -usec, -sec, -min, -hour, -mday, -mon, -year
	}
	// itm2interval
	months := year*MonthsPerYear + mon
	if months > math.MaxInt32 || months < math.MinInt32 || mday > math.MaxInt32 || mday < math.MinInt32 {
		return Interval{}, ErrOutOfRange
	}
	us := usec + sec*MicrosPerSecond + min*MicrosPerMinute + hour*MicrosPerHour
	return Interval{Months: int32(months), Days: int32(mday), Micros: us}, nil
}

// canonUnit maps a units word to its canonical name the way Postgres
// resolves the unit of extract and date_trunc: through the interval unit
// words (DecodeUnits, which knows "quarter" and "timezone" too), and for
// extract only, failing that, through the date keywords (DecodeSpecial:
// "epoch", "mm" for minutes, and the day-of-week style units that have
// no interval meaning). "" means the word is not a unit at all.
func canonUnit(u string, special bool) string {
	u = strings.ToLower(u)
	if w, ok := lookupUnit(u); ok {
		switch w {
		case uMicro:
			return "microseconds"
		case uMilli:
			return "milliseconds"
		case uSecond:
			return "second"
		case uMinute:
			return "minute"
		case uHour:
			return "hour"
		case uDay:
			return "day"
		case uWeek:
			return "week"
		case uMonth:
			return "month"
		case uYear:
			return "year"
		case uDecade:
			return "decade"
		case uCentury:
			return "century"
		case uMillennium:
			return "millennium"
		case uQuarter:
			return "quarter"
		case uTimezone:
			return "timezone"
		}
		return "" // "ago"
	}
	if special {
		switch u {
		case "epoch", "dow", "doy", "isodow", "isoyear", "julian", "j", "jd":
			return u
		case "mm":
			return "minute"
		}
	}
	return ""
}

// UnitError is extract's or date_trunc's refusal of a unit, worded as
// Postgres words it. Supported reports which: "unit … not supported for
// type interval" (0A000) for a real unit that has no interval meaning,
// "unit … not recognized for type interval" (22023) otherwise.
type UnitError struct {
	Unit      string
	Supported bool // false: unknown word; true: known but not for interval
	Detail    string
}

func (e *UnitError) Error() string {
	if e.Supported {
		return `unit "` + e.Unit + `" not supported for type interval`
	}
	return `unit "` + e.Unit + `" not recognized for type interval`
}

// parts is interval2itm's split, as extract reads it.
func (iv Interval) parts() (year, mon, mday, hour, min, sec, usec int64) {
	year = int64(iv.Months / MonthsPerYear)
	mon = int64(iv.Months % MonthsPerYear)
	mday = int64(iv.Days)
	t := iv.Micros
	hour = t / MicrosPerHour
	t -= hour * MicrosPerHour
	min = t / MicrosPerMinute
	t -= min * MicrosPerMinute
	sec = t / MicrosPerSecond
	usec = t - sec*MicrosPerSecond
	return
}

// Extract is EXTRACT(unit FROM interval) — Postgres 14+'s numeric
// result. The fields come from the interval as stored (no
// justification): extract(hour from '36 hours') is 36.
func (iv Interval) Extract(unitText string) (decimal.Decimal, error) {
	u := canonUnit(unitText, true)
	year, mon, mday, hour, min, sec, usec := iv.parts()
	switch u {
	case "microseconds":
		return decimal.FromInt64(sec*MicrosPerSecond + usec), nil
	case "milliseconds":
		return decN(sec*MicrosPerSecond+usec, 3), nil
	case "second":
		return decN(sec*MicrosPerSecond+usec, 6), nil
	case "minute":
		return decimal.FromInt64(min), nil
	case "hour":
		return decimal.FromInt64(hour), nil
	case "day":
		return decimal.FromInt64(mday), nil
	case "month":
		return decimal.FromInt64(mon), nil
	case "quarter":
		return decimal.FromInt64(mon/3 + 1), nil
	case "year":
		return decimal.FromInt64(year), nil
	case "decade":
		return decimal.FromInt64(year / 10), nil
	case "century":
		return decimal.FromInt64(year / 100), nil
	case "millennium":
		return decimal.FromInt64(year / 1000), nil
	case "epoch":
		// A year counts 365.25 days here (DAYS_PER_YEAR, so a whole
		// number of seconds), a month 30 days.
		secs := int64(daysPerYear*86400)*int64(iv.Months/MonthsPerYear) +
			DaysPerMonth*86400*int64(iv.Months%MonthsPerYear) + 86400*int64(iv.Days)
		if v, ok := mul64ok(secs, MicrosPerSecond); ok {
			if v, ok = add64(v, iv.Micros); ok {
				return decN(v, 6), nil
			}
		}
		// Past int64 microseconds (around 10^9 days): exact decimal math.
		return decimal.FromInt64(secs).Add(decN(iv.Micros, 6))
	case "":
		return decimal.Decimal{}, &UnitError{Unit: strings.ToLower(unitText)}
	}
	return decimal.Decimal{}, &UnitError{Unit: strings.ToLower(unitText), Supported: true}
}

// decN is the decimal coef × 10^-scale.
func decN(coef int64, scale int32) decimal.Decimal {
	return decimal.New(big.NewInt(coef), scale)
}

// Part is date_part(unit, interval): Extract's value as a float8, with
// Postgres's own float formulas (they can differ from the numeric
// result in the last bit).
func (iv Interval) Part(unitText string) (float64, error) {
	u := canonUnit(unitText, true)
	_, _, _, _, _, sec, usec := iv.parts()
	switch u {
	case "microseconds":
		return float64(float64(sec)*1e6) + float64(usec), nil
	case "milliseconds":
		return float64(float64(sec)*1000) + float64(usec)/1000, nil
	case "second":
		return float64(sec) + float64(usec)/1e6, nil
	case "epoch":
		r := float64(iv.Micros) / 1e6
		r += float64(daysPerYear*86400) * float64(iv.Months/MonthsPerYear)
		r += float64(DaysPerMonth*86400) * float64(iv.Months%MonthsPerYear)
		r += float64(86400) * float64(iv.Days)
		return r, nil
	}
	d, err := iv.Extract(unitText)
	if err != nil {
		return 0, err
	}
	return d.Float64(), nil
}

// Trunc is date_trunc(unit, interval): every field below the unit
// zeroed, from the stored fields (interval_trunc).
func (iv Interval) Trunc(unitText string) (Interval, error) {
	u := canonUnit(unitText, false)
	year, mon, mday, hour, min, sec, usec := iv.parts()
	switch u {
	case "millennium":
		year = (year / 1000) * 1000
		fallthrough
	case "century":
		year = (year / 100) * 100
		fallthrough
	case "decade":
		year = (year / 10) * 10
		fallthrough
	case "year":
		mon = 0
		fallthrough
	case "quarter":
		mon = 3 * (mon / 3)
		fallthrough
	case "month":
		mday = 0
		fallthrough
	case "day":
		hour = 0
		fallthrough
	case "hour":
		min = 0
		fallthrough
	case "minute":
		sec = 0
		fallthrough
	case "second":
		usec = 0
	case "milliseconds":
		usec = (usec / 1000) * 1000
	case "microseconds":
	case "":
		return Interval{}, &UnitError{Unit: strings.ToLower(unitText)}
	default:
		e := &UnitError{Unit: strings.ToLower(unitText), Supported: true}
		if u == "week" {
			e.Detail = "Months usually have fractional weeks."
		}
		return Interval{}, e
	}
	months := year*MonthsPerYear + mon
	if months > math.MaxInt32 || months < math.MinInt32 {
		return Interval{}, ErrOutOfRange
	}
	return Interval{
		Months: int32(months), Days: int32(mday),
		Micros: usec + sec*MicrosPerSecond + min*MicrosPerMinute + hour*MicrosPerHour,
	}, nil
}
