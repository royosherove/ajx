package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"

	_ "github.com/royosherove/ajx/internal/builtin"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// offline isolates HOME and shadows aws so no test reaches a real account.
func offline(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "aws"), []byte("#!/bin/sh\necho 'ajx tests: real aws blocked' >&2\nexit 97\n"), 0o755)
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("COLUMNS", "80")
	t.Setenv("CI", "1")
	t.Setenv("AJX_DEMO_PACE", "0")
	for _, v := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		t.Setenv(v, "")
	}
	return home
}

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	oldOut, oldErr := Stdout, Stderr
	Stdout, Stderr = &out, &errb
	defer func() { Stdout, Stderr = oldOut, oldErr }()
	code := Main(args)
	return code, out.String(), errb.String()
}

func decode(t *testing.T, s string) util.M {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	return v
}

func TestUsageErrorsMatchArgparse(t *testing.T) {
	offline(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"status", "x", "--tail", "abc"}, "ajx status: error: argument --tail: invalid int value: 'abc'\n"},
		{[]string{"console", "again", "t", "--quote", "q", "--plan", "p"},
			"ajx console again: error: one of the arguments --same-folder --fresh-folder is required\n"},
		{[]string{"console", "again", "t", "--quote", "q", "--plan", "p", "--same-folder", "--fresh-folder"},
			"ajx console again: error: argument --fresh-folder: not allowed with argument --same-folder\n"},
		{[]string{"console"}, "ajx console: error: the following arguments are required: action\n"},
		{[]string{}, "ajx: error: the following arguments are required: cmd\n"},
		{[]string{"run", "t", "--dry-run=1"}, "ajx run: error: argument --dry-run: ignored explicit argument '1'\n"},
		{[]string{"run"}, "ajx run: error: the following arguments are required: trial\n"},
		{[]string{"run", "x", "y"}, "ajx: error: unrecognized arguments: y\n"},
		{[]string{"run", "x", "--bogus"}, "ajx: error: unrecognized arguments: --bogus\n"},
		{[]string{"console", "note", "t"}, "ajx console note: error: the following arguments are required: text\n"},
		{[]string{"init"}, "ajx init: error: the following arguments are required: dir, --product\n"},
		{[]string{"init", "x"}, "ajx init: error: the following arguments are required: --product\n"},
		{[]string{"export", "x"}, "ajx export: error: the following arguments are required: --run\n"},
		{[]string{"run", "x", "--cells"}, "ajx run: error: argument --cells: expected one argument\n"},
		{[]string{"init", "x", "--re"}, "ajx init: error: ambiguous option: --re could match --reporter, --reporter-model, --repetitions\n"},
	}
	for _, c := range cases {
		code, out, errText := run(t, c.args...)
		if code != 2 || out != "" || !strings.HasSuffix(errText, c.want) || !strings.HasPrefix(errText, "usage: ajx") {
			t.Errorf("%v: code=%d out=%q err=%q, want suffix %q", c.args, code, out, errText, c.want)
		}
	}
	code, _, errText := run(t, "run", "x", "--k")
	if code != 2 || !strings.Contains(errText, "trial file not found") {
		// --k abbreviates --keep-workspace (unique prefix), so the trial is loaded
		t.Errorf("abbreviation: code=%d err=%q", code, errText)
	}
	code, _, errText = run(t, "console", "stop", "--a", "--j")
	if code != 2 || !strings.Contains(errText, "unrecognized arguments: --j") {
		t.Errorf("console stop: code=%d err=%q", code, errText)
	}
}

func TestHelpAndVersion(t *testing.T) {
	offline(t)
	code, out, _ := run(t, "--version")
	if code != 0 || out != "AJX 1.0.0\n" {
		t.Fatalf("version: %d %q", code, out)
	}
	code, out, _ = run(t, "console", "stop", "-h")
	want := "usage: ajx console stop [-h] [--all] [trial]\n\nStop console servers. Runs keep going; reopen a console with `ajx console\nopen`.\n\n" +
		"positional arguments:\n  trial       trial.toml or its directory; `all` is the same as --all\n\n" +
		"options:\n  -h, --help  show this help message and exit\n  --all       stop every console server running for you\n"
	if code != 0 || out != want {
		t.Fatalf("console stop -h:\n%s\nwant:\n%s", out, want)
	}
	code, out, _ = run(t, "console", "again", "-h")
	if code != 0 || !strings.HasPrefix(out, "usage: ajx console again [-h] --quote QUOTE --plan PLAN (--same-folder |\n"+
		"                         --fresh-folder) [--accept-warnings]\n                         trial\n") {
		t.Fatalf("console again -h:\n%s", out)
	}
	code, out, _ = run(t, "version", "--json")
	if code != 0 || decode(t, out)["version"] != "1.0.0" {
		t.Fatalf("version --json: %s", out)
	}
	for _, sh := range []string{"bash", "zsh", "fish"} {
		code, out, _ = run(t, "completion", sh)
		if code != 0 || !strings.Contains(out, "console") || !strings.Contains(out, "same-folder") {
			t.Errorf("completion %s: %d\n%s", sh, code, out)
		}
	}
	if code, _, _ = run(t, "completion", "tcsh"); code != 2 {
		t.Errorf("completion tcsh: %d", code)
	}
}

func TestHyphenWrap(t *testing.T) {
	got := wrapText("overwrite the files in a non-empty --dest", 20)
	want := []string{"overwrite the files", "in a non-empty", "--dest"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%q", got)
	}
	if got := hyphenChunks("my-cell-r1"); strings.Join(got, "|") != "my-|cell-r1" {
		t.Fatalf("%q", got)
	}
}

func TestInitWritesTheScaffoldThatLoads(t *testing.T) {
	offline(t)
	for _, harness := range []string{"claude-code", "codex", "kiro-cli"} {
		tmp, _ := filepath.EvalSymlinks(t.TempDir())
		dir := filepath.Join(tmp, "My Trial")
		code, out, errText := run(t, "init", dir, "--product", "example", "--harness", harness)
		if code != 0 {
			t.Fatalf("%s: %d %s %s", harness, code, out, errText)
		}
		if !strings.Contains(out, "wrote "+filepath.Join(dir, "trial.toml")) || !strings.Contains(out, "Next: describe the task") {
			t.Fatalf("%s: %s", harness, out)
		}
		sp, err := spec.Load(filepath.Join(dir, "trial.toml"), spec.LoadOpts{})
		if err != nil {
			t.Fatalf("%s: %v", harness, err)
		}
		if got := util.Get(sp, "trial", "id"); got != "my-trial" {
			t.Fatalf("id %v", got)
		}
		cell := util.Maps(sp["cells"])[0]
		if cell["harness"] != harness || util.Get(sp, "reporter", "harness") != harness {
			t.Fatalf("%s: %v %v", harness, cell["harness"], util.Get(sp, "reporter", "harness"))
		}
		code, out, _ = run(t, "init", dir, "--product", "example", "--harness", harness)
		if code != 0 || !strings.Contains(out, "exists, not overwritten: ") {
			t.Fatalf("second init: %s", out)
		}
	}
	code, _, errText := run(t, "init", t.TempDir(), "--product", "x", "--harness", "nope")
	if code != 2 || !strings.Contains(errText, "trial file error: unknown harness 'nope'; run `ajx plugins`") {
		t.Fatalf("unknown harness: %d %s", code, errText)
	}
}

func TestInitFromFlagsHeadless(t *testing.T) {
	offline(t)
	root := t.TempDir()
	fixture := filepath.Join(root, "fixture")
	os.MkdirAll(fixture, 0o755)
	os.WriteFile(filepath.Join(fixture, "orders.json"), []byte("[]\n"), 0o644)
	dir := filepath.Join(root, "trial")
	code, out, errText := run(t, "init", dir, "--product", "jq", "--harness", "codex", "--no-input", "--json",
		"--task", "Convert orders.json to orders.csv.", "--material", "jq --help", "--material", "https://example.com/doc",
		"--check", "test -s orders.csv", "--check", "file:orders.csv", "--check", "http:http://127.0.0.1:9/health",
		"--model", "example-model", "--reporter-model", "example-reporter", "--timeout", "300", "--repetitions", "2",
		"--fixture", fixture)
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, errText)
	}
	doc := decode(t, out)
	if doc["valid"] != true || doc["schema_version"] != float64(1) {
		t.Fatalf("%v", doc)
	}
	sp, err := spec.Load(filepath.Join(dir, "trial.toml"), spec.LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if util.Int(util.Get(sp, "trial", "timeout_seconds"), 0) != 300 || util.Int(util.Get(sp, "trial", "repetitions"), 0) != 2 {
		t.Fatalf("limits: %v", sp["trial"])
	}
	verify := util.Maps(sp["verify"])
	if len(verify) != 3 || verify[0]["type"] != "shell" || verify[1]["type"] != "file_exists" || verify[2]["type"] != "http" {
		t.Fatalf("verify: %v", verify)
	}
	if util.Maps(sp["cells"])[0]["model"] != "example-model" || util.Get(sp, "reporter", "model") != "example-reporter" {
		t.Fatalf("models")
	}
	if real, _ := filepath.EvalSymlinks(fixture); util.S(util.Get(sp, "task", "fixture_dir")) != real {
		t.Fatalf("fixture %v", util.Get(sp, "task", "fixture_dir"))
	}
	prompt, _ := os.ReadFile(filepath.Join(dir, "task-prompt.md"))
	if !strings.Contains(string(prompt), "Convert orders.json to orders.csv.") || !strings.Contains(string(prompt), "- jq --help") {
		t.Fatalf("prompt: %s", prompt)
	}
	// headless and missing settings: argparse's error, no prompt
	code, _, errText = run(t, "init", "--no-input")
	if code != 2 || !strings.Contains(errText, "the following arguments are required: dir, --product") {
		t.Fatalf("missing: %d %s", code, errText)
	}
	code, _, errText = run(t, "init", "x", "--product", "p", "--wizard")
	if code != 2 || !strings.Contains(errText, "--wizard needs a terminal") {
		t.Fatalf("wizard without tty: %d %s", code, errText)
	}
	code, _, errText = run(t, "init", t.TempDir(), "--product", "p", "--environment", "container")
	if code != 2 || !strings.Contains(errText, "--environment container needs --image") {
		t.Fatalf("container: %d %s", code, errText)
	}
}

func TestPluginsAndDoctorJSON(t *testing.T) {
	offline(t)
	code, out, _ := run(t, "plugins", "--json")
	doc := decode(t, out)
	if code != 0 || len(util.List(doc["harness"])) == 0 || len(util.List(doc["check"])) == 0 {
		t.Fatalf("plugins: %s", out)
	}
	code, out, _ = run(t, "plugins")
	if code != 0 || !strings.HasPrefix(out, "harness:\n  ") || !strings.Contains(out, "\ncheck:\n  file_exists") {
		t.Fatalf("plugins text: %s", out)
	}
	trial := filepath.Join(repoRoot(t), "skills", "ajx", "examples", "console-demo", "trial.toml")
	code, out, _ = run(t, "doctor", trial, "--json")
	doc = decode(t, out)
	if code != 0 || doc["ready"] != true || len(util.List(doc["cells"])) != 1 || len(util.List(doc["lines"])) == 0 {
		t.Fatalf("doctor: %d %s", code, out)
	}
	code, out, _ = run(t, "validate", trial, "--json")
	doc = decode(t, out)
	if code != 0 || util.Get(doc, "trial", "id") != "console-demo" || len(util.List(doc["runs"])) != 1 {
		t.Fatalf("validate: %d %s", code, out)
	}
	code, out, errText := run(t, "validate", filepath.Join(t.TempDir(), "missing.toml"), "--json")
	doc = decode(t, out)
	if code != 2 || doc["ok"] != false || util.Get(doc, "error", "type") != "trial_file_error" ||
		!strings.HasPrefix(errText, "trial file error: trial file not found") {
		t.Fatalf("validate error: %d %s %s", code, out, errText)
	}
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
			if info.Name() == "ajx-reports" || info.Name() == ".ajx-console" || info.Name() == "__pycache__" {
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

func TestStatusAndExportOverTheExampleReport(t *testing.T) {
	offline(t)
	out := filepath.Join(t.TempDir(), "matrix")
	copyTree(t, filepath.Join(repoRoot(t), "docs", "example-report"), out)
	code, text, _ := run(t, "status", out)
	if code != 0 || !strings.HasPrefix(text, "run                  prepare execute  verify teardow normali narrate release extract measure  render archive  verified\n") ||
		!strings.Contains(text, "baseline-codex-r1") {
		t.Fatalf("status: %d\n%s", code, text)
	}
	code, text, _ = run(t, "status", out, "--json")
	doc := decode(t, text)
	if code != 0 || len(util.List(doc["runs"])) != 3 {
		t.Fatalf("status json: %s", text)
	}
	code, _, errText := run(t, "export", out, "--run", "nope")
	if code != 1 || !strings.Contains(errText, "export: run 'nope' not found in") || !strings.Contains(errText, "runs: baseline-codex-r1") {
		t.Fatalf("export missing: %d %s", code, errText)
	}
	code, text, errText = run(t, "export", out, "--run", "baseline-codex-r1", "--json")
	if code != 0 {
		t.Fatalf("export: %d %s %s", code, text, errText)
	}
	doc = decode(t, text)
	if len(util.List(doc["files"])) == 0 {
		t.Fatalf("export json: %s", text)
	}
	code, _, errText = run(t, "export", out, "--run", "baseline-codex-r1")
	if code != 1 || !strings.Contains(errText, "is not empty; pass --force") {
		t.Fatalf("export twice: %d %s", code, errText)
	}
}

func TestRunDemoTrialEndToEnd(t *testing.T) {
	offline(t)
	dir := filepath.Join(t.TempDir(), "demo")
	copyTree(t, filepath.Join(repoRoot(t), "skills", "ajx", "examples", "console-demo"), dir)
	trial := filepath.Join(dir, "trial.toml")
	code, out, errText := run(t, "run", trial, "--dry-run")
	if code != 0 || !strings.Contains(out, `"demo-r1"`) {
		t.Fatalf("dry run: %d %s %s", code, out, errText)
	}
	code, _, errText = run(t, "run", trial, "--stages", "rendr")
	if code != 2 || !strings.Contains(errText, "trial file error: unknown stage(s)") {
		t.Fatalf("bad stage: %d %s", code, errText)
	}
	code, _, errText = run(t, "run", trial, "--expect-plan", "0000000")
	if code != 2 || !strings.Contains(errText, "the trial changed after it was approved") {
		t.Fatalf("expect-plan: %d %s", code, errText)
	}
	code, out, errText = run(t, "run", trial, "--no-synthesis", "--json")
	if code != 0 {
		t.Fatalf("run: %d %s %s", code, out, errText)
	}
	doc := decode(t, out)
	if util.Get(doc, "status", "runs") == nil || !strings.Contains(errText, "[demo-r1]") {
		t.Fatalf("run json: %s\nstderr: %s", out, errText)
	}
	code, out, _ = run(t, "status", trial)
	if code != 0 || !strings.Contains(out, "demo-r1                   ok      ok      ok      ok      ok      ok      ok      ok      ok      ok      ok  succeeded") {
		t.Fatalf("status: %s", out)
	}
	code, out, _ = run(t, "report", trial, "--no-synthesis")
	if code != 0 || !strings.HasSuffix(strings.TrimSpace(out), "index.html") {
		t.Fatalf("report: %s", out)
	}
}

func TestCIOnTheDemoTrial(t *testing.T) {
	offline(t)
	dir := filepath.Join(t.TempDir(), "demo")
	copyTree(t, filepath.Join(repoRoot(t), "skills", "ajx", "examples", "console-demo"), dir)
	trial := filepath.Join(dir, "trial.toml")
	out := filepath.Join(t.TempDir(), "ci")
	code, text, errText := run(t, "ci", trial, "--output", out, "--no-synthesis")
	if code != 0 || !strings.Contains(text, "ci: decision passed") {
		t.Fatalf("ci: %d\n%s\n%s", code, text, errText)
	}
	res := util.ReadMap(filepath.Join(out, "ci-result.json"))
	if res["decision"] != "passed" || util.Int(util.Get(res, "coverage", "passed"), 0) != 1 {
		t.Fatalf("%v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "junit.xml")); !strings.Contains(string(b), `name="demo-r1"`) {
		t.Fatalf("junit: %s", b)
	}
	code, text, _ = run(t, "ci", trial, "--output", out)
	if code != 2 || !strings.Contains(text, "output_not_fresh") {
		t.Fatalf("reuse: %d %s", code, text)
	}
	// a check the demo output cannot pass: the decision fails although the worker exited 0
	body, _ := os.ReadFile(trial)
	os.WriteFile(trial, []byte(strings.Replace(string(body), `run = "cmp -s out.csv`, `run = "false && cmp -s out.csv`, 1)), 0o644)
	out2 := filepath.Join(t.TempDir(), "ci2")
	code, text, _ = run(t, "ci", trial, "--output", out2, "--no-synthesis", "--junit", filepath.Join(out2, "reports", "j.xml"))
	if code != 1 || !strings.Contains(text, "success_rate_below_target") {
		t.Fatalf("failing check: %d %s", code, text)
	}
	if _, err := os.Stat(filepath.Join(out2, "reports", "j.xml")); err != nil {
		t.Fatal(err)
	}
	code, text, _ = run(t, "ci", filepath.Join(dir, "missing.toml"), "--output", filepath.Join(t.TempDir(), "x"))
	if code != 2 || !strings.Contains(text, "invalid_config") {
		t.Fatalf("invalid: %d %s", code, text)
	}
	code, _, errText = run(t, "ci", trial, "--min-success-rate", "most")
	if code != 2 || !strings.Contains(errText, "invalid float value: 'most'") {
		t.Fatalf("rate: %d %s", code, errText)
	}
}

func TestRunInterruptCleansUpAndExits130(t *testing.T) {
	offline(t)
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
execute = ["/bin/sh", "-c", "sleep 30"]
[[verify]]
run = "true"
[[teardown]]
run = "touch `+marker+`"
[[cells]]
id = "s"
harness = "sleepy"
`), 0o644)
	go func() {
		for i := 0; i < 100; i++ {
			if m, _ := filepath.Glob(filepath.Join(dir, "ajx-reports", "*", "*", "runs", "*", "execute.raw.jsonl")); len(m) > 0 {
				break
			}
			sleep(100)
		}
		sleep(300)
		syscallKill()
	}()
	code, _, errText := run(t, "run", filepath.Join(dir, "trial.toml"), "--no-synthesis")
	if code != 130 || !strings.Contains(errText, "ajx: interrupted. Captured evidence is kept") {
		t.Fatalf("code=%d err=%s", code, errText)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("teardown did not run: %v", err)
	}
}

func sleep(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

func syscallKill() { _ = syscall.Kill(os.Getpid(), syscall.SIGINT) }

func TestPythonPluginsAreNamedNotRun(t *testing.T) {
	offline(t)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "plugins"), 0o755)
	os.WriteFile(filepath.Join(dir, "plugins", "mine.py"), []byte("from ajx.plugins import register\n@register(\"harness\", \"py-only-agent\")\nclass A: pass\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "task-prompt.md"), []byte("x\n"), 0o644)
	trial := filepath.Join(dir, "trial.toml")
	os.WriteFile(trial, []byte("[trial]\nid = \"t\"\nproduct = \"p\"\n[task]\nprompt_file = \"task-prompt.md\"\n[[cells]]\nid = \"c\"\nharness = \"py-only-agent\"\n"), 0o644)
	for _, cmd := range []string{"validate", "run"} {
		code, _, errText := run(t, cmd, trial)
		if code != 2 || !strings.Contains(errText, `harness plugin "py-only-agent" is a Python plugin`) {
			t.Fatalf("%s: %d %s", cmd, code, errText)
		}
	}
	code, out, _ := run(t, "doctor", trial)
	if code != 1 || !strings.Contains(out, "is a Python plugin") || !strings.Contains(out, "NOT READY") {
		t.Fatalf("doctor: %d %s", code, out)
	}
}

func TestTrialDirectoryIsAcceptedWhereATrialFileIs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my-cli")
	if code, _, errOut := run(t, "init", dir, "--product", "my-cli", "--harness", "claude-code", "--no-input"); code != 0 {
		t.Fatalf("init exit %d: %s", code, errOut)
	}
	code, out, errOut := run(t, "validate", dir)
	if code != 0 || !strings.Contains(out, "trial my-cli product=my-cli") {
		t.Fatalf("validate <dir> exit %d\n%s\n%s", code, out, errOut)
	}
	code, out, _ = run(t, "status", dir)
	if code != 0 || !strings.HasPrefix(out, "run ") {
		t.Fatalf("status <dir> exit %d\n%s", code, out)
	}
}

func TestInitRefusesAModelNameNoAgentAcceptsAndSlugsOddFolderNames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Vercel CLI and Supabase CLI - Set up a TODO app that allows creating and listing todo items.")
	code, _, errOut := run(t, "init", dir, "--product", "Vercel CLI", "--model", "sonnet 5.5", "--no-input")
	if code != 2 || !strings.Contains(errOut, "not a model name") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	code, _, errOut = run(t, "init", dir, "--product", "Vercel CLI", "--check", `echo "hello"`, "--no-input")
	if code != 0 || !strings.Contains(errOut, "always passes") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if code, out, errOut := run(t, "validate", dir); code != 0 {
		t.Fatalf("the scaffold does not load: %s %s", out, errOut)
	}
}
