package pgwire_test

// End-to-end test that an encrypted-at-rest bytdb serves and persists
// correctly across the real Postgres wire protocol: write rows over pgx,
// restart the server on the same encrypted file, and read them back — then
// confirm the on-disk file is unreadable both as bytes (the plaintext value is
// absent) and without the key (Open fails).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"

	"github.com/rohanthewiz/bytdb/pgwire"
)

func TestEncryptedServerEndToEnd(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "enc.db")
	key := bytes.Repeat([]byte{0x2B}, 32)

	// serve opens the encrypted engine, serves it on a loopback port, and
	// returns the connection string plus a stop func that closes both.
	serve := func() (string, func()) {
		e, err := bytdb.Open(dbPath, bytdb.WithEncryptionKey(key))
		if err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			e.Close()
			t.Fatal(err)
		}
		srv := pgwire.NewServer(bsql.New(e))
		go srv.Serve(ln)
		return fmt.Sprintf("postgres://test@%s/test", ln.Addr()), func() {
			srv.Close()
			e.Close()
		}
	}

	// First session: create a table and insert secret-bearing rows.
	conn1, stop1 := serve()
	c1, err := pgx.Connect(ctx, conn1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c1.Exec(ctx, `CREATE TABLE users (id int PRIMARY KEY, name text)`); err != nil {
		t.Fatal(err)
	}
	if _, err := c1.Exec(ctx, `INSERT INTO users VALUES (1, 'ada-SECRET'), (2, 'grace-SECRET')`); err != nil {
		t.Fatal(err)
	}
	c1.Close(ctx)
	stop1() // closes the engine → final fsync of the encrypted log

	// The plaintext value must not survive in the on-disk log.
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("SECRET")) {
		t.Fatalf("plaintext value found in on-disk log")
	}

	// Second session: reopen the same encrypted file and read the rows back
	// over the wire — proving replay-decrypt works through the full stack.
	conn2, stop2 := serve()
	defer stop2()
	c2, err := pgx.Connect(ctx, conn2)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close(ctx)

	var name string
	if err := c2.QueryRow(ctx, `SELECT name FROM users WHERE id = 1`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "ada-SECRET" {
		t.Fatalf("got %q, want ada-SECRET", name)
	}
	var n int
	if err := c2.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("got %d rows, want 2", n)
	}

	// Without the key the file cannot be opened at all.
	if _, err := bytdb.Open(dbPath); !errors.Is(err, btypedb.ErrKeyRequired) {
		t.Fatalf("no-key open: got %v, want ErrKeyRequired", err)
	}
}
