package decimal

// The small-coefficient form (N-050) against the wide one. The wide
// code paths are the original *big.Int implementation and accept a
// non-canonical operand (a wide value that would fit int64), so running
// an operation on the forced-wide form of its operands gives the result
// the small path must reproduce: same digits, same scale, same error,
// and a canonical representation.

import (
	"errors"
	"math"
	"math/big"
	"strconv"
	"testing"
)

// wideOf is d with its coefficient forced into a *big.Int. Zero stays
// as is: the wide form is never zero (IsZero reads the small form), and
// a zero has no small/wide distinction to test.
func wideOf(d Decimal) Decimal {
	if d.IsZero() {
		return d
	}
	return Decimal{wide: d.Coef(), scale: d.scale}
}

// canonical reports whether d is in the one representation mk keeps.
func canonical(d Decimal) bool {
	if d.wide == nil {
		return d.small != math.MinInt64
	}
	return !d.wide.IsInt64() || d.wide.Int64() == math.MinInt64
}

func sameResult(t *testing.T, what string, got Decimal, gerr error, want Decimal, werr error) {
	t.Helper()
	if (gerr == nil) != (werr == nil) || (gerr != nil && !errors.Is(gerr, werr)) {
		t.Fatalf("%s: error %v, wide path %v", what, gerr, werr)
	}
	if gerr != nil {
		return
	}
	if got.String() != want.String() || got.scale != want.scale {
		t.Fatalf("%s = %s (scale %d), wide path %s (scale %d)", what, got, got.scale, want, want.scale)
	}
	if !canonical(got) {
		t.Fatalf("%s = %s: not canonical (%#v)", what, got, got)
	}
}

// smallCoefs straddles every boundary the small paths check: digit-count
// steps, rounding halves, the 10^18/10^19 powers, √MaxInt64 (products),
// and the int64 edge itself.
var smallCoefs = []int64{
	0, 1, -1, 4, 5, -5, 9, 10, 15, -15, 25, 49, 50, -50, 99, 100, 101, 12345, -12345,
	999_999_999, 1_000_000_000, 1 << 31, 3_037_000_499, 3_037_000_500, -3_037_000_500,
	1e17, 1e18 - 1, 1e18, -1e18, 5e18, -5e18, math.MaxInt64 / 10, math.MaxInt64/2 + 1,
	math.MaxInt64 - 1, math.MaxInt64, -math.MaxInt64, 1 << 53, 1<<53 + 1, -(1 << 53),
}

var smallScales = []int32{0, 1, 2, 3, 5, 9, 18, 19, 20, 25}

func smallValues() []Decimal {
	var vs []Decimal
	for _, c := range smallCoefs {
		for _, s := range smallScales {
			vs = append(vs, mkSmall(c, s))
		}
	}
	return vs
}

func TestSmallMatchesWide(t *testing.T) {
	vs := smallValues()
	for _, a := range vs {
		checkUnary(t, a)
	}
	for _, a := range vs {
		for _, b := range vs {
			checkBinary(t, a, b)
		}
	}
}

func checkUnary(t *testing.T, a Decimal) {
	t.Helper()
	w := wideOf(a)
	name := a.String()
	sameResult(t, "Neg("+name+")", a.Neg(), nil, w.Neg(), nil)
	sameResult(t, "Abs("+name+")", a.Abs(), nil, w.Abs(), nil)
	sameResult(t, "Normalize("+name+")", a.Normalize(), nil, w.Normalize(), nil)
	sameResult(t, "Floor("+name+")", a.Floor(), nil, w.Floor(), nil)
	sameResult(t, "Ceil("+name+")", a.Ceil(), nil, w.Ceil(), nil)
	for _, n := range []int32{-25, -20, -19, -18, -3, -1, 0, 1, 2, 5, 18, 19, 20, 25} {
		sameResult(t, "Round("+name+", "+itoa(n)+")", a.Round(n), nil, w.Round(n), nil)
		sameResult(t, "Trunc("+name+", "+itoa(n)+")", a.Trunc(n), nil, w.Trunc(n), nil)
	}
	for _, tm := range [][2]int32{{10, 2}, {5, 0}, {3, 5}, {2, -3}, {38, 10}, {19, 0}, {1000, 20}} {
		g, gerr := a.FitTypmod(tm[0], tm[1])
		x, xerr := w.FitTypmod(tm[0], tm[1])
		if (gerr == nil) != (xerr == nil) {
			t.Fatalf("FitTypmod(%s, %v): error %v, wide path %v", name, tm, gerr, xerr)
		}
		if gerr == nil {
			sameResult(t, "FitTypmod("+name+")", g, nil, x, nil)
		}
	}
	if g, x := a.IntDigits(), w.IntDigits(); g != x {
		t.Fatalf("IntDigits(%s) = %d, wide path %d", name, g, x)
	}
	if !a.IsZero() {
		if g, x := a.lead(), w.lead(); g != x {
			t.Fatalf("lead(%s) = %d, wide path %d", name, g, x)
		}
	}
	if g, x := a.IsInteger(), w.IsInteger(); g != x {
		t.Fatalf("IsInteger(%s) = %v, wide path %v", name, g, x)
	}
	gn, gok := a.Int64()
	xn, xok := w.Int64()
	if gn != xn || gok != xok {
		t.Fatalf("Int64(%s) = %d,%v, wide path %d,%v", name, gn, gok, xn, xok)
	}
	if g, x := a.Float64(), w.Float64(); math.Float64bits(g) != math.Float64bits(x) {
		t.Fatalf("Float64(%s) = %v, wide path %v", name, g, x)
	}
	if g, x := a.Sign(), w.Sign(); g != x {
		t.Fatalf("Sign(%s) = %d, wide path %d", name, g, x)
	}
	if g, x := string(a.AppendText([]byte("x"))), string(w.AppendText([]byte("x"))); g != x {
		t.Fatalf("AppendText(%s) = %q, wide path %q", name, g, x)
	}
	gw, gf := a.weightAndFirst()
	xw, xf := w.weightAndFirst()
	if gw != xw || gf != xf {
		t.Fatalf("weightAndFirst(%s) = %d,%d, wide path %d,%d", name, gw, gf, xw, xf)
	}
	// Mantissa and both of its inverses round-trip to the same value.
	neg, digits, exp := a.Mantissa()
	xneg, xdigits, xexp := w.Mantissa()
	if neg != xneg || digits != xdigits || exp != xexp {
		t.Fatalf("Mantissa(%s) = %v,%q,%d, wide path %v,%q,%d", name, neg, digits, exp, xneg, xdigits, xexp)
	}
	back, err := FromMantissa(neg, digits, exp, a.scale)
	sameResult(t, "FromMantissa(Mantissa("+name+"))", back, err, a, nil)
	if digits != "" {
		m, _ := strconv.ParseUint(digits, 10, 64)
		back, err = FromMantissaU64(neg, m, len(digits), exp, a.scale)
		sameResult(t, "FromMantissaU64(Mantissa("+name+"))", back, err, a, nil)
	}
	// Text round trip through Parse's small path.
	p, err := Parse(name)
	sameResult(t, "Parse("+name+")", p, err, a, nil)
}

func checkBinary(t *testing.T, a, b Decimal) {
	t.Helper()
	wa, wb := wideOf(a), wideOf(b)
	name := a.String() + ", " + b.String()
	if g, x := a.Cmp(b), wa.Cmp(wb); g != x {
		t.Fatalf("Cmp(%s) = %d, wide path %d", name, g, x)
	}
	g, gerr := a.Add(b)
	x, xerr := wa.Add(wb)
	sameResult(t, "Add("+name+")", g, gerr, x, xerr)
	g, gerr = a.Sub(b)
	x, xerr = wa.Sub(wb)
	sameResult(t, "Sub("+name+")", g, gerr, x, xerr)
	g, gerr = a.Mul(b)
	x, xerr = wa.Mul(wb)
	sameResult(t, "Mul("+name+")", g, gerr, x, xerr)
	g, gerr = a.Div(b)
	x, xerr = wa.Div(wb)
	sameResult(t, "Div("+name+")", g, gerr, x, xerr)
	g, gerr = a.Mod(b)
	x, xerr = wa.Mod(wb)
	sameResult(t, "Mod("+name+")", g, gerr, x, xerr)
	for _, s := range []int32{0, 2, 7, 21} {
		g, gerr = a.DivRound(b, s)
		x, xerr = wa.DivRound(wb, s)
		sameResult(t, "DivRound("+name+", "+itoa(s)+")", g, gerr, x, xerr)
	}
	if g, x := DivScale(a, b), DivScale(wa, wb); g != x {
		t.Fatalf("DivScale(%s) = %d, wide path %d", name, g, x)
	}
}

// TestSumSmallMatchesAdd runs a Sum (small total, promoting on
// overflow) against Decimal.Add on the forced-wide operands, through
// sequences that cross the int64 edge in both directions and mix
// scales and AddInt64.
func TestSumSmallMatchesAdd(t *testing.T) {
	vs := smallValues()
	for start := range vs {
		var s Sum
		want := Decimal{}
		for i := 0; i < 40; i++ {
			v := vs[(start*7+i*13)%len(vs)]
			if i%5 == 4 {
				n := smallCoefs[(start+i)%len(smallCoefs)]
				if i%10 == 9 {
					n = math.MinInt64
				}
				if err := s.AddInt64(n); err != nil {
					t.Fatal(err)
				}
				want, _ = wideOf(want).Add(Decimal{wide: big.NewInt(n)})
				continue
			}
			if err := s.Add(v); err != nil {
				t.Fatal(err)
			}
			want, _ = wideOf(want).Add(wideOf(v))
		}
		sameResult(t, "Sum from "+vs[start].String(), s.Value(), nil, want, nil)
	}
}

func TestDigitsU64(t *testing.T) {
	for _, x := range []uint64{0, 1, 9, 10, 11, 99, 100, 1<<63 - 1, 1 << 63, math.MaxUint64} {
		if g, w := digitsU64(x), len(strconv.FormatUint(x, 10)); g != w {
			t.Errorf("digitsU64(%d) = %d, want %d", x, g, w)
		}
	}
	for i, p := range pow10u64 {
		for _, x := range []uint64{p - 1, p, p + 1} {
			if g, w := digitsU64(x), len(strconv.FormatUint(x, 10)); g != w {
				t.Errorf("digitsU64(%d) (10^%d±1) = %d, want %d", x, i, g, w)
			}
		}
	}
}

// TestSmallFormBoundaries pins the representation at the int64 edge.
func TestSmallFormBoundaries(t *testing.T) {
	maxD := FromInt64(math.MaxInt64)
	if !maxD.isSmall() {
		t.Fatal("MaxInt64 should be small")
	}
	if FromInt64(math.MinInt64).isSmall() {
		t.Fatal("MinInt64 must be wide (its magnitude is not an int64)")
	}
	// Overflowing the small form promotes, exactly.
	sum, err := maxD.Add(FromInt64(1))
	if err != nil || sum.isSmall() || sum.String() != "9223372036854775808" {
		t.Fatalf("MaxInt64 + 1 = %s (%v), small=%v", sum, err, sum.isSmall())
	}
	// ... and coming back demotes.
	back, err := sum.Sub(FromInt64(1))
	if err != nil || !back.isSmall() || !back.Equal(maxD) {
		t.Fatalf("(MaxInt64 + 1) - 1 = %s (%v), small=%v", back, err, back.isSmall())
	}
	neg, _ := FromInt64(-math.MaxInt64).Sub(FromInt64(1))
	if neg.isSmall() || neg.String() != "-9223372036854775808" {
		t.Fatalf("-MaxInt64 - 1 = %s, small=%v", neg, neg.isSmall())
	}
	if p := MustParse("12345678901234567890"); p.isSmall() {
		t.Fatal("a 20-digit literal should be wide")
	}
	if p := MustParse("922337203685477580.7"); !p.isSmall() || p.String() != "922337203685477580.7" {
		t.Fatalf("19 digits within int64: %s small=%v", p, p.isSmall())
	}
}

// FuzzSmallMatchesWide is TestSmallMatchesWide over arbitrary int64
// coefficients and scales.
func FuzzSmallMatchesWide(f *testing.F) {
	f.Add(int64(12345), uint8(2), int64(-5), uint8(0))
	f.Add(int64(math.MaxInt64), uint8(0), int64(math.MaxInt64), uint8(18))
	f.Add(int64(3_037_000_500), uint8(19), int64(-3_037_000_500), uint8(1))
	f.Add(int64(5e18), uint8(20), int64(7), uint8(3))
	f.Fuzz(func(t *testing.T, ac int64, as uint8, bc int64, bs uint8) {
		a, b := mkSmall(ac, int32(as%45)), mkSmall(bc, int32(bs%45))
		checkUnary(t, a)
		checkUnary(t, b)
		checkBinary(t, a, b)
		checkBinary(t, b, a)
	})
}
