package sql

// funcs.go: the scalar function library (deep-look D-53).
//
// Each function is one registry entry carrying everything both halves
// of the engine need:
//
//	typing pass ──► scalarFunc.typ(argument types) ─► result type, or a
//	                42883 "function f(t1, t2) does not exist" at prepare
//	                time, plus any casts evaluation relies on (a date
//	                argument to extract() becomes a timestamp; a non-text
//	                argument to concat() becomes its text)
//	evaluator  ───► scalarFunc.eval(argument values)
//
// Arguments arrive at eval as runtime values (int64, float64, string,
// bool, []byte; a timestamp as its microsecond count). Unless a function
// says otherwise it is strict: a NULL argument makes the result NULL
// without calling eval, as Postgres's STRICT functions behave.
//
// Functions that existed before this file (now(), lower(), pg_*...) stay
// in evalFunc's switch; knownFuncs lists them so the typing pass can
// tell an unknown function from a known one (a test keeps the list in
// step with the switch).

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/decimal"
	"github.com/rohanthewiz/bytdb/interval"
	"github.com/rohanthewiz/serr"
)

// scalarFunc is one library function.
type scalarFunc struct {
	min, max int // argument count bounds; max < 0 is variadic
	// nullSafe functions see NULL arguments (concat skips them); others
	// are strict.
	nullSafe bool
	// typ checks the argument types (tyUnknown for an untyped literal)
	// and returns the result type. args lets it ask for casts: it may
	// replace an argument with a cast of it (see castArg). nil means
	// the fixed type ret, accepting any arguments.
	typ  func(f *funcCall) (bytdb.ColType, error)
	ret  bytdb.ColType
	eval func(args []any) (any, error)
	// envEval replaces eval for functions that read the statement's
	// environment — session state, the catalog, the statement's context
	// (sessinfo.go). Exactly one of eval and envEval is set.
	envEval func(env *exEnv, args []any) (any, error)
}

// funcCall is a call being typed: its name, argument expressions and
// their static types. castArg rewrites one argument to a cast, which is
// how typing hands evaluation values of a known form.
type funcCall struct {
	name    string
	args    []Expr
	types   []bytdb.ColType
	changed bool
}

func (c *funcCall) castArg(i int, to string) {
	c.args[i] = &ExCast{E: c.args[i], Type: to, From: c.types[i]}
	c.types[i] = castColType(to)
	c.changed = true
}

// noFunc is Postgres's refusal for a call no function matches (42883).
func noFunc(name string, types []bytdb.ColType) error {
	names := make([]string, len(types))
	for i, t := range types {
		names[i] = pgTypeName(t)
	}
	return serr.New("function "+name+"("+strings.Join(names, ", ")+") does not exist",
		"hint", "No function matches the given name and argument types. You might need to add explicit type casts.")
}

// Static type classes for argument checks.
func numericOrUnknown(t bytdb.ColType) bool {
	return numericType(t) || t == tyUnknown || t == tyNull
}
func textOrUnknown(t bytdb.ColType) bool {
	return t == bytdb.TString || t == tyUnknown || t == tyNull
}

// numericResult is the result of a numeric function that keeps its
// argument's type: integer stays integer, anything else is float.
func numericResult(f *funcCall) (bytdb.ColType, error) {
	for _, t := range f.types {
		if !numericOrUnknown(t) {
			return "", noFunc(f.name, f.types)
		}
	}
	// A numeric argument makes the result numeric unless a float one
	// makes it float (N-048: the math functions have numeric
	// implementations, which Postgres prefers for numeric input).
	var widest bytdb.ColType
	for _, t := range f.types {
		widest = widerNumeric(widest, t)
	}
	if widest == bytdb.TNumeric {
		return bytdb.TNumeric, nil
	}
	if len(f.types) > 0 && f.types[0] == bytdb.TInt {
		return bytdb.TInt, nil
	}
	return bytdb.TFloat, nil
}

// roundResult types round(x, n) and trunc(x, n): numeric when x is an
// integer or a numeric, as Postgres resolves the two-argument forms
// (only numeric has them); otherwise numericResult's rule.
func roundResult(f *funcCall) (bytdb.ColType, error) {
	t, err := numericResult(f)
	if err == nil && len(f.types) == 2 && (f.types[0] == bytdb.TInt || f.types[0] == bytdb.TNumeric) {
		return bytdb.TNumeric, nil
	}
	return t, err
}

// numericArg types the scale functions: one numeric (or integer)
// argument, result ret.
func numericArg(ret bytdb.ColType) func(f *funcCall) (bytdb.ColType, error) {
	return func(f *funcCall) (bytdb.ColType, error) {
		if len(f.types) != 1 || !numericOrUnknown(f.types[0]) || f.types[0] == bytdb.TFloat {
			return "", noFunc(f.name, f.types)
		}
		return ret, nil
	}
}

// mathResult types sqrt, exp, ln, power and one-argument log, which
// Postgres defines for both double precision and numeric. A numeric
// argument with no float one resolves to the numeric version, the only
// exact match. Anything else resolves to double precision, the numeric
// category's preferred type, so integer and untyped arguments land
// there: sqrt(2) is float8, sqrt(2.0) numeric, power(2, 3.0) numeric,
// power(2.0, 3::float8) float8. Checked against Postgres 16's pg_typeof.
func mathResult(f *funcCall) (bytdb.ColType, error) {
	return pickMathVersion(f, false)
}

// logResult types log. One argument follows mathResult. Two-argument
// log exists only as log(numeric, numeric), so integers and untyped
// arguments resolve to numeric too (log(2, 8) is 3.0000000000000000). A
// float argument, which Postgres refuses outright (no
// log(double precision, double precision)), keeps bytdb's float version,
// as before.
func logResult(f *funcCall) (bytdb.ColType, error) {
	return pickMathVersion(f, len(f.types) == 2)
}

// pickMathVersion is the shared rule: numeric when there is a numeric
// argument (or numericDefault) and no float one, else float. Untyped
// arguments are cast to the chosen type, and for the numeric version
// integer ones too. Evaluation then picks the version from the values
// alone (log(2, 8) arrives as two Decimals), and a placeholder describes
// as the chosen type.
func pickMathVersion(f *funcCall, numericDefault bool) (bytdb.ColType, error) {
	hasNum, hasFloat := false, false
	for _, t := range f.types {
		if !numericOrUnknown(t) {
			return "", noFunc(f.name, f.types)
		}
		hasNum = hasNum || t == bytdb.TNumeric
		hasFloat = hasFloat || t == bytdb.TFloat
	}
	res, cast := bytdb.ColType(bytdb.TFloat), "float8"
	if !hasFloat && (hasNum || numericDefault) {
		res, cast = bytdb.TNumeric, "numeric"
	}
	for i, t := range f.types {
		if t == tyUnknown || (t == bytdb.TInt && res == bytdb.TNumeric) {
			f.castArg(i, cast)
		}
	}
	return res, nil
}

// mathEval evaluates a function typed by mathResult or logResult. The
// version follows the values, which the typing made consistent: any
// float argument means the float version, and otherwise integers and
// Decimals go to the numeric one. A numeric-typed call never sees a
// float, and a float-typed call with integer-only arguments promotes
// them to float. Untyped arguments arrive already cast.
func mathEval(fl func([]float64) (float64, error), dec func([]bytdb.Decimal) (bytdb.Decimal, error)) func([]any) (any, error) {
	return func(a []any) (any, error) {
		numeric, float := false, false
		for _, v := range a {
			switch v.(type) {
			case bytdb.Decimal:
				numeric = true
			case float64:
				float = true
			}
		}
		if numeric && !float {
			ds := make([]bytdb.Decimal, len(a))
			for i, v := range a {
				d, ok := asDecimal(v)
				if !ok {
					return nil, serr.New("numeric argument expected")
				}
				ds[i] = d
			}
			r, err := dec(ds)
			if err != nil {
				return nil, decimalErr(err)
			}
			return r, nil
		}
		xs := make([]float64, len(a))
		for i, v := range a {
			switch n := v.(type) {
			case int64:
				xs[i] = float64(n)
			case float64:
				xs[i] = n
			case bytdb.Decimal:
				xs[i] = n.Float64()
			default:
				return nil, serr.New("numeric argument expected")
			}
		}
		r, err := fl(xs)
		if err != nil {
			return nil, decimalErr(err)
		}
		return r, nil
	}
}

// floatLogArg is the float logarithms' domain check, worded as
// Postgres's dlog1/dlog10 word it.
func floatLogArg(x float64) error {
	switch {
	case x == 0:
		return decimal.ErrLogZero
	case x < 0:
		return decimal.ErrLogNegative
	}
	return nil
}

// floatResult accepts numeric arguments and returns float.
func floatResult(f *funcCall) (bytdb.ColType, error) {
	for _, t := range f.types {
		if !numericOrUnknown(t) {
			return "", noFunc(f.name, f.types)
		}
	}
	return bytdb.TFloat, nil
}

// textArgs checks every argument is text (or untyped) and returns ret.
func textArgs(ret bytdb.ColType, intArgs ...int) func(f *funcCall) (bytdb.ColType, error) {
	return func(f *funcCall) (bytdb.ColType, error) {
		for i, t := range f.types {
			isInt := false
			for _, j := range intArgs {
				isInt = isInt || i == j
			}
			if isInt {
				if !numericOrUnknown(t) {
					return "", noFunc(f.name, f.types)
				}
				continue
			}
			if !textOrUnknown(t) {
				return "", noFunc(f.name, f.types)
			}
		}
		return ret, nil
	}
}

// bitFunc types a function whose first argument is a bit string (an
// untyped literal read as one) and whose arguments at intArgs are
// integers, returning ret.
func bitFunc(ret bytdb.ColType, intArgs ...int) func(f *funcCall) (bytdb.ColType, error) {
	return func(f *funcCall) (bytdb.ColType, error) {
		for i, t := range f.types {
			switch {
			case slices.Contains(intArgs, i):
				if t != bytdb.TInt && t != tyNull && t != tyUnknown {
					return "", noFunc(f.name, f.types)
				}
				if t == tyUnknown {
					f.castArg(i, "bigint")
				}
			case bitType(t) || t == tyNull:
			case t == tyUnknown:
				f.castArg(i, "varbit")
			default:
				return "", noFunc(f.name, f.types)
			}
		}
		return ret, nil
	}
}

// bitOrText types a text function that also takes bit strings: when its
// first argument is one, every non-integer argument must be one (an
// untyped literal read as one) and the result is bitRet; otherwise the
// text typing applies.
func bitOrText(text func(*funcCall) (bytdb.ColType, error), bitRet bytdb.ColType, intArgs ...int) func(*funcCall) (bytdb.ColType, error) {
	bits := bitFunc(bitRet, intArgs...)
	return func(f *funcCall) (bytdb.ColType, error) {
		if len(f.types) > 0 && bitType(f.types[0]) {
			return bits(f)
		}
		return text(f)
	}
}

// intervalFunc types a one-interval-argument function returning an
// interval (justify_*): an untyped literal is read as an interval.
func intervalFunc(f *funcCall) (bytdb.ColType, error) {
	switch f.types[0] {
	case bytdb.TInterval, tyNull:
	case tyUnknown:
		f.castArg(0, "interval")
	default:
		return "", noFunc(f.name, f.types)
	}
	return bytdb.TInterval, nil
}

// temporalArg makes argument i a timestamp for evaluation: a date
// becomes its midnight, an untyped literal is read as a timestamp.
func temporalArg(f *funcCall, i int) error {
	switch f.types[i] {
	case bytdb.TTimestamp:
	case bytdb.TDate, tyUnknown:
		f.castArg(i, "timestamp")
	case tyNull:
	default:
		return noFunc(f.name, f.types)
	}
	return nil
}

// textArg makes argument i renderable by textOf: the types whose
// runtime value is not their text — a timestamp or date (a count), a
// uuid (16 bytes) — are cast to text first. Numbers, booleans and bytea
// render by textOf as Postgres's output functions do (true is 't', not
// the 'true' a ::text cast gives).
func textArg(f *funcCall, i int) {
	switch f.types[i] {
	case bytdb.TTimestamp, bytdb.TDate, bytdb.TUUID:
		f.castArg(i, "text")
	}
}

var scalarFuncs map[string]*scalarFunc

func init() {
	// num reads a float argument. A numeric converts here, which is the
	// float8 fallback for the functions without a numeric
	// implementation (sqrt, exp, ln, log, power, cbrt; plan decision Q9).
	num := func(v any) (float64, bool) {
		switch n := v.(type) {
		case int64:
			return float64(n), true
		case float64:
			return n, true
		case bytdb.Decimal:
			return n.Float64(), true
		}
		return 0, false
	}
	str := func(v any) string {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprint(v)
	}
	intArg := func(v any) (int64, error) {
		switch n := v.(type) {
		case int64:
			return n, nil
		case float64:
			if n != math.Trunc(n) {
				return 0, serr.New("integer argument expected", "value", fmt.Sprint(n))
			}
			return int64(n), nil
		}
		return 0, serr.New("integer argument expected", "value", fmt.Sprint(v))
	}
	floatFn := func(fn func(float64) (float64, error)) func([]any) (any, error) {
		return func(a []any) (any, error) {
			x, ok := num(a[0])
			if !ok {
				return nil, serr.New("numeric argument expected")
			}
			return fn(x)
		}
	}
	// keepInt applies a whole-number function: an integer is itself, a
	// numeric goes through decFn (exact, scale 0), a float through fn.
	keepIntDec := func(fn func(float64) float64, decFn func(bytdb.Decimal) bytdb.Decimal) func([]any) (any, error) {
		return func(a []any) (any, error) {
			if n, ok := a[0].(int64); ok {
				return n, nil // ceil/floor/round/trunc of an integer is itself
			}
			if d, ok := a[0].(bytdb.Decimal); ok {
				return decFn(d), nil
			}
			x, ok := num(a[0])
			if !ok {
				return nil, serr.New("numeric argument expected")
			}
			return fn(x), nil
		}
	}
	// scaled applies fn at n decimal places: round(2.345, 2) = 2.35. An
	// integer or numeric x takes numeric's exact path (decFn at the
	// places, half away from zero for round), whose result is numeric
	// with max(places, 0) digits after the point: round(5, 1) is 5.0.
	scaled := func(fn func(float64) float64, decFn func(bytdb.Decimal, int32) bytdb.Decimal) func([]any) (any, error) {
		one := keepIntDec(fn, func(d bytdb.Decimal) bytdb.Decimal { return decFn(d, 0) })
		return func(a []any) (any, error) {
			if len(a) == 1 {
				return one(a)
			}
			if d, ok := asDecimal(a[0]); ok {
				places, err := intArg(a[1])
				if err != nil {
					return nil, err
				}
				// Postgres clamps the places to numeric's display range.
				places = max(min(places, decimal.MaxScale), -decimal.MaxIntDigits)
				return decFn(d, int32(places)), nil
			}
			x, ok := num(a[0])
			if !ok {
				return nil, serr.New("numeric argument expected")
			}
			places, err := intArg(a[1])
			if err != nil {
				return nil, err
			}
			p := math.Pow(10, float64(places))
			return fn(x*p) / p, nil
		}
	}
	runes := func(s string) []rune { return []rune(s) }
	pad := func(left bool) func([]any) (any, error) {
		return func(a []any) (any, error) {
			s := runes(str(a[0]))
			n, err := intArg(a[1])
			if err != nil {
				return nil, err
			}
			fill := []rune(" ")
			if len(a) > 2 {
				fill = runes(str(a[2]))
			}
			if n <= int64(len(s)) {
				return string(s[:max(n, 0)]), nil // Postgres truncates to n
			}
			if len(fill) == 0 {
				return string(s), nil
			}
			padding := make([]rune, 0, int(n)-len(s))
			for i := 0; len(padding) < int(n)-len(s); i++ {
				padding = append(padding, fill[i%len(fill)])
			}
			if left {
				return string(padding) + string(s), nil
			}
			return string(s) + string(padding), nil
		}
	}
	trimFn := func(l, r bool) func([]any) (any, error) {
		return func(a []any) (any, error) {
			s, cut := str(a[0]), " "
			if len(a) > 1 {
				cut = str(a[1])
			}
			if l {
				s = strings.TrimLeft(s, cut)
			}
			if r {
				s = strings.TrimRight(s, cut)
			}
			return s, nil
		}
	}
	extremum := func(greatest bool) func([]any) (any, error) {
		return func(a []any) (any, error) {
			var best any
			for _, v := range a {
				if v == nil {
					continue // NULLs are ignored; all-NULL gives NULL
				}
				if best == nil {
					best = v
					continue
				}
				c, ok := compareVals(v, best)
				if !ok {
					return nil, serr.New("greatest/least arguments are not comparable")
				}
				if (greatest && c > 0) || (!greatest && c < 0) {
					best = v
				}
			}
			return best, nil
		}
	}
	jsonbParse := func(v any) (any, error) {
		var doc any
		if err := json.Unmarshal([]byte(str(v)), &doc); err != nil {
			return nil, serr.New("invalid input syntax for type json")
		}
		return doc, nil
	}

	scalarFuncs = map[string]*scalarFunc{
		// --- math ---
		"abs": {min: 1, max: 1, typ: numericResult, eval: func(a []any) (any, error) {
			if n, ok := a[0].(int64); ok {
				if n == math.MinInt64 {
					return nil, errIntRange()
				}
				if n < 0 {
					return -n, nil
				}
				return n, nil
			}
			if d, ok := a[0].(bytdb.Decimal); ok {
				return d.Abs(), nil // keeps the scale: abs(-2.50) is 2.50
			}
			x, ok := num(a[0])
			if !ok {
				return nil, serr.New("numeric argument expected")
			}
			return math.Abs(x), nil
		}},
		// round and trunc follow numeric's rules: half away from zero, and
		// an optional number of decimal places.
		"round":   {min: 1, max: 2, typ: roundResult, eval: scaled(math.Round, bytdb.Decimal.Round)},
		"trunc":   {min: 1, max: 2, typ: roundResult, eval: scaled(math.Trunc, bytdb.Decimal.Trunc)},
		"ceil":    {min: 1, max: 1, typ: numericResult, eval: keepIntDec(math.Ceil, bytdb.Decimal.Ceil)},
		"ceiling": {min: 1, max: 1, typ: numericResult, eval: keepIntDec(math.Ceil, bytdb.Decimal.Ceil)},
		"floor":   {min: 1, max: 1, typ: numericResult, eval: keepIntDec(math.Floor, bytdb.Decimal.Floor)},
		"sign": {min: 1, max: 1, typ: numericResult, eval: func(a []any) (any, error) {
			if d, ok := a[0].(bytdb.Decimal); ok {
				if d.IsNaN() {
					return d, nil // sign(NaN) is NaN (N-051); the infinities are ±1
				}
				return decimal.FromInt64(int64(d.Sign())), nil // numeric -1, 0 or 1
			}
			if n, ok := a[0].(int64); ok {
				switch {
				case n > 0:
					return int64(1), nil
				case n < 0:
					return int64(-1), nil
				}
				return int64(0), nil
			}
			x, _ := num(a[0])
			switch {
			case x > 0:
				return 1.0, nil
			case x < 0:
				return -1.0, nil
			}
			return 0.0, nil
		}},
		"mod": {min: 2, max: 2, typ: numericResult, eval: func(a []any) (any, error) {
			_, xd := a[0].(bytdb.Decimal)
			_, yd := a[1].(bytdb.Decimal)
			if xd || yd {
				x, xok := asDecimal(a[0])
				y, yok := asDecimal(a[1])
				if xok && yok {
					return decimalArith("%", x, y)
				}
			}
			if x, ok := a[0].(int64); ok {
				if y, ok := a[1].(int64); ok {
					if y == 0 {
						return nil, serr.New("division by zero")
					}
					if y == -1 {
						return int64(0), nil // MinInt64 % -1 would trap
					}
					return x % y, nil
				}
			}
			x, _ := num(a[0])
			y, _ := num(a[1])
			if y == 0 {
				return nil, serr.New("division by zero")
			}
			return math.Mod(x, y), nil
		}},
		// sqrt, exp, ln, log and power exist for double precision and for
		// numeric, as in Postgres: mathResult (logResult for log) picks
		// the version, and mathEval runs the matching one. The numeric
		// versions are decimal.Sqrt/Exp/Ln/Log/Pow, which reproduce
		// Postgres's result scales and digits (sqrt(16.0) is
		// 4.000000000000000). cbrt has no numeric version in Postgres
		// either, so a numeric argument converts to float8.
		"power": {min: 2, max: 2, typ: mathResult, eval: mathEval(
			func(x []float64) (float64, error) {
				switch {
				case x[0] == 0 && x[1] < 0:
					return 0, decimal.ErrPowZeroNeg
				case x[0] < 0 && x[1] != math.Trunc(x[1]):
					return 0, decimal.ErrPowNegNonInt
				}
				return math.Pow(x[0], x[1]), nil
			},
			func(d []bytdb.Decimal) (bytdb.Decimal, error) { return decimal.Pow(d[0], d[1]) })},
		"sqrt": {min: 1, max: 1, typ: mathResult, eval: mathEval(
			func(x []float64) (float64, error) {
				if x[0] < 0 {
					return 0, decimal.ErrSqrtNegative
				}
				return math.Sqrt(x[0]), nil
			},
			func(d []bytdb.Decimal) (bytdb.Decimal, error) { return decimal.Sqrt(d[0]) })},
		"cbrt": {min: 1, max: 1, typ: floatResult, eval: floatFn(func(x float64) (float64, error) { return math.Cbrt(x), nil })},
		"exp": {min: 1, max: 1, typ: mathResult, eval: mathEval(
			func(x []float64) (float64, error) { return math.Exp(x[0]), nil },
			func(d []bytdb.Decimal) (bytdb.Decimal, error) { return decimal.Exp(d[0]) })},
		"ln": {min: 1, max: 1, typ: mathResult, eval: mathEval(
			func(x []float64) (float64, error) {
				if err := floatLogArg(x[0]); err != nil {
					return 0, err
				}
				return math.Log(x[0]), nil
			},
			func(d []bytdb.Decimal) (bytdb.Decimal, error) { return decimal.Ln(d[0]) })},
		// log(x) is base 10; log(b, x) is base b.
		"log": {min: 1, max: 2, typ: logResult, eval: mathEval(
			func(x []float64) (float64, error) {
				if len(x) == 1 {
					if err := floatLogArg(x[0]); err != nil {
						return 0, err
					}
					return math.Log10(x[0]), nil
				}
				if err := floatLogArg(x[0]); err != nil {
					return 0, err
				}
				if err := floatLogArg(x[1]); err != nil {
					return 0, err
				}
				if x[0] == 1 {
					return 0, decimal.ErrDivByZero
				}
				return math.Log(x[1]) / math.Log(x[0]), nil
			},
			func(d []bytdb.Decimal) (bytdb.Decimal, error) {
				if len(d) == 1 {
					return decimal.Log(decimal.FromInt64(10), d[0])
				}
				return decimal.Log(d[0], d[1])
			})},
		// The scale functions (Postgres 13+): scale is the display
		// scale; min_scale the smallest that shows the value exactly;
		// trim_scale the value at that scale. NaN and the infinities
		// have no digits, so scale and min_scale are NULL for them, and
		// trim_scale returns them unchanged (N-051).
		"scale": {min: 1, max: 1, typ: numericArg(bytdb.TInt), eval: func(a []any) (any, error) {
			d, _ := asDecimal(a[0])
			if !d.IsFinite() {
				return nil, nil
			}
			return int64(d.Scale()), nil
		}},
		"min_scale": {min: 1, max: 1, typ: numericArg(bytdb.TInt), eval: func(a []any) (any, error) {
			d, _ := asDecimal(a[0])
			if !d.IsFinite() {
				return nil, nil
			}
			return int64(d.Normalize().Scale()), nil
		}},
		"trim_scale": {min: 1, max: 1, typ: numericArg(bytdb.TNumeric), eval: func(a []any) (any, error) {
			d, _ := asDecimal(a[0])
			return d.Normalize(), nil
		}},
		"pi":     {min: 0, max: 0, ret: bytdb.TFloat, eval: func([]any) (any, error) { return math.Pi, nil }},
		"random": {min: 0, max: 0, ret: bytdb.TFloat, eval: func([]any) (any, error) { return rand.Float64(), nil }},
		// greatest/least ignore NULLs; the typing pass unifies their
		// arguments (typer.funcExpr), as it does coalesce's.
		"greatest": {min: 1, max: -1, nullSafe: true, eval: extremum(true)},
		"least":    {min: 1, max: -1, nullSafe: true, eval: extremum(false)},

		// --- strings ---
		// substring of a bit string is a bit string (Postgres's bitsubstr
		// returns bit): the same positions over its '0'/'1' text (N-053).
		"substring": {min: 2, max: 3, typ: bitOrText(textArgs(bytdb.TString, 1, 2), bytdb.TBit, 1, 2), eval: func(a []any) (any, error) {
			r := runes(str(a[0]))
			from, err := intArg(a[1])
			if err != nil {
				return nil, err
			}
			// Positions are 1-based; a start before 1 still counts toward
			// the length, as in Postgres: substring('abc', 0, 2) is 'a'.
			end := int64(len(r)) + 1
			if len(a) > 2 {
				n, err := intArg(a[2])
				if err != nil {
					return nil, err
				}
				if n < 0 {
					return nil, serr.New("negative substring length not allowed")
				}
				end = min(from+n, end)
			}
			from = max(from, 1)
			if from >= end {
				return "", nil
			}
			return string(r[from-1 : end-1]), nil
		}},
		"left": {min: 2, max: 2, typ: textArgs(bytdb.TString, 1), eval: func(a []any) (any, error) {
			r := runes(str(a[0]))
			n, err := intArg(a[1])
			if err != nil {
				return nil, err
			}
			if n < 0 {
				n = max(int64(len(r))+n, 0)
			}
			return string(r[:min(n, int64(len(r)))]), nil
		}},
		"right": {min: 2, max: 2, typ: textArgs(bytdb.TString, 1), eval: func(a []any) (any, error) {
			r := runes(str(a[0]))
			n, err := intArg(a[1])
			if err != nil {
				return nil, err
			}
			if n < 0 {
				n = max(int64(len(r))+n, 0)
			}
			n = min(n, int64(len(r)))
			return string(r[int64(len(r))-n:]), nil
		}},
		"btrim":   {min: 1, max: 2, typ: textArgs(bytdb.TString), eval: trimFn(true, true)},
		"ltrim":   {min: 1, max: 2, typ: textArgs(bytdb.TString), eval: trimFn(true, false)},
		"rtrim":   {min: 1, max: 2, typ: textArgs(bytdb.TString), eval: trimFn(false, true)},
		"replace": {min: 3, max: 3, typ: textArgs(bytdb.TString), eval: func(a []any) (any, error) { return strings.ReplaceAll(str(a[0]), str(a[1]), str(a[2])), nil }},
		// strpos is also POSITION(b IN a) — for bit strings too (N-053).
		"strpos": {min: 2, max: 2, typ: bitOrText(textArgs(bytdb.TInt), bytdb.TInt), eval: func(a []any) (any, error) {
			s, sub := str(a[0]), str(a[1])
			i := strings.Index(s, sub)
			if i < 0 {
				return int64(0), nil
			}
			return int64(utf8.RuneCountInString(s[:i]) + 1), nil
		}},
		"split_part": {min: 3, max: 3, typ: textArgs(bytdb.TString, 2), eval: func(a []any) (any, error) {
			n, err := intArg(a[2])
			if err != nil {
				return nil, err
			}
			if n == 0 {
				return nil, serr.New("field position must not be zero")
			}
			s, d := str(a[0]), str(a[1])
			var parts []string
			if d == "" {
				parts = []string{s}
			} else {
				parts = strings.Split(s, d)
			}
			if n < 0 {
				n = int64(len(parts)) + n + 1 // counted from the end (Postgres 14)
			}
			if n < 1 || n > int64(len(parts)) {
				return "", nil
			}
			return parts[n-1], nil
		}},
		"repeat": {min: 2, max: 2, typ: textArgs(bytdb.TString, 1), eval: func(a []any) (any, error) {
			n, err := intArg(a[1])
			if err != nil {
				return nil, err
			}
			if n <= 0 {
				return "", nil
			}
			s := str(a[0])
			if int64(len(s))*n > 1<<28 {
				return nil, serr.New("requested length too large")
			}
			return strings.Repeat(s, int(n)), nil
		}},
		"reverse": {min: 1, max: 1, typ: textArgs(bytdb.TString), eval: func(a []any) (any, error) {
			r := runes(str(a[0]))
			for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
				r[i], r[j] = r[j], r[i]
			}
			return string(r), nil
		}},
		"lpad": {min: 2, max: 3, typ: textArgs(bytdb.TString, 1), eval: pad(true)},
		"rpad": {min: 2, max: 3, typ: textArgs(bytdb.TString, 1), eval: pad(false)},
		"initcap": {min: 1, max: 1, typ: textArgs(bytdb.TString), eval: func(a []any) (any, error) {
			r := runes(str(a[0]))
			start := true
			for i, c := range r {
				if unicode.IsLetter(c) || unicode.IsDigit(c) {
					if start {
						r[i] = unicode.ToUpper(c)
					} else {
						r[i] = unicode.ToLower(c)
					}
					start = false
				} else {
					start = true
				}
			}
			return string(r), nil
		}},
		"starts_with": {min: 2, max: 2, typ: textArgs(bytdb.TBool), eval: func(a []any) (any, error) { return strings.HasPrefix(str(a[0]), str(a[1])), nil }},
		"md5": {min: 1, max: 1, typ: textArgs(bytdb.TString), eval: func(a []any) (any, error) {
			sum := md5.Sum([]byte(str(a[0])))
			return hex.EncodeToString(sum[:]), nil
		}},
		"ascii": {min: 1, max: 1, typ: textArgs(bytdb.TInt), eval: func(a []any) (any, error) {
			r, _ := utf8.DecodeRuneInString(str(a[0]))
			if r == utf8.RuneError {
				return int64(0), nil
			}
			return int64(r), nil
		}},
		"chr": {min: 1, max: 1, typ: textArgs(bytdb.TString, 0), eval: func(a []any) (any, error) {
			n, err := intArg(a[0])
			if err != nil {
				return nil, err
			}
			if n == 0 {
				return nil, serr.New("null character not permitted")
			}
			if n < 0 || n > unicode.MaxRune {
				return nil, serr.New("requested character too large for encoding")
			}
			return string(rune(n)), nil
		}},
		// bit_length of text or bytea is 8 bits per byte. Over a bit
		// string the typing pass rewrites it to the bit count first.
		"bit_length": {min: 1, max: 1, typ: func(f *funcCall) (bytdb.ColType, error) {
			if t := f.types[0]; !textOrUnknown(t) && t != bytdb.TBytes {
				return "", noFunc(f.name, f.types)
			}
			return bytdb.TInt, nil
		}, eval: func(a []any) (any, error) {
			if b, ok := a[0].([]byte); ok {
				return 8 * int64(len(b)), nil
			}
			return 8 * int64(len(str(a[0]))), nil
		}},
		"octet_length": {min: 1, max: 1, typ: func(f *funcCall) (bytdb.ColType, error) {
			if t := f.types[0]; !textOrUnknown(t) && t != bytdb.TBytes {
				return "", noFunc(f.name, f.types)
			}
			return bytdb.TInt, nil
		}, eval: func(a []any) (any, error) {
			if b, ok := a[0].([]byte); ok {
				return int64(len(b)), nil
			}
			return int64(len(str(a[0]))), nil
		}},
		// concat and concat_ws skip NULLs and render other values as text
		// (typing casts typed non-text arguments to text first).
		"concat": {min: 1, max: -1, nullSafe: true, typ: func(f *funcCall) (bytdb.ColType, error) {
			for i := range f.types {
				textArg(f, i)
			}
			return bytdb.TString, nil
		}, eval: func(a []any) (any, error) {
			var b strings.Builder
			for _, v := range a {
				if v != nil {
					b.WriteString(textOf(v))
				}
			}
			return b.String(), nil
		}},
		"concat_ws": {min: 2, max: -1, nullSafe: true, typ: func(f *funcCall) (bytdb.ColType, error) {
			for i := range f.types {
				textArg(f, i)
			}
			return bytdb.TString, nil
		}, eval: func(a []any) (any, error) {
			if a[0] == nil {
				return nil, nil // a NULL separator makes the result NULL
			}
			var parts []string
			for _, v := range a[1:] {
				if v != nil {
					parts = append(parts, textOf(v))
				}
			}
			return strings.Join(parts, textOf(a[0])), nil
		}},

		// --- date/time (a timestamp is its microsecond count; typing
		// turns a date argument into its midnight) ---
		"date_part": {min: 2, max: 2, typ: func(f *funcCall) (bytdb.ColType, error) {
			if !textOrUnknown(f.types[0]) {
				return "", noFunc(f.name, f.types)
			}
			if f.types[1] == bytdb.TInterval {
				return bytdb.TFloat, nil
			}
			return bytdb.TFloat, temporalArg(f, 1)
		}, eval: func(a []any) (any, error) {
			if iv, ok := a[1].(bytdb.Interval); ok {
				// EXTRACT lowers to date_part, so both come here: a
				// float8, as timestamps' are in bytdb (Postgres 14+'s
				// EXTRACT is numeric; the values agree).
				return iv.Part(str(a[0]))
			}
			micros, ok := a[1].(int64)
			if !ok {
				return nil, serr.New("timestamp argument expected")
			}
			return datePart(strings.ToLower(str(a[0])), micros)
		}},
		"date_trunc": {min: 2, max: 2, typ: func(f *funcCall) (bytdb.ColType, error) {
			if !textOrUnknown(f.types[0]) {
				return "", noFunc(f.name, f.types)
			}
			if f.types[1] == bytdb.TInterval {
				return bytdb.TInterval, nil
			}
			return bytdb.TTimestamp, temporalArg(f, 1)
		}, eval: func(a []any) (any, error) {
			if iv, ok := a[1].(bytdb.Interval); ok {
				return iv.Trunc(str(a[0]))
			}
			micros, ok := a[1].(int64)
			if !ok {
				return nil, serr.New("timestamp argument expected")
			}
			return dateTrunc(strings.ToLower(str(a[0])), micros)
		}},
		"to_char": {min: 2, max: 2, typ: func(f *funcCall) (bytdb.ColType, error) {
			if !textOrUnknown(f.types[1]) {
				return "", noFunc(f.name, f.types)
			}
			return bytdb.TString, temporalArg(f, 0)
		}, eval: func(a []any) (any, error) {
			micros, ok := a[0].(int64)
			if !ok {
				return nil, serr.New("to_char supports timestamp and date arguments only")
			}
			return toChar(micros, str(a[1])), nil
		}},
		// to_timestamp(seconds since the epoch).
		// --- bit strings (N-053): the values are '0'/'1' text ---
		"get_bit": {min: 2, max: 2, typ: bitFunc(bytdb.TInt, 1), eval: func(a []any) (any, error) {
			b := a[0].(string)
			n, err := intArg(a[1])
			if err != nil {
				return nil, err
			}
			if n < 0 || n >= int64(len(b)) {
				return nil, serr.New(fmt.Sprintf("bit index %d out of valid range (0..%d)", n, len(b)-1))
			}
			return int64(b[n] - '0'), nil
		}},
		"set_bit": {min: 3, max: 3, typ: bitFunc(bytdb.TBit, 1, 2), eval: func(a []any) (any, error) {
			b := a[0].(string)
			n, err := intArg(a[1])
			if err != nil {
				return nil, err
			}
			v, err := intArg(a[2])
			if err != nil {
				return nil, err
			}
			if n < 0 || n >= int64(len(b)) {
				return nil, serr.New(fmt.Sprintf("bit index %d out of valid range (0..%d)", n, len(b)-1))
			}
			if v != 0 && v != 1 {
				return nil, serr.New("new bit must be 0 or 1")
			}
			out := []byte(b)
			out[n] = '0' + byte(v)
			return string(out), nil
		}},
		"bit_count": {min: 1, max: 1, typ: bitFunc(bytdb.TInt), eval: func(a []any) (any, error) {
			return int64(strings.Count(a[0].(string), "1")), nil
		}},

		// --- money (N-053) ---
		"cash_words": {min: 1, max: 1, typ: func(f *funcCall) (bytdb.ColType, error) {
			switch f.types[0] {
			case bytdb.TMoney, tyNull:
			case tyUnknown:
				f.castArg(0, "money")
			default:
				return "", noFunc(f.name, f.types)
			}
			return bytdb.TString, nil
		}, eval: func(a []any) (any, error) {
			return bytdb.MoneyWords(a[0].(int64)), nil
		}},

		// --- intervals (N-053) ---
		"justify_days": {min: 1, max: 1, typ: intervalFunc, eval: func(a []any) (any, error) {
			return a[0].(bytdb.Interval).JustifyDays()
		}},
		"justify_hours": {min: 1, max: 1, typ: intervalFunc, eval: func(a []any) (any, error) {
			return a[0].(bytdb.Interval).JustifyHours()
		}},
		"justify_interval": {min: 1, max: 1, typ: intervalFunc, eval: func(a []any) (any, error) {
			return a[0].(bytdb.Interval).JustifyInterval()
		}},
		// make_interval(years, months, weeks, days, hours, mins, secs):
		// the parser fills every parameter (namedArgFuncs), so seven
		// arguments arrive; fewer when called positionally with a short
		// list, the rest defaulting to 0.
		"make_interval": {min: 0, max: 7, typ: func(f *funcCall) (bytdb.ColType, error) {
			for i, t := range f.types {
				switch {
				case i == 6 && numericOrUnknown(t):
					if t != bytdb.TFloat && t != tyNull {
						f.castArg(i, "float8")
					}
				case t == bytdb.TInt || t == tyNull:
				case t == tyUnknown:
					f.castArg(i, "bigint")
				default:
					return "", noFunc(f.name, f.types)
				}
			}
			return bytdb.TInterval, nil
		}, eval: func(a []any) (any, error) {
			var ints [6]int64
			for i := range min(len(a), 6) {
				ints[i] = a[i].(int64)
			}
			var secs float64
			if len(a) == 7 {
				secs, _ = num(a[6])
			}
			return interval.Make(ints[0], ints[1], ints[2], ints[3], ints[4], ints[5], secs)
		}},
		// age(t1, t2) is the symbolic difference (years, months, days);
		// age(t) measures from today's midnight, as Postgres's
		// age(current_date, t) does (bytdb's day is the UTC one).
		"age": {min: 1, max: 2, typ: func(f *funcCall) (bytdb.ColType, error) {
			for i := range f.types {
				if err := temporalArg(f, i); err != nil {
					return "", err
				}
			}
			return bytdb.TInterval, nil
		}, eval: func(a []any) (any, error) {
			t1, ok1 := a[0].(int64)
			if len(a) == 1 {
				u := time.Now().UTC()
				midnight := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC).UnixMicro()
				return interval.Age(midnight, t1)
			}
			t2, ok2 := a[1].(int64)
			if !ok1 || !ok2 {
				return nil, serr.New("timestamp arguments expected")
			}
			return interval.Age(t1, t2)
		}},
		// isfinite: every Postgres 16 interval is finite; a timestamp or
		// date is unless it is ±infinity.
		"isfinite": {min: 1, max: 1, typ: func(f *funcCall) (bytdb.ColType, error) {
			if f.types[0] == bytdb.TInterval {
				return bytdb.TBool, nil
			}
			return bytdb.TBool, temporalArg(f, 0)
		}, eval: func(a []any) (any, error) {
			if us, ok := a[0].(int64); ok {
				return us != bytdb.TimestampInfinity && us != bytdb.TimestampNegInfinity, nil
			}
			return true, nil
		}},

		"to_timestamp": {min: 1, max: 1, typ: floatResultAs(bytdb.TTimestamp), eval: func(a []any) (any, error) {
			x, ok := num(a[0])
			if !ok {
				return nil, serr.New("to_timestamp takes seconds since the epoch")
			}
			// NaN has no instant (float8_timestamptz's 22008); it
			// reaches here from a float8 NaN, and since N-051 from
			// numeric's. int64(NaN) would be an arbitrary instant.
			if math.IsNaN(x) {
				return nil, serr.New("timestamp cannot be NaN")
			}
			if math.IsInf(x, 1) {
				return int64(bytdb.TimestampInfinity), nil
			}
			if math.IsInf(x, -1) {
				return int64(bytdb.TimestampNegInfinity), nil
			}
			return int64(math.Round(x * 1e6)), nil
		}},
		"make_date": {min: 3, max: 3, typ: floatResultAs(bytdb.TDate), eval: func(a []any) (any, error) {
			y, err := intArg(a[0])
			if err != nil {
				return nil, err
			}
			m, err := intArg(a[1])
			if err != nil {
				return nil, err
			}
			d, err := intArg(a[2])
			if err != nil {
				return nil, err
			}
			t := time.Date(int(y), time.Month(m), int(d), 0, 0, 0, 0, time.UTC)
			if t.Year() != int(y) || t.Month() != time.Month(m) || t.Day() != int(d) {
				return nil, serr.New(fmt.Sprintf("date field value out of range: %d-%02d-%02d", y, m, d))
			}
			return t.Unix() / 86400, nil
		}},
		// timezone(zone, ts) is x AT TIME ZONE zone: the wall-clock time
		// of the instant in that zone, as a timestamp (bytdb's one
		// timestamp type renders it with +00).
		"timezone": {min: 2, max: 2, typ: func(f *funcCall) (bytdb.ColType, error) {
			if !textOrUnknown(f.types[0]) {
				return "", noFunc(f.name, f.types)
			}
			return bytdb.TTimestamp, temporalArg(f, 1)
		}, eval: func(a []any) (any, error) {
			micros, ok := a[1].(int64)
			if !ok {
				return nil, serr.New("timestamp argument expected")
			}
			loc, err := zoneLocation(str(a[0]))
			if err != nil {
				return nil, err
			}
			wall := time.UnixMicro(micros).In(loc)
			y, mo, d := wall.Date()
			return time.Date(y, mo, d, wall.Hour(), wall.Minute(), wall.Second(), wall.Nanosecond(), time.UTC).UnixMicro(), nil
		}},

		// --- jsonb ---
		"jsonb_typeof": {min: 1, max: 1, ret: bytdb.TString, eval: func(a []any) (any, error) {
			doc, err := jsonbParse(a[0])
			if err != nil {
				return nil, err
			}
			switch doc.(type) {
			case map[string]any:
				return "object", nil
			case []any:
				return "array", nil
			case string:
				return "string", nil
			case float64:
				return "number", nil
			case bool:
				return "boolean", nil
			}
			return "null", nil
		}},
		"jsonb_array_length": {min: 1, max: 1, ret: bytdb.TInt, eval: func(a []any) (any, error) {
			doc, err := jsonbParse(a[0])
			if err != nil {
				return nil, err
			}
			arr, ok := doc.([]any)
			if !ok {
				return nil, serr.New("cannot get array length of a non-array")
			}
			return int64(len(arr)), nil
		}},
	}
	// Postgres's other spellings.
	scalarFuncs["pow"] = scalarFuncs["power"]
	// log10 (Postgres 12+) is one-argument log under its own name. The
	// map holds pointers, so it gets its own copy to narrow.
	log10 := *scalarFuncs["log"]
	log10.max = 1
	scalarFuncs["log10"] = &log10
	scalarFuncs["substr"] = scalarFuncs["substring"]
	scalarFuncs["json_typeof"] = scalarFuncs["jsonb_typeof"]
	scalarFuncs["json_array_length"] = scalarFuncs["jsonb_array_length"]
	registerSessionFuncs()
}

// resultType is the call's result type for a result description: the
// typing rule run over the arguments' types, on a throwaway call (its
// casts were already applied by the typing pass).
func (f *scalarFunc) resultType(sc *scope, n *ExFunc) bytdb.ColType {
	if f.typ == nil {
		return f.ret
	}
	types := make([]bytdb.ColType, len(n.Args))
	for i, a := range n.Args {
		types[i] = exprType(sc, a)
		if types[i] == "" {
			types[i] = tyUnknown
		}
	}
	t, err := f.typ(&funcCall{name: n.Name, args: append([]Expr(nil), n.Args...), types: types})
	if err != nil || t == "" {
		return bytdb.TString
	}
	return t
}

// floatResultAs accepts numeric arguments and returns t.
func floatResultAs(t bytdb.ColType) func(f *funcCall) (bytdb.ColType, error) {
	return func(f *funcCall) (bytdb.ColType, error) {
		if _, err := floatResult(f); err != nil {
			return "", err
		}
		return t, nil
	}
}

// textOf renders a runtime value as concat does.
func textOf(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bytdb.CIText:
		return string(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return bytdb.FormatFloat(x)
	case bool:
		if x {
			return "t"
		}
		return "f"
	case []byte:
		return `\x` + hex.EncodeToString(x)
	}
	return fmt.Sprint(v)
}

// zoneLocation reads AT TIME ZONE's zone: UTC/GMT/Z, an IANA name, or a
// numeric offset (+05, -08:00).
func zoneLocation(name string) (*time.Location, error) {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "UTC", "GMT", "Z", "ZULU":
		return time.UTC, nil
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc, nil
	}
	s := strings.TrimSpace(name)
	if s != "" && (s[0] == '+' || s[0] == '-') {
		h, m, _ := strings.Cut(s[1:], ":")
		hh, err1 := strconv.Atoi(h)
		mm := 0
		var err2 error
		if m != "" {
			mm, err2 = strconv.Atoi(m)
		}
		if err1 == nil && err2 == nil {
			off := hh*3600 + mm*60
			// POSIX-style: Postgres reads '+05' as five hours WEST of UTC.
			if s[0] == '+' {
				off = -off
			}
			return time.FixedZone(name, off), nil
		}
	}
	return nil, serr.New(`time zone "` + name + `" not recognized`)
}

// datePart is EXTRACT / date_part for a timestamp's field.
func datePart(field string, micros int64) (any, error) {
	if micros == bytdb.TimestampInfinity || micros == bytdb.TimestampNegInfinity {
		if field == "epoch" || field == "year" {
			if micros > 0 {
				return math.Inf(1), nil
			}
			return math.Inf(-1), nil
		}
		return nil, nil
	}
	t := time.UnixMicro(micros).UTC()
	sec := float64(t.Second()) + float64(t.Nanosecond())/1e9
	switch field {
	case "year", "years":
		return float64(t.Year()), nil
	case "month", "months":
		return float64(t.Month()), nil
	case "day", "days":
		return float64(t.Day()), nil
	case "hour", "hours":
		return float64(t.Hour()), nil
	case "minute", "minutes":
		return float64(t.Minute()), nil
	case "second", "seconds":
		return sec, nil
	case "millisecond", "milliseconds":
		return sec * 1e3, nil
	case "microsecond", "microseconds":
		return float64(t.Second())*1e6 + float64(t.Nanosecond()/1e3), nil
	case "dow":
		return float64(t.Weekday()), nil
	case "isodow":
		wd := int(t.Weekday())
		if wd == 0 {
			wd = 7
		}
		return float64(wd), nil
	case "doy":
		return float64(t.YearDay()), nil
	case "week":
		_, w := t.ISOWeek()
		return float64(w), nil
	case "isoyear":
		y, _ := t.ISOWeek()
		return float64(y), nil
	case "quarter":
		return float64((int(t.Month())-1)/3 + 1), nil
	case "decade":
		return math.Floor(float64(t.Year()) / 10), nil
	case "century":
		y := t.Year()
		if y > 0 {
			return float64((y + 99) / 100), nil
		}
		return float64(-((99 - y) / 100)), nil
	case "millennium":
		y := t.Year()
		if y > 0 {
			return float64((y + 999) / 1000), nil
		}
		return float64(-((999 - y) / 1000)), nil
	case "epoch":
		return float64(micros) / 1e6, nil
	case "timezone", "timezone_hour", "timezone_minute":
		return 0.0, nil // every bytdb timestamp is UTC
	}
	return nil, serr.New(`unit "` + field + `" not recognized for type timestamp`)
}

// dateTrunc truncates a timestamp to the start of its field, in UTC.
func dateTrunc(field string, micros int64) (any, error) {
	if micros == bytdb.TimestampInfinity || micros == bytdb.TimestampNegInfinity {
		return micros, nil
	}
	t := time.UnixMicro(micros).UTC()
	y, mo, d := t.Date()
	var out time.Time
	switch field {
	case "microseconds", "microsecond":
		return micros, nil
	case "milliseconds", "millisecond":
		out = t.Truncate(time.Millisecond)
	case "second", "seconds":
		out = t.Truncate(time.Second)
	case "minute", "minutes":
		out = time.Date(y, mo, d, t.Hour(), t.Minute(), 0, 0, time.UTC)
	case "hour", "hours":
		out = time.Date(y, mo, d, t.Hour(), 0, 0, 0, time.UTC)
	case "day", "days":
		out = time.Date(y, mo, d, 0, 0, 0, 0, time.UTC)
	case "week", "weeks":
		wd := (int(t.Weekday()) + 6) % 7 // ISO weeks start Monday
		out = time.Date(y, mo, d-wd, 0, 0, 0, 0, time.UTC)
	case "month", "months":
		out = time.Date(y, mo, 1, 0, 0, 0, 0, time.UTC)
	case "quarter":
		out = time.Date(y, time.Month((int(mo)-1)/3*3+1), 1, 0, 0, 0, 0, time.UTC)
	case "year", "years":
		out = time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
	case "decade":
		out = time.Date(y-((y%10+10)%10), 1, 1, 0, 0, 0, 0, time.UTC)
	case "century":
		out = time.Date((y-1)/100*100+1, 1, 1, 0, 0, 0, 0, time.UTC)
	case "millennium":
		out = time.Date((y-1)/1000*1000+1, 1, 1, 0, 0, 0, 0, time.UTC)
	default:
		return nil, serr.New(`unit "` + field + `" not recognized for type timestamp`)
	}
	return out.UnixMicro(), nil
}

// toCharPatterns are the to_char template patterns supported, longest
// first so a scan takes the longest match (HH24 before HH).
var toCharPatterns = []string{
	"MONTH", "Month", "month", "HH24", "HH12", "YYYY", "DAY", "Day", "day",
	"MON", "Mon", "mon", "DDD", "DY", "Dy", "dy", "YY", "MM", "DD", "HH", "MI", "SS", "MS", "US",
	"AM", "PM", "am", "pm", "Q", "D",
}

// toChar renders a timestamp with a Postgres to_char template (the
// common date/time patterns; text in double quotes is literal).
func toChar(micros int64, layout string) string {
	t := time.UnixMicro(micros).UTC()
	var b strings.Builder
	for i := 0; i < len(layout); {
		if layout[i] == '"' {
			j := strings.IndexByte(layout[i+1:], '"')
			if j < 0 {
				b.WriteString(layout[i+1:])
				break
			}
			b.WriteString(layout[i+1 : i+1+j])
			i += j + 2
			continue
		}
		matched := ""
		for _, p := range toCharPatterns {
			if strings.HasPrefix(layout[i:], p) {
				matched = p
				break
			}
		}
		if matched == "" {
			b.WriteByte(layout[i])
			i++
			continue
		}
		i += len(matched)
		h12 := t.Hour() % 12
		if h12 == 0 {
			h12 = 12
		}
		switch matched {
		case "YYYY":
			fmt.Fprintf(&b, "%04d", t.Year())
		case "YY":
			fmt.Fprintf(&b, "%02d", t.Year()%100)
		case "MM":
			fmt.Fprintf(&b, "%02d", int(t.Month()))
		case "DD":
			fmt.Fprintf(&b, "%02d", t.Day())
		case "DDD":
			fmt.Fprintf(&b, "%03d", t.YearDay())
		case "D":
			fmt.Fprintf(&b, "%d", int(t.Weekday())+1)
		case "Q":
			fmt.Fprintf(&b, "%d", (int(t.Month())-1)/3+1)
		case "HH24":
			fmt.Fprintf(&b, "%02d", t.Hour())
		case "HH", "HH12":
			fmt.Fprintf(&b, "%02d", h12)
		case "MI":
			fmt.Fprintf(&b, "%02d", t.Minute())
		case "SS":
			fmt.Fprintf(&b, "%02d", t.Second())
		case "MS":
			fmt.Fprintf(&b, "%03d", t.Nanosecond()/1e6)
		case "US":
			fmt.Fprintf(&b, "%06d", t.Nanosecond()/1e3)
		case "AM", "PM":
			b.WriteString(map[bool]string{true: "PM", false: "AM"}[t.Hour() >= 12])
		case "am", "pm":
			b.WriteString(map[bool]string{true: "pm", false: "am"}[t.Hour() >= 12])
		case "MONTH":
			fmt.Fprintf(&b, "%-9s", strings.ToUpper(t.Month().String()))
		case "Month":
			fmt.Fprintf(&b, "%-9s", t.Month().String())
		case "month":
			fmt.Fprintf(&b, "%-9s", strings.ToLower(t.Month().String()))
		case "MON":
			b.WriteString(strings.ToUpper(t.Month().String()[:3]))
		case "Mon":
			b.WriteString(t.Month().String()[:3])
		case "mon":
			b.WriteString(strings.ToLower(t.Month().String()[:3]))
		case "DAY":
			fmt.Fprintf(&b, "%-9s", strings.ToUpper(t.Weekday().String()))
		case "Day":
			fmt.Fprintf(&b, "%-9s", t.Weekday().String())
		case "day":
			fmt.Fprintf(&b, "%-9s", strings.ToLower(t.Weekday().String()))
		case "DY":
			b.WriteString(strings.ToUpper(t.Weekday().String()[:3]))
		case "Dy":
			b.WriteString(t.Weekday().String()[:3])
		case "dy":
			b.WriteString(strings.ToLower(t.Weekday().String()[:3]))
		}
	}
	return b.String()
}

// knownFuncs are the functions evalFunc's switch (and evalEx's special
// forms) implement outside the registry; with scalarFuncs, every
// function a statement may call. TestKnownFuncsMatchEvalFunc keeps the
// list in step with the switch.
var knownFuncs = map[string]bool{
	"now": true, "transaction_timestamp": true, "statement_timestamp": true, "clock_timestamp": true,
	"current_date": true, "gen_random_uuid": true, "coalesce": true, "nullif": true,
	"lower": true, "upper": true, "length": true, "char_length": true, "character_length": true,
	"format_type": true, "pg_get_userbyid": true, "pg_table_is_visible": true,
	"pg_function_is_visible": true, "pg_type_is_visible": true, "pg_relation_is_publishable": true,
	"pg_get_indexdef": true, "array_to_string": true, "array_length": true,
	"pg_get_constraintdef": true, "pg_get_expr": true, "pg_get_viewdef": true,
	"pg_get_partkeydef": true, "pg_table_size": true, "pg_total_relation_size": true,
	"pg_relation_size": true, "pg_indexes_size": true, "pg_size_pretty": true,
	"pg_encoding_to_char": true, "lastval": true, "currval": true, "nextval": true, "setval": true,
	"pg_get_ruledef": true, "pg_get_triggerdef": true, "pg_get_statisticsobjdef_columns": true,
	"obj_description": true, "col_description": true, "shobj_description": true,
	"array": true, "exists": true,
}

// isKnownFunc reports whether name is a function a statement may call.
func isKnownFunc(name string) bool {
	_, lib := scalarFuncs[name]
	return lib || knownFuncs[name]
}

// callScalarFunc evaluates a registry function, or reports ok=false.
func callScalarFunc(env *exEnv, name string, args []any) (v any, ok bool, err error) {
	f, found := scalarFuncs[name]
	if !found {
		return nil, false, nil
	}
	if len(args) < f.min || (f.max >= 0 && len(args) > f.max) {
		return nil, true, serr.New("wrong number of arguments", "function", name, "got", strconv.Itoa(len(args)))
	}
	if !f.nullSafe {
		for _, a := range args {
			if a == nil {
				return nil, true, nil
			}
		}
	}
	if f.envEval != nil {
		v, err = f.envEval(env, args)
	} else {
		v, err = f.eval(args)
	}
	return v, true, err
}
