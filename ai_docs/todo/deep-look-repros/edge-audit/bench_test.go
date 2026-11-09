package edgeaudit

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/pgwire"
	bsql "github.com/rohanthewiz/bytdb/sql"
	"github.com/rohanthewiz/bytdb/stdlib"
)

const benchRows = 100000

func benchEngine(b *testing.B) (*bytdb.Engine, *bsql.DB) {
	b.Helper()
	e, err := bytdb.Open(filepath.Join(b.TempDir(), "b.db"), bytdb.WithSyncNever())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { e.Close() })
	db := bsql.New(e)
	s := db.NewSession()
	if _, err := s.Exec(`create table kv (id int primary key, name text, n int, f float, ts timestamp)`); err != nil {
		b.Fatal(err)
	}
	s.Exec("begin")
	ins, _ := db.Prepare(`insert into kv values ($1, $2, $3, $4, $5)`)
	for i := 0; i < benchRows; i++ {
		if _, err := s.ExecStmt(ins, int64(i), "name-of-row", int64(i*7), float64(i)*1.5, int64(1700000000000000+i)); err != nil {
			b.Fatal(err)
		}
	}
	s.Exec("commit")
	return e, db
}

func serve(b *testing.B, db *bsql.DB) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	srv := pgwire.NewServer(db)
	go srv.Serve(ln)
	b.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

// --- point read: session vs stdlib vs wire -------------------------------

func BenchmarkPointRead_Session(b *testing.B) {
	_, db := benchEngine(b)
	s := db.NewSession()
	st, _ := db.Prepare(`select name, n from kv where id = $1`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.ExecStmt(st, int64(i%benchRows)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPointRead_SessionUnprepared(b *testing.B) {
	_, db := benchEngine(b)
	s := db.NewSession()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Exec(`select name, n from kv where id = $1`, int64(i%benchRows)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPointRead_StdlibPrepared(b *testing.B) {
	e, _ := benchEngine(b)
	db := stdlib.OpenEngine(e)
	defer db.Close()
	st, err := db.Prepare(`select name, n from kv where id = $1`)
	if err != nil {
		b.Fatal(err)
	}
	var name string
	var n int64
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := st.QueryRow(int64(i % benchRows)).Scan(&name, &n); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPointRead_StdlibUnprepared(b *testing.B) {
	e, _ := benchEngine(b)
	db := stdlib.OpenEngine(e)
	defer db.Close()
	var name string
	var n int64
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.QueryRow(`select name, n from kv where id = $1`, int64(i%benchRows)).Scan(&name, &n); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPointRead_WirePgx(b *testing.B) {
	_, db := benchEngine(b)
	addr := serve(b, db)
	c := pgxConn(b, addr)
	ctx := context.Background()
	var name string
	var n int64
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.QueryRow(ctx, `select name, n from kv where id = $1`, int64(i%benchRows)).Scan(&name, &n); err != nil {
			b.Fatal(err)
		}
	}
}

// rawClient is a minimal wire client that allocates nothing per query,
// so -benchmem and the CPU profile show the server side.
type rawClient struct {
	nc  net.Conn
	r   *bufio.Reader
	buf []byte
}

func rawConnect(b testing.TB, addr string) *rawClient {
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatal(err)
	}
	return rawFrom(b, nc)
}

func rawFrom(b testing.TB, nc net.Conn) *rawClient {
	c := &rawClient{nc: nc, r: bufio.NewReaderSize(nc, 1<<16)}
	body := []byte{0, 3, 0, 0}
	body = append(body, "user\x00bench\x00\x00"...)
	msg := binary.BigEndian.AppendUint32(nil, uint32(len(body)+4))
	nc.Write(append(msg, body...))
	c.drain(b)
	return c
}

// drain discards backend messages through ReadyForQuery, returning the
// number of DataRows seen.
func (c *rawClient) drain(b testing.TB) (rows int) {
	var hdr [5]byte
	for {
		if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
			b.Fatal(err)
		}
		n := int(binary.BigEndian.Uint32(hdr[1:])) - 4
		if hdr[0] == 'E' {
			body := make([]byte, n)
			io.ReadFull(c.r, body)
			b.Fatalf("server error: %q", body)
		}
		if _, err := c.r.Discard(n); err != nil {
			b.Fatal(err)
		}
		switch hdr[0] {
		case 'D':
			rows++
		case 'Z':
			return rows
		}
	}
}

func (c *rawClient) msg(typ byte, body []byte) {
	c.buf = append(c.buf, typ)
	c.buf = binary.BigEndian.AppendUint32(c.buf, uint32(len(body)+4))
	c.buf = append(c.buf, body...)
}
func (c *rawClient) flush() { c.nc.Write(c.buf); c.buf = c.buf[:0] }

// One Bind/Execute/Sync round trip against a statement parsed once: the
// cheapest thing a driver can do per query.
func BenchmarkPointRead_WireRaw(b *testing.B) {
	_, db := benchEngine(b)
	benchRawPoint(b, rawConnect(b, serve(b, db)))
}

func benchRawPoint(b *testing.B, c *rawClient) {
	c.msg('P', append([]byte("s1\x00select name, n from kv where id = $1\x00"), 0, 0))
	c.msg('S', nil)
	c.flush()
	c.drain(b)
	bind := []byte("\x00s1\x00")
	bind = append(bind, 0, 1, 0, 1) // one param format: binary
	bind = append(bind, 0, 1, 0, 0, 0, 8)
	valOff := len(bind)
	bind = append(bind, 0, 0, 0, 0, 0, 0, 0, 0)
	bind = append(bind, 0, 1, 0, 1) // results binary
	exec := []byte("\x00\x00\x00\x00\x00")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		binary.BigEndian.PutUint64(bind[valOff:], uint64(i%benchRows))
		c.msg('B', bind)
		c.msg('E', exec)
		c.msg('S', nil)
		c.flush()
		if c.drain(b) != 1 {
			b.Fatal("want 1 row")
		}
	}
}

// --- bulk result: per-DataRow cost ---------------------------------------

func BenchmarkScan100k_Session(b *testing.B) {
	_, db := benchEngine(b)
	s := db.NewSession()
	st, _ := db.Prepare(`select id, name, n, f, ts from kv`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := s.ExecStmt(st)
		if err != nil || len(res.Rows) != benchRows {
			b.Fatal(err)
		}
	}
}

func benchScanWire(b *testing.B, resultFmt byte) {
	_, db := benchEngine(b)
	c := rawConnect(b, serve(b, db))
	c.msg('P', append([]byte("s1\x00select id, name, n, f, ts from kv\x00"), 0, 0))
	c.msg('S', nil)
	c.flush()
	c.drain(b)
	bind := []byte("\x00s1\x00")
	bind = append(bind, 0, 0, 0, 0, 0, 1, 0, resultFmt)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.msg('B', bind)
		c.msg('E', []byte("\x00\x00\x00\x00\x00"))
		c.msg('S', nil)
		c.flush()
		if c.drain(b) != benchRows {
			b.Fatal("row count")
		}
	}
}

func BenchmarkScan100k_WireRawText(b *testing.B)   { benchScanWire(b, 0) }
func BenchmarkScan100k_WireRawBinary(b *testing.B) { benchScanWire(b, 1) }

func BenchmarkScan100k_Stdlib(b *testing.B) {
	e, _ := benchEngine(b)
	db := stdlib.OpenEngine(e)
	defer db.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.Query(`select id, name, n, f, ts from kv`)
		if err != nil {
			b.Fatal(err)
		}
		var raw [5]sql.RawBytes
		n := 0
		for rows.Next() {
			if err := rows.Scan(&raw[0], &raw[1], &raw[2], &raw[3], &raw[4]); err != nil {
				b.Fatal(err)
			}
			n++
		}
		rows.Close()
		if n != benchRows {
			b.Fatal(n)
		}
	}
}
