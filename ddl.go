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
