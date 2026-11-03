package console

// Test support. Everything is synthetic: the test binary doubles as a fake
// `ajx` CLI (AJX_BIN points at a symlink named "ajx", so process checks still
// see an ajx command), its `run` writes scripted run records, and no agent,
// model or network is used.

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/royosherove/ajx/internal/planedit"
	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/runner"
	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

const fakeEnv = "AJX_CONSOLE_TEST_FAKE_CLI"

// demoHarness stands in for the console demo's simulated worker: it reports
// live progress from execute.raw.jsonl like the real adapters do.
type demoHarness struct{ plugin.BaseHarness }

func (h *demoHarness) Available() bool { return true }

func (h *demoHarness) Progress(runDir, stage string, limit int) []M {
	var out []M
	for _, rec := range plugin.StreamRecords(filepath.Join(runDir, stage+".raw.jsonl")) {
		ev := util.MapOr(rec.Parsed)
		text := util.S(ev["text"])
		if ev["kind"] == "tool" {
			text = util.S(ev["command"])
		}
		out = append(out, M{"at": rec.At, "kind": ev["kind"], "text": text, "error": util.Truthy(ev["error"])})
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// plainHarness inherits Progress (reports nothing live).
type plainHarness struct{ plugin.BaseHarness }

var usingFallbackLoader bool

func setupDeps() {
	if !plugin.Has("harness", "console-demo") {
		plugin.RegisterHarness("console-demo", "test", func() plugin.Harness { return &demoHarness{} })
	}
	if !plugin.Has("harness", "plain-test") {
		plugin.RegisterHarness("plain-test", "test", func() plugin.Harness { return &plainHarness{} })
	}
	// Use the real trial loader when it can load the demo; otherwise a small
	// synthetic one with the same record shape (while spec is being ported).
	dir, err := os.MkdirTemp("", "ajx-console-probe")
	if err != nil {
		return
	}
	defer os.RemoveAll(dir)
	trial, err := CopyDemo(filepath.Join(dir, "demo"), false)
	if err != nil {
		return
	}
	if sp, err := spec.Load(trial, spec.LoadOpts{SkipReadiness: true}); err == nil && sp != nil {
		if _, err := spec.RunPlan(sp, nil); err == nil {
			return
		}
	}
	usingFallbackLoader = true
	if os.Getenv(fakeEnv) != "1" {
		fmt.Fprintln(os.Stderr, "console tests: spec.Load cannot load the demo trial yet; using the synthetic loader")
	}
	specLoad = fallbackLoad
	specHarnessFor = func(sp M, name string) (plugin.Harness, error) { return plugin.NewHarness(name) }
	specAuthFor = func(sp, cell M) (plugin.Auth, error) {
		a := plugin.NewAuthBase(nil)
		a.Profile = "inherit"
		return a, nil
	}
	specEnvironmentFor = func(sp, cell M) M { return nil }
	specRunPlan = func(sp M) ([]spec.PlanItem, error) {
		var out []spec.PlanItem
		reps := util.Int(util.Get(sp, "trial", "repetitions"), 1)
		for r := 1; r <= reps; r++ {
			for _, c := range util.Maps(sp["cells"]) {
				out = append(out, spec.PlanItem{Cell: c, Rep: r})
			}
		}
		return out, nil
	}
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// fallbackLoad is a synthetic trial loader with spec.Load's record shape for
// the keys the console reads.
func fallbackLoad(path string) (M, error) {
	path = resolvePath(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, spec.Errorf("trial file not found: %s", path)
	}
	raw, err := parseTOML(string(data))
	if err != nil {
		return nil, spec.Errorf("%v", err)
	}
	base := filepath.Dir(path)
	plugin.Load(base)
	trial := util.Copy(util.MapOr(raw["trial"]))
	if !slugRe.MatchString(pstr(trial["id"])) {
		return nil, spec.Errorf("trial.id must be a short lowercase slug")
	}
	defaults := M{"workspace_root": "/tmp", "repetitions": int64(1), "parallel": int64(1), "timeout_seconds": int64(1800),
		"max_budget_usd": nil, "cache": "isolated", "narrate_timeout_seconds": int64(900), "shell_startup": "inherit"}
	for k, v := range defaults {
		if _, ok := trial[k]; !ok {
			trial[k] = v
		}
	}
	if s := pstr(trial["shell_startup"]); s != "inherit" && s != "skip" {
		return nil, spec.Errorf("trial.shell_startup must be inherit|skip")
	}
	if _, ok := trial["output_dir"]; !ok {
		trial["output_dir"] = filepath.Join(base, "ajx-reports", pstr(trial["product"]), pstr(trial["id"]))
	}
	od := pstr(trial["output_dir"])
	if !filepath.IsAbs(od) {
		od = filepath.Join(base, od)
	}
	trial["output_dir"] = resolvePath(od)
	trial["workspace_root"] = resolvePath(expandUser(pstr(trial["workspace_root"])))
	task := util.Copy(util.MapOr(raw["task"]))
	pp := resolvePath(filepath.Join(base, pstr(task["prompt_file"])))
	prompt, err := os.ReadFile(pp)
	if err != nil {
		return nil, spec.Errorf("task prompt not found: %s", pp)
	}
	task["prompt_path"], task["prompt_sha256"] = pp, sha256Hex(prompt)
	if _, ok := task["materials"]; !ok {
		task["materials"] = []any{}
	}
	if _, ok := task["human"]; !ok {
		task["human"] = "unavailable"
	}
	if util.Truthy(task["fixture_dir"]) {
		task["fixture_dir"] = resolvePath(filepath.Join(base, pstr(task["fixture_dir"])))
	} else {
		task["fixture_dir"] = nil
	}
	sp := M{"path": path, "trial": trial, "task": task, "environments": M{}, "agent_configurations": M{}}
	for _, key := range []string{"verify", "setup", "teardown", "preflight"} {
		var items []any
		for _, it := range util.Maps(raw[key]) {
			e := util.Copy(it)
			if _, ok := e["type"]; !ok {
				e["type"] = "shell"
			}
			if _, ok := e["name"]; !ok {
				e["name"] = util.Clip(pstr(e["run"]), 60)
			}
			if _, ok := e["location"]; !ok {
				e["location"] = "environment"
				if key == "verify" {
					e["location"] = "host"
				}
			}
			items = append(items, e)
		}
		sp[key] = listOr(items)
	}
	var cells []any
	for _, c := range util.Maps(raw["cells"]) {
		cell := util.Copy(c)
		if !plugin.Has("harness", pstr(cell["harness"])) {
			return nil, spec.Errorf("no harness plugin named %q", pstr(cell["harness"]))
		}
		for _, k := range []string{"model", "effort", "config", "auth", "args", "runner", "environment",
			"agent_configuration", "shell_startup"} {
			if _, ok := cell[k]; !ok {
				cell[k] = nil
			}
		}
		cells = append(cells, cell)
	}
	sp["cells"] = listOr(cells)
	rep := util.Copy(util.MapOr(raw["reporter"]))
	for _, k := range []string{"model", "effort", "auth"} {
		if _, ok := rep[k]; !ok {
			rep[k] = nil
		}
	}
	sp["reporter"] = rep
	return sp, nil
}

func TestMain(m *testing.M) {
	planedit.StubDocker(nil, "") // never ask Docker in tests
	setupDeps()
	if os.Getenv(fakeEnv) == "1" {
		os.Exit(fakeCLI(os.Args[1:]))
	}
	dir, err := os.MkdirTemp("", "ajx-console-bin")
	if err != nil {
		panic(err)
	}
	exe, _ := os.Executable()
	bin := filepath.Join(dir, "ajx")
	if err := os.Symlink(exe, bin); err != nil {
		panic(err)
	}
	os.Setenv("AJX_BIN", bin)
	os.Setenv(fakeEnv, "1")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// ---------------------------------------------------------------- fake ajx CLI

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func fakeCLI(args []string) int {
	if len(args) == 0 {
		return 2
	}
	switch args[0] {
	case "validate":
		fmt.Println("trial file ok")
		for _, line := range strings.Split(os.Getenv("AJX_FAKE_VALIDATE"), "|") {
			if line != "" {
				fmt.Println(line)
			}
		}
		return 0
	case "doctor":
		fmt.Println("doctor: ready")
		for _, line := range strings.Split(os.Getenv("AJX_FAKE_DOCTOR"), "|") {
			if line != "" {
				fmt.Println(line)
			}
		}
		return 0
	case "report":
		fmt.Println("reports rebuilt")
		return 0
	case "export":
		trial, _ := TrialFile(args[1])
		sp, _ := load(trial)
		dest := filepath.Join(outputDir(trial, sp), "share", flagValue(args, "--run"))
		_ = os.MkdirAll(dest, 0o755)
		for _, n := range []string{"report.html", "journey.html", "asks.md"} {
			_ = os.WriteFile(filepath.Join(dest, n), []byte("shared "+n), 0o644)
		}
		fmt.Println("exported to " + dest)
		return 0
	case "run":
		return fakeRun(args[1:])
	case "console":
		if len(args) >= 3 && args[1] == "serve" {
			port, _ := strconv.Atoi(flagValue(args, "--port"))
			if err := Serve(args[2], ServeOpts{Port: port, Announce: func(u string) { fmt.Println("serving " + u) }}); err != nil {
				fmt.Fprintln(os.Stderr, "console:", err)
				return 2
			}
			return 0
		}
	}
	fmt.Fprintln(os.Stderr, "fake ajx: unsupported", args)
	return 2
}

func pace() time.Duration {
	f, err := strconv.ParseFloat(os.Getenv("AJX_DEMO_PACE"), 64)
	if err != nil {
		f = 0
	}
	return time.Duration(f * float64(time.Second) / 4)
}

// fakeRun simulates `ajx run`: every pending planned attempt goes through the
// lifecycle stages, writing the same records the runner writes. SIGINT/SIGTERM
// during execute takes the handled interruption (exit 130).
func fakeRun(args []string) int {
	trialArg := args[0]
	trial, err := TrialFile(trialArg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	sp, loadErr := load(trial)
	if loadErr != "" {
		fmt.Fprintln(os.Stderr, "trial file error: "+loadErr)
		return 2
	}
	if expected := flagValue(args, "--expect-plan"); expected != "" && planSha(trial, sp) != expected {
		fmt.Fprintln(os.Stderr, "trial file error: the trial changed after it was approved (trial.toml, the task prompt, "+
			"the starting files or the trial's plugins); review it and approve it again")
		return 2
	}
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	out := outputDir(trial, sp)
	_ = os.MkdirAll(out, 0o755)
	lock := filepath.Join(out, ".lock")
	_ = util.WriteJSON(lock, M{"pid": os.Getpid(), "host": hostname(), "at": util.Now()})
	defer os.Remove(lock)
	var order []any
	for _, id := range plannedIDs(sp) {
		order = append(order, id)
	}
	_ = util.WriteJSON(filepath.Join(out, "matrix-plan.json"), M{"order": order,
		"task_prompt_sha256": util.Get(sp, "task", "prompt_sha256"), "cells_contract": runner.CellsContract(sp)})
	only := flagValue(args, "--cells")
	stageArg := flagValue(args, "--stages")
	for _, rid := range plannedIDs(sp) {
		if only != "" && !strings.HasPrefix(rid, only+"-r") {
			continue
		}
		d := filepath.Join(out, "runs", rid)
		st := readMap(filepath.Join(d, "state.json"))
		marks := util.MapOr(st["stages"])
		if stageArg == "" && util.Get(marks, "render", "status") == "done" {
			continue
		}
		st["stages"] = marks
		save := func() { _ = util.WriteJSON(filepath.Join(d, "state.json"), st) }
		mark := func(stage, status string) {
			marks[stage] = M{"status": status, "at": util.Now()}
			save()
		}
		_ = os.MkdirAll(d, 0o755)
		if stageArg != "" {
			for _, s := range strings.Split(stageArg, ",") {
				mark(s, "done")
			}
			continue
		}
		if util.Get(marks, "execute", "status") == "interrupted" {
			// A resume never re-runs an interrupted worker; it finishes the rest of the lifecycle.
			_ = util.WriteJSON(filepath.Join(d, "verify.json"), M{"outcome": "failed", "passed": 0, "total": 1, "checks": []any{}})
			for _, s := range []string{"verify", "normalize", "narrate", "release", "extract", "measure"} {
				mark(s, "done")
			}
			_ = util.WriteJSON(filepath.Join(d, "run.json"), M{"review_status": "incomplete", "agent": M{"harness_name": "console-demo"}})
			mark("render", "done")
			continue
		}
		mark("prepare", "done")
		started := util.Now()
		st["execute"] = M{"started_at": started, "requested": M{"harness": util.Get(cellForRun(sp, rid), "harness"),
			"model": util.Get(cellForRun(sp, rid), "model")}}
		mark("execute", "running")
		raw, _ := os.Create(filepath.Join(d, "execute.raw.jsonl"))
		w := bufio.NewWriter(raw)
		for i, text := range []string{"I'll start with the tool's built-in help.", "Reading the input.", "Created out.csv."} {
			select {
			case <-sigs:
				// The handled interruption: stop the worker, run task cleanup, archive.
				w.Flush()
				raw.Close()
				ex := util.MapOr(st["execute"])
				ex["interrupted"], ex["stopped_at"], ex["stop_reason"] = true, util.Now(), "interrupted"
				mark("execute", "interrupted")
				time.Sleep(1500 * time.Millisecond)
				st["teardown"] = M{"status": "ok"}
				mark("teardown", "done")
				mark("archive", "done")
				return 130
			case <-time.After(pace()):
			}
			fmt.Fprintln(w, util.Compact(M{"at": util.Now(), "line": util.Compact(M{"kind": "text", "text": text, "n": i})}))
			w.Flush()
		}
		raw.Close()
		ex := util.MapOr(st["execute"])
		ex["exit_code"], ex["stopped_at"], ex["stop_reason"] = 0, util.Now(), "end_turn"
		mark("execute", "done")
		_ = util.WriteJSON(filepath.Join(d, "verify.json"), M{"outcome": "succeeded", "passed": 1, "total": 1,
			"checks": []any{M{"name": "out.csv", "passed": true, "detail": "ok"}}})
		mark("verify", "done")
		st["teardown"] = M{"status": "ok"}
		for _, s := range []string{"teardown", "normalize", "narrate", "release"} {
			mark(s, "done")
		}
		asks := []any{}
		for i := 1; i <= 3; i++ {
			asks = append(asks, M{"id": fmt.Sprintf("A-%d", i), "title": fmt.Sprintf("ask %d", i), "labels": []any{"Roadblock"}})
		}
		_ = util.WriteJSON(filepath.Join(d, "asks.json"), M{"extraction_status": "ok", "asks": asks})
		mark("extract", "done")
		mark("measure", "done")
		_ = util.WriteJSON(filepath.Join(d, "run.json"), M{"review_status": "ready", "agent": M{"harness_name": "console-demo"}})
		_ = os.WriteFile(filepath.Join(d, "report.html"), []byte("<p>report</p>"), 0o644)
		mark("render", "done")
		mark("archive", "done")
	}
	_ = os.WriteFile(filepath.Join(out, "index.html"), []byte("<p>index</p>"), 0o644)
	return 0
}

// ---------------------------------------------------------------- helpers

type demo struct {
	t     *testing.T
	tmp   string
	trial string
}

func newDemo(t *testing.T, pace string) *demo {
	t.Helper()
	tmp := resolvePath(t.TempDir())
	t.Setenv("AJX_DEMO_PACE", pace)
	trial, err := CopyDemo(filepath.Join(tmp, "demo"), false)
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(tmp, "ws")
	_ = os.Mkdir(ws, 0o755)
	data, _ := os.ReadFile(trial)
	text, err := SetValue(string(data), "trial", "workspace_root", ws, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trial, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return &demo{t: t, tmp: tmp, trial: trial}
}

func (d *demo) out() string {
	sp, _ := load(d.trial)
	return outputDir(d.trial, sp)
}

func (d *demo) spec() M {
	sp, err := load(d.trial)
	if err != "" {
		d.t.Fatal(err)
	}
	return sp
}

func (d *demo) kinds() []string {
	entries, _ := readEntries(d.trial, 0)
	var out []string
	for _, e := range entries {
		out = append(out, util.S(e["kind"]))
	}
	return out
}

func (d *demo) text() string {
	data, _ := os.ReadFile(d.trial)
	return string(data)
}

func (d *demo) write(text string) {
	if err := os.WriteFile(d.trial, []byte(text), 0o644); err != nil {
		d.t.Fatal(err)
	}
}

func (d *demo) set(table, key string, value any, index int) {
	text, err := SetValue(d.text(), table, key, value, index)
	if err != nil {
		d.t.Fatal(err)
	}
	d.write(text)
}

func (d *demo) sha() string {
	data, _ := os.ReadFile(d.trial)
	return sha256Hex(data)
}

func (d *demo) snap() M {
	s, err := Snapshot(d.trial, SnapshotOpts{})
	if err != nil {
		d.t.Fatal(err)
	}
	return s
}

func until(t *testing.T, timeout time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}

func isConflict(err error) bool {
	var ce *Error
	return errors.As(err, &ce) && ce.Status == 409
}

func isConsoleErr(err error) bool {
	var ce *Error
	return errors.As(err, &ce)
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	if err := util.WriteJSON(path, v); err != nil {
		t.Fatal(err)
	}
}
