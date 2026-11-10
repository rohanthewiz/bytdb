package sql

// colneed.go: decode only the columns a SELECT reads (deep-look D-39).
//
// A scan used to decode every column of every row, and decoding was
// most of an aggregate scan's cost: SELECT count(*), sum(amount) FROM
// orders built the status and note strings of 100k rows to throw them
// away. A SELECT now tells the engine which columns it can ever look at
// (bytdb.ScanOptions.Cols); the rest stay nil in the row.
//
// The set is deliberately an over-approximation: every column NAME the
// statement mentions anywhere — select list, WHERE, ON, GROUP BY,
// HAVING, ORDER BY, window specs, CTE bodies, and every subquery
// (a correlated subquery reads the outer row by name) — whatever table
// it is qualified with. A name this walk over-collects costs a decode;
// one it missed would read a stored value as NULL, so anything it does
// not understand (a star, a whole-row reference, an unknown node) turns
// the mask off for the statement.
//
// Only the SELECT executors set it, on their join steps, and only once a
// step plans a real scan (a point get decodes one row; the mask is not
// worth its walk). Writes, FK checks and the reduced subquery runners
// read whole rows as before.

import (
	"slices"

	"github.com/rohanthewiz/bytdb"
)

// useColumnMasks switches the masks on; tests turn it off to check a
// query's answer does not depend on them.
var useColumnMasks = true

// nameSet is the column names a statement mentions; all is set when the
// statement may read columns it does not name.
type nameSet struct {
	names map[string]struct{}
	all   bool
}

func (ns *nameSet) add(name string) {
	if ns.names == nil {
		ns.names = map[string]struct{}{}
	}
	ns.names[name] = struct{}{}
}

func (ns *nameSet) has(name string) bool {
	_, ok := ns.names[name]
	return ok
}

func (ns *nameSet) selectNames(sel *Select) {
	if sel == nil || ns.all {
		return
	}
	if sel.Star {
		ns.all = true
		return
	}
	for _, c := range sel.With {
		ns.selectNames(c.Sel)
	}
	for _, f := range sel.From {
		ns.boolNames(f.On)
		for _, a := range f.FuncArgs {
			ns.exprNames(a)
		}
	}
	for _, it := range sel.Items {
		ns.itemNames(it)
	}
	ns.boolNames(sel.Where)
	for _, g := range sel.GroupBy {
		ns.add(g.Col.Name)
		ns.exprNames(g.Ex)
	}
	ns.boolNames(sel.Having)
	for _, o := range sel.OrderBy {
		ns.itemNames(o.SelectItem)
	}
	for _, u := range sel.Union {
		ns.selectNames(u.Sel)
	}
}

func (ns *nameSet) itemNames(it SelectItem) {
	switch {
	case it.Star && it.Agg == AggNone:
		ns.all = true // SELECT * or t.*: every column
	case it.Ex != nil:
		ns.exprNames(it.Ex)
	case !it.IsLit && !it.Star:
		ns.add(it.Col.Name) // a column, or an aggregate's argument column
	}
}

func (ns *nameSet) boolNames(b BoolExpr) {
	switch n := b.(type) {
	case nil:
	case *Pred:
		ns.itemNames(n.Item)
		if n.RItem != nil {
			ns.itemNames(*n.RItem)
		}
	case *Cond:
		ns.exprNames(n.Ex)
	case *Not:
		ns.boolNames(n.Expr)
	case *And:
		for _, sub := range n.Exprs {
			ns.boolNames(sub)
		}
	case *Or:
		for _, sub := range n.Exprs {
			ns.boolNames(sub)
		}
	default:
		ns.all = true
	}
}

func (ns *nameSet) exprNames(e Expr) {
	walkExpr(e, func(x Expr) bool {
		switch n := x.(type) {
		case *ExCol:
			ns.add(n.Col.Name)
		case *ExAgg:
			if !n.Star {
				ns.add(n.Col.Name)
			}
		case *ExSub:
			ns.selectNames(n.Sel)
		case *ExWindow:
			// walkExpr visits a window's ORDER BY expressions but not its
			// column-shaped items.
			for _, o := range n.OrderBy {
				ns.itemNames(o.SelectItem)
			}
		case *ExLit, *ExAnd, *ExOr, *ExNot, *ExCmp, *ExAny, *ExIsNull, *ExIn,
			*ExCase, *ExFunc, *ExCast, *ExIndex, *ExArray, *ExArith:
		default:
			ns.all = true // a node this walk does not know may hide references
		}
		return !ns.all
	})
}

// needCols returns the step's column mask for bytdb.ScanOptions.Cols,
// computing it on first use: nil (decode everything) for a virtual
// table, a step outside a SELECT, a statement that may read unnamed
// columns, or a mask that would skip nothing.
func (step *joinStep) needCols() []bool {
	if step.colsDone {
		return step.cols
	}
	step.colsDone = true
	if !useColumnMasks || step.colSel == nil || step.st.rows != nil || step.st.desc == nil {
		return nil
	}
	var ns nameSet
	ns.selectNames(step.colSel)
	desc := step.st.desc
	// A name equal to the table's own name or alias may be a whole-row
	// reference (SELECT t FROM t): keep every column.
	if ns.all || ns.has(step.st.name) || ns.has(desc.Name) {
		return nil
	}
	need := make([]bool, len(desc.Columns))
	skips := false
	for i, c := range desc.Columns {
		if ns.has(c.Name) {
			need[i] = true
		} else if !isPKOrdinal(desc, i) {
			skips = true
		}
	}
	if !skips {
		return nil
	}
	step.cols = need
	return need
}

// isPKOrdinal reports whether column ord is part of the primary key
// (always decoded, whatever the mask says).
func isPKOrdinal(desc *bytdb.TableDesc, ord int) bool {
	return slices.Contains(desc.PKCols, ord)
}

// readColumnsOf marks every step of a SELECT's FROM plan with the
// statement whose column references bound what its scans decode.
func (fp *fromPlan) readColumnsOf(sel *Select) {
	for i := range fp.steps {
		fp.steps[i].colSel = sel
	}
}
