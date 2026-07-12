package sql

// fk.go: row-level foreign-key enforcement. The engine stores FKDesc
// on the child table and guards the schema side (see bytdb/fk.go);
// this file checks references as rows change, inside the statement's
// own transaction:
//
//	child INSERT/UPDATE  -> the referenced parent row must exist
//	parent DELETE/UPDATE -> no child row may still reference the old key
//	TRUNCATE             -> a referenced table needs its children in the list
//
// Semantics are MATCH SIMPLE with NO ACTION checked at end of
// statement: a NULL in any child FK column satisfies the constraint,
// and parent-side checks run after the statement's writes, so a
// statement that deletes both a row and everything referencing it is
// legal, as in Postgres.

import (
	"fmt"
	"strings"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// fkVerifyRow checks a stored child row's foreign keys: each fully
// non-NULL FK tuple must exist in its parent. Runs after the write —
// the transaction rolls the row back if this errors — which is what
// lets a self-referencing row (a node that is its own parent) insert.
func (d *DB) fkVerifyRow(tx *bytdb.Txn, desc *bytdb.TableDesc, vals []any) error {
	for i := range desc.ForeignKeys {
		fk := &desc.ForeignKeys[i]
		refVals := make([]any, len(fk.Cols))
		null := false
		for j, ord := range fk.Cols {
			if refVals[j] = vals[ord]; refVals[j] == nil {
				null = true // MATCH SIMPLE
				break
			}
		}
		if null {
			continue
		}
		found, err := d.fkRowExists(tx, fk.RefTable, fk.RefCols, refVals)
		if err != nil {
			return err
		}
		if !found {
			return serr.New(`insert or update on table "`+desc.Name+
				`" violates foreign key constraint "`+fk.Name+`"`,
				"detail", fkKeyDetail(fk.RefCols, refVals)+` is not present in table "`+fk.RefTable+`"`)
		}
	}
	return nil
}

// fkVerifyReferenced checks, for one former parent-row image, that no
// child row still references it — the DELETE/UPDATE-side of every
// inbound constraint. refs comes from Txn.ReferencingFKs; oldVals is
// the parent row as it was before the statement's write.
func (d *DB) fkVerifyReferenced(tx *bytdb.Txn, desc *bytdb.TableDesc, refs []bytdb.FKRef, oldVals []any) error {
	for _, r := range refs {
		refVals := make([]any, len(r.FK.RefCols))
		null := false
		for i, name := range r.FK.RefCols {
			ord := desc.ColIndex(name)
			if ord < 0 || oldVals[ord] == nil {
				null = true // a NULL key tuple can never be referenced
				break
			}
			refVals[i] = oldVals[ord]
		}
		if null {
			continue
		}
		childCols := make([]string, len(r.FK.Cols))
		for i, ord := range r.FK.Cols {
			childCols[i] = r.Child.Columns[ord].Name
		}
		found, err := d.fkRowExists(tx, r.Child.Name, childCols, refVals)
		if err != nil {
			return err
		}
		if found {
			return serr.New(`update or delete on table "`+desc.Name+
				`" violates foreign key constraint "`+r.FK.Name+`" on table "`+r.Child.Name+`"`,
				"detail", fkKeyDetail(r.FK.RefCols, refVals)+` is still referenced from table "`+r.Child.Name+`"`)
		}
	}
	return nil
}

// fkRowExists reports whether table holds a row with cols = vals,
// through the ordinary planner: an equality conjunct per column, so a
// primary key or index on the columns makes this a point get or
// bounded scan rather than a full scan.
func (d *DB) fkRowExists(tx *bytdb.Txn, table string, cols []string, vals []any) (bool, error) {
	desc := tx.Table(table)
	if desc == nil {
		return false, serr.New("no such table", "table", table)
	}
	preds := make([]BoolExpr, len(cols))
	for i, name := range cols {
		preds[i] = &Pred{Item: SelectItem{Col: ColRef{Name: name}}, Op: OpEQ, Val: vals[i]}
	}
	pl, err := planScan(desc, table, &And{Exprs: preds})
	if err != nil {
		return false, err
	}
	found := false
	err = scanPlan(d.ctx, tx, table, pl, d.tableEnv(tx, table, desc), func(bytdb.Row) bool {
		found = true
		return false
	})
	return found, err
}

// fkKeyDetail renders Postgres's Key (a, b)=(1, 2) detail text.
func fkKeyDetail(cols []string, vals []any) string {
	vs := make([]string, len(vals))
	for i, v := range vals {
		vs[i] = fmt.Sprint(v)
	}
	return "Key (" + strings.Join(cols, ", ") + ")=(" + strings.Join(vs, ", ") + ")"
}

// fkRefColsChanged reports whether any column some inbound FK
// references differs between a parent row's old and new images —
// the trigger for re-checking references on UPDATE.
func fkRefColsChanged(desc *bytdb.TableDesc, refs []bytdb.FKRef, oldVals, newVals []any) bool {
	for _, r := range refs {
		for _, name := range r.FK.RefCols {
			ord := desc.ColIndex(name)
			if ord < 0 {
				continue
			}
			if c, ok := compareVals(oldVals[ord], newVals[ord]); !ok || c != 0 {
				// Incomparable covers NULL transitions: NULL -> value and
				// value -> NULL both change the key tuple.
				if !(oldVals[ord] == nil && newVals[ord] == nil) {
					return true
				}
			}
		}
	}
	return false
}

// resolveFKs maps a CREATE TABLE / ALTER TABLE foreign-key definition
// onto the engine's descriptor form, naming unnamed constraints
// table_col_fkey (numeric suffix on collision) per Postgres.
func resolveFK(desc *bytdb.TableDesc, def FKDef) (bytdb.FKDesc, error) {
	fkd := bytdb.FKDesc{Name: def.Name, RefTable: def.RefTable, RefCols: def.RefCols}
	for _, name := range def.Cols {
		ord := desc.ColIndex(name)
		if ord < 0 {
			return bytdb.FKDesc{}, serr.New("no such column", "table", desc.Name, "column", name)
		}
		fkd.Cols = append(fkd.Cols, ord)
	}
	if fkd.Name == "" {
		taken := map[string]bool{}
		for _, c := range desc.Checks {
			taken[c.Name] = true
		}
		for _, f := range desc.ForeignKeys {
			taken[f.Name] = true
		}
		base := desc.Name + "_" + def.Cols[0] + "_fkey"
		fkd.Name = base
		for n := 1; taken[fkd.Name]; n++ {
			fkd.Name = fmt.Sprintf("%s%d", base, n)
		}
	}
	return fkd, nil
}

// execAddFK handles ALTER TABLE ... ADD FOREIGN KEY: the engine
// validates the shape and every existing row in the transaction that
// publishes the constraint.
func (d *DB) execAddFK(s *AddFK) (*Result, error) {
	desc := d.e.Table(s.Table)
	if desc == nil {
		return nil, serr.New("no such table", "table", s.Table)
	}
	fkd, err := resolveFK(desc, s.FK)
	if err != nil {
		return nil, err
	}
	if err := d.e.AddForeignKey(s.Table, fkd, true); err != nil {
		return nil, err
	}
	return &Result{}, nil
}
