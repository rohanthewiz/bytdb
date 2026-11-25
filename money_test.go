package bytdb_test

// N-053: the money column type at the engine level — the Go input
// shapes (an integer is cents, a Decimal is units, text is money text),
// a money key, the automatic ALTER casts to and from numeric and from
// int, and a DEFAULT carried across them. Postgres's answers for the
// money arithmetic and text are in money_golden_test.go.

import (
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func TestMoneyColumn(t *testing.T) {
	e := openTestEngine(t)
	if _, err := e.CreateTable("acct", []bytdb.Column{
		{Name: "id", Type: bytdb.TInt},
		{Name: "bal", Type: bytdb.TMoney},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		in   any
		want int64
	}{
		{int64(12345), 12345}, // cents
		{7, 7},
		{dec("12.345"), 1235}, // units, rounded half away from zero
		{dec("-0.005"), -1},
		{"$1,234.56", 123456},
		{"(5)", -500},
		{[]byte("0.1"), 10},
	}
	for i, c := range cases {
		if err := e.Insert("acct", i, c.in); err != nil {
			t.Fatalf("insert %v: %v", c.in, err)
		}
		r, _, err := e.Get("acct", i)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.Col("bal"); got != c.want {
			t.Errorf("%v: stored %v, want %d cents", c.in, got, c.want)
		}
	}
	// A float has no exact money value; Postgres has no float8 -> money
	// cast either.
	if err := e.Insert("acct", 100, 1.5); err == nil {
		t.Error("a float64 was accepted into a money column")
	}
	if err := e.Insert("acct", 101, "1.2.3"); err == nil ||
		!strings.Contains(err.Error(), `invalid input syntax for type money: "1.2.3"`) {
		t.Errorf("bad text: %v", err)
	}
}

func TestMoneyKeysAlterDefault(t *testing.T) {
	e := openTestEngine(t)
	if _, err := e.CreateTable("p", []bytdb.Column{
		{Name: "amt", Type: bytdb.TMoney},
		{Name: "n", Type: bytdb.TInt},
	}, "amt"); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"10", "-0.01", "0", "99.999"} {
		if err := e.Insert("p", s, nil); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for r, err := range e.Scan("p") {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, bytdb.FormatMoney(r.Col("amt").(int64)))
	}
	if strings.Join(got, " ") != "-$0.01 $0.00 $10.00 $100.00" {
		t.Errorf("key order = %v", got)
	}
	// money -> numeric and back are automatic, as in Postgres.
	if err := e.AlterColumnType("p", "amt", bytdb.ColumnTypeChange{Type: bytdb.TNumeric}); err != nil {
		t.Fatal(err)
	}
	r, _, _ := e.Get("p", dec("-0.01"))
	if d, _ := r.Decimal("amt"); d.String() != "-0.01" {
		t.Errorf("as numeric: %v", r.Col("amt"))
	}
	if err := e.AlterColumnType("p", "amt", bytdb.ColumnTypeChange{Type: bytdb.TMoney}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.Get("p", int64(10000)); !ok {
		t.Error("$100.00 not found after numeric -> money")
	}
	// int -> money reads whole units, and the int default comes along
	// as money text, not bare cents (which SQL would read as dollars).
	if err := e.SetColumnDefault("p", "n", "7"); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("p", "1", 7); err != nil {
		t.Fatal(err)
	}
	if err := e.AlterColumnType("p", "n", bytdb.ColumnTypeChange{Type: bytdb.TMoney}); err != nil {
		t.Fatal(err)
	}
	desc := e.TableShared("p")
	if d := desc.Columns[1].Default; d != "'$7.00'" {
		t.Errorf("default after int -> money = %q", d)
	}
	r, _, _ = e.Get("p", int64(100))
	if r.Col("n") != int64(700) {
		t.Errorf("int 7 -> money = %v cents", r.Col("n"))
	}
}
