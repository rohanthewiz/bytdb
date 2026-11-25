package sql

// N-053: money through the SQL layer, for what the difftest corpus
// (corpus/82_money.sql, checked against Postgres 16) does not reach:
// the assignment rules — an integer or numeric assigned to money is
// whole units, a float is refused — across VALUES, INSERT ... SELECT,
// UPDATE, DEFAULT and Go parameters, plus result typing and EXPLAIN.

import (
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// moneyCells renders a result's money cells (int64 cents) as text.
func moneyCells(t *testing.T, r *Result) string {
	t.Helper()
	var rows []string
	for _, row := range r.Rows {
		var cells []string
		for i, v := range row {
			switch {
			case v == nil:
				cells = append(cells, "NULL")
			case r.Types[i] == bytdb.TMoney:
				cells = append(cells, bytdb.FormatMoney(v.(int64)))
			default:
				cells = append(cells, valText(v))
			}
		}
		rows = append(rows, strings.Join(cells, " "))
	}
	return strings.Join(rows, " | ")
}

func TestMoneyAssignment(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table m (id int primary key, a money, b money default 3)`)
	exec(t, d, `insert into m values (1, 5, 1.255), (2, -2, '7'), (3, '$1,000.10', -0.005)`)
	exec(t, d, `insert into m (id, a) select 4, 12`)
	exec(t, d, `insert into m (id, a) select 5, 0.5`)
	exec(t, d, `update m set b = 9 where id = 4`)
	exec(t, d, `update m set a = a * 2 where id = 1`)
	if _, err := d.Exec(`insert into m (id, a) values ($1, $2)`, 6, int64(4)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`insert into m (id, a) values ($1, $2)`, 7, "$0.99"); err != nil {
		t.Fatal(err)
	}
	res := exec(t, d, `select id, a, b from m order by id`)
	want := "1 $10.00 $1.26 | 2 -$2.00 $7.00 | 3 $1,000.10 -$0.01 | 4 $12.00 $9.00 | 5 $0.50 $3.00 | 6 $4.00 $3.00 | 7 $0.99 $3.00"
	if got := moneyCells(t, res); got != want {
		t.Errorf("rows = %q\nwant   %q", got, want)
	}
	// Postgres has no float8 -> money assignment.
	for _, q := range []string{
		`insert into m (id, a) values (8, 1.5::float8)`,
		`insert into m (id, a) select 9, 1.5::float8`,
		`update m set a = 2.5::float8 where id = 1`,
	} {
		if _, err := d.Exec(q); err == nil || !strings.Contains(err.Error(), "is of type money but expression is of type double precision") {
			t.Errorf("%s: %v", q, err)
		}
	}
}

func TestMoneyTyping(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table m (id int primary key, a money)`)
	exec(t, d, `insert into m values (1, '10'), (2, '0.07')`)
	res := exec(t, d, `select a + a, a * 1.5, a / 2, a / '3', sum(a) over (), cash_words(a) from m order by id`)
	wantTypes := []bytdb.ColType{bytdb.TMoney, bytdb.TMoney, bytdb.TMoney, bytdb.TFloat, bytdb.TMoney, bytdb.TString}
	for i, typ := range res.Types {
		if typ != wantTypes[i] {
			t.Errorf("column %d typed %q, want %q", i, typ, wantTypes[i])
		}
	}
	want := "$20.00 $15.00 $5.00 3.3333333333333335 $10.07 Ten dollars and zero cents | $0.14 $0.10 $0.03 0.023333333333333334 $10.07 Zero dollars and seven cents"
	if got := moneyCells(t, res); got != want {
		t.Errorf("rows = %q\nwant   %q", got, want)
	}
	res = exec(t, d, `explain select id from m where a > '$1.00'`)
	var plan strings.Builder
	for _, row := range res.Rows {
		plan.WriteString(row[0].(string) + "\n")
	}
	if !strings.Contains(plan.String(), "'$1.00'::money") {
		t.Errorf("explain does not render the money literal:\n%s", plan.String())
	}
}
