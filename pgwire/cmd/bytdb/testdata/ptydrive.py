# ptydrive.py: drive the interactive bytdb shell through a real pty.
#
# The Go tests cover the shell through run() with plain readers and
# writers. They cannot cover what only a terminal shows: liner's raw mode,
# the cooked/raw handoff around .shell and .edit, a Ctrl-C reaching a
# running statement as SIGINT (ISIG on between prompts), and the terminal
# being left as it was found. This script types at the shell key by key
# and checks each of those (N-033).
#
# It expects the binary and a scratch directory mounted at /w and needs
# python3, bash and stty, so it runs in a stock Linux container:
#
#   GOOS=linux GOARCH=arm64 GOWORK=off go build -o $D/bytdb ./cmd/bytdb   # from pgwire/
#   cp cmd/bytdb/testdata/ptydrive.py $D/
#   docker run --rm -v $D:/w golang:1.26.1 python3 -I /w/ptydrive.py
#
# For amd64: GOARCH=amd64 and docker run --platform linux/amd64 with any
# image that has python3 (python:3.13-slim works).
#
# Design notes:
#   - A fresh prompt is matched only after a newline. liner redraws the
#     line as ESC[1G + prompt + text on every key, and those redraws must
#     not count as "the statement finished".
#   - The wrapper is bash with job control on (set -m). Without it the
#     shell, bytdb and the wrapper share one process group, the Ctrl-C
#     sent mid-query also signals the wrapper, and dash then exits before
#     it can record the after-exit terminal mode.
#   - The .edit editor is a script that reads a word from the terminal.
#     A canonical-mode read only completes when the shell really handed
#     the terminal over cooked, so it checks the handoff and does not just
#     look at stty output.
import os, pty, sys, time, select, termios, struct, fcntl, re

W = "/w"
results = []

def check(name, ok, detail=""):
    results.append((name, ok))
    print(("PASS " if ok else "FAIL ") + name + ("" if ok else "  -- " + detail[-600:]), flush=True)

def spawn(argv, cols=80, rows=24, env=None):
    pid, fd = pty.fork()
    if pid == 0:
        # set the window size on the slave before exec so the child
        # sees it from its first TIOCGWINSZ.
        fcntl.ioctl(0, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
        os.execvpe(argv[0], argv, env)
    return pid, fd

class Term:
    def __init__(self, fd):
        self.fd, self.buf = fd, b""
    def read_until(self, pat, timeout=10):
        rx = re.compile(pat.encode() if isinstance(pat, str) else pat)
        end = time.time() + timeout
        while True:
            m = rx.search(self.buf)
            if m:
                out = self.buf[: m.end()]
                self.buf = self.buf[m.end():]
                return out.decode(errors="replace"), True
            left = end - time.time()
            if left <= 0:
                out = self.buf
                self.buf = b""
                return out.decode(errors="replace"), False
            r, _, _ = select.select([self.fd], [], [], left)
            if r:
                try:
                    chunk = os.read(self.fd, 65536)
                except OSError:
                    chunk = b""
                if not chunk:
                    out = self.buf
                    self.buf = b""
                    return out.decode(errors="replace"), bool(rx.search(out))
                self.buf += chunk
    def send(self, s, delay=0.05):
        for key in re.findall(r"\x1b\[[0-9;]*[A-Za-z~]|.", s, re.S):  # key by key, like a person
            os.write(self.fd, key.encode())
            time.sleep(0.01)
        time.sleep(delay)

env = dict(os.environ, HOME=W + "/home", TERM="xterm", SHELL="/bin/sh",
           EDITOR=W + "/editor.sh")
env.pop("VISUAL", None)
os.makedirs(W + "/home", exist_ok=True)
for f in ("t.db", "home/.bytdb_history", "before", "after", "after_a", "child_stty", "editor_stty"):
    try:
        os.remove(W + "/" + f)
    except FileNotFoundError:
        pass

# The editor: records the mode it was handed, reads one line from the
# terminal (only works if the terminal is cooked: canonical + echo), and
# writes it back as a statement.
with open(W + "/editor.sh", "w") as f:
    f.write('#!/bin/sh\nstty -a > /w/editor_stty\nprintf "word? "\nread w\nprintf "SELECT \'%s\' AS edited;\\n" "$w" > "$1"\n')
os.chmod(W + "/editor.sh", 0o755)

wrapper = ("stty -g > /w/before; /w/bytdb /w/t.db; echo EXIT=$?; "
           "stty -g > /w/after; stty -a > /w/after_a; echo WRAPPER_DONE")
# set -m gives bytdb its own foreground process group, as an interactive
# shell would, so a Ctrl-C mid-query signals bytdb and not the wrapper.
pid, fd = spawn(["/bin/bash", "-c", "set -m; " + wrapper], cols=80, env=env)
t = Term(fd)
P = r"(?:\A|\n)bytdb[*!]?> "
PC = r"\n  \.\.\.> "

out, ok = t.read_until(P)
check("startup prompt", ok, out)

# 1. prompt editing: type "SELECT 4", Left, "1", Ctrl-E, "2", a stray "x"
#    then Backspace, ";" -> SELECT 142;
t.send("SELECT 4\x1b[D1\x052x\x7f;\r")
out, ok = t.read_until(P)
check("line editing (left, ctrl-e, backspace)", ok and "142" in out and "x" not in out.split("142")[-1][:3], out)

# 2. continuation prompt
t.send("SELECT\r")
out, ok = t.read_until(PC)
check("continuation prompt shown", ok, out)
t.send("7 AS seven;\r")
out, ok = t.read_until(P)
check("multi-line statement runs", ok and "seven" in out and "7" in out, out)

# 3. history: Up recalls the last statement
t.send("\x1b[A")
out, ok = t.read_until(r"seven;")
check("history Up recalls statement", ok, out)
t.send("\r")
out, ok = t.read_until(P)
check("recalled statement re-runs", ok and "seven" in out, out)

# 4. Ctrl-C at the prompt discards the line, shell stays
t.send("SELECT 'never';\x1b[D\x1b[D\x03")
out, ok = t.read_until(P)
t.send("SELECT 'after' AS a;\r")
out2, ok2 = t.read_until(P)
check("Ctrl-C at prompt discards line", ok and ok2 and "never" not in out2 and "after" in out2, out + out2)

# 5. tab completion
t.send("CREATE TABLE widgets(id int primary key, name text);\r")
out, ok = t.read_until(P)
t.send("SELECT * FROM widg\t")
out, ok = t.read_until(r"widgets")
check("tab completion", ok, out)
t.send(";\r")
out, ok = t.read_until(P)
check("completed statement runs", ok and "id" in out, out)

# 6. Ctrl-C cancels a running query
t.send("CREATE TABLE n(i int primary key); INSERT INTO n VALUES " + ",".join("(%d)" % i for i in range(100)) + ";\r", delay=0)
out, ok = t.read_until(P)
check("setup 100-row table", ok and "Error" not in out, out)
q = "SELECT count(*) FROM n a, n b, n c, n d;"
t.send(q + "\r", delay=0)
time.sleep(1.5)
t0 = time.time()
os.write(fd, b"\x03")
out, ok = t.read_until(P, timeout=15)
dt = time.time() - t0
check("Ctrl-C cancels running query (%.2fs)" % dt, ok and dt < 5 and re.search(r"cancel", out, re.I) is not None, out)
t.send("SELECT 'alive' AS still;\r")
out, ok = t.read_until(P)
check("shell usable after cancel", ok and "alive" in out, out)

# 7. .shell runs a child in cooked mode, then editing still works
t.send(".shell stty -a > /w/child_stty; echo CHILD_RAN\r")
out, ok = t.read_until(P)
child = open(W + "/child_stty").read() if os.path.exists(W + "/child_stty") else ""
cooked = re.search(r"(?<!-)icanon", child) and re.search(r"(?<![-\w])echo\b", child)
check(".shell child ran", ok and "CHILD_RAN" in out, out)
check(".shell child got cooked mode", bool(cooked), child)
t.send("SELECT 4\x1b[D3\x05;\r")
out, ok = t.read_until(P)
check("editing after .shell", ok and "34" in out, out)

# 8. .edit: editor reads a word from the terminal in cooked mode
t.send(".edit\r")
out, ok = t.read_until(r"word\? ")
check(".edit editor started", ok, out)
t.send("bananas\r")
out, ok = t.read_until(P)
ed = open(W + "/editor_stty").read() if os.path.exists(W + "/editor_stty") else ""
check(".edit editor got cooked mode", bool(re.search(r"(?<!-)icanon", ed)), ed)
check(".edit ran the edited statement", ok and "bananas" in out and "edited" in out, out)
t.send("SELECT 5\x1b[D6\x05;\r")
out, ok = t.read_until(P)
check("editing after .edit", ok and "65" in out, out)

# 9. exit with Ctrl-D, terminal restored
t.send("\x04")
out, ok = t.read_until(r"WRAPPER_DONE", timeout=30)
check("Ctrl-D exits 0", ok and "EXIT=0" in out, out)
os.waitpid(pid, 0)
before = open(W + "/before").read().strip()
after = open(W + "/after").read().strip()
check("terminal mode restored after exit (stty -g equal)", before == after, before + " vs " + after)
hist = open(W + "/home/.bytdb_history").read() if os.path.exists(W + "/home/.bytdb_history") else ""
check("history file written", "widgets" in hist and "seven" in hist, hist)

# 10. width fitting at 40 columns
pid, fd = spawn(["/w/bytdb", "/w/t.db"], cols=40, env=env)
t = Term(fd)
t.read_until(P)
t.send("SELECT 'aaaaaaaaaaaaaaaaaaaaaaaaa' AS c1, 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbb' AS c2, 'ccccccccccccccccccccccccc' AS c3;\r")
out, ok = t.read_until(P)
# strip escape sequences and the echoed input line
plain = re.sub(r"\x1b\[[0-9;?]*[A-Za-z]", "", out)
rows = [l for l in plain.replace("\r", "").split("\n") if l and ("│" in l or "┌" in l or "└" in l)]
widest = max((len(l) for l in rows), default=0)
check("box output fits 40 columns (widest %d)" % widest, ok and rows and widest <= 40, plain)
t.send(".quit\r")
os.waitpid(pid, 0)

print("\n%d/%d passed" % (sum(1 for _, ok in results if ok), len(results)))
sys.exit(0 if all(ok for _, ok in results) else 1)
