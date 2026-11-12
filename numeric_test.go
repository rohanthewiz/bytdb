package bytdb_test

// N-048: the numeric column type at the engine level — coercion, the
// numeric(p, s) typmod, scale surviving the key encoding, unique-index
// equality across scales, ALTER COLUMN TYPE, DEFAULT backfill, and a
// reopen. The SQL and wire layers have their own tests.

import (
	"errors"
	"math/big"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func dec(s string) bytdb.Decimal {
	d, err := bytdb.ParseDecimal(s)
	if err != nil {
		panic(err)
	}
	return d
}

func numStr(t *testing.T, r bytdb.Row, col string) string {
	t.Helper()
	d, ok := r.Decimal(col)
	if !ok {
		t.Fatalf("column %s is %T (%v), not a Decimal", col, r.Col(col), r.Col(col))
	}
	return d.String()
}

func TestNumericColumnCoercionAndTypmod(t *testing.T) {
	e := openTestEngine(t)
	if _, err := e.CreateTable("acct", []bytdb.Column{
		{Name: "id", Type: bytdb.TInt},
		{Name: "bal", Type: bytdb.TNumeric, Precision: 7, Scale: 2},
		{Name: "raw", Type: bytdb.TNumeric},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	// Every accepted input shape; bal rounds half away from zero to 2
	// places, raw keeps what it was given.
	cases := []struct {
		in       any
		bal, raw string
	}{
		{dec("12.345"), "12.35", "12.345"},
		{int64(-7), "-7.00", "-7"},
		{3, "3.00", "3"},
		{0.1, "0.10", "0.1"}, // a float64 means its shortest text
		{float32(0.1), "0.10", "0.1"},
		{"19.995", "20.00", "19.995"},
		{"-0.005", "-0.01", "-0.005"},
		{[]byte("1e2"), "100.00", "100"},
		{new(big.Int).SetInt64(42), "42.00", "42"},
		{uint64(1 << 40), "1099511627776.00", "1099511627776"}, // raw only; bal would overflow
	}
	for i, c := range cases {
		id := int64(i + 1)
		bal := c.in
		if c.bal == "1099511627776.00" {
			bal = nil
		}
		if err := e.Insert("acct", id, bal, c.in); err != nil {
			t.Fatalf("insert %v: %v", c.in, err)
		}
		r, ok, err := e.Get("acct", id)
		if err != nil || !ok {
			t.Fatalf("get %d: %v %v", id, ok, err)
		}
		if bal != nil {
			if got := numStr(t, r, "bal"); got != c.bal {
				t.Errorf("bal from %#v = %s, want %s", c.in, got, c.bal)
			}
		}
		if got := numStr(t, r, "raw"); got != c.raw {
			t.Errorf("raw from %#v = %s, want %s", c.in, got, c.raw)
		}
	}
	// Overflow: 99999.995 rounds to 100000.00, which needs 6 integer
	// digits in a numeric(7,2).
	err := e.Insert("acct", int64(100), dec("99999.995"), nil)
	if err == nil || !strings.Contains(err.Error(), "numeric field overflow") {
		t.Fatalf("overflow insert: %v", err)
	}
	if !strings.Contains(bytdb.ErrText(err), "must round to an absolute value less than 10^5") {
		t.Errorf("overflow detail missing: %s", bytdb.ErrText(err))
	}
	for _, bad := range []any{"abc", "NaN", 1.5i} {
		if err := e.Insert("acct", int64(101), nil, bad); err == nil {
			t.Errorf("insert of %#v into numeric succeeded", bad)
		}
	}
	if err := e.Insert("acct", int64(102), nil, "abc"); err == nil ||
		err.Error() != `invalid input syntax for type numeric: "abc"` {
		t.Errorf("syntax error wording: %v", err)
	}
	// Update goes through the same typmod.
	if _, err := e.Update("acct", []any{int64(1)}, map[string]any{"bal": "1.239"}); err != nil {
		t.Fatal(err)
	}
	r, _, _ := e.Get("acct", int64(1))
	if got := numStr(t, r, "bal"); got != "1.24" {
		t.Errorf("updated bal = %s", got)
	}
}

func TestNumericTypmodDeclarations(t *testing.T) {
	e := openTestEngine(t)
	for _, c := range []struct {
		col  bytdb.Column
		want string
	}{
		{bytdb.Column{Name: "x", Type: bytdb.TNumeric, Precision: 0, Scale: 2}, "NUMERIC precision 0 must be between 1 and 1000"},
		{bytdb.Column{Name: "x", Type: bytdb.TNumeric, Precision: 1001}, "NUMERIC precision 1001"},
		{bytdb.Column{Name: "x", Type: bytdb.TNumeric, Precision: 5, Scale: 1001}, "NUMERIC scale 1001"},
		{bytdb.Column{Name: "x", Type: bytdb.TInt, Precision: 5}, "only valid on numeric columns"},
	} {
		_, err := e.CreateTable("bad", []bytdb.Column{{Name: "id", Type: bytdb.TInt}, c.col}, "id")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: err = %v, want %q", c.col, err, c.want)
		}
	}
	// Scale above precision and negative scale are Postgres 15+ forms.
	if _, err := e.CreateTable("odd", []bytdb.Column{
		{Name: "id", Type: bytdb.TInt},
		{Name: "tiny", Type: bytdb.TNumeric, Precision: 3, Scale: 5},
		{Name: "thou", Type: bytdb.TNumeric, Precision: 2, Scale: -3},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("odd", int64(1), "0.00123", "12345"); err != nil {
		t.Fatal(err)
	}
	r, _, _ := e.Get("odd", int64(1))
	if numStr(t, r, "tiny") != "0.00123" || numStr(t, r, "thou") != "12000" {
		t.Errorf("odd typmods: %v", r.Vals)
	}
	if err := e.Insert("odd", int64(2), "0.0123", nil); err == nil {
		t.Error("0.0123 fit a numeric(3,5)")
	}
}

// TestNumericKeysKeepScale covers the key encoding: a numeric primary key
// reads back at the scale it was written (the row value carries it,
// decodeRow prefers it), ordering is numeric, a unique index treats
// 1.0 and 1.00 as one value, and all of it survives a reopen.
func TestNumericKeysKeepScale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.db")
	e, err := bytdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTable("price", []bytdb.Column{
		{Name: "p", Type: bytdb.TNumeric},
		{Name: "code", Type: bytdb.TNumeric},
		{Name: "note", Type: bytdb.TString},
	}, "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateIndex("price", "price_code", true, "code"); err != nil {
		t.Fatal(err)
	}
	for i, p := range []string{"10.50", "-2.000", "9.9", "100", "0.05", "1e3"} {
		if err := e.Insert("price", dec(p), dec(strings.Repeat("1", i+1)+".0"), p); err != nil {
			t.Fatalf("insert %s: %v", p, err)
		}
	}
	// A key equal in value to an existing one is a duplicate, whatever
	// its scale.
	if err := e.Insert("price", dec("10.5"), dec("7"), "dup"); !errors.Is(err, bytdb.ErrDuplicateKey) {
		t.Fatalf("10.5 after 10.50: %v", err)
	}
	if err := e.Insert("price", dec("7"), dec("1.00"), "dup"); !errors.Is(err, bytdb.ErrUniqueViolation) {
		t.Fatalf("code 1.00 after 1.0: %v", err)
	}
	check := func(e *bytdb.Engine) {
		t.Helper()
		var got []string
		for r, err := range e.Scan("price") {
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, numStr(t, r, "p"))
		}
		want := "-2.000 0.05 9.9 10.50 100 1000"
		if strings.Join(got, " ") != want {
			t.Errorf("scan order/scale = %v, want %s", got, want)
		}
		// A point lookup by an equal value at another scale finds the
		// row and reports the stored scale.
		r, ok, err := e.Get("price", dec("10.5000"))
		if err != nil || !ok || numStr(t, r, "p") != "10.50" {
			t.Errorf("Get(10.5000) = %v %v %v", r.Vals, ok, err)
		}
		n := 0
		for r, err := range e.ScanIndex("price", "price_code", []any{dec("11")}, []any{dec("1111")}) {
			if err != nil {
				t.Fatal(err)
			}
			n++
			_ = r
		}
		if n != 2 { // 11.0 and 111.0: from ≤ code < to
			t.Errorf("index range found %d rows", n)
		}
	}
	check(e)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if e, err = bytdb.Open(path); err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	check(e)
}

func TestNumericAlterAndDefault(t *testing.T) {
	e := openTestEngine(t)
	if _, err := e.CreateTable("m", []bytdb.Column{
		{Name: "id", Type: bytdb.TInt},
		{Name: "i", Type: bytdb.TInt},
		{Name: "f", Type: bytdb.TFloat},
		{Name: "n", Type: bytdb.TNumeric},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("m", int64(1), int64(5), 0.1, dec("2.555")); err != nil {
		t.Fatal(err)
	}
	// DEFAULT backfill keeps the literal's scale.
	if err := e.AddColumn("m", bytdb.Column{Name: "fee", Type: bytdb.TNumeric, Precision: 6, Scale: 2, Default: "0.5"}); err != nil {
		t.Fatal(err)
	}
	// Implicit/assignment casts, no USING needed: int → numeric,
	// float8 → numeric (15 significant digits), numeric → numeric(p,s)
	// (rounds), numeric → float8.
	for col, ch := range map[string]bytdb.ColumnTypeChange{
		"i": {Type: bytdb.TNumeric},
		"f": {Type: bytdb.TNumeric, Precision: 5, Scale: 3},
		"n": {Type: bytdb.TNumeric, Precision: 4, Scale: 2},
	} {
		if err := e.AlterColumnType("m", col, ch); err != nil {
			t.Fatalf("alter %s: %v", col, err)
		}
	}
	r, _, _ := e.Get("m", int64(1))
	for col, want := range map[string]string{"i": "5", "f": "0.100", "n": "2.56", "fee": "0.50"} {
		if got := numStr(t, r, col); got != want {
			t.Errorf("%s = %s, want %s", col, got, want)
		}
	}
	// Narrowing below what a row holds fails the whole ALTER.
	if err := e.AlterColumnType("m", "n", bytdb.ColumnTypeChange{Type: bytdb.TNumeric, Precision: 2, Scale: 2}); err == nil ||
		!strings.Contains(err.Error(), "numeric field overflow") {
		t.Errorf("narrowing alter: %v", err)
	}
	if err := e.AlterColumnType("m", "n", bytdb.ColumnTypeChange{Type: bytdb.TFloat}); err != nil {
		t.Fatal(err)
	}
	if err := e.AlterColumnType("m", "i", bytdb.ColumnTypeChange{Type: bytdb.TInt}); err != nil {
		t.Fatal(err)
	}
	r, _, _ = e.Get("m", int64(1))
	if f, _ := r.Float("n"); f != 2.56 {
		t.Errorf("numeric → float = %v", r.Col("n"))
	}
	if i, _ := r.Int("i"); i != 5 {
		t.Errorf("numeric → int = %v", r.Col("i"))
	}
	if err := e.AlterColumnType("m", "fee", bytdb.ColumnTypeChange{Type: bytdb.TString}); err != nil {
		t.Fatal(err)
	}
	r, _, _ = e.Get("m", int64(1))
	if s, _ := r.String("fee"); s != "0.50" {
		t.Errorf("numeric → text = %v", r.Col("fee"))
	}
}
