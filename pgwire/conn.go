package pgwire

// conn.go: one client session — the startup handshake, then the
// simple ('Q') and extended (Parse/Bind/Describe/Execute/Sync) query
// protocols, both funneling into the sql package's Prepare/Describe/
// Exec.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/sql"
	"github.com/rohanthewiz/serr"
)

type conn struct {
	srv    *Server
	nc     net.Conn      // the raw socket, for read deadlines
	idleTx time.Duration // idle-in-transaction timeout; 0 = disabled
	r      *bufio.Reader
	w      *bufio.Writer
	sess   *sql.Session

	stmts   map[string]*prepared
	portals map[string]*portal

	// inErr: an extended-protocol message failed; per the protocol,
	// discard everything until the client's Sync.
	inErr bool
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

// startup performs the handshake: refuse SSL and GSS encryption
// politely, ignore cancel requests (out-of-band cancellation is not
// supported), then trust-authenticate any startup message.
func (c *conn) startup() error {
	for {
		body, err := readStartup(c.r)
		if err != nil {
			return err
		}
		r := &rbuf{b: body}
		switch code := r.u32(); code {
		case codeSSLRequest, codeGSSENCRequest:
			// A bare byte, not a framed message.
			if err := c.w.WriteByte('N'); err != nil {
				return err
			}
			c.w.Flush()
		case codeCancelRequest:
			return serr.New("cancel request")
		case protoVersion3:
			// Parameters (user, database, ...) follow as key/value
			// cstring pairs; the engine has no users or databases, so
			// they are accepted and ignored.
			var auth wbuf
			auth.int32(0) // AuthenticationOk
			c.send(msgAuth, auth)
			for _, kv := range [][2]string{
				{"server_version", "16.0 (bytdb)"},
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
			key.int32(int(c.srv.nextPID.Add(1)))
			key.int32(0)
			c.send(msgBackendKeyData, key)
			c.ready()
			return c.wErr()
		default:
			return serr.New("unsupported protocol version", "version", fmt.Sprint(code))
		}
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
	for _, p := range parts {
		st, err := c.srv.db.Prepare(p.text)
		if err != nil {
			c.sendError(err, q, p.off)
			break
		}
		res, err := c.sess.ExecStmt(st)
		if err != nil {
			c.sendError(err, q, p.off)
			break
		}
		if res.Notice != "" {
			c.sendNotice(res.Notice)
		}
		if len(res.Cols) > 0 {
			c.sendRowDescription(res.Cols, res.Types, nil)
			if !c.sendDataRows(res.Rows, nil) {
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
	res, err := c.sess.ExecStmt(pt.prep.stmt, pt.args...)
	if err != nil {
		c.sendError(err, pt.prep.query, 0)
		return
	}
	if res.Notice != "" {
		c.sendNotice(res.Notice)
	}
	if len(res.Cols) > 0 && !c.sendDataRows(res.Rows, pt.formats) {
		return
	}
	c.sendCommandComplete(pt.prep.info.Command, res)
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
// failure (already reported).
func (c *conn) sendDataRows(rows [][]any, formats []int) bool {
	for _, row := range rows {
		var b wbuf
		b.int16(len(row))
		for i, v := range row {
			if v == nil {
				b.int32(-1)
				continue
			}
			enc, err := encodeValue(v, formatFor(formats, i))
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
