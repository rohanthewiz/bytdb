package decimal

// NaN and ±Infinity (N-051) against Postgres 16 itself.
//
// pgSpecialPairs and pgSpecialUnary are golden tables captured from
// Postgres 16.9: every binary operation over every pair of the operands
// below in which at least one is special, and every unary function over
// a mix of special and finite values. Each result is Postgres's ::text
// rendering or "ERROR: <message>". They were produced by a plpgsql
// wrapper that runs one expression per call and catches its error:
//
//	binary:  (a::numeric OP b::numeric)::text   for + - * /
//	         f(a::numeric, b::numeric)::text    for mod, power, log (base a)
//	         cmp: the sign of a <=> b, from < and =
//	unary:   f(a::numeric)::text; neg is -(a), round1 is round(a, 1)
//
// Keeping the captured text rather than hand-written expectations means
// the special-value rules are checked against what Postgres does, not
// against a reading of numeric.c. The finite operands include a wide
// coefficient (beyond int64) and a scaled zero, so both coefficient
// forms meet every special value.

import (
	"errors"
	"math"
	"testing"
)

// evalPair runs one binary operation the way the SQL layer does.
func evalPair(op string, a, b Decimal) (Decimal, error) {
	switch op {
	case "+":
		return a.Add(b)
	case "-":
		return a.Sub(b)
	case "*":
		return a.Mul(b)
	case "/":
		return a.Div(b)
	case "mod":
		return a.Mod(b)
	case "power":
		return Pow(a, b)
	case "log":
		return Log(a, b)
	case "cmp":
		return FromInt64(int64(a.Cmp(b))), nil
	}
	panic("unknown op " + op)
}

// evalUnary runs one unary function.
func evalUnary(op string, a Decimal) (Decimal, error) {
	switch op {
	case "neg":
		return a.Neg(), nil
	case "abs":
		return a.Abs(), nil
	case "sign":
		// SQL's sign(numeric): NaN for NaN, else the sign as a numeric.
		if a.IsNaN() {
			return a, nil
		}
		return FromInt64(int64(a.Sign())), nil
	case "round":
		return a.Round(0), nil
	case "round1":
		return a.Round(1), nil
	case "trunc":
		return a.Trunc(0), nil
	case "ceil":
		return a.Ceil(), nil
	case "floor":
		return a.Floor(), nil
	case "sqrt":
		return Sqrt(a)
	case "exp":
		return Exp(a)
	case "ln":
		return Ln(a)
	case "log":
		return Log(FromInt64(10), a)
	case "trim_scale":
		return a.Normalize(), nil
	}
	panic("unknown op " + op)
}

// render is a result in the golden tables' form.
func render(d Decimal, err error) string {
	if err != nil {
		return "ERROR: " + err.Error()
	}
	return d.String()
}

func TestSpecialMatchesPostgres(t *testing.T) {
	for _, c := range pgSpecialPairs {
		a, b := MustParse(c[1]), MustParse(c[2])
		if got := render(evalPair(c[0], a, b)); got != c[3] {
			t.Errorf("%s(%s, %s) = %s, Postgres %s", c[0], c[1], c[2], got, c[3])
		}
		// The forced-wide form of a finite operand must agree too: the
		// special checks sit in front of both coefficient forms.
		if a.IsFinite() && !a.IsZero() || b.IsFinite() && !b.IsZero() {
			if got := render(evalPair(c[0], wideIfFinite(a), wideIfFinite(b))); got != c[3] {
				t.Errorf("%s(%s, %s) wide = %s, Postgres %s", c[0], c[1], c[2], got, c[3])
			}
		}
	}
	for _, c := range pgSpecialUnary {
		a := MustParse(c[1])
		if got := render(evalUnary(c[0], a)); got != c[2] {
			t.Errorf("%s(%s) = %s, Postgres %s", c[0], c[1], got, c[2])
		}
	}
}

// wideIfFinite is wideOf for a finite value; a special one has no
// coefficient to widen.
func wideIfFinite(d Decimal) Decimal {
	if !d.IsFinite() {
		return d
	}
	return wideOf(d)
}

// TestSpecialParse covers numeric_in's spellings: any case, "inf" as
// well as "Infinity", a sign on the infinities only, and nothing else
// around them but whitespace.
func TestSpecialParse(t *testing.T) {
	for in, want := range map[string]string{
		"NaN":         "NaN",
		"nan":         "NaN",
		" NAN\t":      "NaN",
		"Infinity":    "Infinity",
		"+infinity":   "Infinity",
		"inf":         "Infinity",
		"+INF":        "Infinity",
		"-Infinity":   "-Infinity",
		" -inf ":      "-Infinity",
		"-iNfInItY":   "-Infinity",
		"+Inf":        "Infinity", // Go's strconv spelling (float32 → numeric goes through it)
		"1.5":         "1.5",      // the number path is untouched
		"-.5":         "-0.5",
		"+00012.3400": "12.3400",
	} {
		d, err := Parse(in)
		if err != nil || d.String() != want {
			t.Errorf("Parse(%q) = %s, %v; want %s", in, d, err, want)
		}
	}
	for _, in := range []string{
		"-NaN", "+nan", "nanx", "infx", "infinit", "- inf", "+ Infinity", "in", "Inf1", "NaN.5",
		"-", "+", "-.", "+e5", "-_1",
	} {
		if _, err := Parse(in); !errors.Is(err, ErrSyntax) {
			t.Errorf("Parse(%q) error = %v, want ErrSyntax", in, err)
		}
	}
}

// TestSpecialRepresentation pins the canonical forms and the accessors
// a special value answers.
func TestSpecialRepresentation(t *testing.T) {
	nan, pinf, ninf := NaN(), Inf(1), Inf(-1)
	if MustParse("nan") != nan || MustParse("inf") != pinf || MustParse("-inf") != ninf {
		t.Fatal("parsed special values are not the canonical structs")
	}
	if Inf(0) != pinf {
		t.Error("Inf(0) is not +Infinity (math.Inf's convention)")
	}
	if nan.Neg() != nan || pinf.Neg() != ninf || ninf.Neg() != pinf || ninf.Abs() != pinf {
		t.Error("Neg/Abs of a special value")
	}
	// numeric → float8 NaN is Postgres's bit pattern (and a stored float8
	// NaN's), not Go's math.NaN(), so binary results match byte for byte.
	if bits := math.Float64bits(nan.Float64()); bits != 0x7FF8000000000000 {
		t.Errorf("NaN.Float64() bits = %#x", bits)
	}
	var zero Decimal
	if !zero.IsFinite() || zero.IsNaN() || zero.IsInf(0) {
		t.Error("the zero value must stay finite 0")
	}
	for _, c := range []struct {
		d                  Decimal
		nan, inf, pos, neg bool
		sign               int
		float              float64
	}{
		{nan, true, false, false, false, 0, math.NaN()},
		{pinf, false, true, true, false, 1, math.Inf(1)},
		{ninf, false, true, false, true, -1, math.Inf(-1)},
	} {
		d := c.d
		if d.IsNaN() != c.nan || d.IsInf(0) != c.inf || d.IsInf(1) != c.pos || d.IsInf(-1) != c.neg || d.IsFinite() {
			t.Errorf("%s: predicates", d)
		}
		if d.Sign() != c.sign || d.IsZero() || d.IsInteger() || d.Scale() != 0 || d.IntDigits() != 0 {
			t.Errorf("%s: Sign/IsZero/IsInteger/Scale/IntDigits", d)
		}
		if f := d.Float64(); f != c.float && !(math.IsNaN(f) && math.IsNaN(c.float)) {
			t.Errorf("%s: Float64 = %v", d, f)
		}
		if _, ok := d.Int64(); ok {
			t.Errorf("%s: Int64 must fail", d)
		}
		if neg, digits, exp := d.Mantissa(); neg || digits != "" || exp != 0 {
			t.Errorf("%s: Mantissa must be empty", d)
		}
		if d.Normalize() != d || d.Round(3) != d || d.Trunc(-2) != d || d.Floor() != d || d.Ceil() != d {
			t.Errorf("%s: rounding must return it unchanged", d)
		}
		if DivScale(d, FromInt64(3)) != 0 || DivScale(FromInt64(3), d) != 0 {
			t.Errorf("%s: DivScale", d)
		}
		f, err := FromFloat64(c.float, -1)
		if err != nil || f != d {
			t.Errorf("FromFloat64(%v) = %s, %v", c.float, f, err)
		}
		f, err = FromFloat64(c.float, 15)
		if err != nil || f != d {
			t.Errorf("FromFloat64(%v, 15) = %s, %v", c.float, f, err)
		}
	}
	// DivRound keeps its target scale for finite / Inf, where Div gives
	// Postgres's scale-0 zero.
	if q, err := MustParse("1.5").DivRound(pinf, 4); err != nil || q.String() != "0.0000" {
		t.Errorf("DivRound(1.5, Inf, 4) = %s, %v", q, err)
	}
	if q, err := MustParse("1.5").Div(ninf); err != nil || q.String() != "0" {
		t.Errorf("1.5 / -Inf = %s, %v", q, err)
	}
	if _, err := pinf.DivRound(zero, 2); !errors.Is(err, ErrDivByZero) {
		t.Errorf("DivRound(Inf, 0) error = %v", err)
	}
}

// TestSpecialTypmod: NaN fits any numeric(p, s); an infinity fits none,
// with Postgres's own DETAIL.
func TestSpecialTypmod(t *testing.T) {
	if d, err := NaN().FitTypmod(5, 2); err != nil || !d.IsNaN() {
		t.Errorf("NaN into numeric(5,2) = %s, %v", d, err)
	}
	for _, d := range []Decimal{Inf(1), Inf(-1)} {
		_, err := d.FitTypmod(5, 2)
		var te *TypmodError
		if !errors.As(err, &te) || !te.Infinite || err.Error() != "numeric field overflow" {
			t.Fatalf("%s into numeric(5,2): %v", d, err)
		}
		if want := "A field with precision 5, scale 2 cannot hold an infinite value."; te.Detail() != want {
			t.Errorf("Detail = %q", te.Detail())
		}
	}
	// The finite DETAIL is unchanged.
	_, err := MustParse("1000").FitTypmod(5, 2)
	var te *TypmodError
	if !errors.As(err, &te) || te.Infinite ||
		te.Detail() != "A field with precision 5, scale 2 must round to an absolute value less than 10^3." {
		t.Errorf("finite typmod error = %v", err)
	}
}

// TestSumSpecial: specials are counted beside the finite total, and the
// total follows addition's rules whatever order they arrive in.
func TestSumSpecial(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{[]string{"1.5", "2"}, "3.5"},
		{[]string{"1.5", "Infinity", "2"}, "Infinity"},
		{[]string{"-Infinity", "1.5"}, "-Infinity"},
		{[]string{"Infinity", "1.5", "-Infinity"}, "NaN"},
		{[]string{"1", "NaN", "Infinity"}, "NaN"},
		{[]string{"Infinity", "Infinity"}, "Infinity"},
		{[]string{"NaN"}, "NaN"},
		// The finite total keeps going underneath: past int64 into the
		// wide form while an infinity is pending.
		{[]string{"Infinity", "9223372036854775807", "9223372036854775807"}, "Infinity"},
	} {
		var s Sum
		for _, x := range c.in {
			if err := s.Add(MustParse(x)); err != nil {
				t.Fatal(err)
			}
		}
		if got := s.Value().String(); got != c.want {
			t.Errorf("Sum%v = %s, want %s", c.in, got, c.want)
		}
		// Value agrees with folding the inputs through Add.
		acc := Decimal{}
		for _, x := range c.in {
			acc, _ = acc.Add(MustParse(x))
		}
		if acc.Cmp(s.Value()) != 0 {
			t.Errorf("Sum%v = %s, Add fold %s", c.in, s.Value(), acc)
		}
	}
	var s Sum
	_ = s.Add(Inf(1))
	_ = s.AddInt64(5)
	if !s.Value().IsInf(1) {
		t.Error("AddInt64 after an infinity")
	}
}

// pgSpecialPairs: {op, a, b, Postgres 16's result}.
var pgSpecialPairs = [][4]string{
	{"+", "NaN", "NaN", "NaN"},
	{"+", "NaN", "Infinity", "NaN"},
	{"+", "NaN", "-Infinity", "NaN"},
	{"+", "NaN", "0", "NaN"},
	{"+", "NaN", "0.00", "NaN"},
	{"+", "NaN", "1", "NaN"},
	{"+", "NaN", "1.0", "NaN"},
	{"+", "NaN", "-1", "NaN"},
	{"+", "NaN", "2", "NaN"},
	{"+", "NaN", "-2", "NaN"},
	{"+", "NaN", "0.5", "NaN"},
	{"+", "NaN", "-0.5", "NaN"},
	{"+", "NaN", "2.5", "NaN"},
	{"+", "NaN", "-3", "NaN"},
	{"+", "NaN", "3", "NaN"},
	{"+", "NaN", "100000000000000000000", "NaN"},
	{"+", "NaN", "-12345678901234567890.5", "NaN"},
	{"+", "Infinity", "NaN", "NaN"},
	{"+", "Infinity", "Infinity", "Infinity"},
	{"+", "Infinity", "-Infinity", "NaN"},
	{"+", "Infinity", "0", "Infinity"},
	{"+", "Infinity", "0.00", "Infinity"},
	{"+", "Infinity", "1", "Infinity"},
	{"+", "Infinity", "1.0", "Infinity"},
	{"+", "Infinity", "-1", "Infinity"},
	{"+", "Infinity", "2", "Infinity"},
	{"+", "Infinity", "-2", "Infinity"},
	{"+", "Infinity", "0.5", "Infinity"},
	{"+", "Infinity", "-0.5", "Infinity"},
	{"+", "Infinity", "2.5", "Infinity"},
	{"+", "Infinity", "-3", "Infinity"},
	{"+", "Infinity", "3", "Infinity"},
	{"+", "Infinity", "100000000000000000000", "Infinity"},
	{"+", "Infinity", "-12345678901234567890.5", "Infinity"},
	{"+", "-Infinity", "NaN", "NaN"},
	{"+", "-Infinity", "Infinity", "NaN"},
	{"+", "-Infinity", "-Infinity", "-Infinity"},
	{"+", "-Infinity", "0", "-Infinity"},
	{"+", "-Infinity", "0.00", "-Infinity"},
	{"+", "-Infinity", "1", "-Infinity"},
	{"+", "-Infinity", "1.0", "-Infinity"},
	{"+", "-Infinity", "-1", "-Infinity"},
	{"+", "-Infinity", "2", "-Infinity"},
	{"+", "-Infinity", "-2", "-Infinity"},
	{"+", "-Infinity", "0.5", "-Infinity"},
	{"+", "-Infinity", "-0.5", "-Infinity"},
	{"+", "-Infinity", "2.5", "-Infinity"},
	{"+", "-Infinity", "-3", "-Infinity"},
	{"+", "-Infinity", "3", "-Infinity"},
	{"+", "-Infinity", "100000000000000000000", "-Infinity"},
	{"+", "-Infinity", "-12345678901234567890.5", "-Infinity"},
	{"+", "0", "NaN", "NaN"},
	{"+", "0", "Infinity", "Infinity"},
	{"+", "0", "-Infinity", "-Infinity"},
	{"+", "0.00", "NaN", "NaN"},
	{"+", "0.00", "Infinity", "Infinity"},
	{"+", "0.00", "-Infinity", "-Infinity"},
	{"+", "1", "NaN", "NaN"},
	{"+", "1", "Infinity", "Infinity"},
	{"+", "1", "-Infinity", "-Infinity"},
	{"+", "1.0", "NaN", "NaN"},
	{"+", "1.0", "Infinity", "Infinity"},
	{"+", "1.0", "-Infinity", "-Infinity"},
	{"+", "-1", "NaN", "NaN"},
	{"+", "-1", "Infinity", "Infinity"},
	{"+", "-1", "-Infinity", "-Infinity"},
	{"+", "2", "NaN", "NaN"},
	{"+", "2", "Infinity", "Infinity"},
	{"+", "2", "-Infinity", "-Infinity"},
	{"+", "-2", "NaN", "NaN"},
	{"+", "-2", "Infinity", "Infinity"},
	{"+", "-2", "-Infinity", "-Infinity"},
	{"+", "0.5", "NaN", "NaN"},
	{"+", "0.5", "Infinity", "Infinity"},
	{"+", "0.5", "-Infinity", "-Infinity"},
	{"+", "-0.5", "NaN", "NaN"},
	{"+", "-0.5", "Infinity", "Infinity"},
	{"+", "-0.5", "-Infinity", "-Infinity"},
	{"+", "2.5", "NaN", "NaN"},
	{"+", "2.5", "Infinity", "Infinity"},
	{"+", "2.5", "-Infinity", "-Infinity"},
	{"+", "-3", "NaN", "NaN"},
	{"+", "-3", "Infinity", "Infinity"},
	{"+", "-3", "-Infinity", "-Infinity"},
	{"+", "3", "NaN", "NaN"},
	{"+", "3", "Infinity", "Infinity"},
	{"+", "3", "-Infinity", "-Infinity"},
	{"+", "100000000000000000000", "NaN", "NaN"},
	{"+", "100000000000000000000", "Infinity", "Infinity"},
	{"+", "100000000000000000000", "-Infinity", "-Infinity"},
	{"+", "-12345678901234567890.5", "NaN", "NaN"},
	{"+", "-12345678901234567890.5", "Infinity", "Infinity"},
	{"+", "-12345678901234567890.5", "-Infinity", "-Infinity"},
	{"-", "NaN", "NaN", "NaN"},
	{"-", "NaN", "Infinity", "NaN"},
	{"-", "NaN", "-Infinity", "NaN"},
	{"-", "NaN", "0", "NaN"},
	{"-", "NaN", "0.00", "NaN"},
	{"-", "NaN", "1", "NaN"},
	{"-", "NaN", "1.0", "NaN"},
	{"-", "NaN", "-1", "NaN"},
	{"-", "NaN", "2", "NaN"},
	{"-", "NaN", "-2", "NaN"},
	{"-", "NaN", "0.5", "NaN"},
	{"-", "NaN", "-0.5", "NaN"},
	{"-", "NaN", "2.5", "NaN"},
	{"-", "NaN", "-3", "NaN"},
	{"-", "NaN", "3", "NaN"},
	{"-", "NaN", "100000000000000000000", "NaN"},
	{"-", "NaN", "-12345678901234567890.5", "NaN"},
	{"-", "Infinity", "NaN", "NaN"},
	{"-", "Infinity", "Infinity", "NaN"},
	{"-", "Infinity", "-Infinity", "Infinity"},
	{"-", "Infinity", "0", "Infinity"},
	{"-", "Infinity", "0.00", "Infinity"},
	{"-", "Infinity", "1", "Infinity"},
	{"-", "Infinity", "1.0", "Infinity"},
	{"-", "Infinity", "-1", "Infinity"},
	{"-", "Infinity", "2", "Infinity"},
	{"-", "Infinity", "-2", "Infinity"},
	{"-", "Infinity", "0.5", "Infinity"},
	{"-", "Infinity", "-0.5", "Infinity"},
	{"-", "Infinity", "2.5", "Infinity"},
	{"-", "Infinity", "-3", "Infinity"},
	{"-", "Infinity", "3", "Infinity"},
	{"-", "Infinity", "100000000000000000000", "Infinity"},
	{"-", "Infinity", "-12345678901234567890.5", "Infinity"},
	{"-", "-Infinity", "NaN", "NaN"},
	{"-", "-Infinity", "Infinity", "-Infinity"},
	{"-", "-Infinity", "-Infinity", "NaN"},
	{"-", "-Infinity", "0", "-Infinity"},
	{"-", "-Infinity", "0.00", "-Infinity"},
	{"-", "-Infinity", "1", "-Infinity"},
	{"-", "-Infinity", "1.0", "-Infinity"},
	{"-", "-Infinity", "-1", "-Infinity"},
	{"-", "-Infinity", "2", "-Infinity"},
	{"-", "-Infinity", "-2", "-Infinity"},
	{"-", "-Infinity", "0.5", "-Infinity"},
	{"-", "-Infinity", "-0.5", "-Infinity"},
	{"-", "-Infinity", "2.5", "-Infinity"},
	{"-", "-Infinity", "-3", "-Infinity"},
	{"-", "-Infinity", "3", "-Infinity"},
	{"-", "-Infinity", "100000000000000000000", "-Infinity"},
	{"-", "-Infinity", "-12345678901234567890.5", "-Infinity"},
	{"-", "0", "NaN", "NaN"},
	{"-", "0", "Infinity", "-Infinity"},
	{"-", "0", "-Infinity", "Infinity"},
	{"-", "0.00", "NaN", "NaN"},
	{"-", "0.00", "Infinity", "-Infinity"},
	{"-", "0.00", "-Infinity", "Infinity"},
	{"-", "1", "NaN", "NaN"},
	{"-", "1", "Infinity", "-Infinity"},
	{"-", "1", "-Infinity", "Infinity"},
	{"-", "1.0", "NaN", "NaN"},
	{"-", "1.0", "Infinity", "-Infinity"},
	{"-", "1.0", "-Infinity", "Infinity"},
	{"-", "-1", "NaN", "NaN"},
	{"-", "-1", "Infinity", "-Infinity"},
	{"-", "-1", "-Infinity", "Infinity"},
	{"-", "2", "NaN", "NaN"},
	{"-", "2", "Infinity", "-Infinity"},
	{"-", "2", "-Infinity", "Infinity"},
	{"-", "-2", "NaN", "NaN"},
	{"-", "-2", "Infinity", "-Infinity"},
	{"-", "-2", "-Infinity", "Infinity"},
	{"-", "0.5", "NaN", "NaN"},
	{"-", "0.5", "Infinity", "-Infinity"},
	{"-", "0.5", "-Infinity", "Infinity"},
	{"-", "-0.5", "NaN", "NaN"},
	{"-", "-0.5", "Infinity", "-Infinity"},
	{"-", "-0.5", "-Infinity", "Infinity"},
	{"-", "2.5", "NaN", "NaN"},
	{"-", "2.5", "Infinity", "-Infinity"},
	{"-", "2.5", "-Infinity", "Infinity"},
	{"-", "-3", "NaN", "NaN"},
	{"-", "-3", "Infinity", "-Infinity"},
	{"-", "-3", "-Infinity", "Infinity"},
	{"-", "3", "NaN", "NaN"},
	{"-", "3", "Infinity", "-Infinity"},
	{"-", "3", "-Infinity", "Infinity"},
	{"-", "100000000000000000000", "NaN", "NaN"},
	{"-", "100000000000000000000", "Infinity", "-Infinity"},
	{"-", "100000000000000000000", "-Infinity", "Infinity"},
	{"-", "-12345678901234567890.5", "NaN", "NaN"},
	{"-", "-12345678901234567890.5", "Infinity", "-Infinity"},
	{"-", "-12345678901234567890.5", "-Infinity", "Infinity"},
	{"*", "NaN", "NaN", "NaN"},
	{"*", "NaN", "Infinity", "NaN"},
	{"*", "NaN", "-Infinity", "NaN"},
	{"*", "NaN", "0", "NaN"},
	{"*", "NaN", "0.00", "NaN"},
	{"*", "NaN", "1", "NaN"},
	{"*", "NaN", "1.0", "NaN"},
	{"*", "NaN", "-1", "NaN"},
	{"*", "NaN", "2", "NaN"},
	{"*", "NaN", "-2", "NaN"},
	{"*", "NaN", "0.5", "NaN"},
	{"*", "NaN", "-0.5", "NaN"},
	{"*", "NaN", "2.5", "NaN"},
	{"*", "NaN", "-3", "NaN"},
	{"*", "NaN", "3", "NaN"},
	{"*", "NaN", "100000000000000000000", "NaN"},
	{"*", "NaN", "-12345678901234567890.5", "NaN"},
	{"*", "Infinity", "NaN", "NaN"},
	{"*", "Infinity", "Infinity", "Infinity"},
	{"*", "Infinity", "-Infinity", "-Infinity"},
	{"*", "Infinity", "0", "NaN"},
	{"*", "Infinity", "0.00", "NaN"},
	{"*", "Infinity", "1", "Infinity"},
	{"*", "Infinity", "1.0", "Infinity"},
	{"*", "Infinity", "-1", "-Infinity"},
	{"*", "Infinity", "2", "Infinity"},
	{"*", "Infinity", "-2", "-Infinity"},
	{"*", "Infinity", "0.5", "Infinity"},
	{"*", "Infinity", "-0.5", "-Infinity"},
	{"*", "Infinity", "2.5", "Infinity"},
	{"*", "Infinity", "-3", "-Infinity"},
	{"*", "Infinity", "3", "Infinity"},
	{"*", "Infinity", "100000000000000000000", "Infinity"},
	{"*", "Infinity", "-12345678901234567890.5", "-Infinity"},
	{"*", "-Infinity", "NaN", "NaN"},
	{"*", "-Infinity", "Infinity", "-Infinity"},
	{"*", "-Infinity", "-Infinity", "Infinity"},
	{"*", "-Infinity", "0", "NaN"},
	{"*", "-Infinity", "0.00", "NaN"},
	{"*", "-Infinity", "1", "-Infinity"},
	{"*", "-Infinity", "1.0", "-Infinity"},
	{"*", "-Infinity", "-1", "Infinity"},
	{"*", "-Infinity", "2", "-Infinity"},
	{"*", "-Infinity", "-2", "Infinity"},
	{"*", "-Infinity", "0.5", "-Infinity"},
	{"*", "-Infinity", "-0.5", "Infinity"},
	{"*", "-Infinity", "2.5", "-Infinity"},
	{"*", "-Infinity", "-3", "Infinity"},
	{"*", "-Infinity", "3", "-Infinity"},
	{"*", "-Infinity", "100000000000000000000", "-Infinity"},
	{"*", "-Infinity", "-12345678901234567890.5", "Infinity"},
	{"*", "0", "NaN", "NaN"},
	{"*", "0", "Infinity", "NaN"},
	{"*", "0", "-Infinity", "NaN"},
	{"*", "0.00", "NaN", "NaN"},
	{"*", "0.00", "Infinity", "NaN"},
	{"*", "0.00", "-Infinity", "NaN"},
	{"*", "1", "NaN", "NaN"},
	{"*", "1", "Infinity", "Infinity"},
	{"*", "1", "-Infinity", "-Infinity"},
	{"*", "1.0", "NaN", "NaN"},
	{"*", "1.0", "Infinity", "Infinity"},
	{"*", "1.0", "-Infinity", "-Infinity"},
	{"*", "-1", "NaN", "NaN"},
	{"*", "-1", "Infinity", "-Infinity"},
	{"*", "-1", "-Infinity", "Infinity"},
	{"*", "2", "NaN", "NaN"},
	{"*", "2", "Infinity", "Infinity"},
	{"*", "2", "-Infinity", "-Infinity"},
	{"*", "-2", "NaN", "NaN"},
	{"*", "-2", "Infinity", "-Infinity"},
	{"*", "-2", "-Infinity", "Infinity"},
	{"*", "0.5", "NaN", "NaN"},
	{"*", "0.5", "Infinity", "Infinity"},
	{"*", "0.5", "-Infinity", "-Infinity"},
	{"*", "-0.5", "NaN", "NaN"},
	{"*", "-0.5", "Infinity", "-Infinity"},
	{"*", "-0.5", "-Infinity", "Infinity"},
	{"*", "2.5", "NaN", "NaN"},
	{"*", "2.5", "Infinity", "Infinity"},
	{"*", "2.5", "-Infinity", "-Infinity"},
	{"*", "-3", "NaN", "NaN"},
	{"*", "-3", "Infinity", "-Infinity"},
	{"*", "-3", "-Infinity", "Infinity"},
	{"*", "3", "NaN", "NaN"},
	{"*", "3", "Infinity", "Infinity"},
	{"*", "3", "-Infinity", "-Infinity"},
	{"*", "100000000000000000000", "NaN", "NaN"},
	{"*", "100000000000000000000", "Infinity", "Infinity"},
	{"*", "100000000000000000000", "-Infinity", "-Infinity"},
	{"*", "-12345678901234567890.5", "NaN", "NaN"},
	{"*", "-12345678901234567890.5", "Infinity", "-Infinity"},
	{"*", "-12345678901234567890.5", "-Infinity", "Infinity"},
	{"/", "NaN", "NaN", "NaN"},
	{"/", "NaN", "Infinity", "NaN"},
	{"/", "NaN", "-Infinity", "NaN"},
	{"/", "NaN", "0", "NaN"},
	{"/", "NaN", "0.00", "NaN"},
	{"/", "NaN", "1", "NaN"},
	{"/", "NaN", "1.0", "NaN"},
	{"/", "NaN", "-1", "NaN"},
	{"/", "NaN", "2", "NaN"},
	{"/", "NaN", "-2", "NaN"},
	{"/", "NaN", "0.5", "NaN"},
	{"/", "NaN", "-0.5", "NaN"},
	{"/", "NaN", "2.5", "NaN"},
	{"/", "NaN", "-3", "NaN"},
	{"/", "NaN", "3", "NaN"},
	{"/", "NaN", "100000000000000000000", "NaN"},
	{"/", "NaN", "-12345678901234567890.5", "NaN"},
	{"/", "Infinity", "NaN", "NaN"},
	{"/", "Infinity", "Infinity", "NaN"},
	{"/", "Infinity", "-Infinity", "NaN"},
	{"/", "Infinity", "0", "ERROR: division by zero"},
	{"/", "Infinity", "0.00", "ERROR: division by zero"},
	{"/", "Infinity", "1", "Infinity"},
	{"/", "Infinity", "1.0", "Infinity"},
	{"/", "Infinity", "-1", "-Infinity"},
	{"/", "Infinity", "2", "Infinity"},
	{"/", "Infinity", "-2", "-Infinity"},
	{"/", "Infinity", "0.5", "Infinity"},
	{"/", "Infinity", "-0.5", "-Infinity"},
	{"/", "Infinity", "2.5", "Infinity"},
	{"/", "Infinity", "-3", "-Infinity"},
	{"/", "Infinity", "3", "Infinity"},
	{"/", "Infinity", "100000000000000000000", "Infinity"},
	{"/", "Infinity", "-12345678901234567890.5", "-Infinity"},
	{"/", "-Infinity", "NaN", "NaN"},
	{"/", "-Infinity", "Infinity", "NaN"},
	{"/", "-Infinity", "-Infinity", "NaN"},
	{"/", "-Infinity", "0", "ERROR: division by zero"},
	{"/", "-Infinity", "0.00", "ERROR: division by zero"},
	{"/", "-Infinity", "1", "-Infinity"},
	{"/", "-Infinity", "1.0", "-Infinity"},
	{"/", "-Infinity", "-1", "Infinity"},
	{"/", "-Infinity", "2", "-Infinity"},
	{"/", "-Infinity", "-2", "Infinity"},
	{"/", "-Infinity", "0.5", "-Infinity"},
	{"/", "-Infinity", "-0.5", "Infinity"},
	{"/", "-Infinity", "2.5", "-Infinity"},
	{"/", "-Infinity", "-3", "Infinity"},
	{"/", "-Infinity", "3", "-Infinity"},
	{"/", "-Infinity", "100000000000000000000", "-Infinity"},
	{"/", "-Infinity", "-12345678901234567890.5", "Infinity"},
	{"/", "0", "NaN", "NaN"},
	{"/", "0", "Infinity", "0"},
	{"/", "0", "-Infinity", "0"},
	{"/", "0.00", "NaN", "NaN"},
	{"/", "0.00", "Infinity", "0"},
	{"/", "0.00", "-Infinity", "0"},
	{"/", "1", "NaN", "NaN"},
	{"/", "1", "Infinity", "0"},
	{"/", "1", "-Infinity", "0"},
	{"/", "1.0", "NaN", "NaN"},
	{"/", "1.0", "Infinity", "0"},
	{"/", "1.0", "-Infinity", "0"},
	{"/", "-1", "NaN", "NaN"},
	{"/", "-1", "Infinity", "0"},
	{"/", "-1", "-Infinity", "0"},
	{"/", "2", "NaN", "NaN"},
	{"/", "2", "Infinity", "0"},
	{"/", "2", "-Infinity", "0"},
	{"/", "-2", "NaN", "NaN"},
	{"/", "-2", "Infinity", "0"},
	{"/", "-2", "-Infinity", "0"},
	{"/", "0.5", "NaN", "NaN"},
	{"/", "0.5", "Infinity", "0"},
	{"/", "0.5", "-Infinity", "0"},
	{"/", "-0.5", "NaN", "NaN"},
	{"/", "-0.5", "Infinity", "0"},
	{"/", "-0.5", "-Infinity", "0"},
	{"/", "2.5", "NaN", "NaN"},
	{"/", "2.5", "Infinity", "0"},
	{"/", "2.5", "-Infinity", "0"},
	{"/", "-3", "NaN", "NaN"},
	{"/", "-3", "Infinity", "0"},
	{"/", "-3", "-Infinity", "0"},
	{"/", "3", "NaN", "NaN"},
	{"/", "3", "Infinity", "0"},
	{"/", "3", "-Infinity", "0"},
	{"/", "100000000000000000000", "NaN", "NaN"},
	{"/", "100000000000000000000", "Infinity", "0"},
	{"/", "100000000000000000000", "-Infinity", "0"},
	{"/", "-12345678901234567890.5", "NaN", "NaN"},
	{"/", "-12345678901234567890.5", "Infinity", "0"},
	{"/", "-12345678901234567890.5", "-Infinity", "0"},
	{"mod", "NaN", "NaN", "NaN"},
	{"mod", "NaN", "Infinity", "NaN"},
	{"mod", "NaN", "-Infinity", "NaN"},
	{"mod", "NaN", "0", "NaN"},
	{"mod", "NaN", "0.00", "NaN"},
	{"mod", "NaN", "1", "NaN"},
	{"mod", "NaN", "1.0", "NaN"},
	{"mod", "NaN", "-1", "NaN"},
	{"mod", "NaN", "2", "NaN"},
	{"mod", "NaN", "-2", "NaN"},
	{"mod", "NaN", "0.5", "NaN"},
	{"mod", "NaN", "-0.5", "NaN"},
	{"mod", "NaN", "2.5", "NaN"},
	{"mod", "NaN", "-3", "NaN"},
	{"mod", "NaN", "3", "NaN"},
	{"mod", "NaN", "100000000000000000000", "NaN"},
	{"mod", "NaN", "-12345678901234567890.5", "NaN"},
	{"mod", "Infinity", "NaN", "NaN"},
	{"mod", "Infinity", "Infinity", "NaN"},
	{"mod", "Infinity", "-Infinity", "NaN"},
	{"mod", "Infinity", "0", "ERROR: division by zero"},
	{"mod", "Infinity", "0.00", "ERROR: division by zero"},
	{"mod", "Infinity", "1", "NaN"},
	{"mod", "Infinity", "1.0", "NaN"},
	{"mod", "Infinity", "-1", "NaN"},
	{"mod", "Infinity", "2", "NaN"},
	{"mod", "Infinity", "-2", "NaN"},
	{"mod", "Infinity", "0.5", "NaN"},
	{"mod", "Infinity", "-0.5", "NaN"},
	{"mod", "Infinity", "2.5", "NaN"},
	{"mod", "Infinity", "-3", "NaN"},
	{"mod", "Infinity", "3", "NaN"},
	{"mod", "Infinity", "100000000000000000000", "NaN"},
	{"mod", "Infinity", "-12345678901234567890.5", "NaN"},
	{"mod", "-Infinity", "NaN", "NaN"},
	{"mod", "-Infinity", "Infinity", "NaN"},
	{"mod", "-Infinity", "-Infinity", "NaN"},
	{"mod", "-Infinity", "0", "ERROR: division by zero"},
	{"mod", "-Infinity", "0.00", "ERROR: division by zero"},
	{"mod", "-Infinity", "1", "NaN"},
	{"mod", "-Infinity", "1.0", "NaN"},
	{"mod", "-Infinity", "-1", "NaN"},
	{"mod", "-Infinity", "2", "NaN"},
	{"mod", "-Infinity", "-2", "NaN"},
	{"mod", "-Infinity", "0.5", "NaN"},
	{"mod", "-Infinity", "-0.5", "NaN"},
	{"mod", "-Infinity", "2.5", "NaN"},
	{"mod", "-Infinity", "-3", "NaN"},
	{"mod", "-Infinity", "3", "NaN"},
	{"mod", "-Infinity", "100000000000000000000", "NaN"},
	{"mod", "-Infinity", "-12345678901234567890.5", "NaN"},
	{"mod", "0", "NaN", "NaN"},
	{"mod", "0", "Infinity", "0"},
	{"mod", "0", "-Infinity", "0"},
	{"mod", "0.00", "NaN", "NaN"},
	{"mod", "0.00", "Infinity", "0.00"},
	{"mod", "0.00", "-Infinity", "0.00"},
	{"mod", "1", "NaN", "NaN"},
	{"mod", "1", "Infinity", "1"},
	{"mod", "1", "-Infinity", "1"},
	{"mod", "1.0", "NaN", "NaN"},
	{"mod", "1.0", "Infinity", "1.0"},
	{"mod", "1.0", "-Infinity", "1.0"},
	{"mod", "-1", "NaN", "NaN"},
	{"mod", "-1", "Infinity", "-1"},
	{"mod", "-1", "-Infinity", "-1"},
	{"mod", "2", "NaN", "NaN"},
	{"mod", "2", "Infinity", "2"},
	{"mod", "2", "-Infinity", "2"},
	{"mod", "-2", "NaN", "NaN"},
	{"mod", "-2", "Infinity", "-2"},
	{"mod", "-2", "-Infinity", "-2"},
	{"mod", "0.5", "NaN", "NaN"},
	{"mod", "0.5", "Infinity", "0.5"},
	{"mod", "0.5", "-Infinity", "0.5"},
	{"mod", "-0.5", "NaN", "NaN"},
	{"mod", "-0.5", "Infinity", "-0.5"},
	{"mod", "-0.5", "-Infinity", "-0.5"},
	{"mod", "2.5", "NaN", "NaN"},
	{"mod", "2.5", "Infinity", "2.5"},
	{"mod", "2.5", "-Infinity", "2.5"},
	{"mod", "-3", "NaN", "NaN"},
	{"mod", "-3", "Infinity", "-3"},
	{"mod", "-3", "-Infinity", "-3"},
	{"mod", "3", "NaN", "NaN"},
	{"mod", "3", "Infinity", "3"},
	{"mod", "3", "-Infinity", "3"},
	{"mod", "100000000000000000000", "NaN", "NaN"},
	{"mod", "100000000000000000000", "Infinity", "100000000000000000000"},
	{"mod", "100000000000000000000", "-Infinity", "100000000000000000000"},
	{"mod", "-12345678901234567890.5", "NaN", "NaN"},
	{"mod", "-12345678901234567890.5", "Infinity", "-12345678901234567890.5"},
	{"mod", "-12345678901234567890.5", "-Infinity", "-12345678901234567890.5"},
	{"power", "NaN", "NaN", "NaN"},
	{"power", "NaN", "Infinity", "NaN"},
	{"power", "NaN", "-Infinity", "NaN"},
	{"power", "NaN", "0", "1"},
	{"power", "NaN", "0.00", "1"},
	{"power", "NaN", "1", "NaN"},
	{"power", "NaN", "1.0", "NaN"},
	{"power", "NaN", "-1", "NaN"},
	{"power", "NaN", "2", "NaN"},
	{"power", "NaN", "-2", "NaN"},
	{"power", "NaN", "0.5", "NaN"},
	{"power", "NaN", "-0.5", "NaN"},
	{"power", "NaN", "2.5", "NaN"},
	{"power", "NaN", "-3", "NaN"},
	{"power", "NaN", "3", "NaN"},
	{"power", "NaN", "100000000000000000000", "NaN"},
	{"power", "NaN", "-12345678901234567890.5", "NaN"},
	{"power", "Infinity", "NaN", "NaN"},
	{"power", "Infinity", "Infinity", "Infinity"},
	{"power", "Infinity", "-Infinity", "0"},
	{"power", "Infinity", "0", "1"},
	{"power", "Infinity", "0.00", "1"},
	{"power", "Infinity", "1", "Infinity"},
	{"power", "Infinity", "1.0", "Infinity"},
	{"power", "Infinity", "-1", "0"},
	{"power", "Infinity", "2", "Infinity"},
	{"power", "Infinity", "-2", "0"},
	{"power", "Infinity", "0.5", "Infinity"},
	{"power", "Infinity", "-0.5", "0"},
	{"power", "Infinity", "2.5", "Infinity"},
	{"power", "Infinity", "-3", "0"},
	{"power", "Infinity", "3", "Infinity"},
	{"power", "Infinity", "100000000000000000000", "Infinity"},
	{"power", "Infinity", "-12345678901234567890.5", "0"},
	{"power", "-Infinity", "NaN", "NaN"},
	{"power", "-Infinity", "Infinity", "Infinity"},
	{"power", "-Infinity", "-Infinity", "0"},
	{"power", "-Infinity", "0", "1"},
	{"power", "-Infinity", "0.00", "1"},
	{"power", "-Infinity", "1", "-Infinity"},
	{"power", "-Infinity", "1.0", "-Infinity"},
	{"power", "-Infinity", "-1", "0"},
	{"power", "-Infinity", "2", "Infinity"},
	{"power", "-Infinity", "-2", "0"},
	{"power", "-Infinity", "0.5", "ERROR: a negative number raised to a non-integer power yields a complex result"},
	{"power", "-Infinity", "-0.5", "ERROR: a negative number raised to a non-integer power yields a complex result"},
	{"power", "-Infinity", "2.5", "ERROR: a negative number raised to a non-integer power yields a complex result"},
	{"power", "-Infinity", "-3", "0"},
	{"power", "-Infinity", "3", "-Infinity"},
	{"power", "-Infinity", "100000000000000000000", "Infinity"},
	{"power", "-Infinity", "-12345678901234567890.5", "ERROR: a negative number raised to a non-integer power yields a complex result"},
	{"power", "0", "NaN", "NaN"},
	{"power", "0", "Infinity", "0"},
	{"power", "0", "-Infinity", "ERROR: zero raised to a negative power is undefined"},
	{"power", "0.00", "NaN", "NaN"},
	{"power", "0.00", "Infinity", "0"},
	{"power", "0.00", "-Infinity", "ERROR: zero raised to a negative power is undefined"},
	{"power", "1", "NaN", "1"},
	{"power", "1", "Infinity", "1"},
	{"power", "1", "-Infinity", "1"},
	{"power", "1.0", "NaN", "1"},
	{"power", "1.0", "Infinity", "1"},
	{"power", "1.0", "-Infinity", "1"},
	{"power", "-1", "NaN", "NaN"},
	{"power", "-1", "Infinity", "1"},
	{"power", "-1", "-Infinity", "1"},
	{"power", "2", "NaN", "NaN"},
	{"power", "2", "Infinity", "Infinity"},
	{"power", "2", "-Infinity", "0"},
	{"power", "-2", "NaN", "NaN"},
	{"power", "-2", "Infinity", "Infinity"},
	{"power", "-2", "-Infinity", "0"},
	{"power", "0.5", "NaN", "NaN"},
	{"power", "0.5", "Infinity", "0"},
	{"power", "0.5", "-Infinity", "Infinity"},
	{"power", "-0.5", "NaN", "NaN"},
	{"power", "-0.5", "Infinity", "0"},
	{"power", "-0.5", "-Infinity", "Infinity"},
	{"power", "2.5", "NaN", "NaN"},
	{"power", "2.5", "Infinity", "Infinity"},
	{"power", "2.5", "-Infinity", "0"},
	{"power", "-3", "NaN", "NaN"},
	{"power", "-3", "Infinity", "Infinity"},
	{"power", "-3", "-Infinity", "0"},
	{"power", "3", "NaN", "NaN"},
	{"power", "3", "Infinity", "Infinity"},
	{"power", "3", "-Infinity", "0"},
	{"power", "100000000000000000000", "NaN", "NaN"},
	{"power", "100000000000000000000", "Infinity", "Infinity"},
	{"power", "100000000000000000000", "-Infinity", "0"},
	{"power", "-12345678901234567890.5", "NaN", "NaN"},
	{"power", "-12345678901234567890.5", "Infinity", "Infinity"},
	{"power", "-12345678901234567890.5", "-Infinity", "0"},
	{"log", "NaN", "NaN", "NaN"},
	{"log", "NaN", "Infinity", "NaN"},
	{"log", "NaN", "-Infinity", "NaN"},
	{"log", "NaN", "0", "NaN"},
	{"log", "NaN", "0.00", "NaN"},
	{"log", "NaN", "1", "NaN"},
	{"log", "NaN", "1.0", "NaN"},
	{"log", "NaN", "-1", "NaN"},
	{"log", "NaN", "2", "NaN"},
	{"log", "NaN", "-2", "NaN"},
	{"log", "NaN", "0.5", "NaN"},
	{"log", "NaN", "-0.5", "NaN"},
	{"log", "NaN", "2.5", "NaN"},
	{"log", "NaN", "-3", "NaN"},
	{"log", "NaN", "3", "NaN"},
	{"log", "NaN", "100000000000000000000", "NaN"},
	{"log", "NaN", "-12345678901234567890.5", "NaN"},
	{"log", "Infinity", "NaN", "NaN"},
	{"log", "Infinity", "Infinity", "NaN"},
	{"log", "Infinity", "-Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "Infinity", "0", "ERROR: cannot take logarithm of zero"},
	{"log", "Infinity", "0.00", "ERROR: cannot take logarithm of zero"},
	{"log", "Infinity", "1", "0"},
	{"log", "Infinity", "1.0", "0"},
	{"log", "Infinity", "-1", "ERROR: cannot take logarithm of a negative number"},
	{"log", "Infinity", "2", "0"},
	{"log", "Infinity", "-2", "ERROR: cannot take logarithm of a negative number"},
	{"log", "Infinity", "0.5", "0"},
	{"log", "Infinity", "-0.5", "ERROR: cannot take logarithm of a negative number"},
	{"log", "Infinity", "2.5", "0"},
	{"log", "Infinity", "-3", "ERROR: cannot take logarithm of a negative number"},
	{"log", "Infinity", "3", "0"},
	{"log", "Infinity", "100000000000000000000", "0"},
	{"log", "Infinity", "-12345678901234567890.5", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-Infinity", "NaN", "NaN"},
	{"log", "-Infinity", "Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-Infinity", "-Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-Infinity", "0", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-Infinity", "0.00", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-Infinity", "1", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-Infinity", "1.0", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-Infinity", "-1", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-Infinity", "2", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-Infinity", "-2", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-Infinity", "0.5", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-Infinity", "-0.5", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-Infinity", "2.5", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-Infinity", "-3", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-Infinity", "3", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-Infinity", "100000000000000000000", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-Infinity", "-12345678901234567890.5", "ERROR: cannot take logarithm of a negative number"},
	{"log", "0", "NaN", "NaN"},
	{"log", "0", "Infinity", "ERROR: cannot take logarithm of zero"},
	{"log", "0", "-Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "0.00", "NaN", "NaN"},
	{"log", "0.00", "Infinity", "ERROR: cannot take logarithm of zero"},
	{"log", "0.00", "-Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "1", "NaN", "NaN"},
	{"log", "1", "Infinity", "Infinity"},
	{"log", "1", "-Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "1.0", "NaN", "NaN"},
	{"log", "1.0", "Infinity", "Infinity"},
	{"log", "1.0", "-Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-1", "NaN", "NaN"},
	{"log", "-1", "Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-1", "-Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "2", "NaN", "NaN"},
	{"log", "2", "Infinity", "Infinity"},
	{"log", "2", "-Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-2", "NaN", "NaN"},
	{"log", "-2", "Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-2", "-Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "0.5", "NaN", "NaN"},
	{"log", "0.5", "Infinity", "Infinity"},
	{"log", "0.5", "-Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-0.5", "NaN", "NaN"},
	{"log", "-0.5", "Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-0.5", "-Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "2.5", "NaN", "NaN"},
	{"log", "2.5", "Infinity", "Infinity"},
	{"log", "2.5", "-Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-3", "NaN", "NaN"},
	{"log", "-3", "Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-3", "-Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "3", "NaN", "NaN"},
	{"log", "3", "Infinity", "Infinity"},
	{"log", "3", "-Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "100000000000000000000", "NaN", "NaN"},
	{"log", "100000000000000000000", "Infinity", "Infinity"},
	{"log", "100000000000000000000", "-Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-12345678901234567890.5", "NaN", "NaN"},
	{"log", "-12345678901234567890.5", "Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "-12345678901234567890.5", "-Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"cmp", "NaN", "NaN", "0"},
	{"cmp", "NaN", "Infinity", "1"},
	{"cmp", "NaN", "-Infinity", "1"},
	{"cmp", "NaN", "0", "1"},
	{"cmp", "NaN", "0.00", "1"},
	{"cmp", "NaN", "1", "1"},
	{"cmp", "NaN", "1.0", "1"},
	{"cmp", "NaN", "-1", "1"},
	{"cmp", "NaN", "2", "1"},
	{"cmp", "NaN", "-2", "1"},
	{"cmp", "NaN", "0.5", "1"},
	{"cmp", "NaN", "-0.5", "1"},
	{"cmp", "NaN", "2.5", "1"},
	{"cmp", "NaN", "-3", "1"},
	{"cmp", "NaN", "3", "1"},
	{"cmp", "NaN", "100000000000000000000", "1"},
	{"cmp", "NaN", "-12345678901234567890.5", "1"},
	{"cmp", "Infinity", "NaN", "-1"},
	{"cmp", "Infinity", "Infinity", "0"},
	{"cmp", "Infinity", "-Infinity", "1"},
	{"cmp", "Infinity", "0", "1"},
	{"cmp", "Infinity", "0.00", "1"},
	{"cmp", "Infinity", "1", "1"},
	{"cmp", "Infinity", "1.0", "1"},
	{"cmp", "Infinity", "-1", "1"},
	{"cmp", "Infinity", "2", "1"},
	{"cmp", "Infinity", "-2", "1"},
	{"cmp", "Infinity", "0.5", "1"},
	{"cmp", "Infinity", "-0.5", "1"},
	{"cmp", "Infinity", "2.5", "1"},
	{"cmp", "Infinity", "-3", "1"},
	{"cmp", "Infinity", "3", "1"},
	{"cmp", "Infinity", "100000000000000000000", "1"},
	{"cmp", "Infinity", "-12345678901234567890.5", "1"},
	{"cmp", "-Infinity", "NaN", "-1"},
	{"cmp", "-Infinity", "Infinity", "-1"},
	{"cmp", "-Infinity", "-Infinity", "0"},
	{"cmp", "-Infinity", "0", "-1"},
	{"cmp", "-Infinity", "0.00", "-1"},
	{"cmp", "-Infinity", "1", "-1"},
	{"cmp", "-Infinity", "1.0", "-1"},
	{"cmp", "-Infinity", "-1", "-1"},
	{"cmp", "-Infinity", "2", "-1"},
	{"cmp", "-Infinity", "-2", "-1"},
	{"cmp", "-Infinity", "0.5", "-1"},
	{"cmp", "-Infinity", "-0.5", "-1"},
	{"cmp", "-Infinity", "2.5", "-1"},
	{"cmp", "-Infinity", "-3", "-1"},
	{"cmp", "-Infinity", "3", "-1"},
	{"cmp", "-Infinity", "100000000000000000000", "-1"},
	{"cmp", "-Infinity", "-12345678901234567890.5", "-1"},
	{"cmp", "0", "NaN", "-1"},
	{"cmp", "0", "Infinity", "-1"},
	{"cmp", "0", "-Infinity", "1"},
	{"cmp", "0.00", "NaN", "-1"},
	{"cmp", "0.00", "Infinity", "-1"},
	{"cmp", "0.00", "-Infinity", "1"},
	{"cmp", "1", "NaN", "-1"},
	{"cmp", "1", "Infinity", "-1"},
	{"cmp", "1", "-Infinity", "1"},
	{"cmp", "1.0", "NaN", "-1"},
	{"cmp", "1.0", "Infinity", "-1"},
	{"cmp", "1.0", "-Infinity", "1"},
	{"cmp", "-1", "NaN", "-1"},
	{"cmp", "-1", "Infinity", "-1"},
	{"cmp", "-1", "-Infinity", "1"},
	{"cmp", "2", "NaN", "-1"},
	{"cmp", "2", "Infinity", "-1"},
	{"cmp", "2", "-Infinity", "1"},
	{"cmp", "-2", "NaN", "-1"},
	{"cmp", "-2", "Infinity", "-1"},
	{"cmp", "-2", "-Infinity", "1"},
	{"cmp", "0.5", "NaN", "-1"},
	{"cmp", "0.5", "Infinity", "-1"},
	{"cmp", "0.5", "-Infinity", "1"},
	{"cmp", "-0.5", "NaN", "-1"},
	{"cmp", "-0.5", "Infinity", "-1"},
	{"cmp", "-0.5", "-Infinity", "1"},
	{"cmp", "2.5", "NaN", "-1"},
	{"cmp", "2.5", "Infinity", "-1"},
	{"cmp", "2.5", "-Infinity", "1"},
	{"cmp", "-3", "NaN", "-1"},
	{"cmp", "-3", "Infinity", "-1"},
	{"cmp", "-3", "-Infinity", "1"},
	{"cmp", "3", "NaN", "-1"},
	{"cmp", "3", "Infinity", "-1"},
	{"cmp", "3", "-Infinity", "1"},
	{"cmp", "100000000000000000000", "NaN", "-1"},
	{"cmp", "100000000000000000000", "Infinity", "-1"},
	{"cmp", "100000000000000000000", "-Infinity", "1"},
	{"cmp", "-12345678901234567890.5", "NaN", "-1"},
	{"cmp", "-12345678901234567890.5", "Infinity", "-1"},
	{"cmp", "-12345678901234567890.5", "-Infinity", "1"},
}

// pgSpecialUnary: {op, a, Postgres 16's result}.
var pgSpecialUnary = [][3]string{
	{"neg", "NaN", "NaN"},
	{"neg", "Infinity", "-Infinity"},
	{"neg", "-Infinity", "Infinity"},
	{"neg", "0", "0"},
	{"neg", "0.00", "0.00"},
	{"neg", "1", "-1"},
	{"neg", "-1", "1"},
	{"neg", "2.5", "-2.5"},
	{"neg", "-2.5", "2.5"},
	{"neg", "0.5", "-0.5"},
	{"neg", "100000000000000000000", "-100000000000000000000"},
	{"abs", "NaN", "NaN"},
	{"abs", "Infinity", "Infinity"},
	{"abs", "-Infinity", "Infinity"},
	{"abs", "0", "0"},
	{"abs", "0.00", "0.00"},
	{"abs", "1", "1"},
	{"abs", "-1", "1"},
	{"abs", "2.5", "2.5"},
	{"abs", "-2.5", "2.5"},
	{"abs", "0.5", "0.5"},
	{"abs", "100000000000000000000", "100000000000000000000"},
	{"sign", "NaN", "NaN"},
	{"sign", "Infinity", "1"},
	{"sign", "-Infinity", "-1"},
	{"sign", "0", "0"},
	{"sign", "0.00", "0"},
	{"sign", "1", "1"},
	{"sign", "-1", "-1"},
	{"sign", "2.5", "1"},
	{"sign", "-2.5", "-1"},
	{"sign", "0.5", "1"},
	{"sign", "100000000000000000000", "1"},
	{"round", "NaN", "NaN"},
	{"round", "Infinity", "Infinity"},
	{"round", "-Infinity", "-Infinity"},
	{"round", "0", "0"},
	{"round", "0.00", "0"},
	{"round", "1", "1"},
	{"round", "-1", "-1"},
	{"round", "2.5", "3"},
	{"round", "-2.5", "-3"},
	{"round", "0.5", "1"},
	{"round", "100000000000000000000", "100000000000000000000"},
	{"round1", "NaN", "NaN"},
	{"round1", "Infinity", "Infinity"},
	{"round1", "-Infinity", "-Infinity"},
	{"round1", "0", "0.0"},
	{"round1", "0.00", "0.0"},
	{"round1", "1", "1.0"},
	{"round1", "-1", "-1.0"},
	{"round1", "2.5", "2.5"},
	{"round1", "-2.5", "-2.5"},
	{"round1", "0.5", "0.5"},
	{"round1", "100000000000000000000", "100000000000000000000.0"},
	{"trunc", "NaN", "NaN"},
	{"trunc", "Infinity", "Infinity"},
	{"trunc", "-Infinity", "-Infinity"},
	{"trunc", "0", "0"},
	{"trunc", "0.00", "0"},
	{"trunc", "1", "1"},
	{"trunc", "-1", "-1"},
	{"trunc", "2.5", "2"},
	{"trunc", "-2.5", "-2"},
	{"trunc", "0.5", "0"},
	{"trunc", "100000000000000000000", "100000000000000000000"},
	{"ceil", "NaN", "NaN"},
	{"ceil", "Infinity", "Infinity"},
	{"ceil", "-Infinity", "-Infinity"},
	{"ceil", "0", "0"},
	{"ceil", "0.00", "0"},
	{"ceil", "1", "1"},
	{"ceil", "-1", "-1"},
	{"ceil", "2.5", "3"},
	{"ceil", "-2.5", "-2"},
	{"ceil", "0.5", "1"},
	{"ceil", "100000000000000000000", "100000000000000000000"},
	{"floor", "NaN", "NaN"},
	{"floor", "Infinity", "Infinity"},
	{"floor", "-Infinity", "-Infinity"},
	{"floor", "0", "0"},
	{"floor", "0.00", "0"},
	{"floor", "1", "1"},
	{"floor", "-1", "-1"},
	{"floor", "2.5", "2"},
	{"floor", "-2.5", "-3"},
	{"floor", "0.5", "0"},
	{"floor", "100000000000000000000", "100000000000000000000"},
	{"sqrt", "NaN", "NaN"},
	{"sqrt", "Infinity", "Infinity"},
	{"sqrt", "-Infinity", "ERROR: cannot take square root of a negative number"},
	{"sqrt", "0", "0.000000000000000"},
	{"sqrt", "0.00", "0.000000000000000"},
	{"sqrt", "1", "1.000000000000000"},
	{"sqrt", "-1", "ERROR: cannot take square root of a negative number"},
	{"sqrt", "2.5", "1.581138830084190"},
	{"sqrt", "-2.5", "ERROR: cannot take square root of a negative number"},
	{"sqrt", "0.5", "0.70710678118654752"},
	{"sqrt", "100000000000000000000", "10000000000.00000"},
	{"exp", "NaN", "NaN"},
	{"exp", "Infinity", "Infinity"},
	{"exp", "-Infinity", "0"},
	{"exp", "0", "1.0000000000000000"},
	{"exp", "0.00", "1.0000000000000000"},
	{"exp", "1", "2.7182818284590452"},
	{"exp", "-1", "0.3678794411714423"},
	{"exp", "2.5", "12.182493960703473"},
	{"exp", "-2.5", "0.08208499862389880"},
	{"exp", "0.5", "1.6487212707001281"},
	{"exp", "100000000000000000000", "ERROR: value overflows numeric format"},
	{"ln", "NaN", "NaN"},
	{"ln", "Infinity", "Infinity"},
	{"ln", "-Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"ln", "0", "ERROR: cannot take logarithm of zero"},
	{"ln", "0.00", "ERROR: cannot take logarithm of zero"},
	{"ln", "1", "0.0000000000000000"},
	{"ln", "-1", "ERROR: cannot take logarithm of a negative number"},
	{"ln", "2.5", "0.9162907318741551"},
	{"ln", "-2.5", "ERROR: cannot take logarithm of a negative number"},
	{"ln", "0.5", "-0.6931471805599453"},
	{"ln", "100000000000000000000", "46.051701859880914"},
	{"log", "NaN", "NaN"},
	{"log", "Infinity", "Infinity"},
	{"log", "-Infinity", "ERROR: cannot take logarithm of a negative number"},
	{"log", "0", "ERROR: cannot take logarithm of zero"},
	{"log", "0.00", "ERROR: cannot take logarithm of zero"},
	{"log", "1", "0.0000000000000000"},
	{"log", "-1", "ERROR: cannot take logarithm of a negative number"},
	{"log", "2.5", "0.3979400086720376"},
	{"log", "-2.5", "ERROR: cannot take logarithm of a negative number"},
	{"log", "0.5", "-0.3010299956639812"},
	{"log", "100000000000000000000", "20.000000000000000"},
	{"trim_scale", "NaN", "NaN"},
	{"trim_scale", "Infinity", "Infinity"},
	{"trim_scale", "-Infinity", "-Infinity"},
	{"trim_scale", "0", "0"},
	{"trim_scale", "0.00", "0"},
	{"trim_scale", "1", "1"},
	{"trim_scale", "-1", "-1"},
	{"trim_scale", "2.5", "2.5"},
	{"trim_scale", "-2.5", "-2.5"},
	{"trim_scale", "0.5", "0.5"},
	{"trim_scale", "100000000000000000000", "100000000000000000000"},
}
