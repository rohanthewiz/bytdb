package sql

// sessinfo.go: the session surface clients and tools touch on connect
// and around migrations (deep-look D-51) — advisory locks, the session
// information functions (pg_backend_pid, current_setting, set_config,
// txid_current, ...), and the per-session state they read.
//
// Functions evaluate against a *DB, not a *Session, and a session's
// statements run on copies of its DB (the copy-the-DB convention; a
// block runs on yet another DB carrying the open transaction). So the
// state a function needs rides those copies by pointer, exactly as the
// sequence-function state (seq) does:
//
//	Session ─► db  (DB copy) ─┐
//	        └► sdb (DB copy, open block) ─┤─► ss *sessState ─► ses *Session
//	                                      └─► adv *advLocks (one per New)
//
// A bare DB (no Session) is one shared session: New gives it a
// sessState of its own with no Session behind it.

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// sessState is the per-session state functions reach through the DB.
// Only the session's own goroutine touches its fields (a Session is not
// safe for concurrent use), so they need no lock; the shared advisory
// lock table synchronizes itself.
type sessState struct {
	// pid is what pg_backend_pid() reports: the wire server's
	// BackendKeyData pid once it calls SetBackendPID, otherwise a
	// process-unique number drawn at creation.
	pid int32
	// ses is the Session the state belongs to; nil for a bare DB, whose
	// SET state does not exist (current_setting reads the defaults).
	ses *Session
	// txidTx and txid memoize txid_current() for the open block: the
	// same transaction answers the same id, as in Postgres.
	txidTx *bytdb.Txn
	txid   int64
	// xactHeld counts transaction-level advisory holds taken since the
	// last transaction end, so ending a transaction that took none —
	// nearly every one — skips the shared lock table entirely: one
	// atomic load per autocommit statement instead of a shared mutex.
	// Atomic because a bare DB's one state is shared by every goroutine
	// that calls Exec.
	xactHeld atomic.Int32
}

// localPIDs numbers sessions that no wire server names. Starting high
// keeps them clear of a server's own counter in logs.
var localPIDs atomic.Int32

func newSessState(ses *Session) *sessState {
	return &sessState{pid: 100000 + localPIDs.Add(1), ses: ses}
}

// txids is the txid_current() counter: unique within the process, not
// durable — bytdb has no transaction ids of its own to report.
var txids atomic.Int64

// --- advisory locks ---

// advKey is one advisory lock name. Postgres keeps the one-bigint and
// the two-int4 key spaces apart (pg_advisory_lock(1) and
// pg_advisory_lock(0, 1) are different locks), so the form is part of
// the key.
type advKey struct {
	a, b int64
	pair bool
}

// advLocks is the advisory lock table shared by every session created
// from one DB (a wire server's sessions all come from one). Locks are
// exclusive and reentrant per owner: a session that takes a lock twice
// must release it twice, and session-level and transaction-level holds
// on one key count separately, both as in Postgres. Shared-mode locks
// (pg_advisory_lock_shared) are not implemented.
type advLocks struct {
	mu   sync.Mutex
	held map[advKey]*advHold
}

// advHold is one held lock. freed is closed when the lock is released
// entirely; waiters block on it and then race to take the lock afresh,
// so no queue is kept (Postgres does not promise FIFO either).
type advHold struct {
	owner   *sessState
	session int // session-level holds, released by pg_advisory_unlock
	xact    int // transaction-level holds, released at transaction end
	freed   chan struct{}
}

func newAdvLocks() *advLocks { return &advLocks{held: map[advKey]*advHold{}} }

// acquire takes k for owner. With wait it blocks until the lock is free
// or ctx ends (statement_timeout and a wire CancelRequest both reach
// it); without, it reports whether the lock was taken. There is no
// deadlock detector: two sessions waiting on each other's locks wait
// until a timeout or cancel ends one.
func (l *advLocks) acquire(ctx context.Context, k advKey, owner *sessState, xact, wait bool) (bool, error) {
	for {
		l.mu.Lock()
		h := l.held[k]
		if h == nil {
			h = &advHold{owner: owner, freed: make(chan struct{})}
			l.held[k] = h
		}
		if h.owner == owner {
			if xact {
				h.xact++
			} else {
				h.session++
			}
			l.mu.Unlock()
			return true, nil
		}
		if !wait {
			l.mu.Unlock()
			return false, nil
		}
		freed := h.freed
		l.mu.Unlock()
		select {
		case <-freed:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// release drops one session-level hold of k; false when owner holds
// none (Postgres returns false and warns).
func (l *advLocks) release(k advKey, owner *sessState) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.held[k]
	if h == nil || h.owner != owner || h.session == 0 {
		return false
	}
	h.session--
	l.dropIfFree(k, h)
	return true
}

// releaseOwner drops every hold owner has at the chosen levels:
// session-level ones for pg_advisory_unlock_all, transaction-level ones
// at transaction end, both when the session closes. The table holds
// only locks currently taken, so the scan is short.
func (l *advLocks) releaseOwner(owner *sessState, session, xact bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, h := range l.held {
		if h.owner != owner {
			continue
		}
		if session {
			h.session = 0
		}
		if xact {
			h.xact = 0
		}
		l.dropIfFree(k, h)
	}
}

// dropIfFree removes a hold with no remaining holds and wakes its
// waiters. Callers hold l.mu.
func (l *advLocks) dropIfFree(k advKey, h *advHold) {
	if h.session == 0 && h.xact == 0 {
		delete(l.held, k)
		close(h.freed)
	}
}

// endXact releases d's session's transaction-level advisory locks: at
// the end of an autocommit statement (DB.run) and when a block ends.
func (d *DB) endXact() {
	if d.adv != nil && d.ss != nil && d.ss.xactHeld.Load() > 0 {
		d.ss.xactHeld.Store(0)
		d.adv.releaseOwner(d.ss, false, true)
	}
}

// advArgs reads an advisory lock key: one bigint, or two int4 halves.
func advArgs(args []any) (advKey, error) {
	ints := make([]int64, len(args))
	for i, a := range args {
		n, ok := a.(int64)
		if !ok {
			return advKey{}, serr.New("advisory lock keys are integers")
		}
		ints[i] = n
	}
	if len(ints) == 2 {
		for _, n := range ints {
			if n < -1<<31 || n > 1<<31-1 {
				return advKey{}, errIntRange()
			}
		}
		return advKey{a: ints[0], b: ints[1], pair: true}, nil
	}
	return advKey{a: ints[0]}, nil
}

// advisoryFunc builds one pg_advisory_* function. xact picks the
// transaction-level form; try returns whether the lock was taken
// instead of waiting for it.
func advisoryFunc(xact, try bool) func(env *exEnv, args []any) (any, error) {
	return func(env *exEnv, args []any) (any, error) {
		k, err := advArgs(args)
		if err != nil {
			return nil, err
		}
		ctx := env.d.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		got, err := env.d.adv.acquire(ctx, k, env.d.ss, xact, !try)
		if err != nil {
			return nil, err
		}
		if got && xact {
			env.d.ss.xactHeld.Add(1)
		}
		if try {
			return got, nil
		}
		return "", nil // void, which Postgres renders as an empty value
	}
}

// --- session information functions ---

// settingValue answers current_setting(name) as SHOW would: the
// session's SET state over the defaults, or the defaults alone for a
// bare DB.
func (d *DB) settingValue(name string) (string, bool) {
	name = strings.ToLower(name)
	if d.ss != nil && d.ss.ses != nil {
		s := d.ss.ses
		return showValue(name, s.vars, s.timeout, s.liveParams())
	}
	lvl := isoDefault(d.e.ConcurrentWrites())
	return showValue(name, nil, 0, map[string]string{
		"transaction_isolation":         lvl,
		"default_transaction_isolation": lvl,
	})
}

// quoteIdent quotes an identifier when it would not survive unquoted:
// anything but lowercase letters, digits, underscores and dollar signs
// (not leading with a digit or dollar sign), or a reserved word.
func quoteIdent(s string) string {
	plain := s != "" && !reservedWords[s]
	for i := 0; plain && i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c == '_':
		case (c >= '0' && c <= '9') || c == '$':
			plain = i > 0
		default:
			plain = false
		}
	}
	if plain {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// reservedWords are Postgres's reserved keywords (the "reserved" and
// "reserved (can be function or type)" classes): the words quote_ident
// must quote even when they are otherwise plain.
var reservedWords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`all analyse analyze and any array as asc
		asymmetric authorization binary both case cast check collate collation
		column concurrently constraint create cross current_catalog current_date
		current_role current_schema current_time current_timestamp current_user
		default deferrable desc distinct do else end except false fetch for
		foreign freeze from full grant group having ilike in initially inner
		intersect into is isnull join lateral leading left like limit localtime
		localtimestamp natural not notnull null offset on only or order outer
		overlaps placing primary references returning right select session_user
		similar some symmetric system_user table tablesample then to trailing
		true union unique user using variadic verbose when where window with`) {
		m[w] = true
	}
	return m
}()

// serialSequence answers pg_get_serial_sequence(table, column): the
// schema-qualified sequence a serial column's default draws from, or
// NULL for a column without one.
func serialSequence(env *exEnv, table, column string) (any, error) {
	name := strings.TrimPrefix(table, "public.")
	if len(name) > 1 && name[0] == '"' && name[len(name)-1] == '"' {
		name = strings.ReplaceAll(name[1:len(name)-1], `""`, `"`)
	} else {
		name = strings.ToLower(name)
	}
	desc := env.d.e.TableShared(name)
	if desc == nil {
		return nil, serr.New(`relation "` + table + `" does not exist`)
	}
	i := desc.ColIndex(column)
	if i < 0 {
		return nil, serr.New(`column "` + column + `" of relation "` + name + `" does not exist`)
	}
	seq := seqOfDefault(desc.Columns[i].Default)
	if desc.Columns[i].Identity {
		// Serial and identity columns are bytdb identities: their
		// sequence is the implied one setval accepts.
		seq = identitySeqName(desc.Name, desc.Columns[i].Name)
	}
	if seq == "" {
		return nil, nil
	}
	return "public." + quoteIdent(seq), nil
}

// seqOfDefault extracts the sequence name from a nextval('name'...)
// default, "" when the default is something else.
func seqOfDefault(def string) string {
	rest, ok := strings.CutPrefix(strings.TrimSpace(def), "nextval('")
	if !ok {
		return ""
	}
	name, _, ok := strings.Cut(rest, "'")
	if !ok {
		return ""
	}
	name = strings.TrimPrefix(name, "public.")
	if len(name) > 1 && name[0] == '"' && name[len(name)-1] == '"' {
		name = strings.ReplaceAll(name[1:len(name)-1], `""`, `"`)
	}
	return name
}

// goTypeName names a runtime value's type for pg_typeof when the
// typing pass did not fold the call (it normally does: the static type
// is what Postgres reports, and only it can tell a date from a count).
func goTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "unknown"
	case int64:
		return "bigint"
	case float64:
		return "double precision"
	case bool:
		return "boolean"
	case []byte:
		return "bytea"
	}
	return "text"
}

// argKinds is a typing rule for the session functions: kinds[i] names
// argument i's type — "bigint", "bool", or "" for text — and an untyped
// argument (a literal or a placeholder) is cast to it, so a placeholder
// Describes with that type and a text-bound value still arrives as one.
// goose binds pg_try_advisory_lock's key as a bare $1, which used to
// reach the function as a string. A typed argument of another type is
// Postgres's 42883.
func argKinds(ret bytdb.ColType, kinds ...string) func(f *funcCall) (bytdb.ColType, error) {
	want := map[string]bytdb.ColType{"bigint": bytdb.TInt, "bool": bytdb.TBool, "": bytdb.TString}
	return func(f *funcCall) (bytdb.ColType, error) {
		for i, t := range f.types {
			k := ""
			if i < len(kinds) {
				k = kinds[i]
			}
			switch {
			case t == tyNull || t == want[k]:
			case t == tyUnknown && k != "":
				f.castArg(i, k)
			case t == tyUnknown:
			default:
				return "", noFunc(f.name, f.types)
			}
		}
		return ret, nil
	}
}

// registerSessionFuncs adds the D-51 functions to the library; called
// at the end of the library's own init so the registry exists.
func registerSessionFuncs() {
	textArg0 := func(a []any) string { s, _ := a[0].(string); return s }
	alwaysTrue := &scalarFunc{min: 1, max: 3, ret: bytdb.TBool, nullSafe: true,
		envEval: func(env *exEnv, a []any) (any, error) { return true, nil }}
	for name, f := range map[string]*scalarFunc{
		"pg_advisory_lock":          {min: 1, max: 2, ret: bytdb.TString, typ: argKinds(bytdb.TString, "bigint", "bigint"), envEval: advisoryFunc(false, false)},
		"pg_try_advisory_lock":      {min: 1, max: 2, ret: bytdb.TBool, typ: argKinds(bytdb.TBool, "bigint", "bigint"), envEval: advisoryFunc(false, true)},
		"pg_advisory_xact_lock":     {min: 1, max: 2, ret: bytdb.TString, typ: argKinds(bytdb.TString, "bigint", "bigint"), envEval: advisoryFunc(true, false)},
		"pg_try_advisory_xact_lock": {min: 1, max: 2, ret: bytdb.TBool, typ: argKinds(bytdb.TBool, "bigint", "bigint"), envEval: advisoryFunc(true, true)},
		"pg_advisory_unlock": {min: 1, max: 2, ret: bytdb.TBool, typ: argKinds(bytdb.TBool, "bigint", "bigint"), envEval: func(env *exEnv, a []any) (any, error) {
			k, err := advArgs(a)
			if err != nil {
				return nil, err
			}
			return env.d.adv.release(k, env.d.ss), nil
		}},
		"pg_advisory_unlock_all": {min: 0, max: 0, ret: bytdb.TString, envEval: func(env *exEnv, a []any) (any, error) {
			env.d.adv.releaseOwner(env.d.ss, true, false)
			return "", nil
		}},
		"pg_backend_pid": {min: 0, max: 0, ret: bytdb.TInt, envEval: func(env *exEnv, a []any) (any, error) {
			return int64(env.d.ss.pid), nil
		}},
		// current_setting(name [, missing_ok]): an unknown parameter is
		// Postgres's error unless missing_ok, which makes it NULL.
		"current_setting": {min: 1, max: 2, ret: bytdb.TString, nullSafe: true, typ: argKinds(bytdb.TString, "", "bool"), envEval: func(env *exEnv, a []any) (any, error) {
			if a[0] == nil {
				return nil, nil
			}
			name := textArg0(a)
			if v, ok := env.d.settingValue(name); ok {
				return v, nil
			}
			if len(a) == 2 && a[1] == true {
				return nil, nil
			}
			return nil, serr.New(`unrecognized configuration parameter "` + name + `"`)
		}},
		// set_config(name, value, is_local) is SET name = value, with the
		// SET's semantics (statement_timeout takes effect); is_local
		// degrades to session scope, as SET LOCAL does here.
		"set_config": {min: 3, max: 3, ret: bytdb.TString, nullSafe: true, typ: argKinds(bytdb.TString, "", "", "bool"), envEval: func(env *exEnv, a []any) (any, error) {
			if a[0] == nil {
				return nil, serr.New("SET requires parameter name")
			}
			name := strings.ToLower(textArg0(a))
			val, _ := a[1].(string)
			sv := &SetVar{Name: name, Value: val, IsDefault: a[1] == nil, Tag: "SET"}
			if env.d.ss == nil || env.d.ss.ses == nil {
				if name == "statement_timeout" {
					return nil, serr.New("SET statement_timeout requires a Session")
				}
				return val, nil
			}
			if _, err := env.d.ss.ses.setVar(sv); err != nil {
				return nil, err
			}
			v, _ := env.d.settingValue(name)
			return v, nil
		}},
		"current_schemas": {min: 1, max: 1, ret: bytdb.TTextArray, typ: argKinds(bytdb.TTextArray, "bool"), envEval: func(env *exEnv, a []any) (any, error) {
			if a[0] == true {
				return "{pg_catalog,public}", nil
			}
			return "{public}", nil
		}},
		"pg_is_in_recovery": {min: 0, max: 0, ret: bytdb.TBool, envEval: func(env *exEnv, a []any) (any, error) {
			return false, nil
		}},
		// txid_current() / pg_current_xact_id(): stable for the open block,
		// a fresh id per autocommit statement.
		"txid_current": {min: 0, max: 0, ret: bytdb.TInt, envEval: func(env *exEnv, a []any) (any, error) {
			ss := env.d.ss
			if env.d.tx != nil && ss.txidTx == env.d.tx {
				return ss.txid, nil
			}
			id := 1000 + txids.Add(1)
			if env.d.tx != nil {
				ss.txidTx, ss.txid = env.d.tx, id
			}
			return id, nil
		}},
		"to_regclass": {min: 1, max: 1, ret: bytdb.TInt, envEval: func(env *exEnv, a []any) (any, error) {
			v, err := castVal(env, textArg0(a), "regclass")
			if err != nil {
				return nil, nil // no such relation: NULL, not an error
			}
			return v, nil
		}},
		"pg_get_serial_sequence": {min: 2, max: 2, ret: bytdb.TString, envEval: func(env *exEnv, a []any) (any, error) {
			col, _ := a[1].(string)
			return serialSequence(env, textArg0(a), col)
		}},
		// bytdb has no roles: every session may do everything, so the
		// privilege probes tools run before acting all answer true.
		"has_table_privilege":    alwaysTrue,
		"has_schema_privilege":   alwaysTrue,
		"has_database_privilege": alwaysTrue,
		"has_sequence_privilege": alwaysTrue,
		"has_column_privilege":   {min: 2, max: 4, ret: bytdb.TBool, nullSafe: true, envEval: alwaysTrue.envEval},
		"quote_ident": {min: 1, max: 1, ret: bytdb.TString, eval: func(a []any) (any, error) {
			return quoteIdent(textArg0(a)), nil
		}},
		// pg_typeof folds to its argument's static type in the typing
		// pass; this is the fallback for an unfolded call.
		"pg_typeof": {min: 1, max: 1, ret: bytdb.TString, nullSafe: true, eval: func(a []any) (any, error) {
			return goTypeName(a[0]), nil
		}},
	} {
		scalarFuncs[name] = f
	}
	scalarFuncs["pg_current_xact_id"] = scalarFuncs["txid_current"]
}
