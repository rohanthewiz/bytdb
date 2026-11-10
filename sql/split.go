package sql

// split.go: splitting a script into statements — the simple Query
// buffer a wire client sends, a migration file handed to ExecScript or
// to the database/sql driver's Exec (deep-look D-56; the wire server's
// splitter moved here so the three share one). The split respects the
// lexer's string, identifier, and comment syntax, so a quoted ';'
// never splits. The opaque regions mirror sql/lexer.go:
//
//	'...'      standard string, '' is an escaped quote
//	E'...'     escape string: '' or \' escape the quote, \\ a backslash
//	"..."      quoted identifier, "" is an escaped quote
//	$$...$$    dollar-quoted string; $tag$...$tag$ likewise, no escapes
//	-- ...     line comment, to end of line
//	/* */      block comment, nesting allowed (as in Postgres)
//
// Getting E-strings right matters because psql sends everything it
// reads through the simple protocol: before they were tracked,
// SELECT E'it\'s; fine' split at the ';' and both halves failed to
// parse. Dollar quoting arrived with deep-look D-52; until the splitter
// learned it, SELECT $$a;b$$ split inside the string. The shell's own
// scanner (pgwire/cmd/bytdb/scan.go) follows the same rules.

import "strings"

// ScriptPart is one statement of a script: its text, trimmed, and its
// byte offset in the script (for error positions).
type ScriptPart struct {
	Text   string
	Offset int
}

// SplitScript splits a script on top-level semicolons, dropping empty
// statements. The regions listed in the file header are opaque, so a
// ';' inside one never splits. An unterminated region runs to the end
// of the script; the parser then reports it properly.
//
// A statement made only of comments and whitespace is empty, as in
// Postgres. pgx's liveness check sends exactly that ("-- ping"), and
// handing it to the parser made every database/sql Ping fail with
// "driver: bad connection", so pools discarded healthy connections; a
// trailing comment after the last ';' failed the same way after the
// statements before it had run (deep-look D-08). The test lives here,
// in the lexer-aware scan, because only the scan knows where comments
// are: nested block comments and "--" inside strings defeat a regexp.
func SplitScript(q string) []ScriptPart {
	var parts []ScriptPart
	start := 0
	content := false // the current statement has a byte outside comments and whitespace
	emit := func(end int) {
		text := q[start:end]
		if lead := len(text) - len(strings.TrimLeft(text, " \t\r\n")); content {
			parts = append(parts, ScriptPart{Text: strings.TrimSpace(text), Offset: start + lead})
		}
		start = end + 1
		content = false
	}
	depth := 0 // block comment nesting
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case depth > 0:
			switch {
			case c == '*' && i+1 < len(q) && q[i+1] == '/':
				depth--
				i++
			case c == '/' && i+1 < len(q) && q[i+1] == '*':
				depth++
				i++
			}
		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			depth++
			i++
		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			for i < len(q) && q[i] != '\n' {
				i++
			}
		case c == '\'' || c == '"':
			content = true
			i = skipQuoted(q, i)
		case c == '$' && opensDollarQuote(q, i):
			content = true
			i = skipDollarQuoted(q, i)
		case c == ';':
			emit(i)
		case c != ' ' && c != '\t' && c != '\r' && c != '\n' && c != '\f' && c != '\v':
			content = true
		}
	}
	emit(len(q))
	return parts
}

// IsEmptyScript reports whether q holds no statement at all: nothing
// but whitespace, comments and semicolons. Postgres answers such a
// query with EmptyQueryResponse in either protocol.
func IsEmptyScript(q string) bool { return len(SplitScript(q)) == 0 }

// skipQuoted returns the index of the quote that closes the region
// opened at q[open], or len(q) when the region is unterminated, so the
// caller's loop increment steps past it either way.
//
// A doubled quote is consumed inside the region rather than read as a
// close and a reopen. For a standard string the two readings split the
// same way, but for an E-string the reopen would forget the escape
// mode and misread a later \' as the terminator:
//
//	E'a''b\'c'   one string: a ' b ' c
//	E'a'  'b\'c' what the close-and-reopen reading would see
func skipQuoted(q string, open int) int {
	quote := q[open]
	escapes := quote == '\'' && isEStringPrefix(q, open)
	for i := open + 1; i < len(q); i++ {
		switch c := q[i]; {
		case escapes && c == '\\':
			i++ // the escaped byte, whatever it is, cannot close
		case c == quote:
			if i+1 < len(q) && q[i+1] == quote {
				i++
				continue
			}
			return i
		}
	}
	return len(q)
}

// isEStringPrefix reports whether the quote at q[open] opens an E'...'
// escape string: the byte before it is an E (either case) that starts
// a token of its own. "some'x'" is the identifier some followed by a
// standard string, because the lexer reads identifiers greedily and the
// e never stands alone. A digit before the E is read as part of a
// token here too; the lexer refuses 1e'x' outright ("trailing junk
// after numeric literal"), so such a statement fails to parse whichever
// way it splits.
func isEStringPrefix(q string, open int) bool {
	if open == 0 || (q[open-1] != 'e' && q[open-1] != 'E') {
		return false
	}
	return open < 2 || !isIdentPart(q[open-2])
}

// opensDollarQuote reports whether the '$' at q[i] opens a dollar-quoted
// string, by the lexer's rule: '$' then '$' or an identifier start
// ($1 is a placeholder), and not inside an identifier — the lexer reads
// a$b as one identifier, so a '$' right after an identifier byte never
// starts a token.
func opensDollarQuote(q string, i int) bool {
	if i+1 >= len(q) || (q[i+1] != '$' && !isIdentStart(q[i+1])) {
		return false
	}
	return i == 0 || !isIdentPart(q[i-1])
}

// skipDollarQuoted returns the index of the last byte of the closing
// tag of the dollar-quoted string opened at q[open] (so the caller's
// increment steps past it), or len(q) when the tag or the string is
// unterminated.
func skipDollarQuoted(q string, open int) int {
	end := open + 1
	for end < len(q) && q[end] != '$' {
		if !isIdentPart(q[end]) {
			// Not a tag after all ($a b...): the lexer refuses it; treat
			// the '$' as an ordinary byte and let parsing report it.
			return open
		}
		end++
	}
	if end >= len(q) {
		return len(q)
	}
	tag := q[open : end+1]
	body := strings.Index(q[end+1:], tag)
	if body < 0 {
		return len(q)
	}
	return end + 1 + body + len(tag) - 1
}
