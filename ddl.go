package bytdb

import (
	"encoding/binary"
	"encoding/json"
	"slices"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

var validTypes = map[ColType]bool{
	TBool: true, TInt: true, TFloat: true, TString: true, TBytes: true,
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

	e.ddlMu.Lock()
	defer e.ddlMu.Unlock()
	if e.Table(name) != nil {
		return nil, serr.New("table already exists", "table", name)
	}
	err := e.kv.Update(func(tx *btypedb.Tx[string, []byte]) error {
		// Allocate the next table ID from the sequence key.
		next := uint64(firstUserTableID)
		if raw, ok := tx.Get(seqKey()); ok {
			next = binary.BigEndian.Uint64(raw)
		}
		desc.ID = next
		if err := tx.Set(seqKey(), binary.BigEndian.AppendUint64(nil, next+1)); err != nil {
			return err
		}
		blob, err := json.Marshal(desc)
		if err != nil {
			return serr.Wrap(err, "op", "encode table descriptor")
		}
		return tx.Set(descKey(name), blob)
	})
	if err != nil {
		return nil, serr.Wrap(err, "op", "create table", "table", name)
	}
	e.mu.Lock()
	e.tables[name] = desc
	e.mu.Unlock()
	return desc, nil
}

// DropTable removes a table — descriptor, rows, and every index —
// atomically.
func (e *Engine) DropTable(name string) error {
	e.ddlMu.Lock()
	defer e.ddlMu.Unlock()
	desc := e.Table(name)
	if desc == nil {
		return serr.New("no such table", "table", name)
	}
	prefix := tableSpace(desc.ID)
	err := e.kv.Update(func(tx *btypedb.Tx[string, []byte]) error {
		if _, err := tx.DeleteRange(string(prefix), string(tuple.PrefixEnd(prefix))); err != nil {
			return err
		}
		if _, err := tx.Delete(descKey(name)); err != nil {
			return err
		}
		e.mu.Lock()
		delete(e.tables, name)
		e.mu.Unlock()
		return nil
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
	e.ddlMu.Lock()
	defer e.ddlMu.Unlock()
	old := e.Table(table)
	if old == nil {
		return serr.New("no such table", "table", table)
	}
	if col.Name == "" {
		return serr.New("column name is required", "table", table)
	}
	if old.ColIndex(col.Name) >= 0 {
		return serr.New("column already exists", "table", table, "column", col.Name)
	}
	if !validTypes[col.Type] {
		return serr.New("unknown column type", "table", table, "column", col.Name, "type", string(col.Type))
	}
	desc := old.clone()
	col.ID = desc.NextColID
	desc.NextColID++
	desc.Columns = append(desc.Columns, col)
	err := e.kv.Update(func(tx *btypedb.Tx[string, []byte]) error {
		if col.NotNull && hasRows(tx, desc.ID) {
			return serr.New(`column "` + col.Name + `" of relation "` + table +
				`" contains null values`)
		}
		return e.writeDescIn(tx, table, desc)
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
	e.ddlMu.Lock()
	defer e.ddlMu.Unlock()
	old := e.Table(table)
	if old == nil {
		return serr.New("no such table", "table", table)
	}
	ord := old.ColIndex(name)
	if ord < 0 {
		return serr.New("no such column", "table", table, "column", name)
	}
	if old.isPK(ord) {
		return serr.New("cannot drop a primary key column", "table", table, "column", name)
	}
	for i := range old.Indexes {
		if slices.Contains(old.Indexes[i].Cols, ord) {
			return serr.New("cannot drop an indexed column; drop the index first",
				"table", table, "column", name, "index", old.Indexes[i].Name)
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
	if err := e.writeDesc(table, desc); err != nil {
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
	e.ddlMu.Lock()
	defer e.ddlMu.Unlock()
	old := e.Table(table)
	if old == nil {
		return serr.New("no such table", "table", table)
	}
	if ck.Name == "" {
		return serr.New("check constraint name is required", "table", table)
	}
	for _, c := range old.Checks {
		if c.Name == ck.Name {
			return serr.New(`constraint "` + ck.Name + `" for relation "` + table +
				`" already exists`)
		}
	}
	desc := old.clone()
	desc.Checks = append(desc.Checks, ck)
	err := e.kv.Update(func(tx *btypedb.Tx[string, []byte]) error {
		if validate != nil {
			for row, err := range scanRows(tx, old, nil, nil) {
				if err != nil {
					return err
				}
				if err := validate(row); err != nil {
					return err
				}
			}
		}
		return e.writeDescIn(tx, table, desc)
	})
	if err != nil {
		return serr.Wrap(err, "op", "add check", "table", table, "constraint", ck.Name)
	}
	return nil
}

// DropCheck removes the named CHECK constraint, reporting whether it
// existed.
func (e *Engine) DropCheck(table, name string) (bool, error) {
	e.ddlMu.Lock()
	defer e.ddlMu.Unlock()
	old := e.Table(table)
	if old == nil {
		return false, serr.New("no such table", "table", table)
	}
	i := slices.IndexFunc(old.Checks, func(c CheckDesc) bool { return c.Name == name })
	if i < 0 {
		return false, nil
	}
	desc := old.clone()
	desc.Checks = slices.Delete(desc.Checks, i, i+1)
	if err := e.writeDesc(table, desc); err != nil {
		return false, serr.Wrap(err, "op", "drop check", "table", table, "constraint", name)
	}
	return true, nil
}

// writeDesc persists an updated descriptor and publishes it as the
// last step inside the transaction (callers hold ddlMu).
func (e *Engine) writeDesc(table string, desc *TableDesc) error {
	return e.kv.Update(func(tx *btypedb.Tx[string, []byte]) error {
		return e.writeDescIn(tx, table, desc)
	})
}

// writeDescIn stages the descriptor write and publishes it within an
// existing transaction (callers hold ddlMu).
func (e *Engine) writeDescIn(tx *btypedb.Tx[string, []byte], table string, desc *TableDesc) error {
	blob, err := json.Marshal(desc)
	if err != nil {
		return serr.Wrap(err, "op", "encode table descriptor")
	}
	if err := tx.Set(descKey(table), blob); err != nil {
		return err
	}
	e.mu.Lock()
	e.tables[table] = desc
	e.mu.Unlock()
	return nil
}

// Tables returns the names of all tables, sorted.
func (e *Engine) Tables() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	names := make([]string, 0, len(e.tables))
	for name := range e.tables {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Table returns the descriptor for a table name, or nil if absent.
func (e *Engine) Table(name string) *TableDesc {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.tables[name]
}
