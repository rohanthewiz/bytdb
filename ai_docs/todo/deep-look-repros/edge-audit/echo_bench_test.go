package edgeaudit

import (
	"io"
	"net"
	"testing"
	"time"
)

// Floor for any request/response protocol on this machine's loopback:
// one ~60-byte write and one ~60-byte read per side, nothing else.
// withDeadlines adds the 3 SetRead/WriteDeadline calls per frontend
// message that pgwire's run loop makes (x3 messages: Bind, Execute, Sync).
func benchEcho(b *testing.B, withDeadlines bool) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 64)
		for {
			if withDeadlines {
				for range 3 {
					nc.SetReadDeadline(time.Time{})
					nc.SetReadDeadline(time.Now().Add(time.Minute))
					nc.SetWriteDeadline(time.Now().Add(time.Minute))
				}
			}
			if _, err := io.ReadFull(nc, buf); err != nil {
				return
			}
			nc.Write(buf)
		}
	}()
	c, _ := net.Dial("tcp", ln.Addr().String())
	defer c.Close()
	buf := make([]byte, 64)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Write(buf)
		io.ReadFull(c, buf)
	}
}

func BenchmarkLoopbackEcho(b *testing.B)              { benchEcho(b, false) }
func BenchmarkLoopbackEchoWithDeadlines(b *testing.B) { benchEcho(b, true) }
