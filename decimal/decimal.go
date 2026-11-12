// Package decimal is bytdb's exact numeric value: an arbitrary-precision
// decimal with a display scale, following Postgres's numeric type.
//
// A Decimal is coef × 10^-scale. The scale is the number of digits after
// the decimal point the value displays with, and it is part of the value
// the way Postgres's dscale is: 1.0 and 1.00 are equal (Cmp returns 0,
// keys and hash keys normalize) but render differently, and arithmetic
// carries the scale through by Postgres's rules:
//
//	a + b, a - b   scale max(sa, sb)
//	a * b          scale sa + sb
//	a / b          scale from Postgres's select_div_scale (DivScale)
//	a % b          scale max(sa, sb)
//	round(a, n)    scale max(n, 0), half away from zero
//
// There is no NaN or ±Infinity: Postgres has them, but every operation
// would need a special case, and bytdb refuses them on input instead
// (ErrSpecial). The tuple encoding leaves room to add them later.
//
// A Decimal is immutable. Every operation returns a new value and never
// writes through the *big.Int it holds, so values can be shared freely
// across rows, goroutines and cached plans. The zero value is 0 with
// scale 0.
//
// Design note: the coefficient is a *big.Int even for small values. A
// fast path holding an int64 coefficient would save allocations in
// money-sized arithmetic, and is left for when a benchmark asks for it:
// correctness and Postgres parity come first.
package decimal

import (
	"errors"
	"math"
	"math/big"
	"math/bits"
	"strconv"
	"strings"
)

// Limits, from Postgres's numeric implementation.
const (
	// MaxScale is the largest display scale a value may carry
	// (Postgres's NUMERIC_DSCALE_MAX).
	MaxScale = 16383
	// MaxIntDigits bounds the digits before the decimal point
	// (Postgres's NUMERIC_WEIGHT_MAX × DEC_DIGITS, about 131072).
	MaxIntDigits = 131072
	// MaxPrecision is the largest precision numeric(p, s) accepts
	// (Postgres's NUMERIC_MAX_PRECISION).
	MaxPrecision = 1000
	// maxDisplayScale caps the scale division and the transcendental
	// functions choose by themselves (NUMERIC_MAX_DISPLAY_SCALE).
	maxDisplayScale = 1000
	// minSigDigits is how many significant digits a quotient gets at
	// least (NUMERIC_MIN_SIG_DIGITS).
	minSigDigits = 16
)

var (
	// ErrSyntax is the input-syntax failure; callers wrap it with the
	// offending text in Postgres's wording.
	ErrSyntax = errors.New("invalid input syntax for type numeric")
	// ErrSpecial refuses NaN and ±Infinity, which bytdb's numeric does
	// not represent.
	ErrSpecial = errors.New("numeric NaN and Infinity are not supported")
	// ErrOverflow is Postgres's "value overflows numeric format": more
	// integer digits or a larger scale than the type can hold.
	ErrOverflow = errors.New("value overflows numeric format")
	// ErrDivByZero is division (or modulo) by zero.
	ErrDivByZero = errors.New("division by zero")
)

// Decimal is an exact decimal number; see the package doc.
type Decimal struct {
	coef  *big.Int // nil means 0; never mutated once the value exists
	scale int32    // digits after the point; 0 ≤ scale ≤ MaxScale
}

var (
	bigOne = big.NewInt(1)
	bigTen = big.NewInt(10)
)

// pow10 returns 10^n. Small powers come from a shared table, returned
// as is — rescaling by a handful of digits is by far the common case,
// and copying the entry was most of a comparison's allocation — so the
// result is READ-ONLY: callers use it only as an operand (x.Mul(x, p),
// never p.Mul(...)). Every caller in this file does.
func pow10(n int) *big.Int {
	if n < len(pow10Table) {
		return pow10Table[n]
	}
	return new(big.Int).Exp(bigTen, big.NewInt(int64(n)), nil)
}

// pow10u64 holds 10^0..10^19, the powers a uint64 can hold.
var pow10u64 = func() [20]uint64 {
	var t [20]uint64
	p := uint64(1)
	for i := range t {
		t[i] = p
		p *= 10
	}
	return t
}()

var pow10Table = func() []*big.Int {
	t := make([]*big.Int, 40)
	p := big.NewInt(1)
	for i := range t {
		t[i] = new(big.Int).Set(p)
		p.Mul(p, bigTen)
	}
	return t
}()

// mk builds a Decimal from a coefficient it takes ownership of. Every
// constructor goes through it, which keeps one invariant: the
// coefficient is nil exactly when the value is zero. Equality never
// depended on it (Cmp and both encodings read the value), but a single
// representation of zero makes Decimals compare equal structurally too
// (reflect.DeepEqual in tests, %#v in logs).
func mk(coef *big.Int, scale int32) Decimal {
	if coef != nil && coef.Sign() == 0 {
		coef = nil
	}
	return Decimal{coef: coef, scale: scale}
}

// c returns the coefficient, materializing zero for the zero value. The
// result must not be mutated.
func (d Decimal) c() *big.Int {
	if d.coef == nil {
		return new(big.Int)
	}
	return d.coef
}

// New returns coef × 10^-scale. scale must be in [0, MaxScale]; New
// copies coef, so the caller may reuse it.
func New(coef *big.Int, scale int32) Decimal {
	return mk(new(big.Int).Set(coef), scale)
}

// FromInt64 returns n with scale 0.
func FromInt64(n int64) Decimal {
	return mk(big.NewInt(n), 0)
}

// FromFloat64 converts a float64. digits is the number of significant
// digits kept: 15 reproduces Postgres's float8 → numeric cast (it prints
// the float with DBL_DIG digits first, so 0.1::float8::numeric is 0.1),
// and -1 keeps the shortest text that round-trips to the same float,
// which is what a Go caller handing over a float64 means. NaN and
// ±Inf are ErrSpecial.
func FromFloat64(f float64, digits int) (Decimal, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return Decimal{}, ErrSpecial
	}
	// 'g' trims trailing zeros, as C's %.15g does in Postgres's
	// float8_numeric: 0.1 becomes "0.1", not "1.00000000000000e-01"
	// (which would parse with a scale of 15).
	return Parse(strconv.FormatFloat(f, 'g', digits, 64))
}

// Parse reads Postgres numeric input: optional surrounding whitespace,
// an optional sign, digits with an optional point (".5" and "5." both
// count), an optional exponent, and Postgres 16's underscores between
// digits ("1_000.5"). The scale is the digits written after the point,
// less the exponent, floored at 0: "1.50e1" is 15.0, "1e3" is 1000.
func Parse(s string) (Decimal, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Decimal{}, ErrSyntax
	}
	neg := false
	switch s[0] {
	case '-':
		neg = true
		s = s[1:]
	case '+':
		s = s[1:]
	}
	switch strings.ToLower(s) {
	case "nan", "infinity", "inf":
		return Decimal{}, ErrSpecial
	}
	var digits strings.Builder
	frac, sawDigit, sawPoint := 0, false, false
	i := 0
scan:
	for ; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= '0' && ch <= '9':
			digits.WriteByte(ch)
			sawDigit = true
			if sawPoint {
				frac++
			}
		case ch == '_':
			// An underscore must sit between two digits.
			if i == 0 || !isDigit(s[i-1]) || i+1 >= len(s) || !isDigit(s[i+1]) {
				return Decimal{}, ErrSyntax
			}
		case ch == '.' && !sawPoint:
			sawPoint = true
		default:
			break scan // the exponent, or junk
		}
	}
	if !sawDigit {
		return Decimal{}, ErrSyntax
	}
	exp := int64(0)
	if i < len(s) {
		if s[i] != 'e' && s[i] != 'E' {
			return Decimal{}, ErrSyntax
		}
		e, err := strconv.ParseInt(strings.ReplaceAll(s[i+1:], "_", ""), 10, 32)
		if err != nil || s[i+1:] == "" {
			if errors.Is(err, strconv.ErrRange) {
				return Decimal{}, ErrOverflow
			}
			return Decimal{}, ErrSyntax
		}
		exp = e
	}
	coef, ok := new(big.Int).SetString(digits.String(), 10)
	if !ok {
		return Decimal{}, ErrSyntax
	}
	// value = coef × 10^(exp - frac). A positive net exponent folds into
	// the coefficient (scale 0); a negative one is the scale.
	net := exp - int64(frac)
	if net > 0 {
		if net > MaxIntDigits {
			return Decimal{}, ErrOverflow
		}
		coef.Mul(coef, pow10(int(net)))
		net = 0
	}
	if -net > MaxScale {
		return Decimal{}, ErrOverflow
	}
	if neg {
		coef.Neg(coef)
	}
	d := mk(coef, int32(-net))
	if d.IntDigits() > MaxIntDigits {
		return Decimal{}, ErrOverflow
	}
	return d, nil
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// MustParse is Parse that panics; for tests and constants.
func MustParse(s string) Decimal {
	d, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return d
}

// String renders the value in Postgres's output form: plain notation,
// never an exponent, exactly Scale digits after the point, "-" for a
// negative value and no sign for zero.
func (d Decimal) String() string {
	return string(d.AppendText(nil))
}

// AppendText appends String's form to buf.
func (d Decimal) AppendText(buf []byte) []byte {
	c := d.c()
	if c.Sign() < 0 {
		buf = append(buf, '-')
	}
	digits := new(big.Int).Abs(c).Text(10)
	sc := int(d.scale)
	if sc == 0 {
		return append(buf, digits...)
	}
	if len(digits) <= sc {
		buf = append(buf, '0', '.')
		for range sc - len(digits) {
			buf = append(buf, '0')
		}
		return append(buf, digits...)
	}
	buf = append(buf, digits[:len(digits)-sc]...)
	buf = append(buf, '.')
	return append(buf, digits[len(digits)-sc:]...)
}

// Scale is the display scale: digits after the point.
func (d Decimal) Scale() int32 { return d.scale }

// Coef returns a copy of the coefficient: the value is Coef × 10^-Scale.
func (d Decimal) Coef() *big.Int { return new(big.Int).Set(d.c()) }

// Sign is -1, 0 or +1.
func (d Decimal) Sign() int {
	if d.coef == nil {
		return 0
	}
	return d.coef.Sign()
}

// IsZero reports whether the value is 0 (at any scale).
func (d Decimal) IsZero() bool { return d.Sign() == 0 }

// IntDigits is the number of digits before the decimal point, 0 for a
// value whose magnitude is below 1.
func (d Decimal) IntDigits() int {
	n := len(new(big.Int).Abs(d.c()).Text(10)) - int(d.scale)
	if n < 0 || d.IsZero() {
		return 0
	}
	return n
}

// align returns both coefficients at the larger of the two scales.
func align(a, b Decimal) (ac, bc *big.Int, scale int32) {
	ac, bc = a.c(), b.c()
	switch {
	case a.scale < b.scale:
		ac = new(big.Int).Mul(ac, pow10(int(b.scale-a.scale)))
		return ac, bc, b.scale
	case b.scale < a.scale:
		bc = new(big.Int).Mul(bc, pow10(int(a.scale-b.scale)))
		return ac, bc, a.scale
	}
	return ac, bc, a.scale
}

// Cmp compares by value, ignoring scale: 1.0 and 1.00 are equal.
func (d Decimal) Cmp(o Decimal) int {
	// Signs settle most comparisons without aligning.
	ds, os := d.Sign(), o.Sign()
	if ds != os {
		if ds < os {
			return -1
		}
		return 1
	}
	if ds == 0 {
		return 0
	}
	// Fast path, no allocation: both magnitudes fit a uint64 and the
	// rescaled one still does (checked with a 128-bit product). This is
	// every money-sized comparison — a numeric(10,2) column against a
	// literal of another scale on every row of a scan — which otherwise
	// allocated twice per row in align.
	if d.coef.IsInt64() && o.coef.IsInt64() {
		if c, ok := cmpSmall(absU64(d.coef.Int64()), d.scale, absU64(o.coef.Int64()), o.scale); ok {
			return c * ds // both have sign ds: a larger magnitude is smaller when negative
		}
	}
	ac, bc, _ := align(d, o)
	return ac.Cmp(bc)
}

// absU64 is |n| as a uint64 (exact for MinInt64 too).
func absU64(n int64) uint64 {
	if n < 0 {
		return uint64(-(n + 1)) + 1
	}
	return uint64(n)
}

// cmpSmall compares a×10^-as with b×10^-bs for magnitudes that fit a
// uint64. ok is false when bringing them to one scale would overflow.
func cmpSmall(a uint64, as int32, b uint64, bs int32) (int, bool) {
	if as < bs {
		var ok bool
		if a, ok = mulPow10(a, int(bs-as)); !ok {
			return 0, false
		}
	} else if bs < as {
		var ok bool
		if b, ok = mulPow10(b, int(as-bs)); !ok {
			return 0, false
		}
	}
	switch {
	case a < b:
		return -1, true
	case a > b:
		return 1, true
	}
	return 0, true
}

// mulPow10 is x×10^n when it fits a uint64.
func mulPow10(x uint64, n int) (uint64, bool) {
	if n >= len(pow10u64) {
		return 0, x == 0
	}
	hi, lo := bits.Mul64(x, pow10u64[n])
	return lo, hi == 0
}

// Equal reports value equality (Cmp == 0).
func (d Decimal) Equal(o Decimal) bool { return d.Cmp(o) == 0 }

// Normalize strips trailing fractional zeros: the scale drops to the
// smallest that still shows the value exactly (never below 0). Equal
// values normalize to identical (coefficient, scale) pairs, which is
// what hash and index keys need.
func (d Decimal) Normalize() Decimal {
	if d.scale == 0 {
		return d
	}
	if d.IsZero() {
		return Decimal{}
	}
	c := new(big.Int).Set(d.coef)
	s := d.scale
	r := new(big.Int)
	q := new(big.Int)
	for s > 0 {
		q.QuoRem(c, bigTen, r)
		if r.Sign() != 0 {
			break
		}
		c, q = q, c
		s--
	}
	return mk(c, s)
}

// Neg is -d, same scale.
func (d Decimal) Neg() Decimal {
	return mk(new(big.Int).Neg(d.c()), d.scale)
}

// Abs is |d|, same scale.
func (d Decimal) Abs() Decimal {
	if d.Sign() >= 0 {
		return d
	}
	return d.Neg()
}

// Add is d + o at scale max(sd, so).
func (d Decimal) Add(o Decimal) (Decimal, error) {
	ac, bc, s := align(d, o)
	return checked(mk(new(big.Int).Add(ac, bc), s))
}

// Sub is d - o at scale max(sd, so).
func (d Decimal) Sub(o Decimal) (Decimal, error) {
	ac, bc, s := align(d, o)
	return checked(mk(new(big.Int).Sub(ac, bc), s))
}

// Mul is d × o at scale sd + so. Postgres caps the product's scale at
// NUMERIC_DSCALE_MAX by rounding; the same happens here.
func (d Decimal) Mul(o Decimal) (Decimal, error) {
	p := mk(new(big.Int).Mul(d.c(), o.c()), d.scale+o.scale)
	if p.scale > MaxScale {
		p = p.Round(MaxScale)
	}
	return checked(p)
}

// checked enforces the type's limits on a computed value. It runs on
// every Add, so it avoids rendering the coefficient: a b-bit integer has
// at most ⌊b·log10(2)⌋+1 decimal digits, and while that bound minus the
// scale is within MaxIntDigits — every value short of 131072 integer
// digits — no exact count is needed.
func checked(d Decimal) (Decimal, error) {
	if d.scale > MaxScale {
		return Decimal{}, ErrOverflow
	}
	if bound := d.c().BitLen()*30103/100000 + 1 - int(d.scale); bound > MaxIntDigits &&
		d.IntDigits() > MaxIntDigits {
		return Decimal{}, ErrOverflow
	}
	return d, nil
}

// weightAndFirst returns Postgres's view of a nonzero value in base
// 10000 (NBASE): the weight of its most significant base-10000 digit
// (10000^weight ≤ |d| < 10000^(weight+1)) and that digit's value
// (1..9999). Zero is (0, 0). Division's result scale depends on both,
// so they must be computed exactly as Postgres's representation would
// hold them.
func (d Decimal) weightAndFirst() (weight, first int) {
	if d.IsZero() {
		return 0, 0
	}
	digits := new(big.Int).Abs(d.coef).Text(10)
	// e is the decimal exponent of the leading digit: |d| ∈ [10^e, 10^(e+1)).
	e := len(digits) - 1 - int(d.scale)
	weight = floorDiv(e, 4)
	// The leading base-10000 digit spans decimal exponents 4w..4w+3,
	// so it holds the first e-4w+1 decimal digits.
	n := e - 4*weight + 1
	first, _ = strconv.Atoi(digits[:min(n, len(digits))])
	for i := len(digits); i < n; i++ { // fewer digits than the group spans
		first *= 10
	}
	return weight, first
}

func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// DivScale is the result scale Postgres gives d / o (select_div_scale):
// enough digits that the quotient has at least 16 significant ones,
// never fewer than either operand's scale, at most 1000.
func DivScale(d, o Decimal) int32 {
	w1, f1 := d.weightAndFirst()
	w2, f2 := o.weightAndFirst()
	qweight := w1 - w2
	if f1 <= f2 {
		qweight--
	}
	rscale := minSigDigits - qweight*4
	rscale = max(rscale, int(d.scale), int(o.scale), 0)
	return int32(min(rscale, maxDisplayScale))
}

// Div is d / o at DivScale(d, o), rounded half away from zero.
func (d Decimal) Div(o Decimal) (Decimal, error) {
	if o.IsZero() {
		return Decimal{}, ErrDivByZero
	}
	return d.DivRound(o, DivScale(d, o))
}

// DivRound is d / o rounded half away from zero to scale (≥ 0).
func (d Decimal) DivRound(o Decimal, scale int32) (Decimal, error) {
	if o.IsZero() {
		return Decimal{}, ErrDivByZero
	}
	// d/o = (cd·10^-sd)/(co·10^-so). Scaled to the target:
	// q = cd·10^(scale - sd + so) / co.
	num := new(big.Int).Set(d.c())
	den := new(big.Int).Set(o.c())
	if shift := int(scale) - int(d.scale) + int(o.scale); shift >= 0 {
		num.Mul(num, pow10(shift))
	} else {
		den.Mul(den, pow10(-shift))
	}
	return checked(mk(quoRoundHalfAway(num, den), scale))
}

// quoRoundHalfAway is num/den rounded half away from zero.
func quoRoundHalfAway(num, den *big.Int) *big.Int {
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	if r.Sign() == 0 {
		return q
	}
	// Compare 2|r| with |den|: at or above, round away from zero.
	r2 := new(big.Int).Abs(r)
	r2.Lsh(r2, 1)
	if r2.Cmp(new(big.Int).Abs(den)) >= 0 {
		if (num.Sign() < 0) != (den.Sign() < 0) {
			q.Sub(q, bigOne)
		} else {
			q.Add(q, bigOne)
		}
	}
	return q
}

// Mod is Postgres's numeric modulo: d - o·trunc(d/o), at scale
// max(sd, so). The result has d's sign.
func (d Decimal) Mod(o Decimal) (Decimal, error) {
	if o.IsZero() {
		return Decimal{}, ErrDivByZero
	}
	ac, bc, s := align(d, o)
	return checked(mk(new(big.Int).Rem(ac, bc), s))
}

// Round rounds half away from zero to scale places after the point. A
// negative scale rounds to the left of the point (round(1250, -2) is
// 1300) and leaves scale 0, as Postgres does.
func (d Decimal) Round(scale int32) Decimal {
	return d.rescale(scale, true)
}

// Trunc truncates toward zero to scale places (negative as in Round).
func (d Decimal) Trunc(scale int32) Decimal {
	return d.rescale(scale, false)
}

func (d Decimal) rescale(scale int32, round bool) Decimal {
	target := max(scale, 0)
	if scale >= d.scale {
		// Widening: pad with zeros, exact.
		if target == d.scale {
			return d
		}
		return mk(new(big.Int).Mul(d.c(), pow10(int(target-d.scale))), target)
	}
	// Narrowing: drop (d.scale - scale) digits, rounding or truncating.
	div := pow10(int(d.scale - scale))
	var q *big.Int
	if round {
		q = quoRoundHalfAway(d.c(), div)
	} else {
		q = new(big.Int).Quo(d.c(), div)
	}
	if scale < 0 {
		// Rounded to tens, hundreds, ...: scale back up to an integer.
		q.Mul(q, pow10(int(-scale)))
	}
	return mk(q, target)
}

// Floor is the largest integer ≤ d, scale 0.
func (d Decimal) Floor() Decimal {
	t := d.Trunc(0)
	if d.Sign() < 0 && t.Cmp(d) != 0 {
		return mk(new(big.Int).Sub(t.c(), bigOne), 0)
	}
	return t
}

// Ceil is the smallest integer ≥ d, scale 0.
func (d Decimal) Ceil() Decimal {
	t := d.Trunc(0)
	if d.Sign() > 0 && t.Cmp(d) != 0 {
		return mk(new(big.Int).Add(t.c(), bigOne), 0)
	}
	return t
}

// FitTypmod applies a numeric(precision, scale) column's limits the way
// Postgres's apply_typmod does: round half away from zero to scale, then
// refuse a value whose integer part needs more than precision - scale
// digits. The returned error carries Postgres's message text.
//
// The test is on the decimal exponent of the leading digit, not on a
// count of integer digits, because Postgres 15+ allows a scale above
// the precision: numeric(3,5) holds 0.00123 (leading digit at 10^-3,
// so "-2 digits before the point", within 3 - 5 = -2) but not 0.0123.
// A negative scale (numeric(2,-3)) rounds to thousands and allows
// 2 - (-3) = 5 integer digits.
func (d Decimal) FitTypmod(precision, scale int32) (Decimal, error) {
	r := d.Round(scale)
	if !r.IsZero() && r.lead() > int(precision-scale) {
		return Decimal{}, &TypmodError{Precision: precision, Scale: scale}
	}
	return r, nil
}

// lead is the decimal exponent of the leading digit plus one: the
// count of digits before the point for |d| ≥ 1, and zero or negative
// below that (0.05 → -1). Postgres calls this ddigits in apply_typmod.
// Meaningless for zero.
func (d Decimal) lead() int {
	return len(new(big.Int).Abs(d.c()).Text(10)) - int(d.scale)
}

// TypmodError is "numeric field overflow" (SQLSTATE 22003).
type TypmodError struct{ Precision, Scale int32 }

func (e *TypmodError) Error() string { return "numeric field overflow" }

// Detail is Postgres's DETAIL line for the error.
func (e *TypmodError) Detail() string {
	return "A field with precision " + itoa(e.Precision) + ", scale " + itoa(e.Scale) +
		" must round to an absolute value less than " + pow10Text(e.Precision-e.Scale) + "."
}

// pow10Text renders 10^n as Postgres's DETAIL does: "10^3", but "1" for
// n = 0 and "10^-2" below that.
func pow10Text(n int32) string {
	if n == 0 {
		return "1"
	}
	return "10^" + itoa(n)
}

func itoa(n int32) string { return strconv.Itoa(int(n)) }

// IsInteger reports whether the value has no fractional part (at any
// scale: 2.00 is an integer).
func (d Decimal) IsInteger() bool {
	if d.scale == 0 {
		return true
	}
	return new(big.Int).Rem(d.c(), pow10(int(d.scale))).Sign() == 0
}

// Int64 converts to int64 the way Postgres's numeric → bigint cast
// does: round half away from zero, then range-check. ok is false when
// the rounded value does not fit.
func (d Decimal) Int64() (n int64, ok bool) {
	r := d.Round(0).c()
	if !r.IsInt64() {
		return 0, false
	}
	return r.Int64(), true
}

// Float64 is the nearest float64 (correctly rounded from the exact
// decimal text), as Postgres's numeric → float8 cast.
func (d Decimal) Float64() float64 {
	f, _ := strconv.ParseFloat(d.String(), 64)
	return f
}

// Mantissa returns the value in scientific form for order-preserving
// encodings: |d| = 0.digits × 10^exp, digits without leading or
// trailing zeros (so equal values give identical parts whatever their
// scale). Zero returns ("", 0).
func (d Decimal) Mantissa() (neg bool, digits string, exp int) {
	if d.IsZero() {
		return false, "", 0
	}
	full := new(big.Int).Abs(d.coef).Text(10)
	exp = len(full) - int(d.scale)
	return d.coef.Sign() < 0, strings.TrimRight(full, "0"), exp
}

// FromMantissaU64 is FromMantissa for a mantissa already parsed into
// a uint64: ±0.<mant's nDigits digits> × 10^exp, nDigits counting any
// leading zeros a digit string would have had. It saves decoders the
// digit string altogether for the common case (up to 19 digits).
func FromMantissaU64(neg bool, mant uint64, nDigits, exp int, scale int32) (Decimal, error) {
	if mant == 0 {
		return Decimal{scale: scale}, nil
	}
	own := nDigits - exp
	coef := new(big.Int).SetUint64(mant)
	if own < 0 {
		coef.Mul(coef, pow10(-own))
		own = 0
	}
	if own > MaxScale || exp > MaxIntDigits {
		return Decimal{}, ErrOverflow
	}
	if int(scale) > own {
		coef.Mul(coef, pow10(int(scale)-own))
		own = int(scale)
	}
	if neg {
		coef.Neg(coef)
	}
	return mk(coef, int32(own)), nil
}

// FromMantissa is Mantissa's inverse: ±0.digits × 10^exp, displayed at
// scale (which must be at least the value's own minimal scale; a
// smaller one is raised to it, never rounded). digits "" is zero.
func FromMantissa(neg bool, digits string, exp int, scale int32) (Decimal, error) {
	if digits == "" {
		return Decimal{scale: scale}, nil
	}
	var coef *big.Int
	if len(digits) <= 19 {
		// Up to 19 digits fit a uint64: accumulate directly. SetString
		// builds a scanner per call, which dominated decoding numeric
		// rows (one per row per column).
		var n uint64
		for i := 0; i < len(digits); i++ {
			c := digits[i]
			if c < '0' || c > '9' {
				return Decimal{}, ErrSyntax
			}
			n = n*10 + uint64(c-'0')
		}
		coef = new(big.Int).SetUint64(n)
	} else {
		var ok bool
		if coef, ok = new(big.Int).SetString(digits, 10); !ok {
			return Decimal{}, ErrSyntax
		}
	}
	// 0.digits × 10^exp = digits × 10^(exp - len(digits)).
	own := len(digits) - exp // the minimal scale; negative means trailing zeros
	if own < 0 {
		coef.Mul(coef, pow10(-own))
		own = 0
	}
	if own > MaxScale || exp > MaxIntDigits {
		return Decimal{}, ErrOverflow
	}
	if int(scale) > own {
		coef.Mul(coef, pow10(int(scale)-own))
		own = int(scale)
	}
	if neg {
		coef.Neg(coef)
	}
	return mk(coef, int32(own)), nil
}

// Sum is a mutable running total for aggregates. Decimal is immutable,
// so summing a column with Add allocates a new value per row; a Sum
// owns its coefficient and adds into it in place, which for inputs of
// one scale (any numeric(p, s) column) reuses the same words row after
// row. The zero value is 0. Not safe for concurrent use; Value returns
// an independent Decimal.
type Sum struct {
	coef  big.Int
	scale int32
	tmp   big.Int // rescaled operand, reused
}

// Add adds d to the total at scale max(total's, d's), as Decimal.Add.
func (s *Sum) Add(d Decimal) error {
	c := d.c()
	switch {
	case d.scale > s.scale:
		// Widen the total to d's scale first, in place.
		s.coef.Mul(&s.coef, pow10(int(d.scale-s.scale)))
		s.scale = d.scale
		s.coef.Add(&s.coef, c)
	case d.scale < s.scale:
		s.tmp.Mul(c, pow10(int(s.scale-d.scale)))
		s.coef.Add(&s.coef, &s.tmp)
	default:
		s.coef.Add(&s.coef, c)
	}
	if bound := s.coef.BitLen()*30103/100000 + 1 - int(s.scale); bound > MaxIntDigits {
		if _, err := checked(s.Value()); err != nil {
			return err
		}
	}
	return nil
}

// AddInt64 adds an integer (scale 0).
func (s *Sum) AddInt64(n int64) error {
	s.tmp.SetInt64(n)
	if s.scale > 0 {
		s.tmp.Mul(&s.tmp, pow10(int(s.scale)))
	}
	s.coef.Add(&s.coef, &s.tmp)
	return nil
}

// Value is the total as an independent Decimal.
func (s *Sum) Value() Decimal {
	return mk(new(big.Int).Set(&s.coef), s.scale)
}
