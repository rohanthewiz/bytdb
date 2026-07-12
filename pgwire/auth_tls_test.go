package pgwire_test

// End-to-end TLS and SCRAM-SHA-256 tests through the real pgx driver:
// pgx implements the client half of both (including SASLprep and the
// server-signature check), so a passing handshake here means real
// Postgres clients interoperate.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"

	"github.com/rohanthewiz/bytdb/pgwire"
)

// startConfiguredServer is startServer with a hook to set TLS/auth
// fields before the server begins accepting. It returns just the
// address; callers build connection strings with the credentials and
// sslmode under test.
func startConfiguredServer(t *testing.T, configure func(*pgwire.Server)) string {
	t.Helper()
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := pgwire.NewServer(bsql.New(e))
	if configure != nil {
		configure(srv)
	}
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		e.Close()
	})
	return ln.Addr().String()
}

// testTLSConfig builds a fresh self-signed cert for 127.0.0.1. pgx's
// sslmode=require encrypts without verifying the chain (libpq
// semantics), which is exactly what a self-signed server cert needs.
func testTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
	}
}

// roundTrip proves the connection actually works: DDL, a bound
// insert, and a read back.
func roundTrip(t *testing.T, c *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	mustExec(t, c, `create table secrets (id int primary key, v text)`)
	mustExec(t, c, `insert into secrets values ($1, $2)`, int64(1), "hush")
	var v string
	if err := c.QueryRow(ctx, `select v from secrets where id = $1`, int64(1)).Scan(&v); err != nil || v != "hush" {
		t.Fatalf("round trip: %v %q", err, v)
	}
	mustExec(t, c, `drop table secrets`)
}

func TestTLS(t *testing.T) {
	addr := startConfiguredServer(t, func(s *pgwire.Server) {
		s.TLSConfig = testTLSConfig(t)
	})

	c := connect(t, fmt.Sprintf("postgres://any@%s/any?sslmode=require", addr))
	// The driver-level connection must really be TLS, not a fallback.
	if _, ok := c.PgConn().Conn().(*tls.Conn); !ok {
		t.Fatalf("connection is %T, want *tls.Conn", c.PgConn().Conn())
	}
	roundTrip(t, c)

	// Without RequireTLS, plaintext clients stay welcome alongside.
	roundTrip(t, connect(t, fmt.Sprintf("postgres://any@%s/any?sslmode=disable", addr)))
}

func TestRequireTLS(t *testing.T) {
	addr := startConfiguredServer(t, func(s *pgwire.Server) {
		s.TLSConfig = testTLSConfig(t)
		s.RequireTLS = true
	})

	roundTrip(t, connect(t, fmt.Sprintf("postgres://any@%s/any?sslmode=require", addr)))

	var pgErr *pgconn.PgError
	_, err := pgx.Connect(context.Background(), fmt.Sprintf("postgres://any@%s/any?sslmode=disable", addr))
	if !errors.As(err, &pgErr) || pgErr.Code != "28000" || pgErr.Severity != "FATAL" {
		t.Fatalf("plaintext connect: %+v", err)
	}
}

func TestSCRAMAuth(t *testing.T) {
	creds := pgwire.NewCredentials()
	creds.SetPassword("ada", "s3cret")
	addr := startConfiguredServer(t, func(s *pgwire.Server) { s.Auth = creds })

	roundTrip(t, connect(t, fmt.Sprintf("postgres://ada:s3cret@%s/any?sslmode=disable", addr)))

	// Wrong password and unknown user must fail identically: same
	// SQLSTATE, same message shape (modulo the user name).
	for _, url := range []string{
		fmt.Sprintf("postgres://ada:wrong@%s/any?sslmode=disable", addr),
		fmt.Sprintf("postgres://nobody:s3cret@%s/any?sslmode=disable", addr),
	} {
		var pgErr *pgconn.PgError
		if _, err := pgx.Connect(context.Background(), url); !errors.As(err, &pgErr) || pgErr.Code != "28P01" {
			t.Fatalf("connect %s: %+v", url, err)
		}
	}

	// Re-keying a live server takes effect on the next login.
	creds.SetPassword("ada", "n3w")
	roundTrip(t, connect(t, fmt.Sprintf("postgres://ada:n3w@%s/any?sslmode=disable", addr)))
	if _, err := pgx.Connect(context.Background(), fmt.Sprintf("postgres://ada:s3cret@%s/any?sslmode=disable", addr)); err == nil {
		t.Fatal("old password still accepted")
	}
}

// TestSCRAMOverTLS matters because pgx switches its SCRAM gs2 header
// from "n,," to "y,," on TLS connections; the server must accept both
// and verify the matching c= echo in the client proof.
func TestSCRAMOverTLS(t *testing.T) {
	creds := pgwire.NewCredentials()
	creds.SetPassword("ada", "s3cret")
	addr := startConfiguredServer(t, func(s *pgwire.Server) {
		s.TLSConfig = testTLSConfig(t)
		s.RequireTLS = true
		s.Auth = creds
	})

	c := connect(t, fmt.Sprintf("postgres://ada:s3cret@%s/any?sslmode=require", addr))
	if _, ok := c.PgConn().Conn().(*tls.Conn); !ok {
		t.Fatalf("connection is %T, want *tls.Conn", c.PgConn().Conn())
	}
	roundTrip(t, c)

	var pgErr *pgconn.PgError
	if _, err := pgx.Connect(context.Background(),
		fmt.Sprintf("postgres://ada:wrong@%s/any?sslmode=require", addr)); !errors.As(err, &pgErr) || pgErr.Code != "28P01" {
		t.Fatalf("wrong password over TLS: %+v", err)
	}
}

// TestVerifierAndSASLprep covers the no-plaintext path (MakeVerifier →
// SetVerifier) and a non-ASCII password, which the client normalizes
// with SASLprep before hashing — the server must derive the same keys.
func TestVerifierAndSASLprep(t *testing.T) {
	creds := pgwire.NewCredentials()
	if err := creds.SetVerifier("grace", pgwire.MakeVerifier("hopper")); err != nil {
		t.Fatal(err)
	}
	creds.SetPassword("ünïcode", "påsswörd !") // U+00A0 nbsp: SASLprep maps it to a space
	addr := startConfiguredServer(t, func(s *pgwire.Server) { s.Auth = creds })

	roundTrip(t, connect(t, fmt.Sprintf("postgres://grace:hopper@%s/any?sslmode=disable", addr)))

	cfg, err := pgx.ParseConfig(fmt.Sprintf("postgres://%s/any?sslmode=disable", addr))
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Password = "ünïcode", "påsswörd !"
	c, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	roundTrip(t, c)

	// Malformed verifiers are rejected up front, not at login time.
	for _, bad := range []string{
		"md5abc123",
		"SCRAM-SHA-256$notanumber:c2FsdA==$AAAA:BBBB",
		"SCRAM-SHA-256$4096:c2FsdA==$short:short",
	} {
		if err := creds.SetVerifier("eve", bad); err == nil {
			t.Fatalf("SetVerifier(%q) accepted", bad)
		}
	}
}

// TestCancelWithAuth: CancelRequest arrives on a fresh, deliberately
// unauthenticated connection; the shared secret is its credential.
// The pgx cancel path must still work when SCRAM and TLS are on.
func TestCancelWithAuth(t *testing.T) {
	creds := pgwire.NewCredentials()
	creds.SetPassword("ada", "s3cret")
	addr := startConfiguredServer(t, func(s *pgwire.Server) {
		s.TLSConfig = testTLSConfig(t)
		s.Auth = creds
	})
	c := connect(t, fmt.Sprintf("postgres://ada:s3cret@%s/any?sslmode=require", addr))
	seedHeavy(t, c, 400)

	errCh := make(chan error, 1)
	go func() {
		_, err := c.Exec(context.Background(), heavyQuery)
		errCh <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the query reach the executor
	// pgx's cancel connection repeats the client's transport setup —
	// SSLRequest, TLS handshake — then sends CancelRequest instead of
	// a startup message, skipping SCRAM entirely.
	if err := c.PgConn().CancelRequest(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
			t.Fatalf("got %v, want SQLSTATE 57014", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CancelRequest did not stop the query")
	}
	// Only the statement died: the session must still serve queries.
	var n int64
	if err := c.QueryRow(context.Background(), `select count(*) from n`).Scan(&n); err != nil || n != 400 {
		t.Fatalf("after cancel: %v %d", err, n)
	}
}
