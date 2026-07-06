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
			name, err := p.tableName()
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
	case p.acceptKw("begin"):
		if !p.acceptKw("work") {
			p.acceptKw("transaction")
		}
		return p.txnModes(&TxnControl{Kind: TxnBegin, Tag: "BEGIN"})
	case p.acceptKw("start"):
		if err := p.expectKw("transaction"); err != nil {
			return nil, err
		}
		return p.txnModes(&TxnControl{Kind: TxnBegin, Tag: "START TRANSACTION"})
	case p.acceptKw("commit"), p.acceptKw("end"):
		return p.txnEnd(&TxnControl{Kind: TxnCommit, Tag: "COMMIT"})
	case p.acceptKw("rollback"), p.acceptKw("abort"):
		return p.txnEnd(&TxnControl{Kind: TxnRollback, Tag: "ROLLBACK"})
	case p.acceptKw("savepoint"):
		name, err := p.ident("a savepoint name")
		if err != nil {
			return nil, err
		}
		return &TxnControl{Kind: TxnSavepoint, Tag: "SAVEPOINT", Name: name}, nil
	case p.acceptKw("release"):
		p.acceptKw("savepoint")
		name, err := p.ident("a savepoint name")
		if err != nil {
			return nil, err
		}
		return &TxnControl{Kind: TxnRelease, Tag: "RELEASE", Name: name}, nil
	}
	return nil, p.unexpected("a SQL statement")
}

// txnModes parses BEGIN's transaction modes, in any order with
// optional commas: ISOLATION LEVEL ... (accepted and ignored — every
// engine transaction is serializable), READ ONLY / READ WRITE, and
// [NOT] DEFERRABLE (ignored; it only matters for concurrent
// serializable reads, which the engine gives for free).
func (p *parser) txnModes(tc *TxnControl) (Statement, error) {
	for {
		switch {
		case p.acceptKw("isolation"):
			if err := p.expectKw("level"); err != nil {
				return nil, err
			}
			switch {
			case p.acceptKw("serializable"):
			case p.acceptKw("repeatable"):
				if err := p.expectKw("read"); err != nil {
					return nil, err
				}
			case p.acceptKw("read"):
				if !p.acceptKw("committed") && !p.acceptKw("uncommitted") {
					return nil, p.unexpected("COMMITTED or UNCOMMITTED")
				}
			default:
				return nil, p.unexpected("an isolation level")
			}
		case p.acceptKw("read"):
			switch {
			case p.acceptKw("only"):
				tc.ReadOnly = true
			case p.acceptKw("write"):
				tc.ReadOnly = false
			default:
				return nil, p.unexpected("ONLY or WRITE")
			}
		case p.acceptKw("not"):
			if err := p.expectKw("deferrable"); err != nil {
				return nil, err
			}
		case p.acceptKw("deferrable"):
		default:
			return tc, nil
		}
		p.acceptOp(",")
	}
}

// txnEnd parses COMMIT/ROLLBACK's tail: [WORK | TRANSACTION]
// [AND [NO] CHAIN], or for ROLLBACK a TO [SAVEPOINT] name. AND CHAIN
// (immediately restarting a transaction with the same modes) is not
// supported.
func (p *parser) txnEnd(tc *TxnControl) (Statement, error) {
	if !p.acceptKw("work") {
		p.acceptKw("transaction")
	}
	if tc.Kind == TxnRollback && p.acceptKw("to") {
		p.acceptKw("savepoint")
		name, err := p.ident("a savepoint name")
		if err != nil {
			return nil, err
		}
		tc.Kind, tc.Name = TxnRollbackTo, name
		return tc, nil
	}
	if p.acceptKw("and") {
		no := p.acceptKw("no")
		if err := p.expectKw("chain"); err != nil {
			return nil, err
		}
		if !no {
			return nil, serr.New("AND CHAIN is not supported")
		}
	}
	return tc, nil
}

// --- DDL ---

func (p *parser) createTable() (Statement, error) {
	name, err := p.tableName()
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
	table, err := p.tableName()
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
		if di.Table, err = p.tableName(); err != nil {
			return nil, err
		}
	}
	return di, nil
}

func (p *parser) alterTable() (Statement, error) {
	if err := p.expectKw("table"); err != nil {
		return nil, err
	}
	table, err := p.tableName()
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
	table, err := p.tableName()
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
	s, err := p.selectCore()
	if err != nil {
		return nil, err
	}
	// UNION [ALL] chains; ORDER BY / LIMIT / OFFSET then apply to the
	// combined result.
	for p.acceptKw("union") {
		all := p.acceptKw("all")
		if err := p.expectKw("select"); err != nil {
			return nil, err
		}
		arm, err := p.selectCore()
		if err != nil {
			return nil, err
		}
		s.Union = append(s.Union, UnionArm{All: all, Sel: arm})
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

// selectCore parses one SELECT's items, FROM, WHERE, GROUP BY, and
// HAVING — the part a UNION arm owns; ORDER BY, LIMIT, and OFFSET
// belong to the whole statement.
func (p *parser) selectCore() (*Select, error) {
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
	var err error
	if p.acceptKw("from") { // FROM is optional: SELECT 1, SELECT version()
		if s.From, err = p.fromClause(); err != nil {
			return nil, err
		}
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
			g, err := p.groupItem()
			if err != nil {
				return nil, err
			}
			s.GroupBy = append(s.GroupBy, g)
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
	return s, nil
}

// groupItem parses one GROUP BY key through the expression grammar.
// A bare integer constant is an ordinal naming a select-list position
// (GROUP BY 1); any other bare constant draws the same complaint
// Postgres makes; a column stays a column, anything richer is a
// general expression key.
func (p *parser) groupItem() (GroupItem, error) {
	e, err := p.expression()
	if err != nil {
		return GroupItem{}, err
	}
	switch n := e.(type) {
	case *ExLit:
		v, ok := n.Val.(int64)
		if !ok {
			return GroupItem{}, serr.New("non-integer constant in GROUP BY",
				"got", fmt.Sprint(n.Val))
		}
		if v < 1 {
			return GroupItem{}, serr.New("GROUP BY position is not in the select list",
				"position", fmt.Sprint(v))
		}
		return GroupItem{Pos: v}, nil
	case *ExCol:
		return GroupItem{Col: n.Col}, nil
	}
	return GroupItem{Ex: e}, nil
}

func (p *parser) nonNegInt(clause string) (int64, error) {
	t := p.cur()
	if t.kind == tParam {
		return 0, serr.New("placeholders are not supported in "+clause, "param", t.text)
	}
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
	table, err := p.tableName()
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
	table, err := p.tableName()
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

// tableName parses a table name with an optional schema qualifier:
// public.t normalizes to t; pg_catalog.t and information_schema.t
// stay qualified (they name the virtual system catalog); any other
// schema is an error.
func (p *parser) tableName() (string, error) {
	first, err := p.ident("a table name")
	if err != nil {
		return "", err
	}
	if !p.acceptOp(".") {
		return first, nil
	}
	name, err := p.ident("a table name")
	if err != nil {
		return "", err
	}
	switch first {
	case "public":
		return name, nil
	case "pg_catalog", "information_schema":
		return first + "." + name, nil
	}
	return "", serr.New("no such schema", "schema", first)
}

// tokAt is the token n places ahead (saturating at EOF).
func (p *parser) tokAt(n int) token {
	if p.i+n >= len(p.toks) {
		return p.toks[len(p.toks)-1]
	}
	return p.toks[p.i+n]
}

// sysFuncCall folds a whitelisted zero-argument function call —
// version(), current_schema(), ..., optionally pg_catalog-qualified —
// to its constant. ok reports whether a call was consumed (or began
// and failed).
func (p *parser) sysFuncCall() (val, name string, ok bool, err error) {
	t := p.cur()
	if t.kind != tIdent {
		return "", "", false, nil
	}
	name, at := t.text, 0
	if name == "pg_catalog" && p.peekOpAt(1, ".") && p.tokAt(2).kind == tIdent {
		name, at = p.tokAt(2).text, 2
	}
	v, isFn := sysFuncs[name]
	if !isFn || !p.peekOpAt(at+1, "(") {
		return "", "", false, nil
	}
	p.i += at + 2 // the name (with any qualifier) and '('
	if err := p.expectOp(")"); err != nil {
		return "", "", true, serr.Wrap(err, "msg", name+" takes no arguments")
	}
	return v, name, true, nil
}

// tableRef parses "t", "t alias", or "t AS alias". A function call in
// FROM (generate_series(...) s) parses too — psql writes them inside
// subqueries that never run — but errors if its scan is ever built.
func (p *parser) tableRef() (FromItem, error) {
	name, err := p.tableName()
	if err != nil {
		return FromItem{}, err
	}
	it := FromItem{Table: name}
	if p.cur().kind == tOp && p.cur().text == "(" {
		p.advance()
		it.FuncArgs = []Expr{}
		if !p.acceptOp(")") {
			for {
				a, err := p.expression()
				if err != nil {
					return FromItem{}, err
				}
				it.FuncArgs = append(it.FuncArgs, a)
				if !p.acceptOp(",") {
					break
				}
			}
			if err := p.expectOp(")"); err != nil {
				return FromItem{}, err
			}
		}
	}
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

// itemEnd is the keywords that cannot follow a select item as a bare
// output alias; an alias spelled like one needs AS or double quotes.
var itemEnd = map[string]bool{
	"from": true, "where": true, "group": true, "having": true,
	"order": true, "limit": true, "offset": true, "union": true,
	"as": true,
}

// selectListItem parses one select-list entry: t.*, or an expression
// with an optional [AS] output alias.
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
	item, err := p.selectItem()
	if err != nil {
		return item, err
	}
	if p.acceptKw("as") {
		item.As, err = p.ident("an output name")
	} else if t := p.cur(); t.kind == tQIdent || (t.kind == tIdent && !itemEnd[t.text]) {
		item.As, err = p.ident("an output name")
	}
	return item, err
}

// selectItem parses one select-list or ORDER BY entry through the
// expression grammar, lowering the simple shapes — a column, an
// aggregate call, a literal (in ORDER BY an integer literal is a
// select-list position) — to their legacy item kinds; anything richer
// stays a general expression.
func (p *parser) selectItem() (SelectItem, error) {
	e, err := p.expression()
	if err != nil {
		return SelectItem{}, err
	}
	return lowerItem(e), nil
}

// lowerItem maps an expression to a select item, keeping the legacy
// shapes where they fit.
func lowerItem(e Expr) SelectItem {
	switch n := e.(type) {
	case *ExCol:
		return SelectItem{Col: n.Col}
	case *ExAgg:
		if n.Arg != nil { // an expression argument stays an expression
			return SelectItem{Ex: e}
		}
		return SelectItem{Agg: n.Fn, Col: n.Col, Star: n.Star}
	case *ExLit:
		name := n.Name
		if name == "" {
			name = "?column?"
		}
		return SelectItem{IsLit: true, Lit: n.Val, LitName: name}
	}
	return SelectItem{Ex: e}
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

// --- expressions ---

// boolExpr parses a condition (WHERE, ON, HAVING) through the
// expression grammar, then lowers it: AND/OR/NOT map to the legacy
// tree, simple comparisons become Pred leaves (keeping index pushdown
// and literal coercion), and everything else becomes a Cond leaf for
// the expression evaluator. allowAgg permits aggregate calls (HAVING).
func (p *parser) boolExpr(allowAgg bool) (BoolExpr, error) {
	e, err := p.expression()
	if err != nil {
		return nil, err
	}
	if !allowAgg {
		if fn := findAgg(e); fn != AggNone {
			return nil, serr.New("aggregates are not allowed in WHERE; use HAVING", "function", fn.name())
		}
	}
	return lowerBool(e)
}

func lowerBool(e Expr) (BoolExpr, error) {
	switch n := e.(type) {
	case *ExAnd:
		out := &And{Exprs: make([]BoolExpr, len(n.Exprs))}
		for i, sub := range n.Exprs {
			var err error
			if out.Exprs[i], err = lowerBool(sub); err != nil {
				return nil, err
			}
		}
		return out, nil
	case *ExOr:
		out := &Or{Exprs: make([]BoolExpr, len(n.Exprs))}
		for i, sub := range n.Exprs {
			var err error
			if out.Exprs[i], err = lowerBool(sub); err != nil {
				return nil, err
			}
		}
		return out, nil
	case *ExNot:
		sub, err := lowerBool(n.E)
		if err != nil {
			return nil, err
		}
		return &Not{Expr: sub}, nil
	case *ExIsNull:
		if it, ok := simpleItem(n.E); ok {
			op := OpIsNull
			if n.Not {
				op = OpIsNotNull
			}
			return &Pred{Item: it, Op: op}, nil
		}
		return &Cond{Ex: n}, nil
	case *ExCmp:
		lIt, lOK := simpleItem(n.L)
		rIt, rOK := simpleItem(n.R)
		lLit, lIsLit := n.L.(*ExLit)
		rLit, rIsLit := n.R.(*ExLit)
		switch {
		case lOK && rOK:
			r := rIt
			return &Pred{Item: lIt, Op: n.Op, RItem: &r}, nil
		case lOK && rIsLit:
			if rLit.Val == nil {
				return nil, serr.New("cannot compare with NULL; use IS [NOT] NULL")
			}
			return &Pred{Item: lIt, Op: n.Op, Val: rLit.Val}, nil
		case rOK && lIsLit && flippable(n.Op):
			if lLit.Val == nil {
				return nil, serr.New("cannot compare with NULL; use IS [NOT] NULL")
			}
			return &Pred{Item: rIt, Op: flip(n.Op), Val: lLit.Val}, nil
		}
		// Anything else — including WHERE 1 = 1 — evaluates per row.
		return &Cond{Ex: n}, nil
	}
	return &Cond{Ex: e}, nil
}

// simpleItem recognizes the operands the legacy Pred shape carries: a
// plain column or an aggregate call.
func simpleItem(e Expr) (SelectItem, bool) {
	switch n := e.(type) {
	case *ExCol:
		return SelectItem{Col: n.Col}, true
	case *ExAgg:
		if n.Arg == nil { // an expression argument stays an expression
			return SelectItem{Agg: n.Fn, Col: n.Col, Star: n.Star}, true
		}
	}
	return SelectItem{}, false
}

// findAgg reports an aggregate call anywhere in an expression, not
// descending into subqueries (their aggregates are their own).
func findAgg(e Expr) AggFunc {
	found := AggNone
	walkExpr(e, func(sub Expr) bool {
		if _, isSub := sub.(*ExSub); isSub {
			return false
		}
		if a, ok := sub.(*ExAgg); ok && found == AggNone {
			found = a.Fn
		}
		return true
	})
	return found
}

// flippable reports whether a comparison can mirror across its
// operands (the regex operators cannot).
func flippable(op PredOp) bool {
	switch op {
	case OpEQ, OpNE, OpLT, OpLE, OpGT, OpGE:
		return true
	}
	return false
}

// expression parses a full scalar/boolean expression: OR binds
// loosest, then AND, NOT, comparisons (with IS NULL, IN, ANY, and
// COLLATE postfixes), additive (+ - ||), multiplicative (* / %),
// unary sign, and the :: cast and [] subscript postfixes.
func (p *parser) expression() (Expr, error) { return p.exprOr() }

func (p *parser) exprOr() (Expr, error) {
	e, err := p.exprAnd()
	if err != nil {
		return nil, err
	}
	for p.acceptKw("or") {
		r, err := p.exprAnd()
		if err != nil {
			return nil, err
		}
		if or, ok := e.(*ExOr); ok {
			or.Exprs = append(or.Exprs, r)
		} else {
			e = &ExOr{Exprs: []Expr{e, r}}
		}
	}
	return e, nil
}

func (p *parser) exprAnd() (Expr, error) {
	e, err := p.exprNot()
	if err != nil {
		return nil, err
	}
	for p.acceptKw("and") {
		r, err := p.exprNot()
		if err != nil {
			return nil, err
		}
		if and, ok := e.(*ExAnd); ok {
			and.Exprs = append(and.Exprs, r)
		} else {
			e = &ExAnd{Exprs: []Expr{e, r}}
		}
	}
	return e, nil
}

func (p *parser) exprNot() (Expr, error) {
	if p.acceptKw("not") {
		e, err := p.exprNot()
		if err != nil {
			return nil, err
		}
		return &ExNot{E: e}, nil
	}
	return p.exprCmp()
}

// exprCmp parses an additive expression followed by comparison-level
// postfixes: COLLATE (parsed and ignored), IS [NOT] NULL, [NOT] IN
// (...), and one comparison operator — plain or OPERATOR-qualified —
// whose right side may be ANY(...).
func (p *parser) exprCmp() (Expr, error) {
	e, err := p.exprAdd()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.acceptKw("collate"):
			if _, err := p.ident("a collation name"); err != nil {
				return nil, err
			}
			if p.acceptOp(".") {
				if _, err := p.ident("a collation name"); err != nil {
					return nil, err
				}
			}
		case p.acceptKw("is"):
			not := p.acceptKw("not")
			if err := p.expectKw("null"); err != nil {
				return nil, err
			}
			e = &ExIsNull{E: e, Not: not}
		case p.cur().kind == tIdent && p.cur().text == "in":
			p.advance()
			list, err := p.exprList()
			if err != nil {
				return nil, err
			}
			e = &ExIn{E: e, List: list}
		case p.cur().kind == tIdent && p.cur().text == "not" &&
			p.tokAt(1).kind == tIdent && p.tokAt(1).text == "in":
			p.advance()
			p.advance()
			list, err := p.exprList()
			if err != nil {
				return nil, err
			}
			e = &ExIn{E: e, List: list, Not: true}
		default:
			op, ok, err := p.cmpOp()
			if err != nil {
				return nil, err
			}
			if !ok {
				return e, nil
			}
			if t := p.cur(); t.kind == tIdent && t.text == "any" && p.peekOp("(") {
				p.advance()
				p.advance()
				r, err := p.expression()
				if err != nil {
					return nil, err
				}
				if err := p.expectOp(")"); err != nil {
					return nil, err
				}
				e = &ExAny{Op: op, L: e, R: r}
				continue
			}
			r, err := p.exprAdd()
			if err != nil {
				return nil, err
			}
			e = &ExCmp{Op: op, L: e, R: r}
		}
	}
}

// exprList parses "( expr [, expr]* )".
func (p *parser) exprList() ([]Expr, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var out []Expr
	for {
		e, err := p.expression()
		if err != nil {
			return nil, err
		}
		out = append(out, e)
		if !p.acceptOp(",") {
			break
		}
	}
	return out, p.expectOp(")")
}

func (p *parser) exprAdd() (Expr, error) {
	e, err := p.exprMul()
	if err != nil {
		return nil, err
	}
	for {
		t := p.cur()
		if t.kind != tOp || (t.text != "+" && t.text != "-" && t.text != "||") {
			return e, nil
		}
		p.advance()
		r, err := p.exprMul()
		if err != nil {
			return nil, err
		}
		e = &ExArith{Op: t.text, L: e, R: r}
	}
}

func (p *parser) exprMul() (Expr, error) {
	e, err := p.exprUnary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.cur()
		if t.kind != tOp || (t.text != "*" && t.text != "/" && t.text != "%") {
			return e, nil
		}
		p.advance()
		r, err := p.exprUnary()
		if err != nil {
			return nil, err
		}
		e = &ExArith{Op: t.text, L: e, R: r}
	}
}

func (p *parser) exprUnary() (Expr, error) {
	t := p.cur()
	if t.kind == tOp && (t.text == "-" || t.text == "+") {
		// A signed number folds to a literal; any other operand
		// becomes 0 - E (or passes through for unary +).
		if p.tokAt(1).kind == tNumber {
			v, err := p.literal()
			return &ExLit{Val: v}, err
		}
		p.advance()
		e, err := p.exprPostfix()
		if err != nil {
			return nil, err
		}
		if t.text == "-" {
			return &ExArith{Op: "-", L: &ExLit{Val: int64(0)}, R: e}, nil
		}
		return e, nil
	}
	return p.exprPostfix()
}

func (p *parser) exprPostfix() (Expr, error) {
	e, err := p.exprPrimary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.acceptOp("::"):
			typ, err := p.castType()
			if err != nil {
				return nil, err
			}
			e = &ExCast{E: e, Type: typ}
		case p.acceptOp("["):
			idx, err := p.expression()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp("]"); err != nil {
				return nil, err
			}
			e = &ExIndex{E: e, Idx: idx}
		default:
			return e, nil
		}
	}
}

// castType parses [schema.]name[[]]; the schema qualifier drops, a
// trailing [] is kept on the name.
func (p *parser) castType() (string, error) {
	name, err := p.ident("a type name")
	if err != nil {
		return "", err
	}
	if p.acceptOp(".") {
		if name, err = p.ident("a type name"); err != nil {
			return "", err
		}
	}
	if name == "double" {
		p.acceptKw("precision")
		name = "float8"
	}
	if p.acceptOp("[") {
		if err := p.expectOp("]"); err != nil {
			return "", err
		}
		name += "[]"
	}
	return name, nil
}

func (p *parser) exprPrimary() (Expr, error) {
	// Folded zero-argument system functions: version(), ...
	if v, name, ok, err := p.sysFuncCall(); err != nil {
		return nil, err
	} else if ok {
		return &ExLit{Val: v, Name: name}, nil
	}
	t := p.cur()
	if t.kind == tNumber || t.kind == tString || t.kind == tParam ||
		(t.kind == tIdent && (t.text == "true" || t.text == "false" || t.text == "null")) {
		v, err := p.literal()
		return &ExLit{Val: v}, err
	}
	if t.kind == tOp && t.text == "(" {
		p.advance()
		if c := p.cur(); c.kind == tIdent && c.text == "select" {
			p.advance()
			st, err := p.selectStmt()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return &ExSub{Sel: st.(*Select)}, nil
		}
		e, err := p.expression()
		if err != nil {
			return nil, err
		}
		return e, p.expectOp(")")
	}
	if t.kind == tIdent && t.text == "case" {
		p.advance()
		return p.caseExpr()
	}
	if t.kind == tIdent && aggNames[t.text] != AggNone && p.peekOp("(") {
		fn := aggNames[t.text]
		p.advance() // function name
		p.advance() // (
		agg := &ExAgg{Fn: fn}
		if fn == AggCount && p.acceptOp("*") {
			agg.Star = true
		} else {
			e, err := p.expression()
			if err != nil {
				return nil, err
			}
			if c, ok := e.(*ExCol); ok {
				agg.Col = c.Col
			} else {
				if findAgg(e) != AggNone {
					return nil, serr.New("aggregate function calls cannot be nested")
				}
				agg.Arg = e
			}
		}
		return agg, p.expectOp(")")
	}
	if t.kind == tIdent && p.peekOp("(") {
		name := t.text
		p.advance()
		return p.funcCall(name)
	}
	if t.kind == tIdent && p.peekOpAt(1, ".") && p.tokAt(2).kind == tIdent && p.peekOpAt(3, "(") {
		// A schema-qualified function call; the schema drops.
		name := p.tokAt(2).text
		p.i += 3
		return p.funcCall(name)
	}
	if t.kind == tIdent || t.kind == tQIdent {
		col, err := p.colRef()
		return &ExCol{Col: col}, err
	}
	return nil, p.unexpected("an expression")
}

// funcCall parses the argument list of name(...); the caller has
// consumed the name, cur is '('. An argument may itself be a bare
// subquery — the ARRAY(SELECT ...) constructor psql writes (which,
// like any unknown function, errors only if evaluated).
func (p *parser) funcCall(name string) (Expr, error) {
	p.advance() // (
	f := &ExFunc{Name: name}
	if p.acceptOp(")") {
		return f, nil
	}
	for {
		var a Expr
		if t := p.cur(); t.kind == tIdent && t.text == "select" {
			p.advance()
			st, err := p.selectStmt()
			if err != nil {
				return nil, err
			}
			a = &ExSub{Sel: st.(*Select)}
		} else {
			var err error
			if a, err = p.expression(); err != nil {
				return nil, err
			}
		}
		f.Args = append(f.Args, a)
		if !p.acceptOp(",") {
			break
		}
	}
	return f, p.expectOp(")")
}

// caseExpr parses both CASE forms; "case" is consumed.
func (p *parser) caseExpr() (Expr, error) {
	c := &ExCase{}
	if t := p.cur(); !(t.kind == tIdent && t.text == "when") {
		op, err := p.expression()
		if err != nil {
			return nil, err
		}
		c.Operand = op
	}
	for {
		if err := p.expectKw("when"); err != nil {
			return nil, err
		}
		w, err := p.expression()
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("then"); err != nil {
			return nil, err
		}
		th, err := p.expression()
		if err != nil {
			return nil, err
		}
		c.Whens = append(c.Whens, ExWhen{When: w, Then: th})
		if t := p.cur(); !(t.kind == tIdent && t.text == "when") {
			break
		}
	}
	if p.acceptKw("else") {
		var err error
		if c.Else, err = p.expression(); err != nil {
			return nil, err
		}
	}
	return c, p.expectKw("end")
}

// cmpOp consumes a comparison operator if one is next; ok is false
// when the next token is not one. The OPERATOR(pg_catalog.op) form
// reads as the operator it names.
func (p *parser) cmpOp() (op PredOp, ok bool, err error) {
	t := p.cur()
	if t.kind == tIdent && t.text == "operator" && p.peekOp("(") {
		p.advance()
		p.advance()
		if p.cur().kind == tIdent {
			if _, err := p.ident("an operator schema"); err != nil {
				return 0, false, err
			}
			if err := p.expectOp("."); err != nil {
				return 0, false, err
			}
		}
		op, ok := predOpFor(p.cur())
		if !ok {
			return 0, false, p.unexpected("an operator")
		}
		p.advance()
		return op, true, p.expectOp(")")
	}
	op, ok = predOpFor(t)
	if !ok {
		return 0, false, nil
	}
	p.advance()
	return op, true, nil
}

func predOpFor(t token) (PredOp, bool) {
	if t.kind != tOp {
		return 0, false
	}
	switch t.text {
	case "=":
		return OpEQ, true
	case "!=", "<>":
		return OpNE, true
	case "<":
		return OpLT, true
	case "<=":
		return OpLE, true
	case ">":
		return OpGT, true
	case ">=":
		return OpGE, true
	case "~":
		return OpRegex, true
	case "!~":
		return OpNotRegex, true
	case "~*":
		return OpRegexI, true
	case "!~*":
		return OpNotRegexI, true
	}
	return 0, false
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

// literal parses one literal value — [+|-] number, 'string', TRUE,
// FALSE, or NULL — or a $n placeholder, which becomes a Param marker.
// Numbers become int64 when they fit, else float64.
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
		if neg {
			return nil, p.unexpected("a number after '-'")
		}
		n, err := strconv.Atoi(t.text[1:])
		if err != nil || n < 1 {
			return nil, serr.New("parameter numbers start at $1", "param", t.text, "pos", fmt.Sprint(t.pos))
		}
		p.advance()
		return Param(n), nil
	}
	return nil, p.unexpected("a literal value")
}
