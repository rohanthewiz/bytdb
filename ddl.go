package bytdb

import (
	"slices"
	"time"

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
	// Captured for the post-commit allocator invalidation below; a
	// retried closure just overwrites it with the same ID.
	var droppedID uint64
	err := e.updateDDL(func(tx *btypedb.Tx[string, []byte]) error {
		desc, err := e.descFromView(tx, name)
		if err != nil {
			return err
		}
		droppedID = desc.ID
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
	// In concurrent-writes mode the table's identity counters may have
	// live in-memory allocators; drop them so no cached draw can write
	// the deleted keys back.
	e.invalidateCounterPrefix(string(identitySeqTablePrefix(droppedID)))
	e.cacheEvict(name)
	return nil
}

// AddColumn appends a column to a table. Without a DEFAULT no rows
// are rewritten: existing rows read the new column as NULL.
// Subsequent inserts must supply the new arity.
//
// With a DEFAULT, existing rows are backfilled with its value in the
// same transaction that publishes the descriptor, so the column reads
// back the way Postgres would read it — the whole statement is atomic,
// and a failure part-way leaves neither the rows nor the descriptor
// changed. That makes NOT NULL DEFAULT x legal on a non-empty table
// too; NOT NULL without a (non-NULL) default still requires an empty
// table, since existing rows would read as NULL.
//
// The backfill costs one rewrite per row. It is skipped entirely when
// the default evaluates to NULL, which is what stored rows already
// read.
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
		// The default's value is wanted twice over: to decide whether
		// existing rows can satisfy NOT NULL, and to write into them.
		// A default the engine cannot evaluate (see default.go) fails
		// the statement here rather than leaving rows reading NULL
		// while new inserts get the default.
		var fill any
		if col.Default != "" {
			var err error
			// One instant for the entire statement, so every backfilled
			// row shares a single now() — the same granularity a
			// multi-row INSERT gets.
			if fill, err = columnDefaultValue(&col, time.Now().UTC()); err != nil {
				return nil, err
			}
		}
		if fill == nil && col.NotNull && hasRows(tx, desc.ID) {
			return nil, notNullValueErr(table, col.Name)
		}
		if fill != nil {
			if err := backfillColumn(tx, desc, col.ID, fill); err != nil {
				return nil, err
			}
		}
		return desc, nil
	})
	if err != nil {
		return serr.Wrap(err, "op", "add column", "table", table, "column", col.Name)
	}
	return nil
}

// backfillColumn writes val into every existing row of the table as
// the new column's value. Rows are stored as a sparse sequence of
// (column ID, value) pairs (see encodeRowValue), and the column ID is
// freshly allocated, so no row can already carry a pair for it: the
// new pair is appended to the stored value as-is, with no decode and
// re-encode of the columns already there. Appending also keeps the
// pairs in column order, which is how encodeRowValue writes them,
// though decodeRow does not depend on that.
//
// No index maintenance is needed: the column is brand new, so no
// index can cover it, and no other column's value moves.
//
// Keys are collected before any write rather than written during the
// scan, so the iteration is never walking a tree it is mutating.
func backfillColumn(tx *btypedb.Tx[string, []byte], desc *TableDesc, colID uint32, val any) error {
	prefix := tablePrefix(desc.ID)
	end := string(tuple.PrefixEnd(prefix))
	var keys []string
	var vals [][]byte
	for k, v := range tx.Ascend(string(prefix)) {
		if k >= end {
			break
		}
		keys = append(keys, k)
		// The iterator's value may alias storage; the appended copy
		// below is what gets written, so never append in place.
		vals = append(vals, v)
	}
	for i, k := range keys {
		buf, err := tuple.Append(append([]byte(nil), vals[i]...), int64(colID), val)
		if err != nil {
			return serr.Wrap(err, "op", "backfill column default", "table", desc.Name)
		}
		if err := tx.Set(k, buf); err != nil {
			return serr.Wrap(err, "op", "backfill column default", "table", desc.Name)
		}
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
	// Captured for the post-commit allocator invalidation below ("" when
	// the dropped column had no identity counter).
	var droppedCounterKey string
	err := e.alterDesc(table, func(tx *btypedb.Tx[string, []byte], old *TableDesc) (*TableDesc, error) {
		ord := old.ColIndex(name)
		if ord < 0 {
			return nil, serr.New("no such column", "table", table, "column", name)
		}
		if old.Columns[ord].Identity {
			// The counter goes with the column; the column ID is never
			// reused, so nothing can resurrect it.
			droppedCounterKey = identitySeqKey(old.ID, old.Columns[ord].ID)
			if _, err := tx.Delete(droppedCounterKey); err != nil {
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
	if droppedCounterKey != "" {
		// Concurrent-writes mode: no cached draw may write the deleted
		// counter key back.
		e.invalidateCounter(droppedCounterKey)
	}
	return nil
}

// SetColumnDefault sets or replaces a column's DEFAULT, given as the
// SQL literal text the descriptor stores (what CREATE TABLE's DEFAULT
// clause renders to: 'a string', 42, true, or the evaluated markers
// now() / current_date). Passing "" is the same as DropColumnDefault.
//
// Existing rows are not touched, as in Postgres — a DEFAULT only
// supplies values for inserts that omit the column. Backfilling old
// rows is AddColumn's job, or an explicit UPDATE.
//
// The literal is validated against the column type here rather than
// at the first insert, so a typo fails the DDL statement. That means
// only defaults the engine can evaluate (see default.go) are
// accepted.
func (e *Engine) SetColumnDefault(table, column, literal string) error {
	if literal == "" {
		return e.DropColumnDefault(table, column)
	}
	err := e.alterDesc(table, func(tx *btypedb.Tx[string, []byte], old *TableDesc) (*TableDesc, error) {
		ord := old.ColIndex(column)
		if ord < 0 {
			return nil, serr.New("no such column", "table", table, "column", column)
		}
		if old.Columns[ord].Identity {
			// The identity counter is the column's value source; a
			// DEFAULT alongside it would be a second, conflicting one.
			return nil, serr.New("conflicting DEFAULT for identity column",
				"table", table, "column", column)
		}
		desc := old.clone()
		desc.Columns[ord].Default = literal
		probe := desc.Columns[ord]
		if _, err := columnDefaultValue(&probe, time.Now().UTC()); err != nil {
			return nil, err
		}
		return desc, nil
	})
	if err != nil {
		return serr.Wrap(err, "op", "set column default", "table", table, "column", column)
	}
	return nil
}

// DropColumnDefault clears a column's DEFAULT. Like Postgres's DROP
// DEFAULT it succeeds whether or not one was set, and leaves stored
// rows alone: columns already written keep their values, and later
// inserts that omit the column get NULL (which a NOT NULL column then
// rejects).
func (e *Engine) DropColumnDefault(table, column string) error {
	err := e.alterDesc(table, func(tx *btypedb.Tx[string, []byte], old *TableDesc) (*TableDesc, error) {
		ord := old.ColIndex(column)
		if ord < 0 {
			return nil, serr.New("no such column", "table", table, "column", column)
		}
		if old.Columns[ord].Default == "" {
			return nil, nil // nothing to publish; alterDesc skips the write
		}
		desc := old.clone()
		desc.Columns[ord].Default = ""
		return desc, nil
	})
	if err != nil {
		return serr.Wrap(err, "op", "drop column default", "table", table, "column", column)
	}
	return nil
}

// SetColumnNotNull marks a column NOT NULL, validating every existing
// row inside the transaction that publishes the flag: a single NULL
// aborts the statement with Postgres's wording (SQLSTATE 23502), and
// no write can slip in between the check and the publish.
//
// This is the cheap half of the "add a required column" migration on
// a large table: the scan is read-only, so unlike AddColumn's DEFAULT
// backfill it rewrites nothing and holds nothing but the descriptor
// in memory. The usual sequence is ADD COLUMN (O(1)) -> SET DEFAULT
// (O(1)) -> fill the rows in batches -> SET NOT NULL.
func (e *Engine) SetColumnNotNull(table, column string) error {
	err := e.alterDesc(table, func(tx *btypedb.Tx[string, []byte], old *TableDesc) (*TableDesc, error) {
		ord := old.ColIndex(column)
		if ord < 0 {
			return nil, serr.New("no such column", "table", table, "column", column)
		}
		if old.Columns[ord].NotNull {
			return nil, nil // already set; nothing to publish
		}
		// A key column is non-NULL by construction (coercePK rejects
		// NULL), so the scan has nothing to find.
		if !old.isPK(ord) {
			ok, err := columnHasNoNulls(tx, old, old.Columns[ord].ID)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, notNullValueErr(table, column)
			}
		}
		desc := old.clone()
		desc.Columns[ord].NotNull = true
		return desc, nil
	})
	if err != nil {
		return serr.Wrap(err, "op", "set not null", "table", table, "column", column)
	}
	return nil
}

// DropColumnNotNull clears a column's NOT NULL flag. Descriptor-only
// and always cheap. A primary-key column cannot lose it (the key
// encoding has no NULL to encode), nor can an identity column, whose
// counter is what guarantees a value.
func (e *Engine) DropColumnNotNull(table, column string) error {
	err := e.alterDesc(table, func(tx *btypedb.Tx[string, []byte], old *TableDesc) (*TableDesc, error) {
		ord := old.ColIndex(column)
		if ord < 0 {
			return nil, serr.New("no such column", "table", table, "column", column)
		}
		if old.isPK(ord) {
			return nil, serr.New(`column "`+column+`" is in a primary key`,
				"table", table, "column", column)
		}
		if old.Columns[ord].Identity {
			return nil, serr.New("an identity column is always NOT NULL",
				"table", table, "column", column)
		}
		if !old.Columns[ord].NotNull {
			return nil, nil // nothing to publish
		}
		desc := old.clone()
		desc.Columns[ord].NotNull = false
		return desc, nil
	})
	if err != nil {
		return serr.Wrap(err, "op", "drop not null", "table", table, "column", column)
	}
	return nil
}

// notNullValueErr is the "existing rows violate the new constraint"
// error, worded as Postgres words it (SQLSTATE 23502, the same
// wording AddColumn uses for the equivalent case).
func notNullValueErr(table, column string) error {
	return serr.New(`column "` + column + `" of relation "` + table +
		`" contains null values`)
}

// columnHasNoNulls reports whether every row in the table holds a
// value for the column with the given stable ID.
//
// It reads the stored value tuples directly rather than going through
// decodeRow: a NULL column is simply omitted from a row's value
// (encodeRowValue), so the presence of a pair tagged with this column
// ID is exactly what "not NULL" means. That keeps the validation scan
// to one tuple decode per row and no row materialization at all —
// which is the point of SET NOT NULL on a table too large to rewrite.
func columnHasNoNulls(tx *btypedb.Tx[string, []byte], desc *TableDesc, colID uint32) (bool, error) {
	prefix := tablePrefix(desc.ID)
	end := string(tuple.PrefixEnd(prefix))
	for k, v := range tx.Ascend(string(prefix)) {
		if k >= end {
			break
		}
		pairs, err := tuple.Decode(v)
		if err != nil {
			return false, serr.Wrap(err, "op", "validate not null", "table", desc.Name)
		}
		found := false
		for j := 0; j+1 < len(pairs); j += 2 {
			if id, ok := pairs[j].(int64); ok && uint32(id) == colID && pairs[j+1] != nil {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
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
	e.cacheEvict(oldName)
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
