package bytdb

// catalog_test.go: catalog robustness at Open and CreateTable — a
// corrupt table-id sequence must fail loudly rather than re-issue an
// ID an existing table owns, and bad or too-new descriptors must fail
// the open with every offender named, never silently hide a table.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/btypedb"
)

// setRaw writes a raw kv pair through the engine's store, bypassing
// the relational layer — the only way to plant corruption.
func setRaw(t *testing.T, e *Engine, key string, val []byte) {
	t.Helper()
	err := e.kv.Update(func(tx *btypedb.Tx[string, []byte]) error {
		return tx.Set(key, val)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCreateTableCorruptSeqKey(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	if _, err := e.CreateTable("a", []Column{{Name: "id", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	setRaw(t, e, seqKey(), []byte{1, 2, 3}) // not 8 bytes

	_, err := e.CreateTable("b", []Column{{Name: "id", Type: TInt}}, "id")
	if err == nil || !strings.Contains(err.Error(), "sequence") {
		t.Fatalf("create over a corrupt sequence key: got %v, want a sequence-corruption error", err)
	}
	// The failed create must not have landed anywhere.
	if e.Table("b") != nil {
		t.Fatal("table b exists after a failed create")
	}
}

func TestOpenReportsBadDescriptors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	e := openEngine(t, path)
	if _, err := e.CreateTable("good", []Column{{Name: "id", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	// Two independent kinds of damage, planted side by side: Open must
	// name both in one error, not brick on the first alone.
	setRaw(t, e, descKey("mangled"), []byte("{not json"))
	setRaw(t, e, descKey("future"), []byte(
		`{"format_version": 99, "id": 900, "name": "future", "columns": [{"name":"id","type":"int","id":1}], "pk_cols": [0], "next_col_id": 2}`))
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := Open(path)
	if err == nil {
		t.Fatal("open succeeded over corrupt descriptors")
	}
	msg := err.Error()
	for _, want := range []string{"mangled", "future", "format version 99"} {
		if !strings.Contains(msg, want) {
			t.Errorf("open error %q does not mention %q", msg, want)
		}
	}
}

// TestDescriptorFormatVersionRoundTrip pins that every write path
// stamps the current version and that a stamped database reopens.
func TestDescriptorFormatVersionRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	e := openEngine(t, path)
	if _, err := e.CreateTable("t", []Column{
		{Name: "id", Type: TInt}, {Name: "v", Type: TInt},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateIndex("t", "v_idx", false, "v"); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e = openEngine(t, path)
	defer e.Close()
	d := e.Table("t")
	if d == nil {
		t.Fatal("table missing after reopen")
	}
	if d.FormatVersion != descFormatVersion {
		t.Fatalf("descriptor format version = %d, want %d", d.FormatVersion, descFormatVersion)
	}
	if d.Index("v_idx") == nil {
		t.Fatal("index missing after reopen")
	}
}
