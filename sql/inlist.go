package sql

// inlist.go: IN-lists and = ANY(constant array) as key probes
// (deep-look D-38).
//
// WHERE id IN (1, 2, 3) on a primary key was a full scan — 50 ms for ten
// keys against 5 µs for one point get — and it is what ORM eager loading
// emits (SELECT * FROM posts WHERE user_id IN (...)). The planner now
// treats one such conjunct as a set of equalities on its column:
//
//	WHERE user_id IN (9, 3, 3, NULL)        index posts_user (user_id)
//	   │ constant keys → coerce to the column type, drop NULLs,
//	   │ sort in the path column's direction, de-duplicate
//	   ▼
//	keys [3, 9] ──► plan built as if WHERE user_id = <k>   (synthetic Pred)
//	   │
//	   ▼  scanPlan: for each key, set the Pred's Val, rebind, run the scan
//	probe user_id = 3 ──► rows ...
//	probe user_id = 9 ──► rows ...
//
// Because the keys run in the path column's own order, the probes visit
// rows in exactly the order one scan of the path would, so a plan that
// serves ORDER BY keeps serving it — with the IN column treated as an
// ordered column, not one pinned to a constant (orderSatisfied).
//
// The original conjunct stays in the residual filter, as every pushed
// predicate does; constant IN lists are checked there through a hash set
// built once per statement (inListSet) rather than a walk of the list
// per row, which would make N probes cost O(N²).

import (
	"math"
	"slices"
	"strings"

	"github.com/rohanthewiz/bytdb"
)

// inProbe is a plan's multi-key equality.
type inProbe struct {
	ord  int   // the column's ordinal in the table
	pred *Pred // synthetic col = k in the plan's pushed list; Val set per probe
	keys []any // canonical, de-duplicated, in the path column's scan direction
	// cond is the conjunct the probe came from; EXPLAIN reports it as the
	// Index Cond instead of the synthetic equality.
	cond BoolExpr
}

// inCandidate is one pushable IN conjunct before path selection.
type inCandidate struct {
	ord  int
	keys []any // canonical, ascending, de-duplicated
	cond BoolExpr
}

// inCandidates finds the top-level AND conjuncts of where that are
// col IN (constants) or col = ANY(constant array) on a column of this
// table that has no ordinary equality already. Lists whose elements do
// not all convert to the column's key form are skipped: their residual
// comparison is left to decide (and to report a conversion error).
func inCandidates(desc *bytdb.TableDesc, alias string, where BoolExpr, probes []BoolExpr, eq map[int]*Pred) []inCandidate {
	var out []inCandidate
	consider := func(part BoolExpr) {
		col, vals, ok := inShape(part)
		if !ok || (col.Table != "" && col.Table != alias) {
			return
		}
		ord := desc.ColIndex(col.Name)
		if ord < 0 {
			return
		}
		if _, has := eq[ord]; has {
			return // an equality already pins the column; the IN only filters
		}
		keys, ok := inKeys(vals, desc.Columns[ord].Type)
		if !ok {
			return
		}
		out = append(out, inCandidate{ord: ord, keys: keys, cond: part})
	}
	eachPart(where, consider)
	for _, part := range probes {
		consider(part)
	}
	return out
}

// eachPart calls fn on every part of e's top-level AND chain — what
// boolParts returns — without building the slice: planning runs per
// statement, and a WHERE with no IN list should cost it nothing.
func eachPart(e BoolExpr, fn func(BoolExpr)) {
	switch n := e.(type) {
	case nil:
	case *And:
		for _, sub := range n.Exprs {
			eachPart(sub, fn)
		}
	default:
		fn(e)
	}
}

// isInShape reports whether a conjunct can drive IN-list probes and on
// which column (prepareFrom then checks the column is the step's own).
func isInShape(e BoolExpr) (ColRef, bool) {
	col, _, ok := inShape(e)
	return col, ok
}

// inShape recognizes the three spellings of "column equals one of these
// constants" and returns the column and the constants:
//
//	col IN (k1, k2, ...)                     a Cond over ExIn
//	col = ANY(ARRAY[k1, ...]) / ANY('{...}') a Cond over ExAny
//	col = k1 OR col = k2 OR ...              an Or of equality Preds on one column
//
// ok is false for any other shape, a NOT IN, or a non-constant element.
func inShape(e BoolExpr) (col ColRef, vals []any, ok bool) {
	switch n := e.(type) {
	case *Cond:
		switch x := n.Ex.(type) {
		case *ExIn:
			c, isCol := x.E.(*ExCol)
			if !isCol || x.Not {
				return ColRef{}, nil, false
			}
			vals, ok = constListValues(x.List)
			return c.Col, vals, ok
		case *ExAny:
			c, isCol := x.L.(*ExCol)
			if !isCol || x.All || x.Op != OpEQ {
				return ColRef{}, nil, false
			}
			vals, ok = constArrayValues(x.R)
			return c.Col, vals, ok
		}
	case *Or:
		for i, sub := range n.Exprs {
			pr, isPred := sub.(*Pred)
			if !isPred || pr.Op != OpEQ || pr.RItem != nil || pr.Val == nil || !plainColumn(pr.Item) {
				return ColRef{}, nil, false
			}
			if _, isParam := pr.Val.(Param); isParam {
				return ColRef{}, nil, false
			}
			if i == 0 {
				col = pr.Item.Col
			} else if pr.Item.Col != col {
				return ColRef{}, nil, false
			}
			vals = append(vals, pr.Val)
		}
		return col, vals, len(vals) > 0
	}
	return ColRef{}, nil, false
}

// plainColumn reports whether a select item is a bare column reference.
func plainColumn(it SelectItem) bool {
	return it.Ex == nil && !it.IsLit && !it.Star && it.Agg == AggNone
}

// constListValues returns an IN list's values when every element is a
// literal (parameters are literals by execution time).
func constListValues(list []Expr) ([]any, bool) {
	vals := make([]any, len(list))
	for i, e := range list {
		l, ok := e.(*ExLit)
		if !ok {
			return nil, false
		}
		if _, isParam := l.Val.(Param); isParam {
			return nil, false
		}
		vals[i] = l.Val
	}
	return vals, true
}

// constArrayValues returns the elements of a constant ANY operand:
// ARRAY[literals], a bound array value ([]any, or the '{...}' text a
// driver sends), each optionally under an array-type cast. A NULL array
// is not constant here: x = ANY(NULL) is NULL, which the residual filter
// answers.
func constArrayValues(e Expr) ([]any, bool) {
	switch n := e.(type) {
	case *ExArray:
		return constListValues(n.Elems)
	case *ExCast:
		if strings.HasSuffix(n.Type, "[]") {
			return constArrayValues(n.E)
		}
	case *ExLit:
		switch v := n.Val.(type) {
		case []any:
			return v, true
		case string:
			vals, err := parseArrayLiteral(v)
			return vals, err == nil
		}
	}
	return nil, false
}

// inKeys converts IN values to the column's canonical key form, drops
// NULLs (they equal nothing), and sorts and de-duplicates the rest in
// key order. ok is false if any value cannot be a key of type t.
func inKeys(vals []any, t bytdb.ColType) ([]any, bool) {
	keys := make([]any, 0, len(vals))
	for _, v := range vals {
		if v == nil {
			continue
		}
		cv, err := coerceLit(v, t)
		if err != nil {
			return nil, false
		}
		if n, isInt := cv.(int64); isInt && t == bytdb.TFloat {
			cv = float64(n) // one numeric kind, so sorting and dedup compare like with like
		}
		if !litFits(cv, t) {
			return nil, false
		}
		keys = append(keys, cv)
	}
	slices.SortFunc(keys, func(a, b any) int {
		c, _ := compareVals(a, b)
		return c
	})
	keys = slices.CompactFunc(keys, func(a, b any) bool {
		c, ok := compareVals(a, b)
		return ok && c == 0
	})
	return keys, true
}

// accessScore is the best path score an equality/range map affords: a
// full primary-key equality (a point get) beats every scan.
func accessScore(desc *bytdb.TableDesc, eq, lo, hi map[int]*Pred) int {
	if len(desc.PKCols) > 0 && eqPrefix(desc.PKCols, eq) == len(desc.PKCols) {
		return math.MaxInt
	}
	s := pathScore(desc.PKCols, eq, lo, hi)
	for i := range desc.Indexes {
		s = max(s, pathScore(desc.Indexes[i].Cols, eq, lo, hi))
	}
	return s
}

// chooseInProbe picks the IN candidate whose column, treated as an
// equality, improves the best access path the most, adds its synthetic
// equality to eq, and returns the probe (nil if none helps). Only one IN
// conjunct drives probes; any others stay residual filters.
func chooseInProbe(desc *bytdb.TableDesc, alias string, where BoolExpr, probes []BoolExpr, eq, lo, hi map[int]*Pred) *inProbe {
	cands := inCandidates(desc, alias, where, probes, eq)
	if len(cands) == 0 {
		return nil
	}
	best, bestScore := -1, accessScore(desc, eq, lo, hi)
	for i, c := range cands {
		eq[c.ord] = &Pred{} // a stand-in for scoring only
		if s := accessScore(desc, eq, lo, hi); s > bestScore {
			best, bestScore = i, s
		}
		delete(eq, c.ord)
	}
	if best < 0 {
		return nil
	}
	c := cands[best]
	col := desc.Columns[c.ord]
	// The plan is built around the first key; scanPlan rebinds the rest.
	// With no keys at all (every element NULL) nothing can match, and any
	// value of the column's type keeps the plan's shape well formed.
	val := placeholderVal(col.Type)
	if len(c.keys) > 0 {
		val = c.keys[0]
	}
	pr := &Pred{Item: SelectItem{Col: ColRef{Name: col.Name}}, Op: OpEQ, Val: val}
	eq[c.ord] = pr
	return &inProbe{ord: c.ord, pred: pr, keys: c.keys, cond: c.cond}
}

// attachProbe installs probe on a finished plan when the plan actually
// pushed its synthetic equality (a path that ignores the column needs no
// probes: the residual filter alone applies the IN). The keys are put in
// the scan direction of the probed column: ascending, except on a
// descending index column.
func (p *plan) attachProbe(probe *inProbe) {
	if probe == nil || !slices.Contains(p.pushed, probe.pred) {
		return
	}
	if p.index != "" {
		if idx := p.desc.Index(p.index); idx != nil {
			if i := slices.Index(idx.Cols, probe.ord); i >= 0 && idx.DescAt(i) {
				slices.Reverse(probe.keys)
			}
		}
	}
	p.in = probe
}

// inListSet answers x IN (constant list) from a hash set built once per
// statement. handled is false when the set cannot decide — no statement
// memo, a list that is not constant, or values whose kinds differ from
// x's, which need checkPred's coercion — and the caller walks the list.
// Three-valued like the walk: a hit is true; a miss is NULL when the list
// holds a NULL, false otherwise.
func inListSet(env *exEnv, n *ExIn, x any) (result any, handled bool) {
	m := env.subMemo()
	if m == nil || len(n.List) < 4 { // a short list is as fast to walk
		return nil, false
	}
	set, ok := m.lists[n]
	if !ok {
		vals, isConst := constListValues(n.List)
		if isConst {
			set = buildSubSet(vals)
		} // else set stays nil: remembered as "not constant"
		if m.lists == nil {
			m.lists = map[*ExIn]*subSet{}
		}
		m.lists[n] = set
	}
	if set == nil || !set.usable {
		return nil, false
	}
	if classOf(x) != set.class && len(set.keys) > 0 {
		return nil, false
	}
	k, err := hashKey(x)
	if err != nil {
		return nil, false
	}
	if _, hit := set.keys[string(k)]; hit {
		return true, true
	}
	if set.hasNull {
		return nil, true
	}
	return false, true
}
