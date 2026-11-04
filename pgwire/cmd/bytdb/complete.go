package main

// complete.go: tab completion. Completion is word-based: the word under
// the cursor is completed against a candidate set chosen by context —
//
//	first word, starting with '.'          → dot commands
//	argument of a file-taking dot command   → file paths
//	anything else                           → SQL keywords, table, view
//	                                          and column names
//
// Catalog names are read from the engine on every Tab press rather than
// cached: the catalog is in memory, a lookup costs microseconds, and a
// cache would need invalidating after every DDL statement.

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode"
)

// sqlKeywords are the words offered for completion. Not the parser's
// full keyword set — the words people actually type at a prompt.
var sqlKeywords = []string{
	"ADD", "ALL", "ALTER", "AND", "ANY", "AS", "ASC", "BEGIN", "BETWEEN", "BIGINT",
	"BOOLEAN", "BY", "BYTEA", "CASCADE", "CASE", "CAST", "CHECK", "COALESCE",
	"COLUMN", "COMMIT", "CONFLICT", "CONSTRAINT", "COUNT", "CREATE", "CROSS",
	"DATE", "DEFAULT", "DELETE", "DESC", "DESCRIBE", "DISTINCT", "DO", "DOUBLE",
	"DROP", "ELSE", "END", "EXCLUDED", "EXISTS", "EXPLAIN", "FALSE", "FOREIGN",
	"FROM", "FULL", "GROUP", "HAVING", "IDENTITY", "IF", "ILIKE", "IN", "INDEX",
	"INNER", "INSERT", "INTEGER", "INTO", "IS", "JOIN", "JSONB", "KEY", "LEFT",
	"LIKE", "LIMIT", "NOT", "NOTHING", "NULL", "OFFSET", "ON", "OR", "ORDER",
	"OUTER", "OVER", "PARTITION", "PRECISION", "PRIMARY", "RANGE", "REFERENCES",
	"RELEASE", "RENAME", "RESTART", "RETURNING", "RIGHT", "ROLLBACK", "ROWS",
	"SAVEPOINT", "SELECT", "SEQUENCE", "SERIAL", "SET", "SHOW", "TABLE", "TABLES",
	"TEXT", "THEN", "TIMESTAMP", "TO", "TRANSACTION", "TRUE", "TRUNCATE", "TYPE",
	"UNION", "UNIQUE", "UPDATE", "USING", "UUID", "VACUUM", "VALUES", "VARCHAR",
	"VIEW", "WHEN", "WHERE", "WINDOW", "WITH",
}

// fileArgCommands are the dot commands whose arguments are file paths.
var fileArgCommands = map[string]bool{
	".open": true, ".read": true, ".import": true, ".output": true,
	".once": true, ".backup": true, ".cd": true,
	`\i`: true, `\o`: true,
}

// complete is the liner.WordCompleter: it returns the text before the
// completed word, the candidate words, and the text after the cursor.
func (s *Shell) complete(line string, pos int) (head string, completions []string, tail string) {
	runes := []rune(line)
	if pos > len(runes) {
		pos = len(runes)
	}
	// The word is the run of word characters ending at the cursor.
	// '.' and '/' join words so "t.col" and "./dir/file" complete whole.
	start := pos
	for start > 0 && isWordRune(runes[start-1]) {
		start--
	}
	head, word, tail := string(runes[:start]), string(runes[start:pos]), string(runes[pos:])

	trimmedHead := strings.TrimLeft(head, " \t")
	switch {
	case trimmedHead == "" && (strings.HasPrefix(word, ".") || strings.HasPrefix(word, `\`)):
		return head, matchPrefix(dotCommandNames(), word, false), tail
	case isFileArg(trimmedHead):
		return head, completePath(word), tail
	}
	return head, s.sqlCandidates(word), tail
}

func isWordRune(r rune) bool {
	return r == '_' || r == '.' || r == '/' || r == '\\' || r == '~' || r == '-' ||
		unicode.IsLetter(r) || unicode.IsDigit(r)
}

// isFileArg reports whether the text before the word is a dot command
// that takes a path, e.g. ".read " or ".import --csv ".
func isFileArg(head string) bool {
	fields := strings.Fields(head)
	return len(fields) > 0 && fileArgCommands[fields[0]] && strings.HasSuffix(head, " ")
}

// sqlCandidates offers keywords and catalog names. A word typed in
// upper case gets upper-case keywords, otherwise lower case — matching
// the user's own style. "t.c" completes column c of table t.
func (s *Shell) sqlCandidates(word string) []string {
	if word == "" {
		return nil
	}
	if i := strings.LastIndexByte(word, '.'); i > 0 && s.eng != nil {
		table, prefix := word[:i], word[i+1:]
		var out []string
		if desc := s.eng.Table(strings.ToLower(table)); desc != nil {
			for _, c := range desc.Columns {
				if strings.HasPrefix(strings.ToLower(c.Name), strings.ToLower(prefix)) {
					out = append(out, table+"."+c.Name)
				}
			}
		}
		return out
	}
	upper := strings.ToUpper(word) == word && strings.ToLower(word) != word
	var cands []string
	for _, k := range sqlKeywords {
		if upper {
			cands = append(cands, k)
		} else {
			cands = append(cands, strings.ToLower(k))
		}
	}
	if s.eng != nil {
		for _, t := range s.eng.Tables() {
			cands = append(cands, t)
			if desc := s.eng.Table(t); desc != nil {
				for _, c := range desc.Columns {
					cands = append(cands, c.Name)
				}
			}
		}
		for _, v := range s.eng.Views() {
			cands = append(cands, v.Name)
		}
	}
	return matchPrefix(cands, word, true)
}

// matchPrefix returns the sorted, de-duplicated candidates starting with
// prefix (case-insensitively when fold is set).
func matchPrefix(cands []string, prefix string, fold bool) []string {
	var out []string
	lp := strings.ToLower(prefix)
	for _, c := range cands {
		if strings.HasPrefix(c, prefix) || (fold && strings.HasPrefix(strings.ToLower(c), lp)) {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// completePath completes a file path, appending "/" to directories so
// Tab can keep descending. A leading ~ expands for the lookup but is
// kept in what the user sees.
func completePath(word string) []string {
	expanded := word
	home, _ := os.UserHomeDir()
	if strings.HasPrefix(word, "~") && home != "" {
		expanded = home + word[1:]
	}
	matches, _ := filepath.Glob(globEscape(expanded) + "*")
	var out []string
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.IsDir() {
			m += string(filepath.Separator)
		}
		if strings.HasPrefix(word, "~") && home != "" {
			m = "~" + strings.TrimPrefix(m, home)
		}
		out = append(out, m)
	}
	return out
}

// globEscape escapes glob metacharacters in a literal path prefix. On
// Windows '\' is the path separator, not filepath.Match's escape, so
// there the prefix passes through unescaped.
func globEscape(s string) string {
	if runtime.GOOS == "windows" {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`*?[\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
