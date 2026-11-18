package bytdb

import "strings"

// QuoteIdent quotes an identifier the way Postgres's quote_ident does:
// only when it would not survive unquoted. That is anything but
// lowercase letters, digits, underscores and dollar signs (and not
// leading with a digit or dollar sign), or a reserved word.
//
// It lives in the engine, not the SQL layer, because the engine renders
// Postgres-worded messages too. A refused drop names its target and
// dependents the way Postgres's dependency report does ("cannot drop
// table "P 5" because ..."), and the SQL layer's catalog functions
// (pg_get_indexdef, pg_get_serial_sequence) quote with the same rule.
func QuoteIdent(s string) string {
	plain := s != "" && !reservedWords[s]
	for i := 0; plain && i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c == '_':
		case (c >= '0' && c <= '9') || c == '$':
			plain = i > 0
		default:
			plain = false
		}
	}
	if plain {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// reservedWords are Postgres's reserved keywords (the "reserved" and
// "reserved (can be function or type)" classes): the words quote_ident
// must quote even when they are otherwise plain.
var reservedWords = func() map[string]bool {
	m := map[string]bool{}
	for w := range strings.FieldsSeq(`all analyse analyze and any array as asc
		asymmetric authorization binary both case cast check collate collation
		column concurrently constraint create cross current_catalog current_date
		current_role current_schema current_time current_timestamp current_user
		default deferrable desc distinct do else end except false fetch for
		foreign freeze from full grant group having ilike in initially inner
		intersect into is isnull join lateral leading left like limit localtime
		localtimestamp natural not notnull null offset on only or order outer
		overlaps placing primary references returning right select session_user
		similar some symmetric system_user table tablesample then to trailing
		true union unique user using variadic verbose when where window with`) {
		m[w] = true
	}
	return m
}()
