package sql

// expr.go: evaluation of general expressions — the Cond leaves and
// expression select items the legacy Pred/SelectItem shapes cannot
// carry. Evaluation resolves column names per row against an
// environment chain: the innermost scope first, then each enclosing
// query's, which is what makes correlated subqueries work. Names that
// never evaluate never resolve, so queries whose exotic branches run
// over zero rows (most of psql's catalog probes) succeed without
// bytdb implementing every function they mention.

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// exEnv is one evaluation environment: the query's scope and current
// combined row, plus the transaction and DB for subqueries and
// catalog-reading functions. outer chains to the enclosing query.
type exEnv struct {
	d     *DB
	tx    *bytdb.Txn
	sc    *scope
	row   []any
	grp   *group // current group in an aggregate query's group phase
	outer *exEnv
}

// exGroupRef and exAccRef are the shapes resolveAgg rewrites an
// aggregate query's expressions into: references to a GROUP BY key's
// value and to an accumulator's result, read from the environment's
// current group. They never come from the parser.
type exGroupRef struct {
	idx int
	typ bytdb.ColType
}
type exAccRef struct {
	idx int
	typ bytdb.ColType
}

func (*exGroupRef) expr() {}
func (*exAccRef) expr()   {}

// lookupVal resolves a column reference against the environment
// chain, innermost scope first. An ON condition evaluates against a
// partial combined row, so an ordinal past its end (a column of a
// table that has not joined yet) is an error, as in Postgres.
func (env *exEnv) lookupVal(c ColRef) (any, error) {
	var firstErr error
	for e := env; e != nil; e = e.outer {
		ord, err := e.sc.resolve(c)
		if err == nil {
			if ord >= len(e.row) {
				return nil, serr.New("column is not yet available here", "column", c.String())
			}
			return e.row[ord], nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

// evalEx evaluates an expression for the environment's current row.
func evalEx(env *exEnv, e Expr) (any, error) {
	switch n := e.(type) {
	case *ExLit:
		return n.Val, nil
	case *ExCol:
		return env.lookupVal(n.Col)
	case *ExAnd:
		out := triTrue
		for _, sub := range n.Exprs {
			t, err := evalTruth(env, sub)
			if err != nil {
				return nil, err
			}
			if t == triFalse {
				return false, nil
			}
			if t == triUnknown {
				out = triUnknown
			}
		}
		return triVal(out), nil
	case *ExOr:
		out := triFalse
		for _, sub := range n.Exprs {
			t, err := evalTruth(env, sub)
			if err != nil {
				return nil, err
			}
			if t == triTrue {
				return true, nil
			}
			if t == triUnknown {
				out = triUnknown
			}
		}
		return triVal(out), nil
	case *ExNot:
		t, err := evalTruth(env, n.E)
		if err != nil {
			return nil, err
		}
		return triVal(t.not()), nil
	case *ExCmp:
		l, err := evalEx(env, n.L)
		if err != nil {
			return nil, err
		}
		r, err := evalEx(env, n.R)
		if err != nil {
			return nil, err
		}
		t, err := checkPred(l, n.Op, r)
		if err != nil {
			return nil, err
		}
		return triVal(t), nil
	case *ExAny:
		return nil, serr.New("ANY is not supported (bytdb has no array values)")
	case *ExIsNull:
		v, err := evalEx(env, n.E)
		if err != nil {
			return nil, err
		}
		return (v == nil) != n.Not, nil
	case *ExIn:
		return evalIn(env, n)
	case *ExCase:
		return evalCase(env, n)
	case *ExFunc:
		// ARRAY(SELECT ...) materializes the subquery's column as a
		// Postgres array literal text ("{a,b}") — enough for psql's
		// role listings; bytdb has no first-class arrays.
		if len(n.Args) == 1 {
			if sub, ok := n.Args[0].(*ExSub); ok {
				switch n.Name {
				case "array":
					return evalArraySub(env, sub.Sel)
				case "exists":
					return evalExistsSub(env, sub.Sel)
				}
			}
		}
		args := make([]any, len(n.Args))
		for i, a := range n.Args {
			var err error
			if args[i], err = evalEx(env, a); err != nil {
				return nil, err
			}
		}
		return evalFunc(env, n.Name, args)
	case *ExCast:
		v, err := evalEx(env, n.E)
		if err != nil {
			return nil, err
		}
		return castVal(env, v, n.Type)
	case *ExIndex:
		return nil, serr.New("array subscripts are not supported")
	case *ExArith:
		// Every arithmetic/concat operator is NULL-strict, so a NULL
		// left operand decides the result without evaluating the right
		// (psql leans on this: NULL || ARRAY(SELECT ... unnest(...))).
		l, err := evalEx(env, n.L)
		if err != nil {
			return nil, err
		}
		if l == nil {
			return nil, nil
		}
		r, err := evalEx(env, n.R)
		if err != nil {
			return nil, err
		}
		return arith(n.Op, l, r)
	case *ExAgg:
		return nil, serr.New("aggregate calls are not supported inside expressions",
			"function", n.Fn.name())
	case *exGroupRef:
		if env.grp == nil {
			return nil, serr.New("group reference outside an aggregate query")
		}
		return env.grp.keyVals[n.idx], nil
	case *exAccRef:
		if env.grp == nil {
			return nil, serr.New("aggregate reference outside an aggregate query")
		}
		return env.grp.accs[n.idx].value(), nil
	case *ExSub:
		return evalSubquery(env, n.Sel)
	}
	return nil, serr.New("unhandled expression kind")
}

// evalTruth evaluates a boolean-typed expression to a truth value.
func evalTruth(env *exEnv, e Expr) (tri, error) {
	v, err := evalEx(env, e)
	if err != nil {
		return triUnknown, err
	}
	switch b := v.(type) {
	case nil:
		return triUnknown, nil
	case bool:
		if b {
			return triTrue, nil
		}
		return triFalse, nil
	default:
		return triUnknown, serr.New("condition must be boolean", "got", fmt.Sprintf("%T", b))
	}
}

// triVal renders a truth value as a SQL boolean (unknown is NULL).
func triVal(t tri) any {
	switch t {
	case triTrue:
		return true
	case triFalse:
		return false
	}
	return nil
}

func evalIn(env *exEnv, n *ExIn) (any, error) {
	v, err := evalEx(env, n.E)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	out := triFalse
	for _, le := range n.List {
		lv, err := evalEx(env, le)
		if err != nil {
			return nil, err
		}
		t, err := checkPred(v, OpEQ, lv)
		if err != nil {
			return nil, err
		}
		if t == triTrue {
			out = triTrue
			break
		}
		if t == triUnknown {
			out = triUnknown // a NULL element leaves a miss unknown
		}
	}
	if n.Not {
		out = out.not()
	}
	return triVal(out), nil
}

func evalCase(env *exEnv, n *ExCase) (any, error) {
	if n.Operand != nil {
		ov, err := evalEx(env, n.Operand)
		if err != nil {
			return nil, err
		}
		for _, w := range n.Whens {
			wv, err := evalEx(env, w.When)
			if err != nil {
				return nil, err
			}
			t, err := checkPred(ov, OpEQ, wv)
			if err != nil {
				return nil, err
			}
			if t == triTrue {
				return evalEx(env, w.Then)
			}
		}
	} else {
		for _, w := range n.Whens {
			t, err := evalTruth(env, w.When)
			if err != nil {
				return nil, err
			}
			if t == triTrue {
				return evalEx(env, w.Then)
			}
		}
	}
	if n.Else != nil {
		return evalEx(env, n.Else)
	}
	return nil, nil
}

// --- regex matching (~, !~, ~*, !~*) ---

var reCache sync.Map // pattern (with flags applied) -> *regexp.Regexp

func compileRegex(pat string, insensitive bool) (*regexp.Regexp, error) {
	key := pat
	if insensitive {
		key = "(?i)" + pat
	}
	if re, ok := reCache.Load(key); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(key)
	if err != nil {
		return nil, serr.New("invalid regular expression", "pattern", pat)
	}
	reCache.Store(key, re)
	return re, nil
}

// --- arithmetic and concatenation ---

func arith(op string, l, r any) (any, error) {
	if op == "||" {
		if l == nil || r == nil {
			return nil, nil
		}
		return valText(l) + valText(r), nil
	}
	if l == nil || r == nil {
		return nil, nil
	}
	li, lInt := l.(int64)
	ri, rInt := r.(int64)
	if lInt && rInt {
		switch op {
		case "+":
			return li + ri, nil
		case "-":
			return li - ri, nil
		case "*":
			return li * ri, nil
		case "/":
			if ri == 0 {
				return nil, serr.New("division by zero")
			}
			return li / ri, nil
		case "%":
			if ri == 0 {
				return nil, serr.New("division by zero")
			}
			return li % ri, nil
		}
	}
	lf, lOK := asFloat(l)
	rf, rOK := asFloat(r)
	if !lOK || !rOK {
		return nil, serr.New("arithmetic requires numeric operands", "op", op)
	}
	switch op {
	case "+":
		return lf + rf, nil
	case "-":
		return lf - rf, nil
	case "*":
		return lf * rf, nil
	case "/":
		if rf == 0 {
			return nil, serr.New("division by zero")
		}
		return lf / rf, nil
	case "%":
		if rf == 0 {
			return nil, serr.New("division by zero")
		}
		return math.Mod(lf, rf), nil
	}
	return nil, serr.New("unhandled arithmetic operator", "op", op)
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// valText is a value's text form, as ::text renders it.
func valText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case []byte:
		return `\x` + fmt.Sprintf("%x", x)
	}
	return fmt.Sprint(v)
}

// --- casts ---

// castVal applies E::type. Casts exist for catalog compatibility:
// numbers and digit strings move between the integer/oid families,
// anything renders as text, and the reg* object-identifier types stay
// numeric (a name string resolves through the catalog for regclass).
func castVal(env *exEnv, v any, typ string) (any, error) {
	if strings.HasSuffix(typ, "[]") {
		return nil, serr.New("array casts are not supported", "type", typ)
	}
	switch typ {
	case "oid", "int", "integer", "bigint", "smallint", "int2", "int4", "int8":
		switch x := v.(type) {
		case nil:
			return nil, nil
		case int64:
			return x, nil
		case float64:
			return int64(x), nil
		case string:
			n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
			if err != nil {
				return nil, serr.New("invalid input syntax for type "+typ, "value", x)
			}
			return n, nil
		}
	case "regclass":
		switch x := v.(type) {
		case nil:
			return nil, nil
		case int64:
			return x, nil
		case string:
			if n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil {
				return n, nil
			}
			name := strings.TrimPrefix(strings.TrimSpace(x), "public.")
			if desc := env.d.e.Table(name); desc != nil {
				return int64(desc.ID), nil
			}
			return nil, serr.New("relation does not exist", "relation", x)
		}
	case "regtype", "regnamespace", "regproc", "regprocedure", "regoper", "regrole":
		switch x := v.(type) {
		case nil:
			return nil, nil
		case int64:
			return x, nil
		case string:
			if n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil {
				return n, nil
			}
		}
	case "text", "varchar", "name", "char", "bpchar", "cstring":
		if v == nil {
			return nil, nil
		}
		return valText(v), nil
	case "bool", "boolean":
		if v == nil {
			return nil, nil
		}
		return coerceLit(v, bytdb.TBool)
	case "float4", "float8", "real", "numeric", "decimal":
		switch x := v.(type) {
		case nil:
			return nil, nil
		case int64:
			return float64(x), nil
		case float64:
			return x, nil
		case string:
			return coerceLit(x, bytdb.TFloat)
		}
	}
	return nil, serr.New("unsupported cast", "type", typ)
}

// --- functions with arguments ---

// evalFunc applies a whitelisted catalog/utility function. Unknown
// names parse fine and error only here — if a query's rows never
// reach them (empty catalog tables), the query succeeds.
func evalFunc(env *exEnv, name string, args []any) (any, error) {
	argN := func(i int) any {
		if i < len(args) {
			return args[i]
		}
		return nil
	}
	switch name {
	case "coalesce":
		for _, a := range args {
			if a != nil {
				return a, nil
			}
		}
		return nil, nil
	case "nullif":
		t, err := checkPred(argN(0), OpEQ, argN(1))
		if err != nil {
			return nil, err
		}
		if t == triTrue {
			return nil, nil
		}
		return argN(0), nil
	case "lower":
		if s, ok := argN(0).(string); ok {
			return strings.ToLower(s), nil
		}
		return nil, nil
	case "upper":
		if s, ok := argN(0).(string); ok {
			return strings.ToUpper(s), nil
		}
		return nil, nil
	case "length", "char_length", "character_length":
		if s, ok := argN(0).(string); ok {
			return int64(len([]rune(s))), nil
		}
		return nil, nil
	case "format_type":
		return formatType(argN(0), argN(1)), nil
	case "pg_get_userbyid":
		return sysDatabase, nil
	case "pg_table_is_visible", "pg_function_is_visible", "pg_type_is_visible":
		return true, nil
	case "pg_relation_is_publishable":
		return true, nil
	case "pg_get_indexdef":
		oid, ok := argN(0).(int64)
		if !ok {
			return nil, nil
		}
		colNo, _ := argN(1).(int64)
		return indexdef(env.d, oid, colNo), nil
	case "array_to_string", "array_length": // no arrays exist: NULL in, NULL out
		return nil, nil
	case "pg_get_constraintdef":
		oid, ok := argN(0).(int64)
		if !ok {
			return nil, nil
		}
		return constraintdef(env.d, oid), nil
	case "pg_get_expr", "pg_get_partkeydef",
		"pg_get_viewdef", "pg_get_triggerdef", "pg_get_ruledef",
		"pg_get_statisticsobjdef_columns",
		"obj_description", "col_description", "shobj_description":
		return nil, nil
	case "pg_table_size", "pg_total_relation_size", "pg_relation_size",
		"pg_indexes_size":
		return int64(0), nil
	case "pg_size_pretty":
		return "0 bytes", nil
	case "pg_encoding_to_char":
		return "UTF8", nil
	}
	return nil, serr.New("unknown function", "function", name)
}

// formatType is pg_catalog.format_type(typid, typmod).
func formatType(typid, typmod any) any {
	oid, ok := typid.(int64)
	if !ok {
		return nil
	}
	name := ""
	switch oid {
	case 16:
		name = "boolean"
	case 17:
		name = "bytea"
	case 19:
		name = "name"
	case 20:
		name = "bigint"
	case 21:
		name = "smallint"
	case 23:
		name = "integer"
	case 25:
		name = "text"
	case 26:
		name = "oid"
	case 700:
		name = "real"
	case 701:
		name = "double precision"
	case 1043:
		name = "character varying"
		if m, ok := typmod.(int64); ok && m >= 4 {
			return fmt.Sprintf("character varying(%d)", m-4)
		}
	default:
		return "???"
	}
	return name
}

// indexdef renders pg_get_indexdef for one of the catalog's index
// oids; colNo > 0 returns just that key column's name. psql keeps the
// text after " USING " for its Indexes: listing.
func indexdef(d *DB, oid int64, colNo int64) any {
	for _, desc := range d.userDescs() {
		var name string
		var cols []int
		var descs []bool
		unique := false
		found := false
		if oid == indexOID(desc.ID, 0) {
			name, cols, unique, found = desc.Name+"_pkey", desc.PKCols, true, true
		} else {
			for _, ix := range desc.Indexes {
				if oid == indexOID(desc.ID, ix.ID) {
					name, cols, unique, found = ix.Name, ix.Cols, ix.Unique, true
					descs = ix.Desc
					break
				}
			}
		}
		if !found {
			continue
		}
		names := make([]string, len(cols))
		keys := make([]string, len(cols))
		for i, o := range cols {
			names[i] = desc.Columns[o].Name
			keys[i] = names[i]
			if i < len(descs) && descs[i] {
				keys[i] += " DESC"
			}
		}
		if colNo > 0 { // the per-column form returns the bare column name
			if int(colNo) <= len(names) {
				return names[colNo-1]
			}
			return ""
		}
		u := ""
		if unique {
			u = "UNIQUE "
		}
		return fmt.Sprintf("CREATE %sINDEX %s ON public.%s USING btree (%s)",
			u, name, desc.Name, strings.Join(keys, ", "))
	}
	return nil
}

// constraintdef renders pg_get_constraintdef for a check constraint's
// oid, in Postgres's shape: CHECK ((expr)).
func constraintdef(d *DB, oid int64) any {
	for _, desc := range d.userDescs() {
		for i, ck := range desc.Checks {
			if oid == checkOID(desc.ID, i) {
				return "CHECK ((" + ck.Expr + "))"
			}
		}
	}
	return nil
}

// --- scalar subqueries ---

// evalSubquery runs a scalar subquery for the current row. The
// subquery's FROM binds normally; any predicate that does not resolve
// in its own scope re-lowers to a Cond, whose eval-time resolution
// reaches the enclosing rows through the environment chain — that is
// the whole correlation mechanism. The result is the single item's
// value from the single row; zero rows read as NULL.
func evalSubquery(env *exEnv, sel *Select) (any, error) {
	if len(sel.Union) > 0 {
		return nil, serr.New("UNION in a scalar subquery is not supported")
	}
	if sel.Star || len(sel.Items) != 1 {
		return nil, serr.New("a scalar subquery must select exactly one column")
	}
	it := sel.Items[0]
	if sel.GroupBy != nil || sel.Having != nil {
		return nil, serr.New("GROUP BY in a scalar subquery is not supported")
	}
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
	where := decorrelate(sel.Where, sc)
	fp, err := prepareFrom(lk, from, where)
	if err != nil {
		return nil, err
	}

	// A single aggregate item — (SELECT count(*) FROM ... WHERE
	// correlated) — folds the matching rows into one accumulator.
	if it.Agg != AggNone {
		acc := accum{fn: it.Agg, ord: -1}
		if !it.Star {
			ord, err := fp.sc.resolve(it.Col)
			if err != nil {
				return nil, err
			}
			t := fp.sc.column(ord).Type
			if (it.Agg == AggSum || it.Agg == AggAvg) && t != bytdb.TInt && t != bytdb.TFloat {
				return nil, serr.New(it.Agg.name()+" requires a numeric column",
					"column", it.Col.String())
			}
			acc.ord, acc.intSum = ord, t == bytdb.TInt
		} else if it.Agg != AggCount {
			return nil, serr.New("only COUNT takes *", "function", it.Agg.name())
		}
		sub := &exEnv{d: env.d, tx: env.tx, sc: fp.sc, outer: env}
		var addErr error
		err = runJoin(env.tx, fp, sub, func(vals []any) bool {
			sub.row = vals
			addErr = acc.add(sub, vals)
			return addErr == nil
		})
		if err != nil {
			return nil, err
		}
		if addErr != nil {
			return nil, addErr
		}
		return acc.value(), nil
	}

	itemEx := itemToExpr(it)
	sub := &exEnv{d: env.d, tx: env.tx, sc: fp.sc, outer: env}

	var out any
	skip, take := sel.Offset, sel.Limit
	count := int64(0)
	var evalErr error
	err = runJoin(env.tx, fp, sub, func(vals []any) bool {
		if skip > 0 {
			skip--
			return true
		}
		if take >= 0 && count >= take {
			return false
		}
		count++
		if count > 1 {
			return false
		}
		rowEnv := *sub
		rowEnv.row = vals
		out, evalErr = evalEx(&rowEnv, itemEx)
		return evalErr == nil
	})
	if err != nil {
		return nil, err
	}
	if evalErr != nil {
		return nil, evalErr
	}
	if count > 1 {
		return nil, serr.New("more than one row returned by a scalar subquery")
	}
	return out, nil
}

// evalArraySub runs ARRAY(SELECT ...): every row's single value,
// rendered as a Postgres array literal ("{}" when empty). The
// subquery's single output column sorts ascending when it has any
// ORDER BY (the one psql writes is ORDER BY 1).
func evalArraySub(env *exEnv, sel *Select) (any, error) {
	if sel.Star || len(sel.Items) != 1 {
		return nil, serr.New("ARRAY(SELECT ...) must select exactly one column")
	}
	it := sel.Items[0]
	if it.Agg != AggNone || sel.GroupBy != nil || sel.Having != nil || len(sel.Union) > 0 {
		return nil, serr.New("this ARRAY(SELECT ...) shape is not supported")
	}
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
	fp, err := prepareFrom(lk, from, decorrelate(sel.Where, sc))
	if err != nil {
		return nil, err
	}
	itemEx := itemToExpr(it)
	sub := &exEnv{d: env.d, tx: env.tx, sc: fp.sc, outer: env}
	var vals []any
	var evalErr error
	err = runJoin(env.tx, fp, sub, func(rowVals []any) bool {
		rowEnv := *sub
		rowEnv.row = rowVals
		var v any
		if v, evalErr = evalEx(&rowEnv, itemEx); evalErr != nil {
			return false
		}
		vals = append(vals, v)
		return true
	})
	if err != nil {
		return nil, err
	}
	if evalErr != nil {
		return nil, evalErr
	}
	if len(sel.OrderBy) > 0 {
		slices.SortStableFunc(vals, orderCmp)
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = valText(v)
	}
	return "{" + strings.Join(parts, ",") + "}", nil
}

// evalExistsSub runs EXISTS (SELECT ...): whether the (possibly
// correlated) subquery yields any row; its select list is irrelevant.
func evalExistsSub(env *exEnv, sel *Select) (any, error) {
	if len(sel.Union) > 0 || sel.GroupBy != nil || sel.Having != nil {
		return nil, serr.New("this EXISTS (SELECT ...) shape is not supported")
	}
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
	fp, err := prepareFrom(lk, from, decorrelate(sel.Where, sc))
	if err != nil {
		return nil, err
	}
	sub := &exEnv{d: env.d, tx: env.tx, sc: fp.sc, outer: env}
	found := false
	err = runJoin(env.tx, fp, sub, func([]any) bool {
		found = true
		return false
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// decorrelate rewrites the predicates of a subquery condition that do
// not resolve in the subquery's own scope into Cond leaves.
func decorrelate(e BoolExpr, sc *scope) BoolExpr {
	switch n := e.(type) {
	case nil:
		return nil
	case *Pred:
		ok := n.Item.Agg == AggNone
		if ok {
			_, err := sc.resolve(n.Item.Col)
			ok = err == nil
		}
		if ok && n.RItem != nil {
			_, err := sc.resolve(n.RItem.Col)
			ok = err == nil
		}
		if ok {
			return n
		}
		return &Cond{Ex: predExpr(n)}
	case *Not:
		return &Not{Expr: decorrelate(n.Expr, sc)}
	case *And:
		out := &And{Exprs: make([]BoolExpr, len(n.Exprs))}
		for i, sub := range n.Exprs {
			out.Exprs[i] = decorrelate(sub, sc)
		}
		return out
	case *Or:
		out := &Or{Exprs: make([]BoolExpr, len(n.Exprs))}
		for i, sub := range n.Exprs {
			out.Exprs[i] = decorrelate(sub, sc)
		}
		return out
	}
	return e // Cond resolves at eval time already
}

// predExpr renders a legacy predicate back as an expression.
func predExpr(pr *Pred) Expr {
	l := Expr(&ExCol{Col: pr.Item.Col})
	switch pr.Op {
	case OpIsNull:
		return &ExIsNull{E: l}
	case OpIsNotNull:
		return &ExIsNull{E: l, Not: true}
	}
	var r Expr
	if pr.RItem != nil {
		r = &ExCol{Col: pr.RItem.Col}
	} else {
		r = &ExLit{Val: pr.Val}
	}
	return &ExCmp{Op: pr.Op, L: l, R: r}
}

// itemToExpr renders a select item back as an expression (aggregates
// are rejected before this point).
func itemToExpr(it SelectItem) Expr {
	switch {
	case it.Ex != nil:
		return it.Ex
	case it.IsLit:
		return &ExLit{Val: it.Lit, Name: it.LitName}
	}
	return &ExCol{Col: it.Col}
}

// --- static shape (Describe) ---

// exprType is the column type an expression reports, best-effort;
// shapes with no static type read as text.
func exprType(sc *scope, e Expr) bytdb.ColType {
	switch n := e.(type) {
	case *ExLit:
		return litType(n.Val)
	case *ExCol:
		if ord, err := sc.resolve(n.Col); err == nil {
			return sc.column(ord).Type
		}
	case *ExCmp, *ExAny, *ExIsNull, *ExIn, *ExAnd, *ExOr, *ExNot:
		return bytdb.TBool
	case *ExAgg:
		switch n.Fn {
		case AggCount:
			return bytdb.TInt
		case AggAvg:
			return bytdb.TFloat
		}
		if !n.Star {
			if ord, err := sc.resolve(n.Col); err == nil {
				return sc.column(ord).Type
			}
		}
	case *ExFunc:
		if t, ok := funcTypes[n.Name]; ok {
			return t
		}
	case *ExCase:
		if len(n.Whens) > 0 {
			return exprType(sc, n.Whens[0].Then)
		}
	case *ExCast:
		return castColType(n.Type)
	case *exGroupRef:
		return n.typ
	case *exAccRef:
		return n.typ
	case *ExArith:
		if n.Op == "||" {
			return bytdb.TString
		}
		if exprType(sc, n.L) == bytdb.TFloat || exprType(sc, n.R) == bytdb.TFloat {
			return bytdb.TFloat
		}
		return bytdb.TInt
	}
	return bytdb.TString
}

var funcTypes = map[string]bytdb.ColType{
	"pg_table_is_visible":        bytdb.TBool,
	"pg_function_is_visible":     bytdb.TBool,
	"pg_type_is_visible":         bytdb.TBool,
	"pg_relation_is_publishable": bytdb.TBool,
	"pg_table_size":              bytdb.TInt,
	"pg_total_relation_size":     bytdb.TInt,
	"pg_relation_size":           bytdb.TInt,
	"pg_indexes_size":            bytdb.TInt,
	"length":                     bytdb.TInt,
	"char_length":                bytdb.TInt,
	"character_length":           bytdb.TInt,
}

func castColType(typ string) bytdb.ColType {
	switch typ {
	case "oid", "int", "integer", "bigint", "smallint", "int2", "int4", "int8",
		"regclass", "regtype", "regnamespace", "regproc", "regprocedure",
		"regoper", "regrole":
		return bytdb.TInt
	case "bool", "boolean":
		return bytdb.TBool
	case "float4", "float8", "real", "numeric", "decimal":
		return bytdb.TFloat
	}
	return bytdb.TString
}

// exprName is the output column name an expression carries when no
// alias is given.
func exprName(e Expr) string {
	switch n := e.(type) {
	case *ExFunc:
		return n.Name
	case *ExAgg:
		return n.Fn.name() // sum(a*b) names its column "sum", as Postgres does
	case *ExCase:
		return "case"
	case *ExCast:
		return exprName(n.E)
	case *ExCol:
		return n.Col.Name
	case *ExLit:
		if n.Name != "" {
			return n.Name
		}
	}
	return "?column?"
}
