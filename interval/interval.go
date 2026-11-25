// Package interval is bytdb's interval value: Postgres's interval type,
// a span of time kept as three separate fields.
//
//	Months  int32   calendar months (a year is 12)
//	Days    int32   calendar days
//	Micros  int64   clock time in microseconds
//
// The fields stay apart because they mean different things to a
// timestamp: adding '1 mon' to Jan 31 lands on the last day of February,
// adding '30 days' lands on Mar 1 or 2, and adding '720 hours' ignores
// the calendar altogether. Postgres keeps them apart for that reason and
// never normalizes them on its own, so '36 hours' stays '36:00:00' until
// justify_hours folds it into '1 day 12:00:00'.
//
// # Equality and order
//
// Comparison is not field by field. Postgres compares intervals by
// their span, which treats a month as 30 days and a day as 24 hours:
//
//	span = (Months*30 + Days) * 86_400_000_000 + Micros
//
// so '1 mon' = '30 days' = '720 hours', and a unique index holds only
// one of them. The span needs up to 74 bits (2^31 months × 2.6e12 µs
// per month), so it is computed as a 128-bit integer (Span); Cmp and
// the tuple key encoding both use it.
//
// # Errors
//
// Arithmetic that leaves a field's range fails with ErrOutOfRange
// ("interval out of range", SQLSTATE 22008), and timestamp arithmetic
// that leaves the timestamp range with ErrTimestampOutOfRange. Bad input
// text fails with an *InputError carrying Postgres's message.
//
// The rules follow Postgres 16's src/backend/utils/adt/timestamp.c and
// datetime.c, including the floating-point steps of interval * float8
// and interval / float8, so results match digit for digit (checked
// against PG 16.9 by the golden tests in this package).
//
// Interval infinity ('infinity'::interval) arrived in Postgres 17 and
// is not modelled: Postgres 16 refuses it as bad input, and so does
// Parse.
package interval

import (
	"errors"
	"math"
	"math/bits"
)

// Interval is one interval value. The zero value is the zero interval
// ('00:00:00'). It is a plain comparable struct, but == is field
// equality, which is stricter than SQL equality: use Cmp for that.
type Interval struct {
	Months int32
	Days   int32
	Micros int64
}

// Unit sizes, as Postgres defines them for interval arithmetic.
const (
	MicrosPerSecond int64 = 1_000_000
	MicrosPerMinute       = 60 * MicrosPerSecond
	MicrosPerHour         = 60 * MicrosPerMinute
	MicrosPerDay          = 24 * MicrosPerHour
	DaysPerMonth          = 30 // a month's length in span, justify_days and fraction cascades
	MonthsPerYear         = 12
	// daysPerYear is DAYS_PER_YEAR: extract(epoch) counts a year as
	// 365.25 days, unlike the span, which counts it as 12 × 30.
	daysPerYear = 365.25
)

// ErrOutOfRange is Postgres's "interval out of range" (22008): a result
// field that does not fit its type.
var ErrOutOfRange = errors.New("interval out of range")

// ErrTimestampOutOfRange is "timestamp out of range" (22008): timestamp
// arithmetic that leaves Postgres's timestamp range.
var ErrTimestampOutOfRange = errors.New("timestamp out of range")

// FromMicros is the interval of a plain microsecond count, with no
// calendar part.
func FromMicros(us int64) Interval { return Interval{Micros: us} }

// Span is the interval's position on the comparison line, as a 128-bit
// two's-complement integer hi:lo: (Months*30 + Days) days in
// microseconds, plus Micros. It is Postgres's interval_cmp_value.
func (iv Interval) Span() (hi int64, lo uint64) {
	// Months*30 + Days fits easily in int64 (|x| < 2^37).
	days := int64(iv.Months)*DaysPerMonth + int64(iv.Days)
	hi, lo = mul64(days, MicrosPerDay)
	return add128(hi, lo, iv.Micros)
}

// mul64 is the full 128-bit product of two int64s.
func mul64(a, b int64) (hi int64, lo uint64) {
	neg := (a < 0) != (b < 0)
	ua, ub := uabs(a), uabs(b)
	h, l := bits.Mul64(ua, ub)
	if neg {
		h, l = neg128(h, l)
	}
	return int64(h), l
}

// add128 adds a sign-extended int64 to a 128-bit value.
func add128(hi int64, lo uint64, v int64) (int64, uint64) {
	ext := uint64(0)
	if v < 0 {
		ext = math.MaxUint64 // sign extension of v into the high word
	}
	l, carry := bits.Add64(lo, uint64(v), 0)
	h, _ := bits.Add64(uint64(hi), ext, carry)
	return int64(h), l
}

// neg128 is two's-complement negation of hi:lo.
func neg128(hi, lo uint64) (uint64, uint64) {
	l, borrow := bits.Sub64(0, lo, 0)
	h, _ := bits.Sub64(0, hi, borrow)
	return h, l
}

func uabs(v int64) uint64 {
	if v < 0 {
		return uint64(-v) // -MinInt64 wraps to itself, whose uint64 is 2^63: still right
	}
	return uint64(v)
}

// Cmp orders two intervals by span: -1, 0 or +1. Equal spans compare
// equal even when the fields differ ('1 mon' vs '30 days').
func (iv Interval) Cmp(o Interval) int {
	ah, al := iv.Span()
	bh, bl := o.Span()
	switch {
	case ah < bh:
		return -1
	case ah > bh:
		return 1
	case al < bl:
		return -1
	case al > bl:
		return 1
	}
	return 0
}

// FromSpan rebuilds an interval from a span, choosing the fields
// justify_interval would: whole 30-day months, then whole days, then the
// clock remainder, all with the span's sign. Span(FromSpan(s)) == s for
// every span an Interval can have (ValidSpan); this is what the tuple
// key form decodes to, and keys are re-encoded from it (an index entry's
// primary key is decoded and encoded again to find the row).
//
// For the most extreme spans the whole months do not fit an int32. The
// months then saturate and the excess stays in Days and, past that, in
// Micros, which keeps the span exact. The work is done on magnitudes,
// whose limits are one larger for a negative span (int32 and int64
// reach -2^31 and -2^63 but only +2^31-1 and +2^63-1), so the most
// negative interval rebuilds exactly too.
func FromSpan(hi int64, lo uint64) Interval {
	neg := hi < 0
	uh, ul := uint64(hi), lo
	if neg {
		uh, ul = neg128(uh, ul)
	}
	// Micros needs no limit of its own: ValidSpan's bound keeps the
	// remainder within int64 once months and days hold all they can.
	limit32 := uint64(math.MaxInt32)
	if neg {
		limit32++
	}
	// |span| < 2^75 (ValidSpan), so dividing by a day leaves a quotient
	// that fits a uint64 (bits.Div64 requires uh < divisor, which holds).
	days, rem := bits.Div64(uh, ul, uint64(MicrosPerDay))
	months := days / DaysPerMonth
	d := days - months*DaysPerMonth
	micros := rem
	if months > limit32 {
		d += (months - limit32) * DaysPerMonth
		months = limit32
	}
	if d > limit32 {
		micros += (d - limit32) * uint64(MicrosPerDay)
		d = limit32
	}
	if neg {
		return Interval{
			Months: int32(-int64(months)),
			Days:   int32(-int64(d)),
			Micros: int64(^micros + 1), // two's-complement negation; -2^63 included
		}
	}
	return Interval{Months: int32(months), Days: int32(d), Micros: int64(micros)}
}

// ValidSpan reports whether hi:lo is a span some Interval has: within
// ±((2^31 months × 30 + 2^31 days) × 86_400_000_000 + 2^63 µs). Keys
// decoded from disk are checked with it before FromSpan, whose quotient
// arithmetic assumes the bound.
func ValidSpan(hi int64, lo uint64) bool {
	mh, ml := Interval{Months: math.MaxInt32, Days: math.MaxInt32, Micros: math.MaxInt64}.Span()
	nh, nl := Interval{Months: math.MinInt32, Days: math.MinInt32, Micros: math.MinInt64}.Span()
	le := func(ah int64, al uint64, bh int64, bl uint64) bool { return ah < bh || (ah == bh && al <= bl) }
	return le(nh, nl, hi, lo) && le(hi, lo, mh, ml)
}

// Neg is unary minus (interval_um). Negating a field holding its type's
// minimum overflows.
func (iv Interval) Neg() (Interval, error) {
	if iv.Months == math.MinInt32 || iv.Days == math.MinInt32 || iv.Micros == math.MinInt64 {
		return Interval{}, ErrOutOfRange
	}
	return Interval{Months: -iv.Months, Days: -iv.Days, Micros: -iv.Micros}, nil
}

// Add is field-wise addition (interval_pl); any field overflowing fails.
func (iv Interval) Add(o Interval) (Interval, error) {
	m, ok1 := add32(iv.Months, o.Months)
	d, ok2 := add32(iv.Days, o.Days)
	us, ok3 := add64(iv.Micros, o.Micros)
	if !ok1 || !ok2 || !ok3 {
		return Interval{}, ErrOutOfRange
	}
	return Interval{Months: m, Days: d, Micros: us}, nil
}

// Sub is field-wise subtraction (interval_mi).
func (iv Interval) Sub(o Interval) (Interval, error) {
	m, ok1 := sub32(iv.Months, o.Months)
	d, ok2 := sub32(iv.Days, o.Days)
	us, ok3 := sub64(iv.Micros, o.Micros)
	if !ok1 || !ok2 || !ok3 {
		return Interval{}, ErrOutOfRange
	}
	return Interval{Months: m, Days: d, Micros: us}, nil
}

func add32(a, b int32) (int32, bool) {
	s := int64(a) + int64(b)
	return int32(s), s >= math.MinInt32 && s <= math.MaxInt32
}

func sub32(a, b int32) (int32, bool) {
	s := int64(a) - int64(b)
	return int32(s), s >= math.MinInt32 && s <= math.MaxInt32
}

func add64(a, b int64) (int64, bool) {
	s := a + b
	// Overflow iff both operands share a sign the sum does not.
	return s, !((a >= 0) == (b >= 0) && (s >= 0) != (a >= 0))
}

func sub64(a, b int64) (int64, bool) {
	s := a - b
	return s, !((a >= 0) != (b >= 0) && (s >= 0) != (a >= 0))
}

func mul64ok(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	p := a * b
	if p/b != a || (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64) {
		return 0, false
	}
	return p, true
}

// fitsInt32 and fitsInt64 are FLOAT8_FITS_IN_INT32/64: a float that
// truncates to a value of the type. NaN fails both.
func fitsInt32(f float64) bool { return f >= math.MinInt32 && f < -float64(math.MinInt32) }
func fitsInt64(f float64) bool { return f >= math.MinInt64 && f < -float64(math.MinInt64) }

// tsround is Postgres's TSROUND: round to the nearest microsecond's
// worth of a unit, to clean up float noise before truncating.
func tsround(x float64) float64 { return math.RoundToEven(float64(x*1e6)) / 1e6 }

// Mul is interval * float8 (interval_mul). The month and day products
// keep their whole parts; their fractions cascade down — a fractional
// month into 30-day days, a fractional day into seconds — but never up,
// so '1 mon' * 1.5 is '1 mon 15 days', not '45 days'.
//
// Every float step mirrors timestamp.c's, in the same order, with an
// explicit float64() around each product so the compiler cannot fuse a
// multiply-add (Go may on arm64) and round differently from Postgres.
func (iv Interval) Mul(factor float64) (Interval, error) {
	return iv.scale(factor, false, false)
}

// Div is interval / float8 (interval_div): the same cascade as Mul with
// division, and division by zero refused.
func (iv Interval) Div(divisor float64) (Interval, error) {
	if divisor == 0 {
		return Interval{}, ErrDivisionByZero
	}
	return iv.scale(divisor, true, false)
}

// ErrDivisionByZero is "division by zero" (22012).
var ErrDivisionByZero = errors.New("division by zero")

// scale is the shared body of Mul (div false: x*f) and Div (x/f).
//
//	month' = trunc(month·f)                    whole months
//	day'   = trunc(day·f)                      whole days
//	mrd    = tsround((month·f − month')·30)    fractional month, in days
//	sec    = tsround((day·f − day' + mrd − trunc(mrd))·86400)
//	         fractional day plus the month's fractional day, in seconds
//	if |sec| ≥ 1 day: carry whole days into day'
//	day'  += trunc(mrd)
//	time'  = rint(time·f + sec·1e6)
//
// fused computes the a·b − c and a·b + c steps as single fused
// multiply-adds instead, which is what a Postgres built by gcc for arm64
// does (gcc contracts them by default there; x86-64 builds have no FMA
// to contract to). The two agree except in the last bits of very large
// products, e.g. '2147483647 days' * 0.1. bytdb computes unfused, as
// x86-64 Postgres does; the golden tests, captured from an arm64
// Postgres, use fused to explain the handful of cases that differ.
func (iv Interval) scale(f float64, div, fused bool) (Interval, error) {
	op := func(x float64) float64 {
		if div {
			return float64(x / f)
		}
		return float64(x * f)
	}
	// opSub is op(x) - y, the step gcc fuses for multiplication.
	opSub := func(x, y float64) float64 {
		if fused && !div {
			return math.FMA(x, f, -y)
		}
		return op(x) - y
	}
	var out Interval
	m := op(float64(iv.Months))
	if math.IsNaN(m) || !fitsInt32(m) {
		return Interval{}, ErrOutOfRange
	}
	out.Months = int32(m)
	d := op(float64(iv.Days))
	if math.IsNaN(d) || !fitsInt32(d) {
		return Interval{}, ErrOutOfRange
	}
	out.Days = int32(d)

	monthRemDays := tsround(float64(opSub(float64(iv.Months), float64(out.Months)) * DaysPerMonth))
	secRem := tsround(float64((opSub(float64(iv.Days), float64(out.Days)) +
		monthRemDays - float64(int32(monthRemDays))) * 86400))
	if math.Abs(secRem) >= 86400 {
		carry := int32(secRem / 86400)
		days, ok := add32(out.Days, carry)
		if !ok {
			return Interval{}, ErrOutOfRange
		}
		out.Days = days
		secRem -= float64(carry) * 86400
	}
	days, ok := add32(out.Days, int32(monthRemDays))
	if !ok {
		return Interval{}, ErrOutOfRange
	}
	out.Days = days
	var t float64
	switch {
	case fused && !div:
		t = math.FMA(float64(iv.Micros), f, float64(secRem*1e6))
	case fused:
		t = math.FMA(secRem, 1e6, op(float64(iv.Micros)))
	default:
		t = op(float64(iv.Micros)) + float64(secRem*1e6)
	}
	t = math.RoundToEven(t)
	if math.IsNaN(t) || !fitsInt64(t) {
		return Interval{}, ErrOutOfRange
	}
	out.Micros = int64(t)
	return out, nil
}

// tmodulo is TMODULO: whole units of u split off t, truncating toward
// zero, so the remainder keeps t's sign.
func tmodulo(t, u int64) (rest, whole int64) {
	whole = t / u
	return t - whole*u, whole
}

// JustifyHours folds whole 24-hour runs of the clock part into days,
// leaving days and time with one sign (justify_hours).
func (iv Interval) JustifyHours() (Interval, error) {
	out := iv
	rest, whole := tmodulo(out.Micros, MicrosPerDay)
	out.Micros = rest
	d, ok := add32(out.Days, int32(whole))
	if !ok || whole > math.MaxInt32 || whole < math.MinInt32 {
		return Interval{}, ErrOutOfRange
	}
	out.Days = d
	if out.Days > 0 && out.Micros < 0 {
		out.Micros += MicrosPerDay
		out.Days--
	} else if out.Days < 0 && out.Micros > 0 {
		out.Micros -= MicrosPerDay
		out.Days++
	}
	return out, nil
}

// JustifyDays folds whole 30-day runs of days into months, leaving
// months and days with one sign (justify_days).
func (iv Interval) JustifyDays() (Interval, error) {
	out := iv
	whole := out.Days / DaysPerMonth
	out.Days -= whole * DaysPerMonth
	m, ok := add32(out.Months, whole)
	if !ok {
		return Interval{}, ErrOutOfRange
	}
	out.Months = m
	if out.Months > 0 && out.Days < 0 {
		out.Days += DaysPerMonth
		out.Months--
	} else if out.Months < 0 && out.Days > 0 {
		out.Days -= DaysPerMonth
		out.Months++
	}
	return out, nil
}

// JustifyInterval is justify_days and justify_hours together, with every
// field ending up with one sign (justify_interval).
func (iv Interval) JustifyInterval() (Interval, error) {
	out := iv
	// Pre-justify days when that keeps the day sum below from
	// overflowing (same-signed days and time).
	if (out.Days > 0 && out.Micros > 0) || (out.Days < 0 && out.Micros < 0) {
		whole := out.Days / DaysPerMonth
		out.Days -= whole * DaysPerMonth
		m, ok := add32(out.Months, whole)
		if !ok {
			return Interval{}, ErrOutOfRange
		}
		out.Months = m
	}
	rest, wholeDay := tmodulo(out.Micros, MicrosPerDay)
	out.Micros = rest
	out.Days += int32(wholeDay) // cannot overflow, as timestamp.c argues
	whole := out.Days / DaysPerMonth
	out.Days -= whole * DaysPerMonth
	m, ok := add32(out.Months, whole)
	if !ok {
		return Interval{}, ErrOutOfRange
	}
	out.Months = m
	if out.Months > 0 && (out.Days < 0 || (out.Days == 0 && out.Micros < 0)) {
		out.Days += DaysPerMonth
		out.Months--
	} else if out.Months < 0 && (out.Days > 0 || (out.Days == 0 && out.Micros > 0)) {
		out.Days -= DaysPerMonth
		out.Months++
	}
	if out.Days > 0 && out.Micros < 0 {
		out.Micros += MicrosPerDay
		out.Days--
	} else if out.Days < 0 && out.Micros > 0 {
		out.Micros -= MicrosPerDay
		out.Days++
	}
	return out, nil
}

// Make is make_interval(years, months, weeks, days, hours, mins, secs).
// The integer arguments are Postgres's int4s; secs is a float8.
func Make(years, months, weeks, days, hours, mins int64, secs float64) (Interval, error) {
	for _, v := range []int64{years, months, weeks, days, hours, mins} {
		if v < math.MinInt32 || v > math.MaxInt32 {
			return Interval{}, ErrOutOfRange
		}
	}
	if math.IsInf(secs, 0) || math.IsNaN(secs) {
		return Interval{}, ErrOutOfRange
	}
	var out Interval
	m := years * MonthsPerYear
	if m < math.MinInt32 || m > math.MaxInt32 {
		return Interval{}, ErrOutOfRange
	}
	mm, ok := add32(int32(m), int32(months))
	if !ok {
		return Interval{}, ErrOutOfRange
	}
	out.Months = mm
	d := weeks * 7
	if d < math.MinInt32 || d > math.MaxInt32 {
		return Interval{}, ErrOutOfRange
	}
	dd, ok := add32(int32(d), int32(days))
	if !ok {
		return Interval{}, ErrOutOfRange
	}
	out.Days = dd
	// hours and mins are int4s, so this cannot overflow int64.
	out.Micros = hours*MicrosPerHour + mins*MicrosPerMinute
	s := math.RoundToEven(float64(secs * float64(MicrosPerSecond)))
	if !fitsInt64(s) {
		return Interval{}, ErrOutOfRange
	}
	us, ok := add64(out.Micros, int64(s))
	if !ok {
		return Interval{}, ErrOutOfRange
	}
	out.Micros = us
	return out, nil
}
