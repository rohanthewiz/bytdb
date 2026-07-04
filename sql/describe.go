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
	// UPDATE, DELETE, CREATE TABLE, DROP TABLE, ALTER TABLE,
	// CREATE INDEX, or DROP INDEX.
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
	lk := s.db.lookup(s.db.e.Table)
	switch st := s.st.(type) {
	case *Insert:
		desc, _ := lk(st.Table)
		if desc == nil {
			return nil, serr.New("no such table", "table", st.Table)
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
					return nil, serr.New("no such column", "table", st.Table, "column", name)
				}
				colTypes = append(colTypes, desc.Columns[i].Type)
			}
		}
		for _, row := range st.Rows {
			for j, v := range row {
				if j < len(colTypes) { // arity mismatches error at execution
					note(v, colTypes[j])
				}
			}
		}
	case *Update:
		sc, err := buildScope(lk, []FromItem{{Table: st.Table}})
		if err != nil {
			return nil, err
		}
		desc := sc.tables[0].desc
		for name, v := range st.Set {
			i := desc.ColIndex(name)
			if i < 0 {
				return nil, serr.New("no such column", "table", st.Table, "column", name)
			}
			note(v, desc.Columns[i].Type)
		}
		if err := inferPredParams(st.Where, columnType(sc), note); err != nil {
			return nil, err
		}
	case *Delete:
		sc, err := buildScope(lk, []FromItem{{Table: st.Table}})
		if err != nil {
			return nil, err
		}
		if err := inferPredParams(st.Where, columnType(sc), note); err != nil {
			return nil, err
		}
	case *Select:
		sc, err := buildScope(lk, st.From)
		if err != nil {
			return nil, err
		}
		for k, it := range st.From {
			if it.On != nil {
				if err := inferPredParams(it.On, columnType(sc.prefix(k+1)), note); err != nil {
					return nil, err
				}
			}
		}
		if err := inferPredParams(st.Where, columnType(sc), note); err != nil {
			return nil, err
		}
		if err := inferPredParams(st.Having, itemType(sc), note); err != nil {
			return nil, err
		}
		res := &Result{}
		if st.isAggregate() {
			q, err := resolveAgg(sc, st)
			if err != nil {
				return nil, err
			}
			q.resultCols(st, res)
		} else if _, err := projectSelect(sc, st, res); err != nil {
			return nil, err
		}
		info.Cols, info.Types = res.Cols, res.Types
	}
	return info, nil
}

// Command is the statement's command tag word(s) — SELECT, INSERT,
// CREATE TABLE, ... — from the parse alone, without catalog access.
func (s *Stmt) Command() string { return command(s.st) }

// command is the statement's command tag word(s).
func command(st Statement) string {
	switch st.(type) {
	case *CreateTable:
		return "CREATE TABLE"
	case *DropTable:
		return "DROP TABLE"
	case *AddColumn, *DropColumn:
		return "ALTER TABLE"
	case *CreateIndex:
		return "CREATE INDEX"
	case *DropIndex:
		return "DROP INDEX"
	case *Insert:
		return "INSERT"
	case *Select:
		return "SELECT"
	case *Update:
		return "UPDATE"
	case *Delete:
		return "DELETE"
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
