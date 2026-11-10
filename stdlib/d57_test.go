package stdlib

// Deep-look D-57: []string and [16]byte arguments, the DSN's engine
// settings and :memory:, encryption_key_file (and redaction), and
// symlinked paths sharing one engine.

import (
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/serr"
)

// errFields renders an error's serr fields, where a leaked value would hide.
func errFields(err error) string {
	if err == nil {
		return ""
	}
	return strings.Join(serr.SErrFromErr(err).Fields(), " ")
}

type uuidT [16]byte

func TestArraysAndUUIDArgs(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	exec(t, db, `CREATE TABLE t (id uuid PRIMARY KEY, tags text[])`)
	id := uuidT{0xa0, 0xee, 0xbc, 0x99}
	if _, err := db.Exec(`INSERT INTO t VALUES ($1, $2)`, id, []string{"a", "b c"}); err != nil {
		t.Fatal(err)
	}
	var tags, uid string
	if err := db.QueryRow(`SELECT tags, id FROM t WHERE id = $1`, [16]byte(id)).Scan(&tags, &uid); err != nil {
		t.Fatal(err)
	}
	if tags != `{a,"b c"}` || !strings.HasPrefix(uid, "a0eebc99-") {
		t.Errorf("tags %q uid %q", tags, uid)
	}
}

func TestDSNEngineSettings(t *testing.T) {
	path := tempDSN(t, "")
	db := open(t, path+"?max_result_rows=2&max_txn_writes=100&memory_ceiling=off&auto_compact=false")
	exec(t, db, `CREATE TABLE t (id int PRIMARY KEY)`, `INSERT INTO t VALUES (1), (2), (3)`)
	if _, err := db.Query(`SELECT * FROM t`); err == nil || !strings.Contains(err.Error(), "result row limit") {
		t.Errorf("max_result_rows=2 did not cap a 3-row result: %v", err)
	}
	enginesMu.Lock()
	sh := engines[mustResolve(t, path)]
	enginesMu.Unlock()
	if sh == nil {
		t.Fatal("engine not registered")
	}
	if got := sh.e.MemoryCeiling(); got != -1 {
		t.Errorf("memory ceiling = %d, want -1 (off)", got)
	}
	// Another *sql.DB on the path must ask for the same settings
	// (sql.Open runs OpenConnector, so the refusal comes from it).
	if db2, err := sql.Open(DriverName, path+"?max_result_rows=5"); err == nil || !strings.Contains(err.Error(), "different options") {
		if db2 != nil {
			db2.Close()
		}
		t.Errorf("mismatched settings on a shared engine: %v", err)
	}
	for _, bad := range []string{"?max_result_rows=-1", "?memory_ceiling=lots", "?auto_compact=maybe", "?compact_growth_pct=x"} {
		if _, err := ParseDSN("x.db" + bad); err == nil {
			t.Errorf("ParseDSN accepted %s", bad)
		}
	}
	cfg, err := ParseDSN("x.db?compact_min_bytes=1048576&compact_growth_pct=50")
	if err != nil || len(cfg.Options) != 1 {
		t.Errorf("compaction tuning: %v %v", cfg, err)
	}
}

func mustResolve(t *testing.T, p string) string {
	t.Helper()
	r, err := resolvePath(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestEncryptionKeyFile(t *testing.T) {
	dir := t.TempDir()
	keyHex := strings.Repeat("ab", 32)
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, []byte(keyHex+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "enc.bytdb")
	db := open(t, path+"?encryption_key_file="+keyFile)
	exec(t, db, `CREATE TABLE t (id int PRIMARY KEY, s text)`, `INSERT INTO t VALUES (1, 'secret')`)
	db.Close()
	// The inline key opens it too: same key, same signature.
	db = open(t, path+"?encryption_key="+keyHex)
	var s string
	if err := db.QueryRow(`SELECT s FROM t`).Scan(&s); err != nil || s != "secret" {
		t.Fatalf("reopen with the inline key: %q %v", s, err)
	}
	db.Close()
	if _, err := ParseDSN(path + "?encryption_key=" + keyHex + "&encryption_key_file=" + keyFile); err == nil {
		t.Error("both key forms accepted")
	}
	if _, err := ParseDSN(path + "?encryption_key_file=" + filepath.Join(dir, "missing")); err == nil {
		t.Error("missing key file accepted")
	}
	// A key never appears in an error, even in a malformed DSN.
	secret := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	_, err := ParseDSN("?encryption_key=" + secret)
	if err == nil || strings.Contains(err.Error()+errFields(err), secret) {
		t.Errorf("key leaked into %v %s", err, errFields(err))
	}
	if got := redactDSN("p?a=1&encryption_key=" + secret + "&b=2"); got != "p?a=1&encryption_key=REDACTED&b=2" {
		t.Errorf("redactDSN = %s", got)
	}
}

func TestMemoryDSN(t *testing.T) {
	a := open(t, ":memory:")
	b := open(t, ":memory:")
	exec(t, a, `CREATE TABLE t (id int PRIMARY KEY)`, `INSERT INTO t VALUES (1)`)
	// Each :memory: open is its own database.
	if _, err := b.Exec(`SELECT * FROM t`); err == nil {
		t.Error("two :memory: databases share a table")
	}
	sc, err := a.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var dir string
	if err := sc.Raw(func(dc any) error {
		dir = dc.(*conn).sh.tempDir
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sc.Close()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("temp dir missing while open: %v", err)
	}
	a.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("temp dir %s survived Close: %v", dir, err)
	}
}

func TestSymlinkSharesEngine(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.bytdb")
	db := open(t, real)
	exec(t, db, `CREATE TABLE t (id int PRIMARY KEY)`)
	link := filepath.Join(dir, "link.bytdb")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	db2 := open(t, link) // used to fail on the file lock (ErrLocked)
	exec(t, db2, `INSERT INTO t VALUES (1)`)
	if refs(t, real) != 2 {
		t.Errorf("refs = %d, want 2 (one engine)", refs(t, real))
	}
}
