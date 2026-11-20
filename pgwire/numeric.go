package pgwire

// numeric.go: Postgres's numeric binary wire format (N-048).
//
// A numeric travels as base-10000 digits ("NBASE" digits) around the
// decimal point:
//
//	int16  ndigits   number of base-10000 digits that follow
//	int16  weight    power of 10000 of the first digit (0 = units group)
//	uint16 sign      0x0000 positive, 0x4000 negative, 0xC000 NaN,
//	                 0xD000 +Infinity, 0xF000 -Infinity
//	uint16 dscale    display scale: decimal digits after the point
//	int16  digits[ndigits], each 0..9999
//
// For example 1234567.89 is the groups 123|4567|.8900: ndigits 3,
// weight 1, dscale 2, digits {123, 4567, 8900}. Leading and trailing
// zero groups are not sent (0.0001 is ndigits 1, weight -1, digit 1),
// and zero is ndigits 0 with its dscale. dscale is what keeps 1.50
// from reading back as 1.5: the digits alone do not carry it.
//
// NaN and ±Infinity (N-051) are a bare header: ndigits 0, weight 0 and
// the special sign word, which is what numeric_send writes for them.
// Its dscale is 0 for NaN but 32 for both infinities: numeric_send reads
// a special value's header as if it were a short numeric's, and the
// infinities' sign bits (0xD000, 0xF000) overlap the short header's
// dscale field (0x1F80, shifted right by 7) as 0x1000 → 32. Clients
// ignore dscale on a special value; it is matched so the bytes equal
// Postgres's, as TestNumericBinaryMatchesPostgres pins for numbers. On receipt numeric_recv reads (and range-checks) any digits
// sent along, then ignores everything but the sign; so does this.

import (
	"encoding/binary"
	"math/big"
	"strings"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/decimal"
	"github.com/rohanthewiz/serr"
)

const (
	oidNumeric = 1700

	numericPos  = 0x0000
	numericNeg  = 0x4000
	numericNaN  = 0xC000
	numericPInf = 0xD000
	numericNInf = 0xF000

	nbaseDigits = 4 // decimal digits per base-10000 digit

	// infDscale is the dscale numeric_send writes for ±Infinity (see
	// the file comment): (0xD000 & 0x1F80) >> 7, the same for 0xF000.
	infDscale = 32
)

// appendNumericBinary appends d in numeric's binary format.
func appendNumericBinary(dst []byte, d bytdb.Decimal) []byte {
	switch {
	case d.IsNaN():
		return appendNumericHeader(dst, 0, 0, numericNaN, 0)
	case d.IsInf(1):
		return appendNumericHeader(dst, 0, 0, numericPInf, infDscale)
	case d.IsInf(-1):
		return appendNumericHeader(dst, 0, 0, numericNInf, infDscale)
	}
	sign := uint16(numericPos)
	if d.Sign() < 0 {
		sign = numericNeg
	}
	scale := int(d.Scale())
	c := d.Coef()
	digits := c.Abs(c).Text(10)
	if d.IsZero() {
		return appendNumericHeader(dst, 0, 0, numericPos, uint16(scale))
	}
	// Split at the decimal point, then pad each side to whole groups:
	// the integer part on the left, the fraction on the right.
	intPart, frac := "", digits
	if len(digits) > scale {
		intPart, frac = digits[:len(digits)-scale], digits[len(digits)-scale:]
	} else {
		frac = strings.Repeat("0", scale-len(digits)) + digits
	}
	if r := len(intPart) % nbaseDigits; r != 0 {
		intPart = strings.Repeat("0", nbaseDigits-r) + intPart
	}
	if r := len(frac) % nbaseDigits; r != 0 {
		frac += strings.Repeat("0", nbaseDigits-r)
	}
	all := intPart + frac
	groups := make([]uint16, 0, len(all)/nbaseDigits)
	for i := 0; i < len(all); i += nbaseDigits {
		g := uint16(0)
		for _, c := range all[i : i+nbaseDigits] {
			g = g*10 + uint16(c-'0')
		}
		groups = append(groups, g)
	}
	weight := len(intPart)/nbaseDigits - 1
	// Drop leading zero groups (each moves the weight down one) and
	// trailing zero groups (dscale still says how many decimals show).
	for len(groups) > 0 && groups[0] == 0 {
		groups = groups[1:]
		weight--
	}
	for len(groups) > 0 && groups[len(groups)-1] == 0 {
		groups = groups[:len(groups)-1]
	}
	dst = appendNumericHeader(dst, len(groups), weight, sign, uint16(scale))
	for _, g := range groups {
		dst = binary.BigEndian.AppendUint16(dst, g)
	}
	return dst
}

func appendNumericHeader(dst []byte, ndigits, weight int, sign, dscale uint16) []byte {
	dst = binary.BigEndian.AppendUint16(dst, uint16(int16(ndigits)))
	dst = binary.BigEndian.AppendUint16(dst, uint16(int16(weight)))
	dst = binary.BigEndian.AppendUint16(dst, sign)
	return binary.BigEndian.AppendUint16(dst, dscale)
}

// decodeNumericBinary reads a numeric parameter in binary format. Every
// field is checked against the bytes present and the type's limits: a
// Bind body is client-controlled, and a forged weight or ndigits must
// fail the parameter, not allocate without bound.
func decodeNumericBinary(raw []byte) (bytdb.Decimal, error) {
	bad := func(why string) (bytdb.Decimal, error) {
		return bytdb.Decimal{}, serr.New("bad binary numeric parameter", "reason", why)
	}
	if len(raw) < 8 {
		return bad("short header")
	}
	ndigits := int(int16(binary.BigEndian.Uint16(raw[0:2])))
	weight := int(int16(binary.BigEndian.Uint16(raw[2:4])))
	sign := binary.BigEndian.Uint16(raw[4:6])
	dscale := int(binary.BigEndian.Uint16(raw[6:8]))
	switch sign {
	case numericPos, numericNeg, numericNaN, numericPInf, numericNInf:
	default:
		return bad("unknown sign")
	}
	if ndigits < 0 || len(raw) != 8+2*ndigits {
		return bad("digit count does not match the body")
	}
	switch sign {
	case numericNaN, numericPInf, numericNInf:
		// The sign word alone is the value. numeric_recv still rejects
		// a digit ≥ NBASE in the ignored digits, so check them too:
		// the same bytes fail here exactly when they fail there.
		for i := range ndigits {
			if binary.BigEndian.Uint16(raw[8+2*i:]) > 9999 {
				return bad("digit out of range")
			}
		}
		switch sign {
		case numericNaN:
			return decimal.NaN(), nil
		case numericPInf:
			return decimal.Inf(1), nil
		}
		return decimal.Inf(-1), nil
	}
	if dscale > decimal.MaxScale {
		return bad("display scale out of range")
	}
	// The value is Σ digit_i × 10000^(weight - i). As an integer
	// coefficient over 10^(4·(ndigits - weight - 1)) — the number of
	// decimal places the digits span — then rescaled to dscale.
	coef := new(big.Int)
	base := big.NewInt(10000)
	for i := range ndigits {
		g := binary.BigEndian.Uint16(raw[8+2*i:])
		if g > 9999 {
			return bad("digit out of range")
		}
		coef.Mul(coef, base).Add(coef, big.NewInt(int64(g)))
	}
	if sign == numericNeg {
		coef.Neg(coef)
	}
	places := nbaseDigits * (ndigits - weight - 1) // decimal places the digits reach
	if places < -decimal.MaxIntDigits || places > decimal.MaxScale+nbaseDigits {
		return bad("weight out of range")
	}
	var d bytdb.Decimal
	if places <= 0 {
		// The digits end at or above the units: append the zeros.
		coef.Mul(coef, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-places)), nil))
		d = decimal.New(coef, 0)
	} else {
		d = decimal.New(coef, int32(places))
	}
	// dscale is authoritative for how the value displays. Trailing
	// groups the sender trimmed are zeros, so widening is exact. A
	// dscale narrower than the digits reach hides digits; Postgres's
	// numeric_recv truncates them away (trunc_var) rather than erroring,
	// and so does this.
	return d.Trunc(int32(dscale)), nil
}
