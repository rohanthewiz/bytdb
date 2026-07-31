package sql

// session.go: transaction blocks. A Session adds BEGIN/COMMIT/
// ROLLBACK state on top of a DB, with Postgres semantics: statements
// between BEGIN and COMMIT run in one engine transaction; any error
// inside the block puts the session in the failed state, where every
// statement but ROLLBACK (or COMMIT, which then rolls back) is
// refused. That failure rule is also what keeps failed statements
// atomic — a multi-row INSERT that dies halfway has staged rows in
// the open transaction, but they can only ever be rolled back.
//
// SAVEPOINT refines that: ROLLBACK TO a savepoint rewinds the
// transaction to the mark and clears the failed state, so a block can
// recover from an error instead of losing everything. Every savepoint
// predates the block's first error (a failed block refuses SAVEPOINT),
// so rewinding always discards the failed statement's partial writes
// along with everything after the mark.

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// TxStatus is a session's transaction state, in the wire protocol's
// ReadyForQuery terms.
type TxStatus byte

const (
	TxIdle   TxStatus = 'I' // no transaction block open
	TxActive TxStatus = 'T' // in a transaction block
	TxFailed TxStatus = 'E' // in a failed block; ROLLBACK to leave
)

// Session executes statements with transaction-block state: outside a
// block each statement autocommits like DB; BEGIN opens an engine
// transaction that following statements share until COMMIT or
// ROLLBACK. One session serves one client connection; it is not safe
// for concurrent use.
//
// What a writable block costs other sessions depends on the engine's
// write mode. Default (single-writer): the block holds the engine's
// writer lock from BEGIN to COMMIT/ROLLBACK, so writes in other
// sessions block behind it (reads do not — they run on snapshots).
// With bytdb.WithConcurrentWrites: BEGIN takes no lock, blocks in
// other sessions proceed concurrently under snapshot isolation, and
// contention surfaces at COMMIT as bytdb.ErrTxConflict — which a
// block is never retried past (the client saw its reads), while
// autocommit statements retry internally (see autocommitRetries).
// BEGIN ISOLATION LEVEL SERIALIZABLE opts the block up to full
// serializability in that mode. BEGIN READ ONLY takes no lock in any
// mode. DDL cannot run inside a block: the engine gives each schema
// change its own transaction.
type Session struct {
	db  *DB
	sdb *DB // db with the open transaction threaded in
	tx  *bytdb.Txn

	readOnly bool
	aborted  bool
	saves    []sesSave // savepoint stack, oldest first

	// defIso is the session's default_transaction_isolation when SET,
	// "" meaning the engine-mode default (isoDefault). txIso is the
	// open block's level, fixed at BEGIN or by SET TRANSACTION; stale
	// once the block ends (readers guard on s.tx != nil). txStmts
	// marks that a query has executed in the block, after which SET
	// TRANSACTION must be refused: reads already taken were not
	// tracked, so upgrading late would validate an incomplete read set
	// and silently void the serializable guarantee.
	defIso  string
	txIso   string
	txStmts bool

	// timeout is the session's statement_timeout: when positive, every
	// statement runs under a deadline that far away, and expiry aborts
	// it (SQLSTATE 57014 over the wire). Set with SET statement_timeout.
	timeout time.Duration

	// vars remembers other SET parameters verbatim. None change bytdb
	// behavior; remembering them (rather than erroring) is what lets
	// drivers' connect-time housekeeping SETs succeed.
	vars map[string]string
}

// sesSave is one named savepoint in the open block. Names may repeat;
// references resolve to the most recent, as in Postgres.
type sesSave struct {
	name string
	sp   *bytdb.Savepoint
}

// NewSession wraps the DB with per-connection transaction state. The
// session gets its own sequence-function state: lastval() reports
// this session's draws, not another connection's.
func (d *DB) NewSession() *Session {
	sdb := *d
	sdb.seq = &seqSession{}
	return &Session{db: &sdb}
}

// Status reports the session's transaction state.
func (s *Session) Status() TxStatus {
	switch {
	case s.tx == nil:
		return TxIdle
	case s.aborted:
		return TxFailed
	}
	return TxActive
}

// Close rolls back any open transaction block. The session is not
// usable afterward.
func (s *Session) Close() error {
	tx := s.tx
	s.tx, s.sdb, s.aborted, s.saves = nil, nil, false, nil
	if tx != nil {
		return tx.Rollback()
	}
	return nil
}

// Exec parses and executes one statement in the session, like DB.Exec
// but honoring the open transaction block.
func (s *Session) Exec(query string, args ...any) (*Result, error) {
	st, err := Parse(query)
	if err != nil {
		return nil, err
	}
	return s.run(st, args)
}

// ExecCtx is Exec bounded by ctx: cancellation aborts the statement
// mid-execution (see DB.ExecCtx). The session's statement_timeout, if
// set, applies on top as a deadline.
func (s *Session) ExecCtx(ctx context.Context, query string, args ...any) (*Result, error) {
	st, err := Parse(query)
	if err != nil {
		return nil, err
	}
	return s.runCtx(ctx, st, args)
}

// ExecStmt executes a prepared statement in the session. The Stmt
// must come from the session's DB.
func (s *Session) ExecStmt(stmt *Stmt, args ...any) (*Result, error) {
	return s.run(stmt.st, args)
}

// ExecStmtCtx is ExecStmt bounded by ctx; see ExecCtx.
func (s *Session) ExecStmtCtx(ctx context.Context, stmt *Stmt, args ...any) (*Result, error) {
	return s.runCtx(ctx, stmt.st, args)
}

// run dispatches one statement with no caller cancellation scope; the
// statement_timeout still applies.
func (s *Session) run(st Statement, args []any) (*Result, error) {
	return s.runCtx(context.Background(), st, args)
}

// runCtx dispatches one statement against the session's state, under
// ctx narrowed by the session's statement_timeout.
func (s *Session) runCtx(ctx context.Context, st Statement, args []any) (*Result, error) {
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	if tc, ok := st.(*TxnControl); ok {
		return s.txnControl(tc)
	}
	if s.aborted {
		return nil, serr.New("current transaction is aborted, " +
			"commands ignored until end of transaction block")
	}
	if sv, ok := st.(*SetVar); ok {
		return s.setVar(sv)
	}
	if sv, ok := st.(*ShowVar); ok {
		// SHOW reads the session's SET state over the defaults; the
		// isolation parameters report live state, not stored text.
		return execShow(sv, s.vars, s.timeout, s.isoShow())
	}
	if s.tx == nil {
		return s.db.runCtx(ctx, st, args)
	}
	// Inside a block. DDL would deadlock behind the block's own
	// writer lock (each engine schema change is its own transaction),
	// so refuse it up front; refuse writes in a read-only block
	// likewise. Any error — these included — fails the block.
	if isDDL(st) {
		s.aborted = true
		return nil, serr.New(command(st)+" cannot run inside a transaction block",
			"hint", "bytdb DDL is not transactional")
	}
	if s.readOnly && isWrite(st) {
		s.aborted = true
		return nil, serr.New("cannot execute " + command(st) +
			" in a read-only transaction")
	}
	// The block is about to touch data (even a failed statement may
	// have read), which closes the SET TRANSACTION window for good.
	s.txStmts = true
	res, err := s.sdb.runCtx(ctx, st, args)
	if err != nil {
		s.aborted = true
	}
	return res, err
}

// sessionIso is the isolation level a new transaction gets when the
// BEGIN names none: the SET default, else the engine mode's level.
func (s *Session) sessionIso() string {
	if s.defIso != "" {
		return s.defIso
	}
	return isoDefault(s.db.e.ConcurrentWrites())
}

// isoShow computes the live values SHOW reports for the two isolation
// parameters: the open block's level while one is open, the session
// default otherwise.
func (s *Session) isoShow() map[string]string {
	cur := s.sessionIso()
	if s.tx != nil {
		cur = s.txIso
	}
	return map[string]string{
		"transaction_isolation":         cur,
		"default_transaction_isolation": s.sessionIso(),
	}
}

// normalizeIso validates an isolation-parameter value, tolerating case
// and whitespace as Postgres does. READ UNCOMMITTED normalizes to read
// committed (Postgres treats them identically); false means the value
// names no isolation level at all.
func normalizeIso(v string) (string, bool) {
	switch strings.ToLower(strings.Join(strings.Fields(v), " ")) {
	case "serializable":
		return "serializable", true
	case "repeatable read":
		return "repeatable read", true
	case "read committed", "read uncommitted":
		return "read committed", true
	}
	return "", false
}

// setVar applies a SET or RESET. statement_timeout is the one
// parameter with bytdb semantics; the rest are remembered and
// otherwise ignored. Postgres scopes SET to the transaction only for
// SET LOCAL, which the parser already folded to session scope, and an
// erroring SET fails an open block via runCtx's normal path — but a
// successful one here deliberately does not join the block: with no
// transactional parameters, unwinding on ROLLBACK has nothing to
// restore.
func (s *Session) setVar(sv *SetVar) (*Result, error) {
	if sv.Name == "transaction_isolation" {
		return s.setTxnIso(sv)
	}
	if sv.Name == "default_transaction_isolation" {
		return s.setDefaultIso(sv)
	}
	if sv.Name == "statement_timeout" {
		d, err := parseTimeout(sv)
		if err != nil {
			if s.tx != nil {
				s.aborted = true
			}
			return nil, err
		}
		s.timeout = d
		return &Result{Tag: sv.Tag}, nil
	}
	if sv.Name == "all" && sv.IsDefault { // RESET ALL
		s.timeout = 0
		s.vars = nil
		s.defIso = ""
		s.db.serial = false
		return &Result{Tag: sv.Tag}, nil
	}
	if sv.IsDefault {
		delete(s.vars, sv.Name)
		return &Result{Tag: sv.Tag}, nil
	}
	if s.vars == nil {
		s.vars = map[string]string{}
	}
	s.vars[sv.Name] = sv.Value
	return &Result{Tag: sv.Tag}, nil
}

// setTxnIso applies SET TRANSACTION ISOLATION LEVEL (equivalently SET
// transaction_isolation). It only has an effect inside a block, and
// only before the block's first query — the upgrade to serializable
// works by tracking reads from now on, so reads already taken would
// escape validation. Both restrictions are Postgres's own, wording
// included.
func (s *Session) setTxnIso(sv *SetVar) (*Result, error) {
	lvl := s.sessionIso() // RESET transaction_isolation
	if !sv.IsDefault {
		var ok bool
		if lvl, ok = normalizeIso(sv.Value); !ok {
			if s.tx != nil {
				s.aborted = true
			}
			return nil, serr.New(`invalid value for parameter "transaction_isolation"`,
				"value", sv.Value)
		}
	}
	if s.tx == nil {
		// A warning, not an error, as in Postgres: drivers issue this
		// statement unconditionally on pool checkout.
		return &Result{Tag: sv.Tag,
			Notice: "SET TRANSACTION can only be used in transaction blocks"}, nil
	}
	if s.txStmts {
		s.aborted = true
		return nil, serr.New(
			"SET TRANSACTION ISOLATION LEVEL must be called before any query")
	}
	// Upgrading to serializable means tracking reads from here on; with
	// no query run yet, "from here on" is the whole block, so the
	// guarantee is intact. Only meaningful for a writable block under
	// concurrent writes — a read-only serializable transaction always
	// commits, and the single-writer default is serializable already —
	// and there is no downgrade: once tracking, a weaker requested
	// level just means validating reads nobody required us to (sound,
	// merely conservative).
	if lvl == "serializable" && s.txIso != "serializable" &&
		!s.readOnly && s.db.e.ConcurrentWrites() {
		s.tx.TrackReads()
	}
	s.txIso = lvl
	return &Result{Tag: sv.Tag}, nil
}

// setDefaultIso applies SET default_transaction_isolation (also the
// lowered form of SET SESSION CHARACTERISTICS AS TRANSACTION
// ISOLATION LEVEL): the default for transactions that do not name a
// level — including autocommit statements, which under concurrent
// writes really do run serializable when the default says so (two
// overlapping single statements can write-skew just like two blocks).
func (s *Session) setDefaultIso(sv *SetVar) (*Result, error) {
	if sv.IsDefault {
		s.defIso = ""
		s.db.serial = false
		return &Result{Tag: sv.Tag}, nil
	}
	lvl, ok := normalizeIso(sv.Value)
	if !ok {
		if s.tx != nil {
			s.aborted = true
		}
		return nil, serr.New(`invalid value for parameter "default_transaction_isolation"`,
			"value", sv.Value)
	}
	s.defIso = lvl
	// The serial flag reaches every autocommit write through the
	// session's DB copy; gate on the engine mode so the single-writer
	// default never pays for read tracking it does not need.
	s.db.serial = lvl == "serializable" && s.db.e.ConcurrentWrites()
	return &Result{Tag: sv.Tag}, nil
}

// parseTimeout reads a statement_timeout value the way Postgres does:
// a bare integer is milliseconds; a string may carry one of the time
// units us, ms, s, min, h, or d. Zero (or DEFAULT/RESET) disables the
// timeout; negative values are invalid.
func parseTimeout(sv *SetVar) (time.Duration, error) {
	if sv.IsDefault {
		return 0, nil
	}
	text := strings.TrimSpace(sv.Value)
	num, unit := text, ""
	for i, r := range text {
		if (r < '0' || r > '9') && r != '-' && r != '.' {
			num, unit = strings.TrimSpace(text[:i]), strings.TrimSpace(text[i:])
			break
		}
	}
	n, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, serr.New(`invalid value for parameter "statement_timeout"`, "value", sv.Value)
	}
	scale := time.Millisecond
	switch unit {
	case "", "ms":
	case "us":
		scale = time.Microsecond
	case "s":
		scale = time.Second
	case "min":
		scale = time.Minute
	case "h":
		scale = time.Hour
	case "d":
		scale = 24 * time.Hour
	default:
		return 0, serr.New(`invalid value for parameter "statement_timeout"`,
			"value", sv.Value, "hint", "valid units are us, ms, s, min, h, and d")
	}
	if n < 0 {
		return 0, serr.New(`invalid value for parameter "statement_timeout"`,
			"value", sv.Value, "hint", "the timeout must be zero (disabled) or positive")
	}
	return time.Duration(n * float64(scale)), nil
}

// txnControl handles BEGIN, COMMIT, ROLLBACK, and the savepoint
// statements. Redundant BEGIN/COMMIT/ROLLBACK forms warn and do
// nothing, as in Postgres; COMMIT of a failed block rolls back and
// says so in its tag. Savepoint statements outside a block are
// errors, not warnings, again as in Postgres.
func (s *Session) txnControl(tc *TxnControl) (*Result, error) {
	switch tc.Kind {
	case TxnBegin:
		if s.tx != nil {
			return &Result{Notice: "there is already a transaction in progress"}, nil
		}
		iso := tc.Isolation
		if iso == "" {
			iso = s.sessionIso()
		}
		// A serializable writable block under concurrent writes gets
		// read tracking from the start; anywhere else — weaker levels,
		// read-only blocks (which always commit), or the single-writer
		// default (serializable for free) — the plain Begin already
		// delivers at least the level asked for.
		var tx *bytdb.Txn
		var err error
		if iso == "serializable" && !tc.ReadOnly && s.db.e.ConcurrentWrites() {
			tx, err = s.db.e.BeginSerializable()
		} else {
			tx, err = s.db.e.Begin(!tc.ReadOnly)
		}
		if err != nil {
			return nil, err
		}
		s.tx, s.readOnly, s.aborted, s.saves = tx, tc.ReadOnly, false, nil
		s.txIso, s.txStmts = iso, false
		s.sdb = &DB{e: s.db.e, tx: tx, seq: s.db.seq}
		return &Result{}, nil
	case TxnCommit:
		if s.tx == nil {
			return &Result{Notice: "there is no transaction in progress"}, nil
		}
		tx, aborted := s.tx, s.aborted
		s.tx, s.sdb, s.aborted, s.saves = nil, nil, false, nil
		if aborted {
			if err := tx.Rollback(); err != nil {
				return nil, err
			}
			return &Result{Tag: "ROLLBACK"}, nil
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return &Result{}, nil
	case TxnRollback:
		if s.tx == nil {
			return &Result{Notice: "there is no transaction in progress"}, nil
		}
		tx := s.tx
		s.tx, s.sdb, s.aborted, s.saves = nil, nil, false, nil
		if err := tx.Rollback(); err != nil {
			return nil, err
		}
		return &Result{}, nil
	default:
		return s.savepointControl(tc)
	}
}

// savepointControl handles SAVEPOINT, RELEASE, and ROLLBACK TO within
// the open block. ROLLBACK TO is the one statement besides COMMIT and
// ROLLBACK that a failed block accepts: it rewinds the transaction —
// staged writes, indexes, and the WAL batch — to the mark (an O(1)
// copy-on-write snapshot) and clears the failed state. A savepoint
// name may repeat; references resolve to the most recent, and RELEASE
// or ROLLBACK TO destroys every savepoint after the one named.
func (s *Session) savepointControl(tc *TxnControl) (*Result, error) {
	verb := "SAVEPOINT"
	switch tc.Kind {
	case TxnRelease:
		verb = "RELEASE SAVEPOINT"
	case TxnRollbackTo:
		verb = "ROLLBACK TO SAVEPOINT"
	}
	if s.tx == nil {
		return nil, serr.New(verb + " can only be used in transaction blocks")
	}
	if s.aborted && tc.Kind != TxnRollbackTo {
		return nil, serr.New("current transaction is aborted, " +
			"commands ignored until end of transaction block")
	}
	if tc.Kind == TxnSavepoint {
		sp, err := s.tx.Savepoint()
		if err != nil {
			s.aborted = true
			return nil, err
		}
		s.saves = append(s.saves, sesSave{tc.Name, sp})
		return &Result{}, nil
	}
	i := len(s.saves) - 1
	for ; i >= 0 && s.saves[i].name != tc.Name; i-- {
	}
	if i < 0 { // an error like any other: it fails the block
		s.aborted = true
		return nil, serr.New(`savepoint "` + tc.Name + `" does not exist`)
	}
	if tc.Kind == TxnRelease {
		if err := s.tx.Release(s.saves[i].sp); err != nil {
			s.aborted = true
			return nil, err
		}
		s.saves = s.saves[:i]
		return &Result{}, nil
	}
	if err := s.tx.RollbackTo(s.saves[i].sp); err != nil {
		s.aborted = true
		return nil, err
	}
	s.saves, s.aborted = s.saves[:i+1], false
	return &Result{}, nil
}

// isDDL reports whether st changes the schema.
func isDDL(st Statement) bool {
	switch st.(type) {
	case *CreateTable, *DropTable, *AddColumn, *DropColumn,
		*RenameTable, *RenameColumn,
		*AddConstraint, *AddFK, *DropConstraint, *CreateIndex, *DropIndex,
		*CreateSequence, *DropSequence, *AlterSequence,
		*CreateView, *DropView:
		return true
	}
	return false
}

// isWrite reports whether st writes at all (DML or DDL). TRUNCATE is
// deliberately DML here — like Postgres, it runs inside transaction
// blocks (and rolls back with them), unlike bytdb DDL.
func isWrite(st Statement) bool {
	switch st.(type) {
	case *Insert, *Update, *Delete, *Truncate:
		return true
	}
	return isDDL(st)
}
