package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runShell drives the whole command — flag parsing, batch input, exit
// status — the way a script would.
func runShell(t *testing.T, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(args, strings.NewReader(stdin), &out, &errb)
	return out.String(), errb.String(), code
}

func mustContain(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q:\n%s", w, got)
		}
	}
}

func TestBatchBasics(t *testing.T) {
	db := filepath.Join(t.TempDir(), "app.bytdb")
	out, errOut, code := runShell(t, `
CREATE TABLE users (id serial PRIMARY KEY, name text, city text);
INSERT INTO users (name, city) VALUES ('ada', 'london'), ('grace', 'nyc');
-- comments and blank lines are fine

SELECT name
  FROM users
 WHERE city = 'nyc';
SELECT count(*) AS n FROM users`, "-csv", db) // last statement has no ';'
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if want := "name\ngrace\nn\n2\n"; out != want {
		t.Errorf("stdout %q, want %q", out, want)
	}

	// The data persisted: a second run over the same file sees it, and
	// options may follow the file name (DuckDB argument order).
	out, _, code = runShell(t, "", db, "-json", "-c", "SELECT id, name FROM users ORDER BY id")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if want := "[{\"id\":1,\"name\":\"ada\"},\n{\"id\":2,\"name\":\"grace\"}]\n"; out != want {
		t.Errorf("json %q, want %q", out, want)
	}
}

func TestPositionalCommandsAndErrors(t *testing.T) {
	db := filepath.Join(t.TempDir(), "e.bytdb")
	// Positional commands after the file run in order; the failing one
	// stops the rest (-c semantics) and sets exit status 1.
	out, errOut, code := runShell(t, "", "-list", "-noheader", db,
		"CREATE TABLE t (id int PRIMARY KEY)",
		"INSERT INTO t VALUES (1)",
		"SELECT * FROM t WHERE",
		"SELECT 'unreached'")
	if code != 1 {
		t.Errorf("code %d, want 1", code)
	}
	if strings.Contains(out, "unreached") {
		t.Errorf("ran past the failure: %q", out)
	}
	mustContain(t, errOut, "Error: syntax error", "LINE 1: SELECT * FROM t WHERE", "^")

	// Without -bail, a script keeps going past an error but still exits 1.
	out, errOut, code = runShell(t, "SELECT * FROM missing;\nSELECT 7;\n", "-list", "-noheader", db)
	if code != 1 || out != "7\n" {
		t.Errorf("code %d out %q, want 1 and 7", code, out)
	}
	mustContain(t, errOut, "Error:")

	// With -bail the script stops at the first error.
	out, _, code = runShell(t, "SELECT * FROM missing;\nSELECT 7;\n", "-bail", "-list", db)
	if code != 1 || out != "" {
		t.Errorf("bail: code %d out %q", code, out)
	}
}

func TestTransactionsAcrossStatements(t *testing.T) {
	out, errOut, code := runShell(t, `
CREATE TABLE t (id int PRIMARY KEY);
BEGIN;
INSERT INTO t VALUES (1);
INSERT INTO t VALUES (2);
ROLLBACK;
BEGIN;
INSERT INTO t VALUES (3);
COMMIT;
SELECT id FROM t;
`, "-list", "-noheader")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d stderr %q", code, errOut)
	}
	if out != "3\n" {
		t.Errorf("out %q, want 3", out)
	}
}

func TestShorthandsAndPsqlAliases(t *testing.T) {
	out, errOut, code := runShell(t, `
CREATE TABLE "Mixed" (id int PRIMARY KEY, label varchar(10) NOT NULL DEFAULT 'x');
CREATE TABLE plain (id serial PRIMARY KEY, mixed_id int REFERENCES "Mixed" (id));
CREATE INDEX by_label ON "Mixed" (label DESC);
DESCRIBE "Mixed";
SHOW TABLES;
\dt
\d plain
\di
.mode list
\x
SELECT 1 AS one, 2 AS two;
\x
SELECT 3 AS three;
`)
	if code != 0 || errOut != "" {
		t.Fatalf("code %d stderr %q", code, errOut)
	}
	mustContain(t, out,
		"varchar(10)", "'x'", "PRI", // DESCRIBE "Mixed"
		"Mixed\nplain\n",         // SHOW TABLES / \dt, one per line off a terminal
		"identity",               // \d plain
		"by_label", "label DESC", // \di
		"one = 1\ntwo = 2\n", // \x on: line mode
		"three\n3\n",         // \x off: back to list
	)
}

func TestSchemaAndDumpRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src.bytdb"), filepath.Join(dir, "dst.bytdb")
	_, errOut, code := runShell(t, `
CREATE SEQUENCE order_no START 100 INCREMENT 5;
SELECT nextval('order_no');
CREATE TABLE "Customer" ("Id" int PRIMARY KEY, "full name" text NOT NULL, email varchar(50) UNIQUE,
  created timestamp DEFAULT now(), CONSTRAINT name_len CHECK (length("full name") > 0));
CREATE TABLE orders (id serial PRIMARY KEY, cust int REFERENCES "Customer" ("Id") ON DELETE CASCADE,
  total double precision, items text[], meta jsonb, ts timestamp, d date, u uuid, raw bytea, paid boolean);
CREATE INDEX by_total ON orders (total DESC, id);
CREATE TABLE tree (id int PRIMARY KEY, parent int REFERENCES tree (id));
INSERT INTO "Customer" VALUES (1, 'Ada O''Neil', 'ada@x.io', '2024-01-01 00:00:00'),
  (2, 'back\slash
newline', NULL, NULL);
INSERT INTO orders (cust, total, items, meta, ts, d, u, raw, paid) VALUES
  (1, 12.5, '{x,"y z",NULL}', '{"a": {"b": [1, "two"]}}', '2024-02-03 04:05:06.789012', '2024-02-03',
   'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', '\xdeadbeef', true),
  (2, 1e20, '{}', 'null', NULL, NULL, NULL, '\x', false),
  (1, -0.000001234, NULL, '"str"', NULL, NULL, NULL, NULL, NULL);
INSERT INTO tree VALUES (1, NULL), (2, 1), (3, 2);
INSERT INTO orders (cust) VALUES (2);
DELETE FROM orders WHERE id = 4;
CREATE VIEW big_orders AS SELECT * FROM orders WHERE total > 10;
CREATE VIEW a_view AS SELECT id FROM big_orders;
`, "-batch", src)
	if code != 0 {
		t.Fatalf("setup: code %d stderr %s", code, errOut)
	}

	dump, errOut, code := runShell(t, "", src, ".dump")
	if code != 0 {
		t.Fatalf("dump: code %d stderr %s", code, errOut)
	}
	// Foreign keys come after the data, views after their dependencies.
	if i, j := strings.Index(dump, "INSERT INTO tree"), strings.Index(dump, "ADD CONSTRAINT tree_parent_fkey"); i < 0 || j < i {
		t.Errorf("foreign key should follow the rows it validates:\n%s", dump)
	}
	if i, j := strings.Index(dump, "CREATE VIEW big_orders"), strings.Index(dump, "CREATE VIEW a_view"); i < 0 || j < i {
		t.Errorf("a_view depends on big_orders and must follow it:\n%s", dump)
	}
	// The identity counter rides along after the rows (N-032). id 4 was
	// drawn and deleted, so the counter (5) is ahead of the largest
	// stored id (3) and only the RESTART line can carry it.
	restart := "ALTER TABLE orders ALTER COLUMN id RESTART WITH 5;"
	if i, j := strings.LastIndex(dump, "INSERT INTO orders"), strings.Index(dump, restart); j < 0 || j < i {
		t.Errorf("want %q after the orders rows:\n%s", restart, dump)
	}
	// Tables without an identity column get no RESTART line.
	if n := strings.Count(dump, "RESTART"); n != 1 {
		t.Errorf("%d RESTART lines, want 1:\n%s", n, dump)
	}

	if _, errOut, code = runShell(t, dump, "-batch", dst); code != 0 {
		t.Fatalf("restore: code %d stderr %s", code, errOut)
	}
	again, _, _ := runShell(t, "", dst, ".dump")
	if again != dump {
		t.Errorf("dump of the restored database differs.\nfirst:\n%s\nsecond:\n%s", dump, again)
	}

	// The restored database behaves like the original: the sequence
	// continues, the identity counter continues past the deleted 4
	// rather than re-issuing it, and cascades fire.
	out, errOut, code := runShell(t, "", "-list", "-noheader", dst,
		"SELECT nextval('order_no')",
		"INSERT INTO orders (cust) VALUES (2) RETURNING id",
		`DELETE FROM "Customer" WHERE "Id" = 1`,
		"SELECT count(*) FROM orders")
	if code != 0 {
		t.Fatalf("code %d stderr %s", code, errOut)
	}
	if want := "105\n5\n2\n"; out != want {
		t.Errorf("restored behavior %q, want %q", out, want)
	}

	schema, _, _ := runShell(t, "", src, ".schema orders")
	mustContain(t, schema, "CREATE TABLE orders (", "id serial,", "CONSTRAINT orders_cust_fkey FOREIGN KEY (cust) REFERENCES \"Customer\" (\"Id\") ON DELETE CASCADE",
		"CREATE INDEX by_total ON orders (total DESC, id);")
}

func TestImport(t *testing.T) {
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "people.csv")
	os.WriteFile(csvPath, []byte("\ufeffName,zip,age,score,joined,active,born,notes\n"+
		"ada,02134,36,1.5,2024-01-02 03:04:05,true,1990-01-01,\"has, comma\"\n"+
		"grace,10001,45,2,2023-05-06 07:08:09,false,1980-12-31,\n"), 0o644)
	badPath := filepath.Join(dir, "bad.csv")
	os.WriteFile(badPath, []byte("id,n\n3,30\n4,not-a-number\n"), 0o644)
	raggedPath := filepath.Join(dir, "ragged.csv")
	os.WriteFile(raggedPath, []byte("a,b\n1,2\n3\n"), 0o644)
	idPath := filepath.Join(dir, "ids.csv")
	os.WriteFile(idPath, []byte("id,n\n1,10\n2,20\n"), 0o644)

	out, errOut, code := runShell(t, `
.import `+csvPath+` people
.import `+csvPath+` people
.mode list
SELECT rowid, name, zip, age, score, active, born, notes FROM people ORDER BY rowid;
.mode duckbox
DESCRIBE people;
.import `+idPath+` nums
.import `+badPath+` nums
.mode list
SELECT 'count=' || count(*) FROM nums;
.import `+raggedPath+` fresh
.tables
`)
	if code != 1 { // the two bad imports fail
		t.Errorf("code %d, want 1", code)
	}
	mustContain(t, out,
		"imported 2 rows into people (created; primary key rowid, added)",
		"imported 2 rows into people\n", // second run: header row recognized, not data
		"1|ada|02134|36|1.5|true|1990-01-01|has, comma\n",
		"4|grace|10001|45|2|false|1980-12-31|\n",
		"imported 2 rows into nums (created; primary key id)",
	)
	for _, typ := range []string{"bigint", "double precision", "boolean", "date", "timestamp"} {
		mustContain(t, out, typ)
	}
	mustContain(t, errOut,
		"line 3:",                  // the not-a-number row, by file line
		"line 3: 1 fields, want 2", // the ragged row
	)
	// The failed import into nums rolled back whole: still 2 rows.
	mustContain(t, out, "count=2\n")
	// The failed import that created "fresh" dropped it again.
	if strings.Contains(out, "fresh") {
		t.Errorf("failed import left table fresh behind:\n%s", out)
	}
}

func TestOutputRedirectAndRead(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "out.csv")
	onceFile := filepath.Join(dir, "once.txt")
	script := filepath.Join(dir, "script.sql")
	os.WriteFile(script, []byte("SELECT 'from script' AS s;\n.read "+script+"\n"), 0o644)

	out, errOut, _ := runShell(t, `
.mode csv
.output `+outFile+`
SELECT 1 AS a;
.output
.once `+onceFile+`
SELECT 2 AS b;
SELECT 3 AS c;
.mode list
.headers off
.read `+script+`
`)
	if b, _ := os.ReadFile(outFile); string(b) != "a\n1\n" {
		t.Errorf(".output file %q", b)
	}
	if b, _ := os.ReadFile(onceFile); string(b) != "b\n2\n" {
		t.Errorf(".once file %q", b)
	}
	mustContain(t, out, "c\n3\n", "from script\n")
	// The script reads itself; the nesting bound stops it.
	mustContain(t, errOut, ".read nested more than 16 deep")
}

func TestTransientAndPrompt(t *testing.T) {
	s := newShell(&bytes.Buffer{}, &bytes.Buffer{})
	if err := s.open(""); err != nil {
		t.Fatal(err)
	}
	dir := s.tempDir
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("transient dir missing: %v", err)
	}
	if p := s.currentPrompt(false); p != "bytdb> " {
		t.Errorf("idle prompt %q", p)
	}
	s.execStatement("BEGIN")
	if p := s.currentPrompt(false); p != "bytdb*> " {
		t.Errorf("in-block prompt %q", p)
	}
	s.execStatement("SELECT * FROM nope")
	if p := s.currentPrompt(false); p != "bytdb!> " {
		t.Errorf("failed-block prompt %q", p)
	}
	if p := s.currentPrompt(true); p != "  ...> " {
		t.Errorf("continuation prompt %q", p)
	}
	s.close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("transient dir %s not removed (err %v)", dir, err)
	}
}

func TestLockedDatabase(t *testing.T) {
	db := filepath.Join(t.TempDir(), "locked.bytdb")
	s := newShell(&bytes.Buffer{}, &bytes.Buffer{})
	if err := s.open(db); err != nil {
		t.Fatal(err)
	}
	defer s.close()
	_, errOut, code := runShell(t, "", db, "SELECT 1")
	if code != 1 {
		t.Errorf("code %d, want 1", code)
	}
	mustContain(t, errOut, "locked by another process", "psql")
}

func TestCompletion(t *testing.T) {
	s := newShell(&bytes.Buffer{}, &bytes.Buffer{})
	if err := s.open(""); err != nil {
		t.Fatal(err)
	}
	defer s.close()
	s.execStatement("CREATE TABLE customers (id int PRIMARY KEY, city text)")

	_, c, _ := s.complete(".ta", 3)
	if len(c) != 1 || c[0] != ".tables" {
		t.Errorf(".ta → %v", c)
	}
	_, c, _ = s.complete("SELECT * FROM cus", 17)
	if len(c) != 1 || c[0] != "customers" {
		t.Errorf("cus → %v", c)
	}
	_, c, _ = s.complete("SEL", 3)
	if len(c) != 1 || c[0] != "SELECT" {
		t.Errorf("SEL → %v", c)
	}
	_, c, _ = s.complete("sel", 3)
	if len(c) != 1 || c[0] != "select" {
		t.Errorf("sel → %v", c)
	}
	_, c, _ = s.complete("select customers.ci", 19)
	if len(c) != 1 || c[0] != "customers.city" {
		t.Errorf("customers.ci → %v", c)
	}
}

// TestChangesTags pins which statements print a command tag under
// .changes: psql's rule — tags for row-less statements, and for DML
// with RETURNING, but none after a SELECT's or EXPLAIN's rows.
func TestChangesTags(t *testing.T) {
	out, errOut, code := runShell(t, `
.changes on
.mode list
.headers off
CREATE TABLE t (id int PRIMARY KEY);
INSERT INTO t VALUES (1), (2);
INSERT INTO t VALUES (3) RETURNING id;
SELECT count(*) FROM t;
EXPLAIN SELECT * FROM t WHERE id = 1;
UPDATE t SET id = id + 10 WHERE id > 1;
DELETE FROM t;
`)
	if code != 0 || errOut != "" {
		t.Fatalf("code %d stderr %q", code, errOut)
	}
	want := "CREATE TABLE\nINSERT 0 2\n3\nINSERT 0 1\n3\nPoint Get on t\n  Key: (id = 1)\nUPDATE 2\nDELETE 3\n"
	if out != want {
		t.Errorf("out\n%q\nwant\n%q", out, want)
	}
}

// TestOnceCoversDotCommands checks .once redirects the next command's
// output even when that command is a dot command, then disarms.
func TestOnceCoversDotCommands(t *testing.T) {
	f := filepath.Join(t.TempDir(), "schema.sql")
	out, _, _ := runShell(t, `
CREATE TABLE t (id int PRIMARY KEY);
.once `+f+`
.schema
.tables
`)
	if b, _ := os.ReadFile(f); !strings.Contains(string(b), "CREATE TABLE t (") {
		t.Errorf(".once file %q", b)
	}
	if out != "t\n" {
		t.Errorf("stdout %q: .tables should print to stdout after .once fired", out)
	}
}

// TestDumpNumeric (N-048): numeric columns dump with their typmod and
// DEFAULT, values dump as exact unquoted literals (trailing zeros and
// all), and the restored database dumps identically.
func TestDumpNumeric(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src.bytdb"), filepath.Join(dir, "dst.bytdb")
	if _, errOut, code := runShell(t, `
CREATE TABLE price (sku numeric PRIMARY KEY, amt numeric(10,2) NOT NULL DEFAULT 0.00 CHECK (amt >= 0), rate numeric(5,-1));
INSERT INTO price VALUES (1.50, 19.999, 12345), (-0.001, 0, NULL), (123456789012345678901234567890.5, 3, 4);
INSERT INTO price (sku) VALUES (7);
`, "-batch", src); code != 0 {
		t.Fatalf("setup: code %d stderr %s", code, errOut)
	}
	dump, errOut, code := runShell(t, "", src, ".dump")
	if code != 0 {
		t.Fatalf("dump: code %d stderr %s", code, errOut)
	}
	mustContain(t, dump, "sku numeric", "amt numeric(10,2) NOT NULL DEFAULT 0.00", "rate numeric(5,-1)",
		"1.50, 20.00, 12350", "-0.001, 0.00, NULL", "123456789012345678901234567890.5, 3.00, 0", "7, 0.00, NULL")
	if _, errOut, code = runShell(t, dump, "-batch", dst); code != 0 {
		t.Fatalf("restore: code %d stderr %s", code, errOut)
	}
	if again, _, _ := runShell(t, "", dst, ".dump"); again != dump {
		t.Errorf("dump of the restored database differs.\nfirst:\n%s\nsecond:\n%s", dump, again)
	}
	out, errOut, code := runShell(t, "", "-list", "-noheader", dst,
		"SELECT sum(amt), avg(amt) FROM price", "SELECT sku FROM price WHERE sku = 1.5")
	if code != 0 {
		t.Fatalf("code %d stderr %s", code, errOut)
	}
	if want := "23.00|5.7500000000000000\n1.50\n"; out != want {
		t.Errorf("restored values %q, want %q", out, want)
	}
}
