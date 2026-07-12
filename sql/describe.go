package sql

import (
	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// StmtInfo describes a prepared statement without executing it — what
// a wire protocol needs at Parse and Describe time: the command word,
// the inferred type of each placeholder, and the output shape a
// SELECT will produce.
type StmtInfo struct {
	// Command is the statement's command tag word(s): SELECT, INSERT,
	// UPDATE, DELETE, EXPLAIN, CREATE TABLE, DROP TABLE, ALTER TABLE,
	// CREATE INDEX, DROP INDEX, BEGIN, START TRANSACTION, COMMIT, or
	// ROLLBACK.
	Command string
	// Params holds one inferred type per placeholder, $1 first: the
	// type of the column each placeholder is compared against,
	// inserted into, or assigned to (a HAVING placeholder compared to
	// an aggregate takes the aggregate's result type). When the same
	// $n appears in differently-typed positions the first position
	// wins; a $n the statement never mentions stays "".
	Params []bytdb.ColType
	// Cols and Types are the output columns a SELECT will produce, in
	// order; empty for statements that return no rows.
	Cols  []string
	Types []bytdb.ColType
}

// Describe resolves the prepared statement against the current
// catalog. It fails where execution would: on tables or columns the
// statement names that do not exist.
func (s *Stmt) Describe() (*StmtInfo, error) {
	info := &StmtInfo{Command: command(s.st), Params: make([]bytdb.ColType, s.n)}
	note := func(v any, t bytdb.ColType) {
		if p, ok := v.(Param); ok && info.Params[p-1] == "" {
			info.Params[p-1] = t
		}
	}
	if err := s.db.describeInto(s.st, note, info); err != nil {
		return nil, err
	}
	return info, nil
}

// describeInto infers a statement's parameter types and output shape
// into info.
func (d *DB) describeInto(st Statement, note func(any, bytdb.ColType), info *StmtInfo) error {
	lk := d.lookup(d.e.Table)
	switch st := st.(type) {
	case *Explain:
		// Parameters are the inner statement's; the output is always
		// one text column of plan lines.
		if err := d.describeInto(st.Stmt, note, &StmtInfo{}); err != nil {
			return err
		}
		info.Cols = []string{"QUERY PLAN"}
		info.Types = []bytdb.ColType{bytdb.TString}
	case *Insert:
		desc, _ := lk(st.Table)
		if desc == nil {
			return serr.New("no such table", "table", st.Table)
		}
		var colTypes []bytdb.ColType
		if st.Cols == nil {
			for _, c := range desc.Columns {
				colTypes = append(colTypes, c.Type)
			}
		} else {
			for _, name := range st.Cols {
				i := desc.ColIndex(name)
				if i < 0 {
					return serr.New("no such column", "table", st.Table, "column", name)
				}
				colTypes = append(colTypes, desc.Columns[i].Type)
			}
		}
		for _, row := range st.Rows {
			for j, v := range row {
				if j >= len(colTypes) { // arity mismatches error at execution
					continue
				}
				// As in UPDATE's SET expressions: a placeholder inside an
				// expression value infers as the target column's type,
				// best-effort ($1 + 1 in an int column infers int).
				if ex, ok := v.(Expr); ok {
					t := colTypes[j]
					noteExprVals(ex, func(v any) { note(v, t) })
				} else {
					note(v, colTypes[j])
				}
			}
		}
		if oc := st.Conflict; oc != nil && oc.Update {
			for name, v := range oc.Set {
				i := desc.ColIndex(name)
				if i < 0 {
					return serr.New("no such column", "table", st.Table, "column", name)
				}
				note(v, desc.Columns[i].Type)
			}
			// As in UPDATE: a placeholder inside a SET expression infers
			// as the target column's type, best-effort.
			for name, ex := range oc.SetEx {
				i := desc.ColIndex(name)
				if i < 0 {
					return serr.New("no such column", "table", st.Table, "column", name)
				}
				t := desc.Columns[i].Type
				noteExprVals(ex, func(v any) { note(v, t) })
			}
			ocSc := &scope{tables: []scopeTable{
				{name: st.Table, desc: desc},
				{name: "excluded", desc: desc, off: len(desc.Columns)},
			}, width: 2 * len(desc.Columns)}
			if err := inferPredParams(oc.Where, columnType(ocSc), note); err != nil {
				return err
			}
		}
		sc, err := buildScope(lk, []FromItem{{Table: st.Table}})
		if err != nil {
			return err
		}
		if err := describeReturning(sc, &st.Returning, info); err != nil {
			return err
		}
	case *Update:
		sc, err := buildScope(lk, []FromItem{{Table: st.Table}})
		if err != nil {
			return err
		}
		desc := sc.tables[0].desc
		for name, v := range st.Set {
			i := desc.ColIndex(name)
			if i < 0 {
				return serr.New("no such column", "table", st.Table, "column", name)
			}
			note(v, desc.Columns[i].Type)
		}
		// Placeholders inside a SET expression infer as the target
		// column's type — best-effort, but right for the dominant shape
		// (SET age = age + $1), and note keeps the first inference, so a
		// $n also compared against a column elsewhere is unaffected.
		for name, ex := range st.SetEx {
			i := desc.ColIndex(name)
			if i < 0 {
				return serr.New("no such column", "table", st.Table, "column", name)
			}
			t := desc.Columns[i].Type
			noteExprVals(ex, func(v any) { note(v, t) })
		}
		if err := inferPredParams(st.Where, columnType(sc), note); err != nil {
			return err
		}
		if err := describeReturning(sc, &st.Returning, info); err != nil {
			return err
		}
	case *Delete:
		sc, err := buildScope(lk, []FromItem{{Table: st.Table}})
		if err != nil {
			return err
		}
		if err := inferPredParams(st.Where, columnType(sc), note); err != nil {
			return err
		}
		if err := describeReturning(sc, &st.Returning, info); err != nil {
			return err
		}
	case *ShowVar:
		// The output column is named after the parameter, as Postgres
		// names it; the shape is knowable without touching the catalog.
		if st.Name == "all" {
			info.Cols = []string{"name", "setting", "description"}
			info.Types = []bytdb.ColType{bytdb.TString, bytdb.TString, bytdb.TString}
		} else {
			info.Cols = []string{st.Name}
			info.Types = []bytdb.ColType{bytdb.TString}
		}
	case *Select:
		// CTEs and referenced views register as statically shaped
		// virtual tables so their names resolve without execution.
		dd, err := d.staticViews(st)
		if err != nil {
			return err
		}
		if dd, err = dd.staticWith(st); err != nil {
			return err
		}
		lk = dd.lookup(dd.e.Table)
		res := &Result{}
		if err := describeSelect(lk, st, note, res); err != nil {
			return err
		}
		// A UNION's shape is its first arm's; later arms only
		// contribute parameter positions.
		for _, arm := range st.Union {
			if err := describeSelect(lk, arm.Sel, note, &Result{}); err != nil {
				return err
			}
		}
		info.Cols, info.Types = res.Cols, res.Types
	}
	return nil
}

// describeReturning fills a DML statement's output shape from its
// RETURNING clause — the piece that makes INSERT ... RETURNING work
// over the extended protocol, where clients learn the row description
// from Describe before any row is produced. Without the clause the
// statement keeps its rowless shape.
func describeReturning(sc *scope, r *Returning, info *StmtInfo) error {
	if !r.hasReturning() {
		return nil
	}
	res := &Result{}
	if _, _, err := projectSelect(sc, r.retSelect(), res); err != nil {
		return err
	}
	info.Cols, info.Types = res.Cols, res.Types
	return nil
}

// describeSelect infers one SELECT core's parameter types and output
// shape without executing it.
func describeSelect(lk tableLookup, st *Select, note func(any, bytdb.ColType), res *Result) error {
	sc, err := buildScope(lk, st.From)
	if err != nil {
		return err
	}
	for k, it := range st.From {
		if it.On != nil {
			if err := inferPredParams(it.On, columnType(sc.prefix(k+1)), note); err != nil {
				return err
			}
		}
	}
	if err := inferPredParams(st.Where, columnType(sc), note); err != nil {
		return err
	}
	if err := inferPredParams(st.Having, itemType(sc), note); err != nil {
		return err
	}
	// Select-list placeholders are generally untyped (they present as
	// text on the wire), but a window function's extra arguments are
	// knowable: LAG/LEAD's offset and NTH_VALUE's n are integers, and
	// LAG/LEAD's default takes the value argument's type. Inferring
	// them keeps the lag(v, $1, $2) shape working for wire drivers
	// that encode bindings by the described type.
	noteWin := func(e Expr) {
		walkExpr(e, func(sub Expr) bool {
			if w, ok := sub.(*ExWindow); ok {
				if l, ok := w.Offset.(*ExLit); ok {
					note(l.Val, bytdb.TInt)
				}
				if l, ok := w.Default.(*ExLit); ok {
					note(l.Val, exprType(sc, w.Arg))
				}
				// ROWS/GROUPS frame offsets count rows/groups — integers.
				// A RANGE offset is a distance on the window ORDER BY key,
				// so it takes the key's type: int offsets over a float key
				// still execute (mixed numerics compare), but a driver
				// encoding by described type needs the float OID to send
				// fractional offsets.
				if w.Frame != nil {
					ft := bytdb.TInt
					if w.Frame.Mode == FrameRange && len(w.OrderBy) == 1 {
						ft = exprType(sc, w.OrderBy[0].Ex)
					}
					if l, ok := w.Frame.Start.Offset.(*ExLit); ok {
						note(l.Val, ft)
					}
					if l, ok := w.Frame.End.Offset.(*ExLit); ok {
						note(l.Val, ft)
					}
				}
			}
			return true
		})
	}
	for _, it := range st.Items {
		noteWin(it.Ex)
	}
	for _, o := range st.OrderBy {
		noteWin(o.Ex)
	}
	if st.isAggregate() {
		q, err := resolveAgg(sc, st, false)
		if err != nil {
			return err
		}
		q.resultCols(st, res)
		return nil
	}
	_, _, err = projectSelect(sc, st, res)
	return err
}

// Command is the statement's command tag word(s) — SELECT, INSERT,
// CREATE TABLE, ... — from the parse alone, without catalog access.
func (s *Stmt) Command() string { return command(s.st) }

// command is the statement's command tag word(s).
func command(st Statement) string {
	if tc, ok := st.(*TxnControl); ok {
		return tc.Tag
	}
	if sv, ok := st.(*SetVar); ok {
		return sv.Tag
	}
	switch st.(type) {
	case *CreateTable:
		return "CREATE TABLE"
	case *DropTable:
		return "DROP TABLE"
	case *Truncate:
		return "TRUNCATE TABLE"
	case *ShowVar:
		return "SHOW"
	case *CreateView:
		return "CREATE VIEW"
	case *DropView:
		return "DROP VIEW"
	case *AddColumn, *DropColumn, *AddConstraint, *AddFK, *DropConstraint:
		return "ALTER TABLE"
	case *CreateIndex:
		return "CREATE INDEX"
	case *DropIndex:
		return "DROP INDEX"
	case *CreateSequence:
		return "CREATE SEQUENCE"
	case *DropSequence:
		return "DROP SEQUENCE"
	case *AlterSequence:
		return "ALTER SEQUENCE"
	case *Insert:
		return "INSERT"
	case *Select:
		return "SELECT"
	case *Update:
		return "UPDATE"
	case *Delete:
		return "DELETE"
	case *Explain:
		return "EXPLAIN"
	}
	return ""
}

// inferPredParams notes, for every leaf predicate comparing an item
// to a placeholder, the item's type as the placeholder's.
func inferPredParams(e BoolExpr, typeOf func(SelectItem) (bytdb.ColType, error), note func(any, bytdb.ColType)) error {
	return walkPreds(e, func(pr *Pred) error {
		if _, ok := pr.Val.(Param); !ok {
			return nil
		}
		t, err := typeOf(pr.Item)
		if err != nil {
			return err
		}
		note(pr.Val, t)
		return nil
	})
}

// columnType types a plain column item against a scope (WHERE and ON
// predicates).
func columnType(sc *scope) func(SelectItem) (bytdb.ColType, error) {
	return func(it SelectItem) (bytdb.ColType, error) {
		ord, err := sc.resolve(it.Col)
		if err != nil {
			return "", err
		}
		return sc.column(ord).Type, nil
	}
}

// itemType types a HAVING item: a plain column, or an aggregate with
// resultCols' typing — COUNT -> int, AVG -> float, SUM/MIN/MAX -> the
// argument's type.
func itemType(sc *scope) func(SelectItem) (bytdb.ColType, error) {
	return func(it SelectItem) (bytdb.ColType, error) {
		switch it.Agg {
		case AggCount:
			return bytdb.TInt, nil
		case AggAvg:
			return bytdb.TFloat, nil
		}
		ord, err := sc.resolve(it.Col)
		if err != nil {
			return "", err
		}
		return sc.column(ord).Type, nil
	}
}
