package sql

// coerce.go: Postgres-style adaptation of quoted literals. Postgres
// treats a quoted literal as untyped and coerces it to the type its
// context demands ('2' compares and inserts as an integer against an
// int column). The parser reads every quoted literal as a string —
// it has no catalog — so this pass runs per execution, after
// parameter binding, converting string values sitting in typed
// positions. Wire-protocol clients depend on it: pgx's simple
// protocol renders every argument as a quoted literal.
//
// The pass is best-effort about resolution: an unknown table or
// column leaves the value alone so the executor reports it in its own
// words. Only genuine conversion failures error here ('abc' where an
// int is required), as in Postgres.

import (
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// coerceLiterals returns st with string values in typed positions
// converted to their column's value kind, copying whatever it
// changes; statements with nothing to coerce pass through as is.
func (d *DB) coerceLiterals(st Statement) (Statement, error) {
	lookup := d.e.Table
	switch s := st.(type) {
	case *Insert:
		desc := lookup(s.Table)
		if desc == nil {
			return st, nil
		}
		var colTypes []bytdb.ColType
		if s.Cols == nil {
			for _, c := range desc.Columns {
				colTypes = append(colTypes, c.Type)
			}
		} else {
			for _, name := range s.Cols {
				i := desc.ColIndex(name)
				if i < 0 {
					return st, nil
				}
				colTypes = append(colTypes, desc.Columns[i].Type)
			}
		}
		c := *s
		c.Rows = make([][]any, len(s.Rows))
		for i, row := range s.Rows {
			r := make([]any, len(row))
			for j, v := range row {
				r[j] = v
				if j < len(colTypes) {
					cv, err := coerceLit(v, colTypes[j])
					if err != nil {
						return nil, err
					}
					r[j] = cv
				}
			}
			c.Rows[i] = r
		}
		return &c, nil
	case *Update:
		desc := lookup(s.Table)
		if desc == nil {
			return st, nil
		}
		c := *s
		c.Set = make(map[string]any, len(s.Set))
		for name, v := range s.Set {
			c.Set[name] = v
			if i := desc.ColIndex(name); i >= 0 {
				cv, err := coerceLit(v, desc.Columns[i].Type)
				if err != nil {
					return nil, err
				}
				c.Set[name] = cv
			}
		}
		sc, err := buildScope(lookup, []FromItem{{Table: s.Table}})
		if err != nil {
			return &c, nil
		}
		if c.Where, err = coerceBool(s.Where, columnType(sc)); err != nil {
			return nil, err
		}
		return &c, nil
	case *Delete:
		sc, err := buildScope(lookup, []FromItem{{Table: s.Table}})
		if err != nil {
			return st, nil
		}
		where, err := coerceBool(s.Where, columnType(sc))
		if err != nil {
			return nil, err
		}
		c := *s
		c.Where = where
		return &c, nil
	case *Select:
		sc, err := buildScope(lookup, s.From)
		if err != nil {
			return st, nil
		}
		c := *s
		c.From = make([]FromItem, len(s.From))
		copy(c.From, s.From)
		for k := range c.From {
			if c.From[k].On, err = coerceBool(c.From[k].On, columnType(sc.prefix(k+1))); err != nil {
				return nil, err
			}
		}
		if c.Where, err = coerceBool(s.Where, columnType(sc)); err != nil {
			return nil, err
		}
		if c.Having, err = coerceBool(s.Having, itemType(sc)); err != nil {
			return nil, err
		}
		return &c, nil
	}
	return st, nil
}

// coerceBool clones a condition tree, converting each leaf's string
// literal to its left item's type. Leaves that fail to resolve stay
// as they are — the executor owns that error.
func coerceBool(e BoolExpr, typeOf func(SelectItem) (bytdb.ColType, error)) (BoolExpr, error) {
	switch n := e.(type) {
	case nil:
		return nil, nil
	case *Pred:
		if _, ok := n.Val.(string); !ok {
			return n, nil
		}
		t, err := typeOf(n.Item)
		if err != nil {
			return n, nil
		}
		cv, err := coerceLit(n.Val, t)
		if err != nil {
			return nil, err
		}
		if cv == n.Val {
			return n, nil
		}
		c := *n
		c.Val = cv
		return &c, nil
	case *Not:
		expr, err := coerceBool(n.Expr, typeOf)
		if err != nil {
			return nil, err
		}
		return &Not{Expr: expr}, nil
	case *And:
		exprs, err := coerceBools(n.Exprs, typeOf)
		if err != nil {
			return nil, err
		}
		return &And{Exprs: exprs}, nil
	case *Or:
		exprs, err := coerceBools(n.Exprs, typeOf)
		if err != nil {
			return nil, err
		}
		return &Or{Exprs: exprs}, nil
	}
	return e, nil
}

func coerceBools(in []BoolExpr, typeOf func(SelectItem) (bytdb.ColType, error)) ([]BoolExpr, error) {
	out := make([]BoolExpr, len(in))
	for i, e := range in {
		var err error
		if out[i], err = coerceBool(e, typeOf); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// coerceLit converts a string value to a non-string column type's
// value kind, per Postgres literal syntax. Non-strings and string
// columns pass through.
func coerceLit(v any, t bytdb.ColType) (any, error) {
	s, ok := v.(string)
	if !ok {
		return v, nil
	}
	switch t {
	case bytdb.TInt:
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, serr.New("invalid input syntax for type int", "value", s)
		}
		return n, nil
	case bytdb.TFloat:
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, serr.New("invalid input syntax for type float", "value", s)
		}
		return f, nil
	case bytdb.TBool:
		b, err := strconv.ParseBool(strings.TrimSpace(strings.ToLower(s)))
		if err != nil {
			return nil, serr.New("invalid input syntax for type bool", "value", s)
		}
		return b, nil
	case bytdb.TBytes:
		if strings.HasPrefix(s, `\x`) {
			b, err := hex.DecodeString(s[2:])
			if err != nil {
				return nil, serr.New("invalid input syntax for type bytea", "value", s)
			}
			return b, nil
		}
		return []byte(s), nil
	}
	return s, nil
}
