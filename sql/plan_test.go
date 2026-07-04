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
	p, err := planScan(planDesc(), "users", and(
		cpred("id", OpEQ, int64(7)),
		cpred("city", OpEQ, "Reno"),
	))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.get, []any{int64(7)}) {
		t.Fatalf("want point get, got %#v", p)
	}
	if p.filter == nil {
		t.Fatal("residual filter must keep the whole WHERE tree")
	}
}

func TestPlanPKRange(t *testing.T) {
	p, err := planScan(planDesc(), "users", and(
		cpred("id", OpGE, int64(10)),
		cpred("id", OpLT, int64(20)),
	))
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
	p, err := planScan(planDesc(), "users", and(
		cpred("name", OpEQ, "ada"),
		cpred("age", OpGT, int64(21)),
		cpred("age", OpLE, int64(65)),
	))
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
	p, err := planScan(planDesc(), "users", and(
		cpred("id", OpGE, int64(1)),
		cpred("city", OpGE, "a"),
	))
	if err != nil {
		t.Fatal(err)
	}
	if p.index != "" || !reflect.DeepEqual(p.from, []any{int64(1)}) {
		t.Fatalf("want primary scan from id>=1, got %#v", p)
	}
}

func TestPlanFullScanFallback(t *testing.T) {
	// A float literal does not fit an int key column: nothing pushes.
	p, err := planScan(planDesc(), "users", and(
		cpred("id", OpEQ, 1.5),
		cpred("age", OpNE, int64(3)),
	))
	if err != nil {
		t.Fatal(err)
	}
	if p.get != nil || p.index != "" || p.from != nil || p.stops != nil {
		t.Fatalf("want unbounded primary scan, got %#v", p)
	}
	if p.filter == nil {
		t.Fatal("residual filter must keep the whole WHERE tree")
	}
}

func TestPlanUnknownColumn(t *testing.T) {
	if _, err := planScan(planDesc(), "users", cpred("nope", OpEQ, int64(1))); err == nil {
		t.Fatal("expected error")
	}
	// Columns are validated even under OR and NOT.
	bad := or(cpred("id", OpEQ, int64(1)), &Not{Expr: cpred("nope", OpEQ, int64(1))})
	if _, err := planScan(planDesc(), "users", bad); err == nil {
		t.Fatal("expected error inside OR/NOT")
	}
}

func TestPlanORIsResidualOnly(t *testing.T) {
	// A top-level OR pushes nothing, even when every branch touches
	// the primary key.
	p, err := planScan(planDesc(), "users", or(
		cpred("id", OpEQ, int64(1)),
		cpred("id", OpEQ, int64(2)),
	))
	if err != nil {
		t.Fatal(err)
	}
	if p.get != nil || p.from != nil || p.stops != nil || p.index != "" {
		t.Fatalf("want unbounded primary scan, got %#v", p)
	}

	// NOT blocks pushdown of its subtree too.
	p, err = planScan(planDesc(), "users", &Not{Expr: cpred("id", OpLT, int64(5))})
	if err != nil {
		t.Fatal(err)
	}
	if p.from != nil || p.stops != nil {
		t.Fatalf("want unbounded scan, got %#v", p)
	}
}

func TestPlanColVsColIsResidualOnly(t *testing.T) {
	// name = city compares two columns: valid, but nothing pushes.
	p, err := planScan(planDesc(), "users", and(
		ccpred(col("name"), OpEQ, col("city")),
		cpred("id", OpGE, int64(10)),
	))
	if err != nil {
		t.Fatal(err)
	}
	if p.get != nil || p.index != "" || !reflect.DeepEqual(p.from, []any{int64(10)}) {
		t.Fatalf("got %#v", p)
	}
	// A qualifier must match the table's FROM name.
	if _, err := planScan(planDesc(), "u", cpred("id", OpEQ, int64(1))); err != nil {
		t.Fatalf("unqualified ref should resolve under any alias: %v", err)
	}
	qualified := &Pred{Item: SelectItem{Col: ColRef{Table: "nope", Name: "id"}}, Op: OpEQ, Val: int64(1)}
	if _, err := planScan(planDesc(), "users", qualified); err == nil {
		t.Fatal("expected error for a foreign qualifier")
	}
}

func TestPlanBoundParamPushdown(t *testing.T) {
	// An unbound Param is not a literal the planner can use; after
	// binding, the value pushes down like any literal, and binding
	// leaves the parsed statement untouched.
	st := mustParse(t, `select * from users where id = $1`)
	where := st.(*Select).Where
	p, err := planScan(planDesc(), "users", where)
	if err != nil {
		t.Fatal(err)
	}
	if p.get != nil || p.from != nil || p.stops != nil {
		t.Fatalf("unbound param must not push, got %#v", p)
	}

	bound, err := bindParams(st, []any{7})
	if err != nil {
		t.Fatal(err)
	}
	p, err = planScan(planDesc(), "users", bound.(*Select).Where)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.get, []any{int64(7)}) {
		t.Fatalf("want point get, got %#v", p)
	}
	if where.(*Pred).Val != Param(1) {
		t.Fatalf("binding mutated the parsed statement: %#v", where)
	}
}

func TestPlanConjunctBesideOR(t *testing.T) {
	// id >= 10 AND (city = 'a' OR age > 3): the conjunct still pushes;
	// the OR stays residual.
	p, err := planScan(planDesc(), "users", and(
		cpred("id", OpGE, int64(10)),
		or(cpred("city", OpEQ, "a"), cpred("age", OpGT, int64(3))),
	))
	if err != nil {
		t.Fatal(err)
	}
	if p.index != "" || !reflect.DeepEqual(p.from, []any{int64(10)}) || p.stops != nil {
		t.Fatalf("got %#v", p)
	}
}
