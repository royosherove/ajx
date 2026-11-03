package runner

// A run started by one CLI can be resumed by the other: the Python CLI
// (skills/ajx/bin/ajx) and this package share state.json, matrix-plan.json and
// the evidence files. A declarative `sh` adapter keeps both sides offline.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

func pythonCLI(t *testing.T) []string {
	t.Helper()
	if os.Getenv("AJX_SKIP_PYTHON_PARITY") != "" {
		t.Skip("AJX_SKIP_PYTHON_PARITY set")
	}
	py := util.Which("python3")
	if py == "" {
		t.Skip("python3 not available")
	}
	_, file, _, _ := runtime.Caller(0)
	cli := filepath.Join(filepath.Dir(file), "..", "..", "skills", "ajx", "bin", "ajx")
	if !exists(cli) {
		t.Skip("Python CLI not found")
	}
	return []string{py, "-I", cli}
}

func runPython(t *testing.T, cli []string, dir string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(cli[0], append(append([]string{}, cli[1:]...), args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	} else if err != nil {
		t.Fatalf("python: %v", err)
	}
	return 0, string(out)
}

func crossTrial(t *testing.T, tmp string) string {
	t.Helper()
	_ = os.WriteFile(filepath.Join(tmp, "task-prompt.md"), []byte("x\n"), 0o644)
	text := fmt.Sprintf(`[trial]
id = "t"
product = "p"
workspace_root = %q
timeout_seconds = 30
[task]
prompt_file = "task-prompt.md"
[adapters.shtool]
execute = ["sh", "-c", "echo did-it > made.txt; echo {run_token}"]
[[verify]]
name = "made"
type = "file_exists"
path = "made.txt"
contains = "did-it"
[[teardown]]
run = "echo td-$AJX_RUN_TOKEN"
[[cells]]
id = "s"
harness = "shtool"
env = { B = "1", A = "x" }
`, filepath.Join(tmp, "ws"))
	path := filepath.Join(tmp, "trial.toml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func goMatrix(t *testing.T, path string, stages []string) {
	t.Helper()
	sp, err := spec.Load(path, spec.LoadOpts{CleanupOnly: CleanupOnly(stages)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RunMatrix(context.Background(), sp, MatrixOpts{Stages: stages, Log: func(string) {}}); err != nil {
		t.Fatal(err)
	}
}

func checkCrossState(t *testing.T, tmp string) M {
	t.Helper()
	st := readMapT(t, filepath.Join(tmp, "ajx-reports", "p", "t", "runs", "s-r1", "state.json"))
	for _, s := range []string{"prepare", "execute", "verify", "teardown", "archive"} {
		if util.Get(st, "stages", s, "status") != "done" {
			t.Fatalf("%s: %v", s, statuses(st))
		}
	}
	if util.Get(st, "teardown", "status") != "ok" || util.Get(st, "execute", "reaped") != true {
		t.Fatalf("teardown %v execute %v", st["teardown"], st["execute"])
	}
	if left := dirEntries(t, filepath.Join(tmp, "ws")); len(left) != 0 {
		t.Fatalf("not archived: %v", left)
	}
	td := readListT(t, filepath.Join(tmp, "ajx-reports", "p", "t", "runs", "s-r1", "teardown.json"))
	if !strings.Contains(util.S(td[0]["stdout"]), "td-"+util.S(st["run_token"])) {
		t.Fatalf("teardown.json %v", td)
	}
	return st
}

func TestPythonStartedRunIsResumedByGo(t *testing.T) {
	cli := pythonCLI(t)
	tmp := resolvePath(t.TempDir())
	path := crossTrial(t, tmp)
	if code, out := runPython(t, cli, tmp, "run", path, "--stages", "prepare,execute"); code != 0 {
		t.Fatalf("python run: %d\n%s", code, out)
	}
	goMatrix(t, path, []string{"verify", "teardown", "archive"})
	checkCrossState(t, tmp)
	if code, out := runPython(t, cli, tmp, "status", path); code != 0 || !strings.Contains(out, "succeeded") {
		t.Fatalf("python status after Go resume: %d\n%s", code, out)
	}
}

func TestGoStartedRunIsResumedByPython(t *testing.T) {
	cli := pythonCLI(t)
	tmp := resolvePath(t.TempDir())
	path := crossTrial(t, tmp)
	goMatrix(t, path, []string{"prepare", "execute"})
	if code, out := runPython(t, cli, tmp, "run", path, "--stages", "verify,teardown,archive"); code != 0 {
		t.Fatalf("python resume: %d\n%s", code, out)
	}
	checkCrossState(t, tmp)
	// Go refuses to redo the task Python resumed, like Python does.
	sp := loadSpec(t, path)
	var logs []string
	if _, err := RunMatrix(context.Background(), sp, MatrixOpts{Stages: []string{"execute"},
		Log: func(m string) { logs = append(logs, m) }}); err != nil {
		t.Fatal(err)
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "[s-r1] refusing to redo ['execute']") {
		t.Fatalf("logs %q", logs)
	}
}

func TestGoCrashedExecuteIsMarkedInterruptedByPython(t *testing.T) {
	cli := pythonCLI(t)
	tmp := resolvePath(t.TempDir())
	path := crossTrial(t, tmp)
	goMatrix(t, path, []string{"prepare"})
	// ajx died mid-execute: the stage is left running.
	sp := loadSpec(t, path)
	r := newRun(t, sp, nil)
	r.State["execute"] = M{"session_id": nil, "started_at": util.Now(), "stopped_at": nil, "stop_reason": "running"}
	if err := r.mark("execute", "running", nil); err != nil {
		t.Fatal(err)
	}
	if code, out := runPython(t, cli, tmp, "run", path, "--stages", "teardown,archive"); code != 0 ||
		!strings.Contains(out, "execute was interrupted; keeping its evidence, not re-running the task") {
		t.Fatalf("python resume: %d\n%s", code, out)
	}
	st := readMapT(t, filepath.Join(r.RunDir, "state.json"))
	if util.Get(st, "stages", "execute", "status") != "interrupted" || util.Get(st, "stages", "archive", "status") != "done" {
		t.Fatalf("%v", statuses(st))
	}
}
