// Package stdlib registers bytdb as a database/sql driver named "bytdb", so
// an embedded bytdb database can be reached through the interface the Go
// ecosystem already speaks: sqlx, ORMs, migration tools, and any code written
// against *sql.DB.
//
// It is the in-process counterpart to the pgwire module. Both put bytdb
// behind database/sql; this one does it with no server, no socket, and no
// wire encoding, at the cost that the database lives in the calling process.
//
//	import (
//		"database/sql"
//		_ "github.com/rohanthewiz/bytdb/stdlib"
//	)
//
//	db, err := sql.Open("bytdb", "app.bytdb?concurrent_writes=true")
//
// The DSN is a filesystem path, optionally followed by engine options (see
// ParseDSN). bytdb has no in-memory mode, so a path is always a real file,
// created on first open.
//
// # Engine sharing
//
// btypedb takes the database file for itself, so one path is one engine per
// process. Every *sql.DB on a path shares that engine and every pooled
// connection gets its own bytdb Session — one engine, many sessions, the same
// shape pgwire serves. Opening the same path twice with different options is
// an error rather than a silent adoption of whichever set got there first.
//
// A program already holding an *bytdb.Engine should use OpenEngine rather
// than a DSN: the file cannot be opened twice, and OpenEngine wraps the
// engine it is given without trying.
//
// # Transactions and conflicts
//
// BeginTx opens a bytdb transaction block, mapping database/sql's isolation
// levels onto BEGIN's. Under bytdb.WithConcurrentWrites, a losing commit
// returns bytdb.ErrTxConflict, which callers match with errors.Is and retry
// from the top; IsRetryable is the shorthand. Autocommit statements are
// retried inside bytdb and never surface a conflict.
//
// # What database/sql cannot carry
//
// A statement's Notice (bytdb's warning for a redundant BEGIN, say) has
// nowhere to go in database/sql's interfaces and is dropped. Callers who need
// it should use the sql package directly, or reach the engine through Engine.
// One Tag override does matter and is NOT dropped: COMMIT of a failed
// transaction block actually rolls back, and Tx.Commit reports that as
// ErrCommitRolledBack rather than a nil that would read as durability.
package stdlib

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"
	"github.com/rohanthewiz/serr"
)

// DriverName is the name this driver registers under, i.e. what to pass to
// sql.Open.
const DriverName = "bytdb"

func init() { sql.Register(DriverName, Driver{}) }

// IsRetryable reports whether err is a commit-time optimistic-concurrency
// conflict, meaning the transaction lost a race and should be re-run from the
// top. It only ever holds under bytdb.WithConcurrentWrites.
func IsRetryable(err error) bool { return errors.Is(err, bytdb.ErrTxConflict) }

// Driver is the database/sql driver for embedded bytdb databases.
type Driver struct{}

var (
	_ driver.Driver        = Driver{}
	_ driver.DriverContext = Driver{}
)

// Open satisfies driver.Driver. database/sql takes the OpenConnector path
// instead, so this only serves a caller holding the driver directly; the
// connection it returns owns its engine reference and releases it on Close,
// since there is no *sql.DB above it to do that.
func (d Driver) Open(dsn string) (driver.Conn, error) {
	c, err := d.OpenConnector(dsn)
	if err != nil {
		return nil, err
	}
	cn, err := c.Connect(context.Background())
	if err != nil {
		_ = c.(*connector).Close()
		return nil, err
	}
	cn.(*conn).owner = c.(*connector)
	return cn, nil
}

// OpenConnector opens the engine named by dsn and takes a reference to it,
// released when database/sql closes the *sql.DB.
func (d Driver) OpenConnector(dsn string) (driver.Connector, error) {
	cfg, err := ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	sh, err := acquire(cfg)
	if err != nil {
		return nil, err
	}
	return &connector{sh: sh}, nil
}

// OpenEngine wraps an engine the caller already opened in a *sql.DB. Use it
// instead of sql.Open when the program also uses bytdb's native API: the file
// cannot be opened twice, so a DSN would fail against a database this process
// already holds.
//
// The caller keeps ownership: closing the returned *sql.DB closes its
// connections but leaves the engine open, to be closed with bytdb's Close.
func OpenEngine(e *bytdb.Engine) *sql.DB {
	return sql.OpenDB(&connector{sh: &shared{e: e, db: bsql.New(e)}})
}

// Engine returns the bytdb engine behind a *sql.DB this driver opened, for
// work the SQL dialect does not cover — ordered range scans, index scans, the
// tuple encoding. It is the engine's owner only when the *sql.DB is: one
// obtained from a DSN dies with the last *sql.DB on that path, while one
// passed to OpenEngine outlives it.
//
// It errors if db was not opened by this driver.
func Engine(ctx context.Context, db *sql.DB) (*bytdb.Engine, error) {
	c, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()

	var e *bytdb.Engine
	err = c.Raw(func(dc any) error {
		cn, ok := dc.(*conn)
		if !ok {
			return serr.New("not a bytdb connection", "type", fmt.Sprintf("%T", dc))
		}
		e = cn.sh.e
		return nil
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// Config is a parsed DSN: the database file and the engine options opening it
// should apply.
type Config struct {
	// Path is the database file, as spelled in the DSN.
	Path string

	// Options are the bytdb open options the DSN asked for.
	Options []btypedb.Option

	// Memory makes the database a private temporary file, removed when
	// the *sql.DB closes: the DSN ":memory:" (bytdb itself always has a
	// file; the data still lives in memory, as it always does).
	Memory bool

	// Settings applied to the engine after it opens (deep-look D-57);
	// nil leaves the engine's default. See ParseDSN.
	MemoryCeiling *int64
	MaxResultRows *int64
	MaxTxnWrites  *int64

	// signature identifies the option set, so two DSNs on one path can be
	// compared without holding on to the closures. It never contains key
	// material.
	signature string
}

// ParseDSN parses a bytdb DSN:
//
//	[file:]<path>[?option=value&...]
//
// The path is taken verbatim (spaces and all); everything after the first "?"
// is the option set. A path that itself contains "?" cannot be expressed —
// use OpenEngine for one.
//
// Options, all optional:
//
//	sync=never              leave WAL fsyncs to the OS: much faster writes,
//	                        at the risk of losing recently acknowledged
//	                        transactions to a power loss (not a process
//	                        crash). Omitted, the engine's default stands.
//	concurrent_writes=true  run write transactions under optimistic
//	                        concurrency control instead of a single writer
//	                        lock; commits may then fail with
//	                        bytdb.ErrTxConflict (see IsRetryable), and
//	                        sequences become gap-tolerant.
//	encryption_key=<hex>    encrypt the WAL at rest, keyed by 32 bytes given
//	                        as 64 hex digits. Opening with the wrong key, or
//	                        without one, fails.
//	encryption_key_file=<path>
//	                        the same key read from a file (32 raw bytes, 64
//	                        hex characters, or base64) — the preferred form:
//	                        a DSN travels through configs and logs, a key in
//	                        it with them.
//	memory_ceiling=<size>   Engine.SetMemoryCeiling: auto (the default), off,
//	                        or a size with GOMEMLIMIT's suffixes (6GiB).
//	max_result_rows=<n>     Engine.SetMaxResultRows (0 = no cap).
//	max_txn_writes=<n>      Engine.SetMaxTxnWrites (0 = no cap).
//	auto_compact=false      turn background log compaction off.
//	compact_min_bytes=<n>, compact_growth_pct=<n>
//	                        tune it: compact once the log is at least n bytes
//	                        and has grown that many percent since the last
//	                        compaction (defaults 32 MiB and 100).
//
// The path ":memory:" opens a private database in a temporary file that
// is removed when the *sql.DB closes; each sql.Open of it is a database
// of its own. Error messages never repeat an encryption_key value.
//
// An unrecognized option is an error rather than a silent no-op: a typo in a
// durability or encryption setting should not be something you discover from
// its consequences.
func ParseDSN(dsn string) (*Config, error) {
	raw := strings.TrimPrefix(strings.TrimSpace(dsn), "file:")
	path, query, _ := strings.Cut(raw, "?")
	if path == "" {
		return nil, serr.New("bytdb DSN has no path (it should name the database file)",
			"dsn", redactDSN(dsn))
	}
	cfg := &Config{Path: path, Memory: path == ":memory:"}
	if query == "" {
		return cfg, nil
	}

	vals, err := url.ParseQuery(query)
	if err != nil {
		return nil, serr.Wrap(err, "op", "parse bytdb DSN options", "options", redactDSN(query))
	}
	if vals.Has("encryption_key") && vals.Has("encryption_key_file") {
		return nil, serr.New("set only one of encryption_key and encryption_key_file")
	}
	nonNegInt := func(name, v string) (*int64, error) {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return nil, serr.New(name+" must be a non-negative integer", name, v)
		}
		return &n, nil
	}
	compactMin, compactGrowth := int64(0), 0

	var sig []string
	for _, name := range sortedKeys(vals) {
		v := vals.Get(name)
		switch name {
		case "sync":
			if v != "never" {
				return nil, serr.New(`sync accepts only "never"`, "sync", v)
			}
			cfg.Options = append(cfg.Options, bytdb.WithSyncNever())
			sig = append(sig, "sync=never")

		case "concurrent_writes":
			on, err := strconv.ParseBool(v)
			if err != nil {
				return nil, serr.New("concurrent_writes must be a boolean",
					"concurrent_writes", v)
			}
			if on {
				cfg.Options = append(cfg.Options, bytdb.WithConcurrentWrites())
			}
			sig = append(sig, "concurrent_writes="+strconv.FormatBool(on))

		case "encryption_key":
			key, err := hex.DecodeString(v)
			if err != nil {
				return nil, serr.New("encryption_key must be hex digits")
			}
			if len(key) != 32 {
				return nil, serr.New("encryption_key must be 32 bytes (64 hex digits)",
					"bytes", strconv.Itoa(len(key)))
			}
			cfg.Options = append(cfg.Options, bytdb.WithEncryptionKey(key))
			sig = append(sig, "encryption_key=set") // never the key itself

		case "encryption_key_file":
			key, err := bytdb.ReadEncryptionKeyFile(v)
			if err != nil {
				return nil, err
			}
			cfg.Options = append(cfg.Options, bytdb.WithEncryptionKey(key))
			// Same signature as an inline key: what matters for sharing an
			// engine is that it is encrypted, and the key is never recorded.
			sig = append(sig, "encryption_key=set")

		case "memory_ceiling":
			n, err := bytdb.ParseMemoryCeiling(v)
			if err != nil {
				return nil, err
			}
			cfg.MemoryCeiling = &n
			sig = append(sig, "memory_ceiling="+strconv.FormatInt(n, 10))

		case "max_result_rows":
			if cfg.MaxResultRows, err = nonNegInt(name, v); err != nil {
				return nil, err
			}
			sig = append(sig, "max_result_rows="+v)

		case "max_txn_writes":
			if cfg.MaxTxnWrites, err = nonNegInt(name, v); err != nil {
				return nil, err
			}
			sig = append(sig, "max_txn_writes="+v)

		case "auto_compact":
			on, err := strconv.ParseBool(v)
			if err != nil {
				return nil, serr.New("auto_compact must be a boolean", "auto_compact", v)
			}
			if !on {
				compactMin = -1
			}
			sig = append(sig, "auto_compact="+strconv.FormatBool(on))

		case "compact_min_bytes":
			n, err := nonNegInt(name, v)
			if err != nil {
				return nil, err
			}
			if compactMin >= 0 {
				compactMin = *n
			}
			sig = append(sig, "compact_min_bytes="+v)

		case "compact_growth_pct":
			n, err := nonNegInt(name, v)
			if err != nil {
				return nil, err
			}
			compactGrowth = int(*n)
			sig = append(sig, "compact_growth_pct="+v)

		default:
			return nil, serr.New("unknown bytdb DSN option", "option", name,
				"known", "sync, concurrent_writes, encryption_key, encryption_key_file, "+
					"memory_ceiling, max_result_rows, max_txn_writes, auto_compact, "+
					"compact_min_bytes, compact_growth_pct")
		}
	}
	// Compaction is one btypedb option built from up to three settings,
	// the way bytdbd builds it from its flags.
	switch {
	case compactMin < 0:
		cfg.Options = append(cfg.Options, btypedb.WithAutoCompactDisabled())
	case compactMin > 0 || compactGrowth > 0:
		if compactMin == 0 {
			compactMin = 32 << 20
		}
		if compactGrowth == 0 {
			compactGrowth = 100
		}
		cfg.Options = append(cfg.Options, btypedb.WithAutoCompact(compactMin, compactGrowth))
	}
	cfg.signature = strings.Join(sig, "&")
	return cfg, nil
}

// redactDSN blanks encryption_key values, so a DSN echoed into an error
// (and from there into a log) never carries key material.
func redactDSN(s string) string {
	const k = "encryption_key="
	var b strings.Builder
	for {
		i := strings.Index(s, k)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i+len(k)])
		s = s[i+len(k):]
		end := strings.IndexByte(s, '&')
		if end < 0 {
			end = len(s)
		}
		b.WriteString("REDACTED")
		s = s[end:]
	}
}

func sortedKeys(v url.Values) []string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// connector holds one *sql.DB's reference to an engine.
type connector struct {
	sh   *shared
	once sync.Once // Close is idempotent; releasing twice would close a live engine
}

var (
	_ driver.Connector = (*connector)(nil)
	_ io.Closer        = (*connector)(nil)
)

func (c *connector) Driver() driver.Driver { return Driver{} }

// Connect hands out one bytdb Session per pooled connection. database/sql
// serializes calls on a connection and a Session is single-threaded, which
// lines up exactly.
func (c *connector) Connect(context.Context) (driver.Conn, error) {
	return &conn{sh: c.sh, sess: c.sh.db.NewSession()}, nil
}

// Close releases this *sql.DB's reference to the engine. database/sql calls
// it from (*sql.DB).Close.
func (c *connector) Close() error {
	var err error
	c.once.Do(func() { err = c.sh.release() })
	return err
}

// shared is one open engine and the SQL frontend over it, shared by every
// connection that reached it.
type shared struct {
	// path is the registry key, and empty for a caller-supplied engine —
	// which is also how release knows not to close what it does not own.
	path      string
	signature string // the option set path was opened with

	e  *bytdb.Engine
	db *bsql.DB

	refs int // connectors holding this open; guarded by enginesMu

	// tempDir is a ":memory:" database's directory: the engine is this
	// connector's alone, and release closes it and removes the directory.
	tempDir string
}

var (
	enginesMu sync.Mutex
	engines   = map[string]*shared{}
)

// acquire opens the engine cfg names, or takes another reference to the one
// already open on that path.
func acquire(cfg *Config) (*shared, error) {
	if cfg.Memory {
		return openMemory(cfg)
	}
	// Two DSNs spelling one file differently must land on the same engine, or
	// the second open fails on bytdb's file lock (ErrLocked). Symlinks count
	// as spellings too: a link and its target are one file.
	abs, err := resolvePath(cfg.Path)
	if err != nil {
		return nil, serr.Wrap(err, "op", "resolve bytdb path", "path", cfg.Path)
	}

	enginesMu.Lock()
	defer enginesMu.Unlock()

	if sh, ok := engines[abs]; ok {
		// The engine is already open, so these options cannot be applied.
		// Adopting it silently would mean a DSN asking for encryption, or for
		// concurrent writes, quietly getting neither.
		if sh.signature != cfg.signature {
			return nil, serr.New("database already open in this process with different options",
				"path", abs, "open_with", orNone(sh.signature), "requested", orNone(cfg.signature))
		}
		sh.refs++
		return sh, nil
	}

	e, err := bytdb.Open(abs, cfg.Options...)
	if err != nil {
		return nil, serr.Wrap(err, "op", "open bytdb", "path", abs)
	}
	applySettings(e, cfg)
	sh := &shared{path: abs, signature: cfg.signature, e: e, db: bsql.New(e), refs: 1}
	engines[abs] = sh
	return sh, nil
}

// applySettings applies the DSN's engine settings to a fresh engine.
func applySettings(e *bytdb.Engine, cfg *Config) {
	if cfg.MemoryCeiling != nil {
		e.SetMemoryCeiling(*cfg.MemoryCeiling)
	}
	if cfg.MaxResultRows != nil {
		e.SetMaxResultRows(*cfg.MaxResultRows)
	}
	if cfg.MaxTxnWrites != nil {
		e.SetMaxTxnWrites(*cfg.MaxTxnWrites)
	}
}

// resolvePath makes a DSN path absolute with symlinks resolved. The file
// may not exist yet (first open creates it), so a missing file resolves
// through its directory instead.
func resolvePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r, nil
	}
	dir, base := filepath.Split(abs)
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Join(r, base), nil
	}
	return abs, nil
}

// openMemory opens a ":memory:" database: an engine on a file in a fresh
// temporary directory, outside the path registry (every open is its own
// database), removed when its last reference goes.
func openMemory(cfg *Config) (*shared, error) {
	dir, err := os.MkdirTemp("", "bytdb-memory-")
	if err != nil {
		return nil, serr.Wrap(err, "op", "create :memory: database")
	}
	e, err := bytdb.Open(filepath.Join(dir, "memory.bytdb"), cfg.Options...)
	if err != nil {
		os.RemoveAll(dir)
		return nil, serr.Wrap(err, "op", "open :memory: database")
	}
	applySettings(e, cfg)
	return &shared{tempDir: dir, signature: cfg.signature, e: e, db: bsql.New(e), refs: 1}, nil
}

func orNone(sig string) string {
	if sig == "" {
		return "(defaults)"
	}
	return sig
}

// release drops one reference, closing the engine when none are left —
// closing is what flushes the WAL and unlocks the file, so a reopened path
// gets a fresh engine rather than a stale one. An engine the caller owns
// (OpenEngine) is never closed here.
func (s *shared) release() error {
	if s.tempDir != "" {
		enginesMu.Lock()
		s.refs--
		last := s.refs <= 0
		enginesMu.Unlock()
		if !last {
			return nil
		}
		err := s.e.Close()
		os.RemoveAll(s.tempDir)
		if err != nil {
			return serr.Wrap(err, "op", "close :memory: database")
		}
		return nil
	}
	if s.path == "" {
		return nil // caller's engine, caller's Close
	}

	enginesMu.Lock()
	defer enginesMu.Unlock()

	if s.refs--; s.refs > 0 {
		return nil
	}
	delete(engines, s.path)
	if err := s.e.Close(); err != nil {
		return serr.Wrap(err, "op", "close bytdb", "path", s.path)
	}
	return nil
}

// conn is one pooled connection: a bytdb Session over the shared engine.
type conn struct {
	sh   *shared
	sess *bsql.Session

	// owner is set only on the Driver.Open path, where this connection is the
	// sole holder of the engine reference and must release it itself. Under
	// database/sql the *sql.DB owns the connector and this stays nil.
	owner *connector
}

var (
	_ driver.Conn               = (*conn)(nil)
	_ driver.ConnPrepareContext = (*conn)(nil)
	_ driver.ExecerContext      = (*conn)(nil)
	_ driver.QueryerContext     = (*conn)(nil)
	_ driver.ConnBeginTx        = (*conn)(nil)
	_ driver.Pinger             = (*conn)(nil)
	_ driver.SessionResetter    = (*conn)(nil)
	_ driver.Validator          = (*conn)(nil)
	_ driver.NamedValueChecker  = (*conn)(nil)
)

func (c *conn) Close() error {
	sess := c.sess
	c.sess = nil // IsValid reports the connection unusable from here on
	if sess == nil {
		return nil
	}
	err := sess.Close() // rolls back an open block
	if c.owner != nil {
		if cerr := c.owner.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

func (c *conn) IsValid() bool { return c.sess != nil }

// Ping runs the cheapest statement bytdb has. It touches no table, so it
// works against an empty database.
func (c *conn) Ping(ctx context.Context) error {
	_, err := c.run(ctx, "SELECT 1", nil)
	return err
}

// ResetSession returns a pooled connection to a clean state. The pool reuses
// connections across unrelated callers, so a transaction block one of them
// left open must not become the next one's problem.
func (c *conn) ResetSession(ctx context.Context) error {
	if c.sess == nil {
		return driver.ErrBadConn
	}
	if c.sess.Status() == bsql.TxIdle {
		return nil
	}
	if _, err := c.sess.ExecCtx(ctx, "ROLLBACK"); err != nil {
		return driver.ErrBadConn // a session that will not wind back is not reusable
	}
	return nil
}

// ExecContext runs one statement, or a script of several: migration
// runners (goose, golang-migrate, sql-migrate) hand a whole .sql file
// to Exec, which Postgres accepts through its simple protocol. A
// script runs as one unit (Session.ExecScript: an implicit transaction,
// DDL committing what precedes it) and reports the last statement's
// rows affected, as lib/pq does. Placeholders cannot span statements,
// so a script with arguments is Postgres's error for the same request.
func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if parts := bsql.SplitScript(query); len(parts) > 1 {
		if len(args) > 0 {
			return nil, serr.New("cannot insert multiple commands into a prepared statement")
		}
		if c.sess == nil {
			return nil, driver.ErrBadConn
		}
		results, err := c.sess.ExecScriptCtx(ctx, query)
		if err != nil {
			return nil, c.wrap(err, query)
		}
		n := 0
		if len(results) > 0 {
			n = results[len(results)-1].RowsAffected
		}
		return result(n), nil
	}
	res, err := c.run(ctx, query, args)
	if err != nil {
		return nil, err
	}
	return result(res.RowsAffected), nil
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	res, err := c.run(ctx, query, args)
	if err != nil {
		return nil, err
	}
	return newRows(res), nil
}

// run executes one statement in the session under ctx. bytdb's row pumps poll
// ctx, so a canceled query stops mid-scan rather than at the next statement.
func (c *conn) run(ctx context.Context, query string, args []driver.NamedValue) (*bsql.Result, error) {
	if c.sess == nil {
		return nil, driver.ErrBadConn
	}
	vals, err := argValues(args)
	if err != nil {
		return nil, err
	}
	res, err := c.sess.ExecCtx(ctx, query, vals...)
	if err != nil {
		return nil, c.wrap(err, query)
	}
	return res, nil
}

// wrap adds the database and statement to an error, so a message about a
// missing table says which database was missing it. serr keeps the cause
// reachable, so errors.Is still finds bytdb.ErrTxConflict through it.
func (c *conn) wrap(err error, query string) error {
	if err == nil {
		return nil
	}
	return serr.Wrap(err, "db", c.dbName(), "stmt", query)
}

func (c *conn) dbName() string {
	if c.sh.path == "" {
		return "(caller's engine)"
	}
	return c.sh.path
}

// argValues flattens database/sql's named values into the positional args
// bytdb binds to $1, $2, ... It has no named parameters, so a name is an
// error rather than something quietly ignored.
func argValues(args []driver.NamedValue) ([]any, error) {
	vals := make([]any, len(args))
	for _, a := range args {
		if a.Name != "" {
			return nil, serr.New("bytdb has no named parameters (use $1, $2, ...)",
				"name", a.Name)
		}
		if a.Ordinal < 1 || a.Ordinal > len(args) {
			return nil, serr.New("parameter ordinal out of range",
				"ordinal", strconv.Itoa(a.Ordinal), "params", strconv.Itoa(len(args)))
		}
		vals[a.Ordinal-1] = a.Value
	}
	return vals, nil
}

// CheckNamedValue converts an argument to something bytdb can bind. bytdb
// stores timestamps as epoch micros and has no time.Time in its parameter
// set, but it adapts text literals to a column's type the way Postgres does,
// so a time goes over as the text form it would have been typed as. Anything
// else takes database/sql's own conversion, which narrows the int and float
// families to the int64/float64 bytdb wants.
func (c *conn) CheckNamedValue(nv *driver.NamedValue) error {
	if t, ok := nv.Value.(time.Time); ok {
		nv.Value = t.UTC().Format("2006-01-02 15:04:05.999999")
		return nil
	}
	v, err := driver.DefaultParameterConverter.ConvertValue(nv.Value)
	if err != nil {
		// The shapes database/sql's converter refuses but bytdb binds:
		// []string for text[], [16]byte (and named 16-byte arrays, the
		// usual uuid types) for uuid (deep-look D-57).
		nv2, nerr := bytdb.NormalizeValue(nv.Value)
		if nerr != nil {
			return err
		}
		if t, ok := nv2.(time.Time); ok {
			nv2 = t.UTC().Format("2006-01-02 15:04:05.999999")
		}
		nv.Value = nv2
		return nil
	}
	nv.Value = v
	return nil
}

// BeginTx opens a transaction block, mapping database/sql's isolation levels
// onto BEGIN's. bytdb accepts the four Postgres levels (READ UNCOMMITTED
// folding into READ COMMITTED, as on Postgres); the levels database/sql
// defines beyond those have no bytdb meaning and are refused rather than
// silently downgraded.
func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	var modes []string

	switch sql.IsolationLevel(opts.Isolation) {
	case sql.LevelDefault:
		// the engine's own default: serializable under the writer lock,
		// snapshot isolation under concurrent writes
	case sql.LevelSerializable:
		modes = append(modes, "ISOLATION LEVEL SERIALIZABLE")
	case sql.LevelRepeatableRead, sql.LevelSnapshot:
		modes = append(modes, "ISOLATION LEVEL REPEATABLE READ")
	case sql.LevelReadCommitted, sql.LevelReadUncommitted:
		modes = append(modes, "ISOLATION LEVEL READ COMMITTED")
	default:
		return nil, serr.New("unsupported isolation level",
			"level", sql.IsolationLevel(opts.Isolation).String())
	}
	if opts.ReadOnly {
		modes = append(modes, "READ ONLY") // takes no writer lock
	}

	stmt := "BEGIN"
	if len(modes) > 0 {
		stmt += " " + strings.Join(modes, " ")
	}
	if _, err := c.run(ctx, stmt, nil); err != nil {
		return nil, err
	}
	return &tx{c: c}, nil
}

func (c *conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// tx is an open transaction block. Under concurrent writes, Commit is where a
// lost race surfaces, as bytdb.ErrTxConflict (see IsRetryable).
type tx struct{ c *conn }

// ErrCommitRolledBack is returned by Tx.Commit when the transaction block had
// already failed: following Postgres, COMMIT of an aborted block performs a
// rollback, and every write in the block is gone. Without this error a caller
// that missed (or a layer that swallowed) the original statement error would
// read Commit's nil as the data being durable — the same hazard lib/pq's
// ErrInFailedTransaction and pgx's ErrTxCommitRollback exist for. Match with
// errors.Is.
var ErrCommitRolledBack = errors.New("commit of an aborted transaction block: rolled back")

func (t *tx) Commit() error {
	res, err := t.c.run(context.Background(), "COMMIT", nil)
	if err != nil {
		return err
	}
	if res.Tag == "ROLLBACK" {
		return t.c.wrap(ErrCommitRolledBack, "COMMIT")
	}
	return nil
}

func (t *tx) Rollback() error {
	_, err := t.c.run(context.Background(), "ROLLBACK", nil)
	return err
}

// Prepare parses a statement once for repeated execution. The parsed form
// belongs to the shared DB rather than this session, which is fine: executing
// one binds parameters into a copy, and the session supplies the transaction.
func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *conn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	if c.sess == nil {
		return nil, driver.ErrBadConn
	}
	st, err := c.sh.db.Prepare(query)
	if err != nil {
		return nil, c.wrap(err, query)
	}
	return &stmt{c: c, st: st, query: query}, nil
}

type stmt struct {
	c     *conn
	st    *bsql.Stmt
	query string
}

var (
	_ driver.Stmt             = (*stmt)(nil)
	_ driver.StmtExecContext  = (*stmt)(nil)
	_ driver.StmtQueryContext = (*stmt)(nil)
)

// NumInput reports the statement's highest $n, so database/sql catches an
// argument-count mismatch before bytdb has to.
func (s *stmt) NumInput() int { return s.st.NumParams() }

func (s *stmt) Close() error { return nil } // nothing held open

func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	res, err := s.run(ctx, args)
	if err != nil {
		return nil, err
	}
	return result(res.RowsAffected), nil
}

func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	res, err := s.run(ctx, args)
	if err != nil {
		return nil, err
	}
	return newRows(res), nil
}

func (s *stmt) run(ctx context.Context, args []driver.NamedValue) (*bsql.Result, error) {
	if s.c.sess == nil {
		return nil, driver.ErrBadConn
	}
	vals, err := argValues(args)
	if err != nil {
		return nil, err
	}
	res, err := s.c.sess.ExecStmtCtx(ctx, s.st, vals...)
	if err != nil {
		return nil, s.c.wrap(err, s.query)
	}
	return res, nil
}

// Exec and Query are the pre-context forms driver.Stmt still requires.
func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), positional(args))
}

func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), positional(args))
}

func positional(args []driver.Value) []driver.NamedValue {
	nvs := make([]driver.NamedValue, len(args))
	for i, a := range args {
		nvs[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	return nvs
}

// result carries a statement's affected-row count. bytdb draws identity
// values through RETURNING rather than a last-insert-id, so there is no
// honest answer to LastInsertId.
type result int

func (r result) RowsAffected() (int64, error) { return int64(r), nil }

func (r result) LastInsertId() (int64, error) {
	return 0, serr.New("bytdb has no last insert id (use INSERT ... RETURNING)")
}

// rows walks a result set. bytdb executes a statement to completion and hands
// back its rows, so there is nothing to stream here and nothing to cancel;
// Close just drops the reference.
type rows struct {
	cols  []string
	types []bytdb.ColType
	vals  [][]any
	i     int
}

var (
	_ driver.Rows                           = (*rows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*rows)(nil)
	_ driver.RowsColumnTypeScanType         = (*rows)(nil)
)

func newRows(res *bsql.Result) *rows {
	return &rows{cols: res.Cols, types: res.Types, vals: res.Rows}
}

func (r *rows) Columns() []string { return r.cols }

func (r *rows) Close() error {
	r.vals, r.i = nil, 0
	return nil
}

func (r *rows) Next(dest []driver.Value) error {
	if r.i >= len(r.vals) {
		return io.EOF
	}
	row := r.vals[r.i]
	r.i++
	for i := range dest {
		if i >= len(row) {
			dest[i] = nil // a short row reads as NULL rather than panicking
			continue
		}
		v, err := convert(row[i], r.colType(i))
		if err != nil {
			return serr.Wrap(err, "column", r.colName(i))
		}
		dest[i] = v
	}
	return nil
}

// ColumnTypeDatabaseTypeName reports bytdb's own type name for a column
// ("INT", "TIMESTAMP", "JSONB", ...).
func (r *rows) ColumnTypeDatabaseTypeName(i int) string {
	return strings.ToUpper(string(r.colType(i)))
}

// ColumnTypeScanType reports the Go type a column's values arrive as, which
// is what sqlx and the ORMs use to pick a destination. Any column may be
// NULL, so scanning into the bare type needs a value that takes nil (a
// pointer, or a sql.Null[T]).
func (r *rows) ColumnTypeScanType(i int) reflect.Type {
	switch r.colType(i) {
	case bytdb.TBool:
		return reflect.TypeFor[bool]()
	case bytdb.TInt:
		return reflect.TypeFor[int64]()
	case bytdb.TFloat:
		return reflect.TypeFor[float64]()
	case bytdb.TBytes:
		return reflect.TypeFor[[]byte]()
	case bytdb.TTimestamp, bytdb.TDate:
		return reflect.TypeFor[time.Time]()
	case bytdb.TString, bytdb.TUUID, bytdb.TTextArray, bytdb.TJSONB, bytdb.TNumeric:
		// numeric arrives as its exact text, see convert.
		return reflect.TypeFor[string]()
	}
	return reflect.TypeFor[any]()
}

func (r *rows) colType(i int) bytdb.ColType {
	if i < len(r.types) {
		return r.types[i]
	}
	return ""
}

func (r *rows) colName(i int) string {
	if i < len(r.cols) {
		return r.cols[i]
	}
	return strconv.Itoa(i)
}

// convert turns one bytdb value into a driver.Value. The date/time and UUID
// types ride on int64 and []byte runtime representations so they sort in the
// key encoding, which means the column type is the only thing that says a
// number is a moment — without this a timestamp would reach the caller as
// epoch micros.
//
// A value outside the representations bytdb stores is an error, not a
// best-effort rendering: a caller scanning into a typed destination is better
// served by a clear failure here than by a surprise string.
func convert(v any, t bytdb.ColType) (driver.Value, error) {
	switch t {
	case bytdb.TTimestamp:
		if n, ok := v.(int64); ok {
			return time.UnixMicro(n).UTC(), nil
		}
	case bytdb.TDate:
		// A date scans as midnight UTC of that day, the time.Time pgx's and
		// lib/pq's drivers give, so `var d time.Time; row.Scan(&d)` works
		// (deep-look D-57; it used to be the text form, 2026-08-04).
		if n, ok := v.(int64); ok {
			return time.Unix(n*86400, 0).UTC(), nil
		}
	case bytdb.TUUID:
		if b, ok := v.([]byte); ok {
			return bytdb.FormatUUID(b), nil
		}
	}
	// A numeric (N-048) arrives as its exact text ("19.99"), as it does
	// from pgx's and lib/pq's drivers: driver.Value has no decimal kind,
	// a float64 would lose the exactness the column exists for, and
	// database/sql converts the text into a float64, an int or a
	// string destination, and into any sql.Scanner a decimal library
	// provides. A numeric-typed expression yielding an integer at
	// runtime renders the same way.
	if t == bytdb.TNumeric {
		switch x := v.(type) {
		case bytdb.Decimal:
			return x.String(), nil
		case int64:
			return strconv.FormatInt(x, 10), nil
		}
	}
	if d, ok := v.(bytdb.Decimal); ok { // a numeric under an untyped column
		return d.String(), nil
	}
	switch tv := v.(type) {
	case nil:
		return nil, nil
	case bool, int64, float64, string, []byte, time.Time:
		return tv, nil
	}
	return nil, serr.New("unsupported value from bytdb",
		"go_type", fmt.Sprintf("%T", v), "column_type", string(t))
}
