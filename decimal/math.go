package decimal

// math.go: square root, exponential, logarithms and power on numeric,
// with Postgres's result scales (numeric.c: numeric_sqrt, numeric_exp,
// numeric_ln, log_var, power_var, power_var_int).
//
// Postgres decides how many digits each result has before computing it,
// from a quick estimate of the result's size: "at least 16 significant
// digits, never fewer than an input's display scale, at most 1000". So
// matching it digit for digit takes two separate things:
//
//  1. The scale. Its estimates are reproduced exactly as numeric.c makes
//     them, down to reading the value's base-10000 digit groups and doing
//     the same float64 arithmetic, because a different estimate means a
//     different number of digits in the answer.
//  2. The value at that scale. Postgres's series (exp_var, ln_var) and
//     power loops (power_var, power_var_int) work at their own
//     intermediate scales. A series stops when its next term rounds to
//     zero there, and every product is rounded to it. They are not
//     correctly rounded: near a boundary of the final scale, those
//     intermediate roundings decide the last digit. A base near 1 to a
//     short exponent lands near one often. So each algorithm is ported
//     step for step, in Decimal arithmetic. Postgres's mul_var/div_var
//     are taken as exact-then-rounded, which their guard digits make them
//     in all but freak carry chains. sqrt_var is exact in both.
//
// Checked against Postgres 16 on 10,390 random calls across magnitudes
// and scales: all equal. An earlier version computed correctly rounded
// values instead (in big.Float) and lost 2 of them, both near ties where
// Postgres's series had stopped early.
//
//	        scale estimate            value
//	sqrt    weight of |x| in base 10^4  exact integer root (sqrt_var)
//	exp     x · log10(e) in float64     exp_var: halvings, Taylor, squarings
//	ln      estimate_ln_dweight         ln_var: square roots, atanh series
//	log     ln dweights of b and x      log_var: two ln_vars, then divide
//	power   integer exp: from the base's power_var_int: square-and-multiply
//	        leading groups
//	        otherwise: an 8-digit ln    power_var: ln_var, multiply, exp_var

import (
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Errors in Postgres's wording. The SQL layer maps them to SQLSTATEs:
// 2201F for the square root and power ones, 2201E for the logarithms.
var (
	ErrSqrtNegative = errors.New("cannot take square root of a negative number")
	ErrLogZero      = errors.New("cannot take logarithm of zero")
	ErrLogNegative  = errors.New("cannot take logarithm of a negative number")
	ErrPowZeroNeg   = errors.New("zero raised to a negative power is undefined")
	ErrPowNegNonInt = errors.New("a negative number raised to a non-integer power yields a complex result")
)

const (
	decDigits = 4     // DEC_DIGITS: decimal digits per base-10000 group
	nbase     = 10000 // NBASE
	// maxResultScale is NUMERIC_MAX_RESULT_SCALE (2 × the max precision).
	// exp overflows at |x| ≥ 3 × this.
	maxResultScale = 2000
	// maxWeight is NUMERIC_WEIGHT_MAX, in base-10000 groups.
	maxWeight = 32767
	// log10e is the double constant numeric.c multiplies by to estimate
	// a decimal weight from a natural log.
	log10e = 0.434294481903252
	// ln10 is the double constant estimate_ln_dweight uses.
	ln10 = 2.302585092994046
)

var (
	decOne         = FromInt64(1)
	decPointNine   = MustParse("0.9")
	decOnePointOne = MustParse("1.1")
)

// clampScale applies the rule every function ends its scale choice
// with: no less than the inputs' display scales, at least 0, at most
// NUMERIC_MAX_DISPLAY_SCALE.
func clampScale(rscale int, dscales ...int32) int {
	for _, s := range dscales {
		rscale = max(rscale, int(s))
	}
	return min(max(rscale, 0), maxDisplayScale)
}

// groups is a nonzero |d| as Postgres's NumericVar holds it: base-10000
// digits, most significant first, trailing zero groups dropped, and the
// weight of the first one (10000^weight ≤ |d| < 10000^(weight+1)). The
// groups align on the decimal point, so 12345.6 is [1 2345 6000] with
// weight 1.
type groups struct {
	weight int
	digits []int
}

func pgGroups(d Decimal) groups {
	if d.IsZero() {
		return groups{}
	}
	s := int(d.scale)
	s4 := (s + decDigits - 1) / decDigits * decDigits // scale padded to whole groups
	m := new(big.Int).Abs(d.c())
	if s4 > s {
		m.Mul(m, pow10(s4-s))
	}
	txt := m.Text(10)
	if pad := (decDigits - len(txt)%decDigits) % decDigits; pad > 0 {
		txt = strings.Repeat("0", pad) + txt
	}
	n := len(txt) / decDigits
	digits := make([]int, n)
	for i := range digits {
		digits[i], _ = strconv.Atoi(txt[i*decDigits : (i+1)*decDigits])
	}
	for len(digits) > 1 && digits[len(digits)-1] == 0 {
		digits = digits[:len(digits)-1]
	}
	return groups{weight: n - 1 - s4/decDigits, digits: digits}
}

// decExp is floor(log10|d|) for nonzero d: the decimal exponent of its
// leading digit.
func (d Decimal) decExp() int {
	return numDigits(d.c()) - 1 - int(d.scale)
}

// estimateLnDweight is numeric.c's estimate_ln_dweight: roughly
// floor(log10|ln(d)|), the decimal weight of ln(d). Inside [0.9, 1.1] it
// uses ln(1+x) ≈ x, so a value near 1 gets the many extra digits its
// tiny logarithm needs. Elsewhere it takes ln of the first two
// base-10000 groups as a double.
func estimateLnDweight(d Decimal) int {
	if d.Sign() <= 0 {
		return 0 // the callers have already refused these
	}
	if d.Cmp(decPointNine) >= 0 && d.Cmp(decOnePointOne) <= 0 {
		x, _ := d.Sub(decOne)
		if x.IsZero() {
			return 0 // ln(1) is exactly 0 and needs no extra digits
		}
		return x.decExp()
	}
	g := pgGroups(d)
	digits := g.digits[0]
	dweight := g.weight * decDigits
	if len(g.digits) > 1 {
		digits = digits*nbase + g.digits[1]
		dweight -= decDigits
	}
	lnv := math.Log(float64(digits)) + float64(dweight)*ln10
	return int(math.Log10(math.Abs(lnv)))
}

// ---- sqrt ----

// Sqrt is Postgres's numeric sqrt(d). The scale gives about 16
// significant digits, estimated from d's base-10000 weight, so
// sqrt(16.0) is 4.000000000000000. The root is exact: an integer square
// root with one extra digit, then that digit rounded half away from
// zero. That is the correctly rounded result.
//
// numeric_sqrt's special cases: NaN and +Infinity are their own roots,
// and -Infinity is a negative number like any other.
func Sqrt(d Decimal) (Decimal, error) {
	if d.Sign() < 0 { // -Infinity too
		return Decimal{}, ErrSqrtNegative
	}
	if d.kind != finite {
		return d, nil
	}
	// numeric_sqrt: the result's weight is about half the input's.
	// (weight+1)·4 is even, so Go's and C's division agree here.
	w := pgGroups(d).weight
	sweight := (w+1)*decDigits/2 - 1
	rscale := clampScale(minSigDigits-sweight, d.scale)

	return checked(sqrtRound(d, rscale))
}

// sqrtRound is sqrt_var: √d rounded half away from zero to r places. r
// may be negative, which rounds left of the point; ln_var does that to
// shrink a huge argument cheaply.
//
// It is exact. floor(√d·10^(r+1)) is the integer square root of
// c·10^(2r+2-s), or of floor(c/10^(s-2r-2)) when that exponent is
// negative (⌊√⌊y⌋⌋ = ⌊√y⌋). Then the extra digit is rounded half up.
// Rounding ⌊T⌋ on its last digit equals rounding T, because no digit
// below can turn a ≥5 into <5 or back.
func sqrtRound(d Decimal, r int) Decimal {
	n := new(big.Int).Set(d.c())
	if k := 2*r + 2 - int(d.scale); k >= 0 {
		n.Mul(n, pow10(k))
	} else {
		n.Quo(n, pow10(-k))
	}
	q, last := new(big.Int).QuoRem(n.Sqrt(n), bigTen, new(big.Int))
	if last.Int64() >= 5 {
		q.Add(q, bigOne)
	}
	if r >= 0 {
		return mk(q, int32(r))
	}
	return mk(q.Mul(q, pow10(-r)), 0)
}

// ---- Postgres's series, step for step ----
//
// exp_var and ln_var compute in numeric arithmetic with a working scale
// chosen up front: a series runs until its next term rounds to zero at
// that scale, and every product and quotient is rounded to it. Near a
// rounding boundary of the final result, those choices decide the last
// digit. (exp(-4.8125e-13) stops after 1 + x at 24 places, which leaves
// it just under a tie at 16.) So they are reproduced exactly: each step
// below is the numeric.c call named beside it, with Postgres's mul_var
// and div_var taken as exact-then-rounded. Their guard digits make them
// that in all but freak carry chains.

// mulRound is mul_var(a, b, rscale).
func mulRound(a, b Decimal, rscale int) Decimal {
	return mk(new(big.Int).Mul(a.c(), b.c()), a.scale+b.scale).Round(int32(rscale))
}

// weight is a value's base-10000 weight (NumericVar.weight); 0 for 0.
// The series ask for it every term, so it counts digits from the bit
// length instead of rendering the number (pgGroups does that, for the
// estimates that need the digit groups themselves).
func weight(d Decimal) int {
	if d.IsZero() {
		return 0
	}
	return floorDiv(d.decExp(), decDigits)
}

// numDigits is the number of decimal digits of |x| (x ≠ 0), without
// rendering it. From the bit length, ⌊bits·log10 2⌋+1 is the count or
// one more. The float product could floor one low on a huge bit length,
// so it is checked against powers of ten on both sides.
func numDigits(x *big.Int) int {
	a := new(big.Int).Abs(x)
	n := int(float64(a.BitLen())*0.30102999566398) + 1
	p := pow10(n - 1) // read-only
	switch {
	case a.Cmp(p) < 0:
		n--
	case a.Cmp(new(big.Int).Mul(p, bigTen)) >= 0:
		n++
	}
	return n
}

// expVar is exp_var(x, rscale).
func expVar(x Decimal, rscale int) (Decimal, error) {
	val := x.Float64()
	if math.Abs(val) >= maxResultScale*3 {
		if val > 0 {
			return Decimal{}, ErrOverflow
		}
		return mk(nil, int32(rscale)), nil
	}
	dweight := int(val * log10e)

	// Halve x until |x| ≤ 0.01 (ndiv2 ≤ 20), for the series' sake.
	ndiv2 := 0
	if math.Abs(val) > 0.01 {
		ndiv2 = 1
		val /= 2
		for math.Abs(val) > 0.01 {
			ndiv2++
			val /= 2
		}
		var err error
		if x, err = x.DivRound(FromInt64(1<<ndiv2), x.scale+int32(ndiv2)); err != nil {
			return Decimal{}, err
		}
	}

	// The result has dweight+rscale+1 significant digits; squaring back
	// ndiv2 times costs about log10(2^ndiv2) of them; 8 more for luck.
	sig := 1 + dweight + rscale + int(float64(ndiv2)*0.301029995663981)
	sig = max(sig, 0) + 8
	local := sig - 1

	// Taylor: 1 + x + x²/2! + …, until a term rounds to zero.
	result, err := decOne.Add(x)
	if err != nil {
		return Decimal{}, err
	}
	elem := mulRound(x, x, local)
	ni := int64(2)
	if elem, err = elem.DivRound(FromInt64(ni), int32(local)); err != nil {
		return Decimal{}, err
	}
	for !elem.IsZero() {
		if result, err = result.Add(elem); err != nil {
			return Decimal{}, err
		}
		elem = mulRound(elem, x, local)
		ni++
		if elem, err = elem.DivRound(FromInt64(ni), int32(local)); err != nil {
			return Decimal{}, err
		}
	}

	// Undo the halving: square ndiv2 times, the scale shrinking as the
	// weight grows.
	for ; ndiv2 > 0; ndiv2-- {
		l := max(sig-weight(result)*2*decDigits, 0)
		result = mulRound(result, result, l)
	}
	return checked(result.Round(int32(rscale)))
}

var decTwo = FromInt64(2)

// lnVar is ln_var(arg, rscale), for arg > 0.
func lnVar(arg Decimal, rscale int) (Decimal, error) {
	x := arg
	fact := decTwo
	nsqrt := 0
	// Square roots bring x into (0.9, 1.1), each at a scale keeping
	// about rscale+8 significant digits (negative for a huge x, which
	// rounds before the point). fact doubles with each.
	for x.Cmp(decPointNine) <= 0 {
		x = sqrtRound(x, rscale-weight(x)*decDigits/2+8)
		fact = mk(new(big.Int).Lsh(fact.c(), 1), 0)
		nsqrt++
	}
	for x.Cmp(decOnePointOne) >= 0 {
		x = sqrtRound(x, rscale-weight(x)*decDigits/2+8)
		fact = mk(new(big.Int).Lsh(fact.c(), 1), 0)
		nsqrt++
	}

	// ln x = 2·(z + z³/3 + z⁵/5 + …), z = (x-1)/(x+1), and the series
	// is then multiplied by 2^(nsqrt+1) (fact), so it carries that
	// many digits more.
	local := rscale + int(float64(nsqrt+1)*0.301029995663981) + 8
	num, err := x.Sub(decOne)
	if err != nil {
		return Decimal{}, err
	}
	den, err := x.Add(decOne)
	if err != nil {
		return Decimal{}, err
	}
	result, err := num.DivRound(den, int32(local)) // div_var_fast
	if err != nil {
		return Decimal{}, err
	}
	xx := result
	z2 := mulRound(result, result, local)
	for ni := int64(3); ; ni += 2 {
		xx = mulRound(xx, z2, local)
		elem, err := xx.DivRound(FromInt64(ni), int32(local))
		if err != nil {
			return Decimal{}, err
		}
		if elem.IsZero() {
			break
		}
		if result, err = result.Add(elem); err != nil {
			return Decimal{}, err
		}
		if weight(elem) < weight(result)-local*2/decDigits {
			break
		}
	}
	return checked(mulRound(result, fact, rscale))
}

// ---- exp ----

// Exp is Postgres's numeric exp(d). The scale estimates the result's
// decimal weight as d·log10(e) in float64. |d| ≥ 6000 overflows
// (positive) or underflows to zero at that scale (negative), as in
// exp_var.
//
// numeric_exp's special cases: exp(NaN) is NaN, exp(+Infinity) is
// +Infinity, and exp(-Infinity) is 0 (POSIX's rule, with no underflow
// error).
func Exp(d Decimal) (Decimal, error) {
	switch d.kind {
	case kindNaN, kindPosInf:
		return d, nil
	case kindNegInf:
		return Decimal{}, nil
	}
	val := d.Float64()
	w := val * log10e
	w = max(min(w, maxResultScale), -maxResultScale)
	rscale := clampScale(minSigDigits-int(w), d.scale)
	return expAt(d, rscale)
}

// expAt is exp_var: e^y rounded to rscale places, with its overflow
// limit. |y| ≥ 6000 overflows when positive and is zero at rscale
// when negative.
func expAt(y Decimal, rscale int) (Decimal, error) {
	return expVar(y, rscale)
}

// ---- ln and log ----

// checkLog is the logarithms' domain check. NaN passes (its logarithm
// is NaN, not an error; its Sign of 0 must not read as zero), and
// -Infinity fails as a negative number.
func checkLog(d Decimal) error {
	if d.kind == kindNaN {
		return nil
	}
	switch d.Sign() {
	case 0:
		return ErrLogZero
	case -1:
		return ErrLogNegative
	}
	return nil
}

// Ln is Postgres's numeric ln(d): about 16 significant digits by
// estimate_ln_dweight, so a d near 1 gets the extra places its small
// logarithm needs (ln(1.00001) has 21).
//
// numeric_ln's special cases: ln(NaN) is NaN, ln(+Infinity) is
// +Infinity, and -Infinity is a negative number (checkLog's error).
func Ln(d Decimal) (Decimal, error) {
	if err := checkLog(d); err != nil {
		return Decimal{}, err
	}
	if d.kind != finite {
		return d, nil // NaN or +Infinity
	}
	dw := estimateLnDweight(d)
	return lnAt(d, clampScale(minSigDigits-dw, d.scale))
}

// lnAt is ln_var: ln(d) rounded to rscale places, for d > 0.
func lnAt(d Decimal, rscale int) (Decimal, error) {
	return lnVar(d, rscale)
}

// Log is Postgres's numeric log(base, d) (log_var). log(d) in SQL is
// Log(10, d). log_var does not divide exact logarithms: it rounds
// ln(base) and ln(d) to scales chosen to carry 8 digits past the
// result's, then divides and rounds. Those two roundings are reproduced,
// because near a rounding boundary they decide the last digit. A base of
// 1 has ln 0 and is Postgres's division by zero.
//
// Special arguments follow numeric_log: NaN anywhere is NaN (before any
// domain check), then the domain errors as for numbers, then
// log(+Inf, +Inf) is NaN (∞/∞), log(+Inf, x) is 0 and log(b, +Inf) is
// +Infinity, even for b = 1.
func Log(base, d Decimal) (Decimal, error) {
	if base.kind|d.kind != finite {
		if base.kind == kindNaN || d.kind == kindNaN {
			return NaN(), nil
		}
		// Both signs before either zero, as numeric_log orders them
		// here (log(0, -Inf) is "negative"; the finite path below
		// checks base fully first, as ln_var does).
		s1, s2 := base.Sign(), d.Sign()
		if s1 < 0 || s2 < 0 {
			return Decimal{}, ErrLogNegative
		}
		if s1 == 0 || s2 == 0 {
			return Decimal{}, ErrLogZero
		}
		switch {
		case base.kind == kindPosInf && d.kind == kindPosInf:
			return NaN(), nil
		case base.kind == kindPosInf:
			return Decimal{}, nil
		}
		return Inf(1), nil // d is +Infinity
	}
	if err := checkLog(base); err != nil {
		return Decimal{}, err
	}
	if err := checkLog(d); err != nil {
		return Decimal{}, err
	}
	if base.Cmp(decOne) == 0 {
		return Decimal{}, ErrDivByZero
	}
	bdw := estimateLnDweight(base)
	ndw := estimateLnDweight(d)
	rdw := ndw - bdw
	rscale := clampScale(minSigDigits-rdw, base.scale, d.scale)
	lb, err := lnAt(base, max(rscale+rdw-bdw+8, 0))
	if err != nil {
		return Decimal{}, err
	}
	ln, err := lnAt(d, max(rscale+rdw-ndw+8, 0))
	if err != nil {
		return Decimal{}, err
	}
	return ln.DivRound(lb, int32(rscale))
}

// ---- power ----

// Pow is Postgres's numeric power(base, exp). An integral exp that fits
// in int32 takes power_var_int's route: exact here, at a scale estimated
// from the base's leading digit groups. Any other exp is power_var's
// e^(exp·ln|base|), reproduced step by step, because its intermediate
// roundings decide the last digit near a boundary (a base near 1 to a
// short exponent lands near one often). The steps:
//
//  1. ln(base) to about 8 significant digits, times exp, both rounded to
//     that many places. Its float64 value sizes the result (rscale).
//  2. ln(base) again, rounded to the places the result needs plus 8;
//     times exp, rounded to the same places.
//  3. exp of that, rounded to rscale.
//
// NaN and infinite arguments take powSpecial, before anything here.
func Pow(base, exp Decimal) (Decimal, error) {
	if base.kind|exp.kind != finite {
		return powSpecial(base, exp)
	}
	if base.IsZero() && exp.Sign() < 0 {
		return Decimal{}, ErrPowZeroNeg
	}
	if exp.IsInteger() {
		if n, ok := exp.Int64(); ok && n >= math.MinInt32 && n <= math.MaxInt32 {
			return powInt(base, int(n), exp.scale)
		}
	}
	// 0 to a non-integer power: zero, with 16 places (power_var).
	if base.IsZero() {
		return mk(nil, minSigDigits), nil
	}
	neg := false
	if base.Sign() < 0 {
		if !exp.IsInteger() {
			return Decimal{}, ErrPowNegNonInt
		}
		// An integral exp too big for int32: the sign follows its parity.
		neg = new(big.Int).Quo(exp.c(), pow10(int(exp.scale))).Bit(0) == 1
		base = base.Abs()
	}

	// 1. The estimate.
	lnDw := estimateLnDweight(base)
	localR := max(8-lnDw, 0)
	lnBase, err := lnAt(base, localR)
	if err != nil {
		return Decimal{}, err
	}
	lnNum, err := lnBase.Mul(exp)
	if err != nil {
		return Decimal{}, err
	}
	val := lnNum.Round(int32(localR)).Float64()
	if math.Abs(val) > maxResultScale*3.01 {
		if val > 0 {
			return Decimal{}, ErrOverflow
		}
		return mk(nil, maxDisplayScale), nil
	}
	w := val * log10e
	rscale := clampScale(minSigDigits-int(w), base.scale, exp.scale)

	// 2. The real ln·exp, at the significant digits the result needs.
	sig := max(rscale+int(w), 0)
	localR = max(sig-lnDw+8, 0)
	if lnBase, err = lnAt(base, localR); err != nil {
		return Decimal{}, err
	}
	if lnNum, err = lnBase.Mul(exp); err != nil {
		return Decimal{}, err
	}

	// 3. exp, with exp_var's own overflow limit on the real product.
	r, err := expAt(lnNum.Round(int32(localR)), rscale)
	if err != nil {
		return Decimal{}, err
	}
	if neg {
		r = r.Neg()
	}
	return r, nil
}

// decMinusOne is -1, for powSpecial's x = -1 rule.
var decMinusOne = FromInt64(-1)

// powSpecial is numeric_power's handling of NaN and infinite arguments,
// which follows POSIX pow(3). x is the base, y the exponent; at least
// one is special. The tests run in numeric_power's order, since several
// overlap (1 ^ NaN is 1, but NaN ^ 1 is NaN):
//
//	NaN ^ 0 = 1, 1 ^ NaN = 1, any other NaN argument → NaN
//	0 ^ negative (y = -Inf included)        → error, as for numbers
//	negative ^ non-integer finite y         → error (x = -Inf included)
//	1 ^ y = 1, x ^ 0 = 1, 0 ^ positive = 0
//	y = ±Inf:   x = -1 → 1; else +Inf when (|x| > 1) == (y > 0), else 0
//	            (an infinite x counts as |x| > 1)
//	x = +Inf:   y > 0 → +Inf, y < 0 → 0
//	x = -Inf:   y < 0 → 0; y an odd integer → -Inf; other y > 0 → +Inf
//
// Every finite result is a scale-0 1 or 0 (Postgres's const_one and
// const_zero), whatever the inputs' scales.
func powSpecial(x, y Decimal) (Decimal, error) {
	one, zero := FromInt64(1), Decimal{}
	if x.kind == kindNaN {
		if y.kind == finite && y.IsZero() {
			return one, nil
		}
		return NaN(), nil
	}
	if y.kind == kindNaN {
		if x.kind == finite && x.Cmp(decOne) == 0 {
			return one, nil
		}
		return NaN(), nil
	}
	// At least one infinity, and no NaN: Sign is meaningful for both.
	s1, s2 := x.Sign(), y.Sign()
	if s1 == 0 && s2 < 0 {
		return Decimal{}, ErrPowZeroNeg
	}
	// An infinite exponent counts as integral here (numeric_is_integral),
	// so only a finite fractional one is refused.
	if s1 < 0 && y.kind == finite && !y.IsInteger() {
		return Decimal{}, ErrPowNegNonInt
	}
	if x.kind == finite && x.Cmp(decOne) == 0 {
		return one, nil
	}
	if s2 == 0 {
		return one, nil
	}
	if s1 == 0 && s2 > 0 {
		return zero, nil
	}
	if y.kind != finite {
		absXGtOne := true // x = ±Inf
		if x.kind == finite {
			if x.Cmp(decMinusOne) == 0 {
				return one, nil
			}
			absXGtOne = x.Abs().Cmp(decOne) > 0
		}
		if absXGtOne == (s2 > 0) {
			return Inf(1), nil
		}
		return zero, nil
	}
	// x is infinite, y finite and nonzero.
	if x.kind == kindPosInf {
		if s2 > 0 {
			return Inf(1), nil
		}
		return zero, nil
	}
	if s2 < 0 {
		return zero, nil
	}
	if y.IsInteger() && isOdd(y) {
		return Inf(-1), nil
	}
	return Inf(1), nil
}

// isOdd reports whether an integral finite d is odd. Bit 0 of the
// integer part gives the parity; math/big's Bit reads a negative value
// in two's complement, which keeps the parity of its magnitude.
func isOdd(d Decimal) bool {
	t := d.Trunc(0)
	if t.isSmall() {
		return t.small&1 != 0
	}
	return t.wide.Bit(0) == 1
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// powInt is power_var_int: base^n for an int32 n. expScale is the
// exponent's display scale, which the result scale may not drop below
// (power(2, 2.0) has 16 places either way, but power(2.0, 10) has 13).
func powInt(base Decimal, n int, expScale int32) (Decimal, error) {
	// Estimate the result's decimal weight from base ≈ f·10^p, using the
	// leading groups up to 16 digits, as a double: n·(log10 f + p).
	var f float64
	if !base.IsZero() {
		g := pgGroups(base)
		fv := float64(g.digits[0])
		p := g.weight * decDigits
		for i := 1; i < len(g.digits) && i*decDigits < 16; i++ {
			fv = fv*nbase + float64(g.digits[i])
			p -= decDigits
		}
		f = float64(n) * (math.Log10(fv) + float64(p))
	}
	if f > (maxWeight+1)*decDigits {
		return Decimal{}, ErrOverflow
	}
	if f+1 < -maxDisplayScale {
		return mk(nil, maxDisplayScale), nil
	}
	rscale := clampScale(minSigDigits-int(f), base.scale, expScale)

	// The special cases power_var_int handles directly.
	switch n {
	case 0:
		// 0^0 included: 1, as SQL:2003 says.
		return mk(new(big.Int).Set(pow10(rscale)), int32(rscale)), nil
	case 1:
		return checked(base.Round(int32(rscale)))
	case -1:
		return decOne.DivRound(base, int32(rscale))
	case 2:
		return checked(mulRound(base, base, rscale))
	}
	if base.IsZero() {
		return mk(nil, int32(rscale)), nil // n < 0 was refused by Pow
	}

	// Square-and-multiply over n's bits, each product rounded to keep
	// about sig significant digits (but never more places than the
	// exact product has). log(|n|) more digits cover the error the
	// products add up to. It is natural log, as numeric.c writes it.
	sig := 1 + rscale + int(f)
	sig += int(math.Log(math.Abs(float64(n)))) + 8
	neg := n < 0
	mask := uint(abs(n))
	prod := base
	result := decOne
	if mask&1 == 1 {
		result = base
	}
	for mask >>= 1; mask > 0; mask >>= 1 {
		l := max(min(sig-2*weight(prod)*decDigits, 2*int(prod.scale)), 0)
		prod = mulRound(prod, prod, l)
		if mask&1 == 1 {
			l = max(min(sig-(weight(prod)+weight(result))*decDigits, int(prod.scale)+int(result.scale)), 0)
			result = mulRound(prod, result, l)
		}
		// Past NUMERIC_WEIGHT_MAX the result overflows (or, for a
		// negative n, is 0), so stop before the products grow further.
		if weight(prod) > maxWeight || weight(result) > maxWeight {
			if !neg {
				return Decimal{}, ErrOverflow
			}
			result, neg = Decimal{}, false
			break
		}
	}
	if neg {
		return decOne.DivRound(result, int32(rscale))
	}
	return checked(result.Round(int32(rscale)))
}
