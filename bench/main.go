// Command bench runs the same small OLTP-ish workload against bytdb
// (embedded and over pgwire), PostgreSQL, DuckDB, and Redis, and
// prints a markdown results table plus JSON for the docs.
//
// It benchmarks one client, one connection, sequential operations —
// engine latency, not saturation throughput. See docs/benchmarks.md
// for methodology and caveats.
//
// Run with GOWORK=off (this module is not in the repo workspace):
//
//	GOWORK=off go run . -targets bytdb-always,bytdb-lazy,bytdb-pgwire,postgres,duckdb,redis
//
// External services are expected up before the run (the Makefile-ish
// steps live in run.sh): postgres on 127.0.0.1:54329 with PGDATA on a
// tmpfs, redis-server on 127.0.0.1:63799.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/marcboeker/go-duckdb/v2"
	"github.com/redis/go-redis/v9"
	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"
	"github.com/rohanthewiz/bytdb/pgwire"
)

const (
	nPointInsert = 5_000
	nBulkRows    = 100_000
	bulkBatch    = 1_000
	nPointRead   = 20_000
	nPointUpdate = 5_000
	nAggReps     = 20

	pgAddr     = "127.0.0.1:54329"
	redisAddr  = "127.0.0.1:63799"
	pgwireAddr = "127.0.0.1:55433"
)

// workload names, in report order.
var workloads = []string{"insert_point", "insert_batch", "read_point", "update_point", "scan_agg"}

type result struct {
	Target   string             `json:"target"`
	Note     string             `json:"note"`
	MicrosOp map[string]float64 `json:"us_per_op"` // workload -> µs/op (0 = not applicable)
}

// sqlTarget is the surface every SQL system exposes to the runner.
type sqlTarget interface {
	exec(q string) error
	insertOne(id int, name string, amt float64) error
	insertBatch(q string) error
	readOne(id int) error
	updateOne(id int, amt float64) error
	scanAgg() error
	close()
}

func main() {
	targetsFlag := flag.String("targets", "bytdb-always,bytdb-lazy,bytdb-pgwire,postgres,duckdb,redis", "comma-separated targets")
	outFlag := flag.String("out", "results.json", "JSON output path")
	flag.Parse()

	var results []result
	for _, t := range strings.Split(*targetsFlag, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		fmt.Fprintf(os.Stderr, "=== %s ===\n", t)
		r, err := runTarget(t)
		if err != nil {
			fmt.Fprintf(os.Stderr, "target %s failed: %v\n", t, err)
			os.Exit(1)
		}
		results = append(results, r)
	}

	printTable(results)
	blob, _ := json.MarshalIndent(results, "", "  ")
	if err := os.WriteFile(*outFlag, blob, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runTarget(name string) (result, error) {
	switch name {
	case "bytdb-always", "bytdb-lazy":
		policy := btypedb.SyncAlways
		note := "embedded, SyncAlways (fsync per commit)"
		if name == "bytdb-lazy" {
			policy = btypedb.SyncEverySecond
			note = "embedded, SyncEverySecond"
		}
		dir, err := os.MkdirTemp("", "bytdbbench")
		if err != nil {
			return result{}, err
		}
		defer os.RemoveAll(dir)
		e, err := bytdb.Open(filepath.Join(dir, "bench.db"), btypedb.WithSyncPolicy(policy))
		if err != nil {
			return result{}, err
		}
		t := &embedTarget{db: bsql.New(e)}
		defer e.Close()
		return runSQL(name, note, t)

	case "bytdb-pgwire":
		dir, err := os.MkdirTemp("", "bytdbbench")
		if err != nil {
			return result{}, err
		}
		defer os.RemoveAll(dir)
		e, err := bytdb.Open(filepath.Join(dir, "bench.db"), btypedb.WithSyncPolicy(btypedb.SyncEverySecond))
		if err != nil {
			return result{}, err
		}
		defer e.Close()
		srv := pgwire.NewServer(bsql.New(e))
		ln, err := net.Listen("tcp", pgwireAddr)
		if err != nil {
			return result{}, err
		}
		go srv.Serve(ln)
		defer srv.Close()
		t, err := newPgxTarget("postgres://bench@" + pgwireAddr + "/bench?sslmode=disable")
		if err != nil {
			return result{}, err
		}
		defer t.close()
		return runSQL(name, "pgwire TCP loopback, SyncEverySecond", t)

	case "postgres":
		t, err := newPgxTarget("postgres://postgres:bench@" + pgAddr + "/postgres?sslmode=disable")
		if err != nil {
			return result{}, err
		}
		defer t.close()
		return runSQL(name, "Postgres 16, Docker, PGDATA on tmpfs", t)

	case "duckdb":
		db, err := sql.Open("duckdb", "")
		if err != nil {
			return result{}, err
		}
		t := &stdTarget{db: db}
		defer db.Close()
		return runSQL(name, "in-memory, database/sql driver", t)

	case "redis":
		return runRedis()
	}
	return result{}, fmt.Errorf("unknown target %q", name)
}

// runSQL drives the five workloads against one SQL target.
func runSQL(name, note string, t sqlTarget) (result, error) {
	for _, q := range []string{
		"DROP TABLE t_point", "DROP TABLE t_bulk",
	} {
		t.exec(q) // best-effort cleanup; tables may not exist
	}
	for _, q := range []string{
		"CREATE TABLE t_point (id INT PRIMARY KEY, name TEXT, amount FLOAT)",
		"CREATE TABLE t_bulk (id INT PRIMARY KEY, name TEXT, amount FLOAT)",
	} {
		if err := t.exec(q); err != nil {
			return result{}, fmt.Errorf("setup: %w", err)
		}
	}
	us := map[string]float64{}

	// insert_point: autocommit single-row inserts.
	d, err := timeN(nPointInsert, func(i int) error {
		return t.insertOne(i, fmt.Sprintf("user-%d", i), float64(i%1000)+0.25)
	})
	if err != nil {
		return result{}, fmt.Errorf("insert_point: %w", err)
	}
	us["insert_point"] = usPerOp(d, nPointInsert)

	// insert_batch: literal multi-row VALUES, bulkBatch rows per statement.
	start := time.Now()
	for base := 0; base < nBulkRows; base += bulkBatch {
		if err := t.insertBatch(batchSQL(base)); err != nil {
			return result{}, fmt.Errorf("insert_batch: %w", err)
		}
	}
	us["insert_batch"] = usPerOp(time.Since(start), nBulkRows)

	rng := rand.New(rand.NewSource(42))
	ids := make([]int, nPointRead)
	for i := range ids {
		ids[i] = rng.Intn(nBulkRows)
	}

	d, err = timeN(nPointRead, func(i int) error { return t.readOne(ids[i]) })
	if err != nil {
		return result{}, fmt.Errorf("read_point: %w", err)
	}
	us["read_point"] = usPerOp(d, nPointRead)

	d, err = timeN(nPointUpdate, func(i int) error {
		return t.updateOne(ids[i%nPointRead], float64(i)+0.5)
	})
	if err != nil {
		return result{}, fmt.Errorf("update_point: %w", err)
	}
	us["update_point"] = usPerOp(d, nPointUpdate)

	d, err = timeN(nAggReps, func(int) error { return t.scanAgg() })
	if err != nil {
		return result{}, fmt.Errorf("scan_agg: %w", err)
	}
	us["scan_agg"] = usPerOp(d, nAggReps)

	return result{Target: name, Note: note, MicrosOp: us}, nil
}

func batchSQL(base int) string {
	var b strings.Builder
	b.WriteString("INSERT INTO t_bulk VALUES ")
	for i := 0; i < bulkBatch; i++ {
		id := base + i
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "(%d,'user-%d',%g)", id, id, float64(id%1000)+0.25)
	}
	return b.String()
}

func timeN(n int, f func(i int) error) (time.Duration, error) {
	start := time.Now()
	for i := 0; i < n; i++ {
		if err := f(i); err != nil {
			return 0, err
		}
	}
	return time.Since(start), nil
}

func usPerOp(d time.Duration, n int) float64 {
	return float64(d.Microseconds()) / float64(n)
}

func printTable(results []result) {
	fmt.Printf("| target | note |")
	for _, w := range workloads {
		fmt.Printf(" %s |", w)
	}
	fmt.Println()
	fmt.Printf("|---|---|")
	for range workloads {
		fmt.Printf("---:|")
	}
	fmt.Println()
	for _, r := range results {
		fmt.Printf("| %s | %s |", r.Target, r.Note)
		for _, w := range workloads {
			v := r.MicrosOp[w]
			if v == 0 {
				fmt.Printf(" — |")
			} else {
				fmt.Printf(" %.1f µs |", v)
			}
		}
		fmt.Println()
	}
}

// --- bytdb embedded ---

type embedTarget struct {
	db      *bsql.DB
	insStmt *bsql.Stmt
	getStmt *bsql.Stmt
	updStmt *bsql.Stmt
}

func (t *embedTarget) exec(q string) error { _, err := t.db.Exec(q); return err }

func (t *embedTarget) insertOne(id int, name string, amt float64) error {
	if t.insStmt == nil {
		s, err := t.db.Prepare("INSERT INTO t_point VALUES ($1, $2, $3)")
		if err != nil {
			return err
		}
		t.insStmt = s
	}
	_, err := t.insStmt.Exec(int64(id), name, amt)
	return err
}

func (t *embedTarget) insertBatch(q string) error { _, err := t.db.Exec(q); return err }

func (t *embedTarget) readOne(id int) error {
	if t.getStmt == nil {
		s, err := t.db.Prepare("SELECT name, amount FROM t_bulk WHERE id = $1")
		if err != nil {
			return err
		}
		t.getStmt = s
	}
	res, err := t.getStmt.Exec(int64(id))
	if err != nil {
		return err
	}
	if len(res.Rows) != 1 {
		return fmt.Errorf("expected 1 row, got %d", len(res.Rows))
	}
	return nil
}

func (t *embedTarget) updateOne(id int, amt float64) error {
	if t.updStmt == nil {
		s, err := t.db.Prepare("UPDATE t_bulk SET amount = $1 WHERE id = $2")
		if err != nil {
			return err
		}
		t.updStmt = s
	}
	_, err := t.updStmt.Exec(amt, int64(id))
	return err
}

func (t *embedTarget) scanAgg() error {
	res, err := t.db.Exec("SELECT count(*), sum(amount) FROM t_bulk")
	if err != nil {
		return err
	}
	if len(res.Rows) != 1 {
		return fmt.Errorf("expected 1 row")
	}
	return nil
}

func (t *embedTarget) close() {}

// --- pgx (postgres and bytdb-over-pgwire) ---

type pgxTarget struct {
	ctx  context.Context
	conn *pgx.Conn
}

func newPgxTarget(dsn string) (*pgxTarget, error) {
	ctx := context.Background()
	// Retry while the server (container) finishes starting.
	var conn *pgx.Conn
	var err error
	for i := 0; i < 60; i++ {
		conn, err = pgx.Connect(ctx, dsn)
		if err == nil {
			return &pgxTarget{ctx: ctx, conn: conn}, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, fmt.Errorf("connect %s: %w", dsn, err)
}

func (t *pgxTarget) exec(q string) error { _, err := t.conn.Exec(t.ctx, q); return err }

func (t *pgxTarget) insertOne(id int, name string, amt float64) error {
	_, err := t.conn.Exec(t.ctx, "INSERT INTO t_point VALUES ($1, $2, $3)", int64(id), name, amt)
	return err
}

func (t *pgxTarget) insertBatch(q string) error { _, err := t.conn.Exec(t.ctx, q); return err }

func (t *pgxTarget) readOne(id int) error {
	var name string
	var amt float64
	return t.conn.QueryRow(t.ctx, "SELECT name, amount FROM t_bulk WHERE id = $1", int64(id)).Scan(&name, &amt)
}

func (t *pgxTarget) updateOne(id int, amt float64) error {
	_, err := t.conn.Exec(t.ctx, "UPDATE t_bulk SET amount = $1 WHERE id = $2", amt, int64(id))
	return err
}

func (t *pgxTarget) scanAgg() error {
	var n int64
	var s float64
	return t.conn.QueryRow(t.ctx, "SELECT count(*), sum(amount) FROM t_bulk").Scan(&n, &s)
}

func (t *pgxTarget) close() { t.conn.Close(t.ctx) }

// --- duckdb via database/sql ---

type stdTarget struct {
	db      *sql.DB
	insStmt *sql.Stmt
	getStmt *sql.Stmt
	updStmt *sql.Stmt
}

func (t *stdTarget) exec(q string) error { _, err := t.db.Exec(q); return err }

func (t *stdTarget) insertOne(id int, name string, amt float64) error {
	if t.insStmt == nil {
		s, err := t.db.Prepare("INSERT INTO t_point VALUES ($1, $2, $3)")
		if err != nil {
			return err
		}
		t.insStmt = s
	}
	_, err := t.insStmt.Exec(int64(id), name, amt)
	return err
}

func (t *stdTarget) insertBatch(q string) error { _, err := t.db.Exec(q); return err }

func (t *stdTarget) readOne(id int) error {
	if t.getStmt == nil {
		s, err := t.db.Prepare("SELECT name, amount FROM t_bulk WHERE id = $1")
		if err != nil {
			return err
		}
		t.getStmt = s
	}
	var name string
	var amt float64
	return t.getStmt.QueryRow(int64(id)).Scan(&name, &amt)
}

func (t *stdTarget) updateOne(id int, amt float64) error {
	if t.updStmt == nil {
		s, err := t.db.Prepare("UPDATE t_bulk SET amount = $1 WHERE id = $2")
		if err != nil {
			return err
		}
		t.updStmt = s
	}
	_, err := t.updStmt.Exec(amt, int64(id))
	return err
}

func (t *stdTarget) scanAgg() error {
	var n int64
	var s float64
	return t.db.QueryRow("SELECT count(*), sum(amount) FROM t_bulk").Scan(&n, &s)
}

func (t *stdTarget) close() {}

// --- redis (KV workloads only; no SQL surface) ---

func runRedis() (result, error) {
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer rdb.Close()
	if err := rdb.FlushAll(ctx).Err(); err != nil {
		return result{}, fmt.Errorf("redis flush: %w", err)
	}
	us := map[string]float64{}

	val := func(id int) string { return fmt.Sprintf("user-%d|%g", id, float64(id%1000)+0.25) }

	d, err := timeN(nPointInsert, func(i int) error {
		return rdb.Set(ctx, fmt.Sprintf("p:%d", i), val(i), 0).Err()
	})
	if err != nil {
		return result{}, err
	}
	us["insert_point"] = usPerOp(d, nPointInsert)

	start := time.Now()
	for base := 0; base < nBulkRows; base += bulkBatch {
		pipe := rdb.Pipeline()
		for i := 0; i < bulkBatch; i++ {
			pipe.Set(ctx, fmt.Sprintf("b:%d", base+i), val(base+i), 0)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return result{}, err
		}
	}
	us["insert_batch"] = usPerOp(time.Since(start), nBulkRows)

	rng := rand.New(rand.NewSource(42))
	ids := make([]int, nPointRead)
	for i := range ids {
		ids[i] = rng.Intn(nBulkRows)
	}
	d, err = timeN(nPointRead, func(i int) error {
		return rdb.Get(ctx, fmt.Sprintf("b:%d", ids[i])).Err()
	})
	if err != nil {
		return result{}, err
	}
	us["read_point"] = usPerOp(d, nPointRead)

	d, err = timeN(nPointUpdate, func(i int) error {
		return rdb.Set(ctx, fmt.Sprintf("b:%d", ids[i%nPointRead]), val(i), 0).Err()
	})
	if err != nil {
		return result{}, err
	}
	us["update_point"] = usPerOp(d, nPointUpdate)

	// scan_agg intentionally absent: no SQL surface to aggregate with.
	return result{Target: "redis", Note: "local server, default persistence (RDB), pipelined batch", MicrosOp: us}, nil
}
