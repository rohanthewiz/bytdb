package pgwire_test

// upsert_test.go: INSERT ... ON CONFLICT over the wire — the shape
// GORM (clause.OnConflict), SQLAlchemy (on_conflict_do_update), and
// hand-written app code all send, parameterized over the extended
// protocol and composed with RETURNING.

import (
	"context"
	"testing"
)

func TestUpsertOverWire(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create table settings (key text primary key, value text, version int)`)

	// GORM's UpdateAll shape: every non-key column from excluded.
	q := `insert into settings (key, value, version) values ($1, $2, 1)
		on conflict (key) do update set value = excluded.value, version = settings.version + 1
		returning version`
	var version int64
	if err := c.QueryRow(ctx, q, "theme", "dark").Scan(&version); err != nil || version != 1 {
		t.Fatalf("insert path: %v version %d", err, version)
	}
	if err := c.QueryRow(ctx, q, "theme", "light").Scan(&version); err != nil || version != 2 {
		t.Fatalf("update path: %v version %d", err, version)
	}
	var value string
	if err := c.QueryRow(ctx, `select value from settings where key = 'theme'`).Scan(&value); err != nil || value != "light" {
		t.Fatalf("stored value: %v %q", err, value)
	}

	// DO NOTHING reports the true insert count in the tag.
	tag := mustExec(t, c, `insert into settings values ('theme', 'x', 0), ('lang', 'en', 1) on conflict (key) do nothing`)
	if tag.String() != "INSERT 0 1" {
		t.Fatalf("do-nothing tag: %q", tag.String())
	}
}
