package sql

import (
	"reflect"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func mustParse(t *testing.T, src string) Statement {
	t.Helper()
	st, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	return st
}

// tree-building helpers for expected values
func cpred(col string, op PredOp, val any) *Pred {
	return &Pred{Item: SelectItem{Col: col}, Op: op, Val: val}
}

func apred(item SelectItem, op PredOp, val any) *Pred {
	return &Pred{Item: item, Op: op, Val: val}
}

func and(es ...BoolExpr) *And { return &And{Exprs: es} }
func or(es ...BoolExpr) *Or   { return &Or{Exprs: es} }

func TestParseCreateTable(t *testing.T) {
	st := mustParse(t, `CREATE TABLE Users (
		id BIGINT PRIMARY KEY,
		name varchar(40),
		score double precision,
		active boolean,
		blob bytea
	);`)
	want := &CreateTable{
		Table: "users",
		Cols: []ColDef{
			{"id", bytdb.TInt}, {"name", bytdb.TString}, {"score", bytdb.TFloat},
			{"active", bytdb.TBool}, {"blob", bytdb.TBytes},
		},
		PK: []string{"id"},
	}
	if !reflect.DeepEqual(st, want) {
		t.Fatalf("got %#v", st)
	}

	st = mustParse(t, `create table ev (a int, b int, v text, primary key (a, b))`)
	ct := st.(*CreateTable)
	if !reflect.DeepEqual(ct.PK, []string{"a", "b"}) {
		t.Fatalf("composite pk: got %v", ct.PK)
	}
}

func TestParseCreateTableErrors(t *testing.T) {
	for _, src := range []string{
		`create table t (a int primary key, b int primary key)`,
		`create table t (a int primary key, primary key (a))`,
		`create table t (a wibble)`,
		`create table t (a int`,
	} {
		if _, err := Parse(src); err == nil {
			t.Errorf("Parse(%q): expected error", src)
		}
	}
}

func TestParseSelect(t *testing.T) {
	st := mustParse(t, `SELECT name, age FROM users
		WHERE age >= 21 AND 100 > score AND city = 'Reno' AND note IS NOT NULL
		ORDER BY age DESC, name LIMIT 10 OFFSET 5`)
	want := &Select{
		Table: "users",
		Items: []SelectItem{{Col: "name"}, {Col: "age"}},
		Where: and(
			cpred("age", OpGE, int64(21)),
			cpred("score", OpLT, int64(100)), // flipped
			cpred("city", OpEQ, "Reno"),
			cpred("note", OpIsNotNull, nil),
		),
		OrderBy: []OrderItem{
			{SelectItem: SelectItem{Col: "age"}, Desc: true},
			{SelectItem: SelectItem{Col: "name"}},
		},
		Limit:  10,
		Offset: 5,
	}
	if !reflect.DeepEqual(st, want) {
		t.Fatalf("got %#v", st)
	}

	st = mustParse(t, `select * from t where a is null offset 2 limit 3`)
	s := st.(*Select)
	if !s.Star || s.Limit != 3 || s.Offset != 2 || s.Where.(*Pred).Op != OpIsNull {
		t.Fatalf("got %#v", s)
	}
	if mustParse(t, `select * from t`).(*Select).Limit != -1 {
		t.Fatal("no LIMIT should parse as -1")
	}
}

func TestParseInsert(t *testing.T) {
	st := mustParse(t, `INSERT INTO t (a, b, c, d) VALUES (1, -2.5, 'x', NULL), (2, 3, 'y', true)`)
	want := &Insert{
		Table: "t",
		Cols:  []string{"a", "b", "c", "d"},
		Rows: [][]any{
			{int64(1), -2.5, "x", nil},
			{int64(2), int64(3), "y", true},
		},
	}
	if !reflect.DeepEqual(st, want) {
		t.Fatalf("got %#v", st)
	}
	ins := mustParse(t, `insert into t values (1, 'z')`).(*Insert)
	if ins.Cols != nil || len(ins.Rows) != 1 {
		t.Fatalf("got %#v", ins)
	}
}

func TestParseUpdateDelete(t *testing.T) {
	st := mustParse(t, `UPDATE users SET city = 'Reno', age = 30 WHERE id = 7`)
	u := st.(*Update)
	if u.Table != "users" || u.Set["city"] != "Reno" || u.Set["age"] != int64(30) ||
		!reflect.DeepEqual(u.Where, cpred("id", OpEQ, int64(7))) {
		t.Fatalf("got %#v", u)
	}
	d := mustParse(t, `delete from users where id != 3`).(*Delete)
	if d.Where.(*Pred).Op != OpNE {
		t.Fatalf("got %#v", d)
	}
}

func TestParseBoolExprs(t *testing.T) {
	// AND binds tighter than OR; NOT tighter still; parens group.
	s := mustParse(t, `select * from t where a = 1 or b = 2 and not c = 3`).(*Select)
	want := or(
		cpred("a", OpEQ, int64(1)),
		and(cpred("b", OpEQ, int64(2)), &Not{Expr: cpred("c", OpEQ, int64(3))}),
	)
	if !reflect.DeepEqual(s.Where, BoolExpr(want)) {
		t.Fatalf("got %#v", s.Where)
	}

	s = mustParse(t, `select * from t where (a = 1 or b = 2) and c = 3`).(*Select)
	want2 := and(
		or(cpred("a", OpEQ, int64(1)), cpred("b", OpEQ, int64(2))),
		cpred("c", OpEQ, int64(3)),
	)
	if !reflect.DeepEqual(s.Where, BoolExpr(want2)) {
		t.Fatalf("got %#v", s.Where)
	}

	// Same-operator chains flatten to one n-ary node.
	s = mustParse(t, `select * from t where a = 1 or b = 2 or c = 3`).(*Select)
	if len(s.Where.(*Or).Exprs) != 3 {
		t.Fatalf("got %#v", s.Where)
	}

	// NOT applies to a whole parenthesized expression, and stacks.
	s = mustParse(t, `select * from t where not (a = 1 and not b is null)`).(*Select)
	inner := s.Where.(*Not).Expr.(*And)
	if len(inner.Exprs) != 2 {
		t.Fatalf("got %#v", s.Where)
	}
	if _, ok := inner.Exprs[1].(*Not); !ok {
		t.Fatalf("got %#v", inner.Exprs[1])
	}
}

func TestParseDDLRest(t *testing.T) {
	if st := mustParse(t, `create unique index by_email on users (email)`); !reflect.DeepEqual(st,
		&CreateIndex{Name: "by_email", Table: "users", Unique: true, Cols: []string{"email"}}) {
		t.Fatalf("got %#v", st)
	}
	if st := mustParse(t, `drop index by_email on users`); !reflect.DeepEqual(st,
		&DropIndex{Name: "by_email", Table: "users"}) {
		t.Fatalf("got %#v", st)
	}
	if st := mustParse(t, `alter table users add column email text`); !reflect.DeepEqual(st,
		&AddColumn{Table: "users", Col: ColDef{"email", bytdb.TString}}) {
		t.Fatalf("got %#v", st)
	}
	if st := mustParse(t, `alter table users drop email`); !reflect.DeepEqual(st,
		&DropColumn{Table: "users", Col: "email"}) {
		t.Fatalf("got %#v", st)
	}
	if st := mustParse(t, `drop table users`); !reflect.DeepEqual(st, &DropTable{Table: "users"}) {
		t.Fatalf("got %#v", st)
	}
}

func TestParseErrors(t *testing.T) {
	for _, src := range []string{
		`select * from t where a = null`,       // must use IS NULL
		`select * from t where a = b`,          // column-column compare
		`select * from t where 1 = 2`,          // literal-literal compare
		`select * from t where a = $1`,         // placeholders not yet supported
		`select * from t limit -1`,             // negative limit
		`select * from t; select * from t`,     // one statement per call
		`insert into t (a, a) values (1, 2)`,   // duplicate column
		`update t set a = 1, a = 2`,            // duplicate SET column
		`alter table t add c int primary key`,  // can't add a pk column
		`bogus`,                                // not a statement
	} {
		if _, err := Parse(src); err == nil {
			t.Errorf("Parse(%q): expected error", src)
		}
	}
}

func TestParseQuotedIdents(t *testing.T) {
	s := mustParse(t, `select "Weird Col" from "MyTable" where "true" = 1`).(*Select)
	if s.Table != "MyTable" || s.Items[0].Col != "Weird Col" || s.Where.(*Pred).Item.Col != "true" {
		t.Fatalf("got %#v", s)
	}
}

func TestParseAggregates(t *testing.T) {
	st := mustParse(t, `SELECT city, count(*), avg(age) FROM users
		WHERE age > 18 GROUP BY city HAVING count(*) >= 2 AND min(age) IS NOT NULL
		ORDER BY count(*) DESC, city LIMIT 3`)
	want := &Select{
		Table: "users",
		Items: []SelectItem{
			{Col: "city"},
			{Agg: AggCount, Star: true},
			{Agg: AggAvg, Col: "age"},
		},
		Where:   cpred("age", OpGT, int64(18)),
		GroupBy: []string{"city"},
		Having: and(
			apred(SelectItem{Agg: AggCount, Star: true}, OpGE, int64(2)),
			apred(SelectItem{Agg: AggMin, Col: "age"}, OpIsNotNull, nil),
		),
		OrderBy: []OrderItem{
			{SelectItem: SelectItem{Agg: AggCount, Star: true}, Desc: true},
			{SelectItem: SelectItem{Col: "city"}},
		},
		Limit: 3,
	}
	if !reflect.DeepEqual(st, want) {
		t.Fatalf("got %#v", st)
	}
	if !st.(*Select).isAggregate() {
		t.Fatal("should be an aggregate query")
	}

	// A column that shares an aggregate's name stays a column without
	// parentheses.
	s := mustParse(t, `select count from t group by count`).(*Select)
	if s.Items[0].Agg != AggNone || s.Items[0].Col != "count" {
		t.Fatalf("got %#v", s.Items[0])
	}
}

func TestParseAggregateErrors(t *testing.T) {
	for _, src := range []string{
		`select * from t where count(*) > 1`,       // aggregates belong in HAVING
		`select count(*) from t having sum(v) = null`, // must use IS NULL
		`select sum() from t`,                      // missing argument
		`select sum(*) from t`,                     // * only for count
	} {
		if _, err := Parse(src); err == nil {
			t.Errorf("Parse(%q): expected error", src)
		}
	}
}
