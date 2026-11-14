package sql

// alter.go: the SQL side of the ALTER TABLE forms that rebuild a table
// — ALTER COLUMN TYPE and the primary-key replacement. The engine owns
// the rewrite (bytdb.Engine.AlterColumnType / SetPrimaryKey); this file
// supplies what only the SQL layer can: evaluating a USING expression
// per row, and re-checking CHECK constraints against the converted
// rows, since the engine treats both expression languages as opaque.

import (
	"fmt"

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
	desc := d.e.TableShared(s.Table)
	if desc == nil {
		return nil, noSuchTable(s.Table)
	}
	if desc.ColIndex(s.Col) < 0 {
		return nil, serr.New(`column "`+s.Col+`" of relation "`+s.Table+`" does not exist`,
			"table", s.Table, "column", s.Col)
	}
	ch := bytdb.ColumnTypeChange{Type: s.Type, MaxLen: s.MaxLen, Precision: s.Precision, Scale: s.Scale}

	// An identity USING is left off, so the engine sees that nothing
	// changes and skips the table rewrite. Its no-op shortcut needs Using
	// nil. See usingIsIdentity.
	if s.Using != nil && !usingIsIdentity(s, desc.Columns[desc.ColIndex(s.Col)]) {
		sc := tableScope(s.Table, desc)
		if err := validateRowExpr(sc, s.Using, "transform expressions", "transform expression"); err != nil {
			return nil, err
		}
		// Typed over the old row, so USING ts::date knows its operand is
		// a timestamp and converts it; untyped, it stored the microsecond
		// count as a day number (deep-look D-23/D-24).
		// (A local, not s.Using: a prepared statement's tree is shared.)
		using, err := typeRowExpr(sc, s.Using)
		if err != nil {
			return nil, err
		}
		env := rowEnvFor(d, s.Table)
		ch.Using = func(old bytdb.Row) (any, error) {
			e := env(old)
			v, err := evalEx(e, using)
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
	// rows, and so does this, with ADD CONSTRAINT's wording. They are
	// typed against the column's new type, which is what the rewritten
	// rows carry.
	newDesc := desc.Clone()
	newDesc.Columns[newDesc.ColIndex(s.Col)].Type = s.Type
	checks, err := tableChecks(newDesc)
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
	if d.e.TableShared(s.Table) == nil {
		return nil, noSuchTable(s.Table)
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

// usingIsIdentity reports whether s's USING expression just restates the
// column's current value: `USING c` or `USING c::T`, where T and the
// TYPE clause both name the type col already has, modifiers included.
//
// GORM's AutoMigrate sends exactly this, on every run, for any column
// whose reported type it doesn't recognize as its own spelling.
// bytdb stores smallint as int8, so a `smallint` field gets
// `ALTER COLUMN "age" TYPE smallint USING "age"::smallint` each time the
// application starts. With USING present, the engine would rewrite every
// row and rebuild every index to store the same values.
//
// It's an identity because col already has the target type: a cast to
// a value's own type returns the value. Integer casts don't range-check
// (smallint is int8 here), type modifiers on casts other than numeric's
// are not applied, and a numeric(p,s) cast to the column's own (p,s)
// only rounds what is already rounded. Postgres rewrites the table for
// any USING, but the rows come out the same, so skipping the rewrite
// changes nothing a reader can see.
//
// The column is the one read before the engine's DDL transaction. If a
// concurrent ALTER changes its type in between, the engine sees a real
// change with no USING, applies the automatic cast, and refuses when
// there is none.
func usingIsIdentity(s *AlterColumnType, col bytdb.Column) bool {
	if col.Type != s.Type || col.MaxLen != s.MaxLen || col.Precision != s.Precision || col.Scale != s.Scale {
		return false
	}
	isCol := func(e Expr) bool {
		c, ok := e.(*ExCol)
		return ok && c.Col.Name == s.Col && (c.Col.Table == "" || c.Col.Table == s.Table)
	}
	if isCol(s.Using) {
		return true
	}
	cast, ok := s.Using.(*ExCast)
	if !ok || !isCol(cast.E) || castColType(cast.Type) != s.Type {
		return false
	}
	// castColType falls back to text for names it doesn't know, so a
	// text column must be cast to a text spelling, not an unknown name
	// that only defaulted to text.
	if s.Type == bytdb.TString {
		switch cast.Type {
		case "text", "varchar", "string", "name":
		default:
			return false
		}
	}
	// numeric(p,s) is the one cast that applies its modifier, so it
	// must match the column's own. A bare numeric cast changes nothing.
	if isNumericCast(cast.Type) && cast.Type != "numeric" && cast.Type != "decimal" {
		return cast.Type == fmt.Sprintf("numeric(%d,%d)", s.Precision, s.Scale)
	}
	return true
}
