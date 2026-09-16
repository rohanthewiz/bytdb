package sql

// alter.go: the SQL side of the ALTER TABLE forms that rebuild a table
// — ALTER COLUMN TYPE and the primary-key replacement. The engine owns
// the rewrite (bytdb.Engine.AlterColumnType / SetPrimaryKey); this file
// supplies what only the SQL layer can: evaluating a USING expression
// per row, and re-checking CHECK constraints against the converted
// rows, since the engine treats both expression languages as opaque.

import (
	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// execAlterColumnType runs ALTER COLUMN ... TYPE t [USING expr].
//
// Both callbacks run inside the engine's DDL transaction, once per row,
// against the descriptor the ENGINE resolved there — which is the one
// that matters: it is the schema of the rows being converted, whereas
// the descriptor read here beforehand is only good enough to validate
// the statement's column references up front. So each callback builds
// its evaluation scope from the row's own descriptor, rebuilding it only
// when that pointer changes (in practice: once).
func (d *DB) execAlterColumnType(s *AlterColumnType) (*Result, error) {
	desc := d.e.Table(s.Table)
	if desc == nil {
		return nil, serr.New("no such table", "table", s.Table)
	}
	if desc.ColIndex(s.Col) < 0 {
		return nil, serr.New(`column "`+s.Col+`" of relation "`+s.Table+`" does not exist`,
			"table", s.Table, "column", s.Col)
	}
	ch := bytdb.ColumnTypeChange{Type: s.Type, MaxLen: s.MaxLen}

	if s.Using != nil {
		sc := tableScope(s.Table, desc)
		if err := validateRowExpr(sc, s.Using, "transform expressions", "transform expression"); err != nil {
			return nil, err
		}
		env := rowEnvFor(d, s.Table)
		ch.Using = func(old bytdb.Row) (any, error) {
			e := env(old)
			v, err := evalEx(e, s.Using)
			if err != nil {
				return nil, serr.Wrap(err, "clause", "USING")
			}
			// The engine coerces to the column type the way an insert
			// would; clock values from a USING expression need the SQL
			// layer's literal coercion first (time.Time -> micros/days).
			return coerceLit(v, s.Type)
		}
	}

	// CHECK constraints are stored as text and may mention the column
	// being converted; Postgres re-verifies them against the rewritten
	// rows, and so does this, with ADD CONSTRAINT's wording.
	checks, err := tableChecks(desc)
	if err != nil {
		return nil, err
	}
	if len(checks) > 0 {
		env := rowEnvFor(d, s.Table)
		ch.Validate = func(row bytdb.Row) error {
			e := env(row)
			for _, ck := range checks {
				t, err := evalTruth(e, ck.ex)
				if err != nil {
					return serr.Wrap(err, "constraint", ck.name)
				}
				if t == triFalse {
					return serr.New(`check constraint "` + ck.name + `" of relation "` +
						s.Table + `" is violated by some row`)
				}
			}
			return nil
		}
	}

	if err := d.e.AlterColumnType(s.Table, s.Col, ch); err != nil {
		return nil, err
	}
	return &Result{}, nil
}

// execReplacePrimaryKey runs DROP CONSTRAINT t_pkey, ADD PRIMARY KEY
// (cols). The dropped constraint must be the primary key itself: any
// other pairing would, in Postgres, drop that constraint and then fail
// adding a second key — so refusing it is the same outcome, minus the
// dropped constraint.
func (d *DB) execReplacePrimaryKey(s *ReplacePrimaryKey) (*Result, error) {
	if d.e.Table(s.Table) == nil {
		return nil, serr.New("no such table", "table", s.Table)
	}
	if want := bytdb.PKConstraintName(s.Table); s.Dropped != want {
		return nil, serr.New(`multiple primary keys for table "`+s.Table+`" are not allowed`,
			"hint", "only DROP CONSTRAINT "+want+" can be combined with ADD PRIMARY KEY")
	}
	if err := d.e.SetPrimaryKey(s.Table, s.Cols...); err != nil {
		return nil, err
	}
	return &Result{}, nil
}

// tableScope is the one-table evaluation scope stored-row expressions
// resolve their column references in.
func tableScope(table string, desc *bytdb.TableDesc) *scope {
	return &scope{tables: []scopeTable{{name: table, desc: desc}}, width: len(desc.Columns)}
}

// rowEnvFor returns a function mapping a stored row to an evaluation
// environment positioned on it. The environment is reused across calls
// (only .row changes) and its scope rebuilt only when the row's
// descriptor differs from the previous call's. No transaction is set:
// row expressions admit no subqueries (validateRowExpr), so evaluation
// never reads the store.
func rowEnvFor(d *DB, table string) func(bytdb.Row) *exEnv {
	env := &exEnv{d: d}
	var cur *bytdb.TableDesc
	return func(r bytdb.Row) *exEnv {
		if r.Desc != cur {
			cur = r.Desc
			env.sc = tableScope(table, cur)
		}
		env.row = r.Vals
		return env
	}
}
