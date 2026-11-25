package sql

// N-053: citext through the SQL layer, for what the difftest corpus
// (corpus/83_citext.sql, checked against Postgres 16) does not reach:
// foreign keys, upserts and joins on citext keys, IN-list key pushdown,
// citext beside text in comparisons, and EXPLAIN.

import (
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func TestCITextConstraints(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create extension if not exists citext`)
	exec(t, d, `create table team (name citext primary key, note text)`)
	exec(t, d, `create table member (id int primary key, team citext references team (name))`)
	exec(t, d, `insert into team values ('Red', 'r'), ('blue', 'b')`)
	// The foreign key matches ignoring case, as citext equality does.
	exec(t, d, `insert into member values (1, 'RED'), (2, 'Blue')`)
	if _, err := d.Exec(`insert into member values (3, 'green')`); err == nil {
		t.Error("an unmatched citext foreign key was accepted")
	}
	// ON CONFLICT on a citext key finds the row in another case and
	// keeps the stored key's spelling.
	exec(t, d, `insert into team values ('RED', 'again') on conflict (name) do update set note = excluded.note`)
	res := exec(t, d, `select name, note from team order by name`)
	if got := rowsText(res); got != "blue b | Red again" {
		t.Errorf("after upsert: %q", got)
	}
	// Joins on citext columns match ignoring case.
	res = exec(t, d, `select m.id, t.name from member m join team t on t.name = m.team order by m.id`)
	if got := rowsText(res); got != "1 Red | 2 blue" {
		t.Errorf("join: %q", got)
	}
	// IN-list key lookups fold the probes.
	res = exec(t, d, `select note from team where name in ('red', 'BLUE', 'x') order by note`)
	if got := rowsText(res); got != "again | b" {
		t.Errorf("IN: %q", got)
	}
	if res.Types[0] != bytdb.TString {
		t.Errorf("note typed %q", res.Types[0])
	}
}

func TestCITextAgainstText(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table c (id int primary key, ci citext, tx text)`)
	exec(t, d, `insert into c values (1, 'Abc', 'abc'), (2, 'abc', 'abc')`)
	// citext = text is text = text: case-sensitive.
	res := exec(t, d, `select id from c where ci = tx order by id`)
	if got := rowsText(res); got != "2" {
		t.Errorf("ci = tx: %q", got)
	}
	// citext = citext and citext = an untyped literal ignore case.
	res = exec(t, d, `select id from c where ci = 'ABC' order by id`)
	if got := rowsText(res); got != "1 | 2" {
		t.Errorf("ci = 'ABC': %q", got)
	}
	res = exec(t, d, `select id from c where ci = tx::citext order by id`)
	if got := rowsText(res); got != "1 | 2" {
		t.Errorf("ci = tx::citext: %q", got)
	}
	// Functions see text and return text; the result's case is kept.
	res = exec(t, d, `select upper(ci), ci || '!', pg_typeof(ci), pg_typeof(lower(ci)) from c where id = 1`)
	if got := rowsText(res); got != "ABC Abc! citext text" {
		t.Errorf("functions: %q", got)
	}
	exec(t, d, `create index c_ci on c (ci)`)
	res = exec(t, d, `explain select id from c where ci = 'X''Y'`)
	var plan strings.Builder
	for _, row := range res.Rows {
		plan.WriteString(row[0].(string) + "\n")
	}
	if !strings.Contains(plan.String(), `'X''Y'::citext`) {
		t.Errorf("explain does not render the citext bound:\n%s", plan.String())
	}
}
