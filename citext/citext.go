// Package citext is bytdb's citext value: text that compares without
// regard to case, as Postgres's citext extension type does.
//
// A Text holds the string exactly as written and reads back that way;
// only comparison ignores case. Two values are equal when their lowercase
// forms are equal, and they order by their lowercase forms, so 'ABC' =
// 'abc' and 'a' < 'B' < 'c'. That is citext's citext_cmp, which lowers
// both sides with the database collation and compares them; bytdb's
// collation is C (byte order), so lowercase-then-bytes is the whole rule.
//
// Why a distinct Go type rather than a plain string with a column flag:
// equality has to hold everywhere a value is compared, hashed or keyed —
// WHERE, ORDER BY, GROUP BY, DISTINCT, hash joins, unique and primary-key
// indexes, foreign-key checks, upserts — and those paths dispatch on the
// runtime value. A value that carries its own comparison is right in all
// of them by construction. The tuple encoding gives it two forms, as it
// does numerics and intervals: a key holds Fold (so 'ABC' and 'abc' are
// one key), a stored value the text as written.
//
// Lowercasing is Go's strings.ToLower (Unicode simple case mapping),
// which agrees with Postgres's lower() under the usual UTF-8 locales for
// everything but a few special cases (the Turkish dotted I, final sigma).
package citext

import (
	"cmp"
	"strings"
)

// Text is one citext value.
type Text string

// Fold is the value's comparison form: its lowercase text.
func (t Text) Fold() string { return strings.ToLower(string(t)) }

// Cmp orders two values by their lowercase forms: -1, 0 or +1.
func (t Text) Cmp(o Text) int { return cmp.Compare(t.Fold(), o.Fold()) }

// String is the value as written.
func (t Text) String() string { return string(t) }
