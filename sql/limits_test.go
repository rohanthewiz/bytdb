package sql

// limits_test.go: numeric edges in result shaping — the OFFSET+LIMIT
// collection cutoff must saturate rather than wrap (a wrapped negative
// sum silently truncated results after the first row), and float
// ordering must treat NaN as Postgres does (largest, equal to itself)
// rather than Go's default (smallest).

import (
	"math"
	"reflect"
	"testing"
)

func TestLimitOffsetOverflow(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	// Individually valid but jointly overflowing: OFFSET+LIMIT wraps
	// negative in naive int64 arithmetic. Postgres returns all rows
	// past the offset.
	res := exec(t, d, `select id from users order by id
		limit 9223372036854775807 offset 9223372036854775807`)
	if len(res.Rows) != 0 {
		t.Fatalf("offset past the end: got %d rows", len(res.Rows))
	}
	res = exec(t, d, `select id from users order by id limit 9223372036854775807 offset 1`)
	want := [][]any{{int64(2)}, {int64(3)}, {int64(4)}, {int64(5)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("huge limit, offset 1: got %v, want %v", res.Rows, want)
	}
	// The unsorted path takes the early-cutoff branch under test.
	res = exec(t, d, `select id from users limit 9223372036854775807 offset 2`)
	if len(res.Rows) != 3 {
		t.Fatalf("unsorted huge limit, offset 2: got %d rows, want 3", len(res.Rows))
	}
}

func TestCmpFloatNaN(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	cases := []struct {
		a, b float64
		want int
	}{
		{nan, nan, 0},   // NaN groups with itself
		{nan, inf, 1},   // NaN above even +Inf
		{inf, nan, -1},
		{nan, 1.5, 1},
		{1.5, nan, -1},
		{1.5, 2.5, -1}, // non-NaN comparisons unchanged
	}
	for _, c := range cases {
		if got := cmpFloat(c.a, c.b); got != c.want {
			t.Errorf("cmpFloat(%v, %v) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	// The same convention must flow through the generic comparators
	// used by ORDER BY and MIN/MAX.
	if got, ok := compareVals(nan, int64(5)); !ok || got != 1 {
		t.Errorf("compareVals(NaN, 5) = %d, %v; want 1, true", got, ok)
	}
	if got := orderCmp(nan, 5.0); got != 1 {
		t.Errorf("orderCmp(NaN, 5.0) = %d, want 1 (NaN sorts last ascending)", got)
	}
}
