package bytdb

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/bytdb/tuple"
)

// Property test for reverse scans: over random data and random bounds,
// a reverse scan must be exactly its forward twin read back to front —
// for the primary key, an ascending secondary index, and a
// mixed-direction secondary index alike. The toIncl variant is checked
// against a model built purely from forward scans, so the reverse
// implementation is never used to predict itself:
//
//	rev(from, to, false) == reverse(fwd(from, to))
//	rev(from, to, true)  == reverse(fwd(from, to) ++ group(to) rows ≥ from)
//
// where group(to) is the prefix group toIncl adds — rows whose leading
// key columns all equal the bound. Those rows are exactly the leading
// run of fwd(to, nil) whose key columns compare equal to the bound:
// the group occupies [enc(to), PrefixEnd(enc(to))) in key space, so
// anything past it sorts after every member.
func TestScanRevProperty(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()

	// Composite PK so partial-prefix bounds mean something; a nullable
	// indexed column so NULL ordering is exercised; small value domains
	// so random bounds collide with real keys often.
	if _, err := e.CreateTable("ev", []Column{
		{Name: "a", Type: TInt}, {Name: "b", Type: TInt},
		{Name: "c", Type: TInt}, {Name: "d", Type: TString},
	}, "a", "b"); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(42))
	strs := []string{"", "x", "y", "z\x00"}
	seen := map[[2]int64]bool{}
	for range 90 {
		a, b := rng.Int63n(8), rng.Int63n(8)
		if seen[[2]int64{a, b}] {
			continue
		}
		seen[[2]int64{a, b}] = true
		var c any
		if rng.Intn(4) > 0 {
			c = rng.Int63n(5)
		}
		if err := e.Insert("ev", a, b, c, strs[rng.Intn(len(strs))]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.CreateIndex("ev", "idx_asc", false, "c", "d"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateIndexCols("ev", "idx_mixed", false,
		[]IndexCol{{Name: "c", Desc: true}, {Name: "d"}}); err != nil {
		t.Fatal(err)
	}

	// One scan family = a forward and a reverse scan over the same key
	// space, plus the row's key columns in that space (for the
	// prefix-group model).
	type family struct {
		name    string
		fwd     func(from, to []any) []Row
		rev     func(from, to []any, toIncl bool) []Row
		keyCols func(r Row) []any
	}
	rows := func(seq func(func(Row, error) bool)) []Row { return collect(t, seq) }
	families := []family{
		{
			name: "pk",
			fwd:  func(f, to []any) []Row { return rows(e.ScanRange("ev", f, to)) },
			rev: func(f, to []any, ti bool) []Row {
				return rows(e.ScanRangeRev("ev", f, to, ti))
			},
			keyCols: func(r Row) []any { return []any{r.Col("a"), r.Col("b")} },
		},
		{
			name: "idx_asc",
			fwd:  func(f, to []any) []Row { return rows(e.ScanIndex("ev", "idx_asc", f, to)) },
			rev: func(f, to []any, ti bool) []Row {
				return rows(e.ScanIndexRev("ev", "idx_asc", f, to, ti))
			},
			keyCols: func(r Row) []any { return []any{r.Col("c"), r.Col("d")} },
		},
		{
			name: "idx_mixed",
			fwd:  func(f, to []any) []Row { return rows(e.ScanIndex("ev", "idx_mixed", f, to)) },
			rev: func(f, to []any, ti bool) []Row {
				return rows(e.ScanIndexRev("ev", "idx_mixed", f, to, ti))
			},
			keyCols: func(r Row) []any { return []any{r.Col("c"), r.Col("d")} },
		},
	}

	// Random bound: nil (unbounded), or a 1-2 column prefix typed to
	// the scanned key — (a INT, b INT) for the PK, (c INT, d STRING)
	// for the indexes — drawn from a domain slightly wider than the
	// data so empty regions and off-key bounds occur too. Index bounds
	// may include NULL (the indexed column is nullable).
	randBound := func(index bool) []any {
		intVal := func() any {
			if index && rng.Intn(8) == 0 {
				return nil
			}
			return rng.Int63n(10) - 1
		}
		strVal := func() any {
			if index && rng.Intn(8) == 0 {
				return nil
			}
			return strs[rng.Intn(len(strs))]
		}
		switch rng.Intn(4) {
		case 0:
			return nil
		case 1:
			return []any{intVal()}
		default:
			if index {
				return []any{intVal(), strVal()}
			}
			return []any{intVal(), intVal()}
		}
	}

	for iter := range 300 {
		for _, fam := range families {
			from := randBound(fam.name != "pk")
			to := randBound(fam.name != "pk")

			fwd := fam.fwd(from, to)
			rev := fam.rev(from, to, false)
			if !sameRows(fwd, rev, true) {
				t.Fatalf("iter %d %s from=%v to=%v: rev != reverse(fwd)\nfwd: %v\nrev: %v",
					iter, fam.name, from, to, pks(fwd), pks(rev))
			}

			if to == nil {
				continue // toIncl is meaningless without an upper bound
			}
			// Model the inclusive upper bound from forward scans only:
			// the [from, to) region plus to's prefix group, keeping
			// forward order (filtering fwd(from, nil) preserves it) —
			// group rows below `from` stay excluded.
			inRegion := map[string]bool{}
			for _, r := range fwd {
				inRegion[pkKeyOf(t, r)] = true
			}
			for _, r := range fam.fwd(to, nil) {
				if !boundEq(fam.keyCols(r)[:len(to)], to) {
					break // past the prefix group; nothing later is in it
				}
				inRegion[pkKeyOf(t, r)] = true
			}
			var want []Row
			for _, r := range fam.fwd(from, nil) {
				if inRegion[pkKeyOf(t, r)] {
					want = append(want, r)
				}
			}
			got := fam.rev(from, to, true)
			if !sameRows(want, got, true) {
				t.Fatalf("iter %d %s from=%v to=%v toIncl: got %v want reverse(%v)",
					iter, fam.name, from, to, pks(got), pks(want))
			}
		}
	}
}

// boundEq reports element-wise semantic equality between a row's key
// columns and a bound — the equality the encoding preserves (desc
// inversion included, since inversion is a bijection).
func boundEq(cols, bound []any) bool {
	for i := range bound {
		if tuple.Compare([]any{cols[i]}, []any{bound[i]}) != 0 {
			return false
		}
	}
	return true
}

// pkKeyOf identifies a row by its primary key, encoded canonically.
func pkKeyOf(t *testing.T, r Row) string {
	t.Helper()
	k, err := tuple.Encode(r.Col("a"), r.Col("b"))
	if err != nil {
		t.Fatal(err)
	}
	return string(k)
}

// sameRows compares two row sequences by primary key, optionally
// reversing the second.
func sameRows(fwd, rev []Row, reversed bool) bool {
	if len(fwd) != len(rev) {
		return false
	}
	for i := range fwd {
		j := i
		if reversed {
			j = len(rev) - 1 - i
		}
		if fwd[i].Col("a") != rev[j].Col("a") || fwd[i].Col("b") != rev[j].Col("b") {
			return false
		}
	}
	return true
}

// pks renders rows as their PK pairs for failure messages.
func pks(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = fmt.Sprintf("(%v,%v)", r.Col("a"), r.Col("b"))
	}
	return out
}
