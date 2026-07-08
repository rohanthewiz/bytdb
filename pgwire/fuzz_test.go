package pgwire

// Fuzz target for the frontend message layer: arbitrary framed bytes
// must produce protocol errors, never a panic or out-of-bounds read.
//
// The fuzz body drives the same per-message dispatch conn.run uses,
// but *below* run's recover() fence — the fence would swallow panics
// and hide them from the fuzzer. Panics escaping message handling are
// exactly what this target exists to find.

import (
	"bufio"
	"encoding/binary"
	"io"
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/sql"
)

// frame builds one typed frontend frame: type byte, self-inclusive
// int32 length, body.
func frame(typ byte, body []byte) []byte {
	out := make([]byte, 0, len(body)+5)
	out = append(out, typ)
	out = binary.BigEndian.AppendUint32(out, uint32(len(body)+4))
	return append(out, body...)
}

func FuzzMessageParse(f *testing.F) {
	// One engine and SQL frontend shared by every fuzz execution; each
	// execution gets its own session (deferred Close rolls back any
	// transaction hostile input managed to open, releasing the writer
	// lock). A pre-made table with a secondary index gives Parse/Bind/
	// Execute real schema to hit.
	e, err := bytdb.Open(filepath.Join(f.TempDir(), "fuzz.db"))
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { e.Close() })
	db := sql.New(e)
	setup := db.NewSession()
	for _, q := range []string{
		"CREATE TABLE t (id INT PRIMARY KEY, name TEXT, age INT)",
		"CREATE INDEX by_age ON t (age)",
		"INSERT INTO t VALUES (1, 'ada', 36), (2, 'grace', 45)",
	} {
		st, err := db.Prepare(q)
		if err != nil {
			f.Fatal(err)
		}
		if _, err := setup.ExecStmt(st); err != nil {
			f.Fatal(err)
		}
	}
	setup.Close()
	srv := NewServer(db)

	// Seeds: well-formed exchanges, then truncations and hostile
	// variants of each message type's body.
	var q wbuf
	q.cstr("select name from t where age > 40")
	var parse wbuf
	parse.cstr("s1")
	parse.cstr("select name from t where id = $1")
	parse.int16(1)
	parse.int32(20) // int8 OID
	var bind wbuf
	bind.cstr("") // portal
	bind.cstr("s1")
	bind.int16(0)         // no param format codes: all text
	bind.int16(1)         // one param
	bind.int32(1)         // 1 byte
	bind.raw([]byte{'1'}) // "1"
	bind.int16(0)         // no result format codes
	var descS, descP, exec, closeS wbuf
	descS.byte('S')
	descS.cstr("s1")
	descP.byte('P')
	descP.cstr("")
	exec.cstr("")
	exec.int32(0)
	closeS.byte('S')
	closeS.cstr("s1")

	full := frame(msgParse, parse)
	full = append(full, frame(msgDescribe, descS)...)
	full = append(full, frame(msgBind, bind)...)
	full = append(full, frame(msgDescribe, descP)...)
	full = append(full, frame(msgExecute, exec)...)
	full = append(full, frame(msgClose, closeS)...)
	full = append(full, frame(msgSync, nil)...)

	seeds := [][]byte{
		frame(msgQuery, q),
		full,
		frame(msgQuery, []byte("no null terminator")),
		frame(msgQuery, nil),
		frame(msgParse, []byte{0}),                                       // name only, then truncated
		frame(msgBind, []byte{0, 0, 0xFF, 0xFF}),                         // absurd format count
		frame(msgBind, []byte{0, 0, 0, 0, 0, 1, 0xFF, 0xFF, 0xFF, 0xFE}), // value length -2
		frame(msgDescribe, []byte{'X', 0}),                               // bad kind
		frame(msgExecute, nil),                                           // truncated
		frame(msgClose, []byte{'P'}),                                     // kind without name
		frame(msgFlush, nil),
		frame(msgSync, nil),
		frame(msgTerminate, nil),
		frame('z', []byte("unknown type")),
		{msgQuery, 0xFF, 0xFF, 0xFF, 0xFF}, // hostile length, no body
		{msgQuery, 0x00, 0x00, 0x00, 0x00}, // length < 4
		{msgQuery, 0x00, 0x00, 0x00},       // truncated length
		{},
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 {
			t.Skip("cap per-exec work; frame parsing is length-agnostic past this")
		}
		sess := db.NewSession()
		defer sess.Close()
		c := &conn{
			srv:     srv,
			r:       bufio.NewReader(&sliceReader{b: data}),
			w:       bufio.NewWriter(io.Discard),
			sess:    sess,
			stmts:   map[string]*prepared{},
			portals: map[string]*portal{},
		}
		// The same dispatch loop as conn.run, minus startup, deadlines,
		// and the recover fence. Keep the switch in sync with run.
		for {
			typ, body, err := readMessage(c.r)
			if err != nil {
				return // error-or-dispatch; EOF and bad frames land here
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
				return
			default:
				return // run() sends an error and drops the connection
			}
		}
	})
}

// sliceReader is a minimal io.Reader over a byte slice (bytes.Reader
// without the extra interfaces, so bufio can't shortcut around it).
type sliceReader struct{ b []byte }

func (s *sliceReader) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.b)
	s.b = s.b[n:]
	return n, nil
}
