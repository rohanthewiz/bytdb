package edgeaudit

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/rohanthewiz/bytdb/stdlib"
)

func openStd(t testing.TB, opts string) *sql.DB {
	t.Helper()
	db, err := sql.Open("bytdb", filepath.Join(t.TempDir(), "s.db")+opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestStdlibArgs(t *testing.T) {
	db := openStd(t, "")
	if _, err := db.Exec(`create table ev (id int primary key, at timestamp, d date, tags text[], u uuid, n int, b bytea)`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2024, 5, 6, 7, 8, 9, 123456000, time.FixedZone("x", 3600))
	try := func(name, q string, args ...any) {
		_, err := db.Exec(q, args...)
		t.Logf("%-34s -> %v", name, short(err))
	}
	try("time.Time", `insert into ev (id, at) values (1, $1)`, now)
	try("*time.Time", `insert into ev (id, at) values (2, $1)`, &now)
	try("sql.NullTime{Valid}", `insert into ev (id, at) values (3, $1)`, sql.NullTime{Time: now, Valid: true})
	try("sql.Null[time.Time]", `insert into ev (id, at) values (4, $1)`, sql.Null[time.Time]{V: now, Valid: true})
	try("time.Time into date col", `insert into ev (id, d) values (5, $1)`, now)
	try("[]string into text[]", `insert into ev (id, tags) values (6, $1)`, []string{"a", "b"})
	try("uint64 max", `insert into ev (id, n) values (7, $1)`, uint64(1<<63))
	try("[16]byte uuid", `insert into ev (id, u) values (8, $1)`, [16]byte{1})
	try("nil []byte", `insert into ev (id, b) values (9, $1)`, []byte(nil))
	try("multi-statement Exec", `insert into ev (id) values (20); insert into ev (id) values (21)`)
	try("where at < time.Time", `delete from ev where at < $1`, time.Now())
	try("where at < NullTime", `delete from ev where at < $1`, sql.NullTime{Time: time.Now(), Valid: true})

	var d time.Time
	db.Exec(`insert into ev (id, d) values (30, '2024-01-02')`)
	err := db.QueryRow(`select d from ev where id = 30`).Scan(&d)
	t.Logf("scan date into time.Time            -> %v", short(err))
	var b []byte
	var isNull sql.NullString
	err = db.QueryRow(`select b, b from ev where id = 9`).Scan(&b, &isNull)
	t.Logf("nil []byte stored as: valid=%v err=%v", isNull.Valid, err)
}

// BeginTx / statements blocked on the single writer lock: do they honor ctx?
func TestStdlibCtxOnWriterLock(t *testing.T) {
	db := openStd(t, "")
	db.Exec(`create table t (id int primary key)`)
	ctx := context.Background()
	tx1, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx1.Exec(`insert into t values (1)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"BeginTx", "autocommit Exec"} {
		cctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		start := time.Now()
		done := make(chan error, 1)
		go func() {
			if name == "BeginTx" {
				tx2, err := db.BeginTx(cctx, nil)
				if err == nil {
					_, err = tx2.ExecContext(cctx, `insert into t values (2)`)
					tx2.Rollback()
				}
				done <- err
			} else {
				_, err := db.ExecContext(cctx, `insert into t values (3)`)
				done <- err
			}
		}()
		select {
		case err := <-done:
			t.Logf("%s behind a held writer lock, 200ms ctx: returned after %v: %v", name, time.Since(start).Round(time.Millisecond), short(err))
		case <-time.After(3 * time.Second):
			t.Errorf("%s: still blocked 3s after its 200ms context expired (ctx not honored while waiting for the writer lock)", name)
			tx1.Rollback()
			<-done
			tx1, _ = db.BeginTx(ctx, nil)
			tx1.Exec(`insert into t values (1)`)
		}
		cancel()
	}
	tx1.Rollback()
}
