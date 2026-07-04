package pgwire

// split.go: splitting a simple-Query buffer into statements. The
// simple protocol allows several ;-separated statements per message;
// the split respects the dialect's string, identifier, and comment
// syntax so a quoted ';' never splits.

import "strings"

// stmtPart is one statement of a Query buffer: its text and its byte
// offset in the original buffer (for error positions).
type stmtPart struct {
	text string
	off  int
}

// splitStatements splits on top-level semicolons, dropping empty
// statements. '...' strings (with ” escapes), "..." identifiers,
// -- line comments, and nested /* */ block comments are opaque.
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
			quote := c
			i++
			for i < len(q) && q[i] != quote {
				i++
			}
			// A '' escape reads as two adjacent strings; both scan fine.
		case c == ';':
			emit(i)
		}
	}
	emit(len(q))
	return parts
}
