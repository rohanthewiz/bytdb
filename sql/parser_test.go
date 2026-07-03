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
		Cols:  []string{"name", "age"},
		Where: []Pred{
			{Col: "age", Op: OpGE, Val: int64(21)},
			{Col: "score", Op: OpLT, Val: int64(100)}, // flipped
			{Col: "city", Op: OpEQ, Val: "Reno"},
			{Col: "note", Op: OpIsNotNull},
		},
		OrderBy: []OrderItem{{Col: "age", Desc: true}, {Col: "name"}},
		Limit:   10,
		Offset:  5,
	}
	if !reflect.DeepEqual(st, want) {
		t.Fatalf("got %#v", st)
	}

	st = mustParse(t, `select * from t where a is null offset 2 limit 3`)
	s := st.(*Select)
	if !s.Star || s.Limit != 3 || s.Offset != 2 || s.Where[0].Op != OpIsNull {
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
		!reflect.DeepEqual(u.Where, []Pred{{Col: "id", Op: OpEQ, Val: int64(7)}}) {
		t.Fatalf("got %#v", u)
	}
	d := mustParse(t, `delete from users where id != 3`).(*Delete)
	if d.Where[0].Op != OpNE {
		t.Fatalf("got %#v", d)
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
	if s.Table != "MyTable" || s.Cols[0] != "Weird Col" || s.Where[0].Col != "true" {
		t.Fatalf("got %#v", s)
	}
}
