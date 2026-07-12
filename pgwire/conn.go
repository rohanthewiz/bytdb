package pgwire

// conn.go: one client session — the startup handshake, then the
// simple ('Q') and extended (Parse/Bind/Describe/Execute/Sync) query
// protocols, both funneling into the sql package's Prepare/Describe/
// Exec.

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/sql"
	"github.com/rohanthewiz/serr"
)

type conn struct {
	srv    *Server
	nc     net.Conn      // the raw socket (or its TLS wrapper), for read deadlines
	idleTx time.Duration // idle-in-transaction timeout; 0 = disabled
	r      *bufio.Reader
	w      *bufio.Writer
	sess   *sql.Session

	// tlsOn: the stream has been upgraded to TLS. Gates RequireTLS and
	// refuses a second SSLRequest.
	tlsOn bool

	// overLimit: the server was at MaxConns when this connection
	// arrived. The startup handshake still runs far enough to deliver a
	// proper FATAL 53300 (through TLS if the client asked for it), and
	// CancelRequests still work — see Server.MaxConns.
	overLimit bool

	// pid and secret form the connection's BackendKeyData: a client
	// proves the right to cancel this connection's queries by echoing
	// both in a CancelRequest. The secret is crypto-random — a
	// guessable one would let any peer who can reach the port kill
	// other sessions' statements.
	pid    int32
	secret uint32

	// queryCancel aborts the statement currently executing, nil between
	// statements. The connection goroutine arms and disarms it; a
	// CancelRequest arrives on a different goroutine, hence the mutex.
	cancelMu    sync.Mutex
	queryCancel context.CancelFunc

	stmts   map[string]*prepared
	portals map[string]*portal

	// inErr: an extended-protocol message failed; per the protocol,
	// discard everything until the client's Sync.
	inErr bool
}

// newCancelSecret draws the BackendKeyData secret. Failure of the
// system randomness source is unrecoverable enough that Go's own
// crypto/rand panics on it; a zero fallback here would silently issue
// forgeable cancel keys instead.
func newCancelSecret() uint32 {
	var b [4]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic("pgwire: reading random cancel secret: " + err.Error())
	}
	return binary.BigEndian.Uint32(b[:])
}

// armCancel installs a fresh cancellation scope for one statement (or
// one simple-query batch) and returns its context. disarmCancel must
// follow when the work ends.
func (c *conn) armCancel() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	c.cancelMu.Lock()
	c.queryCancel = cancel
	c.cancelMu.Unlock()
	return ctx
}

// disarmCancel retires the current statement's cancellation scope; a
// CancelRequest arriving from here on is a no-op, as in Postgres (the
// protocol makes cancellation advisory and racy by design).
func (c *conn) disarmCancel() {
	c.cancelMu.Lock()
	if c.queryCancel != nil {
		c.queryCancel() // release the context's resources; the query is done
		c.queryCancel = nil
	}
	c.cancelMu.Unlock()
}

// cancelQuery aborts whatever statement the connection is running.
// Called from a CancelRequest connection's goroutine via the server.
func (c *conn) cancelQuery() {
	c.cancelMu.Lock()
	if c.queryCancel != nil {
		c.queryCancel()
	}
	c.cancelMu.Unlock()
}

// prepared is one named (or unnamed) statement from Parse. A nil stmt
// is the empty statement, which Describe answers with NoData and
// Execute with EmptyQueryResponse.
type prepared struct {
	stmt      *sql.Stmt
	info      *sql.StmtInfo
	query     string
	paramOIDs []uint32 // client-declared in Parse; 0 = infer
}

// portal is one named (or unnamed) Bind result: a prepared statement
// with its arguments bound and result formats chosen.
type portal struct {
	prep    *prepared
	args    []any
	formats []int // result-column format codes, Bind semantics
}

// paramOID is parameter i's wire type: the client's declaration when
// it made one, else the OID of the inferred column type.
func (p *prepared) paramOID(i int) uint32 {
	if i < len(p.paramOIDs) && p.paramOIDs[i] != 0 {
		return p.paramOIDs[i]
	}
	return oidForType(p.info.Params[i])
}

// run drives the session; the connection closes when it returns.
//
// A recover() fence turns any panic reachable from parse, plan, or
// execution into the loss of this one connection instead of the whole
// process — without it, one hostile or buggy statement would kill
// every other client's connection and the server itself. The client
// gets a best-effort ErrorResponse (XX000 internal_error), and the
// deferred close in Serve tears down the socket and rolls back any
// open transaction, so the engine's writer lock is never leaked.
func (c *conn) run() (err error) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "pgwire: panic on connection: %v\n%s", r, debug.Stack())
			c.send(msgErrorResponse, errorBody(
				serr.New("internal error", "panic", fmt.Sprint(r)), "", 0))
			c.w.Flush()
			err = serr.New("panic on connection", "panic", fmt.Sprint(r))
		}
	}()
	if err := c.startup(); err != nil {
		if errors.Is(err, errCancelHandled) {
			return nil
		}
		return err
	}
	for {
		c.armIdleDeadline()
		typ, body, err := readMessage(c.r)
		if err != nil {
			if c.idleTxExpired(err) {
				// The client sat inside a transaction block past the
				// timeout. Terminate it as Postgres does (FATAL 25P03);
				// the deferred session close in Serve rolls the block
				// back, releasing the engine's writer lock.
				c.send(msgErrorResponse, fatalBody(
					"terminating connection due to idle-in-transaction timeout",
					"25P03"))
				c.w.Flush()
				return serr.New("idle-in-transaction timeout")
			}
			return err
		}
		if c.inErr && typ != msgSync && typ != msgTerminate {
			continue
		}
		r := &rbuf{b: body}
		switch typ {
		case msgQuery:
			c.simpleQuery(r.cstr())
		case msgParse:
			c.parse(r)
		case msgBind:
			c.bind(r)
		case msgDescribe:
			c.describe(r)
		case msgExecute:
			c.execute(r)
		case msgClose:
			c.closeTarget(r)
		case msgSync:
			c.inErr = false
			c.ready()
		case msgFlush:
			c.w.Flush()
		case msgTerminate:
			return nil
		default:
			c.send(msgErrorResponse, errorBody(
				serr.New("unsupported frontend message", "type", string(typ)), "", 0))
			c.w.Flush()
			return serr.New("unsupported frontend message")
		}
		if err := c.wErr(); err != nil {
			return err
		}
	}
}

// armIdleDeadline sets a read deadline while the session is inside a
// transaction block ('T' or failed 'E' — both hold state; a writable
// block holds the engine's global writer lock) and clears it when the
// session is idle, where a silent client costs nothing.
func (c *conn) armIdleDeadline() {
	if c.nc == nil || c.idleTx <= 0 {
		return
	}
	if c.sess.Status() != sql.TxIdle {
		c.nc.SetReadDeadline(time.Now().Add(c.idleTx))
	} else {
		c.nc.SetReadDeadline(time.Time{})
	}
}

// idleTxExpired reports whether a read error is the armed
// idle-in-transaction deadline firing, as opposed to any other I/O
// failure.
func (c *conn) idleTxExpired(err error) bool {
	var ne net.Error
	return c.idleTx > 0 && c.sess.Status() != sql.TxIdle &&
		errors.As(err, &ne) && ne.Timeout()
}

// errCancelHandled reports a connection that existed only to deliver a
// CancelRequest; run treats it as a clean exit.
var errCancelHandled = errors.New("cancel request handled")

// startupTimeout bounds the whole pre-session phase — startup message,
// TLS handshake, SCRAM exchange. Postgres enforces the same idea as
// authentication_timeout: an unauthenticated peer must not pin a
// socket and goroutine forever by going silent mid-handshake.
const startupTimeout = 30 * time.Second

// startup performs the handshake: upgrade to TLS when configured
// (decline politely when not), dispatch cancel requests to their
// target connection, then authenticate the startup message — SCRAM
// when the server holds credentials, trust otherwise.
func (c *conn) startup() error {
	c.nc.SetReadDeadline(time.Now().Add(startupTimeout))
	for {
		body, err := readStartup(c.r)
		if err != nil {
			return err
		}
		r := &rbuf{b: body}
		switch code := r.u32(); code {
		case codeSSLRequest:
			if err := c.maybeStartTLS(); err != nil {
				return err
			}
		case codeGSSENCRequest:
			// Declined with a bare byte, not a framed message.
			if err := c.w.WriteByte('N'); err != nil {
				return err
			}
			c.w.Flush()
		case codeCancelRequest:
			// The whole connection exists to carry this one message: PID
			// and secret follow the code, and the protocol's answer is
			// silence — the socket just closes, whether or not anything
			// was canceled. Deliberately reachable without auth: the
			// crypto-random secret is itself the credential, and the
			// issuing session may be blocked inside a query, unable to
			// authenticate a second connection's exchange.
			pid := int32(r.u32())
			secret := r.u32()
			if !r.bad {
				c.srv.cancelBackend(pid, secret)
			}
			return errCancelHandled
		case protoVersion3:
			// Parameters (user, database, ...) follow as key/value
			// cstring pairs. The engine has no databases, so only user
			// matters, and only when authenticating.
			params := startupParams(r)
			if c.overLimit {
				// Postgres's exact message and SQLSTATE (53300
				// too_many_connections); pgx and libpq both surface it
				// verbatim.
				c.send(msgErrorResponse, fatalBody(
					"sorry, too many clients already", "53300"))
				c.w.Flush()
				return serr.New("connection limit reached",
					"max", fmt.Sprint(c.srv.MaxConns))
			}
			if c.srv.RequireTLS && !c.tlsOn {
				// What a hostssl-only pg_hba.conf answers, with its
				// SQLSTATE (28000 invalid_authorization_specification).
				c.send(msgErrorResponse, fatalBody(
					"connection requires TLS (send SSLRequest first)", "28000"))
				c.w.Flush()
				return serr.New("plaintext connection refused: TLS required")
			}
			if c.srv.Auth != nil {
				if err := c.authSCRAM(params["user"]); err != nil {
					return err
				}
			}
			var auth wbuf
			auth.int32(authOK) // AuthenticationOk
			c.send(msgAuth, auth)
			for _, kv := range [][2]string{
				{"server_version", sql.ServerVersion},
				{"server_encoding", "UTF8"},
				{"client_encoding", "UTF8"},
				{"DateStyle", "ISO, MDY"},
				{"integer_datetimes", "on"},
				{"standard_conforming_strings", "on"},
			} {
				var b wbuf
				b.cstr(kv[0])
				b.cstr(kv[1])
				c.send(msgParameterStatus, b)
			}
			var key wbuf
			key.int32(int(c.pid))
			key.int32(int(int32(c.secret)))
			c.send(msgBackendKeyData, key)
			c.ready()
			// The session is established; from here the read deadline
			// belongs to armIdleDeadline (which only manages it inside
			// transaction blocks), so the startup one must go.
			c.nc.SetReadDeadline(time.Time{})
			return c.wErr()
		default:
			return serr.New("unsupported protocol version", "version", fmt.Sprint(code))
		}
	}
}

// maybeStartTLS answers one SSLRequest: 'S' and a TLS upgrade when the
// server has a TLS config, a polite 'N' when it does not (or when the
// stream is already TLS — a client bug Postgres also refuses).
func (c *conn) maybeStartTLS() error {
	if c.srv.TLSConfig == nil || c.tlsOn {
		if err := c.w.WriteByte('N'); err != nil {
			return err
		}
		return c.w.Flush()
	}
	// A compliant client sends nothing more until it sees 'S', so any
	// bytes already buffered were pipelined in the clear. Honoring them
	// after the upgrade would let an active attacker splice a plaintext
	// prefix into the "secure" session — drop the connection instead.
	if c.r.Buffered() > 0 {
		return serr.New("protocol data pipelined after SSLRequest")
	}
	if err := c.w.WriteByte('S'); err != nil {
		return err
	}
	if err := c.w.Flush(); err != nil {
		return err
	}
	tc := tls.Server(c.nc, c.srv.TLSConfig)
	// Handshake reads inherit the startup deadline already armed on the
	// underlying socket.
	if err := tc.Handshake(); err != nil {
		return serr.Wrap(err, "TLS handshake")
	}
	// Everything from here — including the startup message the client
	// now retries — flows through the TLS record layer, so the buffered
	// reader and writer must be rebuilt around it.
	c.nc = tc
	c.r = bufio.NewReader(tc)
	c.w = bufio.NewWriter(tc)
	c.tlsOn = true
	return nil
}

// startupParams collects the startup message's key/value cstring
// pairs; the list ends at its empty-key terminator.
func startupParams(r *rbuf) map[string]string {
	params := map[string]string{}
	for {
		k := r.cstr()
		if k == "" || r.bad {
			return params
		}
		params[k] = r.cstr()
	}
}

// testPanicQuery, when set to a non-empty string, makes the matching
// simple query panic. Only tests set it, to exercise run's recover
// fence — there is no known statement that panics, and there should
// never be one. Atomic because connection goroutines read it.
var testPanicQuery atomic.Value // string

// simpleQuery runs a Query buffer: each ;-separated statement in
// order, stopping at the first error. Results are always text format.
func (c *conn) simpleQuery(q string) {
	if s, _ := testPanicQuery.Load().(string); s != "" && strings.TrimSpace(q) == s {
		panic("test-injected panic: " + q)
	}
	parts := splitStatements(q)
	if len(parts) == 0 {
		c.send(msgEmptyQuery, nil)
		c.ready()
		return
	}
	// One cancellation scope spans the whole query buffer, so a
	// CancelRequest arriving between two of its statements still stops
	// the batch (the next statement fails on the canceled context).
	ctx := c.armCancel()
	defer c.disarmCancel()
	for _, p := range parts {
		start := time.Now()
		st, err := c.srv.db.Prepare(p.text)
		if err != nil {
			c.logQuery(p.text, start, err)
			c.sendError(err, q, p.off)
			break
		}
		res, err := c.sess.ExecStmtCtx(ctx, st)
		c.logQuery(p.text, start, err)
		if err != nil {
			c.sendError(err, q, p.off)
			break
		}
		if res.Notice != "" {
			c.sendNotice(res.Notice)
		}
		if len(res.Cols) > 0 {
			c.sendRowDescription(res.Cols, res.Types, nil)
			if !c.sendDataRows(res.Rows, nil, res.Types) {
				break
			}
		}
		c.sendCommandComplete(st.Command(), res)
	}
	c.inErr = false // simple-query errors end at their own ReadyForQuery
	c.ready()
}

// parse handles Parse: name, query, and optional parameter-type
// declarations. The statement is parsed and described eagerly, so
// unknown tables and columns fail here, as in Postgres.
func (c *conn) parse(r *rbuf) {
	name := r.cstr()
	query := r.cstr()
	n := r.u16()
	oids := make([]uint32, n)
	for i := range oids {
		oids[i] = r.u32()
	}
	if r.bad {
		c.protoError("bad Parse message")
		return
	}
	p := &prepared{query: query, paramOIDs: oids}
	if q := strings.TrimRight(strings.TrimSpace(query), "; \t\r\n"); q != "" {
		st, err := c.srv.db.Prepare(q)
		if err != nil {
			c.sendError(err, query, 0)
			return
		}
		info, err := st.Describe()
		if err != nil {
			c.sendError(err, query, 0)
			return
		}
		p.stmt, p.info = st, info
	}
	c.stmts[name] = p
	c.send(msgParseComplete, nil)
}

// bind handles Bind: decode the parameter values against the
// statement's parameter types and remember the result formats.
func (c *conn) bind(r *rbuf) {
	pname := r.cstr()
	sname := r.cstr()
	pformats := make([]int, r.u16())
	for i := range pformats {
		pformats[i] = r.u16()
	}
	vals := make([][]byte, r.u16())
	nulls := make([]bool, len(vals))
	for i := range vals {
		v, ok := r.bytesN()
		vals[i], nulls[i] = v, !ok
	}
	formats := make([]int, r.u16())
	for i := range formats {
		formats[i] = r.u16()
	}
	if r.bad {
		c.protoError("bad Bind message")
		return
	}
	p, ok := c.stmts[sname]
	if !ok {
		c.sendError(serr.New("prepared statement does not exist", "name", sname), "", 0)
		return
	}
	want := 0
	if p.stmt != nil {
		want = p.stmt.NumParams()
	}
	if len(vals) != want {
		c.sendError(serr.New("wrong number of parameters",
			"want", fmt.Sprint(want), "got", fmt.Sprint(len(vals))), "", 0)
		return
	}
	args := make([]any, len(vals))
	for i := range vals {
		if nulls[i] {
			continue // nil arg = SQL NULL
		}
		v, err := decodeParam(vals[i], formatFor(pformats, i), p.paramOID(i))
		if err != nil {
			c.sendError(serr.Wrap(err, "param", fmt.Sprintf("$%d", i+1)), "", 0)
			return
		}
		args[i] = v
	}
	c.portals[pname] = &portal{prep: p, args: args, formats: formats}
	c.send(msgBindComplete, nil)
}

// describe handles Describe for a statement ('S': parameter types
// then row shape) or a portal ('P': row shape in the bound formats).
func (c *conn) describe(r *rbuf) {
	kind := r.byte()
	name := r.cstr()
	switch kind {
	case 'S':
		p, ok := c.stmts[name]
		if !ok {
			c.sendError(serr.New("prepared statement does not exist", "name", name), "", 0)
			return
		}
		var b wbuf
		n := 0
		if p.stmt != nil {
			n = p.stmt.NumParams()
		}
		b.int16(n)
		for i := range n {
			b.int32(int(p.paramOID(i)))
		}
		c.send(msgParamDesc, b)
		if p.info == nil || len(p.info.Cols) == 0 {
			c.send(msgNoData, nil)
			return
		}
		c.sendRowDescription(p.info.Cols, p.info.Types, nil)
	case 'P':
		pt, ok := c.portals[name]
		if !ok {
			c.sendError(serr.New("portal does not exist", "name", name), "", 0)
			return
		}
		if pt.prep.info == nil || len(pt.prep.info.Cols) == 0 {
			c.send(msgNoData, nil)
			return
		}
		c.sendRowDescription(pt.prep.info.Cols, pt.prep.info.Types, pt.formats)
	default:
		c.protoError("bad Describe kind")
	}
}

// execute runs a bound portal. The row-count limit (portal
// suspension) is not supported: every result is delivered whole.
func (c *conn) execute(r *rbuf) {
	name := r.cstr()
	r.u32() // max rows; ignored
	pt, ok := c.portals[name]
	if !ok {
		c.sendError(serr.New("portal does not exist", "name", name), "", 0)
		return
	}
	if pt.prep.stmt == nil {
		c.send(msgEmptyQuery, nil)
		return
	}
	ctx := c.armCancel()
	start := time.Now()
	res, err := c.sess.ExecStmtCtx(ctx, pt.prep.stmt, pt.args...)
	c.logQuery(pt.prep.query, start, err)
	c.disarmCancel()
	if err != nil {
		c.sendError(err, pt.prep.query, 0)
		return
	}
	if res.Notice != "" {
		c.sendNotice(res.Notice)
	}
	if len(res.Cols) > 0 && !c.sendDataRows(res.Rows, pt.formats, res.Types) {
		return
	}
	c.sendCommandComplete(pt.prep.info.Command, res)
}

// logQuery reports one executed statement to the server's QueryLog
// hook, if configured. Execution only: Parse-time failures of the
// extended protocol never reach an Execute and are not logged.
func (c *conn) logQuery(query string, start time.Time, err error) {
	if f := c.srv.QueryLog; f != nil {
		f(query, time.Since(start), err)
	}
}

// closeTarget handles Close for a statement or portal; closing an
// unknown name succeeds, per the protocol.
func (c *conn) closeTarget(r *rbuf) {
	kind := r.byte()
	name := r.cstr()
	switch kind {
	case 'S':
		delete(c.stmts, name)
	case 'P':
		delete(c.portals, name)
	default:
		c.protoError("bad Close kind")
		return
	}
	c.send(msgCloseComplete, nil)
}

// --- backend message helpers ---

// send frames and buffers one backend message.
func (c *conn) send(typ byte, body wbuf) {
	c.w.WriteByte(typ)
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], uint32(len(body)+4))
	c.w.Write(lb[:])
	c.w.Write(body)
}

// ready sends ReadyForQuery with the session's transaction status —
// 'I' idle, 'T' in a transaction block, 'E' in a failed one — and
// flushes.
func (c *conn) ready() {
	c.send(msgReadyForQuery, wbuf{byte(c.sess.Status())})
	c.w.Flush()
}

// sendError sends an ErrorResponse and enters the discard-until-Sync
// state.
func (c *conn) sendError(err error, query string, base int) {
	c.send(msgErrorResponse, errorBody(err, query, base))
	c.inErr = true
}

// sendNotice sends a statement's warning (a redundant BEGIN, a stray
// COMMIT) as a NoticeResponse.
func (c *conn) sendNotice(msg string) {
	c.send(msgNoticeResponse, noticeBody(msg))
}

func (c *conn) protoError(msg string) {
	c.sendError(serr.New(msg), "", 0)
}

func (c *conn) sendRowDescription(cols []string, types []bytdb.ColType, formats []int) {
	var b wbuf
	b.int16(len(cols))
	for i, name := range cols {
		b.cstr(name)
		b.int32(0) // originating table OID: none
		b.int16(0) // originating column: none
		b.int32(int(oidForType(types[i])))
		b.int16(typeSize(types[i]))
		b.int32(-1) // type modifier
		b.int16(formatFor(formats, i))
	}
	c.send(msgRowDescription, b)
}

// sendDataRows sends one DataRow per result row; false on an encoding
// failure (already reported). types are the declared column types —
// the date/time and uuid types share runtime representations with the
// integer and byte kinds, so encoding must be driven by declaration,
// not representation.
func (c *conn) sendDataRows(rows [][]any, formats []int, types []bytdb.ColType) bool {
	for _, row := range rows {
		var b wbuf
		b.int16(len(row))
		for i, v := range row {
			if v == nil {
				b.int32(-1)
				continue
			}
			var t bytdb.ColType
			if i < len(types) {
				t = types[i]
			}
			enc, err := encodeValue(v, formatFor(formats, i), t)
			if err != nil {
				c.sendError(err, "", 0)
				return false
			}
			b.int32(len(enc))
			b.raw(enc)
		}
		c.send(msgDataRow, b)
	}
	return true
}

// sendCommandComplete renders the command tag: rows returned for
// SELECT, rows affected for INSERT/UPDATE/DELETE, the bare command
// for DDL and transaction control. A result may override its
// statement's tag (COMMIT of a failed transaction reports ROLLBACK).
func (c *conn) sendCommandComplete(cmd string, res *sql.Result) {
	if res.Tag != "" {
		cmd = res.Tag
	}
	var b wbuf
	switch cmd {
	case "SELECT":
		b.cstr(fmt.Sprintf("SELECT %d", len(res.Rows)))
	case "INSERT":
		b.cstr(fmt.Sprintf("INSERT 0 %d", res.RowsAffected))
	case "UPDATE", "DELETE":
		b.cstr(fmt.Sprintf("%s %d", cmd, res.RowsAffected))
	default:
		b.cstr(cmd)
	}
	c.send(msgCommandComplete, b)
}

// wErr reports the buffered writer's sticky error, so I/O failures
// surface once per message batch instead of on every write.
func (c *conn) wErr() error {
	_, err := c.w.Write(nil)
	return err
}
