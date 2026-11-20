package sql

// Drops that follow dependencies the way Postgres's do (N-056). Each
// scenario is one run against PG16 (docker postgres:16) when this was
// built. The notices and refusal texts here are what PG16 printed for
// the same statements, except where noted: Postgres folds several
// cascaded objects into one "drop cascades to N other objects" notice
// with a DETAIL, and bytdb sends one notice per object.

import (
	"errors"
	"slices"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// indexNames lists a table's secondary indexes.
func indexNames(d *DB, table string) []string {
	var out []string
	for _, ix := range d.e.Table(table).Indexes {
		out = append(out, ix.Name)
	}
	slices.Sort(out)
	return out
}

// constraintNames lists a table's CHECK and foreign-key constraints.
func constraintNames(d *DB, table string) []string {
	desc := d.e.Table(table)
	var out []string
	for _, ck := range desc.Checks {
		out = append(out, ck.Name)
	}
	for _, fk := range desc.ForeignKeys {
		out = append(out, fk.Name)
	}
	slices.Sort(out)
	return out
}

// TestDropColumnTakesDependents: DROP COLUMN drops, with no CASCADE,
// every index that includes the column (multi-column and unique ones
// too), every CHECK that mentions it (multi-column ones too), and the
// table's own foreign keys over it. PG16 does the same silently.
func TestDropColumnTakesDependents(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, a int, b int, c int,
		check (a > 0), check (a < b), constraint b_pos check (b > 0))`)
	exec(t, d, `create index t_a_idx on t (a)`)
	exec(t, d, `create index t_ab_idx on t (b, a)`)
	exec(t, d, `create unique index t_ca_key on t (c, a)`)
	exec(t, d, `create unique index t_c_key on t (c)`)
	exec(t, d, `insert into t values (1, 1, 2, 3), (2, 2, 3, 4)`)

	if r := exec(t, d, `alter table t drop column a`); r.Notice != "" {
		t.Errorf("notice = %q; PG16 drops these silently", r.Notice)
	}
	if got := indexNames(d, "t"); !slices.Equal(got, []string{"t_c_key"}) {
		t.Errorf("indexes = %v", got)
	}
	if got := constraintNames(d, "t"); !slices.Equal(got, []string{"b_pos"}) {
		t.Errorf("constraints = %v", got)
	}
	// The survivors still work, at their shifted ordinals: t_c_key
	// enforces and serves lookups, b_pos still checks b.
	wantErr(t, d, `insert into t values (3, 5, 3)`, "duplicate key")
	wantErr(t, d, `insert into t values (3, -1, 9)`, `violates check constraint "b_pos"`)
	exec(t, d, `insert into t values (3, 5, 9)`)
	r := exec(t, d, `select id, b from t where c = 9`)
	if !slices.EqualFunc(r.Rows, [][]any{{int64(3), int64(5)}}, slices.Equal) {
		t.Errorf("lookup through t_c_key: %v", r.Rows)
	}

	// Foreign keys: a child column's own FK goes silently, single- or
	// multi-column (dropping y takes the (x, y) key).
	exec(t, d, `create table p (id int primary key, a int, b int, unique (a, b))`)
	exec(t, d, `create table ch (id int primary key, pid int references p (id), x int, y int,
		foreign key (x, y) references p (a, b))`)
	exec(t, d, `alter table ch drop column pid`)
	// One FK left: the (x, y) one, named from both its columns as
	// Postgres names it (N-060).
	if got := constraintNames(d, "ch"); !slices.Equal(got, []string{"ch_x_y_fkey"}) {
		t.Errorf("ch after dropping pid: %v", got)
	}
	exec(t, d, `alter table ch drop column y`)
	if got := constraintNames(d, "ch"); len(got) != 0 {
		t.Errorf("ch after dropping y: %v", got)
	}

	// A self-reference over the dropped column goes too.
	exec(t, d, `create table s (id int primary key, parent int references s (id))`)
	exec(t, d, `alter table s drop column parent`)
	if got := constraintNames(d, "s"); len(got) != 0 {
		t.Errorf("s: %v", got)
	}
}

// TestDropColumnReferenced: a column another foreign key references
// needs CASCADE, which drops the foreign key (and the unique index over
// the column) with Postgres's notice. That holds when the reference is
// from the same table as well.
func TestDropColumnReferenced(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table p (id int primary key, code int unique)`)
	exec(t, d, `create table ch (id int primary key, pc int references p (code))`)

	_, err := d.Exec(`alter table p drop column code`)
	if err == nil || err.Error() != "cannot drop column code of table p because other objects depend on it" ||
		!errors.Is(err, bytdb.ErrDependentObjects) {
		t.Fatalf("RESTRICT: %v", err)
	}
	r := exec(t, d, `alter table p drop column code cascade`)
	if r.Notice != "drop cascades to constraint ch_pc_fkey on table ch" {
		t.Errorf("notice = %q", r.Notice)
	}
	if got := constraintNames(d, "ch"); len(got) != 0 {
		t.Errorf("ch: %v", got)
	}
	if got := indexNames(d, "p"); len(got) != 0 {
		t.Errorf("p: %v", got)
	}
	exec(t, d, `insert into ch values (1, 999)`) // no longer enforced

	exec(t, d, `create table t (id int primary key, code int unique, pc int references t (code))`)
	wantErr(t, d, `alter table t drop column code`,
		"cannot drop column code of table t because other objects depend on it")
	if r := exec(t, d, `alter table t drop column code cascade`); r.Notice != "drop cascades to constraint t_pc_fkey on table t" {
		t.Errorf("self-reference notice = %q", r.Notice)
	}
	if got := constraintNames(d, "t"); len(got) != 0 {
		t.Errorf("t: %v", got)
	}

	// A column whose own FK references it: the FK depends on the column
	// both ways, and the automatic way wins, so no CASCADE is needed.
	exec(t, d, `create table u (id int primary key, a int unique references u (a))`)
	if r := exec(t, d, `alter table u drop column a`); r.Notice != "" {
		t.Errorf("notice = %q", r.Notice)
	}
	if got := constraintNames(d, "u"); len(got) != 0 {
		t.Errorf("u: %v", got)
	}

	// The primary key stays undroppable: a bytdb table always has one.
	wantErr(t, d, `alter table t drop column id cascade`, "cannot drop a primary key column")
}

// TestDropTableCascade: DROP TABLE refuses while another table's
// foreign key or a view depends on it. CASCADE drops the foreign keys
// (never the tables) and the views, transitively.
func TestDropTableCascade(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table p2 (id int primary key)`)
	exec(t, d, `create table c2 (id int primary key, pid int references p2 (id))`)
	exec(t, d, `create table c3 (id int primary key, pid int references p2 (id))`)
	exec(t, d, `create view v2 as select * from p2`)
	exec(t, d, `create view v3 as select * from v2`)

	_, err := d.Exec(`drop table p2`)
	if err == nil || err.Error() != "cannot drop table p2 because other objects depend on it" {
		t.Fatalf("RESTRICT: %v", err)
	}
	r := exec(t, d, `drop table p2 cascade`)
	want := "drop cascades to constraint c2_pid_fkey on table c2\n" +
		"drop cascades to constraint c3_pid_fkey on table c3\n" +
		"drop cascades to view v2\n" +
		"drop cascades to view v3"
	if r.Notice != want {
		t.Errorf("notice =\n%s\nwant\n%s", r.Notice, want)
	}
	if d.e.Table("c2") == nil || d.e.Table("c3") == nil {
		t.Fatal("CASCADE dropped a referencing table")
	}
	if len(d.e.Table("c2").ForeignKeys)+len(d.e.Table("c3").ForeignKeys) != 0 {
		t.Fatal("referencing FKs survived")
	}
	if d.e.View("v2") != nil || d.e.View("v3") != nil {
		t.Fatal("dependent views survived")
	}
	exec(t, d, `insert into c2 values (1, 42)`) // no longer enforced

	// A single dependent: Postgres's one-object notice.
	exec(t, d, `create table p3 (id int primary key)`)
	exec(t, d, `create table c4 (id int primary key, pid int references p3 (id))`)
	if r := exec(t, d, `drop table p3 cascade`); r.Notice != "drop cascades to constraint c4_pid_fkey on table c4" {
		t.Errorf("notice = %q", r.Notice)
	}

	// Parent and child together, in either order, need no CASCADE, and
	// a FK inside the set raises no notice.
	exec(t, d, `create table p4 (id int primary key)`)
	exec(t, d, `create table c5 (id int primary key, pid int references p4 (id))`)
	if r := exec(t, d, `drop table p4, c5`); r.Notice != "" {
		t.Errorf("notice = %q", r.Notice)
	}

	// A view alone blocks a drop, as in Postgres.
	exec(t, d, `create table p5 (id int primary key)`)
	exec(t, d, `create view v5 as select id from p5`)
	wantErr(t, d, `drop table p5`, "cannot drop table p5 because other objects depend on it")
	exec(t, d, `drop view v5`)
	exec(t, d, `drop table p5`)

	// IF EXISTS skips come first, then what CASCADE dropped.
	exec(t, d, `create table p6 (id int primary key)`)
	exec(t, d, `create table c6 (id int primary key, pid int references p6 (id))`)
	r = exec(t, d, `drop table if exists nope, p6 cascade`)
	if want := "table \"nope\" does not exist, skipping\n" +
		"drop cascades to constraint c6_pid_fkey on table c6"; r.Notice != want {
		t.Errorf("notice = %q", r.Notice)
	}

	// A self-referencing table drops alone.
	exec(t, d, `create table s2 (id int primary key, parent int references s2 (id))`)
	exec(t, d, `drop table s2`)
}

// TestDropTablesAllOrNothing: a multi-table DROP runs as one
// transaction. Before N-056 each table dropped in its own, so a refusal
// part-way left the earlier tables dropped.
func TestDropTablesAllOrNothing(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table a (id int primary key)`)
	exec(t, d, `create table p (id int primary key)`)
	exec(t, d, `create table c (id int primary key, pid int references p (id))`)
	_, err := d.Exec(`drop table a, p`)
	if err == nil || err.Error() != "cannot drop desired object(s) because other objects depend on them" {
		t.Fatalf("multi-table RESTRICT: %v", err)
	}
	if d.e.Table("a") == nil {
		t.Fatal("a was dropped by a refused statement")
	}
	// DROP TABLE a, a is one drop of a, as in Postgres.
	exec(t, d, `drop table a, a`)
}

// TestDropViewCascade: views that read a view block its drop; CASCADE
// drops them too.
func TestDropViewCascade(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table p (id int primary key)`)
	exec(t, d, `create view v6 as select id from p`)
	exec(t, d, `create view v7 as select id from v6`)
	exec(t, d, `create view "user" as select id from v7`)
	_, err := d.Exec(`drop view v6`)
	if err == nil || err.Error() != "cannot drop view v6 because other objects depend on it" {
		t.Fatalf("RESTRICT: %v", err)
	}
	r := exec(t, d, `drop view v6 cascade`)
	if want := "drop cascades to view v7\ndrop cascades to view \"user\""; r.Notice != want {
		t.Errorf("notice = %q", r.Notice)
	}
	if len(d.e.Views()) != 0 {
		t.Errorf("views left: %v", d.e.Views())
	}
	exec(t, d, `drop table p`) // nothing reads it now
}

// TestDropIndexCascade: DROP INDEX of the unique index a foreign key's
// referenced columns rely on needs CASCADE, which drops the foreign key.
func TestDropIndexCascade(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table p6 (id int primary key, code int)`)
	exec(t, d, `create unique index p6_code_idx on p6 (code)`)
	exec(t, d, `create table c6 (id int primary key, pc int references p6 (code))`)
	wantErr(t, d, `drop index p6_code_idx`, "cannot drop index p6_code_idx because other objects depend on it")
	if r := exec(t, d, `drop index p6_code_idx cascade`); r.Notice != "drop cascades to constraint c6_pc_fkey on table c6" {
		t.Errorf("notice = %q", r.Notice)
	}
	if len(d.e.Table("c6").ForeignKeys) != 0 || d.e.Table("p6").Index("p6_code_idx") != nil {
		t.Fatal("index or FK survived")
	}
}

// TestViewReads: what a view depends on is the relations its stored
// query reads, at any depth, less the names its own WITH entries bind.
func TestViewReads(t *testing.T) {
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{`select * from a join b on a.id = b.id`, []string{"a", "b"}},
		{`select * from a where id in (select id from b)`, []string{"a", "b"}},
		{`select (select max(id) from b) from a union select id from c`, []string{"a", "b", "c"}},
		// A CTE named like a table shadows it, so the view reads no p.
		{`with p as (select 1 as id) select * from p`, nil},
		// A CTE's own body is read, and a CTE may read another.
		{`with q as (select id from b), r as (select id from q) select * from r join a using (id)`, []string{"a", "b"}},
		{`select * from (select id from a) s`, []string{"a"}},
		{`not sql at all`, nil},
	} {
		got, err := viewReads(tc.q)
		if err != nil {
			t.Fatalf("%s: %v", tc.q, err)
		}
		slices.Sort(got)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.q, got, tc.want)
		}
	}

	// End to end: the shadowing view does not block a drop of p.
	d := openDB(t)
	exec(t, d, `create table p (id int primary key)`)
	exec(t, d, `create view v as with p as (select 1 as id) select * from p`)
	exec(t, d, `drop table p`)
	if d.e.View("v") == nil {
		t.Fatal("a view that never read p was dropped")
	}
}

// TestCheckReads: the columns a CHECK mentions, bare or qualified by its
// own table.
func TestCheckReads(t *testing.T) {
	got, err := checkReads("t", `(a > 0) AND (t.b < coalesce(c, 1))`)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("got %v", got)
	}
	if _, err := checkReads("t", `a >`); err == nil {
		t.Error("an unparsable CHECK must fail the drop, not be skipped")
	}
}

// TestDropRestrictKeyword: RESTRICT is accepted and is the default: it
// refuses a drop something depends on and runs any other.
func TestDropRestrictKeyword(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table p (id int primary key)`)
	exec(t, d, `create table c (id int primary key, pid int references p (id))`)
	exec(t, d, `create view v as select id from p`)
	if _, err := d.Exec(`drop table p restrict`); !errors.Is(err, bytdb.ErrDependentObjects) {
		t.Errorf("drop table p restrict: %v", err)
	}
	exec(t, d, `drop view v restrict`)                   // nothing reads v
	exec(t, d, `alter table c drop column pid restrict`) // its own FK goes with it
	exec(t, d, `drop table p restrict`)
}
