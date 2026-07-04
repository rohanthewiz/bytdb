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
	case *Update:
		for _, v := range s.Set {
			note(v)
		}
		notePredVals(s.Where, note)
	case *Delete:
		notePredVals(s.Where, note)
	case *Select:
		for _, f := range s.From {
			notePredVals(f.On, note)
		}
		notePredVals(s.Where, note)
		notePredVals(s.Having, note)
	}
	return n
}

// notePredVals feeds every leaf predicate's literal side to note.
func notePredVals(e BoolExpr, note func(any)) {
	walkPreds(e, func(pr *Pred) error { note(pr.Val); return nil })
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
		return &c, nil
	case *Update:
		c := *s
		c.Set = make(map[string]any, len(s.Set))
		for k, v := range s.Set {
			c.Set[k] = sub(v)
		}
		c.Where = bindBool(s.Where, sub)
		return &c, nil
	case *Delete:
		c := *s
		c.Where = bindBool(s.Where, sub)
		return &c, nil
	case *Select:
		c := *s
		c.From = make([]FromItem, len(s.From))
		copy(c.From, s.From)
		for i := range c.From {
			c.From[i].On = bindBool(c.From[i].On, sub)
		}
		c.Where = bindBool(s.Where, sub)
		c.Having = bindBool(s.Having, sub)
		return &c, nil
	}
	return st, nil
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
