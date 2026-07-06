package sql

import (
	"slices"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// plan is the access strategy for one table read: which path to use,
// the lower bound pushed into the ordered scan, where the scan can
// stop early, and the residual filter. The whole WHERE tree stays in
// the residual filter — pushdown only narrows what is visited, so
// correctness never depends on it.
type plan struct {
	desc *bytdb.TableDesc

	get     []any    // full-PK point lookup (values in key order); nil when scanning
	index   string   // secondary index to scan; "" scans the primary index
	from    []any    // inclusive lower bound pushed into the scan; nil is unbounded
	stops   []stop   // early-termination checks, in key-column order
	reverse bool     // read the path backward (descending key order); set only for an unbounded scan
	filter  BoolExpr // residual filter: the full condition (nil: all rows)
	binds   binds    // the filter's operand ordinals within this table's rows
	pushed  []*Pred  // the conjuncts behind get/from/stops (EXPLAIN's Index Cond)
}

type stopKind int

const (
	stopNE stopKind = iota // stop when col != val (an equality prefix has ended)
	stopGE                 // stop when col >= val (pushed: col < val)
	stopGT                 // stop when col > val  (pushed: col <= val)
	stopLE                 // stop when col <= val (pushed: col > val, descending column)
	stopLT                 // stop when col < val  (pushed: col >= val, descending column)
)

// stop ends a scan at the first row where the column at ordinal ord
// leaves the pushed-down region. NULL column values never trigger an
// ascending range stop: NULL sorts before every value in the key
// encoding, so a NULL there means the scan is still inside (or
// entering) the region's NULL group, which the residual filter
// discards row by row. On a descending column NULLs sort after every
// value, so a NULL does stop the descending kinds.
type stop struct {
	ord  int
	kind stopKind
	val  any
}

// planScan validates the condition's columns against one table
// (qualifiers must match alias — the table's name or its FROM alias)
// and picks the access path: a point Get when every primary-key
// column has an equality predicate, else the primary index or the
// secondary index with the longest equality prefix (plus at most one
// range column), else a full scan. Only predicates that are top-level
// AND conjuncts push down; anything under OR or NOT is residual-only.
func planScan(desc *bytdb.TableDesc, alias string, where BoolExpr) (*plan, error) {
	p := &plan{desc: desc, filter: where, binds: binds{}}

	// Every column referenced anywhere in the tree must exist in this
	// table; record each leaf's operand ordinals for the row filter.
	res := func(c ColRef) (int, error) {
		if c.Table != "" && c.Table != alias {
			return -1, serr.New("no such table in FROM", "table", c.Table)
		}
		ord := desc.ColIndex(c.Name)
		if ord < 0 {
			return -1, serr.New("no such column", "table", alias, "column", c.Name)
		}
		return ord, nil
	}
	if err := walkPreds(where, func(pr *Pred) error {
		l, err := res(pr.Item.Col)
		if err != nil {
			return err
		}
		r := -1
		if pr.RItem != nil {
			if r, err = res(pr.RItem.Col); err != nil {
				return err
			}
		}
		p.binds[pr] = binding{l, r}
		return nil
	}); err != nil {
		return nil, err
	}

	// First usable conjunct per column, by kind. Only literals that
	// fit the column type are pushed; column-to-column comparisons and
	// the rest stay residual-only.
	eq := map[int]*Pred{}
	lo := map[int]*Pred{}
	hi := map[int]*Pred{}
	for _, pr := range conjuncts(where) {
		ord := p.binds[pr].l
		if pr.RItem != nil || pr.Val == nil || !litFits(pr.Val, desc.Columns[ord].Type) {
			continue
		}
		switch pr.Op {
		case OpEQ:
			if _, ok := eq[ord]; !ok {
				eq[ord] = pr
			}
		case OpGT, OpGE:
			if _, ok := lo[ord]; !ok {
				lo[ord] = pr
			}
		case OpLT, OpLE:
			if _, ok := hi[ord]; !ok {
				hi[ord] = pr
			}
		}
	}

	// A full-PK equality is a point lookup.
	if k := eqPrefix(desc.PKCols, eq); k == len(desc.PKCols) {
		p.get = make([]any, k)
		for i, ord := range desc.PKCols {
			p.get[i] = eq[ord].Val
			p.pushed = append(p.pushed, eq[ord])
		}
		return p, nil
	}

	// Otherwise score the primary index and every secondary index;
	// equality columns dominate, a bounded range column breaks ties.
	// The primary index wins ties (no entry -> row indirection).
	var bestIdx *bytdb.IndexDesc // nil: the primary index
	bestCols, bestScore := desc.PKCols, pathScore(desc.PKCols, eq, lo, hi)
	for i := range desc.Indexes {
		idx := &desc.Indexes[i]
		if s := pathScore(idx.Cols, eq, lo, hi); s > bestScore {
			bestIdx, bestCols, bestScore = idx, idx.Cols, s
		}
	}
	if bestScore == 0 {
		return p, nil // full scan of the primary index
	}
	if bestIdx != nil {
		p.index = bestIdx.Name
	}
	k := eqPrefix(bestCols, eq)
	for i := 0; i < k; i++ {
		pr := eq[bestCols[i]]
		p.from = append(p.from, pr.Val)
		p.stops = append(p.stops, stop{bestCols[i], stopNE, pr.Val})
		p.pushed = append(p.pushed, pr)
	}
	if k < len(bestCols) {
		if bestIdx != nil && bestIdx.DescAt(k) {
			// A descending key column scans from high to low: the upper
			// bound starts the scan (inclusive; for OpLT the residual
			// filter discards the rows equal to it) and the lower bound
			// stops it.
			if h, ok := hi[bestCols[k]]; ok {
				p.from = append(p.from, h.Val)
				p.pushed = append(p.pushed, h)
			}
			if l, ok := lo[bestCols[k]]; ok {
				kind := stopLE
				if l.Op == OpGE {
					kind = stopLT
				}
				p.stops = append(p.stops, stop{bestCols[k], kind, l.Val})
				p.pushed = append(p.pushed, l)
			}
		} else {
			if l, ok := lo[bestCols[k]]; ok {
				// The bound is inclusive; for OpGT the residual filter
				// discards the rows equal to it.
				p.from = append(p.from, l.Val)
				p.pushed = append(p.pushed, l)
			}
			if h, ok := hi[bestCols[k]]; ok {
				kind := stopGE
				if h.Op == OpLE {
					kind = stopGT
				}
				p.stops = append(p.stops, stop{bestCols[k], kind, h.Val})
				p.pushed = append(p.pushed, h)
			}
		}
	}
	return p, nil
}

// conjuncts flattens the predicates that must hold for every matching
// row: leaves of top-level AND chains. OR and NOT subtrees contribute
// nothing (their predicates are not individually required).
func conjuncts(e BoolExpr) []*Pred {
	switch n := e.(type) {
	case *Pred:
		return []*Pred{n}
	case *And:
		var out []*Pred
		for _, sub := range n.Exprs {
			out = append(out, conjuncts(sub)...)
		}
		return out
	}
	return nil
}

// eqPrefix is the number of leading key columns with an equality
// predicate.
func eqPrefix(cols []int, eq map[int]*Pred) int {
	k := 0
	for k < len(cols) {
		if _, ok := eq[cols[k]]; !ok {
			break
		}
		k++
	}
	return k
}

func pathScore(cols []int, eq, lo, hi map[int]*Pred) int {
	k := eqPrefix(cols, eq)
	score := 4 * k
	if k < len(cols) {
		if _, ok := lo[cols[k]]; ok {
			score++
		}
		if _, ok := hi[cols[k]]; ok {
			score++
		}
	}
	return score
}

// litFits reports whether a parsed literal can be pushed into a scan
// bound for a column of type t — i.e. the engine's coercion accepts
// it. Literals that don't fit are still evaluated by the residual
// filter, just without pushdown.
func litFits(v any, t bytdb.ColType) bool {
	switch t {
	case bytdb.TInt:
		_, ok := v.(int64)
		return ok
	case bytdb.TFloat:
		switch v.(type) {
		case int64, float64:
			return true
		}
	case bytdb.TString:
		_, ok := v.(string)
		return ok
	case bytdb.TBool:
		_, ok := v.(bool)
		return ok
	}
	return false // TBytes: no bytes literals exist in the dialect
}

// chooseOrderedPlan picks the scan path for a single-table SELECT,
// preferring one that reads rows already in ORDER BY order so the sort
// can be skipped. It starts from the ordinary WHERE-driven plan: if
// that path already serves the order (forward, or — when unbounded —
// reversed) it uses it. Otherwise, only when the WHERE pushed nothing
// and a LIMIT bounds the result, it looks for a secondary index whose
// order serves the sort, trading a full scan plus sort for a bounded
// ordered index walk. The second result reports whether the sort can
// be skipped.
func chooseOrderedPlan(desc *bytdb.TableDesc, alias string, where BoolExpr, keys []sortKey, limited bool) (*plan, bool, error) {
	p, err := planScan(desc, alias, where)
	if err != nil {
		return nil, false, err
	}
	if len(keys) == 0 {
		return p, false, nil
	}
	if ok, rev := p.orderSatisfied(desc, keys); ok {
		p.reverse = rev
		return p, true, nil
	}
	// Override only a plain full scan (WHERE chose no path) and only
	// when a LIMIT makes the ordered index walk clearly worthwhile.
	if !limited || p.get != nil || p.index != "" || p.from != nil || len(p.stops) > 0 {
		return p, false, nil
	}
	for i := range desc.Indexes {
		cand := &plan{desc: desc, index: desc.Indexes[i].Name, filter: p.filter, binds: p.binds}
		if ok, rev := cand.orderSatisfied(desc, keys); ok {
			cand.reverse = rev
			return cand, true, nil
		}
	}
	return p, false, nil
}

// orderSatisfied reports whether scanning this plan's access path
// yields rows already in the order the sort keys request, and if so
// whether the scan must run in reverse (descending key order). keys
// carry combined-row ordinals which, for a single table, are the
// table's column ordinals.
//
// Each key column of the path advances a fixed way as the forward scan
// proceeds — ascending for a primary-key or plain index column,
// descending for a DESC index column. A leading run pinned to a
// constant by an equality predicate carries no order and is skipped in
// both the keys and the path; the remaining sort keys must then match
// the remaining path columns one for one, all demanding the same scan
// direction. NULLs are the catch: the key encoding orders them opposite
// to ORDER BY's NULLS LAST/FIRST, so any column that can hold NULL
// cannot have its order served from the scan — every ordering column
// must be NOT NULL (a primary-key column always is). Reverse applies
// only to an unbounded scan; a pushed lower bound or early-stop marks a
// forward-only region.
func (p *plan) orderSatisfied(desc *bytdb.TableDesc, keys []sortKey) (ok, reverse bool) {
	if p.get != nil {
		return false, false
	}
	cols := desc.PKCols
	asc := func(int) bool { return true } // primary-key columns sort ascending
	if p.index != "" {
		idx := desc.Index(p.index)
		if idx == nil {
			return false, false
		}
		cols = idx.Cols
		asc = func(i int) bool { return !idx.DescAt(i) }
	}
	eq := map[int]bool{}
	for _, st := range p.stops {
		if st.kind == stopNE {
			eq[st.ord] = true
		}
	}
	nonNull := func(ord int) bool {
		return slices.Contains(desc.PKCols, ord) || desc.Columns[ord].NotNull
	}
	try := func(rev bool) bool {
		ki := 0
		for _, k := range keys {
			if eq[k.ord] {
				continue // pinned to a constant: contributes no order
			}
			for ki < len(cols) && eq[cols[ki]] {
				ki++
			}
			if ki >= len(cols) || cols[ki] != k.ord || !nonNull(k.ord) {
				return false
			}
			colAsc := asc(ki)
			if rev {
				colAsc = !colAsc
			}
			if k.desc == colAsc { // want descending where the column ascends, or vice versa
				return false
			}
			ki++
		}
		return true
	}
	if try(false) {
		return true, false
	}
	if p.from == nil && len(p.stops) == 0 && try(true) {
		return true, true
	}
	return false, false
}
