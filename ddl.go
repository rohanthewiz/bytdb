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
	if name == "" {
		return nil, serr.New("table name is required")
	}
	if len(cols) == 0 {
		return nil, serr.New("at least one column is required", "table", name)
	}
	if len(pk) == 0 {
		return nil, serr.New("a primary key is required", "table", name)
	}
	desc := &TableDesc{Name: name, Columns: slices.Clone(cols)}
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
// supply the new arity.
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
	if err := e.writeDesc(table, desc); err != nil {
		return serr.Wrap(err, "op", "add column", "table", table, "column", col.Name)
	}
	return nil
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

// writeDesc persists an updated descriptor and publishes it as the
// last step inside the transaction (callers hold ddlMu).
func (e *Engine) writeDesc(table string, desc *TableDesc) error {
	return e.kv.Update(func(tx *btypedb.Tx[string, []byte]) error {
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
	})
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
