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
	"math/big"
	"strconv"

	"github.com/rohanthewiz/bytdb/decimal"
	"github.com/rohanthewiz/bytdb/interval"
	"github.com/rohanthewiz/serr"
)

// parseDecimalErr parses numeric text with Postgres's error wording:
// `invalid input syntax for type numeric: "abc"` (22P02), and
// "value overflows numeric format" (22003) for out-of-range input.
// 'NaN', 'Infinity' and '-Infinity' are values since N-051 (decimal.Parse
// reads every spelling numeric_in does), so there is no refusal here.
func parseDecimalErr(s string) (Decimal, error) {
	d, err := decimal.Parse(s)
	if err == nil {
		return d, nil
	}
	if errors.Is(err, decimal.ErrSyntax) {
		return Decimal{}, serr.New(`invalid input syntax for type numeric: "` + s + `"`)
	}
	return Decimal{}, serr.New(err.Error(), "value", s)
}

// DecimalFromFloat converts a float64 as a Go caller handing one to a
// numeric column means it: the shortest decimal text that round-trips
// to the same float (0.1, not 0.1000000000000000055...). NaN and ±Inf
// become numeric NaN and ±Infinity (N-051).
func DecimalFromFloat(f float64) (Decimal, error) {
	d, err := decimal.FromFloat64(f, -1)
	if err != nil {
		return Decimal{}, serr.Wrap(err, "value", strconv.FormatFloat(f, 'g', -1, 64))
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
		// the float64 widening's 0.10000000149011612. NaN and ±Inf print
		// as "NaN" / "+Inf" / "-Inf", which Parse reads as numeric's
		// special values, so they convert as float8's do.
		d, err := parseDecimalErr(strconv.FormatFloat(float64(x), 'g', -1, 32))
		return d, true, err
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

// coerceColumn is coerce for a value bound for column c, then the
// column's declared limits (enforceTypmod). Every write path — insert,
// update, a default — goes through it. Interval text is the one input
// read differently per column: a bare number takes the modifier's last
// field as its unit ('5' is 5 minutes in an interval minute column), so
// it is parsed with the modifier rather than coerced and then adjusted.
func coerceColumn(v any, c *Column) (any, error) {
	if c.Type == TInterval && c.IntervalTypmod != 0 {
		switch x := v.(type) {
		case string:
			return interval.ParseTypmod(x, c.IntervalTypmod)
		case []byte:
			return interval.ParseTypmod(string(x), c.IntervalTypmod)
		}
	}
	cv, err := coerce(v, c.Type)
	if err != nil {
		return nil, err
	}
	return enforceTypmod(cv, c)
}

// enforceTypmod applies a column's declared limits to an already-coerced
// value: VARCHAR(n)'s length (enforceMaxLen), numeric(p, s)'s rounding
// and range, and an interval modifier's truncation and rounding. Every
// write path calls this one function, so a new kind of typmod has one
// place to go.
func enforceTypmod(v any, c *Column) (any, error) {
	v, err := enforceMaxLen(v, c)
	if err != nil {
		return nil, err
	}
	if c.Type == TInterval && c.IntervalTypmod != 0 {
		if iv, ok := v.(Interval); ok {
			return interval.Adjust(iv, c.IntervalTypmod)
		}
		return v, nil
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
	if c.IntervalTypmod != 0 {
		if c.Type != TInterval {
			return serr.New("an interval type modifier is only valid on interval columns",
				"column", c.Name, "type", string(c.Type))
		}
		// Adjust refuses a modifier it does not recognize.
		if _, err := interval.Adjust(Interval{}, c.IntervalTypmod); err != nil {
			return serr.Wrap(err, "column", c.Name)
		}
	}
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
// round half away from zero, range-checked. NaN and the infinities fail
// with their own messages (specialToIntErr).
func numericToInt(d Decimal) (int64, error) {
	if !d.IsFinite() {
		return 0, specialToIntErr(d, "bigint")
	}
	n, ok := d.Int64()
	if !ok {
		return 0, serr.New("bigint out of range")
	}
	return n, nil
}

// specialToIntErr is Postgres's error for casting numeric NaN or an
// infinity to an integer type: "cannot convert NaN to bigint", "cannot
// convert infinity to integer" (SQLSTATE 0A000, feature_not_supported,
// as numeric_int8 raises it). typeName is the target as Postgres names
// it: bigint, integer or smallint.
func specialToIntErr(d Decimal, typeName string) error {
	what := "infinity"
	if d.IsNaN() {
		what = "NaN"
	}
	return serr.New("cannot convert " + what + " to " + typeName)
}

// numericToFloat is the numeric → float8 conversion: the nearest float,
// and NaN / ±Infinity to the float ones.
func numericToFloat(d Decimal) float64 { return d.Float64() }

// floatToNumeric is Postgres's float8 → numeric cast: the float printed
// with 15 significant digits first (DBL_DIG), so 0.1::float8::numeric
// is 0.1 and 1/3.0::float8::numeric is 0.333333333333333. NaN and
// ±Infinity map to numeric's own (Postgres 14+; N-051).
func floatToNumeric(f float64) (Decimal, error) {
	return decimal.FromFloat64(f, 15)
}
