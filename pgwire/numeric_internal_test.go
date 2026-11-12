package pgwire

import (
	"encoding/hex"
	"testing"

	"github.com/rohanthewiz/bytdb"
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
// of allocating or panicking, and NaN/Infinity get the engine's refusal.
func TestNumericBinaryRefusals(t *testing.T) {
	for name, h := range map[string]string{
		"short":           "000100",
		"count mismatch":  "0002000000000000270f",
		"digit too big":   "0001000000000000ffff",
		"bad sign":        "000000001234000000",
		"scale too big":   "0000000000007fff",
		"NaN":             "000000000c00000000"[:16],
		"negative ndigit": "ffff000000000000",
	} {
		raw, _ := hex.DecodeString(h)
		if name == "NaN" {
			raw = []byte{0, 0, 0, 0, 0xC0, 0, 0, 0}
		}
		if _, err := decodeNumericBinary(raw); err == nil {
			t.Errorf("%s: accepted % x", name, raw)
		}
	}
}
