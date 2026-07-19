package sql

// explain.go: EXPLAIN renders the plan the executor would run —
// which access path each table gets (point get, index scan, seq
// scan), what pushes into the scan (Index Cond) versus filters per
// row (Filter), and the join, aggregate, sort, and limit structure on
// top — as the indented tree Postgres prints. Costs are never shown:
// bytdb has no cost model, and inventing numbers would be worse than
// omitting them. Scalar subqueries render inline as (subquery)
// without their own plan nodes.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// planNode is one node of the rendered plan tree.
type planNode struct {
	title    string
	details  []string
	children []*planNode
}

// renderPlan flattens the tree into Postgres's text shape: children
// prefixed with "->", detail lines indented two past their node.
func renderPlan(n *planNode, level int, out *[]string) {
	if level == 0 {
		*out = append(*out, n.title)
	} else {
		*out = append(*out, strings.Repeat(" ", level*6-4)+"->  "+n.title)
	}
	pad := strings.Repeat(" ", level*6+2)
	for _, d := range n.details {
		*out = append(*out, pad+d)
	}
	for _, c := range n.children {
		renderPlan(c, level+1, out)
	}
}

func (d *DB) execExplain(s *Explain) (*Result, error) {
	res := &Result{Cols: []string{"QUERY PLAN"}, Types: []bytdb.ColType{bytdb.TString}}
	err := d.read(func(tx *bytdb.Txn) error {
		x := &explainer{d: d, tx: tx, tmpl: map[*Pred]string{}}
		var root *planNode
		var err error
		switch st := s.Stmt.(type) {
		case *Select:
			root, err = x.selectNode(st)
		case *Insert:
			root, err = x.insertNode(st)
		case *Update:
			root, err = x.writeNode("Update", st.Table, st.Where)
		case *Delete:
			root, err = x.writeNode("Delete", st.Table, st.Where)
		default:
			err = serr.New("EXPLAIN supports SELECT, INSERT, UPDATE, and DELETE")
		}
		if err != nil {
			return err
		}
		var lines []string
		renderPlan(root, 0, &lines)
		for _, l := range lines {
			res.Rows = append(res.Rows, []any{l})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// explainer carries the plan-building context: tmpl maps the
// placeholder predicates synthesized for join templates to their real
// rendering ("(user_id = u.id)" instead of "(user_id = 0)").
type explainer struct {
	d    *DB
	tx   *bytdb.Txn
	tmpl map[*Pred]string
	// Set for a single-table SELECT whose scan yields ORDER BY order:
	// scanNode renders step0Plan instead of re-planning, and selectNode
	// drops the Sort node. Reset per SELECT core.
	step0Plan *plan
	orderElim bool
}

// --- statement nodes ---

func (x *explainer) insertNode(s *Insert) (*planNode, error) {
	if x.tx.Table(s.Table) == nil {
		return nil, serr.New("no such table", "table", s.Table)
	}
	return &planNode{
		title:    "Insert on " + s.Table,
		children: []*planNode{{title: `Values Scan on "*VALUES*"`}},
	}, nil
}

// writeNode explains an UPDATE or DELETE: the write on top of the
// scan that finds its rows.
func (x *explainer) writeNode(verb, table string, where BoolExpr) (*planNode, error) {
	desc := x.tx.Table(table)
	if desc == nil {
		return nil, serr.New("no such table", "table", table)
	}
	scan, err := x.scanNode(desc, table, table, where)
	if err != nil {
		return nil, err
	}
	return &planNode{title: verb + " on " + table, children: []*planNode{scan}}, nil
}

// selectNode explains a whole SELECT: UNION arms under an Append,
// then Sort and Limit on top.
func (x *explainer) selectNode(s *Select) (*planNode, error) {
	var n *planNode
	var err error
	if len(s.Union) > 0 {
		app := &planNode{title: "Append"}
		head := *s
		head.Union, head.OrderBy = nil, nil
		head.Limit, head.Offset = -1, 0
		arm, err := x.armNode(&head)
		if err != nil {
			return nil, err
		}
		app.children = append(app.children, arm)
		distinct := false
		for _, u := range s.Union {
			if arm, err = x.armNode(u.Sel); err != nil {
				return nil, err
			}
			app.children = append(app.children, arm)
			distinct = distinct || !u.All
		}
		n = app
		if distinct {
			n = &planNode{title: "Unique", children: []*planNode{app}}
		}
	} else if n, err = x.armNode(s); err != nil {
		return nil, err
	}
	if len(s.OrderBy) > 0 && !x.orderElim {
		keys := make([]string, len(s.OrderBy))
		for i, o := range s.OrderBy {
			keys[i] = itemText(o.SelectItem)
			if o.Desc {
				keys[i] += " DESC"
			}
		}
		n = &planNode{title: "Sort", details: []string{"Sort Key: " + strings.Join(keys, ", ")},
			children: []*planNode{n}}
	}
	if s.Limit >= 0 || s.Offset > 0 {
		n = &planNode{title: "Limit", children: []*planNode{n}}
	}
	return n, nil
}

// armNode explains one SELECT body (a UNION arm or a whole statement's
// core): the core plan, under a Unique node when it is SELECT
// DISTINCT. Mirroring execution, a DISTINCT body's core runs with
// ORDER BY / OFFSET / LIMIT stripped — dedup happens on the projected
// rows first, then the statement-level Sort and Limit nodes apply
// above the Unique (so an order-eliminating scan is never claimed:
// the sort runs after dedup, not in the scan).
func (x *explainer) armNode(s *Select) (*planNode, error) {
	if !s.Distinct {
		return x.coreNode(s)
	}
	core := *s
	core.Distinct = false
	core.OrderBy, core.Limit, core.Offset = nil, -1, 0
	n, err := x.coreNode(&core)
	if err != nil {
		return nil, err
	}
	return &planNode{title: "Unique", children: []*planNode{n}}, nil
}

// coreNode explains one SELECT core: the FROM tree, wrapped in an
// aggregation node when the query aggregates rows.
func (x *explainer) coreNode(s *Select) (*planNode, error) {
	if len(s.From) == 0 {
		return &planNode{title: "Result"}, nil
	}
	fp, err := prepareFrom(x.d.lookup(x.tx.Table), s.From, s.Where)
	if err != nil {
		return nil, err
	}
	x.step0Plan, x.orderElim = nil, false // reset per core (UNION reuses the explainer)
	if s.isAggregate() {
		n, err := x.fromNode(fp)
		if err != nil {
			return nil, err
		}
		q, err := resolveAgg(fp.sc, s, false) // validate as execution would
		if err != nil {
			return nil, err
		}
		agg := &planNode{title: "Aggregate", children: []*planNode{n}}
		if len(q.keys) > 0 {
			agg.title = "HashAggregate"
			keys := make([]string, len(q.keys))
			for i, k := range q.keys {
				keys[i] = exprText(k.ex)
			}
			agg.details = append(agg.details, "Group Key: "+strings.Join(keys, ", "))
		}
		if s.Having != nil {
			agg.details = append(agg.details, "Filter: "+x.condText(boolParts(s.Having)))
		}
		// Windows evaluate after grouping, so the WindowAgg node sits
		// above the aggregation, as in Postgres.
		if hasWindow(s) {
			return windowNode(s, agg), nil
		}
		return agg, nil
	}
	proj, exprs, err := projectSelect(fp.sc, s, &Result{}) // validate the select list as execution would
	if err != nil {
		return nil, err
	}
	// Decide the order-eliminating scan before building the FROM node,
	// so scanNode can render it (and selectNode can drop the sort).
	if !hasWindow(s) {
		keys, _, err := buildSortKeys(fp.sc, s, proj, exprs)
		if err != nil {
			return nil, err
		}
		if p, elim, err := orderedScan(fp, s, keys); err != nil {
			return nil, err
		} else if elim {
			x.step0Plan, x.orderElim = p, true
		}
	}
	n, err := x.fromNode(fp)
	if err != nil {
		return nil, err
	}
	if hasWindow(s) {
		return windowNode(s, n), nil
	}
	return n, nil
}

// windowNode wraps a plan node in a WindowAgg listing the query's
// window calls (from the select list and ORDER BY).
func windowNode(s *Select, n *planNode) *planNode {
	win := &planNode{title: "WindowAgg", children: []*planNode{n}}
	visit := func(e Expr) bool {
		if w, ok := e.(*ExWindow); ok {
			win.details = append(win.details, "Window: "+windowText(w))
		}
		return true
	}
	for _, it := range s.Items {
		walkExpr(it.Ex, visit)
	}
	for _, o := range s.OrderBy {
		walkExpr(o.Ex, visit)
	}
	return win
}

// fromNode explains a prepared FROM clause as the left-deep chain of
// nested loops runJoin executes, replaying each step's conjunct
// assignment so every scan shows the access path it gets per outer
// row.
func (x *explainer) fromNode(fp *fromPlan) (*planNode, error) {
	// Conjuncts a step consumed (pushed or per-scan filter); whatever
	// remains of the WHERE applies at the top of the join.
	claimed := map[BoolExpr]bool{}
	var n *planNode
	for k := range fp.steps {
		step := &fp.steps[k]
		scan, err := x.stepScan(fp, step, claimed)
		if err != nil {
			return nil, err
		}
		if k == 0 {
			n = scan
			continue
		}
		title := "Nested Loop"
		if step.it.Join == JoinLeft {
			title = "Nested Loop Left Join"
		}
		var hashCond string
		if len(step.hashEq) > 0 {
			title = "Hash Join"
			if step.it.Join == JoinLeft {
				title = "Hash Left Join"
			}
			var conds []string
			for _, tp := range step.tmpls {
				if tp.op != OpEQ {
					continue
				}
				src := fp.sc.tableOf(tp.srcOrd)
				st := fp.sc.tables[src]
				conds = append(conds, "("+tp.item.Col.String()+" = "+
					st.name+"."+st.desc.Columns[tp.srcOrd-st.off].Name+")")
			}
			hashCond = strings.Join(conds, " AND ")
		}
		loop := &planNode{title: title, children: []*planNode{n, scan}}
		if hashCond != "" {
			loop.details = append(loop.details, "Hash Cond: "+hashCond)
		}
		if parts := unclaimed(boolParts(step.it.On), claimed); len(parts) > 0 {
			loop.details = append(loop.details, "Join Filter: "+x.condText(parts))
		}
		n = loop
	}
	// WHERE parts no scan consumed apply after the join — on the top
	// node (which for one table is the scan itself).
	if parts := unclaimed(boolParts(fp.where), claimed); len(parts) > 0 {
		n.details = append(n.details, "Filter: "+x.condText(parts))
	}
	return n, nil
}

// stepScan explains one join step's table access, synthesizing the
// per-outer-row template predicates with placeholder values of the
// source column's type — the chosen path depends only on the shape,
// so the plan matches what each outer row will get.
func (x *explainer) stepScan(fp *fromPlan, step *joinStep, claimed map[BoolExpr]bool) (*planNode, error) {
	display := step.it.Table
	if alias := step.st.name; alias != display {
		display += " " + alias
	}
	if step.st.rows != nil { // a virtual system table materializes
		return &planNode{title: "Seq Scan on " + display}, nil
	}
	exprs := make([]BoolExpr, 0, len(step.static)+len(step.tmpls))
	exprs = append(exprs, step.static...)
	for _, e := range step.static {
		claimed[e] = true
	}
	for _, tp := range step.tmpls {
		// A hash step consumes its equality templates as the hash
		// condition (rendered on the join node), not as per-row scan
		// predicates; its other templates fall through to Join Filter.
		if len(step.hashEq) > 0 {
			if tp.op == OpEQ {
				claimed[tp.pr] = true
			}
			continue
		}
		src := fp.sc.tableOf(tp.srcOrd)
		st := fp.sc.tables[src]
		srcName := st.name + "." + st.desc.Columns[tp.srcOrd-st.off].Name
		pr := &Pred{Item: tp.item, Op: tp.op, Val: placeholderVal(fp.sc.column(tp.srcOrd).Type)}
		x.tmpl[pr] = "(" + tp.item.Col.String() + " " + opText(tp.op) + " " + srcName + ")"
		exprs = append(exprs, pr)
		claimed[tp.pr] = true
	}
	var where BoolExpr
	switch {
	case len(exprs) == 1:
		where = exprs[0]
	case len(exprs) > 1:
		where = &And{Exprs: exprs}
	}
	return x.scanNode(step.st.desc, step.st.name, display, where)
}

// scanNode explains one table read: planScan picks the path exactly
// as execution will; its pushed conjuncts render as the Index Cond
// (or point-get Key) and the rest as the scan's Filter.
func (x *explainer) scanNode(desc *bytdb.TableDesc, alias, display string, where BoolExpr) (*planNode, error) {
	pl := x.step0Plan // the order-aware plan, when this is the sorted single-table scan
	if pl == nil {
		var err error
		if pl, err = planScan(desc, alias, where); err != nil {
			return nil, err
		}
	}
	pushed := map[BoolExpr]bool{}
	for _, pr := range pl.pushed {
		pushed[pr] = true
	}
	n := &planNode{}
	cond := "Index Cond: "
	backward := ""
	if pl.reverse {
		backward = " Backward"
	}
	ordered := pl == x.step0Plan // this scan is read in ORDER BY order
	switch {
	case pl.get != nil:
		n.title = "Point Get on " + display
		cond = "Key: "
	case pl.index != "":
		n.title = "Index Scan" + backward + " using " + pl.index + " on " + display
	case len(pl.from) > 0 || len(pl.stops) > 0 || ordered:
		n.title = "Index Scan" + backward + " using " + desc.Name + "_pkey on " + display
	default:
		n.title = "Seq Scan on " + display
	}
	if len(pl.pushed) > 0 {
		parts := make([]BoolExpr, len(pl.pushed))
		for i, pr := range pl.pushed {
			parts[i] = pr
		}
		n.details = append(n.details, cond+x.condText(parts))
	}
	if parts := unclaimed(boolParts(where), pushed); len(parts) > 0 {
		n.details = append(n.details, "Filter: "+x.condText(parts))
	}
	return n, nil
}

// boolParts flattens a condition into the parts that must all hold:
// the children of its top-level AND chain.
func boolParts(e BoolExpr) []BoolExpr {
	switch n := e.(type) {
	case nil:
		return nil
	case *And:
		var out []BoolExpr
		for _, sub := range n.Exprs {
			out = append(out, boolParts(sub)...)
		}
		return out
	}
	return []BoolExpr{e}
}

// unclaimed filters parts down to those not in the claimed set.
func unclaimed(parts []BoolExpr, claimed map[BoolExpr]bool) []BoolExpr {
	var out []BoolExpr
	for _, p := range parts {
		if !claimed[p] {
			out = append(out, p)
		}
	}
	return out
}

// placeholderVal is a stand-in of a source column's value kind, so a
// synthesized template predicate pushes down exactly when the real
// per-row value would.
func placeholderVal(t bytdb.ColType) any {
	switch t {
	case bytdb.TInt:
		return int64(0)
	case bytdb.TFloat:
		return 0.0
	case bytdb.TBool:
		return false
	case bytdb.TBytes:
		return []byte{}
	}
	return ""
}

// condText renders parts joined with AND, parenthesized when there is
// more than one.
func (x *explainer) condText(parts []BoolExpr) string {
	txts := make([]string, len(parts))
	for i, p := range parts {
		txts[i] = x.boolText(p)
	}
	out := strings.Join(txts, " AND ")
	if len(txts) > 1 {
		out = "(" + out + ")"
	}
	return out
}

// boolText renders one condition subtree.
func (x *explainer) boolText(e BoolExpr) string {
	switch n := e.(type) {
	case nil:
		return ""
	case *Pred:
		if t, ok := x.tmpl[n]; ok {
			return t
		}
		return exprText(predToExpr(n))
	case *Cond:
		return exprText(n.Ex)
	case *Not:
		return "(NOT " + x.boolText(n.Expr) + ")"
	case *And:
		return x.joinBool(n.Exprs, " AND ")
	case *Or:
		return x.joinBool(n.Exprs, " OR ")
	}
	return "?"
}

func (x *explainer) joinBool(subs []BoolExpr, sep string) string {
	txts := make([]string, len(subs))
	for i, s := range subs {
		txts[i] = x.boolText(s)
	}
	return "(" + strings.Join(txts, sep) + ")"
}

// predToExpr renders a predicate leaf back as an expression,
// aggregate items included (predExpr handles only column items).
func predToExpr(pr *Pred) Expr {
	l := itemToAggExpr(pr.Item)
	switch pr.Op {
	case OpIsNull:
		return &ExIsNull{E: l}
	case OpIsNotNull:
		return &ExIsNull{E: l, Not: true}
	}
	var r Expr
	if pr.RItem != nil {
		r = itemToAggExpr(*pr.RItem)
	} else {
		r = &ExLit{Val: pr.Val}
	}
	return &ExCmp{Op: pr.Op, L: l, R: r}
}

func itemToAggExpr(it SelectItem) Expr {
	if it.Agg != AggNone {
		return &ExAgg{Fn: it.Agg, Col: it.Col, Star: it.Star}
	}
	return itemToExpr(it)
}

// itemText renders a select item (for Sort Key and Group Key lines).
func itemText(it SelectItem) string {
	return exprText(itemToAggExpr(it))
}

// opText is a comparison operator's SQL spelling (!= normalizes to
// <>, as Postgres prints it).
func opText(op PredOp) string {
	switch op {
	case OpEQ:
		return "="
	case OpNE:
		return "<>"
	case OpLT:
		return "<"
	case OpLE:
		return "<="
	case OpGT:
		return ">"
	case OpGE:
		return ">="
	case OpRegex:
		return "~"
	case OpNotRegex:
		return "!~"
	case OpRegexI:
		return "~*"
	case OpNotRegexI:
		return "!~*"
	// Postgres prints LIKE through its operator names (~~ family) in
	// plan output; matching that keeps EXPLAIN diff-able against it.
	case OpLike:
		return "~~"
	case OpNotLike:
		return "!~~"
	case OpILike:
		return "~~*"
	case OpNotILike:
		return "!~~*"
	}
	return "?"
}

// exprText renders an expression as SQL-shaped text for plan lines.
func exprText(e Expr) string {
	switch n := e.(type) {
	case nil:
		return ""
	case *ExLit:
		return litText(n.Val)
	case *ExCol:
		return n.Col.String()
	case *ExAgg:
		arg := "*"
		switch {
		case n.Arg != nil:
			arg = exprText(n.Arg)
		case !n.Star:
			arg = n.Col.String()
		}
		if n.Distinct {
			arg = "DISTINCT " + arg
		}
		return n.Fn.name() + "(" + arg + ")"
	case *ExAnd:
		return joinExprs(n.Exprs, " AND ")
	case *ExOr:
		return joinExprs(n.Exprs, " OR ")
	case *ExNot:
		return "(NOT " + exprText(n.E) + ")"
	case *ExCmp:
		return "(" + exprText(n.L) + " " + opText(n.Op) + " " + exprText(n.R) + ")"
	case *ExAny:
		kw := "ANY"
		if n.All {
			kw = "ALL"
		}
		return "(" + exprText(n.L) + " " + opText(n.Op) + " " + kw + "(" + exprText(n.R) + "))"
	case *ExIsNull:
		if n.Not {
			return "(" + exprText(n.E) + " IS NOT NULL)"
		}
		return "(" + exprText(n.E) + " IS NULL)"
	case *ExIn:
		txts := make([]string, len(n.List))
		for i, le := range n.List {
			txts[i] = exprText(le)
		}
		not := ""
		if n.Not {
			not = " NOT"
		}
		return "(" + exprText(n.E) + not + " IN (" + strings.Join(txts, ", ") + "))"
	case *ExCase:
		var b strings.Builder
		b.WriteString("CASE")
		if n.Operand != nil {
			b.WriteString(" " + exprText(n.Operand))
		}
		for _, w := range n.Whens {
			b.WriteString(" WHEN " + exprText(w.When) + " THEN " + exprText(w.Then))
		}
		if n.Else != nil {
			b.WriteString(" ELSE " + exprText(n.Else))
		}
		b.WriteString(" END")
		return b.String()
	case *ExFunc:
		txts := make([]string, len(n.Args))
		for i, a := range n.Args {
			txts[i] = exprText(a)
		}
		return n.Name + "(" + strings.Join(txts, ", ") + ")"
	case *ExCast:
		return exprText(n.E) + "::" + n.Type
	case *ExIndex:
		return exprText(n.E) + "[" + exprText(n.Idx) + "]"
	case *ExArray:
		txts := make([]string, len(n.Elems))
		for i, el := range n.Elems {
			txts[i] = exprText(el)
		}
		return "ARRAY[" + strings.Join(txts, ", ") + "]"
	case *ExWindow:
		return windowText(n)
	case *ExArith:
		return "(" + exprText(n.L) + " " + n.Op + " " + exprText(n.R) + ")"
	case *ExSub:
		return "(subquery)"
	}
	return "?"
}

func joinExprs(subs []Expr, sep string) string {
	txts := make([]string, len(subs))
	for i, s := range subs {
		txts[i] = exprText(s)
	}
	return "(" + strings.Join(txts, sep) + ")"
}

// litText renders a literal as Postgres prints it in plan quals.
func litText(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case string:
		return "'" + strings.ReplaceAll(t, "'", "''") + "'"
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case []byte:
		return fmt.Sprintf(`'\x%x'`, t)
	case Param:
		return "$" + strconv.Itoa(int(t))
	}
	return fmt.Sprint(v)
}
