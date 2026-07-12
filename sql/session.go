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
// A writable block holds the engine's single-writer lock from BEGIN
// to COMMIT/ROLLBACK, so writes in other sessions block behind it
// (reads do not — they run on snapshots). BEGIN READ ONLY takes no
// lock. DDL cannot run inside a block: the engine gives each schema
// change its own transaction.
type Session struct {
	db  *DB
	sdb *DB // db with the open transaction threaded in
	tx  *bytdb.Txn

	readOnly bool
	aborted  bool
	saves    []sesSave // savepoint stack, oldest first

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
		// SHOW reads the session's SET state over the defaults.
		return execShow(sv, s.vars, s.timeout)
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
	res, err := s.sdb.runCtx(ctx, st, args)
	if err != nil {
		s.aborted = true
	}
	return res, err
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
		tx, err := s.db.e.Begin(!tc.ReadOnly)
		if err != nil {
			return nil, err
		}
		s.tx, s.readOnly, s.aborted, s.saves = tx, tc.ReadOnly, false, nil
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
		*AddConstraint, *DropConstraint, *CreateIndex, *DropIndex,
		*CreateSequence, *DropSequence, *AlterSequence:
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
