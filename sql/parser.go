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
	p := &parser{toks: toks, src: src}
	st, err := p.statement()
	if err != nil {
		return nil, err
	}
	p.acceptOp(";")
	if p.cur().kind != tEOF {
		return nil, p.unexpected("end of statement")
	}
	if err := attachCTEs(st, p.ctes); err != nil {
		return nil, err
	}
	return st, nil
}

// attachCTEs hangs the statement's accumulated WITH entries (written
// and derived-table synthetics alike) on its top-level SELECT, where
// the executor materializes them. Statements without a SELECT to hang
// them on cannot host them.
func attachCTEs(st Statement, ctes []CTE) error {
	if len(ctes) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, c := range ctes {
		if seen[c.Name] {
			return serr.New(`WITH query name "` + c.Name + `" specified more than once`)
		}
		seen[c.Name] = true
	}
	switch s := st.(type) {
	case *Select:
		s.With = ctes
		return nil
	case *Explain:
		return attachCTEs(s.Stmt, ctes)
	}
	return serr.New("WITH and derived tables are only supported in SELECT statements")
}

// parseCheckExpr parses a stored CHECK constraint expression.
func parseCheckExpr(src string) (Expr, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, src: src}
	ex, err := p.expression()
	if err != nil {
		return nil, err
	}
	if p.cur().kind != tEOF {
		return nil, p.unexpected("end of expression")
	}
	return ex, nil
}

// maxParseDepth bounds expression nesting. The parser is recursive
// descent, so input depth is call-stack depth: without a bound, a few
// MB of nested parens is a fatal stack overflow that recover() cannot
// catch — a remote kill for any server embedding the parser. The limit
// also bounds AST depth, protecting every later recursive walk
// (lowering, planning, evaluation). 1000 is far beyond any real query
// (Postgres's default max_stack_depth allows on the order of a hundred
// levels) while costing only a few thousand Go frames.
const maxParseDepth = 1000

// maxParamNumber bounds $n placeholders. 65535 is the wire protocol's
// own ceiling (Bind carries the parameter count as an int16), so no
// conforming client can bind more; see the tParam case in literal for
// why an unbounded $n is dangerous.
const maxParamNumber = 65535

type parser struct {
	toks []token
	src  string // for slicing verbatim expression text (CHECK constraints)
	i    int

	// depth is the current expression-nesting depth; see enter.
	depth int

	// ctes accumulates the statement's WITH entries — written ones and
	// the synthetic ones derived tables lower to — in completion order,
	// which is dependency order (an inner query finishes parsing before
	// whatever contains it). Parse attaches them to the top-level
	// SELECT; nDerived numbers the synthetic names.
	ctes     []CTE
	nDerived int
}

// enter counts one level of expression nesting, rejecting input past
// maxParseDepth; every call must pair with leave (via defer).
func (p *parser) enter() error {
	p.depth++
	if p.depth > maxParseDepth {
		return serr.New("expression is nested too deeply",
			"limit", fmt.Sprint(maxParseDepth), "pos", fmt.Sprint(p.cur().pos))
	}
	return nil
}

func (p *parser) leave() { p.depth-- }

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
		if p.acceptKw("sequence") {
			return p.createSequence()
		}
		if p.acceptKw("or") {
			if err := p.expectKw("replace"); err != nil {
				return nil, err
			}
			if err := p.expectKw("view"); err != nil {
				return nil, err
			}
			return p.createView(true)
		}
		if p.acceptKw("view") {
			return p.createView(false)
		}
		unique := p.acceptKw("unique")
		if p.acceptKw("index") {
			return p.createIndex(unique)
		}
		return nil, p.unexpected("TABLE, VIEW, SEQUENCE, or [UNIQUE] INDEX")
	case p.acceptKw("drop"):
		switch {
		case p.acceptKw("table"):
			name, err := p.tableName()
			if err != nil {
				return nil, err
			}
			return &DropTable{Table: name}, nil
		case p.acceptKw("view"):
			dv := &DropView{}
			if p.acceptKw("if") {
				if err := p.expectKw("exists"); err != nil {
					return nil, err
				}
				dv.IfExists = true
			}
			var err error
			if dv.Name, err = p.tableName(); err != nil {
				return nil, err
			}
			return dv, nil
		case p.acceptKw("sequence"):
			return p.dropSequence()
		case p.acceptKw("index"):
			return p.dropIndex()
		}
		return nil, p.unexpected("TABLE, VIEW, SEQUENCE, or INDEX")
	case p.acceptKw("alter"):
		if p.acceptKw("sequence") {
			return p.alterSequence()
		}
		return p.alterTable()
	case p.acceptKw("truncate"):
		return p.truncateStmt()
	case p.acceptKw("show"):
		return p.showStmt()
	case p.acceptKw("insert"):
		return p.insert()
	case p.acceptKw("with"):
		return p.withStmt()
	case p.acceptKw("select"):
		return p.selectStmt()
	case p.acceptKw("update"):
		return p.update()
	case p.acceptKw("delete"):
		return p.deleteStmt()
	case p.acceptKw("explain"):
		return p.explainStmt()
	case p.acceptKw("set"):
		return p.setStmt()
	case p.acceptKw("reset"):
		// RESET name (or RESET ALL) is SET name TO DEFAULT.
		if p.acceptKw("all") {
			return &SetVar{Name: "all", IsDefault: true, Tag: "RESET"}, nil
		}
		name, err := p.ident("configuration parameter")
		if err != nil {
			return nil, err
		}
		return &SetVar{Name: name, IsDefault: true, Tag: "RESET"}, nil
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

// explainStmt parses EXPLAIN [(option, ...)] | [ANALYZE] [VERBOSE]
// followed by an explainable statement. ANALYZE is rejected — bytdb
// does not instrument execution, and pretending to would lie about
// what ran; the other Postgres options parse and are ignored except
// FORMAT, which must be TEXT.
// setStmt parses SET [SESSION|LOCAL] name {=|TO} value[, ...] after
// the SET keyword. SESSION and LOCAL both scope to the session: bytdb
// has no transaction-scoped parameters, so LOCAL degrades to SESSION.
// SET TIME ZONE, the one grammar special case drivers actually send,
// parses as the timezone parameter.
func (p *parser) setStmt() (Statement, error) {
	if !p.acceptKw("session") {
		p.acceptKw("local")
	}
	if p.acceptKw("time") {
		if err := p.expectKw("zone"); err != nil {
			return nil, err
		}
		v, err := p.setValue()
		if err != nil {
			return nil, err
		}
		return &SetVar{Name: "timezone", Value: v, Tag: "SET"}, nil
	}
	name, err := p.ident("a configuration parameter")
	if err != nil {
		return nil, err
	}
	if !p.acceptOp("=") {
		if err := p.expectKw("to"); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("default") {
		return &SetVar{Name: name, IsDefault: true, Tag: "SET"}, nil
	}
	var vals []string
	for {
		v, err := p.setValue()
		if err != nil {
			return nil, err
		}
		vals = append(vals, v)
		if !p.acceptOp(",") {
			break
		}
	}
	return &SetVar{Name: name, Value: strings.Join(vals, ", "), Tag: "SET"}, nil
}

// setValue consumes one SET value: a literal, an identifier (on, off,
// public, "$user"), or a signed number — value text only, since SET
// applies parameters rather than evaluating expressions.
func (p *parser) setValue() (string, error) {
	t := p.cur()
	switch t.kind {
	case tString, tNumber, tIdent, tQIdent:
		p.advance()
		return t.text, nil
	case tOp:
		if t.text == "-" {
			p.advance()
			if n := p.cur(); n.kind == tNumber {
				p.advance()
				return "-" + n.text, nil
			}
			return "", p.unexpected("a number")
		}
	}
	return "", p.unexpected("a parameter value")
}

func (p *parser) explainStmt() (Statement, error) {
	analyze := false
	if p.cur().kind == tOp && p.cur().text == "(" {
		p.advance()
		for {
			opt, err := p.ident("an EXPLAIN option")
			if err != nil {
				return nil, err
			}
			switch opt {
			case "analyze", "analyse":
				analyze = p.explainOptBool()
			case "verbose", "costs", "buffers", "timing", "summary",
				"settings", "wal", "generic_plan", "memory", "serialize":
				p.explainOptBool()
			case "format":
				f, err := p.ident("a format name")
				if err != nil {
					return nil, err
				}
				if f != "text" {
					return nil, serr.New("only EXPLAIN (FORMAT TEXT) is supported", "format", f)
				}
			default:
				return nil, serr.New("unrecognized EXPLAIN option", "option", opt)
			}
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
	} else {
		for {
			switch {
			case p.acceptKw("analyze"), p.acceptKw("analyse"):
				analyze = true
			case p.acceptKw("verbose"):
			default:
				goto stmt
			}
		}
	}
stmt:
	if analyze {
		return nil, serr.New("EXPLAIN ANALYZE is not supported")
	}
	st, err := p.statement()
	if err != nil {
		return nil, err
	}
	switch st.(type) {
	case *Select, *Insert, *Update, *Delete:
		return &Explain{Stmt: st}, nil
	}
	return nil, serr.New("EXPLAIN supports SELECT, INSERT, UPDATE, and DELETE")
}

// explainOptBool consumes an EXPLAIN option's optional boolean
// argument, defaulting to true (naming an option turns it on).
func (p *parser) explainOptBool() bool {
	t := p.cur()
	if t.kind == tIdent {
		switch t.text {
		case "true", "on", "yes":
			p.advance()
			return true
		case "false", "off", "no":
			p.advance()
			return false
		}
	}
	if t.kind == tNumber && (t.text == "0" || t.text == "1") {
		p.advance()
		return t.text == "1"
	}
	return true
}

// withStmt parses WITH name [(cols)] AS (select), ... SELECT ...;
// "with" is consumed. Entries land in p.ctes (dependency order — each
// may use the ones before it), and Parse attaches them to the SELECT.
// RECURSIVE is rejected: there is no iteration machinery behind it.
func (p *parser) withStmt() (Statement, error) {
	if p.acceptKw("recursive") {
		return nil, serr.New("WITH RECURSIVE is not supported")
	}
	for {
		name, err := p.ident("a WITH query name")
		if err != nil {
			return nil, err
		}
		var cols []string
		if p.cur().kind == tOp && p.cur().text == "(" {
			if cols, err = p.identList("an output column name"); err != nil {
				return nil, err
			}
		}
		if err := p.expectKw("as"); err != nil {
			return nil, err
		}
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		if err := p.expectKw("select"); err != nil {
			return nil, err
		}
		st, err := p.selectStmt()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		p.ctes = append(p.ctes, CTE{Name: name, Cols: cols, Sel: st.(*Select)})
		if !p.acceptOp(",") {
			break
		}
	}
	if err := p.expectKw("select"); err != nil {
		return nil, err
	}
	return p.selectStmt()
}

// createView parses CREATE [OR REPLACE] VIEW name AS select; the
// leading keywords are consumed. The view stores the SELECT's source
// text, re-parsed at each use — so the body's derived-table synthetics
// are trimmed from p.ctes here (they re-hoist when the text is parsed
// again) rather than attached to this statement.
func (p *parser) createView(orReplace bool) (Statement, error) {
	name, err := p.tableName()
	if err != nil {
		return nil, err
	}
	if p.cur().kind == tOp && p.cur().text == "(" {
		return nil, serr.New("CREATE VIEW with a column list is not supported; alias the select items")
	}
	if err := p.expectKw("as"); err != nil {
		return nil, err
	}
	start := p.cur().pos
	preCTE := len(p.ctes)
	var body Statement
	switch {
	case p.acceptKw("with"):
		body, err = p.withStmt()
	case p.acceptKw("select"):
		body, err = p.selectStmt()
	default:
		return nil, p.unexpected("SELECT")
	}
	if err != nil {
		return nil, err
	}
	if _, ok := body.(*Select); !ok {
		return nil, p.unexpected("SELECT")
	}
	p.ctes = p.ctes[:preCTE]
	return &CreateView{
		Name:      name,
		Query:     strings.TrimSpace(p.src[start:p.cur().pos]),
		OrReplace: orReplace,
	}, nil
}

// truncateStmt parses TRUNCATE [TABLE] name [, ...] [RESTART IDENTITY
// | CONTINUE IDENTITY] [RESTRICT]; "truncate" is consumed. CASCADE is
// rejected — there are no foreign-key cascades to follow, and
// accepting the keyword would promise them.
func (p *parser) truncateStmt() (Statement, error) {
	p.acceptKw("table")
	t := &Truncate{}
	for {
		name, err := p.tableName()
		if err != nil {
			return nil, err
		}
		t.Tables = append(t.Tables, name)
		if !p.acceptOp(",") {
			break
		}
	}
	switch {
	case p.acceptKw("restart"):
		if err := p.expectKw("identity"); err != nil {
			return nil, err
		}
		t.RestartIdentity = true
	case p.acceptKw("continue"): // the default, spelled out
		if err := p.expectKw("identity"); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("cascade") {
		return nil, serr.New("TRUNCATE ... CASCADE is not supported")
	}
	p.acceptKw("restrict") // the default
	return t, nil
}

// showStmt parses SHOW name | SHOW ALL, plus the two multi-word
// parameters clients actually send: SHOW TIME ZONE and SHOW
// TRANSACTION ISOLATION LEVEL (JDBC probes the latter on connect).
func (p *parser) showStmt() (Statement, error) {
	switch {
	case p.acceptKw("all"):
		return &ShowVar{Name: "all"}, nil
	case p.acceptKw("time"):
		if err := p.expectKw("zone"); err != nil {
			return nil, err
		}
		return &ShowVar{Name: "timezone"}, nil
	case p.acceptKw("transaction"):
		if err := p.expectKw("isolation"); err != nil {
			return nil, err
		}
		if err := p.expectKw("level"); err != nil {
			return nil, err
		}
		return &ShowVar{Name: "transaction_isolation"}, nil
	}
	name, err := p.ident("a configuration parameter")
	if err != nil {
		return nil, err
	}
	return &ShowVar{Name: strings.ToLower(name)}, nil
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
		// A table-level constraint: [CONSTRAINT name] PRIMARY KEY (...)
		// or [CONSTRAINT name] CHECK (expr). Postgres has no stored name
		// for a bytdb primary key, so a name there is accepted and
		// dropped.
		cname := ""
		named := false
		if p.acceptKw("constraint") {
			if cname, err = p.ident("a constraint name"); err != nil {
				return nil, err
			}
			named = true
		}
		switch {
		case p.acceptKw("primary"):
			if err := p.expectKw("key"); err != nil {
				return nil, err
			}
			if ct.PK != nil {
				return nil, serr.New("multiple primary keys", "table", name)
			}
			if ct.PK, err = p.identList("a primary key column"); err != nil {
				return nil, err
			}
		case p.acceptKw("unique"):
			// UNIQUE (a, b): sugar for a unique index over the columns.
			// A CONSTRAINT name, like the primary key's, is accepted and
			// dropped — the index gets the conventional table_cols_key name.
			cols, err := p.identList("a unique column")
			if err != nil {
				return nil, err
			}
			ct.Uniques = append(ct.Uniques, cols)
		case p.acceptKw("foreign"):
			if err := p.expectKw("key"); err != nil {
				return nil, err
			}
			cols, err := p.identList("a foreign key column")
			if err != nil {
				return nil, err
			}
			if err := p.expectKw("references"); err != nil {
				return nil, err
			}
			fk, err := p.referencesClause(cols)
			if err != nil {
				return nil, err
			}
			fk.Name = cname
			ct.FKs = append(ct.FKs, fk)
		case p.acceptKw("check"):
			ex, text, err := p.checkExpr()
			if err != nil {
				return nil, err
			}
			ct.Checks = append(ct.Checks, CheckDef{Name: cname, Ex: ex, Text: text})
		case named:
			return nil, p.unexpected("PRIMARY KEY, UNIQUE, FOREIGN KEY, or CHECK after CONSTRAINT name")
		default:
			col, inlinePK, checks, err := p.colDef()
			if err != nil {
				return nil, err
			}
			if inlinePK {
				if ct.PK != nil {
					return nil, serr.New("multiple primary keys", "table", name)
				}
				ct.PK = []string{col.Name}
			}
			if col.Unique {
				ct.Uniques = append(ct.Uniques, []string{col.Name})
			}
			if col.Ref != nil {
				ct.FKs = append(ct.FKs, *col.Ref)
			}
			ct.Cols = append(ct.Cols, col)
			ct.Checks = append(ct.Checks, checks...)
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

// colDef parses "name type" followed by column constraints: PRIMARY
// KEY, NOT NULL, NULL, GENERATED BY DEFAULT AS IDENTITY, and
// [CONSTRAINT name] CHECK (expr), in any order. SERIAL types make the
// column identity, as in Postgres. DEFAULT, UNIQUE, REFERENCES, and
// GENERATED ALWAYS are recognized and rejected with a clear error.
func (p *parser) colDef() (ColDef, bool, []CheckDef, error) {
	fail := func(err error) (ColDef, bool, []CheckDef, error) {
		return ColDef{}, false, nil, err
	}
	name, err := p.ident("a column name")
	if err != nil {
		return fail(err)
	}
	col := ColDef{Name: name}
	switch t := p.cur(); {
	case t.kind == tIdent && serialTypes[t.text]:
		// SERIAL is Postgres sugar for an int identity column, NOT NULL
		// included.
		p.advance()
		col.Type, col.Identity, col.NotNull = bytdb.TInt, true, true
	default:
		if col.Type, col.MaxLen, err = p.typeName(); err != nil {
			return fail(err)
		}
	}
	pk := false
	var checks []CheckDef
	for {
		cname := ""
		named := false
		if p.acceptKw("constraint") {
			if cname, err = p.ident("a constraint name"); err != nil {
				return fail(err)
			}
			named = true
		}
		switch {
		case p.acceptKw("primary"):
			if err := p.expectKw("key"); err != nil {
				return fail(err)
			}
			pk = true
		case p.acceptKw("not"):
			if err := p.expectKw("null"); err != nil {
				return fail(err)
			}
			col.NotNull = true
		case p.acceptKw("null"):
			if col.Identity {
				return fail(serr.New("conflicting NULL declaration for identity column", "column", name))
			}
			col.NotNull = false
		case p.acceptKw("check"):
			ex, text, err := p.checkExpr()
			if err != nil {
				return fail(err)
			}
			checks = append(checks, CheckDef{Name: cname, Col: name, Ex: ex, Text: text})
		case p.acceptKw("generated"):
			// GENERATED BY DEFAULT AS IDENTITY. ALWAYS is rejected: it
			// forbids explicit inserts, which we don't enforce — accepting
			// it would silently weaken the declared constraint.
			byDefault := false
			switch {
			case p.acceptKw("by"):
				if err := p.expectKw("default"); err != nil {
					return fail(err)
				}
				byDefault = true
			case p.acceptKw("always"):
			default:
				return fail(p.unexpected("BY DEFAULT or ALWAYS"))
			}
			if err := p.expectKw("as"); err != nil {
				return fail(err)
			}
			if err := p.expectKw("identity"); err != nil {
				return fail(err)
			}
			if !byDefault {
				return fail(serr.New(
					"GENERATED ALWAYS AS IDENTITY is not supported; use GENERATED BY DEFAULT AS IDENTITY",
					"column", name))
			}
			if col.Type != bytdb.TInt {
				return fail(serr.New("identity column must be an int column", "column", name))
			}
			if col.HasDefault {
				return fail(serr.New("conflicting DEFAULT for identity column", "column", name))
			}
			col.Identity, col.NotNull = true, true
		case p.acceptKw("default"):
			if col.Identity {
				return fail(serr.New("conflicting DEFAULT for identity column", "column", name))
			}
			v, err := p.defaultLiteral(name)
			if err != nil {
				return fail(err)
			}
			if v != nil { // DEFAULT NULL declares the absence of a default
				col.Default, col.HasDefault = v, true
			}
		case p.acceptKw("unique"):
			// Sugar for a single-column unique index (a CONSTRAINT name,
			// as with PRIMARY KEY, is accepted and dropped — the index
			// gets the conventional table_col_key name).
			col.Unique = true
		case p.acceptKw("references"):
			// A column-level foreign key; the CONSTRAINT name, if any,
			// names the constraint, as in Postgres.
			fk, err := p.referencesClause([]string{name})
			if err != nil {
				return fail(err)
			}
			fk.Name = cname
			col.Ref = &fk
		case named:
			return fail(p.unexpected("CHECK after CONSTRAINT name"))
		default:
			return col, pk, checks, nil
		}
	}
}

// referencesClause parses the tail of a foreign-key declaration after
// REFERENCES: the parent table, an optional referenced-column list
// (absent: the parent's primary key), and the MATCH / ON DELETE / ON
// UPDATE options. Only NO ACTION and RESTRICT actions are accepted —
// both mean "refuse", which is the only semantics bytdb implements;
// CASCADE and SET NULL/DEFAULT are rejected rather than silently
// weakened, and MATCH FULL/PARTIAL likewise (MATCH SIMPLE is the
// implemented default: any NULL child column satisfies the FK).
func (p *parser) referencesClause(cols []string) (FKDef, error) {
	table, err := p.tableName()
	if err != nil {
		return FKDef{}, err
	}
	fk := FKDef{Cols: cols, RefTable: table}
	if p.cur().kind == tOp && p.cur().text == "(" {
		if fk.RefCols, err = p.identList("a referenced column"); err != nil {
			return FKDef{}, err
		}
	}
	for {
		switch {
		case p.acceptKw("match"):
			if !p.acceptKw("simple") {
				return FKDef{}, serr.New("only MATCH SIMPLE foreign keys are supported")
			}
		case p.acceptKw("on"):
			action := "DELETE"
			if !p.acceptKw("delete") {
				if err := p.expectKw("update"); err != nil {
					return FKDef{}, err
				}
				action = "UPDATE"
			}
			switch {
			case p.acceptKw("no"):
				if err := p.expectKw("action"); err != nil {
					return FKDef{}, err
				}
			case p.acceptKw("restrict"):
			case p.acceptKw("cascade"):
				return FKDef{}, serr.New("ON " + action + " CASCADE is not supported" +
					"; only NO ACTION/RESTRICT foreign keys exist")
			case p.acceptKw("set"):
				return FKDef{}, serr.New("ON " + action + " SET NULL/DEFAULT is not supported" +
					"; only NO ACTION/RESTRICT foreign keys exist")
			default:
				return FKDef{}, p.unexpected("NO ACTION or RESTRICT")
			}
		default:
			return fk, nil
		}
	}
}

// defaultLiteral parses a column DEFAULT: a constant, or one of the
// clock functions (now(), CURRENT_TIMESTAMP, CURRENT_DATE, ...),
// which parse to ExprDefault markers the insert path evaluates per
// statement. The clock functions are the only evaluated defaults —
// general expressions stay rejected, so a default is still either a
// stored constant or a marker, never an expression tree.
func (p *parser) defaultLiteral(col string) (any, error) {
	if t := p.cur(); t.kind == tIdent {
		// All timestamp-valued spellings normalize to one marker, the
		// way Postgres normalizes CURRENT_TIMESTAMP to now(). The
		// keyword forms take no parens; the function forms require
		// them (a bare `now` is a column reference in Postgres too).
		switch t.text {
		case "now", "transaction_timestamp", "statement_timestamp", "clock_timestamp":
			p.advance()
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return DefaultNow, nil
		case "current_timestamp", "localtimestamp":
			p.advance()
			return DefaultNow, nil
		case "current_date":
			p.advance()
			return DefaultCurrentDate, nil
		case "current_time", "localtime":
			return nil, serr.New(
				"DEFAULT "+t.text+" is not supported: no time-of-day type exists"+
					"; use a timestamp column with DEFAULT now()", "column", col)
		}
	}
	if p.cur().kind == tParam {
		return nil, serr.New("placeholders are not allowed in DEFAULT", "column", col)
	}
	v, err := p.literal()
	if err != nil {
		return nil, serr.New("DEFAULT must be a constant", "column", col)
	}
	return v, nil
}

// renderLit renders a constant as the SQL literal text a descriptor
// stores (DEFAULT values), the inverse of parseStoredLiteral.
func renderLit(v any) string {
	switch n := v.(type) {
	case int64:
		return strconv.FormatInt(n, 10)
	case float64:
		return strconv.FormatFloat(n, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(n)
	case string:
		return "'" + strings.ReplaceAll(n, "'", "''") + "'"
	}
	return "null"
}

// parseStoredLiteral reads a descriptor-stored literal back to its
// value.
func parseStoredLiteral(src string) (any, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, src: src}
	v, err := p.literal()
	if err != nil {
		return nil, err
	}
	if p.cur().kind != tEOF {
		return nil, serr.New("trailing tokens in stored literal", "text", src)
	}
	return v, nil
}

// checkExpr parses CHECK's "( expr )", returning both the parsed
// expression and its verbatim source text (what the table descriptor
// stores).
func (p *parser) checkExpr() (Expr, string, error) {
	if err := p.expectOp("("); err != nil {
		return nil, "", err
	}
	start := p.cur().pos
	ex, err := p.expression()
	if err != nil {
		return nil, "", err
	}
	end := p.cur().pos // the closing ")"
	if err := p.expectOp(")"); err != nil {
		return nil, "", err
	}
	return ex, strings.TrimSpace(p.src[start:end]), nil
}

// serialTypes are the Postgres serial pseudo-types: an int column
// that is identity and NOT NULL. Only valid in a column definition,
// so colDef handles them before typeName.
var serialTypes = map[string]bool{
	"serial": true, "bigserial": true, "smallserial": true,
	"serial2": true, "serial4": true, "serial8": true,
}

// typeName parses a column type, accepting common Postgres aliases,
// then an optional [] array suffix. Only text arrays exist (they ride
// on a string representation; see bytdb.TTextArray), so the suffix is
// legal on the text/varchar family alone and rejected elsewhere rather
// than parsing into a type the engine would refuse later.
func (p *parser) typeName() (typ bytdb.ColType, maxLen int, err error) {
	if typ, maxLen, err = p.typeNameBase(); err != nil {
		return "", 0, err
	}
	if !p.acceptOp("[") {
		return typ, maxLen, nil
	}
	if err = p.expectOp("]"); err != nil {
		return "", 0, err
	}
	if typ != bytdb.TString {
		return "", 0, serr.New("only text[] arrays are supported",
			"type", string(typ)+"[]")
	}
	if maxLen != 0 {
		// A per-element length limit would need enforcement inside the
		// stored literal; refuse rather than silently drop the limit.
		return "", 0, serr.New("varchar(n)[] is not supported; use text[]")
	}
	return bytdb.TTextArray, 0, nil
}

// typeNameBase parses the scalar type name itself. maxLen is a
// VARCHAR(n) limit (0: none), enforced on writes; it was formerly
// parsed and dropped, which silently accepted schemas whose declared
// limits meant nothing.
func (p *parser) typeNameBase() (typ bytdb.ColType, maxLen int, err error) {
	t := p.cur()
	if t.kind != tIdent {
		return "", 0, p.unexpected("a column type")
	}
	p.advance()
	switch t.text {
	case "int", "integer", "bigint", "int8", "int4", "int2", "smallint":
		return bytdb.TInt, 0, nil
	case "float", "float4", "float8", "real":
		return bytdb.TFloat, 0, nil
	case "double":
		p.acceptKw("precision")
		return bytdb.TFloat, 0, nil
	case "text", "string":
		return bytdb.TString, 0, nil
	case "character":
		// CHARACTER VARYING(n) is varchar; blank-padded CHAR(n) is not
		// supported (padding on read would need its own type).
		if !p.acceptKw("varying") {
			return "", 0, serr.New("char(n) is not supported; use varchar(n) or text",
				"pos", fmt.Sprint(t.pos))
		}
		fallthrough
	case "varchar":
		if maxLen, err = p.typeLength(); err != nil {
			return "", 0, err
		}
		return bytdb.TString, maxLen, nil
	case "char":
		return "", 0, serr.New("char(n) is not supported; use varchar(n) or text",
			"pos", fmt.Sprint(t.pos))
	case "bool", "boolean":
		return bytdb.TBool, 0, nil
	case "bytea", "bytes":
		return bytdb.TBytes, 0, nil
	case "timestamp", "timestamptz":
		// An optional (p) precision parses and is ignored — storage is
		// always microseconds, Postgres's own maximum precision.
		if p.acceptOp("(") {
			if p.cur().kind != tNumber {
				return "", 0, p.unexpected("a precision")
			}
			p.advance()
			if err := p.expectOp(")"); err != nil {
				return "", 0, err
			}
		}
		// TIMESTAMP [WITH | WITHOUT TIME ZONE]: both store UTC instants
		// here, so the distinction parses and folds away.
		if p.acceptKw("with") || p.acceptKw("without") {
			if err := p.expectKw("time"); err != nil {
				return "", 0, err
			}
			if err := p.expectKw("zone"); err != nil {
				return "", 0, err
			}
		}
		return bytdb.TTimestamp, 0, nil
	case "date":
		return bytdb.TDate, 0, nil
	case "uuid":
		return bytdb.TUUID, 0, nil
	case "json", "jsonb":
		// Both names land on TJSONB, with jsonb semantics: documents
		// canonicalize on write (key order and whitespace vanish).
		// Postgres's json type preserves the source text verbatim —
		// a distinction not worth a second type here, and jsonb is
		// what schemas mean when they reach for either.
		return bytdb.TJSONB, 0, nil
	case "time":
		return "", 0, serr.New("the time-of-day type is not supported; use timestamp",
			"pos", fmt.Sprint(t.pos))
	}
	return "", 0, serr.New("unknown column type", "type", t.text, "pos", fmt.Sprint(t.pos))
}

// typeLength parses an optional "(n)" type modifier; n must be a
// positive integer (varchar(0) admits nothing, as Postgres also
// rejects). Absent modifier reads as 0: unbounded.
func (p *parser) typeLength() (int, error) {
	if !p.acceptOp("(") {
		return 0, nil
	}
	t := p.cur()
	if t.kind != tNumber {
		return 0, p.unexpected("a length")
	}
	n, err := strconv.Atoi(t.text)
	if err != nil || n < 1 {
		return 0, serr.New("length for type varchar must be at least 1", "got", t.text)
	}
	p.advance()
	if err := p.expectOp(")"); err != nil {
		return 0, err
	}
	return n, nil
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
	ci := &CreateIndex{Name: name, Table: table, Unique: unique}
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	anyDesc := false
	for {
		col, err := p.ident("an indexed column")
		if err != nil {
			return nil, err
		}
		desc := false
		if p.acceptKw("desc") {
			desc = true
		} else {
			p.acceptKw("asc")
		}
		if p.acceptKw("nulls") {
			return nil, serr.New("NULLS FIRST/LAST is not supported",
				"hint", "ascending columns put NULLs first, descending columns last")
		}
		ci.Cols = append(ci.Cols, col)
		ci.Desc = append(ci.Desc, desc)
		anyDesc = anyDesc || desc
		if !p.acceptOp(",") {
			break
		}
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	if !anyDesc {
		ci.Desc = nil
	}
	return ci, nil
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
		// A table constraint: [CONSTRAINT name] CHECK (expr). Other
		// constraint kinds are recognized and rejected with a pointer
		// to what bytdb offers instead.
		cname := ""
		named := false
		if p.acceptKw("constraint") {
			if cname, err = p.ident("a constraint name"); err != nil {
				return nil, err
			}
			named = true
		}
		switch {
		case p.acceptKw("check"):
			ex, text, err := p.checkExpr()
			if err != nil {
				return nil, err
			}
			return &AddConstraint{Table: table, Check: CheckDef{Name: cname, Ex: ex, Text: text}}, nil
		case p.acceptKw("primary"):
			return nil, serr.New("ADD PRIMARY KEY is not supported", "table", table)
		case p.acceptKw("unique"):
			return nil, serr.New("ADD UNIQUE is not supported; use CREATE UNIQUE INDEX",
				"table", table)
		case p.acceptKw("foreign"):
			if err := p.expectKw("key"); err != nil {
				return nil, err
			}
			cols, err := p.identList("a foreign key column")
			if err != nil {
				return nil, err
			}
			if err := p.expectKw("references"); err != nil {
				return nil, err
			}
			fk, err := p.referencesClause(cols)
			if err != nil {
				return nil, err
			}
			fk.Name = cname
			return &AddFK{Table: table, FK: fk}, nil
		case named:
			return nil, p.unexpected("CHECK after CONSTRAINT name")
		}
		p.acceptKw("column")
		col, pk, checks, err := p.colDef()
		if err != nil {
			return nil, err
		}
		if pk {
			return nil, serr.New("cannot add a primary key column", "table", table, "column", col.Name)
		}
		if len(checks) > 0 {
			return nil, serr.New("ADD COLUMN with a CHECK constraint is not supported",
				"table", table, "column", col.Name)
		}
		return &AddColumn{Table: table, Col: col}, nil
	case p.acceptKw("rename"):
		// RENAME TO t | RENAME [COLUMN] c TO d.
		if p.acceptKw("to") {
			to, err := p.tableName()
			if err != nil {
				return nil, err
			}
			return &RenameTable{Table: table, To: to}, nil
		}
		p.acceptKw("column")
		col, err := p.ident("a column name")
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("to"); err != nil {
			return nil, err
		}
		to, err := p.ident("the new column name")
		if err != nil {
			return nil, err
		}
		return &RenameColumn{Table: table, Col: col, To: to}, nil
	case p.acceptKw("drop"):
		if p.acceptKw("constraint") {
			dc := &DropConstraint{Table: table}
			if p.acceptKw("if") {
				if err := p.expectKw("exists"); err != nil {
					return nil, err
				}
				dc.IfExists = true
			}
			if dc.Name, err = p.ident("a constraint name"); err != nil {
				return nil, err
			}
			return dc, nil
		}
		p.acceptKw("column")
		name, err := p.ident("a column name")
		if err != nil {
			return nil, err
		}
		return &DropColumn{Table: table, Col: name}, nil
	}
	return nil, p.unexpected("ADD, DROP, or RENAME")
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
	if p.acceptKw("default") {
		// INSERT INTO t DEFAULT VALUES: one row, every column its
		// default (NULL without one) — modeled as an empty column list
		// with an empty row, which the executor already fills.
		if err := p.expectKw("values"); err != nil {
			return nil, err
		}
		ins.Cols, ins.Rows = []string{}, [][]any{{}}
	} else {
		if err := p.expectKw("values"); err != nil {
			return nil, err
		}
		for {
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			var row []any
			for {
				// DEFAULT as a value slots the column's default in at
				// execution (NULL without one), as in Postgres. It must be
				// checked before the expression grammar, which would read
				// the keyword as a column reference.
				if t := p.cur(); t.kind == tIdent && t.text == "default" {
					p.advance()
					row = append(row, defaultMarker{})
				} else {
					v, err := p.insertValue()
					if err != nil {
						return nil, err
					}
					row = append(row, v)
				}
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
	}
	if ins.Conflict, err = p.onConflictClause(table); err != nil {
		return nil, err
	}
	if err := p.returningClause(&ins.Returning); err != nil {
		return nil, err
	}
	return ins, nil
}

// insertValue parses one VALUES entry through the expression grammar.
// A bare literal (or $n placeholder) unwraps to its parsed value —
// the historical representation, which the binding, describe, and
// engine-coercion paths key off — while anything richer stays an Expr
// for per-row evaluation. Aggregates and windows have no row set to
// compute over in VALUES; Postgres rejects them here too.
func (p *parser) insertValue() (any, error) {
	ex, err := p.expression()
	if err != nil {
		return nil, err
	}
	if findAgg(ex) != AggNone {
		return nil, serr.New("aggregate functions are not allowed in VALUES")
	}
	if hasWindowExpr(ex) {
		return nil, serr.New("window functions are not allowed in VALUES")
	}
	if lit, ok := ex.(*ExLit); ok {
		return lit.Val, nil
	}
	return ex, nil
}

// onConflictClause parses an optional ON CONFLICT [(col, ...)]
// DO NOTHING | DO UPDATE SET ... [WHERE cond]; nil when absent.
// Unqualified column references in the DO UPDATE assignments and
// WHERE are qualified with the target table here, at parse time: at
// execution they resolve against a two-table scope (the table plus
// the excluded pseudo-table, both the same descriptor), where a bare
// name would otherwise be ambiguous — Postgres reads bare names as
// the target row and requires excluded to be spelled out.
func (p *parser) onConflictClause(table string) (*OnConflict, error) {
	if !p.acceptKw("on") {
		return nil, nil
	}
	if err := p.expectKw("conflict"); err != nil {
		return nil, err
	}
	oc := &OnConflict{}
	if p.cur().kind == tOp && p.cur().text == "(" {
		cols, err := p.identList("a column name")
		if err != nil {
			return nil, err
		}
		oc.TargetCols = cols
	}
	if p.acceptKw("on") {
		return nil, serr.New("ON CONFLICT ON CONSTRAINT is not supported; name the conflict columns instead")
	}
	if p.acceptKw("where") {
		return nil, serr.New("ON CONFLICT with an index predicate is not supported (bytdb has no partial indexes)")
	}
	if err := p.expectKw("do"); err != nil {
		return nil, err
	}
	switch {
	case p.acceptKw("nothing"):
		return oc, nil
	case p.acceptKw("update"):
		if oc.TargetCols == nil {
			// Postgres's rule and wording: without a target there is no
			// way to know which existing row to update.
			return nil, serr.New("ON CONFLICT DO UPDATE requires the conflict target columns")
		}
		if err := p.expectKw("set"); err != nil {
			return nil, err
		}
		oc.Update = true
		var err error
		if oc.Set, oc.SetEx, err = p.setClause("ON CONFLICT DO UPDATE"); err != nil {
			return nil, err
		}
		if p.acceptKw("where") {
			if oc.Where, err = p.boolExpr(false); err != nil {
				return nil, err
			}
		}
		for _, ex := range oc.SetEx {
			qualifyExprCols(ex, table)
		}
		qualifyBoolCols(oc.Where, table)
		return oc, nil
	}
	return nil, p.unexpected("NOTHING or UPDATE after DO")
}

// qualifyExprCols stamps the table qualifier on every bare column
// reference in a freshly parsed expression, in place. Subqueries are
// left alone (walkExpr treats *ExSub as a leaf): their columns must
// resolve against their own FROM first, and genuinely correlated
// references still reach the outer scope through the environment
// chain at evaluation.
func qualifyExprCols(e Expr, table string) {
	walkExpr(e, func(sub Expr) bool {
		if c, ok := sub.(*ExCol); ok && c.Col.Table == "" {
			c.Col.Table = table
		}
		return true
	})
}

// qualifyBoolCols is qualifyExprCols over a condition tree: predicate
// operands and expression leaves alike.
func qualifyBoolCols(e BoolExpr, table string) {
	walkPreds(e, func(pr *Pred) error {
		if pr.Item.Col.Name != "" && pr.Item.Col.Table == "" {
			pr.Item.Col.Table = table
		}
		if pr.RItem != nil && pr.RItem.Col.Name != "" && pr.RItem.Col.Table == "" {
			pr.RItem.Col.Table = table
		}
		return nil
	})
	qualifyCondCols(e, table)
}

// qualifyCondCols descends to the Cond leaves walkPreds skips.
func qualifyCondCols(e BoolExpr, table string) {
	switch n := e.(type) {
	case *Cond:
		qualifyExprCols(n.Ex, table)
	case *Not:
		qualifyCondCols(n.Expr, table)
	case *And:
		for _, sub := range n.Exprs {
			qualifyCondCols(sub, table)
		}
	case *Or:
		for _, sub := range n.Exprs {
			qualifyCondCols(sub, table)
		}
	}
}

// returningClause parses an optional RETURNING * | item, ... into ret.
// Items are full select-list entries (expressions, aliases, t.*), but
// aggregates and window functions are rejected here as in Postgres:
// there is no row set to aggregate over — each affected row yields
// exactly one output row.
func (p *parser) returningClause(ret *Returning) error {
	if !p.acceptKw("returning") {
		return nil
	}
	if p.acceptOp("*") {
		ret.RetStar = true
		return nil
	}
	for {
		item, err := p.selectListItem()
		if err != nil {
			return err
		}
		// The lowered legacy shape (COUNT(*) → item.Agg) and aggregates
		// buried in expressions both count.
		if item.Agg != AggNone || (item.Ex != nil && findAgg(item.Ex) != AggNone) {
			return serr.New("aggregate functions are not allowed in RETURNING")
		}
		if item.Ex != nil && hasWindowExpr(item.Ex) {
			return serr.New("window functions are not allowed in RETURNING")
		}
		ret.RetItems = append(ret.RetItems, item)
		if !p.acceptOp(",") {
			return nil
		}
	}
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
	// SELECT [ALL | DISTINCT]: ALL is the explicit spelling of the
	// default. DISTINCT ON (...) — Postgres's keep-first-per-group form
	// — is a different feature (it needs ORDER BY-driven row choice,
	// not a dedup) and is rejected outright rather than misread.
	if p.acceptKw("distinct") {
		if p.acceptKw("on") {
			return nil, serr.New("SELECT DISTINCT ON is not supported")
		}
		s.Distinct = true
	} else {
		p.acceptKw("all")
	}
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
	u := &Update{Table: table}
	if u.Set, u.SetEx, err = p.setClause("UPDATE"); err != nil {
		return nil, err
	}
	if p.acceptKw("where") {
		if u.Where, err = p.boolExpr(false); err != nil {
			return nil, err
		}
	}
	if err := p.returningClause(&u.Returning); err != nil {
		return nil, err
	}
	return u, nil
}

// setClause parses SET col = expr, ... into the literal / expression
// assignment split Update documents (shared by UPDATE and ON CONFLICT
// DO UPDATE). stmt names the statement in rejection errors.
func (p *parser) setClause(stmt string) (map[string]any, map[string]Expr, error) {
	set := map[string]any{}
	setEx := map[string]Expr{}
	for {
		col, err := p.ident("a column name")
		if err != nil {
			return nil, nil, err
		}
		if err := p.expectOp("="); err != nil {
			return nil, nil, err
		}
		ex, err := p.expression()
		if err != nil {
			return nil, nil, err
		}
		// Aggregates and windows have no row set to aggregate over in an
		// UPDATE; Postgres rejects them here too.
		if findAgg(ex) != AggNone {
			return nil, nil, serr.New("aggregate functions are not allowed in " + stmt)
		}
		if hasWindowExpr(ex) {
			return nil, nil, serr.New("window functions are not allowed in " + stmt)
		}
		if _, dup := set[col]; dup {
			return nil, nil, serr.New("duplicate column in SET", "column", col)
		}
		if _, dup := setEx[col]; dup {
			return nil, nil, serr.New("duplicate column in SET", "column", col)
		}
		// A bare literal (or $n placeholder) keeps the pre-expression
		// representation: Set values coerce once per statement and skip
		// per-row evaluation. Everything else evaluates per row.
		if lit, ok := ex.(*ExLit); ok {
			set[col] = lit.Val
		} else {
			setEx[col] = ex
		}
		if !p.acceptOp(",") {
			return set, setEx, nil
		}
	}
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
	if err := p.returningClause(&d.Returning); err != nil {
		return nil, err
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
// createSequence parses CREATE SEQUENCE [IF NOT EXISTS] name
// [options]; "create sequence" is consumed.
func (p *parser) createSequence() (Statement, error) {
	s := &CreateSequence{}
	if p.acceptKw("if") {
		if err := p.expectKw("not"); err != nil {
			return nil, err
		}
		if err := p.expectKw("exists"); err != nil {
			return nil, err
		}
		s.IfNotExists = true
	}
	var err error
	if s.Name, err = p.tableName(); err != nil {
		return nil, err
	}
	s.Opts, err = p.seqOptions(false)
	return s, err
}

// dropSequence parses DROP SEQUENCE [IF EXISTS] name [RESTRICT];
// "drop sequence" is consumed. CASCADE is rejected — nothing can
// depend on a bytdb sequence yet, so accepting it would be a lie.
func (p *parser) dropSequence() (Statement, error) {
	s := &DropSequence{}
	if p.acceptKw("if") {
		if err := p.expectKw("exists"); err != nil {
			return nil, err
		}
		s.IfExists = true
	}
	var err error
	if s.Name, err = p.tableName(); err != nil {
		return nil, err
	}
	if p.acceptKw("cascade") {
		return nil, serr.New("DROP SEQUENCE ... CASCADE is not supported")
	}
	p.acceptKw("restrict") // the default
	return s, nil
}

// alterSequence parses ALTER SEQUENCE name options; "alter sequence"
// is consumed. At least one option is required, as in Postgres.
func (p *parser) alterSequence() (Statement, error) {
	s := &AlterSequence{}
	var err error
	if s.Name, err = p.tableName(); err != nil {
		return nil, err
	}
	if s.Opts, err = p.seqOptions(true); err != nil {
		return nil, err
	}
	empty := SeqOptions{}
	if s.Opts == empty {
		return nil, p.unexpected("a sequence option")
	}
	return s, nil
}

// seqOptions parses the CREATE/ALTER SEQUENCE option list. alter
// additionally admits RESTART [[WITH] n]. Unmentioned options stay
// nil pointers, so ALTER can tell "keep" from "set".
func (p *parser) seqOptions(alter bool) (SeqOptions, error) {
	var o SeqOptions
	set := func(dst **int64, clause string) error {
		n, err := p.signedInt(clause)
		if err != nil {
			return err
		}
		*dst = &n
		return nil
	}
	for {
		switch {
		case p.acceptKw("as"):
			t, err := p.ident("a sequence data type")
			if err != nil {
				return o, err
			}
			switch t {
			case "smallint", "int2":
				o.AsType = "smallint"
			case "int", "integer", "int4":
				o.AsType = "integer"
			case "bigint", "int8":
				o.AsType = "bigint"
			default:
				return o, serr.New("sequence type must be smallint, integer, or bigint",
					"type", t)
			}
		case p.acceptKw("increment"):
			p.acceptKw("by")
			if err := set(&o.Increment, "INCREMENT"); err != nil {
				return o, err
			}
		case p.acceptKw("minvalue"):
			if err := set(&o.Min, "MINVALUE"); err != nil {
				return o, err
			}
		case p.acceptKw("maxvalue"):
			if err := set(&o.Max, "MAXVALUE"); err != nil {
				return o, err
			}
		case p.acceptKw("start"):
			p.acceptKw("with")
			if err := set(&o.Start, "START"); err != nil {
				return o, err
			}
		case p.acceptKw("cache"):
			if err := set(&o.Cache, "CACHE"); err != nil {
				return o, err
			}
		case p.acceptKw("cycle"):
			t := true
			o.Cycle = &t
		case p.acceptKw("no"):
			switch {
			case p.acceptKw("minvalue"):
				o.NoMin = true
			case p.acceptKw("maxvalue"):
				o.NoMax = true
			case p.acceptKw("cycle"):
				f := false
				o.Cycle = &f
			default:
				return o, p.unexpected("MINVALUE, MAXVALUE, or CYCLE")
			}
		case alter && p.acceptKw("restart"):
			o.Restart = true
			// RESTART [[WITH] n]: bare RESTART reuses START.
			if p.acceptKw("with") {
				if err := set(&o.RestartWith, "RESTART"); err != nil {
					return o, err
				}
			} else if t := p.cur(); t.kind == tNumber || (t.kind == tOp && t.text == "-") {
				if err := set(&o.RestartWith, "RESTART"); err != nil {
					return o, err
				}
			}
		case p.acceptKw("owned"):
			if err := p.expectKw("by"); err != nil {
				return o, err
			}
			// OWNED BY NONE is the default; column ownership would tie
			// the sequence's life to a table bytdb doesn't track yet.
			if !p.acceptKw("none") {
				return o, serr.New("OWNED BY a column is not supported")
			}
		default:
			return o, nil
		}
	}
}

// signedInt parses an optionally negated integer literal for a
// sequence option.
func (p *parser) signedInt(clause string) (int64, error) {
	neg := p.acceptOp("-")
	t := p.cur()
	if t.kind != tNumber {
		return 0, p.unexpected("an integer after " + clause)
	}
	n, err := strconv.ParseInt(t.text, 10, 64)
	if err != nil {
		return 0, serr.New(clause+" requires an integer", "got", t.text)
	}
	p.advance()
	if neg {
		n = -n
	}
	return n, nil
}

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

// tableRef parses "t", "t alias", "t AS alias", or a derived table
// "(SELECT ...) [AS] alias" — which lowers to a synthetic CTE (see
// parser.ctes): the subquery materializes once and joins like any
// virtual table. A function call in FROM (generate_series(...) s)
// parses too — psql writes them inside subqueries that never run —
// but errors if its scan is ever built.
func (p *parser) tableRef() (FromItem, error) {
	if t := p.cur(); t.kind == tOp && t.text == "(" {
		p.advance()
		if err := p.expectKw("select"); err != nil {
			return FromItem{}, err
		}
		st, err := p.selectStmt()
		if err != nil {
			return FromItem{}, err
		}
		if err := p.expectOp(")"); err != nil {
			return FromItem{}, err
		}
		it := FromItem{}
		if p.acceptKw("as") {
			if it.Alias, err = p.ident("an alias"); err != nil {
				return FromItem{}, err
			}
		} else if t := p.cur(); t.kind == tQIdent || (t.kind == tIdent && !noAlias[t.text]) {
			it.Alias, _ = p.ident("an alias")
		}
		if it.Alias == "" {
			// Postgres's rule and wording: without a name there is no way
			// to reference the derived columns.
			return FromItem{}, serr.New("subquery in FROM must have an alias")
		}
		p.nDerived++
		it.Table = fmt.Sprintf("*derived*%d", p.nDerived) // '*' cannot appear in identifiers
		p.ctes = append(p.ctes, CTE{Name: it.Table, Sel: st.(*Select)})
		return it, nil
	}
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
		if n.Arg != nil || n.Distinct { // beyond the legacy shape: stays an expression
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
		if n.Arg == nil && !n.Distinct { // beyond the legacy shape: stays an expression
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

// hasWindowExpr reports a window call anywhere in an expression, not
// descending into subqueries (their windows are their own).
func hasWindowExpr(e Expr) bool {
	found := false
	walkExpr(e, func(sub Expr) bool {
		if _, isSub := sub.(*ExSub); isSub {
			return false
		}
		if _, ok := sub.(*ExWindow); ok {
			found = true
		}
		return !found
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
//
// Every nesting construct — parens, subqueries, CASE, function and
// list arguments — re-enters through here, so this single depth guard
// bounds them all. The one exception is NOT, which recurses inside
// exprNot without coming back through expression; exprNot guards
// itself.
func (p *parser) expression() (Expr, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	return p.exprOr()
}

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
		// NOT chains recurse here directly, bypassing expression's
		// depth guard, so each NOT counts its own level.
		if err := p.enter(); err != nil {
			return nil, err
		}
		defer p.leave()
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
			r, err := p.inTail(e, false)
			if err != nil {
				return nil, err
			}
			e = r
		case p.cur().kind == tIdent && p.cur().text == "not" &&
			p.tokAt(1).kind == tIdent && p.tokAt(1).text == "in":
			p.advance()
			p.advance()
			r, err := p.inTail(e, true)
			if err != nil {
				return nil, err
			}
			e = r
		case p.cur().kind == tIdent && (p.cur().text == "like" || p.cur().text == "ilike"):
			ilike := p.cur().text == "ilike"
			p.advance()
			r, err := p.likeTail(e, ilike, false)
			if err != nil {
				return nil, err
			}
			e = r
		case p.cur().kind == tIdent && p.cur().text == "not" &&
			p.tokAt(1).kind == tIdent && (p.tokAt(1).text == "like" || p.tokAt(1).text == "ilike"):
			ilike := p.tokAt(1).text == "ilike"
			p.advance()
			p.advance()
			r, err := p.likeTail(e, ilike, true)
			if err != nil {
				return nil, err
			}
			e = r
		default:
			op, ok, err := p.cmpOp()
			if err != nil {
				return nil, err
			}
			if !ok {
				return e, nil
			}
			if t := p.cur(); t.kind == tIdent && (t.text == "any" || t.text == "all") && p.peekOp("(") {
				all := t.text == "all"
				p.advance() // any / all
				p.advance() // (
				var r Expr
				if c := p.cur(); c.kind == tIdent && c.text == "select" {
					p.advance()
					st, err := p.selectStmt()
					if err != nil {
						return nil, err
					}
					r = &ExSub{Sel: st.(*Select)}
				} else {
					var err error
					if r, err = p.expression(); err != nil {
						return nil, err
					}
				}
				if err := p.expectOp(")"); err != nil {
					return nil, err
				}
				e = &ExAny{Op: op, L: e, R: r, All: all}
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

// inTail parses what follows [NOT] IN: a value list, or a subquery —
// which lowers to the ANY machinery (e IN (SELECT ...) is exactly
// e = ANY (SELECT ...), and NOT IN is its three-valued negation).
func (p *parser) inTail(e Expr, not bool) (Expr, error) {
	if p.cur().kind == tOp && p.cur().text == "(" &&
		p.tokAt(1).kind == tIdent && p.tokAt(1).text == "select" {
		p.advance() // (
		p.advance() // select
		st, err := p.selectStmt()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		var out Expr = &ExAny{Op: OpEQ, L: e, R: &ExSub{Sel: st.(*Select)}}
		if not {
			out = &ExNot{E: out}
		}
		return out, nil
	}
	list, err := p.exprList()
	if err != nil {
		return nil, err
	}
	return &ExIn{E: e, List: list, Not: not}, nil
}

// likeTail parses what follows [NOT] LIKE / [NOT] ILIKE: the pattern
// (an additive expression, so `col LIKE '%' || $1 || '%'` composes)
// and an optional ESCAPE clause. Only the default backslash escape is
// implemented; a different character is rejected rather than silently
// matching with the wrong escape semantics.
func (p *parser) likeTail(e Expr, ilike, not bool) (Expr, error) {
	r, err := p.exprAdd()
	if err != nil {
		return nil, err
	}
	if p.acceptKw("escape") {
		t := p.cur()
		if t.kind != tString {
			return nil, p.unexpected("a quoted escape character")
		}
		p.advance()
		if t.text != `\` {
			return nil, serr.New("only the default backslash ESCAPE character is supported",
				"escape", t.text)
		}
	}
	op := OpLike
	switch {
	case ilike && not:
		op = OpNotILike
	case ilike:
		op = OpILike
	case not:
		op = OpNotLike
	}
	return &ExCmp{Op: op, L: e, R: r}, nil
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

// exprAdd parses the additive level, which also hosts || and the
// jsonb accessors (-> ->> #> #>>) — Postgres puts every "other"
// operator between + - and the comparisons, and one shared level keeps
// chains like j->'a'->>'b' and j->'a' || j->'b' left-associative.
func (p *parser) exprAdd() (Expr, error) {
	e, err := p.exprMul()
	if err != nil {
		return nil, err
	}
	for {
		t := p.cur()
		if t.kind != tOp {
			return e, nil
		}
		switch t.text {
		case "+", "-", "||", "->", "->>", "#>", "#>>":
		default:
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
	// The parenthesis-free SQL date/time keywords, lowered to their
	// function forms (now() and current_date) before the column-ref
	// fallback would misread them as column names.
	if t.kind == tIdent && !p.peekOp("(") {
		switch t.text {
		case "current_timestamp", "localtimestamp":
			p.advance()
			return &ExFunc{Name: "now"}, nil
		case "current_date":
			p.advance()
			return &ExFunc{Name: "current_date"}, nil
		}
	}
	if t.kind == tIdent && t.text == "array" && p.peekOp("[") {
		p.advance() // array
		p.advance() // [
		arr := &ExArray{}
		if !p.acceptOp("]") {
			for {
				el, err := p.expression()
				if err != nil {
					return nil, err
				}
				arr.Elems = append(arr.Elems, el)
				if !p.acceptOp(",") {
					break
				}
			}
			if err := p.expectOp("]"); err != nil {
				return nil, err
			}
		}
		return arr, nil
	}
	if t.kind == tIdent && aggNames[t.text] != AggNone && p.peekOp("(") {
		fn := aggNames[t.text]
		p.advance() // function name
		p.advance() // (
		agg := &ExAgg{Fn: fn}
		if p.acceptKw("distinct") {
			agg.Distinct = true
		} else {
			p.acceptKw("all") // the default
		}
		if fn == AggCount && !agg.Distinct && p.acceptOp("*") {
			agg.Star = true
		} else {
			e, err := p.expression()
			if err != nil {
				return nil, err
			}
			if c, ok := e.(*ExCol); ok {
				agg.Col = c.Col
			} else {
				agg.Arg = e
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		if p.cur().kind == tIdent && p.cur().text == "over" {
			if agg.Arg != nil && hasWindowExpr(agg.Arg) {
				return nil, serr.New("window function calls cannot be nested")
			}
			// An aggregate directly inside a window aggregate is legal —
			// SUM(SUM(x)) OVER (...) aggregates the groups' sums — so the
			// nesting check waits until OVER is ruled out. (Deeper
			// nesting inside that inner aggregate is caught at resolve.)
			// DISTINCT in an aggregate window dedups within each row's
			// frame — a bytdb extension (Postgres does not implement it;
			// DuckDB does).
			w := &ExWindow{Agg: agg.Fn, Arg: agg.Arg, Star: agg.Star, Distinct: agg.Distinct}
			if agg.Arg == nil && !agg.Star {
				w.Arg = &ExCol{Col: agg.Col}
			}
			return p.windowOver(w)
		}
		if agg.Arg != nil && findAgg(agg.Arg) != AggNone {
			return nil, serr.New("aggregate function calls cannot be nested")
		}
		return agg, nil
	}
	// Window-only functions: the ranking family (row_number/rank/
	// dense_rank, no arguments) and the value family (lag/lead/
	// first_value/last_value/nth_value, which surface another row of
	// the partition). Both require an OVER clause; winArity validates
	// argument counts here so the executor can trust the AST shape.
	if t.kind == tIdent && winNames[t.text] != WinNone && p.peekOp("(") {
		fn := winNames[t.text]
		name := t.text
		p.advance() // name
		p.advance() // (
		var args []Expr
		if !p.acceptOp(")") {
			for {
				a, err := p.expression()
				if err != nil {
					return nil, err
				}
				args = append(args, a)
				if !p.acceptOp(",") {
					break
				}
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
		}
		lo, hi := winArity(fn)
		if len(args) < lo || len(args) > hi {
			return nil, serr.New("wrong number of arguments for window function",
				"function", name, "got", strconv.Itoa(len(args)),
				"want", strconv.Itoa(lo)+".."+strconv.Itoa(hi))
		}
		for _, a := range args {
			if hasWindowExpr(a) {
				return nil, serr.New("window function calls cannot be nested")
			}
		}
		w := &ExWindow{Win: fn}
		// Positional meaning is per-family: LAG/LEAD are (value, offset,
		// default); NTH_VALUE is (value, n) with n riding in Offset.
		if len(args) > 0 {
			w.Arg = args[0]
		}
		if len(args) > 1 {
			w.Offset = args[1]
		}
		if len(args) > 2 {
			w.Default = args[2]
		}
		return p.windowOver(w)
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

// windowOver parses the OVER (PARTITION BY ... ORDER BY ... [frame])
// clause and attaches it to w; cur is the "over" keyword.
func (p *parser) windowOver(w *ExWindow) (Expr, error) {
	// The aggregate caller has already seen "over"; the window-only
	// path has not, and e.g. lag(x) with no OVER must error clearly.
	if t := p.cur(); !(t.kind == tIdent && t.text == "over") {
		return nil, serr.New("window function requires an OVER clause", "function", w.fnName())
	}
	p.advance() // over
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	if p.acceptKw("partition") {
		if err := p.expectKw("by"); err != nil {
			return nil, err
		}
		for {
			e, err := p.expression()
			if err != nil {
				return nil, err
			}
			w.Partition = append(w.Partition, e)
			if !p.acceptOp(",") {
				break
			}
		}
	}
	if p.acceptKw("order") {
		if err := p.expectKw("by"); err != nil {
			return nil, err
		}
		for {
			e, err := p.expression()
			if err != nil {
				return nil, err
			}
			o := OrderItem{SelectItem: SelectItem{Ex: e}}
			if p.acceptKw("desc") {
				o.Desc = true
			} else {
				p.acceptKw("asc")
			}
			w.OrderBy = append(w.OrderBy, o)
			if !p.acceptOp(",") {
				break
			}
		}
	}
	if t := p.cur(); t.kind == tIdent && (t.text == "rows" || t.text == "range" || t.text == "groups") {
		f, err := p.windowFrame(len(w.OrderBy))
		if err != nil {
			return nil, err
		}
		w.Frame = f
	}
	return w, p.expectOp(")")
}

// windowFrame parses an explicit frame clause; cur is the mode keyword
// (rows/range/groups). Both forms are accepted — BETWEEN <start> AND
// <end>, and the single-bound shorthand whose end is CURRENT ROW.
// Bound-pair validity is checked here with Postgres' wording, so an
// impossible frame fails at parse rather than producing empty frames
// everywhere. An EXCLUDE clause (CURRENT ROW/GROUP/TIES, or the
// default NO OTHERS) removes rows near the current row after the
// bounds have chosen the frame.
func (p *parser) windowFrame(nOrderBy int) (*WindowFrame, error) {
	f := &WindowFrame{}
	switch {
	case p.acceptKw("rows"):
		f.Mode = FrameRows
	case p.acceptKw("groups"):
		f.Mode = FrameGroups
		// GROUPS offsets count peer groups, which only exist under an
		// ORDER BY; Postgres rejects the mode outright without one.
		if nOrderBy == 0 {
			return nil, serr.New("GROUPS mode requires an ORDER BY clause")
		}
	default:
		p.advance() // range
		f.Mode = FrameRange
	}
	if p.acceptKw("between") {
		start, err := p.frameBound(f.Mode, nOrderBy)
		if err != nil {
			return nil, err
		}
		if err = p.expectKw("and"); err != nil {
			return nil, err
		}
		end, err := p.frameBound(f.Mode, nOrderBy)
		if err != nil {
			return nil, err
		}
		f.Start, f.End = start, end
	} else {
		start, err := p.frameBound(f.Mode, nOrderBy)
		if err != nil {
			return nil, err
		}
		f.Start, f.End = start, FrameBound{Type: BoundCurrentRow}
	}
	// A frame's start must not lie after its end; the specific illegal
	// pairs get Postgres' messages. (Equal-offset cases like BETWEEN 2
	// FOLLOWING AND 1 FOLLOWING are legal — they yield empty frames.)
	switch {
	case f.Start.Type == BoundUnboundedFollowing:
		return nil, serr.New("frame start cannot be UNBOUNDED FOLLOWING")
	case f.End.Type == BoundUnboundedPreceding:
		return nil, serr.New("frame end cannot be UNBOUNDED PRECEDING")
	case f.Start.Type == BoundCurrentRow && f.End.Type == BoundOffsetPreceding:
		return nil, serr.New("frame starting from current row cannot have preceding rows")
	case f.Start.Type == BoundOffsetFollowing && f.End.Type == BoundOffsetPreceding:
		return nil, serr.New("frame starting from following row cannot have preceding rows")
	case f.Start.Type == BoundOffsetFollowing && f.End.Type == BoundCurrentRow:
		return nil, serr.New("frame starting from following row cannot reference current row")
	}
	// The EXCLUDE clause. GROUP and TIES are defined through ORDER BY
	// peers but stay legal without one (the whole partition is a single
	// peer group then), matching Postgres — only GROUPS *mode* demands
	// an ORDER BY, checked above.
	if p.acceptKw("exclude") {
		switch {
		case p.acceptKw("current"):
			if err := p.expectKw("row"); err != nil {
				return nil, err
			}
			f.Exclude = ExcludeCurrentRow
		case p.acceptKw("group"):
			f.Exclude = ExcludeGroup
		case p.acceptKw("ties"):
			f.Exclude = ExcludeTies
		case p.acceptKw("no"):
			if err := p.expectKw("others"); err != nil {
				return nil, err
			}
		default:
			return nil, p.unexpected("CURRENT ROW, GROUP, TIES, or NO OTHERS")
		}
	}
	return f, nil
}

// frameBound parses one frame endpoint: UNBOUNDED PRECEDING/FOLLOWING,
// CURRENT ROW, or <expr> PRECEDING/FOLLOWING.
func (p *parser) frameBound(mode FrameMode, nOrderBy int) (FrameBound, error) {
	if p.acceptKw("unbounded") {
		if p.acceptKw("preceding") {
			return FrameBound{Type: BoundUnboundedPreceding}, nil
		}
		if p.acceptKw("following") {
			return FrameBound{Type: BoundUnboundedFollowing}, nil
		}
		return FrameBound{}, p.unexpected("PRECEDING or FOLLOWING")
	}
	if p.acceptKw("current") {
		if err := p.expectKw("row"); err != nil {
			return FrameBound{}, err
		}
		return FrameBound{Type: BoundCurrentRow}, nil
	}
	// An offset bound. A RANGE offset is a distance measured on the
	// window ORDER BY key (added in Postgres 11), so it only makes
	// sense against a single sort key; the key's type is checked at
	// execution, where the scope is known.
	if mode == FrameRange && nOrderBy != 1 {
		return FrameBound{}, serr.New("RANGE with offset PRECEDING/FOLLOWING requires exactly one ORDER BY column")
	}
	off, err := p.expression()
	if err != nil {
		return FrameBound{}, err
	}
	if err = checkFrameOffset(off, mode); err != nil {
		return FrameBound{}, err
	}
	b := FrameBound{Offset: off}
	switch {
	case p.acceptKw("preceding"):
		b.Type = BoundOffsetPreceding
	case p.acceptKw("following"):
		b.Type = BoundOffsetFollowing
	default:
		return FrameBound{}, p.unexpected("PRECEDING or FOLLOWING")
	}
	return b, nil
}

// checkFrameOffset enforces that a frame offset is row-independent.
// Postgres evaluates frame offsets once per window, not per row, so
// anything needing row context — columns, aggregates, window calls,
// subqueries — is rejected at parse; the executor can then evaluate
// the offset without a current row.
func checkFrameOffset(e Expr, mode FrameMode) error {
	var err error
	walkExpr(e, func(sub Expr) bool {
		switch sub.(type) {
		case *ExCol:
			err = serr.New("argument of " + mode.name() + " must not contain variables")
		case *ExAgg, *ExWindow, *ExSub:
			err = serr.New("argument of " + mode.name() + " must be a simple expression")
		}
		return err == nil
	})
	return err
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
	case "@>":
		return OpContains, true
	case "<@":
		return OpContainedBy, true
	case "?":
		return OpKeyExists, true
	case "?|":
		return OpKeyExistsAny, true
	case "?&":
		return OpKeyExistsAll, true
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
		// Cap $n at the wire protocol's limit (Bind counts parameters in
		// an int16). The statement's parameter count is the highest $n it
		// mentions, and Describe allocates one slot per parameter — an
		// unbounded $n would let a 30-byte query demand a terabyte-sized
		// allocation, which is a fatal OOM, not a recoverable error.
		// (Found by FuzzMessageParse.)
		if n > maxParamNumber {
			return nil, serr.New("parameter number out of range",
				"param", t.text, "max", fmt.Sprint(maxParamNumber), "pos", fmt.Sprint(t.pos))
		}
		p.advance()
		return Param(n), nil
	}
	return nil, p.unexpected("a literal value")
}
