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
			item, err := p.selectItem()
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
	if s.Table, err = p.ident("a table name"); err != nil {
		return nil, err
	}
	if p.acceptKw("where") {
		if s.Where, err = p.wherePreds(); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("group") {
		if err := p.expectKw("by"); err != nil {
			return nil, err
		}
		for {
			name, err := p.ident("a column name")
			if err != nil {
				return nil, err
			}
			s.GroupBy = append(s.GroupBy, name)
			if !p.acceptOp(",") {
				break
			}
		}
	}
	if p.acceptKw("having") {
		for {
			pr, err := p.aggPred()
			if err != nil {
				return nil, err
			}
			s.Having = append(s.Having, pr)
			if !p.acceptKw("and") {
				break
			}
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
		if u.Where, err = p.wherePreds(); err != nil {
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
		if d.Where, err = p.wherePreds(); err != nil {
			return nil, err
		}
	}
	return d, nil
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
			col, err := p.ident("a column name")
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
	col, err := p.ident("a column name")
	return SelectItem{Col: col}, err
}

// peekOp reports whether the token after the current one is op.
func (p *parser) peekOp(op string) bool {
	if p.cur().kind == tEOF {
		return false
	}
	t := p.toks[p.i+1]
	return t.kind == tOp && t.text == op
}

// aggPred parses one HAVING conjunct: item op literal, or item IS
// [NOT] NULL.
func (p *parser) aggPred() (AggPred, error) {
	item, err := p.selectItem()
	if err != nil {
		return AggPred{}, err
	}
	if p.acceptKw("is") {
		op := OpIsNull
		if p.acceptKw("not") {
			op = OpIsNotNull
		}
		if err := p.expectKw("null"); err != nil {
			return AggPred{}, err
		}
		return AggPred{Item: item, Op: op}, nil
	}
	op, err := p.cmpOp()
	if err != nil {
		return AggPred{}, err
	}
	val, err := p.literal()
	if err != nil {
		return AggPred{}, err
	}
	if val == nil {
		return AggPred{}, serr.New("cannot compare with NULL; use IS [NOT] NULL")
	}
	return AggPred{Item: item, Op: op, Val: val}, nil
}

// --- WHERE ---

func (p *parser) wherePreds() ([]Pred, error) {
	var preds []Pred
	for {
		pr, err := p.pred()
		if err != nil {
			return nil, err
		}
		preds = append(preds, pr)
		if !p.acceptKw("and") {
			break
		}
	}
	return preds, nil
}

// pred parses one predicate: column op literal (either order; a
// literal-first comparison is flipped), or column IS [NOT] NULL.
func (p *parser) pred() (Pred, error) {
	lCol, lVal, lIsCol, err := p.operand()
	if err != nil {
		return Pred{}, err
	}
	if lIsCol && p.acceptKw("is") {
		op := OpIsNull
		if p.acceptKw("not") {
			op = OpIsNotNull
		}
		if err := p.expectKw("null"); err != nil {
			return Pred{}, err
		}
		return Pred{Col: lCol, Op: op}, nil
	}
	op, err := p.cmpOp()
	if err != nil {
		return Pred{}, err
	}
	rCol, rVal, rIsCol, err := p.operand()
	if err != nil {
		return Pred{}, err
	}
	switch {
	case lIsCol && !rIsCol:
		if rVal == nil {
			return Pred{}, serr.New("cannot compare with NULL; use IS [NOT] NULL", "column", lCol)
		}
		return Pred{Col: lCol, Op: op, Val: rVal}, nil
	case !lIsCol && rIsCol:
		if lVal == nil {
			return Pred{}, serr.New("cannot compare with NULL; use IS [NOT] NULL", "column", rCol)
		}
		return Pred{Col: rCol, Op: flip(op), Val: lVal}, nil
	}
	return Pred{}, serr.New("a predicate must compare a column with a literal")
}

// operand parses a column reference or a literal.
func (p *parser) operand() (col string, val any, isCol bool, err error) {
	t := p.cur()
	if t.kind == tIdent && (t.text == "true" || t.text == "false" || t.text == "null") {
		val, err = p.literal()
		return "", val, false, err
	}
	if t.kind == tIdent && aggNames[t.text] != AggNone && p.peekOp("(") {
		return "", nil, false, serr.New("aggregates are not allowed in WHERE; use HAVING", "function", t.text)
	}
	if t.kind == tIdent || t.kind == tQIdent {
		col, err = p.ident("a column name")
		return col, nil, true, err
	}
	val, err = p.literal()
	return "", val, false, err
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
