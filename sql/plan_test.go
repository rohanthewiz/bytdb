package sql

import (
	"reflect"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// planDesc: users(id int, name string, age int, city string), pk (id),
// by_city on (city), by_name_age on (name, age).
func planDesc() *bytdb.TableDesc {
	return &bytdb.TableDesc{
		Name: "users",
		Columns: []bytdb.Column{
			{Name: "id", Type: bytdb.TInt}, {Name: "name", Type: bytdb.TString},
			{Name: "age", Type: bytdb.TInt}, {Name: "city", Type: bytdb.TString},
		},
		PKCols: []int{0},
		Indexes: []bytdb.IndexDesc{
			{ID: 2, Name: "by_city", Cols: []int{3}},
			{ID: 3, Name: "by_name_age", Cols: []int{1, 2}},
		},
	}
}

func TestPlanPointGet(t *testing.T) {
	p, err := planScan(planDesc(), []Pred{
		{Col: "id", Op: OpEQ, Val: int64(7)},
		{Col: "city", Op: OpEQ, Val: "Reno"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.get, []any{int64(7)}) {
		t.Fatalf("want point get, got %#v", p)
	}
	if len(p.preds) != 2 {
		t.Fatal("residual filter must keep every predicate")
	}
}

func TestPlanPKRange(t *testing.T) {
	p, err := planScan(planDesc(), []Pred{
		{Col: "id", Op: OpGE, Val: int64(10)},
		{Col: "id", Op: OpLT, Val: int64(20)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.get != nil || p.index != "" {
		t.Fatalf("want primary range scan, got %#v", p)
	}
	if !reflect.DeepEqual(p.from, []any{int64(10)}) {
		t.Fatalf("from: got %#v", p.from)
	}
	if !reflect.DeepEqual(p.stops, []stop{{0, stopGE, int64(20)}}) {
		t.Fatalf("stops: got %#v", p.stops)
	}
}

func TestPlanIndexEqPrefixPlusRange(t *testing.T) {
	p, err := planScan(planDesc(), []Pred{
		{Col: "name", Op: OpEQ, Val: "ada"},
		{Col: "age", Op: OpGT, Val: int64(21)},
		{Col: "age", Op: OpLE, Val: int64(65)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.index != "by_name_age" {
		t.Fatalf("index: got %q", p.index)
	}
	if !reflect.DeepEqual(p.from, []any{"ada", int64(21)}) {
		t.Fatalf("from: got %#v", p.from)
	}
	if !reflect.DeepEqual(p.stops, []stop{{1, stopNE, "ada"}, {2, stopGT, int64(65)}}) {
		t.Fatalf("stops: got %#v", p.stops)
	}
}

func TestPlanPrimaryWinsTies(t *testing.T) {
	// eq on id (pk) and eq on city (indexed) score equally at one
	// equality column... they don't: pk eq is a point get. Use a range
	// on id vs a range on city instead: equal scores, primary wins.
	p, err := planScan(planDesc(), []Pred{
		{Col: "id", Op: OpGE, Val: int64(1)},
		{Col: "city", Op: OpGE, Val: "a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.index != "" || !reflect.DeepEqual(p.from, []any{int64(1)}) {
		t.Fatalf("want primary scan from id>=1, got %#v", p)
	}
}

func TestPlanFullScanFallback(t *testing.T) {
	// A float literal does not fit an int key column: nothing pushes.
	p, err := planScan(planDesc(), []Pred{
		{Col: "id", Op: OpEQ, Val: 1.5},
		{Col: "age", Op: OpNE, Val: int64(3)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.get != nil || p.index != "" || p.from != nil || p.stops != nil {
		t.Fatalf("want unbounded primary scan, got %#v", p)
	}
	if len(p.preds) != 2 {
		t.Fatal("residual filter must keep every predicate")
	}
}

func TestPlanUnknownColumn(t *testing.T) {
	if _, err := planScan(planDesc(), []Pred{{Col: "nope", Op: OpEQ, Val: int64(1)}}); err == nil {
		t.Fatal("expected error")
	}
}
