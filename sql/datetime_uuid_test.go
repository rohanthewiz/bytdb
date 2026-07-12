package sql

// Tests for the TIMESTAMP/DATE/UUID types: int64-micros / int64-days /
// 16-byte runtime representations, text-literal adaptation, ordering,
// index pushdown, and the date/time functions.

import (
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/bytdb"
)

func TestTimestampDateColumns(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table ev (
		id int primary key,
		at timestamp not null,
		day date,
		tagged timestamptz
	)`)
	exec(t, d, `create index ev_at on ev (at)`)

	// Text literals adapt to the column types, as quoted literals do
	// for every other type; the T-separated and zoned forms parse too.
	exec(t, d, `insert into ev values
		(1, '2024-03-05 10:30:00', '2024-03-05', '2024-03-05T10:30:00Z'),
		(2, '2024-03-05 10:30:00.123456+00', '1969-12-31', null),
		(3, '2031-01-01 00:00:00+02', null, null)`)

	// Stored values are micros / days since the Unix epoch.
	res := exec(t, d, `select at, day from ev where id = 1`)
	wantAt := time.Date(2024, 3, 5, 10, 30, 0, 0, time.UTC).UnixMicro()
	if res.Rows[0][0].(int64) != wantAt {
		t.Fatalf("at = %v, want %v", res.Rows[0][0], wantAt)
	}
	if res.Types[0] != bytdb.TTimestamp || res.Types[1] != bytdb.TDate {
		t.Fatalf("types = %v", res.Types)
	}
	// A pre-1970 date is negative days.
	res = exec(t, d, `select day from ev where id = 2`)
	if res.Rows[0][0].(int64) != -1 {
		t.Fatalf("1969-12-31 = %v days", res.Rows[0][0])
	}

	// Comparisons and range pushdown against text literals; the zoned
	// insert normalized to UTC (+02 is 2030-12-31 22:00 UTC).
	res = exec(t, d, `select id from ev where at >= '2024-03-05 10:30:00.000001' order by at`)
	if len(res.Rows) != 2 || res.Rows[0][0].(int64) != 2 || res.Rows[1][0].(int64) != 3 {
		t.Fatalf("range over timestamps: %v", res.Rows)
	}
	res = exec(t, d, `select id from ev where at < '2030-12-31' order by id`)
	if len(res.Rows) != 2 {
		t.Fatalf("upper bound: %v", res.Rows)
	}

	// Casts work both ways: text to timestamp, and epoch micros in.
	res = exec(t, d, `select id from ev where at = '2024-03-05T10:30:00'::timestamp`)
	if len(res.Rows) != 1 || res.Rows[0][0].(int64) != 1 {
		t.Fatalf("cast comparison: %v", res.Rows)
	}

	// Garbage errors like any bad literal.
	if _, err := d.Exec(`insert into ev values (9, 'not a time', null, null)`); err == nil ||
		!strings.Contains(err.Error(), "invalid input syntax for type timestamp") {
		t.Fatalf("bad timestamp: %v", err)
	}
	if _, err := d.Exec(`insert into ev values (9, '2024-01-01', '2024-13-40', null)`); err == nil ||
		!strings.Contains(err.Error(), "invalid input syntax for type date") {
		t.Fatalf("bad date: %v", err)
	}
}

func TestDateTimeFunctions(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table logs (id int primary key, at timestamp)`)

	before := time.Now().UnixMicro()
	exec(t, d, `insert into logs values (1, now()), (2, current_timestamp)`)
	after := time.Now().UnixMicro()
	res := exec(t, d, `select at from logs order by id`)
	for _, row := range res.Rows {
		at := row[0].(int64)
		if at < before || at > after {
			t.Fatalf("now() = %d outside [%d, %d]", at, before, after)
		}
	}

	res = exec(t, d, `select current_date`)
	wantDay := time.Now().UTC().Truncate(24*time.Hour).Unix() / 86400
	if got := res.Rows[0][0].(int64); got != wantDay && got != wantDay+1 { // midnight race
		t.Fatalf("current_date = %v, want ~%v", got, wantDay)
	}
	if res.Types[0] != bytdb.TDate {
		t.Fatalf("current_date type = %v", res.Types[0])
	}
}

func TestUUIDColumns(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table u (id uuid primary key, name text)`)

	exec(t, d, `insert into u values
		('550e8400-e29b-41d4-a716-446655440000', 'ada'),
		('00000000-0000-0000-0000-000000000001', 'grace')`)

	// Uppercase and undashed forms normalize on the way in.
	exec(t, d, `insert into u values ('6BA7B810-9DAD-11D1-80B4-00C04FD430C8', 'alan')`)
	res := exec(t, d, `select name from u where id = '6ba7b8109dad11d180b400c04fd430c8'`)
	if len(res.Rows) != 1 || res.Rows[0][0] != "alan" {
		t.Fatalf("uuid normalization: %v", res.Rows)
	}

	// Point get on the uuid primary key; bytewise ordering for ORDER BY.
	res = exec(t, d, `select name from u where id = '550e8400-e29b-41d4-a716-446655440000'`)
	if len(res.Rows) != 1 || res.Rows[0][0] != "ada" {
		t.Fatalf("uuid point get: %v", res.Rows)
	}
	res = exec(t, d, `select name from u order by id`)
	if res.Rows[0][0] != "grace" { // 0000... sorts first
		t.Fatalf("uuid order: %v", res.Rows)
	}

	// Duplicate keys still collide after normalization.
	if _, err := d.Exec(`insert into u values ('550E8400-E29B-41D4-A716-446655440000', 'dup')`); err == nil {
		t.Fatal("duplicate uuid accepted")
	}
	if _, err := d.Exec(`insert into u values ('nope', 'x')`); err == nil ||
		!strings.Contains(err.Error(), "invalid input syntax for type uuid") {
		t.Fatalf("bad uuid: %v", err)
	}

	// gen_random_uuid: right shape, version 4, and 16-byte runtime kind.
	res = exec(t, d, `select gen_random_uuid()`)
	b, ok := res.Rows[0][0].([]byte)
	if !ok || len(b) != 16 || b[6]>>4 != 4 {
		t.Fatalf("gen_random_uuid: %#v", res.Rows[0][0])
	}
	if res.Types[0] != bytdb.TUUID {
		t.Fatalf("gen_random_uuid type = %v", res.Types[0])
	}
}

// TestTimestampGoBinding: embedded callers bind time.Time naturally.
func TestTimestampGoBinding(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table b (id int primary key, at timestamp, day date)`)
	when := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	if _, err := d.Exec(`insert into b values ($1, $2, $3)`, int64(1), when, when); err != nil {
		t.Fatal(err)
	}
	res := exec(t, d, `select at, day from b`)
	if res.Rows[0][0].(int64) != when.UnixMicro() {
		t.Fatalf("bound time.Time: %v", res.Rows[0][0])
	}
	if res.Rows[0][1].(int64) != when.Unix()/86400 {
		t.Fatalf("bound date: %v", res.Rows[0][1])
	}
}
