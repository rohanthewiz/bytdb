package interval

import (
	"math"
	"math/rand/v2"
	"testing"
)

// TestSpanRoundTrip: FromSpan rebuilds an interval with exactly the
// span it came from, for random fields and every extreme (the tuple key
// form depends on it: keys are decoded and re-encoded), and Cmp agrees
// with the span.
func TestSpanRoundTrip(t *testing.T) {
	edge32 := []int32{math.MinInt32, math.MinInt32 + 1, -30, -1, 0, 1, 29, 30, math.MaxInt32 - 1, math.MaxInt32}
	edge64 := []int64{math.MinInt64, math.MinInt64 + 1, -MicrosPerDay, -1, 0, 1, MicrosPerDay, math.MaxInt64 - 1, math.MaxInt64}
	var cases []Interval
	for _, m := range edge32 {
		for _, d := range edge32 {
			for _, us := range edge64 {
				cases = append(cases, Interval{m, d, us})
			}
		}
	}
	r := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		cases = append(cases, Interval{int32(r.Uint32()), int32(r.Uint32()), int64(r.Uint64())})
		cases = append(cases, Interval{int32(r.IntN(25) - 12), int32(r.IntN(61) - 30), r.Int64N(3*MicrosPerDay) - MicrosPerDay})
	}
	for _, iv := range cases {
		hi, lo := iv.Span()
		if !ValidSpan(hi, lo) {
			t.Fatalf("%#v: span reported invalid", iv)
		}
		back := FromSpan(hi, lo)
		if h2, l2 := back.Span(); h2 != hi || l2 != lo {
			t.Fatalf("%#v: FromSpan gave %#v, a different span", iv, back)
		}
		if back.Cmp(iv) != 0 {
			t.Fatalf("%#v vs %#v: Cmp disagrees with equal spans", iv, back)
		}
	}
	for i := 1; i < len(cases); i++ {
		a, b := cases[i-1], cases[i]
		ah, al := a.Span()
		bh, bl := b.Span()
		want := 0
		switch {
		case ah < bh || (ah == bh && al < bl):
			want = -1
		case ah > bh || (ah == bh && al > bl):
			want = 1
		}
		if got := a.Cmp(b); got != want {
			t.Fatalf("Cmp(%#v, %#v) = %d, want %d", a, b, got, want)
		}
	}
}
