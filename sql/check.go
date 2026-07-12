package sql

// check.go: CHECK constraints. The table descriptor stores each
// check's expression as source text (the engine treats it as opaque);
// this file validates and names checks at CREATE TABLE, re-parses
// them at write time, and evaluates them per row — a row is rejected
// only when a check is definitely false, so NULLs pass, per SQL.

import (
	"fmt"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// resolveChecks validates a CREATE TABLE's check constraints against
// its declared columns and returns them named and rendered for the
// descriptor. Default names follow Postgres: table_col_check for a
// column-level check, table_check for a table-level one, with a
// numeric suffix on collision.
func resolveChecks(ct *CreateTable, cols []bytdb.Column) ([]bytdb.CheckDesc, error) {
	if len(ct.Checks) == 0 {
		return nil, nil
	}
	synth := &bytdb.TableDesc{Name: ct.Table, Columns: cols}
	sc := &scope{tables: []scopeTable{{name: ct.Table, desc: synth}}, width: len(cols)}
	taken := map[string]bool{}
	for _, ck := range ct.Checks {
		if ck.Name != "" {
			if taken[ck.Name] {
				return nil, serr.New("duplicate constraint name", "table", ct.Table, "constraint", ck.Name)
			}
			taken[ck.Name] = true
		}
	}
	out := make([]bytdb.CheckDesc, len(ct.Checks))
	for i, ck := range ct.Checks {
		if err := validateCheckExpr(sc, ck.Ex); err != nil {
			return nil, err
		}
		name := ck.Name
		if name == "" {
			base := ct.Table + "_check"
			if ck.Col != "" {
				base = ct.Table + "_" + ck.Col + "_check"
			}
			name = base
			for n := 1; taken[name]; n++ {
				name = fmt.Sprintf("%s%d", base, n)
			}
			taken[name] = true
		}
		out[i] = bytdb.CheckDesc{Name: name, Expr: ck.Text}
	}
	return out, nil
}

// validateCheckExpr enforces what a check expression may contain:
// columns of its own table, literals, and row-level operations — no
// aggregates, subqueries, or placeholders.
func validateCheckExpr(sc *scope, ex Expr) error {
	if fn := findAgg(ex); fn != AggNone {
		return serr.New("aggregate functions are not allowed in check constraints",
			"function", fn.name())
	}
	var fail error
	walkExpr(ex, func(sub Expr) bool {
		if fail != nil {
			return false
		}
		switch n := sub.(type) {
		case *ExSub:
			fail = serr.New("cannot use subquery in check constraint")
			return false
		case *ExLit:
			if _, ok := n.Val.(Param); ok {
				fail = serr.New("placeholders are not allowed in check constraints")
				return false
			}
		case *ExCol:
			if _, err := sc.resolve(n.Col); err != nil {
				fail = err
				return false
			}
		}
		return true
	})
	return fail
}

// namedCheck is one stored check, parsed back for evaluation.
type namedCheck struct {
	name string
	ex   Expr
}

// tableChecks parses a table's stored check constraints; nil when the
// table has none.
func tableChecks(desc *bytdb.TableDesc) ([]namedCheck, error) {
	if len(desc.Checks) == 0 {
		return nil, nil
	}
	out := make([]namedCheck, len(desc.Checks))
	for i, ck := range desc.Checks {
		ex, err := parseCheckExpr(ck.Expr)
		if err != nil {
			return nil, serr.Wrap(err, "op", "parse check constraint", "constraint", ck.Name)
		}
		out[i] = namedCheck{ck.Name, ex}
	}
	return out, nil
}

// checkRow evaluates every check against one row (vals in declared
// column order), erroring on the first definitely-false one.
func checkRow(env *exEnv, table string, checks []namedCheck, vals []any) error {
	for _, ck := range checks {
		rowEnv := *env
		rowEnv.row = vals
		t, err := evalTruth(&rowEnv, ck.ex)
		if err != nil {
			return serr.Wrap(err, "constraint", ck.name)
		}
		if t == triFalse {
			return serr.New(`new row for relation "` + table +
				`" violates check constraint "` + ck.name + `"`)
		}
	}
	return nil
}

// execAddConstraint handles ALTER TABLE ... ADD CHECK: the expression
// is validated against the table's columns, named by convention when
// unnamed, and every existing row must satisfy it — evaluated in the
// transaction that publishes the descriptor, so no write slips in
// between.
func (d *DB) execAddConstraint(s *AddConstraint) (*Result, error) {
	desc := d.e.Table(s.Table)
	if desc == nil {
		return nil, serr.New("no such table", "table", s.Table)
	}
	sc := &scope{tables: []scopeTable{{name: s.Table, desc: desc}}, width: len(desc.Columns)}
	if err := validateCheckExpr(sc, s.Check.Ex); err != nil {
		return nil, err
	}
	name := s.Check.Name
	if name == "" {
		taken := map[string]bool{}
		for _, ck := range desc.Checks {
			taken[ck.Name] = true
		}
		base := s.Table + "_check" // ALTER adds table-level checks only
		name = base
		for n := 1; taken[name]; n++ {
			name = fmt.Sprintf("%s%d", base, n)
		}
	}
	// Checks cannot hold subqueries, so evaluation never needs a
	// transaction in the environment.
	env := &exEnv{d: d, sc: sc}
	err := d.e.AddCheck(s.Table, bytdb.CheckDesc{Name: name, Expr: s.Check.Text},
		func(row bytdb.Row) error {
			rowEnv := *env
			rowEnv.row = row.Vals
			t, err := evalTruth(&rowEnv, s.Check.Ex)
			if err != nil {
				return serr.Wrap(err, "constraint", name)
			}
			if t == triFalse {
				return serr.New(`check constraint "` + name + `" of relation "` +
					s.Table + `" is violated by some row`)
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return &Result{}, nil
}

// execDropConstraint handles ALTER TABLE ... DROP CONSTRAINT for
// CHECK and FOREIGN KEY constraints (they share a namespace, as in
// Postgres). The primary key is structural in bytdb, and unique
// constraints are indexes (DROP INDEX).
func (d *DB) execDropConstraint(s *DropConstraint) (*Result, error) {
	desc := d.e.Table(s.Table)
	if desc == nil {
		return nil, serr.New("no such table", "table", s.Table)
	}
	existed, err := d.e.DropCheck(s.Table, s.Name)
	if err != nil {
		return nil, err
	}
	if !existed {
		if existed, err = d.e.DropForeignKey(s.Table, s.Name); err != nil {
			return nil, err
		}
	}
	if !existed {
		if s.Name == s.Table+"_pkey" {
			return nil, serr.New(`cannot drop constraint "`+s.Name+`" of relation "`+s.Table+`"`,
				"hint", "a bytdb table keeps its primary key for life")
		}
		if s.IfExists {
			return &Result{Notice: `constraint "` + s.Name + `" of relation "` +
				s.Table + `" does not exist, skipping`}, nil
		}
		return nil, serr.New(`constraint "` + s.Name + `" of relation "` +
			s.Table + `" does not exist`)
	}
	return &Result{}, nil
}

// checkDropColumn rejects dropping a column a check constraint
// mentions, as Postgres does without CASCADE.
func (d *DB) checkDropColumn(table, col string) error {
	return d.checkColumnUnmentioned(table, col, "drop")
}

// checkColumnUnmentioned rejects dropping or renaming a column a
// check constraint mentions: check expressions are stored as text, so
// either change would silently orphan the constraint.
func (d *DB) checkColumnUnmentioned(table, col, verb string) error {
	desc := d.e.Table(table)
	if desc == nil {
		return nil // the engine reports the missing table
	}
	checks, err := tableChecks(desc)
	if err != nil {
		return err
	}
	for _, ck := range checks {
		used := false
		walkExpr(ck.ex, func(sub Expr) bool {
			if c, ok := sub.(*ExCol); ok && c.Col.Name == col &&
				(c.Col.Table == "" || c.Col.Table == table) {
				used = true
			}
			return !used
		})
		if used {
			return serr.New(`cannot `+verb+` column "`+col+`" of table "`+table+
				`" because other objects depend on it`, "constraint", ck.name)
		}
	}
	return nil
}
