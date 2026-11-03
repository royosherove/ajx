// Package paritytest holds helpers for tests that compare the Go renderers
// with the Python implementation in skills/ajx (run offline with python3 -I;
// skipped when python3 is unavailable). Only synthetic fixtures are used.
package paritytest

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/render/pyfmt"
	"github.com/royosherove/ajx/internal/util"
)

// Root is the repository root.
func Root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

// Python returns the python3 path, or skips the test.
func Python(t *testing.T) string {
	t.Helper()
	if os.Getenv("AJX_SKIP_PYTHON_PARITY") != "" {
		t.Skip("AJX_SKIP_PYTHON_PARITY is set")
	}
	p, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	if _, err := os.Stat(filepath.Join(Root(), "skills", "ajx", "lib", "ajx", "render.py")); err != nil {
		t.Skip("Python implementation not available")
	}
	return p
}

// Run runs python3 -I with args from dir and fails the test on error.
func Run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(Python(t), append([]string{"-I"}, args...)...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("python %v: %v\n%s", args, err, errb.String())
	}
	return out.String()
}

// CopyTree copies a directory tree.
func CopyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

var (
	generated = regexp.MustCompile(`Generated \d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z`)
	revision  = regexp.MustCompile(`<dt>Skill revision</dt><dd>sha256:[0-9a-f]{16}</dd>`)
)

// Normalizations applied before comparing text files. Each one removes a
// value that legitimately differs between the two implementations:
//   - "Generated <now>" in page footers: the render time.
//   - the skill revision: Python hashes its own source files, Go its embedded resources.
func Normalize(text string) string {
	text = generated.ReplaceAllString(text, "Generated <now>")
	return revision.ReplaceAllString(text, "<dt>Skill revision</dt><dd>sha256:<fingerprint></dd>")
}

// volatileKeys are JSON keys whose values differ by design (time, implementation hash).
var volatileKeys = map[string]bool{"generated_at": true, "ajx_skill_revision": true}

func scrub(v any) any {
	switch t := v.(type) {
	case util.M:
		for k := range t {
			if volatileKeys[k] {
				t[k] = "<volatile>"
			} else {
				t[k] = scrub(t[k])
			}
		}
	case []any:
		for i := range t {
			t[i] = scrub(t[i])
		}
	}
	return v
}

// CanonicalJSON parses JSON (or JSON lines) with exact numbers and re-encodes
// it with sorted keys, so only key order (and the volatile keys) is ignored.
func CanonicalJSON(t *testing.T, name string, data []byte) string {
	t.Helper()
	if strings.HasSuffix(name, ".jsonl") {
		var lines []string
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			lines = append(lines, CanonicalJSON(t, "line.json", []byte(line)))
		}
		return strings.Join(lines, "\n")
	}
	v, err := pyfmt.Decode(data)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return pyfmt.Dumps(scrub(v), pyfmt.DumpOpts{Indent: 2})
}

// CompareTrees reports every file that differs between the Python and Go
// outputs: text files byte for byte after Normalize, JSON files canonically.
// It also checks that both trees hold the same files.
func CompareTrees(t *testing.T, py, goDir string) {
	t.Helper()
	list := func(root string) []string {
		var out []string
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				rel, _ := filepath.Rel(root, p)
				out = append(out, rel)
			}
			return nil
		})
		sort.Strings(out)
		return out
	}
	pyFiles, goFiles := list(py), list(goDir)
	if strings.Join(pyFiles, "\n") != strings.Join(goFiles, "\n") {
		t.Errorf("file sets differ:\npython: %v\ngo:     %v", pyFiles, goFiles)
	}
	for _, rel := range pyFiles {
		a, errA := os.ReadFile(filepath.Join(py, rel))
		b, errB := os.ReadFile(filepath.Join(goDir, rel))
		if errA != nil || errB != nil {
			continue
		}
		// Absolute paths name each tree's own copy of the fixture.
		a = bytes.ReplaceAll(a, []byte(py), []byte("<root>"))
		b = bytes.ReplaceAll(b, []byte(goDir), []byte("<root>"))
		if strings.HasSuffix(rel, ".json") || strings.HasSuffix(rel, ".jsonl") {
			ca, cb := CanonicalJSON(t, rel, a), CanonicalJSON(t, rel, b)
			if ca != cb {
				t.Errorf("%s differs (canonical JSON):\n%s", rel, FirstDiff(ca, cb))
			}
			continue
		}
		na, nb := Normalize(string(a)), Normalize(string(b))
		if na != nb {
			t.Errorf("%s differs:\n%s", rel, FirstDiff(na, nb))
		}
	}
}

// FirstDiff shows the first differing region of two texts.
func FirstDiff(a, b string) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	lo := i - 120
	if lo < 0 {
		lo = 0
	}
	clip := func(s string) string {
		hi := i + 200
		if hi > len(s) {
			hi = len(s)
		}
		if lo > len(s) {
			return ""
		}
		return s[lo:hi]
	}
	return "python: …" + clip(a) + "…\ngo:     …" + clip(b) + "…"
}
