package main

// dot.go: shell commands. A line that starts a statement with '.' is a
// dot command (the sqlite/DuckDB convention); one starting with '\' is a
// psql meta-command, translated to its dot equivalent, because bytdb
// speaks the Postgres dialect and its users' fingers know \dt and \d.
//
// Commands may be abbreviated to any unique prefix (".hea on"), as in
// sqlite. Arguments split on whitespace, with '...' or "..." quoting
// for arguments that contain spaces.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"

	"github.com/mattn/go-runewidth"
	"github.com/rohanthewiz/bytdb"
)

// dotCommand is one shell command. run receives the split arguments
// and the raw text after the command word (for .print and .shell,
// which take free text).
type dotCommand struct {
	names []string // canonical name first, then aliases
	usage string
	help  string
	run   func(s *Shell, args []string, raw string) error
}

// dotCommands is populated in init: several commands (.help) refer to
// the table itself, which a package-level initializer cannot do.
var dotCommands []*dotCommand

func init() {
	dotCommands = []*dotCommand{
		{names: []string{".backup"}, usage: "FILE", help: "write a consistent copy of the database to FILE",
			run: func(s *Shell, a []string, _ string) error {
				if len(a) != 1 {
					return errUsage
				}
				return s.eng.Backup(a[0])
			}},
		{names: []string{".bail"}, usage: "[on|off]", help: "stop at the first error",
			run: func(s *Shell, a []string, _ string) error { return setBool(&s.bail, a) }},
		{names: []string{".cd"}, usage: "DIR", help: "change the working directory",
			run: func(s *Shell, a []string, _ string) error {
				if len(a) != 1 {
					return errUsage
				}
				return os.Chdir(expandHome(a[0]))
			}},
		{names: []string{".changes"}, usage: "[on|off]", help: "print command tags (INSERT 0 3, CREATE TABLE, ...)",
			run: func(s *Shell, a []string, _ string) error { return setBool(&s.changes, a) }},
		{names: []string{".databases"}, help: "show the open database file",
			run: func(s *Shell, _ []string, _ string) error {
				fmt.Fprintln(s.out, "main: "+s.describeDB())
				return nil
			}},
		{names: []string{".describe"}, usage: "TABLE", help: "show a table's (or view's) columns",
			run: func(s *Shell, a []string, _ string) error {
				if len(a) != 1 {
					return errUsage
				}
				return s.describe(a[0])
			}},
		{names: []string{".dump"}, usage: "[TABLE ...]", help: "write the database (or TABLEs) as a SQL script",
			run: func(s *Shell, a []string, _ string) error { return writeDump(s.out, s.eng, a) }},
		{names: []string{".echo"}, usage: "[on|off]", help: "print each statement before running it",
			run: func(s *Shell, a []string, _ string) error { return setBool(&s.echo, a) }},
		{names: []string{".edit"}, help: "edit the last statement in $VISUAL/$EDITOR, then run it",
			run: func(s *Shell, _ []string, _ string) error { return s.edit() }},
		{names: []string{".exit", ".quit"}, usage: "[CODE]", help: "exit the shell (with status CODE)",
			run: func(s *Shell, a []string, _ string) error {
				if len(a) > 0 {
					code, err := strconv.Atoi(a[0])
					if err != nil {
						return errUsage
					}
					s.exitCode = code
				}
				s.quit = true
				return nil
			}},
		{names: []string{".headers", ".header"}, usage: "[on|off]", help: "show column names",
			run: func(s *Shell, a []string, _ string) error { return setBool(&s.headers, a) }},
		{names: []string{".help"}, usage: "[PATTERN]", help: "show this help (commands matching PATTERN)",
			run: func(s *Shell, a []string, _ string) error { s.help(strings.Join(a, " ")); return nil }},
		{names: []string{".import"}, usage: "FILE TABLE [--csv|--tsv] [--header|--no-header] [--skip N] [--pk COLS]",
			help: "load CSV/TSV rows into TABLE (created from the header row if absent)",
			run:  func(s *Shell, a []string, _ string) error { return s.importFile(a) }},
		{names: []string{".indexes", ".indices"}, usage: "[TABLE]", help: "list primary keys and indexes",
			run: func(s *Shell, a []string, _ string) error {
				if len(a) > 1 {
					return errUsage
				}
				t := ""
				if len(a) == 1 {
					t = unquoteIdent(a[0])
				}
				rs, err := indexesResult(s.eng, t)
				if err != nil {
					return err
				}
				return s.renderResult(rs)
			}},
		{names: []string{".maxrows"}, usage: "N|auto", help: "rows duckbox shows before eliding the middle (0 = all)",
			run: func(s *Shell, a []string, _ string) error { return setLimit(&s.maxRows, a, -1) }},
		{names: []string{".maxwidth"}, usage: "N|auto", help: "width duckbox fits itself to (auto = terminal)",
			run: func(s *Shell, a []string, _ string) error { return setLimit(&s.maxWidth, a, 0) }},
		{names: []string{".mode"}, usage: "[MODE [TABLE]]", help: "set the output mode (no MODE: list modes)",
			run: func(s *Shell, a []string, _ string) error { return s.modeCmd(a) }},
		{names: []string{".nullvalue"}, usage: "TEXT", help: "text shown for NULL",
			run: func(s *Shell, a []string, _ string) error {
				if len(a) != 1 {
					return errUsage
				}
				s.nullValue, s.nullExplicit = a[0], true
				return nil
			}},
		{names: []string{".once"}, usage: "FILE", help: "send the next result to FILE",
			run: func(s *Shell, a []string, _ string) error {
				if len(a) != 1 {
					return errUsage
				}
				return s.setOutput(expandHome(a[0]), true)
			}},
		{names: []string{".open"}, usage: "[FILE]", help: "close this database and open FILE (none: transient)",
			run: func(s *Shell, a []string, _ string) error {
				if len(a) > 1 {
					return errUsage
				}
				path := ""
				if len(a) == 1 {
					path = expandHome(a[0])
				}
				return s.open(path)
			}},
		{names: []string{".output"}, usage: "[FILE]", help: "send results to FILE (none: back to stdout)",
			run: func(s *Shell, a []string, _ string) error {
				if len(a) > 1 {
					return errUsage
				}
				path := ""
				if len(a) == 1 {
					path = expandHome(a[0])
				}
				return s.setOutput(path, false)
			}},
		{names: []string{".print"}, usage: "TEXT...", help: "print TEXT",
			run: func(s *Shell, a []string, _ string) error {
				fmt.Fprintln(s.out, strings.Join(a, " "))
				return nil
			}},
		{names: []string{".prompt"}, usage: "MAIN [CONTINUE]", help: "set the prompts",
			run: func(s *Shell, a []string, _ string) error {
				if len(a) < 1 || len(a) > 2 {
					return errUsage
				}
				s.prompt = a[0]
				if len(a) == 2 {
					s.contPrompt = a[1]
				}
				return nil
			}},
		{names: []string{".read"}, usage: "FILE", help: "run the SQL and commands in FILE",
			run: func(s *Shell, a []string, _ string) error {
				if len(a) != 1 {
					return errUsage
				}
				return s.readFile(expandHome(a[0]))
			}},
		{names: []string{".schema"}, usage: "[PATTERN]", help: "show CREATE statements (LIKE PATTERN)",
			run: func(s *Shell, a []string, _ string) error {
				if len(a) > 1 {
					return errUsage
				}
				p := ""
				if len(a) == 1 {
					p = a[0]
				}
				return writeSchema(s.out, s.eng, p)
			}},
		{names: []string{".separator"}, usage: "COL [ROW]", help: "list/csv separators (\\t, \\n escapes allowed)",
			run: func(s *Shell, a []string, _ string) error {
				if len(a) < 1 || len(a) > 2 {
					return errUsage
				}
				s.colSep = unescape(a[0])
				if len(a) == 2 {
					s.rowSep = unescape(a[1])
				}
				return nil
			}},
		{names: []string{".shell", ".system"}, usage: "CMD...", help: "run CMD in the system shell",
			run: func(s *Shell, _ []string, raw string) error { return s.shellCmd(raw) }},
		{names: []string{".show"}, help: "show current settings",
			run: func(s *Shell, _ []string, _ string) error { s.show(); return nil }},
		{names: []string{".stats"}, help: "show engine statistics (size, log, memory)",
			run: func(s *Shell, _ []string, _ string) error { return s.renderResult(statsResult(s.eng.Stats())) }},
		{names: []string{".tables"}, usage: "[PATTERN]", help: "list tables and views (LIKE PATTERN)",
			run: func(s *Shell, a []string, _ string) error {
				if len(a) > 1 {
					return errUsage
				}
				p := ""
				if len(a) == 1 {
					p = a[0]
				}
				return s.listTables(p)
			}},
		{names: []string{".timer"}, usage: "[on|off]", help: "time each statement",
			run: func(s *Shell, a []string, _ string) error { return setBool(&s.timer, a) }},
		{names: []string{".version"}, help: "show version information",
			run: func(s *Shell, _ []string, _ string) error {
				fmt.Fprintln(s.out, versionString())
				return nil
			}},
	}
}

// errUsage makes metaCommand print the command's usage line.
var errUsage = errors.New("usage")

// dotCommandNames lists every command spelling, for tab completion.
func dotCommandNames() []string {
	var out []string
	for _, c := range dotCommands {
		out = append(out, c.names...)
	}
	return out
}

// lookupCommand resolves a command word: an exact name or alias, else a
// unique prefix of one. Ambiguous prefixes list the candidates.
func lookupCommand(word string) (*dotCommand, error) {
	var hits []*dotCommand
	for _, c := range dotCommands {
		for _, n := range c.names {
			if n == word {
				return c, nil
			}
		}
	}
	var names []string
	for _, c := range dotCommands {
		for _, n := range c.names {
			if strings.HasPrefix(n, word) && len(word) > 1 {
				if len(hits) == 0 || hits[len(hits)-1] != c {
					hits = append(hits, c)
					names = append(names, n)
				}
				break
			}
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return nil, fmt.Errorf("unknown command %s; enter \".help\" for a list", word)
	}
	return nil, fmt.Errorf("ambiguous command %s: %s", word, strings.Join(names, ", "))
}

// metaCommand runs one dot (or psql backslash) command line and reports
// whether it succeeded.
func (s *Shell) metaCommand(line string) bool {
	if line[0] == '\\' {
		translated, err := translatePsql(s, line)
		if err != nil {
			return s.check(err)
		}
		if translated == "" {
			return true // fully handled (a toggle)
		}
		line = translated
	}
	word, raw, _ := strings.Cut(line, " ")
	raw = strings.TrimSpace(raw)
	cmd, err := lookupCommand(word)
	if err != nil {
		return s.check(err)
	}
	if s.eng == nil && needsDB(cmd) {
		return s.check(errors.New("no database is open; use .open FILENAME"))
	}
	args, err := splitArgs(raw)
	if err != nil {
		return s.check(err)
	}
	err = cmd.run(s, args, raw)
	// .once covers the next output-producing command, dot command or
	// SQL alike (as in sqlite); only the redirecting commands themselves
	// leave it armed.
	if n := cmd.names[0]; n != ".once" && n != ".output" {
		s.endOnce()
	}
	if err != nil {
		if errors.Is(err, errUsage) {
			return s.check(fmt.Errorf("usage: %s %s", cmd.names[0], cmd.usage))
		}
		return s.check(err)
	}
	return true
}

// needsDB reports whether a command touches the database (and so must
// refuse politely, rather than crash, when none is open).
func needsDB(c *dotCommand) bool {
	switch c.names[0] {
	case ".backup", ".describe", ".dump", ".import", ".indexes", ".schema", ".stats", ".tables":
		return true
	}
	return false
}

// translatePsql maps a psql meta-command onto its dot command. It
// returns "" when it handled the command itself (the \x and \timing
// toggles, which have no one-word dot spelling).
func translatePsql(s *Shell, line string) (string, error) {
	word, rest, _ := strings.Cut(line, " ")
	rest = strings.TrimSpace(rest)
	withArg := func(dot string) string {
		if rest == "" {
			return dot
		}
		return dot + " " + rest
	}
	switch strings.TrimSuffix(word, "+") { // \d+ and \dt+ read like \d, \dt
	case `\q`:
		return ".quit", nil
	case `\?`, `\h`:
		return ".help", nil
	case `\d`:
		if rest == "" {
			return ".tables", nil
		}
		return ".describe " + rest, nil
	case `\dt`, `\dv`:
		return withArg(".tables"), nil
	case `\di`:
		return withArg(".indexes"), nil
	case `\i`, `\ir`:
		return withArg(".read"), nil
	case `\o`:
		return withArg(".output"), nil
	case `\!`:
		return withArg(".shell"), nil
	case `\e`:
		return ".edit", nil
	case `\l`, `\conninfo`:
		return ".databases", nil
	case `\echo`:
		return withArg(".print"), nil
	case `\timing`:
		return withArg(".timer"), nil
	case `\x`:
		// Expanded display: psql's \x is line mode here. Toggling back
		// restores whatever mode was active before.
		on := s.mode != "line"
		if rest != "" {
			v, err := parseBool(rest)
			if err != nil {
				return "", err
			}
			on = v
		}
		switch {
		case on && s.mode != "line":
			s.prevMode = s.mode
			s.setMode("line")
		case !on && s.mode == "line":
			prev := s.prevMode
			if prev == "" {
				prev = "duckbox"
			}
			s.setMode(prev)
		}
		state := "off"
		if on {
			state = "on"
		}
		fmt.Fprintln(s.stdout, "Expanded display is "+state+".")
		return "", nil
	}
	return "", fmt.Errorf("unsupported psql command %s; enter \".help\" for the shell's commands", word)
}

// splitArgs splits a command's argument text on whitespace, honoring
// '...' and "..." quoting (a doubled quote inside is a literal quote).
func splitArgs(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inArg := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' || c == '"':
			inArg = true
			q := c
			i++
			for ; i < len(s); i++ {
				if s[i] == q {
					if i+1 < len(s) && s[i+1] == q {
						cur.WriteByte(q)
						i++
						continue
					}
					break
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, fmt.Errorf("unterminated %c quote in command arguments", q)
			}
		case c == ' ' || c == '\t':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			inArg = true
			cur.WriteByte(c)
		}
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args, nil
}

// parseBool reads the on/off spellings sqlite and psql accept.
func parseBool(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("expected on or off, got %q", v)
}

// setBool applies an on|off argument; with none it toggles and says so.
func setBool(p *bool, args []string) error {
	switch len(args) {
	case 0:
		*p = !*p
		return nil
	case 1:
		v, err := parseBool(args[0])
		if err != nil {
			return err
		}
		*p = v
		return nil
	}
	return errUsage
}

// setLimit applies an N|auto argument; auto stores autoVal.
func setLimit(p *int, args []string, autoVal int) error {
	if len(args) != 1 {
		return errUsage
	}
	if strings.EqualFold(args[0], "auto") {
		*p = autoVal
		return nil
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 0 {
		return fmt.Errorf("expected a non-negative number or auto, got %q", args[0])
	}
	*p = n
	return nil
}

// unescape interprets C-style escapes (\t, \n, \\) in a separator, so
// ".separator \t" means a tab, not a backslash and a t.
func unescape(v string) string {
	if u, err := strconv.Unquote(`"` + strings.ReplaceAll(v, `"`, `\"`) + `"`); err == nil {
		return u
	}
	return v
}

// expandHome expands a leading ~/ in a path argument.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

// unquoteIdent reads a table-name argument as SQL would: "Quoted" keeps
// its case (with "" unescaped), a bare name folds to lower case, and a
// public. schema prefix is dropped (bytdb has one schema).
func unquoteIdent(name string) string {
	if len(name) > len("public.") && strings.EqualFold(name[:len("public.")], "public.") {
		name = name[len("public."):]
	}
	if len(name) >= 2 && name[0] == '"' && name[len(name)-1] == '"' {
		return strings.ReplaceAll(name[1:len(name)-1], `""`, `"`)
	}
	return strings.ToLower(name)
}

// help prints the command list, optionally filtered.
func (s *Shell) help(pattern string) {
	var b strings.Builder
	width := 0
	for _, c := range dotCommands {
		width = max(width, runewidth.StringWidth(c.names[0]+" "+c.usage))
	}
	for _, c := range dotCommands {
		head := strings.TrimSpace(c.names[0] + " " + c.usage)
		if pattern != "" && !strings.Contains(head+" "+c.help, pattern) {
			continue
		}
		line := pad(head, width, 'l') + "  " + c.help
		if len(c.names) > 1 {
			line += " (also " + strings.Join(c.names[1:], ", ") + ")"
		}
		b.WriteString(line + "\n")
	}
	if pattern == "" {
		b.WriteString("\nSQL statements end with ';'. DESCRIBE t and SHOW TABLES also work.\n")
		b.WriteString("psql meta-commands map to these: \\q \\d [t] \\dt \\di \\x \\timing \\i \\o \\! \\e \\l \\echo\n")
		b.WriteString("Ctrl-C cancels a running statement or the line being typed; Ctrl-D exits.\n")
	}
	fmt.Fprint(s.out, b.String())
}

// modeCmd implements .mode.
func (s *Shell) modeCmd(args []string) error {
	if len(args) == 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "current output mode: %s\n", s.mode)
		for _, m := range modes {
			fmt.Fprintf(&b, "  %-10s %s\n", m.name, m.desc)
		}
		fmt.Fprint(s.out, b.String())
		return nil
	}
	if len(args) > 2 {
		return errUsage
	}
	m, ok := canonicalMode(args[0])
	if !ok {
		return fmt.Errorf("unknown mode %q; enter \".mode\" for the list", args[0])
	}
	s.setMode(m)
	if m == "insert" {
		s.insertTable = "tbl"
		if len(args) == 2 {
			s.insertTable = args[1]
		}
	}
	return nil
}

// show prints the current settings, one per line.
func (s *Shell) show() {
	onOff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	limit := func(n int, auto int) string {
		if n == auto {
			return "auto"
		}
		return strconv.Itoa(n)
	}
	out := s.out
	fmt.Fprintf(out, "%10s: %s\n", "database", s.describeDB())
	fmt.Fprintf(out, "%10s: %s\n", "mode", s.mode)
	fmt.Fprintf(out, "%10s: %s\n", "headers", onOff(s.headers))
	fmt.Fprintf(out, "%10s: %q\n", "nullvalue", s.nullValue)
	fmt.Fprintf(out, "%10s: colsep %q rowsep %q\n", "separator", s.colSep, s.rowSep)
	fmt.Fprintf(out, "%10s: %s\n", "maxrows", limit(s.maxRows, -1))
	fmt.Fprintf(out, "%10s: %s\n", "maxwidth", limit(s.maxWidth, 0))
	fmt.Fprintf(out, "%10s: %s\n", "timer", onOff(s.timer))
	fmt.Fprintf(out, "%10s: %s\n", "changes", onOff(s.changes))
	fmt.Fprintf(out, "%10s: %s\n", "echo", onOff(s.echo))
	fmt.Fprintf(out, "%10s: %s\n", "bail", onOff(s.bail))
	output := "stdout"
	if s.outFile != nil {
		output = s.outFile.Name()
	}
	fmt.Fprintf(out, "%10s: %s\n", "output", output)
}

// describeDB names the open database for .databases and .show.
func (s *Shell) describeDB() string {
	switch {
	case s.eng == nil:
		return "(none)"
	case s.path == "":
		return "(transient in-memory database)"
	}
	return s.path
}

// listTables prints table and view names matching pattern, laid out in
// columns on a terminal (like ls) and one per line otherwise, so the
// output stays easy to consume from a script.
func (s *Shell) listTables(pattern string) error {
	match := likePattern(pattern)
	var names []string
	for _, t := range s.eng.Tables() {
		if match(t) {
			names = append(names, t)
		}
	}
	for _, v := range s.eng.Views() {
		if match(v.Name) {
			names = append(names, v.Name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil
	}
	if !s.tty || s.out != s.stdout {
		fmt.Fprintln(s.out, strings.Join(names, "\n"))
		return nil
	}
	colW := 0
	for _, n := range names {
		colW = max(colW, runewidth.StringWidth(n))
	}
	colW += 2
	width := terminalWidth()
	if width <= 0 {
		width = 80
	}
	cols := max(width/colW, 1)
	rows := (len(names) + cols - 1) / cols
	var b strings.Builder
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			i := c*rows + r // column-major, as ls orders
			if i >= len(names) {
				break
			}
			if c == cols-1 || i+rows >= len(names) {
				b.WriteString(names[i])
			} else {
				b.WriteString(pad(names[i], colW, 'l'))
			}
		}
		b.WriteByte('\n')
	}
	fmt.Fprint(s.out, b.String())
	return nil
}

// describe prints a table's columns. A view (or anything else SELECT can
// read) is described from a zero-row query, which yields the column
// names and types without touching data.
func (s *Shell) describe(name string) error {
	name = unquoteIdent(name)
	if desc := s.eng.Table(name); desc != nil {
		return s.renderResult(describeTable(desc))
	}
	res, err := s.sess.Exec("SELECT * FROM " + quoteIdent(name) + " LIMIT 0")
	if err != nil {
		return fmt.Errorf("no such table or view: %s", name)
	}
	rs := &resultSet{
		cols:  []string{"column_name", "column_type", "null", "key", "default", "extra"},
		types: []bytdb.ColType{bytdb.TString, bytdb.TString, bytdb.TString, bytdb.TString, bytdb.TString, bytdb.TString},
	}
	for i, c := range res.Cols {
		var t bytdb.ColType
		if i < len(res.Types) {
			t = res.Types[i]
		}
		rs.rows = append(rs.rows, []any{c, typeName(t), "YES", nil, nil, nil})
	}
	return s.renderResult(rs)
}

// readFile runs a script file through the input loop. Nesting is
// bounded so a file that reads itself fails instead of recursing until
// the stack overflows.
func (s *Shell) readFile(path string) error {
	const maxDepth = 16
	if s.depth >= maxDepth {
		return fmt.Errorf(".read nested more than %d deep", maxDepth)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	s.depth++
	interactive := s.interactive
	s.interactive = false
	defer func() { s.depth--; s.interactive = interactive }()
	s.process(newReaderSource(f))
	return nil
}

// shellCmd runs a command line in the system shell with the terminal
// handed over in its normal mode.
func (s *Shell) shellCmd(line string) error {
	if strings.TrimSpace(line) == "" {
		return errUsage
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", line)
	} else {
		sh := os.Getenv("SHELL")
		if sh == "" {
			sh = "/bin/sh"
		}
		cmd = exec.Command(sh, "-c", line)
	}
	return s.runChild(cmd)
}

// runChild runs cmd attached to the shell's terminal.
func (s *Shell) runChild(cmd *exec.Cmd) error {
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if s.out != s.stdout {
		cmd.Stdout = s.out // .output applies to command output too
	}
	resume := func() {}
	if s.editor != nil {
		resume = s.editor.suspend()
	}
	defer resume()
	return cmd.Run()
}

// edit opens the last statement in the user's editor and runs whatever
// is saved. The editor command goes through the shell, so EDITOR values
// with arguments ("code --wait") work.
func (s *Shell) edit() error {
	f, err := os.CreateTemp("", "bytdb-edit-*.sql")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	if s.lastSQL != "" {
		f.WriteString(s.lastSQL + ";\n")
	}
	f.Close()
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
		if runtime.GOOS == "windows" {
			editor = "notepad"
		}
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", editor+` "`+path+`"`)
	} else {
		// "$0" is the path, passed as an argument rather than spliced
		// into the command text, so no quoting can go wrong.
		cmd = exec.Command("/bin/sh", "-c", editor+` "$0"`, path)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	resume := func() {}
	if s.editor != nil {
		resume = s.editor.suspend()
	}
	err = cmd.Run()
	resume()
	if err != nil {
		return fmt.Errorf("editor %q failed: %w", editor, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return nil
	}
	fmt.Fprintln(s.stdout, text)
	s.addHistory(historyLine(text))
	s.runText(text)
	return nil
}

// statsResult renders Engine.Stats as a two-column result.
func statsResult(st bytdb.Stats) *resultSet {
	rs := &resultSet{cols: []string{"stat", "value"}, types: []bytdb.ColType{bytdb.TString, bytdb.TString}}
	add := func(k, v string) { rs.rows = append(rs.rows, []any{k, v}) }
	add("tables", strconv.Itoa(st.Tables))
	add("kv entries", strconv.Itoa(st.Keys))
	add("log epoch", strconv.FormatUint(st.LogEpoch, 10))
	add("log size", humanBytes(uint64(max(st.LogBytes, 0))))
	add("log growth since compaction", humanBytes(uint64(max(st.LogBytes-st.LogBaseBytes, 0))))
	add("heap live", humanBytes(st.HeapLive))
	add("heap marked", humanBytes(st.HeapMarked))
	add("runtime total", humanBytes(st.RuntimeTotal))
	if st.MemLimit > 0 {
		add("memory limit", humanBytes(st.MemLimit))
	} else {
		add("memory limit", "none (GOMEMLIMIT unset)")
	}
	if st.MemCeiling > 0 {
		add("memory ceiling", humanBytes(st.MemCeiling))
	} else {
		add("memory ceiling", "inactive")
	}
	add("gc cycles", strconv.FormatUint(st.GCCycles, 10))
	add("goroutines", strconv.Itoa(st.Goroutines))
	return rs
}

// humanBytes renders a byte count in binary units.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// versionString reports the bytdb engine version compiled in, read from
// the build info so it is always the real dependency version. A
// workspace build (go.work) reports the working tree as "(devel)".
// pgwire's own go.mod has no replace (N-031), so the "(local build)"
// mark only appears when a module that builds the shell replaces bytdb
// itself.
func versionString() string {
	v := "(devel)"
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Path == "github.com/rohanthewiz/bytdb" {
				v = d.Version
				if d.Replace != nil {
					v += " (local build)" // a replace directive: the working tree, not the tag
				}
			}
		}
	}
	return "bytdb " + v + " " + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH
}
