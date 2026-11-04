package main

// shell.go: the read–execute–print loop. A Shell owns one open
// database (an Engine, its SQL DB and one Session) plus the settings
// dot commands change, and consumes input from a lineSource — the line
// editor for a person at a terminal, a plain reader for scripts, files
// (.read) and -c commands. Every source goes through the same loop, so
// a script behaves exactly like the same lines typed by hand.
//
// Input model (the sqlite/DuckDB/psql convention):
//
//	line at statement start beginning with '.' or '\'  → shell command
//	anything else                                     → SQL, accumulated
//	                                                     until a top-level ';'
//
// One Session serves the whole shell, so BEGIN ... COMMIT spans prompts
// the way it spans statements on one psql connection, and the prompt
// shows the transaction state (bytdb*> in a block, bytdb!> in a failed
// one).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"
	"github.com/rohanthewiz/serr"
	"golang.org/x/term"
)

// errInterrupt is what a lineSource returns when the user pressed
// Ctrl-C at the prompt: discard the statement being typed, not the
// shell.
var errInterrupt = errors.New("interrupted")

// lineSource yields input lines without their trailing newline. prompt
// is shown only by interactive sources.
type lineSource interface {
	readLine(prompt string) (string, error)
}

// Shell is one CLI session over one open database.
type Shell struct {
	// Database. path is "" for a transient database, whose files live
	// in tempDir and are removed when it closes.
	path    string
	tempDir string
	opts    []btypedb.Option // applied to every open (sync policy, key)
	eng     *bytdb.Engine
	db      *bsql.DB
	sess    *bsql.Session

	// Output. Results go to out, which .output/.once may point at a
	// file; status lines (command tags, timer, notices) stay on the
	// terminal so a redirected file holds only data.
	stdout  io.Writer
	stderr  io.Writer
	out     io.Writer
	outFile *os.File
	onceOut bool // outFile closes after the next statement's output

	// Settings, all changeable by dot commands.
	mode         string
	insertTable  string
	headers      bool
	nullValue    string
	nullExplicit bool // .nullvalue was set: csv/list honor it too
	colSep       string
	rowSep       string
	timer        bool
	echo         bool
	bail         bool
	changes      bool
	maxRows      int // -1 = auto (40 on a terminal, unlimited otherwise)
	maxWidth     int // 0 = auto (terminal width, unlimited off a terminal)
	prompt       string
	contPrompt   string
	prevMode     string // mode before \x switched to line, for toggling back

	// Session facts.
	interactive bool // a person is typing at a terminal
	tty         bool // stdout is a terminal
	editor      *lineEditor
	lastSQL     string // most recent SQL statement, for .edit
	failures    int    // statements and commands that failed
	quit        bool
	exitCode    int
	// aborted records that the user canceled a statement with Ctrl-C.
	// Non-interactive input (a script, .read, -c) stops there rather
	// than run on past the statement the user meant to stop; the
	// interactive prompt clears it and carries on.
	aborted bool
	depth   int // .read nesting, bounded to stop a file reading itself

	// mu guards cancel: the SIGINT handler runs on its own goroutine and
	// cancels the running statement, if there is one.
	mu     sync.Mutex
	cancel context.CancelFunc
}

// newShell returns a Shell with default settings, not yet connected.
func newShell(stdout, stderr io.Writer) *Shell {
	s := &Shell{
		stdout:     stdout,
		stderr:     stderr,
		out:        stdout,
		headers:    true,
		nullValue:  "NULL",
		colSep:     "|",
		rowSep:     "\n",
		maxRows:    -1,
		prompt:     "bytdb> ",
		contPrompt: "  ...> ",
	}
	s.setMode("duckbox")
	if f, ok := stdout.(*os.File); ok {
		s.tty = term.IsTerminal(int(f.Fd()))
	}
	return s
}

// setMode switches the output mode and, as sqlite does, resets the
// column separator to the mode's natural delimiter, so ".mode csv"
// after ".separator ;" is plain CSV again.
func (s *Shell) setMode(mode string) {
	s.mode = mode
	switch mode {
	case "csv":
		s.colSep = ","
	case "tabs":
		s.colSep = "\t"
	case "list":
		s.colSep = "|"
	}
}

// open connects the shell to the database at path ("" for a fresh
// transient database). The new database is opened before the current
// one closes, so a failed .open leaves the shell where it was.
func (s *Shell) open(path string) error {
	var (
		eng     *bytdb.Engine
		tempDir string
		err     error
	)
	if path == "" {
		// bytdb has no pure in-memory mode: every engine is backed by a
		// WAL file. A transient database is an ordinary one in a
		// private temp directory, with fsync off (nothing survives it
		// anyway), deleted on close — memory speed, no residue.
		tempDir, err = os.MkdirTemp("", "bytdb-transient-")
		if err != nil {
			return serr.Wrap(err, "creating transient database directory")
		}
		eng, err = bytdb.Open(filepath.Join(tempDir, "transient.bytdb"), append(s.opts, bytdb.WithSyncNever())...)
		if err != nil {
			os.RemoveAll(tempDir)
		}
	} else {
		if abs, aerr := filepath.Abs(path); aerr == nil {
			path = abs
		}
		if s.eng != nil && path == s.path {
			return nil // already open; reopening would only hit our own lock
		}
		eng, err = bytdb.Open(path, s.opts...)
	}
	if err != nil {
		if errors.Is(err, bytdb.ErrLocked) {
			return fmt.Errorf("%s is locked by another process; if bytdbd is serving it, connect with psql instead", path)
		}
		return err
	}
	s.closeDB()
	s.path, s.tempDir, s.eng = path, tempDir, eng
	s.db = bsql.New(eng)
	s.sess = s.db.NewSession()
	return nil
}

// closeDB closes the current database, rolling back any open
// transaction block (with a warning, since that discards work).
func (s *Shell) closeDB() {
	if s.eng == nil {
		return
	}
	if s.sess != nil {
		if s.sess.Status() != bsql.TxIdle {
			fmt.Fprintln(s.stderr, "WARNING: rolling back the open transaction")
		}
		s.sess.Close()
	}
	if err := s.eng.Close(); err != nil {
		s.errorf("closing database: %s", bytdb.ErrText(err))
	}
	if s.tempDir != "" {
		os.RemoveAll(s.tempDir)
	}
	s.eng, s.db, s.sess, s.path, s.tempDir = nil, nil, nil, "", ""
}

// close releases everything: output redirection, the database, and
// the line editor (saving history).
func (s *Shell) close() {
	s.setOutput("", false)
	s.closeDB()
	if s.editor != nil {
		s.editor.close()
		s.editor = nil
	}
}

// currentPrompt is the prompt for the next line: the continuation
// prompt mid-statement, otherwise the main prompt marked with the
// transaction state the way psql marks it (* in a block, ! failed).
func (s *Shell) currentPrompt(midStatement bool) string {
	if midStatement {
		return s.contPrompt
	}
	if s.sess == nil {
		return s.prompt
	}
	mark := ""
	switch s.sess.Status() {
	case bsql.TxActive:
		mark = "*"
	case bsql.TxFailed:
		mark = "!"
	}
	if mark == "" {
		return s.prompt
	}
	// Insert the mark before the prompt's trailing "> " (or append it
	// when the prompt has a custom shape).
	p := strings.TrimRight(s.prompt, " ")
	trail := s.prompt[len(p):]
	if strings.HasSuffix(p, ">") {
		return p[:len(p)-1] + mark + ">" + trail
	}
	return p + mark + trail
}

// process runs every line src yields until it is exhausted or the shell
// quits. With .bail on, the first failure quits the shell.
func (s *Shell) process(src lineSource) {
	var buf strings.Builder
	stop := func() bool { return s.quit || (s.aborted && !s.interactive) }
	for !stop() {
		if s.interactive {
			s.aborted = false
		}
		line, err := src.readLine(s.currentPrompt(buf.Len() > 0))
		if errors.Is(err, errInterrupt) {
			buf.Reset() // Ctrl-C abandons the statement being typed
			continue
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// Not a clean end of input (a vanished terminal, a read
				// failure): say so instead of exiting silently.
				s.errorf("reading input: %v", err)
			}
			// End of input. A script's last statement may lack its ';'
			// ("echo 'select 1' | bytdb"); run it rather than drop it.
			// At an interactive prompt, Ctrl-D means leave, and a
			// half-typed statement is discarded, as in psql.
			if !s.interactive && strings.TrimSpace(buf.String()) != "" {
				for _, st := range s.statements(buf.String(), true) {
					if stop() {
						break
					}
					if !s.execStatement(st) && s.bail {
						s.quit = true
					}
				}
			}
			if s.interactive {
				fmt.Fprintln(s.stdout)
			}
			return
		}
		if buf.Len() == 0 {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if trimmed[0] == '.' || trimmed[0] == '\\' {
				s.addHistory(trimmed)
				if !s.metaCommand(trimmed) && s.bail {
					s.quit = true
				}
				continue
			}
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
		sc := scanSQL(buf.String())
		if len(sc.stmts) == 0 {
			if !sc.restHasContent && !sc.open {
				buf.Reset() // only comments so far: back to the main prompt
			}
			continue
		}
		text := buf.String()
		s.addHistory(historyLine(text[:len(text)-len(sc.rest)]))
		buf.Reset()
		if sc.restHasContent || sc.open {
			buf.WriteString(sc.rest) // the start of the next statement
		}
		for _, st := range sc.stmts {
			if stop() {
				break
			}
			if !s.execStatement(st) && s.bail {
				s.quit = true
			}
		}
	}
}

// statements splits text into statements; final includes a trailing
// unterminated statement (end of input).
func (s *Shell) statements(text string, final bool) []string {
	sc := scanSQL(text)
	out := sc.stmts
	if final && (sc.restHasContent || sc.open) {
		out = append(out, strings.TrimSpace(sc.rest))
	}
	return out
}

// runText feeds a block of text through the normal input loop, as if
// typed: used for -c, -cmd and .edit.
func (s *Shell) runText(text string) {
	interactive := s.interactive
	s.interactive = false // no prompts, run a trailing statement at EOF
	defer func() { s.interactive = interactive }()
	s.process(newReaderSource(strings.NewReader(text)))
}

// addHistory records an entry in the line editor's history, if there
// is one.
func (s *Shell) addHistory(line string) {
	if s.editor != nil && s.interactive && line != "" {
		s.editor.addHistory(line)
	}
}

// Shorthands the shell answers itself. bytdb's SQL has no DESCRIBE or
// SHOW TABLES, but anyone coming from DuckDB or MySQL types them; both
// are syntax errors to the engine, so intercepting them shadows nothing.
var (
	reDescribe   = regexp.MustCompile(`(?is)^\s*(?:describe|desc)\s+((?:"(?:[^"]|"")+"|[^\s;"]+)(?:\.(?:"(?:[^"]|"")+"|[^\s;"]+))?)\s*$`)
	reShowTables = regexp.MustCompile(`(?is)^\s*show\s+tables\s*$`)
)

// execStatement runs one SQL statement and prints its outcome. It
// reports whether the statement succeeded.
func (s *Shell) execStatement(text string) bool {
	if s.echo {
		fmt.Fprintln(s.out, text+";")
	}
	if s.eng == nil {
		s.errorf("no database is open; use .open FILENAME")
		return false
	}
	s.lastSQL = text
	if m := reDescribe.FindStringSubmatch(text); m != nil {
		defer s.endOnce()
		return s.check(s.describe(m[1]))
	}
	if reShowTables.MatchString(text) {
		defer s.endOnce()
		return s.check(s.listTables(""))
	}

	start := time.Now()
	// Parse once up front, as pgwire does: the parsed statement names
	// its command tag (INSERT, CREATE TABLE, ...), which Exec's Result
	// does not carry.
	st, err := s.db.Prepare(text)
	var res *bsql.Result
	if err == nil {
		ctx, done := s.statementContext()
		res, err = s.sess.ExecStmtCtx(ctx, st)
		done()
	}
	elapsed := time.Since(start)
	if err != nil {
		s.sqlError(err, text)
		s.printTimer(elapsed)
		s.endOnce()
		return false
	}
	if res.Notice != "" {
		for _, line := range strings.Split(res.Notice, "\n") {
			fmt.Fprintf(s.stderr, "%s:  %s\n", noticeLevel(line), line)
		}
	}
	ok := true
	if len(res.Cols) > 0 {
		ok = s.check(s.renderResult(&resultSet{cols: res.Cols, types: res.Types, rows: res.Rows}))
	}
	if s.changes && showTag(st.Command(), res) {
		fmt.Fprintln(s.stdout, commandTag(st.Command(), res))
	}
	s.printTimer(elapsed)
	s.endOnce()
	return ok
}

// statementContext returns the context the next statement runs under,
// registered so a Ctrl-C (SIGINT) cancels it, and the function that
// unregisters it.
func (s *Shell) statementContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
	return ctx, func() {
		s.mu.Lock()
		s.cancel = nil
		s.mu.Unlock()
		cancel()
	}
}

// interrupt is the SIGINT handler's entry point. It cancels the running
// statement and reports whether there was one.
func (s *Shell) interrupt() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		return true
	}
	return false
}

// commandTag renders a statement's completion tag the way pgwire's
// CommandComplete does (and psql prints it): row counts for DML, the
// bare command word for everything else.
func commandTag(cmd string, res *bsql.Result) string {
	if res.Tag != "" {
		cmd = res.Tag
	}
	switch cmd {
	case "SELECT":
		return fmt.Sprintf("SELECT %d", len(res.Rows))
	case "INSERT":
		return fmt.Sprintf("INSERT 0 %d", res.RowsAffected)
	case "UPDATE", "DELETE":
		return fmt.Sprintf("%s %d", cmd, res.RowsAffected)
	}
	return cmd
}

// showTag decides whether .changes prints a statement's tag, following
// psql: every statement that returns no rows gets its tag (CREATE TABLE,
// INSERT 0 3, BEGIN), and of the row-returning ones only DML with
// RETURNING does — its row count is news; a SELECT's or EXPLAIN's is
// already on screen.
func showTag(cmd string, res *bsql.Result) bool {
	if res.Tag != "" {
		cmd = res.Tag
	}
	if cmd == "" {
		return false
	}
	if len(res.Cols) == 0 {
		return cmd != "SELECT"
	}
	return cmd == "INSERT" || cmd == "UPDATE" || cmd == "DELETE"
}

// noticeLevel classifies a statement notice with Postgres's severities
// (the same split pgwire's noticeBody makes): IF [NOT] EXISTS skips are
// NOTICEs, VACUUM's report is INFO, the rest are WARNINGs.
func noticeLevel(msg string) string {
	switch {
	case strings.HasSuffix(msg, ", skipping"):
		return "NOTICE"
	case strings.HasPrefix(msg, "compacted the storage log"):
		return "INFO"
	}
	return "WARNING"
}

// renderOpts snapshots the settings that shape a result's output.
// Fitting (row elision, width) applies only when the output is the
// terminal, so a piped or redirected result is always complete —
// unless .maxrows/.maxwidth were set explicitly.
func (s *Shell) renderOpts() renderOpts {
	o := renderOpts{
		mode:        s.mode,
		headers:     s.headers,
		null:        s.nullValue,
		colSep:      s.colSep,
		rowSep:      s.rowSep,
		insertTable: s.insertTable,
	}
	if s.nullExplicit {
		o.csvNull = s.nullValue
	}
	toTerminal := s.tty && s.out == s.stdout
	switch {
	case s.maxRows >= 0:
		o.maxRows = s.maxRows
	case toTerminal:
		o.maxRows = 40
	}
	switch {
	case s.maxWidth > 0:
		o.width = s.maxWidth
	case toTerminal:
		o.width = terminalWidth()
	}
	return o
}

// terminalWidth is stdout's width in cells, or 0 when unknown. Read on
// every render, so resizing the window takes effect on the next query.
func terminalWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	return 0
}

// renderResult writes a result set to the current output.
func (s *Shell) renderResult(rs *resultSet) error {
	return render(s.out, rs, s.renderOpts())
}

// printTimer reports a statement's wall time when .timer is on, in
// DuckDB's format.
func (s *Shell) printTimer(d time.Duration) {
	if s.timer {
		fmt.Fprintf(s.stdout, "Run Time (s): real %.3f\n", d.Seconds())
	}
}

// setOutput redirects results to path ("" restores stdout). once makes
// the redirect last for one statement (.once).
func (s *Shell) setOutput(path string, once bool) error {
	if s.outFile != nil {
		s.outFile.Close()
		s.outFile = nil
	}
	s.out, s.onceOut = s.stdout, false
	if path == "" || path == "stdout" {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	s.outFile, s.out, s.onceOut = f, f, once
	return nil
}

// endOnce closes a .once redirect after the statement it was for.
func (s *Shell) endOnce() {
	if s.onceOut {
		s.setOutput("", false)
	}
}

// check reports err (if any) and returns whether it was nil.
func (s *Shell) check(err error) bool {
	if err != nil {
		s.errorf("%s", bytdb.ErrText(err))
		return false
	}
	return true
}

// errorf prints a shell-level error and counts it.
func (s *Shell) errorf(format string, args ...any) {
	s.failures++
	fmt.Fprintf(s.stderr, "Error: "+format+"\n", args...)
}

// sqlError reports a failed statement. Cancellation gets Postgres's
// wording; a parse error carrying a byte position gets psql's
// LINE n: excerpt with a caret under the offending spot.
func (s *Shell) sqlError(err error, text string) {
	if errors.Is(err, context.Canceled) {
		s.aborted = true
		s.errorf("canceling statement due to user request")
		return
	}
	s.errorf("%s", bytdb.ErrText(err))
	if pos, ok := errorPos(err); ok && pos <= len(text) {
		fmt.Fprint(s.stderr, caretLine(text, pos))
	}
}

// errorPos extracts the "pos" attribute bytdb attaches to parse errors:
// a byte offset into the statement text.
func errorPos(err error) (int, bool) {
	flds := serr.SErrFromErr(err).UserFields()
	for i := 0; i+1 < len(flds); i += 2 {
		if flds[i] == "pos" {
			var p int
			if _, err := fmt.Sscan(flds[i+1], &p); err == nil && p >= 0 {
				return p, true
			}
		}
	}
	return 0, false
}

// caretLine renders the source line containing byte offset pos, with a
// caret under it:
//
//	LINE 2: WHERE id = = 1
//	                   ^
//
// The caret column counts runes, not bytes, so it lands right under
// non-ASCII text too.
func caretLine(text string, pos int) string {
	lineStart := strings.LastIndexByte(text[:pos], '\n') + 1
	lineEnd := strings.IndexByte(text[pos:], '\n')
	if lineEnd < 0 {
		lineEnd = len(text)
	} else {
		lineEnd += pos
	}
	lineNo := strings.Count(text[:pos], "\n") + 1
	prefix := fmt.Sprintf("LINE %d: ", lineNo)
	col := utf8.RuneCountInString(text[lineStart:pos])
	return prefix + text[lineStart:lineEnd] + "\n" +
		strings.Repeat(" ", len(prefix)+col) + "^\n"
}

// readerSource reads lines from an io.Reader: stdin in batch mode, a
// .read file, a -c command. bufio.Reader.ReadString has no line-length
// limit (bufio.Scanner stops at 64 KiB by default, and a dump's INSERT
// of a large jsonb document can be longer than that).
type readerSource struct {
	r *bufio.Reader
}

func newReaderSource(r io.Reader) *readerSource {
	return &readerSource{r: bufio.NewReader(r)}
}

func (rs *readerSource) readLine(string) (string, error) {
	line, err := rs.r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	// A final line without a newline is still a line; the next call
	// reports the EOF.
	line = strings.TrimSuffix(line, "\n")
	return strings.TrimSuffix(line, "\r"), nil
}
