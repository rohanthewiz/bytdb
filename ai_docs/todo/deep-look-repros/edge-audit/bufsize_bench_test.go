package edgeaudit

import (
	"bufio"
	"io"
	"net"
	"testing"
)

// Cost of pushing a 100k-row result (~6.9 MB of 69-byte DataRows) through
// a bufio.Writer of the given size to a draining loopback peer: isolates
// the flush-syscall component of pgwire's sendDataRows.
func benchBuf(b *testing.B, size int) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		io.Copy(io.Discard, nc)
	}()
	c, _ := net.Dial("tcp", ln.Addr().String())
	defer c.Close()
	w := bufio.NewWriterSize(c, size)
	row := make([]byte, 69)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for r := 0; r < 100000; r++ {
			w.Write(row[:1])
			w.Write(row[1:5])
			w.Write(row[5:])
		}
		w.Flush()
	}
}

func BenchmarkResultFlush_4KiB(b *testing.B)  { benchBuf(b, 4096) }
func BenchmarkResultFlush_32KiB(b *testing.B) { benchBuf(b, 32<<10) }
func BenchmarkResultFlush_64KiB(b *testing.B) { benchBuf(b, 64<<10) }
