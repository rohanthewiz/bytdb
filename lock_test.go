package bytdb

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/serr"
)

// TestLockSameProcess: a second engine on an open path is refused with
// ErrLocked (naming this process as holder), and Close frees the path —
// including after a compaction has renamed a fresh file over it, the
// case that rules out locking the database file itself.
func TestLockSameProcess(t *testing.T) {
	if !lockingSupported() {
		t.Skip("no file locking on " + runtime.GOOS)
	}
	path := filepath.Join(t.TempDir(), "db.bytdb")
	e, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	usersTable(t, e)
	if err := e.Insert("users", 1, "a", 1.0, true, nil); err != nil {
		t.Fatal(err)
	}

	_, err = Open(path)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open: want ErrLocked, got %v", err)
	}
	if got := fieldOf(err, "holder_pid"); got != strconv.Itoa(os.Getpid()) {
		t.Errorf("holder_pid = %q, want %d", got, os.Getpid())
	}

	// Compaction swaps the database file's inode; the sidecar lock must
	// still exclude.
	if err := e.kv.Compact(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("Open after compaction: want ErrLocked, got %v", err)
	}

	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	e2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after Close: %v", err)
	}
	defer e2.Close()
	if n := len(collect(t, e2.Scan("users"))); n != 1 {
		t.Fatalf("reopened rows = %d, want 1", n)
	}
}

// TestLockSymlink: a symlink to an open database resolves to the same
// sidecar, so it cannot sneak a second engine onto the file.
func TestLockSymlink(t *testing.T) {
	if !lockingSupported() {
		t.Skip("no file locking on " + runtime.GOOS)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "db.bytdb")
	e, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	link := filepath.Join(dir, "alias.bytdb")
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Open(link); !errors.Is(err, ErrLocked) {
		t.Fatalf("Open via symlink: want ErrLocked, got %v", err)
	}
}

// TestLockReleasedOnFailedOpen: an Open that fails after taking the lock
// (here btypedb refuses a key for a plaintext file) must give it back,
// or the database would stay locked until the process exits.
func TestLockReleasedOnFailedOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.bytdb")
	e, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path, WithEncryptionKey(encKey())); !errors.Is(err, btypedb.ErrNotEncrypted) {
		t.Fatalf("keyed Open of plaintext db: want ErrNotEncrypted, got %v", err)
	}
	e, err = Open(path)
	if err != nil {
		t.Fatalf("Open after failed Open: %v", err)
	}
	e.Close()
}

// TestLockRawKVOpen: the lock is btypedb's, so a raw btypedb.Open of a
// file an engine holds — the gap when bytdb took the lock itself — is
// refused too, and the error matches both ErrLocked names.
func TestLockRawKVOpen(t *testing.T) {
	if !lockingSupported() {
		t.Skip("no file locking on " + runtime.GOOS)
	}
	path := filepath.Join(t.TempDir(), "db.bytdb")
	e, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	_, err = btypedb.Open(path, btypedb.StringCodec, btypedb.BytesCodec)
	if !errors.Is(err, ErrLocked) || !errors.Is(err, btypedb.ErrLocked) {
		t.Fatalf("raw btypedb.Open of a live engine's file: want ErrLocked, got %v", err)
	}
}

// TestLockBackupDestination: Engine.Backup refuses to land on a live
// database — another engine's file, or its own — and the refused
// destination keeps every row, including ones written after the attempt.
func TestLockBackupDestination(t *testing.T) {
	if !lockingSupported() {
		t.Skip("no file locking on " + runtime.GOOS)
	}
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.bytdb")
	src, err := Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	usersTable(t, src)

	destPath := filepath.Join(dir, "dest.bytdb")
	dest, err := Open(destPath)
	if err != nil {
		t.Fatal(err)
	}
	usersTable(t, dest)
	if err := dest.Insert("users", 1, "a", 1.0, true, nil); err != nil {
		t.Fatal(err)
	}

	if err := src.Backup(destPath); !errors.Is(err, ErrLocked) {
		t.Fatalf("Backup onto a live engine: want ErrLocked, got %v", err)
	}
	if err := src.Backup(srcPath); !errors.Is(err, ErrLocked) {
		t.Fatalf("Backup onto its own path: want ErrLocked, got %v", err)
	}

	if err := dest.Insert("users", 2, "b", 2.0, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := dest.Close(); err != nil {
		t.Fatal(err)
	}
	e, err := Open(destPath)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if n := len(collect(t, e.Scan("users"))); n != 2 {
		t.Fatalf("dest rows after refused Backup = %d, want 2", n)
	}
}

// lockHelperEnv switches TestLockHelperProcess from a no-op into the
// child side of the cross-process tests; its value is the database path.
const lockHelperEnv = "BYTDB_LOCK_HELPER_DB"

// TestLockHelperProcess is not a test on its own: the cross-process tests
// re-run this binary with lockHelperEnv set, and then it opens the
// database, reports the outcome on stdout, and holds the engine open
// until stdin closes (or it is killed).
func TestLockHelperProcess(t *testing.T) {
	path := os.Getenv(lockHelperEnv)
	if path == "" {
		t.Skip("helper for the cross-process lock tests")
	}
	e, err := Open(path)
	switch {
	case errors.Is(err, ErrLocked):
		os.Stdout.WriteString("locked " + fieldOf(err, "holder_pid") + "\n")
		os.Exit(0)
	case err != nil:
		os.Stdout.WriteString("error " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("opened\n")
	bufio.NewReader(os.Stdin).ReadString('\n') // hold until the parent lets go
	e.Close()
	os.Exit(0)
}

// startHelper launches the helper child on path and returns its first
// output line, the running command, and a writer whose Close releases it.
func startHelper(t *testing.T, path string) (string, *exec.Cmd, func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$")
	cmd.Env = append(os.Environ(), lockHelperEnv+"="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	return line, cmd, func() { stdin.Close(); cmd.Wait() }
}

// TestLockCrossProcess is the reported bug: a second process opening a
// live database must be refused, and must be told who holds it.
func TestLockCrossProcess(t *testing.T) {
	if !lockingSupported() {
		t.Skip("no file locking on " + runtime.GOOS)
	}
	path := filepath.Join(t.TempDir(), "db.bytdb")
	e, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	line, _, done := startHelper(t, path)
	done()
	if want := "locked " + strconv.Itoa(os.Getpid()) + "\n"; line != want {
		t.Fatalf("child: got %q, want %q", line, want)
	}
}

// TestLockReleasedOnKill: the lock is the OS's, not the sidecar's
// contents, so a holder killed without Close leaves nothing that blocks
// the next Open — no stale-lock cleanup is ever needed.
func TestLockReleasedOnKill(t *testing.T) {
	if !lockingSupported() {
		t.Skip("no file locking on " + runtime.GOOS)
	}
	path := filepath.Join(t.TempDir(), "db.bytdb")

	line, cmd, done := startHelper(t, path)
	if line != "opened\n" {
		done()
		t.Fatalf("child: got %q, want opened", line)
	}
	if _, err := Open(path); !errors.Is(err, ErrLocked) {
		done()
		t.Fatalf("Open while child holds it: want ErrLocked, got %v", err)
	}

	cmd.Process.Kill()
	done()

	e, err := Open(path)
	if err != nil {
		t.Fatalf("Open after holder was killed: %v", err)
	}
	e.Close()
}

func lockingSupported() bool {
	switch runtime.GOOS {
	case "darwin", "dragonfly", "freebsd", "linux", "netbsd", "openbsd", "windows":
		return true
	}
	return false
}

// fieldOf returns the value of a structured field on a serr error.
func fieldOf(err error, key string) string {
	flds := serr.SErrFromErr(err).UserFields()
	for i := 0; i+1 < len(flds); i += 2 {
		if flds[i] == key {
			return flds[i+1]
		}
	}
	return ""
}
