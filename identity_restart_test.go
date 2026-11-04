package bytdb

// identity_restart_test.go: RestartIdentity and PeekIdentity (N-032),
// the engine half of ALTER TABLE ... ALTER COLUMN ... RESTART. Every
// case runs in both write modes, because the two keep counters
// differently: serialized mode in the transaction's kv writes,
// concurrent-writes mode in engine-level allocators that cache ahead of
// the stored record and must be discarded when a restart commits.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestartIdentity(t *testing.T) {
	modes := []struct {
		name string
		open func(t *testing.T, path string) *Engine
	}{
		{"serialized", openEngine},
		{"concurrent", openOCCEngine},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.db")
			e := m.open(t, path)
			idTable(t, e)

			peek := func(want uint64, wantOK bool) {
				t.Helper()
				got, ok, err := e.PeekIdentity("t", "id")
				if err != nil || got != want || ok != wantOK {
					t.Fatalf("PeekIdentity = %d, %v, %v; want %d, %v", got, ok, err, want, wantOK)
				}
			}
			insertGets := func(want int64) {
				t.Helper()
				var got int64
				err := e.WriteTxn(func(tx *Txn) error {
					row, err := tx.InsertReturning("t", nil, "x")
					if err != nil {
						return err
					}
					got = row.Vals[0].(int64)
					return nil
				})
				if err != nil || got != want {
					t.Fatalf("insert drew %d (err %v), want %d", got, err, want)
				}
			}

			// Nothing drawn yet: no counter, and the first draw is 1.
			peek(0, false)
			insertGets(1)
			insertGets(2)
			insertGets(3)
			// In concurrent mode an allocator is now loaded and caches
			// ahead of the stored watermark; peek must report the next
			// draw, not the watermark.
			peek(4, true)

			// A committed restart moves the next draw, in concurrent
			// mode only once the commit has dropped the cached range.
			if err := e.RestartIdentity("t", "id", 100); err != nil {
				t.Fatal(err)
			}
			peek(100, true)
			insertGets(100)
			peek(101, true)

			// A rolled-back restart leaves the counter alone.
			boom := errors.New("roll back")
			err := e.WriteTxn(func(tx *Txn) error {
				if err := tx.RestartIdentity("t", "id", 7); err != nil {
					return err
				}
				// Serialized mode reads its own write; concurrent mode
				// reports the engine's allocator, untouched until commit.
				next, _, err := tx.PeekIdentity("t", "id")
				if err != nil {
					return err
				}
				if want := map[bool]uint64{false: 7, true: 101}[e.occ]; next != want {
					t.Errorf("in-transaction peek = %d, want %d", next, want)
				}
				return boom
			})
			if !errors.Is(err, boom) {
				t.Fatalf("rolled-back restart: %v", err)
			}
			insertGets(101)

			// Restarting below existing values is allowed (the caller's
			// contract), and the draw then collides with the stored row.
			if err := e.RestartIdentity("t", "id", 2); err != nil {
				t.Fatal(err)
			}
			if err := e.Insert("t", nil, "dup"); err == nil {
				t.Fatal("draw of an existing id: want a uniqueness error")
			}

			// Refusals, with Postgres's wording.
			for _, c := range []struct {
				table, col string
				next       int64
				want       string
			}{
				{"t", "id", 0, "RESTART value (0) cannot be less than MINVALUE (1)"},
				{"t", "id", -5, "RESTART value (-5) cannot be less than MINVALUE (1)"},
				{"t", "name", 10, `column "name" of relation "t" is not an identity column`},
				{"t", "nope", 10, `column "nope" of relation "t" does not exist`},
				{"missing", "id", 10, "no such table"},
			} {
				err := e.RestartIdentity(c.table, c.col, c.next)
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Errorf("RestartIdentity(%s, %s, %d) = %v; want %q", c.table, c.col, c.next, err, c.want)
				}
			}
			if _, _, err := e.PeekIdentity("t", "name"); err == nil ||
				!strings.Contains(err.Error(), "is not an identity column") {
				t.Errorf("PeekIdentity on a plain column: %v", err)
			}

			// The restart is durable: it survives a reopen.
			if err := e.RestartIdentity("t", "id", 500); err != nil {
				t.Fatal(err)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			e = m.open(t, path)
			defer e.Close()
			peek(500, true)
			insertGets(500)
		})
	}
}
