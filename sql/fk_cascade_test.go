package sql

// ON DELETE CASCADE tests: direct cascades, transitive chains,
// self-referencing trees, FK cycles, mixed actions on one parent, the
// end-of-statement NO ACTION check over cascaded rows, statement
// accounting (RowsAffected/RETURNING exclude cascaded rows), parse
// rejections for the unsupported actions, and catalog reporting.

import (
	"reflect"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func count(t *testing.T, d *DB, table string) int64 {
	t.Helper()
	res := exec(t, d, `select count(*) from `+table)
	return res.Rows[0][0].(int64)
}

func TestFKCascadeBasic(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table users (id int primary key, name text)`)
	exec(t, d, `create table orders (
		id int primary key,
		user_id int references users on delete cascade,
		note text
	)`)
	exec(t, d, `insert into users values (1, 'ada'), (2, 'grace')`)
	exec(t, d, `insert into orders values
		(10, 1, 'a'), (11, 1, 'b'), (12, 2, 'kept'), (13, null, 'orphan by design')`)

	// Only the targeted parent row counts / returns; its two orders go
	// with it. The other parent's order and the NULL-FK order survive.
	res := exec(t, d, `delete from users where id = 1 returning name`)
	if res.RowsAffected != 1 || !reflect.DeepEqual(res.Rows, [][]any{{"ada"}}) {
		t.Fatalf("delete: affected=%d rows=%v", res.RowsAffected, res.Rows)
	}
	if got := count(t, d, "orders"); got != 2 {
		t.Fatalf("orders after cascade: %d", got)
	}
	res = exec(t, d, `select id from orders order by 1`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(12)}, {int64(13)}}) {
		t.Fatalf("surviving orders: %v", res.Rows)
	}
}

func TestFKCascadeChain(t *testing.T) {
	d := openDB(t)
	// a <- b <- c, cascade at both hops: deleting from a empties all
	// three levels transitively.
	exec(t, d, `create table a (id int primary key)`)
	exec(t, d, `create table b (id int primary key, a_id int references a on delete cascade)`)
	exec(t, d, `create table c (id int primary key, b_id int references b on delete cascade)`)
	exec(t, d, `insert into a values (1), (2)`)
	exec(t, d, `insert into b values (10, 1), (11, 1), (12, 2)`)
	exec(t, d, `insert into c values (100, 10), (101, 11), (102, 12)`)

	res := exec(t, d, `delete from a where id = 1`)
	if res.RowsAffected != 1 {
		t.Fatalf("affected: %d", res.RowsAffected)
	}
	if b, c := count(t, d, "b"), count(t, d, "c"); b != 1 || c != 1 {
		t.Fatalf("after chain cascade: b=%d c=%d", b, c)
	}
}

func TestFKCascadeGrandchildRestrict(t *testing.T) {
	d := openDB(t)
	// The cascade would delete b's row, but c still references it with
	// NO ACTION — the end-of-statement check over cascaded rows blocks
	// the whole statement, and nothing is deleted.
	exec(t, d, `create table a (id int primary key)`)
	exec(t, d, `create table b (id int primary key, a_id int references a on delete cascade)`)
	exec(t, d, `create table c (id int primary key, b_id int references b)`)
	exec(t, d, `insert into a values (1)`)
	exec(t, d, `insert into b values (10, 1)`)
	exec(t, d, `insert into c values (100, 10)`)

	wantErr(t, d, `delete from a where id = 1`,
		`update or delete on table "b" violates foreign key constraint "c_b_id_fkey" on table "c"`)
	if a, b := count(t, d, "a"), count(t, d, "b"); a != 1 || b != 1 {
		t.Fatalf("blocked cascade still deleted: a=%d b=%d", a, b)
	}

	// Once the grandchild is gone the same delete cascades cleanly.
	exec(t, d, `delete from c where id = 100`)
	exec(t, d, `delete from a where id = 1`)
	if b := count(t, d, "b"); b != 0 {
		t.Fatalf("b after cascade: %d", b)
	}
}

func TestFKCascadeMixedActions(t *testing.T) {
	d := openDB(t)
	// One parent, two children: logs cascades, orders refuses. The
	// refusing child keeps its veto regardless of the cascading one.
	exec(t, d, `create table users (id int primary key)`)
	exec(t, d, `create table logs (id int primary key, user_id int references users on delete cascade)`)
	exec(t, d, `create table orders (id int primary key, user_id int references users)`)
	exec(t, d, `insert into users values (1), (2)`)
	exec(t, d, `insert into logs values (10, 1), (11, 2)`)
	exec(t, d, `insert into orders values (20, 1)`)

	wantErr(t, d, `delete from users where id = 1`, "violates foreign key constraint")
	if u, l := count(t, d, "users"), count(t, d, "logs"); u != 2 || l != 2 {
		t.Fatalf("blocked delete mutated: users=%d logs=%d", u, l)
	}

	// User 2 has only the cascading reference.
	exec(t, d, `delete from users where id = 2`)
	res := exec(t, d, `select id from logs order by 1`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(10)}}) {
		t.Fatalf("logs: %v", res.Rows)
	}
}

func TestFKCascadeSelfRefTree(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table nodes (
		id int primary key,
		parent_id int references nodes on delete cascade,
		label text
	)`)
	// A root, a subtree under it, and a sibling tree that must survive.
	exec(t, d, `insert into nodes values
		(1, null, 'root'), (2, 1, 'child'), (3, 2, 'grandchild'), (4, 3, 'great'),
		(5, null, 'other root'), (6, 5, 'other child')`)

	res := exec(t, d, `delete from nodes where id = 1`)
	if res.RowsAffected != 1 {
		t.Fatalf("affected: %d", res.RowsAffected)
	}
	rows := exec(t, d, `select id from nodes order by 1`)
	if !reflect.DeepEqual(rows.Rows, [][]any{{int64(5)}, {int64(6)}}) {
		t.Fatalf("surviving nodes: %v", rows.Rows)
	}

	// A row that is its own parent deletes without looping.
	exec(t, d, `insert into nodes values (7, 7, 'self')`)
	exec(t, d, `delete from nodes where id = 7`)
	if got := count(t, d, "nodes"); got != 2 {
		t.Fatalf("after self-parent delete: %d", got)
	}
}

func TestFKCascadeCycle(t *testing.T) {
	d := openDB(t)
	// Mutual cascade between two tables. The cycle is built through
	// nullable FKs and an UPDATE (neither row can insert first
	// otherwise); deleting either side must remove both and terminate.
	exec(t, d, `create table a (id int primary key, b_id int)`)
	exec(t, d, `create table b (id int primary key, a_id int references a on delete cascade)`)
	exec(t, d, `alter table a add foreign key (b_id) references b on delete cascade`)
	exec(t, d, `insert into a values (1, null)`)
	exec(t, d, `insert into b values (2, 1)`)
	exec(t, d, `update a set b_id = 2 where id = 1`)

	exec(t, d, `delete from a where id = 1`)
	if ca, cb := count(t, d, "a"), count(t, d, "b"); ca != 0 || cb != 0 {
		t.Fatalf("after cycle cascade: a=%d b=%d", ca, cb)
	}
}

func TestFKCascadeDiamond(t *testing.T) {
	d := openDB(t)
	// b and c both cascade from a; d references both. The d row is
	// reached twice — the second probe must tolerate it being gone.
	exec(t, d, `create table a (id int primary key)`)
	exec(t, d, `create table b (id int primary key, a_id int references a on delete cascade)`)
	exec(t, d, `create table c (id int primary key, a_id int references a on delete cascade)`)
	exec(t, d, `create table d (
		id int primary key,
		b_id int references b on delete cascade,
		c_id int references c on delete cascade
	)`)
	exec(t, d, `insert into a values (1)`)
	exec(t, d, `insert into b values (10, 1)`)
	exec(t, d, `insert into c values (20, 1)`)
	exec(t, d, `insert into d values (30, 10, 20)`)

	exec(t, d, `delete from a where id = 1`)
	for _, tbl := range []string{"a", "b", "c", "d"} {
		if got := count(t, d, tbl); got != 0 {
			t.Fatalf("%s after diamond cascade: %d", tbl, got)
		}
	}
}

func TestFKCascadeMultiColumn(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table parents (x int, y int, primary key (x, y))`)
	exec(t, d, `create table kids (
		id int primary key, px int, py int,
		foreign key (px, py) references parents on delete cascade
	)`)
	exec(t, d, `insert into parents values (1, 1), (1, 2)`)
	// MATCH SIMPLE: the partially-NULL tuple references nothing and
	// must survive any parent delete.
	exec(t, d, `insert into kids values (10, 1, 1), (11, 1, 2), (12, 1, null)`)

	exec(t, d, `delete from parents where x = 1 and y = 1`)
	res := exec(t, d, `select id from kids order by 1`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(11)}, {int64(12)}}) {
		t.Fatalf("kids: %v", res.Rows)
	}
}

func TestFKCascadeUpdateStillRefused(t *testing.T) {
	d := openDB(t)
	// ON DELETE CASCADE says nothing about UPDATE: changing a
	// referenced key keeps refusing while children point at it.
	exec(t, d, `create table users (id int primary key)`)
	exec(t, d, `create table orders (id int primary key, user_id int references users on delete cascade)`)
	exec(t, d, `insert into users values (1)`)
	exec(t, d, `insert into orders values (10, 1)`)

	wantErr(t, d, `update users set id = 5 where id = 1`,
		"violates foreign key constraint")
}

func TestFKCascadeParseRejections(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table p (id int primary key)`)

	wantErr(t, d, `create table c1 (id int primary key, p_id int references p on update cascade)`,
		"ON UPDATE CASCADE is not supported")
	wantErr(t, d, `create table c2 (id int primary key, p_id int references p on delete set null)`,
		"ON DELETE SET NULL/DEFAULT is not supported")
	wantErr(t, d, `create table c3 (id int primary key, p_id int references p on delete set default)`,
		"ON DELETE SET NULL/DEFAULT is not supported")
}

func TestFKCascadeCatalog(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table p (id int primary key)`)
	exec(t, d, `create table c (
		id int primary key,
		cas int references p on delete cascade,
		noact int references p
	)`)

	res := exec(t, d, `select conname, confdeltype, confupdtype,
		pg_get_constraintdef(oid) from pg_constraint
		where conrelid = 'c'::regclass order by 1`)
	want := [][]any{
		{"c_cas_fkey", "c", "a", "FOREIGN KEY (cas) REFERENCES p(id) ON DELETE CASCADE"},
		{"c_noact_fkey", "a", "a", "FOREIGN KEY (noact) REFERENCES p(id)"},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("pg_constraint: got %v", res.Rows)
	}
}

func TestFKCascadeSurvivesReopen(t *testing.T) {
	// The action rides the persisted descriptor: after a close/reopen
	// the constraint still cascades.
	dir := t.TempDir() + "/reopen.db"
	e, err := bytdb.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := New(e)
	exec(t, d, `create table p (id int primary key)`)
	exec(t, d, `create table c (id int primary key, p_id int references p on delete cascade)`)
	exec(t, d, `insert into p values (1)`)
	exec(t, d, `insert into c values (10, 1)`)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e, err = bytdb.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	d = New(e)
	exec(t, d, `delete from p where id = 1`)
	if got := count(t, d, "c"); got != 0 {
		t.Fatalf("c after reopened cascade: %d", got)
	}
}

func TestFKCascadeEngineActionValidation(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table p (id int primary key)`)
	exec(t, d, `create table c (id int primary key, p_id int)`)
	// The engine API refuses actions it would otherwise persist and
	// later misread as NO ACTION.
	err := d.e.AddForeignKey("c", bytdb.FKDesc{
		Name: "bad", Cols: []int{1}, RefTable: "p", OnDelete: "set null",
	}, false)
	if err == nil {
		t.Fatal("unknown OnDelete accepted")
	}
}
