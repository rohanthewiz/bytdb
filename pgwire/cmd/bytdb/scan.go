package main

// scan.go: the lexical scan the shell needs before anything reaches
// the parser. The shell reads SQL a line at a time and must decide when
// a statement is complete. A naive "line ends with ;" test fails as
// soon as a ';' sits inside a string, a quoted identifier or a comment,
// so the scan tracks exactly the bytdb lexer's opaque regions:
//
//	'...'    standard string, '' is an escaped quote
//	E'...'   escape string: '' or \' escape the quote, \\ a backslash
//	"..."    quoted identifier, "" is an escaped quote
//	-- ...   line comment, to end of line
//	/* */    block comment, nesting allowed (as in Postgres)
//
// Dollar quoting ($$...$$) is absent on purpose: the bytdb lexer has no
// dollar-quoted strings, and $1 is a parameter, so treating '$' as a
// quote would only mis-split statements the engine would reject anyway.
//
// pgwire has its own splitter (pgwire/split.go), but it is unexported
// and does not know E-strings: E'it\'s; fine' would split at the ';'.
// The shell is the one place users type escape strings by hand, so it
// carries its own, stricter scan.

import "strings"

// scanResult is the outcome of scanning a buffer of SQL text.
type scanResult struct {
	// stmts are the complete statements: text up to and excluding each
	// top-level ';', trimmed, with comment-only pieces dropped.
	stmts []string
	// rest is the text after the last top-level ';'.
	rest string
	// restHasContent reports whether rest holds anything besides
	// whitespace and comments — i.e. a statement still being typed.
	restHasContent bool
	// open reports that the scan ended inside a string, quoted
	// identifier or block comment, so rest cannot be complete however
	// it continues.
	open bool
}

// scanSQL splits src on top-level semicolons. It never fails: an
// unterminated string simply leaves open set, and the caller decides
// whether to keep reading (interactive) or hand the text to the parser
// for a proper error (end of input).
func scanSQL(src string) scanResult {
	var r scanResult
	start := 0       // byte offset where the current piece begins
	content := false // current piece has a non-comment token
	depth := 0       // block-comment nesting depth
	var quote byte   // 0, '\'' or '"': the open quoted region
	escapes := false // the open string is an E'...' string
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case quote != 0:
			// Inside a quoted region only the closing quote (or, in an
			// E-string, a backslash escape) matters.
			if escapes && c == '\\' {
				i++ // skip the escaped byte, whatever it is
				continue
			}
			if c == quote {
				// A doubled quote is an escape, not a close; consume
				// both so the region keeps its E-string-ness (closing
				// and reopening would forget it and misread a later
				// \' as a terminator).
				if i+1 < len(src) && src[i+1] == quote {
					i++
					continue
				}
				quote, escapes = 0, false
			}
		case depth > 0:
			switch {
			case c == '*' && i+1 < len(src) && src[i+1] == '/':
				depth--
				i++
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				depth++
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			depth++
			i++
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '\'' || c == '"':
			quote = c
			// E'...' (or e'...') is an escape string only when the E
			// is a token of its own — "some'" is an identifier ending
			// in e followed by a string, not an escape string.
			escapes = c == '\'' && i > 0 && (src[i-1] == 'e' || src[i-1] == 'E') &&
				(i < 2 || !isIdentByte(src[i-2]))
			content = true
		case c == ';':
			if content {
				r.stmts = append(r.stmts, strings.TrimSpace(src[start:i]))
			}
			start, content = i+1, false
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			// whitespace: no content
		default:
			content = true
		}
	}
	r.rest = src[start:]
	r.restHasContent = content
	r.open = quote != 0 || depth > 0
	return r
}

// isIdentByte reports whether c can continue an unquoted identifier.
// Bytes >= 0x80 count, so a UTF-8 identifier ending just before a quote
// is not mistaken for a token boundary.
func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 ||
		('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}

// historyLine flattens a multi-line statement into one history entry.
// The line editor's history is line-based, and recalling a statement
// spread over several entries would be useless. Joining lines with a
// space is only safe once every "-- comment" is closed, or the comment
// would swallow everything joined after it; line comments are therefore
// rewritten as block comments. Newlines inside strings are kept as
// they are, because rewriting them would change the value.
func historyLine(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	var quote byte
	escapes := false
	depth := 0
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case quote != 0:
			b.WriteByte(c)
			if escapes && c == '\\' && i+1 < len(src) {
				i++
				b.WriteByte(src[i])
				continue
			}
			if c == quote {
				if i+1 < len(src) && src[i+1] == quote {
					i++
					b.WriteByte(src[i])
					continue
				}
				quote, escapes = 0, false
			}
		case depth > 0:
			if c == '\n' {
				b.WriteByte(' ')
				continue
			}
			b.WriteByte(c)
			switch {
			case c == '*' && i+1 < len(src) && src[i+1] == '/':
				depth--
				i++
				b.WriteByte('/')
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				depth++
				i++
				b.WriteByte('*')
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			depth++
			i++
			b.WriteString("/*")
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				j = len(src) - i
			}
			// "*/" inside the comment would close the block comment
			// early; break it up so the rewrite stays one comment.
			body := strings.ReplaceAll(src[i+2:i+j], "*/", "* /")
			b.WriteString("/*")
			b.WriteString(body)
			b.WriteString(" */")
			i += j - 1 // the loop's i++ lands on the '\n', handled next
		case c == '\'' || c == '"':
			quote = c
			escapes = c == '\'' && i > 0 && (src[i-1] == 'e' || src[i-1] == 'E') &&
				(i < 2 || !isIdentByte(src[i-2]))
			b.WriteByte(c)
		case c == '\n' || c == '\r':
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	// Collapse the runs of spaces left where newlines and indentation
	// met; strings were written verbatim above, so only collapse
	// outside them — simplest done by a second, quote-aware pass.
	return collapseSpaces(b.String())
}

// collapseSpaces squeezes runs of spaces and tabs outside quoted
// regions into one space and trims the ends.
func collapseSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	var quote byte
	escapes := false
	space := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			b.WriteByte(c)
			if escapes && c == '\\' && i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			} else if c == quote {
				if i+1 < len(s) && s[i+1] == quote {
					i++
					b.WriteByte(s[i])
				} else {
					quote, escapes = 0, false
				}
			}
			continue
		}
		if c == ' ' || c == '\t' {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		if c == '\'' || c == '"' {
			quote = c
			escapes = c == '\'' && i > 0 && (s[i-1] == 'e' || s[i-1] == 'E') &&
				(i < 2 || !isIdentByte(s[i-2]))
		}
		b.WriteByte(c)
	}
	return b.String()
}
