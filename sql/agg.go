package sql

import (
	"fmt"
	"slices"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

// isAggregate reports whether the query aggregates rows: any
// aggregate in the select list or ORDER BY, a GROUP BY, or a HAVING.
func (s *Select) isAggregate() bool {
	if s.GroupBy != nil || s.Having != nil {
		return true
	}
	for _, it := range s.Items {
		if it.Agg != AggNone {
			return true
		}
	}
	for _, o := range s.OrderBy {
		if o.Agg != AggNone {
			return true
		}
	}
	return false
}

// accum is one aggregate call's running state within one group.
// Aggregates ignore NULL inputs (COUNT(*) counts rows); over zero
// inputs COUNT is 0 and the rest are NULL, per SQL.
type accum struct {
	fn       AggFunc
	ord      int // argument column ordinal; -1 for COUNT(*)
	intSum   bool
	count    int64
	sumI     int64
	sumF     float64
	min, max any
}

func (a *accum) add(vals []any) {
	if a.ord < 0 {
		a.count++ // COUNT(*)
		return
	}
	v := vals[a.ord]
	if v == nil {
		return
	}
	a.count++
	switch a.fn {
	case AggSum, AggAvg:
		switch n := v.(type) {
		case int64:
			a.sumI += n
			a.sumF += float64(n)
		case float64:
			a.sumF += n
		}
	case AggMin:
		if c, ok := compareVals(v, a.min); a.min == nil || (ok && c < 0) {
			a.min = v
		}
	case AggMax:
		if c, ok := compareVals(v, a.max); a.max == nil || (ok && c > 0) {
			a.max = v
		}
	}
}

func (a *accum) value() any {
	switch a.fn {
	case AggCount:
		return a.count
	case AggSum:
		if a.count == 0 {
			return nil
		}
		if a.intSum {
			return a.sumI
		}
		return a.sumF
	case AggAvg:
		if a.count == 0 {
			return nil
		}
		return a.sumF / float64(a.count)
	case AggMin:
		return a.min
	case AggMax:
		return a.max
	}
	return nil
}

// aggQuery is a resolved aggregate SELECT: the accumulator templates
// (one per distinct aggregate call anywhere in the query) and, for
// each output position, HAVING operand, and sort key, a reference to
// either a group-by column or an accumulator.
type aggQuery struct {
	sc        *scope
	groupOrds []int   // combined-row ordinals of the GROUP BY columns
	accums    []accum // templates, cloned per group

	outputs    []aggRef
	having     BoolExpr // leaves resolved via havingRefs
	havingRefs map[*Pred]havingLeaf
	sorts      []aggSortKey
}

// aggRef points at a group-by column (acc < 0) or an accumulator.
type aggRef struct {
	group int // index into groupOrds
	acc   int // index into accums, or -1
}

// havingLeaf is one HAVING predicate's resolved operands; the right
// side is r when hasR, else the leaf's literal.
type havingLeaf struct {
	l, r aggRef
	hasR bool
}

type aggSortKey struct {
	ref  aggRef
	desc bool
}

// resolveAgg validates the aggregate query against the FROM scope.
// Plain columns anywhere in the select list, HAVING, or ORDER BY must
// appear in GROUP BY; SUM and AVG require a numeric argument.
func resolveAgg(sc *scope, s *Select) (*aggQuery, error) {
	if s.Star {
		return nil, serr.New("SELECT * is not allowed in an aggregate query")
	}
	q := &aggQuery{sc: sc}
	groupIdx := map[int]int{} // combined ordinal -> groupOrds index
	for _, col := range s.GroupBy {
		ord, err := sc.resolve(col)
		if err != nil {
			return nil, err
		}
		if _, dup := groupIdx[ord]; dup {
			return nil, serr.New("duplicate GROUP BY column", "column", col.String())
		}
		groupIdx[ord] = len(q.groupOrds)
		q.groupOrds = append(q.groupOrds, ord)
	}

	// ref resolves one item to a group column or a (deduplicated)
	// accumulator.
	type accKey struct {
		fn  AggFunc
		ord int
	}
	accIdx := map[accKey]int{}
	ref := func(it SelectItem) (aggRef, error) {
		if it.IsLit {
			return aggRef{}, serr.New("literals are not supported in aggregate queries")
		}
		if it.Ex != nil {
			return aggRef{}, serr.New("expressions are not supported in aggregate queries")
		}
		if it.Agg == AggNone {
			if it.Star {
				return aggRef{}, serr.New("t.* is not allowed in an aggregate query", "table", it.Col.Table)
			}
			ord, err := sc.resolve(it.Col)
			if err != nil {
				return aggRef{}, err
			}
			gi, ok := groupIdx[ord]
			if !ok {
				return aggRef{}, serr.New("column must appear in GROUP BY or inside an aggregate",
					"column", it.Col.String())
			}
			return aggRef{group: gi, acc: -1}, nil
		}
		ord := -1
		intSum := false
		if !it.Star {
			var err error
			if ord, err = sc.resolve(it.Col); err != nil {
				return aggRef{}, err
			}
			t := sc.column(ord).Type
			if (it.Agg == AggSum || it.Agg == AggAvg) && t != bytdb.TInt && t != bytdb.TFloat {
				return aggRef{}, serr.New(it.Agg.name()+" requires a numeric column",
					"column", it.Col.String(), "type", string(t))
			}
			intSum = t == bytdb.TInt
		}
		k := accKey{it.Agg, ord}
		i, ok := accIdx[k]
		if !ok {
			i = len(q.accums)
			accIdx[k] = i
			q.accums = append(q.accums, accum{fn: it.Agg, ord: ord, intSum: intSum})
		}
		return aggRef{acc: i}, nil
	}

	for _, it := range s.Items {
		r, err := ref(it)
		if err != nil {
			return nil, err
		}
		q.outputs = append(q.outputs, r)
	}
	q.having = s.Having
	q.havingRefs = map[*Pred]havingLeaf{}
	if err := walkPreds(s.Having, func(pr *Pred) error {
		hl := havingLeaf{}
		var err error
		if hl.l, err = ref(pr.Item); err != nil {
			return err
		}
		if pr.RItem != nil {
			if hl.r, err = ref(*pr.RItem); err != nil {
				return err
			}
			hl.hasR = true
		}
		q.havingRefs[pr] = hl
		return nil
	}); err != nil {
		return nil, err
	}
	for _, o := range s.OrderBy {
		if o.IsLit { // ORDER BY n: a select-list position
			n, ok := o.Lit.(int64)
			if !ok {
				return nil, serr.New("non-integer constant in ORDER BY")
			}
			if n < 1 || int(n) > len(q.outputs) {
				return nil, serr.New("ORDER BY position is not in the select list",
					"position", fmt.Sprint(n))
			}
			q.sorts = append(q.sorts, aggSortKey{ref: q.outputs[n-1], desc: o.Desc})
			continue
		}
		r, err := ref(o.SelectItem)
		if err != nil {
			return nil, err
		}
		q.sorts = append(q.sorts, aggSortKey{ref: r, desc: o.Desc})
	}
	return q, nil
}

// resultCols names and types the output columns: group columns keep
// their name and type; aggregates render as fn(col), typed COUNT ->
// int, AVG -> float, SUM/MIN/MAX -> the argument's type.
func (q *aggQuery) resultCols(s *Select, res *Result) {
	for i, it := range s.Items {
		r := q.outputs[i]
		if r.acc < 0 {
			c := q.sc.column(q.groupOrds[r.group])
			res.Cols = append(res.Cols, c.Name)
			res.Types = append(res.Types, c.Type)
			continue
		}
		arg := "*"
		if !it.Star {
			arg = it.Col.String()
		}
		res.Cols = append(res.Cols, it.Agg.name()+"("+arg+")")
		switch {
		case it.Agg == AggCount:
			res.Types = append(res.Types, bytdb.TInt)
		case it.Agg == AggAvg:
			res.Types = append(res.Types, bytdb.TFloat)
		default:
			res.Types = append(res.Types, q.sc.column(q.accums[r.acc].ord).Type)
		}
	}
}

// group is one group's key values and accumulator states.
type group struct {
	keyVals []any
	accs    []accum
}

func (q *aggQuery) newGroup(keyVals []any) *group {
	return &group{keyVals: keyVals, accs: slices.Clone(q.accums)}
}

// valueOf reads a finished group through a reference.
func (q *aggQuery) valueOf(g *group, r aggRef) any {
	if r.acc < 0 {
		return g.keyVals[r.group]
	}
	return g.accs[r.acc].value()
}

func (d *DB) execSelectAgg(s *Select) (*Result, error) {
	res := &Result{}
	var q *aggQuery
	groups := map[string]*group{}
	err := d.read(func(tx *bytdb.Txn) error {
		fp, err := prepareFrom(d.lookup(tx.Table), s.From, s.Where)
		if err != nil {
			return err
		}
		if q, err = resolveAgg(fp.sc, s); err != nil {
			return err
		}
		// Without GROUP BY the whole input is one group, present even
		// over zero rows.
		if len(q.groupOrds) == 0 {
			groups[""] = q.newGroup(nil)
		}
		var scanErr error
		env := &exEnv{d: d, tx: tx, sc: fp.sc}
		err = runJoin(tx, fp, env, func(vals []any) bool {
			keyVals := make([]any, len(q.groupOrds))
			for i, ord := range q.groupOrds {
				keyVals[i] = vals[ord]
			}
			// The order-preserving tuple encoding makes group values a
			// map key (NULLs group together) whose byte order is the
			// groups' semantic order.
			kb, err := tuple.Encode(keyVals...)
			if err != nil {
				scanErr = serr.Wrap(err, "op", "encode group key")
				return false
			}
			g, ok := groups[string(kb)]
			if !ok {
				g = q.newGroup(keyVals)
				groups[string(kb)] = g
			}
			for i := range g.accs {
				g.accs[i].add(vals)
			}
			return true
		})
		if err != nil {
			return err
		}
		return scanErr
	})
	if err != nil {
		return nil, err
	}
	q.resultCols(s, res)

	// Emit groups in key-byte order (ascending group columns), then
	// apply ORDER BY on top.
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	kept := make([]*group, 0, len(groups))
	for _, k := range keys {
		g := groups[k]
		ok, err := evalBool(q.having, func(leaf BoolExpr) (tri, error) {
			pr, isPred := leaf.(*Pred)
			if !isPred {
				return triUnknown, serr.New("expressions are not supported in HAVING")
			}
			hl := q.havingRefs[pr]
			rhs := pr.Val
			if hl.hasR {
				rhs = q.valueOf(g, hl.r)
			}
			return checkPred(q.valueOf(g, hl.l), pr.Op, rhs)
		})
		if err != nil {
			return nil, err
		}
		if ok == triTrue {
			kept = append(kept, g)
		}
	}
	if len(q.sorts) > 0 {
		slices.SortStableFunc(kept, func(a, b *group) int {
			for _, sk := range q.sorts {
				c := orderCmp(q.valueOf(a, sk.ref), q.valueOf(b, sk.ref))
				if sk.desc {
					c = -c
				}
				if c != 0 {
					return c
				}
			}
			return 0
		})
	}
	if s.Offset > 0 {
		if s.Offset >= int64(len(kept)) {
			kept = nil
		} else {
			kept = kept[s.Offset:]
		}
	}
	if s.Limit >= 0 && int64(len(kept)) > s.Limit {
		kept = kept[:s.Limit]
	}
	res.Rows = make([][]any, len(kept))
	for i, g := range kept {
		out := make([]any, len(q.outputs))
		for j, r := range q.outputs {
			out[j] = q.valueOf(g, r)
		}
		res.Rows[i] = out
	}
	return res, nil
}
