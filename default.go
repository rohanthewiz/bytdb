package bytdb

import (
	"strconv"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"
)

// default.go: reading a column's stored DEFAULT back to a value.
//
// The descriptor stores a DEFAULT as SQL literal text (Column.Default)
// and the SQL layer owns that syntax — it is what renders the text on
// the way in and what evaluates it on every INSERT. The engine needs
// the same value in exactly one place: AddColumn's backfill, which
// must write the default into rows that already exist and therefore
// cannot ask the SQL layer (sql imports bytdb, not the other way).
//
// So this file re-reads the narrow subset of literal forms the SQL
// layer's renderLit can emit, plus the two evaluated clock markers.
// Anything outside that vocabulary is an error rather than a guess:
// the caller (AddColumn) then refuses the backfill instead of writing
// a value it invented.
//
// Kept deliberately in sync with sql/parser.go's renderLit and
// parseStoredLiteral; a new literal form there needs an arm here.

// Stored spellings of the evaluated (non-constant) defaults. These
// mirror sql.DefaultNow / sql.DefaultCurrentDate, which are stored
// unquoted precisely so they cannot collide with a constant string
// default (renderLit always quotes those).
const (
	defaultNowText         = "now()"
	defaultCurrentDateText = "current_date"
)

// columnDefaultValue evaluates a column's stored DEFAULT to the value
// a row would hold, coerced to the column type and length-checked the
// same way an insert's value would be. now is the instant the clock
// markers resolve to — passed in so one DDL statement stamps every
// backfilled row with a single instant.
//
// A nil value with a nil error means "the default is SQL NULL", which
// is indistinguishable from having no default at all as far as stored
// rows are concerned.
func columnDefaultValue(col *Column, now time.Time) (any, error) {
	v, err := decodeDefaultLiteral(col.Default, now)
	if err != nil {
		return nil, serr.Wrap(err, "column", col.Name, "default", col.Default)
	}
	cv, err := coerce(v, col.Type)
	if err != nil {
		return nil, serr.Wrap(err, "op", "coerce column default",
			"column", col.Name, "default", col.Default)
	}
	if cv, err = enforceMaxLen(cv, col); err != nil {
		return nil, serr.Wrap(err, "column", col.Name, "default", col.Default)
	}
	return cv, nil
}

// decodeDefaultLiteral turns stored DEFAULT text into a Go value,
// before type coercion. current_date yields midnight UTC of now's
// day, which coerce then lands correctly on either target type (a
// date column takes the day number, a timestamp column midnight) —
// the same truncate-then-coerce order the SQL layer's insert path
// uses.
func decodeDefaultLiteral(text string, now time.Time) (any, error) {
	s := strings.TrimSpace(text)
	switch {
	case s == "" || strings.EqualFold(s, "null"):
		return nil, nil
	case s == defaultNowText:
		return now, nil
	case s == defaultCurrentDateText:
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), nil
	case strings.EqualFold(s, "true"):
		return true, nil
	case strings.EqualFold(s, "false"):
		return false, nil
	}
	if s[0] == '\'' {
		lit, ok := unquoteSQLString(s)
		if !ok {
			return nil, serr.New("malformed string literal in column DEFAULT", "text", text)
		}
		return lit, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f, nil
	}
	// An expression, a cast, a function call the engine does not
	// evaluate — the SQL layer may still handle it on INSERT, but the
	// engine will not fabricate a value for existing rows.
	return nil, serr.New("column DEFAULT is not a literal the engine can evaluate", "text", text)
}

// unquoteSQLString reads a single-quoted SQL string literal, undoing
// the doubled-quote escape renderLit applies. ok is false when s is
// not one complete literal — including the case of a quote that
// closes it before the end, which would mean the text is an
// expression ('a' || 'b'), not a constant.
func unquoteSQLString(s string) (string, bool) {
	if len(s) < 2 || s[0] != '\'' || s[len(s)-1] != '\'' {
		return "", false
	}
	body := s[1 : len(s)-1]
	var b strings.Builder
	b.Grow(len(body))
	for i := 0; i < len(body); i++ {
		if body[i] != '\'' {
			b.WriteByte(body[i])
			continue
		}
		if i+1 < len(body) && body[i+1] == '\'' {
			b.WriteByte('\'')
			i++
			continue
		}
		return "", false
	}
	return b.String(), true
}
