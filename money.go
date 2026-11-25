package bytdb

// money.go: Postgres's money type (N-053), riding on int64 cents.
//
// A money value is a whole number of cents, exactly as Postgres stores
// it (an int64 "Cash"), so it orders, compares, hashes and keys as the
// int64 it is — no tuple encoding of its own. What differs from an
// integer is its text and its arithmetic, both defined here once for
// the SQL layer, the wire server, the driver and the shell:
//
//	text out   $1,234.56   -$1,234.56   $0.05   (cash_out, en_US)
//	text in    $1,234.56   1234.565 → $1,234.57   (1.50)   1.50-   -$ 1
//	           (cash_in: a sign, parentheses or a trailing sign, an
//	           optional $ before or after the sign, thousands commas,
//	           rounding on the third fraction digit)
//
// The format is fixed to Postgres's en_US/C conventions — "$", ",",
// "." and two fraction digits — which is what Postgres prints under the
// usual en_US.UTF-8 and C locales. lc_monetary is not modelled (plan
// decision Q6).
//
// The Go API's money value is the int64 count of cents: Insert takes an
// int64 (or any Go integer) as cents, a Decimal as currency units
// (rounded half away from zero to cents, as numeric::money does), and
// money text; Row values are cents. SQL follows Postgres instead: an
// integer literal or parameter assigned to a money column is whole
// units (5 is $5.00), because that is Postgres's int-to-money cast.

import (
	"math"
	"math/big"
	"strconv"

	"github.com/rohanthewiz/bytdb/decimal"
	"github.com/rohanthewiz/serr"
)

// TMoney is declared with the other column types in engine.go.

// errMoneyRange is "money out of range" (22003): money arithmetic
// whose result does not fit int64 cents. A fresh error each time, so a
// caller's serr.Wrap fields never land on a shared value.
func errMoneyRange() error { return serr.New("money out of range") }

// ParseMoney reads money text to cents, as Postgres's cash_in does:
//
//   - leading spaces, then an optional "$", then a sign ("-" or "+") or
//     an opening parenthesis (negative), then optionally "$" again;
//   - digits with any number of "," thousands separators and one ".";
//     two fraction digits are kept, and a third rounds the magnitude up
//     when it is 5 or more (any further digits are ignored);
//   - then only spaces, ")", a trailing "-" (negative) or "+", or "$".
//
// Empty text is $0.00, as in Postgres.
func ParseMoney(s string) (int64, error) {
	const csym, nsym, psym = "$", "-", "+"
	i := 0
	skipSpace := func() {
		for i < len(s) && isSpaceByte(s[i]) {
			i++
		}
	}
	skipSym := func(sym string) bool {
		if len(s)-i >= len(sym) && s[i:i+len(sym)] == sym {
			i += len(sym)
			return true
		}
		return false
	}
	skipSpace()
	skipSym(csym)
	skipSpace()
	neg := false
	switch {
	case skipSym(nsym):
		neg = true
	case i < len(s) && s[i] == '(':
		neg = true
		i++
	default:
		skipSym(psym)
	}
	skipSpace()
	skipSym(csym)
	skipSpace()

	// Accumulate the magnitude negatively, as cash_in does, so the most
	// negative value (-$92,233,720,368,547,758.08) fits.
	var value int64
	dec, seenDot := 0, false
	rangeErr := func() error {
		return serr.New(`value "` + s + `" is out of range for type money`)
	}
	for ; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9' && (!seenDot || dec < 2):
			if value < math.MinInt64/10 || value*10 < math.MinInt64+int64(c-'0') {
				return 0, rangeErr()
			}
			value = value*10 - int64(c-'0')
			if seenDot {
				dec++
			}
			continue
		case c == '.' && !seenDot:
			seenDot = true
			continue
		case c == ',':
			continue // thousands separator, anywhere
		}
		break
	}
	// Round on the next digit (only that one: 1.0049 is $1.00).
	if i < len(s) && s[i] >= '5' && s[i] <= '9' {
		if value == math.MinInt64 {
			return 0, rangeErr()
		}
		value--
	}
	for ; dec < 2; dec++ {
		if value < math.MinInt64/10 {
			return 0, rangeErr()
		}
		value *= 10
	}
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	for i < len(s) {
		switch {
		case isSpaceByte(s[i]) || s[i] == ')':
			i++
		case skipSym(nsym):
			neg = true
		case skipSym(psym), skipSym(csym):
		default:
			return 0, serr.New(`invalid input syntax for type money: "` + s + `"`)
		}
	}
	if !neg {
		if value == math.MinInt64 {
			return 0, rangeErr()
		}
		return -value, nil
	}
	return value, nil
}

func isSpaceByte(c byte) bool { return c == ' ' || (c >= '\t' && c <= '\r') }

// FormatMoney renders cents as Postgres's cash_out does under en_US:
// "$1,234.56", "-$1,234.56", "$0.05".
func FormatMoney(cents int64) string { return string(AppendMoney(nil, cents)) }

// AppendMoney appends FormatMoney's text to dst.
func AppendMoney(dst []byte, cents int64) []byte {
	u := uint64(cents)
	if cents < 0 {
		dst = append(dst, '-')
		u = -u // two's complement: MinInt64's magnitude as uint64
	}
	dst = append(dst, '$')
	whole, frac := u/100, u%100
	digits := strconv.FormatUint(whole, 10)
	// Thousands commas, counting from the right.
	for k, c := range []byte(digits) {
		if k > 0 && (len(digits)-k)%3 == 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, c)
	}
	return append(dst, '.', byte('0'+frac/10), byte('0'+frac%10))
}

// MoneyFromDecimal converts currency units to cents as numeric::money
// does: rounded half away from zero to whole cents; a result beyond
// int64 is "bigint out of range" (Postgres goes through numeric_int8).
func MoneyFromDecimal(d Decimal) (int64, error) {
	if !d.IsFinite() {
		return 0, specialToIntErr(d, "bigint")
	}
	c, err := d.Mul(decimal.FromInt64(100))
	if err != nil {
		return 0, err
	}
	n, ok := c.Round(0).Int64()
	if !ok {
		return 0, serr.New("bigint out of range")
	}
	return n, nil
}

// MoneyToDecimal is money::numeric: the cents with scale 2 ("1.50").
func MoneyToDecimal(cents int64) Decimal {
	return decimal.New(big.NewInt(cents), 2)
}

// MoneyFromInt is int::money: whole units, so 5 is $5.00 (500 cents),
// overflow "bigint out of range" as Postgres's int8_cash reports it.
func MoneyFromInt(units int64) (int64, error) {
	if units > math.MaxInt64/100 || units < math.MinInt64/100 {
		return 0, serr.New("bigint out of range")
	}
	return units * 100, nil
}

// MoneyAdd, MoneySub, MoneyMulInt and MoneyDivInt are cash_pl, cash_mi,
// cash_mul_int8 and cash_div_int8: exact integer arithmetic on cents,
// "money out of range" on overflow. Division truncates toward zero
// ($7.00 / 2 is $3.50; $0.07 / 2 is $0.03), and the most negative value
// divided by -1 stays itself, as Postgres's does on arm64 and Go's does
// everywhere.
func MoneyAdd(a, b int64) (int64, error) {
	s := a + b
	if (a >= 0) == (b >= 0) && (s >= 0) != (a >= 0) {
		return 0, errMoneyRange()
	}
	return s, nil
}

func MoneySub(a, b int64) (int64, error) {
	s := a - b
	if (a >= 0) != (b >= 0) && (s >= 0) != (a >= 0) {
		return 0, errMoneyRange()
	}
	return s, nil
}

func MoneyMulInt(c, n int64) (int64, error) {
	if c == 0 || n == 0 {
		return 0, nil
	}
	p := c * n
	if p/n != c || (c == -1 && n == math.MinInt64) || (n == -1 && c == math.MinInt64) {
		return 0, errMoneyRange()
	}
	return p, nil
}

func MoneyDivInt(c, n int64) (int64, error) {
	if n == 0 {
		return 0, serr.New("division by zero")
	}
	return c / n, nil
}

// MoneyMulFloat and MoneyDivFloat are cash_mul_flt8 and cash_div_flt8:
// the product or quotient in float8, rounded half to even to whole
// cents (so $0.05 * 0.5 is $0.02). An infinite float result from finite
// operands is float8's "value out of range: overflow"; NaN or a result
// past int64 is "money out of range".
func MoneyMulFloat(c int64, f float64) (int64, error) {
	r := float64(float64(c) * f)
	if math.IsInf(r, 0) && !math.IsInf(f, 0) {
		return 0, serr.New("value out of range: overflow")
	}
	return moneyFromFloat(math.RoundToEven(r))
}

func MoneyDivFloat(c int64, f float64) (int64, error) {
	if f == 0 {
		return 0, serr.New("division by zero")
	}
	r := float64(float64(c) / f)
	if math.IsInf(r, 0) && !math.IsInf(f, 0) {
		return 0, serr.New("value out of range: overflow")
	}
	return moneyFromFloat(math.RoundToEven(r))
}

func moneyFromFloat(r float64) (int64, error) {
	if math.IsNaN(r) || r < math.MinInt64 || r >= -float64(math.MinInt64) {
		return 0, errMoneyRange()
	}
	return int64(r), nil
}

// MoneyDivMoney is cash_div_cash: the ratio as a float8.
func MoneyDivMoney(a, b int64) (float64, error) {
	if b == 0 {
		return 0, serr.New("division by zero")
	}
	return float64(a) / float64(b), nil
}

// MoneyWords is cash_words: the amount spelled out in English, as
// Postgres spells it ("One hundred twenty three dollars and forty five
// cents"), quirks included — groups of zero leave their space behind
// ("One million  dollars"), and a hundreds group with tens below twenty
// takes "and" ("one hundred and five").
func MoneyWords(cents int64) string {
	var b []byte
	start := 0
	if cents < 0 {
		b = append(b, "minus "...)
		start = len(b)
	}
	val := uint64(cents)
	if cents < 0 {
		val = -val
	}
	groups := []struct {
		div  uint64
		name string
	}{
		{100000000000000000, " quadrillion "},
		{100000000000000, " trillion "},
		{100000000000, " billion "},
		{100000000, " million "},
		{100000, " thousand "},
	}
	for _, g := range groups {
		if m := (val / g.div) % 1000; m != 0 {
			b = append(b, numWord(m)...)
			b = append(b, g.name...)
		}
	}
	if m := (val / 100) % 1000; m != 0 {
		b = append(b, numWord(m)...)
	}
	if len(b) == start {
		b = append(b, "zero"...)
	}
	if val/100 == 1 {
		b = append(b, " dollar and "...)
	} else {
		b = append(b, " dollars and "...)
	}
	m0 := val % 100
	b = append(b, numWord(m0)...)
	if m0 == 1 {
		b = append(b, " cent"...)
	} else {
		b = append(b, " cents"...)
	}
	if b[0] >= 'a' && b[0] <= 'z' {
		b[0] -= 'a' - 'A'
	}
	return string(b)
}

var smallWords = [...]string{
	"zero", "one", "two", "three", "four", "five", "six", "seven",
	"eight", "nine", "ten", "eleven", "twelve", "thirteen", "fourteen",
	"fifteen", "sixteen", "seventeen", "eighteen", "nineteen", "twenty",
	"thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety",
}

// numWord is cash.c's num_word for 0..999.
func numWord(v uint64) string {
	big := smallWords[18:] // big[2] is "twenty", big[9] "ninety"
	tu := v % 100
	if v <= 20 {
		return smallWords[v]
	}
	if tu == 0 {
		return smallWords[v/100] + " hundred"
	}
	if v > 99 {
		switch {
		case v%10 == 0 && tu > 10:
			return smallWords[v/100] + " hundred " + big[tu/10]
		case tu < 20:
			return smallWords[v/100] + " hundred and " + smallWords[tu]
		}
		return smallWords[v/100] + " hundred " + big[tu/10] + " " + smallWords[tu%10]
	}
	switch {
	case v%10 == 0 && tu > 10:
		return big[tu/10]
	case tu < 20:
		return smallWords[tu]
	}
	return big[tu/10] + " " + smallWords[tu%10]
}
