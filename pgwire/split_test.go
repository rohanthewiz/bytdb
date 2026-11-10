package pgwire

import (
	"reflect"
	"testing"
)

func TestSplitStatements(t *testing.T) {
	cases := []struct {
		q    string
		want []stmtPart
	}{
		{"select 1", []stmtPart{{"select 1", 0}}},
		{"select 1;", []stmtPart{{"select 1", 0}}},
		{"  ; ;\n;", nil},
		// Comments alone are an empty statement (deep-look D-08): pgx's
		// ping, a trailing comment after the last ';', and a nested block
		// comment. A "--" inside a string is not a comment.
		{"-- ping", nil},
		{"/* a /* nested ; */ b */  -- c\n ;", nil},
		{"select 1; -- trailing", []stmtPart{{"select 1", 0}}},
		{"select 1; /* trailing */ ;", []stmtPart{{"select 1", 0}}},
		{"select '--x'", []stmtPart{{"select '--x'", 0}}},
		{"-- lead\nselect 2", []stmtPart{{"-- lead\nselect 2", 0}}},
		{"create table t (a int); insert into t values (1)", []stmtPart{
			{"create table t (a int)", 0},
			{"insert into t values (1)", 24},
		}},
		{"select ';' ; select 2", []stmtPart{
			{"select ';'", 0},
			{"select 2", 13},
		}},
		{`select ";" from "a;b"; select 2`, []stmtPart{
			{`select ";" from "a;b"`, 0},
			{"select 2", 23},
		}},
		{"select 1 -- ; not a split\n; select 2", []stmtPart{
			{"select 1 -- ; not a split", 0},
			{"select 2", 28},
		}},
		{"select 1 /* ; /* nested ; */ ; */; select 2", []stmtPart{
			{"select 1 /* ; /* nested ; */ ; */", 0},
			{"select 2", 35},
		}},
		{"select 'it''s; fine'", []stmtPart{{"select 'it''s; fine'", 0}}},
		// N-030: E'...' escape strings. \' does not close the string,
		// so the ';' after it stays inside.
		{`SELECT E'it\'s; fine'`, []stmtPart{{`SELECT E'it\'s; fine'`, 0}}},
		{`select e'it\'s; fine'; select 2`, []stmtPart{
			{`select e'it\'s; fine'`, 0},
			{"select 2", 23},
		}},
		// \\ is an escaped backslash, so the quote after it closes.
		{`select E'a\\'; select 2`, []stmtPart{
			{`select E'a\\'`, 0},
			{"select 2", 15},
		}},
		// A doubled quote keeps the string in escape mode: the \'
		// after it is still an escape, not the terminator.
		{`select E'a''b\'; c'; select 2`, []stmtPart{
			{`select E'a''b\'; c'`, 0},
			{"select 2", 21},
		}},
		// E only prefixes a string when it starts a token: here it
		// ends the identifier "some", so 'x\' is a standard string
		// and its backslash is an ordinary character.
		{`select some'x\'; select 2`, []stmtPart{
			{`select some'x\'`, 0},
			{"select 2", 17},
		}},
		{`select (E'\';'); select 2`, []stmtPart{
			{`select (E'\';')`, 0},
			{"select 2", 17},
		}},
		// Backslashes are not escapes in standard strings or quoted
		// identifiers.
		{`select 'a\'; select "b\"; select 3`, []stmtPart{
			{`select 'a\'`, 0},
			{`select "b\"`, 13},
			{"select 3", 26},
		}},
		// An unterminated E-string runs to the end of the buffer, even
		// when its last byte is a lone backslash.
		{`select E'x\`, []stmtPart{{`select E'x\`, 0}}},
		{`select E'x\'; select 2`, []stmtPart{{`select E'x\'; select 2`, 0}}},
	}
	for _, c := range cases {
		got := splitStatements(c.q)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitStatements(%q) = %v, want %v", c.q, got, c.want)
		}
	}
}
