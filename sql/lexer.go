package sql

import (
	"fmt"
	"strings"

	"github.com/rohanthewiz/serr"
)

// tokKind classifies one lexed token.
type tokKind int

const (
	tEOF    tokKind = iota
	tIdent          // unquoted identifier or keyword, folded to lowercase
	tQIdent         // "quoted" identifier, verbatim
	tString         // 'string' literal, escapes resolved
	tNumber         // integer or float literal
	tOp             // operator or punctuation
	tParam          // $1-style placeholder
)

type token struct {
	kind tokKind
	text string
	pos  int // byte offset in the source, for error messages
}

// lex tokenizes one SQL statement. Unquoted identifiers fold to
// lowercase, so keywords and column names match case-insensitively;
// "quoted" identifiers and 'string' literals resolve their doubled
// quote escapes. -- line and /* block */ comments are skipped.
func lex(src string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, serr.New("unterminated block comment", "pos", fmt.Sprint(i))
			}
			i += 2 + end + 2
		case (c == 'e' || c == 'E') && i+1 < len(src) && src[i+1] == '\'':
			// E'...' escape-string literal (\n, \t, \\, '', \', ...)
			text, n, err := scanEString(src[i+1:])
			if err != nil {
				return nil, serr.Wrap(err, "pos", fmt.Sprint(i))
			}
			toks = append(toks, token{tString, text, i})
			i += 1 + n
		case isIdentStart(c):
			start := i
			for i < len(src) && isIdentPart(src[i]) {
				i++
			}
			toks = append(toks, token{tIdent, strings.ToLower(src[start:i]), start})
		case c == '"':
			text, n, err := scanQuoted(src[i:], '"')
			if err != nil {
				return nil, serr.Wrap(err, "pos", fmt.Sprint(i))
			}
			toks = append(toks, token{tQIdent, text, i})
			i += n
		case c == '\'':
			text, n, err := scanQuoted(src[i:], '\'')
			if err != nil {
				return nil, serr.Wrap(err, "pos", fmt.Sprint(i))
			}
			toks = append(toks, token{tString, text, i})
			i += n
		case isDigit(c) || (c == '.' && i+1 < len(src) && isDigit(src[i+1])):
			start := i
			i = scanNumber(src, i)
			toks = append(toks, token{tNumber, src[start:i], start})
		case c == '$' && i+1 < len(src) && isDigit(src[i+1]):
			start := i
			i++
			for i < len(src) && isDigit(src[i]) {
				i++
			}
			toks = append(toks, token{tParam, src[start:i], start})
		default:
			op := scanOp(src, i)
			if op == "" {
				return nil, serr.New("unexpected character", "char", string(c), "pos", fmt.Sprint(i))
			}
			toks = append(toks, token{tOp, op, i})
			i += len(op)
		}
	}
	return append(toks, token{tEOF, "", len(src)}), nil
}

// scanQuoted scans a q-delimited region starting at s[0] == q, where a
// doubled qq is an escaped q. It returns the unescaped content and the
// number of source bytes consumed.
func scanQuoted(s string, q byte) (string, int, error) {
	var b strings.Builder
	i := 1
	for i < len(s) {
		if s[i] == q {
			if i+1 < len(s) && s[i+1] == q {
				b.WriteByte(q)
				i += 2
				continue
			}
			return b.String(), i + 1, nil
		}
		b.WriteByte(s[i])
		i++
	}
	what := "string literal"
	if q == '"' {
		what = "quoted identifier"
	}
	return "", 0, serr.New("unterminated " + what)
}

// scanEString scans an E-prefixed string body starting at s[0] == ',
// resolving backslash escapes and doubled quotes. It returns the
// content and the bytes consumed from s.
func scanEString(s string) (string, int, error) {
	var b strings.Builder
	i := 1
	for i < len(s) {
		switch {
		case s[i] == '\'':
			if i+1 < len(s) && s[i+1] == '\'' {
				b.WriteByte('\'')
				i += 2
				continue
			}
			return b.String(), i + 1, nil
		case s[i] == '\\' && i+1 < len(s):
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			default: // \\ \' and any other char stand for themselves
				b.WriteByte(s[i+1])
			}
			i += 2
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return "", 0, serr.New("unterminated string literal")
}

// scanNumber consumes digits [. digits] [e[+-]digits] starting at i.
func scanNumber(src string, i int) int {
	for i < len(src) && isDigit(src[i]) {
		i++
	}
	if i < len(src) && src[i] == '.' {
		i++
		for i < len(src) && isDigit(src[i]) {
			i++
		}
	}
	if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
		j := i + 1
		if j < len(src) && (src[j] == '+' || src[j] == '-') {
			j++
		}
		if j < len(src) && isDigit(src[j]) {
			i = j
			for i < len(src) && isDigit(src[i]) {
				i++
			}
		}
	}
	return i
}

// scanOp recognizes the operator starting at src[i], longest first, or
// returns "".
func scanOp(src string, i int) string {
	if i+2 < len(src) && src[i:i+3] == "!~*" {
		return "!~*"
	}
	if i+1 < len(src) {
		switch two := src[i : i+2]; two {
		case "!=", "<>", "<=", ">=", "!~", "~*", "::", "||":
			return two
		}
	}
	switch src[i] {
	case '=', '<', '>', '(', ')', ',', ';', '*', '.', '+', '-',
		'~', '[', ']', '/', '%':
		return string(src[i])
	}
	return ""
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }
