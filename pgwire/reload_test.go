package pgwire_test

// Deep-look D-58: what bytdbd's SIGHUP reload rests on — a TLS
// configuration swapped while serving reaches new handshakes (and
// SCRAM-PLUS binds to the certificate each connection was served), and
// a credentials registry replaced whole changes who can log in.

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/rohanthewiz/bytdb/pgwire"
)

func TestSetTLSConfigWhileServing(t *testing.T) {
	ctx := context.Background()
	first, second := testTLSConfig(t), testTLSConfig(t)
	creds := pgwire.NewCredentials()
	creds.SetPassword("ada", "s3cret")
	var srv *pgwire.Server
	addr := startConfiguredServer(t, func(s *pgwire.Server) {
		s.TLSConfig = first
		s.Auth = creds
		srv = s
	})
	url := fmt.Sprintf("postgres://ada:s3cret@%s/any?sslmode=require&channel_binding=require", addr)
	served := func(c *pgx.Conn) []byte {
		cs := c.PgConn().Conn().(*tls.Conn).ConnectionState()
		return cs.PeerCertificates[0].Raw
	}
	before := connect(t, url)
	if !bytes.Equal(served(before), first.Certificates[0].Certificate[0]) {
		t.Fatal("first connection not served the first certificate")
	}
	srv.SetTLSConfig(second)
	after := connect(t, url) // channel binding must match the new certificate
	if !bytes.Equal(served(after), second.Certificates[0].Certificate[0]) {
		t.Error("new connection not served the reloaded certificate")
	}
	roundTrip(t, after)
	// The connection made before the swap keeps working.
	var one int64
	if err := before.QueryRow(ctx, `select 1`).Scan(&one); err != nil {
		t.Errorf("pre-reload connection: %v", err)
	}
}

func TestCredentialsReplaceFrom(t *testing.T) {
	ctx := context.Background()
	creds := pgwire.NewCredentials()
	creds.SetPassword("ada", "old")
	addr := startConfiguredServer(t, func(s *pgwire.Server) { s.Auth = creds })
	url := func(user, pw string) string { return fmt.Sprintf("postgres://%s:%s@%s/any", user, pw, addr) }
	connect(t, url("ada", "old"))

	fresh := pgwire.NewCredentials()
	fresh.SetPassword("ada", "new")
	fresh.SetPassword("bob", "pw")
	creds.ReplaceFrom(fresh)
	if c, err := pgx.Connect(ctx, url("ada", "old")); err == nil {
		c.Close(ctx)
		t.Error("old password still accepted after ReplaceFrom")
	}
	connect(t, url("ada", "new"))
	connect(t, url("bob", "pw"))
	// Later changes to the source do not leak into the live registry.
	fresh.SetPassword("eve", "x")
	if c, err := pgx.Connect(ctx, url("eve", "x")); err == nil {
		c.Close(ctx)
		t.Error("registry aliases its source")
	}
}
