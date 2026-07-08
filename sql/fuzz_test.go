package sql

import (
	"strings"
	"testing"
)

// FuzzParse asserts the parser's contract on hostile input: Parse
// always returns error-or-AST, never panics, and never overflows the
// stack (the maxParseDepth guard — a stack overflow is a fatal runtime
// error recover() cannot catch, so it would kill a whole server).
//
// Run with: go test -fuzz=FuzzParse ./sql
func FuzzParse(f *testing.F) {
	seeds := []string{
		// DDL across the grammar
		"CREATE TABLE users (id INT PRIMARY KEY, name TEXT NOT NULL, age INT, bal FLOAT DEFAULT 0, ok BOOL, CHECK (age > 0), UNIQUE (name))",
		"CREATE TABLE t (a INT, b TEXT, PRIMARY KEY (a, b))",
		"CREATE INDEX by_age ON public.users USING btree (age DESC, name ASC)",
		"CREATE UNIQUE INDEX u_name ON users (name)",
		"DROP TABLE IF EXISTS users",
		"DROP INDEX by_age",
		"ALTER TABLE t ADD COLUMN c FLOAT",
		"ALTER TABLE t DROP COLUMN c",
		"ALTER TABLE t ADD CHECK (a < 10)",
		// DML
		"INSERT INTO t (a, b, c, d) VALUES (1, -2.5, 'x', NULL), (2, 3, 'y', true)",
		"INSERT INTO t VALUES ($1, $2)",
		"UPDATE users SET city = 'Reno', age = age + 1 WHERE id = 7",
		"DELETE FROM users WHERE age NOT IN (1, 2, NULL)",
		// SELECT surface: joins, grouping, subqueries, windows, ANY/ALL
		"SELECT name, age FROM users WHERE city = 'london' ORDER BY age DESC NULLS FIRST LIMIT 10 OFFSET 2",
		"SELECT a.x, b.y FROM a JOIN b ON a.id = b.aid LEFT JOIN c ON b.id = c.bid WHERE a.x BETWEEN 1 AND 10",
		"SELECT city, COUNT(*), SUM(age) FROM users GROUP BY city HAVING COUNT(*) > 1",
		"SELECT * FROM t WHERE EXISTS (SELECT 1 FROM u WHERE u.id = t.id)",
		"SELECT * FROM t WHERE a = ANY (SELECT b FROM u) AND c >= ALL (SELECT d FROM v)",
		"SELECT rank() OVER (PARTITION BY city ORDER BY age DESC), row_number() OVER () FROM users",
		"SELECT CASE WHEN a > 0 THEN 'pos' WHEN a < 0 THEN 'neg' ELSE 'zero' END FROM t",
		"SELECT DISTINCT ON (a) a, b FROM t",
		"SELECT (SELECT max(x) FROM u) + 1, NOT (a AND b OR c) FROM t WHERE s LIKE 'a%' AND t IS NOT NULL",
		"SELECT -1 + 2 * 3 / 4 % 5, 'a' || 'b', a::int FROM t",
		// txn control and misc
		"BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY",
		"START TRANSACTION READ WRITE",
		"COMMIT", "ROLLBACK", "END", "ABORT",
		"SAVEPOINT sp1", "RELEASE SAVEPOINT sp1", "ROLLBACK TO SAVEPOINT sp1",
		"EXPLAIN (FORMAT TEXT, VERBOSE ON) SELECT * FROM t",
		"EXPLAIN ANALYZE SELECT 1",
		// malformed fragments: unterminated strings, dangling operators,
		// stray tokens, half-finished clauses
		"SELECT 'unterminated",
		"SELECT \"unterminated ident",
		"SELECT 1 +",
		"SELECT FROM WHERE",
		"INSERT INTO",
		"CREATE TABLE t (",
		"SELECT * FROM t WHERE a = ;",
		"SELECT 1e999999",
		"SELECT $0, $99999999999999999999",
		"SELECT literally\x00null bytes",
		";;;",
		"",
		// deep nesting: past-the-guard depth must be a normal error
		strings.Repeat("(", 2000) + "1" + strings.Repeat(")", 2000),
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		// Fuzzing catches panics and (fatally) stack overflows; there
		// is nothing to assert about the result beyond error-or-AST.
		st, err := Parse(src)
		if err == nil && st == nil {
			t.Fatalf("Parse(%q) returned nil statement and nil error", src)
		}
	})
}
