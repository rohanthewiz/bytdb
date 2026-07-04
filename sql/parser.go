package sql

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// Parse parses one SQL statement; a trailing semicolon is allowed.
func Parse(src string) (Statement, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	st, err := p.statement()
	if err != nil {
		return nil, err
	}
	p.acceptOp(";")
	if p.cur().kind != tEOF {
		return nil, p.unexpected("end of statement")
	}
	return st, nil
}

type parser struct {
	toks []token
	i    int
}

func (p *parser) cur() token { return p.toks[p.i] }

func (p *parser) advance() {
	if p.cur().kind != tEOF {
		p.i++
	}
}

// acceptKw consumes kw (an unquoted, lowercased keyword) if it is next.
func (p *parser) acceptKw(kw string) bool {
	if t := p.cur(); t.kind == tIdent && t.text == kw {
		p.advance()
		return true
	}
	return false
}

func (p *parser) expectKw(kw string) error {
	if !p.acceptKw(kw) {
		return p.unexpected(strings.ToUpper(kw))
	}
	return nil
}

func (p *parser) acceptOp(op string) bool {
	if t := p.cur(); t.kind == tOp && t.text == op {
		p.advance()
		return true
	}
	return false
}

func (p *parser) expectOp(op string) error {
	if !p.acceptOp(op) {
		return p.unexpected("'" + op + "'")
	}
	return nil
}

func (p *parser) unexpected(want string) error {
	t := p.cur()
	got := "'" + t.text + "'"
	if t.kind == tEOF {
		got = "end of input"
	}
	return serr.New("syntax error", "want", want, "got", got, "pos", fmt.Sprint(t.pos))
}

// ident consumes an identifier, quoted or not.
func (p *parser) ident(what string) (string, error) {
	t := p.cur()
	switch t.kind {
	case tIdent:
		p.advance()
		return t.text, nil
	case tQIdent:
		if t.text == "" {
			return "", serr.New("empty quoted identifier", "pos", fmt.Sprint(t.pos))
		}
		p.advance()
		return t.text, nil
	}
	return "", p.unexpected(what)
}

func (p *parser) statement() (Statement, error) {
	switch {
	case p.acceptKw("create"):
		if p.acceptKw("table") {
			return p.createTable()
		}
		unique := p.acceptKw("unique")
		if p.acceptKw("index") {
			return p.createIndex(unique)
		}
		return nil, p.unexpected("TABLE or [UNIQUE] INDEX")
	case p.acceptKw("drop"):
		switch {
		case p.acceptKw("table"):
			name, err := p.ident("a table name")
			if err != nil {
				return nil, err
			}
			return &DropTable{Table: name}, nil
		case p.acceptKw("index"):
			return p.dropIndex()
		}
		return nil, p.unexpected("TABLE or INDEX")
	case p.acceptKw("alter"):
		return p.alterTable()
	case p.acceptKw("insert"):
		return p.insert()
	case p.acceptKw("select"):
		return p.selectStmt()
	case p.acceptKw("update"):
		return p.update()
	case p.acceptKw("delete"):
		return p.deleteStmt()
	}
	return nil, p.unexpected("a SQL statement")
}

// --- DDL ---

func (p *parser) createTable() (Statement, error) {
	name, err := p.ident("a table name")
	if err != nil {
		return nil, err
	}
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	ct := &CreateTable{Table: name}
	for {
		if p.acceptKw("primary") {
			if err := p.expectKw("key"); err != nil {
				return nil, err
			}
			if ct.PK != nil {
				return nil, serr.New("multiple primary keys", "table", name)
			}
			if ct.PK, err = p.identList("a primary key column"); err != nil {
				return nil, err
			}
		} else {
			col, inlinePK, err := p.colDef()
			if err != nil {
				return nil, err
			}
			if inlinePK {
				if ct.PK != nil {
					return nil, serr.New("multiple primary keys", "table", name)
				}
				ct.PK = []string{col.Name}
			}
			ct.Cols = append(ct.Cols, col)
		}
		if !p.acceptOp(",") {
			break
		}
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return ct, nil
}

// colDef parses "name type [PRIMARY KEY]".
func (p *parser) colDef() (ColDef, bool, error) {
	name, err := p.ident("a column name")
	if err != nil {
		return ColDef{}, false, err
	}
	typ, err := p.typeName()
	if err != nil {
		return ColDef{}, false, err
	}
	pk := false
	if p.acceptKw("primary") {
		if err := p.expectKw("key"); err != nil {
			return ColDef{}, false, err
		}
		pk = true
	}
	return ColDef{Name: name, Type: typ}, pk, nil
}

// typeName parses a column type, accepting common Postgres aliases.
func (p *parser) typeName() (bytdb.ColType, error) {
	t := p.cur()
	if t.kind != tIdent {
		return "", p.unexpected("a column type")
	}
	p.advance()
	switch t.text {
	case "int", "integer", "bigint", "int8", "int4", "int2", "smallint":
		return bytdb.TInt, nil
	case "float", "float4", "float8", "real":
		return bytdb.TFloat, nil
	case "double":
		p.acceptKw("precision")
		return bytdb.TFloat, nil
	case "text", "string":
		return bytdb.TString, nil
	case "varchar":
		if p.acceptOp("(") {
			if p.cur().kind != tNumber {
				return "", p.unexpected("a length")
			}
			p.advance()
			if err := p.expectOp(")"); err != nil {
				return "", err
			}
		}
		return bytdb.TString, nil
	case "bool", "boolean":
		return bytdb.TBool, nil
	case "bytea", "bytes":
		return bytdb.TBytes, nil
	}
	return "", serr.New("unknown column type", "type", t.text, "pos", fmt.Sprint(t.pos))
}

// identList parses "( ident [, ident]* )".
func (p *parser) identList(what string) ([]string, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var out []string
	for {
		name, err := p.ident(what)
		if err != nil {
			return nil, err
		}
		out = append(out, name)
		if !p.acceptOp(",") {
			break
		}
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *parser) createIndex(unique bool) (Statement, error) {
	name, err := p.ident("an index name")
	if err != nil {
		return nil, err
	}
	if err := p.expectKw("on"); err != nil {
		return nil, err
	}
	table, err := p.ident("a table name")
	if err != nil {
		return nil, err
	}
	cols, err := p.identList("an indexed column")
	if err != nil {
		return nil, err
	}
	return &CreateIndex{Name: name, Table: table, Unique: unique, Cols: cols}, nil
}

func (p *parser) dropIndex() (Statement, error) {
	name, err := p.ident("an index name")
	if err != nil {
		return nil, err
	}
	di := &DropIndex{Name: name}
	if p.acceptKw("on") {
		if di.Table, err = p.ident("a table name"); err != nil {
			return nil, err
		}
	}
	return di, nil
}

func (p *parser) alterTable() (Statement, error) {
	if err := p.expectKw("table"); err != nil {
		return nil, err
	}
	table, err := p.ident("a table name")
	if err != nil {
		return nil, err
	}
	switch {
	case p.acceptKw("add"):
		p.acceptKw("column")
		col, pk, err := p.colDef()
		if err != nil {
			return nil, err
		}
		if pk {
			return nil, serr.New("cannot add a primary key column", "table", table, "column", col.Name)
		}
		return &AddColumn{Table: table, Col: col}, nil
	case p.acceptKw("drop"):
		p.acceptKw("column")
		name, err := p.ident("a column name")
		if err != nil {
			return nil, err
		}
		return &DropColumn{Table: table, Col: name}, nil
	}
	return nil, p.unexpected("ADD or DROP COLUMN")
}

// --- DML ---

func (p *parser) insert() (Statement, error) {
	if err := p.expectKw("into"); err != nil {
		return nil, err
	}
	table, err := p.ident("a table name")
	if err != nil {
		return nil, err
	}
	ins := &Insert{Table: table}
	if p.cur().kind == tOp && p.cur().text == "(" {
		if ins.Cols, err = p.identList("a column name"); err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, c := range ins.Cols {
			if seen[c] {
				return nil, serr.New("duplicate column in INSERT", "column", c)
			}
			seen[c] = true
		}
	}
	if err := p.expectKw("values"); err != nil {
		return nil, err
	}
	for {
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		var row []any
		for {
			v, err := p.literal()
			if err != nil {
				return nil, err
			}
			row = append(row, v)
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		ins.Rows = append(ins.Rows, row)
		if !p.acceptOp(",") {
			break
		}
	}
	return ins, nil
}

func (p *parser) selectStmt() (Statement, error) {
	s := &Select{Limit: -1}
	if p.acceptOp("*") {
		s.Star = true
	} else {
		for {
			item, err := p.selectListItem()
			if err != nil {
				return nil, err
			}
			s.Items = append(s.Items, item)
			if !p.acceptOp(",") {
				break
			}
		}
	}
	if err := p.expectKw("from"); err != nil {
		return nil, err
	}
	var err error
	if s.From, err = p.fromClause(); err != nil {
		return nil, err
	}
	if p.acceptKw("where") {
		if s.Where, err = p.boolExpr(false); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("group") {
		if err := p.expectKw("by"); err != nil {
			return nil, err
		}
		for {
			col, err := p.colRef()
			if err != nil {
				return nil, err
			}
			s.GroupBy = append(s.GroupBy, col)
			if !p.acceptOp(",") {
				break
			}
		}
	}
	if p.acceptKw("having") {
		if s.Having, err = p.boolExpr(true); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("order") {
		if err := p.expectKw("by"); err != nil {
			return nil, err
		}
		for {
			si, err := p.selectItem()
			if err != nil {
				return nil, err
			}
			item := OrderItem{SelectItem: si}
			if p.acceptKw("desc") {
				item.Desc = true
			} else {
				p.acceptKw("asc")
			}
			s.OrderBy = append(s.OrderBy, item)
			if !p.acceptOp(",") {
				break
			}
		}
	}
	for {
		switch {
		case p.acceptKw("limit"):
			if s.Limit, err = p.nonNegInt("LIMIT"); err != nil {
				return nil, err
			}
		case p.acceptKw("offset"):
			if s.Offset, err = p.nonNegInt("OFFSET"); err != nil {
				return nil, err
			}
		default:
			return s, nil
		}
	}
}

func (p *parser) nonNegInt(clause string) (int64, error) {
	t := p.cur()
	if t.kind != tNumber {
		return 0, p.unexpected("a count after " + clause)
	}
	n, err := strconv.ParseInt(t.text, 10, 64)
	if err != nil || n < 0 {
		return 0, serr.New(clause+" requires a non-negative integer", "got", t.text)
	}
	p.advance()
	return n, nil
}

func (p *parser) update() (Statement, error) {
	table, err := p.ident("a table name")
	if err != nil {
		return nil, err
	}
	if err := p.expectKw("set"); err != nil {
		return nil, err
	}
	u := &Update{Table: table, Set: map[string]any{}}
	for {
		col, err := p.ident("a column name")
		if err != nil {
			return nil, err
		}
		if err := p.expectOp("="); err != nil {
			return nil, err
		}
		val, err := p.literal()
		if err != nil {
			return nil, err
		}
		if _, dup := u.Set[col]; dup {
			return nil, serr.New("duplicate column in SET", "column", col)
		}
		u.Set[col] = val
		if !p.acceptOp(",") {
			break
		}
	}
	if p.acceptKw("where") {
		if u.Where, err = p.boolExpr(false); err != nil {
			return nil, err
		}
	}
	return u, nil
}

func (p *parser) deleteStmt() (Statement, error) {
	if err := p.expectKw("from"); err != nil {
		return nil, err
	}
	table, err := p.ident("a table name")
	if err != nil {
		return nil, err
	}
	d := &Delete{Table: table}
	if p.acceptKw("where") {
		if d.Where, err = p.boolExpr(false); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// fromClause parses FROM t [alias] followed by any number of joins.
// A comma-separated table reads as CROSS JOIN; RIGHT and FULL joins
// are rejected.
func (p *parser) fromClause() ([]FromItem, error) {
	first, err := p.tableRef()
	if err != nil {
		return nil, err
	}
	items := []FromItem{first}
	for {
		var it FromItem
		switch {
		case p.acceptOp(","):
			if it, err = p.tableRef(); err != nil {
				return nil, err
			}
			it.Join = JoinCross
		case p.acceptKw("cross"):
			if err := p.expectKw("join"); err != nil {
				return nil, err
			}
			if it, err = p.tableRef(); err != nil {
				return nil, err
			}
			it.Join = JoinCross
		case p.acceptKw("inner"):
			if err := p.expectKw("join"); err != nil {
				return nil, err
			}
			if it, err = p.joinOn(JoinInner); err != nil {
				return nil, err
			}
		case p.acceptKw("left"):
			p.acceptKw("outer")
			if err := p.expectKw("join"); err != nil {
				return nil, err
			}
			if it, err = p.joinOn(JoinLeft); err != nil {
				return nil, err
			}
		case p.acceptKw("join"):
			if it, err = p.joinOn(JoinInner); err != nil {
				return nil, err
			}
		case p.cur().kind == tIdent && (p.cur().text == "right" || p.cur().text == "full"):
			return nil, serr.New("only INNER, LEFT [OUTER], and CROSS joins are supported",
				"join", strings.ToUpper(p.cur().text))
		default:
			return items, nil
		}
		items = append(items, it)
	}
}

// joinOn parses "t [alias] ON expr" after a JOIN keyword.
func (p *parser) joinOn(jt JoinType) (FromItem, error) {
	it, err := p.tableRef()
	if err != nil {
		return FromItem{}, err
	}
	it.Join = jt
	if err := p.expectKw("on"); err != nil {
		return FromItem{}, err
	}
	if it.On, err = p.boolExpr(false); err != nil {
		return FromItem{}, err
	}
	return it, nil
}

// noAlias is the keywords that cannot follow a table name as a bare
// alias; an alias spelled like one needs AS or double quotes.
var noAlias = map[string]bool{
	"where": true, "group": true, "having": true, "order": true,
	"limit": true, "offset": true, "on": true, "join": true,
	"inner": true, "left": true, "right": true, "full": true,
	"cross": true, "union": true, "as": true, "and": true, "or": true,
	"not": true, "using": true, "natural": true, "set": true,
	"values": true, "returning": true,
}

// tableRef parses "t", "t alias", or "t AS alias".
func (p *parser) tableRef() (FromItem, error) {
	name, err := p.ident("a table name")
	if err != nil {
		return FromItem{}, err
	}
	it := FromItem{Table: name}
	if p.acceptKw("as") {
		if it.Alias, err = p.ident("an alias"); err != nil {
			return FromItem{}, err
		}
	} else if t := p.cur(); t.kind == tQIdent || (t.kind == tIdent && !noAlias[t.text]) {
		it.Alias, _ = p.ident("an alias")
	}
	return it, nil
}

// colRef parses a column reference: col or table.col.
func (p *parser) colRef() (ColRef, error) {
	first, err := p.ident("a column name")
	if err != nil {
		return ColRef{}, err
	}
	if !p.acceptOp(".") {
		return ColRef{Name: first}, nil
	}
	name, err := p.ident("a column name")
	if err != nil {
		return ColRef{}, err
	}
	return ColRef{Table: first, Name: name}, nil
}

// selectListItem parses one select-list entry: selectItem, plus t.*.
func (p *parser) selectListItem() (SelectItem, error) {
	if t := p.cur(); (t.kind == tIdent || t.kind == tQIdent) && p.peekOpAt(1, ".") && p.peekOpAt(2, "*") {
		qual, err := p.ident("a table name")
		if err != nil {
			return SelectItem{}, err
		}
		p.advance() // .
		p.advance() // *
		return SelectItem{Col: ColRef{Table: qual}, Star: true}, nil
	}
	return p.selectItem()
}

// selectItem parses one select-list or ORDER BY entry: a column, or
// an aggregate call fn(col) / COUNT(*). An identifier that happens to
// share an aggregate's name stays a column unless a '(' follows.
func (p *parser) selectItem() (SelectItem, error) {
	t := p.cur()
	if t.kind == tIdent && aggNames[t.text] != AggNone && p.peekOp("(") {
		fn := aggNames[t.text]
		p.advance() // function name
		p.advance() // (
		item := SelectItem{Agg: fn}
		if fn == AggCount && p.acceptOp("*") {
			item.Star = true
		} else {
			col, err := p.colRef()
			if err != nil {
				return SelectItem{}, err
			}
			item.Col = col
		}
		if err := p.expectOp(")"); err != nil {
			return SelectItem{}, err
		}
		return item, nil
	}
	col, err := p.colRef()
	return SelectItem{Col: col}, err
}

// peekOp reports whether the token after the current one is op.
func (p *parser) peekOp(op string) bool { return p.peekOpAt(1, op) }

// peekOpAt reports whether the token n places ahead is op.
func (p *parser) peekOpAt(n int, op string) bool {
	if p.i+n >= len(p.toks) {
		return false
	}
	t := p.toks[p.i+n]
	return t.kind == tOp && t.text == op
}

// --- boolean expressions (WHERE / HAVING) ---

// boolExpr parses a boolean expression with SQL precedence — OR
// binds loosest, then AND, then NOT, then predicates; parentheses
// group. allowAgg permits aggregate calls in predicates (HAVING).
func (p *parser) boolExpr(allowAgg bool) (BoolExpr, error) {
	e, err := p.boolAnd(allowAgg)
	if err != nil {
		return nil, err
	}
	for p.acceptKw("or") {
		r, err := p.boolAnd(allowAgg)
		if err != nil {
			return nil, err
		}
		if or, ok := e.(*Or); ok {
			or.Exprs = append(or.Exprs, r)
		} else {
			e = &Or{Exprs: []BoolExpr{e, r}}
		}
	}
	return e, nil
}

func (p *parser) boolAnd(allowAgg bool) (BoolExpr, error) {
	e, err := p.boolNot(allowAgg)
	if err != nil {
		return nil, err
	}
	for p.acceptKw("and") {
		r, err := p.boolNot(allowAgg)
		if err != nil {
			return nil, err
		}
		if and, ok := e.(*And); ok {
			and.Exprs = append(and.Exprs, r)
		} else {
			e = &And{Exprs: []BoolExpr{e, r}}
		}
	}
	return e, nil
}

func (p *parser) boolNot(allowAgg bool) (BoolExpr, error) {
	if p.acceptKw("not") {
		e, err := p.boolNot(allowAgg)
		if err != nil {
			return nil, err
		}
		return &Not{Expr: e}, nil
	}
	if p.acceptOp("(") {
		e, err := p.boolExpr(allowAgg)
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return e, nil
	}
	return p.predLeaf(allowAgg)
}

// predLeaf parses one predicate: item op literal, item op item, or
// item IS [NOT] NULL. A literal-first comparison is flipped so the
// item is always on the left.
func (p *parser) predLeaf(allowAgg bool) (BoolExpr, error) {
	lItem, lVal, lIsItem, err := p.predOperand(allowAgg)
	if err != nil {
		return nil, err
	}
	if lIsItem && p.acceptKw("is") {
		op := OpIsNull
		if p.acceptKw("not") {
			op = OpIsNotNull
		}
		if err := p.expectKw("null"); err != nil {
			return nil, err
		}
		return &Pred{Item: lItem, Op: op}, nil
	}
	op, err := p.cmpOp()
	if err != nil {
		return nil, err
	}
	rItem, rVal, rIsItem, err := p.predOperand(allowAgg)
	if err != nil {
		return nil, err
	}
	switch {
	case lIsItem && rIsItem:
		return &Pred{Item: lItem, Op: op, RItem: &rItem}, nil
	case lIsItem:
		if rVal == nil {
			return nil, serr.New("cannot compare with NULL; use IS [NOT] NULL")
		}
		return &Pred{Item: lItem, Op: op, Val: rVal}, nil
	case rIsItem:
		if lVal == nil {
			return nil, serr.New("cannot compare with NULL; use IS [NOT] NULL")
		}
		return &Pred{Item: rItem, Op: flip(op), Val: lVal}, nil
	}
	return nil, serr.New("a predicate cannot compare two literals")
}

// predOperand parses a column, an aggregate call (HAVING only), or a
// literal.
func (p *parser) predOperand(allowAgg bool) (item SelectItem, val any, isItem bool, err error) {
	t := p.cur()
	if t.kind == tIdent && (t.text == "true" || t.text == "false" || t.text == "null") {
		val, err = p.literal()
		return SelectItem{}, val, false, err
	}
	if t.kind == tIdent && aggNames[t.text] != AggNone && p.peekOp("(") {
		if !allowAgg {
			return SelectItem{}, nil, false, serr.New("aggregates are not allowed in WHERE; use HAVING", "function", t.text)
		}
		item, err = p.selectItem()
		return item, nil, true, err
	}
	if t.kind == tIdent || t.kind == tQIdent {
		col, err := p.colRef()
		return SelectItem{Col: col}, nil, true, err
	}
	val, err = p.literal()
	return SelectItem{}, val, false, err
}

func (p *parser) cmpOp() (PredOp, error) {
	t := p.cur()
	if t.kind == tOp {
		var op PredOp
		switch t.text {
		case "=":
			op = OpEQ
		case "!=", "<>":
			op = OpNE
		case "<":
			op = OpLT
		case "<=":
			op = OpLE
		case ">":
			op = OpGT
		case ">=":
			op = OpGE
		default:
			return 0, p.unexpected("a comparison operator")
		}
		p.advance()
		return op, nil
	}
	return 0, p.unexpected("a comparison operator")
}

// flip mirrors a comparison across its operands (5 < id  ==>  id > 5).
func flip(op PredOp) PredOp {
	switch op {
	case OpLT:
		return OpGT
	case OpLE:
		return OpGE
	case OpGT:
		return OpLT
	case OpGE:
		return OpLE
	}
	return op // EQ, NE are symmetric
}

// literal parses one literal value: [+|-] number, 'string', TRUE,
// FALSE, or NULL. Numbers become int64 when they fit, else float64.
func (p *parser) literal() (any, error) {
	neg := false
	if p.acceptOp("-") {
		neg = true
	} else {
		p.acceptOp("+")
	}
	t := p.cur()
	switch t.kind {
	case tNumber:
		p.advance()
		if n, err := strconv.ParseInt(t.text, 10, 64); err == nil {
			if neg {
				n = -n
			}
			return n, nil
		}
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, serr.New("bad numeric literal", "text", t.text, "pos", fmt.Sprint(t.pos))
		}
		if neg {
			f = -f
		}
		return f, nil
	case tString:
		if neg {
			return nil, p.unexpected("a number after '-'")
		}
		p.advance()
		return t.text, nil
	case tIdent:
		if !neg {
			switch t.text {
			case "true":
				p.advance()
				return true, nil
			case "false":
				p.advance()
				return false, nil
			case "null":
				p.advance()
				return nil, nil
			}
		}
	case tParam:
		return nil, serr.New("placeholders are not supported yet", "param", t.text)
	}
	return nil, p.unexpected("a literal value")
}
