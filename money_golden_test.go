package bytdb_test

// money_golden_test.go replays testdata/money/golden.tsv: Postgres
// 16.9's answers (lc_monetary en_US.utf8) for money input, output,
// cash_words, the casts to and from numeric and int8, and the
// arithmetic operators, captured by testdata/money/gen.sh. Every case
// must match: the same text, or an error with the same SQLSTATE.

import (
	"bufio"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// moneySQLState maps the money helpers' errors to Postgres's codes.
func moneySQLState(err error) string {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "invalid input syntax for type money"):
		return "22P02"
	case msg == "division by zero":
		return "22012"
	case msg == "money out of range", msg == "bigint out of range",
		msg == "value out of range: overflow",
		strings.Contains(msg, "is out of range for type money"):
		return "22003"
	}
	return "XX000 " + msg
}

func TestMoneyGolden(t *testing.T) {
	f, err := os.Open("testdata/money/golden.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n, failed := 0, 0
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) != 4 {
			t.Fatalf("line %d: %d fields", n+1, len(parts))
		}
		op, a, b, want := parts[0], parts[1], parts[2], parts[3]
		n++
		if got := moneyCase(t, op, a, b, want); got != want {
			failed++
			if failed <= 40 {
				t.Errorf("%s(%q, %q): got %q, want %q", op, a, b, got, want)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if failed > 0 {
		t.Errorf("%d of %d golden cases differ", failed, n)
	}
	t.Logf("%d golden cases", n)
}

func moneyCase(t *testing.T, op, a, b, want string) string {
	cents := func(s string) int64 {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("bad cents %q", s)
		}
		return v
	}
	money := func(v int64, err error) string {
		if err != nil {
			return "ERROR " + moneySQLState(err)
		}
		return bytdb.FormatMoney(v)
	}
	switch op {
	case "in":
		return money(bytdb.ParseMoney(a))
	case "out":
		return bytdb.FormatMoney(cents(a))
	case "words":
		return bytdb.MoneyWords(cents(a))
	case "tonum":
		return bytdb.MoneyToDecimal(cents(a)).String()
	case "add":
		return money(bytdb.MoneyAdd(cents(a), cents(b)))
	case "sub":
		return money(bytdb.MoneySub(cents(a), cents(b)))
	case "div":
		r, err := bytdb.MoneyDivMoney(cents(a), cents(b))
		if err != nil {
			return "ERROR " + moneySQLState(err)
		}
		// Compare as floats (Postgres and Go spell some differently).
		if w, err := strconv.ParseFloat(want, 64); err == nil && w == r {
			return want
		}
		return bytdb.FormatFloat(r)
	case "muli":
		return money(bytdb.MoneyMulInt(cents(a), cents(b)))
	case "divi":
		return money(bytdb.MoneyDivInt(cents(a), cents(b)))
	case "mulf", "divf":
		var fv float64
		switch strings.ToLower(b) {
		case "nan":
			fv = math.NaN()
		case "infinity":
			fv = math.Inf(1)
		default:
			var err error
			if fv, err = strconv.ParseFloat(b, 64); err != nil {
				t.Fatalf("bad float %q", b)
			}
		}
		if op == "mulf" {
			return money(bytdb.MoneyMulFloat(cents(a), fv))
		}
		return money(bytdb.MoneyDivFloat(cents(a), fv))
	case "fromnum":
		d, err := bytdb.ParseDecimal(a)
		if err != nil {
			t.Fatal(err)
		}
		return money(bytdb.MoneyFromDecimal(d))
	case "fromint":
		return money(bytdb.MoneyFromInt(cents(a)))
	}
	t.Fatalf("unknown golden op %q", op)
	return ""
}
