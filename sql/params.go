package sql

import (
	"fmt"
	"math"
	"time"

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
				// An expression value carries its placeholders inside the
				// tree (as ExLit leaves); a plain value may itself be one.
				if ex, ok := v.(Expr); ok {
					noteExprVals(ex, note)
				} else {
					note(v)
				}
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
// expression items, subqueries, union arms, WITH bodies — to note.
func noteSelectVals(s *Select, note func(any)) {
	for _, c := range s.With {
		noteSelectVals(c.Sel, note)
	}
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
	// LIMIT/OFFSET placeholders live outside the expression fields the
	// walks above cover — they are the statement's only value positions
	// held as bare Params rather than ExLit leaves.
	if s.LimitParam != 0 {
		note(s.LimitParam)
	}
	if s.OffsetParam != 0 {
		note(s.OffsetParam)
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
	b := &binder{vals: vals}
	// Every arm assigns into out rather than returning, so the one
	// error check below covers them all: a bad LIMIT/OFFSET value can
	// surface from any statement shape (a subquery deep inside an
	// UPDATE's WHERE binds through the same walk).
	out := st
	switch s := st.(type) {
	case *Insert:
		c := *s
		c.Rows = make([][]any, len(s.Rows))
		for i, row := range s.Rows {
			r := make([]any, len(row))
			for j, v := range row {
				// Expression values re-bind as expressions do everywhere
				// else: a cloned tree with ExLit placeholders substituted.
				if ex, ok := v.(Expr); ok {
					r[j] = bindExpr(ex, b)
				} else {
					r[j] = b.sub(v)
				}
			}
			c.Rows[i] = r
		}
		if oc := s.Conflict; oc != nil {
			oc2 := *oc
			oc2.Set = make(map[string]any, len(oc.Set))
			for k, v := range oc.Set {
				oc2.Set[k] = b.sub(v)
			}
			oc2.SetEx = make(map[string]Expr, len(oc.SetEx))
			for k, ex := range oc.SetEx {
				oc2.SetEx[k] = bindExpr(ex, b)
			}
			oc2.Where = bindBool(oc.Where, b)
			c.Conflict = &oc2
		}
		c.Returning = bindReturning(s.Returning, b)
		out = &c
	case *Update:
		c := *s
		c.Set = make(map[string]any, len(s.Set))
		for k, v := range s.Set {
			c.Set[k] = b.sub(v)
		}
		c.SetEx = make(map[string]Expr, len(s.SetEx))
		for k, ex := range s.SetEx {
			c.SetEx[k] = bindExpr(ex, b)
		}
		c.Where = bindBool(s.Where, b)
		c.Returning = bindReturning(s.Returning, b)
		out = &c
	case *Delete:
		c := *s
		c.Where = bindBool(s.Where, b)
		c.Returning = bindReturning(s.Returning, b)
		out = &c
	case *Select:
		out = bindSelect(s, b)
	case *Explain:
		inner, err := bindParams(s.Stmt, args)
		if err != nil {
			return nil, err
		}
		out = &Explain{Stmt: inner}
	}
	if b.err != nil {
		return nil, b.err
	}
	return out, nil
}

// binder carries one execution's normalized argument values through
// the clone-and-substitute walk. Substitution itself cannot fail, so
// the walk helpers stay error-free; the few value positions that
// demand a specific shape once bound (LIMIT/OFFSET counts) record
// their complaint in err, checked once when bindParams finishes.
type binder struct {
	vals []any
	err  error
}

// sub replaces a Param marker with its bound value; any other value
// passes through untouched.
func (b *binder) sub(v any) any {
	if p, ok := v.(Param); ok {
		return b.vals[p-1]
	}
	return v
}

// count resolves a LIMIT/OFFSET placeholder. NULL means the clause's
// no-op default — Postgres reads LIMIT NULL as "no limit" and OFFSET
// NULL as "skip nothing"; anything else must be a non-negative
// integer, the same rule the grammar enforces for inline counts.
func (b *binder) count(p Param, clause string, nullDefault int64) int64 {
	switch v := b.vals[p-1].(type) {
	case nil:
		return nullDefault
	case int64:
		if v >= 0 {
			return v
		}
		if b.err == nil {
			b.err = serr.New(clause+" must not be negative",
				"param", fmt.Sprintf("$%d", p), "got", fmt.Sprint(v))
		}
	default:
		if b.err == nil {
			b.err = serr.New(clause+" requires a non-negative integer",
				"param", fmt.Sprintf("$%d", p), "got", fmt.Sprintf("%v (%T)", v, v))
		}
	}
	return nullDefault
}

// bindReturning clones a RETURNING clause with placeholders
// substituted, mirroring bindSelect's item handling.
func bindReturning(r Returning, b *binder) Returning {
	if len(r.RetItems) == 0 {
		return r
	}
	c := r
	c.RetItems = make([]SelectItem, len(r.RetItems))
	copy(c.RetItems, r.RetItems)
	for i := range c.RetItems {
		if c.RetItems[i].IsLit {
			c.RetItems[i].Lit = b.sub(c.RetItems[i].Lit)
		}
		c.RetItems[i].Ex = bindExpr(c.RetItems[i].Ex, b)
	}
	return c
}

// bindSelect clones a SELECT with placeholders substituted, including
// expression items, subqueries, and union arms.
func bindSelect(s *Select, b *binder) *Select {
	c := *s
	if len(s.With) > 0 {
		c.With = make([]CTE, len(s.With))
		copy(c.With, s.With)
		for i := range c.With {
			c.With[i].Sel = bindSelect(c.With[i].Sel, b)
		}
	}
	c.From = make([]FromItem, len(s.From))
	copy(c.From, s.From)
	for i := range c.From {
		c.From[i].On = bindBool(c.From[i].On, b)
	}
	c.Where = bindBool(s.Where, b)
	c.Having = bindBool(s.Having, b)
	c.Items = make([]SelectItem, len(s.Items))
	copy(c.Items, s.Items)
	for i := range c.Items {
		if c.Items[i].IsLit {
			c.Items[i].Lit = b.sub(c.Items[i].Lit)
		}
		c.Items[i].Ex = bindExpr(c.Items[i].Ex, b)
	}
	if len(s.GroupBy) > 0 { // preserve nil: it means "not an aggregate"
		c.GroupBy = make([]GroupItem, len(s.GroupBy))
		copy(c.GroupBy, s.GroupBy)
		for i := range c.GroupBy {
			c.GroupBy[i].Ex = bindExpr(c.GroupBy[i].Ex, b)
		}
	}
	c.OrderBy = make([]OrderItem, len(s.OrderBy))
	copy(c.OrderBy, s.OrderBy)
	for i := range c.OrderBy {
		c.OrderBy[i].Ex = bindExpr(c.OrderBy[i].Ex, b)
	}
	c.Union = make([]UnionArm, len(s.Union))
	copy(c.Union, s.Union)
	for i := range c.Union {
		c.Union[i].Sel = bindSelect(c.Union[i].Sel, b)
	}
	// LIMIT/OFFSET placeholders resolve into their int64 twins here,
	// on the per-execution copy — everything downstream (sorting,
	// slicing, EXPLAIN) keeps reading plain integers.
	if c.LimitParam != 0 {
		c.Limit = b.count(c.LimitParam, "LIMIT", -1)
		c.LimitParam = 0
	}
	if c.OffsetParam != 0 {
		c.Offset = b.count(c.OffsetParam, "OFFSET", 0)
		c.OffsetParam = 0
	}
	return &c
}

// bindBool clones a condition tree, substituting placeholder leaves.
func bindBool(e BoolExpr, b *binder) BoolExpr {
	switch n := e.(type) {
	case nil:
		return nil
	case *Pred:
		c := *n
		c.Val = b.sub(c.Val)
		return &c
	case *Cond:
		return &Cond{Ex: bindExpr(n.Ex, b)}
	case *Not:
		return &Not{Expr: bindBool(n.Expr, b)}
	case *And:
		exprs := make([]BoolExpr, len(n.Exprs))
		for i, s := range n.Exprs {
			exprs[i] = bindBool(s, b)
		}
		return &And{Exprs: exprs}
	case *Or:
		exprs := make([]BoolExpr, len(n.Exprs))
		for i, s := range n.Exprs {
			exprs[i] = bindBool(s, b)
		}
		return &Or{Exprs: exprs}
	}
	return e
}

// bindExpr clones an expression with placeholders substituted.
func bindExpr(e Expr, b *binder) Expr {
	switch n := e.(type) {
	case nil:
		return nil
	case *ExLit:
		c := *n
		c.Val = b.sub(c.Val)
		return &c
	case *ExCol:
		return e
	case *ExAgg:
		if n.Arg == nil {
			return e
		}
		c := *n
		c.Arg = bindExpr(n.Arg, b)
		return &c
	case *ExAnd:
		c := &ExAnd{Exprs: make([]Expr, len(n.Exprs))}
		for i, s := range n.Exprs {
			c.Exprs[i] = bindExpr(s, b)
		}
		return c
	case *ExOr:
		c := &ExOr{Exprs: make([]Expr, len(n.Exprs))}
		for i, s := range n.Exprs {
			c.Exprs[i] = bindExpr(s, b)
		}
		return c
	case *ExNot:
		return &ExNot{E: bindExpr(n.E, b)}
	case *ExCmp:
		return &ExCmp{Op: n.Op, L: bindExpr(n.L, b), R: bindExpr(n.R, b)}
	case *ExAny:
		return &ExAny{Op: n.Op, L: bindExpr(n.L, b), R: bindExpr(n.R, b), All: n.All}
	case *ExIsNull:
		return &ExIsNull{E: bindExpr(n.E, b), Not: n.Not}
	case *ExIn:
		c := &ExIn{E: bindExpr(n.E, b), Not: n.Not, List: make([]Expr, len(n.List))}
		for i, s := range n.List {
			c.List[i] = bindExpr(s, b)
		}
		return c
	case *ExCase:
		c := &ExCase{Operand: bindExpr(n.Operand, b), Else: bindExpr(n.Else, b),
			Whens: make([]ExWhen, len(n.Whens))}
		for i, w := range n.Whens {
			c.Whens[i] = ExWhen{When: bindExpr(w.When, b), Then: bindExpr(w.Then, b)}
		}
		return c
	case *ExFunc:
		c := &ExFunc{Name: n.Name, Args: make([]Expr, len(n.Args))}
		for i, a := range n.Args {
			c.Args[i] = bindExpr(a, b)
		}
		return c
	case *ExCast:
		return &ExCast{E: bindExpr(n.E, b), Type: n.Type}
	case *ExIndex:
		return &ExIndex{E: bindExpr(n.E, b), Idx: bindExpr(n.Idx, b)}
	case *ExArray:
		c := &ExArray{Elems: make([]Expr, len(n.Elems))}
		for i, el := range n.Elems {
			c.Elems[i] = bindExpr(el, b)
		}
		return c
	case *ExWindow:
		c := *n
		c.Arg = bindExpr(n.Arg, b)
		c.Offset = bindExpr(n.Offset, b)
		c.Default = bindExpr(n.Default, b)
		c.Partition = make([]Expr, len(n.Partition))
		for i, e := range n.Partition {
			c.Partition[i] = bindExpr(e, b)
		}
		c.OrderBy = make([]OrderItem, len(n.OrderBy))
		for i, o := range n.OrderBy {
			o.Ex = bindExpr(o.Ex, b)
			c.OrderBy[i] = o
		}
		if n.Frame != nil {
			// Deep-copy the frame: the shallow struct copy above shares
			// the pointer, and binding must not mutate the cached AST.
			f := *n.Frame
			f.Start.Offset = bindExpr(n.Frame.Start.Offset, b)
			f.End.Offset = bindExpr(n.Frame.End.Offset, b)
			c.Frame = &f
		}
		return &c
	case *ExArith:
		return &ExArith{Op: n.Op, L: bindExpr(n.L, b), R: bindExpr(n.R, b)}
	case *ExSub:
		return &ExSub{Sel: bindSelect(n.Sel, b)}
	}
	return e
}

// bindArg normalizes one argument to the value kinds the executor
// compares and the engine stores: int64, float64, string, bool,
// []byte, or nil (SQL NULL). A time.Time stays itself — whether it
// means timestamp micros or date days depends on the column it meets,
// so the conversion happens where the type is known (coerceLit, the
// engine's coerce); a [16]byte is a UUID's natural Go shape.
func bindArg(a any) (any, error) {
	switch v := a.(type) {
	case nil, int64, float64, string, bool, []byte, time.Time:
		return v, nil
	case [16]byte:
		return v[:], nil
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
