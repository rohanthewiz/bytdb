package pgwire

// bind_formats_test.go: regression for the Bind format-code count
// checks. The protocol allows 0, 1, or exactly-one-per-item format
// codes; other counts used to index formatFor out of range — a panic
// (killed connection, XX000) any client could trigger at will. They
// must instead produce an ordinary protocol error on a connection that
// keeps working.

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

// frame appends one typed message (type byte + self-inclusive length).
func frameMsg(typ byte, body []byte) []byte {
	out := []byte{typ}
	out = binary.BigEndian.AppendUint32(out, uint32(len(body)+4))
	return append(out, body...)
}

// collectUntilReady reads backend messages through ReadyForQuery,
// returning the set of message types seen.
func collectUntilReady(t *testing.T, r *bufio.Reader) map[byte]int {
	t.Helper()
	seen := map[byte]int{}
	for {
		typ, err := r.ReadByte()
		if err != nil {
			t.Fatalf("read frame type: %v (connection died?)", err)
		}
		var lb [4]byte
		if _, err := io.ReadFull(r, lb[:]); err != nil {
			t.Fatalf("read frame length: %v", err)
		}
		n := int(binary.BigEndian.Uint32(lb[:])) - 4
		if _, err := io.ReadFull(r, make([]byte, n)); err != nil {
			t.Fatalf("read frame body: %v", err)
		}
		seen[typ]++
		if typ == msgReadyForQuery {
			return seen
		}
	}
}

func TestBindFormatCountMismatchIsProtocolError(t *testing.T) {
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(bsql.New(e))
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		e.Close()
	})

	nc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	// Startup: protocol 3.0, user + database, terminator.
	var body []byte
	body = binary.BigEndian.AppendUint32(body, protoVersion3)
	for _, kv := range [][2]string{{"user", "test"}, {"database", "test"}} {
		body = append(append(body, kv[0]...), 0)
		body = append(append(body, kv[1]...), 0)
	}
	body = append(body, 0)
	pkt := binary.BigEndian.AppendUint32(nil, uint32(len(body)+4))
	pkt = append(pkt, body...)
	if _, err := nc.Write(pkt); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(nc)
	collectUntilReady(t, r)

	// Case 1: 2 parameter format codes for 3 parameters.
	var parse []byte
	parse = append(parse, 0) // unnamed statement
	parse = append(parse, "select $1::int, $2::int, $3::int"...)
	parse = append(parse, 0)
	parse = binary.BigEndian.AppendUint16(parse, 0) // no param type hints

	var bind []byte
	bind = append(bind, 0, 0)                     // unnamed portal, unnamed statement
	bind = binary.BigEndian.AppendUint16(bind, 2) // TWO param format codes...
	bind = binary.BigEndian.AppendUint16(bind, 0) // text
	bind = binary.BigEndian.AppendUint16(bind, 0) // text
	bind = binary.BigEndian.AppendUint16(bind, 3) // ...for THREE parameters
	for range 3 {
		bind = binary.BigEndian.AppendUint32(bind, 1)
		bind = append(bind, '7')
	}
	bind = binary.BigEndian.AppendUint16(bind, 0) // no result formats

	msg := frameMsg(msgParse, parse)
	msg = append(msg, frameMsg(msgBind, bind)...)
	msg = append(msg, frameMsg(msgSync, nil)...)
	if _, err := nc.Write(msg); err != nil {
		t.Fatal(err)
	}
	if seen := collectUntilReady(t, r); seen[msgErrorResponse] == 0 {
		t.Fatalf("param-format mismatch produced no ErrorResponse (saw %v)", seen)
	}

	// Case 2: 2 result format codes for a 3-column result.
	parse = parse[:0]
	parse = append(parse, 0)
	parse = append(parse, "select 1, 2, 3"...)
	parse = append(parse, 0)
	parse = binary.BigEndian.AppendUint16(parse, 0)

	bind = bind[:0]
	bind = append(bind, 0, 0)
	bind = binary.BigEndian.AppendUint16(bind, 0) // no param formats
	bind = binary.BigEndian.AppendUint16(bind, 0) // no params
	bind = binary.BigEndian.AppendUint16(bind, 2) // TWO result format codes for THREE columns
	bind = binary.BigEndian.AppendUint16(bind, 0)
	bind = binary.BigEndian.AppendUint16(bind, 0)

	var execB []byte
	execB = append(execB, 0) // unnamed portal
	execB = binary.BigEndian.AppendUint32(execB, 0)

	msg = frameMsg(msgParse, parse)
	msg = append(msg, frameMsg(msgBind, bind)...)
	msg = append(msg, frameMsg(msgExecute, execB)...)
	msg = append(msg, frameMsg(msgSync, nil)...)
	if _, err := nc.Write(msg); err != nil {
		t.Fatal(err)
	}
	if seen := collectUntilReady(t, r); seen[msgErrorResponse] == 0 {
		t.Fatalf("result-format mismatch produced no ErrorResponse (saw %v)", seen)
	}

	// The connection must still work — a panic would have killed it.
	q := append([]byte("select 42"), 0)
	if _, err := nc.Write(frameMsg(msgQuery, q)); err != nil {
		t.Fatal(err)
	}
	if seen := collectUntilReady(t, r); seen[msgDataRow] != 1 || seen[msgErrorResponse] != 0 {
		t.Fatalf("connection unhealthy after format-code errors (saw %v)", seen)
	}
}
