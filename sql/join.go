package sql

import (
	"strings"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// scope is the row shape a FROM clause produces: each table's columns
// concatenated in FROM order. Column references resolve to ordinals
// in the combined row.
type scope struct {
	tables []scopeTable
	width  int
}

type scopeTable struct {
	name string // what qualifiers match: the alias, or the table name
	desc *bytdb.TableDesc
	off  int     // this table's first ordinal in the combined row
	rows [][]any // materialized rows of a virtual system table; nil for real tables
}

func buildScope(lookup tableLookup, items []FromItem) (*scope, error) {
	sc := &scope{}
	seen := map[string]bool{}
	for _, it := range items {
		desc, rows := lookup(it.Table)
		if desc == nil {
			return nil, serr.New("no such table", "table", it.Table)
		}
		name := it.Alias
		if name == "" {
			// A schema-qualified system table is referenced by its bare
			// name (pg_catalog.pg_class c aside, ON n.oid = ... uses
			// pg_class.oid), as in Postgres.
			name = it.Table
			if _, bare, ok := strings.Cut(name, "."); ok {
				name = bare
			}
		}
		if seen[name] {
			return nil, serr.New("table name appears twice in FROM; alias one of them", "name", name)
		}
		seen[name] = true
		sc.tables = append(sc.tables, scopeTable{name: name, desc: desc, off: sc.width, rows: rows})
		sc.width += len(desc.Columns)
	}
	return sc, nil
}

// prefix is the scope an ON condition sees: the first n tables.
func (sc *scope) prefix(n int) *scope {
	last := sc.tables[n-1]
	return &scope{tables: sc.tables[:n], width: last.off + len(last.desc.Columns)}
}

// resolve maps a column reference to its combined-row ordinal. An
// unqualified name must be unambiguous across the scope's tables.
func (sc *scope) resolve(c ColRef) (int, error) {
	if c.Table != "" {
		for _, st := range sc.tables {
			if st.name == c.Table {
				ord := st.desc.ColIndex(c.Name)
				if ord < 0 {
					return -1, serr.New("no such column", "table", c.Table, "column", c.Name)
				}
				return st.off + ord, nil
			}
		}
		return -1, serr.New("no such table in FROM", "table", c.Table)
	}
	found := -1
	for _, st := range sc.tables {
		if ord := st.desc.ColIndex(c.Name); ord >= 0 {
			if found >= 0 {
				return -1, serr.New("column reference is ambiguous; qualify it", "column", c.Name)
			}
			found = st.off + ord
		}
	}
	if found < 0 {
		return -1, serr.New("no such column", "column", c.Name)
	}
	return found, nil
}

// table finds a scope table by alias or name (for t.* expansion).
func (sc *scope) table(name string) (scopeTable, error) {
	for _, st := range sc.tables {
		if st.name == name {
			return st, nil
		}
	}
	return scopeTable{}, serr.New("no such table in FROM", "table", name)
}

// column is the descriptor column behind a combined ordinal.
func (sc *scope) column(ord int) bytdb.Column {
	st := sc.tables[sc.tableOf(ord)]
	return st.desc.Columns[ord-st.off]
}

// tableOf is the index of the scope table containing ordinal ord.
func (sc *scope) tableOf(ord int) int {
	for i := len(sc.tables) - 1; i >= 0; i-- {
		if ord >= sc.tables[i].off {
			return i
		}
	}
	return 0
}

// binding is one predicate's resolved operand ordinals in the
// combined row; r is -1 when the right side is a literal.
type binding struct{ l, r int }

type binds map[*Pred]binding

// bind resolves every column reference in a condition against sc,
// recording each leaf's ordinals.
func (b binds) bind(e BoolExpr, sc *scope) error {
	return walkPreds(e, func(pr *Pred) error {
		l, err := sc.resolve(pr.Item.Col)
		if err != nil {
			return err
		}
		r := -1
		if pr.RItem != nil {
			if r, err = sc.resolve(pr.RItem.Col); err != nil {
				return err
			}
		}
		b[pr] = binding{l, r}
		return nil
	})
}

// evalPreds evaluates a bound condition over one combined row.
func evalPreds(e BoolExpr, b binds, vals []any) tri {
	return evalBool(e, func(pr *Pred) tri {
		bd := b[pr]
		rhs := pr.Val
		if bd.r >= 0 {
			rhs = vals[bd.r]
		}
		return checkPred(vals[bd.l], pr.Op, rhs)
	})
}

// fromPlan is a prepared FROM clause: the scope, one nested-loop step
// per table, and the bound WHERE, applied to full combined rows.
type fromPlan struct {
	sc    *scope
	steps []joinStep
	where BoolExpr
	b     binds // combined-space bindings for the WHERE and every ON
}

// joinStep scans one table once per combination of the tables before
// it. static conjuncts mention only this table and push into every
// scan; each template re-binds a conjunct between this table and an
// earlier one, turning it into a literal predicate per outer row (the
// index nested-loop: an equality template on a key prefix makes the
// inner scan a point get or bounded range).
type joinStep struct {
	it     FromItem
	st     scopeTable
	static []BoolExpr
	tmpls  []predTmpl
}

// predTmpl is "this table's column op the value at srcOrd of the
// outer row".
type predTmpl struct {
	item   SelectItem
	op     PredOp
	srcOrd int
}

// prepareFrom builds the scope, binds every ON (against the tables
// joined so far) and the WHERE (against everything), and assigns each
// step its pushable conjuncts. ON conjuncts always narrow their
// table's scan; WHERE conjuncts narrow a table's scan only when the
// table is not NULL-extended (the right side of a LEFT JOIN), where
// they must wait for the post-join filter.
func prepareFrom(lk tableLookup, items []FromItem, where BoolExpr) (*fromPlan, error) {
	sc, err := buildScope(lk, items)
	if err != nil {
		return nil, err
	}
	fp := &fromPlan{sc: sc, where: where, b: binds{}}
	for k, it := range items {
		if it.On != nil {
			if err := fp.b.bind(it.On, sc.prefix(k+1)); err != nil {
				return nil, err
			}
		}
	}
	if err := fp.b.bind(where, sc); err != nil {
		return nil, err
	}

	whereConj := conjuncts(where)
	for k, it := range items {
		st := sc.tables[k]
		step := joinStep{it: it, st: st}
		avail := conjuncts(it.On)
		if it.Join != JoinLeft {
			avail = append(avail, whereConj...)
		}
		end := st.off + len(st.desc.Columns)
		for _, pr := range avail {
			bd := fp.b[pr]
			lIn := bd.l >= st.off && bd.l < end
			rIn := bd.r >= st.off && bd.r < end
			switch {
			case lIn && (bd.r < 0 || rIn):
				step.static = append(step.static, pr)
			case lIn && bd.r >= 0 && bd.r < st.off:
				step.tmpls = append(step.tmpls, predTmpl{item: pr.Item, op: pr.Op, srcOrd: bd.r})
			case rIn && bd.l < st.off:
				step.tmpls = append(step.tmpls, predTmpl{item: *pr.RItem, op: flip(pr.Op), srcOrd: bd.l})
			}
		}
		fp.steps = append(fp.steps, step)
	}
	return fp, nil
}

// runJoin nested-loops the FROM clause within tx's snapshot, yielding
// each combined row on which every ON and the WHERE are definitely
// true. yield returning false stops the whole join.
func runJoin(tx *bytdb.Txn, fp *fromPlan, yield func([]any) bool) error {
	j := &joinRun{tx: tx, fp: fp, yield: yield}
	j.rec(0, nil)
	return j.err
}

type joinRun struct {
	tx    *bytdb.Txn
	fp    *fromPlan
	yield func([]any) bool
	stop  bool
	err   error
}

func (j *joinRun) rec(k int, partial []any) {
	if k == len(j.fp.steps) {
		if evalPreds(j.fp.where, j.fp.b, partial) == triTrue && !j.yield(partial) {
			j.stop = true
		}
		return
	}
	step := &j.fp.steps[k]

	// Fill the templates from the outer row. A NULL source value can
	// never satisfy its conjunct, so the scan is skipped outright (a
	// LEFT JOIN still NULL-extends below).
	exprs := make([]BoolExpr, 0, len(step.static)+len(step.tmpls))
	exprs = append(exprs, step.static...)
	skip := false
	for _, tp := range step.tmpls {
		v := partial[tp.srcOrd]
		if v == nil {
			skip = true
			break
		}
		exprs = append(exprs, &Pred{Item: tp.item, Op: tp.op, Val: v})
	}

	matched := false
	if !skip {
		next := func(rowVals []any) bool {
			vals := make([]any, len(partial)+len(rowVals))
			copy(vals, partial)
			copy(vals[len(partial):], rowVals)
			if step.it.On != nil && evalPreds(step.it.On, j.fp.b, vals) != triTrue {
				return true
			}
			matched = true
			j.rec(k+1, vals)
			return !j.stop && j.err == nil
		}
		if step.st.rows != nil {
			// A virtual system table: no pushdown, just its
			// materialized rows — ON and WHERE still filter fully.
			for _, rv := range step.st.rows {
				if !next(rv) {
					break
				}
			}
		} else {
			var expr BoolExpr
			switch {
			case len(exprs) == 1:
				expr = exprs[0]
			case len(exprs) > 1:
				expr = &And{Exprs: exprs}
			}
			pl, err := planScan(step.st.desc, step.st.name, expr)
			if err != nil {
				j.err = err
				return
			}
			err = scanPlan(j.tx, step.it.Table, pl, func(r bytdb.Row) bool {
				return next(r.Vals)
			})
			if err != nil && j.err == nil {
				j.err = err
			}
		}
		if j.stop || j.err != nil {
			return
		}
	}
	if step.it.Join == JoinLeft && !matched {
		vals := make([]any, len(partial)+len(step.st.desc.Columns))
		copy(vals, partial)
		j.rec(k+1, vals)
	}
}
