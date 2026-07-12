package sql

// views.go: CTEs, derived tables, and views — all one mechanism. A
// statement's WITH entries (written, or synthesized from FROM
// (SELECT ...) at parse) materialize once, in order, into per-
// statement virtual tables (DB.vtabs) that lookup layers over the
// catalog; a view is the same thing sourced from stored SELECT text,
// materialized for any statement that references its name. The join
// machinery already runs virtual tables (that is how the system
// catalog is served), so downstream nothing changes.

import (
	"strings"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// vtabDesc builds a virtual table's synthetic descriptor from a
// result shape, applying WITH's optional output-column renames.
func vtabDesc(name string, colNames []string, res *Result, renames []string) (*bytdb.TableDesc, error) {
	if len(renames) > 0 {
		if len(renames) != len(res.Cols) {
			return nil, serr.New("WITH column list does not match the query's columns",
				"name", name)
		}
		colNames = renames
	}
	cols := make([]bytdb.Column, len(colNames))
	for i, cn := range colNames {
		cols[i] = bytdb.Column{Name: cn, Type: res.Types[i]}
	}
	return &bytdb.TableDesc{Name: name, Columns: cols}, nil
}

// execSelectWith is the SELECT entry point above runSelectCore: it
// materializes the statement's WITH entries onto a DB copy, then runs
// the body (union chain or single core) with them visible.
func (d *DB) execSelectWith(s *Select) (*Result, error) {
	if len(s.With) > 0 {
		dw := *d
		dw.vtabs = make(map[string]vtab, len(d.vtabs)+len(s.With))
		for k, v := range d.vtabs {
			dw.vtabs[k] = v
		}
		for _, cte := range s.With {
			// Each entry sees the ones before it (and the statement's
			// views); its own With is always empty — Parse hoists
			// everything to the top level.
			res, err := dw.runSelectBody(cte.Sel)
			if err != nil {
				return nil, serr.Wrap(err, "with_query", displayCTEName(cte.Name))
			}
			desc, err := vtabDesc(displayCTEName(cte.Name), res.Cols, res, cte.Cols)
			if err != nil {
				return nil, err
			}
			dw.vtabs[cte.Name] = vtab{desc: desc, rows: res.Rows}
		}
		d = &dw
	}
	return d.runSelectBody(s)
}

// runSelectBody runs a SELECT's body — union chain or single core —
// ignoring any With (the caller has materialized it).
func (d *DB) runSelectBody(s *Select) (*Result, error) {
	if len(s.Union) > 0 {
		return d.execUnion(s)
	}
	return d.runSelectCore(s)
}

// displayCTEName strips the synthetic marker off a derived table's
// generated name for error messages; written CTE names pass through.
func displayCTEName(name string) string {
	if strings.HasPrefix(name, "*derived*") {
		return "subquery in FROM"
	}
	return name
}

// --- views ---

// maxViewDepth bounds view-over-view expansion. Cycles cannot be
// created in one statement (CREATE VIEW validates by running the
// body), but OR REPLACE can close a loop after the fact; the bound
// turns that into an error instead of unbounded recursion.
const maxViewDepth = 32

// withViews returns d, or a copy with every view the statement
// references materialized into vtabs. The common statement touches no
// views and pays one map probe per referenced name.
func (d *DB) withViews(st Statement) (*DB, error) {
	names := map[string]bool{}
	collectStmtTables(st, names)
	var dw *DB
	for name := range names {
		if _, shadowed := d.vtabs[name]; shadowed {
			continue
		}
		if d.e.View(name) == nil {
			continue
		}
		if dw == nil {
			c := *d
			c.vtabs = make(map[string]vtab, len(d.vtabs)+1)
			for k, v := range d.vtabs {
				c.vtabs[k] = v
			}
			dw = &c
		}
		if err := dw.materializeView(name, 0); err != nil {
			return nil, err
		}
	}
	if dw == nil {
		return d, nil
	}
	return dw, nil
}

// materializeView parses and runs one view's stored query, first
// materializing any views it references itself, and registers the
// result under the view's name.
func (dw *DB) materializeView(name string, depth int) error {
	if _, done := dw.vtabs[name]; done {
		return nil
	}
	vd := dw.e.View(name)
	if vd == nil {
		return nil
	}
	if depth >= maxViewDepth {
		return serr.New("views nest too deeply (circular view definitions?)", "view", name)
	}
	st, err := Parse(vd.Query)
	if err != nil {
		return serr.Wrap(err, "op", "parse view", "view", name)
	}
	sel, ok := st.(*Select)
	if !ok {
		return serr.New("stored view query is not a SELECT", "view", name)
	}
	inner := map[string]bool{}
	collectSelTables(sel, inner)
	for n := range inner {
		if _, done := dw.vtabs[n]; !done && dw.e.View(n) != nil {
			if err := dw.materializeView(n, depth+1); err != nil {
				return err
			}
		}
	}
	res, err := dw.execSelectWith(sel)
	if err != nil {
		return serr.Wrap(err, "view", name)
	}
	desc, err := vtabDesc(name, res.Cols, res, nil)
	if err != nil {
		return err
	}
	dw.vtabs[name] = vtab{desc: desc, rows: res.Rows}
	return nil
}

// collectStmtTables gathers every table name a statement's FROM
// clauses reference, at any depth — subqueries, union arms, and WITH
// bodies included — so view expansion can see the whole statement.
func collectStmtTables(st Statement, out map[string]bool) {
	switch s := st.(type) {
	case *Select:
		collectSelTables(s, out)
	case *Insert:
		for _, row := range s.Rows {
			for _, v := range row {
				if ex, ok := v.(Expr); ok {
					collectExprTables(ex, out)
				}
			}
		}
		if oc := s.Conflict; oc != nil {
			for _, ex := range oc.SetEx {
				collectExprTables(ex, out)
			}
			collectBoolTables(oc.Where, out)
		}
		collectItemsTables(s.RetItems, out)
	case *Update:
		for _, ex := range s.SetEx {
			collectExprTables(ex, out)
		}
		collectBoolTables(s.Where, out)
		collectItemsTables(s.RetItems, out)
	case *Delete:
		collectBoolTables(s.Where, out)
		collectItemsTables(s.RetItems, out)
	case *Explain:
		collectStmtTables(s.Stmt, out)
	}
}

func collectSelTables(s *Select, out map[string]bool) {
	for _, c := range s.With {
		collectSelTables(c.Sel, out)
	}
	for _, f := range s.From {
		if f.Table != "" {
			out[f.Table] = true
		}
		collectBoolTables(f.On, out)
	}
	collectBoolTables(s.Where, out)
	collectBoolTables(s.Having, out)
	collectItemsTables(s.Items, out)
	for _, g := range s.GroupBy {
		collectExprTables(g.Ex, out)
	}
	for _, o := range s.OrderBy {
		collectExprTables(o.Ex, out)
	}
	for _, arm := range s.Union {
		collectSelTables(arm.Sel, out)
	}
}

func collectItemsTables(items []SelectItem, out map[string]bool) {
	for _, it := range items {
		collectExprTables(it.Ex, out)
	}
}

func collectBoolTables(e BoolExpr, out map[string]bool) {
	switch n := e.(type) {
	case *Cond:
		collectExprTables(n.Ex, out)
	case *Not:
		collectBoolTables(n.Expr, out)
	case *And:
		for _, sub := range n.Exprs {
			collectBoolTables(sub, out)
		}
	case *Or:
		for _, sub := range n.Exprs {
			collectBoolTables(sub, out)
		}
	}
}

func collectExprTables(e Expr, out map[string]bool) {
	walkExpr(e, func(sub Expr) bool {
		if s, ok := sub.(*ExSub); ok {
			collectSelTables(s.Sel, out)
			return false
		}
		return true
	})
}

// usesViews reports whether the statement references any view — the
// cheap pre-check that decides whether execution needs the wrapping
// snapshot and materialization pass.
func (d *DB) usesViews(st Statement) bool {
	names := map[string]bool{}
	collectStmtTables(st, names)
	for name := range names {
		if _, shadowed := d.vtabs[name]; shadowed {
			continue
		}
		if d.e.View(name) != nil {
			return true
		}
	}
	return false
}

// staticWith registers a SELECT's WITH entries as empty-row virtual
// tables with statically derived shapes — for the paths that must
// resolve names without executing anything (EXPLAIN, Describe).
func (d *DB) staticWith(s *Select) (*DB, error) {
	if len(s.With) == 0 {
		return d, nil
	}
	dw := *d
	dw.vtabs = make(map[string]vtab, len(d.vtabs)+len(s.With))
	for k, v := range d.vtabs {
		dw.vtabs[k] = v
	}
	for _, cte := range s.With {
		res := &Result{}
		if err := describeSelect(dw.lookup(dw.e.Table), cte.Sel,
			func(any, bytdb.ColType) {}, res); err != nil {
			return nil, serr.Wrap(err, "with_query", displayCTEName(cte.Name))
		}
		desc, err := vtabDesc(displayCTEName(cte.Name), res.Cols, res, cte.Cols)
		if err != nil {
			return nil, err
		}
		dw.vtabs[cte.Name] = vtab{desc: desc, rows: [][]any{}}
	}
	return &dw, nil
}

// staticViews is withViews without execution: each referenced view's
// shape comes from describing its stored query, and it registers with
// no rows. Depth-limited like the real expansion.
func (d *DB) staticViews(st Statement) (*DB, error) {
	names := map[string]bool{}
	collectStmtTables(st, names)
	dw := d
	for name := range names {
		next, err := dw.staticView(name, 0)
		if err != nil {
			return nil, err
		}
		dw = next
	}
	return dw, nil
}

func (d *DB) staticView(name string, depth int) (*DB, error) {
	if _, done := d.vtabs[name]; done {
		return d, nil
	}
	vd := d.e.View(name)
	if vd == nil {
		return d, nil
	}
	if depth >= maxViewDepth {
		return nil, serr.New("views nest too deeply (circular view definitions?)", "view", name)
	}
	st, err := Parse(vd.Query)
	if err != nil {
		return nil, serr.Wrap(err, "op", "parse view", "view", name)
	}
	sel, ok := st.(*Select)
	if !ok {
		return nil, serr.New("stored view query is not a SELECT", "view", name)
	}
	inner := map[string]bool{}
	collectSelTables(sel, inner)
	dw := d
	for n := range inner {
		if dw, err = dw.staticView(n, depth+1); err != nil {
			return nil, err
		}
	}
	// The body's CTE shapes live on a side copy used only to describe
	// it — they must not leak into the outer scope, where they could
	// shadow real names.
	cteDW, err := dw.staticWith(sel)
	if err != nil {
		return nil, err
	}
	res := &Result{}
	if err := describeSelect(cteDW.lookup(cteDW.e.Table), sel,
		func(any, bytdb.ColType) {}, res); err != nil {
		return nil, serr.Wrap(err, "view", name)
	}
	desc, err := vtabDesc(name, res.Cols, res, nil)
	if err != nil {
		return nil, err
	}
	out := *dw
	out.vtabs = make(map[string]vtab, len(dw.vtabs)+1)
	for k, v := range dw.vtabs {
		out.vtabs[k] = v
	}
	out.vtabs[name] = vtab{desc: desc, rows: [][]any{}}
	return &out, nil
}

// execCreateView validates the view by running its body once (which
// also proves any referenced views resolve) and stores the text.
func (d *DB) execCreateView(s *CreateView) (*Result, error) {
	st, err := Parse(s.Query)
	if err != nil {
		return nil, err
	}
	dv, err := d.withViews(st)
	if err != nil {
		return nil, err
	}
	if _, err := dv.execSelectWith(st.(*Select)); err != nil {
		return nil, err
	}
	if err := d.e.CreateView(s.Name, s.Query, s.OrReplace); err != nil {
		return nil, err
	}
	return &Result{}, nil
}

func (d *DB) execDropView(s *DropView) (*Result, error) {
	existed, err := d.e.DropView(s.Name)
	if err != nil {
		return nil, err
	}
	if !existed {
		if s.IfExists {
			return &Result{Notice: `view "` + s.Name + `" does not exist, skipping`}, nil
		}
		return nil, serr.New(`view "` + s.Name + `" does not exist`)
	}
	return &Result{}, nil
}
