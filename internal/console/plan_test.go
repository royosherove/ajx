package console

// Ports SavePlanTest (skills/ajx/tests/test_plan_edit.py) and PlanApiTest
// (skills/ajx/tests/test_console.py): the Plan tab's preview and save, the
// output-folder locks, and the worker's files served from its workspace.

import (
	"bytes"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/planedit"
	"github.com/royosherove/ajx/internal/runner"
	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

func (d *demo) model() *planedit.Dict {
	d.t.Helper()
	m, err := planedit.ReadModel(d.trial)
	if err != nil {
		d.t.Fatal(err)
	}
	return m
}

func (d *demo) promptPath() string { return filepath.Join(filepath.Dir(d.trial), "task-prompt.md") }

func (d *demo) promptSha() string {
	data, _ := os.ReadFile(d.promptPath())
	return sha256Hex(data)
}

// recordAttempt makes the output folder hold an attempt with what `ajx run` records in matrix-plan.json.
func (d *demo) recordAttempt() string {
	sp := d.spec()
	out := outputDir(d.trial, sp)
	writeJSONFile(d.t, filepath.Join(out, "runs", "demo-r1", "state.json"), M{"stages": M{"prepare": M{"status": "done"}}})
	writeJSONFile(d.t, filepath.Join(out, "matrix-plan.json"), M{
		"task_prompt_sha256": util.Get(sp, "task", "prompt_sha256"), "cells_contract": runner.CellsContract(sp),
		"normalized_profiles": M{"schema_version": 1, "environments": util.MapOr(sp["environments"]),
			"agent_configurations": util.MapOr(sp["agent_configurations"])},
		"task_inputs": runner.TaskInputs(sp)})
	return out
}

func psub(d *planedit.Dict, key string) *planedit.Dict { return d.Get(key).(*planedit.Dict) }

func pworker(m *planedit.Dict, i int) *planedit.Dict {
	return m.Get("workers").([]any)[i].(*planedit.Dict)
}

func pcheck(m *planedit.Dict, i int) *planedit.Dict {
	return m.Get("checks").([]any)[i].(*planedit.Dict)
}

func preview(t *testing.T, trial string, model any) M {
	t.Helper()
	view, err := savePlan(trial, model, nil, nil, "user", true)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func problems(t *testing.T, trial string, model any) []string {
	t.Helper()
	return util.Strings(preview(t, trial, model)["problems"])
}

func save(trial string, model any, base, promptBase any) (M, error) {
	return savePlan(trial, model, base, promptBase, "user", false)
}

func TestPreviewReportsTheDiffChangesAndProblemsWithoutWriting(t *testing.T) {
	d := newDemo(t, "0")
	before := d.text()
	m := d.model()
	psub(m, "run").Set("minutes", int64(3))
	view := preview(t, d.trial, m)
	if !util.Contains(util.Strings(view["diff"]), "+timeout_seconds = 180") || len(util.List(view["problems"])) != 0 ||
		!strings.Contains(util.S(view["text"]), "timeout_seconds = 180") || d.text() != before {
		t.Fatalf("preview %v", view)
	}
	m.Set("envs", append(m.Get("envs").([]any), planedit.NewDict("id", "box", "kind", "container",
		"image", "sha256:"+strings.Repeat("b", 64), "network", "none")))
	pworker(m, 0).Set("env", "box")
	if p := problems(t, d.trial, m); len(p) == 0 || !strings.Contains(p[0], "fresh HOME") {
		t.Fatalf("problems %v", p)
	}
	psub(m, "run").Set("minutes", "soon")
	if p := problems(t, d.trial, m); len(p) == 0 || !strings.Contains(p[0], "must be a number") {
		t.Fatalf("problems %v", p)
	}
}

func TestSaveWritesTheFileAndThePromptAndRecordsTheChange(t *testing.T) {
	d := newDemo(t, "0")
	m := d.model()
	psub(m, "task").Set("prompt", "Convert orders.json to out.csv with orders-cli.\n")
	pworker(m, 0).Set("model", "demo-model")
	base := d.sha()
	if _, err := save(d.trial, m, base, d.promptSha()); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(d.promptPath()); string(data) != "Convert orders.json to out.csv with orders-cli.\n" {
		t.Fatalf("prompt %q", data)
	}
	if got := util.Maps(d.spec()["cells"])[0]["model"]; got != "demo-model" {
		t.Fatalf("model %v", got)
	}
	entries, _ := readEntries(d.trial, 0)
	entry := last(entries, "plan_changed")
	if entry == nil || !util.Contains(util.Strings(entry["changes"]), "task prompt edited") {
		t.Fatalf("journal %v", entry)
	}
	if !strings.HasPrefix(describe(entry), "Changed the plan: ") {
		t.Fatalf("describe %q", describe(entry))
	}
	if _, err := save(d.trial, m, base, nil); !isConflict(err) {
		t.Fatalf("stale base: %v", err)
	}
	if _, err := save(d.trial, m, nil, nil); !isConsoleErr(err) || !strings.Contains(err.Error(), "which version") {
		t.Fatalf("no base: %v", err)
	}
}

func TestAnOutputFolderWithAttemptsTakesOnlyMoreAttemptsNewWorkersOrAnotherFolder(t *testing.T) {
	d := newDemo(t, "0")
	d.recordAttempt()
	for i, edit := range []func(m *planedit.Dict){
		func(m *planedit.Dict) { pworker(m, 0).Set("model", "other") },
		func(m *planedit.Dict) { pcheck(m, 0).Set("expect_exit", int64(3)) },
		func(m *planedit.Dict) { psub(m, "task").Set("prompt", "A different task.\n") },
		func(m *planedit.Dict) { psub(m, "reporter").Set("model", "demo-model") },
	} {
		m := d.model()
		edit(m)
		if _, err := save(d.trial, m, d.sha(), d.promptSha()); !isConsoleErr(err) || !strings.Contains(err.Error(), "Locked for this output folder") {
			t.Fatalf("edit %d: %v", i, err)
		}
	}
	m := d.model()
	psub(m, "run").Set("attempts", int64(2))
	extra := planedit.DeepCopy(pworker(m, 0)).(*planedit.Dict)
	extra.Set("id", "demo-two")
	m.Set("workers", append(m.Get("workers").([]any), extra))
	if _, err := save(d.trial, m, d.sha(), nil); err != nil {
		t.Fatal(err)
	}
	if got := util.Int(util.Get(d.spec(), "trial", "repetitions"), 0); got != 2 {
		t.Fatalf("repetitions %d", got)
	}
	m = d.model()
	pworker(m, 0).Set("model", "other")
	psub(m, "run").Set("output_dir", "ajx-reports/orders-cli/fresh")
	if _, err := save(d.trial, m, d.sha(), nil); err != nil {
		t.Fatal(err)
	}
	if got := util.Maps(d.spec()["cells"])[0]["model"]; got != "other" {
		t.Fatalf("model %v", got)
	}
}

func TestSwitchingBackToAFolderChecksWhatItsAttemptsRecorded(t *testing.T) {
	d := newDemo(t, "0")
	first := d.recordAttempt()
	m := d.model()
	psub(m, "run").Set("output_dir", "ajx-reports/orders-cli/second")
	if _, err := save(d.trial, m, d.sha(), nil); err != nil {
		t.Fatal(err)
	}
	m = d.model()
	pworker(m, 0).Set("model", "other")
	if _, err := save(d.trial, m, d.sha(), nil); err != nil {
		t.Fatal(err)
	}
	m = d.model()
	rel, _ := filepath.Rel(filepath.Dir(d.trial), first)
	psub(m, "run").Set("output_dir", rel)
	if p := problems(t, d.trial, m); len(p) == 0 || !strings.Contains(p[0], "Locked for this output folder") {
		t.Fatalf("problems %v", p)
	}
}

func TestSwitchingBackToAFolderWithOtherChecksIsLocked(t *testing.T) {
	d := newDemo(t, "0")
	first := d.recordAttempt()
	m := d.model()
	psub(m, "run").Set("output_dir", "ajx-reports/orders-cli/second")
	pcheck(m, 0).Set("run", "test -s out.csv")
	if _, err := save(d.trial, m, d.sha(), nil); err != nil {
		t.Fatal(err)
	}
	m = d.model()
	rel, _ := filepath.Rel(filepath.Dir(d.trial), first)
	psub(m, "run").Set("output_dir", rel)
	if p := problems(t, d.trial, m); len(p) == 0 || !strings.Contains(p[0], "the success checks") {
		t.Fatalf("problems %v", p)
	}
}

func TestAPromptEditWaitsUntilTheTrialFileLoads(t *testing.T) {
	d := newDemo(t, "0")
	d.recordAttempt()
	if err := os.RemoveAll(filepath.Join(filepath.Dir(d.trial), "fixture")); err != nil { // trial.toml no longer loads
		t.Fatal(err)
	}
	m := d.model()
	psub(m, "task").Set("prompt", "A different task.\n")
	if p := problems(t, d.trial, m); len(p) == 0 || !strings.Contains(p[0], "fixture_dir") {
		t.Fatalf("problems %v", p)
	}
}

func TestTheEditorCanRepairAnEmptyPrompt(t *testing.T) {
	d := newDemo(t, "0")
	os.WriteFile(d.promptPath(), nil, 0o644)
	m := d.model()
	psub(m, "task").Set("prompt", "Convert orders.json to out.csv with orders-cli.")
	if _, err := save(d.trial, m, d.sha(), sha256Hex(nil)); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(d.promptPath()); string(data) != "Convert orders.json to out.csv with orders-cli.\n" {
		t.Fatalf("prompt %q", data)
	}
}

func TestACRLFTrialSavesThePromptAndTheFileTogether(t *testing.T) {
	d := newDemo(t, "0")
	d.write(strings.ReplaceAll(d.text(), "\n", "\r\n"))
	base := util.S(util.Get(d.snap(), "trial", "sha256"))
	if base != d.sha() {
		t.Fatalf("snapshot sha %s, file %s", base, d.sha())
	}
	m := d.model()
	psub(m, "task").Set("prompt", "A different task.\n")
	psub(m, "run").Set("minutes", int64(3))
	if _, err := save(d.trial, m, base, d.promptSha()); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(d.promptPath()); string(data) != "A different task.\n" {
		t.Fatalf("prompt %q", data)
	}
	if got := util.Int(util.Get(d.spec(), "trial", "timeout_seconds"), 0); got != 180 {
		t.Fatalf("timeout %d", got)
	}
}

func TestAFailedTrialWritePutsTheOldPromptBack(t *testing.T) {
	d := newDemo(t, "0")
	before, _ := os.ReadFile(d.promptPath())
	m := d.model()
	psub(m, "task").Set("prompt", "A different task.\n")
	psub(m, "run").Set("minutes", int64(3))
	writeTrialHook = func(string, string, string, string, string, M) (string, error) {
		return "", Conflict("trial.toml changed on disk")
	}
	defer func() { writeTrialHook = writeTrial }()
	if _, err := save(d.trial, m, d.sha(), sha256Hex(before)); !isConflict(err) {
		t.Fatalf("err %v", err)
	}
	if after, _ := os.ReadFile(d.promptPath()); string(after) != string(before) {
		t.Fatalf("prompt %q, want %q", after, before)
	}
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if e.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTaskInputsSurviveMovingTheTrialFolderAndHoldOnlyJSON(t *testing.T) {
	d := newDemo(t, "0")
	inputs := runner.TaskInputs(d.spec())
	if inputs["fixture_dir"] != "fixture" {
		t.Fatalf("fixture %v", inputs["fixture_dir"])
	}
	moved := filepath.Join(d.tmp, "moved")
	copyDir(t, filepath.Dir(d.trial), moved)
	sp, err := spec.Load(filepath.Join(moved, "trial.toml"), spec.LoadOpts{SkipReadiness: true})
	if err != nil {
		t.Fatal(err)
	}
	if util.PyJSON(runner.TaskInputs(sp)) != util.PyJSON(inputs) {
		t.Fatalf("moved %v\n  was %v", runner.TaskInputs(sp), inputs)
	}
	d.write(strings.Replace(d.text(), "[[verify]]", "[[setup]]\nrun = \"true\"\nwhen = 2026-01-01\n\n[[verify]]", 1))
	if got := util.Maps(runner.TaskInputs(d.spec())["setup"])[0]["when"]; got != "2026-01-01" {
		t.Fatalf("when %#v", got)
	}
}

func TestSavingNothingWritesNoJournalEntry(t *testing.T) {
	d := newDemo(t, "0")
	before, _ := readEntries(d.trial, 0)
	if _, err := save(d.trial, d.model(), d.sha(), nil); err != nil {
		t.Fatal(err)
	}
	if after, _ := readEntries(d.trial, 0); len(after) != len(before) {
		t.Fatalf("journal grew: %d -> %d", len(before), len(after))
	}
}

func TestTheSnapshotCarriesTheModelOptionsAndJourneyHighlights(t *testing.T) {
	d := newDemo(t, "0")
	snap := d.snap()
	if util.Get(util.Maps(util.Get(snap, "plan_model", "workers"))[0], "id") != "demo" {
		t.Fatalf("model %v", snap["plan_model"])
	}
	names := map[any]bool{}
	for _, h := range util.Maps(util.Get(snap, "plan_options", "harnesses")) {
		names[h["name"]] = true
	}
	if !names["claude-code"] || !names["console-demo"] || util.Get(snap, "plan_options", "auto_reporter") != "console-demo" {
		t.Fatalf("options %v", snap["plan_options"])
	}
	if len(util.List(util.Get(snap, "plan_options", "prompt_lint"))) == 0 || len(util.List(util.Get(snap, "plan_options", "bash_only"))) == 0 {
		t.Fatalf("options %v", snap["plan_options"])
	}
	if k := util.Get(snap, "locks", "kind"); k != nil && k != "attempts" && k != "job" {
		t.Fatalf("lock kind %v", k)
	}
	steps := JourneyHighlights("## Chronological account\n1. \U0001f6a7 Roadblock. It failed [E-003].\n" +
		"2. I read the help [E-004].\n\n## What helped\nnothing\n")
	if util.PyJSON(steps) != `[{"label": "Roadblock", "refs": ["E-003"], "text": "It failed [E-003]."}, `+
		`{"label": null, "refs": ["E-004"], "text": "I read the help [E-004]. "}]` {
		t.Fatalf("steps %s", util.PyJSON(steps))
	}
}

func TestTOMLDatesKeepThePageWorkingWithoutTheEditor(t *testing.T) {
	d := newDemo(t, "0")
	d.write(strings.Replace(d.text(), "[task]", "[task]\nwritten = 2024-05-01", 1))
	snap := d.snap()
	if v, ok := snap["plan_model"]; !ok || v != nil {
		t.Fatalf("plan_model %v", v)
	}
}

// ---------------------------------------------------------------- the API

func TestPreviewAndSaveGoThroughTheAPIWithTheKey(t *testing.T) {
	ts := newTestServer(t, "0")
	m := util.MapOr(ts.state()["plan_model"])
	util.MapOr(m["run"])["minutes"] = 3
	status, _, raw := ts.request("/api/plan/preview", M{"model": m}, true, nil)
	if status != 200 || !util.Contains(util.Strings(decode(t, raw)["diff"]), "+timeout_seconds = 180") {
		t.Fatalf("preview %d %s", status, raw)
	}
	if status, _, _ := ts.request("/api/plan/save", M{"model": m}, false, nil); status != 403 {
		t.Fatalf("no key %d", status)
	}
	if status, _, raw := ts.request("/api/plan/save", M{"model": m, "base": ts.sha()}, true, nil); status != 200 {
		t.Fatalf("save %d %s", status, raw)
	}
	if got := util.Int(util.Get(ts.spec(), "trial", "timeout_seconds"), 0); got != 180 {
		t.Fatalf("timeout %d", got)
	}
	if !util.Contains(ts.kinds(), "plan_changed") {
		t.Fatalf("journal %v", ts.kinds())
	}
	if status, _, _ := ts.request("/api/plan/save", M{"model": m, "base": strings.Repeat("0", 64)}, true, nil); status != 409 {
		t.Fatalf("stale base %d", status)
	}
	util.Maps(m["workers"])[0]["model"] = "has space"
	status, _, raw = ts.request("/api/plan/preview", M{"model": m}, true, nil)
	if p := util.Strings(decode(t, raw)["problems"]); status != 200 || len(p) == 0 || !strings.Contains(p[0], "not a model name") {
		t.Fatalf("bad model %d %s", status, raw)
	}
	status, _, raw = ts.request("/api/plan/preview", M{"model": M{"workers": "x"}}, true, nil)
	if p := util.Strings(decode(t, raw)["problems"]); status != 200 || len(p) == 0 || !strings.Contains(p[0], "incomplete") {
		t.Fatalf("incomplete %d %s", status, raw)
	}
}

func TestWorkerFilesAreServedFromInsideTheWorkspaceOnly(t *testing.T) {
	ts := newTestServer(t, "0")
	ws := filepath.Join(ts.tmp, "ws", "attempt")
	os.MkdirAll(filepath.Join(ws, "sub"), 0o755)
	os.MkdirAll(filepath.Join(ws, "node_modules"), 0o755)
	os.WriteFile(filepath.Join(ws, "out.csv"), []byte("id\n1\n"), 0o644)
	weird := "we\"ird;nameé.bin"
	os.WriteFile(filepath.Join(ws, weird), []byte{0xff, 0xfe, 0x00}, 0o644)
	os.WriteFile(filepath.Join(ts.tmp, "secret.txt"), []byte("not for the page"), 0o644)
	if err := os.Symlink(filepath.Join(ts.tmp, "secret.txt"), filepath.Join(ws, "link.txt")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(ws, "sub", "deep.txt"), []byte("deep"), 0o644)
	os.WriteFile(filepath.Join(ws, "node_modules", "dep.js"), []byte("x"), 0o644)
	writeJSONFile(t, filepath.Join(ts.out(), "runs", "demo-r1", "state.json"), M{"paths": M{"workspace": ws}, "stages": M{}})
	status, headers, raw := ts.request("/workspace/demo-r1/out.csv", nil, true, nil)
	if status != 200 || string(raw) != "id\n1\n" || !strings.HasPrefix(headers.Get("Content-Type"), "text/plain") {
		t.Fatalf("out.csv %d %q %v", status, raw, headers)
	}
	for _, bad := range []string{"/workspace/demo-r1/link.txt", "/workspace/demo-r1/..%2F..%2Fsecret.txt",
		"/workspace/demo-r1/../../secret.txt", "/workspace/..%2Fdemo-r1/out.csv", "/workspace/demo-r1/",
		"/workspace/demo-r1/" + url.PathEscape(filepath.Join(ts.tmp, "secret.txt"))} {
		if status, _, _ := ts.request(bad, nil, true, nil); status != 404 {
			t.Errorf("%s: %d", bad, status)
		}
	}
	status, headers, _ = ts.request("/workspace/demo-r1/"+url.PathEscape(weird), nil, true, nil)
	if status != 200 || headers.Get("Content-Disposition") != "attachment; filename*=UTF-8''we%22ird%3Bname%C3%A9.bin" {
		t.Fatalf("download %d %v", status, headers)
	}
	if status, _, _ := ts.request("/workspace/demo-r1/out.csv", nil, false, nil); status != 403 {
		t.Fatalf("no key %d", status)
	}
	var listed []string
	files, _ := outputs(filepath.Join(ts.out(), "runs", "demo-r1"), M{"paths": M{"workspace": ws}}, "")
	for _, o := range files {
		listed = append(listed, util.S(o.(M)["path"]))
	}
	if strings.Join(listed, "|") != "out.csv|"+weird+"|sub/deep.txt" {
		t.Fatalf("outputs %v", listed)
	}
}

func TestUTF8TextDropsOnlyAnIncompleteTail(t *testing.T) {
	euro := []byte("€")
	data := append(bytes.Repeat(euro, 3), euro[:2]...)
	if got, ok := utf8Text(data); !ok || len(got) != 9 {
		t.Fatalf("cut tail: %v %d", ok, len(got))
	}
	if _, ok := utf8Text([]byte{0xff, 'a'}); ok {
		t.Fatal("binary data called text")
	}
}
