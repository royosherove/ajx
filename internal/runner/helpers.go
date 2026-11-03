package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/royosherove/ajx/internal/util"
)

// randHex is secrets.token_hex(5).
func randHex() string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// resolvePath is Path.resolve() (non-strict): absolute, symlinks of the
// existing prefix resolved.
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	dir, base := filepath.Split(abs)
	dir = filepath.Clean(dir)
	if dir == abs {
		return abs
	}
	return filepath.Join(resolvePath(dir), base)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// ---------------------------------------------------------------- shlex

var shSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote is shlex.quote.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if shSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// shellJoin is shlex.join.
func shellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

// ---------------------------------------------------------------- probes

// probe runs a version command: first output line (<=160 chars) or nil.
func probe(argv []string) any {
	if len(argv) == 0 || util.Which(argv[0]) == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return nil
	}
	text := stdout.String()
	if text == "" {
		text = stderr.String()
	}
	text = strings.TrimSpace(string(bytes.ToValidUTF8([]byte(text), []byte("�"))))
	if text == "" {
		return nil
	}
	return util.Clip(strings.SplitN(text, "\n", 2)[0], 160)
}

var (
	unameOnce sync.Once
	unameText string
)

// platformText is f"{platform.system()} {platform.release()} {platform.machine()}".
func platformText() string {
	unameOnce.Do(func() {
		parts := make([]string, 3)
		for i, flag := range []string{"-s", "-r", "-m"} {
			out, err := exec.Command("uname", flag).Output()
			if err == nil {
				parts[i] = strings.TrimSpace(string(out))
			}
		}
		if parts[0] == "" {
			parts[0] = strings.ToUpper(runtime.GOOS[:1]) + runtime.GOOS[1:]
		}
		if parts[2] == "" {
			parts[2] = runtime.GOARCH
		}
		unameText = parts[0] + " " + parts[1] + " " + parts[2]
	})
	return unameText
}

// firstLine is text.splitlines()[0][:n].
func firstLine(text string, n int) string {
	line := text
	if i := strings.IndexAny(text, "\r\n"); i >= 0 {
		line = text[:i]
	}
	return util.Clip(line, n)
}

// ---------------------------------------------------------------- copying

// copyTree is shutil.copytree(src, dst, dirs_exist_ok=True): symlinks are
// followed (their targets copied), file modes and times preserved.
func copyTree(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return &fs.PathError{Op: "copytree", Path: src, Err: syscall.ENOTDIR}
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		info, err := os.Stat(s) // follow symlinks (copytree symlinks=False)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := copyTree(s, d); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(s, d, info); err != nil {
			return err
		}
	}
	_ = os.Chmod(dst, st.Mode().Perm())
	_ = os.Chtimes(dst, st.ModTime(), st.ModTime())
	return nil
}

// copyFile is shutil.copy2 (content, mode, times).
func copyFile(src, dst string, info os.FileInfo) error {
	if info == nil {
		var err error
		if info, err = os.Stat(src); err != nil {
			return err
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	_ = os.Chmod(dst, info.Mode().Perm())
	_ = os.Chtimes(dst, info.ModTime(), info.ModTime())
	return nil
}

// copyTreeLinks copies a tree keeping symlinks as symlinks (shutil.move's
// cross-device fallback, copytree(symlinks=True)).
func copyTreeLinks(src, dst string) error {
	st, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	}
	if !st.IsDir() {
		return copyFile(src, dst, st)
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyTreeLinks(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	_ = os.Chmod(dst, st.Mode().Perm())
	_ = os.Chtimes(dst, st.ModTime(), st.ModTime())
	return nil
}

// moveTree is shutil.move(src, dst) for a destination that does not exist.
func moveTree(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	var le *os.LinkError
	if !errors.As(err, &le) || !errors.Is(le.Err, syscall.EXDEV) {
		return err
	}
	if err := copyTreeLinks(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

// readText reads a UTF-8 text file.
func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// writeText writes a UTF-8 text file (Path.write_text).
func writeText(path, text string) error {
	return os.WriteFile(path, []byte(text), 0o644)
}

// anyList converts strings to a non-nil []any (JSON [] rather than null).
func anyList(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// mapsList converts []M to a non-nil []any.
func mapsList(items []M) []any {
	out := make([]any, len(items))
	for i, m := range items {
		out[i] = m
	}
	return out
}

// exitCode reads a result's exit_code: (code, true) or (0, false) for None.
func exitCode(res M) (int, bool) {
	v := res["exit_code"]
	if v == nil {
		return 0, false
	}
	f, ok := util.Num(v)
	return int(f), ok
}

// exitZero is res["exit_code"] == 0.
func exitZero(res M) bool {
	c, ok := exitCode(res)
	return ok && c == 0
}

// pick copies keys from res (missing keys become nil, like a KeyError-free subset).
func pick(res M, keys ...string) M {
	out := M{}
	for _, k := range keys {
		out[k] = res[k]
	}
	return out
}

// pyTuple formats a string tuple as Python's repr ("('prepare',)"; None for nil).
func pyTuple(items []string) string {
	if items == nil {
		return "None"
	}
	if len(items) == 1 {
		return "(" + pyStrRepr(items[0]) + ",)"
	}
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = pyStrRepr(s)
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// envMap reads an environment held as map[string]string or as an object.
func envMap(v any) map[string]string {
	if m, ok := v.(map[string]string); ok {
		return m
	}
	return util.StrMap(v)
}
