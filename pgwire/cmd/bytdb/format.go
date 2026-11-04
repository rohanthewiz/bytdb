package main

// format.go: turning engine values into text. bytdb results carry Go
// runtime values (int64, float64, string, bool, []byte, nil) plus a
// declared column type per column. The declared type matters because
// several SQL types share one runtime representation: a timestamp and
// a date are both int64, a uuid is []byte. Every renderer therefore
// formats through (value, type) pairs, never the value alone — the same
// rule pgwire's encodeValue follows.
//
// Three renderings exist, for three audiences:
//
//	displayValue  what a person reads in box/list/csv output
//	sqlLiteral    a literal the bytdb parser reads back to the same value
//	              (.mode insert, .dump)
//	jsonValue     a JSON value (.mode json / jsonlines)

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/rohanthewiz/bytdb"
)

// typeName is the SQL spelling of a column type, as the shell shows it
// in the duckbox header row and in DESCRIBE. These are the canonical
// Postgres names, which is also what the bytdb parser accepts back, so
// .schema output is valid DDL.
func typeName(t bytdb.ColType) string {
	switch t {
	case bytdb.TInt:
		return "bigint"
	case bytdb.TFloat:
		return "double precision"
	case bytdb.TString:
		return "text"
	case bytdb.TBool:
		return "boolean"
	case bytdb.TBytes:
		return "bytea"
	case bytdb.TTimestamp:
		return "timestamp"
	case bytdb.TDate:
		return "date"
	case bytdb.TUUID:
		return "uuid"
	case bytdb.TTextArray:
		return "text[]"
	case bytdb.TJSONB:
		return "jsonb"
	}
	return string(t)
}

// shortTypeName is typeName compacted for the duckbox header row, where
// "double precision" would widen every float column for no information.
func shortTypeName(t bytdb.ColType) string {
	if t == bytdb.TFloat {
		return "double"
	}
	return typeName(t)
}

// isNumeric reports whether a column type right-aligns in box output.
func isNumeric(t bytdb.ColType) bool { return t == bytdb.TInt || t == bytdb.TFloat }

// displayValue renders v for a human. ok is false for SQL NULL, so each
// renderer can substitute its own null text (.nullvalue).
func displayValue(v any, t bytdb.ColType) (s string, ok bool) {
	if v == nil {
		return "", false
	}
	// Declared-type arms first: they are the only way to tell an
	// instant from a count, or a uuid from opaque bytes.
	switch t {
	case bytdb.TTimestamp:
		if x, ok := v.(int64); ok {
			return bytdb.FormatTimestamp(x), true
		}
	case bytdb.TDate:
		if x, ok := v.(int64); ok {
			return bytdb.FormatDate(x), true
		}
	case bytdb.TUUID:
		if x, ok := v.([]byte); ok && len(x) == 16 {
			return bytdb.FormatUUID(x), true
		}
	}
	switch x := v.(type) {
	case bool:
		if x {
			return "true", true
		}
		return "false", true
	case int64:
		return strconv.FormatInt(x, 10), true
	case float64:
		return formatFloat(x), true
	case string:
		return x, true
	case []byte:
		return `\x` + hex.EncodeToString(x), true
	}
	// Unknown kinds should not occur (the engine produces only the
	// kinds above), but a shell must never crash on output.
	return strings.TrimSpace(strconv.Quote(toString(v))), true
}

// toString is a last-resort rendering for unexpected value kinds.
func toString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	return string(b)
}

// formatFloat renders a float8 the way Postgres does (shortest exact
// representation, exponent only outside [1e-4, 1e15)), rather than Go's
// 'g', which prints 1e+06 for one million. Infinities and NaN use the
// Postgres spellings, which the bytdb parser also accepts as quoted
// float input.
func formatFloat(x float64) string {
	switch {
	case math.IsNaN(x):
		return "NaN"
	case math.IsInf(x, 1):
		return "Infinity"
	case math.IsInf(x, -1):
		return "-Infinity"
	}
	if a := math.Abs(x); a != 0 && (a < 1e-4 || a >= 1e15) {
		return strconv.FormatFloat(x, 'g', -1, 64)
	}
	return strconv.FormatFloat(x, 'f', -1, 64)
}

// sqlLiteral renders v as a SQL literal that reads back as the same
// value when inserted into a column of type t. Everything that is not a
// plain number or boolean goes through a quoted string, because bytdb
// (like Postgres) coerces a quoted literal to the target column's type
// on INSERT — the one spelling that works for timestamp, date, uuid,
// bytea, text[] and jsonb alike.
func sqlLiteral(v any, t bytdb.ColType) string {
	if v == nil {
		return "NULL"
	}
	switch x := v.(type) {
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int64:
		if t == bytdb.TTimestamp || t == bytdb.TDate {
			break // an int64 instant must be spelled as its text form
		}
		return strconv.FormatInt(x, 10)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return quoteString(formatFloat(x))
		}
		// 'g' with -1 precision is the shortest text that parses back
		// to the identical float64, so a dump round-trips exactly.
		return strconv.FormatFloat(x, 'g', -1, 64)
	}
	s, _ := displayValue(v, t)
	return quoteString(s)
}

// quoteString renders s as a standard SQL string literal. bytdb strings
// are standard-conforming: a backslash is an ordinary character, so the
// only escape needed is doubling the quote.
func quoteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// jsonValue renders v as a JSON value. Types with a JSON shape keep it:
// jsonb is embedded as the document itself rather than a string of it,
// text[] becomes a JSON array, numbers and booleans stay native. NaN and
// infinities have no JSON spelling and become strings.
func jsonValue(v any, t bytdb.ColType) string {
	if v == nil {
		return "null"
	}
	switch t {
	case bytdb.TJSONB:
		if s, ok := v.(string); ok && json.Valid([]byte(s)) {
			return s // already canonical JSON text
		}
	case bytdb.TTextArray:
		if s, ok := v.(string); ok {
			if elems, err := bytdb.ParseTextArray(s); err == nil {
				b, err := json.Marshal(elems) // elements are string or nil
				if err == nil {
					return string(b)
				}
			}
		}
	}
	switch x := v.(type) {
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int64:
		if t != bytdb.TTimestamp && t != bytdb.TDate {
			return strconv.FormatInt(x, 10)
		}
	case float64:
		if !math.IsNaN(x) && !math.IsInf(x, 0) {
			return strconv.FormatFloat(x, 'g', -1, 64)
		}
	}
	s, _ := displayValue(v, t)
	b, _ := json.Marshal(s) // a string always marshals
	return string(b)
}

// reservedWords are the words that cannot appear as a bare identifier.
// The list is Postgres's reserved set plus a few bytdb treats as
// statement keywords; quoting a word that did not need it is harmless,
// while failing to quote one that did would make .schema/.dump output
// unparseable, so the list errs on the long side.
var reservedWords = map[string]bool{
	"all": true, "analyse": true, "analyze": true, "and": true, "any": true,
	"array": true, "as": true, "asc": true, "asymmetric": true, "between": true,
	"both": true, "by": true, "case": true, "cast": true, "check": true,
	"collate": true, "column": true, "constraint": true, "create": true,
	"cross": true, "current_date": true, "current_role": true,
	"current_time": true, "current_timestamp": true, "current_user": true,
	"default": true, "deferrable": true, "delete": true, "desc": true,
	"distinct": true, "do": true, "drop": true, "else": true, "end": true,
	"except": true, "exists": true, "false": true, "fetch": true, "for": true,
	"foreign": true, "from": true, "full": true, "grant": true, "group": true,
	"having": true, "ilike": true, "in": true, "index": true, "initially": true,
	"inner": true, "insert": true, "intersect": true, "into": true, "is": true,
	"join": true, "key": true, "lateral": true, "leading": true, "left": true,
	"like": true, "limit": true, "localtime": true, "localtimestamp": true,
	"natural": true, "not": true, "null": true, "offset": true, "on": true,
	"only": true, "or": true, "order": true, "outer": true, "over": true,
	"placing": true, "primary": true, "references": true, "returning": true,
	"right": true, "select": true, "session_user": true, "set": true,
	"some": true, "symmetric": true, "table": true, "then": true, "to": true,
	"trailing": true, "true": true, "union": true, "unique": true,
	"update": true, "user": true, "using": true, "values": true,
	"variadic": true, "when": true, "where": true, "window": true, "with": true,
}

// quoteIdent renders a name as a SQL identifier: bare when it is a
// plain lowercase word the parser would read back unchanged, double
// quoted otherwise (uppercase letters, spaces, punctuation, reserved
// words, a leading digit).
func quoteIdent(name string) string {
	if name != "" && !reservedWords[name] && isPlainIdent(name) {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// isPlainIdent reports whether name is [a-z_][a-z0-9_]*.
func isPlainIdent(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '_' || ('a' <= c && c <= 'z'):
		case '0' <= c && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
