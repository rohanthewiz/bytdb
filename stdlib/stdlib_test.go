package stdlib

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/bytdb"
)

// open gives a *sql.DB on a fresh database, closed when the test ends.
func open(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open(DriverName, dsn)
	if err != nil {
		t.Fatalf("open %q: %v", dsn, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = db.Ping(); err != nil {
		t.Fatalf("ping %q: %v", dsn, err)
	}
	return db
}

func tempDSN(t *testing.T, suffix string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.bytdb") + suffix
}

func exec(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%.50s: %v", s, err)
		}
	}
}

// refs reports how many connectors hold the engine for path open, 0 when none
// is.
func refs(t *testing.T, path string) int {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	enginesMu.Lock()
	defer enginesMu.Unlock()
	if sh, ok := engines[abs]; ok {
		return sh.refs
	}
	return 0
}

func TestParseDSN(t *testing.T) {
	cases := []struct {
		dsn      string
		wantPath string
		wantSig  string
		wantOpts int
	}{
		{"app.bytdb", "app.bytdb", "", 0},
		{"file:app.bytdb", "app.bytdb", "", 0},
		{"/var/lib/app.bytdb", "/var/lib/app.bytdb", "", 0},
		{"my db.bytdb", "my db.bytdb", "", 0}, // spaces need no escaping
		{"app.bytdb?sync=never", "app.bytdb", "sync=never", 1},
		{"app.bytdb?concurrent_writes=true", "app.bytdb", "concurrent_writes=true", 1},
		// off is recorded but adds no option, so it still differs from unset
		{"app.bytdb?concurrent_writes=false", "app.bytdb", "concurrent_writes=false", 0},
		{"app.bytdb?sync=never&concurrent_writes=1", "app.bytdb",
			"concurrent_writes=true&sync=never", 2},
		{"app.bytdb?encryption_key=" + strings.Repeat("ab", 32), "app.bytdb",
			"encryption_key=set", 1},
	}
	for _, c := range cases {
		cfg, err := ParseDSN(c.dsn)
		if err != nil {
			t.Errorf("ParseDSN(%q): %v", c.dsn, err)
			continue
		}
		if cfg.Path != c.wantPath {
			t.Errorf("ParseDSN(%q).Path = %q, want %q", c.dsn, cfg.Path, c.wantPath)
		}
		if cfg.signature != c.wantSig {
			t.Errorf("ParseDSN(%q) signature = %q, want %q", c.dsn, cfg.signature, c.wantSig)
		}
		if len(cfg.Options) != c.wantOpts {
			t.Errorf("ParseDSN(%q) gave %d options, want %d",
				c.dsn, len(cfg.Options), c.wantOpts)
		}
	}
}

// A DSN that does not say what it means has to fail at open, not quietly do
// something else — an unnoticed durability or encryption setting is the whole
// point of the option.
func TestParseDSNRejects(t *testing.T) {
	bad := []string{
		"",
		"file:",
		"?sync=never",              // no path
		"app.bytdb?syn=never",      // typo'd name
		"app.bytdb?sync=sometimes", // not a policy
		"app.bytdb?concurrent_writes=yes-please",
		"app.bytdb?encryption_key=nothex",
		"app.bytdb?encryption_key=abcd", // 2 bytes, not 32
	}
	for _, dsn := range bad {
		if _, err := ParseDSN(dsn); err == nil {
			t.Errorf("ParseDSN(%q) should have failed", dsn)
		}
	}
	// the key must never appear in an error
	key := strings.Repeat("cd", 40) // 40 bytes: wrong length, valid hex
	_, err := ParseDSN("app.bytdb?encryption_key=" + key)
	if err == nil {
		t.Fatal("a 40-byte key should be refused")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("the error leaks key material: %v", err)
	}
}

func TestQueryAndExec(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	exec(t, db, "CREATE TABLE cats (id int PRIMARY KEY, name text, age int)")

	res, err := db.Exec("INSERT INTO cats VALUES (1, 'mia', 4), (2, 'otto', 7)")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if aff, _ := res.RowsAffected(); aff != 2 {
		t.Errorf("affected = %d, want 2", aff)
	}

	var name string
	var age int
	if err = db.QueryRow("SELECT name, age FROM cats WHERE age > $1", 5).
		Scan(&name, &age); err != nil {
		t.Fatalf("query: %v", err)
	}
	if name != "otto" || age != 7 {
		t.Errorf("got %s/%d, want otto/7", name, age)
	}

	// NULL needs a destination that takes it
	exec(t, db, "INSERT INTO cats (id, name) VALUES (3, 'nell')")
	var nullAge sql.NullInt64
	if err = db.QueryRow("SELECT age FROM cats WHERE id = 3").Scan(&nullAge); err != nil {
		t.Fatalf("null scan: %v", err)
	}
	if nullAge.Valid {
		t.Errorf("age = %v, want NULL", nullAge)
	}
}

// The date/time and UUID types ride on int64 and []byte so they sort in the
// key encoding; the driver owes callers the values those stand for.
func TestValueConversion(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	exec(t, db,
		`CREATE TABLE ev (id int PRIMARY KEY, at timestamp, day date,
			uid uuid, tags text[], meta jsonb, raw bytea, ok bool, ratio float)`,
		`INSERT INTO ev VALUES (1, '2024-03-05 10:30:00', '1815-12-10',
			'11111111-2222-3333-4444-555555555555', '{a,b}', '{"x":1}',
			'\x0102', true, 1.5)`)

	var (
		at             time.Time
		day, uid, tags string
		meta           string
		raw            []byte
		ok             bool
		ratio          float64
	)
	err := db.QueryRow("SELECT at, day, uid, tags, meta, raw, ok, ratio FROM ev").
		Scan(&at, &day, &uid, &tags, &meta, &raw, &ok, &ratio)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if want := time.Date(2024, 3, 5, 10, 30, 0, 0, time.UTC); !at.Equal(want) {
		t.Errorf("at = %v, want %v", at, want)
	}
	if day != "1815-12-10" {
		t.Errorf("day = %q — a date should not be a midnight instant", day)
	}
	if uid != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("uid = %q", uid)
	}
	if tags != "{a,b}" || meta != `{"x":1}` {
		t.Errorf("tags = %q, meta = %q", tags, meta)
	}
	if string(raw) != "\x01\x02" || !ok || ratio != 1.5 {
		t.Errorf("raw = %x, ok = %v, ratio = %v", raw, ok, ratio)
	}
}

func TestColumnTypes(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	exec(t, db,
		`CREATE TABLE ev (id int PRIMARY KEY, at timestamp, name text, ok bool,
			ratio float, raw bytea, day date, uid uuid, tags text[], meta jsonb)`,
		`INSERT INTO ev VALUES (1, '2024-03-05 10:30:00', 'x', true, 1.5, '\x01',
			'2024-03-05', '11111111-2222-3333-4444-555555555555', '{a}', '{"x":1}')`)

	rows, err := db.Query(
		"SELECT id, at, name, ok, ratio, raw, day, uid, tags, meta FROM ev")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	types, err := rows.ColumnTypes()
	if err != nil {
		t.Fatalf("column types: %v", err)
	}
	wantName := []string{"INT", "TIMESTAMP", "STRING", "BOOL", "FLOAT", "BYTES",
		"DATE", "UUID", "TEXT_ARRAY", "JSONB"}
	// the types that ride on another representation scan as what the driver
	// decodes them into, not as what they are stored as
	wantScan := []string{"int64", "time.Time", "string", "bool", "float64", "[]uint8",
		"string", "string", "string", "string"}
	for i, ct := range types {
		if ct.DatabaseTypeName() != wantName[i] {
			t.Errorf("column %d type name = %q, want %q", i, ct.DatabaseTypeName(), wantName[i])
		}
		if got := ct.ScanType().String(); got != wantScan[i] {
			t.Errorf("column %d scan type = %s, want %s", i, got, wantScan[i])
		}
	}
}

// btypedb takes the file for itself, so handles on one path must share an
// engine — including when the path is spelled differently.
func TestSharedEngine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shared.bytdb")

	a := open(t, path)
	b := open(t, "file:"+path)
	c := open(t, filepath.Join(dir, ".", "shared.bytdb"))

	exec(t, a, "CREATE TABLE t (n int PRIMARY KEY)")
	exec(t, b, "INSERT INTO t VALUES (1)")

	var n int
	if err := c.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1 — the handles are not on one engine", n)
	}
	if got := refs(t, path); got != 3 {
		t.Errorf("engine has %d references, want 3", got)
	}
}

// Adopting an open engine for a DSN that asked for different options would
// mean silently getting neither encryption nor concurrency.
func TestConflictingOptionsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opts.bytdb")
	open(t, path+"?concurrent_writes=true")

	second, err := sql.Open(DriverName, path+"?sync=never")
	if err == nil {
		err = second.Ping()
		_ = second.Close()
	}
	if err == nil {
		t.Fatal("a second open with different options should fail")
	}
	if !strings.Contains(err.Error(), "different options") {
		t.Errorf("error should explain the clash: %v", err)
	}

	// the same options are fine
	same := open(t, path+"?concurrent_writes=true")
	if err = same.Ping(); err != nil {
		t.Errorf("matching options should share the engine: %v", err)
	}
}

// Closing the last handle closes the engine — that is what flushes the WAL
// and drops the file lock, so a reopen must find the data.
func TestEngineClosesWithLastHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.bytdb")

	first, err := sql.Open(DriverName, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	second, err := sql.Open(DriverName, path)
	if err != nil {
		t.Fatalf("open again: %v", err)
	}
	if _, err = first.Exec("CREATE TABLE t (n int PRIMARY KEY)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err = first.Exec("INSERT INTO t VALUES (42)"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err = first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}
	var n int
	if err = second.QueryRow("SELECT n FROM t").Scan(&n); err != nil {
		t.Fatalf("the engine should outlive the first handle: %v", err)
	}
	if got := refs(t, path); got != 1 {
		t.Errorf("engine has %d references after one handle closed, want 1", got)
	}

	if err = second.Close(); err != nil {
		t.Fatalf("close second: %v", err)
	}
	if got := refs(t, path); got != 0 {
		t.Errorf("engine still open (%d references) after the last handle closed", got)
	}

	again := open(t, path)
	if err = again.QueryRow("SELECT n FROM t").Scan(&n); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if n != 42 {
		t.Errorf("n = %d, want 42", n)
	}
}

// A connection straight from the driver, with no *sql.DB to own the
// connector, releases the engine itself.
func TestDriverOpenReleasesEngine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "direct.bytdb")

	cn, err := (Driver{}).Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := refs(t, path); got != 1 {
		t.Fatalf("engine has %d references, want 1", got)
	}
	if err = cn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := refs(t, path); got != 0 {
		t.Errorf("engine still open (%d references) after the connection closed", got)
	}
}

// A program already holding an engine cannot open the file again, so
// OpenEngine wraps the one it has — and must not close it.
func TestOpenEngine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned.bytdb")
	e, err := bytdb.Open(path)
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	defer func() { _ = e.Close() }()

	db := OpenEngine(e)
	if _, err = db.Exec("CREATE TABLE t (n int PRIMARY KEY)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err = db.Exec("INSERT INTO t VALUES (7)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// a caller-owned engine never enters the registry
	if got := refs(t, path); got != 0 {
		t.Errorf("a caller's engine should not be registered (%d references)", got)
	}
	if err = db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	// the engine is still usable through its native API
	row, ok, err := e.Get("t", int64(7))
	if err != nil || !ok {
		t.Fatalf("the engine should outlive the *sql.DB: ok=%v err=%v", ok, err)
	}
	_ = row
}

// The escape hatch has to reach the same engine the SQL went to.
func TestEngineAccessor(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	exec(t, db, "CREATE TABLE t (n int PRIMARY KEY)", "INSERT INTO t VALUES (1), (2)")

	e, err := Engine(context.Background(), db)
	if err != nil {
		t.Fatalf("Engine: %v", err)
	}
	var count int
	for _, err := range e.Scan("t") {
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		count++
	}
	if count != 2 {
		t.Errorf("native scan saw %d rows, want 2", count)
	}
}

func TestPreparedStatement(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	exec(t, db, "CREATE TABLE t (n int PRIMARY KEY, s text)")

	ins, err := db.Prepare("INSERT INTO t VALUES ($1, $2)")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer ins.Close()
	for i, s := range []string{"a", "b", "c"} {
		if _, err = ins.Exec(i, s); err != nil {
			t.Fatalf("exec %d: %v", i, err)
		}
	}
	if _, err = ins.Exec(9); err == nil {
		t.Error("the wrong argument count should be refused")
	}

	var s string
	if err = db.QueryRow("SELECT s FROM t WHERE n = $1", 1).Scan(&s); err != nil || s != "b" {
		t.Errorf("s = %q, %v", s, err)
	}
}

func TestNamedParametersRefused(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	exec(t, db, "CREATE TABLE t (n int PRIMARY KEY)")

	_, err := db.Exec("INSERT INTO t VALUES ($1)", sql.Named("n", 1))
	if err == nil {
		t.Fatal("a named parameter should be refused")
	}
	if !strings.Contains(err.Error(), "named parameters") {
		t.Errorf("error should explain the refusal: %v", err)
	}
}

func TestTimeParameter(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	exec(t, db, "CREATE TABLE ev (id int PRIMARY KEY, at timestamp)")

	when := time.Date(2024, 3, 5, 10, 30, 0, 0, time.UTC)
	if _, err := db.Exec("INSERT INTO ev VALUES (1, $1)", when); err != nil {
		t.Fatalf("insert with a time arg: %v", err)
	}
	var got time.Time
	if err := db.QueryRow("SELECT at FROM ev").Scan(&got); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !got.Equal(when) {
		t.Errorf("at = %v, want %v", got, when)
	}
}

func TestTransaction(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	exec(t, db, "CREATE TABLE t (n int PRIMARY KEY)")

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err = tx.Exec("INSERT INTO t VALUES (1)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	tx, err = db.Begin()
	if err != nil {
		t.Fatalf("begin again: %v", err)
	}
	if _, err = tx.Exec("INSERT INTO t VALUES (2)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var n int
	if err = db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1 — the rolled-back row survived", n)
	}
}

// The levels bytdb understands must reach BEGIN; the ones it does not must be
// refused rather than silently downgraded.
func TestIsolationLevels(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	exec(t, db, "CREATE TABLE t (n int PRIMARY KEY)")
	ctx := context.Background()

	for _, lvl := range []sql.IsolationLevel{
		sql.LevelDefault, sql.LevelSerializable, sql.LevelRepeatableRead,
		sql.LevelReadCommitted, sql.LevelReadUncommitted, sql.LevelSnapshot,
	} {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: lvl})
		if err != nil {
			t.Errorf("BeginTx(%s): %v", lvl, err)
			continue
		}
		if _, err = tx.Exec("INSERT INTO t VALUES ($1)", int(lvl)); err != nil {
			t.Errorf("insert under %s: %v", lvl, err)
		}
		if err = tx.Commit(); err != nil {
			t.Errorf("commit under %s: %v", lvl, err)
		}
	}

	if _, err := db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelLinearizable,
	}); err == nil {
		t.Error("linearizable is not a bytdb level and should be refused")
	}

	// a read-only block takes no writer lock, and refuses writes
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin read only: %v", err)
	}
	if _, err = tx.Exec("INSERT INTO t VALUES (99)"); err == nil {
		t.Error("a read-only block should refuse a write")
	}
	_ = tx.Rollback()
}

// A block left open when the connection goes back to the pool must not become
// the next caller's problem.
func TestResetRollsBackOpenBlock(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	exec(t, db, "CREATE TABLE t (n int PRIMARY KEY)")
	ctx := context.Background()

	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	for _, s := range []string{"BEGIN", "INSERT INTO t VALUES (1)"} {
		if _, err = c.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err = c.Close(); err != nil { // no COMMIT
		t.Fatalf("close conn: %v", err)
	}

	var n int
	if err = db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0 — the abandoned block was not rolled back", n)
	}
}

// Under concurrent writes a losing commit is retryable, and has to stay
// recognizable through the driver's error wrapping.
func TestConcurrentWriteConflict(t *testing.T) {
	db := open(t, tempDSN(t, "?concurrent_writes=true"))
	exec(t, db,
		"CREATE TABLE t (id int PRIMARY KEY, n int)",
		"INSERT INTO t VALUES (1, 0)")
	ctx := context.Background()

	tx1, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin 1: %v", err)
	}
	tx2, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin 2: %v", err)
	}

	if _, err = tx1.Exec("UPDATE t SET n = 1 WHERE id = 1"); err != nil {
		t.Fatalf("update 1: %v", err)
	}
	if _, err = tx2.Exec("UPDATE t SET n = 2 WHERE id = 1"); err != nil {
		t.Fatalf("update 2: %v", err)
	}

	if err = tx1.Commit(); err != nil {
		t.Fatalf("the first committer should win: %v", err)
	}
	err = tx2.Commit()
	if err == nil {
		t.Fatal("the second commit should have conflicted")
	}
	if !errors.Is(err, bytdb.ErrTxConflict) {
		t.Errorf("errors.Is should see through the wrapping: %v", err)
	}
	if !IsRetryable(err) {
		t.Errorf("IsRetryable should hold for a conflict: %v", err)
	}
	_ = tx2.Rollback()
}

// Without the option, writers serialize on the engine lock and no caller ever
// sees a conflict.
func TestSingleWriterHasNoConflicts(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	exec(t, db, "CREATE TABLE t (n int PRIMARY KEY)")

	const writers, writes = 4, 20
	errs := make(chan error, writers)
	done := make(chan struct{})
	for w := range writers {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := range writes {
				if _, err := db.Exec("INSERT INTO t VALUES ($1)", w*writes+i); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for range writers {
		<-done
	}
	close(errs)
	if err := <-errs; err != nil {
		t.Fatalf("concurrent write failed: %v", err)
	}

	var n int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != writers*writes {
		t.Errorf("count = %d, want %d", n, writers*writes)
	}
}

// Cancellation stops a statement mid-execution, because bytdb's row pumps
// poll the context.
func TestCancel(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	exec(t, db, "CREATE TABLE t (n int PRIMARY KEY)")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := db.QueryContext(ctx, "SELECT n FROM t"); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

// bytdb draws identity values through RETURNING, so LastInsertId has to say
// it has no answer rather than invent a zero.
func TestNoLastInsertID(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	exec(t, db, "CREATE TABLE t (n int PRIMARY KEY)")

	res, err := db.Exec("INSERT INTO t VALUES (1)")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err = res.LastInsertId(); err == nil {
		t.Error("LastInsertId should report that bytdb has none")
	}
}

// The file is created on first open, the way sqlite's is.
func TestCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.bytdb")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the file should not exist yet: %v", err)
	}
	open(t, path)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("opening should have created the file: %v", err)
	}
}

// INSERT ... RETURNING is what LastInsertId points callers at, so it had
// better work: a DML statement handed to Query, coming back with the row as
// stored — the drawn identity value included.
func TestReturning(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	exec(t, db, "CREATE TABLE cats (id serial PRIMARY KEY, name text)")

	var id int64
	var name string
	if err := db.QueryRow("INSERT INTO cats (name) VALUES ($1) RETURNING id, name", "mia").
		Scan(&id, &name); err != nil {
		t.Fatalf("insert returning: %v", err)
	}
	if id != 1 || name != "mia" {
		t.Errorf("returned %d/%s, want 1/mia", id, name)
	}

	// the drawn value is the row's, and the next draw moves on
	if err := db.QueryRow("INSERT INTO cats (name) VALUES ('otto') RETURNING id").
		Scan(&id); err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if id != 2 {
		t.Errorf("second id = %d, want 2", id)
	}

	// UPDATE and DELETE return rows too
	rows, err := db.Query("UPDATE cats SET name = 'otto2' WHERE id = 2 RETURNING id, name")
	if err != nil {
		t.Fatalf("update returning: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("UPDATE ... RETURNING gave no rows")
	}
	if err = rows.Scan(&id, &name); err != nil || name != "otto2" {
		t.Errorf("updated to %q: %v", name, err)
	}
}

// A statement that fails inside a block aborts it: everything but ROLLBACK is
// refused until it ends. The connection then goes back to the pool, and must
// come out of it clean.
func TestFailedStatementAbortsBlock(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	exec(t, db, "CREATE TABLE t (n int PRIMARY KEY)")

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err = tx.Exec("INSERT INTO t VALUES (1)"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err = tx.Exec("INSERT INTO nosuchtable VALUES (1)"); err == nil {
		t.Fatal("a write to a missing table should fail")
	}
	// the block is now aborted, and says so rather than pretending
	_, err = tx.Exec("INSERT INTO t VALUES (2)")
	if err == nil {
		t.Fatal("an aborted block should refuse further statements")
	}
	if !strings.Contains(err.Error(), "aborted") {
		t.Errorf("error should name the aborted block: %v", err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatalf("rollback of an aborted block: %v", err)
	}

	// the pooled connection is reusable, and the block took its row with it
	if _, err = db.Exec("INSERT INTO t VALUES (3)"); err != nil {
		t.Fatalf("the connection did not come back clean: %v", err)
	}
	var n int
	if err = db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1 — only the row written after the block", n)
	}
}

// Savepoints are reachable only as statements, database/sql having no API for
// them; a block must still be able to rewind to one and carry on.
func TestSavepoints(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	exec(t, db, "CREATE TABLE t (n int PRIMARY KEY)")

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, s := range []string{
		"INSERT INTO t VALUES (1)",
		"SAVEPOINT sp",
		"INSERT INTO t VALUES (2)",
		"ROLLBACK TO SAVEPOINT sp",
		"INSERT INTO t VALUES (3)",
	} {
		if _, err = tx.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	rows, err := db.Query("SELECT n FROM t ORDER BY n")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var n int
		if err = rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, n)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Errorf("rows = %v, want [1 3] — the rewound row survived", got)
	}
}

// A prepared statement has to work as a query and inside a transaction, which
// is where database/sql re-binds it to the transaction's connection.
func TestPreparedQueryAndTxStmt(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	exec(t, db, "CREATE TABLE t (n int PRIMARY KEY, s text)",
		"INSERT INTO t VALUES (1, 'a'), (2, 'b')")

	sel, err := db.Prepare("SELECT s FROM t WHERE n = $1")
	if err != nil {
		t.Fatalf("prepare select: %v", err)
	}
	defer sel.Close()
	var s string
	if err = sel.QueryRow(2).Scan(&s); err != nil || s != "b" {
		t.Errorf("prepared query gave %q: %v", s, err)
	}

	ins, err := db.Prepare("INSERT INTO t VALUES ($1, $2)")
	if err != nil {
		t.Fatalf("prepare insert: %v", err)
	}
	defer ins.Close()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err = tx.Stmt(ins).Exec(3, "c"); err != nil {
		t.Fatalf("prepared insert in a transaction: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if err = sel.QueryRow(3).Scan(&s); err != nil || s != "c" {
		t.Errorf("row written through the transaction's statement: %q, %v", s, err)
	}
}

// The DSN's engine options have to actually reach bytdb.Open, not merely
// parse. Encryption is the one that proves it: the database is unreadable
// without the key.
func TestEncryptionOptionApplies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sealed.bytdb")
	const key = "encryption_key=" + "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"

	db, err := sql.Open(DriverName, path+"?"+key)
	if err != nil {
		t.Fatalf("open encrypted: %v", err)
	}
	if _, err = db.Exec("CREATE TABLE t (n int PRIMARY KEY, s text)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err = db.Exec("INSERT INTO t VALUES (1, 'secret')"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err = db.Close(); err != nil { // releases the engine and the file lock
		t.Fatalf("close: %v", err)
	}

	// without the key it must not open
	plain, err := sql.Open(DriverName, path)
	if err == nil {
		err = plain.Ping()
		_ = plain.Close()
	}
	if err == nil {
		t.Error("an encrypted database should not open without its key")
	}

	// nor with the wrong one
	wrong, err := sql.Open(DriverName, path+"?encryption_key="+strings.Repeat("aa", 32))
	if err == nil {
		err = wrong.Ping()
		_ = wrong.Close()
	}
	if err == nil {
		t.Error("an encrypted database should not open under the wrong key")
	}

	// with the right key the rows are there
	again := open(t, path+"?"+key)
	var s string
	if err = again.QueryRow("SELECT s FROM t WHERE n = 1").Scan(&s); err != nil {
		t.Fatalf("reopen with the key: %v", err)
	}
	if s != "secret" {
		t.Errorf("s = %q, want secret", s)
	}
}

// The pre-context driver interfaces are the ones database/sql never calls,
// reachable only by a caller holding the driver itself. They still have to
// work.
func TestPreContextDriverInterfaces(t *testing.T) {
	cn, err := (Driver{}).Open(filepath.Join(t.TempDir(), "direct.bytdb"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = cn.Close() }()

	ddl, err := cn.Prepare("CREATE TABLE t (n int PRIMARY KEY)")
	if err != nil {
		t.Fatalf("prepare ddl: %v", err)
	}
	if ddl.NumInput() != 0 {
		t.Errorf("NumInput = %d, want 0", ddl.NumInput())
	}
	if _, err = ddl.Exec(nil); err != nil {
		t.Fatalf("exec ddl: %v", err)
	}
	_ = ddl.Close()

	// a transaction through the pre-context Begin
	tx, err := cn.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	ins, err := cn.Prepare("INSERT INTO t VALUES ($1)")
	if err != nil {
		t.Fatalf("prepare insert: %v", err)
	}
	if ins.NumInput() != 1 {
		t.Errorf("NumInput = %d, want 1", ins.NumInput())
	}
	if _, err = ins.Exec([]driver.Value{int64(7)}); err != nil {
		t.Fatalf("exec insert: %v", err)
	}
	_ = ins.Close()
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	sel, err := cn.Prepare("SELECT n FROM t WHERE n = $1")
	if err != nil {
		t.Fatalf("prepare select: %v", err)
	}
	defer func() { _ = sel.Close() }()
	rows, err := sel.Query([]driver.Value{int64(7)})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()

	dest := make([]driver.Value, len(rows.Columns()))
	if err = rows.Next(dest); err != nil {
		t.Fatalf("next: %v", err)
	}
	if dest[0] != int64(7) {
		t.Errorf("n = %v, want 7", dest[0])
	}
	if err = rows.Next(dest); err != io.EOF {
		t.Errorf("a spent result should report io.EOF, got %v", err)
	}
}

// convert is the one place a value's Go type is trusted; an unrecognized one
// has to be an error rather than a best-effort rendering.
func TestConvertRejectsUnknownType(t *testing.T) {
	if _, err := convert(struct{ x int }{1}, bytdb.TString); err == nil {
		t.Error("an unsupported Go type should be an error")
	}
	// a stored type whose value is not the representation it rides on falls
	// through to the general cases rather than misreading it
	v, err := convert("2024-03-05", bytdb.TTimestamp)
	if err != nil || v != "2024-03-05" {
		t.Errorf("convert(string, timestamp) = %v, %v", v, err)
	}
}

// A row shorter than the result's column count reads as NULL rather than
// panicking.
func TestShortRowReadsAsNull(t *testing.T) {
	r := &rows{
		cols:  []string{"a", "b"},
		types: []bytdb.ColType{bytdb.TInt, bytdb.TInt},
		vals:  [][]any{{int64(1)}},
	}
	dest := make([]driver.Value, 2)
	if err := r.Next(dest); err != nil {
		t.Fatalf("next: %v", err)
	}
	if dest[0] != int64(1) || dest[1] != nil {
		t.Errorf("dest = %v, want [1 <nil>]", dest)
	}
}
