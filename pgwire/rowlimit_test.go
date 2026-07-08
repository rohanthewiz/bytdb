package pgwire

// Row-limited Execute (portal suspension) is documented as
// unsupported: the Execute message's max-rows field is ignored and
// every result is delivered whole, with CommandComplete and never
// PortalSuspended ('s'). No driver we test with sends a row limit, so
// this pins the behavior with raw frames through the same dispatch
// the fuzz harness drives.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/sql"
)

func TestExecuteRowLimitIgnored(t *testing.T) {
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	db := sql.New(e)
	sess := db.NewSession()
	defer sess.Close()
	for _, q := range []string{
		"CREATE TABLE t (id INT PRIMARY KEY)",
		"INSERT INTO t VALUES (1), (2), (3), (4), (5)",
	} {
		st, err := db.Prepare(q)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sess.ExecStmt(st); err != nil {
			t.Fatal(err)
		}
	}

	// Parse/Bind/Execute(max rows = 2)/Sync as raw frames.
	var parse, bind, exec wbuf
	parse.cstr("")
	parse.cstr("select id from t order by id")
	parse.int16(0)
	bind.cstr("")
	bind.cstr("")
	bind.int16(0)
	bind.int16(0)
	bind.int16(0)
	exec.cstr("")
	exec.int32(2) // the row limit a suspending server would honor

	stream := frame(msgParse, parse)
	stream = append(stream, frame(msgBind, bind)...)
	stream = append(stream, frame(msgExecute, exec)...)
	stream = append(stream, frame(msgSync, nil)...)
	stream = append(stream, frame(msgTerminate, nil)...)

	var out bytes.Buffer
	c := &conn{
		srv:     NewServer(db),
		r:       bufio.NewReader(bytes.NewReader(stream)),
		w:       bufio.NewWriter(&out),
		sess:    db.NewSession(),
		stmts:   map[string]*prepared{},
		portals: map[string]*portal{},
	}
	defer c.sess.Close()
	for {
		typ, body, err := readMessage(c.r)
		if err != nil || typ == msgTerminate {
			break
		}
		r := &rbuf{b: body}
		switch typ {
		case msgParse:
			c.parse(r)
		case msgBind:
			c.bind(r)
		case msgExecute:
			c.execute(r)
		case msgSync:
			c.ready()
		}
	}
	c.w.Flush()

	// Walk the backend frames: all 5 rows arrive despite max rows = 2,
	// completion is CommandComplete, and PortalSuspended never appears.
	var dataRows int
	var sawComplete, sawSuspended bool
	b := out.Bytes()
	for len(b) >= 5 {
		typ := b[0]
		n := int(binary.BigEndian.Uint32(b[1:5])) - 4
		if n < 0 || 5+n > len(b) {
			t.Fatalf("malformed backend frame %q at tail %d", typ, len(b))
		}
		switch typ {
		case msgDataRow:
			dataRows++
		case msgCommandComplete:
			sawComplete = true
		case 's': // PortalSuspended
			sawSuspended = true
		}
		b = b[5+n:]
	}
	if dataRows != 5 || !sawComplete || sawSuspended {
		t.Fatalf("rows=%d complete=%v suspended=%v; want 5/true/false", dataRows, sawComplete, sawSuspended)
	}
}
