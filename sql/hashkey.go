package sql

// hashkey.go: map keys for values compared under SQL equality — GROUP
// BY, DISTINCT, UNION, count(DISTINCT x), window partitions (deep-look
// D-26).
//
// Those keys used to be the raw order-preserving tuple encoding of the
// values, which distinguishes an int64 from a float64. SQL does not:
// 1 = 1.0, so SELECT 1 UNION SELECT 1.0 is one row and a GROUP BY over
// a column fed both forms is one group. And the tuple encoding refuses
// NaN outright ("tuple: NaN is not encodable"), so grouping a float
// column holding a NaN failed the statement, where Postgres groups all
// NaNs together and sorts them last.
//
// hashKey canonicalizes before encoding:
//
//	float64 with an integral value in int64 range → that int64
//	NaN                                            → nanKey (one group, sorts after every number)
//	Decimal with an integral value in int64 range  → that int64 (N-048)
//	any other Decimal                              → unchanged (its key encoding ignores scale)
//	anything else                                  → unchanged
//
// A Decimal needs no normalization of its own for 1.0 = 1.00: the tuple
// key form already drops the display scale. Folding integral values to
// int64 is what lets an integer and a numeric holding the same number
// meet in one group, as they do under SQL equality.
//
// The tuple encoding is kept (rather than a plain hash) because group
// keys' byte order is also used as the groups' order.

import (
	"math"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/tuple"
)

// nanKey stands for NaN in a key. As a byte string it encodes with the
// bytes tag, which sorts after both number tags; a column whose values
// are numbers never holds bytes, so it cannot collide.
var nanKey = []byte("\xff\xffNaN")

// hashNorm is one value's canonical key form.
func hashNorm(v any) any {
	if d, ok := v.(bytdb.Decimal); ok {
		if d.IsInteger() {
			if n, ok := d.Int64(); ok {
				return n
			}
		}
		return d
	}
	f, ok := v.(float64)
	if !ok {
		return v
	}
	switch {
	case math.IsNaN(f):
		return nanKey
	case f == math.Trunc(f) && f >= -(1<<63) && f < 1<<63:
		return int64(f)
	}
	return f
}

// hashKey encodes vals as one map key under SQL equality.
func hashKey(vals ...any) ([]byte, error) {
	norm := vals
	for i, v := range vals {
		_, isFloat := v.(float64)
		_, isDec := v.(bytdb.Decimal)
		if isFloat || isDec {
			if &norm[0] == &vals[0] {
				norm = append([]any(nil), vals...) // copy before the first change
			}
			norm[i] = hashNorm(v)
		}
	}
	return tuple.Encode(norm...)
}
