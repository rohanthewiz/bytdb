package bytdb

// Deep-look D-41: the Append formatters must render exactly what the
// Format functions always did. The reference implementations below are
// the pre-D-41 Format bodies.

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"
)

func refFormatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	if f == 0 {
		return strings.TrimSuffix(s, "e+00")
	}
	_, exp, _ := strings.Cut(s, "e")
	if e, _ := strconv.Atoi(exp); e < -4 || e >= 15 {
		return s
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func refFormatTimestamp(micros int64) string {
	t := time.UnixMicro(micros).UTC()
	out := t.Format("2006-01-02 15:04:05")
	if frac := ((micros % 1e6) + 1e6) % 1e6; frac != 0 {
		out += strings.TrimRight(fmt.Sprintf(".%06d", frac), "0")
	}
	return out + "+00"
}

func refFormatDate(days int64) string {
	return time.Unix(days*86400, 0).UTC().Format("2006-01-02")
}

func refFormatUUID(b []byte) string {
	if len(b) != 16 {
		return fmt.Sprintf("\\x%x", b)
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func TestAppendFormattersMatchFormat(t *testing.T) {
	rng := rand.New(rand.NewPCG(41, 41))
	floats := []float64{0, math.Copysign(0, -1), 1, -1, 0.5, 1e-4, 1e-5, 9.99e-5, 1e14, 1e15, 123456789012345.6,
		1e300, -1e-300, math.MaxFloat64, math.SmallestNonzeroFloat64, math.NaN(), math.Inf(1), math.Inf(-1), 1.25e8}
	for range 5000 {
		floats = append(floats, math.Float64frombits(rng.Uint64()), rng.NormFloat64()*math.Pow(10, float64(rng.IntN(40)-20)))
	}
	for _, f := range floats {
		if got, want := string(AppendFloat([]byte("x"), f)), "x"+refFormatFloat(f); got != want {
			t.Fatalf("AppendFloat(%v) = %q, want %q", f, got, want)
		}
	}
	micros := []int64{0, 1, -1, 999999, -999999, 1e6, 500000, 123456, 1704067200000000, -62135596800000000, 253402300799999999}
	for range 5000 {
		micros = append(micros, rng.Int64N(1<<55)-(1<<54))
	}
	for _, m := range micros {
		if got, want := string(AppendTimestamp([]byte("x"), m)), "x"+refFormatTimestamp(m); got != want {
			t.Fatalf("AppendTimestamp(%d) = %q, want %q", m, got, want)
		}
		days := m / 86400e6
		if got, want := string(AppendDate(nil, days)), refFormatDate(days); got != want {
			t.Fatalf("AppendDate(%d) = %q, want %q", days, got, want)
		}
	}
	for _, n := range []int{0, 3, 16, 17} {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.IntN(256))
		}
		if got, want := string(AppendUUID([]byte("x"), b)), "x"+refFormatUUID(b); got != want {
			t.Fatalf("AppendUUID(%x) = %q, want %q", b, got, want)
		}
	}
}
