package decimal

import (
	"errors"
	"math/big"
	"math/rand"
	"strings"
	"testing"
)

// The expected values below were produced by Postgres 16 (numeric
// sqrt, exp, ln, log, power) and are copied verbatim. N-052 checked the
// implementation against 14,683 such calls (10,390 random across
// magnitudes and scales, 4,293 built to be hard), all equal. These are
// the cases worth keeping: each scale rule, the near ties, the
// extremes, and the two powers that showed why the series must be
// ported step for step rather than computed correctly rounded.

type mathCase struct {
	fn, a, b string
	want     string
	// For results too long to inline: the length and both ends.
	wantLen        int
	prefix, suffix string
}

func runMath(fn string, a, b Decimal) (Decimal, error) {
	switch fn {
	case "sqrt":
		return Sqrt(a)
	case "exp":
		return Exp(a)
	case "ln":
		return Ln(a)
	case "log10":
		return Log(FromInt64(10), a)
	case "log":
		return Log(a, b)
	case "pow":
		return Pow(a, b)
	}
	panic(fn)
}

func TestMathMatchesPostgres(t *testing.T) {
	zeros := func(n int) string { return strings.Repeat("0", n) }
	cases := []mathCase{
		// Each function's scale rule, at the textbook values.
		{fn: "sqrt", a: "16.0", want: "4.000000000000000"},
		{fn: "sqrt", a: "0", want: "0.000000000000000"},
		{fn: "sqrt", a: "2", want: "1.414213562373095"},
		{fn: "sqrt", a: "0.0001", want: "0.01000000000000000"},
		{fn: "sqrt", a: "123456789.123", want: "11111.11106609056"},
		{fn: "exp", a: "1.0", want: "2.7182818284590452"},
		{fn: "exp", a: "0", want: "1.0000000000000000"},
		{fn: "exp", a: "-1.5", want: "0.2231301601484298"},
		{fn: "exp", a: "100.5", want: "44319559098458954160107061979564816895899481.9"},
		{fn: "exp", a: "123.456789012345678901234567890", want: "413620658369743493708909260854829501271542557186287239.777559258884482405335287289"},
		{fn: "ln", a: "2.0", want: "0.6931471805599453"},
		{fn: "ln", a: "1.0", want: "0.0000000000000000"},
		{fn: "ln", a: "1.00001", want: "0.000009999950000333331"},
		{fn: "log10", a: "100.0", want: "2.0000000000000000"},
		{fn: "log", a: "2.0", b: "8.0", want: "3.0000000000000000"},
		{fn: "log", a: "10", b: "2.5", want: "0.3979400086720376"},
		{fn: "pow", a: "2.0", b: "10", want: "1024.0000000000000"},
		{fn: "pow", a: "2", b: "2.0", want: "4.0000000000000000"},
		{fn: "pow", a: "1.5", b: "3", want: "3.3750000000000000"},
		{fn: "pow", a: "2.0", b: "-2", want: "0.2500000000000000"},
		{fn: "pow", a: "2.0", b: "0.5", want: "1.4142135623730950"},
		{fn: "pow", a: "-2.0", b: "3", want: "-8.0000000000000000"},
		{fn: "pow", a: "0.0", b: "0.5", want: "0.0000000000000000"},
		{fn: "pow", a: "0", b: "0", want: "1.0000000000000000"},
		{fn: "pow", a: "10", b: "0.5", want: "3.1622776601683793"},

		// Near 1, where the estimates add places and ties sit close.
		{fn: "ln", a: "1.0000000000000001", want: "0.00000000000000010000000000000000"},
		{fn: "ln", a: "0.9999999999999999", want: "-0.00000000000000010000000000000001"},
		{fn: "ln", a: "1.00000000000000000000000000000001", want: "0.000000000000000000000000000000010000000000000000"},
		{fn: "log10", a: "1.000000000000000000005", want: "0.0000000000000000000021714724095162591"},
		{fn: "exp", a: "0.0000000000001", want: "1.0000000000001000"},
		{fn: "exp", a: "-0.00000000000005", want: "0.9999999999999500"},
		{fn: "pow", a: "1.0000000000000001", b: "13.75", want: "1.0000000000000014"},
		{fn: "pow", a: "0.9999999999999995", b: "100.125", want: "0.9999999999999499"},
		// The two that a correctly rounded exp(b·ln a) gets wrong. exp_var's
		// series stops after 1 + x at its working scale, leaving the value
		// just under a tie (the true values are ...984158 5000...0001 and
		// ...5187 5000...0001).
		{fn: "pow", a: "0.999999999999999999999999105", b: "17.7", want: "0.999999999999999999999984158"},
		{fn: "pow", a: "0.999999999999965", b: "13.75", want: "0.9999999999995187"},

		// Integer exponents (power_var_int), large ones included.
		{fn: "pow", a: "1.0000001", b: "100000000", want: "22026.454781577307"},
		{fn: "pow", a: "0.9999", b: "70000", want: "0.0009115628414414317"},
		{fn: "pow", a: "1.1", b: "-300", want: "0.0000000000003821153221963801"},
		{fn: "pow", a: "0.99999999", b: "-2147483648", want: "2120327908.25374139"},
		{fn: "pow", a: "7.123456789012345678", b: "97", want: "51405198239484368957911465695030340002605856767950557811161389024867390047895913609.174721090407251751"},
		// Integral but past int32: power_var's ln/exp route, sign by parity.
		{fn: "pow", a: "-1.0000000001", b: "3000000001", want: "-1.3498588076907411"},
		{fn: "pow", a: "1.0000000001", b: "30000000000", want: "20.085536920174837"},
		{fn: "pow", a: "1.000000000000000000000000000001", b: "1e30", want: "2.718281828459045235360287471351"},

		// The extremes: the 1000-place cap, underflow to zero at it, and
		// results thousands of digits long.
		{fn: "ln", a: "1" + zeros(1000), want: "2302.5850929940457"},
		{fn: "ln", a: "0." + zeros(1499) + "7", wantLen: 1006, prefix: "-3451.9317293420132127218818292831031316", suffix: "981043858208930838713960207993"},
		{fn: "sqrt", a: "1" + zeros(2600), wantLen: 1301, prefix: "1000000000000000000000000000000000000000", suffix: zeros(30)},
		{fn: "exp", a: "-2400.25", wantLen: 1002, prefix: "0." + zeros(38), suffix: zeros(30)},
		{fn: "exp", a: "-1000", wantLen: 452, prefix: "0." + zeros(38), suffix: "000000000000005075958897549457"},
		{fn: "exp", a: "700", wantLen: 305, prefix: "1014232054735004509455329595231267615204", suffix: "130430066930357357609994944589"},
		{fn: "pow", a: "0.1", b: "2000", wantLen: 1002, prefix: "0." + zeros(38), suffix: zeros(30)},
		{fn: "pow", a: "0.1", b: "999", wantLen: 1002, prefix: "0." + zeros(38), suffix: "000000000000000000000000000010"},
	}
	for _, c := range cases {
		a := MustParse(c.a)
		var b Decimal
		if c.b != "" {
			b = MustParse(c.b)
		}
		got, err := runMath(c.fn, a, b)
		name := c.fn + "(" + trunc(c.a) + ", " + c.b + ")"
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		s := got.String()
		if c.wantLen == 0 {
			if s != c.want {
				t.Errorf("%s = %s, want %s", name, s, c.want)
			}
			continue
		}
		if len(s) != c.wantLen || !strings.HasPrefix(s, c.prefix) || !strings.HasSuffix(s, c.suffix) {
			t.Errorf("%s = %s… (%d chars), want %s…%s (%d chars)", name, trunc(s), len(s), c.prefix, c.suffix, c.wantLen)
		}
	}
}

func trunc(s string) string {
	if len(s) > 50 {
		return s[:50] + "…"
	}
	return s
}

// TestMathErrors: Postgres's refusals, by sentinel.
func TestMathErrors(t *testing.T) {
	for _, c := range []struct {
		fn, a, b string
		want     error
	}{
		{"sqrt", "-1.0", "", ErrSqrtNegative},
		{"ln", "0.0", "", ErrLogZero},
		{"ln", "-1", "", ErrLogNegative},
		{"log", "0", "10", ErrLogZero},       // the base is checked first
		{"log", "-2", "0", ErrLogNegative},   // ... and refused first
		{"log", "10", "-1", ErrLogNegative},  // then the argument
		{"log", "1.0", "10.0", ErrDivByZero}, // ln(1) = 0
		{"pow", "0.0", "-1", ErrPowZeroNeg},
		{"pow", "0", "-0.5", ErrPowZeroNeg},
		{"pow", "-2.0", "0.5", ErrPowNegNonInt},
		{"exp", "6000.0", "", ErrOverflow},
		{"pow", "3", "2147483647", ErrOverflow},
		{"pow", "10", "200000", ErrOverflow},
		{"pow", "1.5", "400000.5", ErrOverflow},
	} {
		var b Decimal
		if c.b != "" {
			b = MustParse(c.b)
		}
		if _, err := runMath(c.fn, MustParse(c.a), b); !errors.Is(err, c.want) {
			t.Errorf("%s(%s, %s): %v, want %v", c.fn, c.a, c.b, err, c.want)
		}
	}
	// Underflow is zero at the result scale, not an error.
	if got, err := Exp(MustParse("-6000.5")); err != nil || !got.IsZero() || got.Scale() != 1000 {
		t.Errorf("exp(-6000.5) = %v %v, want 0 at scale 1000", got, err)
	}
	for _, e := range []string{"10000", "10000.5"} { // power_var_int's and power_var's underflow
		if got, err := Pow(MustParse("0.5"), MustParse(e)); err != nil || !got.IsZero() || got.Scale() != 1000 {
			t.Errorf("0.5^%s = %v %v, want 0 at scale 1000", e, got, err)
		}
	}
}

// TestNumDigits: the digit count the series' weight() relies on, at and
// around powers of ten and on random sizes, against the rendered length.
func TestNumDigits(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 3000; i++ {
		x := new(big.Int).Rand(r, new(big.Int).Lsh(big.NewInt(1), uint(r.Intn(4000)+1)))
		if i%3 == 0 {
			x = new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(r.Intn(1200))), nil)
			x.Add(x, big.NewInt(int64(r.Intn(3)-1)))
		}
		if x.Sign() == 0 {
			continue
		}
		if r.Intn(2) == 0 {
			x.Neg(x)
		}
		if got, want := numDigits(x), len(new(big.Int).Abs(x).Text(10)); got != want {
			t.Fatalf("numDigits(%v) = %d, want %d", x, got, want)
		}
	}
}
