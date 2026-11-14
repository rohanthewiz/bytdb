package sql

import (
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// sysRowsOf resolves a system table through the DB's lookup, the way a
// statement's FROM does, and returns its rows.
func sysRowsOf(d *DB, name string) [][]any {
	_, rows := d.lookup(d.e.TableShared)(name)
	return rows
}

// Within one catalog version a memoized table is built once and the
// same rows are handed out again; DDL moves the version and forces a
// rebuild that sees the change.
func TestCatalogMemoReusesRowsUntilDDL(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, name text)`)

	r1 := sysRowsOf(d, "information_schema.columns")
	r2 := sysRowsOf(d, "information_schema.columns")
	if len(r1) != 2 || len(r2) != 2 {
		t.Fatalf("columns rows: %d then %d, want 2", len(r1), len(r2))
	}
	if &r1[0] != &r2[0] {
		t.Fatal("second lookup at the same catalog version rebuilt the rows")
	}

	exec(t, d, `alter table t add column note text`)
	r3 := sysRowsOf(d, "information_schema.columns")
	if len(r3) != 3 {
		t.Fatalf("after ADD COLUMN: %d rows, want 3", len(r3))
	}
	if &r3[0] == &r1[0] {
		t.Fatal("rows from before the DDL were reused")
	}
}

// Every kind of DDL a catalog row reflects shows up in the next query:
// tables, columns, indexes, views, sequence options.
func TestCatalogMemoFollowsDDL(t *testing.T) {
	d := openDB(t)
	cols := func() [][]any {
		return exec(t, d, `select column_name from information_schema.columns
			where table_name = 't' order by ordinal_position`).Rows
	}
	exec(t, d, `create table t (id int primary key)`)
	if got := cols(); !reflect.DeepEqual(got, [][]any{{"id"}}) {
		t.Fatalf("columns: %v", got)
	}
	exec(t, d, `alter table t add column a text`)
	if got := cols(); !reflect.DeepEqual(got, [][]any{{"id"}, {"a"}}) {
		t.Fatalf("columns after ADD COLUMN: %v", got)
	}
	exec(t, d, `alter table t rename column a to b`)
	if got := cols(); !reflect.DeepEqual(got, [][]any{{"id"}, {"b"}}) {
		t.Fatalf("columns after RENAME COLUMN: %v", got)
	}

	exec(t, d, `create index t_b on t (b)`)
	res := exec(t, d, `select indexname from pg_indexes where tablename = 't' order by 1`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"t_b"}, {"t_pkey"}}) {
		t.Fatalf("pg_indexes after CREATE INDEX: %v", res.Rows)
	}

	exec(t, d, `create view v as select id from t`)
	res = exec(t, d, `select relname from pg_class where relkind = 'v'`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"v"}}) {
		t.Fatalf("pg_class after CREATE VIEW: %v", res.Rows)
	}

	exec(t, d, `create sequence s`)
	exec(t, d, `alter sequence s increment by 5`)
	res = exec(t, d, `select increment from information_schema.sequences where sequence_name = 's'`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"5"}}) {
		t.Fatalf("information_schema.sequences after ALTER SEQUENCE: %v", res.Rows)
	}

	exec(t, d, `drop view v`)
	exec(t, d, `drop table t`)
	if got := cols(); len(got) != 0 {
		t.Fatalf("columns after DROP TABLE: %v", got)
	}
}

// The tables that read live state are rebuilt on every query: none of
// what they report moves the catalog version.
func TestCatalogLiveTablesNotMemoized(t *testing.T) {
	d := openDB(t)

	// pg_sequences.last_value follows nextval, which is not DDL.
	exec(t, d, `create sequence s`)
	exec(t, d, `select nextval('s')`)
	last := func() any {
		return exec(t, d, `select last_value from pg_sequences where sequencename = 's'`).Rows[0][0]
	}
	if got := last(); got != int64(1) {
		t.Fatalf("last_value after one nextval = %v, want 1", got)
	}
	exec(t, d, `select nextval('s')`)
	if got := last(); got != int64(2) {
		t.Fatalf("last_value after two nextvals = %v, want 2", got)
	}

	// pg_settings reports the reading session's SET values.
	s := d.NewSession()
	setting := func() any {
		res, err := s.Exec(`select setting from pg_settings where name = 'application_name'`)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rows) == 0 {
			return nil
		}
		return res.Rows[0][0]
	}
	if _, err := s.Exec(`set application_name = 'one'`); err != nil {
		t.Fatal(err)
	}
	if got := setting(); got != "one" {
		t.Fatalf("pg_settings application_name = %v, want one", got)
	}
	if _, err := s.Exec(`set application_name = 'two'`); err != nil {
		t.Fatal(err)
	}
	if got := setting(); got != "two" {
		t.Fatalf("pg_settings after a second SET = %v, want two", got)
	}

	// pg_stat_activity asks its provider every time.
	var mu sync.Mutex
	pids := []int32{7}
	d.SetActivityProvider(func() []Activity {
		mu.Lock()
		defer mu.Unlock()
		out := make([]Activity, len(pids))
		for i, p := range pids {
			out[i] = Activity{PID: p, State: "idle"}
		}
		return out
	})
	count := func() any { return exec(t, d, `select count(*) from pg_stat_activity`).Rows[0][0] }
	if got := count(); got != int64(1) {
		t.Fatalf("pg_stat_activity rows = %v, want 1", got)
	}
	mu.Lock()
	pids = append(pids, 8)
	mu.Unlock()
	if got := count(); got != int64(2) {
		t.Fatalf("pg_stat_activity rows after a new backend = %v, want 2", got)
	}
}

// Memoized rows are shared by every statement of every session, and a
// one-table scan hands them to the WHERE uncopied. A caller that
// rewrites the rows it got back must not reach the memo, whether the
// virtual table was the whole FROM or the first of a join.
func TestSharedCatalogRowsSurviveCallerMutation(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	exec(t, d, `create table orders (id int primary key, user_id int)`)

	queries := []string{
		`select * from pg_class order by oid`,
		`select * from information_schema.columns where table_name = 'users' order by ordinal_position`,
		`select a.attname, c.relname from pg_attribute a join pg_class c on c.oid = a.attrelid
			where a.attnum > 0 order by c.relname, a.attnum`,
	}
	for _, q := range queries {
		want := exec(t, d, q).Rows
		got := exec(t, d, q).Rows
		for _, row := range got {
			for i := range row {
				row[i] = "clobbered"
			}
		}
		if again := exec(t, d, q).Rows; !reflect.DeepEqual(again, want) {
			t.Fatalf("%s\nafter the caller rewrote its result rows:\n got %v\nwant %v", q, again, want)
		}
	}
}

// A virtual table's static conjuncts narrow its rows before the join
// combines them (joinRun.virtualRows). That must never change a result:
// WHERE conjuncts on a LEFT JOIN's nullable side are not statics and
// still apply after NULL-extension, ON conjuncts that are statics
// leave unmatched left rows NULL-extended, and the nested-loop and hash
// paths agree.
func TestVirtualStaticPrefilter(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	exec(t, d, `create table orders (id int primary key, user_id int, total float)`)

	// Hash path: the static relname = 'users' leaves one pg_class row
	// in the build table.
	res := exec(t, d, `select a.attname from pg_attribute a join pg_class c on c.oid = a.attrelid
		where c.relname = 'users' and a.attnum > 0 order by a.attnum`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"id"}, {"name"}, {"age"}, {"city"}}) {
		t.Fatalf("hash join with a static filter: %v", res.Rows)
	}
	// The filter on the other side: probes come only from orders.
	res = exec(t, d, `select a.attname from pg_class c join pg_attribute a on a.attrelid = c.oid
		where a.attname = 'total'`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"total"}}) {
		t.Fatalf("hash join, static on the build side's partner: %v", res.Rows)
	}

	const ctes = `with l as (select id from users where id <= 3),
		r as (select id, city as tag from users where id <= 3) `
	var rows [][]any
	for _, r := range exec(t, d, ctes+`select id, tag from r order by id`).Rows {
		rows = append(rows, r)
	}
	if len(rows) != 3 {
		t.Fatalf("fixture: want 3 rows, got %v", rows)
	}
	tagA := rows[0][1] // whatever city user 1 has
	var wantLeftOn [][]any
	for _, r := range rows {
		if r[1] == tagA {
			wantLeftOn = append(wantLeftOn, []any{r[0], r[1]})
		} else {
			wantLeftOn = append(wantLeftOn, []any{r[0], nil})
		}
	}

	// ON static on a LEFT JOIN's virtual right side: unmatched left rows
	// are NULL-extended, not dropped.
	res = exec(t, d, ctes+fmt.Sprintf(`select l.id, r.tag from l left join r
		on r.id = l.id and r.tag = '%s' order by l.id`, tagA))
	if !reflect.DeepEqual(res.Rows, wantLeftOn) {
		t.Fatalf("LEFT JOIN with a static ON conjunct:\n got %v\nwant %v", res.Rows, wantLeftOn)
	}

	// The same conjunct in the WHERE runs after NULL-extension, so the
	// NULL-extended rows go.
	var wantWhere [][]any
	for _, r := range wantLeftOn {
		if r[1] != nil {
			wantWhere = append(wantWhere, r)
		}
	}
	res = exec(t, d, ctes+fmt.Sprintf(`select l.id, r.tag from l left join r
		on r.id = l.id where r.tag = '%s' order by l.id`, tagA))
	if !reflect.DeepEqual(res.Rows, wantWhere) {
		t.Fatalf("LEFT JOIN with the conjunct in WHERE:\n got %v\nwant %v", res.Rows, wantWhere)
	}

	// Nested-loop path (no equality to hash on): the static filters r
	// once for every outer row.
	res = exec(t, d, ctes+fmt.Sprintf(`select l.id, r.id from l join r
		on r.id > l.id where r.tag = '%s' order by l.id, r.id`, tagA))
	var wantNL [][]any
	for _, lr := range rows {
		for _, rr := range rows {
			if rr[0].(int64) > lr[0].(int64) && rr[1] == tagA {
				wantNL = append(wantNL, []any{lr[0], rr[0]})
			}
		}
	}
	if !reflect.DeepEqual(res.Rows, wantNL) {
		t.Fatalf("nested loop with a static filter:\n got %v\nwant %v", res.Rows, wantNL)
	}

	// A static no row passes: the join is empty, not an error.
	res = exec(t, d, `select a.attname from pg_attribute a join pg_class c on c.oid = a.attrelid
		where c.relname = 'no_such_table'`)
	if len(res.Rows) != 0 {
		t.Fatalf("filter that rejects every row: %v", res.Rows)
	}
}

// Sessions read memoized catalog rows while DDL moves the version
// under them. Run with -race: the memo's map and the shared rows are
// touched from every goroutine.
func TestCatalogMemoConcurrentDDL(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t0 (id int primary key, v text)`)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := d.NewSession()
			for range 50 {
				res, err := s.Exec(`select count(*) from information_schema.columns where table_name = 't0'`)
				if err != nil {
					errs <- err
					return
				}
				if got := res.Rows[0][0]; got != int64(2) {
					errs <- fmt.Errorf("t0 has %v columns, want 2", got)
					return
				}
			}
		}()
	}
	for i := 1; i <= 20; i++ {
		exec(t, d, fmt.Sprintf(`create table t%d (id int primary key)`, i))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	res := exec(t, d, `select count(*) from information_schema.tables where table_schema = 'public'`)
	if got := res.Rows[0][0]; got != int64(21) {
		t.Fatalf("tables after the DDL burst = %v, want 21", got)
	}
}

// The mechanism behind the test above, pinned where SQL cannot reach
// it: a consumer that appends to a yielded row (execSelect appends
// expression columns) or overwrites it must not write into a virtual
// table's shared row, even when that row has spare capacity — the
// builders happen to cap theirs, and nothing should depend on it.
func TestBorrowedVirtualRowCopiedAtYield(t *testing.T) {
	backing := make([]any, 2, 4)
	backing[0], backing[1] = int64(1), "a"
	rows := [][]any{backing}
	desc := sysDesc("v", sysCol("id", bytdb.TInt), sysCol("s", bytdb.TString))
	lk := func(string) (*bytdb.TableDesc, [][]any) { return desc, rows }
	fp, err := prepareFrom(lk, []FromItem{{Table: "v"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = runJoin(nil, fp, nil, func(vals []any) bool {
		vals = append(vals, "appended")
		vals[0] = int64(99)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if backing[0] != int64(1) || backing[:3][2] != nil {
		t.Fatalf("yield wrote through to the shared row: %v", backing[:3])
	}
}
