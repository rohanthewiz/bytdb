package sql

import (
	"fmt"
	"slices"
	"strings"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

// agg.go: aggregate queries. GROUP BY keys are expressions (a plain
// column is the common case); every select item, HAVING condition,
// and sort key is rewritten against them — a subtree that matches a
// key becomes a reference to the group's key value, an aggregate call
// becomes a reference to an accumulator, and any column left over is
// the classic "must appear in the GROUP BY clause" error. After
// grouping, the rewritten expressions evaluate per group through the
// ordinary evaluator, so functions, CASE, arithmetic, and casts all
// work on top of grouped data.

// isAggregate reports whether the query aggregates rows: any
// aggregate in the select list or ORDER BY (including inside an
// expression), a GROUP BY, or a HAVING.
func (s *Select) isAggregate() bool {
	if s.GroupBy != nil || s.Having != nil {
		return true
	}
	for _, it := range s.Items {
		if it.Agg != AggNone || findAgg(it.Ex) != AggNone {
			return true
		}
	}
	for _, o := range s.OrderBy {
		if o.Agg != AggNone || findAgg(o.Ex) != AggNone {
			return true
		}
	}
	return false
}

// accum is one aggregate call's running state within one group.
// The argument is a column ordinal, an expression evaluated per input
// row, or neither for COUNT(*). Aggregates ignore NULL inputs
// (COUNT(*) counts rows); over zero inputs COUNT is 0 and the rest
// are NULL, per SQL. A distinct accumulator consumes each distinct
// value once, deduplicated by the order-preserving tuple encoding
// (the same equivalence GROUP BY uses).
type accum struct {
	fn       AggFunc
	ord      int  // argument column ordinal; -1 when argEx or COUNT(*)
	argEx    Expr // expression argument; nil for a column or COUNT(*)
	distinct bool
	seen     map[string]bool // per group; allocated by newGroup
	intSum   bool
	count    int64
	sumI     int64
	sumF     float64
	min, max any
}

func (a *accum) add(env *exEnv, vals []any) error {
	var v any
	switch {
	case a.argEx != nil:
		var err error
		if v, err = evalEx(env, a.argEx); err != nil {
			return err
		}
	case a.ord >= 0:
		v = vals[a.ord]
	default:
		a.count++ // COUNT(*)
		return nil
	}
	if v == nil {
		return nil
	}
	if a.distinct {
		kb, err := tuple.Encode(v)
		if err != nil {
			return serr.Wrap(err, "op", "encode DISTINCT value")
		}
		if a.seen[string(kb)] {
			return nil
		}
		a.seen[string(kb)] = true
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
		default:
			return serr.New(a.fn.name()+" requires a numeric argument",
				"got", fmt.Sprintf("%T", v))
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
	return nil
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

// aggKey is one resolved GROUP BY key.
type aggKey struct {
	ex  Expr
	ord int // combined-row ordinal when ex is a plain column; else -1
	typ bytdb.ColType
	txt string // canonical text, for matching item subtrees
}

type aggSortKey struct {
	ex   Expr // rewritten: evaluates per group
	desc bool
}

// aggQuery is a resolved aggregate SELECT: the group keys, the
// accumulator templates (one per distinct aggregate call anywhere in
// the query), and the rewritten output, HAVING, and sort expressions.
type aggQuery struct {
	sc     *scope
	keys   []aggKey
	accums []accum

	outs   []Expr
	having Expr // nil: all groups
	sorts  []aggSortKey
}

// exprKey renders an expression canonically for GROUP BY matching:
// columns by resolved ordinal (so city and users.city match),
// literals tagged by type. Aggregates and subqueries render uniquely
// by pointer, so they never match a key.
func exprKey(sc *scope, e Expr) (string, error) {
	var b strings.Builder
	if err := writeExprKey(&b, sc, e); err != nil {
		return "", err
	}
	return b.String(), nil
}

func writeExprKey(b *strings.Builder, sc *scope, e Expr) error {
	list := func(op string, subs ...Expr) error {
		b.WriteString(op + "(")
		for _, sub := range subs {
			if err := writeExprKey(b, sc, sub); err != nil {
				return err
			}
			b.WriteByte(',')
		}
		b.WriteByte(')')
		return nil
	}
	switch n := e.(type) {
	case nil:
		b.WriteByte('~')
	case *ExLit:
		fmt.Fprintf(b, "l(%T:%v)", n.Val, n.Val)
	case *ExCol:
		ord, err := sc.resolve(n.Col)
		if err != nil {
			return err
		}
		fmt.Fprintf(b, "c%d", ord)
	case *ExAgg, *ExSub:
		fmt.Fprintf(b, "!%p", e)
	case *ExAnd:
		return list("and", n.Exprs...)
	case *ExOr:
		return list("or", n.Exprs...)
	case *ExNot:
		return list("not", n.E)
	case *ExCmp:
		return list(fmt.Sprintf("cmp%d", n.Op), n.L, n.R)
	case *ExAny:
		return list(fmt.Sprintf("any%d/%v", n.Op, n.All), n.L, n.R)
	case *ExIsNull:
		return list(fmt.Sprintf("isnull%v", n.Not), n.E)
	case *ExIn:
		return list(fmt.Sprintf("in%v", n.Not), append([]Expr{n.E}, n.List...)...)
	case *ExCase:
		subs := []Expr{n.Operand}
		for _, w := range n.Whens {
			subs = append(subs, w.When, w.Then)
		}
		return list("case", append(subs, n.Else)...)
	case *ExFunc:
		return list("fn:"+n.Name, n.Args...)
	case *ExCast:
		return list("cast:"+n.Type, n.E)
	case *ExIndex:
		return list("idx", n.E, n.Idx)
	case *ExArray:
		return list("array", n.Elems...)
	case *ExArith:
		return list("op"+n.Op, n.L, n.R)
	default:
		fmt.Fprintf(b, "!%p", e)
	}
	return nil
}

// aggItemExpr renders a select item as an expression, aggregate
// shapes included.
func aggItemExpr(it SelectItem) (Expr, error) {
	if it.Agg != AggNone {
		return &ExAgg{Fn: it.Agg, Col: it.Col, Star: it.Star}, nil
	}
	if it.Star {
		return nil, serr.New("t.* is not allowed in an aggregate query", "table", it.Col.Table)
	}
	return itemToExpr(it), nil
}

// resolveAgg validates the aggregate query against the FROM scope and
// rewrites its expressions into group-key and accumulator references.
func resolveAgg(sc *scope, s *Select) (*aggQuery, error) {
	if s.Star {
		return nil, serr.New("SELECT * is not allowed in an aggregate query")
	}
	q := &aggQuery{sc: sc}
	for _, g := range s.GroupBy {
		var ex Expr
		switch {
		case g.Pos > 0: // GROUP BY n: a select-list position
			if g.Pos > int64(len(s.Items)) {
				return nil, serr.New("GROUP BY position is not in the select list",
					"position", fmt.Sprint(g.Pos))
			}
			var err error
			if ex, err = aggItemExpr(s.Items[g.Pos-1]); err != nil {
				return nil, err
			}
		case g.Ex != nil:
			ex = g.Ex
		default:
			ex = &ExCol{Col: g.Col}
		}
		if findAgg(ex) != AggNone {
			return nil, serr.New("aggregate functions are not allowed in GROUP BY")
		}
		txt, err := exprKey(sc, ex)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(q.keys, func(k aggKey) bool { return k.txt == txt }) {
			return nil, serr.New("duplicate GROUP BY expression")
		}
		ord := -1
		if c, ok := ex.(*ExCol); ok {
			ord, _ = sc.resolve(c.Col) // exprKey above already resolved it
		}
		q.keys = append(q.keys, aggKey{ex: ex, ord: ord, typ: exprType(sc, ex), txt: txt})
	}

	for _, it := range s.Items {
		ex, err := aggItemExpr(it)
		if err != nil {
			return nil, err
		}
		r, err := q.rewrite(ex)
		if err != nil {
			return nil, err
		}
		q.outs = append(q.outs, r)
	}

	hex, err := havingToExpr(s.Having)
	if err != nil {
		return nil, err
	}
	if q.having, err = q.rewrite(hex); err != nil {
		return nil, err
	}

	for _, o := range s.OrderBy {
		if o.IsLit { // ORDER BY n: a select-list position
			n, ok := o.Lit.(int64)
			if !ok {
				return nil, serr.New("non-integer constant in ORDER BY")
			}
			if n < 1 || int(n) > len(q.outs) {
				return nil, serr.New("ORDER BY position is not in the select list",
					"position", fmt.Sprint(n))
			}
			q.sorts = append(q.sorts, aggSortKey{ex: q.outs[n-1], desc: o.Desc})
			continue
		}
		// A bare name matching an output alias sorts by that output.
		if o.Ex == nil && o.Agg == AggNone && !o.Star && o.Col.Table == "" {
			if i := slices.IndexFunc(s.Items, func(it SelectItem) bool {
				return it.As == o.Col.Name
			}); i >= 0 {
				q.sorts = append(q.sorts, aggSortKey{ex: q.outs[i], desc: o.Desc})
				continue
			}
		}
		ex, err := aggItemExpr(o.SelectItem)
		if err != nil {
			return nil, err
		}
		r, err := q.rewrite(ex)
		if err != nil {
			return nil, err
		}
		q.sorts = append(q.sorts, aggSortKey{ex: r, desc: o.Desc})
	}
	return q, nil
}

// rewrite maps an expression onto the group phase: a subtree matching
// a GROUP BY key becomes a key-value reference, an aggregate call an
// accumulator reference, and any other column reference is an error.
// Literals and subqueries pass through; composites rebuild around
// their rewritten children.
func (q *aggQuery) rewrite(e Expr) (Expr, error) {
	if e == nil {
		return nil, nil
	}
	// Unresolvable columns fall through to the per-node handling,
	// which reports the right error.
	if txt, err := exprKey(q.sc, e); err == nil {
		for i, k := range q.keys {
			if k.txt == txt {
				return &exGroupRef{idx: i, typ: k.typ}, nil
			}
		}
	}
	sub := func(subs ...*Expr) error {
		for _, s := range subs {
			r, err := q.rewrite(*s)
			if err != nil {
				return err
			}
			*s = r
		}
		return nil
	}
	switch n := e.(type) {
	case *ExLit, *ExSub:
		// A subquery evaluates per group with only the enclosing
		// scopes' columns; references to this query's ungrouped
		// columns fail at evaluation.
		return e, nil
	case *ExCol:
		if _, err := q.sc.resolve(n.Col); err != nil {
			return nil, err
		}
		return nil, serr.New(`column "` + n.Col.String() +
			`" must appear in the GROUP BY clause or be used in an aggregate function`)
	case *ExAgg:
		i, err := q.accumFor(n)
		if err != nil {
			return nil, err
		}
		return &exAccRef{idx: i, typ: q.accumType(n, i)}, nil
	case *ExAnd:
		c := &ExAnd{Exprs: slices.Clone(n.Exprs)}
		for i := range c.Exprs {
			if err := sub(&c.Exprs[i]); err != nil {
				return nil, err
			}
		}
		return c, nil
	case *ExOr:
		c := &ExOr{Exprs: slices.Clone(n.Exprs)}
		for i := range c.Exprs {
			if err := sub(&c.Exprs[i]); err != nil {
				return nil, err
			}
		}
		return c, nil
	case *ExNot:
		c := *n
		return &c, sub(&c.E)
	case *ExCmp:
		c := *n
		return &c, sub(&c.L, &c.R)
	case *ExAny:
		c := *n
		return &c, sub(&c.L, &c.R)
	case *ExIsNull:
		c := *n
		return &c, sub(&c.E)
	case *ExIn:
		c := *n
		c.List = slices.Clone(n.List)
		if err := sub(&c.E); err != nil {
			return nil, err
		}
		for i := range c.List {
			if err := sub(&c.List[i]); err != nil {
				return nil, err
			}
		}
		return &c, nil
	case *ExCase:
		c := *n
		c.Whens = slices.Clone(n.Whens)
		if err := sub(&c.Operand, &c.Else); err != nil {
			return nil, err
		}
		for i := range c.Whens {
			if err := sub(&c.Whens[i].When, &c.Whens[i].Then); err != nil {
				return nil, err
			}
		}
		return &c, nil
	case *ExFunc:
		c := *n
		c.Args = slices.Clone(n.Args)
		for i := range c.Args {
			if err := sub(&c.Args[i]); err != nil {
				return nil, err
			}
		}
		return &c, nil
	case *ExCast:
		c := *n
		return &c, sub(&c.E)
	case *ExIndex:
		c := *n
		return &c, sub(&c.E, &c.Idx)
	case *ExArray:
		c := *n
		c.Elems = slices.Clone(n.Elems)
		for i := range c.Elems {
			if err := sub(&c.Elems[i]); err != nil {
				return nil, err
			}
		}
		return &c, nil
	case *ExArith:
		c := *n
		return &c, sub(&c.L, &c.R)
	}
	return e, nil
}

// accumFor finds or creates the accumulator for one aggregate call;
// identical calls share one. SUM and AVG over a column require a
// numeric column; expression arguments are checked at evaluation.
func (q *aggQuery) accumFor(n *ExAgg) (int, error) {
	a := accum{fn: n.Fn, ord: -1, distinct: n.Distinct}
	key := "*"
	switch {
	case n.Arg != nil:
		if findAgg(n.Arg) != AggNone {
			return 0, serr.New("aggregate function calls cannot be nested")
		}
		txt, err := exprKey(q.sc, n.Arg)
		if err != nil {
			return 0, err
		}
		a.argEx, a.intSum = n.Arg, exprType(q.sc, n.Arg) == bytdb.TInt
		key = "e:" + txt
	case !n.Star:
		ord, err := q.sc.resolve(n.Col)
		if err != nil {
			return 0, err
		}
		t := q.sc.column(ord).Type
		if (n.Fn == AggSum || n.Fn == AggAvg) && t != bytdb.TInt && t != bytdb.TFloat {
			return 0, serr.New(n.Fn.name()+" requires a numeric column",
				"column", n.Col.String(), "type", string(t))
		}
		a.ord, a.intSum = ord, t == bytdb.TInt
		key = fmt.Sprintf("c%d", ord)
	}
	key = fmt.Sprintf("%d|%v|%s", n.Fn, n.Distinct, key)
	for i, ex := range q.accums {
		got := fmt.Sprintf("%d|%v|", ex.fn, ex.distinct)
		switch {
		case ex.argEx != nil:
			txt, _ := exprKey(q.sc, ex.argEx)
			got += "e:" + txt
		case ex.ord >= 0:
			got += fmt.Sprintf("c%d", ex.ord)
		default:
			got += "*"
		}
		if got == key {
			return i, nil
		}
	}
	q.accums = append(q.accums, a)
	return len(q.accums) - 1, nil
}

// accumType is the result type of accumulator i for call n: COUNT ->
// int, AVG -> float, SUM/MIN/MAX -> the argument's type (float for
// SUM over an expression that is not statically integer).
func (q *aggQuery) accumType(n *ExAgg, i int) bytdb.ColType {
	switch n.Fn {
	case AggCount:
		return bytdb.TInt
	case AggAvg:
		return bytdb.TFloat
	}
	a := q.accums[i]
	switch {
	case a.argEx != nil:
		if n.Fn == AggSum && !a.intSum {
			return bytdb.TFloat
		}
		return exprType(q.sc, a.argEx)
	case a.ord >= 0:
		return q.sc.column(a.ord).Type
	}
	return bytdb.TInt
}

// havingToExpr renders a HAVING tree as one expression; Pred leaves
// may carry aggregate items.
func havingToExpr(b BoolExpr) (Expr, error) {
	rebuild := func(subs []BoolExpr) ([]Expr, error) {
		out := make([]Expr, len(subs))
		for i, s := range subs {
			var err error
			if out[i], err = havingToExpr(s); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	switch n := b.(type) {
	case nil:
		return nil, nil
	case *Pred:
		l, err := aggItemExpr(n.Item)
		if err != nil {
			return nil, err
		}
		switch n.Op {
		case OpIsNull:
			return &ExIsNull{E: l}, nil
		case OpIsNotNull:
			return &ExIsNull{E: l, Not: true}, nil
		}
		var r Expr
		if n.RItem != nil {
			if r, err = aggItemExpr(*n.RItem); err != nil {
				return nil, err
			}
		} else {
			r = &ExLit{Val: n.Val}
		}
		return &ExCmp{Op: n.Op, L: l, R: r}, nil
	case *Cond:
		return n.Ex, nil
	case *Not:
		sub, err := havingToExpr(n.Expr)
		if err != nil {
			return nil, err
		}
		return &ExNot{E: sub}, nil
	case *And:
		subs, err := rebuild(n.Exprs)
		if err != nil {
			return nil, err
		}
		return &ExAnd{Exprs: subs}, nil
	case *Or:
		subs, err := rebuild(n.Exprs)
		if err != nil {
			return nil, err
		}
		return &ExOr{Exprs: subs}, nil
	}
	return nil, serr.New("unhandled HAVING shape")
}

// resultCols names and types the output columns: group columns keep
// their name, aggregates over columns render as fn(col), expression
// items name like their non-aggregate counterparts; [AS] wins.
func (q *aggQuery) resultCols(s *Select, res *Result) {
	for i, it := range s.Items {
		name := it.As
		if name == "" {
			switch {
			case it.Agg != AggNone:
				arg := "*"
				if !it.Star {
					arg = it.Col.String()
				}
				name = it.Agg.name() + "(" + arg + ")"
			case it.IsLit:
				name = it.LitName
			case it.Ex != nil:
				name = exprName(it.Ex)
			default:
				name = it.Col.Name
			}
		}
		res.Cols = append(res.Cols, name)
		res.Types = append(res.Types, exprType(q.sc, q.outs[i]))
	}
}

// group is one group's key values and accumulator states.
type group struct {
	keyVals []any
	accs    []accum
}

func (q *aggQuery) newGroup(keyVals []any) *group {
	g := &group{keyVals: keyVals, accs: slices.Clone(q.accums)}
	for i := range g.accs {
		if g.accs[i].distinct { // dedup state is per group, not shared
			g.accs[i].seen = map[string]bool{}
		}
	}
	return g
}

func (d *DB) execSelectAgg(s *Select) (*Result, error) {
	res := &Result{}
	err := d.read(func(tx *bytdb.Txn) error {
		fp, err := prepareFrom(d.lookup(tx.Table), s.From, s.Where)
		if err != nil {
			return err
		}
		q, err := resolveAgg(fp.sc, s)
		if err != nil {
			return err
		}
		q.resultCols(s, res)
		groups := map[string]*group{}
		// Without GROUP BY the whole input is one group, present even
		// over zero rows.
		if len(q.keys) == 0 {
			groups[""] = q.newGroup(nil)
		}
		env := &exEnv{d: d, tx: tx, sc: fp.sc}
		var scanErr error
		err = runJoin(tx, fp, env, func(vals []any) bool {
			env.row = vals
			keyVals := make([]any, len(q.keys))
			for i, k := range q.keys {
				if k.ord >= 0 {
					keyVals[i] = vals[k.ord]
					continue
				}
				if keyVals[i], scanErr = evalEx(env, k.ex); scanErr != nil {
					return false
				}
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
				if scanErr = g.accs[i].add(env, vals); scanErr != nil {
					return false
				}
			}
			return true
		})
		if err != nil {
			return err
		}
		if scanErr != nil {
			return scanErr
		}

		// Group phase: rewritten expressions read the current group
		// through env.grp. Emit in key-byte order (ascending group
		// columns), then apply ORDER BY on top.
		env.row = nil
		keys := make([]string, 0, len(groups))
		for k := range groups {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		kept := make([]*group, 0, len(groups))
		for _, k := range keys {
			g := groups[k]
			env.grp = g
			if q.having != nil {
				t, err := evalTruth(env, q.having)
				if err != nil {
					return err
				}
				if t != triTrue {
					continue
				}
			}
			kept = append(kept, g)
		}
		if len(q.sorts) > 0 {
			sortVals := make(map[*group][]any, len(kept))
			for _, g := range kept {
				env.grp = g
				vs := make([]any, len(q.sorts))
				for i, sk := range q.sorts {
					var err error
					if vs[i], err = evalEx(env, sk.ex); err != nil {
						return err
					}
				}
				sortVals[g] = vs
			}
			slices.SortStableFunc(kept, func(a, b *group) int {
				for i, sk := range q.sorts {
					c := orderCmp(sortVals[a][i], sortVals[b][i])
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
			env.grp = g
			out := make([]any, len(q.outs))
			for j, ex := range q.outs {
				var err error
				if out[j], err = evalEx(env, ex); err != nil {
					return err
				}
			}
			res.Rows[i] = out
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}
