package sql

// numeric.go: the SQL layer's numeric type (N-048) — arithmetic,
// conversions and the errors Postgres raises for them.
//
// Promotion follows Postgres's implicit casts:
//
//	integer ─► numeric ─► double precision
//
// so integer ∘ numeric is numeric (exact) and anything ∘ float8 is
// float8. The typing pass (typecheck.go) decides the static type; at
// runtime arith dispatches on the operands' Go kinds the same way, so a
// value whose static type the pass could not see still computes by the
// same rule.

import (
	"errors"
	"strconv"
	"strings"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/decimal"
	"github.com/rohanthewiz/serr"
)

// numRank orders the numeric types for promotion: the wider of two
// operands' ranks is the result's type.
func numRank(t bytdb.ColType) int {
	switch t {
	case bytdb.TInt:
		return 1
	case bytdb.TNumeric:
		return 2
	case bytdb.TFloat:
		return 3
	}
	return 0
}

// widerNumeric is the promoted type of two numeric types.
func widerNumeric(a, b bytdb.ColType) bytdb.ColType {
	if numRank(b) > numRank(a) {
		return b
	}
	return a
}

// asDecimal reads an exact numeric operand: a Decimal, or an integer
// (promoted, as Postgres casts int to numeric implicitly).
func asDecimal(v any) (bytdb.Decimal, bool) {
	switch x := v.(type) {
	case bytdb.Decimal:
		return x, true
	case int64:
		return decimal.FromInt64(x), true
	}
	return bytdb.Decimal{}, false
}

// decimalErr words a decimal package error as Postgres does.
func decimalErr(err error) error {
	switch {
	case errors.Is(err, decimal.ErrDivByZero):
		return serr.New("division by zero")
	case errors.Is(err, decimal.ErrOverflow):
		return serr.New("value overflows numeric format")
	}
	return err
}

// decimalArith is + - * / % over two exact operands (at least one a
// Decimal; an int64 is promoted). Scales follow Postgres (see package
// decimal): / picks enough digits for 16 significant ones.
func decimalArith(op string, l, r bytdb.Decimal) (any, error) {
	var (
		d   bytdb.Decimal
		err error
	)
	switch op {
	case "+":
		d, err = l.Add(r)
	case "-":
		d, err = l.Sub(r)
	case "*":
		d, err = l.Mul(r)
	case "/":
		d, err = l.Div(r)
	case "%":
		d, err = l.Mod(r)
	default:
		return nil, serr.New("unhandled arithmetic operator", "op", op)
	}
	if err != nil {
		return nil, decimalErr(err)
	}
	return d, nil
}

// castToNumeric is E::numeric and E::numeric(p,s) (typ holds the
// modifier, as the parser's castType writes it). Postgres's casts:
// integer exactly, float8 through its 15-significant-digit text, text
// as numeric input; then the typmod rounds and range-checks.
func castToNumeric(v any, typ string) (any, error) {
	var (
		d   bytdb.Decimal
		err error
	)
	switch x := v.(type) {
	case nil:
		return nil, nil
	case bytdb.Decimal:
		d = x
	case int64:
		d = decimal.FromInt64(x)
	case float64:
		if d, err = floatToNumeric(x); err != nil {
			return nil, err
		}
	case string:
		if d, err = bytdb.ParseDecimal(x); err != nil {
			return nil, err
		}
	case bool:
		return nil, serr.New("cannot cast type boolean to numeric")
	default:
		return nil, serr.New("unsupported cast", "type", typ)
	}
	prec, scale, ok := numericCastMods(typ)
	if !ok {
		return d, nil
	}
	c := bytdb.Column{Type: bytdb.TNumeric, Precision: prec, Scale: scale}
	r, err := d.FitTypmod(c.Precision, c.Scale)
	if err != nil {
		var te *decimal.TypmodError
		if errors.As(err, &te) {
			return nil, serr.New("numeric field overflow", "detail", te.Detail())
		}
		return nil, err
	}
	return r, nil
}

// floatToNumeric is Postgres's float8 → numeric: the float printed with
// 15 significant digits first, so 0.1::float8::numeric is 0.1.
func floatToNumeric(f float64) (bytdb.Decimal, error) {
	d, err := decimal.FromFloat64(f, 15)
	if err != nil {
		return bytdb.Decimal{}, serr.New("cannot convert " + bytdb.FormatFloat(f) + " to numeric")
	}
	return d, nil
}

// numericCastMods reads the modifier out of a cast type string
// "numeric(p,s)"; ok is false for plain "numeric".
func numericCastMods(typ string) (prec, scale int32, ok bool) {
	rest, found := strings.CutPrefix(typ, "numeric(")
	if !found {
		return 0, 0, false
	}
	ps, _ := strings.CutSuffix(rest, ")")
	pStr, sStr, _ := strings.Cut(ps, ",")
	p, err1 := strconv.ParseInt(pStr, 10, 32)
	s, err2 := strconv.ParseInt(sStr, 10, 32)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return int32(p), int32(s), true
}

// isNumericCast reports whether a cast type string names numeric.
func isNumericCast(typ string) bool {
	return typ == "numeric" || typ == "decimal" || strings.HasPrefix(typ, "numeric(")
}
