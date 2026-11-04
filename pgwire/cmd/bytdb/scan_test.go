package main

import (
	"slices"
	"testing"
)

// TestScanSQL pins statement splitting on exactly the constructs that
// make a naive "split on ;" wrong: quotes, escape strings, quoted
// identifiers and both comment forms.
func TestScanSQL(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		stmts   []string
		content bool // restHasContent
		open    bool
	}{
		{"one", "select 1;", []string{"select 1"}, false, false},
		{"two", "select 1; select 2;", []string{"select 1", "select 2"}, false, false},
		{"unterminated", "select 1", nil, true, false},
		{"partial rest", "select 1; select", []string{"select 1"}, true, false},
		{"semicolon in string", "select 'a;b';", []string{"select 'a;b'"}, false, false},
		{"doubled quote", "select 'it''s;';", []string{"select 'it''s;'"}, false, false},
		{"escape string", `select E'it\'s; ok';`, []string{`select E'it\'s; ok'`}, false, false},
		{"escape string doubled then backslash", `select E'a''b\'; c';`, []string{`select E'a''b\'; c'`}, false, false},
		{"identifier ending in e is not E-string", `select some'x\'; y';`, []string{`select some'x\'`}, true, true},
		{"quoted identifier", `select 1 as "a;b";`, []string{`select 1 as "a;b"`}, false, false},
		{"line comment", "select 1 -- a; b\n;", []string{"select 1 -- a; b"}, false, false},
		{"block comment", "select /* ; */ 1;", []string{"select /* ; */ 1"}, false, false},
		{"nested block comment", "select /* a /* ; */ ; */ 1;", []string{"select /* a /* ; */ ; */ 1"}, false, false},
		{"open string", "select 'abc;", nil, true, true},
		{"open block comment", "select 1 /* ;", nil, true, true},
		{"comment only", "-- nothing here\n", nil, false, false},
		{"empty statements dropped", ";;select 1;;", []string{"select 1"}, false, false},
		{"comment after statement", "select 1; -- trailing\n", []string{"select 1"}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := scanSQL(c.src)
			if !slices.Equal(r.stmts, c.stmts) {
				t.Errorf("stmts = %q, want %q", r.stmts, c.stmts)
			}
			if r.restHasContent != c.content {
				t.Errorf("restHasContent = %v, want %v (rest %q)", r.restHasContent, c.content, r.rest)
			}
			if r.open != c.open {
				t.Errorf("open = %v, want %v", r.open, c.open)
			}
		})
	}
}

// TestHistoryLine checks that flattening a multi-line statement keeps
// it runnable: line comments become block comments (or they would
// swallow the rest of the joined line) and string contents survive.
func TestHistoryLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"select 1\n  , 2;", "select 1 , 2;"},
		{"select 1 -- first\n, 2;", "select 1 /* first */ , 2;"},
		{"select 1 -- has */ inside\n;", "select 1 /* has * / inside */ ;"},
		{"select 'a\nb';", "select 'a\nb';"},
		{"select 'a    b'   ,   1;", "select 'a    b' , 1;"},
		{"select E'x\\'  y';", "select E'x\\'  y';"},
		{"select /* a\nb */ 1;", "select /* a b */ 1;"},
	}
	for _, c := range cases {
		if got := historyLine(c.in); got != c.want {
			t.Errorf("historyLine(%q) = %q, want %q", c.in, got, c.want)
		}
		// The flattened form must scan to the same statements.
		if a, b := scanSQL(c.in).stmts, scanSQL(historyLine(c.in)).stmts; len(a) != len(b) {
			t.Errorf("%q: %d statements before flattening, %d after", c.in, len(a), len(b))
		}
	}
}
