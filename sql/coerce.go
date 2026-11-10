package sql

// coerce.go: Postgres-style adaptation of quoted literals. Postgres
// treats a quoted literal as untyped and coerces it to the type its
// context demands ('2' compares and inserts as an integer against an
// int column). The parser reads every quoted literal as a string — it
// has no catalog — so the conversion happens after parsing: coerceLit
// below is the conversion itself, and the typed bind pass
// (typecheck.go) decides where it applies. That pass replaced
// coerceLiterals, which converted literals only in a top-level
// statement's simple predicates (deep-look D-23); the plan's rule was to
// remove it rather than keep two passes that both claim to type
// literals, since that is how the gaps arose.

import (
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// coerceLit converts a string value to a non-string column type's
// value kind, per Postgres literal syntax. Non-strings and string
// columns pass through. A bound time.Time converts here too — only
// the column type can say whether it means timestamp micros or date
// days.
func coerceLit(v any, t bytdb.ColType) (any, error) {
	if tv, ok := v.(time.Time); ok {
		switch t {
		case bytdb.TTimestamp:
			return tv.UnixMicro(), nil
		case bytdb.TDate:
			u := tv.UTC()
			return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC).Unix() / 86400, nil
		}
		return v, nil // any other column type: the engine's coercion reports it
	}
	s, ok := v.(string)
	if !ok {
		return v, nil
	}
	switch t {
	case bytdb.TInt:
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, serr.New("invalid input syntax for type int", "value", s)
		}
		return n, nil
	case bytdb.TFloat:
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, serr.New("invalid input syntax for type float", "value", s)
		}
		return f, nil
	case bytdb.TBool:
		b, err := strconv.ParseBool(strings.TrimSpace(strings.ToLower(s)))
		if err != nil {
			return nil, serr.New("invalid input syntax for type bool", "value", s)
		}
		return b, nil
	case bytdb.TBytes:
		if strings.HasPrefix(s, `\x`) {
			b, err := hex.DecodeString(s[2:])
			if err != nil {
				return nil, serr.New("invalid input syntax for type bytea", "value", s)
			}
			return b, nil
		}
		return []byte(s), nil
	case bytdb.TTimestamp:
		return bytdb.ParseTimestamp(s)
	case bytdb.TDate:
		return bytdb.ParseDate(s)
	case bytdb.TUUID:
		return bytdb.ParseUUID(s)
	case bytdb.TTextArray:
		// Canonicalize here as well as in the engine so comparisons in
		// WHERE ('{a, b}' = categories) match the stored spelling.
		return bytdb.CanonTextArray(s)
	case bytdb.TJSONB:
		// Same as TTextArray: WHERE '{"a": 1}' = doc must compare in
		// canonical spelling, not the query's.
		return bytdb.CanonJSONB(s)
	}
	return s, nil
}
