package sql

import (
	"fmt"
	"math/big"
	"strings"
	"unicode/utf8"

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
// quote escapes. -- line and /* block */ comments are skipped; block
// comments nest (blockCommentLen).
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
			n := blockCommentLen(src[i:])
			if n < 0 {
				return nil, serr.New("unterminated block comment", "pos", fmt.Sprint(i))
			}
			i += n
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
			start := i
			i += n
			// String constants separated only by whitespace that includes
			// a newline are one constant, as the SQL standard and Postgres
			// have it ('foo'\n'bar' is 'foobar'); on one line the second
			// is a syntax error, as there (deep-look D-52).
			for {
				j, newline := i, false
				for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\n' || src[j] == '\r') {
					newline = newline || src[j] == '\n'
					j++
				}
				if !newline || j >= len(src) || src[j] != '\'' {
					break
				}
				more, m, err := scanQuoted(src[j:], '\'')
				if err != nil {
					return nil, serr.Wrap(err, "pos", fmt.Sprint(j))
				}
				text += more
				i = j + m
			}
			toks = append(toks, token{tString, text, start})
		case isDigit(c) || (c == '.' && i+1 < len(src) && isDigit(src[i+1])):
			start := i
			end, err := scanNumber(src, i)
			if err != nil {
				return nil, serr.Wrap(err, "pos", fmt.Sprint(start))
			}
			i = end
			text, err := normalizeNumber(src[start:i])
			if err != nil {
				return nil, serr.Wrap(err, "pos", fmt.Sprint(start))
			}
			toks = append(toks, token{tNumber, text, start})
		case c == '$' && i+1 < len(src) && (src[i+1] == '$' || isIdentStart(src[i+1])):
			// Dollar quoting: $$...$$ or $tag$...$tag$, a string constant
			// with no escapes at all — how function bodies and migration
			// scripts quote text full of quotes (deep-look D-52).
			text, n, err := scanDollarQuoted(src[i:])
			if err != nil {
				return nil, serr.Wrap(err, "pos", fmt.Sprint(i))
			}
			toks = append(toks, token{tString, text, i})
			i += n
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

// blockCommentLen returns the length of the block comment that opens at
// s[0:2] == "/*", or -1 when it never closes. Block comments nest, as in
// Postgres (and the SQL standard): each "/*" inside opens another level
// and the comment ends at the "*/" that closes the outermost one.
//
//	/* a /* b */ c */ x     the comment is everything before " x"
//	/* a /* b */ c           unterminated: depth never returns to 0
//
// pgwire's statement splitter and the shell's scanner count depth the
// same way, so all three agree on where a comment ends (N-034). Before
// this, the lexer stopped at the first "*/" and the splitter did not.
func blockCommentLen(s string) int {
	depth := 0
	for i := 0; i+1 < len(s); {
		switch {
		case s[i] == '/' && s[i+1] == '*':
			depth++
			i += 2
		case s[i] == '*' && s[i+1] == '/':
			depth--
			i += 2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return -1
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
			// Octal and hex escapes write raw bytes, which can spell
			// something that is not UTF-8; Postgres refuses that too.
			if !utf8.ValidString(b.String()) {
				return "", 0, serr.New("invalid byte sequence for encoding \"UTF8\"")
			}
			return b.String(), i + 1, nil
		case s[i] == '\\' && i+1 < len(s):
			switch c := s[i+1]; {
			case c == 'n':
				b.WriteByte('\n')
			case c == 't':
				b.WriteByte('\t')
			case c == 'r':
				b.WriteByte('\r')
			case c == 'b':
				b.WriteByte('\b')
			case c == 'f':
				b.WriteByte('\f')
			// The numeric escapes Postgres defines (deep-look D-22): they
			// used to fall to the default, so E'\x41' stored "x41" and
			// E'\u00e9' stored "u00e9" — silently changed data.
			case c >= '0' && c <= '7': // \o, \oo, \ooo: an octal byte value
				n, j := digitsValue(s, i+1, 3, 8)
				b.WriteByte(byte(n))
				i = j
				continue
			case c == 'x' && i+2 < len(s) && isHexDigit(s[i+2]): // \xh, \xhh: a hex byte value
				n, j := digitsValue(s, i+2, 2, 16)
				b.WriteByte(byte(n))
				i = j
				continue
			case c == 'u' || c == 'U': // \uXXXX, \UXXXXXXXX: a Unicode code point
				width := 4
				if c == 'U' {
					width = 8
				}
				n, j := digitsValue(s, i+2, width, 16)
				if j-(i+2) != width || !utf8.ValidRune(rune(n)) || n == 0 {
					return "", 0, serr.New("invalid Unicode escape",
						"hint", `Unicode escapes must be \uXXXX or \UXXXXXXXX`)
				}
				b.WriteRune(rune(n))
				i = j
				continue
			default: // \\ \' and any other char stand for themselves
				b.WriteByte(c)
			}
			i += 2
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return "", 0, serr.New("unterminated string literal")
}

// digitsValue reads up to max digits of the given base starting at
// s[i], returning their value and the index after the last one read.
func digitsValue(s string, i, max, base int) (int, int) {
	n := 0
	for k := 0; k < max && i < len(s); k++ {
		d := digitValue(s[i])
		if d < 0 || d >= base {
			break
		}
		n = n*base + d
		i++
	}
	return n, i
}

// digitValue is a hex digit's value, or -1.
func digitValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

func isHexDigit(c byte) bool { return digitValue(c) >= 0 }

// scanNumber consumes a numeric literal starting at i and returns the
// index after it. Postgres 16's forms (deep-look D-22):
//
//	123  1_000_000  1.5  .5  1e10  1.5E-3   decimal, underscores between digits
//	0x1F  0o17  0b101  0x_FF_FF              integer with a base prefix
//
// A literal followed directly by an identifier character is an error,
// "trailing junk after numeric literal", as in Postgres 15+. Before,
// 0x1F lexed as 0 followed by the alias x1F, 1_000 as 1 aliased _000,
// and 1a as 1 aliased a — each a silently wrong answer.
func scanNumber(src string, i int) (int, error) {
	start := i
	if src[i] == '0' && i+1 < len(src) {
		base := 0
		switch src[i+1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			j, ok := scanDigits(src, i+2, base, true)
			if !ok || j == i+2 {
				return i, serr.New("trailing junk after numeric literal", "text", src[start:min(j+1, len(src))])
			}
			return endOfNumber(src, start, j)
		}
	}
	j, ok := scanDigits(src, i, 10, false)
	if !ok {
		return i, serr.New("trailing junk after numeric literal", "text", src[start:min(j+1, len(src))])
	}
	i = j
	if i < len(src) && src[i] == '.' && !(i+1 < len(src) && src[i+1] == '.') {
		i++
		if i < len(src) && isDigit(src[i]) {
			if i, ok = scanDigits(src, i, 10, false); !ok {
				return i, serr.New("trailing junk after numeric literal", "text", src[start:min(i+1, len(src))])
			}
		}
	}
	if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
		j := i + 1
		if j < len(src) && (src[j] == '+' || src[j] == '-') {
			j++
		}
		if j < len(src) && isDigit(src[j]) {
			if i, ok = scanDigits(src, j, 10, false); !ok {
				return i, serr.New("trailing junk after numeric literal", "text", src[start:min(i+1, len(src))])
			}
		}
	}
	return endOfNumber(src, start, i)
}

// scanDigits consumes digits of base from i, allowing single
// underscores between digits (and, after a base prefix, before the
// first). ok is false for a misplaced underscore.
func scanDigits(src string, i, base int, afterPrefix bool) (int, bool) {
	first := true
	for i < len(src) {
		c := src[i]
		if c == '_' {
			// An underscore must sit between two digits (or right after
			// a base prefix, before a digit).
			if (first && !afterPrefix) || i+1 >= len(src) {
				return i, false
			}
			if d := digitValue(src[i+1]); d < 0 || d >= base {
				return i, false
			}
			i++
			continue
		}
		if d := digitValue(c); d < 0 || d >= base || (base == 10 && !isDigit(c)) {
			break
		}
		first = false
		i++
	}
	return i, true
}

// endOfNumber refuses an identifier character directly after a numeric
// literal and otherwise returns end.
func endOfNumber(src string, start, end int) (int, error) {
	if end < len(src) && isIdentPart(src[end]) {
		return end, serr.New("trailing junk after numeric literal",
			"text", src[start:end+1])
	}
	return end, nil
}

// normalizeNumber rewrites a scanned literal into the plain decimal text
// every consumer of a number token parses with strconv: underscores
// dropped, a base-prefixed integer converted to decimal (beyond int64 it
// stays exact as decimal text, which then parses as a float, as an
// oversized decimal integer does).
func normalizeNumber(text string) (string, error) {
	text = strings.ReplaceAll(text, "_", "")
	if len(text) > 2 && text[0] == '0' {
		base := 0
		switch text[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			n, ok := new(big.Int).SetString(text[2:], base)
			if !ok {
				return "", serr.New("bad numeric literal", "text", text)
			}
			return n.String(), nil
		}
	}
	return text, nil
}

// scanOp recognizes the operator starting at src[i], longest first, or
// returns "".
func scanOp(src string, i int) string {
	if i+2 < len(src) {
		switch three := src[i : i+3]; three {
		case "!~*", "->>", "#>>":
			return three
		}
	}
	if i+1 < len(src) {
		switch two := src[i : i+2]; two {
		case "!=", "<>", "<=", ">=", "!~", "~*", "::", "||",
			"->", "#>", "@>", "<@", "?|", "?&":
			return two
		}
	}
	switch src[i] {
	case '=', '<', '>', '(', ')', ',', ';', '*', '.', '+', '-',
		'~', '[', ']', '/', '%', '?':
		return string(src[i])
	}
	return ""
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isIdentStart accepts the bytes Postgres's scanner does: ASCII letters,
// underscore, and every byte of a multi-byte UTF-8 character (>= 0x80),
// so unquoted non-ASCII identifiers (ñame, 名前) lex as identifiers
// (deep-look D-52). strings.ToLower folds them as Unicode.
func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

// isIdentPart also takes digits and '$', which Postgres allows after an
// identifier's first character.
func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) || c == '$' }

// scanDollarQuoted reads a dollar-quoted constant at s[0] == '$': the
// opening tag ($$ or $tag$), the body, and the identical closing tag.
// It returns the body and the bytes consumed.
func scanDollarQuoted(s string) (string, int, error) {
	end := 1
	for end < len(s) && s[end] != '$' {
		if !isIdentPart(s[end]) || s[end] == '$' {
			return "", 0, serr.New("malformed dollar-quote tag")
		}
		end++
	}
	if end >= len(s) {
		return "", 0, serr.New("unterminated dollar-quoted string")
	}
	tag := s[:end+1] // $tag$ (or $$)
	body := strings.Index(s[len(tag):], tag)
	if body < 0 {
		return "", 0, serr.New("unterminated dollar-quoted string")
	}
	return s[len(tag) : len(tag)+body], len(tag) + body + len(tag), nil
}
