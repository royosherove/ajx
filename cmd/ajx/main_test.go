package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ptyProbe runs a command on a pseudo-terminal that never answers terminal
// queries (as many agent tool shells and IDE consoles behave) and prints the
// elapsed seconds and everything the command wrote.
const ptyProbe = `
import os, pty, select, sys, time
t0 = time.time()
pid, fd = pty.fork()
if pid == 0:
    os.execv(sys.argv[1], sys.argv[1:])
out = b""
while True:
    r, _, _ = select.select([fd], [], [], 0.05)
    if r:
        try:
            data = os.read(fd, 65536)
        except OSError:
            break
        if not data:
            break
        out += data
    if os.waitpid(pid, os.WNOHANG)[0]:
        break
sys.stdout.write("%.2f\n" % (time.time() - t0))
sys.stdout.write(out.decode("utf-8", "replace"))
`

// A terminal-styling dependency once queried the terminal's background color
// in a package init, so every command run on a terminal that does not reply
// stalled for 5 s and printed escape sequences. Commands must start at once
// and write only their own output.
func TestStartupSendsNoTerminalQueries(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable for the pty probe")
	}
	bin := filepath.Join(t.TempDir(), "ajx")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOFLAGS=-mod=vendor", "GOPROXY=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(py, "-I", "-c", ptyProbe, bin, "version")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe: %v\n%s", err, out)
	}
	elapsed, text, _ := strings.Cut(string(out), "\n")
	if strings.Contains(text, "\x1b") {
		t.Fatalf("ajx version wrote escape sequences on a terminal: %q", text)
	}
	if !strings.Contains(text, "AJX ") {
		t.Fatalf("unexpected output: %q", text)
	}
	// The query always writes ESC]11;? before it waits, so the escape check
	// above is the reliable signal; wall time on a freshly built binary also
	// includes the OS's first-launch checks, so it is only logged.
	if secs, err := strconv.ParseFloat(strings.TrimSpace(elapsed), 64); err == nil {
		t.Logf("ajx version took %.2fs on a pty", secs)
	}
}
