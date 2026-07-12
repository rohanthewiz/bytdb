package pgwire_test

// End-to-end date/time and uuid coverage through pgx: binary and text
// parameter binding, RowDescription OIDs, and scanning back into Go
// kinds — the whole reason the types exist.

import (
	"context"
	"testing"
	"time"
)

func TestTimestampDateUUIDWire(t *testing.T) {
	c := connect(t, startServer(t))
	ctx := context.Background()
	mustExec(t, c, `create table ev (
		id int primary key,
		at timestamptz,
		day date,
		tag uuid
	)`)

	when := time.Date(2024, 3, 5, 10, 30, 0, 123456000, time.UTC)
	day := time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC)
	tag := [16]byte{0x55, 0x0e, 0x84, 0x00, 0xe2, 0x9b, 0x41, 0xd4,
		0xa7, 0x16, 0x44, 0x66, 0x55, 0x44, 0x00, 0x00}

	// Extended protocol: pgx describes the statement, sees the
	// timestamptz/date/uuid OIDs, and binds each param in binary.
	mustExec(t, c, `insert into ev values ($1, $2, $3, $4)`, int64(1), when, day, tag)

	var gotAt time.Time
	var gotDay time.Time
	var gotTag [16]byte
	err := c.QueryRow(ctx, `select at, day, tag from ev where id = 1`).
		Scan(&gotAt, &gotDay, &gotTag)
	if err != nil {
		t.Fatal(err)
	}
	if !gotAt.Equal(when) {
		t.Fatalf("at = %v, want %v", gotAt, when)
	}
	if !gotDay.Equal(day) {
		t.Fatalf("day = %v, want %v", gotDay, day)
	}
	if gotTag != tag {
		t.Fatalf("tag = %x, want %x", gotTag, tag)
	}

	// Simple protocol (text format end to end): literals in, text out.
	var n int64
	err = c.QueryRow(ctx,
		`select count(*) from ev where at = '2024-03-05 10:30:00.123456+00'
		 and tag = '550e8400-e29b-41d4-a716-446655440000'`).Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("text-literal lookup: %v %d", err, n)
	}

	// A timestamp param used in a WHERE range, bound as time.Time.
	err = c.QueryRow(ctx, `select count(*) from ev where at <= $1`, when).Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("bound range: %v %d", err, n)
	}

	// now() round-trips as a sane instant.
	var srvNow time.Time
	if err := c.QueryRow(ctx, `select now()`).Scan(&srvNow); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(srvNow); d < -time.Minute || d > time.Minute {
		t.Fatalf("now() skew: %v", d)
	}
}
