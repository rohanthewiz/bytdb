package pgwire_test

// Deep-look D-42: a Unix-domain socket listener, named so psql and pgx
// find it from a directory and port.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/pgwire"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

// socketDir is a short temporary directory: a Unix socket path must fit
// in sun_path (104 bytes on macOS), which t.TempDir's long names can
// overflow.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bsk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestUnixSocketServe(t *testing.T) {
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	srv := pgwire.NewServer(bsql.New(e))
	// RequireTLS refuses plaintext TCP; the local socket is exempt, as a
	// Postgres "local" line is. (No TLSConfig: TCP would be refused.)
	srv.RequireTLS = true

	dir := socketDir(t)
	const port = 54329
	ln, err := pgwire.ListenUnix(dir, port)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(tcp)

	path := pgwire.SocketPath(dir, port)
	if path != filepath.Join(dir, ".s.PGSQL.54329") {
		t.Fatalf("socket path %s", path)
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, fmt.Sprintf("host=%s port=%d user=u dbname=u", dir, port))
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := c.QueryRow(ctx, "select 41 + 1").Scan(&n); err != nil || n != 42 {
		t.Fatalf("query over the socket = %d, %v", n, err)
	}
	c.Close(ctx)
	if _, err := pgx.Connect(ctx, fmt.Sprintf("postgres://u@%s/u?sslmode=disable", tcp.Addr())); err == nil {
		t.Fatal("plaintext TCP accepted under RequireTLS")
	}

	// A second server cannot take a live socket.
	if _, err := pgwire.ListenUnix(dir, port); err == nil {
		t.Fatal("ListenUnix took a socket another server is serving")
	}

	// Shutdown closes both listeners and removes the socket file.
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket file left after shutdown: %v", err)
	}
	if c, err := net.DialTimeout("tcp", tcp.Addr().String(), time.Second); err == nil {
		c.Close()
		t.Fatal("TCP listener still open after shutdown")
	}
}

func TestListenUnixPathHandling(t *testing.T) {
	dir := socketDir(t)
	// A stale socket file (its server died without closing it) is
	// replaced.
	path := pgwire.SocketPath(dir, 5)
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("stale socket not left behind: %v", err)
	}
	ln, err := pgwire.ListenUnix(dir, 5)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o777 {
		t.Fatalf("socket mode %v, %v; want 0777", fi.Mode(), err)
	}
	ln.Close()

	// Anything else at the path is not ours to delete.
	other := pgwire.SocketPath(dir, 6)
	if err := os.WriteFile(other, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pgwire.ListenUnix(dir, 6); err == nil {
		t.Fatal("ListenUnix replaced a regular file")
	}
	if b, _ := os.ReadFile(other); string(b) != "keep" {
		t.Fatal("regular file at the socket path was modified")
	}
}
