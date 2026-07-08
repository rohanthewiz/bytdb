package sql

// Tests for expressions in UPDATE SET (SET age = age + 1) and for
// checked integer SUM overflow. Both close gaps noted during the
// reliability-hardening milestone: the SET clause previously accepted
// only literals/placeholders, and SUM silently wrapped at int64.

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

// execErr runs a statement that must fail and returns the error text.
func execErr(t *testing.T, d *DB, q string) string {
	t.Helper()
	if _, err := d.Exec(q); err != nil {
		return err.Error()
	}
	t.Fatalf("Exec(%q) succeeded; want error", q)
	return ""
}

func TestUpdateSetExpression(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, age int, name text, score float)`)
	exec(t, d, `insert into t values (1, 30, 'ada', 1.5), (2, 40, 'grace', 2.5)`)

	// Self-referential arithmetic reads the pre-update value of every
	// matched row.
	if res := exec(t, d, `update t set age = age + 1`); res.RowsAffected != 2 {
		t.Fatalf("affected %d; want 2", res.RowsAffected)
	}
	res := exec(t, d, `select age from t order by id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(31)}, {int64(41)}}) {
		t.Fatalf("age+1: %v", res.Rows)
	}

	// An expression referencing a different column, mixed with a plain
	// literal assignment in the same statement.
	exec(t, d, `update t set score = age * 2, name = 'x' where id = 1`)
	res = exec(t, d, `select score, name from t where id = 1`)
	if !reflect.DeepEqual(res.Rows, [][]any{{float64(62), "x"}}) {
		t.Fatalf("mixed set: %v", res.Rows)
	}

	// String expressions: concatenation and a function call.
	exec(t, d, `update t set name = name || '_v2' where id = 2`)
	exec(t, d, `update t set name = upper(name) where id = 2`)
	res = exec(t, d, `select name from t where id = 2`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"GRACE_V2"}}) {
		t.Fatalf("string exprs: %v", res.Rows)
	}

	// CASE evaluates per row against that row's values.
	exec(t, d, `update t set age = case when age > 35 then 100 else 0 end`)
	res = exec(t, d, `select age from t order by id`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(0)}, {int64(100)}}) {
		t.Fatalf("case: %v", res.Rows)
	}

	// A scalar subquery is an expression too.
	exec(t, d, `update t set age = (select max(age) from t) where id = 1`)
	res = exec(t, d, `select age from t where id = 1`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(100)}}) {
		t.Fatalf("subquery: %v", res.Rows)
	}
}

// TestUpdateSetExpressionSwap pins that all SET expressions of one
// statement evaluate against the pre-update row, so two columns can
// swap through each other without an intermediate clobber.
func TestUpdateSetExpressionSwap(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table p (id int primary key, a int, b int)`)
	exec(t, d, `insert into p values (1, 10, 20)`)
	exec(t, d, `update p set a = b, b = a`)
	res := exec(t, d, `select a, b from p`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(20), int64(10)}}) {
		t.Fatalf("swap: %v", res.Rows)
	}
}

// TestUpdateSetExpressionViaIndex re-runs the scan-the-index-you-
// mutate hazard with a genuine self-referential expression (the
// original test had to move rows with literals because expressions
// were unsupported).
func TestUpdateSetExpressionViaIndex(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, age int)`)
	exec(t, d, `create index by_age on t (age)`)
	exec(t, d, `insert into t values (1, 25), (2, 31), (3, 32), (4, 33), (5, 34), (6, 35)`)

	// Each matched row moves ahead of the scan window (+10 keeps it
	// > 30); a live index scan would meet and increment it again.
	q := `update t set age = age + 10 where age > 30`
	explainsAs(t, d, q, "Index Scan")
	if res := exec(t, d, q); res.RowsAffected != 5 {
		t.Fatalf("affected %d; want 5", res.RowsAffected)
	}
	res := exec(t, d, `select age from t order by age`)
	want := [][]any{{int64(25)}, {int64(41)}, {int64(42)}, {int64(43)}, {int64(44)}, {int64(45)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("exactly one increment per row: %v", res.Rows)
	}
	// The index agrees with the table: no stale pre-move entries.
	explainsAs(t, d, `select id from t where age >= 0`, "Index Scan")
	if res := exec(t, d, `select count(*) from t where age >= 0`); res.Rows[0][0] != int64(6) {
		t.Fatalf("index disagrees with table: %v", res.Rows)
	}
}

func TestUpdateSetExpressionChecksAndErrors(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table c (id int primary key, n int check (n > 0), s text)`)
	exec(t, d, `insert into c values (1, 5, 'a')`)

	// CHECK judges the evaluated result, not the expression.
	if msg := execErr(t, d, `update c set n = n - 10 where id = 1`); !strings.Contains(msg, "check constraint") {
		t.Fatalf("check violation not raised: %q", msg)
	}
	if res := exec(t, d, `select n from c`); res.Rows[0][0] != int64(5) {
		t.Fatalf("failed update mutated the row: %v", res.Rows)
	}
	exec(t, d, `update c set n = n + 10 where id = 1`)
	if res := exec(t, d, `select n from c`); res.Rows[0][0] != int64(15) {
		t.Fatalf("passing update: %v", res.Rows)
	}

	// Runtime evaluation errors abort the statement.
	if msg := execErr(t, d, `update c set n = n / 0`); !strings.Contains(msg, "division by zero") {
		t.Fatalf("want division by zero, got %q", msg)
	}
	if msg := execErr(t, d, `update c set n = s + 1`); msg == "" {
		t.Fatal("text + int should error")
	}

	// A string expression result coerces into the column like a quoted
	// literal ('1' || '0' -> text "10" -> int 10).
	exec(t, d, `update c set n = '1' || '0' where id = 1`)
	if res := exec(t, d, `select n from c`); res.Rows[0][0] != int64(10) {
		t.Fatalf("coerced concat: %v", res.Rows)
	}

	// Aggregates and window functions have no meaning in SET.
	if msg := execErr(t, d, `update c set n = max(n)`); !strings.Contains(msg, "aggregate") {
		t.Fatalf("aggregate in SET: %q", msg)
	}
	if msg := execErr(t, d, `update c set n = row_number() over ()`); !strings.Contains(msg, "window") {
		t.Fatalf("window in SET: %q", msg)
	}

	// Unknown columns fail whether the assignment is a literal or an
	// expression referencing one.
	if msg := execErr(t, d, `update c set nope = n + 1`); !strings.Contains(msg, "no such column") {
		t.Fatalf("unknown target column: %q", msg)
	}
	if msg := execErr(t, d, `update c set n = nope + 1`); msg == "" {
		t.Fatal("unknown source column should error")
	}
	if msg := execErr(t, d, `update c set n = n + 1, n = n + 2`); !strings.Contains(msg, "duplicate column") {
		t.Fatalf("duplicate SET column: %q", msg)
	}
}

// TestUpdateSetExpressionParams covers $n placeholders inside SET
// expressions through the session (prepared-statement) path, twice to
// prove the parsed statement re-binds cleanly.
func TestUpdateSetExpressionParams(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, age int)`)
	exec(t, d, `insert into t values (1, 30)`)
	s := d.NewSession()

	for i, want := range []int64{35, 40} {
		if _, err := s.Exec(`update t set age = age + $1 where id = $2`, int64(5), int64(1)); err != nil {
			t.Fatalf("bind %d: %v", i, err)
		}
		res := exec(t, d, `select age from t`)
		if res.Rows[0][0] != want {
			t.Fatalf("bind %d: age = %v; want %d", i, res.Rows[0][0], want)
		}
	}
}

// TestSumOverflow: an integer SUM whose true total leaves int64's
// range must error like scalar arithmetic ("bigint out of range"),
// not wrap silently. Float SUM and AVG never take that path.
func TestSumOverflow(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table s (id int primary key, n int, f float)`)
	exec(t, d, `insert into s values (1, 9223372036854775807, 1.0), (2, 1, 2.0)`)

	if msg := execErr(t, d, `select sum(n) from s`); !strings.Contains(msg, "bigint out of range") {
		t.Fatalf("sum overflow: %q", msg)
	}
	// The negative direction overflows too.
	exec(t, d, `update s set n = -9223372036854775807 where id = 1`)
	exec(t, d, `update s set n = -2 where id = 2`)
	if msg := execErr(t, d, `select sum(n) from s`); !strings.Contains(msg, "bigint out of range") {
		t.Fatalf("negative sum overflow: %q", msg)
	}

	// A sum that stays in range is unaffected.
	exec(t, d, `update s set n = 9223372036854775806 where id = 1`)
	exec(t, d, `update s set n = 1 where id = 2`)
	res := exec(t, d, `select sum(n) from s`)
	if res.Rows[0][0] != int64(math.MaxInt64) {
		t.Fatalf("exact-boundary sum: %v", res.Rows)
	}

	// AVG reads the float accumulator, so it survives inputs whose
	// integer sum would overflow — matching Postgres, where avg(bigint)
	// is numeric.
	exec(t, d, `update s set n = 9223372036854775807 where id = 1`)
	exec(t, d, `update s set n = 9223372036854775807 where id = 2`)
	res = exec(t, d, `select avg(n) from s`)
	if got, ok := res.Rows[0][0].(float64); !ok || got != float64(math.MaxInt64) {
		t.Fatalf("avg over big ints: %v", res.Rows)
	}
	// Float SUM likewise accumulates in float64.
	if res := exec(t, d, `select sum(f) from s`); res.Rows[0][0] != 3.0 {
		t.Fatalf("float sum: %v", res.Rows)
	}

	// The window-function SUM shares the accumulator and the check.
	if msg := execErr(t, d, `select sum(n) over () from s`); !strings.Contains(msg, "bigint out of range") {
		t.Fatalf("window sum overflow: %q", msg)
	}
}
