// Package pgwire serves a bytdb database over the PostgreSQL wire
// protocol (version 3.0), so psql, pgx, database/sql, and ORM drivers
// can speak to it as if it were Postgres.
//
// The server is intentionally small, in bytdb's spirit:
//
//   - Both the simple ('Q') and extended (Parse/Bind/Describe/
//     Execute/Sync) query protocols, in text and binary formats for
//     the five column types (bool, int8, float8, text, bytea).
//   - Parse maps onto sql.Prepare and Describe onto sql.Stmt.Describe,
//     so drivers see real parameter types (inferred from the columns
//     each $n touches) and the result shape before execution.
//   - Trust authentication: any user and database name is accepted
//     and ignored. TLS and GSS encryption are declined; clients must
//     connect with sslmode=disable or tolerate the refusal (the
//     default sslmode=prefer does).
//   - Transaction blocks: each connection is a sql.Session, so BEGIN
//     ... COMMIT/ROLLBACK behave as in Postgres — ReadyForQuery
//     reports the real status (idle / in transaction / failed), an
//     error fails the block until ROLLBACK, COMMIT of a failed block
//     reports ROLLBACK, redundant control statements raise
//     NoticeResponse warnings, and a dropped connection rolls back.
//     A writable block holds the engine's single-writer lock, so
//     other connections' writes (not reads) wait behind it.
//   - Errors travel structurally: the SQL layer's serr fields become
//     ErrorResponse fields — a parse position becomes Position
//     (1-based character offset), the rest become Detail — with a
//     SQLSTATE mapped from the error's kind.
//
// Cancellation keys, portal suspension (Execute row limits), COPY,
// NOTIFY, and the pg_catalog schema are not implemented.
package pgwire

import (
	"bufio"
	"net"
	"sync"
	"sync/atomic"

	"github.com/rohanthewiz/bytdb/sql"
)

// Server serves one bytdb SQL frontend to any number of concurrent
// client connections.
type Server struct {
	db *sql.DB

	nextPID atomic.Int32

	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	closed bool
}

// NewServer wraps a SQL frontend for serving.
func NewServer(db *sql.DB) *Server {
	return &Server{db: db, conns: map[net.Conn]struct{}{}}
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
		s.mu.Lock()
		s.conns[nc] = struct{}{}
		s.mu.Unlock()
		go func() {
			defer func() {
				nc.Close()
				s.mu.Lock()
				delete(s.conns, nc)
				s.mu.Unlock()
			}()
			c := &conn{
				srv:     s,
				r:       bufio.NewReader(nc),
				w:       bufio.NewWriter(nc),
				sess:    s.db.NewSession(),
				stmts:   map[string]*prepared{},
				portals: map[string]*portal{},
			}
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
