package sql

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// TestParseVacuum covers both of Postgres's option spellings and the
// table list. Options are discarded except VERBOSE, so the table list
// and the Verbose flag are all that survive into the AST.
func TestParseVacuum(t *testing.T) {
	for _, tc := range []struct {
		src     string
		tables  []string
		verbose bool
	}{
		{`vacuum`, nil, false},
		{`VACUUM;`, nil, false},
		{`vacuum full`, nil, false},
		{`vacuum full freeze verbose analyze`, nil, true},
		{`vacuum verbose users`, []string{"users"}, true},
		{`vacuum users, public.orders`, []string{"users", "orders"}, false},
		{`vacuum (full)`, nil, false},
		{`vacuum (verbose, analyze) users`, []string{"users"}, true},
		{`vacuum (VERBOSE true)`, nil, true},
		{`vacuum (verbose on)`, nil, true},
		{`vacuum (verbose 1)`, nil, true},
		{`vacuum (verbose false)`, nil, false},
		{`vacuum (verbose off, full)`, nil, false},
		{`vacuum (verbose '0')`, nil, false},
		{`vacuum (full true, index_cleanup off, parallel 4, buffer_usage_limit '256kB') users`, []string{"users"}, false},
		{`vacuum pg_catalog.pg_class`, []string{"pg_catalog.pg_class"}, false},
	} {
		v, ok := mustParse(t, tc.src).(*Vacuum)
		if !ok {
			t.Fatalf("Parse(%q) is not a *Vacuum", tc.src)
		}
		if !reflect.DeepEqual(v.Tables, tc.tables) {
			t.Fatalf("Parse(%q) tables = %v; want %v", tc.src, v.Tables, tc.tables)
		}
		if v.Verbose != tc.verbose {
			t.Fatalf("Parse(%q) verbose = %v; want %v", tc.src, v.Verbose, tc.verbose)
		}
	}

	for _, src := range []string{
		`vacuum (`,                  // unterminated option list
		`vacuum ()`,                 // Postgres requires at least one option
		`vacuum (full`,              // missing ")"
		`vacuum users,`,             // dangling comma
		`vacuum analyze users (id)`, // column lists are not supported
		`vacuum nosuch.users`,       // unknown schema
	} {
		if _, err := Parse(src); err == nil {
			t.Fatalf("Parse(%q): want error", src)
		}
	}
}

// TestVacuumCompacts runs VACUUM end to end: after churn the log must
// shrink and its epoch bump — the observable proof the engine compacted
// — while the surviving rows read back unchanged.
func TestVacuumCompacts(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, v text)`)
	vals := make([]string, 500)
	for i := range vals {
		vals[i] = "(" + strconv.Itoa(i+1) + ", 'row')"
	}
	exec(t, d, `insert into t values `+strings.Join(vals, ", "))
	exec(t, d, `delete from t where id > 2`)

	before := d.e.Stats()
	res := exec(t, d, `vacuum`)
	after := d.e.Stats()

	if res.Notice != "" {
		t.Fatalf("unexpected notice %q", res.Notice)
	}
	if after.LogEpoch <= before.LogEpoch {
		t.Fatalf("log epoch %d -> %d; VACUUM did not compact", before.LogEpoch, after.LogEpoch)
	}
	if after.LogBytes >= before.LogBytes {
		t.Fatalf("log did not shrink: %d -> %d bytes", before.LogBytes, after.LogBytes)
	}
	got := exec(t, d, `select id, v from t order by id`).Rows
	if want := [][]any{{int64(1), "row"}, {int64(2), "row"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rows after VACUUM = %v; want %v", got, want)
	}

	// The prepared-statement path reports the right command tag.
	st, err := d.Prepare(`vacuum t`)
	if err != nil {
		t.Fatal(err)
	}
	if st.Command() != "VACUUM" {
		t.Fatalf("Command() = %q; want VACUUM", st.Command())
	}
}

// TestVacuumVerbose checks the report line: the log's sizes on either
// side of the compaction, matching Stats, after any skip warnings. With
// no other session writing, "after" is exactly the compacted size.
func TestVacuumVerbose(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, v text)`)
	exec(t, d, `create view w as select id from t`)
	for i := range 200 {
		exec(t, d, `insert into t values (`+strconv.Itoa(i)+`, 'row')`)
	}
	exec(t, d, `delete from t where id > 1`)

	before := d.e.Stats().LogBytes
	res := exec(t, d, `vacuum verbose w, t`)
	after := d.e.Stats().LogBytes

	lines := strings.Split(res.Notice, "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], `skipping "w"`) {
		t.Fatalf("notice = %q; want the skip warning, then the report", res.Notice)
	}
	want := fmt.Sprintf("compacted the storage log: %d bytes before, %d after (%d reclaimed)",
		before, after, before-after)
	if lines[1] != want {
		t.Fatalf("report = %q; want %q", lines[1], want)
	}

	// A plain VACUUM stays silent; nothing vacuumable means no report.
	if res := exec(t, d, `vacuum (verbose false) t`); res.Notice != "" {
		t.Fatalf("VERBOSE false notice = %q", res.Notice)
	}
	if res := exec(t, d, `vacuum verbose w`); strings.Contains(res.Notice, "compacted") {
		t.Fatalf("VACUUM VERBOSE of only a view reported a compaction: %q", res.Notice)
	}

	// Writes landing between the two size reads can leave the log no
	// smaller; the report then drops "reclaimed" rather than going negative.
	if got := vacuumReport(100, 120); got != "compacted the storage log: 100 bytes before, 120 after" {
		t.Fatalf("vacuumReport(100, 120) = %q", got)
	}
}

// TestVacuumTargets checks name resolution: tables and system catalogs
// are vacuumable, views and sequences are skipped with a warning (and a
// list of only those compacts nothing), and an unknown name fails the
// statement before any compaction.
func TestVacuumTargets(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key)`)
	exec(t, d, `create view v as select id from t`)
	exec(t, d, `create sequence s`)

	epoch := func() uint64 { return d.e.Stats().LogEpoch }

	e0 := epoch()
	exec(t, d, `vacuum t, pg_catalog.pg_class`)
	if epoch() == e0 {
		t.Fatal("VACUUM of a table did not compact")
	}

	e0 = epoch()
	res := exec(t, d, `vacuum v, s`)
	if epoch() != e0 {
		t.Fatal("VACUUM of only non-tables compacted")
	}
	for _, name := range []string{`"v"`, `"s"`} {
		if !strings.Contains(res.Notice, "skipping "+name) {
			t.Fatalf("notice %q lacks a skip warning for %s", res.Notice, name)
		}
	}

	// A skipped relation alongside a real table still compacts.
	e0 = epoch()
	if res := exec(t, d, `vacuum v, t`); !strings.Contains(res.Notice, `skipping "v"`) {
		t.Fatalf("notice = %q", res.Notice)
	}
	if epoch() == e0 {
		t.Fatal("VACUUM v, t did not compact")
	}

	e0 = epoch()
	if _, err := d.Exec(`vacuum t, nosuch`); err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("VACUUM of unknown table err = %v", err)
	}
	if epoch() != e0 {
		t.Fatal("failed VACUUM still compacted")
	}
}

// TestSessionRejectsVacuumInBlock mirrors Postgres: VACUUM inside a
// transaction block fails and aborts the block; outside one it runs.
func TestSessionRejectsVacuumInBlock(t *testing.T) {
	d := openDB(t)
	s := d.NewSession()
	defer s.Close()

	sessExec(t, s, `begin`)
	if _, err := s.Exec(`vacuum`); err == nil ||
		err.Error() != "VACUUM cannot run inside a transaction block" {
		t.Fatalf("VACUUM in block err = %v", err)
	}
	if s.Status() != TxFailed {
		t.Fatalf("status = %c; want E", s.Status())
	}
	sessExec(t, s, `rollback`)
	sessExec(t, s, `vacuum`) // fine outside
}
