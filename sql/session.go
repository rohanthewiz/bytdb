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
	"math"
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

	// armed and implicit implement Postgres's implicit transaction for a
	// unit of several statements — a simple Query holding more than one,
	// or an extended-protocol pipeline segment up to its Sync (deep-look
	// D-32). armed marks such a unit in progress (ArmImplicit); the
	// first data-modifying statement in it then opens a block, and
	// implicit marks that block as the unit's, which EndImplicit commits
	// (or rolls back, if it failed). Without it each statement
	// autocommitted, so in "UPDATE acct ...; INSERT <duplicate>" the
	// update stayed committed though the batch failed.
	//
	// The block opens lazily, at the first write: reads before it run as
	// they always did, so a read-only batch never takes the writer lock.
	// DDL cannot join a block in bytdb, so DDL inside a unit commits the
	// implicit block first and runs on its own (documented in
	// docs/gotchas.md); the next write opens a fresh block.
	armed    bool
	implicit bool

	// defReadOnly is default_transaction_read_only: blocks that name no
	// access mode begin READ ONLY, and autocommit writes are refused, as
	// in Postgres (deep-look D-51).
	defReadOnly bool

	// deallocate is the wire server's hook for DEALLOCATE and DISCARD
	// ALL: protocol-level prepared statements live in the server, not
	// here, but SQL can name them. name "" means all. nil (no server):
	// there are no named statements to drop.
	deallocate func(name string) error
}

// SetBackendPID makes pg_backend_pid() report the wire server's
// BackendKeyData pid, so it matches pg_stat_activity.pid.
func (s *Session) SetBackendPID(pid int32) { s.db.ss.pid = pid }

// OnDeallocate installs the hook DEALLOCATE and DISCARD ALL call to
// drop protocol-level prepared statements; name "" means all of them.
// The hook runs on the session's goroutine (the connection's).
func (s *Session) OnDeallocate(f func(name string) error) { s.deallocate = f }

// ApplyStartupParams applies a connection's startup parameters as SET
// would, as Postgres does: application_name (and a driver's DateStyle,
// TimeZone, extra_float_digits, ...) become visible to SHOW and
// current_setting, and statement_timeout or
// default_transaction_isolation take effect. The protocol's own keys
// are skipped. An invalid value is an error, which fails the
// connection, as in Postgres.
func (s *Session) ApplyStartupParams(params map[string]string) error {
	for k, v := range params {
		switch k = strings.ToLower(k); k {
		case "user", "database", "replication", "options", "":
			continue
		}
		if _, err := s.setVar(&SetVar{Name: k, Value: v, Tag: "SET"}); err != nil {
			return err
		}
	}
	return nil
}

// endBlock clears the open block's state after it commits or rolls
// back; its transaction-level advisory locks end with it.
func (s *Session) endBlock() {
	s.tx, s.sdb, s.aborted, s.saves = nil, nil, false, nil
	s.db.endXact()
}

// ArmImplicit marks the start of a multi-statement unit; see
// Session.armed. A no-op inside an explicit block, which already is the
// unit's transaction.
func (s *Session) ArmImplicit() {
	if s.tx == nil || s.implicit {
		s.armed = true
	}
}

// EndImplicit ends a unit begun with ArmImplicit: an implicit block it
// opened is committed, or rolled back if a statement in it failed. The
// error is the commit's (or rollback's); a failed block is rolled back
// silently, since its error was already reported. Explicit blocks are
// untouched.
func (s *Session) EndImplicit() error {
	s.armed = false
	if !s.implicit || s.tx == nil {
		s.implicit = false
		return nil
	}
	s.implicit = false
	tx, aborted := s.tx, s.aborted
	s.endBlock()
	if aborted {
		return tx.Rollback()
	}
	return tx.Commit()
}

// beginImplicit opens the unit's implicit block, as BEGIN would.
func (s *Session) beginImplicit(ctx context.Context) error {
	if _, err := s.txnControl(ctx, &TxnControl{Kind: TxnBegin, Tag: "BEGIN"}); err != nil {
		return err
	}
	s.implicit = true
	return nil
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
	s := &Session{db: &sdb}
	sdb.ss = newSessState(s) // its own pid, settings and lock ownership
	return s
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

// Close rolls back any open transaction block and releases the
// session's advisory locks — at both levels, as Postgres does when a
// client disconnects, gracefully or not. The session is not usable
// afterward.
func (s *Session) Close() error {
	tx := s.tx
	s.endBlock()
	s.db.adv.releaseOwner(s.db.ss, true, true)
	if tx != nil {
		return tx.Rollback()
	}
	return nil
}

// Exec parses and executes one statement in the session, like DB.Exec
// but honoring the open transaction block.
func (s *Session) Exec(query string, args ...any) (*Result, error) {
	return s.ExecCtx(context.Background(), query, args...)
}

// ExecCtx is Exec bounded by ctx: cancellation aborts the statement
// mid-execution (see DB.ExecCtx). The session's statement_timeout, if
// set, applies on top as a deadline.
//
// A statement that fails to parse fails an open block exactly as one
// that fails at run time does. It used to return before reaching the
// block's bookkeeping, so BEGIN; INSERT ...; <typo>; COMMIT committed
// the insert, where Postgres answers the COMMIT with ROLLBACK (deep-look
// D-07).
func (s *Session) ExecCtx(ctx context.Context, query string, args ...any) (*Result, error) {
	st, err := s.db.parse(query)
	if err != nil {
		s.Fail()
		return nil, err
	}
	return s.runCtx(ctx, st, args)
}

// Fail marks an open transaction block failed, as an error inside the
// block does: every later statement except ROLLBACK, COMMIT (which then
// rolls back) and ROLLBACK TO SAVEPOINT is refused until the block
// ends. Outside a block it does nothing. It exists for errors that
// happen before a statement reaches the session — a Parse, Bind or
// Describe failure on the wire — which Postgres treats like any other
// error in the block.
func (s *Session) Fail() {
	if s.tx != nil {
		s.aborted = true
	}
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
		if s.implicit {
			// Explicit transaction control takes the implicit block over
			// (as in Postgres): BEGIN makes it an ordinary block the unit's
			// end will not commit; COMMIT/ROLLBACK end it here.
			s.implicit = false
			if tc.Kind == TxnBegin {
				return &Result{}, nil
			}
		}
		return s.txnControl(ctx, tc)
	}
	if s.implicit && s.tx != nil && !s.aborted && (isDDL(st) || isVacuum(st)) {
		// DDL cannot join a block: commit the unit's writes so far and run
		// it on its own (the carve-out documented with Session.armed).
		if err := s.EndImplicit(); err != nil {
			return nil, err
		}
		s.armed = true
	}
	if s.armed && s.tx == nil && isWrite(st) && !isDDL(st) {
		if err := s.beginImplicit(ctx); err != nil {
			return nil, err
		}
	}
	if s.aborted {
		return nil, serr.New("current transaction is aborted, " +
			"commands ignored until end of transaction block")
	}
	if sv, ok := st.(*SetVar); ok {
		return s.setVar(sv)
	}
	switch st := st.(type) {
	case *Discard:
		return s.discard(st)
	case *Deallocate:
		return s.deallocateStmt(st)
	}
	if s.tx == nil && s.defReadOnly && isWrite(st) {
		// default_transaction_read_only: an autocommit statement is a
		// transaction of its own, and it starts read-only.
		return nil, serr.New("cannot execute " + command(st) + " in a read-only transaction")
	}
	if sv, ok := st.(*ShowVar); ok {
		// SHOW reads the session's SET state over the defaults; the
		// isolation parameters report live state, not stored text.
		return execShow(sv, s.vars, s.timeout, s.liveParams())
	}
	if s.tx == nil {
		return s.db.runCtx(ctx, st, args)
	}
	// Inside a block. DDL would deadlock behind the block's own
	// writer lock (each engine schema change is its own transaction),
	// so refuse it up front; refuse writes in a read-only block
	// likewise. Any error — these included — fails the block.
	// VACUUM is not DDL but is refused for the same kind of reason, with
	// Postgres's wording: compaction rewrites the file beneath every
	// open snapshot, and there is nothing a ROLLBACK could undo.
	if _, ok := st.(*Vacuum); ok {
		s.aborted = true
		return nil, serr.New("VACUUM cannot run inside a transaction block")
	}
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

// liveParams computes the live values SHOW and current_setting report
// for the parameters the session gives semantics: the isolation levels
// (the open block's while one is open, the session default otherwise)
// and the read-only state, likewise.
func (s *Session) liveParams() map[string]string {
	cur := s.sessionIso()
	ro := s.defReadOnly
	if s.tx != nil {
		cur, ro = s.txIso, s.readOnly
	}
	return map[string]string{
		"transaction_isolation":         cur,
		"default_transaction_isolation": s.sessionIso(),
		"transaction_read_only":         onOff(ro),
		"default_transaction_read_only": onOff(s.defReadOnly),
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
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
	for _, more := range sv.Also { // SET TRANSACTION with several modes
		if _, err := s.setVar(more); err != nil {
			return nil, err
		}
	}
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
	switch sv.Name {
	case "transaction_read_only":
		return s.setTxnReadOnly(sv)
	case "default_transaction_read_only":
		ro := false
		if !sv.IsDefault {
			var err error
			if ro, err = boolParam(sv); err != nil {
				if s.tx != nil {
					s.aborted = true
				}
				return nil, err
			}
		}
		s.defReadOnly = ro
		return &Result{Tag: sv.Tag}, nil
	}
	if sv.Name == "all" && sv.IsDefault { // RESET ALL
		s.timeout = 0
		s.vars = nil
		s.defIso = ""
		s.defReadOnly = false
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

// setTxnReadOnly applies SET TRANSACTION READ ONLY / READ WRITE
// (equivalently SET transaction_read_only). Outside a block it warns,
// as SET TRANSACTION does. READ ONLY may come at any point of a block
// (later writes are refused). READ WRITE in a READ ONLY block is
// refused: such a block began without the writer lock (or, under
// concurrent writes, without a write transaction), so it cannot become
// writable — Postgres allows it before the first query; bytdb cannot.
func (s *Session) setTxnReadOnly(sv *SetVar) (*Result, error) {
	ro := s.defReadOnly // RESET transaction_read_only
	if !sv.IsDefault {
		var err error
		if ro, err = boolParam(sv); err != nil {
			if s.tx != nil {
				s.aborted = true
			}
			return nil, err
		}
	}
	if s.tx == nil {
		return &Result{Tag: sv.Tag,
			Notice: "SET TRANSACTION can only be used in transaction blocks"}, nil
	}
	if !ro && s.readOnly {
		s.aborted = true
		return nil, serr.New("cannot make a READ ONLY transaction block writable",
			"hint", "begin the block READ WRITE instead")
	}
	s.readOnly = ro
	return &Result{Tag: sv.Tag}, nil
}

// boolParam reads an on/off parameter value with bytdb.ParseBool, which
// accepts Postgres's spellings (on, off, true, false, yes, no, 1, 0).
func boolParam(sv *SetVar) (bool, error) {
	b, err := bytdb.ParseBool(sv.Value)
	if err != nil {
		return false, serr.New(`parameter "`+sv.Name+`" requires a Boolean value`, "value", sv.Value)
	}
	return b, nil
}

// discard runs DISCARD: ALL resets everything a session carries — SET
// state, protocol-level prepared statements, session-level advisory
// locks, sequence-function state — as a connection pool's reset query
// expects; SEQUENCES resets the last of these alone. PLANS and TEMP
// have nothing to drop (no plan cache per session, no temporary
// tables) and are accepted.
func (s *Session) discard(st *Discard) (*Result, error) {
	switch st.What {
	case "all":
		if s.tx != nil {
			s.aborted = true
			return nil, serr.New("DISCARD ALL cannot run inside a transaction block")
		}
		if _, err := s.setVar(&SetVar{Name: "all", IsDefault: true}); err != nil {
			return nil, err
		}
		if s.deallocate != nil {
			if err := s.deallocate(""); err != nil {
				return nil, err
			}
		}
		s.db.adv.releaseOwner(s.db.ss, true, false)
		s.db.seq.reset()
	case "sequences":
		s.db.seq.reset()
	}
	return &Result{Tag: command(st)}, nil
}

// deallocateStmt runs DEALLOCATE [PREPARE] name | ALL against the wire
// server's protocol-level statements (SQL PREPARE does not exist here,
// so those are the only named statements there are).
func (s *Session) deallocateStmt(st *Deallocate) (*Result, error) {
	var err error
	switch {
	case s.deallocate != nil:
		err = s.deallocate(st.Name)
	case st.Name != "":
		err = noPrepared(st.Name)
	}
	if err != nil {
		if s.tx != nil {
			s.aborted = true
		}
		return nil, err
	}
	return &Result{Tag: command(st)}, nil
}

// noPrepared is Postgres's error for DEALLOCATE of an unknown name.
func noPrepared(name string) error {
	return serr.New(`prepared statement "` + name + `" does not exist`)
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
	// Postgres keeps the parameter as an int of milliseconds, so the
	// range tops out at 2147483647 ms (~24.8 days). Converting a larger
	// float to time.Duration overflowed into a negative or garbage
	// timeout (deep-look D-21).
	if ms := n * float64(scale) / float64(time.Millisecond); ms > math.MaxInt32 {
		return 0, serr.New(`invalid value for parameter "statement_timeout"`,
			"value", sv.Value, "hint", "the timeout must be at most 2147483647 ms")
	}
	return time.Duration(n * float64(scale)), nil
}

// txnControl handles BEGIN, COMMIT, ROLLBACK, and the savepoint
// statements. Redundant BEGIN/COMMIT/ROLLBACK forms warn and do
// nothing, as in Postgres; COMMIT of a failed block rolls back and
// says so in its tag. Savepoint statements outside a block are
// errors, not warnings, again as in Postgres.
//
// ctx bounds BEGIN's wait for the writer lock (single-writer mode):
// statement_timeout and a CancelRequest now stop a BEGIN queued behind
// another session's block (deep-look D-33).
func (s *Session) txnControl(ctx context.Context, tc *TxnControl) (*Result, error) {
	switch tc.Kind {
	case TxnBegin:
		if s.tx != nil {
			// Postgres refuses BEGIN in a failed block like any other
			// command (25P02) and only warns in a healthy one.
			if s.aborted {
				return nil, serr.New("current transaction is aborted, " +
					"commands ignored until end of transaction block")
			}
			return &Result{Notice: "there is already a transaction in progress"}, nil
		}
		iso := tc.Isolation
		if iso == "" {
			iso = s.sessionIso()
		}
		// A block naming no access mode takes default_transaction_read_only.
		readOnly := tc.ReadOnly || (s.defReadOnly && !tc.ReadWrite)
		// A serializable writable block under concurrent writes gets
		// read tracking from the start; anywhere else — weaker levels,
		// read-only blocks (which always commit), or the single-writer
		// default (serializable for free) — the plain Begin already
		// delivers at least the level asked for.
		var tx *bytdb.Txn
		var err error
		if iso == "serializable" && !readOnly && s.db.e.ConcurrentWrites() {
			tx, err = s.db.e.BeginSerializableCtx(ctx)
		} else {
			tx, err = s.db.e.BeginCtx(ctx, !readOnly)
		}
		if err != nil {
			return nil, err
		}
		s.tx, s.readOnly, s.aborted, s.saves = tx, readOnly, false, nil
		s.txIso, s.txStmts = iso, false
		// The block's DB carries the session's per-connection state along
		// with the transaction: sequence functions, session functions and
		// advisory locks, and pg_stat_activity's provider.
		s.sdb = &DB{e: s.db.e, tx: tx, seq: s.db.seq, ss: s.db.ss, adv: s.db.adv,
			activity: s.db.activity}
		return &Result{}, nil
	case TxnCommit:
		if s.tx == nil {
			return &Result{Notice: "there is no transaction in progress"}, nil
		}
		tx, aborted := s.tx, s.aborted
		s.endBlock()
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
		s.endBlock()
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

func isVacuum(st Statement) bool {
	_, ok := st.(*Vacuum)
	return ok
}

// isDDL reports whether st changes the schema.
func isDDL(st Statement) bool {
	switch st.(type) {
	case *CreateTable, *DropTable, *AddColumn, *DropColumn,
		*RenameTable, *RenameColumn, *AlterColumnDefault, *AlterColumnNotNull,
		*AlterColumnType, *ReplacePrimaryKey,
		*AddConstraint, *AddFK, *DropConstraint, *CreateIndex, *DropIndex,
		*CreateSequence, *DropSequence, *AlterSequence,
		*CreateView, *DropView:
		return true
	}
	return false
}

// isWrite reports whether st writes at all (DML or DDL). TRUNCATE is
// deliberately DML here — like Postgres, it runs inside transaction
// blocks (and rolls back with them), unlike bytdb DDL. ALTER COLUMN
// ... RESTART is the same kind of write: it moves an identity counter
// in the statement's transaction and leaves the schema alone.
func isWrite(st Statement) bool {
	switch st.(type) {
	case *Insert, *Update, *Delete, *Truncate, *AlterColumnRestart:
		return true
	}
	return isDDL(st)
}
