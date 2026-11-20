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
// # Special values (N-051)
//
// Besides finite numbers a Decimal can be NaN, +Infinity or -Infinity,
// as Postgres 14+'s numeric can. They read and print as Postgres spells
// them ("NaN", "Infinity", "-Infinity"; Parse also takes "inf" and any
// case, and a sign on the infinities but not on NaN), and every
// operation follows numeric.c's rules for them:
//
//	order        -Infinity < every finite value < +Infinity < NaN, NaN = NaN
//	x ± y        NaN in, NaN out; Inf + -Inf and Inf - Inf are NaN;
//	             otherwise an infinite operand wins
//	x × y        NaN in, NaN out; Inf × 0 is NaN; otherwise ±Inf by signs
//	x / y        NaN in, NaN out (even NaN / 0); Inf / Inf is NaN;
//	             Inf / 0 is division by zero; finite / Inf is 0
//	x % y        NaN in, NaN out; Inf % 0 is division by zero;
//	             Inf % finite is NaN; finite % Inf is the finite one
//	round etc.   a special value is returned unchanged
//	typmod       NaN fits any numeric(p, s); ±Infinity fits none
//
// They live in the kind field, which is zero (finite) for every value
// that existed before, so the zero value is still 0 and nothing about
// the finite representation below changed. A special value has its
// other fields zeroed, so the representation stays canonical: all NaNs
// are one struct, as are each infinity's.
//
// Each operation checks kind first and hands any special operand to a
// small function that applies the rules above; the finite fast paths
// behind that check are untouched. The check is one byte compare (both
// operands' kinds OR-ed), so the money-sized paths N-050 tuned pay
// nothing measurable for it.
//
// A Decimal is immutable. Every operation returns a new value and never
// writes through the *big.Int it holds, so values can be shared freely
// across rows, goroutines and cached plans. The zero value is 0 with
// scale 0.
//
// # Two coefficient forms (N-050)
//
// The coefficient lives inline as an int64 while it fits, and in a
// *big.Int only beyond that:
//
//	small   wide == nil, coefficient = small,  |small| ≤ MaxInt64
//	wide    wide != nil, coefficient = *wide,  |*wide| > MaxInt64
//
// Money-sized values (anything with at most 18 significant digits) are
// therefore one machine word: decoding a row, comparing, adding or
// multiplying two of them allocates nothing. A TPC-H Q1-shaped query
// (products of numeric(15,2) columns summed per group,
// sql/numeric_workload_bench_test.go) allocated ~50 objects a row with
// every coefficient a *big.Int, against ~17 for the same table in
// float8; the inline form is what closes most of that gap.
//
// Every operation tries the small form first, with explicit overflow
// checks (128-bit products from math/bits), and falls back to the
// *big.Int code, which is the original implementation and still handles
// any value. The forms are canonical: mk demotes a *big.Int that fits,
// so a value has exactly one representation and Decimals compare equal
// structurally (reflect.DeepEqual) exactly when they are equal digit
// for digit and scale for scale. MinInt64 is kept wide so that negation
// and |x| never overflow in the small paths.
//
// The wide path accepts non-canonical input (a wide value that would
// fit), which the tests use as an oracle: each operation run on the
// forced-wide form of its operands must give the same result as the
// small path (TestSmallMatchesWide, FuzzSmallMatchesWide).
package decimal

import (
	"bytes"
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
	// ErrSpecial refused NaN and ±Infinity before bytdb's numeric could
	// represent them.
	//
	// Deprecated: since N-051 they are values (see NaN and Inf), and
	// nothing in this package returns ErrSpecial. It stays exported so
	// code that matches it still compiles.
	ErrSpecial = errors.New("numeric NaN and Infinity are not supported")
	// ErrOverflow is Postgres's "value overflows numeric format": more
	// integer digits or a larger scale than the type can hold.
	ErrOverflow = errors.New("value overflows numeric format")
	// ErrDivByZero is division (or modulo) by zero.
	ErrDivByZero = errors.New("division by zero")
)

// Decimal is an exact decimal number, or NaN or ±Infinity; see the
// package doc.
//
// 24 bytes: the int64 coefficient sits beside the pointer rather than
// sharing storage with it (an unsafe union would save 8 bytes but make
// the GC's view of the pointer word depend on a tag), so boxing a
// Decimal in an interface is still a single small allocation. kind
// fits in the padding after scale, so N-051 did not grow the struct.
type Decimal struct {
	wide  *big.Int // the coefficient when it does not fit small; never mutated once the value exists
	small int64    // the coefficient when wide is nil; never MinInt64
	scale int32    // digits after the point; 0 ≤ scale ≤ MaxScale
	kind  kind     // finite, or one of the special values (then the fields above are all zero)
}

// kind tells a finite value from Postgres's three special numerics.
// finite is the zero value, so Decimal{} is 0 and every finite value
// built before N-051 keeps its meaning.
type kind uint8

const (
	finite kind = iota
	kindNaN
	kindPosInf
	kindNegInf
)

// rank orders the kinds as Postgres orders the values (cmp_numerics):
// -Infinity below every finite value, +Infinity above, and NaN above
// everything, equal only to itself.
func (k kind) rank() int {
	switch k {
	case kindNegInf:
		return 0
	case finite:
		return 1
	case kindPosInf:
		return 2
	}
	return 3 // kindNaN
}

// NaN returns numeric NaN: not a number, but in Postgres's numeric
// equal to itself and greater than every other value, so it sorts,
// groups and indexes like an ordinary value.
func NaN() Decimal { return Decimal{kind: kindNaN} }

// Inf returns +Infinity when sign ≥ 0 and -Infinity when sign < 0, as
// math.Inf does.
func Inf(sign int) Decimal {
	if sign < 0 {
		return Decimal{kind: kindNegInf}
	}
	return Decimal{kind: kindPosInf}
}

// IsNaN reports whether d is NaN.
func (d Decimal) IsNaN() bool { return d.kind == kindNaN }

// IsInf reports whether d is an infinity, as math.IsInf does: +Infinity
// when sign > 0, -Infinity when sign < 0, either when sign is 0.
func (d Decimal) IsInf(sign int) bool {
	switch {
	case sign > 0:
		return d.kind == kindPosInf
	case sign < 0:
		return d.kind == kindNegInf
	}
	return d.kind == kindPosInf || d.kind == kindNegInf
}

// IsFinite reports whether d is a number: not NaN and not infinite.
// The accessors that only make sense for a number (Coef, Scale,
// Mantissa, IntDigits) treat a special value as 0; callers that
// serialize a value check IsFinite first.
func (d Decimal) IsFinite() bool { return d.kind == finite }

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

// mk builds a Decimal from a *big.Int coefficient it takes ownership of
// (nil means 0). Every wide-path result goes through it, which keeps
// the representation canonical (see the package doc): a coefficient
// that fits the small form is demoted to it, so zero is always the
// all-zero struct plus its scale, and equal (digits, scale) pairs are
// equal structs (reflect.DeepEqual in tests, %#v in logs).
func mk(coef *big.Int, scale int32) Decimal {
	if coef == nil {
		return Decimal{scale: scale}
	}
	if coef.IsInt64() {
		if n := coef.Int64(); n != math.MinInt64 {
			return Decimal{small: n, scale: scale}
		}
	}
	return Decimal{wide: coef, scale: scale}
}

// mkSmall builds a Decimal from an int64 coefficient. MinInt64 is the
// one int64 the small form excludes (its magnitude has no int64), so it
// goes wide; the small-path arithmetic below can produce it only at
// the very edge (e.g. -MaxInt64 - 1).
func mkSmall(n int64, scale int32) Decimal {
	if n == math.MinInt64 {
		return Decimal{wide: big.NewInt(n), scale: scale}
	}
	return Decimal{small: n, scale: scale}
}

// isSmall reports whether the coefficient is held inline.
func (d Decimal) isSmall() bool { return d.wide == nil }

// c returns the coefficient as a *big.Int for the wide paths,
// materializing the small form (an allocation; the small paths never
// call it). The result must not be mutated.
func (d Decimal) c() *big.Int {
	if d.wide != nil {
		return d.wide
	}
	return big.NewInt(d.small)
}

// New returns coef × 10^-scale. scale must be in [0, MaxScale]; New
// copies coef (or reads it into the small form), so the caller may
// reuse it.
func New(coef *big.Int, scale int32) Decimal {
	if coef.IsInt64() {
		return mkSmall(coef.Int64(), scale)
	}
	return mk(new(big.Int).Set(coef), scale)
}

// FromInt64 returns n with scale 0.
func FromInt64(n int64) Decimal {
	return mkSmall(n, 0)
}

// FromFloat64 converts a float64. digits is the number of significant
// digits kept: 15 reproduces Postgres's float8 → numeric cast (it prints
// the float with DBL_DIG digits first, so 0.1::float8::numeric is 0.1),
// and -1 keeps the shortest text that round-trips to the same float,
// which is what a Go caller handing over a float64 means. NaN and ±Inf
// become numeric NaN and ±Infinity, as Postgres 14+'s float8 → numeric
// cast does.
func FromFloat64(f float64, digits int) (Decimal, error) {
	switch {
	case math.IsNaN(f):
		return NaN(), nil
	case math.IsInf(f, 1):
		return Inf(1), nil
	case math.IsInf(f, -1):
		return Inf(-1), nil
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
//
// The special values are spelled as numeric_in reads them, in any case:
// "NaN" (unsigned only: "-NaN" is a syntax error), and "Infinity" or
// "inf" with an optional sign.
func Parse(s string) (Decimal, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Decimal{}, ErrSyntax
	}
	neg, signed := false, false
	switch s[0] {
	case '-':
		neg, signed = true, true
		s = s[1:]
	case '+':
		signed = true
		s = s[1:]
	}
	// Every number has a digit or a point right after the sign, so
	// anything else is a special value or a syntax error (numeric_in
	// makes the same split). Testing the first byte keeps the special
	// words off the per-literal path entirely.
	if s != "" && !isDigit(s[0]) && s[0] != '.' {
		return parseSpecial(s, neg, signed)
	}
	// The digits accumulate into acc while the number stays within an
	// int64 (the small form); only longer input pays for a digit string
	// and big.Int.SetString, rebuilt from s after the scan. Parse runs
	// on every numeric literal and every text-format wire parameter.
	var acc uint64
	accFits := true
	frac, sawDigit, sawPoint := 0, false, false
	i := 0
scan:
	for ; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= '0' && ch <= '9':
			if acc <= (math.MaxInt64-9)/10 {
				acc = acc*10 + uint64(ch-'0')
			} else {
				accFits = false
			}
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
	// value = coef × 10^(exp - frac). A positive net exponent folds into
	// the coefficient (scale 0); a negative one is the scale.
	net := exp - int64(frac)
	if accFits && net <= 0 && -net <= MaxScale {
		// Small form: at most 18 significant digits, so no integer-digit
		// limit can be in play either.
		n := int64(acc)
		if neg {
			n = -n
		}
		return Decimal{small: n, scale: int32(-net)}, nil
	}
	var digits strings.Builder
	for j := 0; j < i; j++ {
		if isDigit(s[j]) {
			digits.WriteByte(s[j])
		}
	}
	coef, ok := new(big.Int).SetString(digits.String(), 10)
	if !ok {
		return Decimal{}, ErrSyntax
	}
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

// parseSpecial reads the text after an optional sign as one of the
// special values. NaN takes no sign; the infinities take either.
// Anything else is ErrSyntax, including internal whitespace ("- inf"),
// which numeric_in refuses too.
func parseSpecial(s string, neg, signed bool) (Decimal, error) {
	switch {
	case !signed && strings.EqualFold(s, "nan"):
		return NaN(), nil
	case strings.EqualFold(s, "infinity"), strings.EqualFold(s, "inf"):
		if neg {
			return Inf(-1), nil
		}
		return Inf(1), nil
	}
	return Decimal{}, ErrSyntax
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
// negative value and no sign for zero. The special values are "NaN",
// "Infinity" and "-Infinity".
func (d Decimal) String() string {
	return string(d.AppendText(nil))
}

// AppendText appends String's form to buf.
func (d Decimal) AppendText(buf []byte) []byte {
	switch d.kind {
	case kindNaN:
		return append(buf, "NaN"...)
	case kindPosInf:
		return append(buf, "Infinity"...)
	case kindNegInf:
		return append(buf, "-Infinity"...)
	}
	if d.isSmall() {
		// The magnitude's digits go to a stack array: no allocation
		// beyond buf's own growth. This is every numeric a text-format
		// wire response renders.
		if d.small < 0 {
			buf = append(buf, '-')
		}
		var a [20]byte
		return appendPointed(buf, strconv.AppendUint(a[:0], absU64(d.small), 10), int(d.scale))
	}
	c := d.wide
	if c.Sign() < 0 {
		buf = append(buf, '-')
	}
	return appendPointed(buf, new(big.Int).Abs(c).Text(10), int(d.scale))
}

// appendPointed appends a magnitude's decimal digits with the point
// placed sc digits from the right, zero-padded on the left as needed
// ("5" at scale 3 is "0.005"). Generic over the two digit sources: the
// small form's stack bytes and the wide form's Text string.
func appendPointed[D []byte | string](buf []byte, digits D, sc int) []byte {
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

// Scale is the display scale: digits after the point. It is 0 for the
// special values, which is the dscale Postgres stores for them (its
// SQL scale() returns NULL for them instead; the SQL layer does that).
func (d Decimal) Scale() int32 { return d.scale }

// Coef returns a copy of the coefficient: the value is Coef × 10^-Scale.
// A special value has none and returns 0; check IsFinite first.
func (d Decimal) Coef() *big.Int {
	if d.isSmall() {
		return big.NewInt(d.small)
	}
	return new(big.Int).Set(d.wide)
}

// Sign is -1, 0 or +1: +1 for +Infinity and -1 for -Infinity. NaN has
// no sign and reports 0 (numeric.c never asks; check IsNaN first where
// it matters, since 0 here does not mean IsZero).
func (d Decimal) Sign() int {
	if d.wide != nil {
		return d.wide.Sign()
	}
	switch {
	case d.small < 0, d.kind == kindNegInf:
		return -1
	case d.small > 0, d.kind == kindPosInf:
		return 1
	}
	return 0
}

// IsZero reports whether the value is 0 (at any scale). The special
// values are not zero, although their coefficient fields are.
func (d Decimal) IsZero() bool { return d.wide == nil && d.small == 0 && d.kind == finite }

// coefDigits is the number of decimal digits in |coefficient| ("0" has
// one), without rendering it in the small form.
func (d Decimal) coefDigits() int {
	if d.isSmall() {
		return digitsU64(absU64(d.small))
	}
	return len(new(big.Int).Abs(d.wide).Text(10))
}

// digitsU64 is the number of decimal digits in x (1 for 0). The bit
// length gives log10 to within one: ⌊bits·log10(2)⌋ (1233/4096 ≈
// 0.30103) is either the digit count or one short, and one comparison
// against the table settles which.
func digitsU64(x uint64) int {
	if x == 0 {
		return 1
	}
	t := bits.Len64(x) * 1233 >> 12
	if t < len(pow10u64) && x >= pow10u64[t] {
		t++
	}
	return t
}

// IntDigits is the number of digits before the decimal point, 0 for a
// value whose magnitude is below 1 and for the special values.
func (d Decimal) IntDigits() int {
	n := d.coefDigits() - int(d.scale)
	if n < 0 || d.IsZero() || d.kind != finite {
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

// Cmp compares by value, ignoring scale: 1.0 and 1.00 are equal. The
// special values order as Postgres orders them: -Infinity below every
// number, +Infinity above, NaN above everything and equal to itself
// (so NaN groups, sorts and indexes like an ordinary value).
func (d Decimal) Cmp(o Decimal) int {
	if d.kind|o.kind != finite {
		// At least one is special: the kinds' ranks decide, and equal
		// ranks can only be the same special value twice.
		return cmpInt64(int64(d.kind.rank()), int64(o.kind.rank()))
	}
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
	// Fast path, no allocation: both coefficients are small and the
	// rescaled one still fits a uint64 (checked with a 128-bit
	// product). This is every money-sized comparison — a numeric(10,2)
	// column against a literal of another scale on every row of a scan —
	// which otherwise allocated twice per row in align.
	if d.isSmall() && o.isSmall() {
		if d.scale == o.scale {
			return cmpInt64(d.small, o.small)
		}
		if c, ok := cmpSmall(absU64(d.small), d.scale, absU64(o.small), o.scale); ok {
			return c * ds // both have sign ds: a larger magnitude is smaller when negative
		}
	}
	ac, bc, _ := align(d, o)
	return ac.Cmp(bc)
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
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
// what hash and index keys need. A special value is returned as is.
func (d Decimal) Normalize() Decimal {
	if d.scale == 0 {
		return d // also every special value, whose scale is 0
	}
	if d.IsZero() {
		return Decimal{}
	}
	if d.isSmall() {
		x, s := d.small, d.scale
		for s > 0 && x%10 == 0 {
			x /= 10
			s--
		}
		return Decimal{small: x, scale: s}
	}
	c := new(big.Int).Set(d.wide)
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

// Neg is -d, same scale. -NaN is NaN; the infinities swap.
func (d Decimal) Neg() Decimal {
	switch d.kind {
	case kindNaN:
		return d
	case kindPosInf:
		return Inf(-1)
	case kindNegInf:
		return Inf(1)
	}
	if d.isSmall() {
		return Decimal{small: -d.small, scale: d.scale} // never MinInt64, so never overflows
	}
	return mk(new(big.Int).Neg(d.wide), d.scale)
}

// Abs is |d|, same scale. |NaN| is NaN and |±Infinity| is +Infinity
// (NaN's Sign is 0 and Neg turns -Infinity around, so the general
// rule covers both).
func (d Decimal) Abs() Decimal {
	if d.Sign() >= 0 {
		return d
	}
	return d.Neg()
}

// alignSmall is align for two small values: both coefficients at the
// larger scale, as int64s. ok is false when rescaling would leave the
// int64 range; the caller then takes the wide path.
func alignSmall(a, b Decimal) (x, y int64, scale int32, ok bool) {
	x, y = a.small, b.small
	switch {
	case a.scale < b.scale:
		x, ok = scaleUp(x, b.scale-a.scale)
		return x, y, b.scale, ok
	case b.scale < a.scale:
		y, ok = scaleUp(y, a.scale-b.scale)
		return x, y, a.scale, ok
	}
	return x, y, a.scale, true
}

// scaleUp is n×10^k when the result is a small coefficient.
func scaleUp(n int64, k int32) (int64, bool) {
	if n == 0 {
		return 0, true
	}
	if k >= int32(len(pow10u64)) {
		return 0, false // |n| ≥ 1, so the product exceeds 10^19 > MaxInt64
	}
	u, ok := mulPow10(absU64(n), int(k))
	if !ok || u > math.MaxInt64 {
		return 0, false
	}
	if n < 0 {
		return -int64(u), true
	}
	return int64(u), true
}

// Add is d + o at scale max(sd, so).
func (d Decimal) Add(o Decimal) (Decimal, error) {
	if d.kind|o.kind != finite {
		return addSpecial(d, o), nil
	}
	if d.isSmall() && o.isSmall() {
		if x, y, s, ok := alignSmall(d, o); ok {
			// Signed overflow: the sum's sign differs from both operands'.
			if sum := x + y; (x^sum)&(y^sum) >= 0 {
				return checked(mkSmall(sum, s))
			}
		}
	}
	ac, bc, s := align(d, o)
	return checked(mk(new(big.Int).Add(ac, bc), s))
}

// Sub is d - o at scale max(sd, so).
func (d Decimal) Sub(o Decimal) (Decimal, error) {
	if d.kind|o.kind != finite {
		// d - o = d + (-o) holds for every special case numeric_sub
		// lists: Inf - Inf = Inf + -Inf = NaN, finite - Inf = -Inf, and
		// so on.
		return addSpecial(d, o.Neg()), nil
	}
	if d.isSmall() && o.isSmall() {
		if x, y, s, ok := alignSmall(d, o); ok {
			// Signed overflow: the operands' signs differ and the
			// difference's sign differs from the minuend's.
			if diff := x - y; (x^y)&(x^diff) >= 0 {
				return checked(mkSmall(diff, s))
			}
		}
	}
	ac, bc, s := align(d, o)
	return checked(mk(new(big.Int).Sub(ac, bc), s))
}

// addSpecial is numeric_add's special-value branch: at least one of d
// and o is NaN or infinite.
//
//	        | -Inf   finite  +Inf   NaN
//	  -Inf  | -Inf   -Inf    NaN    NaN
//	finite  | -Inf     —     +Inf   NaN
//	  +Inf  |  NaN   +Inf    +Inf   NaN
//	   NaN  |  NaN   NaN     NaN    NaN
func addSpecial(d, o Decimal) Decimal {
	switch {
	case d.kind == kindNaN || o.kind == kindNaN:
		return NaN()
	case d.kind == finite:
		return o
	case o.kind == finite || o.kind == d.kind:
		return d
	}
	return NaN() // opposite infinities
}

// Mul is d × o at scale sd + so. Postgres caps the product's scale at
// NUMERIC_DSCALE_MAX by rounding; the same happens here.
func (d Decimal) Mul(o Decimal) (Decimal, error) {
	if d.kind|o.kind != finite {
		return mulSpecial(d, o), nil
	}
	if d.isSmall() && o.isSmall() && d.scale+o.scale <= MaxScale {
		// The magnitudes' full 128-bit product; small when the high
		// word is empty and the low one fits an int64.
		if hi, lo := bits.Mul64(absU64(d.small), absU64(o.small)); hi == 0 && lo <= math.MaxInt64 {
			n := int64(lo)
			if (d.small < 0) != (o.small < 0) {
				n = -n
			}
			return mkSmall(n, d.scale+o.scale), nil
		}
	}
	p := mk(new(big.Int).Mul(d.c(), o.c()), d.scale+o.scale)
	if p.scale > MaxScale {
		p = p.Round(MaxScale)
	}
	return checked(p)
}

// mulSpecial is numeric_mul's special-value branch: NaN in, NaN out;
// otherwise at least one operand is infinite, and the product is the
// infinity with the product of the signs, except that ∞ × 0 is NaN.
// (An infinity's Sign is ±1, so one multiplication covers all four
// sign pairs numeric.c spells out.)
func mulSpecial(d, o Decimal) Decimal {
	if d.kind == kindNaN || o.kind == kindNaN {
		return NaN()
	}
	s := d.Sign() * o.Sign()
	if s == 0 {
		return NaN()
	}
	return Inf(s)
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
	if d.isSmall() {
		return d, nil // at most 19 digits: far inside MaxIntDigits
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
	if d.isSmall() {
		// Same arithmetic on the magnitude itself: the leading n digits
		// are |coef| / 10^(len-n), padded with zeros when len < n.
		u := absU64(d.small)
		nd := digitsU64(u)
		e := nd - 1 - int(d.scale)
		weight = floorDiv(e, 4)
		n := e - 4*weight + 1 // 1..4
		if n <= nd {
			return weight, int(u / pow10u64[nd-n])
		}
		return weight, int(u * pow10u64[n-nd])
	}
	digits := new(big.Int).Abs(d.wide).Text(10)
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
// never fewer than either operand's scale, at most 1000. It is 0 when
// either operand is special: Div's result is then special or a plain 0.
func DivScale(d, o Decimal) int32 {
	if d.kind|o.kind != finite {
		return 0
	}
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
	if d.kind|o.kind != finite {
		// Before the zero check: NaN / 0 is NaN, not an error.
		return divSpecial(d, o, 0)
	}
	if o.IsZero() {
		return Decimal{}, ErrDivByZero
	}
	return d.DivRound(o, DivScale(d, o))
}

// DivRound is d / o rounded half away from zero to scale (≥ 0).
// Special operands follow Div's rules; a finite d over an infinite o
// is 0 at scale.
func (d Decimal) DivRound(o Decimal, scale int32) (Decimal, error) {
	if d.kind|o.kind != finite {
		return divSpecial(d, o, scale)
	}
	if o.IsZero() {
		return Decimal{}, ErrDivByZero
	}
	// d/o = (cd·10^-sd)/(co·10^-so). Scaled to the target:
	// q = cd·10^(scale - sd + so) / co.
	shift := int(scale) - int(d.scale) + int(o.scale)
	if d.isSmall() && o.isSmall() && scale <= MaxScale {
		if q, ok := divRoundSmall(d.small, o.small, shift); ok {
			return mkSmall(q, scale), nil
		}
	}
	num := new(big.Int).Set(d.c())
	den := new(big.Int).Set(o.c())
	if shift := int(scale) - int(d.scale) + int(o.scale); shift >= 0 {
		num.Mul(num, pow10(shift))
	} else {
		den.Mul(den, pow10(-shift))
	}
	return checked(mk(quoRoundHalfAway(num, den), scale))
}

// divRoundSmall is n·10^shift / m rounded half away from zero (shift
// may be negative: the power then multiplies m), for small n and
// nonzero m. The scaled dividend is built as a 128-bit product so that
// avg-style quotients — a sum of cents divided by a count at ~12
// digits of scale — stay on this path. ok is false when a scaled
// operand or the quotient leaves the 64-bit range.
//
//	shift ≥ 0:  (hi:lo) = |n|·10^shift,  q = (hi:lo) / |m|   needs hi < |m|
//	shift < 0:  q = |n| / (|m|·10^-shift)                     needs the product in 64 bits
func divRoundSmall(n, m int64, shift int) (int64, bool) {
	un, den := absU64(n), absU64(m)
	var q, r uint64
	if shift >= 0 {
		if shift >= len(pow10u64) {
			return 0, false
		}
		hi, lo := bits.Mul64(un, pow10u64[shift])
		if hi >= den {
			return 0, false // the quotient would not fit 64 bits (Div64 panics)
		}
		q, r = bits.Div64(hi, lo, den)
	} else {
		var ok bool
		if den, ok = mulPow10(den, -shift); !ok {
			return 0, false
		}
		q, r = un/den, un%den
	}
	// Half away from zero on magnitudes: round up when 2r ≥ den,
	// written r ≥ den-r so that 2r cannot overflow.
	if r >= den-r {
		q++
	}
	if q > math.MaxInt64 {
		return 0, false
	}
	if (n < 0) != (m < 0) {
		return -int64(q), true
	}
	return int64(q), true
}

// divSpecial is numeric_div's special-value branch: at least one of d
// and o is NaN or infinite.
//
//	NaN / anything, anything / NaN   NaN (NaN / 0 included: no error)
//	±Inf / ±Inf                      NaN
//	±Inf / 0                         division by zero
//	±Inf / nonzero finite            the infinity with the signs' product
//	finite / ±Inf                    0, at zeroScale
//
// Postgres returns a scale-0 zero for the last case (its const_zero);
// Div passes 0 for that, and DivRound its own target scale.
func divSpecial(d, o Decimal, zeroScale int32) (Decimal, error) {
	switch {
	case d.kind == kindNaN || o.kind == kindNaN:
		return NaN(), nil
	case d.kind == finite:
		return Decimal{scale: zeroScale}, nil // o is infinite
	case o.kind != finite:
		return NaN(), nil // Inf / Inf
	case o.IsZero():
		return Decimal{}, ErrDivByZero
	}
	return Inf(d.Sign() * o.Sign()), nil
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
//
// Special operands (numeric_mod): NaN in, NaN out, even NaN % 0;
// ±Inf % 0 is division by zero and ±Inf % anything else is NaN; a
// finite d % ±Inf is d itself.
func (d Decimal) Mod(o Decimal) (Decimal, error) {
	if d.kind|o.kind != finite {
		switch {
		case d.kind == kindNaN || o.kind == kindNaN:
			return NaN(), nil
		case d.kind == finite:
			return d, nil // o is infinite
		case o.IsZero():
			return Decimal{}, ErrDivByZero
		}
		return NaN(), nil
	}
	if o.IsZero() {
		return Decimal{}, ErrDivByZero
	}
	if d.isSmall() && o.isSmall() {
		if x, y, s, ok := alignSmall(d, o); ok {
			return checked(mkSmall(x%y, s)) // Go's % truncates, like big.Int.Rem: the dividend's sign
		}
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

// rescale is Round and Trunc; through them also Floor, Ceil, Int64 and
// FitTypmod. A special value has no digits to round and comes back
// unchanged, as from Postgres's numeric_round / numeric_trunc.
func (d Decimal) rescale(scale int32, round bool) Decimal {
	if d.kind != finite {
		return d
	}
	target := max(scale, 0)
	if d.isSmall() {
		if r, ok := d.rescaleSmall(scale, target, round); ok {
			return r
		}
	}
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

// rescaleSmall is rescale for a small coefficient; ok is false when an
// intermediate leaves the int64 range (only widening, or rounding to
// the left of the point, can).
func (d Decimal) rescaleSmall(scale, target int32, round bool) (Decimal, bool) {
	if scale >= d.scale {
		if target == d.scale {
			return d, true
		}
		x, ok := scaleUp(d.small, target-d.scale)
		return Decimal{small: x, scale: target}, ok
	}
	// Narrowing by k digits. With |coef| < 2^63 < 10^19, k ≥ 20 leaves
	// a quotient of 0 and a remainder below half of 10^k, so the result
	// is 0 whether rounding or truncating. k = 19 still divides (the
	// remainder can reach half of 10^19).
	k := int(d.scale - scale)
	if k >= len(pow10u64) {
		return Decimal{scale: target}, true
	}
	u := absU64(d.small)
	p := pow10u64[k]
	q, r := u/p, u%p
	if round && r >= p-r { // 2r ≥ p without overflowing
		q++
	}
	// q ≤ |coef|/10 + 1 here, so it fits an int64.
	x := int64(q)
	if d.small < 0 {
		x = -x
	}
	if scale < 0 {
		// Rounded to tens, hundreds, ...: scale back up to an integer.
		var ok bool
		if x, ok = scaleUp(x, -scale); !ok {
			return Decimal{}, false
		}
	}
	return Decimal{small: x, scale: target}, true
}

// Floor is the largest integer ≤ d, scale 0. A special value comes
// back unchanged.
func (d Decimal) Floor() Decimal {
	if d.kind != finite {
		return d
	}
	t := d.Trunc(0)
	if d.Sign() < 0 && t.Cmp(d) != 0 {
		if t.isSmall() {
			return mkSmall(t.small-1, 0) // t.small ≥ -MaxInt64: no wraparound
		}
		return mk(new(big.Int).Sub(t.c(), bigOne), 0)
	}
	return t
}

// Ceil is the smallest integer ≥ d, scale 0. A special value comes
// back unchanged.
func (d Decimal) Ceil() Decimal {
	if d.kind != finite {
		return d
	}
	t := d.Trunc(0)
	if d.Sign() > 0 && t.Cmp(d) != 0 {
		if t.isSmall() && t.small < math.MaxInt64 {
			return mkSmall(t.small+1, 0)
		}
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
//
// The special values follow apply_typmod_special: NaN fits every
// numeric(p, s), and ±Infinity fits none (TypmodError with Infinite
// set, which changes the DETAIL).
func (d Decimal) FitTypmod(precision, scale int32) (Decimal, error) {
	switch d.kind {
	case kindNaN:
		return d, nil
	case kindPosInf, kindNegInf:
		return Decimal{}, &TypmodError{Precision: precision, Scale: scale, Infinite: true}
	}
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
	return d.coefDigits() - int(d.scale)
}

// TypmodError is "numeric field overflow" (SQLSTATE 22003). Infinite
// marks an infinity offered to a constrained column, which Postgres
// refuses with its own DETAIL.
type TypmodError struct {
	Precision, Scale int32
	Infinite         bool
}

func (e *TypmodError) Error() string { return "numeric field overflow" }

// Detail is Postgres's DETAIL line for the error.
func (e *TypmodError) Detail() string {
	if e.Infinite {
		return "A field with precision " + itoa(e.Precision) + ", scale " + itoa(e.Scale) +
			" cannot hold an infinite value."
	}
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
// scale: 2.00 is an integer). NaN and the infinities are not integers.
// (numeric.c's numeric_is_integral counts the infinities as integral,
// but only power uses it, and Pow decides its special cases itself.)
func (d Decimal) IsInteger() bool {
	if d.kind != finite {
		return false
	}
	if d.scale == 0 {
		return true
	}
	if d.isSmall() {
		// |coef| < 10^19, so a scale of 19 or more leaves a fraction
		// unless the value is 0.
		if int(d.scale) >= len(pow10u64) {
			return d.small == 0
		}
		return absU64(d.small)%pow10u64[d.scale] == 0
	}
	return new(big.Int).Rem(d.c(), pow10(int(d.scale))).Sign() == 0
}

// Int64 converts to int64 the way Postgres's numeric → bigint cast
// does: round half away from zero, then range-check. ok is false when
// the rounded value does not fit, and for NaN and the infinities
// (whose SQL casts fail with their own messages; the caller words
// them).
func (d Decimal) Int64() (n int64, ok bool) {
	if d.kind != finite {
		return 0, false
	}
	rd := d.Round(0)
	if rd.isSmall() {
		return rd.small, true
	}
	r := rd.wide
	if !r.IsInt64() {
		return 0, false
	}
	return r.Int64(), true
}

// Float64 is the nearest float64 (correctly rounded from the exact
// decimal text), as Postgres's numeric → float8 cast. The special
// values become the float NaN and infinities.
func (d Decimal) Float64() float64 {
	switch d.kind {
	case kindNaN:
		return pgNaN
	case kindPosInf:
		return math.Inf(1)
	case kindNegInf:
		return math.Inf(-1)
	}
	// Exact fast path (the one strconv's own parser takes): a
	// coefficient below 2^53 and a power of ten up to 10^22 are both
	// exact float64s, and IEEE division rounds its exact quotient
	// correctly, so coef / 10^scale is the nearest float64.
	if d.isSmall() && d.scale <= 22 && absU64(d.small) <= 1<<53 {
		return float64(d.small) / float64pow10[d.scale]
	}
	f, _ := strconv.ParseFloat(d.String(), 64)
	return f
}

// pgNaN is the NaN Postgres produces (C's NAN: quiet, positive, empty
// payload), so numeric → float8 sends the same eight bytes in binary
// format as Postgres does and as a stored float8 NaN (package tuple's
// canonicalNaN). Go's math.NaN() sets the low payload bit instead.
var pgNaN = math.Float64frombits(0x7FF8000000000000)

// float64pow10 holds 10^0..10^22, the powers of ten a float64 holds exactly.
var float64pow10 = [...]float64{
	1e0, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10,
	1e11, 1e12, 1e13, 1e14, 1e15, 1e16, 1e17, 1e18, 1e19, 1e20, 1e21, 1e22,
}

// Mantissa returns the value in scientific form for order-preserving
// encodings: |d| = 0.digits × 10^exp, digits without leading or
// trailing zeros (so equal values give identical parts whatever their
// scale). Zero returns ("", 0), and so does a special value, which has
// no digits: an encoder checks IsFinite first and writes its own
// marker (package tuple has a class byte for each).
func (d Decimal) Mantissa() (neg bool, digits string, exp int) {
	if d.IsZero() || d.kind != finite {
		return false, "", 0
	}
	if d.isSmall() {
		// The digits come from the stack; only the trimmed string the
		// caller keeps is allocated.
		var a [20]byte
		full := strconv.AppendUint(a[:0], absU64(d.small), 10)
		exp = len(full) - int(d.scale)
		return d.small < 0, string(bytes.TrimRight(full, "0")), exp
	}
	full := new(big.Int).Abs(d.wide).Text(10)
	exp = len(full) - int(d.scale)
	return d.wide.Sign() < 0, strings.TrimRight(full, "0"), exp
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
	if d, ok := mantissaSmall(neg, mant, own, exp, scale); ok {
		return d, nil
	}
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

// mantissaSmall is the small-form body of FromMantissaU64: mant ×
// 10^-own, raised to scale, when every step stays in int64. own is the
// value's minimal scale (negative: that many trailing zeros). ok is
// false — the caller takes the wide path, which also reports the
// limits — for anything else. This is the per-row, per-column decode
// of every stored numeric up to 18 digits, so it must not allocate.
func mantissaSmall(neg bool, mant uint64, own, exp int, scale int32) (Decimal, bool) {
	if exp > MaxIntDigits || own > MaxScale {
		return Decimal{}, false
	}
	if own < 0 {
		// Trailing zeros fold into the coefficient (scale 0).
		if -own >= len(pow10u64) {
			return Decimal{}, false
		}
		var ok bool
		if mant, ok = mulPow10(mant, -own); !ok {
			return Decimal{}, false
		}
		own = 0
	}
	if int(scale) > own {
		// Displayed at a wider scale than the digits need: pad.
		if int(scale)-own >= len(pow10u64) {
			return Decimal{}, false
		}
		var ok bool
		if mant, ok = mulPow10(mant, int(scale)-own); !ok {
			return Decimal{}, false
		}
		own = int(scale)
	}
	if mant > math.MaxInt64 {
		return Decimal{}, false
	}
	n := int64(mant)
	if neg {
		n = -n
	}
	return Decimal{small: n, scale: int32(own)}, true
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
		if n != 0 {
			if d, ok := mantissaSmall(neg, n, len(digits)-exp, exp, scale); ok {
				return d, nil
			}
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
//
// Like Decimal, the total starts in an inline int64 (small) and moves
// to the big.Int (coef) the first time an addition would leave the
// int64 range, and stays there: a sum of 100k money values is ~10^12
// cents, so most totals never move.
//
// Special inputs (N-051) are not added in; they are only noted, the way
// Postgres's NumericAggState counts them beside its finite total. Value
// then applies the rules addition would: any NaN, or both infinities,
// make the total NaN, and otherwise a single kind of infinity wins.
// Counting rather than folding keeps the finite total free of special
// cases (and the per-row path free of anything but one kind test).
type Sum struct {
	small int64 // the total while wide is false; never MinInt64
	wide  bool  // the total lives in coef
	coef  big.Int
	scale int32
	tmp   big.Int // rescaled operand, reused

	sawNaN, sawPosInf, sawNegInf bool // special inputs seen; see Value
}

// promote moves the total into the big.Int form.
func (s *Sum) promote() {
	s.coef.SetInt64(s.small)
	s.small, s.wide = 0, true
}

// addSmall adds x×10^-xs to a small total, reporting false (total
// unchanged) when the result would leave the int64 range.
func (s *Sum) addSmall(x int64, xs int32) bool {
	a, b, sc, ok := alignSmall(Decimal{small: s.small, scale: s.scale}, Decimal{small: x, scale: xs})
	if !ok {
		return false
	}
	sum := a + b
	if (a^sum)&(b^sum) < 0 || sum == math.MinInt64 {
		return false
	}
	s.small, s.scale = sum, sc
	return true
}

// Add adds d to the total at scale max(total's, d's), as Decimal.Add.
// A special d is noted rather than added (see Sum).
func (s *Sum) Add(d Decimal) error {
	switch d.kind {
	case kindNaN:
		s.sawNaN = true
		return nil
	case kindPosInf:
		s.sawPosInf = true
		return nil
	case kindNegInf:
		s.sawNegInf = true
		return nil
	}
	if !s.wide {
		if d.isSmall() && s.addSmall(d.small, d.scale) {
			return nil
		}
		s.promote()
	}
	// The operand as a big.Int without allocating: a small d is read
	// into tmp, which the rescale below may then overwrite in place
	// (math/big allows the receiver to alias an operand).
	c := d.wide
	if c == nil {
		c = s.tmp.SetInt64(d.small)
	}
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
		// The finite part, not Value: a special input seen earlier must
		// not hide an overflow of the digits themselves.
		if _, err := checked(s.finiteValue()); err != nil {
			return err
		}
	}
	return nil
}

// AddInt64 adds an integer (scale 0).
func (s *Sum) AddInt64(n int64) error {
	if !s.wide {
		// n may be MinInt64, which the small form refuses (alignSmall
		// passes an unscaled operand through unchecked), so it goes
		// straight to the big.Int total.
		if n != math.MinInt64 && s.addSmall(n, 0) {
			return nil
		}
		s.promote()
	}
	s.tmp.SetInt64(n)
	if s.scale > 0 {
		s.tmp.Mul(&s.tmp, pow10(int(s.scale)))
	}
	s.coef.Add(&s.coef, &s.tmp)
	return nil
}

// Value is the total as an independent Decimal: NaN when a NaN or both
// infinities were added, the infinity when one kind of infinity was,
// and the finite total otherwise (numeric_sum / numeric_avg's order of
// tests).
func (s *Sum) Value() Decimal {
	switch {
	case s.sawNaN, s.sawPosInf && s.sawNegInf:
		return NaN()
	case s.sawPosInf:
		return Inf(1)
	case s.sawNegInf:
		return Inf(-1)
	}
	return s.finiteValue()
}

// finiteValue is the total of the finite inputs alone.
func (s *Sum) finiteValue() Decimal {
	if !s.wide {
		return Decimal{small: s.small, scale: s.scale}
	}
	return mk(new(big.Int).Set(&s.coef), s.scale)
}
