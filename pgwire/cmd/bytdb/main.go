// Command bytdb is an interactive SQL shell for bytdb databases, in the
// mould of the DuckDB and sqlite3 command-line shells:
//
//	bytdb                      transient in-memory database
//	bytdb app.bytdb            open (or create) a database file
//	bytdb app.bytdb "SELECT 1" run SQL and exit
//	bytdb -csv app.bytdb -c "SELECT * FROM users" > users.csv
//	bytdb app.bytdb < script.sql
//
// At the prompt, SQL statements end with ';' and may span lines;
// commands start with '.' (".help" lists them) — or with '\' for the
// psql meta-commands people already know (\dt, \d t, \x, \timing).
// The shell keeps history in ~/.bytdb_history, runs ~/.bytdbrc at
// interactive startup, and cancels a running statement on Ctrl-C.
//
// The shell opens the database file directly, embedded in its own
// process. A file that bytdbd is serving is locked by the server; reach
// that one over the wire with psql instead.
package main

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
	"golang.org/x/term"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is main with its environment passed in, so tests can drive the
// whole command — flags, batch input, exit status — without a process.
func run(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bytdb", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		cmds, preCmds []string
		mode          string
		header        = -1 // -1 unset, else 0/1
		nullValue     *string
		separator     *string
	)
	fs.Func("c", "run `COMMAND` (SQL or a dot command) and exit; repeatable", func(v string) error {
		cmds = append(cmds, v)
		return nil
	})
	fs.Func("s", "same as -c", func(v string) error { cmds = append(cmds, v); return nil })
	fs.Func("cmd", "run `COMMAND` before reading input; repeatable", func(v string) error {
		preCmds = append(preCmds, v)
		return nil
	})
	initFile := fs.String("init", "", "read startup commands from `FILE` (default: ~/.bytdbrc, interactive only)")
	bail := fs.Bool("bail", false, "stop at the first error")
	echo := fs.Bool("echo", false, "print each statement before running it")
	batch := fs.Bool("batch", false, "force batch mode: no prompts, no line editing")
	interactiveFlag := fs.Bool("interactive", false, "force interactive mode")
	fs.BoolFunc("header", "show column names", func(string) error { header = 1; return nil })
	fs.BoolFunc("noheader", "hide column names", func(string) error { header = 0; return nil })
	fs.Func("nullvalue", "show NULL as `TEXT`", func(v string) error { nullValue = &v; return nil })
	fs.Func("separator", "column separator for list and csv modes (`SEP`)", func(v string) error {
		separator = &v
		return nil
	})
	for _, m := range modes {
		name := m.name
		fs.BoolFunc(name, "output mode: "+m.desc, func(string) error { mode = name; return nil })
	}
	syncMode := fs.String("sync", "always",
		"WAL fsync policy: always (durable through power loss) or never (faster)")
	encKeyFile := fs.String("encryption-key-file", "",
		"open an encrypted database with the AES-256 key in this `FILE` (32 raw bytes, 64 hex chars, or base64)")
	encKeyEnv := fs.String("encryption-key-env", "",
		"open an encrypted database with the AES-256 key in this environment `VAR` (hex or base64)")
	version := fs.Bool("version", false, "print version and exit")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: bytdb [OPTIONS] [FILENAME [COMMAND ...]]")
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "FILENAME is a bytdb database file, created if absent. Without one (or with")
		fmt.Fprintln(stderr, ":memory:) the shell opens a transient in-memory database. COMMANDs after")
		fmt.Fprintln(stderr, "FILENAME run in order, as with -c, and the shell exits.")
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "Options:")
		fs.PrintDefaults()
	}

	// Go's flag package stops at the first positional argument, but
	// DuckDB-style invocations put options after the file name
	// ("bytdb app.db -csv -c ..."). Parse repeatedly, peeling one
	// positional argument off between rounds. Everything after a "--"
	// is positional, flags or not.
	var pos, tail []string
	if i := indexOf(argv, "--"); i >= 0 {
		argv, tail = argv[:i], argv[i+1:]
	}
	for rest := argv; ; {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		pos, rest = append(pos, rest[0]), rest[1:]
	}
	pos = append(pos, tail...)

	if *version {
		fmt.Fprintln(stdout, versionString())
		return 0
	}

	var opts []btypedb.Option
	switch *syncMode {
	case "always":
	case "never":
		opts = append(opts, bytdb.WithSyncNever())
	default:
		fmt.Fprintf(stderr, "Error: -sync must be always or never, got %q\n", *syncMode)
		return 2
	}
	key, err := loadEncryptionKey(*encKeyFile, *encKeyEnv)
	if err != nil {
		fmt.Fprintln(stderr, "Error: "+bytdb.ErrText(err))
		return 2
	}
	if key != nil {
		opts = append(opts, bytdb.WithEncryptionKey(key))
	}

	path := ""
	if len(pos) > 0 {
		path = pos[0]
		cmds = append(cmds, pos[1:]...)
	}
	if path == ":memory:" {
		path = "" // DuckDB's and sqlite's spelling of "transient"
	}

	// Interactive means a person at a terminal on both ends, and nothing
	// to run non-interactively; -batch and -interactive override.
	interactive := isTerminal(stdin) && isTerminal(stdout) && len(cmds) == 0
	if *batch {
		interactive = false
	}
	if *interactiveFlag {
		interactive = true
	}

	s := newShell(stdout, stderr)
	s.opts = opts
	s.interactive = interactive
	s.changes = interactive // command tags help a person, clutter a script
	s.bail, s.echo = *bail, *echo
	if len(cmds) > 0 {
		// -c commands usually depend on each other (create, then
		// insert, then select); stop at the first failure.
		s.bail = true
	}
	if mode != "" {
		s.setMode(mode)
	}
	if header >= 0 {
		s.headers = header == 1
	}
	if nullValue != nil {
		s.nullValue, s.nullExplicit = *nullValue, true
	}
	if separator != nil {
		s.colSep = unescape(*separator)
	}

	if err := s.open(path); err != nil {
		fmt.Fprintln(stderr, "Error: "+bytdb.ErrText(err))
		return 1
	}
	defer s.close()

	// Ctrl-C: cancel the running statement. With nothing running, an
	// interactive shell ignores it (liner handles it at the prompt) and
	// a batch run exits, as an uncaught SIGINT would. Exiting without
	// closing the engine is safe: every commit was already fsynced, and
	// the file lock is an flock the OS releases with the process.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer func() {
		// Stop guarantees no further sends, so closing is safe and ends
		// the handler goroutine (tests call run many times).
		signal.Stop(sig)
		close(sig)
	}()
	go func() {
		for range sig {
			if !s.interrupt() && !interactive {
				os.Exit(130)
			}
		}
	}()

	switch {
	case *initFile != "":
		if err := s.readFile(expandHome(*initFile)); err != nil {
			fmt.Fprintln(stderr, "Error: "+bytdb.ErrText(err))
			return 1
		}
	case interactive:
		if rc := expandHome("~/.bytdbrc"); fileExists(rc) {
			s.check(s.readFile(rc))
		}
	}
	for _, c := range preCmds {
		s.runText(c)
	}

	switch {
	case len(cmds) > 0:
		for _, c := range cmds {
			if s.quit || s.aborted {
				break
			}
			s.runText(c)
		}
	case interactive:
		fmt.Fprintln(stdout, versionString())
		fmt.Fprintln(stdout, `Enter ".help" for usage hints.`)
		if path == "" {
			fmt.Fprintln(stdout, "Connected to a transient in-memory database.")
			fmt.Fprintln(stdout, `Use ".open FILENAME" to reopen on a persistent database.`)
		}
		s.editor = newLineEditor(defaultHistoryPath(), s.complete)
		s.process(s.editor)
	default:
		s.process(newReaderSource(stdin))
	}

	switch {
	case s.exitCode != 0:
		return s.exitCode
	case s.aborted && !interactive:
		return 130
	case s.failures > 0 && !interactive:
		return 1
	}
	return 0
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}

// isTerminal reports whether f is a terminal; non-file readers and
// writers (tests, pipes) are not.
func isTerminal(f any) bool {
	if file, ok := f.(*os.File); ok {
		return term.IsTerminal(int(file.Fd()))
	}
	return false
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// loadEncryptionKey sources the 32-byte WAL encryption key from a file
// or an environment variable (at most one), exactly as bytdbd does, so
// one key file opens a database under either command. Returns nil, nil
// when neither is set. The key never comes from argv, where it would
// leak through ps output and shell history.
func loadEncryptionKey(file, env string) ([]byte, error) {
	var raw []byte
	switch {
	case file != "" && env != "":
		return nil, serr.New("set only one of -encryption-key-file / -encryption-key-env")
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, serr.Wrap(err, "reading encryption key file")
		}
		raw = b
	case env != "":
		v, ok := os.LookupEnv(env)
		if !ok {
			return nil, serr.New("encryption key env var is not set", "env", env)
		}
		raw = []byte(v)
	default:
		return nil, nil
	}
	return decodeEncryptionKey(raw)
}

// decodeEncryptionKey interprets raw key material: exactly 32 bytes is
// used verbatim; otherwise the trimmed text must decode (hex, 64 chars,
// or base64) to 32 bytes. Duplicated from bytdbd rather than shared,
// because the two commands are separate main packages and the logic is
// a dozen lines.
func decodeEncryptionKey(raw []byte) ([]byte, error) {
	if len(raw) == 32 {
		return raw, nil
	}
	s := strings.TrimSpace(string(raw))
	if len(s) == 64 {
		if k, err := hex.DecodeString(s); err == nil && len(k) == 32 {
			return k, nil
		}
	}
	if k, err := base64.StdEncoding.DecodeString(s); err == nil && len(k) == 32 {
		return k, nil
	}
	return nil, serr.New("encryption key must be 32 raw bytes, 64 hex characters, or base64 of 32 bytes")
}
