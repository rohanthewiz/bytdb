package bytdb_test

// N-053: bit and bit varying at the engine level — text input (binary
// and hex), bit(n)'s exact length and varbit(n)'s maximum on every
// write, keys in bit-string order, and ALTER COLUMN TYPE between them.
// The SQL operators, casts and functions are checked against Postgres
// by sql/difftest/corpus/84_bit.sql.

import (
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func TestBitColumns(t *testing.T) {
	e := openTestEngine(t)
	if _, err := e.CreateTable("b", []bytdb.Column{
		{Name: "k", Type: bytdb.TVarBit},
		{Name: "f", Type: bytdb.TBit, MaxLen: 4},
		{Name: "one", Type: bytdb.TBit}, // a bare bit is bit(1)
		{Name: "v", Type: bytdb.TVarBit, MaxLen: 3},
	}, "k"); err != nil {
		t.Fatal(err)
	}
	for _, r := range [][]any{
		{"1", "1010", "1", "11"},
		{"10", "x5", "0", ""},
		{"", []byte("b0001"), nil, "1"},
		{"0111", "0000", "1", nil},
	} {
		if err := e.Insert("b", r...); err != nil {
			t.Fatalf("insert %v: %v", r, err)
		}
	}
	for _, c := range []struct {
		row  []any
		want string
	}{
		{[]any{"11", "101", nil, nil}, "bit string length 3 does not match type bit(4)"},
		{[]any{"11", "1010", "10", nil}, "bit string length 2 does not match type bit(1)"},
		{[]any{"11", "1010", nil, "1010"}, "bit string too long for type bit varying(3)"},
		{[]any{"11", "1012", nil, nil}, `"2" is not a valid binary digit`},
	} {
		if err := e.Insert("b", c.row...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("insert %v: %v, want %q", c.row, err, c.want)
		}
	}
	var keys []string
	for r, err := range e.Scan("b") {
		if err != nil {
			t.Fatal(err)
		}
		k, _ := r.String("k")
		f, _ := r.String("f")
		keys = append(keys, "["+k+"]"+f)
	}
	// Bit-string order: a prefix first, then the first differing bit.
	if got := strings.Join(keys, " "); got != "[]0001 [0111]0000 [1]1010 [10]0101" {
		t.Errorf("rows = %s", got)
	}
	// Changing f's length rewrites every row under the new rule: bit(5)
	// refuses the 4-bit values, varbit(8) takes them.
	if err := e.AlterColumnType("b", "f", bytdb.ColumnTypeChange{Type: bytdb.TBit, MaxLen: 5}); err == nil {
		t.Error("bit(4) -> bit(5) accepted 4-bit values")
	}
	if err := e.AlterColumnType("b", "f", bytdb.ColumnTypeChange{Type: bytdb.TVarBit, MaxLen: 8}); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("b", "1111", "10101010", nil, nil); err != nil {
		t.Fatal(err)
	}
}
