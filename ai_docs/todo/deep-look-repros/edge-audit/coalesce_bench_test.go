package edgeaudit

import (
	"net"
	"testing"

	"github.com/rohanthewiz/bytdb/pgwire"
)

// coalesceLn wraps accepted conns so the server's 4 KiB bufio flushes are
// re-buffered to 64 KiB before reaching the socket — an out-of-tree way
// to measure what a larger bufio.Writer in pgwire would buy. A write
// shorter than 4096 bytes is bufio's explicit Flush (end of batch), so it
// drains immediately.
type coalesceLn struct{ net.Listener }
type coalesceConn struct {
	net.Conn
	buf []byte
}

func (l coalesceLn) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &coalesceConn{Conn: c, buf: make([]byte, 0, 64<<10)}, nil
}
func (c *coalesceConn) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	if len(p) < 4096 || len(c.buf)+4096 > cap(c.buf) {
		_, err := c.Conn.Write(c.buf)
		c.buf = c.buf[:0]
		if err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func benchScanVariant(b *testing.B, coalesce bool, noDeadline bool) {
	_, db := benchEngine(b)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := pgwire.NewServer(db)
	if noDeadline {
		srv.WriteTimeout, srv.ReadTimeout, srv.IdleTxTimeout = -1, -1, -1
	}
	var l net.Listener = ln
	if coalesce {
		l = coalesceLn{ln}
	}
	go srv.Serve(l)
	b.Cleanup(func() { srv.Close() })
	c := rawConnect(b, ln.Addr().String())
	c.msg('P', append([]byte("s1\x00select id, name, n, f, ts from kv\x00"), 0, 0))
	c.msg('S', nil)
	c.flush()
	c.drain(b)
	bind := []byte("\x00s1\x00")
	bind = append(bind, 0, 0, 0, 0, 0, 1, 0, 1)
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

func BenchmarkScanBin_Baseline(b *testing.B)      { benchScanVariant(b, false, false) }
func BenchmarkScanBin_NoDeadlines(b *testing.B)   { benchScanVariant(b, false, true) }
func BenchmarkScanBin_Coalesce64KiB(b *testing.B) { benchScanVariant(b, true, false) }
