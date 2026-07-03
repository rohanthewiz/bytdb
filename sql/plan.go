package sql

import (
	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// plan is the access strategy for one table read: which path to use,
// the lower bound pushed into the ordered scan, where the scan can
// stop early, and the residual filter. Every WHERE predicate stays in
// the residual filter — pushdown only narrows what is visited, so
// correctness never depends on it.
type plan struct {
	desc *bytdb.TableDesc

	get   []any  // full-PK point lookup (values in key order); nil when scanning
	index string // secondary index to scan; "" scans the primary index
	from  []any  // inclusive lower bound pushed into the scan; nil is unbounded
	stops []stop // early-termination checks, in key-column order
	preds []Pred // residual filter: the full WHERE clause
}

type stopKind int

const (
	stopNE stopKind = iota // stop when col != val (an equality prefix has ended)
	stopGE                 // stop when col >= val (pushed: col < val)
	stopGT                 // stop when col > val  (pushed: col <= val)
)

// stop ends a scan at the first row where the column at ordinal ord
// leaves the pushed-down region. NULL column values never trigger a
// range stop: NULL sorts before every value in the key encoding, so a
// NULL here means the scan is still inside (or entering) the region's
// NULL group, which the residual filter discards row by row.
type stop struct {
	ord  int
	kind stopKind
	val  any
}

// planScan validates the WHERE columns and picks the access path:
// a point Get when every primary-key column has an equality predicate,
// else the primary index or the secondary index with the longest
// equality prefix (plus at most one range column), else a full scan.
func planScan(desc *bytdb.TableDesc, preds []Pred) (*plan, error) {
	p := &plan{desc: desc, preds: preds}

	// First usable predicate per column, by kind. Only literals that
	// fit the column type are pushed; the rest stay residual-only.
	eq := map[int]any{}
	lo := map[int]Pred{}
	hi := map[int]Pred{}
	for _, pr := range preds {
		ord := desc.ColIndex(pr.Col)
		if ord < 0 {
			return nil, serr.New("no such column", "table", desc.Name, "column", pr.Col)
		}
		if pr.Val == nil || !litFits(pr.Val, desc.Columns[ord].Type) {
			continue
		}
		switch pr.Op {
		case OpEQ:
			if _, ok := eq[ord]; !ok {
				eq[ord] = pr.Val
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
			p.get[i] = eq[ord]
		}
		return p, nil
	}

	// Otherwise score the primary index and every secondary index;
	// equality columns dominate, a bounded range column breaks ties.
	// The primary index wins ties (no entry -> row indirection).
	bestCols, bestIndex, bestScore := desc.PKCols, "", pathScore(desc.PKCols, eq, lo, hi)
	for i := range desc.Indexes {
		idx := &desc.Indexes[i]
		if s := pathScore(idx.Cols, eq, lo, hi); s > bestScore {
			bestCols, bestIndex, bestScore = idx.Cols, idx.Name, s
		}
	}
	if bestScore == 0 {
		return p, nil // full scan of the primary index
	}
	p.index = bestIndex
	k := eqPrefix(bestCols, eq)
	for i := 0; i < k; i++ {
		v := eq[bestCols[i]]
		p.from = append(p.from, v)
		p.stops = append(p.stops, stop{bestCols[i], stopNE, v})
	}
	if k < len(bestCols) {
		if l, ok := lo[bestCols[k]]; ok {
			// The bound is inclusive; for OpGT the residual filter
			// discards the rows equal to it.
			p.from = append(p.from, l.Val)
		}
		if h, ok := hi[bestCols[k]]; ok {
			kind := stopGE
			if h.Op == OpLE {
				kind = stopGT
			}
			p.stops = append(p.stops, stop{bestCols[k], kind, h.Val})
		}
	}
	return p, nil
}

// eqPrefix is the number of leading key columns with an equality
// predicate.
func eqPrefix(cols []int, eq map[int]any) int {
	k := 0
	for k < len(cols) {
		if _, ok := eq[cols[k]]; !ok {
			break
		}
		k++
	}
	return k
}

func pathScore(cols []int, eq map[int]any, lo, hi map[int]Pred) int {
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
