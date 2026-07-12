// Command bytdbd serves a bytdb database file over the PostgreSQL
// wire protocol:
//
//	bytdbd -db app.bytdb -addr 127.0.0.1:5433
//	psql "postgres://any@127.0.0.1:5433/any?sslmode=disable"
//
// With TLS and SCRAM authentication:
//
//	bytdbd -db app.bytdb -tls-cert server.crt -tls-key server.key \
//	       -require-tls -auth-file users.auth
//	psql "postgres://ada:s3cret@127.0.0.1:5433/any?sslmode=require"
//
// The auth file holds one user per line, either user:password or —
// so the file never carries plaintext — user:SCRAM-SHA-256$<verifier>
// as produced by pgwire.MakeVerifier (the pg_authid rolpassword
// format). Blank lines and #-comments are ignored.
package main

import (
	"bufio"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/pgwire"
	"github.com/rohanthewiz/bytdb/sql"
	"github.com/rohanthewiz/serr"
)

func main() {
	dbPath := flag.String("db", "", "database file (created if absent)")
	addr := flag.String("addr", "127.0.0.1:5433", "listen address")
	idleTx := flag.Duration("idle-tx-timeout", 0,
		"terminate connections idle in a transaction this long (0 = server default, negative = disabled)")
	tlsCert := flag.String("tls-cert", "", "TLS certificate (PEM); with -tls-key, the server accepts SSL connections")
	tlsKey := flag.String("tls-key", "", "TLS private key (PEM)")
	requireTLS := flag.Bool("require-tls", false, "refuse connections that do not upgrade to TLS")
	authFile := flag.String("auth-file", "",
		"SCRAM credentials, one user:password or user:SCRAM-SHA-256$... per line (absent = trust any client)")
	syncMode := flag.String("sync", "always",
		"WAL fsync policy: always (durable through power loss) or never (faster; the OS decides when to flush)")
	maxConns := flag.Int("max-conns", 0,
		"maximum concurrent client connections (0 = unlimited); excess clients get FATAL 53300")
	logQueries := flag.Bool("log-queries", false,
		"log every executed statement with its duration and outcome to stderr")
	flag.Parse()
	if *dbPath == "" {
		log.Fatal("bytdbd: -db is required")
	}

	// All configuration is validated before the engine opens: log.Fatal
	// skips defers, so nothing fatal may run once e.Close is deferred.
	tlsConfig, err := loadTLS(*tlsCert, *tlsKey)
	if err != nil {
		log.Fatalf("bytdbd: %s", serr.StringFromErr(err))
	}
	if *requireTLS && tlsConfig == nil {
		log.Fatal("bytdbd: -require-tls needs -tls-cert and -tls-key")
	}
	var creds *pgwire.Credentials
	if *authFile != "" {
		if creds, err = loadAuthFile(*authFile); err != nil {
			log.Fatalf("bytdbd: %s", serr.StringFromErr(err))
		}
	}
	var engineOpts []btypedb.Option
	switch *syncMode {
	case "always": // the default policy needs no option
	case "never":
		engineOpts = append(engineOpts, bytdb.WithSyncNever())
	default:
		log.Fatalf("bytdbd: -sync must be always or never (got %q)", *syncMode)
	}

	e, err := bytdb.Open(*dbPath, engineOpts...)
	if err != nil {
		log.Fatalf("bytdbd: %s", bytdb.ErrText(err))
	}
	// Closed on the way out of main — which the signal path below
	// reaches too — so the engine's final flush and fsync always run.
	defer e.Close()

	srv := pgwire.NewServer(sql.New(e))
	srv.IdleTxTimeout = *idleTx
	srv.TLSConfig = tlsConfig
	srv.RequireTLS = *requireTLS
	srv.Auth = creds
	srv.MaxConns = *maxConns
	if *logQueries {
		// log (not fmt) so entries interleave atomically across
		// connection goroutines and carry timestamps.
		srv.QueryLog = func(q string, d time.Duration, err error) {
			outcome := "ok"
			if err != nil {
				outcome = "error: " + err.Error()
			}
			log.Printf("query (%s) %s [%s]", d.Round(time.Microsecond), q, outcome)
		}
	}

	// A signal must not kill the process outright: under a relaxed
	// sync policy the engine buffers acknowledged writes, and only
	// Close guarantees they reach disk. Closing the server makes
	// ListenAndServe return nil, letting main unwind through the
	// deferred e.Close.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-sig
		fmt.Printf("bytdbd: %s received, shutting down\n", s)
		srv.Close()
	}()

	fmt.Printf("bytdbd: serving %s on %s (tls: %v, auth: %v)\n",
		*dbPath, *addr, tlsConfig != nil, creds != nil)
	if err := srv.ListenAndServe(*addr); err != nil {
		// Not Fatalf: the engine still needs its deferred Close.
		fmt.Fprintf(os.Stderr, "bytdbd: %v\n", err)
	}
}

// loadTLS builds the server TLS config from a cert/key pair; both
// flags or neither.
func loadTLS(certFile, keyFile string) (*tls.Config, error) {
	if certFile == "" && keyFile == "" {
		return nil, nil
	}
	if certFile == "" || keyFile == "" {
		return nil, serr.New("-tls-cert and -tls-key must be set together")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, serr.Wrap(err, "loading TLS key pair")
	}
	// Go's server-side default still admits TLS 1.0/1.1 handshakes;
	// nothing that can speak SCRAM needs anything below 1.2.
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// loadAuthFile parses the credentials file into a registry. A value
// with the SCRAM verifier prefix is stored as-is; anything else is
// treated as a plaintext password and hashed on the spot.
func loadAuthFile(path string) (*pgwire.Credentials, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, serr.Wrap(err, "opening auth file")
	}
	defer f.Close()
	creds := pgwire.NewCredentials()
	sc := bufio.NewScanner(f)
	for lineNo := 1; sc.Scan(); lineNo++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Split on the first colon only: verifiers (and passwords)
		// contain colons of their own.
		user, secret, ok := strings.Cut(line, ":")
		if !ok || user == "" || secret == "" {
			return nil, serr.New("auth file: want user:password or user:verifier",
				"file", path, "line", fmt.Sprint(lineNo))
		}
		if strings.HasPrefix(secret, "SCRAM-SHA-256$") {
			if err := creds.SetVerifier(user, secret); err != nil {
				return nil, serr.Wrap(err, "auth file", "file", path, "line", fmt.Sprint(lineNo))
			}
		} else {
			creds.SetPassword(user, secret)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, serr.Wrap(err, "reading auth file")
	}
	return creds, nil
}
