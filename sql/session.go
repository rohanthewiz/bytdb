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
}

// sesSave is one named savepoint in the open block. Names may repeat;
// references resolve to the most recent, as in Postgres.
type sesSave struct {
	name string
	sp   *bytdb.Savepoint
}

// NewSession wraps the DB with per-connection transaction state.
func (d *DB) NewSession() *Session { return &Session{db: d} }

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

// ExecStmt executes a prepared statement in the session. The Stmt
// must come from the session's DB.
func (s *Session) ExecStmt(stmt *Stmt, args ...any) (*Result, error) {
	return s.run(stmt.st, args)
}

// run dispatches one statement against the session's state.
func (s *Session) run(st Statement, args []any) (*Result, error) {
	if tc, ok := st.(*TxnControl); ok {
		return s.txnControl(tc)
	}
	if s.aborted {
		return nil, serr.New("current transaction is aborted, " +
			"commands ignored until end of transaction block")
	}
	if s.tx == nil {
		return s.db.run(st, args)
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
	res, err := s.sdb.run(st, args)
	if err != nil {
		s.aborted = true
	}
	return res, err
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
		s.sdb = &DB{e: s.db.e, tx: tx}
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
		*AddConstraint, *DropConstraint, *CreateIndex, *DropIndex:
		return true
	}
	return false
}

// isWrite reports whether st writes at all (DML or DDL).
func isWrite(st Statement) bool {
	switch st.(type) {
	case *Insert, *Update, *Delete:
		return true
	}
	return isDDL(st)
}
