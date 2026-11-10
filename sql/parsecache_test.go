package sql

// Deep-look D-40 step 4: the parse cache shares one tree among every
// execution of a text, which is only sound if execution never writes to
// it. Each statement here runs (twice, with parameters where it has
// them) from the cache, then its cached tree must still deep-equal a
// fresh parse.

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestParseCacheTreesStayPristine(t *testing.T) {
	d := openDB(t)
	for _, q := range []string{
		`create table pc (id int primary key, name text, amt float, ref uuid, at timestamp, day date, tags text[], doc jsonb)`,
		`create index pc_name on pc (name)`,
		`create table pk2 (id serial primary key, pid int references pc (id) on delete cascade, note text default 'n' check (note <> ''))`,
		`create view pcv as select id, name from pc`,
	} {
		exec(t, d, q)
	}
	stmts := []struct {
		q    string
		args [][]any
	}{
		{`insert into pc values ($1, $2, $3, $4, $5, $6, '{a,b}', '{"k": 1}')`, [][]any{
			{1, "a", 1.5, "00000000-0000-4000-8000-000000000001", "2024-01-01 00:00:00", "2024-01-01"},
			{2, "b", 2.5, "00000000-0000-4000-8000-000000000002", "2024-01-02 00:00:00", "2024-01-02"}}},
		{`insert into pc (id, name) values (3, 'c'), (4, 'd') on conflict (id) do update set name = excluded.name || '!' returning id, name`, nil},
		{`insert into pk2 (pid) values ($1) returning id, note`, [][]any{{1}, {2}}},
		{`select * from pc where id = $1`, [][]any{{1}, {2}}},
		{`select id from pc where ref = $1`, [][]any{{"00000000-0000-4000-8000-000000000001"}, {"00000000-0000-4000-8000-000000000002"}}},
		{`select id from pc where id in (1, 3, 9) and name = any('{a,c}') order by id`, nil},
		{`select id from pc where at >= '2024-01-02'::timestamp or day = '2024-01-01'`, nil},
		{`select name, count(*), sum(amt) from pc group by name having count(*) > 0 order by 2 desc, name limit $1`, [][]any{{2}, {5}}},
		{`select id, row_number() over (partition by name order by amt desc) from pc`, nil},
		{`with x as (select id, name from pc where id < $1) select x.name, p.note from x join pk2 p on p.pid = x.id`, [][]any{{3}, {9}}},
		{`select id from pc p where exists (select 1 from pk2 k where k.pid = p.id) and id not in (select 9) order by id`, nil},
		{`select (select max(amt) from pc), name from pcv where id = 1 union all select 0, 'z' order by 2`, nil},
		{`select doc->>'k', tags from pc where doc ? 'k' and 'a' = any(tags)`, nil},
		{`update pc set amt = amt * $1, name = upper(name) where id = $2 returning id, amt`, [][]any{{2, 1}, {3, 2}}},
		{`update pc set day = '2024-03-01' where ref = '00000000-0000-4000-8000-000000000002'`, nil},
		{`delete from pk2 where pid = $1 returning id`, [][]any{{2}, {1}}},
		{`delete from pc where id in (3, 4)`, nil},
	}
	for _, s := range stmts {
		runs := s.args
		if runs == nil {
			runs = [][]any{nil, nil}
		}
		for _, args := range runs {
			if _, err := d.Exec(s.q, args...); err != nil {
				t.Fatalf("%s %v: %v", s.q, args, err)
			}
		}
		el, ok := d.parses.ent[s.q]
		if !ok {
			t.Fatalf("not cached: %s", s.q)
		}
		fresh, err := Parse(s.q)
		if err != nil {
			t.Fatal(err)
		}
		if cached := el.Value.(*parseEntry).st; !reflect.DeepEqual(cached, fresh) {
			t.Errorf("execution modified the cached tree of %s", s.q)
		}
	}
}

func TestParseCacheLRU(t *testing.T) {
	c := newParseCache()
	for i := range parseCacheSize + 10 {
		if _, err := c.parse(fmt.Sprintf("select %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if c.ll.Len() != parseCacheSize || len(c.ent) != parseCacheSize {
		t.Fatalf("cache holds %d/%d entries, want %d", c.ll.Len(), len(c.ent), parseCacheSize)
	}
	if _, ok := c.ent["select 0"]; ok {
		t.Fatal("least recently used entry survived")
	}
	// Uncacheable kinds, parse errors and oversized texts stay out.
	c.parse("begin")
	c.parse("select from from")
	c.parse("select '" + strings.Repeat("x", maxCachedQueryLen) + "'")
	if _, ok := c.ent["begin"]; ok || c.ll.Len() != parseCacheSize {
		t.Fatal("uncacheable statement cached")
	}
	// Concurrent use is race-free and returns one shared tree per text.
	var wg sync.WaitGroup
	trees := make([]Statement, 8)
	for i := range trees {
		wg.Add(1)
		go func() {
			defer wg.Done()
			trees[i], _ = c.parse("select 1 + 1")
		}()
	}
	wg.Wait()
	for _, tr := range trees {
		if tr == nil {
			t.Fatal("parse failed")
		}
	}
	if a, b := cachedParse(c, "select 1 + 1"), cachedParse(c, "select 1 + 1"); a != b {
		t.Fatal("a cached text did not return its one shared tree")
	}
}

func cachedParse(c *parseCache, q string) Statement {
	st, err := c.parse(q)
	if err != nil {
		panic(err)
	}
	return st
}
