package difftest

import (
	"fmt"
	"strings"
)

// Corpus file format
//
// A corpus file is plain SQL that both bytdb and Postgres accept, plus
// directive comments. A line starting with "-- @" is a directive; any
// other "--" line is an ordinary comment and is dropped. A statement
// ends at the first line whose text ends with ";".
//
// Section directives switch mode for every following statement:
//
//	-- @setup     statements must succeed on both sides; not compared
//	-- @queries   statements are run on both sides and compared (default)
//
// Entry directives apply to the next statement only:
//
//	-- @ordered            compare rows in order (default: as a multiset)
//	-- @notype             skip the column type-class comparison
//	-- @error [SQLSTATE]   both sides must fail; with a code, bytdb's must match
//	-- @todo D-xx: why     a known bytdb bug: the result is expected to
//	                       differ until the plan item lands, and the run
//	                       fails once it matches, so the line gets deleted
//	-- @bytdb: why         a deliberate, documented divergence (the
//	                       gotchas): same mechanics as @todo, but meant
//	                       to stay
//
// The two divergence annotations make the corpus the living list of
// every known difference from Postgres: each fix removes one line, and
// nothing can silently start or stop diverging.

// entry is one statement of a corpus file with its directives.
type entry struct {
	line    int // 1-based line the statement starts on
	sql     string
	setup   bool
	ordered bool
	noType  bool
	wantErr string // "", "any", or a SQLSTATE
	todo    string // plan item of a known bug, e.g. "D-23"
	bytdb   bool   // deliberate divergence
	reason  string
}

func (e entry) expectDiff() bool { return e.todo != "" || e.bytdb }

func (e entry) annotationKind() string {
	if e.bytdb {
		return "@bytdb"
	}
	return "@todo " + e.todo
}

// parseCorpus splits a corpus file into entries. It is deliberately
// line-based rather than a SQL lexer: the corpus is written for this
// harness, so a statement-terminating ";" inside a multi-line string
// literal is simply not written.
func parseCorpus(src string) ([]entry, error) {
	var (
		out     []entry
		setup   bool
		pending entry // directives waiting for their statement
		buf     []string
		start   int
	)
	for i, raw := range strings.Split(src, "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(raw)
		if len(buf) == 0 {
			// Between statements: blank lines, comments and directives.
			if line == "" {
				continue
			}
			if d, ok := strings.CutPrefix(line, "-- @"); ok {
				if err := applyDirective(&pending, &setup, d); err != nil {
					return nil, fmt.Errorf("line %d: %w", lineNo, err)
				}
				continue
			}
			if strings.HasPrefix(line, "--") {
				continue
			}
			start = lineNo
		}
		buf = append(buf, raw)
		if strings.HasSuffix(line, ";") {
			stmt := strings.TrimSpace(strings.Join(buf, "\n"))
			stmt = strings.TrimSpace(strings.TrimSuffix(stmt, ";"))
			e := pending
			e.line, e.sql, e.setup = start, stmt, setup
			if e.setup && (e.expectDiff() || e.wantErr != "") {
				return nil, fmt.Errorf("line %d: setup statements take no entry directives", start)
			}
			out = append(out, e)
			pending, buf = entry{}, nil
		}
	}
	if len(buf) > 0 {
		return nil, fmt.Errorf("line %d: statement not terminated with ';'", start)
	}
	return out, nil
}

func applyDirective(e *entry, setup *bool, d string) error {
	word, rest, _ := strings.Cut(d, " ")
	rest = strings.TrimSpace(rest)
	switch {
	case word == "setup":
		*setup = true
	case word == "queries":
		*setup = false
	case word == "ordered":
		e.ordered = true
	case word == "notype":
		e.noType = true
	case word == "error":
		e.wantErr = "any"
		if rest != "" {
			e.wantErr = rest
		}
	case word == "todo":
		item, why, ok := strings.Cut(rest, ":")
		if !ok || !strings.HasPrefix(item, "D-") {
			return fmt.Errorf("want '-- @todo D-xx: reason', got %q", d)
		}
		e.todo, e.reason = strings.TrimSpace(item), strings.TrimSpace(why)
	case word == "bytdb:":
		if rest == "" {
			return fmt.Errorf("@bytdb needs a reason")
		}
		e.bytdb, e.reason = true, rest
	default:
		return fmt.Errorf("unknown directive %q", d)
	}
	return nil
}
