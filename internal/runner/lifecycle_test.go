package runner

// Lifecycle tests with a fake harness (no LLM calls): ports of the runner parts
// of skills/ajx/tests/test_ajx.py. Worker processes are real /bin/sh scripts so
// process groups, timeouts and interruption are exercised for real.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/royosherove/ajx/internal/evidence"
	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/render"
	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

// fakeHarness is deterministic: writes a file, emits two events, supports narrate.
type fakeHarness struct {
	plugin.BaseHarness
	script   string // execute command (sh -c)
	onExec   func(ctx *plugin.Ctx) error
	execErr  error
	narrated int
}

func newFakeHarness(script string) func() plugin.Harness {
	return func() plugin.Harness {
		return &fakeHarness{BaseHarness: plugin.BaseHarness{BinaryName: "true", CanResume: true,
			CleanSupported: true, VerifiedLive: true}, script: script}
	}
}

func init() {
	plugin.RegisterHarness("fake", "test", newFakeHarness("echo fake-run; sleep 0.2; echo done"))
	// Records its pid and a background child's, then runs until stopped.
	plugin.RegisterHarness("fake-slow", "test", newFakeHarness(
		`echo $$ > worker.pid; sleep 60 & echo $! > child.pid; echo started; wait`))
	// Leaves a server running after a normal exit (kept alive for verification).
	plugin.RegisterHarness("fake-server", "test", newFakeHarness(
		`sleep 60 & echo $! > server.pid; echo fake-run; echo done`))
}

func (h *fakeHarness) Plan(ctx *plugin.Ctx) M { return M{"session_id": "fake-sid"} }

func (h *fakeHarness) Execute(ctx *plugin.Ctx) (M, error) {
	if h.execErr != nil {
		return nil, h.execErr
	}
	if h.onExec != nil {
		if err := h.onExec(ctx); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(ctx.Workspace, "made.txt"), []byte("hello"), 0o644); err != nil {
		return nil, err
	}
	proc, err := h.Run(ctx, []string{"sh", "-c", h.script}, "execute", plugin.RunOpts{})
	if err != nil {
		return proc, err
	}
	proc["session_id"] = "fake-sid"
	return proc, nil
}

func (h *fakeHarness) Narrate(ctx *plugin.Ctx, prompt string) (M, error) {
	h.narrated++
	text := "# Journey: fake\n\n## Outcome I declared\nI made the file.\n\n## What I asked for first\n- nothing\n\n" +
		"## Chronological account\n1. I ran the command [E-001]. It printed done [E-002].\n" + strings.Repeat("x", 200)
	quoted, _ := json.Marshal(text)
	proc, err := h.Run(ctx, []string{"sh", "-c", "printf %s " + string(quoted)}, "narrate", plugin.RunOpts{Timeout: 10})
	if err != nil {
		return proc, err
	}
	proc["session_id"] = "fake-sid"
	proc["tools_disabled"] = true
	return proc, nil
}

func (h *fakeHarness) Normalize(ctx *plugin.Ctx, stage string, exclude []string) (M, error) {
	rows := util.Maps(util.ReadJSONL(filepath.Join(ctx.RunDir, stage+".raw.jsonl")))
	tel := plugin.EmptyTelemetry(M{"session_id": "fake-sid", "model_ids": []any{"fake-1"}, "usage_status": "complete",
		"expected_output_tokens": int64(7), "tool_status": "complete", "expected_tool_calls": int64(1),
		"timestamp_source": "arrival"})
	if stage == "execute" {
		if len(rows) == 0 {
			return tel, nil
		}
		first, last := rows[0]["at"], rows[len(rows)-1]["at"]
		tool := plugin.ToolEvent("t1", "shell", M{"command": "echo fake-run"}, first, "m1")
		plugin.FinishTool(tool, last, false, "done")
		tel["events"] = []any{tool, plugin.TextEvent("I made the file.", last, "m2")}
		tel["usage"] = []any{M{"id": "m1", "at": first, "output_tokens": int64(3)}, M{"id": "m2", "at": last, "output_tokens": int64(4)}}
		tel["message_ids"] = []any{"m1", "m2"}
		tel["final_text"] = "I made the file."
		tel["stop_reason"] = "success"
	} else {
		var lines []string
		for _, r := range rows {
			lines = append(lines, util.S(r["line"]))
		}
		tel["final_text"] = strings.Join(lines, "\n")
		tel["usage_status"], tel["tool_status"] = "unavailable", "unavailable"
	}
	return tel, nil
}

// ---------------------------------------------------------------- fixtures

func writeTrial(t *testing.T, tmp, body, cells string) string {
	t.Helper()
	if cells == "" {
		cells = "[[cells]]\nid = \"f\"\nharness = \"fake\"\n"
	}
	if err := os.WriteFile(filepath.Join(tmp, "task-prompt.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf("[trial]\nid = \"t\"\nproduct = \"p\"\nworkspace_root = %q\n[task]\nprompt_file = \"task-prompt.md\"\n%s\n%s",
		filepath.Join(tmp, "ws"), body, cells)
	path := filepath.Join(tmp, "trial.toml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadSpec(t *testing.T, path string) M {
	t.Helper()
	sp, err := spec.Load(path, spec.LoadOpts{})
	if err != nil {
		t.Fatalf("spec.Load: %v", err)
	}
	return sp
}

func newRun(t *testing.T, sp M, log func(string)) *Run {
	t.Helper()
	if log == nil {
		log = func(string) {}
	}
	r, err := NewRun(sp, util.Maps(sp["cells"])[0], 1, log)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	return r
}

func noModelExtract(r *Run) error {
	return util.WriteJSON(filepath.Join(r.RunDir, "asks.json"), M{"asks": []any{}, "journey_errata": []any{},
		"strengths": []any{}, "gates": []any{}, "no_obstacles_observed": true, "extraction_status": "ok", "extractor": M{}})
}

func stubExtract(r *Run) { r.Override["extract"] = noModelExtract }

func readMapT(t *testing.T, path string) M {
	t.Helper()
	v, err := ReadJSON(path, nil)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return util.MapOr(v)
}

func readListT(t *testing.T, path string) []M {
	t.Helper()
	v, err := ReadJSON(path, nil)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return util.Maps(v)
}

func statuses(state M) map[string]string {
	out := map[string]string{}
	for k, v := range util.MapOr(state["stages"]) {
		out[k] = util.S(util.Get(v, "status"))
	}
	return out
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// waitDead polls until pid no longer exists (zombies are reaped by init).
func waitDead(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !PidAlive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return !PidAlive(pid)
}

func waitFile(path string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for first := true; first || time.Now().Before(deadline); first = false {
		if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func pidFrom(t *testing.T, path string) int {
	t.Helper()
	var pid int
	if _, err := fmt.Sscan(readFile(t, path), &pid); err != nil {
		t.Fatalf("pid in %s: %v", path, err)
	}
	return pid
}

// ---------------------------------------------------------------- tests

func TestFullRunWithoutLLM(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	if err := os.WriteFile(filepath.Join(tmp, "task-prompt.md"), []byte("make a file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	trial := fmt.Sprintf(`
[trial]
id = "fake-trial"
product = "fakeprod"
workspace_root = %q
product_version_cmd = "echo v1.2.3"
[task]
prompt_file = "task-prompt.md"
[[setup]]
run = "echo setup > setup.marker"
[[verify]]
name = "file made"
type = "file_exists"
path = "made.txt"
contains = "hello"
[[verify]]
name = "shell check fails"
run = "test -f nope.txt"
[[teardown]]
run = "echo torn-$AJX_RUN_TOKEN"
[[cells]]
id = "f"
harness = "fake"
`, filepath.Join(tmp, "ws"))
	if err := os.WriteFile(filepath.Join(tmp, "trial.toml"), []byte(trial), 0o644); err != nil {
		t.Fatal(err)
	}
	sp := loadSpec(t, filepath.Join(tmp, "trial.toml"))
	r := newRun(t, sp, nil)
	// stub the extract stage (reporter LLM) with a deterministic register
	r.Override["extract"] = func(r *Run) error {
		tel := readMapT(t, filepath.Join(r.RunDir, "telemetry.execute.json"))
		asks := []M{{"id": "ASK-001", "title": "Say more", "requested_behavior": "print progress",
			"evidence_status": "observed_friction", "how_observed": "I ran it", "consequence": "waited",
			"event_refs": []any{"E-001"}, "journey_anchors": []any{}, "labels": []any{"Wait"},
			"priority_rationale": "one slow call", "modeled_saving": nil, "verification": "rerun", "workaround": nil}}
		asks = evidence.AskCosts(asks, tel)
		r.State["extract"] = M{"started_at": util.Now(), "stopped_at": util.Now(), "output_tokens": int64(50)}
		return util.WriteJSON(filepath.Join(r.RunDir, "asks.json"), M{"asks": mapsList(asks), "journey_errata": []any{},
			"strengths": []any{}, "gates": []any{}, "no_obstacles_observed": false, "extraction_status": "ok", "extractor": M{}})
	}
	state, err := r.Go(context.Background(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range statuses(state) {
		if v != "done" {
			t.Fatalf("stage %s = %s: %v", k, v, state["stages"])
		}
	}
	rd := r.RunDir
	verify := readMapT(t, filepath.Join(rd, "verify.json"))
	if util.Int(verify["passed"], -1) != 1 || util.Int(verify["total"], -1) != 2 || verify["outcome"] != "partial" {
		t.Fatalf("verify %v", verify)
	}
	td := readListT(t, filepath.Join(rd, "teardown.json"))
	if !strings.Contains(util.S(td[0]["stdout"]), "torn-"+util.S(state["run_token"])) {
		t.Fatalf("teardown %v", td)
	}
	env := readMapT(t, filepath.Join(rd, "environment.json"))
	if env["product_version_before"] != "v1.2.3" {
		t.Fatalf("product_version_before %v", env["product_version_before"])
	}
	runj := readMapT(t, filepath.Join(rd, "run.json"))
	if runj["task_outcome_verified"] != "partial" || runj["task_outcome_declared"] != "I made the file." {
		t.Fatalf("run.json %v / %v", runj["task_outcome_verified"], runj["task_outcome_declared"])
	}
	if util.Truthy(runj["report_instructions_visible_during_task"]) {
		t.Fatal("report instructions visible")
	}
	if !strings.HasPrefix(util.S(runj["journey_provenance"]), "self-narrated") || runj["review_status"] != "ready" {
		t.Fatalf("provenance %v review %v", runj["journey_provenance"], runj["review_status"])
	}
	if !strings.Contains(readFile(t, filepath.Join(rd, "asks.md")), "ASK-001") {
		t.Fatal("asks.md lacks ASK-001")
	}
	// the recorded argv never contains the prompt or auth values
	if strings.Contains(util.Compact(util.Get(state, "execute", "argv")), "make a file") {
		t.Fatal("prompt in argv")
	}
	if !exists(filepath.Join(rd, "report.html")) {
		t.Fatal("no report.html")
	}
	meas := readMapT(t, filepath.Join(rd, "measurements.json"))
	if meas["measurement_error"] != nil || util.Int(util.Get(meas, "task_summary", "output_tokens"), -1) != 7 {
		t.Fatalf("measurements %v", meas["measurement_error"])
	}
	// archive moved the workspace under the run dir and nothing is left in workspace_root
	if !exists(filepath.Join(rd, "workspace", "made.txt")) {
		t.Fatal("workspace not archived")
	}
	if left := dirEntries(t, filepath.Join(tmp, "ws")); len(left) != 0 {
		t.Fatalf("workspace_root not empty: %v", left)
	}
	if strings.Contains(filepath.Base(util.S(util.Get(state, "paths", "workspace"))), "fake") {
		t.Fatal("workspace path names the cell")
	}
	if journey := readFile(t, filepath.Join(rd, "journey.md")); !strings.HasPrefix(journey,
		"<!-- provenance: self-narrated: the original task agent, resumed after the task; tools disabled -->\n") {
		t.Fatalf("journey header: %q", util.Clip(journey, 120))
	}
	if err := render.MatrixReport(sp, render.MatrixOpts{Log: func(string) {}}); err != nil {
		t.Fatal(err)
	}
	out := util.S(util.Get(sp, "trial", "output_dir"))
	mx := readMapT(t, filepath.Join(out, "matrix.json"))
	row := util.Maps(mx["runs"])[0]
	if row["verified"] != "partial" || util.Int(row["asks"], -1) != 1 {
		t.Fatalf("matrix row %v", row)
	}
}

func TestResumeSkipsDoneAndRunsPendingTeardown(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	sp := loadSpec(t, writeTrial(t, tmp, "[[teardown]]\nrun = \"echo td\"\n", ""))
	r := newRun(t, sp, nil)
	if _, err := r.Go(context.Background(), []string{"prepare", "execute"}, false); err != nil {
		t.Fatal(err)
	}
	if r.Status("execute") != "done" || r.Status("teardown") != "" {
		t.Fatalf("after execute: %v", statuses(r.State))
	}
	r2 := newRun(t, sp, nil)
	if _, err := r2.Go(context.Background(), []string{"verify"}, false); err != nil {
		t.Fatal(err)
	}
	if r2.Status("teardown") != "done" || r2.Status("verify") != "done" {
		t.Fatalf("pending teardown not run: %v", statuses(r2.State))
	}
	first := util.Get(r2.State, "stages", "verify", "at")
	time.Sleep(5 * time.Millisecond)
	r3 := newRun(t, sp, nil)
	if _, err := r3.Go(context.Background(), []string{"verify"}, false); err != nil {
		t.Fatal(err)
	}
	if util.Get(r3.State, "stages", "verify", "at") == first {
		t.Fatal("explicit post-processing stage was not redone")
	}
	execAt := util.Get(r3.State, "stages", "execute", "at")
	var logs []string
	r4 := newRun(t, sp, func(m string) { logs = append(logs, m) })
	if _, err := r4.Go(context.Background(), []string{"execute"}, false); err != nil {
		t.Fatal(err)
	}
	if util.Get(r4.State, "stages", "execute", "at") != execAt {
		t.Fatal("a task was re-run in place")
	}
	want := fmt.Sprintf("[f-r1] refusing to redo ['execute']: a task runs once per repetition; add a repetition or delete %s", r4.RunDir)
	if len(logs) != 1 || logs[0] != want {
		t.Fatalf("logs %q", logs)
	}
}

func failureTrial(t *testing.T, tmp, setup string) M {
	body := setup + "\n[[verify]]\nname = \"file made\"\ntype = \"file_exists\"\npath = \"made.txt\"\n"
	return loadSpec(t, writeTrial(t, tmp, body, ""))
}

func TestInterruptedExecuteKeepsEvidenceAndProducesArtifactsWithoutLLM(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	sp := failureTrial(t, tmp, "")
	r := newRun(t, sp, nil)
	if _, err := r.Go(context.Background(), []string{"prepare"}, false); err != nil {
		t.Fatal(err)
	}
	// simulate ajx dying mid-execute: planned session recorded, stage left "running"
	r.State["execute"] = M{"session_id": "fake-sid", "started_at": util.Now(), "stopped_at": nil, "stop_reason": "running"}
	if err := r.mark("execute", "running", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.RunDir, "execute.raw.jsonl"),
		[]byte(util.Compact(M{"at": util.Now(), "line": "partial"})+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs []string
	r2 := newRun(t, sp, func(m string) { logs = append(logs, m) })
	stubExtract(r2)
	state, err := r2.Go(context.Background(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Status("execute") != "interrupted" || util.Get(state, "execute", "stop_reason") != "interrupted" ||
		util.Get(state, "execute", "session_id") != "fake-sid" {
		t.Fatalf("execute %v / %v", statuses(state), state["execute"])
	}
	for _, st := range []string{"verify", "teardown", "normalize", "narrate", "measure", "render", "archive"} {
		if r2.Status(st) != "done" {
			t.Fatalf("%s = %s", st, r2.Status(st))
		}
	}
	runj := readMapT(t, filepath.Join(r2.RunDir, "run.json"))
	if runj["task_stop_reason"] != "interrupted" || runj["task_outcome_verified"] != "failed" {
		t.Fatalf("run.json %v %v", runj["task_stop_reason"], runj["task_outcome_verified"])
	}
	if !exists(filepath.Join(r2.RunDir, "journey.md")) {
		t.Fatal("no journey")
	}
	if !strings.Contains(strings.Join(util.Strings(runj["limitations"]), " "), "interrupted") {
		t.Fatalf("limitations %v", runj["limitations"])
	}
	if len(logs) == 0 || logs[0] != "[f-r1] execute was interrupted; keeping its evidence, not re-running the task" {
		t.Fatalf("logs %q", logs)
	}
}

func TestExecuteErrorYieldsStubArtifactsAndIncompleteStatus(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	sp := failureTrial(t, tmp, "")
	var logs []string
	r := newRun(t, sp, func(m string) { logs = append(logs, m) })
	r.Harness.(*fakeHarness).execErr = errors.New("binary exploded")
	state, err := r.Go(context.Background(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status("execute") != "error" || r.Status("teardown") != "done" || state["journey_provenance"] != "unavailable" {
		t.Fatalf("%v %v", statuses(state), state["journey_provenance"])
	}
	if got := util.Get(state, "stages", "execute", "error"); got != "RuntimeError: binary exploded" {
		t.Fatalf("error %q", got)
	}
	if !util.Contains(logs, "[f-r1] execute failed: binary exploded") {
		t.Fatalf("logs %q", logs)
	}
	asks := readMapT(t, filepath.Join(r.RunDir, "asks.json"))
	if !strings.HasPrefix(util.S(asks["extraction_status"]), "skipped") {
		t.Fatalf("asks %v", asks["extraction_status"])
	}
	runj := readMapT(t, filepath.Join(r.RunDir, "run.json"))
	if runj["review_status"] != "incomplete" {
		t.Fatalf("review_status %v", runj["review_status"])
	}
	if !strings.Contains(readFile(t, filepath.Join(r.RunDir, "asks.md")), "Extraction did not complete") {
		t.Fatal("asks.md")
	}
	if left := dirEntries(t, filepath.Join(tmp, "ws")); len(left) != 0 {
		t.Fatalf("not archived: %v", left)
	}
}

func TestPrepareFailureArchivesWorkspace(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	sp := failureTrial(t, tmp, "[[setup]]\nrun = \"exit 3\"\n")
	r := newRun(t, sp, nil)
	state, err := r.Go(context.Background(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status("prepare") != "error" {
		t.Fatalf("%v", statuses(state))
	}
	if got := util.S(util.Get(state, "stages", "prepare", "error")); got != "RuntimeError: setup command failed: 'exit 3' exit=3" {
		t.Fatalf("error %q", got)
	}
	if left := dirEntries(t, filepath.Join(tmp, "ws")); len(left) != 0 {
		t.Fatalf("not archived: %v", left)
	}
	if !exists(filepath.Join(r.RunDir, "workspace")) {
		t.Fatal("workspace not under run dir")
	}
	if util.Int(readListT(t, filepath.Join(r.RunDir, "setup.json"))[0]["exit_code"], -1) != 3 {
		t.Fatal("setup.json exit code")
	}
}

func TestArchivePurgesCopiedCredentialsAndRepointsTranscript(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	sp := loadSpec(t, writeTrial(t, tmp, "", ""))
	r := newRun(t, sp, nil)
	if _, err := r.Go(context.Background(), []string{"prepare", "execute", "verify", "teardown"}, false); err != nil {
		t.Fatal(err)
	}
	cfg := util.S(util.Get(r.State, "paths", "config_dir"))
	_ = os.WriteFile(filepath.Join(cfg, "auth.json"), []byte(`{"token": "secret"}`), 0o600)
	_ = os.MkdirAll(filepath.Join(cfg, "projects"), 0o755)
	_ = os.WriteFile(filepath.Join(cfg, "projects", "s.jsonl"), []byte("{}\n"), 0o644)
	if err := util.WriteJSON(filepath.Join(r.RunDir, "telemetry.execute.json"),
		M{"events": []any{}, "transcript_path": filepath.Join(cfg, "projects", "s.jsonl")}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Go(context.Background(), []string{"archive"}, false); err != nil {
		t.Fatal(err)
	}
	archived := filepath.Join(r.RunDir, "harness-config")
	if exists(filepath.Join(archived, "auth.json")) || !exists(filepath.Join(archived, "projects", "s.jsonl")) {
		t.Fatal("credentials not purged or transcript lost")
	}
	if got := readMapT(t, filepath.Join(r.RunDir, "telemetry.execute.json"))["transcript_path"]; got != filepath.Join(archived, "projects", "s.jsonl") {
		t.Fatalf("transcript_path %v", got)
	}
	// narrate cannot be redone after archive
	var logs []string
	r.Log = func(m string) { logs = append(logs, m) }
	if _, err := r.Go(context.Background(), []string{"narrate"}, false); err != nil {
		t.Fatal(err)
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "refusing to redo ['narrate'] after archive") {
		t.Fatalf("logs %q", logs)
	}
}

func TestParallelMatrixRunsBothCells(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	path := writeTrial(t, tmp, "", "[[cells]]\nid = \"a\"\nharness = \"fake\"\n[[cells]]\nid = \"b\"\nharness = \"fake\"\n")
	text := strings.Replace(readFile(t, path), "product = \"p\"\n", "product = \"p\"\nparallel = 2\n", 1)
	_ = os.WriteFile(path, []byte(text), 0o644)
	sp := loadSpec(t, path)
	StageOverrides["extract"] = noModelExtract
	defer delete(StageOverrides, "extract")
	if _, err := RunMatrix(context.Background(), sp, MatrixOpts{Log: func(string) {}}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "ajx-reports", "p", "t")
	mx := readMapT(t, filepath.Join(out, "matrix.json"))
	var ids []string
	for _, row := range util.Maps(mx["runs"]) {
		ids = append(ids, util.S(row["run_id"]))
	}
	if strings.Join(sortedStrings(ids), ",") != "a-r1,b-r1" {
		t.Fatalf("runs %v", ids)
	}
	if util.Truthy(util.Get(mx, "plan", "wall_clock_comparable")) {
		t.Fatal("parallel matrix marked wall-clock comparable")
	}
	if exists(filepath.Join(out, ".lock")) {
		t.Fatal("lock left behind")
	}
	plan := readMapT(t, filepath.Join(out, "matrix-plan.json"))
	if util.Int(plan["parallel"], 0) != 2 || plan["wall_clock_comparable"] != false || len(util.List(plan["order"])) != 2 {
		t.Fatalf("matrix-plan %v", plan)
	}
}

func sortedStrings(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestMatrixDryRunUnknownStageAndLock(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	sp := loadSpec(t, writeTrial(t, tmp, "", ""))
	meta, err := RunMatrix(context.Background(), sp, MatrixOpts{DryRun: true, Log: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	if got := util.Strings(meta["order"]); len(got) != 1 || got[0] != "f-r1" {
		t.Fatalf("order %v", got)
	}
	out := filepath.Join(tmp, "ajx-reports", "p", "t")
	if !exists(filepath.Join(out, "matrix-plan.json")) || !exists(filepath.Join(out, "task-prompt.md")) {
		t.Fatal("dry run did not write the plan")
	}
	if exists(filepath.Join(out, "runs")) {
		t.Fatal("dry run created attempts")
	}
	_, err = RunMatrix(context.Background(), sp, MatrixOpts{Stages: []string{"rendr"}})
	var se *spec.Error
	if !errors.As(err, &se) || err.Error() != "unknown stage(s) ['rendr']; stages are ['prepare', 'execute', 'verify', "+
		"'teardown', 'normalize', 'narrate', 'release', 'extract', 'measure', 'render', 'archive']" {
		t.Fatalf("unknown stage: %v", err)
	}
	lock, err := AcquireLock(out)
	if err != nil {
		t.Fatal(err)
	}
	_, err = RunMatrix(context.Background(), sp, MatrixOpts{Log: func(string) {}})
	var le *LockedError
	if !errors.As(err, &le) {
		t.Fatalf("want LockedError, got %v", err)
	}
	ReleaseLock(lock)
}

func TestMatrixRefusesChangedSettingsInTheSameFolder(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	path := writeTrial(t, tmp, "", "")
	sp := loadSpec(t, path)
	StageOverrides["extract"] = noModelExtract
	defer delete(StageOverrides, "extract")
	if _, err := RunMatrix(context.Background(), sp, MatrixOpts{Stages: []string{"prepare", "execute"}, Log: func(string) {}}); err != nil {
		t.Fatal(err)
	}
	// Reloaded unchanged: resume is allowed (saved numbers round-trip through JSON).
	if _, err := RunMatrix(context.Background(), loadSpec(t, path), MatrixOpts{Stages: []string{"verify"}, Log: func(string) {}}); err != nil {
		t.Fatalf("unchanged resume refused: %v", err)
	}
	text := strings.Replace(readFile(t, path), "product = \"p\"\n", "product = \"p\"\ntimeout_seconds = 30\n", 1)
	_ = os.WriteFile(path, []byte(text), 0o644)
	_, err := RunMatrix(context.Background(), loadSpec(t, path), MatrixOpts{Log: func(string) {}})
	if err == nil || !strings.Contains(err.Error(), "this output folder holds attempts made with other settings (limits timeout_seconds: ") ||
		!strings.Contains(err.Error(), " -> 30); use a new trial output directory") {
		t.Fatalf("changed settings: %v", err)
	}
}

func TestPreflightFailureStopsTheRunBeforeTheWorker(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	sp := loadSpec(t, writeTrial(t, tmp, "[[preflight]]\nname = \"creds\"\nrun = \"echo Unable to locate credentials >&2; exit 253\"\n", ""))
	r := newRun(t, sp, nil)
	state, err := r.Go(context.Background(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status("prepare") != "error" || !strings.Contains(util.S(util.Get(state, "stages", "prepare", "error")),
		"preflight 'creds' failed (exit 253") {
		t.Fatalf("%v", util.Get(state, "stages", "prepare"))
	}
	if _, ok := util.MapOr(state["stages"])["execute"]; ok || exists(filepath.Join(r.RunDir, "execute.raw.jsonl")) {
		t.Fatal("worker started")
	}
	if util.Truthy(readListT(t, filepath.Join(r.RunDir, "preflight.json"))[0]["passed"]) {
		t.Fatal("preflight passed")
	}
	if left := dirEntries(t, filepath.Join(tmp, "ws")); len(left) != 0 {
		t.Fatalf("not archived: %v", left)
	}
}

func TestRerunAfterFailedPreflightUsesAFreshWorkspace(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	marker := filepath.Join(tmp, "creds-fixed")
	sp := loadSpec(t, writeTrial(t, tmp, fmt.Sprintf("[[preflight]]\nrun = \"test -f %s\"\n", marker), ""))
	first := newRun(t, sp, nil)
	if _, err := first.Go(context.Background(), nil, false); err != nil || first.Status("prepare") != "error" {
		t.Fatalf("%v %v", err, statuses(first.State))
	}
	if !util.Truthy(first.State["archived"]) {
		t.Fatal("failed prepare not archived")
	}
	_ = os.WriteFile(marker, []byte("ok"), 0o644) // the user fixes the credentials and reruns
	r := newRun(t, sp, nil)
	stubExtract(r)
	var seen []string
	r.Harness.(*fakeHarness).onExec = func(ctx *plugin.Ctx) error { seen = append(seen, ctx.Workspace); return nil }
	state, err := r.Go(context.Background(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status("execute") != "done" || len(seen) != 1 || seen[0] != util.S(util.Get(state, "paths", "workspace")) {
		t.Fatalf("%v seen=%v", statuses(state), seen)
	}
	if strings.Contains(seen[0], r.RunDir) {
		t.Fatal("worker ran inside the report dir")
	}
	if r.Status("archive") != "done" || len(dirEntries(t, filepath.Join(tmp, "ws"))) != 0 {
		t.Fatal("retry not archived")
	}
	if !util.Truthy(util.Get(state, "execute", "reaped")) {
		t.Fatal("execute not reaped")
	}
}

func TestFailingTeardownCommandIsNotOk(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	sp := loadSpec(t, writeTrial(t, tmp, "[[teardown]]\nrun = \"exit 4\"\n", ""))
	var logs []string
	r := newRun(t, sp, func(m string) { logs = append(logs, m) })
	stubExtract(r)
	state, err := r.Go(context.Background(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status("teardown") != "done" || util.Get(state, "teardown", "status") != "failed" ||
		util.Compact(util.Get(state, "teardown", "failed")) != "[0]" {
		t.Fatalf("%v %v", statuses(state), state["teardown"])
	}
	want := "[f-r1] teardown failed: resources the task created may still exist; see teardown.json, then " +
		"`ajx run <trial> --cells f --stages teardown,render`"
	if !util.Contains(logs, want) {
		t.Fatalf("logs %q", logs)
	}
}

func TestTeardownRedoneAfterArchiveClearsTheProblem(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	marker := filepath.Join(tmp, "creds-fixed")
	sp := loadSpec(t, writeTrial(t, tmp, fmt.Sprintf("[[teardown]]\nrun = \"test -f %s || { echo Unable to locate credentials >&2; exit 253; }\"\n", marker), ""))
	r := newRun(t, sp, nil)
	stubExtract(r)
	if _, err := r.Go(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	if util.Get(r.State, "teardown", "status") != "auth_errors" || !util.Truthy(r.State["archived"]) {
		t.Fatalf("%v", r.State["teardown"])
	}
	if !strings.Contains(readFile(t, filepath.Join(r.RunDir, "report.html")), "Cleanup not confirmed") {
		t.Fatal("report lacks cleanup banner")
	}
	_ = os.WriteFile(marker, []byte("ok"), 0o644)
	again := newRun(t, sp, nil)
	if _, err := again.Go(context.Background(), []string{"teardown", "render"}, false); err != nil {
		t.Fatal(err)
	}
	if util.Get(again.State, "teardown", "status") != "ok" ||
		util.Get(readMapT(t, filepath.Join(again.RunDir, "run.json")), "teardown", "status") != "ok" {
		t.Fatalf("%v", again.State["teardown"])
	}
	if strings.Contains(readFile(t, filepath.Join(again.RunDir, "report.html")), "Cleanup not confirmed") {
		t.Fatal("banner still shown")
	}
}

func TestTeardownAndVerifyCredentialErrorsBehindExitZeroAreSurfaced(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	sp := loadSpec(t, writeTrial(t, tmp, "[[preflight]]\nrun = \"true\"\n"+
		"[[verify]]\nname = \"nothing left\"\nrun = 'echo \"An error occurred (ExpiredToken)\" >&2; true'\n"+
		"[[teardown]]\nrun = 'set +e; echo \"aws: [ERROR]: An error occurred (NoCredentials): Unable to locate credentials.\" >&2; true'\n", ""))
	r := newRun(t, sp, nil)
	stubExtract(r)
	state, err := r.Go(context.Background(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range statuses(state) {
		if v != "done" {
			t.Fatalf("%s = %s", k, v)
		}
	}
	if util.Get(state, "teardown", "status") != "auth_errors" ||
		util.Int(readListT(t, filepath.Join(r.RunDir, "teardown.json"))[0]["exit_code"], -1) != 0 {
		t.Fatalf("teardown %v", state["teardown"])
	}
	if !util.Truthy(readListT(t, filepath.Join(r.RunDir, "preflight.json"))[0]["passed"]) {
		t.Fatal("preflight")
	}
	verify := readMapT(t, filepath.Join(r.RunDir, "verify.json"))
	if verify["outcome"] != "succeeded" || util.Compact(verify["checks_with_auth_errors"]) != `["nothing left"]` {
		t.Fatalf("verify %v %v", verify["outcome"], verify["checks_with_auth_errors"])
	}
	limits := strings.Join(util.Strings(readMapT(t, filepath.Join(r.RunDir, "run.json"))["limitations"]), " ")
	for _, want := range []string{"cleanup: teardown printed credential or permission errors",
		"verify: 1 check(s) printed credential or permission errors"} {
		if !strings.Contains(limits, want) {
			t.Fatalf("limitations lack %q: %s", want, limits)
		}
	}
}

// ---------------------------------------------------------------- real interruption

func slowTrial(t *testing.T, tmp, extra string) M {
	return loadSpec(t, writeTrial(t, tmp, "[[teardown]]\nrun = \"echo td-ran\"\n"+extra,
		"[[cells]]\nid = \"f\"\nharness = \"fake-slow\"\n"))
}

func TestCancelDuringExecuteStopsWorkerTearsDownAndArchives(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	sp := slowTrial(t, tmp, "")
	r := newRun(t, sp, nil)
	stubExtract(r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var workerPid, childPid int
	pids := make(chan [2]int, 1)
	go func() {
		ws := waitWorkspaceFile(filepath.Join(tmp, "ws"), "child.pid", 20*time.Second)
		var w, c int
		fmt.Sscan(readFileQuiet(filepath.Join(ws, "worker.pid")), &w)
		fmt.Sscan(readFileQuiet(filepath.Join(ws, "child.pid")), &c)
		pids <- [2]int{w, c}
		cancel()
	}()
	start := time.Now()
	state, err := r.Go(ctx, nil, false)
	if !util.Interrupted(err) {
		t.Fatalf("want interruption, got %v", err)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatalf("interruption took %s", time.Since(start))
	}
	got := <-pids
	workerPid, childPid = got[0], got[1]
	if workerPid == 0 || childPid == 0 {
		t.Fatal("worker never started")
	}
	if !waitDead(workerPid, 5*time.Second) || !waitDead(childPid, 5*time.Second) {
		t.Fatal("worker process group survived the interruption")
	}
	if r.Status("execute") != "interrupted" || util.Get(state, "execute", "stop_reason") != "interrupted" ||
		util.Get(state, "execute", "interrupted") != true || !util.Truthy(util.Get(state, "execute", "stopped_at")) {
		t.Fatalf("execute %v %v", statuses(state), state["execute"])
	}
	if note := util.Get(state, "stages", "execute", "note"); note != "execution interrupted; keeping captured evidence" {
		t.Fatalf("note %v", note)
	}
	if r.Status("teardown") != "done" || r.Status("archive") != "done" {
		t.Fatalf("cleanup %v", statuses(state))
	}
	steps := util.MapOr(util.Get(state, "interruption_cleanup", "steps"))
	for _, s := range []string{"stop_processes", "teardown", "archive"} {
		if util.Get(steps, s, "status") != "done" {
			t.Fatalf("step %s: %v", s, steps)
		}
	}
	if _, ok := steps["credentials"]; ok {
		t.Fatal("credentials step ran although archive is done")
	}
	if util.Get(state, "interruption_cleanup", "stage") != "execute" {
		t.Fatal("cleanup stage")
	}
	if left := dirEntries(t, filepath.Join(tmp, "ws")); len(left) != 0 {
		t.Fatalf("not archived: %v", left)
	}
	if !strings.Contains(readFile(t, filepath.Join(r.RunDir, "execute.raw.jsonl")), "started") {
		t.Fatal("captured evidence lost")
	}
	// Resume: execute is never retried; post-processing finishes the report.
	r2 := newRun(t, sp, nil)
	stubExtract(r2)
	r2.Harness.(*fakeHarness).onExec = func(*plugin.Ctx) error { t.Error("task re-ran on resume"); return nil }
	if _, err := r2.Go(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	if r2.Status("execute") != "interrupted" || r2.Status("render") != "done" || r2.Status("narrate") != "done" {
		t.Fatalf("resume %v", statuses(r2.State))
	}
}

// waitWorkspaceFile finds root/<random>/name once it is written (the run's
// state is not read concurrently).
func waitWorkspaceFile(root, name string, d time.Duration) string {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(root, "*", name))
		for _, m := range matches {
			if waitFile(m, 0) {
				return filepath.Dir(m)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ""
}

func readFileQuiet(p string) string {
	data, _ := os.ReadFile(p)
	return string(data)
}

func TestSecondSignalInterruptsOnlyTheCurrentCleanupStep(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	started := filepath.Join(tmp, "teardown-started")
	sp := loadSpec(t, writeTrial(t, tmp, fmt.Sprintf("[[teardown]]\nrun = \"echo go > %s; sleep 30\"\n", started),
		"[[cells]]\nid = \"f\"\nharness = \"fake-slow\"\n"))
	signals := make(chan os.Signal, 1)
	orig := interruptSignals
	interruptSignals = func() (<-chan os.Signal, func()) { return signals, func() {} }
	defer func() { interruptSignals = orig }()
	r := newRun(t, sp, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		waitWorkspaceFile(filepath.Join(tmp, "ws"), "child.pid", 20*time.Second)
		cancel()
		if waitFile(started, 20*time.Second) {
			signals <- syscall.SIGTERM
		}
	}()
	start := time.Now()
	state, err := r.Go(ctx, nil, false)
	if !util.Interrupted(err) {
		t.Fatalf("want interruption, got %v", err)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("second signal did not stop the teardown command (%s)", time.Since(start))
	}
	if r.Status("teardown") != "interrupted" || util.Get(state, "stages", "teardown", "error") != "KeyboardInterrupt: signal 15" {
		t.Fatalf("teardown %v", util.Get(state, "stages", "teardown"))
	}
	if util.Get(state, "interruption_cleanup", "steps", "teardown", "status") != "interrupted" {
		t.Fatalf("steps %v", util.Get(state, "interruption_cleanup", "steps"))
	}
	if r.Status("archive") != "done" {
		t.Fatalf("cleanup did not continue after the interrupted step: %v", statuses(state))
	}
}

func TestTimeoutKillsWorkerAndRecordsStopReason(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	path := writeTrial(t, tmp, "", "[[cells]]\nid = \"f\"\nharness = \"fake-slow\"\n")
	text := strings.Replace(readFile(t, path), "product = \"p\"\n", "product = \"p\"\ntimeout_seconds = 1\n", 1)
	_ = os.WriteFile(path, []byte(text), 0o644)
	sp := loadSpec(t, path)
	r := newRun(t, sp, nil)
	if _, err := r.Go(context.Background(), []string{"prepare", "execute", "teardown"}, false); err != nil {
		t.Fatal(err)
	}
	if r.Status("execute") != "done" || util.Get(r.State, "execute", "stop_reason") != "timeout" ||
		util.Get(r.State, "execute", "timed_out") != true {
		t.Fatalf("execute %v", r.State["execute"])
	}
	ws := util.S(util.Get(r.State, "paths", "workspace"))
	if !waitDead(pidFrom(t, filepath.Join(ws, "child.pid")), 8*time.Second) {
		t.Fatal("timed out worker's child survived")
	}
}

func TestTeardownReapsServersLeftForVerificationOnce(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	sp := loadSpec(t, writeTrial(t, tmp, "", "[[cells]]\nid = \"f\"\nharness = \"fake-server\"\n"))
	r := newRun(t, sp, nil)
	if _, err := r.Go(context.Background(), []string{"prepare", "execute"}, false); err != nil {
		t.Fatal(err)
	}
	ws := util.S(util.Get(r.State, "paths", "workspace"))
	server := pidFrom(t, filepath.Join(ws, "server.pid"))
	if !PidAlive(server) {
		t.Fatal("server started by the worker was not kept alive for verification")
	}
	if _, err := r.Go(context.Background(), []string{"verify", "teardown"}, false); err != nil {
		t.Fatal(err)
	}
	if !waitDead(server, 5*time.Second) {
		t.Fatal("teardown did not reap the worker's process group")
	}
	if util.Get(r.State, "execute", "reaped") != true {
		t.Fatal("reaped not recorded")
	}
}

func TestMatrixCancelledBeforeStartCreatesNoAttempts(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	sp := loadSpec(t, writeTrial(t, tmp, "", ""))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := RunMatrix(ctx, sp, MatrixOpts{Log: func(string) {}})
	if !util.Interrupted(err) {
		t.Fatalf("want interruption, got %v", err)
	}
	out := filepath.Join(tmp, "ajx-reports", "p", "t")
	if exists(filepath.Join(out, "runs", "f-r1", "state.json")) {
		t.Fatal("an attempt started after the interruption")
	}
	if exists(filepath.Join(out, ".lock")) {
		t.Fatal("lock left behind")
	}
}

func TestParallelMatrixInterruptionCleansUpEveryStartedRun(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	path := writeTrial(t, tmp, "[[teardown]]\nrun = \"echo td\"\n",
		"[[cells]]\nid = \"a\"\nharness = \"fake-slow\"\n[[cells]]\nid = \"b\"\nharness = \"fake-slow\"\n")
	text := strings.Replace(readFile(t, path), "product = \"p\"\n", "product = \"p\"\nparallel = 2\n", 1)
	_ = os.WriteFile(path, []byte(text), 0o644)
	sp := loadSpec(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var logs []string
	go func() {
		out := filepath.Join(tmp, "ajx-reports", "p", "t", "runs")
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			n := 0
			for _, id := range []string{"a-r1", "b-r1"} {
				st := util.ReadMap(filepath.Join(out, id, "state.json"))
				if util.Get(st, "stages", "execute", "status") == "running" {
					n++
				}
			}
			if n == 2 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	_, err := RunMatrix(ctx, sp, MatrixOpts{Log: func(m string) { mu.Lock(); logs = append(logs, m); mu.Unlock() }})
	if !util.Interrupted(err) {
		t.Fatalf("want interruption, got %v", err)
	}
	for _, id := range []string{"a-r1", "b-r1"} {
		st := readMapT(t, filepath.Join(tmp, "ajx-reports", "p", "t", "runs", id, "state.json"))
		if util.Get(st, "stages", "execute", "status") != "interrupted" || util.Get(st, "stages", "archive", "status") != "done" {
			t.Fatalf("%s: %v", id, statuses(st))
		}
	}
	if left := dirEntries(t, filepath.Join(tmp, "ws")); len(left) != 0 {
		t.Fatalf("not archived: %v", left)
	}
}
