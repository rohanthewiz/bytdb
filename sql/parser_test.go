package sql

import (
	"reflect"
	"strings"
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
func col(name string) ColRef { return ColRef{Name: name} }

func cpred(name string, op PredOp, val any) *Pred {
	return &Pred{Item: SelectItem{Col: col(name)}, Op: op, Val: val}
}

// ccpred is a column-vs-column predicate: l op r.
func ccpred(l ColRef, op PredOp, r ColRef) *Pred {
	return &Pred{Item: SelectItem{Col: l}, Op: op, RItem: &SelectItem{Col: r}}
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
			{Name: "id", Type: bytdb.TInt}, {Name: "name", Type: bytdb.TString, MaxLen: 40}, {Name: "score", Type: bytdb.TFloat},
			{Name: "active", Type: bytdb.TBool}, {Name: "blob", Type: bytdb.TBytes},
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
		From:  []FromItem{{Table: "users"}},
		Items: []SelectItem{{Col: col("name")}, {Col: col("age")}},
		Where: and(
			cpred("age", OpGE, int64(21)),
			cpred("score", OpLT, int64(100)), // flipped
			cpred("city", OpEQ, "Reno"),
			cpred("note", OpIsNotNull, nil),
		),
		OrderBy: []OrderItem{
			{SelectItem: SelectItem{Col: col("age")}, Desc: true},
			{SelectItem: SelectItem{Col: col("name")}},
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

func TestParseJoins(t *testing.T) {
	st := mustParse(t, `SELECT u.name, o.total FROM users AS u
		JOIN orders o ON u.id = o.user_id
		LEFT OUTER JOIN notes ON notes.order_id = o.id AND notes.kind = 'memo'
		CROSS JOIN tags
		WHERE u.age > 21 ORDER BY o.total DESC`)
	uid, ouid := ColRef{Table: "u", Name: "id"}, ColRef{Table: "o", Name: "user_id"}
	noid := ColRef{Table: "notes", Name: "order_id"}
	oid := ColRef{Table: "o", Name: "id"}
	want := &Select{
		From: []FromItem{
			{Table: "users", Alias: "u"},
			{Table: "orders", Alias: "o", Join: JoinInner, On: ccpred(uid, OpEQ, ouid)},
			{Table: "notes", Join: JoinLeft, On: and(
				ccpred(noid, OpEQ, oid),
				&Pred{Item: SelectItem{Col: ColRef{Table: "notes", Name: "kind"}}, Op: OpEQ, Val: "memo"},
			)},
			{Table: "tags", Join: JoinCross},
		},
		Items: []SelectItem{
			{Col: ColRef{Table: "u", Name: "name"}},
			{Col: ColRef{Table: "o", Name: "total"}},
		},
		Where: &Pred{Item: SelectItem{Col: ColRef{Table: "u", Name: "age"}}, Op: OpGT, Val: int64(21)},
		OrderBy: []OrderItem{
			{SelectItem: SelectItem{Col: ColRef{Table: "o", Name: "total"}}, Desc: true},
		},
		Limit: -1,
	}
	if !reflect.DeepEqual(st, want) {
		t.Fatalf("got %#v", st)
	}

	// A comma is a cross join; t.* is a per-table star.
	s := mustParse(t, `select a.*, b.v from a, b where a.id = b.a_id`).(*Select)
	if len(s.From) != 2 || s.From[1].Join != JoinCross || s.From[1].On != nil {
		t.Fatalf("from: %#v", s.From)
	}
	if !s.Items[0].Star || s.Items[0].Col.Table != "a" || s.Items[0].Agg != AggNone {
		t.Fatalf("a.*: %#v", s.Items[0])
	}
	if pr := s.Where.(*Pred); pr.RItem == nil || pr.RItem.Col.Table != "b" {
		t.Fatalf("where: %#v", s.Where)
	}

	// A keyword can't be a bare alias; quoting or AS makes it one.
	s = mustParse(t, `select * from t where a = 1`).(*Select)
	if s.From[0].Alias != "" {
		t.Fatalf("alias: %q", s.From[0].Alias)
	}
	if mustParse(t, `select * from t as "where"`).(*Select).From[0].Alias != "where" {
		t.Fatal("AS alias failed")
	}

	for _, src := range []string{
		`select * from a right join b on a.id = b.id`, // unsupported join
		`select * from a full join b on a.id = b.id`,
		`select * from a join b`,                      // missing ON
		`select * from a cross join b on a.id = b.id`, // stray ON
	} {
		if _, err := Parse(src); err == nil {
			t.Errorf("Parse(%q): expected error", src)
		}
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
		&AddColumn{Table: "users", Col: ColDef{Name: "email", Type: bytdb.TString}}) {
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
	// A literal-literal comparison (WHERE 1 = 2) evaluates per row now.
	ll := mustParse(t, `select * from t where 1 = 2`).(*Select)
	if _, ok := ll.Where.(*Cond); !ok {
		t.Fatalf("got %#v", ll.Where)
	}

	for _, src := range []string{
		`select * from t where a = null`,      // must use IS NULL
		`select * from t limit -1`,            // negative limit
		`select * from t; select * from t`,    // one statement per call
		`insert into t (a, a) values (1, 2)`,  // duplicate column
		`update t set a = 1, a = 2`,           // duplicate SET column
		`alter table t add c int primary key`, // can't add a pk column
		`bogus`,                               // not a statement
	} {
		if _, err := Parse(src); err == nil {
			t.Errorf("Parse(%q): expected error", src)
		}
	}
}

func TestParseParams(t *testing.T) {
	// Placeholders parse wherever a literal may appear; a literal-first
	// comparison still flips.
	s := mustParse(t, `select * from t where a = $1 and $2 < b`).(*Select)
	want := and(cpred("a", OpEQ, Param(1)), cpred("b", OpGT, Param(2)))
	if !reflect.DeepEqual(s.Where, BoolExpr(want)) {
		t.Fatalf("got %#v", s.Where)
	}

	ins := mustParse(t, `insert into t values ($1, $2), ($3, 'x')`).(*Insert)
	if !reflect.DeepEqual(ins.Rows, [][]any{{Param(1), Param(2)}, {Param(3), "x"}}) {
		t.Fatalf("got %#v", ins.Rows)
	}

	u := mustParse(t, `update t set a = $2 where b = $1`).(*Update)
	if u.Set["a"] != Param(2) || !reflect.DeepEqual(u.Where, BoolExpr(cpred("b", OpEQ, Param(1)))) {
		t.Fatalf("got %#v", u)
	}

	h := mustParse(t, `select count(*) from t having count(*) > $1`).(*Select)
	if !reflect.DeepEqual(h.Having, BoolExpr(apred(SelectItem{Agg: AggCount, Star: true}, OpGT, Param(1)))) {
		t.Fatalf("got %#v", h.Having)
	}

	// A negated param is a general expression now: 0 - $1.
	neg := mustParse(t, `select * from t where a = -$1`).(*Select)
	if _, ok := neg.Where.(*Cond); !ok {
		t.Fatalf("got %#v", neg.Where)
	}

	for _, src := range []string{
		`select * from t where a = $0`, // params are 1-based
		`select * from t limit $1`,     // LIMIT takes a literal count
		`select * from t offset $1`,
	} {
		if _, err := Parse(src); err == nil {
			t.Errorf("Parse(%q): expected error", src)
		}
	}
}

func TestParseQuotedIdents(t *testing.T) {
	s := mustParse(t, `select "Weird Col" from "MyTable" where "true" = 1`).(*Select)
	if s.From[0].Table != "MyTable" || s.Items[0].Col.Name != "Weird Col" || s.Where.(*Pred).Item.Col.Name != "true" {
		t.Fatalf("got %#v", s)
	}
}

func TestParseAggregates(t *testing.T) {
	st := mustParse(t, `SELECT city, count(*), avg(age) FROM users
		WHERE age > 18 GROUP BY city HAVING count(*) >= 2 AND min(age) IS NOT NULL
		ORDER BY count(*) DESC, city LIMIT 3`)
	want := &Select{
		From: []FromItem{{Table: "users"}},
		Items: []SelectItem{
			{Col: col("city")},
			{Agg: AggCount, Star: true},
			{Agg: AggAvg, Col: col("age")},
		},
		Where:   cpred("age", OpGT, int64(18)),
		GroupBy: []GroupItem{{Col: col("city")}},
		Having: and(
			apred(SelectItem{Agg: AggCount, Star: true}, OpGE, int64(2)),
			apred(SelectItem{Agg: AggMin, Col: col("age")}, OpIsNotNull, nil),
		),
		OrderBy: []OrderItem{
			{SelectItem: SelectItem{Agg: AggCount, Star: true}, Desc: true},
			{SelectItem: SelectItem{Col: col("city")}},
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
	if s.Items[0].Agg != AggNone || s.Items[0].Col.Name != "count" {
		t.Fatalf("got %#v", s.Items[0])
	}

	// GROUP BY ordinals parse as select-list positions, mixable with
	// columns.
	s = mustParse(t, `select city, age, count(*) from t group by 1, age`).(*Select)
	if !reflect.DeepEqual(s.GroupBy, []GroupItem{{Pos: 1}, {Col: col("age")}}) {
		t.Fatalf("got %#v", s.GroupBy)
	}

	// GROUP BY expressions stay expressions; an aggregate over an
	// expression stays an expression item carrying the ExAgg.
	s = mustParse(t, `select sum(age * 2) from t group by age / 10, upper(city)`).(*Select)
	if len(s.GroupBy) != 2 || s.GroupBy[0].Ex == nil || s.GroupBy[1].Ex == nil {
		t.Fatalf("got %#v", s.GroupBy)
	}
	agg, ok := s.Items[0].Ex.(*ExAgg)
	if !ok || agg.Fn != AggSum || agg.Arg == nil {
		t.Fatalf("got %#v", s.Items[0])
	}
}

func TestParseAggregateErrors(t *testing.T) {
	for _, src := range []string{
		`select * from t where count(*) > 1`,          // aggregates belong in HAVING
		`select count(*) from t having sum(v) = null`, // must use IS NULL
		`select sum() from t`,                         // missing argument
		`select sum(*) from t`,                        // * only for count
		`select city from t group by 1.5`,             // non-integer constant
		`select city from t group by 'city'`,          // non-integer constant
		`select city from t group by 0`,               // position out of range
		`select sum(count(*)) from t`,                 // nested aggregates
	} {
		if _, err := Parse(src); err == nil {
			t.Errorf("Parse(%q): expected error", src)
		}
	}
}

// TestParseDepthLimit locks in the recursion guard: pathologically
// nested input must come back as a normal parse error, never a fatal
// stack overflow (which recover() cannot catch — it would kill any
// server embedding the parser).
func TestParseDepthLimit(t *testing.T) {
	deep := func(n int, open, mid, close string) string {
		return "select " + strings.Repeat(open, n) + mid + strings.Repeat(close, n)
	}
	// 100k levels of each self-nesting construct; all must error.
	for _, src := range []string{
		deep(100_000, "(", "1", ")"),                       // parens: expression -> exprPrimary cycle
		deep(100_000, "(select ", "1", ")"),                // scalar subqueries
		deep(100_000, "not ", "true", ""),                  // NOT chains: exprNot self-recursion
		deep(100_000, "case when true then ", "1", " end"), // CASE nesting
		deep(100_000, "-(", "1", ")"),                      // unary minus + parens
		deep(100_000, "abs(", "1", ")"),                    // function-call nesting
	} {
		if _, err := Parse(src); err == nil {
			t.Errorf("Parse(%.40q...): expected a depth error", src)
		}
	}
	// Reasonable nesting still parses.
	for _, src := range []string{
		deep(500, "(", "1", ")"),
		deep(500, "not ", "true", ""),
	} {
		if _, err := Parse(src); err != nil {
			t.Errorf("Parse(%.40q...): unexpected error: %v", src, err)
		}
	}
}

// TestParamNumberLimit locks in the $n cap: the statement's parameter
// count is its highest $n, and Describe allocates one slot per
// parameter, so an unbounded $n turns a tiny query into an OOM-fatal
// terabyte allocation. (Found by FuzzMessageParse: `select
// $100000000000`.)
func TestParamNumberLimit(t *testing.T) {
	for _, src := range []string{
		"select $100000000000",
		"select $65536",
		"select 1 where 1 = $99999999999999999999", // overflows int too
	} {
		if _, err := Parse(src); err == nil {
			t.Errorf("Parse(%q): expected a parameter-number error", src)
		}
	}
	// The cap itself is usable.
	st, err := Parse("select $65535")
	if err != nil {
		t.Fatalf("Parse($65535): %v", err)
	}
	if n := numParams(st); n != 65535 {
		t.Fatalf("numParams = %d; want 65535", n)
	}
}
