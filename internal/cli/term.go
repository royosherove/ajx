package cli

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

type winsize struct {
	Row, Col, X, Y uint16
}

func ttySize(fd uintptr) (int, bool) {
	var ws winsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return 0, false
	}
	return int(ws.Col), true
}

// IsTerminal reports whether f is a terminal.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	_, ok := ttySize(f.Fd())
	return ok
}

// termColumns is shutil.get_terminal_size().columns: $COLUMNS, the terminal
// on stdout, else 80.
func termColumns() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("COLUMNS"))); err == nil && n > 0 {
		return n
	}
	if n, ok := ttySize(os.Stdout.Fd()); ok && n > 0 {
		return n
	}
	return 80
}

// ColorEnabled reports whether ANSI styling may be written to f: a terminal,
// NO_COLOR unset, TERM not "dumb".
func ColorEnabled(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return IsTerminal(f)
}

// Interactive reports whether the wizard may prompt: stdin and stdout are
// terminals and CI is unset.
func Interactive() bool {
	if os.Getenv("CI") != "" {
		return false
	}
	return IsTerminal(os.Stdin) && IsTerminal(os.Stdout)
}
