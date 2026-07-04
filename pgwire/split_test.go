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
	}
	for _, c := range cases {
		got := splitStatements(c.q)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitStatements(%q) = %v, want %v", c.q, got, c.want)
		}
	}
}
