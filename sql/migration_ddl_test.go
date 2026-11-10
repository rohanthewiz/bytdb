package sql

// Deep-look D-54: the DDL migration tools and pg_dump emit.

import (
	"fmt"
	"strings"
	"testing"
)

func TestMigrationDDLAccepted(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, c int, name text)`)
	exec(t, d, `insert into t values (1, 5, 'a'), (2, 6, 'b')`)

	// Parse-and-ignore: each runs, reports its own tag, changes nothing.
	for q, tag := range map[string]string{
		`create schema if not exists app authorization me`:              "CREATE SCHEMA",
		`create extension if not exists "uuid-ossp" with schema public`: "CREATE EXTENSION",
		`comment on column t.c is 'the c'`:                              "COMMENT",
		`comment on table t is null`:                                    "COMMENT",
		`analyze verbose t`:                                             "ANALYZE",
		`lock table t in share row exclusive mode nowait`:               "LOCK TABLE",
	} {
		st, err := Parse(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if got := command(st); got != tag {
			t.Errorf("%s: tag %q, want %q", q, got, tag)
		}
		exec(t, d, q)
	}

	// Unnamed indexes get Postgres's names, numbered on collision.
	exec(t, d, `create index on t (name)`)
	exec(t, d, `create index concurrently on only t using btree (name)`)
	exec(t, d, `create unique index on t (c, name)`)
	desc := d.e.TableShared("t")
	for _, want := range []string{"t_name_idx", "t_name_idx1", "t_c_name_idx"} {
		if desc.Index(want) == nil {
			t.Errorf("no index %s; have %v", want, desc.Indexes)
		}
	}
	if _, err := d.Exec(`create index on t using hash (name)`); err == nil || !strings.Contains(err.Error(), "btree") {
		t.Errorf("hash index: %v", err)
	}

	// IF EXISTS forms and ONLY.
	if r := exec(t, d, `alter table if exists nope add column z int`); !strings.Contains(r.Notice, "does not exist, skipping") {
		t.Errorf("ALTER TABLE IF EXISTS notice = %q", r.Notice)
	}
	exec(t, d, `alter table if exists t add column z int`)
	exec(t, d, `alter table only t alter column z set default 3`)
	if r := exec(t, d, `alter table t drop column if exists nope`); !strings.Contains(r.Notice, "skipping") {
		t.Errorf("DROP COLUMN IF EXISTS notice = %q", r.Notice)
	}
	exec(t, d, `alter table t drop column if exists z cascade`)

	// NOT VALID parses, but existing rows are still checked.
	exec(t, d, `alter table t add constraint c_pos check (c > 0) not valid`)
	if _, err := d.Exec(`alter table t add constraint c_big check (c > 5) not valid`); err == nil {
		t.Error("NOT VALID constraint violated by an existing row was accepted")
	}
}

func TestDefaultForms(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table df (
		id uuid default gen_random_uuid() primary key,
		v uuid default uuid_generate_v4(),
		s text default 'x'::text,
		vc varchar(10) default ''::character varying,
		n int default (0),
		m int default (-1),
		j jsonb default '{}'::jsonb,
		k int)`)
	exec(t, d, `insert into df (k) values (1), (2), (3)`)
	exec(t, d, `insert into df values (default, default, default, default, default, default, default, 4)`)
	r := exec(t, d, `select count(distinct id), count(distinct v), min(s), min(vc), min(n), min(m), min(j::text) from df`)
	if got := fmt.Sprint(r.Rows); got != "[[4 4 x  0 -1 {}]]" {
		t.Fatalf("defaults = %s", got)
	}
	ps, err := d.Prepare(`insert into df (k) values ($1) returning id`)
	if err != nil {
		t.Fatal(err)
	}
	a, err := ps.Exec(5)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ps.Exec(6)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(a.Rows) == fmt.Sprint(b.Rows) {
		t.Fatal("prepared insert reused one uuid")
	}
	for _, bad := range []string{
		`create table bad1 (id int primary key, s text default gen_random_uuid())`,
		`create table bad2 (id int primary key, r int default nextval('s'::regclass))`,
	} {
		if _, err := d.Exec(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if _, err := d.Exec(`create table ctas as select 1`); err == nil || !strings.Contains(err.Error(), "primary key") {
		t.Errorf("CREATE TABLE AS: %v", err)
	}
}

func TestDropForms(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table a (id int primary key)`)
	exec(t, d, `create table b (id int primary key)`)
	exec(t, d, `create table p (id int primary key)`)
	exec(t, d, `create table ch (id int primary key, pid int references p (id))`)

	// A missing name drops nothing.
	if _, err := d.Exec(`drop table a, nope, b`); err == nil {
		t.Fatal("DROP TABLE with a missing name succeeded")
	}
	if d.e.TableShared("a") == nil || d.e.TableShared("b") == nil {
		t.Fatal("tables dropped by a failed DROP TABLE")
	}
	if r := exec(t, d, `drop table if exists a, nope, b cascade`); !strings.Contains(r.Notice, `"nope" does not exist`) {
		t.Errorf("notice = %q", r.Notice)
	}
	if d.e.TableShared("a") != nil || d.e.TableShared("b") != nil {
		t.Fatal("tables survived DROP TABLE a, b")
	}
	// CASCADE does not cascade: a referenced table is refused, as RESTRICT.
	if _, err := d.Exec(`drop table p cascade`); err == nil {
		t.Fatal("DROP TABLE CASCADE of a referenced table succeeded")
	}
	exec(t, d, `create index ch_pid on ch (pid)`)
	exec(t, d, `drop index ch_pid restrict`)
	exec(t, d, `drop table ch, p`)
}

func TestCastTypeSpellings(t *testing.T) {
	d := openDB(t)
	r := exec(t, d, `select 'abc'::character varying, 'abc'::varchar(2), '2024-01-02 03:04'::timestamp without time zone,
		'2024-01-02 03:04+00'::timestamp(3) with time zone, 1.5::numeric(10,2), 'x'::character(1)`)
	// Timestamps are microsecond counts at this layer; modifiers parse and
	// are not applied ('abc'::varchar(2) stays 'abc' — docs/gotchas.md).
	if got := fmt.Sprint(r.Rows); got != "[[abc abc 1704164640000000 1704164640000000 1.5 x]]" {
		t.Fatalf("casts = %s", got)
	}
}
