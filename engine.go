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
// of a descriptors table keyed by name, the table-ID sequence is a
// single key, and named sequences (see seq.go) are rows of a sequences
// table keyed by name. User tables start at ID 100.
package bytdb

import (
	"encoding/json"
	"fmt"
	"iter"
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
	sysSeqTableID    = 2 // (name) -> big-endian uint64: sequence's next value
	sysViewTableID   = 3 // (name) -> JSON ViewDesc
	firstUserTableID = 100

	primaryIndexID = 1

	// identitySeqIndexID is the "index" within the system sequences
	// table holding identity-column counters, keyed by (tableID, colID)
	// — a namespace of their own, so they can never collide with named
	// sequences (index 1) and survive table renames.
	identitySeqIndexID = 2
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
	// The date/time and UUID types ride on integer and byte runtime
	// representations (see types.go), so keys and index entries order
	// chronologically / bytewise with no new tuple encoding.
	TTimestamp ColType = "timestamp" // int64: microseconds since the Unix epoch, UTC
	TDate      ColType = "date"      // int64: days since the Unix epoch
	TUUID      ColType = "uuid"      // []byte: 16 bytes, RFC 4122 order
)

// Column describes one table column. ID is assigned by the engine
// when the column is created and is never reused within its table —
// row values are stored as (column ID, value) pairs, which is what
// lets AddColumn and DropColumn skip rewriting rows. Leave ID zero
// when declaring columns; input values are ignored. A NotNull column
// rejects NULL on insert and update.
//
// An Identity column (int only) auto-fills: inserting NULL draws the
// next value from the column's own durable counter (starting at 1),
// and inserting an explicit value bumps the counter past it so later
// draws cannot collide. Identity implies NOT NULL.
type Column struct {
	Name     string  `json:"name"`
	Type     ColType `json:"type"`
	ID       uint32  `json:"id"`
	NotNull  bool    `json:"not_null,omitempty"`
	Identity bool    `json:"identity,omitempty"`
	// Default is the column's DEFAULT as SQL literal text ("" = none).
	// Like checks, it is stored and reported by the engine but applied
	// by the SQL layer, which owns the literal syntax — engine-API
	// inserts take explicit values for every column.
	Default string `json:"default,omitempty"`
	// MaxLen is a string column's VARCHAR(n) limit in characters
	// (0 = unbounded). Enforced on every insert and update with
	// Postgres semantics: overflow errors, except overflow that is all
	// spaces, which truncates silently (the SQL standard's one
	// concession). Only valid on string columns.
	MaxLen int `json:"max_len,omitempty"`
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

// FKDesc is one foreign-key constraint, stored on the referencing
// (child) table: the child columns, by ordinal, that must match a row
// of RefTable on RefCols. RefCols name the parent's primary key or the
// columns of one of its unique indexes — the uniqueness is what makes
// "the referenced row" well-defined. Only NO ACTION/RESTRICT semantics
// exist (no cascades). Like checks, the engine stores and reports FKs
// and guards the schema side (you cannot drop a referenced table or an
// involved column), while row-level enforcement belongs to the SQL
// layer — engine-API writes alone do not check references.
type FKDesc struct {
	Name     string   `json:"name"`
	Cols     []int    `json:"cols"`
	RefTable string   `json:"ref_table"`
	RefCols  []string `json:"ref_cols"`
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
	ForeignKeys   []FKDesc    `json:"foreign_keys,omitempty"`
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
	c.ForeignKeys = slices.Clone(d.ForeignKeys)
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
//
// The kv keyspace is the single source of truth for the catalog:
// every transaction — read or write — resolves table descriptors from
// its own kv snapshot, so the schema a transaction sees is exactly the
// schema of the data it sees, by construction. There is no separate
// in-memory catalog to keep coherent (an earlier design had one, plus
// a seqlock to patch the resulting torn-snapshot window); the only
// in-memory state is a parse cache keyed by descriptor bytes.
type Engine struct {
	kv *btypedb.DB[string, []byte]

	// descCache avoids re-parsing descriptor JSON on every resolution.
	// One entry per table name, validated by blob identity: a hit
	// counts only when the snapshot's stored bytes equal the cached
	// ones, so a stale entry can never leak across a DDL — at worst a
	// reader over an old snapshot re-parses. Parsed descriptors are
	// immutable (DDL clones and rewrites), so sharing one *TableDesc
	// across transactions is safe.
	cacheMu   sync.RWMutex
	descCache map[string]descCacheEntry

	// writerGID is the ID of the goroutine holding the open writable
	// transaction (from WriteTxn or Begin(true)), or 0. The kv writer
	// lock is not reentrant, so a one-shot write or DDL issued from
	// that same goroutine would block behind its own transaction — a
	// permanent, server-wide deadlock. The guard turns that programming
	// error into an error return (see checkReentrantWrite).
	writerGID atomic.Uint64

	// testCommitErr, when non-nil, replaces a successful DDL commit's
	// result (see updateDDL). Only tests set it, to simulate a WAL
	// append or fsync failing after the transaction closure ran.
	testCommitErr error
}

type descCacheEntry struct {
	blob string // the exact stored bytes the desc was parsed from
	desc *TableDesc
}

// updateDDL runs one DDL transaction. With the catalog in the kv
// keyspace, the descriptor write commits — or rolls back — atomically
// with the schema change's data (backfill, range deletes), so there is
// no publish/un-publish dance and no window where the engine
// advertises schema that missed the disk. Concurrent DDL serializes on
// the kv writer lock; each DDL resolves the descriptor it mutates
// inside its own transaction, so it always builds on the committed
// state of the one before it.
func (e *Engine) updateDDL(fn func(tx *btypedb.Tx[string, []byte]) error) error {
	if err := e.checkReentrantWrite("ddl"); err != nil {
		return err
	}
	if e.testCommitErr != nil {
		// Simulate the failed commit: run the closure, then roll the
		// transaction back, leaving both disk and the catalog exactly
		// as a failed WAL append would — unchanged.
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
	return e.kv.Update(fn)
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

// WithSyncNever opens the engine with WAL fsyncs left to the operating
// system: much faster writes, at the cost that a power loss (not a
// process crash) may lose recently acknowledged transactions. Exposed
// here so embedders and bytdbd can select the policy without importing
// btypedb.
func WithSyncNever() btypedb.Option {
	return btypedb.WithSyncPolicy(btypedb.SyncNever)
}

// Open opens (creating if necessary) the database at path and
// validates the catalog.
func Open(path string, opts ...btypedb.Option) (*Engine, error) {
	kv, err := btypedb.Open(path, btypedb.StringCodec, btypedb.BytesCodec, opts...)
	if err != nil {
		return nil, serr.Wrap(err, "op", "open kv store")
	}
	e := &Engine{kv: kv, descCache: map[string]descCacheEntry{}}
	if err := e.loadCatalog(); err != nil {
		kv.Close()
		return nil, err
	}
	return e, nil
}

// Close closes the underlying store.
func (e *Engine) Close() error { return e.kv.Close() }

// Backup writes a consistent point-in-time copy of the database to
// destPath without blocking readers or writers: every transaction
// committed before the call is in the copy, whole — catalog and rows
// alike, since both live in the one kv keyspace. The copy lands
// atomically (temp file + fsync + rename), and restoring is just Open
// on the backup file.
func (e *Engine) Backup(destPath string) error {
	if err := e.kv.Backup(destPath); err != nil {
		return serr.Wrap(err, "op", "engine backup")
	}
	return nil
}

// loadCatalog validates every table descriptor at open, warming the
// parse cache. Unreadable or too-new descriptors fail the open —
// silently skipping one would hide the table's rows and let a
// re-CREATE reuse key space an existing table owns — but all offenders
// are collected first, so one error message names every broken
// descriptor instead of surfacing them one restart at a time.
func (e *Engine) loadCatalog() error {
	prefix := descTablePrefix()
	end := string(tuple.PrefixEnd([]byte(prefix)))
	var bad []string
	for k, v := range e.kv.Ascend(prefix) {
		if k >= end {
			break
		}
		desc, err := parseDesc(v)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", descKeyName(k), err))
			continue
		}
		e.cacheStore(desc.Name, string(v), desc)
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

// parseDesc decodes one stored descriptor, refusing versions from the
// future (misreading a newer layout is worse than failing).
func parseDesc(blob []byte) (*TableDesc, error) {
	desc := &TableDesc{}
	if err := json.Unmarshal(blob, desc); err != nil {
		return nil, err
	}
	if desc.FormatVersion > descFormatVersion {
		return nil, fmt.Errorf(
			"descriptor format version %d is newer than this bytdb supports (%d); upgrade bytdb",
			desc.FormatVersion, descFormatVersion)
	}
	return desc, nil
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

// tableFromView resolves a table's descriptor from a kv view — the
// caller's transaction or snapshot, making the schema seen exactly the
// schema of the data seen. Absent tables return (nil, nil); a stored
// descriptor that will not parse is an error (possible only through
// external corruption: loadCatalog validated everything at open, and
// every writer since was this process).
func (e *Engine) tableFromView(v kvView, name string) (*TableDesc, error) {
	blob, ok := v.Get(descKey(name))
	if !ok {
		return nil, nil
	}
	bs := string(blob)
	e.cacheMu.RLock()
	ent, hit := e.descCache[name]
	e.cacheMu.RUnlock()
	if hit && ent.blob == bs {
		return ent.desc, nil
	}
	desc, err := parseDesc(blob)
	if err != nil {
		return nil, serr.Wrap(err, "op", "decode table descriptor", "table", name)
	}
	e.cacheStore(name, bs, desc)
	return desc, nil
}

// descFromView is tableFromView with the absent case as an error, for
// callers that require the table to exist.
func (e *Engine) descFromView(v kvView, table string) (*TableDesc, error) {
	desc, err := e.tableFromView(v, table)
	if err != nil {
		return nil, err
	}
	if desc == nil {
		return nil, serr.New("no such table", "table", table)
	}
	return desc, nil
}

func (e *Engine) cacheStore(name, blob string, desc *TableDesc) {
	e.cacheMu.Lock()
	e.descCache[name] = descCacheEntry{blob: blob, desc: desc}
	e.cacheMu.Unlock()
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

// seqNameKey is a named sequence's key in the system sequences table.
func seqNameKey(name string) string {
	return string(mustEncode(int64(sysSeqTableID), int64(primaryIndexID), name))
}

// identitySeqKey is an identity column's counter key.
func identitySeqKey(tableID uint64, colID uint32) string {
	return string(mustEncode(int64(sysSeqTableID), int64(identitySeqIndexID), int64(tableID), int64(colID)))
}

// identitySeqTablePrefix covers every identity counter of one table,
// for DropTable's cleanup.
func identitySeqTablePrefix(tableID uint64) []byte {
	return mustEncode(int64(sysSeqTableID), int64(identitySeqIndexID), int64(tableID))
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
