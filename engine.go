// Package bytdb is a relational layer over btypedb, in the way
// CockroachDB layers SQL over Pebble: tables, rows, and (eventually)
// secondary indexes are all encoded into one ordered key space, so
// relational operations become key scans.
//
// Key layout (see the tuple package for the order-preserving encoding):
//
//	tuple(tableID, indexID, pk...) -> tuple(non-pk columns in order)
//
// System state lives at reserved table IDs: table descriptors are rows
// of a descriptors table keyed by name, and the table-ID sequence is a
// single key. User tables start at ID 100.
package bytdb

import (
	"encoding/json"
	"iter"
	"maps"
	"slices"
	"sync"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

const (
	sysIDSeqTableID  = 0 // single key: next table ID to allocate
	sysDescTableID   = 1 // (name) -> JSON TableDesc
	firstUserTableID = 100

	primaryIndexID = 1
)

// ColType is a column's SQL-ish type. It decides how Insert coerces
// values and how keys and rows decode.
type ColType string

const (
	TBool   ColType = "bool"
	TInt    ColType = "int"   // int64
	TFloat  ColType = "float" // float64
	TString ColType = "string"
	TBytes  ColType = "bytes"
)

// Column describes one table column. ID is assigned by the engine
// when the column is created and is never reused within its table —
// row values are stored as (column ID, value) pairs, which is what
// lets AddColumn and DropColumn skip rewriting rows. Leave ID zero
// when declaring columns; input values are ignored.
type Column struct {
	Name string  `json:"name"`
	Type ColType `json:"type"`
	ID   uint32  `json:"id"`
}

// TableDesc describes a table: its columns in declared order, which of
// them (by ordinal) form the primary key in key order, and its
// secondary indexes. Descriptors are persisted as JSON rows of the
// system descriptors table.
type TableDesc struct {
	ID        uint64      `json:"id"`
	Name      string      `json:"name"`
	Columns   []Column    `json:"columns"`
	PKCols    []int       `json:"pk_cols"`
	Indexes   []IndexDesc `json:"indexes,omitempty"`
	NextColID uint32      `json:"next_col_id"`
}

// IndexDesc describes one secondary index: which columns (by ordinal)
// it orders, in key order. A unique index rejects two rows with equal
// indexed values, except that rows with NULL in any indexed column
// never conflict (SQL semantics).
type IndexDesc struct {
	ID     uint64 `json:"id"`
	Name   string `json:"name"`
	Cols   []int  `json:"cols"`
	Unique bool   `json:"unique"`
}

// Index returns the named index's descriptor, or nil.
func (d *TableDesc) Index(name string) *IndexDesc {
	for i := range d.Indexes {
		if d.Indexes[i].Name == name {
			return &d.Indexes[i]
		}
	}
	return nil
}

func (d *TableDesc) clone() *TableDesc {
	c := *d
	c.Columns = slices.Clone(d.Columns)
	c.PKCols = slices.Clone(d.PKCols)
	c.Indexes = slices.Clone(d.Indexes)
	return &c
}

// ColIndex returns the ordinal of the named column, or -1.
func (d *TableDesc) ColIndex(name string) int {
	for i, c := range d.Columns {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// colOrdinalByID returns the ordinal of the column with the given
// stable ID, or -1 — the not-found case is normal: it means a row
// value carries data for a since-dropped column.
func (d *TableDesc) colOrdinalByID(id uint32) int {
	for i := range d.Columns {
		if d.Columns[i].ID == id {
			return i
		}
	}
	return -1
}

func (d *TableDesc) isPK(ordinal int) bool {
	for _, p := range d.PKCols {
		if p == ordinal {
			return true
		}
	}
	return false
}

// Engine is a relational store in a single btypedb file. It is safe
// for concurrent use.
type Engine struct {
	kv *btypedb.DB[string, []byte]

	// ddlMu serializes schema changes against each other. Schema
	// visibility for DML is handled differently: writes resolve their
	// descriptor inside their own kv transaction, and DDL publishes the
	// new descriptor (under mu) as the last step inside its own — so a
	// write serialized after an index creation always sees the index.
	ddlMu sync.Mutex

	mu     sync.RWMutex
	tables map[string]*TableDesc
}

// Open opens (creating if necessary) the database at path and loads
// the catalog.
func Open(path string, opts ...btypedb.Option) (*Engine, error) {
	kv, err := btypedb.Open(path, btypedb.StringCodec, btypedb.BytesCodec, opts...)
	if err != nil {
		return nil, serr.Wrap(err, "op", "open kv store")
	}
	e := &Engine{kv: kv, tables: map[string]*TableDesc{}}
	if err := e.loadCatalog(); err != nil {
		kv.Close()
		return nil, err
	}
	return e, nil
}

// Close closes the underlying store.
func (e *Engine) Close() error { return e.kv.Close() }

func (e *Engine) loadCatalog() error {
	prefix := descTablePrefix()
	end := string(tuple.PrefixEnd([]byte(prefix)))
	for k, v := range e.kv.Ascend(prefix) {
		if k >= end {
			break
		}
		desc := &TableDesc{}
		if err := json.Unmarshal(v, desc); err != nil {
			return serr.Wrap(err, "op", "decode table descriptor")
		}
		e.tables[desc.Name] = desc
	}
	return nil
}

// desc returns the descriptor for a table name.
func (e *Engine) desc(table string) (*TableDesc, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	d := e.tables[table]
	if d == nil {
		return nil, serr.New("no such table", "table", table)
	}
	return d, nil
}

// catalogSnapshot returns a point-in-time view of the catalog.
// Descriptors are never mutated in place (DDL clones and swaps), so a
// shallow clone is a consistent snapshot.
func (e *Engine) catalogSnapshot() map[string]*TableDesc {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return maps.Clone(e.tables)
}

// kvView is the read surface shared by the store and its transactions;
// write paths always use a concrete transaction.
type kvView interface {
	Get(key string) ([]byte, bool)
	Contains(key string) bool
	Ascend(from string) iter.Seq2[string, []byte]
}

// --- key helpers ---

func mustEncode(vals ...any) []byte {
	b, err := tuple.Encode(vals...)
	if err != nil {
		panic(err) // internal values only; never user input
	}
	return b
}

// tableSpace is the key prefix covering a table's entire key space:
// primary rows and every secondary index.
func tableSpace(tableID uint64) []byte {
	return mustEncode(int64(tableID))
}

// indexPrefix is the key prefix covering one index of one table.
func indexPrefix(tableID, indexID uint64) []byte {
	return mustEncode(int64(tableID), int64(indexID))
}

// tablePrefix is the key prefix covering every row of a table's
// primary index.
func tablePrefix(tableID uint64) []byte {
	return indexPrefix(tableID, primaryIndexID)
}

func descTablePrefix() string {
	return string(tablePrefix(sysDescTableID))
}

func descKey(name string) string {
	return string(mustEncode(int64(sysDescTableID), int64(primaryIndexID), name))
}

func seqKey() string {
	return string(mustEncode(int64(sysIDSeqTableID)))
}

// rowKey builds a table's primary-index key from already-coerced PK
// values.
func rowKey(d *TableDesc, pkVals []any) (string, error) {
	buf, err := tuple.Append(tablePrefix(d.ID), pkVals...)
	if err != nil {
		return "", serr.Wrap(err, "op", "encode primary key")
	}
	return string(buf), nil
}
