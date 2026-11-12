package decimal

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// The expected strings below are Postgres 16.15's output for the same
// expression, so these tests pin parity, not just self-consistency.

func TestParseAndString(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1.50e1", "15.0"},
		{"1e3", "1000"},
		{".5", "0.5"},
		{"5.", "5"},
		{"1_000.5", "1000.5"},
		{"-0.00", "0.00"},
		{"  +12.340  ", "12.340"},
		{"-0.001", "-0.001"},
		{"1.5E-3", "0.0015"},
		{"0", "0"},
		{"000123.4500", "123.4500"},
		{"12345678901234567890.123456789012345678901", "12345678901234567890.123456789012345678901"},
	}
	for _, c := range cases {
		d, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if got := d.String(); got != c.want {
			t.Errorf("Parse(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestParseRefusals(t *testing.T) {
	for in, want := range map[string]error{
		"":              ErrSyntax,
		"abc":           ErrSyntax,
		".":             ErrSyntax,
		"1.2.3":         ErrSyntax,
		"e5":            ErrSyntax,
		"1e":            ErrSyntax,
		"1_":            ErrSyntax,
		"_1":            ErrSyntax,
		"1__0":          ErrSyntax,
		"1 2":           ErrSyntax,
		"NaN":           ErrSpecial,
		"-Infinity":     ErrSpecial,
		"1e-20000":      ErrOverflow,
		"1e200000":      ErrOverflow,
		"1e99999999999": ErrOverflow,
	} {
		if _, err := Parse(in); !errors.Is(err, want) {
			t.Errorf("Parse(%q) error = %v, want %v", in, err, want)
		}
	}
}

func mustOp(t *testing.T, d Decimal, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return d.String()
}

func TestArithmeticMatchesPostgres(t *testing.T) {
	m := MustParse
	cases := []struct {
		name string
		got  func() (Decimal, error)
		want string
	}{
		{"2/3", func() (Decimal, error) { return m("2").Div(m("3")) }, "0.66666666666666666667"},
		{"1/7", func() (Decimal, error) { return m("1").Div(m("7")) }, "0.14285714285714285714"},
		{"100/3", func() (Decimal, error) { return m("100").Div(m("3")) }, "33.3333333333333333"},
		{"0.001/3", func() (Decimal, error) { return m("0.001").Div(m("3")) }, "0.00033333333333333333"},
		{"123456789/0.0001", func() (Decimal, error) { return m("123456789").Div(m("0.0001")) }, "1234567890000.00000000"},
		{"1/3.0", func() (Decimal, error) { return m("1").Div(m("3.0")) }, "0.33333333333333333333"},
		{"10.0/4", func() (Decimal, error) { return m("10.0").Div(m("4")) }, "2.5000000000000000"},
		{"1.0/7e10", func() (Decimal, error) { return m("1.0").Div(m("7e10")) }, "0.0000000000142857142857142857"},
		{"0/5", func() (Decimal, error) { return m("0").Div(m("5")) }, "0.00000000000000000000"},
		{"99999/7", func() (Decimal, error) { return m("99999").Div(m("7")) }, "14285.571428571429"},
		{"0.05/0.0003", func() (Decimal, error) { return m("0.05").Div(m("0.0003")) }, "166.6666666666666667"},
		{"7 % -3", func() (Decimal, error) { return m("7").Mod(m("-3")) }, "1"},
		{"-7.5 % 2", func() (Decimal, error) { return m("-7.5").Mod(m("2")) }, "-1.5"},
		{"7.5 % 2", func() (Decimal, error) { return m("7.5").Mod(m("2")) }, "1.5"},
		{"0.1 + 0.2", func() (Decimal, error) { return m("0.1").Add(m("0.2")) }, "0.3"},
		{"1.10 * 2.00", func() (Decimal, error) { return m("1.10").Mul(m("2.00")) }, "2.2000"},
		{"1.000 * 0.10", func() (Decimal, error) { return m("1.000").Mul(m("0.10")) }, "0.10000"},
		{"1.5 - 2.25", func() (Decimal, error) { return m("1.5").Sub(m("2.25")) }, "-0.75"},
	}
	for _, c := range cases {
		d, err := c.got()
		if got := mustOp(t, d, err); got != c.want {
			t.Errorf("%s = %s, want %s", c.name, got, c.want)
		}
	}
	if _, err := m("1").Div(m("0.00")); !errors.Is(err, ErrDivByZero) {
		t.Errorf("1/0.00 error = %v", err)
	}
	if _, err := m("1").Mod(m("0")); !errors.Is(err, ErrDivByZero) {
		t.Errorf("1%%0 error = %v", err)
	}
}

func TestRoundingMatchesPostgres(t *testing.T) {
	m := MustParse
	cases := []struct{ got, want string }{
		{m("1250").Round(-2).String(), "1300"},
		{m("-1.005").Round(2).String(), "-1.01"},
		{m("2.5").Round(0).String(), "3"},
		{m("-2.5").Round(0).String(), "-3"},
		{m("-1.239").Trunc(2).String(), "-1.23"},
		{m("1.2").Round(3).String(), "1.200"},
		{m("-1.5").Ceil().String(), "-1"},
		{m("-1.5").Floor().String(), "-2"},
		{m("1.5").Ceil().String(), "2"},
		{m("1.5").Floor().String(), "1"},
		{m("-0.4").Ceil().String(), "0"},
		{m("12345").Round(-3).String(), "12000"},
	}
	for i, c := range cases {
		if c.got != c.want {
			t.Errorf("case %d: got %s, want %s", i, c.got, c.want)
		}
	}
}

func TestFitTypmod(t *testing.T) {
	m := MustParse
	ok := []struct {
		in   string
		p, s int32
		want string
	}{
		{"123.456", 5, 2, "123.46"},
		{"0.00123", 3, 5, "0.00123"},
		{"12345", 2, -3, "12000"},
		{"0.5", 1, 1, "0.5"},
		{"-999.994", 5, 2, "-999.99"},
		{"0", 2, 2, "0.00"},
	}
	for _, c := range ok {
		d, err := m(c.in).FitTypmod(c.p, c.s)
		if err != nil || d.String() != c.want {
			t.Errorf("%s::numeric(%d,%d) = %v, %v; want %s", c.in, c.p, c.s, d, err, c.want)
		}
	}
	bad := []struct {
		in     string
		p, s   int32
		detail string
	}{
		{"0.0123", 3, 5, "A field with precision 3, scale 5 must round to an absolute value less than 10^-2."},
		{"1.5", 2, 2, "A field with precision 2, scale 2 must round to an absolute value less than 1."},
		{"999.995", 5, 2, "A field with precision 5, scale 2 must round to an absolute value less than 10^3."},
	}
	for _, c := range bad {
		_, err := m(c.in).FitTypmod(c.p, c.s)
		var te *TypmodError
		if !errors.As(err, &te) || te.Error() != "numeric field overflow" || te.Detail() != c.detail {
			t.Errorf("%s::numeric(%d,%d) error = %v (detail %q)", c.in, c.p, c.s, err, func() string {
				if te != nil {
					return te.Detail()
				}
				return ""
			}())
		}
	}
}

func TestCmpNormalizeAndConversions(t *testing.T) {
	m := MustParse
	if m("1.0").Cmp(m("1.00")) != 0 || !m("1.0").Equal(m("1")) {
		t.Error("scale must not affect equality")
	}
	if m("-1").Cmp(m("0.5")) >= 0 || m("10").Cmp(m("9.99")) <= 0 || m("-2").Cmp(m("-1.5")) >= 0 {
		t.Error("ordering")
	}
	for in, want := range map[string]string{"1.500": "1.5", "100": "100", "0.000": "0", "-2.50": "-2.5", "3": "3"} {
		if got := m(in).Normalize(); got.String() != want {
			t.Errorf("Normalize(%s) = %s, want %s", in, got, want)
		}
	}
	if n, ok := m("2.5").Int64(); !ok || n != 3 {
		t.Errorf("2.5 → int = %d, %v", n, ok)
	}
	if n, ok := m("-2.5").Int64(); !ok || n != -3 {
		t.Errorf("-2.5 → int = %d, %v", n, ok)
	}
	if _, ok := m("9223372036854775807.5").Int64(); ok {
		t.Error("out-of-range int conversion must fail")
	}
	if f := m("0.1").Float64(); f != 0.1 {
		t.Errorf("Float64(0.1) = %v", f)
	}
	// 15 digits is Postgres's float8 → numeric; -1 is the shortest
	// round-trip text a Go float64 means.
	if d, _ := FromFloat64(1.0/3, 15); d.String() != "0.333333333333333" {
		t.Errorf("FromFloat64(1/3, 15) = %s", d)
	}
	if d, _ := FromFloat64(0.1, -1); d.String() != "0.1" {
		t.Errorf("FromFloat64(0.1, -1) = %s", d)
	}
	if d, _ := FromFloat64(0.1, 15); d.String() != "0.1" { // Postgres: 0.1::float8::numeric
		t.Errorf("FromFloat64(0.1, 15) = %s", d)
	}
	if d, _ := FromFloat64(1e20, 15); d.String() != "100000000000000000000" {
		t.Errorf("FromFloat64(1e20, 15) = %s", d)
	}
	if _, err := FromFloat64(math.NaN(), -1); !errors.Is(err, ErrSpecial) {
		t.Error("NaN must be refused")
	}
	if !m("2.00").IsInteger() || m("2.01").IsInteger() {
		t.Error("IsInteger")
	}
	var zero Decimal
	if zero.String() != "0" || zero.Sign() != 0 || zero.Cmp(m("0.00")) != 0 {
		t.Error("zero value")
	}
	if got := strings.Repeat("9", 3); m(got).IntDigits() != 3 || m("0.5").IntDigits() != 0 {
		t.Error("IntDigits")
	}
}

// FuzzParseString checks that any accepted input renders to text that
// parses back to an identical value (same digits, same scale).
func FuzzParseString(f *testing.F) {
	for _, s := range []string{"0", "-1.50", "1e3", ".5", "1_000.25", "123456789012345678901234567890.1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := Parse(s)
		if err != nil {
			return
		}
		back, err := Parse(d.String())
		if err != nil {
			t.Fatalf("re-parse of %q (from %q): %v", d.String(), s, err)
		}
		if back.String() != d.String() || back.Cmp(d) != 0 {
			t.Fatalf("%q → %s → %s", s, d, back)
		}
		// Arithmetic identities that must hold exactly.
		if sum, err := d.Add(d.Neg()); err == nil && !sum.IsZero() {
			t.Fatalf("%s + -%s = %s", d, d, sum)
		}
		if n := d.Normalize(); n.Cmp(d) != 0 {
			t.Fatalf("Normalize changed the value: %s → %s", d, n)
		}
	})
}
