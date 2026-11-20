package pgwire

import (
	"encoding/hex"
	"testing"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/decimal"
)

// TestNumericBinaryMatchesPostgres pins appendNumericBinary to Postgres
// 16's own numeric_send output (encode(numeric_send(v), 'hex')) and the
// decoder to reading it back with the scale intact.
func TestNumericBinaryMatchesPostgres(t *testing.T) {
	for in, want := range map[string]string{
		"1234567.89":                      "0003000100000002007b11d722c4",
		"0.0001":                          "0001ffff000000040001",
		"-0.5":                            "0001ffff400000011388",
		"10000":                           "00010001000000000001",
		"0":                               "0000000000000000",
		"0.00":                            "0000000000000002",
		"100000000000000000000":           "00010005000000000001",
		"1.50":                            "000200000000000200011388",
		"-12345678901234567890.123456789": "000800044000000904d2162e23340d801ed204d2162e2328",
		"0.000012340":                     "0001fffe0000000904d2",
		"9999.9999":                       "0002000000000004270f270f",
		"0.0000000001":                    "0001fffd0000000a0064",
	} {
		d, err := bytdb.ParseDecimal(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(appendNumericBinary(nil, d)); got != want {
			t.Errorf("encode %s = %s, want %s", in, got, want)
		}
		raw, _ := hex.DecodeString(want)
		back, err := decodeNumericBinary(raw)
		if err != nil || back.String() != d.String() {
			t.Errorf("decode %s = %v, %v", want, back, err)
		}
	}
}

// TestNumericBinaryRefusals: forged headers fail the parameter instead
// of allocating or panicking.
func TestNumericBinaryRefusals(t *testing.T) {
	for name, h := range map[string]string{
		"short":           "000100",
		"count mismatch":  "0002000000000000270f",
		"digit too big":   "0001000000000000ffff",
		"bad sign":        "0000000012340000",
		"scale too big":   "0000000000007fff",
		"negative ndigit": "ffff000000000000",
		// N-051: a special value's digits are ignored but still
		// checked, as numeric_recv checks them.
		"NaN, count mismatch": "00010000c0000000",
		"-Inf, digit too big": "00010000f0000000ffff",
		"+Inf, short":         "00000000d000",
	} {
		raw, _ := hex.DecodeString(h)
		if _, err := decodeNumericBinary(raw); err == nil {
			t.Errorf("%s: accepted % x", name, raw)
		}
	}
}

// TestNumericBinarySpecials (N-051): NaN and ±Infinity travel as a bare
// header with numeric_send's sign words, and read back from it, with or
// without (ignored) digits.
func TestNumericBinarySpecials(t *testing.T) {
	for _, c := range []struct {
		d    bytdb.Decimal
		wire string
	}{
		// Postgres 16's numeric_send bytes, dscale 32 on the infinities
		// included (encode(numeric_send('Infinity'::numeric), 'hex')).
		{decimal.NaN(), "00000000c0000000"},
		{decimal.Inf(1), "00000000d0000020"},
		{decimal.Inf(-1), "00000000f0000020"},
	} {
		if got := hex.EncodeToString(appendNumericBinary(nil, c.d)); got != c.wire {
			t.Errorf("%s encodes as %s, want %s", c.d, got, c.wire)
		}
		raw, _ := hex.DecodeString(c.wire)
		if d, err := decodeNumericBinary(raw); err != nil || d != c.d {
			t.Errorf("%s decodes as %s, %v", c.wire, d, err)
		}
		// Digits, weight and dscale are ignored for a special value.
		raw, _ = hex.DecodeString("0001" + "0003" + c.wire[8:12] + "0005" + "0007")
		if d, err := decodeNumericBinary(raw); err != nil || d != c.d {
			t.Errorf("%s with digits decodes as %s, %v", c.d, d, err)
		}
	}
}
