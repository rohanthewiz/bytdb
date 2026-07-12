package bytdb

// view.go: view objects. A view is a name bound to stored SELECT
// text; the engine stores, lists, and namespaces them (they share the
// relation namespace with tables and sequences), while parsing and
// materializing the query belongs to the SQL layer.

import (
	"encoding/json"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

// ViewDesc is one stored view: its name and query text.
type ViewDesc struct {
	Name  string `json:"name"`
	Query string `json:"query"`
}

// viewKey is a view's key in the system views table.
func viewKey(name string) string {
	return string(mustEncode(int64(sysViewTableID), int64(primaryIndexID), name))
}

// CreateView stores a view. The name must be free across the relation
// namespace (tables, sequences, views); orReplace permits overwriting
// an existing view — never a table or sequence.
func (e *Engine) CreateView(name, query string, orReplace bool) error {
	if name == "" {
		return serr.New("view name is required")
	}
	if query == "" {
		return serr.New("view query is required", "view", name)
	}
	err := e.updateDDL(func(tx *btypedb.Tx[string, []byte]) error {
		if tx.Contains(descKey(name)) || tx.Contains(sqlSeqKey(name)) {
			return serr.New(`relation "` + name + `" already exists`)
		}
		if !orReplace && tx.Contains(viewKey(name)) {
			return serr.New(`relation "` + name + `" already exists`)
		}
		blob, err := json.Marshal(ViewDesc{Name: name, Query: query})
		if err != nil {
			return serr.Wrap(err, "op", "encode view")
		}
		return tx.Set(viewKey(name), blob)
	})
	if err != nil {
		return serr.Wrap(err, "op", "create view", "view", name)
	}
	return nil
}

// DropView removes a view, reporting whether it existed.
func (e *Engine) DropView(name string) (bool, error) {
	existed := false
	err := e.updateDDL(func(tx *btypedb.Tx[string, []byte]) error {
		var err error
		existed, err = tx.Delete(viewKey(name))
		return err
	})
	if err != nil {
		return false, serr.Wrap(err, "op", "drop view", "view", name)
	}
	return existed, nil
}

// View returns the named view, or nil — from the committed state at
// the call (views cannot change under a statement: DDL is excluded
// from transaction blocks).
func (e *Engine) View(name string) *ViewDesc {
	var vd *ViewDesc
	e.kv.View(func(tx *btypedb.Tx[string, []byte]) error {
		blob, ok := tx.Get(viewKey(name))
		if !ok {
			return nil
		}
		v := &ViewDesc{}
		if json.Unmarshal(blob, v) == nil {
			vd = v
		}
		return nil
	})
	return vd
}

// Views returns all views, sorted by name (key order is name order).
func (e *Engine) Views() []ViewDesc {
	var out []ViewDesc
	e.kv.View(func(tx *btypedb.Tx[string, []byte]) error {
		prefix := mustEncode(int64(sysViewTableID), int64(primaryIndexID))
		end := string(tuple.PrefixEnd(prefix))
		for k, blob := range tx.Ascend(string(prefix)) {
			if k >= end {
				break
			}
			v := ViewDesc{}
			if json.Unmarshal(blob, &v) == nil {
				out = append(out, v)
			}
		}
		return nil
	})
	return out
}
