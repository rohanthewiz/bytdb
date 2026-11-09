package edgeaudit

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/rohanthewiz/bytdb/pgwire"
)

// A client that keeps reading, just slowly (64 KiB every 20 ms ~ 3 MB/s),
// receiving a ~30 MB result. WriteTimeout is 1 s; the client never stalls
// for anywhere near that long.
func TestSlowButProgressingReader(t *testing.T) {
	addr, _, e := startServer(t, func(s *pgwire.Server) { s.WriteTimeout = time.Second })
	_ = e
	c := pgxConn(t, addr)
	ctx := t.Context()
	c.Exec(ctx, `create table big (id int primary key, pad text)`)
	pad := make([]byte, 280)
	for i := range pad {
		pad[i] = 'x'
	}
	for base := 0; base < 100000; base += 1000 {
		q := `insert into big values `
		for i := 0; i < 1000; i++ {
			if i > 0 {
				q += ","
			}
			q += "(" + itoa(base+i) + ",'" + string(pad) + "')"
		}
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	body := append([]byte{0, 3, 0, 0}, "user\x00x\x00\x00"...)
	nc.Write(append(binary.BigEndian.AppendUint32(nil, uint32(len(body)+4)), body...))
	q := append([]byte("select * from big"), 0)
	nc.Write(append(append([]byte{'Q'}, binary.BigEndian.AppendUint32(nil, uint32(len(q)+4))...), q...))
	buf := make([]byte, 64<<10)
	total, start := 0, time.Now()
	var last []byte
	for {
		n, err := io.ReadAtLeast(nc, buf, 1)
		total += n
		if n > 0 {
			last = append(last[:0], buf[max(0, n-6):n]...)
		}
		if err != nil {
			t.Logf("stream ended after %v, %d bytes: %v", time.Since(start).Round(time.Millisecond), total, err)
			break
		}
		if len(last) == 6 && string(last) == "Z\x00\x00\x00\x05I" && total > 1000 {
			t.Logf("complete result received after %v, %d bytes", time.Since(start).Round(time.Millisecond), total)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("BUG: result truncated at %d bytes: the per-message write deadline is absolute, so a reader that is slow but never stalled is cut off after WriteTimeout", total)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
