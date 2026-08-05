package sql

// Outer-value binding for correlated subqueries.
//
// decorrelate lowers a subquery predicate that references an enclosing
// query's column into a Cond leaf, and Cond leaves never push into
// scans (ast.go). That made `WHERE t.x = outer.y` a full scan of t per
// outer row even when t.x is the primary key — the correlated analogue
// of the quadratic nested-loop join fixed by joinStep's predTmpl
// machinery. This file closes the gap the same way: the correlated
// conjunct becomes a template Pred whose Val is bound from the current
// outer row just before each subquery invocation, so planScan can turn
// the inner scan into a point get or bounded range, and plan.rebind
// refreshes the pushed bounds across invocations without re-planning.
//
// The shape, per correlated conjunct `inner.col op outer.ref`:
//
//	prepare (once per statement, cached in the subMemo):
//	    WHERE tree  ──decorrelateCollect──►  Cond leaf (residual truth,
//	                        │                exactly as before)
//	                        └──────────────► template Pred on the inner
//	                                         table's joinStep.static
//	each invocation (one outer row):
//	    bind: pred.Val = env.lookupVal(outer.ref)   ── the same climb
//	    runJoin: rowPlan pushes/rebinds the bound value into the scan
//
// Correctness leans on two existing invariants rather than new ones:
//
//  1. The Cond leaf stays in the WHERE tree untouched, so the final
//     row filter is byte-for-byte today's. The template only narrows
//     what the step's scan visits and pre-filters via plan.matches —
//     and both of those evaluate the template through checkPred, the
//     same comparison core the Cond's ExCmp reaches (expr.go), with
//     the same operand values (bind resolves the outer reference with
//     env.lookupVal, the exact climb the Cond's ExCol performs after
//     failing inner resolution). Agreement is by construction, not by
//     parallel reimplementation.
//
//  2. plan.rebind's soundness argument — the access path depends only
//     on which columns are bound and each bound value's Go type —
//     transfers intact: the outer reference names a fixed column, so
//     its value type is fixed too. Where that can wobble (untyped
//     virtual-table rows), bind detects the type change and drops the
//     step's cached plan so it re-plans instead of pushing a wrongly
//     encoded bound.
//
// Templates are extracted only from top-level AND conjuncts of the
// subquery's WHERE (a predicate under OR or NOT is not individually
// required, so pushing it would wrongly narrow the scan), only for the
// strict comparison operators planScan can push, and only onto steps
// that take WHERE pushdown at all — prepareFrom's own rule excludes
// NULL-extended (LEFT JOIN) tables, and virtual tables have no scans
// to push into. Correlated ON conjuncts keep today's Cond-only path.

import (
	"reflect"

	"github.com/rohanthewiz/serr"
)

// corrSpec is one extraction from the WHERE walk: the inner side as a
// pushable item, the operator oriented inner-side-first, and the outer
// column reference to bind per invocation.
type corrSpec struct {
	item SelectItem
	op   PredOp
	ref  ColRef
}

// corrTmpl is a live template: the Pred sitting in a step's static
// list (allocated once; Val mutated by bind) plus what bind needs.
type corrTmpl struct {
	step int          // fp.steps index of the inner table the Pred pushes into
	pred *Pred        // shared with fp.steps[step].static
	ref  ColRef       // outer side, re-resolved against the live env each bind
	typ  reflect.Type // Go type last bound; a change invalidates the step's plan
}

// subPrep is a subquery's prepared FROM, reusable across invocations
// within one statement. Reuse is sound because everything cached here
// derives from the subquery's own AST and its tables' descriptors
// (plus any virtual tables' rows, which are materialized per
// statement); nothing outer-row-dependent is baked in — outer values
// arrive through bind, and outer name resolution is redone per bind.
type subPrep struct {
	fp    *fromPlan
	tmpls []corrTmpl
	// envSC is the enclosing scope the templates were extracted under.
	// Template existence is the one prepare-time decision that looks
	// outward (resolvesOutward), so a different enclosing scope —
	// possible when the enclosing query itself re-prepares — re-runs
	// extraction rather than trusting a stale verdict.
	envSC *scope
}

// prepareSubFrom is the shared front half of the four subquery runners
// (scalar, ANY/ALL column, ARRAY, EXISTS): scope, decorrelation,
// prepareFrom, template extraction, caching, and per-invocation
// binding. empty=true means the subquery provably yields no rows this
// invocation (a correlated operand is NULL) and runJoin can be skipped
// outright — the caller still applies its own empty-result semantics
// (NULL scalar, count()=0, empty array, EXISTS false).
func prepareSubFrom(env *exEnv, sel *Select) (fp *fromPlan, empty bool, err error) {
	m := env.subMemo()
	var sp *subPrep
	if m != nil {
		if cached, ok := m.plans[sel]; ok && cached.envSC == env.sc {
			sp = cached
		}
	}
	if sp == nil {
		sp = &subPrep{envSC: env.sc}
		if sp.fp, err = buildSubFrom(env, sel, sp); err != nil {
			return nil, false, err
		}
		// Statement paths that never seed a memo still get within-
		// invocation pushdown from the fresh templates; they just
		// re-prepare per invocation, the pre-existing cost.
		if m != nil {
			m.plans[sel] = sp
		}
	}
	empty, ok := sp.bind(env)
	if !ok {
		// An outer reference did not evaluate here (e.g. its column is
		// past the end of a partial row during ON evaluation). Fall back
		// to the untemplated preparation, preserving lazy resolution:
		// a name only errors when a row actually reaches its Cond.
		fp, err = buildSubFrom(env, sel, nil)
		return fp, false, err
	}
	return sp.fp, empty, nil
}

// buildSubFrom prepares a subquery's FROM and WHERE. With sp non-nil
// it also extracts correlated templates and attaches them to their
// steps; with sp nil it is exactly the legacy preparation.
func buildSubFrom(env *exEnv, sel *Select, sp *subPrep) (*fromPlan, error) {
	lk := env.d.lookup(env.tx.Table)
	sc, err := buildScope(lk, sel.From)
	if err != nil {
		return nil, err
	}
	from := make([]FromItem, len(sel.From))
	copy(from, sel.From)
	for k := range from {
		from[k].On = decorrelate(from[k].On, sc.prefix(k+1))
	}
	var specs []corrSpec
	var where BoolExpr
	if sp != nil {
		where = decorrelateCollect(sel.Where, sc, env, &specs)
	} else {
		where = decorrelate(sel.Where, sc)
	}
	fp, err := prepareFrom(lk, from, where)
	if err != nil {
		return nil, err
	}
	for _, spec := range specs {
		ord, err := sc.resolve(spec.item.Col)
		if err != nil {
			// Unreachable: collection required inner resolution. Surface
			// it rather than push a misresolved bound.
			return nil, serr.Wrap(err, "op", "attach correlated template")
		}
		k := sc.tableOf(ord)
		step := &fp.steps[k]
		if step.it.Join == JoinLeft || step.st.rows != nil {
			// prepareFrom's WHERE-pushdown rule: a NULL-extended table's
			// WHERE conjuncts wait for the post-join filter. Virtual
			// tables take no pushdown at all. The Cond still filters.
			continue
		}
		pred := &Pred{Item: spec.item, Op: spec.op}
		step.static = append(step.static, pred)
		sp.tmpls = append(sp.tmpls, corrTmpl{step: k, pred: pred, ref: spec.ref})
	}
	return fp, nil
}

// decorrelateCollect is decorrelate plus template extraction at
// conjunct positions. Only nil/Pred/And are walked for collection:
// anything under OR or NOT is not individually required by the WHERE,
// so it must not narrow a scan, and plain decorrelate handles it.
func decorrelateCollect(e BoolExpr, sc *scope, env *exEnv, out *[]corrSpec) BoolExpr {
	switch n := e.(type) {
	case nil:
		return nil
	case *Pred:
		if spec, ok := corrTemplate(n, sc, env); ok {
			*out = append(*out, spec)
		}
		return decorrelate(n, sc)
	case *And:
		res := &And{Exprs: make([]BoolExpr, len(n.Exprs))}
		for i, sub := range n.Exprs {
			res.Exprs[i] = decorrelateCollect(sub, sc, env, out)
		}
		return res
	}
	return decorrelate(e, sc)
}

// corrTemplate recognizes the pushable correlated shape: a strict
// comparison between a column that resolves in the subquery's own
// scope and one that does not but does resolve somewhere up the
// enclosing chain. The template is oriented inner-side-first (flip
// mirrors the operator when the outer side was on the left), matching
// the parser's own literal-first normalization, so checkPred sees the
// same comparison the Cond's ExCmp evaluates.
func corrTemplate(n *Pred, sc *scope, env *exEnv) (corrSpec, bool) {
	switch n.Op {
	case OpEQ, OpLT, OpLE, OpGT, OpGE:
		// The operators planScan pushes; also exactly the strict set
		// bind's NULL short-circuit is sound for.
	default:
		return corrSpec{}, false
	}
	if n.RItem == nil || n.Item.Agg != AggNone || n.RItem.Agg != AggNone {
		return corrSpec{}, false
	}
	_, lErr := sc.resolve(n.Item.Col)
	_, rErr := sc.resolve(n.RItem.Col)
	switch {
	case lErr == nil && rErr != nil && resolvesOutward(n.RItem.Col, env):
		return corrSpec{item: n.Item, op: n.Op, ref: n.RItem.Col}, true
	case rErr == nil && lErr != nil && resolvesOutward(n.Item.Col, env):
		return corrSpec{item: *n.RItem, op: flip(n.Op), ref: n.Item.Col}, true
	}
	return corrSpec{}, false
}

// resolvesOutward reports whether the reference resolves in some
// enclosing scope — the same climb lookupVal performs after inner
// resolution fails. Purely an existence check: bind re-resolves per
// invocation, so a chain that shifts between prepare and bind yields
// the value the Cond would see, never a stale one.
func resolvesOutward(c ColRef, env *exEnv) bool {
	for e := env; e != nil; e = e.outer {
		if e.sc == nil {
			continue
		}
		if _, err := e.sc.resolve(c); err == nil {
			return true
		}
	}
	return false
}

// bind installs the current outer row's values into the templates.
// empty=true short-circuits the whole invocation; ok=false tells the
// caller to fall back to untemplated preparation.
func (sp *subPrep) bind(env *exEnv) (empty, ok bool) {
	for i := range sp.tmpls {
		tm := &sp.tmpls[i]
		v, err := env.lookupVal(tm.ref)
		if err != nil {
			return false, false
		}
		if v == nil {
			// A strict comparison against NULL matches no row, and every
			// template sits on a step whose rows all combined rows need
			// (LEFT-joined steps never get templates), so the subquery
			// yields nothing. Returning before installing values also
			// keeps a nil from ever reaching plan construction.
			return true, true
		}
		if t := reflect.TypeOf(v); t != tm.typ {
			// The value's Go type decides litFits and the pushed key
			// encoding, and plan.rebind assumes both are fixed. Column
			// values have fixed types, so this fires once on the first
			// bind and again only for untyped virtual-table sources —
			// dropping the cached plan makes rowPlan re-plan with the
			// new type instead of rebinding a wrongly encoded bound.
			tm.typ = t
			step := &sp.fp.steps[tm.step]
			step.tmplPlan, step.tmplPreds = nil, nil
		}
		tm.pred.Val = v
	}
	return false, true
}
