package tuple

import (
	"bytes"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb/decimal"
)

func TestRoundTrip(t *testing.T) {
	cases := [][]any{
		{nil},
		{true}, {false},
		{int64(0)}, {int64(1)}, {int64(-1)}, {int64(math.MaxInt64)}, {int64(math.MinInt64)},
		{0.0}, {1.5}, {-1.5}, {math.Inf(1)}, {math.Inf(-1)}, {math.SmallestNonzeroFloat64},
		{""}, {"a"}, {"a\x00b"}, {"a\x00"}, {strings.Repeat("\x00", 3)},
		{[]byte{}}, {[]byte{0x00, 0xFF, 0x00}},
		{nil, false, int64(-7), 2.5, "k\x00ey", []byte{0}, true},
	}
	for _, in := range cases {
		enc, err := Encode(in...)
		if err != nil {
			t.Fatalf("Encode(%v): %v", in, err)
		}
		out, err := Decode(enc)
		if err != nil {
			t.Fatalf("Decode(Encode(%v)): %v", in, err)
		}
		want := normalizeAll(t, in)
		if !reflect.DeepEqual(out, want) {
			t.Fatalf("round trip %#v -> %#v", want, out)
		}
	}

	// Empty []byte decodes as non-nil empty; normalize widths.
	enc, _ := Encode(int32(5), uint8(7), float32(1.5))
	out, _ := Decode(enc)
	if !reflect.DeepEqual(out, []any{int64(5), int64(7), 1.5}) {
		t.Fatalf("width normalization: %#v", out)
	}
}

func normalizeAll(t *testing.T, in []any) []any {
	t.Helper()
	out := make([]any, len(in))
	for i, v := range in {
		n, err := normalize(v)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = n
	}
	return out
}

// TestNaNOrdersLast (deep-look D-36): NaN encodes (it was refused), every
// NaN bit pattern encodes alike, and NaN sorts above +Inf in both byte
// order and Compare, as Postgres orders it; descending reverses it.
func TestNaNOrdersLast(t *testing.T) {
	nan, err := Encode(math.NaN())
	if err != nil {
		t.Fatal(err)
	}
	negNaN, _ := Encode(math.Float64frombits(0xFFF8000000000001))
	if !bytes.Equal(nan, negNaN) {
		t.Fatal("two NaN bit patterns encode differently")
	}
	inf, _ := Encode(math.Inf(1))
	if bytes.Compare(inf, nan) >= 0 || Compare([]any{math.Inf(1)}, []any{math.NaN()}) >= 0 {
		t.Fatal("NaN does not sort above +Inf")
	}
	if Compare([]any{math.NaN()}, []any{math.NaN()}) != 0 {
		t.Fatal("NaN does not compare equal to itself")
	}
	dnan, _ := AppendDesc(nil, math.NaN())
	dinf, _ := AppendDesc(nil, math.Inf(1))
	if bytes.Compare(dnan, dinf) >= 0 {
		t.Fatal("descending NaN does not sort before +Inf")
	}
	vals, err := Decode(nan)
	if err != nil || len(vals) != 1 || !math.IsNaN(vals[0].(float64)) {
		t.Fatalf("NaN round trip: %v %v", vals, err)
	}
}

// TestAppendValueKeepsNegativeZero (D-36): stored values keep -0's sign;
// keys still collapse it.
func TestAppendValueKeepsNegativeZero(t *testing.T) {
	enc, err := AppendValue(nil, math.Copysign(0, -1))
	if err != nil {
		t.Fatal(err)
	}
	vals, err := Decode(enc)
	if err != nil || !math.Signbit(vals[0].(float64)) {
		t.Fatalf("AppendValue lost -0's sign: %v %v", vals, err)
	}
	key, _ := Append(nil, math.Copysign(0, -1))
	if vals, _ := Decode(key); math.Signbit(vals[0].(float64)) {
		t.Fatal("a key kept -0's sign")
	}
}

func TestRejects(t *testing.T) {
	if _, err := Encode(uint64(math.MaxUint64)); err == nil {
		t.Fatal("overflowing uint64 encoded")
	}
	if _, err := Encode(struct{}{}); err == nil {
		t.Fatal("struct encoded")
	}
}

func TestNegativeZero(t *testing.T) {
	a, _ := Encode(math.Copysign(0, -1))
	b, _ := Encode(0.0)
	if !bytes.Equal(a, b) {
		t.Fatal("-0.0 and +0.0 encode differently")
	}
}

// TestKnownOrder pins tricky orderings that random generation might
// miss: string prefixes, embedded zero bytes, tuple prefixes, and the
// int/float boundaries.
func TestKnownOrder(t *testing.T) {
	ordered := [][]any{
		{nil},
		{false},
		{true},
		{int64(math.MinInt64)},
		{int64(-1)},
		{int64(0)},
		{int64(1)},
		{int64(math.MaxInt64)},
		{math.Inf(-1)},
		{-2.0},
		{-1.5},
		{0.0},
		{1.5},
		{math.Inf(1)},
		{[]byte{}},
		{[]byte{0x00}},
		{[]byte{0x01}},
		{""},
		{"a"},
		{"a", int64(1)}, // tuple prefix sorts first
		{"a", int64(2)},
		{"a\x00"},
		{"a\x00b"},
		{"a\x01"},
		{"ab"},
		{"b"},
	}
	for i := range len(ordered) - 1 {
		a, err := Encode(ordered[i]...)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Encode(ordered[i+1]...)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Compare(a, b) >= 0 {
			t.Fatalf("encoding order violated: %v !< %v", ordered[i], ordered[i+1])
		}
		if Compare(ordered[i], ordered[i+1]) >= 0 {
			t.Fatalf("semantic order violated: %v !< %v", ordered[i], ordered[i+1])
		}
	}
}

// TestOrderProperty is the core invariant: for random tuples,
// bytes.Compare on encodings must equal Compare on the tuples.
func TestOrderProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	tuples := make([][]any, 400)
	encs := make([][]byte, len(tuples))
	for i := range tuples {
		tuples[i] = randTuple(rng)
		enc, err := Encode(tuples[i]...)
		if err != nil {
			t.Fatalf("Encode(%v): %v", tuples[i], err)
		}
		encs[i] = enc

		out, err := Decode(enc)
		if err != nil {
			t.Fatalf("Decode(%v): %v", tuples[i], err)
		}
		if Compare(out, tuples[i]) != 0 {
			t.Fatalf("round trip changed order identity: %v vs %v", tuples[i], out)
		}
	}
	for i := range tuples {
		for j := range tuples {
			want := Compare(tuples[i], tuples[j])
			got := bytes.Compare(encs[i], encs[j])
			if sign(got) != sign(want) {
				t.Fatalf("order mismatch: Compare(%v, %v)=%d but bytes=%d",
					tuples[i], tuples[j], want, got)
			}
		}
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

func randTuple(rng *rand.Rand) []any {
	vals := make([]any, 1+rng.Intn(4))
	for i := range vals {
		vals[i] = randValue(rng)
	}
	return vals
}

func randValue(rng *rand.Rand) any {
	switch rng.Intn(6) {
	case 0:
		return nil
	case 1:
		return rng.Intn(2) == 0
	case 2:
		switch rng.Intn(4) {
		case 0:
			return int64(math.MinInt64)
		case 1:
			return int64(math.MaxInt64)
		default:
			return rng.Int63() - rng.Int63()
		}
	case 3:
		switch rng.Intn(5) {
		case 0:
			return math.Inf(1)
		case 1:
			return math.Inf(-1)
		case 2:
			return 0.0
		default:
			return (rng.Float64() - 0.5) * math.Pow(10, float64(rng.Intn(40)-20))
		}
	case 4:
		return string(randBytes(rng))
	default:
		return randBytes(rng)
	}
}

// randBytes skews toward 0x00, 0x01, and 0xFF — the bytes the escape
// scheme and PrefixEnd care about.
func randBytes(rng *rand.Rand) []byte {
	b := make([]byte, rng.Intn(6))
	for i := range b {
		switch rng.Intn(4) {
		case 0:
			b[i] = 0x00
		case 1:
			b[i] = 0x01
		case 2:
			b[i] = 0xFF
		default:
			b[i] = byte(rng.Intn(256))
		}
	}
	return b
}

// TestDescOrderProperty: descending encodings reverse byte order and
// round-trip through DecodeOne.
func TestDescOrderProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	vals := make([]any, 300)
	encs := make([][]byte, len(vals))
	for i := range vals {
		vals[i] = randValue(rng)
		enc, err := AppendDesc(nil, vals[i])
		if err != nil {
			t.Fatalf("AppendDesc(%v): %v", vals[i], err)
		}
		encs[i] = enc

		out, rest, err := DecodeOne(enc, true)
		if err != nil {
			t.Fatalf("DecodeOne(desc %v): %v", vals[i], err)
		}
		if len(rest) != 0 {
			t.Fatalf("DecodeOne(desc %v) left %d bytes", vals[i], len(rest))
		}
		if Compare([]any{out}, []any{vals[i]}) != 0 {
			t.Fatalf("desc round trip changed value: %v vs %v", vals[i], out)
		}
	}
	for i := range vals {
		for j := range vals {
			want := Compare([]any{vals[i]}, []any{vals[j]})
			got := bytes.Compare(encs[i], encs[j])
			if sign(got) != -sign(want) {
				t.Fatalf("desc order not reversed: Compare(%v, %v)=%d but bytes=%d",
					vals[i], vals[j], want, got)
			}
		}
	}
}

// TestMixedDirections: ascending and descending elements interleave in
// one key; each element decodes with its own direction, and the key
// orders by the leading element's direction.
func TestMixedDirections(t *testing.T) {
	key := func(a string, b int64) []byte {
		buf, err := Append(nil, a)
		if err != nil {
			t.Fatal(err)
		}
		if buf, err = AppendDesc(buf, b); err != nil {
			t.Fatal(err)
		}
		return buf
	}
	k1 := key("acme", 10)
	k2 := key("acme", 2)
	k3 := key("beta", 1)
	if !(bytes.Compare(k1, k2) < 0 && bytes.Compare(k2, k3) < 0) {
		t.Fatalf("mixed-direction order wrong: % x, % x, % x", k1, k2, k3)
	}
	a, rest, err := DecodeOne(k1, false)
	if err != nil {
		t.Fatal(err)
	}
	b, rest, err := DecodeOne(rest, true)
	if err != nil {
		t.Fatal(err)
	}
	if a != "acme" || b != int64(10) || len(rest) != 0 {
		t.Fatalf("mixed decode: %v %v rest=%d", a, b, len(rest))
	}
}

func TestDescNullsLast(t *testing.T) {
	null, _ := AppendDesc(nil, nil)
	max, _ := AppendDesc(nil, "zzz")
	if bytes.Compare(null, max) <= 0 {
		t.Fatal("descending NULL does not sort after values")
	}
}

func TestDecodeOneEmpty(t *testing.T) {
	if _, _, err := DecodeOne(nil, false); err == nil {
		t.Fatal("DecodeOne on empty input succeeded")
	}
}

func TestDecodeCorrupt(t *testing.T) {
	for _, data := range [][]byte{
		{tagInt, 0x01},               // truncated int
		{tagFloat},                   // truncated float
		{tagString, 'a'},             // unterminated string
		{tagString, 'a', 0x00},       // dangling escape
		{tagString, 'a', 0x00, 0x77}, // invalid escape
		{0x7F},                       // unknown tag
	} {
		if _, err := Decode(data); err == nil {
			t.Fatalf("Decode(% x) succeeded on corrupt input", data)
		}
	}
}

func TestPrefixEnd(t *testing.T) {
	cases := []struct{ in, want []byte }{
		{[]byte{0x01}, []byte{0x02}},
		{[]byte{0x01, 0xFF}, []byte{0x02}},
		{[]byte{0xFF, 0xFF}, nil},
		{[]byte{0x0A, 0x00}, []byte{0x0A, 0x01}},
	}
	for _, c := range cases {
		if got := PrefixEnd(c.in); !bytes.Equal(got, c.want) {
			t.Fatalf("PrefixEnd(% x) = % x; want % x", c.in, got, c.want)
		}
	}
	// Every string with the prefix is < PrefixEnd(prefix).
	p := []byte{0x03, 0xFF}
	end := PrefixEnd(p)
	for _, ext := range [][]byte{{}, {0x00}, {0xFF, 0xFF}} {
		k := append(bytes.Clone(p), ext...)
		if bytes.Compare(k, end) >= 0 {
			t.Fatalf("key % x not below prefix end % x", k, end)
		}
	}
}

// Skip passes over one element of every kind without decoding it, and
// strings decode the same with and without escapes and in both
// directions (deep-look D-39's one-allocation string path).
func TestSkipAndStrings(t *testing.T) {
	vals := []any{nil, true, false, int64(-9), 3.5, "", "plain", "esc\x00aped\x00", []byte{0, 1, 0}}
	enc, err := Encode(vals...)
	if err != nil {
		t.Fatal(err)
	}
	rest := enc
	for i := range vals {
		if rest, err = Skip(rest); err != nil {
			t.Fatalf("skip %d: %v", i, err)
		}
	}
	if len(rest) != 0 {
		t.Fatalf("%d bytes left after skipping every element", len(rest))
	}
	if _, err := Skip(nil); err == nil {
		t.Fatal("Skip of empty input succeeded")
	}
	for _, s := range []string{"", "plain", "esc\x00aped\x00", "\x00\x00"} {
		for _, desc := range []bool{false, true} {
			var b []byte
			if desc {
				b, err = AppendDesc(nil, s)
			} else {
				b, err = Append(nil, s)
			}
			if err != nil {
				t.Fatal(err)
			}
			v, rest, err := DecodeOne(b, desc)
			if err != nil || v != s || len(rest) != 0 {
				t.Fatalf("DecodeOne(%q, desc=%v) = %q, %d left, %v", s, desc, v, len(rest), err)
			}
		}
	}
	plain, _ := Encode("a plain string value")
	if n := testing.AllocsPerRun(100, func() { DecodeOne(plain, false) }); n > 2 { //nolint:errcheck
		t.Fatalf("string decode allocates %v times, want <= 2 (string + interface box)", n)
	}
}

// TestDecimalForms pins the two decimal forms: keys collapse the scale
// (1.0 and 1.00 are one key, as a unique index needs), stored values
// keep it, and both order numerically in either direction.
func TestDecimalForms(t *testing.T) {
	m := decimal.MustParse
	k1, _ := Encode(m("1.0"))
	k2, _ := Encode(m("1.00"))
	k3, _ := Encode(m("1"))
	if !bytes.Equal(k1, k2) || !bytes.Equal(k1, k3) {
		t.Fatalf("equal decimals must share a key: % x / % x / % x", k1, k2, k3)
	}
	for _, in := range []string{"1.50", "0.00", "-0.050", "123456789012345678901234567890.123400", "1e3", "-7"} {
		enc, err := AppendValue(nil, int64(3), m(in))
		if err != nil {
			t.Fatal(err)
		}
		vals, err := Decode(enc)
		if err != nil {
			t.Fatal(err)
		}
		if got := vals[1].(decimal.Decimal).String(); got != m(in).String() {
			t.Errorf("stored %s read back as %s", m(in), got)
		}
		if rest, err := Skip(enc[9:]); err != nil || len(rest) != 0 {
			t.Errorf("Skip over %s: rest %d, err %v", in, len(rest), err)
		}
		// Key decode is the normalized value.
		key, _ := Encode(m(in))
		kv, _ := Decode(key)
		if got, want := kv[0].(decimal.Decimal).String(), m(in).Normalize().String(); got != want {
			t.Errorf("key of %s decodes as %s, want %s", in, got, want)
		}
	}
	sorted := []string{"-1e40", "-100", "-99.99", "-1.1", "-1", "-0.5", "-0.05", "-0.0005", "0",
		"0.0005", "0.05", "0.5", "0.51", "1", "1.000001", "1.1", "9", "10", "99.99", "100", "1e40"}
	for i := 1; i < len(sorted); i++ {
		a, _ := Encode(m(sorted[i-1]))
		b, _ := Encode(m(sorted[i]))
		if bytes.Compare(a, b) >= 0 {
			t.Errorf("key(%s) should sort below key(%s)", sorted[i-1], sorted[i])
		}
		da, _ := AppendDesc(nil, m(sorted[i-1]))
		db, _ := AppendDesc(nil, m(sorted[i]))
		if bytes.Compare(da, db) <= 0 {
			t.Errorf("desc key(%s) should sort above desc key(%s)", sorted[i-1], sorted[i])
		}
		if v, _, err := DecodeOne(db, true); err != nil || v.(decimal.Decimal).Cmp(m(sorted[i])) != 0 {
			t.Errorf("desc round trip of %s: %v, %v", sorted[i], v, err)
		}
	}
}
