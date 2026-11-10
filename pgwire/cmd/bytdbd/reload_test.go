package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/pgwire"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

// writeKeyPair writes a self-signed PEM certificate and key into dir.
func writeKeyPair(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return certFile, keyFile
}

func TestReload(t *testing.T) {
	dir := t.TempDir()
	e, err := bytdb.Open(filepath.Join(dir, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	srv := pgwire.NewServer(bsql.New(e))
	authFile := filepath.Join(dir, "auth")
	os.WriteFile(authFile, []byte("ada:old\n"), 0o600)
	creds, err := loadAuthFile(authFile)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := writeKeyPair(t, dir)
	rc := reloadConfig{authFile: authFile, tlsCert: certFile, tlsKey: keyFile, creds: creds, srv: srv}

	os.WriteFile(authFile, []byte("ada:new\nbob:pw\n"), 0o600)
	got := strings.Join(rc.reload(), "; ")
	if got != "auth file reloaded; TLS key pair reloaded" {
		t.Errorf("reload = %q", got)
	}
	// Broken files keep what is running.
	os.WriteFile(authFile, []byte("no colon here\n"), 0o600)
	os.WriteFile(certFile, []byte("not pem"), 0o600)
	got = strings.Join(rc.reload(), "; ")
	if !strings.Contains(got, "auth file not reloaded") || !strings.Contains(got, "TLS key pair not reloaded") {
		t.Errorf("reload of broken files = %q", got)
	}
	if got := (reloadConfig{srv: srv}).reload(); len(got) != 1 || !strings.Contains(got[0], "nothing to reload") {
		t.Errorf("reload with nothing configured = %q", got)
	}
}
