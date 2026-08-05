package sql

// Per-statement memoization of uncorrelated subqueries.
//
// Subquery evaluation is lazy and per-row: every ExSub/ExAny/EXISTS
// encountered while filtering or projecting re-plans and re-runs its
// Select from scratch. For a correlated subquery that is the semantics.
// For an UNCORRELATED one it made `WHERE x >= (SELECT min(x) FROM t)`
// O(rows(t)²) — the inner aggregate rescanned the table for every outer
// row. Postgres evaluates such subqueries once per statement (an
// InitPlan); memoizing the result reproduces exactly that.
//
// The memo lives on the statement's root exEnv (subMemo climbs the
// outer chain to find it), so its lifetime is one statement execution:
// prepared-statement re-executions get a fresh env, and bound
// parameters are substituted into a fresh AST copy, so a *Select
// pointer can never carry a stale result across executions. Statement
// paths that never seed a memo (none currently) simply skip caching —
// the safe direction.
//
// Correlation detection is static and default-deny: a subquery is
// treated as uncorrelated only when every column reference it contains
// resolves in its OWN scope and nothing in it could reach an enclosing
// scope some other way (nested subqueries and window functions assume
// the worst) or produce a side effect per evaluation (nextval/setval).
// Anything unrecognized falls back to per-row evaluation, which is
// always correct, just slower.

type subMemo struct {
	// vals holds finished results keyed by the subquery's AST node:
	// the scalar for evalSubquery, []any for collectSubColumn, the
	// rendered array literal for evalArraySub, bool for evalExistsSub.
	// Each Select node sits in one syntactic position, so it is only
	// ever evaluated by one of those shapes.
	vals map[*Select]any
	// corr memoizes the static correlation verdict per node, so a
	// correlated subquery pays for the AST walk once, not per row.
	corr map[*Select]bool
}

func newSubMemo() *subMemo {
	return &subMemo{vals: map[*Select]any{}, corr: map[*Select]bool{}}
}

// The four subquery evaluators: memoize when a memo is present and the
// subquery is provably uncorrelated, otherwise run per row as before.

func evalSubquery(env *exEnv, sel *Select) (any, error) {
	m := env.subMemo()
	if m == nil || !m.uncorrelated(env, sel) {
		return runScalarSub(env, sel)
	}
	if v, ok := m.vals[sel]; ok {
		return v, nil
	}
	v, err := runScalarSub(env, sel)
	if err == nil {
		m.vals[sel] = v
	}
	return v, err
}

func evalArraySub(env *exEnv, sel *Select) (any, error) {
	m := env.subMemo()
	if m == nil || !m.uncorrelated(env, sel) {
		return runArraySub(env, sel)
	}
	if v, ok := m.vals[sel]; ok {
		return v, nil
	}
	v, err := runArraySub(env, sel)
	if err == nil {
		m.vals[sel] = v
	}
	return v, err
}

func evalExistsSub(env *exEnv, sel *Select) (any, error) {
	m := env.subMemo()
	if m == nil || !m.uncorrelated(env, sel) {
		return runExistsSub(env, sel)
	}
	if v, ok := m.vals[sel]; ok {
		return v, nil
	}
	v, err := runExistsSub(env, sel)
	if err == nil {
		m.vals[sel] = v
	}
	return v, err
}

func collectSubColumn(env *exEnv, sel *Select) ([]any, error) {
	m := env.subMemo()
	if m == nil || !m.uncorrelated(env, sel) {
		return runSubColumn(env, sel)
	}
	if v, ok := m.vals[sel]; ok {
		return v.([]any), nil
	}
	v, err := runSubColumn(env, sel)
	if err == nil {
		m.vals[sel] = v
	}
	return v, err
}

// subMemo finds the statement's memo, if the executing path seeded one.
func (env *exEnv) subMemo() *subMemo {
	for e := env; e != nil; e = e.outer {
		if e.subs != nil {
			return e.subs
		}
	}
	return nil
}

// uncorrelated reports whether sel's result is independent of the
// current outer row, memoizing the verdict.
func (m *subMemo) uncorrelated(env *exEnv, sel *Select) bool {
	if c, ok := m.corr[sel]; ok {
		return !c
	}
	u := subqueryLocalOnly(env, sel)
	m.corr[sel] = !u
	return u
}

// subqueryLocalOnly is the static test: build the subquery's own scope
// and require every reference to resolve inside it. Sequence-writing
// subqueries are excluded not for correlation but for side effects —
// their draws must keep happening per evaluation.
func subqueryLocalOnly(env *exEnv, sel *Select) bool {
	if env.tx == nil || selectWritesSequences(sel) {
		return false
	}
	sc, err := buildScope(env.d.lookup(env.tx.Table), sel.From)
	if err != nil {
		return false
	}
	return selectLocalOnly(sel, sc)
}

func selectLocalOnly(sel *Select, sc *scope) bool {
	// Shapes the subquery evaluators reject anyway, plus CTEs and
	// UNION arms whose scopes this walk does not model: assume the
	// worst rather than reason about them.
	if len(sel.With) > 0 || len(sel.Union) > 0 || sel.GroupBy != nil || sel.Having != nil {
		return false
	}
	for _, it := range sel.Items {
		if !itemLocalOnly(it, sc) {
			return false
		}
	}
	if !boolLocalOnly(sel.Where, sc) {
		return false
	}
	for _, f := range sel.From {
		if !boolLocalOnly(f.On, sc) {
			return false
		}
	}
	for _, o := range sel.OrderBy {
		if !itemLocalOnly(o.SelectItem, sc) {
			return false
		}
	}
	return true
}

func itemLocalOnly(it SelectItem, sc *scope) bool {
	if it.IsLit {
		return true
	}
	if it.Ex != nil {
		return exprLocalOnly(it.Ex, sc)
	}
	if it.Star {
		return true
	}
	_, err := sc.resolve(it.Col)
	return err == nil
}

func boolLocalOnly(b BoolExpr, sc *scope) bool {
	switch n := b.(type) {
	case nil:
		return true
	case *Pred:
		if n.Item.Agg != AggNone {
			return false
		}
		if _, err := sc.resolve(n.Item.Col); err != nil {
			return false
		}
		if n.RItem != nil {
			if n.RItem.Agg != AggNone {
				return false
			}
			if _, err := sc.resolve(n.RItem.Col); err != nil {
				return false
			}
		}
		return true
	case *Cond:
		return exprLocalOnly(n.Ex, sc)
	case *Not:
		return boolLocalOnly(n.Expr, sc)
	case *And:
		for _, sub := range n.Exprs {
			if !boolLocalOnly(sub, sc) {
				return false
			}
		}
		return true
	case *Or:
		for _, sub := range n.Exprs {
			if !boolLocalOnly(sub, sc) {
				return false
			}
		}
		return true
	}
	return false // unknown BoolExpr shape: assume it reaches outward
}

// exprLocalOnly walks e requiring every column reference to resolve in
// sc. Nested subqueries and windows end the walk immediately: their
// eval-time resolution can climb past this scope, and modeling that is
// not worth the risk — assuming correlation is always correct.
func exprLocalOnly(e Expr, sc *scope) bool {
	local := true
	walkExpr(e, func(x Expr) bool {
		switch n := x.(type) {
		case *ExCol:
			if _, err := sc.resolve(n.Col); err != nil {
				local = false
			}
		case *ExAgg:
			if !n.Star && n.Arg == nil {
				if _, err := sc.resolve(n.Col); err != nil {
					local = false
				}
			}
		case *ExSub, *ExAny, *ExWindow:
			// ExAny may hold a subquery in R; cheaper to bail on the
			// node than to distinguish its array forms.
			local = false
		}
		return local
	})
	return local
}
