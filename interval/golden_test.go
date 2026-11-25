package interval

// golden_test.go replays testdata/golden.tsv: Postgres 16.9's answers
// for interval input (with and without type modifiers), output,
// arithmetic, comparison, justify_*, timestamp ± interval, timestamp −
// timestamp, age, extract, date_part, date_trunc and make_interval,
// captured by testdata/gen.sh. Every case must match exactly: the same
// text, or an error with the same SQLSTATE.

import (
	"bufio"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

// sqlstate is the code Postgres raises for each of the package's errors.
func sqlstate(err error) string {
	var in *InputError
	var ue *UnitError
	switch {
	case errors.As(err, &in):
		if errors.Is(err, ErrFieldOverflow) {
			return "22015"
		}
		return "22007"
	case errors.As(err, &ue):
		if ue.Supported {
			return "0A000"
		}
		return "22023"
	case errors.Is(err, ErrOutOfRange), errors.Is(err, ErrTimestampOutOfRange),
		errors.Is(err, errInfiniteSub):
		return "22008"
	case errors.Is(err, ErrDivisionByZero):
		return "22012"
	}
	return "XX000 " + err.Error()
}

// typmodOf reads a golden case's modifier text: "", "(2)", "day",
// "day to second(1)".
func typmodOf(t *testing.T, m string) int32 {
	t.Helper()
	q, prec := m, -1
	if i := strings.IndexByte(m, '('); i >= 0 {
		n, err := strconv.Atoi(strings.TrimSuffix(m[i+1:], ")"))
		if err != nil {
			t.Fatalf("bad typmod %q", m)
		}
		q, prec = strings.TrimSpace(m[:i]), n
	}
	tm, err := Typmod(q, prec)
	if err != nil {
		t.Fatalf("typmod %q: %v", m, err)
	}
	return tm
}

// tsOf reads a golden timestamp: microseconds, or infinity/-infinity.
func tsOf(t *testing.T, s string) int64 {
	t.Helper()
	switch s {
	case "infinity":
		return tsInfinity
	case "-infinity":
		return tsNegInfinity
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("bad timestamp %q", s)
	}
	return n
}

func tsText(us int64) string {
	switch us {
	case tsInfinity:
		return "infinity"
	case tsNegInfinity:
		return "-infinity"
	}
	return strconv.FormatInt(us, 10)
}

func pgFloat(s string) float64 {
	switch strings.ToLower(s) {
	case "nan":
		return math.NaN()
	case "infinity":
		return math.Inf(1)
	case "-infinity":
		return math.Inf(-1)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		panic("bad float " + s)
	}
	return f
}

func TestGolden(t *testing.T) {
	f, err := os.Open("testdata/golden.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n, failed, platform := 0, 0, 0
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) != 4 {
			t.Fatalf("line %d: %d fields", n+1, len(parts))
		}
		op, a, b, want := parts[0], parts[1], parts[2], parts[3]
		n++
		got := run(t, op, a, b, want)
		if got != want && (op == "mul" || op == "div") {
			// The goldens come from an arm64 Postgres, whose gcc build
			// fuses multiply-adds; bytdb computes unfused, as x86-64
			// Postgres does. Accept the case if the fused computation
			// reproduces Postgres's answer (see Interval.scale).
			if fused := runScaleFused(t, op, a, b); fused == want {
				platform++
				continue
			}
		}
		if reason, ok := knownDiffs[op+"|"+a+"|"+b]; ok {
			if got == want {
				t.Errorf("%s(%q, %q) now matches Postgres; drop it from knownDiffs", op, a, b)
			}
			t.Logf("known difference %s(%q, %q): %s", op, a, b, reason)
			continue
		}
		if got != want {
			failed++
			if failed <= 60 {
				t.Errorf("%s(%q, %q): got %q, want %q", op, a, b, got, want)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if failed > 0 {
		t.Errorf("%d of %d golden cases differ", failed, n)
	}
	t.Logf("%d golden cases, %d matched only by the fused (arm64) arithmetic", n, platform)
}

// knownDiffs are golden cases bytdb answers differently on purpose,
// keyed op|a|b, with the reason.
var knownDiffs = map[string]string{
	// 1970-01-01 + the largest clock is MaxInt64 microseconds after
	// 1970: inside Postgres's range (its epoch is 2000), but bytdb keeps
	// timestamps as microseconds since 1970 and MaxInt64 is 'infinity'.
	"tsadd|0|2562047788:00:54.775807": "beyond bytdb's timestamp range",
	// Postgres 16's make_interval does not check for overflow: the
	// month product wraps around int32, and a huge secs converts to an
	// int64 by C's undefined float conversion (saturating on arm64).
	// Postgres 17 raises "interval out of range" for all three (probed
	// 2026-11-25 on postgres:17), and so does bytdb.
	"make|178956971,0,0,0,0,0,0|":            "PG16 wraps; PG17 and bytdb refuse",
	"make|0,0,0,0,0,0,1e300|":                "PG16 saturates; PG17 and bytdb refuse",
	"make|0,0,0,0,0,0,9223372036854.775807|": "PG16 saturates; PG17 and bytdb refuse",
}

// runScaleFused is a mul or div case computed with fused multiply-adds.
func runScaleFused(t *testing.T, op, a, b string) string {
	x, err := Parse(a)
	if err != nil {
		return ""
	}
	r, err := x.scale(pgFloat(b), op == "div", true)
	if err != nil {
		return "ERROR " + sqlstate(err)
	}
	return r.String()
}

// run evaluates one golden case in its text form.
func run(t *testing.T, op, a, b, want string) string {
	errText := func(err error) string { return "ERROR " + sqlstate(err) }
	ivText := func(iv Interval, err error) string {
		if err != nil {
			return errText(err)
		}
		return iv.String()
	}
	// Interval operands are Postgres output, but not all of it reads
	// back in Postgres 16 (the most negative clock does not), so a
	// failed operand parse is the case's answer, as it was Postgres's.
	parse := func(srcs ...string) ([]Interval, error) {
		out := make([]Interval, len(srcs))
		for i, s := range srcs {
			iv, err := Parse(s)
			if err != nil {
				return nil, err
			}
			out[i] = iv
		}
		return out, nil
	}
	switch op {
	case "in":
		tm := NoTypmod
		if b != "" {
			tm = typmodOf(t, b)
		}
		return ivText(ParseTypmod(a, tm))
	case "add", "sub", "cmp":
		ivs, err := parse(a, b)
		if err != nil {
			return errText(err)
		}
		switch op {
		case "add":
			return ivText(ivs[0].Add(ivs[1]))
		case "sub":
			return ivText(ivs[0].Sub(ivs[1]))
		}
		return strconv.Itoa(ivs[0].Cmp(ivs[1]))
	case "mul", "div", "neg", "jh", "jd", "ji":
		ivs, err := parse(a)
		if err != nil {
			return errText(err)
		}
		x := ivs[0]
		switch op {
		case "mul":
			return ivText(x.Mul(pgFloat(b)))
		case "div":
			return ivText(x.Div(pgFloat(b)))
		case "neg":
			return ivText(x.Neg())
		case "jh":
			return ivText(x.JustifyHours())
		case "jd":
			return ivText(x.JustifyDays())
		}
		return ivText(x.JustifyInterval())
	case "tsadd", "tssub":
		ivs, err := parse(b)
		if err != nil {
			return errText(err)
		}
		fn := AddTo
		if op == "tssub" {
			fn = SubFrom
		}
		r, err := fn(tsOf(t, a), ivs[0])
		if err != nil {
			return errText(err)
		}
		return tsText(r)
	case "tsmi":
		return ivText(Between(tsOf(t, a), tsOf(t, b)))
	case "age":
		return ivText(Age(tsOf(t, a), tsOf(t, b)))
	case "extract", "part", "trunc":
		ivs, err := parse(b)
		if err != nil {
			return errText(err)
		}
		x := ivs[0]
		switch op {
		case "extract":
			d, err := x.Extract(a)
			if err != nil {
				return errText(err)
			}
			return d.String()
		case "part":
			v, err := x.Part(a)
			if err != nil {
				return errText(err)
			}
			// Compare as floats: Go and Postgres spell some floats
			// differently (1e+15 vs 1e15) but must agree on the value.
			if !strings.HasPrefix(want, "ERROR") {
				w := pgFloat(want)
				if v == w || (math.IsNaN(v) && math.IsNaN(w)) {
					return want
				}
			}
			return strconv.FormatFloat(v, 'g', -1, 64)
		}
		return ivText(x.Trunc(a))
	case "make":
		var ints [6]int64
		args := strings.Split(a, ",")
		for i := range 6 {
			ints[i], _ = strconv.ParseInt(args[i], 10, 64)
		}
		secs := pgFloat(strings.Trim(args[6], "'"))
		return ivText(Make(ints[0], ints[1], ints[2], ints[3], ints[4], ints[5], secs))
	}
	t.Fatalf("unknown golden op %q", op)
	return ""
}
