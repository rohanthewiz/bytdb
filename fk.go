package bytdb

// fk.go: foreign-key constraints at the engine level — storing FKDesc
// on the child descriptor, validating shape (and optionally existing
// rows) when one is added, and guarding schema changes that would
// orphan a constraint. Row-level enforcement on INSERT/UPDATE/DELETE
// belongs to the SQL layer; see FKDesc.

import (
	"fmt"
	"slices"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

// AddForeignKey appends a foreign-key constraint to the child table.
// fk.RefCols empty means the parent's primary key. The referenced
// columns must be the parent's primary key or the columns of one of
// its unique indexes, and each child column's type must match its
// referenced column's. With validateRows, every existing child row is
// checked against the parent inside the transaction that publishes
// the descriptor — a dangling reference aborts the add.
func (e *Engine) AddForeignKey(table string, fk FKDesc, validateRows bool) error {
	if fk.Name == "" {
		return serr.New("foreign key constraint name is required", "table", table)
	}
	if len(fk.Cols) == 0 {
		return serr.New("foreign key needs at least one column", "table", table)
	}
	if fk.OnDelete != "" && fk.OnDelete != FKCascade {
		// The descriptor is persisted; an unknown action stored today
		// would read as a silent NO ACTION later, so refuse it here.
		return serr.New("unsupported ON DELETE action", "table", table, "action", fk.OnDelete)
	}
	err := e.alterDesc(table, func(tx *btypedb.Tx[string, []byte], old *TableDesc) (*TableDesc, error) {
		for _, c := range old.Checks {
			if c.Name == fk.Name {
				return nil, serr.New(`constraint "` + fk.Name + `" for relation "` + table + `" already exists`)
			}
		}
		for _, f := range old.ForeignKeys {
			if f.Name == fk.Name {
				return nil, serr.New(`constraint "` + fk.Name + `" for relation "` + table + `" already exists`)
			}
		}
		for _, ord := range fk.Cols {
			if ord < 0 || ord >= len(old.Columns) {
				return nil, serr.New("foreign key column ordinal out of range", "table", table)
			}
		}
		// A self-reference resolves to the pre-change descriptor, which
		// is fine: the FK does not change columns or keys.
		parent := old
		if fk.RefTable != table {
			p, err := e.tableFromView(tx, fk.RefTable)
			if err != nil {
				return nil, err
			}
			if p == nil {
				return nil, serr.New("no such table", "table", fk.RefTable)
			}
			parent = p
		}
		if len(fk.RefCols) == 0 {
			for _, ord := range parent.PKCols {
				fk.RefCols = append(fk.RefCols, parent.Columns[ord].Name)
			}
		}
		if len(fk.RefCols) != len(fk.Cols) {
			return nil, serr.New("number of referencing and referenced columns for foreign key disagree",
				"table", table, "constraint", fk.Name)
		}
		for i, name := range fk.RefCols {
			pOrd := parent.ColIndex(name)
			if pOrd < 0 {
				return nil, serr.New("no such column", "table", fk.RefTable, "column", name)
			}
			ct, pt := old.Columns[fk.Cols[i]].Type, parent.Columns[pOrd].Type
			if ct != pt {
				return nil, serr.New("foreign key column types do not match",
					"table", table, "column", old.Columns[fk.Cols[i]].Name,
					"type", string(ct), "referenced_type", string(pt))
			}
		}
		if !uniqueKeyCovers(parent, fk.RefCols) {
			// Postgres's wording: uniqueness is what makes "the
			// referenced row" a well-defined thing.
			return nil, serr.New(`there is no unique constraint matching given keys for referenced table "` +
				fk.RefTable + `"`)
		}
		if validateRows {
			if err := validateExistingRefs(tx, old, parent, &fk); err != nil {
				return nil, err
			}
		}
		desc := old.clone()
		desc.ForeignKeys = append(desc.ForeignKeys, fk)
		return desc, nil
	})
	if err != nil {
		return serr.Wrap(err, "op", "add foreign key", "table", table, "constraint", fk.Name)
	}
	return nil
}

// uniqueKeyCovers reports whether cols is exactly the column set of
// the table's primary key or of one of its unique indexes (order-
// insensitive: uniqueness of a column set is order-free).
func uniqueKeyCovers(d *TableDesc, cols []string) bool {
	want := map[string]bool{}
	for _, c := range cols {
		want[c] = true
	}
	sameSet := func(ords []int) bool {
		if len(ords) != len(want) {
			return false
		}
		for _, ord := range ords {
			if !want[d.Columns[ord].Name] {
				return false
			}
		}
		return true
	}
	if sameSet(d.PKCols) {
		return true
	}
	for i := range d.Indexes {
		if d.Indexes[i].Unique && sameSet(d.Indexes[i].Cols) {
			return true
		}
	}
	return false
}

// validateExistingRefs checks every existing child row against the
// parent within the DDL transaction. The parent's referenced tuples
// are materialized into a set first, so the pass is one scan of each
// table instead of a parent probe per child row.
func validateExistingRefs(tx *btypedb.Tx[string, []byte], child, parent *TableDesc, fk *FKDesc) error {
	refOrds := make([]int, len(fk.RefCols))
	for i, name := range fk.RefCols {
		refOrds[i] = parent.ColIndex(name)
	}
	keys := map[string]bool{}
	for row, err := range scanRows(tx, parent, nil, nil) {
		if err != nil {
			return err
		}
		vals := make([]any, len(refOrds))
		skip := false
		for i, ord := range refOrds {
			if vals[i] = row.Vals[ord]; vals[i] == nil {
				skip = true // a NULL key tuple can never be referenced
				break
			}
		}
		if skip {
			continue
		}
		kb, err := tuple.Encode(vals...)
		if err != nil {
			return err
		}
		keys[string(kb)] = true
	}
	for row, err := range scanRows(tx, child, nil, nil) {
		if err != nil {
			return err
		}
		vals := make([]any, len(fk.Cols))
		null := false
		for i, ord := range fk.Cols {
			if vals[i] = row.Vals[ord]; vals[i] == nil {
				null = true // MATCH SIMPLE: any NULL satisfies the FK
				break
			}
		}
		if null {
			continue
		}
		kb, err := tuple.Encode(vals...)
		if err != nil {
			return err
		}
		if !keys[string(kb)] {
			return serr.New(`insert or update on table "`+child.Name+
				`" violates foreign key constraint "`+fk.Name+`"`,
				"detail", fmt.Sprintf("some row's key is not present in table %q", fk.RefTable))
		}
	}
	return nil
}

// DropForeignKey removes the named foreign-key constraint, reporting
// whether it existed.
func (e *Engine) DropForeignKey(table, name string) (bool, error) {
	existed := false
	err := e.alterDesc(table, func(_ *btypedb.Tx[string, []byte], old *TableDesc) (*TableDesc, error) {
		i := slices.IndexFunc(old.ForeignKeys, func(f FKDesc) bool { return f.Name == name })
		if i < 0 {
			return nil, nil // absent: a no-op, not an error
		}
		existed = true
		desc := old.clone()
		desc.ForeignKeys = slices.Delete(desc.ForeignKeys, i, i+1)
		return desc, nil
	})
	if err != nil {
		return false, serr.Wrap(err, "op", "drop foreign key", "table", table, "constraint", name)
	}
	return existed, nil
}

// FKRef is one inbound reference: a foreign key on Child pointing at
// some parent table.
type FKRef struct {
	Child *TableDesc
	FK    *FKDesc
}

// referencingFKs collects, from the view's catalog, every foreign key
// on any table that references the named table. skipSelf drops the
// table's references to itself (a table being dropped takes its own
// FKs with it).
func (e *Engine) referencingFKs(v kvView, table string, skipSelf bool) ([]FKRef, error) {
	prefix := descTablePrefix()
	end := string(tuple.PrefixEnd([]byte(prefix)))
	var refs []FKRef
	for k := range v.Ascend(prefix) {
		if k >= end {
			break
		}
		desc, err := e.tableFromView(v, descKeyName(k))
		if err != nil {
			return nil, err
		}
		if desc == nil || (skipSelf && desc.Name == table) {
			continue
		}
		for i := range desc.ForeignKeys {
			if desc.ForeignKeys[i].RefTable == table {
				refs = append(refs, FKRef{Child: desc, FK: &desc.ForeignKeys[i]})
			}
		}
	}
	return refs, nil
}
