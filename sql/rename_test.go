package sql

// ALTER TABLE ... RENAME tests: table and column renames are
// descriptor-only (rows are keyed by table and column IDs), with
// guards for the text-anchored dependents (checks, FKs).

import (
	"strings"
	"testing"
)

func TestRenameTable(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table old (id serial primary key, v text)`)
	exec(t, d, `create index old_v on old (v)`)
	exec(t, d, `insert into old (v) values ('a'), ('b')`)

	exec(t, d, `alter table old rename to fresh`)
	res := exec(t, d, `select v from fresh where v = 'a'`)
	if len(res.Rows) != 1 {
		t.Fatalf("rows after rename: %v", res.Rows)
	}
	// The identity counter followed (it is keyed by table ID).
	exec(t, d, `insert into fresh (v) values ('c')`)
	if res := exec(t, d, `select max(id) from fresh`); res.Rows[0][0].(int64) != 3 {
		t.Fatalf("identity after rename: %v", res.Rows[0][0])
	}
	if _, err := d.Exec(`select * from old`); err == nil {
		t.Fatal("old name still resolves")
	}
	// The old name is reusable; the new one is taken.
	exec(t, d, `create table old (id int primary key)`)
	if _, err := d.Exec(`alter table old rename to fresh`); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("rename onto a taken name: %v", err)
	}

	// A table referenced by a foreign key stays put; a self-reference
	// renames along.
	exec(t, d, `create table child (id int primary key, fid int references fresh (id))`)
	if _, err := d.Exec(`alter table fresh rename to newer`); err == nil ||
		!strings.Contains(err.Error(), "referenced by a foreign key") {
		t.Fatalf("rename referenced table: %v", err)
	}
	exec(t, d, `create table tree (id int primary key, parent int references tree (id))`)
	exec(t, d, `alter table tree rename to forest`)
	exec(t, d, `insert into forest values (1, 1)`)
	if _, err := d.Exec(`insert into forest values (2, 99)`); err == nil {
		t.Fatal("self-reference lost in rename")
	}
}

func TestRenameColumn(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, v text, w int)`)
	exec(t, d, `create index t_v on t (v)`)
	exec(t, d, `insert into t values (1, 'x', 10)`)

	exec(t, d, `alter table t rename column v to label`)
	res := exec(t, d, `select label from t where label = 'x'`) // still indexed (ordinals)
	if len(res.Rows) != 1 {
		t.Fatalf("rename column read-back: %v", res.Rows)
	}
	if _, err := d.Exec(`select v from t`); err == nil {
		t.Fatal("old column name still resolves")
	}
	if _, err := d.Exec(`alter table t rename column label to w`); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("rename onto a taken column: %v", err)
	}

	// A column a CHECK mentions stays put (its text would orphan).
	exec(t, d, `alter table t add check (w > 0)`)
	if _, err := d.Exec(`alter table t rename column w to n`); err == nil ||
		!strings.Contains(err.Error(), "other objects depend on it") {
		t.Fatalf("rename checked column: %v", err)
	}

	// A column referenced by another table's FK stays put.
	exec(t, d, `create table p (id int primary key, code text unique)`)
	exec(t, d, `create table c (id int primary key, pcode text references p (code))`)
	if _, err := d.Exec(`alter table p rename column code to tag`); err == nil ||
		!strings.Contains(err.Error(), "referenced by a foreign key") {
		t.Fatalf("rename FK-referenced column: %v", err)
	}
	// The child's own FK column renames fine (ordinals, not names).
	exec(t, d, `alter table c rename column pcode to parent_code`)
	if _, err := d.Exec(`insert into c values (1, 'nope')`); err == nil {
		t.Fatal("FK lost after child column rename")
	}
}
