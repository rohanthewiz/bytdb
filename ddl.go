package bytdb

import (
	"slices"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

var validTypes = map[ColType]bool{
	TBool: true, TInt: true, TFloat: true, TString: true, TBytes: true,
	TTimestamp: true, TDate: true, TUUID: true, TTextArray: true,
	TJSONB: true,
}

// CreateTable registers a table. pk names the primary-key columns in
// key order; every name must be a declared column. The descriptor
// write and table-ID allocation commit atomically.
func (e *Engine) CreateTable(name string, cols []Column, pk ...string) (*TableDesc, error) {
	return e.CreateTableWithChecks(name, cols, nil, pk...)
}

// CreateTableWithChecks is CreateTable with CHECK constraints stored
// in the descriptor. The engine treats each check's expression as
// opaque text (see CheckDesc); names must be non-empty and distinct.
func (e *Engine) CreateTableWithChecks(name string, cols []Column, checks []CheckDesc, pk ...string) (*TableDesc, error) {
	if name == "" {
		return nil, serr.New("table name is required")
	}
	if len(cols) == 0 {
		return nil, serr.New("at least one column is required", "table", name)
	}
	if len(pk) == 0 {
		return nil, serr.New("a primary key is required", "table", name)
	}
	desc := &TableDesc{Name: name, Columns: slices.Clone(cols), Checks: slices.Clone(checks)}
	seenCheck := map[string]bool{}
	for _, ck := range checks {
		if ck.Name == "" {
			return nil, serr.New("check constraint name is required", "table", name)
		}
		if seenCheck[ck.Name] {
			return nil, serr.New("duplicate check constraint name", "table", name, "constraint", ck.Name)
		}
		seenCheck[ck.Name] = true
	}
	seen := map[string]bool{}
	for _, c := range cols {
		if c.Name == "" {
			return nil, serr.New("column name is required", "table", name)
		}
		if seen[c.Name] {
			return nil, serr.New("duplicate column", "table", name, "column", c.Name)
		}
		seen[c.Name] = true
		if !validTypes[c.Type] {
			return nil, serr.New("unknown column type", "table", name, "column", c.Name, "type", string(c.Type))
		}
		if c.Identity && c.Type != TInt {
			return nil, serr.New("identity column must be an int column",
				"table", name, "column", c.Name, "type", string(c.Type))
		}
		if err := validMaxLen(c); err != nil {
			return nil, serr.Wrap(err, "table", name)
		}
	}
	for i := range desc.Columns {
		desc.Columns[i].ID = uint32(i + 1)
	}
	desc.NextColID = uint32(len(desc.Columns) + 1)
	for _, p := range pk {
		ord := desc.ColIndex(p)
		if ord < 0 {
			return nil, serr.New("primary key column not declared", "table", name, "column", p)
		}
		if slices.Contains(desc.PKCols, ord) {
			return nil, serr.New("duplicate primary key column", "table", name, "column", p)
		}
		desc.PKCols = append(desc.PKCols, ord)
	}

	err := e.updateDDL(func(tx *btypedb.Tx[string, []byte]) error {
		// The existence check lives inside the transaction: the kv
		// writer lock serializes DDL, so racing CreateTables see each
		// other's committed descriptor, never a stale catalog.
		if tx.Contains(descKey(name)) {
			return serr.New("table already exists", "table", name)
		}
		// Sequences and views share the relation namespace, as in
		// Postgres.
		if tx.Contains(sqlSeqKey(name)) || tx.Contains(viewKey(name)) {
			return serr.New(`relation "` + name + `" already exists`)
		}
		// Allocate the next table ID from the sequence key.
		next, err := nextFromCounter(tx, seqKey(), firstUserTableID, "table-id")
		if err != nil {
			return err
		}
		desc.ID = next
		return writeDescIn(tx, name, desc)
	})
	if err != nil {
		return nil, serr.Wrap(err, "op", "create table", "table", name)
	}
	return desc, nil
}

// validMaxLen checks a column's declared VARCHAR(n) limit: only string
// columns take one, and n must be positive (a zero-or-negative length
// admits no value at all — Postgres rejects it at parse too).
func validMaxLen(c Column) error {
	if c.MaxLen == 0 {
		return nil
	}
	if c.Type != TString {
		return serr.New("length limit is only valid on string columns",
			"column", c.Name, "type", string(c.Type))
	}
	if c.MaxLen < 0 {
		return serr.New("length limit must be positive", "column", c.Name)
	}
	return nil
}

// DropTable removes a table — descriptor, rows, and every index —
// atomically. A table referenced by another table's foreign key
// cannot be dropped (drop the referencing constraint or table first);
// its own foreign keys go with it.
func (e *Engine) DropTable(name string) error {
	err := e.updateDDL(func(tx *btypedb.Tx[string, []byte]) error {
		desc, err := e.descFromView(tx, name)
		if err != nil {
			return err
		}
		refs, err := e.referencingFKs(tx, name, true)
		if err != nil {
			return err
		}
		if len(refs) > 0 {
			return serr.New(`cannot drop table "`+name+`" because other objects depend on it`,
				"detail", `constraint "`+refs[0].FK.Name+`" on table "`+refs[0].Child.Name+
					`" depends on table "`+name+`"`)
		}
		prefix := tableSpace(desc.ID)
		if _, err := tx.DeleteRange(string(prefix), string(tuple.PrefixEnd(prefix))); err != nil {
			return err
		}
		// The table's identity counters live in the system sequences
		// table, outside its own key space.
		idPrefix := identitySeqTablePrefix(desc.ID)
		if _, err := tx.DeleteRange(string(idPrefix), string(tuple.PrefixEnd(idPrefix))); err != nil {
			return err
		}
		_, err = tx.Delete(descKey(name))
		return err
	})
	if err != nil {
		return serr.Wrap(err, "op", "drop table", "table", name)
	}
	return nil
}

// AddColumn appends a column to a table. No rows are rewritten:
// existing rows read the new column as NULL. Subsequent inserts must
// supply the new arity. A NOT NULL column can only be added while the
// table is empty (existing rows would read it as NULL), checked in
// the same transaction that publishes the descriptor.
func (e *Engine) AddColumn(table string, col Column) error {
	if col.Name == "" {
		return serr.New("column name is required", "table", table)
	}
	if !validTypes[col.Type] {
		return serr.New("unknown column type", "table", table, "column", col.Name, "type", string(col.Type))
	}
	if col.Identity {
		// Existing rows would need backfilled values; defer until there
		// is a story for that.
		return serr.New("adding an identity column is not supported", "table", table, "column", col.Name)
	}
	if err := validMaxLen(col); err != nil {
		return serr.Wrap(err, "table", table)
	}
	err := e.alterDesc(table, func(tx *btypedb.Tx[string, []byte], old *TableDesc) (*TableDesc, error) {
		if old.ColIndex(col.Name) >= 0 {
			return nil, serr.New("column already exists", "table", table, "column", col.Name)
		}
		desc := old.clone()
		col.ID = desc.NextColID
		desc.NextColID++
		desc.Columns = append(desc.Columns, col)
		if col.NotNull && hasRows(tx, desc.ID) {
			return nil, serr.New(`column "` + col.Name + `" of relation "` + table +
				`" contains null values`)
		}
		// Postgres backfills existing rows with the default; this engine
		// leaves stored rows untouched (they would read NULL), so the two
		// are only equivalent on an empty table.
		if col.Default != "" && hasRows(tx, desc.ID) {
			return nil, serr.New("adding a column with DEFAULT to a non-empty table is not supported",
				"table", table, "column", col.Name)
		}
		return desc, nil
	})
	if err != nil {
		return serr.Wrap(err, "op", "add column", "table", table, "column", col.Name)
	}
	return nil
}

// hasRows reports whether the table's primary index holds any row in
// tx's view.
func hasRows(tx *btypedb.Tx[string, []byte], tableID uint64) bool {
	prefix := tablePrefix(tableID)
	end := string(tuple.PrefixEnd(prefix))
	for k := range tx.Ascend(string(prefix)) {
		return k < end
	}
	return false
}

// DropColumn removes a column from a table. No rows are rewritten:
// the column's data stays in old row values under its retired ID,
// skipped on decode, and ages out as rows are updated. Key and indexed
// columns cannot be dropped (drop the index first). A later AddColumn
// with the same name gets a fresh ID, so the old data can never
// resurface.
func (e *Engine) DropColumn(table, name string) error {
	err := e.alterDesc(table, func(tx *btypedb.Tx[string, []byte], old *TableDesc) (*TableDesc, error) {
		ord := old.ColIndex(name)
		if ord < 0 {
			return nil, serr.New("no such column", "table", table, "column", name)
		}
		if old.Columns[ord].Identity {
			// The counter goes with the column; the column ID is never
			// reused, so nothing can resurrect it.
			if _, err := tx.Delete(identitySeqKey(old.ID, old.Columns[ord].ID)); err != nil {
				return nil, err
			}
		}
		if old.isPK(ord) {
			return nil, serr.New("cannot drop a primary key column", "table", table, "column", name)
		}
		for i := range old.Indexes {
			if slices.Contains(old.Indexes[i].Cols, ord) {
				return nil, serr.New("cannot drop an indexed column; drop the index first",
					"table", table, "column", name, "index", old.Indexes[i].Name)
			}
		}
		// A column on either side of a foreign key cannot be dropped
		// while the constraint stands (Postgres requires CASCADE).
		for i := range old.ForeignKeys {
			if slices.Contains(old.ForeignKeys[i].Cols, ord) {
				return nil, serr.New("cannot drop a foreign key column; drop the constraint first",
					"table", table, "column", name, "constraint", old.ForeignKeys[i].Name)
			}
		}
		refs, err := e.referencingFKs(tx, table, false)
		if err != nil {
			return nil, err
		}
		for _, r := range refs {
			if slices.Contains(r.FK.RefCols, name) {
				return nil, serr.New("cannot drop a column referenced by a foreign key",
					"table", table, "column", name,
					"constraint", r.FK.Name, "referencing_table", r.Child.Name)
			}
		}
		desc := old.clone()
		desc.Columns = slices.Delete(desc.Columns, ord, ord+1)
		// Ordinal references above the removed column shift down by one.
		for i, p := range desc.PKCols {
			if p > ord {
				desc.PKCols[i] = p - 1
			}
		}
		for i := range desc.Indexes {
			cols := slices.Clone(desc.Indexes[i].Cols)
			for j, c := range cols {
				if c > ord {
					cols[j] = c - 1
				}
			}
			desc.Indexes[i].Cols = cols
		}
		// Foreign-key child columns are stored as ordinals too, and must
		// shift with the same rule. The guard above only refuses dropping a
		// column that IS an FK child column; dropping an unrelated column
		// with a LOWER ordinal is allowed, so without this the FK's Cols
		// would keep pointing one slot too high — silently enforcing the
		// constraint against the wrong column, or (when the child column
		// was the last one) indexing past desc.Columns and panicking.
		for i := range desc.ForeignKeys {
			cols := slices.Clone(desc.ForeignKeys[i].Cols)
			for j, c := range cols {
				if c > ord {
					cols[j] = c - 1
				}
			}
			desc.ForeignKeys[i].Cols = cols
		}
		return desc, nil
	})
	if err != nil {
		return serr.Wrap(err, "op", "drop column", "table", table, "column", name)
	}
	return nil
}

// AddCheck appends a CHECK constraint to a table. The engine treats
// the expression as opaque text (see CheckDesc); validate, when
// non-nil, is called for every existing row inside the transaction
// that publishes the descriptor — its error aborts the add — so the
// caller can verify the constraint holds with no write slipping in
// between. The name must be non-empty and unused.
func (e *Engine) AddCheck(table string, ck CheckDesc, validate func(Row) error) error {
	if ck.Name == "" {
		return serr.New("check constraint name is required", "table", table)
	}
	err := e.alterDesc(table, func(tx *btypedb.Tx[string, []byte], old *TableDesc) (*TableDesc, error) {
		for _, c := range old.Checks {
			if c.Name == ck.Name {
				return nil, serr.New(`constraint "` + ck.Name + `" for relation "` + table +
					`" already exists`)
			}
		}
		desc := old.clone()
		desc.Checks = append(desc.Checks, ck)
		if validate != nil {
			for row, err := range scanRows(tx, old, nil, nil) {
				if err != nil {
					return nil, err
				}
				if err := validate(row); err != nil {
					return nil, err
				}
			}
		}
		return desc, nil
	})
	if err != nil {
		return serr.Wrap(err, "op", "add check", "table", table, "constraint", ck.Name)
	}
	return nil
}

// DropCheck removes the named CHECK constraint, reporting whether it
// existed.
func (e *Engine) DropCheck(table, name string) (bool, error) {
	existed := false
	err := e.alterDesc(table, func(_ *btypedb.Tx[string, []byte], old *TableDesc) (*TableDesc, error) {
		i := slices.IndexFunc(old.Checks, func(c CheckDesc) bool { return c.Name == name })
		if i < 0 {
			return nil, nil // absent: a no-op, not an error
		}
		existed = true
		desc := old.clone()
		desc.Checks = slices.Delete(desc.Checks, i, i+1)
		return desc, nil
	})
	if err != nil {
		return false, serr.Wrap(err, "op", "drop check", "table", table, "constraint", name)
	}
	return existed, nil
}

// RenameTable renames a table. Rows never move — the key space is
// owned by the table ID, and only the descriptor (keyed by name)
// changes. A table referenced by another table's foreign key cannot
// be renamed (RefTable is stored by name; drop the constraint first);
// a self-reference renames along. Views are not tracked: one whose
// stored text names the old table fails at its next use.
func (e *Engine) RenameTable(oldName, newName string) error {
	if newName == "" {
		return serr.New("new table name is required", "table", oldName)
	}
	err := e.updateDDL(func(tx *btypedb.Tx[string, []byte]) error {
		desc, err := e.descFromView(tx, oldName)
		if err != nil {
			return err
		}
		if tx.Contains(descKey(newName)) || tx.Contains(sqlSeqKey(newName)) || tx.Contains(viewKey(newName)) {
			return serr.New(`relation "` + newName + `" already exists`)
		}
		refs, err := e.referencingFKs(tx, oldName, true)
		if err != nil {
			return err
		}
		if len(refs) > 0 {
			return serr.New("cannot rename a table referenced by a foreign key",
				"table", oldName, "constraint", refs[0].FK.Name,
				"referencing_table", refs[0].Child.Name)
		}
		next := desc.clone()
		next.Name = newName
		for i := range next.ForeignKeys {
			if next.ForeignKeys[i].RefTable == oldName { // self-reference follows
				next.ForeignKeys[i].RefTable = newName
			}
		}
		if _, err := tx.Delete(descKey(oldName)); err != nil {
			return err
		}
		return writeDescIn(tx, newName, next)
	})
	if err != nil {
		return serr.Wrap(err, "op", "rename table", "table", oldName, "to", newName)
	}
	return nil
}

// RenameColumn renames a column: descriptor-only, since row values are
// tagged by column ID. Columns that CHECK constraints mention (their
// expressions are stored as text) or that a foreign key references
// from another table (RefCols are names) cannot be renamed while the
// constraint stands; the caller enforces the check side, which owns
// the expression language — this guards the FK side.
func (e *Engine) RenameColumn(table, oldName, newName string) error {
	if newName == "" {
		return serr.New("new column name is required", "table", table, "column", oldName)
	}
	err := e.alterDesc(table, func(tx *btypedb.Tx[string, []byte], old *TableDesc) (*TableDesc, error) {
		ord := old.ColIndex(oldName)
		if ord < 0 {
			return nil, serr.New("no such column", "table", table, "column", oldName)
		}
		if old.ColIndex(newName) >= 0 {
			return nil, serr.New(`column "` + newName + `" of relation "` + table + `" already exists`)
		}
		refs, err := e.referencingFKs(tx, table, false)
		if err != nil {
			return nil, err
		}
		for _, r := range refs {
			if slices.Contains(r.FK.RefCols, oldName) {
				if r.Child.Name == table {
					// The table's own inbound reference renames along below.
					continue
				}
				return nil, serr.New("cannot rename a column referenced by a foreign key",
					"table", table, "column", oldName,
					"constraint", r.FK.Name, "referencing_table", r.Child.Name)
			}
		}
		desc := old.clone()
		desc.Columns[ord].Name = newName
		for i := range desc.ForeignKeys {
			if desc.ForeignKeys[i].RefTable != table {
				continue
			}
			cols := slices.Clone(desc.ForeignKeys[i].RefCols)
			for j, n := range cols {
				if n == oldName {
					cols[j] = newName
				}
			}
			desc.ForeignKeys[i].RefCols = cols
		}
		return desc, nil
	})
	if err != nil {
		return serr.Wrap(err, "op", "rename column", "table", table, "column", oldName)
	}
	return nil
}

// alterDesc runs one descriptor-rewriting DDL: it resolves the current
// descriptor inside the transaction, applies mutate, and persists the
// result — all under the kv writer lock, so concurrent DDL serializes
// and each mutation builds on the committed descriptor before it.
// mutate returning (nil, nil) means "nothing to change": the
// transaction commits without a descriptor write.
func (e *Engine) alterDesc(table string, mutate func(tx *btypedb.Tx[string, []byte], old *TableDesc) (*TableDesc, error)) error {
	return e.updateDDL(func(tx *btypedb.Tx[string, []byte]) error {
		old, err := e.descFromView(tx, table)
		if err != nil {
			return err
		}
		desc, err := mutate(tx, old)
		if err != nil || desc == nil {
			return err
		}
		return writeDescIn(tx, table, desc)
	})
}

// writeDescIn stages the descriptor write within a DDL transaction.
// Committing it is what publishes the schema change: any reader or
// writer serialized after the commit resolves the new descriptor from
// its own snapshot.
func writeDescIn(tx *btypedb.Tx[string, []byte], table string, desc *TableDesc) error {
	blob, err := marshalDesc(desc)
	if err != nil {
		return err
	}
	return tx.Set(descKey(table), blob)
}

// Tables returns the names of all tables, sorted, as of the committed
// state at the call.
func (e *Engine) Tables() []string {
	var names []string
	e.kv.View(func(tx *btypedb.Tx[string, []byte]) error {
		prefix := descTablePrefix()
		end := string(tuple.PrefixEnd([]byte(prefix)))
		// Descriptor keys are tuple-encoded by name, so key order is
		// name order — no re-sort needed.
		for k := range tx.Ascend(prefix) {
			if k >= end {
				break
			}
			names = append(names, descKeyName(k))
		}
		return nil
	})
	return names
}

// Table returns the descriptor for a table name, or nil if absent —
// resolved from the committed state at the call. Transactions should
// use Txn.Table, which resolves from their own snapshot.
func (e *Engine) Table(name string) *TableDesc {
	var desc *TableDesc
	e.kv.View(func(tx *btypedb.Tx[string, []byte]) error {
		var err error
		desc, err = e.tableFromView(tx, name)
		return err
	})
	return desc
}
