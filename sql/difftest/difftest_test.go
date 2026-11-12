// Package difftest runs a corpus of SQL against bytdb and a real
// Postgres side by side and reports every place their answers differ
// (deep-look plan D-02).
//
// Why a differential harness: the hand-pinned tests in sql/ check the
// divergences somebody already thought of. Running the same statements
// through a reference implementation finds the ones nobody did, and it
// turns every "Postgres gives X" claim in a plan or a doc into
// something a machine checks.
//
// Shape of a run:
//
//	corpus/*.sql ──parse──► entries ──┬──► bytdb (in-process pgwire server) ─┐
//	                                  └──► Postgres (BYTDB_DIFF_PG_URL)      ├─► normalize ─► compare
//	                                                                         ┘
//
// Both sides are driven through the same pgx client in the simple
// (text) protocol, so the comparison sees exactly what a client sees:
// the text rendering of every value plus each column's type OID. That
// also makes the harness catch result-typing bugs (a sum described as
// text) and not only wrong values.
//
// The test is skipped unless BYTDB_DIFF_PG_URL names a Postgres to
// compare against, so `go test ./...` stays hermetic. Each corpus file
// runs in a fresh bytdb database and a fresh Postgres schema that is
// dropped afterwards, so files cannot see each other's tables.
package difftest

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/pgwire"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

// envPG names the Postgres the corpus is compared against, as a pgx
// connection string. The harness creates and drops one schema per
// corpus file there and touches nothing else.
const envPG = "BYTDB_DIFF_PG_URL"

func TestCorpus(t *testing.T) {
	pgURL := os.Getenv(envPG)
	if pgURL == "" {
		t.Skipf("%s not set; skipping the Postgres differential corpus", envPG)
	}
	files, err := filepath.Glob(filepath.Join("corpus", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("empty corpus")
	}
	for _, f := range files {
		t.Run(strings.TrimSuffix(filepath.Base(f), ".sql"), func(t *testing.T) {
			runFile(t, f, pgURL)
		})
	}
}

// runFile executes one corpus file against both databases.
func runFile(t *testing.T, path, pgURL string) {
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := parseCorpus(string(src))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	ctx := context.Background()
	bc := startBytdb(t)
	pc := startPG(t, pgURL)

	for _, e := range entries {
		name := fmt.Sprintf("%s:%d", filepath.Base(path), e.line)
		bres := run(ctx, bc, e.sql)
		pres := run(ctx, pc, e.sql)
		if e.setup {
			// A setup statement that fails on either side makes every
			// later comparison meaningless; stop the file here.
			if bres.err != nil || pres.err != nil {
				t.Fatalf("%s setup %q\n  bytdb:    %v\n  postgres: %v", name, e.sql, bres.err, pres.err)
			}
			continue
		}
		diff := compare(bres, pres, e)
		switch {
		case e.expectDiff() && diff == "":
			// A known divergence that stopped diverging: the annotation
			// is now a lie, so make the author delete it. This is what
			// keeps the corpus an accurate list of open divergences.
			t.Errorf("%s %q matches Postgres now; remove its %s annotation (%s)",
				name, e.sql, e.annotationKind(), e.reason)
		case !e.expectDiff() && diff != "":
			t.Errorf("%s %q differs from Postgres:\n%s", name, e.sql, diff)
		case e.expectDiff():
			t.Logf("%s known divergence (%s: %s)", name, e.annotationKind(), e.reason)
		}
	}
}

// startBytdb serves a fresh bytdb database on a loopback port and
// returns a pgx connection to it, all torn down with the test.
func startBytdb(t *testing.T) *pgx.Conn {
	t.Helper()
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "diff.db"), bytdb.WithSyncNever())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := pgwire.NewServer(bsql.New(e))
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		e.Close()
	})
	return connect(t, fmt.Sprintf("postgres://difftest@%s/difftest", ln.Addr()))
}

// startPG connects to the reference Postgres and confines the
// connection to a new, uniquely named schema, dropped at cleanup.
// search_path makes unqualified names in the corpus land there.
func startPG(t *testing.T, url string) *pgx.Conn {
	t.Helper()
	c := connect(t, url)
	schema := fmt.Sprintf("difftest_%d_%d", time.Now().UnixNano(), rand.IntN(1_000_000))
	ctx := context.Background()
	for _, q := range []string{
		"CREATE SCHEMA " + schema,
		"SET search_path TO " + schema,
		// bytdb renders timestamps in UTC; make Postgres agree so a
		// timestamptz-typed expression (now()) compares by value.
		"SET TIME ZONE 'UTC'",
	} {
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatalf("postgres %q: %v", q, err)
		}
	}
	t.Cleanup(func() {
		_, _ = c.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	})
	return c
}

func connect(t *testing.T, url string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The simple protocol returns every value in text format, which is
	// the representation compared below, and keeps pgx from preparing
	// statements (so the corpus tests SQL, not plan caching).
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect %s: %v", url, err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// result is one statement's outcome on one side, already reduced to
// the comparable parts.
type result struct {
	err      error
	sqlstate string
	oids     []uint32
	rows     [][]*string // text values; nil is SQL NULL
	affected int64
	hasRows  bool // the statement returned a row description
}

// run executes one statement and captures its outcome. A statement
// timeout bounds every call: a hang on either side must surface as a
// failure, never a stuck CI job.
func run(ctx context.Context, c *pgx.Conn, q string) result {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := c.Query(ctx, q)
	if err != nil {
		return failed(err)
	}
	var r result
	fds := rows.FieldDescriptions()
	r.hasRows = len(fds) > 0
	for _, fd := range fds {
		r.oids = append(r.oids, fd.DataTypeOID)
	}
	for rows.Next() {
		raw := rows.RawValues()
		row := make([]*string, len(raw))
		for i, b := range raw {
			if b != nil {
				s := string(b)
				row[i] = &s
			}
		}
		r.rows = append(r.rows, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return failed(err)
	}
	r.affected = rows.CommandTag().RowsAffected()
	return r
}

func failed(err error) result {
	r := result{err: err}
	if pe, ok := err.(*pgconn.PgError); ok {
		r.sqlstate = pe.Code
	}
	return r
}

// compare reports how a bytdb result differs from the Postgres one, or
// "" when they agree under the entry's rules.
func compare(b, p result, e entry) string {
	if e.wantErr != "" || p.err != nil || b.err != nil {
		switch {
		case b.err != nil && p.err != nil:
			// Both refuse. The SQLSTATE is compared only when the entry
			// names one: bytdb's codes are a best-effort mapping, and an
			// error where Postgres errors is the property that matters
			// for correctness.
			if e.wantErr != "" && e.wantErr != "any" && b.sqlstate != e.wantErr {
				return fmt.Sprintf("  bytdb error %s (%v), want SQLSTATE %s (postgres: %s %v)",
					b.sqlstate, b.err, e.wantErr, p.sqlstate, p.err)
			}
			return ""
		case b.err != nil:
			return fmt.Sprintf("  bytdb error:  %v\n  postgres ok:  %s", b.err, render(p))
		case p.err != nil:
			return fmt.Sprintf("  bytdb ok:       %s\n  postgres error: %v", render(b), p.err)
		default:
			return fmt.Sprintf("  both succeeded; the entry expects an error (%s)", e.wantErr)
		}
	}
	if b.hasRows != p.hasRows {
		return fmt.Sprintf("  bytdb rows=%v, postgres rows=%v", b.hasRows, p.hasRows)
	}
	if !b.hasRows {
		if b.affected != p.affected {
			return fmt.Sprintf("  rows affected: bytdb %d, postgres %d", b.affected, p.affected)
		}
		return ""
	}
	if len(b.oids) != len(p.oids) {
		return fmt.Sprintf("  column count: bytdb %d, postgres %d", len(b.oids), len(p.oids))
	}
	classes := make([]typeClass, len(p.oids))
	for i := range p.oids {
		classes[i] = classOf(p.oids[i])
		if !e.noType && classOf(b.oids[i]) != classes[i] {
			return fmt.Sprintf("  column %d type: bytdb OID %d (%s), postgres OID %d (%s)",
				i+1, b.oids[i], classOf(b.oids[i]), p.oids[i], classes[i])
		}
	}
	for i := range p.oids {
		if p.oids[i] == 1700 && b.oids[i] == 1700 {
			classes[i] = clsNumericExact
		}
	}
	bk := keys(b.rows, classes)
	pk := keys(p.rows, classes)
	if !e.ordered {
		// Without ORDER BY a result is a multiset; compare it as one.
		slices.Sort(bk)
		slices.Sort(pk)
	}
	if !slices.Equal(bk, pk) {
		return fmt.Sprintf("  bytdb:    %s\n  postgres: %s", render(b), render(p))
	}
	return ""
}

// typeClass groups type OIDs whose values compare the same way. bytdb
// has no int4 or int2 and Postgres has no unsized "int" (sum(int)
// yields numeric there, int8 here), so exact OID equality would flag
// differences that are not bugs. A class mismatch (a number described
// as text) is a real client-visible bug and is reported.
//
// clsNumericExact is not an OID's class: compare assigns it to a column
// both sides type numeric (OID 1700), whose values must then agree as
// exact text — digits and display scale, 2.5000000000000000 vs 2.5 —
// rather than through float64 (N-048: bytdb has a real numeric now,
// and numeric's point is that its digits are exact).
type typeClass string

const (
	clsNumber       typeClass = "number"
	clsNumericExact typeClass = "numeric-exact"
	clsText         typeClass = "text"
	clsOther        typeClass = "other" // compared as exact text, class by OID
)

func classOf(oid uint32) typeClass {
	switch oid {
	case 20, 21, 23, 700, 701, 1700: // int8 int2 int4 float4 float8 numeric
		return clsNumber
	case 25, 1043, 1042, 19, 705: // text varchar bpchar name unknown
		return clsText
	}
	return typeClass(fmt.Sprintf("oid%d", oid))
}

// keys renders each row as one comparable string. Numbers are
// normalized through float64 (Postgres prints avg() as
// 2.5000000000000000 and bytdb as 2.5) at 12 significant digits, which
// absorbs float formatting noise without hiding a wrong answer.
// Everything else compares as exact text, which is how a client sees
// it.
func keys(rows [][]*string, classes []typeClass) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		var sb strings.Builder
		for j, v := range row {
			if j > 0 {
				sb.WriteString(" | ")
			}
			switch {
			case v == nil:
				sb.WriteString("NULL")
			case classes[j] == clsNumber:
				if f, err := strconv.ParseFloat(*v, 64); err == nil {
					sb.WriteString(normFloat(f))
				} else {
					sb.WriteString(*v) // NaN/Infinity spellings stay visible
				}
			default:
				sb.WriteString(strconv.Quote(*v))
			}
		}
		out[i] = sb.String()
	}
	return out
}

func normFloat(f float64) string {
	if f == 0 {
		return "0" // -0 and +0 compare equal in SQL
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatFloat(f, 'f', 0, 64)
	}
	return strconv.FormatFloat(f, 'g', 12, 64)
}

// render prints a result compactly for a failure message.
func render(r result) string {
	if r.err != nil {
		return "error: " + r.err.Error()
	}
	if !r.hasRows {
		return fmt.Sprintf("(%d rows affected)", r.affected)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("oids=%v ", r.oids))
	for i, row := range r.rows {
		if i == 8 {
			sb.WriteString(fmt.Sprintf(" … (%d rows)", len(r.rows)))
			break
		}
		sb.WriteString("(")
		for j, v := range row {
			if j > 0 {
				sb.WriteString(", ")
			}
			if v == nil {
				sb.WriteString("NULL")
			} else {
				sb.WriteString(strconv.Quote(*v))
			}
		}
		sb.WriteString(")")
	}
	if len(r.rows) == 0 {
		sb.WriteString("(no rows)")
	}
	return sb.String()
}
