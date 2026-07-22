package sql

// Regression tests for SUM over an expression whose STATIC type is int
// but whose RUNTIME value is float on some rows — e.g. a CASE whose
// branches mix int and float. The aggregate's intSum flag is set from
// the static type (the type of a CASE is its first branch), so before
// the fix the running total returned the int-only accumulator and
// silently dropped every float row. AVG always read the float
// accumulator, so it stayed correct — a divergence that made the SUM
// bug easy to miss.

import "testing"

// mixedSumRows: SUM(CASE WHEN id=1 THEN 1 ELSE 1.5 END) over ids 1..5
// is 1 + 1.5*4 = 7.0; the pre-fix code returned 1.
const mixedCase = `CASE WHEN id = 1 THEN 1 ELSE 1.5 END`

func TestSumMixedIntFloatExpr(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key)`)
	exec(t, d, `insert into t values (1), (2), (3), (4), (5)`)

	res, err := d.Exec(`select sum(` + mixedCase + `) from t`)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if got := res.Rows[0][0]; got != 7.0 {
		t.Errorf("sum(mixed) = %v (%T), want 7.0 — float rows were dropped", got, got)
	}

	// AVG reads the float accumulator and was always right; assert it
	// agrees so the two can't silently diverge again. 7.0 / 5 = 1.4.
	res, err = d.Exec(`select avg(` + mixedCase + `) from t`)
	if err != nil {
		t.Fatalf("avg: %v", err)
	}
	if got := res.Rows[0][0]; got != 1.4 {
		t.Errorf("avg(mixed) = %v, want 1.4", got)
	}
}

func TestSumMixedDistinct(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key)`)
	exec(t, d, `insert into t values (1), (2), (3)`)

	// DISTINCT over CASE-values {1 (id=1), 1.5 (id=2), 1.5 (id=3)} keeps
	// {1, 1.5} -> 2.5. Pre-fix returned 1 (the int-only slot).
	res, err := d.Exec(`select sum(distinct ` + mixedCase + `) from t`)
	if err != nil {
		t.Fatalf("sum distinct: %v", err)
	}
	if got := res.Rows[0][0]; got != 2.5 {
		t.Errorf("sum(distinct mixed) = %v, want 2.5", got)
	}
}

func TestSumMixedWindow(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key)`)
	exec(t, d, `insert into t values (1), (2)`)

	// The window aggregate reuses the same accumulator; over the whole
	// partition the running SUM is 1 + 1.5 = 2.5 for every row.
	res, err := d.Exec(`select sum(` + mixedCase + `) over () from t order by id`)
	if err != nil {
		t.Fatalf("window sum: %v", err)
	}
	for i, row := range res.Rows {
		if got := row[0]; got != 2.5 {
			t.Errorf("row %d: sum() over () = %v, want 2.5", i, got)
		}
	}
}

// TestSumPureIntUnchanged guards the fix's other half: a genuinely
// integer SUM must still return an exact int64 (not a float), preserving
// overflow checking and integer identity.
func TestSumPureIntUnchanged(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, n int)`)
	exec(t, d, `insert into t values (1, 10), (2, 20), (3, 30)`)

	res, err := d.Exec(`select sum(n) from t`)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	got := res.Rows[0][0]
	if v, ok := got.(int64); !ok || v != 60 {
		t.Errorf("sum(int) = %v (%T), want int64(60)", got, got)
	}
}
