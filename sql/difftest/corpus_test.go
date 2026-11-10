package difftest

import (
	"os"
	"path/filepath"
	"testing"
)

// TestParseCorpusFormat pins the corpus grammar. It needs no Postgres,
// so `go test` here always checks that every corpus file still parses
// even when the differential run itself is skipped.
func TestParseCorpusFormat(t *testing.T) {
	src := `-- a comment
-- @setup
create table t (id int);
insert into t
  values (1);
-- @queries
select 1;
-- @ordered
-- @notype
select id
  from t;
-- @error 42883
select 1 = true;
-- @todo D-23: not typed yet
select 2;
-- @bytdb: documented divergence
select 3;
`
	es, err := parseCorpus(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 7 {
		t.Fatalf("got %d entries, want 7: %+v", len(es), es)
	}
	if !es[0].setup || !es[1].setup || es[2].setup {
		t.Errorf("setup flags wrong: %+v", es[:3])
	}
	if es[1].sql != "insert into t\n  values (1)" || es[1].line != 4 {
		t.Errorf("multi-line statement: %+v", es[1])
	}
	if e := es[3]; !e.ordered || !e.noType || e.line != 10 {
		t.Errorf("ordered/notype entry: %+v", e)
	}
	if e := es[4]; e.wantErr != "42883" {
		t.Errorf("error entry: %+v", e)
	}
	if e := es[5]; e.todo != "D-23" || e.reason != "not typed yet" || !e.expectDiff() {
		t.Errorf("todo entry: %+v", e)
	}
	if e := es[6]; !e.bytdb || !e.expectDiff() {
		t.Errorf("bytdb entry: %+v", e)
	}
	// Directives apply to one statement only.
	if es[2].ordered || es[2].expectDiff() {
		t.Errorf("directive leaked: %+v", es[2])
	}

	for _, bad := range []string{
		"select 1",                        // unterminated
		"-- @todo 23: x\nselect 1;",       // malformed item
		"-- @nonsense\nselect 1;",         // unknown directive
		"-- @setup\n-- @error\nselect 1;", // setup takes no entry directives
	} {
		if _, err := parseCorpus(bad); err == nil {
			t.Errorf("parseCorpus(%q) accepted", bad)
		}
	}

	files, _ := filepath.Glob(filepath.Join("corpus", "*.sql"))
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseCorpus(string(src)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
