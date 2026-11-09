package edgeaudit

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/bytdb/pgwire"
)

// The same raw Bind/Execute/Sync point read, over a Unix-domain socket
// (Server.Serve takes any net.Listener; bytdbd only exposes TCP).
func BenchmarkPointRead_WireRawUnix(b *testing.B) {
	_, db := benchEngine(b)
	dir, _ := os.MkdirTemp("/tmp", "ea")
	defer os.RemoveAll(dir)
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		b.Fatal(err)
	}
	srv := pgwire.NewServer(db)
	go srv.Serve(ln)
	defer srv.Close()
	nc, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	benchRawPoint(b, rawFrom(b, nc))
}
