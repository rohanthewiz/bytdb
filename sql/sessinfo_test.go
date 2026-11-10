package sql

// Deep-look D-51 part 1: the session surface — advisory locks, DISCARD
// and DEALLOCATE, the parenthesis-free functions, the session
// information functions, the SET/SHOW forms tools send, and
// default_transaction_read_only.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/bytdb"
)

// sexec runs q in session s, failing the test on error.
func sexec(t *testing.T, s *Session, q string) *Result {
	t.Helper()
	r, err := s.Exec(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return r
}

// sone returns the single value of a one-row, one-column result.
func sone(t *testing.T, s *Session, q string) any {
	t.Helper()
	r := sexec(t, s, q)
	if len(r.Rows) != 1 || len(r.Rows[0]) != 1 {
		t.Fatalf("%s: rows %v", q, r.Rows)
	}
	return r.Rows[0][0]
}

func TestAdvisoryLocks(t *testing.T) {
	d := openDB(t)
	a, b := d.NewSession(), d.NewSession()
	defer a.Close()
	defer b.Close()

	sexec(t, a, `select pg_advisory_lock(7)`)
	if sone(t, b, `select pg_try_advisory_lock(7)`) != false {
		t.Fatal("B took A's lock")
	}
	// Reentrant: a second hold needs a second unlock.
	sexec(t, a, `select pg_advisory_lock(7)`)
	if sone(t, a, `select pg_advisory_unlock(7)`) != true {
		t.Fatal("first unlock failed")
	}
	if sone(t, b, `select pg_try_advisory_lock(7)`) != false {
		t.Fatal("lock free after one of two unlocks")
	}
	sexec(t, a, `select pg_advisory_unlock(7)`)
	if sone(t, a, `select pg_advisory_unlock(7)`) != false {
		t.Error("unlock of a lock not held returned true")
	}
	if sone(t, b, `select pg_try_advisory_lock(7)`) != true {
		t.Fatal("B could not take the freed lock")
	}
	// The one-bigint and two-int4 key spaces are separate.
	if sone(t, a, `select pg_try_advisory_lock(0, 7)`) != true {
		t.Error("two-key lock collided with the one-key lock")
	}
	if _, err := a.Exec(`select pg_advisory_lock(1, 4294967296)`); err == nil {
		t.Error("two-key lock accepted a key outside int4")
	}

	// A blocked lock wakes when the holder releases.
	done := make(chan error, 1)
	go func() {
		_, err := a.Exec(`select pg_advisory_lock(7)`)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("lock did not wait: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	sexec(t, b, `select pg_advisory_unlock(7)`)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never woke")
	}

	// statement_timeout bounds the wait.
	sexec(t, b, `set statement_timeout = '50ms'`)
	if _, err := b.Exec(`select pg_advisory_lock(7)`); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("blocked lock under statement_timeout: err %v", err)
	}
	sexec(t, b, `reset statement_timeout`)

	// unlock_all releases session-level holds; Close releases everything.
	sexec(t, a, `select pg_advisory_unlock_all()`)
	if sone(t, b, `select pg_try_advisory_lock(7)`) != true {
		t.Error("unlock_all left the lock held")
	}
	b.Close()
	c := d.NewSession()
	defer c.Close()
	if sone(t, c, `select pg_try_advisory_lock(7)`) != true {
		t.Error("Close left the lock held")
	}
}

func TestAdvisoryXactLocks(t *testing.T) {
	d := openDB(t)
	a, b := d.NewSession(), d.NewSession()
	defer a.Close()
	defer b.Close()
	// Autocommit: the statement is the transaction.
	sexec(t, a, `select pg_advisory_xact_lock(3)`)
	if sone(t, b, `select pg_try_advisory_xact_lock(3)`) != true {
		t.Error("autocommit xact lock outlived its statement")
	}
	// In a block: held until COMMIT.
	sexec(t, a, `begin`)
	sexec(t, a, `select pg_advisory_xact_lock(3)`)
	if sone(t, b, `select pg_try_advisory_lock(3)`) != false {
		t.Error("xact lock not held inside the block")
	}
	// pg_advisory_unlock does not release a transaction-level hold.
	if sone(t, a, `select pg_advisory_unlock(3)`) != false {
		t.Error("unlock released an xact hold")
	}
	sexec(t, a, `commit`)
	if sone(t, b, `select pg_try_advisory_lock(3)`) != true {
		t.Error("xact lock survived COMMIT")
	}
	// A bare DB is one shared session with the same rules.
	exec(t, d, `select pg_advisory_lock(9)`)
	if sone(t, a, `select pg_try_advisory_lock(9)`) != false {
		t.Error("bare DB lock not visible to sessions")
	}
	exec(t, d, `select pg_advisory_unlock(9)`)
}

func TestDiscardDeallocate(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id serial primary key, a int)`)
	s := d.NewSession()
	defer s.Close()
	var dropped []string
	s.OnDeallocate(func(name string) error {
		if name == "missing" {
			return noPrepared(name)
		}
		dropped = append(dropped, name)
		return nil
	})
	sexec(t, s, `set app.user_id = '5'`)
	sexec(t, s, `set statement_timeout = '1s'`)
	sexec(t, s, `select pg_advisory_lock(1)`)
	sexec(t, s, `insert into t (a) values (1)`)
	sexec(t, s, `deallocate "stmtcache_1"`)
	sexec(t, s, `deallocate prepare all`)
	if _, err := s.Exec(`deallocate missing`); err == nil || !strings.Contains(err.Error(), `prepared statement "missing" does not exist`) {
		t.Errorf("deallocate missing: %v", err)
	}
	sexec(t, s, `begin`)
	if _, err := s.Exec(`discard all`); err == nil || !strings.Contains(err.Error(), "cannot run inside a transaction block") {
		t.Errorf("DISCARD ALL in a block: %v", err)
	}
	sexec(t, s, `rollback`)
	if r := sexec(t, s, `discard all`); r.Tag != "DISCARD ALL" {
		t.Errorf("tag %q", r.Tag)
	}
	if fmt.Sprint(dropped) != "[stmtcache_1  ]" {
		t.Errorf("hook calls %q", dropped)
	}
	if _, err := s.Exec(`show app.user_id`); err == nil {
		t.Error("DISCARD ALL kept a SET parameter")
	}
	if sone(t, s, `show statement_timeout`) != "0" {
		t.Error("DISCARD ALL kept statement_timeout")
	}
	if _, err := s.Exec(`select lastval()`); err == nil {
		t.Error("DISCARD ALL kept lastval")
	}
	other := d.NewSession()
	defer other.Close()
	if sone(t, other, `select pg_try_advisory_lock(1)`) != true {
		t.Error("DISCARD ALL kept an advisory lock")
	}
	sexec(t, s, `insert into t (a) values (2)`)
	sexec(t, s, `discard sequences`)
	if _, err := s.Exec(`select currval('t_id_seq')`); err == nil {
		t.Error("DISCARD SEQUENCES kept currval")
	}
	for _, q := range []string{`discard plans`, `discard temp`, `discard temporary`} {
		sexec(t, s, q)
	}
	// Without a wire server there are no named statements.
	if _, err := d.Exec(`deallocate x`); err == nil {
		t.Error("bare DB deallocate of a name succeeded")
	}
	exec(t, d, `deallocate all`)
}

func TestBareSessionWords(t *testing.T) {
	d := openDB(t)
	r := exec(t, d, `select current_user, session_user, user, current_role, current_schema, current_catalog`)
	if got := fmt.Sprint(r.Rows, r.Cols); got != "[[bytdb bytdb bytdb bytdb public bytdb]] [current_user session_user user current_role current_schema current_catalog]" {
		t.Errorf("got %s", got)
	}
	if r := exec(t, d, `select current_user = session_user`); r.Rows[0][0] != true {
		t.Error("comparison of bare words")
	}
}

func TestSessionFunctions(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id serial primary key, a int)`)
	exec(t, d, `create table "Odd" (id int generated by default as identity primary key)`)
	exec(t, d, `create table plain (id int primary key)`)
	exec(t, d, `create sequence s1`)
	s := d.NewSession()
	defer s.Close()
	s.SetBackendPID(4242)
	for q, want := range map[string]string{
		`select pg_backend_pid()`:                                                            "4242",
		`select current_setting('server_version_num')`:                                       "160000",
		`select current_setting('Application_Name')`:                                         "",
		`select current_setting('app.nope', true)`:                                           "<nil>",
		`select current_setting('transaction_isolation')`:                                    "serializable",
		`select current_schemas(false), current_schemas(true)`:                               "{public} {pg_catalog,public}",
		`select pg_is_in_recovery()`:                                                         "false",
		`select to_regclass('plain') is not null, to_regclass('nope')`:                       "true <nil>",
		`select pg_get_serial_sequence('t', 'id')`:                                           "public.t_id_seq",
		`select pg_get_serial_sequence('"Odd"', 'id')`:                                       `public."Odd_id_seq"`,
		`select pg_get_serial_sequence('plain', 'id')`:                                       "<nil>",
		`select has_table_privilege('t', 'select'), has_schema_privilege('public', 'usage')`: "true true",
		`select quote_ident('foo'), quote_ident('Foo'), quote_ident('select'), quote_ident('a"b'), quote_ident('1a'), quote_ident('a1$')`: `foo "Foo" "select" "a""b" "1a" a1$`,
		`select pg_typeof(1), pg_typeof('x'::text), pg_typeof(now())`:                                                                     "bigint text timestamp with time zone",
	} {
		r := sexec(t, s, q)
		var parts []string
		for _, v := range r.Rows[0] {
			parts = append(parts, fmt.Sprint(v))
		}
		if got := strings.Join(parts, " "); got != want {
			t.Errorf("%s = %s, want %s", q, got, want)
		}
	}
	for q, want := range map[string]string{
		`select current_setting('app.nope')`:          `unrecognized configuration parameter "app.nope"`,
		`select pg_get_serial_sequence('nope', 'id')`: `relation "nope" does not exist`,
		`select pg_get_serial_sequence('t', 'zz')`:    `column "zz" of relation "t" does not exist`,
	} {
		if _, err := s.Exec(q); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", q, err, want)
		}
	}

	// set_config is SET: it takes effect and SHOW sees it.
	if v := sone(t, s, `select set_config('statement_timeout', '2s', false)`); v != "2000ms" {
		t.Errorf("set_config returned %v", v)
	}
	if v := sone(t, s, `show statement_timeout`); v != "2000ms" {
		t.Errorf("statement_timeout after set_config = %v", v)
	}
	sexec(t, s, `select set_config('app.tenant', 'acme', false)`)
	if v := sone(t, s, `show app.tenant`); v != "acme" {
		t.Errorf("show app.tenant = %v", v)
	}
	sexec(t, s, `set app.tenant to 'other'`)
	if v := sone(t, s, `select current_setting('app.tenant')`); v != "other" {
		t.Errorf("current_setting after SET = %v", v)
	}
	sexec(t, s, `reset app.tenant`)
	if _, err := s.Exec(`show app.tenant`); err == nil {
		t.Error("RESET left app.tenant")
	}

	// txid_current: stable in a block, fresh per autocommit statement.
	a, b := sone(t, s, `select txid_current()`), sone(t, s, `select txid_current()`)
	if a == b {
		t.Error("autocommit statements share a txid")
	}
	sexec(t, s, `begin`)
	x, y := sone(t, s, `select txid_current()`), sone(t, s, `select pg_current_xact_id()`)
	sexec(t, s, `commit`)
	if x != y {
		t.Errorf("one block, two txids: %v %v", x, y)
	}

	// setval on an identity's implied sequence moves the counter, and
	// public.-qualified names (pg_dump's spelling) resolve.
	sexec(t, s, `select setval(pg_get_serial_sequence('t', 'id'), 41)`)
	if v := sone(t, s, `insert into t (a) values (1) returning id`); v != int64(42) {
		t.Errorf("after setval(41) id = %v", v)
	}
	sexec(t, s, `select pg_catalog.setval('public.t_id_seq', 50, false)`)
	if v := sone(t, s, `insert into t (a) values (1) returning id`); v != int64(50) {
		t.Errorf("after setval(50, false) id = %v", v)
	}
	sexec(t, s, `select setval('public.s1', 9)`)
	if v := sone(t, s, `select nextval('public.s1')`); v != int64(10) {
		t.Errorf("nextval after setval(public.s1, 9) = %v", v)
	}
}

func TestSetShowForms(t *testing.T) {
	d := openDB(t)
	s := d.NewSession()
	defer s.Close()
	for _, q := range []string{
		`set names 'UTF8'`, `set role admin`, `set session authorization 'alice'`,
		`set local app.x = 1`, `set session characteristics as transaction isolation level read committed, read only`,
	} {
		sexec(t, s, q)
	}
	for q, want := range map[string]string{
		`show client_encoding`:                     "UTF8",
		`show role`:                                "admin",
		`show session authorization`:               "alice",
		`show app.x`:                               "1",
		`show default_transaction_isolation`:       "read committed",
		`show default_transaction_read_only`:       "on",
		`show transaction_read_only`:               "on",
		`show lc_collate`:                          "C",
		`show is_superuser`:                        "on",
		`show idle_in_transaction_session_timeout`: "0",
	} {
		if v := sone(t, s, q); v != want {
			t.Errorf("%s = %v, want %s", q, v, want)
		}
	}
	sexec(t, s, `reset role`)
	sexec(t, s, `reset session authorization`)
	if v := sone(t, s, `show role`); v != "none" {
		t.Errorf("role after RESET = %v", v)
	}
	if r := sexec(t, s, `show all`); len(r.Rows) < 30 {
		t.Errorf("SHOW ALL lists %d parameters", len(r.Rows))
	}
}

// default_transaction_read_only and SET TRANSACTION READ ONLY/WRITE.
func TestReadOnlyParams(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key)`)
	s := d.NewSession()
	defer s.Close()
	sexec(t, s, `set default_transaction_read_only = on`)
	if _, err := s.Exec(`insert into t values (1)`); err == nil || !strings.Contains(err.Error(), "read-only transaction") {
		t.Errorf("autocommit write under default read-only: %v", err)
	}
	sexec(t, s, `select * from t`)
	sexec(t, s, `begin`)
	if v := sone(t, s, `show transaction_read_only`); v != "on" {
		t.Errorf("block under default read-only: transaction_read_only = %v", v)
	}
	if _, err := s.Exec(`insert into t values (1)`); err == nil {
		t.Error("write in a default read-only block")
	}
	sexec(t, s, `rollback`)
	sexec(t, s, `begin read write`)
	sexec(t, s, `insert into t values (1)`)
	sexec(t, s, `commit`)
	sexec(t, s, `set default_transaction_read_only = off`)

	sexec(t, s, `begin`)
	sexec(t, s, `set transaction isolation level serializable, read only`)
	if v := sone(t, s, `show transaction_read_only`); v != "on" {
		t.Errorf("after SET TRANSACTION READ ONLY: %v", v)
	}
	if v := sone(t, s, `show transaction_isolation`); v != "serializable" {
		t.Errorf("SET TRANSACTION's isolation half: %v", v)
	}
	if _, err := s.Exec(`insert into t values (2)`); err == nil {
		t.Error("write after SET TRANSACTION READ ONLY")
	}
	sexec(t, s, `rollback`)
	sexec(t, s, `begin read only`)
	if _, err := s.Exec(`set transaction read write`); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Errorf("READ WRITE in a READ ONLY block: %v", err)
	}
	sexec(t, s, `rollback`)
	if r := sexec(t, s, `set transaction read only`); r.Notice == "" {
		t.Error("SET TRANSACTION outside a block did not warn")
	}
	if _, err := s.Exec(`set default_transaction_read_only = maybe`); err == nil {
		t.Error("bad boolean accepted")
	}
}

func TestApplyStartupParams(t *testing.T) {
	d := openDB(t)
	s := d.NewSession()
	defer s.Close()
	if err := s.ApplyStartupParams(map[string]string{
		"user": "alice", "database": "x", "application_name": "psql",
		"DateStyle": "ISO, MDY", "statement_timeout": "5s",
	}); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		`show application_name`:  "psql",
		`show datestyle`:         "ISO, MDY",
		`show statement_timeout`: "5000ms",
	} {
		if v := sone(t, s, q); v != want {
			t.Errorf("%s = %v, want %s", q, v, want)
		}
	}
	if err := d.NewSession().ApplyStartupParams(map[string]string{"statement_timeout": "soon"}); err == nil {
		t.Error("invalid startup statement_timeout accepted")
	}
}

// A bare placeholder in a session function Describes with the
// argument's type, and a text-bound value still works: goose binds
// pg_try_advisory_lock's key as an untyped $1.
func TestSessionFuncParamTypes(t *testing.T) {
	d := openDB(t)
	for q, want := range map[string][]bytdb.ColType{
		`select pg_try_advisory_lock($1)`: {bytdb.TInt},
		`select pg_advisory_lock($1, $2)`: {bytdb.TInt, bytdb.TInt},
		`select current_setting($1, $2)`:  {"", bytdb.TBool},
		`select extract(year from $1)`:    {bytdb.TTimestamp},
	} {
		st, err := d.Prepare(q)
		if err != nil {
			t.Fatal(err)
		}
		info, err := st.Describe()
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if fmt.Sprint(info.Params) != fmt.Sprint(want) {
			t.Errorf("%s: params %v, want %v", q, info.Params, want)
		}
	}
	s := d.NewSession()
	defer s.Close()
	if r, err := s.Exec(`select pg_try_advisory_lock($1)`, "42"); err != nil || r.Rows[0][0] != true {
		t.Fatalf("text-bound key: %v %v", r, err)
	}
	if r, err := s.Exec(`select pg_advisory_unlock($1)`, int64(42)); err != nil || r.Rows[0][0] != true {
		t.Fatalf("unlock: %v %v", r, err)
	}
}
