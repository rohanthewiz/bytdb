package sql

// FuzzExec is the executor's counterpart to FuzzParse. FuzzParse proves
// Parse never panics on hostile text; this proves the whole pipeline
// beneath it — bind, literal coercion, planning, and evaluation —
// never panics on a query the parser ACCEPTS. A parse failure is a
// clean error to a client; an executor panic on a well-formed query is
// a process-level fault (it escapes the SQL layer entirely), so this is
// the higher-severity surface to fuzz.
//
// Cardinality is deliberately pinned to one row per seed table. A SQL
// string can express an unbounded cross join (FROM t,t,t,...); with a
// one-row table every such product is still one row, so the fuzzer can
// range freely over join/aggregate/window/order/distinct shapes without
// a crafted input exhausting memory. Multi-row executor correctness is
// covered by the deterministic unit tests; the fuzzer's job here is
// crash-resistance across the plan/eval surface, which one-row inputs
// exercise structurally. A short ExecCtx deadline is a second guard
// against any accidental long-running loop.
//
// The engine is shared across executions (fast, matches the pgwire fuzz
// target). It is therefore stateful: an input that mutates schema/rows
// changes what later inputs see. That is acceptable for panic-hunting —
// a reproducer replays against the same seeded schema — and DDL/DML
// simply widens the executor surface the fuzzer reaches.
//
// Run with: go test -fuzz=FuzzExec ./sql

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rohanthewiz/bytdb"
)

func FuzzExec(f *testing.F) {
	e, err := bytdb.Open(filepath.Join(f.TempDir(), "fuzzexec.db"))
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { e.Close() })
	db := New(e)

	// One row per table: enough schema to bind names and types across
	// every column kind, bounded enough that cross joins can't explode.
	setup := db.NewSession()
	for _, q := range []string{
		`CREATE TABLE t (id INT PRIMARY KEY, name TEXT, age INT, bal FLOAT,
		                 ok BOOL, born DATE, at TIMESTAMPTZ, tags TEXT[], meta JSONB)`,
		`CREATE TABLE u (id INT PRIMARY KEY, tid INT, label TEXT)`,
		`CREATE INDEX t_age ON t (age)`,
		`INSERT INTO t VALUES (1, 'ada', 36, 1.5, true, '2024-01-02',
		                       '2024-01-02 03:04:05+00', '{a,b}', '{"role":"admin"}')`,
		`INSERT INTO u VALUES (1, 1, 'x')`,
	} {
		if _, err := setup.Exec(q); err != nil {
			f.Fatalf("seed %q: %v", q, err)
		}
	}
	setup.Close()

	// Seeds: queries that reach the executor's sharp edges — division and
	// modulo by zero, aggregates and windows over a possibly-empty frame,
	// NULL propagation, casts, jsonb operators, joins of every accepted
	// shape, plus the DML/DDL/txn verbs. Malformed fragments are included
	// so the fuzzer also starts from the parse-error boundary.
	seeds := []string{
		`SELECT 1/0`,
		`SELECT 1 % 0`,
		`SELECT 1.0 / 0.0`,
		`SELECT NULL + 1, NULL AND true, NULL = NULL`,
		`SELECT max(age), min(age), sum(bal), avg(age), count(*) FROM t WHERE false`,
		`SELECT age, sum(age) OVER (ORDER BY age ROWS BETWEEN 100 PRECEDING AND 100 FOLLOWING) FROM t`,
		`SELECT row_number() OVER (), rank() OVER (ORDER BY age), lag(name, 5) OVER () FROM t`,
		`SELECT CASE WHEN age > 0 THEN 'p' WHEN age < 0 THEN 'n' ELSE 'z' END FROM t`,
		`SELECT age::text, name::int, bal::int, '2024-01-02'::date FROM t`,
		`SELECT meta->>'role', meta->'role', meta @> '{"role":"admin"}', tags[1] FROM t`,
		`SELECT t.name, u.label FROM t JOIN u ON u.tid = t.id`,
		`SELECT t.name FROM t LEFT JOIN u ON u.tid = t.id`,
		`SELECT a.id FROM t a JOIN t b ON a.id = b.id JOIN t c ON b.id = c.id`,
		`SELECT count(*) FROM t GROUP BY city HAVING count(*) > 0`,
		`SELECT DISTINCT name FROM t ORDER BY name DESC NULLS FIRST LIMIT 1 OFFSET 0`,
		`SELECT * FROM t WHERE age BETWEEN 1 AND 100 AND name LIKE 'a%' AND age IN (36, 45)`,
		`SELECT * FROM t WHERE EXISTS (SELECT 1 FROM u WHERE u.tid = t.id)`,
		`SELECT (SELECT max(age) FROM t) + 1`,
		`SELECT name FROM t UNION SELECT label FROM u`,
		`WITH c AS (SELECT id FROM t) SELECT * FROM c`,
		`INSERT INTO t (id) VALUES (2) ON CONFLICT (id) DO UPDATE SET name = 'z'`,
		`UPDATE t SET age = age + 1 WHERE id = 1`,
		`DELETE FROM t WHERE age > 1000`,
		`SELECT abs(-1), length('x'), upper('a'), coalesce(NULL, 1), nullif(1, 1)`,
		`SELECT * FROM t WHERE $1 = age`,
		`EXPLAIN SELECT * FROM t WHERE age = 1`,
		`BEGIN`, `COMMIT`, `ROLLBACK`, `SAVEPOINT s1`,
		`SELECT '' , -0.0, 9223372036854775807 + 1`,
		// malformed / hostile fragments (parse-error boundary)
		``, `SELECT`, `SELECT * FROM`, `SELECT 1 +`, `SELECT $0`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, src string) {
		if len(src) > 1<<14 {
			t.Skip("cap per-exec work; parse/plan cost is length-driven past this")
		}
		// Fresh session per execution; deferred Close rolls back any
		// transaction the input opened, releasing the single-writer lock
		// so the next execution can proceed.
		sess := db.NewSession()
		defer sess.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// One placeholder value so $1 queries reach evaluation rather than
		// failing at bind. Result and error are both irrelevant — only a
		// panic (which the fuzzer reports) is a failure.
		_, _ = sess.ExecCtx(ctx, src, int64(1))
	})
}
