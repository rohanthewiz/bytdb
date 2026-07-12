package sql

// Foreign-key tests: child-side and parent-side enforcement, MATCH
// SIMPLE NULLs, end-of-statement (NO ACTION) checks, schema guards,
// and the ALTER TABLE forms.

import (
	"strings"
	"testing"
)

func seedFK(t *testing.T, d *DB) {
	t.Helper()
	exec(t, d, `create table users (id int primary key, name text)`)
	exec(t, d, `create table orders (
		id int primary key,
		user_id int references users,
		note text
	)`)
	exec(t, d, `insert into users values (1, 'ada'), (2, 'grace')`)
}

func wantErr(t *testing.T, d *DB, q, want string) {
	t.Helper()
	if _, err := d.Exec(q); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("%s\n  got: %v\n  want: %s", q, err, want)
	}
}

func TestFKChildEnforcement(t *testing.T) {
	d := openDB(t)
	seedFK(t, d)

	exec(t, d, `insert into orders values (10, 1, 'ok')`)
	// A NULL FK column satisfies the constraint (MATCH SIMPLE).
	exec(t, d, `insert into orders values (11, null, 'orphan by design')`)

	wantErr(t, d, `insert into orders values (12, 99, 'dangling')`,
		`insert or update on table "orders" violates foreign key constraint "orders_user_id_fkey"`)
	// A multi-row insert rolls back whole.
	wantErr(t, d, `insert into orders values (13, 2, 'fine'), (14, 99, 'not')`,
		"violates foreign key constraint")
	if res := exec(t, d, `select count(*) from orders`); res.Rows[0][0].(int64) != 2 {
		t.Fatalf("orders after failed insert: %v", res.Rows[0][0])
	}

	// Updating the child FK column re-checks it.
	wantErr(t, d, `update orders set user_id = 42 where id = 10`,
		"violates foreign key constraint")
	exec(t, d, `update orders set user_id = 2 where id = 10`)
	// ON CONFLICT DO UPDATE re-checks the resolved row too.
	wantErr(t, d, `insert into orders values (10, 1, 'x')
		on conflict (id) do update set user_id = 77`,
		"violates foreign key constraint")
}

func TestFKParentEnforcement(t *testing.T) {
	d := openDB(t)
	seedFK(t, d)
	exec(t, d, `insert into orders values (10, 1, 'ok')`)

	wantErr(t, d, `delete from users where id = 1`,
		`update or delete on table "users" violates foreign key constraint "orders_user_id_fkey" on table "orders"`)
	// An unreferenced parent row deletes fine.
	exec(t, d, `delete from users where id = 2`)

	// Changing a referenced key is refused while children point at it.
	wantErr(t, d, `update users set id = 5 where id = 1`,
		"violates foreign key constraint")
	// Non-key parent updates are unaffected.
	exec(t, d, `update users set name = 'ada l.' where id = 1`)

	// End-of-statement semantics: deleting parent and child rows in one
	// statement is legal when nothing dangles afterward.
	exec(t, d, `delete from orders where id = 10`)
	exec(t, d, `delete from users where id = 1`)
}

func TestFKSelfReference(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table nodes (
		id int primary key,
		parent_id int references nodes (id)
	)`)
	exec(t, d, `insert into nodes values (1, null), (2, 1)`)
	// A row that is its own parent inserts (checked post-write).
	exec(t, d, `insert into nodes values (3, 3)`)
	wantErr(t, d, `insert into nodes values (4, 99)`, "violates foreign key constraint")
	wantErr(t, d, `delete from nodes where id = 1`, "violates foreign key constraint")
	// Deleting a self-referencing row leaves nothing dangling.
	exec(t, d, `delete from nodes where id = 3`)
	// Deleting the whole chain in one statement works (NO ACTION checks
	// after the statement's writes).
	exec(t, d, `delete from nodes where id >= 1`)
}

func TestFKCompositeAndUniqueTarget(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table pk2 (a int, b int, v text, primary key (a, b))`)
	exec(t, d, `create table ref2 (
		id int primary key, x int, y int,
		foreign key (x, y) references pk2 (a, b)
	)`)
	exec(t, d, `insert into pk2 values (1, 2, 'v')`)
	exec(t, d, `insert into ref2 values (1, 1, 2)`)
	wantErr(t, d, `insert into ref2 values (2, 1, 3)`, "violates foreign key constraint")
	// A partial NULL satisfies MATCH SIMPLE.
	exec(t, d, `insert into ref2 values (3, 1, null)`)

	// Referencing a UNIQUE column (not the PK) works; a non-unique
	// target does not.
	exec(t, d, `create table emails (id int primary key, addr text unique)`)
	exec(t, d, `create table subs (id int primary key, addr text references emails (addr))`)
	exec(t, d, `insert into emails values (1, 'a@b')`)
	exec(t, d, `insert into subs values (1, 'a@b')`)
	wantErr(t, d, `insert into subs values (2, 'z@z')`, "violates foreign key constraint")
	wantErr(t, d, `create table bad (id int primary key, n text references users (name))`,
		"no such table") // users doesn't exist in this db
	exec(t, d, `create table plain (id int primary key, v text)`)
	wantErr(t, d, `create table bad (id int primary key, v text references plain (v))`,
		"there is no unique constraint matching given keys")
	// Type mismatch is caught at declaration.
	wantErr(t, d, `create table bad (id int primary key, v text references plain (id))`,
		"types do not match")
	// The failed CREATEs left nothing behind.
	exec(t, d, `create table bad (id int primary key)`)
}

func TestFKSchemaGuards(t *testing.T) {
	d := openDB(t)
	seedFK(t, d)

	wantErr(t, d, `drop table users`, "other objects depend on it")
	wantErr(t, d, `alter table users drop column id`, "cannot drop a primary key column")
	wantErr(t, d, `alter table orders drop column user_id`,
		"cannot drop a foreign key column")
	wantErr(t, d, `truncate users`, "cannot truncate a table referenced in a foreign key")
	// Truncating parent and child together is fine.
	exec(t, d, `truncate users, orders`)

	// The unique index backing an FK cannot be dropped...
	exec(t, d, `create table emails (id int primary key, addr text unique)`)
	exec(t, d, `create table subs (id int primary key, addr text references emails (addr))`)
	wantErr(t, d, `drop index emails_addr_key`,
		"cannot drop the unique index a foreign key depends on")
	// ...until the constraint goes; then everything unwinds.
	exec(t, d, `alter table subs drop constraint subs_addr_fkey`)
	exec(t, d, `drop index emails_addr_key`)
	// Dropping the child first releases the parent.
	exec(t, d, `drop table orders`)
	exec(t, d, `drop table users`)
}

func TestFKAlterAddValidatesRows(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table p (id int primary key)`)
	exec(t, d, `create table c (id int primary key, pid int)`)
	exec(t, d, `insert into p values (1)`)
	exec(t, d, `insert into c values (1, 1), (2, 99)`)

	// Existing dangling rows refuse the constraint.
	wantErr(t, d, `alter table c add constraint c_pid foreign key (pid) references p`,
		"violates foreign key constraint")
	exec(t, d, `update c set pid = null where id = 2`)
	exec(t, d, `alter table c add constraint c_pid foreign key (pid) references p`)
	wantErr(t, d, `insert into c values (3, 42)`, "violates foreign key constraint")

	// DROP CONSTRAINT lifts enforcement.
	exec(t, d, `alter table c drop constraint c_pid`)
	exec(t, d, `insert into c values (3, 42)`)
	// IF EXISTS on a gone FK is a notice, not an error.
	if res := exec(t, d, `alter table c drop constraint if exists c_pid`); res.Notice == "" {
		t.Fatal("expected a notice for the absent constraint")
	}
}

func TestFKInBlock(t *testing.T) {
	d := openDB(t)
	seedFK(t, d)
	s := d.NewSession()
	if _, err := s.Exec(`begin`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(`insert into orders values (10, 1, 'ok')`); err != nil {
		t.Fatal(err)
	}
	// The violation fails the block, like any statement error.
	if _, err := s.Exec(`insert into orders values (11, 99, 'bad')`); err == nil {
		t.Fatal("violation not raised inside block")
	}
	if _, err := s.Exec(`rollback`); err != nil {
		t.Fatal(err)
	}
	if res := exec(t, d, `select count(*) from orders`); res.Rows[0][0].(int64) != 0 {
		t.Fatalf("orders after rollback: %v", res.Rows[0][0])
	}
}
