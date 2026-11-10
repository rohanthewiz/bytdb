package sql

// typecheck.go: the typed bind pass (deep-look D-23).
//
// A SQL value's type used to exist only in the catalog and in exprType.
// At runtime a timestamp and a date are both int64 and a uuid is a
// []byte, so correct behavior hung on one pass, coerceLiterals, which
// converted quoted literals only in a top-level statement's simple
// predicates. Everything it missed — CASE conditions, select-list
// comparisons, IN lists, subqueries, CTE and view bodies, CHECK
// constraints — fell to checkPred, which guessed the type from the Go
// kind, failed, swallowed the error, and treated the comparison as
// unknown: rows silently dropped.
//
//	         before                                   after
//	literal ─► coerceLiterals (top-level Pred only)   literal ─► typeStatement
//	              │ miss                                         (every Expr, every
//	              ▼                                              nested SELECT, CHECK,
//	        checkPred: kindOf(v) guess                           view body)
//	              │ fail                                            │
//	              ▼                                                 ▼
//	        error swallowed → unknown → row dropped       typed literals, explicit
//	                                                      conversions, errors at bind
//
// The pass walks a statement with the scope of every SELECT block
// (subqueries see their enclosing scopes, as correlation does) and,
// working bottom-up, gives each expression a static type. Then:
//
//  1. An untyped literal — a quoted string, or a bound string or
//     time.Time argument — takes the type of its sibling operand
//     (comparison, IN list, ANY, BETWEEN, arithmetic, CASE branch,
//     COALESCE argument) and is converted to that type's runtime
//     representation once, here, instead of per row.
//  2. Two typed operands that differ but compare meaningfully get an
//     explicit conversion: a date compared with a timestamp is cast to
//     a timestamp (days to microseconds).
//  3. Combinations Postgres has no operator for are refused with its
//     42883 wording — 1 = true, text = integer, timestamp + integer
//     (plan decision Q1), integer || integer.
//  4. Casts record their operand's type (ExCast.From), so ts::date and
//     u::text convert the value instead of reinterpreting it (D-24).
//
// What the pass cannot resolve it leaves alone — an unknown table, a
// column it cannot see, an ORDER BY alias — so the executor reports
// that in its own words. It is strict only where both sides are
// definitely typed. And it is copy-on-write: a node is rebuilt only if
// something under it changed. Prepared statements share their parsed
// tree across executions and goroutines (bindParams returns it as is
// when there are no parameters), so mutating in place would race.

import (
	"strings"
	"time"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// Pseudo-types the pass uses internally; they never leave it.
const (
	// tyUnknown is an untyped literal: a quoted string, or a bound
	// string or time.Time, whose type comes from its context — Postgres
	// calls the same thing "unknown".
	tyUnknown bytdb.ColType = "?unknown"
	// tyNull is the NULL literal, which fits any type.
	tyNull bytdb.ColType = "?null"
)

// concrete reports whether t is a real column type the pass can act on
// ("" means the pass could not determine one).
func concrete(t bytdb.ColType) bool { return t != "" && t != tyUnknown && t != tyNull }

func numericType(t bytdb.ColType) bool { return t == bytdb.TInt || t == bytdb.TFloat }

func temporalType(t bytdb.ColType) bool { return t == bytdb.TDate || t == bytdb.TTimestamp }

// pgTypeName spells a column type the way Postgres's error messages do.
func pgTypeName(t bytdb.ColType) string {
	switch t {
	case bytdb.TInt:
		return "bigint"
	case bytdb.TFloat:
		return "double precision"
	case bytdb.TString:
		return "text"
	case bytdb.TBool:
		return "boolean"
	case bytdb.TBytes:
		return "bytea"
	case bytdb.TTimestamp:
		return "timestamp with time zone"
	case bytdb.TDate:
		return "date"
	case bytdb.TUUID:
		return "uuid"
	case bytdb.TTextArray:
		return "text[]"
	case bytdb.TJSONB:
		return "jsonb"
	case tyUnknown:
		return "unknown"
	}
	return string(t)
}

// noOperator is Postgres's refusal for an operand pairing it has no
// operator for (SQLSTATE 42883).
func noOperator(op string, l, r bytdb.ColType) error {
	return serr.New("operator does not exist: "+pgTypeName(l)+" "+op+" "+pgTypeName(r),
		"hint", "no operator matches the given name and argument types; you might need explicit casts")
}

// typer is one run of the pass over a statement.
type typer struct {
	d *DB
	// base resolves catalog tables, system tables and any virtual tables
	// already materialized on d (a view body typed while it runs sees the
	// views it depends on).
	base tableLookup
	// ctes holds the statement's WITH entries as their typed shapes, in
	// the order registered: each entry and the body see the ones before.
	ctes map[string]*bytdb.TableDesc
	// views caches static shapes of views resolved on demand.
	views map[string]*bytdb.TableDesc
	// note, when set (Describe), receives the type a placeholder takes
	// from its context.
	note func(any, bytdb.ColType)
}

func (d *DB) newTyper(note func(any, bytdb.ColType)) *typer {
	tableFn := d.e.TableShared
	if d.tx != nil {
		tableFn = d.tx.TableShared
	}
	return &typer{d: d, base: d.shapeLookup(tableFn), note: note}
}

// lookup resolves a FROM name: statement CTEs first (they shadow real
// tables, as in Postgres), then the catalog, then views — only when the
// engine has any, and computed statically, once per statement.
func (t *typer) lookup(name string) (*bytdb.TableDesc, [][]any) {
	if desc, ok := t.ctes[name]; ok {
		return desc, [][]any{}
	}
	desc, rows := t.base(name)
	if desc != nil || !t.d.e.HasViews() {
		return desc, rows
	}
	if desc, ok := t.views[name]; ok {
		return desc, [][]any{}
	}
	dv, err := t.d.staticView(name, 0)
	if err != nil || dv.vtabs[name].desc == nil {
		return nil, nil // the executor reports a broken or missing view
	}
	if t.views == nil {
		t.views = map[string]*bytdb.TableDesc{}
	}
	t.views[name] = dv.vtabs[name].desc
	return t.views[name], [][]any{}
}

// tscope is a SELECT block's scope plus its enclosing blocks', for
// correlated references. sc is nil where a block has no FROM.
type tscope struct {
	sc    *scope
	outer *tscope
}

// colType resolves a column reference to its type, innermost scope
// first; "" when it does not resolve (the executor reports why).
func (ts *tscope) colType(c ColRef) bytdb.ColType {
	for s := ts; s != nil; s = s.outer {
		if s.sc == nil {
			continue
		}
		ord, err := s.sc.resolve(c)
		if err == nil {
			return s.sc.column(ord).Type
		}
		if strings.Contains(err.Error(), "ambiguous") {
			return ""
		}
	}
	return ""
}

// innermost returns the nearest scope with tables, or nil.
func (ts *tscope) innermost() *scope {
	for s := ts; s != nil; s = s.outer {
		if s.sc != nil {
			return s.sc
		}
	}
	return nil
}

// typeStatement runs the pass over a whole statement. note is nil at
// execution and set by Describe.
func (d *DB) typeStatement(st Statement, note func(any, bytdb.ColType)) (Statement, error) {
	t := d.newTyper(note)
	switch s := st.(type) {
	case *Explain:
		inner, err := d.typeStatement(s.Stmt, note)
		if err != nil {
			return nil, err
		}
		if inner == s.Stmt {
			return s, nil
		}
		return &Explain{Stmt: inner}, nil
	case *Select:
		out, _, err := t.sel(s, nil)
		if err != nil {
			return nil, err
		}
		return out, nil
	case *Insert:
		return t.insert(s)
	case *Update:
		return t.update(s)
	case *Delete:
		return t.delete(s)
	}
	return st, nil
}

// typeSelectStmt types a stored SELECT (a view body) on d.
func (d *DB) typeSelectStmt(s *Select) (*Select, error) {
	out, _, err := d.newTyper(nil).sel(s, nil)
	return out, err
}

// typeRowExpr types an expression evaluated against one row of a table
// — a CHECK constraint or ALTER COLUMN TYPE's USING — over that table's
// scope alone.
func typeRowExpr(sc *scope, ex Expr) (Expr, error) {
	t := &typer{}
	out, _, err := t.expr(ex, &tscope{sc: sc})
	return out, err
}

// --- SELECT ---

// sel types one SELECT (its WITH entries, body and UNION arms) and
// returns the output column types of its first core, where they can be
// determined ("" otherwise).
func (t *typer) sel(s *Select, outer *tscope) (*Select, []bytdb.ColType, error) {
	if s == nil {
		return nil, nil, nil
	}
	c := *s
	changed := false
	if len(s.With) > 0 {
		with := make([]CTE, len(s.With))
		for i, cte := range s.With {
			typed, _, err := t.sel(cte.Sel, outer)
			if err != nil {
				return nil, nil, serr.Wrap(err, "with_query", displayCTEName(cte.Name))
			}
			with[i] = cte
			with[i].Sel = typed
			changed = changed || typed != cte.Sel
			// Register the entry's shape so later entries and the body
			// resolve its columns with their real types. A shape that
			// cannot be derived is left out; execution reports why.
			res := &Result{}
			if err := describeSelect(t.lookup, typed, func(any, bytdb.ColType) {}, res); err == nil {
				if desc, err := vtabDesc(displayCTEName(cte.Name), res.Cols, res, cte.Cols); err == nil {
					if t.ctes == nil { // allocated only for statements with a WITH
						t.ctes = map[string]*bytdb.TableDesc{}
					}
					t.ctes[cte.Name] = desc
				}
			}
		}
		if changed {
			c.With = with
		}
	}
	core, types, err := t.core(s, outer)
	if err != nil {
		return nil, nil, err
	}
	if core != s {
		changed = true
		c.From, c.Where, c.Items, c.GroupBy, c.Having, c.OrderBy =
			core.From, core.Where, core.Items, core.GroupBy, core.Having, core.OrderBy
	}
	if len(s.Union) > 0 {
		arms := make([]UnionArm, len(s.Union))
		armsChanged := false
		for i, arm := range s.Union {
			typed, _, err := t.sel(arm.Sel, outer)
			if err != nil {
				return nil, nil, err
			}
			arms[i] = arm
			arms[i].Sel = typed
			armsChanged = armsChanged || typed != arm.Sel
		}
		if armsChanged {
			c.Union = arms
			changed = true
		}
	}
	if !changed {
		return s, types, nil
	}
	return &c, types, nil
}

// core types one SELECT core: FROM's ON conditions, WHERE, the select
// list, GROUP BY, HAVING and ORDER BY.
func (t *typer) core(s *Select, outer *tscope) (*Select, []bytdb.ColType, error) {
	ts := &tscope{outer: outer}
	if len(s.From) > 0 {
		sc, err := buildScope(t.lookup, s.From)
		if err != nil {
			return s, nil, nil // unknown table and the like: the executor reports it
		}
		ts.sc = sc
	}
	c := *s
	changed := false

	fromCopied := false
	for k, it := range s.From {
		if it.On == nil || ts.sc == nil {
			continue
		}
		on, err := t.boolx(it.On, &tscope{sc: ts.sc.prefix(k + 1), outer: outer}, false)
		if err != nil {
			return nil, nil, err
		}
		if on != it.On {
			if !fromCopied {
				c.From, fromCopied = append([]FromItem(nil), s.From...), true
			}
			c.From[k].On = on
			changed = true
		}
	}

	where, err := t.boolx(s.Where, ts, false)
	if err != nil {
		return nil, nil, err
	}
	if where != s.Where {
		c.Where, changed = where, true
	}

	items, types, itemsChanged, err := t.items(s.Items, ts)
	if err != nil {
		return nil, nil, err
	}
	if itemsChanged {
		c.Items, changed = items, true
	}
	if s.Star && ts.sc != nil {
		for i := range ts.sc.width {
			types = append(types, ts.sc.column(i).Type)
		}
	}

	if len(s.GroupBy) > 0 {
		var group []GroupItem
		for i, g := range s.GroupBy {
			if g.Ex == nil {
				continue
			}
			ex, _, err := t.expr(g.Ex, ts)
			if err != nil {
				return nil, nil, err
			}
			if ex != g.Ex {
				if group == nil {
					group = append([]GroupItem(nil), s.GroupBy...)
				}
				group[i].Ex = ex
			}
		}
		if group != nil {
			c.GroupBy, changed = group, true
		}
	}

	having, err := t.boolx(s.Having, ts, true)
	if err != nil {
		return nil, nil, err
	}
	if having != s.Having {
		c.Having, changed = having, true
	}

	if len(s.OrderBy) > 0 {
		var order []OrderItem
		for i, o := range s.OrderBy {
			if o.Ex == nil {
				continue
			}
			ex, _, err := t.expr(o.Ex, ts)
			if err != nil {
				return nil, nil, err
			}
			if ex != o.Ex {
				if order == nil {
					order = append([]OrderItem(nil), s.OrderBy...)
				}
				order[i].Ex = ex
			}
		}
		if order != nil {
			c.OrderBy, changed = order, true
		}
	}
	if !changed {
		return s, types, nil
	}
	return &c, types, nil
}

// items types a select list (or RETURNING list) and reports each item's
// type ("" where unknown).
func (t *typer) items(in []SelectItem, ts *tscope) ([]SelectItem, []bytdb.ColType, bool, error) {
	var out []SelectItem
	types := make([]bytdb.ColType, 0, len(in))
	for i, it := range in {
		switch {
		case it.Ex != nil:
			ex, typ, err := t.expr(it.Ex, ts)
			if err != nil {
				return nil, nil, false, err
			}
			if ex != it.Ex {
				if out == nil {
					out = append([]SelectItem(nil), in...)
				}
				out[i].Ex = ex
			}
			types = append(types, outputType(typ))
		case it.Agg != AggNone: // before Star: count(*) is an aggregate with Star set
			types = append(types, t.aggType(it.Agg, it.Star, ts.colType(it.Col), it.Col))
		case it.Star:
			// t.* expands at projection; its columns are not one type.
			types = append(types, "")
		case it.IsLit:
			types = append(types, outputType(litStaticType(it.Lit, "")))
		default:
			types = append(types, ts.colType(it.Col))
		}
	}
	if out == nil {
		return in, types, false, nil
	}
	return out, types, true, nil
}

// outputType maps a pseudo-type to what a result column reports: an
// untyped literal or NULL is text, as Postgres resolves them in a
// select list.
func outputType(t bytdb.ColType) bytdb.ColType {
	if t == tyUnknown || t == tyNull {
		return bytdb.TString
	}
	return t
}

func (t *typer) aggType(fn AggFunc, star bool, arg bytdb.ColType, _ ColRef) bytdb.ColType {
	switch fn {
	case AggCount:
		return bytdb.TInt
	case AggAvg:
		return bytdb.TFloat
	case AggSum:
		if arg == bytdb.TInt {
			return bytdb.TInt
		}
		if concrete(arg) {
			return bytdb.TFloat
		}
		return ""
	}
	if star {
		return ""
	}
	return arg
}

// --- DML ---

func (t *typer) insert(s *Insert) (Statement, error) {
	desc, _ := t.lookup(s.Table)
	if desc == nil {
		return s, nil
	}
	var colTypes []bytdb.ColType
	if s.Cols == nil {
		for _, col := range desc.Columns {
			colTypes = append(colTypes, col.Type)
		}
	} else {
		for _, name := range s.Cols {
			i := desc.ColIndex(name)
			if i < 0 {
				return s, nil
			}
			colTypes = append(colTypes, desc.Columns[i].Type)
		}
	}
	ts := &tscope{sc: tableScope(s.Table, desc)}
	c := *s
	changed := false
	rows := make([][]any, len(s.Rows))
	for i, row := range s.Rows {
		var r []any
		for j, v := range row {
			if j >= len(colTypes) {
				break // arity: the engine reports it
			}
			nv, err := t.value(v, colTypes[j], ts)
			if err != nil {
				return nil, err
			}
			if !sameValue(nv, v) {
				if r == nil {
					r = append([]any(nil), row...)
				}
				r[j] = nv
			}
		}
		if r != nil {
			rows[i], changed = r, true
		} else {
			rows[i] = row
		}
	}
	if changed {
		c.Rows = rows
	}
	if oc := s.Conflict; oc != nil {
		// The DO UPDATE arm resolves over the target plus "excluded";
		// both share the descriptor.
		ocTS := &tscope{sc: &scope{tables: []scopeTable{
			{name: s.Table, desc: desc},
			{name: "excluded", desc: desc, off: len(desc.Columns)},
		}, width: 2 * len(desc.Columns)}}
		set, setEx, err := t.assignments(desc, oc.Set, oc.SetEx, ocTS)
		if err != nil {
			return nil, err
		}
		where, err := t.boolx(oc.Where, ocTS, false)
		if err != nil {
			return nil, err
		}
		if set != nil || setEx != nil || where != oc.Where {
			oc2 := *oc
			if set != nil {
				oc2.Set = set
			}
			if setEx != nil {
				oc2.SetEx = setEx
			}
			oc2.Where = where
			c.Conflict, changed = &oc2, true
		}
	}
	ret, retChanged, err := t.returning(s.Returning, ts)
	if err != nil {
		return nil, err
	}
	if retChanged {
		c.Returning, changed = ret, true
	}
	if !changed {
		return s, nil
	}
	return &c, nil
}

func (t *typer) update(s *Update) (Statement, error) {
	desc, _ := t.lookup(s.Table)
	if desc == nil {
		return s, nil
	}
	ts := &tscope{sc: tableScope(s.Table, desc)}
	c := *s
	changed := false
	set, setEx, err := t.assignments(desc, s.Set, s.SetEx, ts)
	if err != nil {
		return nil, err
	}
	if set != nil {
		c.Set, changed = set, true
	}
	if setEx != nil {
		c.SetEx, changed = setEx, true
	}
	where, err := t.boolx(s.Where, ts, false)
	if err != nil {
		return nil, err
	}
	if where != s.Where {
		c.Where, changed = where, true
	}
	ret, retChanged, err := t.returning(s.Returning, ts)
	if err != nil {
		return nil, err
	}
	if retChanged {
		c.Returning, changed = ret, true
	}
	if !changed {
		return s, nil
	}
	return &c, nil
}

func (t *typer) delete(s *Delete) (Statement, error) {
	desc, _ := t.lookup(s.Table)
	if desc == nil {
		return s, nil
	}
	ts := &tscope{sc: tableScope(s.Table, desc)}
	c := *s
	changed := false
	where, err := t.boolx(s.Where, ts, false)
	if err != nil {
		return nil, err
	}
	if where != s.Where {
		c.Where, changed = where, true
	}
	ret, retChanged, err := t.returning(s.Returning, ts)
	if err != nil {
		return nil, err
	}
	if retChanged {
		c.Returning, changed = ret, true
	}
	if !changed {
		return s, nil
	}
	return &c, nil
}

// assignments types SET col = value pairs: plain values convert to the
// column's type, expressions are typed over the row's scope and, when
// their own type is still unknown, converted to the column's. Each map
// is returned only if something in it changed.
func (t *typer) assignments(desc *bytdb.TableDesc, set map[string]any, setEx map[string]Expr, ts *tscope) (map[string]any, map[string]Expr, error) {
	var outSet map[string]any
	for name, v := range set {
		i := desc.ColIndex(name)
		if i < 0 {
			continue
		}
		nv, err := t.value(v, desc.Columns[i].Type, ts)
		if err != nil {
			return nil, nil, err
		}
		if !sameValue(nv, v) {
			if outSet == nil {
				outSet = make(map[string]any, len(set))
				for k, x := range set {
					outSet[k] = x
				}
			}
			outSet[name] = nv
		}
	}
	var outEx map[string]Expr
	for name, ex := range setEx {
		i := desc.ColIndex(name)
		if i < 0 {
			continue
		}
		nv, err := t.value(ex, desc.Columns[i].Type, ts)
		if err != nil {
			return nil, nil, err
		}
		if nex, _ := nv.(Expr); nex != ex {
			if outEx == nil {
				outEx = make(map[string]Expr, len(setEx))
				for k, x := range setEx {
					outEx[k] = x
				}
			}
			outEx[name] = nex
		}
	}
	return outSet, outEx, nil
}

// value types one value headed for a column of type ct: a plain literal
// converts to the column's type; an expression is typed and, if its
// result is still an untyped literal, converted too.
func (t *typer) value(v any, ct bytdb.ColType, ts *tscope) (any, error) {
	switch x := v.(type) {
	case defaultMarker:
		return v, nil
	case Expr:
		ex, typ, err := t.expr(x, ts)
		if err != nil {
			return nil, err
		}
		if typ == tyUnknown {
			if ex, err = t.coerceTo(ex, ct); err != nil {
				return nil, err
			}
		}
		return ex, nil
	case Param:
		if t.note != nil {
			t.note(x, ct)
		}
		return v, nil
	}
	return coerceLit(v, ct)
}

// sameValue reports whether a typed value is still the original one: a
// conversion always yields a new value, so identity is the test, and a
// value of a kind that cannot be compared counts as changed (copying it
// is merely conservative).
func sameValue(a, b any) bool {
	switch x := a.(type) {
	case Expr:
		y, ok := b.(Expr)
		return ok && x == y
	case []byte:
		y, ok := b.([]byte)
		return ok && len(x) == len(y) && (len(x) == 0 || &x[0] == &y[0])
	case nil, string, int64, float64, bool, Param:
		return a == b
	}
	return false
}

func (t *typer) returning(r Returning, ts *tscope) (Returning, bool, error) {
	if len(r.RetItems) == 0 {
		return r, false, nil
	}
	items, _, changed, err := t.items(r.RetItems, ts)
	if err != nil || !changed {
		return r, false, err
	}
	r.RetItems = items
	return r, true, nil
}

// --- conditions ---

// boolx types a WHERE/ON/HAVING tree. having types Pred items that are
// aggregate calls by the aggregate's result type.
func (t *typer) boolx(e BoolExpr, ts *tscope, having bool) (BoolExpr, error) {
	switch n := e.(type) {
	case nil:
		return nil, nil
	case *Pred:
		return t.pred(n, ts, having)
	case *Cond:
		ex, _, err := t.expr(n.Ex, ts)
		if err != nil {
			return nil, err
		}
		if !having {
			if pr := t.foldCastPred(ex, ts); pr != nil {
				return pr, nil
			}
		}
		if ex == n.Ex {
			return n, nil
		}
		return &Cond{Ex: ex}, nil
	case *Not:
		sub, err := t.boolx(n.Expr, ts, having)
		if err != nil {
			return nil, err
		}
		if sub == n.Expr {
			return n, nil
		}
		return &Not{Expr: sub}, nil
	case *And:
		exprs, changed, err := t.bools(n.Exprs, ts, having)
		if err != nil {
			return nil, err
		}
		if !changed {
			return n, nil
		}
		return &And{Exprs: exprs}, nil
	case *Or:
		exprs, changed, err := t.bools(n.Exprs, ts, having)
		if err != nil {
			return nil, err
		}
		if !changed {
			return n, nil
		}
		return &Or{Exprs: exprs}, nil
	}
	return e, nil
}

func (t *typer) bools(in []BoolExpr, ts *tscope, having bool) ([]BoolExpr, bool, error) {
	var out []BoolExpr
	for i, e := range in {
		ne, err := t.boolx(e, ts, having)
		if err != nil {
			return nil, false, err
		}
		if ne != e {
			if out == nil {
				out = append([]BoolExpr(nil), in...)
			}
			out[i] = ne
		}
	}
	if out == nil {
		return in, false, nil
	}
	return out, true, nil
}

// itemType is a Pred item's type: a column's, or in HAVING an
// aggregate's result type.
func (t *typer) itemType(it SelectItem, ts *tscope, having bool) bytdb.ColType {
	if having && it.Agg != AggNone {
		var arg bytdb.ColType
		if !it.Star {
			arg = ts.colType(it.Col)
		}
		return t.aggType(it.Agg, it.Star, arg, it.Col)
	}
	if it.Ex != nil || it.IsLit || it.Star {
		return ""
	}
	return ts.colType(it.Col)
}

// pred types a lowered comparison leaf.
func (t *typer) pred(n *Pred, ts *tscope, having bool) (BoolExpr, error) {
	if !orderingOp(n.Op) {
		return n, nil // IS NULL, patterns, jsonb operators: not typed here
	}
	lt := t.itemType(n.Item, ts, having)
	if n.RItem != nil {
		rt := t.itemType(*n.RItem, ts, having)
		if !concrete(lt) || !concrete(rt) {
			return n, nil
		}
		l, r, err := t.cmpPair(n.Op, itemToExpr(n.Item), lt, itemToExpr(*n.RItem), rt)
		if err != nil {
			return nil, err
		}
		if _, lc := l.(*ExCast); lc {
			return &Cond{Ex: &ExCmp{Op: n.Op, L: l, R: r}}, nil
		}
		if _, rc := r.(*ExCast); rc {
			return &Cond{Ex: &ExCmp{Op: n.Op, L: l, R: r}}, nil
		}
		return n, nil
	}
	if !concrete(lt) {
		return n, nil
	}
	switch v := n.Val.(type) {
	case nil:
		return n, nil
	case Param:
		if t.note != nil {
			t.note(v, lt)
		}
		return n, nil
	}
	vt := litStaticType(n.Val, "")
	if vt == tyUnknown {
		cv, err := coerceLit(n.Val, lt)
		if err != nil {
			return nil, err
		}
		if sameValue(cv, n.Val) {
			return n, nil
		}
		c := *n
		c.Val = cv
		return &c, nil
	}
	if err := compatible(opText(n.Op), lt, vt); err != nil {
		return nil, err
	}
	return n, nil
}

// foldCastPred turns col <op> 'literal'::type (either way round) into
// a Pred leaf carrying the cast's value, when the cast's type is the
// column's own timestamp, date or uuid type. The parser lowers only a
// bare literal to a Pred; a cast stays a Cond the planner cannot push,
// so WHERE id = '…'::uuid — the usual way to write a typed literal in
// Postgres, and what ORMs emit as $1::uuid — was a full scan while
// WHERE id = '…' was a point get (deep-look D-37). The cast is
// evaluated once here; its value is the column's runtime form, which
// both the planner (litFits) and the residual comparison take as is.
// Anything else, including a cast that fails (its error stays with the
// executor, as before), returns nil.
func (t *typer) foldCastPred(ex Expr, ts *tscope) *Pred {
	cmp, ok := ex.(*ExCmp)
	if !ok || !orderingOp(cmp.Op) {
		return nil
	}
	col, lit, op := cmp.L, cmp.R, cmp.Op
	if _, isCol := col.(*ExCol); !isCol {
		if !flippable(op) {
			return nil
		}
		col, lit, op = cmp.R, cmp.L, flip(op)
	}
	c, ok := col.(*ExCol)
	if !ok {
		return nil
	}
	cast, ok := lit.(*ExCast)
	if !ok {
		return nil
	}
	l, ok := cast.E.(*ExLit)
	if !ok || l.Val == nil {
		return nil
	}
	if _, isParam := l.Val.(Param); isParam {
		return nil
	}
	to := castColType(cast.Type)
	switch to {
	case bytdb.TTimestamp, bytdb.TDate, bytdb.TUUID:
	default:
		return nil
	}
	if _, ct, err := t.expr(c, ts); err != nil || ct != to {
		return nil
	}
	v, err := castValFrom(&exEnv{d: t.d}, l.Val, cast.From, cast.Type)
	if err != nil || v == nil || !litFits(v, to) {
		return nil
	}
	return &Pred{Item: SelectItem{Col: c.Col}, Op: op, Val: v}
}

// orderingOp reports whether op is one of the six comparisons.
func orderingOp(op PredOp) bool {
	switch op {
	case OpEQ, OpNE, OpLT, OpLE, OpGT, OpGE:
		return true
	}
	return false
}

// compatible reports whether two concrete types can meet in a
// comparison without a conversion the pass would insert, refusing the
// pairings Postgres has no operator for. A few pairings Postgres
// refuses stay accepted on purpose: an integer against a date or
// timestamp (the runtime value is the day or microsecond count, and
// embedded callers bind those), bytea against uuid (a uuid's runtime
// value is its 16 bytes, and that is how pgx's binary format and Go's
// [16]byte bind one), and jsonb or text[] against text (their runtime
// value is the canonical text).
func compatible(op string, l, r bytdb.ColType) error {
	if l == r || (numericType(l) && numericType(r)) || (temporalType(l) && temporalType(r)) {
		return nil
	}
	if (l == bytdb.TInt && temporalType(r)) || (r == bytdb.TInt && temporalType(l)) {
		return nil
	}
	if (l == bytdb.TUUID && r == bytdb.TBytes) || (l == bytdb.TBytes && r == bytdb.TUUID) {
		return nil
	}
	textual := func(t bytdb.ColType) bool {
		return t == bytdb.TString || t == bytdb.TJSONB || t == bytdb.TTextArray
	}
	if textual(l) && textual(r) {
		return nil
	}
	return noOperator(op, l, r)
}

// cmpPair reconciles the two operands of a comparison: an untyped
// literal takes the other side's type, a date facing a timestamp is cast
// to one, and impossible pairings are refused.
func (t *typer) cmpPair(op PredOp, l Expr, lt bytdb.ColType, r Expr, rt bytdb.ColType) (Expr, Expr, error) {
	switch {
	case lt == "" || rt == "" || lt == tyNull || rt == tyNull:
		return l, r, nil
	case lt == tyUnknown && rt == tyUnknown:
		return l, r, nil // two untyped literals compare as text
	case lt == tyUnknown:
		nl, err := t.coerceTo(l, rt)
		return nl, r, err
	case rt == tyUnknown:
		nr, err := t.coerceTo(r, lt)
		return l, nr, err
	}
	if lt == bytdb.TDate && rt == bytdb.TTimestamp {
		return dateToTimestamp(l), r, nil
	}
	if lt == bytdb.TTimestamp && rt == bytdb.TDate {
		return l, dateToTimestamp(r), nil
	}
	if err := compatible(opText(op), lt, rt); err != nil {
		return nil, nil, err
	}
	return l, r, nil
}

// dateToTimestamp casts a date-typed expression to a timestamp, so it
// compares in microseconds, not days. A literal converts here, once.
func dateToTimestamp(e Expr) Expr {
	if lit, ok := e.(*ExLit); ok {
		if days, ok := lit.Val.(int64); ok {
			return &ExLit{Val: days * 86_400_000_000, Name: lit.Name, Typ: bytdb.TTimestamp}
		}
	}
	return &ExCast{E: e, Type: "timestamp", From: bytdb.TDate}
}

// coerceTo converts an untyped expression to type ct: a string or
// time.Time literal parses as ct's input syntax; a placeholder
// (Describe) records ct as its type; CASE and COALESCE push the type
// into their branches. Anything else is left for the executor.
func (t *typer) coerceTo(e Expr, ct bytdb.ColType) (Expr, error) {
	if !concrete(ct) {
		return e, nil
	}
	switch n := e.(type) {
	case *ExLit:
		if p, ok := n.Val.(Param); ok {
			if t.note != nil {
				t.note(p, ct)
			}
			return e, nil
		}
		if litStaticType(n.Val, n.Typ) != tyUnknown {
			return e, nil
		}
		if ct == bytdb.TString {
			return e, nil
		}
		cv, err := coerceLit(n.Val, ct)
		if err != nil {
			return nil, err
		}
		return &ExLit{Val: cv, Name: n.Name, Typ: ct}, nil
	case *ExCase:
		c := *n
		changed := false
		whens := append([]ExWhen(nil), n.Whens...)
		for i, w := range n.Whens {
			th, err := t.coerceTo(w.Then, ct)
			if err != nil {
				return nil, err
			}
			if th != w.Then {
				whens[i].Then, changed = th, true
			}
		}
		if n.Else != nil {
			el, err := t.coerceTo(n.Else, ct)
			if err != nil {
				return nil, err
			}
			if el != n.Else {
				c.Else, changed = el, true
			}
		}
		if !changed {
			return e, nil
		}
		c.Whens = whens
		return &c, nil
	case *ExFunc:
		if n.Name != "coalesce" && n.Name != "greatest" && n.Name != "least" && n.Name != "nullif" {
			return e, nil
		}
		args, changed := n.Args, false
		for i, a := range n.Args {
			na, err := t.coerceTo(a, ct)
			if err != nil {
				return nil, err
			}
			if na != a {
				if !changed {
					args = append([]Expr(nil), n.Args...)
				}
				args[i], changed = na, true
			}
		}
		if !changed {
			return e, nil
		}
		return &ExFunc{Name: n.Name, Args: args}, nil
	}
	return e, nil
}

// litStaticType is a literal value's type for the pass: strings and
// time.Time are untyped (they adapt to context), NULL is tyNull, a
// placeholder is untyped, and a literal the pass already converted
// carries its type in typ.
func litStaticType(v any, typ bytdb.ColType) bytdb.ColType {
	if typ != "" {
		return typ
	}
	switch v.(type) {
	case nil:
		return tyNull
	case string, Param:
		return tyUnknown
	}
	if isTimeValue(v) {
		return tyUnknown
	}
	return litType(v)
}

func isTimeValue(v any) bool {
	_, ok := v.(time.Time)
	return ok
}

// --- expressions ---

// expr types one expression, returning it (rebuilt only if something
// under it changed) and its static type.
func (t *typer) expr(e Expr, ts *tscope) (Expr, bytdb.ColType, error) {
	switch n := e.(type) {
	case nil:
		return nil, "", nil
	case *ExLit:
		return n, litStaticType(n.Val, n.Typ), nil
	case *exRow:
		return nil, "", errRowConstructor()
	case *ExCol:
		return n, ts.colType(n.Col), nil
	case *ExCmp:
		l, lt, err := t.expr(n.L, ts)
		if err != nil {
			return nil, "", err
		}
		r, rt, err := t.expr(n.R, ts)
		if err != nil {
			return nil, "", err
		}
		if orderingOp(n.Op) {
			if l, r, err = t.cmpPair(n.Op, l, lt, r, rt); err != nil {
				return nil, "", err
			}
		}
		if l == n.L && r == n.R {
			return n, bytdb.TBool, nil
		}
		return &ExCmp{Op: n.Op, L: l, R: r}, bytdb.TBool, nil
	case *ExAny:
		return t.anyAll(n, ts)
	case *ExIn:
		return t.in(n, ts)
	case *ExIsNull:
		sub, _, err := t.expr(n.E, ts)
		if err != nil {
			return nil, "", err
		}
		if sub == n.E {
			return n, bytdb.TBool, nil
		}
		return &ExIsNull{E: sub, Not: n.Not}, bytdb.TBool, nil
	case *ExAnd:
		exprs, changed, err := t.exprs(n.Exprs, ts)
		if err != nil {
			return nil, "", err
		}
		if !changed {
			return n, bytdb.TBool, nil
		}
		return &ExAnd{Exprs: exprs}, bytdb.TBool, nil
	case *ExOr:
		exprs, changed, err := t.exprs(n.Exprs, ts)
		if err != nil {
			return nil, "", err
		}
		if !changed {
			return n, bytdb.TBool, nil
		}
		return &ExOr{Exprs: exprs}, bytdb.TBool, nil
	case *ExNot:
		sub, _, err := t.expr(n.E, ts)
		if err != nil {
			return nil, "", err
		}
		if sub == n.E {
			return n, bytdb.TBool, nil
		}
		return &ExNot{E: sub}, bytdb.TBool, nil
	case *ExCase:
		return t.caseExpr(n, ts)
	case *ExFunc:
		return t.funcExpr(n, ts)
	case *ExCast:
		sub, st, err := t.expr(n.E, ts)
		if err != nil {
			return nil, "", err
		}
		to := castColType(n.Type)
		if st == tyUnknown {
			if lit, ok := sub.(*ExLit); ok {
				if p, ok := lit.Val.(Param); ok && t.note != nil && !strings.HasSuffix(n.Type, "[]") {
					t.note(p, to)
				}
			}
		}
		from := n.From
		if concrete(st) {
			from = st
		}
		if sub == n.E && from == n.From {
			return n, to, nil
		}
		return &ExCast{E: sub, Type: n.Type, From: from}, to, nil
	case *ExArith:
		return t.arith(n, ts)
	case *ExSub:
		sel, types, err := t.sel(n.Sel, ts)
		if err != nil {
			return nil, "", err
		}
		var typ bytdb.ColType
		if len(types) > 0 {
			typ = outputType(types[0])
		}
		if sel == n.Sel && typ == n.Typ {
			return n, typ, nil
		}
		return &ExSub{Sel: sel, Typ: typ}, typ, nil
	case *ExAgg:
		var arg bytdb.ColType
		c := *n
		changed := false
		if n.Arg != nil {
			a, at, err := t.expr(n.Arg, ts)
			if err != nil {
				return nil, "", err
			}
			arg = at
			if a != n.Arg {
				c.Arg, changed = a, true
			}
		} else if !n.Star {
			arg = ts.colType(n.Col)
		}
		typ := t.aggType(n.Fn, n.Star, arg, n.Col)
		if !changed {
			return n, typ, nil
		}
		return &c, typ, nil
	case *ExWindow:
		c := *n
		changed := false
		for _, p := range []*Expr{&c.Arg, &c.Offset, &c.Default} {
			if *p == nil {
				continue
			}
			ne, _, err := t.expr(*p, ts)
			if err != nil {
				return nil, "", err
			}
			if ne != *p {
				*p, changed = ne, true
			}
		}
		part, pc, err := t.exprs(n.Partition, ts)
		if err != nil {
			return nil, "", err
		}
		if pc {
			c.Partition, changed = part, true
		}
		var typ bytdb.ColType
		if sc := ts.innermost(); sc != nil {
			typ = n.resultType(sc)
		}
		if !changed {
			return n, typ, nil
		}
		return &c, typ, nil
	case *ExArray:
		elems, changed, err := t.exprs(n.Elems, ts)
		if err != nil {
			return nil, "", err
		}
		if !changed {
			return n, "", nil
		}
		return &ExArray{Elems: elems}, "", nil
	case *ExIndex:
		sub, _, err := t.expr(n.E, ts)
		if err != nil {
			return nil, "", err
		}
		idx, _, err := t.expr(n.Idx, ts)
		if err != nil {
			return nil, "", err
		}
		if sub == n.E && idx == n.Idx {
			return n, "", nil
		}
		return &ExIndex{E: sub, Idx: idx}, "", nil
	}
	// Executor-internal nodes (group, accumulator and window references)
	// never reach the pass; anything else keeps its exprType typing.
	if sc := ts.innermost(); sc != nil {
		return e, exprType(sc, e), nil
	}
	return e, "", nil
}

func (t *typer) exprs(in []Expr, ts *tscope) ([]Expr, bool, error) {
	var out []Expr
	for i, e := range in {
		ne, _, err := t.expr(e, ts)
		if err != nil {
			return nil, false, err
		}
		if ne != e {
			if out == nil {
				out = append([]Expr(nil), in...)
			}
			out[i] = ne
		}
	}
	if out == nil {
		return in, false, nil
	}
	return out, true, nil
}

// in types x [NOT] IN (list): each untyped list element takes x's type,
// a date element facing a timestamp x is cast to one, impossible
// pairings are refused.
func (t *typer) in(n *ExIn, ts *tscope) (Expr, bytdb.ColType, error) {
	x, xt, err := t.expr(n.E, ts)
	if err != nil {
		return nil, "", err
	}
	list := n.List
	changed := x != n.E
	listCopied := false
	for i, el := range n.List {
		ne, et, err := t.expr(el, ts)
		if err != nil {
			return nil, "", err
		}
		if xt == tyUnknown && concrete(et) {
			// '5' IN (1, 2): the list decides; leave x to the runtime.
		} else if xt == bytdb.TTimestamp && et == bytdb.TDate {
			ne = dateToTimestamp(ne)
		} else if !(xt == bytdb.TDate && et == bytdb.TTimestamp) {
			if _, ne, err = t.cmpPair(OpEQ, x, xt, ne, et); err != nil {
				return nil, "", err
			}
		}
		if ne != el {
			if !listCopied {
				list, listCopied = append([]Expr(nil), n.List...), true
			}
			list[i], changed = ne, true
		}
	}
	if !changed {
		return n, bytdb.TBool, nil
	}
	return &ExIn{E: x, List: list, Not: n.Not}, bytdb.TBool, nil
}

// anyAll types x op ANY|ALL (right): array elements, a subquery's
// column, or a '{...}' array literal take x's type.
func (t *typer) anyAll(n *ExAny, ts *tscope) (Expr, bytdb.ColType, error) {
	x, xt, err := t.expr(n.L, ts)
	if err != nil {
		return nil, "", err
	}
	r := n.R
	// An array cast (ARRAY[...]::uuid[], '{...}'::int[]) only annotates
	// the right side here; the elements are what get typed, so look
	// through it. (anyElements looks through it the same way.)
	right := n.R
	if c, ok := right.(*ExCast); ok && strings.HasSuffix(c.Type, "[]") {
		right = c.E
	}
	switch rn := right.(type) {
	case *ExArray:
		elems := rn.Elems
		changed := false
		for i, el := range rn.Elems {
			ne, et, err := t.expr(el, ts)
			if err != nil {
				return nil, "", err
			}
			if orderingOp(n.Op) && et == tyUnknown && concrete(xt) {
				if ne, err = t.coerceTo(ne, xt); err != nil {
					return nil, "", err
				}
			}
			if ne != el {
				if !changed {
					elems = append([]Expr(nil), rn.Elems...)
				}
				elems[i], changed = ne, true
			}
		}
		if changed {
			r = &ExArray{Elems: elems}
		}
	case *ExSub:
		sub, _, err := t.expr(rn, ts)
		if err != nil {
			return nil, "", err
		}
		r = sub
		if et := sub.(*ExSub).Typ; orderingOp(n.Op) && xt == tyUnknown && concrete(et) {
			if x, err = t.coerceTo(x, et); err != nil {
				return nil, "", err
			}
		} else if xt == bytdb.TDate && et == bytdb.TTimestamp {
			x = dateToTimestamp(x)
		}
	default:
		// A '{...}' literal (or a bound string standing for one), with or
		// without an array cast: convert its elements to x's type once.
		inner := right
		ne, _, err := t.expr(inner, ts)
		if err != nil {
			return nil, "", err
		}
		if lit, ok := ne.(*ExLit); ok && orderingOp(n.Op) && concrete(xt) && xt != bytdb.TString {
			if s, ok := lit.Val.(string); ok {
				elems, err := parseArrayLiteral(s)
				if err != nil {
					return nil, "", err
				}
				for i, el := range elems {
					if elems[i], err = coerceLit(el, xt); err != nil {
						return nil, "", err
					}
				}
				r = &ExLit{Val: elems, Name: lit.Name}
				break
			}
		}
		if ne != inner {
			r = ne
		}
	}
	if x == n.L && r == n.R {
		return n, bytdb.TBool, nil
	}
	return &ExAny{Op: n.Op, L: x, R: r, All: n.All}, bytdb.TBool, nil
}

// caseExpr types CASE: with an operand, each WHEN value compares to it;
// the result type is the first typed branch's (numeric branches widen to
// float), and untyped branches convert to it.
func (t *typer) caseExpr(n *ExCase, ts *tscope) (Expr, bytdb.ColType, error) {
	c := *n
	changed := false
	var ot bytdb.ColType
	if n.Operand != nil {
		op, opt, err := t.expr(n.Operand, ts)
		if err != nil {
			return nil, "", err
		}
		ot = opt
		if op != n.Operand {
			c.Operand, changed = op, true
		}
	}
	whens := append([]ExWhen(nil), n.Whens...)
	var branchTypes []bytdb.ColType
	for i, w := range n.Whens {
		wh, wt, err := t.expr(w.When, ts)
		if err != nil {
			return nil, "", err
		}
		if n.Operand != nil {
			if wt == tyUnknown && concrete(ot) {
				if wh, err = t.coerceTo(wh, ot); err != nil {
					return nil, "", err
				}
			} else if concrete(ot) && concrete(wt) {
				if err := compatible("=", ot, wt); err != nil {
					return nil, "", err
				}
			}
		}
		th, tt, err := t.expr(w.Then, ts)
		if err != nil {
			return nil, "", err
		}
		branchTypes = append(branchTypes, tt)
		if wh != w.When || th != w.Then {
			whens[i] = ExWhen{When: wh, Then: th}
			changed = true
		}
	}
	if n.Else != nil {
		el, et, err := t.expr(n.Else, ts)
		if err != nil {
			return nil, "", err
		}
		branchTypes = append(branchTypes, et)
		if el != n.Else {
			c.Else, changed = el, true
		}
	}
	if changed {
		c.Whens = whens
	}
	var out Expr = n
	if changed {
		out = &c
	}
	rt := unifyTypes(branchTypes)
	if concrete(rt) && rt != bytdb.TString {
		ne, err := t.coerceTo(out, rt)
		if err != nil {
			return nil, "", err
		}
		out = ne
	}
	if !concrete(rt) {
		rt = bytdb.TString
	}
	return out, rt, nil
}

// unifyTypes is the result type of branches that must agree (CASE,
// COALESCE): the first concrete type, widened to float when integer and
// float branches mix; "" when none is concrete.
func unifyTypes(ts []bytdb.ColType) bytdb.ColType {
	var out bytdb.ColType
	for _, t := range ts {
		if !concrete(t) {
			continue
		}
		switch {
		case out == "":
			out = t
		case numericType(out) && numericType(t) && out != t:
			out = bytdb.TFloat
		}
	}
	return out
}

// funcExpr types a function call's arguments and its result.
func (t *typer) funcExpr(n *ExFunc, ts *tscope) (Expr, bytdb.ColType, error) {
	args := n.Args
	changed := false
	argTypes := make([]bytdb.ColType, len(n.Args))
	for i, a := range n.Args {
		na, at, err := t.expr(a, ts)
		if err != nil {
			return nil, "", err
		}
		argTypes[i] = at
		if na != a {
			if !changed {
				args = append([]Expr(nil), n.Args...)
			}
			args[i], changed = na, true
		}
	}
	var typ bytdb.ColType
	switch n.Name {
	case "coalesce", "greatest", "least":
		typ = unifyTypes(argTypes)
		if concrete(typ) && typ != bytdb.TString {
			for i, a := range args {
				if argTypes[i] != tyUnknown {
					continue
				}
				na, err := t.coerceTo(a, typ)
				if err != nil {
					return nil, "", err
				}
				if na != a {
					if !changed {
						args = append([]Expr(nil), n.Args...)
					}
					args[i], changed = na, true
				}
			}
		}
		if !concrete(typ) {
			typ = bytdb.TString
		}
	case "nullif":
		if len(args) == 2 {
			l, r, err := t.cmpPair(OpEQ, args[0], argTypes[0], args[1], argTypes[1])
			if err != nil {
				return nil, "", err
			}
			if l != args[0] || r != args[1] {
				if !changed {
					args = append([]Expr(nil), n.Args...)
				}
				args[0], args[1], changed = l, r, true
			}
			typ = argTypes[0]
		}
		if !concrete(typ) {
			typ = bytdb.TString
		}
	case "exists":
		typ = bytdb.TBool
	default:
		typ = funcTypes[n.Name]
	}
	if !changed {
		return n, typ, nil
	}
	return &ExFunc{Name: n.Name, Args: args}, typ, nil
}

// arith types the binary operators: + - * / % and ||.
func (t *typer) arith(n *ExArith, ts *tscope) (Expr, bytdb.ColType, error) {
	l, lt, err := t.expr(n.L, ts)
	if err != nil {
		return nil, "", err
	}
	r, rt, err := t.expr(n.R, ts)
	if err != nil {
		return nil, "", err
	}
	// The jsonb-or-not decision for || and -, as the evaluator's
	// staticJSONB would make it per row: jsonb when either operand is
	// statically jsonb, plain when both have a concrete other type, and
	// left to run time when an operand's type is not known here.
	kind := n.Kind
	if n.Op == "||" || n.Op == "-" {
		switch {
		case lt == bytdb.TJSONB || rt == bytdb.TJSONB:
			kind = arithJSONB
		case concrete(lt) && concrete(rt):
			kind = arithPlain
		}
	}
	rebuilt := func(typ bytdb.ColType) (Expr, bytdb.ColType, error) {
		if l == n.L && r == n.R && kind == n.Kind {
			return n, typ, nil
		}
		return &ExArith{Op: n.Op, L: l, R: r, Kind: kind}, typ, nil
	}
	switch n.Op {
	case "->", "#>":
		return rebuilt(bytdb.TJSONB)
	case "->>", "#>>":
		return rebuilt(bytdb.TString)
	case "||":
		if lt == bytdb.TJSONB || rt == bytdb.TJSONB {
			return rebuilt(bytdb.TJSONB)
		}
		if lt == bytdb.TTextArray || rt == bytdb.TTextArray {
			return rebuilt(bytdb.TTextArray)
		}
		if lt == bytdb.TBytes && rt == bytdb.TBytes {
			return rebuilt(bytdb.TBytes)
		}
		textish := func(t bytdb.ColType) bool { return !concrete(t) || t == bytdb.TString }
		if !textish(lt) && !textish(rt) {
			return nil, "", noOperator("||", lt, rt)
		}
		// A typed non-text operand renders as text by its type: a date
		// as 2024-02-29, not its day number (D-24).
		if concrete(lt) && lt != bytdb.TString {
			l = &ExCast{E: l, Type: "text", From: lt}
		}
		if concrete(rt) && rt != bytdb.TString {
			r = &ExCast{E: r, Type: "text", From: rt}
		}
		return rebuilt(bytdb.TString)
	}
	if n.Op == "-" && (lt == bytdb.TJSONB) {
		return rebuilt(bytdb.TJSONB)
	}
	// An untyped literal operand takes its sibling's numeric type (or
	// an integer, beside a date: date + '1').
	if lt == tyUnknown && concrete(rt) {
		target := rt
		if temporalType(rt) {
			target = bytdb.TInt
		}
		if l, err = t.coerceTo(l, target); err != nil {
			return nil, "", err
		}
		lt = target
	}
	if rt == tyUnknown && concrete(lt) {
		target := lt
		if temporalType(lt) {
			target = bytdb.TInt
		}
		if r, err = t.coerceTo(r, target); err != nil {
			return nil, "", err
		}
		rt = target
	}
	switch {
	case !concrete(lt) || !concrete(rt):
		return rebuilt("")
	case lt == bytdb.TDate && rt == bytdb.TInt && (n.Op == "+" || n.Op == "-"):
		return rebuilt(bytdb.TDate)
	case lt == bytdb.TInt && rt == bytdb.TDate && n.Op == "+":
		return rebuilt(bytdb.TDate)
	case lt == bytdb.TDate && rt == bytdb.TDate && n.Op == "-":
		return rebuilt(bytdb.TInt)
	case lt == bytdb.TTimestamp && rt == bytdb.TTimestamp && n.Op == "-":
		// Postgres yields an interval, which bytdb lacks; the difference
		// in microseconds is what this has always returned.
		return rebuilt(bytdb.TInt)
	case temporalType(lt) || temporalType(rt):
		// timestamp + integer is the plan's decision Q1: refused as in
		// Postgres rather than read as microseconds.
		return nil, "", noOperator(n.Op, lt, rt)
	case numericType(lt) && numericType(rt):
		if lt == bytdb.TFloat || rt == bytdb.TFloat {
			return rebuilt(bytdb.TFloat)
		}
		return rebuilt(bytdb.TInt)
	}
	return nil, "", noOperator(n.Op, lt, rt)
}
