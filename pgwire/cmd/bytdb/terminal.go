package main

// terminal.go: the interactive line editor. liner (a pure-Go linenoise,
// the same lineage as the DuckDB and sqlite shells' editors) supplies
// editing keys, history with Ctrl-R search, and tab completion.
//
// Terminal modes are the subtle part. liner switches the terminal to raw
// mode when it is created and keeps it there between prompts (with
// signal keys re-enabled while a statement runs, so Ctrl-C reaches the
// SIGINT handler and cancels the statement). A child process — .shell,
// .edit's $EDITOR — needs the user's normal "cooked" mode instead, so
// the editor captures both modes up front and swaps them around child
// processes:
//
//	startup:   cooked := TerminalMode()   (the user's settings)
//	           NewLiner()                 (raw)
//	           raw := TerminalMode()
//	.shell:    cooked.ApplyMode() → run child → raw.ApplyMode()

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/peterh/liner"
)

// lineEditor is the interactive lineSource.
type lineEditor struct {
	st          *liner.State
	cooked, raw liner.ModeApplier // nil when the mode could not be read
	historyPath string
}

// newLineEditor puts the terminal under liner's control and loads the
// history file. complete supplies tab completions.
func newLineEditor(historyPath string, complete liner.WordCompleter) *lineEditor {
	cooked, _ := liner.TerminalMode()
	st := liner.NewLiner()
	raw, _ := liner.TerminalMode()
	st.SetCtrlCAborts(true) // Ctrl-C at the prompt → ErrPromptAborted
	st.SetTabCompletionStyle(liner.TabPrints)
	st.SetWordCompleter(complete)
	le := &lineEditor{st: st, cooked: cooked, raw: raw, historyPath: historyPath}
	if historyPath != "" {
		if f, err := os.Open(historyPath); err == nil {
			st.ReadHistory(f)
			f.Close()
		}
	}
	return le
}

func (le *lineEditor) readLine(prompt string) (string, error) {
	line, err := le.st.Prompt(prompt)
	if errors.Is(err, liner.ErrPromptAborted) {
		return "", errInterrupt
	}
	return line, err
}

func (le *lineEditor) addHistory(line string) { le.st.AppendHistory(line) }

// suspend hands the terminal back in its original mode (for a child
// process) and returns the function that takes it back.
func (le *lineEditor) suspend() func() {
	if le.cooked == nil || le.raw == nil {
		return func() {}
	}
	le.cooked.ApplyMode()
	return func() { le.raw.ApplyMode() }
}

// close saves the history and restores the terminal. The history file
// is written to a temp file and renamed, so a crash mid-write cannot
// truncate the user's history.
func (le *lineEditor) close() {
	if le.historyPath != "" {
		tmp := le.historyPath + ".tmp"
		if f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
			_, werr := le.st.WriteHistory(f)
			cerr := f.Close()
			if werr == nil && cerr == nil {
				os.Rename(tmp, le.historyPath)
			} else {
				os.Remove(tmp)
			}
		}
	}
	le.st.Close()
}

// defaultHistoryPath is $BYTDB_HISTORY, else ~/.bytdb_history. An empty
// BYTDB_HISTORY (set but blank) disables history, for shared accounts.
func defaultHistoryPath() string {
	if p, ok := os.LookupEnv("BYTDB_HISTORY"); ok {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".bytdb_history")
}
