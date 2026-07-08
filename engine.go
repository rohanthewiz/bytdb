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
	"fmt"
	"iter"
	"maps"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

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
// when declaring columns; input values are ignored. A NotNull column
// rejects NULL on insert and update.
type Column struct {
	Name    string  `json:"name"`
	Type    ColType `json:"type"`
	ID      uint32  `json:"id"`
	NotNull bool    `json:"not_null,omitempty"`
}

// CheckDesc is one CHECK constraint: a SQL boolean expression over the
// table's columns, stored as text. The engine stores and reports
// checks; enforcing them belongs to the SQL layer, which owns the
// expression language — writes through the engine API alone do not
// evaluate them.
type CheckDesc struct {
	Name string `json:"name"`
	Expr string `json:"expr"`
}

// descFormatVersion is the version stamped into every descriptor this
// build writes. Bump it when the persisted layout changes in a way an
// older reader would misinterpret; Open refuses descriptors from the
// future with a clear error instead of misreading them. Descriptors
// written before versioning carry 0 and read as current.
const descFormatVersion = 1

// TableDesc describes a table: its columns in declared order, which of
// them (by ordinal) form the primary key in key order, its secondary
// indexes, and its CHECK constraints. Descriptors are persisted as
// JSON rows of the system descriptors table.
type TableDesc struct {
	FormatVersion uint32      `json:"format_version,omitempty"`
	ID            uint64      `json:"id"`
	Name          string      `json:"name"`
	Columns       []Column    `json:"columns"`
	PKCols        []int       `json:"pk_cols"`
	Indexes       []IndexDesc `json:"indexes,omitempty"`
	Checks        []CheckDesc `json:"checks,omitempty"`
	NextColID     uint32      `json:"next_col_id"`
}

// marshalDesc encodes a descriptor for the system descriptors table,
// stamping the current format version. Every descriptor write goes
// through here.
func marshalDesc(desc *TableDesc) ([]byte, error) {
	desc.FormatVersion = descFormatVersion
	blob, err := json.Marshal(desc)
	if err != nil {
		return nil, serr.Wrap(err, "op", "encode table descriptor")
	}
	return blob, nil
}

// IndexDesc describes one secondary index: which columns (by ordinal)
// it orders, in key order. A unique index rejects two rows with equal
// indexed values, except that rows with NULL in any indexed column
// never conflict (SQL semantics). Desc marks which key columns order
// descending (nil: all ascending); a descending column's NULLs sort
// after its values, mirroring ascending.
type IndexDesc struct {
	ID     uint64 `json:"id"`
	Name   string `json:"name"`
	Cols   []int  `json:"cols"`
	Unique bool   `json:"unique"`
	Desc   []bool `json:"desc,omitempty"`
}

// DescAt reports whether the i-th key column orders descending.
func (x *IndexDesc) DescAt(i int) bool {
	return i < len(x.Desc) && x.Desc[i]
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
	c.Checks = slices.Clone(d.Checks)
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
	// catVer versions the catalog, seqlock-style, so read transactions
	// can make their two snapshots — kv and catalog — atomic without a
	// lock spanning both. Even = the catalog matches the committed disk
	// state; odd = a DDL has published its new descriptor but its commit
	// is still in flight (DDL publishes before commit, deliberately —
	// see updateDDL). Readers take the catalog only at an even version,
	// open the kv snapshot, then re-check the version: unchanged proves
	// the pair describes one moment. On odd they wait on catStable
	// (closed when the DDL's outcome lands) rather than spinning through
	// the commit's fsync. This optimistic scheme is deliberate: the
	// alternative — a lock held across the DDL commit that readers
	// share — would either invert lock order against writable Begin
	// (btypedb's writer lock, then mu) or stall every reader behind DDL
	// queued on a long-lived write transaction.
	catVer    uint64
	catStable chan struct{} // non-nil iff catVer is odd

	// writerGID is the ID of the goroutine holding the open writable
	// transaction (from WriteTxn or Begin(true)), or 0. The kv writer
	// lock is not reentrant, so a one-shot write or DDL issued from
	// that same goroutine would block behind its own transaction — a
	// permanent, server-wide deadlock. The guard turns that programming
	// error into an error return (see checkReentrantWrite).
	writerGID atomic.Uint64

	// testCommitErr, when non-nil, replaces a successful DDL commit's
	// result (see updateDDL). Only tests set it, to simulate a WAL
	// append or fsync failing after the transaction closure — and its
	// in-closure catalog publish — already ran.
	testCommitErr error
}

// updateDDL runs one DDL transaction. DDL publishes its new descriptor
// inside the closure — deliberately before commit, so a write that the
// kv store serializes after the commit can never resolve a stale
// descriptor (e.g. miss a just-created index). The cost of that order
// is the failure path this wrapper exists for: when the commit itself
// fails (WAL append, fsync), the in-memory catalog already advertises
// a schema that never reached disk, and the caller must un-publish it.
// See restoreDesc.
func (e *Engine) updateDDL(fn func(tx *btypedb.Tx[string, []byte]) error) error {
	if err := e.checkReentrantWrite("ddl"); err != nil {
		return err
	}
	if e.testCommitErr != nil {
		// Simulate the failed commit faithfully: run the closure — so
		// its catalog publish happens, as it would for real — but roll
		// the transaction back, leaving disk exactly as a failed WAL
		// append would: unchanged.
		tx, err := e.kv.Begin(true)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := fn(tx); err != nil {
			return err
		}
		return e.testCommitErr
	}
	err := e.kv.Update(fn)
	if err == nil {
		// The publish the closure made pending is now durable; return
		// the catalog version to even so readers proceed. Failure paths
		// stabilize in restoreDesc instead, after un-publishing.
		e.stabilizeCatalog()
	}
	return err
}

// publishDesc installs desc under name (nil removes the entry) as a
// stable publish: the caller's kv commit has already succeeded, so the
// catalog version advances by two and stays even (see catVer). Used by
// CreateTable, which publishes after commit.
func (e *Engine) publishDesc(name string, desc *TableDesc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.setDescLocked(name, desc)
	e.catVer += 2
}

// publishDescPending installs desc under name while its kv commit is
// still in flight: the version goes odd, telling readers the catalog
// is ahead of disk until stabilizeCatalog resolves the outcome. Called
// from inside DDL transaction closures (callers hold ddlMu, so at most
// one publish is ever pending).
func (e *Engine) publishDescPending(name string, desc *TableDesc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.setDescLocked(name, desc)
	e.catVer++
	if e.catVer&1 == 1 {
		e.catStable = make(chan struct{})
	}
}

// stabilizeCatalog marks a pending publish resolved — the DDL commit
// succeeded, or a failure was rolled back (restoreDesc) — returning
// the version to even and waking any waiting readers. A no-op when
// nothing is pending.
func (e *Engine) stabilizeCatalog() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.catVer&1 == 1 {
		e.catVer++
		close(e.catStable)
		e.catStable = nil
	}
}

func (e *Engine) setDescLocked(name string, desc *TableDesc) {
	if desc == nil {
		delete(e.tables, name)
	} else {
		e.tables[name] = desc
	}
}

// checkReentrantWrite refuses a one-shot write, DDL, or nested writable
// transaction issued from the goroutine that already holds the open
// writable transaction: the kv writer lock is not reentrant, so the
// call would deadlock the whole engine forever. Calls from other
// goroutines pass — blocking behind the open transaction is their
// normal, correct behavior.
func (e *Engine) checkReentrantWrite(op string) error {
	// Cheap common case first: no writable transaction open at all.
	// The stack parse in curGID only runs while one is.
	if g := e.writerGID.Load(); g != 0 && g == curGID() {
		return serr.New("engine write inside the goroutine's own open write transaction would deadlock",
			"op", op, "hint", "use the Txn methods instead")
	}
	return nil
}

// curGID returns the current goroutine's ID, parsed from the runtime
// stack header ("goroutine N [running]:"). The runtime exposes no
// direct accessor; this parse is the standard fallback and is only
// consulted while a writable transaction is open.
func curGID() uint64 {
	var buf [40]byte
	n := runtime.Stack(buf[:], false)
	var id uint64
	for _, c := range buf[len("goroutine "):n] {
		if c < '0' || c > '9' {
			break
		}
		id = id*10 + uint64(c-'0')
	}
	return id
}

// restoreDesc rolls the in-memory catalog back to old after a failed
// DDL transaction (a nil old removes the entry, undoing a CreateTable-
// style publish) and stabilizes the version. Correct in both failure
// shapes: if the closure failed before publishing, the catalog already
// equals old (same pointer) and the version is already even — a pure
// no-op; if the commit failed after the publish, disk still holds old,
// so this undoes the phantom and resolves the pending version, waking
// readers. Callers hold ddlMu, so no other DDL can interleave. A
// one-shot write that begins in the narrow window between the failed
// commit returning and this restore could still resolve the phantom
// descriptor — closing that fully needs the catalog inside the kv
// keyspace (or a commit hook), a planned follow-on.
func (e *Engine) restoreDesc(name string, old *TableDesc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.setDescLocked(name, old)
	if e.catVer&1 == 1 {
		e.catVer++
		close(e.catStable)
		e.catStable = nil
	}
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

// loadCatalog reads every table descriptor. Unreadable or too-new
// descriptors fail the open — silently skipping one would hide the
// table's rows and let a re-CREATE reuse key space an existing table
// owns — but all offenders are collected first, so one error message
// names every broken descriptor instead of surfacing them one restart
// at a time.
func (e *Engine) loadCatalog() error {
	prefix := descTablePrefix()
	end := string(tuple.PrefixEnd([]byte(prefix)))
	var bad []string
	for k, v := range e.kv.Ascend(prefix) {
		if k >= end {
			break
		}
		desc := &TableDesc{}
		if err := json.Unmarshal(v, desc); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", descKeyName(k), err))
			continue
		}
		if desc.FormatVersion > descFormatVersion {
			bad = append(bad, fmt.Sprintf(
				"%s: descriptor format version %d is newer than this bytdb supports (%d); upgrade bytdb",
				desc.Name, desc.FormatVersion, descFormatVersion))
			continue
		}
		e.tables[desc.Name] = desc
	}
	if len(bad) > 0 {
		// The offenders go in the message itself, not structured
		// fields: this error's job is to tell an operator exactly what
		// to repair, wherever it gets logged.
		return serr.New(fmt.Sprintf("unreadable table descriptors (%d): %s",
			len(bad), strings.Join(bad, "; ")))
	}
	return nil
}

// descKeyName recovers the table name from a descriptor key for error
// reporting, falling back to the raw key when even that won't decode.
func descKeyName(k string) string {
	if vals, err := tuple.Decode([]byte(k)); err == nil && len(vals) == 3 {
		if s, ok := vals[2].(string); ok {
			return s
		}
	}
	return fmt.Sprintf("key %q", k)
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
// shallow clone is a consistent snapshot. Writable transactions use
// this directly: they hold the kv writer lock, which every DDL holds
// through publish and commit, so their catalog can never be ahead of
// their data.
func (e *Engine) catalogSnapshot() map[string]*TableDesc {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return maps.Clone(e.tables)
}

// stableCatalogSnapshot returns the catalog and its version, waiting
// out any pending DDL publish first (see catVer): the returned version
// is always even, so a later equality check against catalogVersion
// proves no publish landed in between.
func (e *Engine) stableCatalogSnapshot() (map[string]*TableDesc, uint64) {
	for {
		e.mu.RLock()
		if e.catVer&1 == 0 {
			snap, ver := maps.Clone(e.tables), e.catVer
			e.mu.RUnlock()
			return snap, ver
		}
		ch := e.catStable
		e.mu.RUnlock()
		<-ch // closed when the pending DDL's outcome lands
	}
}

func (e *Engine) catalogVersion() uint64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.catVer
}

// kvView is the read surface shared by the store and its transactions;
// write paths always use a concrete transaction.
type kvView interface {
	Get(key string) ([]byte, bool)
	Contains(key string) bool
	Ascend(from string) iter.Seq2[string, []byte]
	Descend(from string) iter.Seq2[string, []byte]
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
