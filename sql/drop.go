package sql

// drop.go: the SQL side of dependency-aware drops (N-056). The engine
// (bytdb's drop.go) decides what a drop reaches and runs it as one DDL
// transaction. This file supplies what the engine cannot work out on its
// own, because it lives in SQL text: which relations a view reads and
// which columns a CHECK mentions. It also renders the cascade notices.

import (
	"strings"

	"github.com/rohanthewiz/bytdb"
)

// dropOpts is the engine's DropOptions for a DROP statement: its CASCADE
// flag, plus the readers for the view and CHECK text the engine stores
// but does not parse.
func dropOpts(cascade bool) bytdb.DropOptions {
	return bytdb.DropOptions{Cascade: cascade, ViewReads: viewReads, CheckReads: checkReads}
}

// cascadeNotices renders a CASCADE drop's dependents as Postgres's
// notices, one line each: "drop cascades to constraint c_pid_fkey on
// table c", "drop cascades to view v". When more than one object goes,
// Postgres sends a single "drop cascades to N other objects" notice and
// lists them in its DETAIL. bytdb sends one notice per object instead,
// because a Result carries notices as lines (one NoticeResponse each)
// with no DETAIL. The objects named are the same.
func cascadeNotices(deps []bytdb.Dependent) []string {
	out := make([]string, len(deps))
	for i, d := range deps {
		out[i] = "drop cascades to " + d.String()
	}
	return out
}

// viewReads reports the relation names a stored view query reads: every
// FROM name at any depth (subqueries, union arms and WITH bodies
// included), less the names the query's own WITH entries bind.
//
// Subtracting every CTE name the query binds, at any depth, is broader
// than SQL's scoping. A view that both binds a CTE named t somewhere and
// reads a table t elsewhere loses its dependency on t. Erring that way
// only fails to protect such a view (it breaks when queried, as every
// view did before N-056). Erring the other way would refuse a drop
// needlessly, or have CASCADE drop a view that never read the table.
//
// A stored query that no longer parses reads nothing nameable, so it
// reports no names rather than failing the drop. CREATE VIEW validated
// it, so only a grammar change could cause that.
func viewReads(query string) ([]string, error) {
	st, err := Parse(query)
	if err != nil {
		return nil, nil
	}
	sel, ok := st.(*Select)
	if !ok {
		return nil, nil
	}
	tables := map[string]bool{}
	collectSelTables(sel, tables)
	bound := map[string]bool{}
	eachSelect(sel, func(s *Select) {
		for _, c := range s.With {
			bound[c.Name] = true
		}
	})
	out := make([]string, 0, len(tables))
	for n := range tables {
		if !bound[n] {
			out = append(out, n)
		}
	}
	return out, nil
}

// checkReads reports the columns of table that a stored CHECK
// expression mentions, bare or qualified with the table's name. A parse
// failure is an error: the drop then fails rather than leave behind a
// CHECK that names a missing column, which would fail every later
// insert.
func checkReads(table, expr string) ([]string, error) {
	ex, err := parseCheckExpr(expr)
	if err != nil {
		return nil, err
	}
	var cols []string
	walkExpr(ex, func(sub Expr) bool {
		if c, ok := sub.(*ExCol); ok && (c.Col.Table == "" || c.Col.Table == table) {
			cols = append(cols, c.Col.Name)
		}
		return true
	})
	return cols, nil
}

// eachSelect calls fn on s and on every SELECT nested in it: WITH
// bodies, UNION arms, and subqueries in any expression or condition. It
// walks the same places collectSelTables does.
func eachSelect(s *Select, fn func(*Select)) {
	if s == nil {
		return
	}
	fn(s)
	for _, c := range s.With {
		eachSelect(c.Sel, fn)
	}
	for _, arm := range s.Union {
		eachSelect(arm.Sel, fn)
	}
	inExpr := func(e Expr) {
		walkExpr(e, func(sub Expr) bool {
			if q, ok := sub.(*ExSub); ok {
				eachSelect(q.Sel, fn)
				return false
			}
			return true
		})
	}
	var inBool func(b BoolExpr)
	inBool = func(b BoolExpr) {
		switch n := b.(type) {
		case *Cond:
			inExpr(n.Ex)
		case *Not:
			inBool(n.Expr)
		case *And:
			for _, sub := range n.Exprs {
				inBool(sub)
			}
		case *Or:
			for _, sub := range n.Exprs {
				inBool(sub)
			}
		}
	}
	for _, f := range s.From {
		inBool(f.On)
	}
	inBool(s.Where)
	inBool(s.Having)
	for _, it := range s.Items {
		inExpr(it.Ex)
	}
	for _, g := range s.GroupBy {
		inExpr(g.Ex)
	}
	for _, o := range s.OrderBy {
		inExpr(o.Ex)
	}
}

// execDropTables runs DROP TABLE a, b, ... [CASCADE]. Every name is
// checked first, so a missing table (without IF EXISTS) refuses the
// statement before anything is dropped, as Postgres's single-transaction
// DROP does (deep-look D-54). The tables that exist then drop in one
// engine transaction (Engine.DropTables). That makes the statement all
// or nothing, and lets references among the named tables pass:
// DROP TABLE parent, child needs no CASCADE.
//
// The notices come in Postgres's order: the IF EXISTS skips first, then
// what CASCADE dropped.
func (d *DB) execDropTables(s *DropTable) (*Result, error) {
	names := append([]string{s.Table}, s.More...)
	var present []string
	var notice []string
	for _, n := range names {
		if d.e.TableShared(n) != nil {
			present = append(present, n)
			continue
		}
		if !s.IfExists {
			// Postgres's wording for DROP TABLE, classified as an
			// undefined table (42P01) like the engine's own error.
			return nil, bytdb.KindErr(0, `table "`+n+`" does not exist`,
				[]error{bytdb.ErrNoSuchTable}, "table", n)
		}
		notice = append(notice, `table "`+n+`" does not exist, skipping`)
	}
	if len(present) > 0 {
		deps, err := d.e.DropTables(present, dropOpts(s.Cascade))
		if err != nil {
			return nil, err
		}
		notice = append(notice, cascadeNotices(deps)...)
	}
	return &Result{Notice: strings.Join(notice, "\n")}, nil
}
