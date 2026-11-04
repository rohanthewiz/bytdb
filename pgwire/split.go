package pgwire

// split.go: splitting a simple-Query buffer into statements. The
// simple protocol allows several ;-separated statements per message;
// the split respects the dialect's string, identifier, and comment
// syntax so a quoted ';' never splits.
//
// The opaque regions mirror the bytdb lexer (sql/lexer.go):
//
//	'...'    standard string, '' is an escaped quote
//	E'...'   escape string: '' or \' escape the quote, \\ a backslash
//	"..."    quoted identifier, "" is an escaped quote
//	-- ...   line comment, to end of line
//	/* */    block comment, nesting allowed (as in Postgres)
//
// Getting E-strings right matters because psql sends everything it
// reads through the simple protocol: before they were tracked,
// SELECT E'it\'s; fine' split at the ';' and both halves failed to
// parse. The extended protocol never reaches this code (one statement
// per Parse). The shell's own scanner (pgwire/cmd/bytdb/scan.go)
// follows the same rules.

import "strings"

// stmtPart is one statement of a Query buffer: its text and its byte
// offset in the original buffer (for error positions).
type stmtPart struct {
	text string
	off  int
}

// splitStatements splits on top-level semicolons, dropping empty
// statements. The regions listed in the file header are opaque, so a
// ';' inside one never splits. An unterminated region runs to the end
// of the buffer; the parser then reports it properly.
func splitStatements(q string) []stmtPart {
	var parts []stmtPart
	start := 0
	emit := func(end int) {
		text := q[start:end]
		if lead := len(text) - len(strings.TrimLeft(text, " \t\r\n")); strings.TrimSpace(text) != "" {
			parts = append(parts, stmtPart{text: strings.TrimSpace(text), off: start + lead})
		}
		start = end + 1
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
			i = skipQuoted(q, i)
		case c == ';':
			emit(i)
		}
	}
	emit(len(q))
	return parts
}

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
// e never stands alone.
//
// The identifier test matches the lexer's: ASCII letters, digits and
// underscore. The lexer rejects every other byte outside quotes, so a
// statement where the two could disagree fails to parse whichever way
// it splits. The one byte they read differently is a digit: the lexer
// ends a number before an e that no exponent digits follow, so 1e'x'
// lexes as 1 then E'x', where this reads a standard string. A number
// directly followed by a string is a syntax error either way.
func isEStringPrefix(q string, open int) bool {
	if open == 0 || (q[open-1] != 'e' && q[open-1] != 'E') {
		return false
	}
	return open < 2 || !isIdentByte(q[open-2])
}

// isIdentByte reports whether c can continue an unquoted identifier
// (the lexer's isIdentPart).
func isIdentByte(c byte) bool {
	return c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}
