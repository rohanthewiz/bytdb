// Command bytdbd serves a bytdb database file over the PostgreSQL
// wire protocol:
//
//	bytdbd -db app.bytdb -addr 127.0.0.1:5433
//	psql "postgres://any@127.0.0.1:5433/any?sslmode=disable"
//
// Local clients are faster over a Unix-domain socket (no TCP round
// trip); -socket adds one, named the way psql and pgx look for it:
//
//	bytdbd -db app.bytdb -addr 127.0.0.1:5433 -socket /tmp
//	psql -h /tmp -p 5433
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
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/pgwire"
	"github.com/rohanthewiz/bytdb/replicate"
	"github.com/rohanthewiz/bytdb/sql"
	"github.com/rohanthewiz/serr"
)

func main() {
	dbPath := flag.String("db", "", "database file (created if absent)")
	addr := flag.String("addr", "127.0.0.1:5433", "TCP listen address (empty = no TCP listener; then -socket is required)")
	socketDir := flag.String("socket", "",
		"also listen on a Unix-domain socket in this directory, named .s.PGSQL.<port> with the port of -addr (5433 when -addr is empty); psql -h <dir> connects to it")
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
		"maximum concurrent client connections (0 = the default, 100; negative = unlimited); excess clients get FATAL 53300")
	logQueries := flag.Bool("log-queries", false,
		"log every executed statement with its duration and outcome to stderr")
	encKeyFile := flag.String("encryption-key-file", "",
		"encrypt the WAL at rest with the AES-256 key in this file (32 raw bytes, 64 hex chars, or base64 of 32 bytes)")
	encKeyEnv := flag.String("encryption-key-env", "",
		"encrypt the WAL at rest with the AES-256 key in this environment variable (hex or base64 of 32 bytes)")
	metricsAddr := flag.String("metrics-addr", "",
		"serve Prometheus metrics (engine size, heap, GC, connections, replication) at http://<addr>/metrics and health at /healthz (empty = disabled)")
	// The server's default is a tenth of the embedded engine's: every
	// client's result is materialized in full before its first row is
	// sent (deep-look D-50), so the cap bounds memory per connection,
	// and a server has many connections. An embedded caller has one
	// process's worth of queries and keeps bytdb.DefaultMaxResultRows.
	maxResultRows := flag.Int64("max-result-rows", defaultServerMaxResultRows,
		"maximum rows one query may hold in memory (0 or negative = no cap); larger queries fail with 54000")
	maxTxnWrites := flag.Int64("max-txn-writes", bytdb.DefaultMaxTxnWrites,
		"maximum row inserts and updates one transaction may stage before commit (0 or negative = no cap); "+
			"the write past it fails with 54000")
	shutdownGrace := flag.Duration("shutdown-grace", 10*time.Second,
		"on SIGTERM/SIGINT, how long running statements may take to finish before they are canceled")
	memCeiling := flag.String("memory-ceiling", "auto",
		"refuse inserts, updates and large results with 53200 once the live heap passes this: "+
			"auto (90% of GOMEMLIMIT; inactive without it), off, or a size like 6GiB")
	// Server I/O bounds and engine modes that previously needed a Go
	// program to set (deep-look D-58). Zero keeps each library default.
	writeTimeout := flag.Duration("write-timeout", 0,
		"max time blocked writing one statement's output to a client that stopped reading (0 = default 60s, negative = off)")
	readTimeout := flag.Duration("read-timeout", 0,
		"max time to read one message body once it has started arriving (0 = default 60s, negative = off)")
	idleTimeout := flag.Duration("idle-timeout", 0,
		"close a session idle between statements this long, FATAL 57P05 (0 = never)")
	concurrent := flag.Bool("concurrent-writes", false,
		"optimistic concurrent write transactions (snapshot isolation, 40001 on conflict) instead of one writer at a time")
	compactMin := flag.Int64("compact-min-bytes", 0,
		"auto-compaction: minimum log size before it runs (0 = default 32 MB; negative disables auto-compaction)")
	compactGrowth := flag.Int("compact-growth-pct", 0,
		"auto-compaction: growth since the last compaction that triggers it, in percent (0 = default 100)")
	// Replication (deep-look D-58; see replication.go). Credentials come
	// from AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN.
	replicateTo := flag.String("replicate-s3", "",
		"ship the log to s3://bucket/prefix while serving (credentials from AWS_* env vars)")
	restoreFrom := flag.String("restore-from", "",
		"at startup, if -db does not exist, restore it from s3://bucket/prefix first")
	replicateInterval := flag.Duration("replicate-interval", 5*time.Second,
		"how often the log ships: the replica's data-loss window")
	s3Endpoint := flag.String("s3-endpoint", "",
		"S3-compatible endpoint URL (default https://s3.<region>.amazonaws.com)")
	s3Region := flag.String("s3-region", "", "S3 region (default $AWS_REGION, else us-east-1)")
	s3VirtualHost := flag.Bool("s3-virtual-host", false,
		"virtual-hosted-style addressing (bucket.endpoint) instead of path-style")
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
	if *concurrent {
		engineOpts = append(engineOpts, bytdb.WithConcurrentWrites())
	}
	switch {
	case *compactMin < 0:
		engineOpts = append(engineOpts, btypedb.WithAutoCompactDisabled())
	case *compactMin > 0 || *compactGrowth > 0:
		minSize, growth := *compactMin, *compactGrowth
		if minSize == 0 {
			minSize = 32 << 20
		}
		if growth == 0 {
			growth = 100
		}
		if growth < 0 {
			log.Fatal("bytdbd: -compact-growth-pct must not be negative")
		}
		engineOpts = append(engineOpts, btypedb.WithAutoCompact(minSize, growth))
	}
	// The key is sourced from a file or env var, never argv — a key on the
	// command line would leak through ps and shell history.
	encKey, err := loadEncryptionKey(*encKeyFile, *encKeyEnv)
	if err != nil {
		log.Fatalf("bytdbd: %s", serr.StringFromErr(err))
	}
	if encKey != nil {
		engineOpts = append(engineOpts, bytdb.WithEncryptionKey(encKey))
	}
	ceiling, err := parseMemCeiling(*memCeiling)
	if err != nil {
		log.Fatalf("bytdbd: %s", serr.StringFromErr(err))
	}
	if *addr == "" && *socketDir == "" {
		log.Fatal("bytdbd: -addr is empty and no -socket is set: nothing to listen on")
	}
	socketPort, err := portOf(*addr)
	if err != nil {
		log.Fatalf("bytdbd: %s", serr.StringFromErr(err))
	}
	s3cfg := s3Settings{endpoint: *s3Endpoint, region: *s3Region, virtualHost: *s3VirtualHost}
	var replStore replicate.Storage
	var replLoc s3Location
	if *replicateTo != "" {
		if replLoc, err = parseS3URL(*replicateTo); err != nil {
			log.Fatalf("bytdbd: -replicate-s3: %s", serr.StringFromErr(err))
		}
		client, err := s3cfg.client(replLoc)
		if err != nil {
			log.Fatalf("bytdbd: -replicate-s3: %s", serr.StringFromErr(err))
		}
		replStore = client
	}
	// Restore runs before the engine opens (it writes the file Open will
	// read) and only when the file is missing: an existing database is
	// never replaced from the replica.
	if *restoreFrom != "" {
		loc, err := parseS3URL(*restoreFrom)
		if err != nil {
			log.Fatalf("bytdbd: -restore-from: %s", serr.StringFromErr(err))
		}
		client, err := s3cfg.client(loc)
		if err != nil {
			log.Fatalf("bytdbd: -restore-from: %s", serr.StringFromErr(err))
		}
		ran, info, err := restoreIfMissing(context.Background(), client, loc.prefix, *dbPath)
		switch {
		case err != nil:
			log.Fatalf("bytdbd: restore from %s: %s", *restoreFrom, serr.StringFromErr(err))
		case ran:
			fmt.Printf("bytdbd: restored %s from %s (generation %s, %d bytes, %d objects)\n",
				*dbPath, *restoreFrom, info.Generation, info.Bytes, info.Chunks)
		default:
			fmt.Printf("bytdbd: %s exists; not restoring from %s\n", *dbPath, *restoreFrom)
		}
	}

	e, err := bytdb.Open(*dbPath, engineOpts...)
	if err != nil {
		log.Fatalf("bytdbd: %s", bytdb.ErrText(err))
	}
	// Closed on the way out of main — which the signal path below
	// reaches too — so the engine's final flush and fsync always run.
	defer e.Close()
	e.SetMaxResultRows(*maxResultRows)
	e.SetMaxTxnWrites(*maxTxnWrites)
	e.SetMemoryCeiling(ceiling)

	// The replicator starts after the engine and closes before it (defers
	// run in reverse), so its final ship sees every committed write.
	var repl replication
	if replStore != nil {
		r := replicate.New(e, replStore, replicate.Options{Interval: *replicateInterval, Prefix: replLoc.prefix})
		r.Start()
		defer r.Close()
		repl = replication{r: r, interval: *replicateInterval, started: time.Now()}
		fmt.Printf("bytdbd: replicating to %s every %v\n", *replicateTo, *replicateInterval)
	}

	srv := pgwire.NewServer(sql.New(e))
	srv.IdleTxTimeout = *idleTx
	srv.WriteTimeout = *writeTimeout
	srv.ReadTimeout = *readTimeout
	srv.IdleTimeout = *idleTimeout
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

	// The metrics listener is separate from the wire-protocol one so a
	// scraper never competes with clients for the connection cap, and
	// so it can be bound to a private interface on its own. It is
	// unauthenticated by design — the figures are operational, not
	// data — which is why the flag has no default address: the
	// operator chooses where it is reachable from.
	var metricsSrv *http.Server
	if *metricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", bytdb.MetricsHandler(e, func(w io.Writer) {
			// Appended after the engine's own gauges; same exposition
			// format, same prefix, so one scrape config covers both.
			fmt.Fprintf(w, "# HELP bytdb_pgwire_connections Open client connections.\n"+
				"# TYPE bytdb_pgwire_connections gauge\nbytdb_pgwire_connections %d\n", srv.ConnCount())
			repl.writeMetrics(w)
		}))
		mux.Handle("/healthz", healthHandler(e, repl))
		metricsSrv = &http.Server{Addr: *metricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				// Metrics are an aid, not the service: log and carry on
				// rather than take the database down with them.
				log.Printf("bytdbd: metrics listener: %v", err)
			}
		}()
		fmt.Printf("bytdbd: metrics on http://%s/metrics\n", *metricsAddr)
	}

	// A signal must not kill the process outright: under a relaxed
	// sync policy the engine buffers acknowledged writes, and only
	// Close guarantees they reach disk. Closing the server makes
	// ListenAndServe return nil, letting main unwind through the
	// deferred e.Close.
	//
	// The shutdown is graceful (deep-look D-35): idle sessions end with
	// FATAL 57P01, running statements get up to -shutdown-grace to finish
	// and reply, and only then is anything still running canceled.
	// Shutdown waits for every connection, so the deferred e.Close never
	// runs under an executing statement. A second signal skips the wait.
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-sig
		fmt.Printf("bytdbd: %s received, shutting down (grace %v)\n", s, *shutdownGrace)
		if metricsSrv != nil {
			metricsSrv.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), *shutdownGrace)
		defer cancel()
		go func() {
			<-sig
			fmt.Println("bytdbd: second signal, stopping now")
			cancel()
		}()
		if err := srv.Shutdown(ctx); err != nil {
			fmt.Printf("bytdbd: grace period over, canceled running statements: %v\n", err)
		}
	}()

	// The Unix socket opens after the engine, like the TCP listener, so
	// a failure here still unwinds through the deferred e.Close. Each
	// listener serves on its own goroutine; Close and Shutdown stop all.
	var unixLn net.Listener
	if *socketDir != "" {
		if unixLn, err = pgwire.ListenUnix(*socketDir, socketPort); err != nil {
			fmt.Fprintf(os.Stderr, "bytdbd: %s\n", serr.StringFromErr(err))
			return
		}
		fmt.Printf("bytdbd: serving %s on %s\n", *dbPath, pgwire.SocketPath(*socketDir, socketPort))
	}
	if *addr == "" {
		if err := srv.Serve(unixLn); err != nil {
			fmt.Fprintf(os.Stderr, "bytdbd: %v\n", err)
		}
		return
	}
	if unixLn != nil {
		go func() {
			if err := srv.Serve(unixLn); err != nil {
				fmt.Fprintf(os.Stderr, "bytdbd: unix socket: %v\n", err)
			}
		}()
	}
	fmt.Printf("bytdbd: serving %s on %s (tls: %v, auth: %v)\n",
		*dbPath, *addr, tlsConfig != nil, creds != nil)
	if err := srv.ListenAndServe(*addr); err != nil {
		// Not Fatalf: the engine still needs its deferred Close.
		fmt.Fprintf(os.Stderr, "bytdbd: %v\n", err)
	}
}

// defaultServerMaxResultRows is -max-result-rows' default (plan Q6).
const defaultServerMaxResultRows = 1_000_000

// portOf is the port a Unix socket is named after: -addr's, or 5433
// (bytdbd's default) when there is no TCP listener.
func portOf(addr string) (int, error) {
	if addr == "" {
		return 5433, nil
	}
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, serr.Wrap(err, "flag", "-addr")
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 0 || n > 65535 {
		return 0, serr.New("-addr needs a numeric port to name the Unix socket", "addr", addr)
	}
	return n, nil
}

// loadEncryptionKey sources the 32-byte WAL encryption key from a file or an
// environment variable (at most one). Returns nil,nil when neither is set —
// the database stays plaintext. Accepted encodings: exactly 32 raw bytes, 64
// hex characters, or base64 of 32 bytes. The key is never taken from argv, so
// it cannot leak through ps output or shell history.
func loadEncryptionKey(file, env string) ([]byte, error) {
	var raw []byte
	switch {
	case file != "" && env != "":
		return nil, serr.New("set only one of -encryption-key-file / -encryption-key-env")
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, serr.Wrap(err, "reading encryption key file")
		}
		raw = b
	case env != "":
		v, ok := os.LookupEnv(env)
		if !ok {
			return nil, serr.New("encryption key env var is not set", "env", env)
		}
		raw = []byte(v)
	default:
		return nil, nil // no encryption requested
	}
	return decodeEncryptionKey(raw)
}

// decodeEncryptionKey interprets raw key material. Exactly 32 bytes is used
// verbatim (a raw binary key file); otherwise the trimmed text is decoded as
// hex (64 chars) or base64, and must yield 32 bytes.
// The rule is the engine's (bytdb.DecodeEncryptionKey), so the server
// and the database/sql DSN accept the same key files.
func decodeEncryptionKey(raw []byte) ([]byte, error) {
	return bytdb.DecodeEncryptionKey(raw)
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

// parseMemCeiling maps the -memory-ceiling flag onto
// Engine.SetMemoryCeiling's encoding: "auto" is 0, "off" is -1, and a
// size is a positive byte count. Sizes take the same unit suffixes as
// GOMEMLIMIT (B, KiB, MiB, GiB, TiB — binary multiples), so an operator
// can write both settings in one vocabulary: GOMEMLIMIT=7GiB with
// -memory-ceiling 6GiB.
//
// The parsing is the engine's (bytdb.ParseMemoryCeiling, shared with the
// database/sql DSN's memory_ceiling); the flag keeps its own wording.
func parseMemCeiling(v string) (int64, error) {
	n, err := bytdb.ParseMemoryCeiling(v)
	if err != nil {
		return 0, serr.New("-memory-ceiling must be auto, off, or a positive size such as 6GiB",
			"value", v)
	}
	return n, nil
}
