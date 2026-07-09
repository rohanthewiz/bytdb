package sql

import (
	"fmt"
	"math"

	"github.com/rohanthewiz/serr"
)

// numParams is the number of values a statement's placeholders
// require: the highest $n it mentions (the same $n may appear more
// than once). DDL never parses a literal, so it is always 0 there.
func numParams(st Statement) int {
	n := 0
	note := func(v any) {
		if p, ok := v.(Param); ok && int(p) > n {
			n = int(p)
		}
	}
	switch s := st.(type) {
	case *Insert:
		for _, row := range s.Rows {
			for _, v := range row {
				note(v)
			}
		}
		if oc := s.Conflict; oc != nil {
			for _, v := range oc.Set {
				note(v)
			}
			for _, ex := range oc.SetEx {
				noteExprVals(ex, note)
			}
			notePredVals(oc.Where, note)
		}
		noteReturningVals(&s.Returning, note)
	case *Update:
		for _, v := range s.Set {
			note(v)
		}
		for _, ex := range s.SetEx {
			noteExprVals(ex, note)
		}
		notePredVals(s.Where, note)
		noteReturningVals(&s.Returning, note)
	case *Delete:
		notePredVals(s.Where, note)
		noteReturningVals(&s.Returning, note)
	case *Select:
		noteSelectVals(s, note)
	case *Explain:
		return numParams(s.Stmt)
	}
	return n
}

// noteSelectVals feeds every literal in a SELECT — predicates,
// expression items, subqueries, union arms — to note.
func noteSelectVals(s *Select, note func(any)) {
	for _, f := range s.From {
		notePredVals(f.On, note)
	}
	notePredVals(s.Where, note)
	notePredVals(s.Having, note)
	for _, it := range s.Items {
		if it.IsLit {
			note(it.Lit)
		}
		noteExprVals(it.Ex, note)
	}
	for _, g := range s.GroupBy {
		noteExprVals(g.Ex, note)
	}
	for _, o := range s.OrderBy {
		noteExprVals(o.Ex, note)
	}
	for _, arm := range s.Union {
		noteSelectVals(arm.Sel, note)
	}
}

// noteReturningVals feeds a RETURNING clause's literals to note —
// items carry the same shapes as select-list entries.
func noteReturningVals(r *Returning, note func(any)) {
	for _, it := range r.RetItems {
		if it.IsLit {
			note(it.Lit)
		}
		noteExprVals(it.Ex, note)
	}
}

// notePredVals feeds every leaf predicate's literal side to note,
// descending into expression leaves.
func notePredVals(e BoolExpr, note func(any)) {
	walkPreds(e, func(pr *Pred) error { note(pr.Val); return nil })
	noteCondVals(e, note)
}

// noteCondVals visits the Cond leaves walkPreds skips.
func noteCondVals(e BoolExpr, note func(any)) {
	switch n := e.(type) {
	case *Cond:
		noteExprVals(n.Ex, note)
	case *Not:
		noteCondVals(n.Expr, note)
	case *And:
		for _, sub := range n.Exprs {
			noteCondVals(sub, note)
		}
	case *Or:
		for _, sub := range n.Exprs {
			noteCondVals(sub, note)
		}
	}
}

func noteExprVals(e Expr, note func(any)) {
	walkExpr(e, func(sub Expr) bool {
		switch n := sub.(type) {
		case *ExLit:
			note(n.Val)
		case *ExSub:
			noteSelectVals(n.Sel, note)
		}
		return true
	})
}

// bindParams replaces every $n placeholder with args[n-1]. It returns
// a copy and leaves the parsed statement untouched, so a prepared
// statement re-binds cleanly on every execution; a statement with no
// placeholders passes through as is.
func bindParams(st Statement, args []any) (Statement, error) {
	n := numParams(st)
	if len(args) != n {
		return nil, serr.New("wrong number of parameters",
			"want", fmt.Sprint(n), "got", fmt.Sprint(len(args)))
	}
	if n == 0 {
		return st, nil
	}
	vals := make([]any, len(args))
	for i, a := range args {
		v, err := bindArg(a)
		if err != nil {
			return nil, serr.Wrap(err, "param", fmt.Sprintf("$%d", i+1))
		}
		vals[i] = v
	}
	sub := func(v any) any {
		if p, ok := v.(Param); ok {
			return vals[p-1]
		}
		return v
	}
	switch s := st.(type) {
	case *Insert:
		c := *s
		c.Rows = make([][]any, len(s.Rows))
		for i, row := range s.Rows {
			r := make([]any, len(row))
			for j, v := range row {
				r[j] = sub(v)
			}
			c.Rows[i] = r
		}
		if oc := s.Conflict; oc != nil {
			oc2 := *oc
			oc2.Set = make(map[string]any, len(oc.Set))
			for k, v := range oc.Set {
				oc2.Set[k] = sub(v)
			}
			oc2.SetEx = make(map[string]Expr, len(oc.SetEx))
			for k, ex := range oc.SetEx {
				oc2.SetEx[k] = bindExpr(ex, sub)
			}
			oc2.Where = bindBool(oc.Where, sub)
			c.Conflict = &oc2
		}
		c.Returning = bindReturning(s.Returning, sub)
		return &c, nil
	case *Update:
		c := *s
		c.Set = make(map[string]any, len(s.Set))
		for k, v := range s.Set {
			c.Set[k] = sub(v)
		}
		c.SetEx = make(map[string]Expr, len(s.SetEx))
		for k, ex := range s.SetEx {
			c.SetEx[k] = bindExpr(ex, sub)
		}
		c.Where = bindBool(s.Where, sub)
		c.Returning = bindReturning(s.Returning, sub)
		return &c, nil
	case *Delete:
		c := *s
		c.Where = bindBool(s.Where, sub)
		c.Returning = bindReturning(s.Returning, sub)
		return &c, nil
	case *Select:
		return bindSelect(s, sub), nil
	case *Explain:
		inner, err := bindParams(s.Stmt, args)
		if err != nil {
			return nil, err
		}
		return &Explain{Stmt: inner}, nil
	}
	return st, nil
}

// bindReturning clones a RETURNING clause with placeholders
// substituted, mirroring bindSelect's item handling.
func bindReturning(r Returning, sub func(any) any) Returning {
	if len(r.RetItems) == 0 {
		return r
	}
	c := r
	c.RetItems = make([]SelectItem, len(r.RetItems))
	copy(c.RetItems, r.RetItems)
	for i := range c.RetItems {
		if c.RetItems[i].IsLit {
			c.RetItems[i].Lit = sub(c.RetItems[i].Lit)
		}
		c.RetItems[i].Ex = bindExpr(c.RetItems[i].Ex, sub)
	}
	return c
}

// bindSelect clones a SELECT with placeholders substituted, including
// expression items, subqueries, and union arms.
func bindSelect(s *Select, sub func(any) any) *Select {
	c := *s
	c.From = make([]FromItem, len(s.From))
	copy(c.From, s.From)
	for i := range c.From {
		c.From[i].On = bindBool(c.From[i].On, sub)
	}
	c.Where = bindBool(s.Where, sub)
	c.Having = bindBool(s.Having, sub)
	c.Items = make([]SelectItem, len(s.Items))
	copy(c.Items, s.Items)
	for i := range c.Items {
		if c.Items[i].IsLit {
			c.Items[i].Lit = sub(c.Items[i].Lit)
		}
		c.Items[i].Ex = bindExpr(c.Items[i].Ex, sub)
	}
	if len(s.GroupBy) > 0 { // preserve nil: it means "not an aggregate"
		c.GroupBy = make([]GroupItem, len(s.GroupBy))
		copy(c.GroupBy, s.GroupBy)
		for i := range c.GroupBy {
			c.GroupBy[i].Ex = bindExpr(c.GroupBy[i].Ex, sub)
		}
	}
	c.OrderBy = make([]OrderItem, len(s.OrderBy))
	copy(c.OrderBy, s.OrderBy)
	for i := range c.OrderBy {
		c.OrderBy[i].Ex = bindExpr(c.OrderBy[i].Ex, sub)
	}
	c.Union = make([]UnionArm, len(s.Union))
	copy(c.Union, s.Union)
	for i := range c.Union {
		c.Union[i].Sel = bindSelect(c.Union[i].Sel, sub)
	}
	return &c
}

// bindBool clones a condition tree, substituting placeholder leaves.
func bindBool(e BoolExpr, sub func(any) any) BoolExpr {
	switch n := e.(type) {
	case nil:
		return nil
	case *Pred:
		c := *n
		c.Val = sub(c.Val)
		return &c
	case *Cond:
		return &Cond{Ex: bindExpr(n.Ex, sub)}
	case *Not:
		return &Not{Expr: bindBool(n.Expr, sub)}
	case *And:
		exprs := make([]BoolExpr, len(n.Exprs))
		for i, s := range n.Exprs {
			exprs[i] = bindBool(s, sub)
		}
		return &And{Exprs: exprs}
	case *Or:
		exprs := make([]BoolExpr, len(n.Exprs))
		for i, s := range n.Exprs {
			exprs[i] = bindBool(s, sub)
		}
		return &Or{Exprs: exprs}
	}
	return e
}

// bindExpr clones an expression with placeholders substituted.
func bindExpr(e Expr, sub func(any) any) Expr {
	switch n := e.(type) {
	case nil:
		return nil
	case *ExLit:
		c := *n
		c.Val = sub(c.Val)
		return &c
	case *ExCol:
		return e
	case *ExAgg:
		if n.Arg == nil {
			return e
		}
		c := *n
		c.Arg = bindExpr(n.Arg, sub)
		return &c
	case *ExAnd:
		c := &ExAnd{Exprs: make([]Expr, len(n.Exprs))}
		for i, s := range n.Exprs {
			c.Exprs[i] = bindExpr(s, sub)
		}
		return c
	case *ExOr:
		c := &ExOr{Exprs: make([]Expr, len(n.Exprs))}
		for i, s := range n.Exprs {
			c.Exprs[i] = bindExpr(s, sub)
		}
		return c
	case *ExNot:
		return &ExNot{E: bindExpr(n.E, sub)}
	case *ExCmp:
		return &ExCmp{Op: n.Op, L: bindExpr(n.L, sub), R: bindExpr(n.R, sub)}
	case *ExAny:
		return &ExAny{Op: n.Op, L: bindExpr(n.L, sub), R: bindExpr(n.R, sub), All: n.All}
	case *ExIsNull:
		return &ExIsNull{E: bindExpr(n.E, sub), Not: n.Not}
	case *ExIn:
		c := &ExIn{E: bindExpr(n.E, sub), Not: n.Not, List: make([]Expr, len(n.List))}
		for i, s := range n.List {
			c.List[i] = bindExpr(s, sub)
		}
		return c
	case *ExCase:
		c := &ExCase{Operand: bindExpr(n.Operand, sub), Else: bindExpr(n.Else, sub),
			Whens: make([]ExWhen, len(n.Whens))}
		for i, w := range n.Whens {
			c.Whens[i] = ExWhen{When: bindExpr(w.When, sub), Then: bindExpr(w.Then, sub)}
		}
		return c
	case *ExFunc:
		c := &ExFunc{Name: n.Name, Args: make([]Expr, len(n.Args))}
		for i, a := range n.Args {
			c.Args[i] = bindExpr(a, sub)
		}
		return c
	case *ExCast:
		return &ExCast{E: bindExpr(n.E, sub), Type: n.Type}
	case *ExIndex:
		return &ExIndex{E: bindExpr(n.E, sub), Idx: bindExpr(n.Idx, sub)}
	case *ExArray:
		c := &ExArray{Elems: make([]Expr, len(n.Elems))}
		for i, el := range n.Elems {
			c.Elems[i] = bindExpr(el, sub)
		}
		return c
	case *ExWindow:
		c := *n
		c.Arg = bindExpr(n.Arg, sub)
		c.Partition = make([]Expr, len(n.Partition))
		for i, e := range n.Partition {
			c.Partition[i] = bindExpr(e, sub)
		}
		c.OrderBy = make([]OrderItem, len(n.OrderBy))
		for i, o := range n.OrderBy {
			o.Ex = bindExpr(o.Ex, sub)
			c.OrderBy[i] = o
		}
		return &c
	case *ExArith:
		return &ExArith{Op: n.Op, L: bindExpr(n.L, sub), R: bindExpr(n.R, sub)}
	case *ExSub:
		return &ExSub{Sel: bindSelect(n.Sel, sub)}
	}
	return e
}

// bindArg normalizes one argument to the value kinds the executor
// compares and the engine stores: int64, float64, string, bool,
// []byte, or nil (SQL NULL).
func bindArg(a any) (any, error) {
	switch v := a.(type) {
	case nil, int64, float64, string, bool, []byte:
		return v, nil
	case int:
		return int64(v), nil
	case int8:
		return int64(v), nil
	case int16:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case uint8:
		return int64(v), nil
	case uint16:
		return int64(v), nil
	case uint32:
		return int64(v), nil
	case uint:
		if uint64(v) > math.MaxInt64 {
			return nil, serr.New("integer parameter overflows int64", "value", fmt.Sprint(v))
		}
		return int64(v), nil
	case uint64:
		if v > math.MaxInt64 {
			return nil, serr.New("integer parameter overflows int64", "value", fmt.Sprint(v))
		}
		return int64(v), nil
	case float32:
		return float64(v), nil
	}
	return nil, serr.New("unsupported parameter type", "type", fmt.Sprintf("%T", a))
}
