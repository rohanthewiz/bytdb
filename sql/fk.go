package sql

// fk.go: row-level foreign-key enforcement. The engine stores FKDesc
// on the child table and guards the schema side (see bytdb/fk.go);
// this file checks references as rows change, inside the statement's
// own transaction:
//
//	child INSERT/UPDATE  -> the referenced parent row must exist
//	parent DELETE        -> ON DELETE CASCADE children are deleted too;
//	                        for the rest, no child row may still
//	                        reference the old key
//	parent UPDATE        -> no child row may still reference the old key
//	TRUNCATE             -> a referenced table needs its children in the list
//
// Semantics are MATCH SIMPLE with NO ACTION checked at end of
// statement: a NULL in any child FK column satisfies the constraint,
// and parent-side checks run after the statement's writes — cascaded
// deletes included — so a statement that deletes both a row and
// everything referencing it is legal, as in Postgres.

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
		refVals, ok := fkRefTuple(desc, r.FK.RefCols, oldVals)
		if !ok {
			continue // a NULL key tuple can never be referenced
		}
		childCols := fkChildCols(&r)
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

// fkRefTuple extracts a parent row's referenced-key tuple by column
// name. ok is false when any component is NULL (or the column is
// somehow gone): a NULL key tuple can never be referenced, so callers
// skip the row.
func fkRefTuple(desc *bytdb.TableDesc, refCols []string, vals []any) (refVals []any, ok bool) {
	refVals = make([]any, len(refCols))
	for i, name := range refCols {
		ord := desc.ColIndex(name)
		if ord < 0 || vals[ord] == nil {
			return nil, false
		}
		refVals[i] = vals[ord]
	}
	return refVals, true
}

// fkChildCols names the referencing columns of one inbound FK (they
// are stored as ordinals on the child descriptor).
func fkChildCols(r *bytdb.FKRef) []string {
	cols := make([]string, len(r.FK.Cols))
	for i, ord := range r.FK.Cols {
		cols[i] = r.Child.Columns[ord].Name
	}
	return cols
}

// fkAfterDelete is the parent side of DELETE, run after the
// statement's own deletes: first every ON DELETE CASCADE constraint
// is honored by deleting the referencing rows — transitively, since a
// cascaded row may itself be a parent — and then the remaining
// (NO ACTION/RESTRICT) inbound constraints are verified against every
// row the statement removed, cascaded rows included. Doing the checks
// last preserves the end-of-statement semantics: a NO ACTION
// grandchild whose parent was cascade-deleted blocks the whole
// statement, while a statement that deletes a row and everything
// referencing it stays legal.
//
// The worklist terminates even through FK cycles and self-references:
// a table is re-queued only when rows were actually deleted from it,
// rows are finite, and a revisited scan runs against the transaction's
// own view, so already-deleted rows are simply not found.
//
// Cascaded rows do not count toward the statement's RowsAffected and
// do not appear in RETURNING, matching Postgres.
func (d *DB) fkAfterDelete(tx *bytdb.Txn, desc *bytdb.TableDesc, refs []bytdb.FKRef, rows [][]any) error {
	type deleted struct {
		desc *bytdb.TableDesc
		refs []bytdb.FKRef // all inbound FKs of desc
		rows [][]any       // old images of rows this statement removed
	}
	pending := []deleted{{desc: desc, refs: refs, rows: rows}}
	var done []deleted
	for len(pending) > 0 {
		cur := pending[0]
		pending = pending[1:]
		done = append(done, cur)
		for _, r := range cur.refs {
			if r.FK.OnDelete != bytdb.FKCascade {
				continue
			}
			childCols := fkChildCols(&r)
			var childRows [][]any
			for _, old := range cur.rows {
				refVals, ok := fkRefTuple(cur.desc, r.FK.RefCols, old)
				if !ok {
					continue
				}
				got, err := d.fkDeleteMatching(tx, r.Child, childCols, refVals)
				if err != nil {
					return err
				}
				childRows = append(childRows, got...)
			}
			if len(childRows) == 0 {
				continue
			}
			childRefs, err := tx.ReferencingFKs(r.Child.Name)
			if err != nil {
				return err
			}
			pending = append(pending, deleted{desc: r.Child, refs: childRefs, rows: childRows})
		}
	}
	// End-of-statement verification of the non-cascade constraints.
	// Cascade constraints need none: their referencing rows were just
	// deleted, which is the enforcement.
	for _, ds := range done {
		var restrict []bytdb.FKRef
		for _, r := range ds.refs {
			if r.FK.OnDelete != bytdb.FKCascade {
				restrict = append(restrict, r)
			}
		}
		if len(restrict) == 0 {
			continue
		}
		for _, old := range ds.rows {
			if err := d.fkVerifyReferenced(tx, ds.desc, restrict, old); err != nil {
				return err
			}
		}
	}
	return nil
}

// fkDeleteMatching deletes every row of desc whose cols equal vals and
// returns the old images of the rows actually removed. The lookup goes
// through the ordinary planner, so an index on the child FK columns
// makes each cascade probe a point get or bounded scan; without one it
// is a full scan per deleted parent key, the same cost the verify path
// already pays.
func (d *DB) fkDeleteMatching(tx *bytdb.Txn, desc *bytdb.TableDesc, cols []string, vals []any) ([][]any, error) {
	preds := make([]BoolExpr, len(cols))
	for i, name := range cols {
		preds[i] = &Pred{Item: SelectItem{Col: ColRef{Name: name}}, Op: OpEQ, Val: vals[i]}
	}
	pl, err := planScan(desc, desc.Name, &And{Exprs: preds})
	if err != nil {
		return nil, err
	}
	var rows [][]any
	pks, err := collectPKs(d.ctx, tx, desc.Name, desc, pl, d.tableEnv(tx, desc.Name, desc), func(rowVals []any) {
		rows = append(rows, rowVals)
	})
	if err != nil {
		return nil, err
	}
	removed := make([][]any, 0, len(rows))
	for i, pk := range pks {
		ok, err := tx.Delete(desc.Name, pk...)
		if err != nil {
			return nil, err
		}
		if ok {
			removed = append(removed, rows[i])
		}
	}
	return removed, nil
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
	fkd := bytdb.FKDesc{Name: def.Name, RefTable: def.RefTable, RefCols: def.RefCols, OnDelete: def.OnDelete}
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
