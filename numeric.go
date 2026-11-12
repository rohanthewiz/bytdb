package bytdb

// numeric.go: the engine side of the numeric / decimal column type
// (N-048) — parsing, coercion into a numeric column, and the
// numeric(p, s) typmod.
//
// The arithmetic and the Postgres rules live in package decimal; the
// order-preserving encoding lives in package tuple. What is here is the
// glue every write path shares: coerceRow, the default backfill, ALTER
// COLUMN TYPE.

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"

	"github.com/rohanthewiz/bytdb/decimal"
	"github.com/rohanthewiz/serr"
)

// parseDecimalErr parses numeric text with Postgres's error wording:
// `invalid input syntax for type numeric: "abc"` (22P02), and
// "value overflows numeric format" (22003) for out-of-range input.
func parseDecimalErr(s string) (Decimal, error) {
	d, err := decimal.Parse(s)
	if err == nil {
		return d, nil
	}
	switch {
	case errors.Is(err, decimal.ErrSyntax):
		return Decimal{}, serr.New(`invalid input syntax for type numeric: "` + s + `"`)
	case errors.Is(err, decimal.ErrSpecial):
		return Decimal{}, serr.New("numeric NaN and Infinity are not supported",
			"value", s, "hint", "store them in a double precision column, or use NULL")
	}
	return Decimal{}, serr.New(err.Error(), "value", s)
}

// DecimalFromFloat converts a float64 as a Go caller handing one to a
// numeric column means it: the shortest decimal text that round-trips
// to the same float (0.1, not 0.1000000000000000055...). NaN and ±Inf
// are refused.
func DecimalFromFloat(f float64) (Decimal, error) {
	d, err := decimal.FromFloat64(f, -1)
	if err != nil {
		return Decimal{}, serr.New("numeric NaN and Infinity are not supported",
			"value", strconv.FormatFloat(f, 'g', -1, 64))
	}
	return d, nil
}

// coerceNumeric is coerce's TNumeric arm: every shape a numeric column
// accepts. ok is false for a shape it does not know (coerce then tries
// NormalizeValue and reports the mismatch).
func coerceNumeric(v any) (d Decimal, ok bool, err error) {
	switch x := v.(type) {
	case Decimal:
		return x, true, nil
	case *Decimal:
		if x == nil {
			return Decimal{}, false, nil
		}
		return *x, true, nil
	case int64:
		return decimal.FromInt64(x), true, nil
	case int:
		return decimal.FromInt64(int64(x)), true, nil
	case int32:
		return decimal.FromInt64(int64(x)), true, nil
	case int16:
		return decimal.FromInt64(int64(x)), true, nil
	case int8:
		return decimal.FromInt64(int64(x)), true, nil
	case uint8:
		return decimal.FromInt64(int64(x)), true, nil
	case uint16:
		return decimal.FromInt64(int64(x)), true, nil
	case uint32:
		return decimal.FromInt64(int64(x)), true, nil
	case uint64:
		return decimal.New(new(big.Int).SetUint64(x), 0), true, nil
	case uint:
		return decimal.New(new(big.Int).SetUint64(uint64(x)), 0), true, nil
	case *big.Int:
		if x == nil {
			return Decimal{}, false, nil
		}
		return decimal.New(x, 0), true, nil
	case float64:
		d, err := DecimalFromFloat(x)
		return d, true, err
	case float32:
		// Through float32's own shortest text: float32(0.1) is 0.1, not
		// the float64 widening's 0.10000000149011612.
		d, err := decimal.Parse(strconv.FormatFloat(float64(x), 'g', -1, 32))
		if err != nil {
			return Decimal{}, true, serr.New("numeric NaN and Infinity are not supported")
		}
		return d, true, nil
	case string:
		d, err := parseDecimalErr(x)
		return d, true, err
	case []byte:
		// Text drivers hand numerics over as bytes ("19.99").
		d, err := parseDecimalErr(string(x))
		return d, true, err
	}
	return Decimal{}, false, nil
}

// enforceTypmod applies a column's declared limits to an already-coerced
// value: VARCHAR(n)'s length (enforceMaxLen) and numeric(p, s)'s
// rounding and range. Every write path calls this one function, so a
// new kind of typmod has one place to go.
func enforceTypmod(v any, c *Column) (any, error) {
	v, err := enforceMaxLen(v, c)
	if err != nil {
		return nil, err
	}
	return enforceNumericTypmod(v, c)
}

// enforceNumericTypmod rounds a numeric column's value to its declared
// scale and refuses one that overflows its precision, with Postgres's
// message and DETAIL. Unconstrained numeric (Precision 0) and non-numeric
// columns pass through.
func enforceNumericTypmod(v any, c *Column) (any, error) {
	if c.Type != TNumeric || c.Precision == 0 {
		return v, nil
	}
	d, ok := v.(Decimal)
	if !ok { // NULL, or a mismatch reported elsewhere
		return v, nil
	}
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

// validTypmod checks a column's numeric(p, s) declaration: Postgres
// allows precision 1..1000 and scale -1000..1000, and only numeric
// columns carry one.
func validTypmod(c Column) error {
	if c.Precision == 0 && c.Scale == 0 {
		return nil
	}
	if c.Type != TNumeric {
		return serr.New("precision and scale are only valid on numeric columns",
			"column", c.Name, "type", string(c.Type))
	}
	if c.Precision < 1 || c.Precision > decimal.MaxPrecision {
		return serr.New(fmt.Sprintf("NUMERIC precision %d must be between 1 and %d",
			c.Precision, decimal.MaxPrecision), "column", c.Name)
	}
	if c.Scale < -decimal.MaxPrecision || c.Scale > decimal.MaxPrecision {
		return serr.New(fmt.Sprintf("NUMERIC scale %d must be between %d and %d",
			c.Scale, -decimal.MaxPrecision, decimal.MaxPrecision), "column", c.Name)
	}
	return nil
}

// NumericTypeName spells a numeric column's type as Postgres's
// format_type does: "numeric", "numeric(10,2)".
func NumericTypeName(c Column) string {
	if c.Precision == 0 {
		return "numeric"
	}
	return fmt.Sprintf("numeric(%d,%d)", c.Precision, c.Scale)
}

// NumericTypmod is the atttypmod Postgres reports for numeric(p, s):
// ((p << 16) | (s & 0x7ff)) + 4, or -1 when unconstrained. The scale is
// stored as an 11-bit two's-complement field, which is how Postgres 15+
// fits a negative scale.
func NumericTypmod(c Column) int64 {
	if c.Precision == 0 {
		return -1
	}
	return int64(c.Precision)<<16 | int64(c.Scale)&0x7ff + 4
}

// numericToInt is the numeric → integer conversion Postgres's casts use:
// round half away from zero, range-checked.
func numericToInt(d Decimal) (int64, error) {
	n, ok := d.Int64()
	if !ok {
		return 0, serr.New("bigint out of range")
	}
	return n, nil
}

// numericToFloat is the numeric → float8 conversion: the nearest float.
func numericToFloat(d Decimal) float64 { return d.Float64() }

// floatToNumeric is Postgres's float8 → numeric cast: the float printed
// with 15 significant digits first (DBL_DIG), so 0.1::float8::numeric
// is 0.1 and 1/3.0::float8::numeric is 0.333333333333333. NaN and
// ±Infinity are refused (bytdb's numeric has neither).
func floatToNumeric(f float64) (Decimal, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return Decimal{}, serr.New("numeric NaN and Infinity are not supported",
			"value", FormatFloat(f))
	}
	return decimal.FromFloat64(f, 15)
}
