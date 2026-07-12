// Package pgwire serves a bytdb database over the PostgreSQL wire
// protocol (version 3.0), so psql, pgx, database/sql, and ORM drivers
// can speak to it as if it were Postgres.
//
// The server is intentionally small, in bytdb's spirit:
//
//   - Both the simple ('Q') and extended (Parse/Bind/Describe/
//     Execute/Sync) query protocols, in text and binary formats for
//     the five column types (bool, int8, float8, text, bytea).
//
//   - Parse maps onto sql.Prepare and Describe onto sql.Stmt.Describe,
//     so drivers see real parameter types (inferred from the columns
//     each $n touches) and the result shape before execution.
//
//   - Authentication and transport: trust by default (any user and
//     database name is accepted and ignored). Setting Server.Auth
//     enables SCRAM-SHA-256 (RFC 7677) — the server stores only
//     Postgres-format verifiers, never passwords, and unknown users
//     get a mock exchange so they are indistinguishable from wrong
//     passwords. Setting Server.TLSConfig makes the server accept
//     SSLRequest and upgrade the stream to TLS; RequireTLS then
//     refuses plaintext clients outright. On TLS with a single static
//     certificate the server also offers SCRAM-SHA-256-PLUS
//     (tls-server-end-point channel binding, RFC 5929), so
//     channel_binding=require clients are satisfied and binding
//     downgrades fail the exchange. GSS encryption is declined.
//
//   - Transaction blocks: each connection is a sql.Session, so BEGIN
//     ... COMMIT/ROLLBACK behave as in Postgres — ReadyForQuery
//     reports the real status (idle / in transaction / failed), an
//     error fails the block until ROLLBACK, COMMIT of a failed block
//     reports ROLLBACK, redundant control statements raise
//     NoticeResponse warnings, and a dropped connection rolls back.
//     A writable block holds the engine's single-writer lock, so
//     other connections' writes (not reads) wait behind it.
//     SAVEPOINT / RELEASE / ROLLBACK TO work too — pgx's nested
//     transactions ride on them — with ROLLBACK TO recovering a
//     failed block ('E' back to 'T').
//
//   - Errors travel structurally: the SQL layer's serr fields become
//     ErrorResponse fields — a parse position becomes Position
//     (1-based character offset), the rest become Detail — with a
//     SQLSTATE mapped from the error's kind.
//
//   - Out-of-band query cancellation: BackendKeyData carries a
//     crypto-random secret, CancelRequest (verified by PID + secret)
//     aborts the target connection's running statement with SQLSTATE
//     57014, and SET statement_timeout bounds every statement the
//     session runs. Only the statement dies; the connection lives on.
//
// Portal suspension (Execute row limits), COPY, and NOTIFY are not
// implemented.
package pgwire

import (
	"bufio"
	"crypto/tls"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rohanthewiz/bytdb/sql"
)

// DefaultIdleTxTimeout is the idle-in-transaction timeout applied when
// Server.IdleTxTimeout is zero. Postgres ships with this protection
// off, but bytdb cannot afford that default: a writable transaction
// block holds the engine's single global writer lock, so one client
// that sends BEGIN and then goes silent stalls every other
// connection's writes server-wide, indefinitely.
const DefaultIdleTxTimeout = 5 * time.Minute

// Server serves one bytdb SQL frontend to any number of concurrent
// client connections.
type Server struct {
	db *sql.DB

	// IdleTxTimeout bounds how long a connection may sit idle inside a
	// transaction block before the server terminates it (rolling the
	// block back, which releases the engine's writer lock). Zero means
	// DefaultIdleTxTimeout; negative disables the timeout. Set before
	// Serve.
	IdleTxTimeout time.Duration

	// TLSConfig, when non-nil, makes the server accept the protocol's
	// SSLRequest and upgrade the connection to TLS (it needs at least
	// one certificate). Nil declines SSLRequest with 'N', as before.
	// Set before Serve.
	TLSConfig *tls.Config

	// RequireTLS refuses startup on connections that have not upgraded
	// to TLS, the way a hostssl-only pg_hba.conf does. Meaningless
	// without TLSConfig — every client would be refused. Set before
	// Serve.
	RequireTLS bool

	// Auth, when non-nil, requires clients to prove a password via
	// SCRAM-SHA-256 before the session starts; nil keeps trust
	// authentication. The registry may be updated while serving. Set
	// before Serve.
	Auth *Credentials

	// MaxConns caps concurrent client connections (0 = unlimited).
	// Connections over the cap are accepted just long enough to be
	// refused properly — FATAL 53300 "sorry, too many clients already",
	// which drivers understand — rather than left hanging in a TCP
	// backlog. CancelRequest connections are exempt: they carry one
	// message and close, and refusing them would make an overloaded
	// server impossible to relieve. Set before Serve.
	MaxConns int

	// QueryLog, when non-nil, is called after every executed statement
	// with its SQL text, wall-clock duration, and error (nil on
	// success). Called from connection goroutines concurrently, so
	// implementations must be safe for concurrent use; keep them fast —
	// the statement's ReadyForQuery waits on the call. Set before Serve.
	QueryLog func(query string, d time.Duration, err error)

	// bindOnce/bindData cache the RFC 5929 tls-server-end-point
	// channel-binding value (a hash of the server certificate) that
	// SCRAM-SHA-256-PLUS signs into the authentication exchange. Derived
	// lazily from TLSConfig on the first TLS+SCRAM login; nil means the
	// PLUS mechanism is not advertised (no TLS, ambiguous certificate
	// selection, or a signature algorithm RFC 5929 leaves undefined).
	bindOnce sync.Once
	bindData []byte

	nextPID atomic.Int32

	mu       sync.Mutex
	ln       net.Listener
	conns    map[net.Conn]struct{}
	backends map[int32]*conn // BackendKeyData PID → live connection, for CancelRequest
	closed   bool
}

// cancelBackend handles an out-of-band CancelRequest: find the target
// connection by PID, verify the shared secret, and cancel whatever
// statement it is running. A miss is silent — the protocol sends no
// response either way, precisely so cancellation cannot be used to
// probe for live PIDs.
func (s *Server) cancelBackend(pid int32, secret uint32) {
	s.mu.Lock()
	c := s.backends[pid]
	s.mu.Unlock()
	if c != nil && c.secret == secret {
		c.cancelQuery()
	}
}

// idleTxTimeout resolves the configured timeout to an effective one.
func (s *Server) idleTxTimeout() time.Duration {
	switch {
	case s.IdleTxTimeout < 0:
		return 0 // disabled
	case s.IdleTxTimeout == 0:
		return DefaultIdleTxTimeout
	}
	return s.IdleTxTimeout
}

// NewServer wraps a SQL frontend for serving.
func NewServer(db *sql.DB) *Server {
	s := &Server{db: db, conns: map[net.Conn]struct{}{}, backends: map[int32]*conn{}}
	// pg_stat_activity reads live backends off this server's registry.
	// Installed here, before any session copies the DB.
	db.SetActivityProvider(s.activity)
	return s
}

// activity snapshots every live backend for pg_stat_activity, sorted
// by PID so the view reads stably.
func (s *Server) activity() []sql.Activity {
	s.mu.Lock()
	conns := make([]*conn, 0, len(s.backends))
	for _, c := range s.backends {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	out := make([]sql.Activity, 0, len(conns))
	for _, c := range conns {
		a := c.statSnapshot()
		if a.PID == 0 {
			continue // still in startup; not yet a session
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// ListenAndServe listens on addr (e.g. "127.0.0.1:5432") and serves
// until Close.
func (s *Server) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Serve accepts connections on ln until Close, one goroutine per
// connection. After Close it returns nil; any other accept error is
// returned.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		return nil
	}
	s.ln = ln
	s.mu.Unlock()
	for {
		nc, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		c := &conn{
			srv:     s,
			nc:      nc,
			idleTx:  s.idleTxTimeout(),
			r:       bufio.NewReader(nc),
			w:       bufio.NewWriter(nc),
			sess:    s.db.NewSession(),
			stmts:   map[string]*prepared{},
			portals: map[string]*portal{},
			pid:     s.nextPID.Add(1),
			secret:  newCancelSecret(),
		}
		s.mu.Lock()
		// The over-limit decision is made under the same lock that
		// registers the connection, so racing accepts cannot both squeak
		// under the cap.
		c.overLimit = s.MaxConns > 0 && len(s.conns) >= s.MaxConns
		s.conns[nc] = struct{}{}
		s.backends[c.pid] = c
		s.mu.Unlock()
		go func() {
			defer func() {
				nc.Close()
				s.mu.Lock()
				delete(s.conns, nc)
				delete(s.backends, c.pid)
				s.mu.Unlock()
			}()
			defer c.sess.Close() // a dropped connection rolls back
			c.run()
		}()
	}
}

// Close stops accepting and closes every open connection. The
// underlying Engine is the caller's to close.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	var err error
	if s.ln != nil {
		err = s.ln.Close()
	}
	for nc := range s.conns {
		nc.Close()
	}
	return err
}
