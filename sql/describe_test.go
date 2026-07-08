package sql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func describe(t *testing.T, d *DB, q string) *StmtInfo {
	t.Helper()
	st, err := d.Prepare(q)
	if err != nil {
		t.Fatalf("Prepare(%q): %v", q, err)
	}
	info, err := st.Describe()
	if err != nil {
		t.Fatalf("Describe(%q): %v", q, err)
	}
	return info
}

func TestDescribe(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	exec(t, d, `create table orders (id int primary key, user_id int, total float, paid bool, memo bytea)`)

	cases := []struct {
		q       string
		command string
		params  []bytdb.ColType
		cols    []string
		types   []bytdb.ColType
	}{
		{
			q:       `select name, age from users where city = $1 and age > $2`,
			command: "SELECT",
			params:  []bytdb.ColType{bytdb.TString, bytdb.TInt},
			cols:    []string{"name", "age"},
			types:   []bytdb.ColType{bytdb.TString, bytdb.TInt},
		},
		{
			q:       `select * from users`,
			command: "SELECT",
			params:  []bytdb.ColType{},
			cols:    []string{"id", "name", "age", "city"},
			types:   []bytdb.ColType{bytdb.TInt, bytdb.TString, bytdb.TInt, bytdb.TString},
		},
		{
			// Placeholders in join ON (prefix scope) and WHERE; output
			// across both tables.
			q:       `select u.name, o.total from users u join orders o on o.user_id = u.id and o.total > $1 where u.city = $2`,
			command: "SELECT",
			params:  []bytdb.ColType{bytdb.TFloat, bytdb.TString},
			cols:    []string{"name", "total"},
			types:   []bytdb.ColType{bytdb.TString, bytdb.TFloat},
		},
		{
			// Aggregate output typing and a HAVING placeholder against
			// an aggregate: COUNT -> int, AVG -> float, SUM -> arg type.
			q:       `select city, count(*), avg(age), sum(age) from users group by city having count(*) >= $1`,
			command: "SELECT",
			params:  []bytdb.ColType{bytdb.TInt},
			cols:    []string{"city", "count(*)", "avg(age)", "sum(age)"},
			types:   []bytdb.ColType{bytdb.TString, bytdb.TInt, bytdb.TFloat, bytdb.TInt},
		},
		{
			// INSERT with explicit column list, out of declared order.
			q:       `insert into orders (total, id, paid) values ($1, $2, $3)`,
			command: "INSERT",
			params:  []bytdb.ColType{bytdb.TFloat, bytdb.TInt, bytdb.TBool},
		},
		{
			// INSERT in declared order, params mixed with literals.
			q:       `insert into orders values (1, $1, 9.5, true, $2)`,
			command: "INSERT",
			params:  []bytdb.ColType{bytdb.TInt, bytdb.TBytes},
		},
		{
			q:       `update users set city = $1, age = $2 where id = $3`,
			command: "UPDATE",
			params:  []bytdb.ColType{bytdb.TString, bytdb.TInt, bytdb.TInt},
		},
		{
			// A placeholder inside a SET expression infers as the target
			// column's type.
			q:       `update users set age = age + $1 where id = $2`,
			command: "UPDATE",
			params:  []bytdb.ColType{bytdb.TInt, bytdb.TInt},
		},
		{
			q:       `delete from users where age < $1`,
			command: "DELETE",
			params:  []bytdb.ColType{bytdb.TInt},
		},
		{
			q:       `create table t2 (a int primary key)`,
			command: "CREATE TABLE",
			params:  []bytdb.ColType{},
		},
		{
			// A skipped placeholder stays untyped; repeats keep the
			// first inference.
			q:       `select id from users where name = $2 or city = $2`,
			command: "SELECT",
			params:  []bytdb.ColType{"", bytdb.TString},
			cols:    []string{"id"},
			types:   []bytdb.ColType{bytdb.TInt},
		},
	}
	for _, c := range cases {
		info := describe(t, d, c.q)
		if info.Command != c.command {
			t.Errorf("%s: command %q, want %q", c.q, info.Command, c.command)
		}
		if !reflect.DeepEqual(info.Params, c.params) && !(len(info.Params) == 0 && len(c.params) == 0) {
			t.Errorf("%s: params %v, want %v", c.q, info.Params, c.params)
		}
		if !reflect.DeepEqual(info.Cols, c.cols) && !(len(info.Cols) == 0 && len(c.cols) == 0) {
			t.Errorf("%s: cols %v, want %v", c.q, info.Cols, c.cols)
		}
		if !reflect.DeepEqual(info.Types, c.types) && !(len(info.Types) == 0 && len(c.types) == 0) {
			t.Errorf("%s: types %v, want %v", c.q, info.Types, c.types)
		}
	}
}

func TestDescribeErrors(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	for _, c := range []struct{ q, want string }{
		{`select * from missing`, "no such table"},
		{`select nope from users`, "no such column"},
		{`insert into users (id, nope) values ($1, $2)`, "no such column"},
		{`update users set nope = $1`, "no such column"},
		{`update users set nope = age + $1`, "no such column"},
		{`select city from users group by city having sum(name) > $1`, "numeric"},
	} {
		st, err := d.Prepare(c.q)
		if err != nil {
			t.Fatalf("Prepare(%q): %v", c.q, err)
		}
		if _, err := st.Describe(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Describe(%q): err %v, want containing %q", c.q, err, c.want)
		}
	}
}
