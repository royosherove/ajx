// Package parity runs the Go and Python CLIs side by side on synthetic
// inputs and reports every difference. It needs Python 3.11+ and is gated:
//
//	AJX_PARITY=1 go test ./internal/parity/ -run Parity -v
//
// AJX_PYTHON selects the interpreter (default: python3 on PATH). Workers are
// the simulated console-demo harness or /bin/sh scripts declared as
// [adapters.*]; no agent, model provider, network service or cloud account
// is used. HOME is a temporary directory and `aws` and `docker` are shadowed.
package parity

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	ajx "github.com/royosherove/ajx"
)

var (
	root    string
	goBin   string
	python  string
	fakeBin string
	home    string
)

func TestMain(m *testing.M) {
	if os.Getenv("AJX_PARITY") != "1" {
		fmt.Println("parity: skipped (set AJX_PARITY=1 to compare the Go and Python CLIs)")
		os.Exit(0)
	}
	_, file, _, _ := runtime.Caller(0)
	root = filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	tmp, err := os.MkdirTemp("", "ajx-parity-")
	if err != nil {
		panic(err)
	}
	goBin = filepath.Join(tmp, "ajx-go")
	build := exec.Command("go", "build", "-o", goBin, "./cmd/ajx")
	build.Dir = root
	build.Env = append(os.Environ(), "GOFLAGS=-mod=vendor", "GOPROXY=off")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Printf("parity: go build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	python = os.Getenv("AJX_PYTHON")
	if python == "" {
		python, _ = exec.LookPath("python3")
	}
	if out, err := exec.Command(python, "-c", "import tomllib").CombinedOutput(); err != nil {
		fmt.Printf("parity: %s cannot import tomllib (Python 3.11+ needed; set AJX_PYTHON): %s\n", python, out)
		os.Exit(1)
	}
	home = filepath.Join(tmp, "home")
	fakeBin = filepath.Join(tmp, "bin")
	os.MkdirAll(home, 0o755)
	os.MkdirAll(fakeBin, 0o755)
	for _, b := range []string{"claude", "codex", "kiro-cli", "jq"} {
		os.WriteFile(filepath.Join(fakeBin, b), []byte("#!/bin/sh\necho \""+b+" 0.0.0-fake\"\n"), 0o755)
	}
	os.WriteFile(filepath.Join(fakeBin, "aws"), []byte("#!/bin/sh\necho 'ajx tests: real aws blocked' >&2\nexit 97\n"), 0o755)
	// The console's container editor lists local images; neither CLI may ask a Docker daemon here.
	os.WriteFile(filepath.Join(fakeBin, "docker"), []byte("#!/bin/sh\necho 'ajx tests: docker blocked' >&2\nexit 97\n"), 0o755)
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

type result struct {
	code           int
	stdout, stderr string
}

func env(extra ...string) []string {
	e := []string{"HOME=" + home, "PATH=" + fakeBin + ":/usr/bin:/bin", "COLUMNS=80", "CI=1", "AJX_DEMO_PACE=0",
		"LANG=C.UTF-8", "TMPDIR=" + os.TempDir(), "SHELL=/bin/sh", "NO_COLOR=1"}
	return append(e, extra...)
}

func runCLI(t *testing.T, which, dir string, args ...string) result {
	t.Helper()
	var cmd *exec.Cmd
	if which == "py" {
		cmd = exec.Command(python, append([]string{"-I", filepath.Join(root, "skills", "ajx", "bin", "ajx")}, args...)...)
	} else {
		cmd = exec.Command(goBin, args...)
	}
	cmd.Dir = dir
	cmd.Env = env()
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%s %v: %v", which, args, err)
	}
	return result{code, out.String(), errb.String()}
}

var (
	clockRe = regexp.MustCompile(`\[\d\d:\d\d:\d\d\]`)
	isoRe   = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)?`)
	hhmmRe  = regexp.MustCompile(`\b\d\d:\d\d:\d\d\b`)
)

// norm replaces per-CLI directories and clock times.
func norm(s string, dirs map[string]string) string {
	keys := make([]string, 0, len(dirs))
	for k := range dirs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		s = strings.ReplaceAll(s, k, dirs[k])
	}
	s = clockRe.ReplaceAllString(s, "[T]")
	return s
}

// marks maps each directory to a placeholder: its resolved form to "<NAME>"
// and the unresolved form (macOS /var -> /private/var) to "<NAME~>", so a CLI
// that prints the other form shows up as a difference.
func marks(name string, paths ...string) map[string]string {
	out := map[string]string{}
	for _, p := range paths {
		out[p] = "<" + name + "~>"
		out[realpath(p)] = "<" + name + ">"
	}
	return out
}

// realpath resolves the longest existing prefix (Python's Path.resolve()).
func realpath(p string) string {
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// diff reports the first differing lines precisely.
func diff(t *testing.T, what, py, gov string) {
	t.Helper()
	if py == gov {
		return
	}
	a, b := strings.Split(py, "\n"), strings.Split(gov, "\n")
	var out []string
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			out = append(out, fmt.Sprintf("  line %d\n    python: %q\n    go:     %q", i+1, x, y))
			if len(out) >= 8 {
				out = append(out, "  ...")
				break
			}
		}
	}
	t.Errorf("%s differs:\n%s", what, strings.Join(out, "\n"))
}

func same(t *testing.T, what string, py, gov result, dirs map[string]string) {
	t.Helper()
	if py.code != gov.code {
		t.Errorf("%s: exit code python=%d go=%d\npython stderr: %s\ngo stderr: %s", what, py.code, gov.code, py.stderr, gov.stderr)
	}
	diff(t, what+" stdout", norm(py.stdout, dirs), norm(gov.stdout, dirs))
	diff(t, what+" stderr", norm(py.stderr, dirs), norm(gov.stderr, dirs))
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			switch info.Name() {
			case "ajx-reports", ".ajx-console", "__pycache__":
				return filepath.SkipDir
			}
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
	if err != nil {
		t.Fatal(err)
	}
}

func example(name string) string { return filepath.Join(root, "skills", "ajx", "examples", name) }

func TestParityPlugins(t *testing.T) {
	dir := t.TempDir()
	same(t, "plugins", runCLI(t, "py", dir, "plugins"), runCLI(t, "go", dir, "plugins"), nil)
}

func TestParityUsageErrors(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"bogus"}, {"run"}, {"run", "x", "y"}, {"run", "x", "--bogus"}, {"status", "x", "--tail", "abc"},
		{"console"}, {"console", "again", "t", "--quote", "q", "--plan", "p"},
		{"console", "again", "t", "--quote", "q", "--plan", "p", "--same-folder", "--fresh-folder"},
		{"console", "wait", "t", "--timeout", "1.5"}, {"run", "t", "--dry-run=1"}, {"console", "note", "t"},
		{"init"}, {"init", "x"}, {"export", "x"}, {"run", "x", "--cells"}, {"console", "stop", "--a", "--j"},
	} {
		py, gov := runCLI(t, "py", dir, args...), runCLI(t, "go", dir, args...)
		last := func(s string) string {
			lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
			return lines[len(lines)-1]
		}
		lp, lg := last(py.stderr), last(gov.stderr)
		if strings.Contains(lp, "invalid choice") {
			// the Go CLI adds commands: compare up to the choices list
			lp, lg = strings.SplitN(lp, " (choose", 2)[0], strings.SplitN(lg, " (choose", 2)[0]
		}
		if py.code != gov.code || lp != lg {
			t.Errorf("%v: python %d %q / go %d %q", args, py.code, lp, gov.code, lg)
		}
	}
	for _, args := range [][]string{{"--version"}, {"console", "-h"}, {"console", "stop", "-h"}, {"console", "again", "-h"},
		{"console", "open", "-h"}, {"console", "wait", "-h"}} {
		same(t, strings.Join(args, " "), runCLI(t, "py", dir, args...), runCLI(t, "go", dir, args...), nil)
	}
}

func TestParityBrandedHelp(t *testing.T) {
	dir := t.TempDir()
	prefix := ajx.Logo + "v" + ajx.Version + "\n\nusage: ajx"
	for _, implementation := range []string{"py", "go"} {
		var baseline string
		for _, args := range [][]string{nil, {"help"}, {"--help"}, {"-h"}} {
			result := runCLI(t, implementation, dir, args...)
			if result.code != 0 || result.stderr != "" || !strings.HasPrefix(result.stdout, prefix) {
				t.Fatalf("%s %v: code=%d stdout=%q stderr=%q",
					implementation, args, result.code, result.stdout, result.stderr)
			}
			if baseline != "" && result.stdout != baseline {
				t.Fatalf("%s %v: help differs from bare ajx", implementation, args)
			}
			baseline = result.stdout
		}
	}
}

func TestParityInit(t *testing.T) {
	for _, harness := range []string{"claude-code", "codex", "kiro-cli"} {
		base := t.TempDir()
		py, gov := filepath.Join(base, "py", "My Trial"), filepath.Join(base, "go", "My Trial")
		rp := runCLI(t, "py", base, "init", py, "--product", "example", "--harness", harness)
		rg := runCLI(t, "go", base, "init", gov, "--product", "example", "--harness", harness)
		// Native init adds headless guidance. Check that contract explicitly;
		// the written scaffold and all remaining output must still match.
		for _, command := range []string{"ajx validate '", "ajx doctor '", "ajx run '", "ajx ci '"} {
			if !strings.Contains(rg.stdout, command) {
				t.Fatalf("native headless guidance lacks %q: %s", command, rg.stdout)
			}
		}
		withoutNext := func(text string) string {
			lines := strings.Split(text, "\n")
			out := []string{}
			found := 0
			for _, line := range lines {
				if strings.HasPrefix(line, "Next: ") {
					found++
				} else {
					out = append(out, line)
				}
			}
			if found != 1 {
				t.Fatalf("expected one next-step explanation, got %d", found)
			}
			return strings.Join(out, "\n")
		}
		rp.stdout, rg.stdout = withoutNext(rp.stdout), withoutNext(rg.stdout)
		same(t, "init "+harness, rp, rg, marks("DIR", py, gov))
		for _, f := range []string{"trial.toml", "task-prompt.md"} {
			a, _ := os.ReadFile(filepath.Join(py, f))
			b, _ := os.ReadFile(filepath.Join(gov, f))
			diff(t, "init "+harness+" "+f, string(a), string(b))
		}
		for _, which := range []string{"py", "go"} {
			for _, d := range []string{py, gov} {
				if r := runCLI(t, which, base, "validate", filepath.Join(d, "trial.toml")); r.code != 0 {
					t.Errorf("%s validate %s: %d %s", which, d, r.code, r.stderr)
				}
			}
		}
	}
}

func TestParityInitTaskFileAndBudget(t *testing.T) {
	base := t.TempDir()
	task := []byte("  Keep café.\r\n\r\nAnd final newlines.\n\n")
	source := filepath.Join(base, "source.md")
	if err := os.WriteFile(source, task, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, which := range []string{"py", "go"} {
		dir := filepath.Join(base, which, "trial")
		r := runCLI(t, which, base, "init", dir, "--product", "demo", "--task-file", source,
			"--max-budget-usd", "2.5")
		if r.code != 0 {
			t.Fatalf("%s init: %d %s %s", which, r.code, r.stdout, r.stderr)
		}
		saved, err := os.ReadFile(filepath.Join(dir, "task-prompt.md"))
		if err != nil || string(saved) != string(task) {
			t.Fatalf("%s changed task file bytes: %q, %v", which, saved, err)
		}
		if !strings.Contains(r.stdout, fmt.Sprintf("task-prompt.md sha256: %x", sha256.Sum256(task))) {
			t.Fatalf("%s did not report the saved hash: %s", which, r.stdout)
		}
		var document struct {
			Trial struct {
				MaxBudgetUSD float64 `toml:"max_budget_usd"`
			}
		}
		if _, err := toml.DecodeFile(filepath.Join(dir, "trial.toml"), &document); err != nil ||
			document.Trial.MaxBudgetUSD != 2.5 {
			t.Fatalf("%s budget: %+v, %v", which, document, err)
		}
		for _, validator := range []string{"py", "go"} {
			r := runCLI(t, validator, base, "validate", filepath.Join(dir, "trial.toml"))
			if r.code != 0 {
				t.Fatalf("%s validate %s scaffold: %d %s", validator, which, r.code, r.stderr)
			}
		}
	}
}

func TestParityValidateAndDoctor(t *testing.T) {
	dir := t.TempDir()
	for _, trial := range []string{example("jq-smoke"), example(""), example("console-demo"), example("environment-profiles"), example("aws-cloud")} {
		path := filepath.Join(trial, "trial.toml")
		same(t, "validate "+path, runCLI(t, "py", dir, "validate", path), runCLI(t, "go", dir, "validate", path), nil)
		py, gov := runCLI(t, "py", dir, "doctor", path), runCLI(t, "go", dir, "doctor", path)
		banner := ajx.Logo + "v" + ajx.Version + "\n\n"
		if !strings.HasPrefix(py.stdout, banner) || !strings.HasPrefix(gov.stdout, banner) {
			t.Fatalf("doctor %s did not show the shared banner:\npython: %s\ngo: %s", path, py.stdout, gov.stdout)
		}
		py.stdout = strings.TrimPrefix(py.stdout, banner)
		gov.stdout = strings.TrimPrefix(gov.stdout, banner)
		// After the shared banner, the first line names the implementation.
		py.stdout = strings.SplitN(py.stdout, "\n", 2)[1]
		gov.stdout = strings.SplitN(gov.stdout, "\n", 2)[1]
		// Native readiness now rejects a failed identity command even when
		// its text contains no recognized credential error. The fake aws
		// deliberately exits 97; assert that new diagnostic before comparing
		// every other line with the Python implementation.
		if trial == example("") || trial == example("aws-cloud") {
			if gov.code == 0 || !strings.Contains(gov.stdout, "    FAILED model identity check: exit 97\n") {
				t.Fatalf("native doctor did not reject the blocked identity probe: %d %s", gov.code, gov.stdout)
			}
			gov.stdout = strings.ReplaceAll(gov.stdout, "    FAILED model identity check: exit 97\n", "")
			gov.stdout = strings.ReplaceAll(gov.stdout, "    PROBLEMS: ['model identity check failed: exit 97']\n", "")
		}
		same(t, "doctor "+path, py, gov, nil)
	}
}

func listFiles(t *testing.T, dir string) []string {
	var out []string
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func compareTrees(t *testing.T, what, a, b string, dirs map[string]string, contents func(rel string) bool) {
	t.Helper()
	fa, fb := listFiles(t, a), listFiles(t, b)
	diff(t, what+" file list", strings.Join(fa, "\n"), strings.Join(fb, "\n"))
	for _, rel := range fa {
		if contents != nil && !contents(rel) {
			continue
		}
		x, err1 := os.ReadFile(filepath.Join(a, rel))
		y, err2 := os.ReadFile(filepath.Join(b, rel))
		if err1 != nil || err2 != nil {
			continue
		}
		mx, my := isoRe.ReplaceAllString(norm(string(x), dirs), "<TS>"), isoRe.ReplaceAllString(norm(string(y), dirs), "<TS>")
		if strings.HasSuffix(rel, ".json") {
			diff(t, what+" "+rel, canonJSON(mx), canonJSON(my))
		} else {
			diff(t, what+" "+rel, mx, my)
		}
	}
}

// canonJSON re-encodes JSON with sorted keys (key order is the one intended difference).
func canonJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

func TestParityExample(t *testing.T) {
	base := t.TempDir()
	py, gov := filepath.Join(base, "py"), filepath.Join(base, "go")
	dirs := marks("OUT", py, gov)
	same(t, "example", runCLI(t, "py", base, "example", py), runCLI(t, "go", base, "example", gov), dirs)
	compareTrees(t, "example", py, gov, dirs, func(rel string) bool {
		return strings.HasSuffix(rel, ".md") || strings.HasSuffix(rel, ".json")
	})
}

func TestParityStatusAndExportOverTheExampleReport(t *testing.T) {
	base := t.TempDir()
	py, gov := filepath.Join(base, "py"), filepath.Join(base, "go")
	copyTree(t, filepath.Join(root, "docs", "example-report"), py)
	copyTree(t, filepath.Join(root, "docs", "example-report"), gov)
	dirs := marks("OUT", py, gov)
	for _, args := range [][]string{{"status"}, {"status", "--tail", "3"}, {"export", "--run", "nope"}} {
		same(t, strings.Join(args, " "), runCLI(t, "py", base, append([]string{args[0], py}, args[1:]...)...),
			runCLI(t, "go", base, append([]string{args[0], gov}, args[1:]...)...), dirs)
	}
	for _, id := range []string{"baseline-codex-r1", "headless-kiro-cli-r1"} {
		rp := runCLI(t, "py", base, "export", py, "--run", id)
		rg := runCLI(t, "go", base, "export", gov, "--run", id)
		same(t, "export "+id, rp, rg, dirs)
		compareTrees(t, "export "+id, filepath.Join(py, "share", id), filepath.Join(gov, "share", id), dirs, nil)
	}
}

func TestParityConsoleViews(t *testing.T) {
	base := t.TempDir()
	py, gov := filepath.Join(base, "py"), filepath.Join(base, "go")
	copyTree(t, example("console-demo"), py)
	copyTree(t, example("console-demo"), gov)
	dirs := marks("TRIAL", py, gov)
	for _, args := range [][]string{{"console", "state"}, {"console", "since", "--cursor", "0"}} {
		rp := runCLI(t, "py", base, append(args[:2:2], append([]string{py}, args[2:]...)...)...)
		rg := runCLI(t, "go", base, append(args[:2:2], append([]string{gov}, args[2:]...)...)...)
		if rp.code != rg.code {
			t.Errorf("%v: exit python=%d go=%d (%s / %s)", args, rp.code, rg.code, rp.stderr, rg.stderr)
			continue
		}
		mask := func(s string) string {
			s = norm(s, dirs)
			s = isoRe.ReplaceAllString(s, "<TS>")
			s = hhmmRe.ReplaceAllString(s, "<HMS>")
			return canonJSON(s)
		}
		diff(t, strings.Join(args, " ")+" JSON", mask(rp.stdout), mask(rg.stdout))
	}
}

func stageStatuses(t *testing.T, runDir string) string {
	data, err := os.ReadFile(filepath.Join(runDir, "state.json"))
	if err != nil {
		return "missing state.json"
	}
	var st struct {
		Stages map[string]struct {
			Status string `json:"status"`
		} `json:"stages"`
	}
	json.Unmarshal(data, &st)
	var parts []string
	for name, s := range st.Stages {
		parts = append(parts, name+"="+s.Status)
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func readJSON(path string) map[string]any {
	var v map[string]any
	data, _ := os.ReadFile(path)
	json.Unmarshal(data, &v)
	return v
}

func demoCopies(t *testing.T, mutate func(base, dir string)) (base, py, gov string, dirs map[string]string) {
	base = t.TempDir()
	py, gov = filepath.Join(base, "py"), filepath.Join(base, "go")
	for _, d := range []string{py, gov} {
		copyTree(t, example("console-demo"), d)
		if mutate != nil {
			mutate(base, d)
		}
	}
	dirs = marks("TRIAL", py, gov)
	return
}

const runDir = "ajx-reports/orders-cli/console-demo/runs/demo-r1"

func TestParityFullRunStatusReport(t *testing.T) {
	base, py, gov, dirs := demoCopies(t, nil)
	same(t, "run", runCLI(t, "py", base, "run", filepath.Join(py, "trial.toml"), "--no-synthesis"),
		runCLI(t, "go", base, "run", filepath.Join(gov, "trial.toml"), "--no-synthesis"), dirs)
	diff(t, "stage statuses", stageStatuses(t, filepath.Join(py, runDir)), stageStatuses(t, filepath.Join(gov, runDir)))
	vp, vg := readJSON(filepath.Join(py, runDir, "verify.json")), readJSON(filepath.Join(gov, runDir, "verify.json"))
	if vp["outcome"] != vg["outcome"] || fmt.Sprint(vp["passed"]) != fmt.Sprint(vg["passed"]) {
		t.Errorf("verify.json: python %v/%v go %v/%v", vp["outcome"], vp["passed"], vg["outcome"], vg["passed"])
	}
	// Native execution records worker ownership for recovery. This extra file
	// must confirm a stopped group after a complete run, while common evidence
	// and reports remain present in both implementations.
	record := readJSON(filepath.Join(gov, runDir, "execute.process.json"))
	if record["stopped"] != true || record["version"] != float64(1) {
		t.Fatal("native run did not retain a stopped worker ownership record")
	}
	nativeFiles := []string{}
	for _, name := range listFiles(t, filepath.Join(gov, runDir)) {
		if name != "execute.process.json" {
			nativeFiles = append(nativeFiles, name)
		}
	}
	diff(t, "run files", strings.Join(listFiles(t, filepath.Join(py, runDir)), "\n"), strings.Join(nativeFiles, "\n"))
	// each CLI reads the other's output the same way
	for _, d := range []string{py, gov} {
		trial := filepath.Join(d, "trial.toml")
		same(t, "status "+filepath.Base(d), runCLI(t, "py", base, "status", trial), runCLI(t, "go", base, "status", trial), dirs)
		same(t, "status --tail "+filepath.Base(d), runCLI(t, "py", base, "status", trial, "--tail", "5"),
			runCLI(t, "go", base, "status", trial, "--tail", "5"), dirs)
	}
	same(t, "report", runCLI(t, "py", base, "report", filepath.Join(py, "trial.toml"), "--no-synthesis"),
		runCLI(t, "go", base, "report", filepath.Join(gov, "trial.toml"), "--no-synthesis"), dirs)
}

func TestParityCrossResume(t *testing.T) {
	for _, order := range [][2]string{{"py", "go"}, {"go", "py"}} {
		base, py, _, _ := demoCopies(t, nil)
		trial := filepath.Join(py, "trial.toml")
		first := runCLI(t, order[0], base, "run", trial, "--stages", "prepare,execute,verify,teardown", "--no-synthesis")
		if first.code != 0 {
			t.Errorf("%s partial run: %d %s", order[0], first.code, first.stderr)
			continue
		}
		before := readJSON(filepath.Join(py, runDir, "state.json"))
		second := runCLI(t, order[1], base, "run", trial, "--no-synthesis")
		if second.code != 0 {
			t.Errorf("%s resume of %s output: %d\n%s\n%s", order[1], order[0], second.code, second.stdout, second.stderr)
		}
		after := readJSON(filepath.Join(py, runDir, "state.json"))
		started := func(m map[string]any) any {
			e, _ := m["execute"].(map[string]any)
			return e["started_at"]
		}
		if started(before) == nil || fmt.Sprint(started(before)) != fmt.Sprint(started(after)) {
			t.Errorf("%s->%s: execute ran again or was not recorded (%v -> %v)", order[0], order[1], started(before), started(after))
		}
		if got := stageStatuses(t, filepath.Join(py, runDir)); strings.Contains(got, "error") || !strings.Contains(got, "render=done") {
			t.Errorf("%s->%s: stages after resume: %s", order[0], order[1], got)
		}
	}
}

const fakeAgent = `#!/bin/sh
# a synthetic worker: reads the prompt file, writes the expected CSV
echo "reading $1"
cp "$AJX_FAKE_EXPECTED" out.csv
echo "wrote out.csv"
`

func TestParityDeclarativeAdapterRun(t *testing.T) {
	// the script lives outside both trial folders so the two trial files are byte-identical
	base, py, gov, dirs := demoCopies(t, func(base, d string) {
		os.WriteFile(filepath.Join(base, "fake-agent.sh"), []byte(fakeAgent), 0o755)
		body, _ := os.ReadFile(filepath.Join(d, "trial.toml"))
		text := strings.Replace(string(body), "[[cells]]\nid = \"demo\"\nharness = \"console-demo\"",
			"[adapters.fake]\nexecute = [\"/bin/sh\", \""+filepath.Join(base, "fake-agent.sh")+"\", \"{prompt_file}\"]\n"+
				"env = { AJX_FAKE_EXPECTED = \""+filepath.Join(example("console-demo"), "expected.csv")+"\" }\n\n[[cells]]\nid = \"demo\"\nharness = \"fake\"", 1)
		os.WriteFile(filepath.Join(d, "trial.toml"), []byte(text), 0o644)
	})
	same(t, "validate adapter", runCLI(t, "py", base, "validate", filepath.Join(py, "trial.toml")),
		runCLI(t, "go", base, "validate", filepath.Join(gov, "trial.toml")), dirs)
	same(t, "run adapter", runCLI(t, "py", base, "run", filepath.Join(py, "trial.toml"), "--no-synthesis"),
		runCLI(t, "go", base, "run", filepath.Join(gov, "trial.toml"), "--no-synthesis"), dirs)
	diff(t, "adapter stage statuses", stageStatuses(t, filepath.Join(py, runDir)), stageStatuses(t, filepath.Join(gov, runDir)))
	for _, d := range []string{py, gov} {
		v := readJSON(filepath.Join(d, runDir, "verify.json"))
		if v["outcome"] != "succeeded" {
			t.Errorf("%s: verify outcome %v", filepath.Base(d), v["outcome"])
		}
	}
}

// TestParityInterrupt stops a sleeping worker with SIGTERM, and with SIGINT
// from a launcher that left SIGINT ignored (a background job of a
// non-interactive shell): both CLIs must stop the worker, tear down and exit
// 130 with the same message.
func TestParityInterrupt(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		for _, which := range []string{"py", "go"} {
			dir := t.TempDir()
			marker := filepath.Join(dir, "teardown-ran")
			os.WriteFile(filepath.Join(dir, "task-prompt.md"), []byte("x\n"), 0o644)
			os.WriteFile(filepath.Join(dir, "trial.toml"), []byte(`[trial]
id = "t"
product = "p"
workspace_root = "`+filepath.Join(dir, "ws")+`"
[task]
prompt_file = "task-prompt.md"
[adapters.sleepy]
execute = ["/bin/sh", "-c", "sleep 60"]
[[verify]]
run = "true"
[[teardown]]
run = "touch `+marker+`"
[[cells]]
id = "s"
harness = "sleepy"
`), 0o644)
			var cmd *exec.Cmd
			args := []string{"run", filepath.Join(dir, "trial.toml"), "--no-synthesis"}
			if which == "py" {
				cmd = exec.Command(python, append([]string{"-I", filepath.Join(root, "skills", "ajx", "bin", "ajx")}, args...)...)
			} else {
				cmd = exec.Command(goBin, args...)
			}
			cmd.Dir, cmd.Env = dir, env()
			var stderr strings.Builder
			cmd.Stderr = &stderr
			if sig == syscall.SIGINT {
				signal.Ignore(syscall.SIGINT)
			}
			err := cmd.Start()
			if sig == syscall.SIGINT {
				signal.Reset(syscall.SIGINT)
			}
			if err != nil {
				t.Fatal(err)
			}
			raw := filepath.Join(dir, "ajx-reports", "p", "t", "runs", "s-r1", "execute.raw.jsonl")
			for i := 0; i < 200; i++ {
				if _, err := os.Stat(raw); err == nil {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			time.Sleep(500 * time.Millisecond)
			cmd.Process.Signal(sig)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(60 * time.Second):
				cmd.Process.Kill()
				t.Fatalf("%s %v: did not stop", which, sig)
			}
			if code := cmd.ProcessState.ExitCode(); code != 130 {
				t.Errorf("%s %v: exit %d\n%s", which, sig, code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "ajx: interrupted. Captured evidence is kept; rerun the same command to finish the remaining stages.") {
				t.Errorf("%s %v: stderr %s", which, sig, stderr.String())
			}
			if _, err := os.Stat(marker); err != nil {
				t.Errorf("%s %v: teardown did not run", which, sig)
			}
			if got := stageStatuses(t, filepath.Dir(raw)); !strings.Contains(got, "execute=interrupted") || !strings.Contains(got, "teardown=done") {
				t.Errorf("%s %v: stages %s", which, sig, got)
			}
		}
	}
}
