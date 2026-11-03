package console

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/royosherove/ajx/internal/util"
)

// ---------------------------------------------------------------- journal

func TestCursorCountsCompleteLinesOnly(t *testing.T) {
	d := newDemo(t, "0")
	record(d.trial, "user", "message", M{"text": "one"})
	record(d.trial, "agent", "note", M{"text": "two"})
	f, _ := os.OpenFile(filepath.Join(StateDir(d.trial), "actions.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"kind": "half-writ`)
	f.Close()
	entries, cursor := readEntries(d.trial, 0)
	if cursor != 2 || len(entries) != 2 || entryN(entries[0]) != 1 || entryN(entries[1]) != 2 {
		t.Fatalf("cursor %d entries %v", cursor, entries)
	}
	later, _ := readEntries(d.trial, 1)
	if len(later) != 1 || later[0]["text"] != "two" {
		t.Fatalf("later %v", later)
	}
}

func TestJournalLinesAreWhatPythonWrites(t *testing.T) {
	d := newDemo(t, "0")
	e := record(d.trial, "user", "message", M{"text": "café \"quoted\"\n", "about": nil, "count": 3})
	data, _ := os.ReadFile(journalPath(d.trial))
	want := `{"actor": "user", "at": "` + util.S(e["at"]) + `", "count": 3, "kind": "message", "text": "café \"quoted\"\n"}` + "\n"
	if string(data) != want {
		t.Fatalf("journal line\n got %q\nwant %q", data, want)
	}
	if entryN(e) != 1 {
		t.Fatalf("n = %v", e["n"])
	}
}

func TestWaitWakesOnANewActionAndOtherwiseTimesOut(t *testing.T) {
	d := newDemo(t, "0")
	_, cursor := readEntries(d.trial, 0)
	time.AfterFunc(400*time.Millisecond, func() { _, _ = message(d.trial, "why did it fail?", nil) })
	began := time.Now()
	woke, err := Wait(nil, d.trial, cursor, 20)
	if err != nil {
		t.Fatal(err)
	}
	if woke["woke"] != "journal" || time.Since(began) > 10*time.Second {
		t.Fatalf("woke %v after %v", woke["woke"], time.Since(began))
	}
	msgs := util.Maps(woke["messages_for_agent"])
	if len(msgs) != 1 || msgs[0]["text"] != "why did it fail?" {
		t.Fatalf("messages %v", msgs)
	}
	quiet, err := Wait(nil, d.trial, util.Int(woke["cursor"], 0), 1)
	if err != nil || quiet["woke"] != "timeout" {
		t.Fatalf("quiet %v %v", quiet["woke"], err)
	}
	if presence(d.trial)["state"] != "seen" {
		t.Fatalf("presence %v", presence(d.trial))
	}
}

func TestAReplyClearsTheMessageForTheAgent(t *testing.T) {
	d := newDemo(t, "0")
	_, n := readEntries(d.trial, 0)
	if _, err := message(d.trial, "is it done?", nil); err != nil {
		t.Fatal(err)
	}
	view, _ := Since(d.trial, 0, true)
	if len(util.List(view["messages_for_agent"])) != 1 {
		t.Fatalf("messages %v", view["messages_for_agent"])
	}
	if _, err := Note(d.trial, "Yes: verified, three asks.", n+1, "agent"); err != nil {
		t.Fatal(err)
	}
	view, _ = Since(d.trial, 0, true)
	if len(util.List(view["messages_for_agent"])) != 0 {
		t.Fatalf("messages %v", view["messages_for_agent"])
	}
	found := false
	for _, c := range util.Maps(view["changes"]) {
		if c["text"] == "Yes: verified, three asks." {
			found = true
		}
	}
	if !found {
		t.Fatal("note missing from changes")
	}
	if _, err := Note(d.trial, "hi", 99, "agent"); err == nil || !strings.Contains(err.Error(), "#99 is not a message") {
		t.Fatalf("reply to a non-message: %v", err)
	}
	if _, err := Note(d.trial, "   ", 0, "agent"); err == nil || err.Error() != "a note needs text" {
		t.Fatalf("empty note: %v", err)
	}
}

// ---------------------------------------------------------------- plan

func TestPlanCoversTheTrialFilePromptAndStartingFiles(t *testing.T) {
	d := newDemo(t, "0")
	sp := d.spec()
	first := planSha(d.trial, sp)
	os.WriteFile(filepath.Join(filepath.Dir(d.trial), "fixture", "orders.json"), []byte("[]\n"), 0o644)
	second := planSha(d.trial, sp)
	os.WriteFile(filepath.Join(filepath.Dir(d.trial), "task-prompt.md"), []byte("Convert the orders.\n"), 0o644)
	third := planSha(d.trial, sp)
	if first == second || second == third || first == third {
		t.Fatalf("plan ids %s %s %s", first, second, third)
	}
}

func TestAnApprovalGoesStaleWhenTheTaskChanges(t *testing.T) {
	d := newDemo(t, "0")
	snap := d.snap()
	record(d.trial, "user", "approved", M{"plan": util.Get(snap, "plan", "sha"), "via": "console"})
	if got := util.Get(d.snap(), "plan", "approval", "state"); got != "valid" {
		t.Fatalf("approval %v", got)
	}
	if _, err := savePrompt(d.trial, "Convert orders.json to out.csv.\n", util.Get(snap, "trial", "prompt", "sha256"), "user"); err != nil {
		t.Fatal(err)
	}
	if got := util.Get(d.snap(), "plan", "approval", "state"); got != "stale" {
		t.Fatalf("approval %v", got)
	}
}

func TestAPlanIDPrefixIsAcceptedAndAWrongOneExplained(t *testing.T) {
	d := newDemo(t, "0")
	plan := util.S(util.Get(d.snap(), "plan", "sha"))
	if err := PreflightStart(d.trial, plan[:12]); err != nil {
		t.Fatal(err)
	}
	err := PreflightStart(d.trial, "000000000000")
	if !isConflict(err) || !strings.Contains(err.Error(), "plan mismatch: you passed 000000000000, the current plan is "+plan[:12]) {
		t.Fatalf("mismatch: %v", err)
	}
	if err := PreflightStart(d.trial, ""); !isConflict(err) || !strings.Contains(err.Error(), "you passed nothing") {
		t.Fatalf("empty plan: %v", err)
	}
}

func TestThePlanCoversTrialPluginsButNotTheirCaches(t *testing.T) {
	d := newDemo(t, "0")
	first := planSha(d.trial, d.spec())
	cache := filepath.Join(filepath.Dir(d.trial), "plugins", "__pycache__")
	os.MkdirAll(cache, 0o755)
	os.WriteFile(filepath.Join(cache, "x.pyc"), []byte("cache"), 0o644)
	if planSha(d.trial, d.spec()) != first {
		t.Fatal("a plugin cache changed the plan")
	}
	plugin := filepath.Join(filepath.Dir(d.trial), "plugins", "console_demo.py")
	data, _ := os.ReadFile(plugin)
	os.WriteFile(plugin, append(data, []byte("\n# edited\n")...), 0o644)
	if planSha(d.trial, d.spec()) == first {
		t.Fatal("a plugin edit did not change the plan")
	}
}

func TestAStartRequestNothingFollowedDoesNotStayStarting(t *testing.T) {
	d := newDemo(t, "0")
	f, _ := os.OpenFile(filepath.Join(StateDir(d.trial), "actions.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"at": "2026-01-01T10:00:00Z", "actor": "user", "kind": "start_requested"}` + "\n")
	f.Close()
	if got := util.Get(d.snap(), "process", "state"); got != "idle" {
		t.Fatalf("state %v", got)
	}
}

func TestPromptLintFlagsHintsWithoutBlocking(t *testing.T) {
	found := lintPrompt("This is a test for AJX; there is a known bug, so find a workaround.")
	whys := map[string]bool{}
	for _, f := range util.Maps(found) {
		whys[util.S(f["why"])] = true
	}
	want := []string{"names AJX", "tells the worker it is observed", "mentions an expected problem", "suggests a workaround"}
	if len(whys) != len(want) {
		t.Fatalf("lint %v", whys)
	}
	for _, w := range want {
		if !whys[w] {
			t.Fatalf("lint missing %q: %v", w, whys)
		}
	}
	if len(lintPrompt("Convert the three orders to CSV.")) != 0 {
		t.Fatal("clean prompt flagged")
	}
}

// ---------------------------------------------------------------- edits

var sample = strings.Join([]string{
	"[trial]", `id = "x"`, "timeout_seconds = 60   # keep this note", "# max_budget_usd = 5   # example", "",
	"[[cells]]", `id = "a"`, `harness = "fake"`, "", "[[cells]]", `id = "b"`, `harness = "fake"`, `# model = "m"`, "",
	"[reporter]", `harness = "fake"`, ""}, "\n")

func TestSetValueKeepsCommentsAndReplacesCommentedExamples(t *testing.T) {
	must := func(s string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	text := must(SetValue(sample, "trial", "timeout_seconds", 120, 0))
	if !strings.Contains(text, "timeout_seconds = 120  # keep this note") {
		t.Fatal(text)
	}
	text = must(SetValue(text, "trial", "max_budget_usd", 2.5, 0))
	if !strings.Contains(text, "max_budget_usd = 2.5\n") || strings.Contains(text, "# max_budget_usd") {
		t.Fatal(text)
	}
	text = must(SetValue(text, "cells", "model", "big-model", 1))
	if !strings.Contains(text, "id = \"b\"\nharness = \"fake\"\nmodel = \"big-model\"") || strings.Contains(text, `# model = "m"`) {
		t.Fatal(text)
	}
	text = must(SetValue(text, "cells", "model", nil, 1))
	if strings.Contains(text, "big-model") {
		t.Fatal(text)
	}
	text = must(SetValue(text, "reporter", "model", "small", 0))
	if !strings.Contains(text, "[reporter]\nharness = \"fake\"\nmodel = \"small\"") {
		t.Fatal(text)
	}
	text = must(SetValue("[trial]\nid = \"x\"\n", "env", "FOO", "bar", 0))
	if !strings.HasSuffix(text, "\n\n[env]\nFOO = \"bar\"\n") {
		t.Fatalf("%q", text)
	}
}

func TestSetValueRefusesWhatItCannotEditSafely(t *testing.T) {
	if _, err := SetValue("trial = { id = \"x\" }\n", "trial", "repetitions", 2, 0); !isConsoleErr(err) {
		t.Fatalf("inline table: %v", err)
	}
	if _, err := SetValue(sample, "cells", "model", "m", 5); !isConsoleErr(err) || err.Error() != "cells[5] not found" {
		t.Fatalf("missing cell: %v", err)
	}
}

func TestSettingsAreValidatedSavedAndConflictsRefused(t *testing.T) {
	d := newDemo(t, "0")
	base := d.sha()
	if _, err := saveSettings(d.trial, M{"timeout_minutes": 2.0, "repetitions": 3.0}, base, "user"); err != nil {
		t.Fatal(err)
	}
	sp := d.spec()
	if util.Int(util.Get(sp, "trial", "timeout_seconds"), 0) != 120 || util.Int(util.Get(sp, "trial", "repetitions"), 0) != 3 {
		t.Fatalf("saved %v", sp["trial"])
	}
	if !util.Contains(d.kinds(), "settings_changed") {
		t.Fatal("no settings_changed entry")
	}
	if _, err := saveSettings(d.trial, M{"repetitions": 1.0}, base, "user"); !isConflict(err) {
		t.Fatalf("stale base: %v", err)
	}
	before := d.text()
	if _, err := saveSettings(d.trial, M{"repetitions": 0.0}, d.sha(), "user"); !isConsoleErr(err) ||
		err.Error() != "attempts must be between 1 and 50" {
		t.Fatalf("zero attempts: %v", err)
	}
	if d.text() != before {
		t.Fatal("refused settings changed the file")
	}
	if _, err := saveSettings(d.trial, M{"repetitions": 1.5}, d.sha(), "user"); err == nil || err.Error() != "attempts must be a whole number" {
		t.Fatalf("fraction: %v", err)
	}
	if _, err := saveSettings(d.trial, M{}, d.sha(), "user"); err == nil || err.Error() != "nothing to change" {
		t.Fatalf("empty: %v", err)
	}
}

func TestAnEditThatAJXCannotLoadIsNotSaved(t *testing.T) {
	d := newDemo(t, "0")
	before := d.text()
	broken := strings.Replace(before, "harness = \"console-demo\"\n\n[[cells]]", "harness = \"no-such-agent\"\n\n[[cells]]", 1)
	if broken == before {
		t.Fatal("fixture changed")
	}
	if _, err := saveTrialText(d.trial, broken, sha256Hex([]byte(before)), "user"); !isConsoleErr(err) ||
		!strings.HasPrefix(err.Error(), "not saved: ") {
		t.Fatalf("broken edit: %v", err)
	}
	if d.text() != before {
		t.Fatal("broken edit was saved")
	}
	if _, err := saveTrialText(d.trial, strings.Replace(before, "timeout_seconds = 300", "timeout_seconds = 240", 1),
		sha256Hex([]byte(before)), "user"); err != nil {
		t.Fatal(err)
	}
	if util.Int(util.Get(d.spec(), "trial", "timeout_seconds"), 0) != 240 {
		t.Fatal("edit not saved")
	}
	entries, _ := readEntries(d.trial, 0)
	if last(entries, "trial_edited") == nil {
		t.Fatal("no trial_edited entry")
	}
}

func TestTheTaskIsLockedOnceTheOutputFolderHoldsAttempts(t *testing.T) {
	d := newDemo(t, "0")
	out := d.out()
	writeJSONFile(t, filepath.Join(out, "runs", "demo-r1", "state.json"), M{"stages": M{"prepare": M{"status": "done"}}})
	prompt := filepath.Join(filepath.Dir(d.trial), "task-prompt.md")
	sha := func() string { b, _ := os.ReadFile(prompt); return sha256Hex(b) }
	if _, err := savePrompt(d.trial, "Something else.\n", sha(), "user"); err == nil || !strings.Contains(err.Error(), "fresh output folder") {
		t.Fatalf("locked prompt: %v", err)
	}
	result, err := freshOutput(d.trial, d.sha(), "user")
	if err != nil {
		t.Fatal(err)
	}
	if d.out() == out || !strings.HasPrefix(util.S(result["output_dir"]), "ajx-reports/") {
		t.Fatalf("fresh output %v", result)
	}
	if !regexp.MustCompile(`console-demo-\d{8}-\d{6}$`).MatchString(util.S(result["output_dir"])) {
		t.Fatalf("fresh output name %v", result["output_dir"])
	}
	if _, err := savePrompt(d.trial, "Something else.", sha(), "user"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(prompt); string(b) != "Something else.\n" {
		t.Fatalf("prompt %q", b)
	}
}

func TestContractGuardRefusesMixedSettings(t *testing.T) {
	d := newDemo(t, "0")
	out := d.out()
	writeJSONFile(t, filepath.Join(out, "runs", "demo-r1", "state.json"), M{"stages": M{"prepare": M{"status": "done"}}})
	cmd := exec.Command(os.Getenv("AJX_BIN"), "run", d.trial)
	cmd.Dir = filepath.Dir(d.trial)
	_ = os.Remove(filepath.Join(out, "runs", "demo-r1", "state.json"))
	if outB, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fake run: %v %s", err, outB)
	}
	changed := strings.Replace(d.text(), "timeout_seconds = 300", "timeout_seconds = 240", 1)
	if _, err := saveTrialText(d.trial, changed, d.sha(), "user"); err == nil || !strings.Contains(err.Error(), "holds attempts made with the current settings") {
		t.Fatalf("contract guard: %v", err)
	}
}

// ---------------------------------------------------------------- snapshot

func (d *demo) state(stagesMarks map[string]string, extra M) {
	marks := M{}
	for name, status := range stagesMarks {
		marks[name] = M{"status": status, "at": "2026-01-01T10:00:00Z"}
	}
	st := M{"stages": marks}
	for k, v := range extra {
		st[k] = v
	}
	writeJSONFile(d.t, filepath.Join(d.out(), "runs", "demo-r1", "state.json"), st)
}

func TestARunInReportingShowsItsStageCleanupAndPendingAsks(t *testing.T) {
	d := newDemo(t, "0")
	marks := map[string]string{"extract": "running"}
	for _, s := range []string{"prepare", "execute", "verify", "teardown", "normalize", "narrate", "release"} {
		marks[s] = "done"
	}
	d.state(marks, M{"teardown": M{"status": "failed"}})
	writeJSONFile(t, filepath.Join(d.out(), "runs", "demo-r1", "verify.json"),
		M{"outcome": "partial", "passed": 1, "total": 2, "checks": []any{M{"name": "a", "passed": true}}})
	snap := d.snap()
	run := util.Maps(snap["runs"])[0]
	if run["current"] != "extract" || util.Get(run, "asks", "status") != "generating" || util.Get(run, "verify", "outcome") != "partial" {
		t.Fatalf("run %v %v %v", run["current"], run["asks"], run["verify"])
	}
	rec := util.Maps(run["recovery"])
	if len(rec) != 1 || rec[0]["stages"] != "teardown,render" {
		t.Fatalf("recovery %v", rec)
	}
	out := util.MapOr(snap["output"])
	if util.Int(out["started"], -1) != 1 || util.Int(out["complete"], -1) != 0 || out["resumable"] != true {
		t.Fatalf("output %v", out)
	}
	if !strings.Contains(util.S(util.Get(snap, "locks", "prompt")), "fresh output folder") {
		t.Fatalf("locks %v", snap["locks"])
	}
	// The milestones are journaled once, with Python's texts.
	var progress []string
	entries, _ := readEntries(d.trial, 0)
	for _, e := range entries {
		if e["kind"] == "progress" {
			progress = append(progress, util.S(e["text"]))
		}
	}
	for _, want := range []string{"demo-r1: verification partial (1/2 checks passed)", "demo-r1: task cleanup failed; resources may still exist"} {
		if !util.Contains(progress, want) {
			t.Fatalf("progress %q missing from %v", want, progress)
		}
	}
	n := len(progress)
	d.snap()
	entries, _ = readEntries(d.trial, 0)
	count := 0
	for _, e := range entries {
		if e["kind"] == "progress" {
			count++
		}
	}
	if count != n {
		t.Fatalf("milestones journaled twice: %d then %d", n, count)
	}
	if _, err := os.Stat(filepath.Join(StateDir(d.trial), "seen", "demo-r1.verify.done.2026-01-01T10_00_00Z")); err != nil {
		t.Fatalf("seen marker: %v", err)
	}
}

func TestAChangedTaskCannotResumeAnOutputFolder(t *testing.T) {
	d := newDemo(t, "0")
	d.state(map[string]string{"prepare": "done", "execute": "done", "render": "done"}, nil)
	writeJSONFile(t, filepath.Join(d.out(), "matrix-plan.json"), M{"task_prompt_sha256": strings.Repeat("0", 64), "order": []any{"demo-r1"}})
	if got := util.Get(d.snap(), "output", "blocker_kind"); got != "changed" {
		t.Fatalf("blocker %v", got)
	}
	if _, _, err := preflightStart(d.trial, nil); err == nil || !strings.Contains(err.Error(), "different task prompt") {
		t.Fatalf("preflight: %v", err)
	}
}

func TestAFinishedOutputOffersAnotherAttempt(t *testing.T) {
	d := newDemo(t, "0")
	d.state(map[string]string{"prepare": "done", "execute": "done", "render": "done"}, nil)
	sp := d.spec()
	writeJSONFile(t, filepath.Join(d.out(), "matrix-plan.json"), M{"task_prompt_sha256": util.Get(sp, "task", "prompt_sha256"), "order": []any{"demo-r1"}})
	if got := util.Get(d.snap(), "output", "blocker_kind"); got != "complete" {
		t.Fatalf("blocker %v", got)
	}
	if _, err := addAttempt(d.trial, d.sha(), "user"); err != nil {
		t.Fatal(err)
	}
	out := util.MapOr(d.snap()["output"])
	if out["blocker"] != nil || util.Int(out["planned"], 0) != 2 || out["resumable"] != true {
		t.Fatalf("output %v", out)
	}
}

func TestAWorkerThatNeverStartedIsNamedWithItsError(t *testing.T) {
	d := newDemo(t, "0")
	rd := filepath.Join(d.out(), "runs", "demo-r1")
	marks := M{}
	for _, s := range []string{"prepare", "execute", "verify"} {
		marks[s] = M{"status": "done", "at": "2026-01-01T10:00:05Z"}
	}
	writeJSONFile(t, filepath.Join(rd, "state.json"), M{"stages": marks, "execute": M{
		"exit_code": 1, "stop_reason": "exit_1", "started_at": "2026-01-01T10:00:00Z",
		"stopped_at": "2026-01-01T10:00:03Z", "requested": M{"harness": "console-demo", "model": "sonnet 5.5"}}})
	writeJSONFile(t, filepath.Join(rd, "telemetry.execute.json"), M{"events": []any{M{"kind": "text", "text": "API Error"}},
		"final_text": "API Error (sonnet 5.5): 400 The provided model identifier is invalid."})
	snap := d.snap()
	run := util.Maps(snap["runs"])[0]
	result := util.MapOr(run["result"])
	if result["could_not_start"] != true || !strings.HasPrefix(util.S(result["error"]), "API Error (sonnet 5.5)") || run["model"] != "sonnet 5.5" {
		t.Fatalf("result %v model %v", result, run["model"])
	}
	found := false
	for _, e := range util.Maps(util.Get(snap, "journal", "entries")) {
		if strings.HasPrefix(util.S(e["text"]), "demo-r1: the worker could not start: API Error (sonnet 5.5)") {
			found = true
		}
	}
	if !found {
		t.Fatal("could-not-start milestone missing")
	}
	view, _ := Since(d.trial, 0, true)
	if util.Maps(view["runs"])[0]["could_not_start"] != true {
		t.Fatal("since does not report could_not_start")
	}
}

func TestIconsAndMeaningsComeWithTheSnapshot(t *testing.T) {
	d := newDemo(t, "0")
	vocab := util.MapOr(d.snap()["vocabulary"])
	if util.List(util.Get(vocab, "labels", "Misdirection"))[0] != "\U0001f534" {
		t.Fatalf("labels %v", vocab["labels"])
	}
	meanings := util.MapOr(vocab["meanings"])
	if !strings.Contains(util.S(meanings["Misdirection"]), "contradicted") {
		t.Fatalf("meanings %v", meanings)
	}
	if len(meanings) != len(util.MapOr(vocab["labels"])) {
		t.Fatalf("meanings cover %d of %d labels", len(meanings), len(util.MapOr(vocab["labels"])))
	}
}

func TestSettingsLockOnceAttemptsExistButAttemptsCanGrow(t *testing.T) {
	d := newDemo(t, "0")
	writeJSONFile(t, filepath.Join(d.out(), "runs", "demo-r1", "state.json"), M{"stages": M{"prepare": M{"status": "done"}}})
	base := d.sha()
	if _, err := saveSettings(d.trial, M{"timeout_minutes": 3.0}, base, "user"); err == nil || !strings.Contains(err.Error(), "locked for this output folder") {
		t.Fatalf("locked: %v", err)
	}
	if _, err := saveSettings(d.trial, M{"repetitions": 2.0}, base, "user"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(util.S(util.Get(d.snap(), "locks", "settings")), "Locked for this output folder") {
		t.Fatal("settings lock missing")
	}
}

func TestModelNamesAnAgentCannotTakeAreRefused(t *testing.T) {
	d := newDemo(t, "0")
	base := d.sha()
	for _, bad := range []string{"sonnet 5.5", "--dangerous"} {
		if _, err := saveSettings(d.trial, M{"cell_models": M{"0": bad}}, base, "user"); err == nil || !strings.Contains(err.Error(), "not a model name") {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if _, err := saveSettings(d.trial, M{"cell_models": M{"0": "sonnet"}, "timeout_minutes": 4.0}, base, "user"); err != nil {
		t.Fatal(err)
	}
	entries, _ := readEntries(d.trial, 0)
	notes := util.Strings(last(entries, "settings_changed")["changes"])
	if !util.Contains(notes, "time limit 5 -> 4 min") || !util.Contains(notes, "cell 1 model default -> sonnet") {
		t.Fatalf("notes %v", notes)
	}
	cells := util.Maps(util.Get(d.snap(), "trial", "cells"))
	if !strings.Contains(util.S(cells[0]["model"]), "sonnet") {
		t.Fatalf("cells %v", cells)
	}
	if _, err := saveSettings(d.trial, M{"max_budget_usd": 2.5}, d.sha(), "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := saveSettings(d.trial, M{"max_budget_usd": ""}, d.sha(), "user"); err != nil {
		t.Fatal(err)
	}
	entries, _ = readEntries(d.trial, 0)
	var all []string
	for _, e := range entries {
		if e["kind"] == "settings_changed" {
			all = append(all, util.Strings(e["changes"])...)
		}
	}
	if !util.Contains(all, "budget none -> $2.5") || !util.Contains(all, "budget $2.5 -> none") {
		t.Fatalf("budget notes %v", all)
	}
}

func TestDescribeTexts(t *testing.T) {
	cases := []struct {
		e    M
		want string
	}{
		{M{"kind": "approved", "plan": "abcdef0123456789", "via": "chat", "quote": "go"}, "Approved plan abcdef0 in chat: “go”"},
		{M{"kind": "approved", "plan": "abcdef0123456789", "via": "console"}, "Approved plan abcdef0 in the console"},
		{M{"kind": "job_started", "job": "run", "via": "chat"}, "Started the run (chat)"},
		{M{"kind": "job_started", "job": "stages", "stages": "teardown,render", "run": "demo-r1"}, "Started: Retry task cleanup for demo-r1"},
		{M{"kind": "job_started", "job": "report"}, "Rebuilding the reports (no model calls)"},
		{M{"kind": "job_finished", "job": "run", "exit_code": int64(130)}, "The run was stopped"},
		{M{"kind": "job_finished", "job": "report", "exit_code": int64(1)}, "Rebuilding reports ended with exit 1"},
		{M{"kind": "job_finished", "job": "x"}, "A command ended"},
		{M{"kind": "job_finished", "job": "run", "summary": "finished: demo-r1 succeeded, report ready"}, "The run finished: demo-r1 succeeded, report ready"},
		{M{"kind": "run_again", "mode": "same-folder"}, "Run again: one more attempt in this output folder"},
		{M{"kind": "checked", "status": "not_ready"}, "Readiness: not ready"},
		{M{"kind": "settings_changed", "changes": []any{"a", "b"}}, "Changed settings: a, b"},
		{M{"kind": "start_blocked", "reason": "not ready"}, "Did not start: not ready"},
		{M{"kind": "mystery"}, "mystery"},
		{M{}, "None"},
	}
	for _, c := range cases {
		if got := describe(c.e); got != c.want {
			t.Errorf("describe(%v) = %q, want %q", c.e, got, c.want)
		}
	}
}

func TestDisplayShortensHomeAndPlanIDs(t *testing.T) {
	home := util.Home()
	got := display([]string{"run", filepath.Join(home, "t", "trial.toml"), "--expect-plan", strings.Repeat("ab", 32), "it's"})
	want := "ajx run ~/t/trial.toml --expect-plan abababababab… 'it'\"'\"'s'"
	if got != want {
		t.Fatalf("display %q, want %q", got, want)
	}
}

func TestHasProgressSeesDeclaredMethodsOnly(t *testing.T) {
	if !hasProgress(&demoHarness{}) {
		t.Fatal("declared Progress not seen")
	}
	if hasProgress(&plainHarness{}) {
		t.Fatal("inherited Progress counted as live progress")
	}
	d := newDemo(t, "0")
	items, note := feed(nil, filepath.Join(d.out(), "runs", "x-r1"), 5)
	if len(items) != 0 || note != "The agent for this run is not known yet." {
		t.Fatalf("feed %v %v", items, note)
	}
	rd := filepath.Join(d.out(), "runs", "x-r1")
	writeJSONFile(t, filepath.Join(rd, "run.json"), M{"agent": M{"harness_name": "plain-test"}})
	if _, note := feed(nil, rd, 5); note != "plain-test does not report live progress. The report lists every event after the run." {
		t.Fatalf("note %v", note)
	}
}

// ---------------------------------------------------------------- server

type testServer struct {
	*demo
	s    *server
	done chan error
}

func newTestServer(t *testing.T, pace string) *testServer {
	d := newDemo(t, pace)
	s, err := newServer(d.trial, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ts := &testServer{demo: d, s: s, done: make(chan error, 1)}
	go func() { ts.done <- s.serve() }()
	t.Cleanup(func() {
		deadline := time.Now().Add(120 * time.Second)
		for s.app.checking.Load() && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		s.shutdown()
		<-ts.done
		for _, cmd := range s.app.children {
			if cmd.Process != nil {
				_ = cmd.Process.Signal(syscall.SIGINT)
			}
		}
		deadline = time.Now().Add(30 * time.Second)
		for s.app.busy() && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
	})
	return ts
}

func (ts *testServer) request(path string, body any, token bool, headers map[string]string) (int, http.Header, []byte) {
	ts.t.Helper()
	var reader io.Reader
	method := "GET"
	if body != nil {
		method = "POST"
		if raw, ok := body.([]byte); ok {
			reader = bytes.NewReader(raw)
		} else {
			data, _ := json.Marshal(body)
			reader = bytes.NewReader(data)
		}
	}
	req, _ := http.NewRequest(method, "http://127.0.0.1:"+py(ts.s.info["port"])+path, reader)
	if token {
		req.Header.Set("X-AJX-Token", util.S(ts.s.info["token"]))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		ts.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, data
}

func (ts *testServer) state() M {
	ts.t.Helper()
	status, _, raw := ts.request("/api/state", nil, true, nil)
	if status != 200 {
		ts.t.Fatalf("state %d %s", status, raw)
	}
	var out M
	if err := json.Unmarshal(raw, &out); err != nil {
		ts.t.Fatal(err)
	}
	return out
}

func decode(t *testing.T, raw []byte) M {
	var out M
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return out
}

func TestActionsNeedTheKeyAndALocalHostName(t *testing.T) {
	ts := newTestServer(t, "0")
	status, headers, _ := ts.request("/", nil, false, nil)
	if status != 200 || !strings.Contains(headers.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("page %d %v", status, headers)
	}
	if status, _, _ := ts.request("/static/console.js", nil, false, nil); status != 200 {
		t.Fatalf("static %d", status)
	}
	if status, _, _ := ts.request("/api/state", nil, false, nil); status != 403 {
		t.Fatalf("no key %d", status)
	}
	if status, _, _ := ts.request("/api/state", nil, true, map[string]string{"Host": "attacker.example:" + py(ts.s.info["port"])}); status != 403 {
		t.Fatalf("bad host %d", status)
	}
	if got := util.Get(ts.state(), "trial", "id"); got != "console-demo" {
		t.Fatalf("trial id %v", got)
	}
	status, headers, _ = ts.request("/api/session", M{}, true, nil)
	setCookie := headers.Get("Set-Cookie")
	if status != 200 || !strings.Contains(setCookie, "SameSite=Lax") || !strings.Contains(setCookie, "HttpOnly") {
		t.Fatalf("session %d %q", status, setCookie)
	}
	cookie := strings.Split(setCookie, ";")[0]
	// A link opened from the page carries the cookie; an action never counts on it.
	if status, _, _ := ts.request("/api/state", nil, false, map[string]string{"Cookie": cookie}); status != 200 {
		t.Fatalf("cookie GET %d", status)
	}
	if status, _, _ := ts.request("/api/message", M{"text": "hi"}, false, map[string]string{"Cookie": cookie}); status != 403 {
		t.Fatalf("cookie POST %d", status)
	}
	if status, _, _ := ts.request("/api/message", M{"text": "hi"}, true, nil); status != 200 {
		t.Fatalf("message %d", status)
	}
	if status, _, _ := ts.request("/api/nothing", nil, true, nil); status != 404 {
		t.Fatalf("unknown %d", status)
	}
	if status, headers, _ := ts.request("/api/ping", nil, true, nil); status != 200 || headers.Get("Cache-Control") != "no-store" ||
		headers.Get("X-Content-Type-Options") != "nosniff" || headers.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("ping %d %v", status, headers)
	}
}

func TestFilesAreServedOnlyFromTheOutputFolder(t *testing.T) {
	ts := newTestServer(t, "0")
	report := filepath.Join(ts.out(), "runs", "demo-r1", "report.html")
	os.MkdirAll(filepath.Dir(report), 0o755)
	os.WriteFile(report, []byte("<p>report</p>"), 0o644)
	if _, _, body := ts.request("/files/runs/demo-r1/report.html", nil, true, nil); string(body) != "<p>report</p>" {
		t.Fatalf("report %q", body)
	}
	if status, _, _ := ts.request("/files/%2e%2e/%2e%2e/%2e%2e/trial.toml", nil, true, nil); status != 404 {
		t.Fatalf("traversal %d", status)
	}
	if status, _, _ := ts.request("/files/runs/demo-r1/state.unknown", nil, true, nil); status != 404 {
		t.Fatalf("unknown suffix %d", status)
	}
	if status, _, _ := ts.request("/files/%2Fetc%2Fhosts", nil, true, nil); status != 404 {
		t.Fatalf("absolute %d", status)
	}
	logs := filepath.Join(StateDir(ts.trial), "logs")
	os.MkdirAll(logs, 0o755)
	os.WriteFile(filepath.Join(logs, "x.log"), []byte("log line"), 0o644)
	if status, _, body := ts.request("/logs/x.log", nil, true, nil); status != 200 || string(body) != "log line" {
		t.Fatalf("log %d %q", status, body)
	}
	if status, _, _ := ts.request("/logs/..%2F..%2Ftrial.toml", nil, true, nil); status != 404 {
		t.Fatalf("log traversal %d", status)
	}
}

func TestReportsRunSandboxedAndBadRequestsGet400(t *testing.T) {
	ts := newTestServer(t, "0")
	report := filepath.Join(ts.out(), "runs", "demo-r1", "report.html")
	os.MkdirAll(filepath.Dir(report), 0o755)
	os.WriteFile(report, []byte("<p>report</p>"), 0o644)
	_, headers, _ := ts.request("/files/runs/demo-r1/report.html", nil, true, nil)
	if !strings.HasPrefix(headers.Get("Content-Security-Policy"), "sandbox ") {
		t.Fatalf("csp %q", headers.Get("Content-Security-Policy"))
	}
	status, _, raw := ts.request("/api/message", []byte("{not json"), true, nil)
	if status != 400 || decode(t, raw)["error"] != "the request body is not valid JSON" {
		t.Fatalf("bad json %d %s", status, raw)
	}
	status, _, raw = ts.request("/api/message", []byte("[1]"), true, nil)
	if status != 400 || decode(t, raw)["error"] != "expected a JSON object" {
		t.Fatalf("array %d %s", status, raw)
	}
	status, _, raw = ts.request("/api/message", M{"text": "  "}, true, nil)
	if status != 400 || decode(t, raw)["error"] != "write a message first" {
		t.Fatalf("empty message %d %s", status, raw)
	}
}

func TestAStartForAPlanTheUserDidNotSeeIsRefused(t *testing.T) {
	ts := newTestServer(t, "0")
	status, _, raw := ts.request("/api/start", M{"plan": strings.Repeat("0", 64)}, true, nil)
	if status != 409 || !strings.Contains(util.S(decode(t, raw)["error"]), "plan mismatch") {
		t.Fatalf("start %d %s", status, raw)
	}
	if util.Contains(ts.kinds(), "job_started") {
		t.Fatal("a job started")
	}
}

func complete(snap M, count int) bool {
	if util.Get(snap, "process", "state") != "idle" {
		return false
	}
	n := 0
	for _, r := range util.Maps(snap["runs"]) {
		if r["complete"] == true {
			n++
		}
	}
	return n >= count
}

func TestStartInTheConsoleChecksApprovesRunsAndReports(t *testing.T) {
	ts := newTestServer(t, "0")
	plan := util.S(util.Get(ts.state(), "plan", "sha"))
	if status, _, raw := ts.request("/api/start", M{"plan": plan}, true, nil); status != 202 {
		t.Fatalf("start %d %s", status, raw)
	}
	var snap M
	until(t, 60*time.Second, func() bool { snap = ts.state(); return complete(snap, 1) })
	run := util.Maps(snap["runs"])[0]
	if util.Get(run, "verify", "outcome") != "succeeded" || util.Get(run, "asks", "status") != "ready" ||
		util.Int(util.Get(run, "asks", "count"), 0) != 3 || run["review_status"] != "ready" || util.Get(run, "links", "report") == nil {
		t.Fatalf("run %v", run)
	}
	entries, _ := readEntries(ts.trial, 0)
	for _, kind := range []string{"start_requested", "check_started", "checked", "approved", "job_started", "job_finished"} {
		if last(entries, kind) == nil {
			t.Fatalf("no %s in %v", kind, ts.kinds())
		}
	}
	finished := last(entries, "job_finished")
	if util.Int(finished["exit_code"], -1) != 0 || !strings.HasPrefix(util.S(finished["summary"]), "finished: demo-r1 succeeded, report ready") {
		t.Fatalf("finished %v", finished)
	}
	if last(entries, "approved")["plan"] != plan || last(entries, "approved")["via"] != "console" {
		t.Fatalf("approved %v", last(entries, "approved"))
	}
	started := last(entries, "job_started")
	if !strings.HasPrefix(util.S(started["command"]), "ajx run ") || !strings.Contains(util.S(started["command"]), "--expect-plan "+plan[:12]+"…") {
		t.Fatalf("command %v", started["command"])
	}
	var progress []string
	for _, e := range entries {
		if e["kind"] == "progress" {
			progress = append(progress, util.S(e["text"]))
		}
	}
	for _, want := range []string{"demo-r1: the worker finished", "demo-r1: verification succeeded (1/1 checks passed)",
		"demo-r1: 3 ask(s) extracted", "demo-r1: report ready"} {
		if !util.Contains(progress, want) {
			t.Fatalf("progress %q missing: %v", want, progress)
		}
	}
	view, _ := Since(ts.trial, 0, true)
	if !strings.HasPrefix(util.S(view["headline"]), "1 of 1 attempts finished") {
		t.Fatalf("headline %v", view["headline"])
	}
	vr := util.Maps(view["runs"])[0]
	if !strings.HasSuffix(util.S(util.Get(vr, "files", "report")), "runs/demo-r1/report.html") || util.Int(vr["asks"], 0) != 3 {
		t.Fatalf("since run %v", vr)
	}
	if util.Get(snap, "output", "blocker_kind") != "complete" {
		t.Fatalf("blocker %v", util.Get(snap, "output", "blocker_kind"))
	}
	status, _, raw := ts.request("/api/export", M{"run": "demo-r1"}, true, nil)
	if status != 200 || len(util.List(decode(t, raw)["files"])) != 3 {
		t.Fatalf("export %d %s", status, raw)
	}
	status, _, raw = ts.request("/api/stages", M{"run": "demo-r1", "stages": "teardown,render"}, true, nil)
	if status != 200 {
		t.Fatalf("stages %d %s", status, raw)
	}
	until(t, 30*time.Second, func() bool { return util.Get(ts.state(), "process", "state") == "idle" })
	if status, _, raw := ts.request("/api/stages", M{"run": "demo-r1", "stages": "rm -rf"}, true, nil); status != 400 ||
		decode(t, raw)["error"] != "unsupported stage set 'rm -rf'" {
		t.Fatalf("bad stages %d %s", status, raw)
	}
	if status, _, raw := ts.request("/api/report", M{}, true, nil); status != 200 {
		t.Fatalf("report %d %s", status, raw)
	}
	until(t, 30*time.Second, func() bool { return util.Get(ts.state(), "process", "state") == "idle" })
}

func TestStopInterruptsTheWorkerAndTakesTheHandledCleanup(t *testing.T) {
	d := newDemo(t, "8")
	var cmds []*exec.Cmd
	var mu sync.Mutex
	capture := func(cmd *exec.Cmd, entry M) { mu.Lock(); cmds = append(cmds, cmd); mu.Unlock() }
	entry, err := start(d.trial, StartOpts{Actor: "agent", Via: "chat", Quote: "Looks right, go ahead."}, nil, "", capture)
	if err != nil {
		t.Fatal(err)
	}
	if entry["via"] != "chat" {
		t.Fatalf("entry %v", entry)
	}
	t.Cleanup(func() {
		for _, c := range cmds {
			_ = c.Process.Kill()
		}
	})
	until(t, 30*time.Second, func() bool {
		s, _ := Snapshot(d.trial, SnapshotOpts{})
		run := util.Maps(s["runs"])[0]
		return util.Get(run, "execute", "status") == "running" && len(util.List(run["feed"])) > 0
	})
	if got := util.Get(d.snap(), "process", "state"); got != "running" {
		t.Fatalf("state %v", got)
	}
	if _, err := Stop(d.trial, "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := Stop(d.trial, "user"); !isConflict(err) || !strings.Contains(err.Error(), "already stopping") {
		t.Fatalf("second stop: %v", err)
	}
	if got := util.Get(d.snap(), "process", "state"); got != "stopping" {
		t.Fatalf("state %v", got)
	}
	_ = cmds[0].Wait()
	if code := util.ExitCode(cmds[0].ProcessState); code != 130 {
		t.Fatalf("exit %d", code)
	}
	st := readMap(filepath.Join(d.out(), "runs", "demo-r1", "state.json"))
	if util.Get(st, "stages", "execute", "status") != "interrupted" {
		t.Fatalf("state %v", st["stages"])
	}
	if exists(filepath.Join(d.out(), ".lock")) {
		t.Fatal("lock left behind")
	}
	entries, _ := readEntries(d.trial, 0)
	approved := last(entries, "approved")
	if approved["via"] != "chat" || approved["quote"] != "Looks right, go ahead." {
		t.Fatalf("approved %v", approved)
	}
	if last(entries, "stop_requested") == nil {
		t.Fatal("no stop_requested")
	}
	// Nothing reaped this job here: the next view records its end.
	time.Sleep(3100 * time.Millisecond)
	d.snap()
	entries, _ = readEntries(d.trial, 0)
	fin := last(entries, "job_finished")
	if fin == nil || fin["exit_code"] != nil {
		t.Fatalf("job_finished %v", fin)
	}
}

func TestAReusedPidIsNeverTreatedAsTheRun(t *testing.T) {
	d := newDemo(t, "0")
	sleeper := exec.Command("sleep", "60")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() })
	record(d.trial, "user", "job_started", M{"job": "run", "pid": sleeper.Process.Pid, "host": hostname(),
		"command": "ajx run", "log": "none.log"})
	if got := util.Get(d.snap(), "process", "state"); got != "idle" {
		t.Fatalf("state %v", got)
	}
	if _, err := Stop(d.trial, "user"); err == nil || err.Error() != "no run is in progress" {
		t.Fatalf("stop: %v", err)
	}
}

func TestStopIsRefusedForParallelRuns(t *testing.T) {
	d := newDemo(t, "0")
	d.set("trial", "parallel", 2, 0)
	// Set argv[0] directly: /bin/sh need not support `exec -a`, and a
	// wrapper that execs sleep loses the command line this test needs.
	sleeper := exec.Command("sleep", "60")
	sleeper.Args[0] = "ajx run x"
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() })
	until(t, 5*time.Second, func() bool { return isAjx(sleeper.Process.Pid, "run") == 1 })
	record(d.trial, "user", "job_started", M{"job": "run", "pid": sleeper.Process.Pid, "host": hostname(),
		"command": "ajx run", "log": "none.log"})
	if got := util.Get(d.snap(), "process", "state"); got != "running" {
		t.Fatalf("state %v", got)
	}
	if _, err := Stop(d.trial, "user"); err == nil || !strings.Contains(err.Error(), "parallel") {
		t.Fatalf("stop: %v", err)
	}
}

func TestReadinessWarningsOnAnAccountTaskNeedAcceptance(t *testing.T) {
	d := newDemo(t, "0")
	plan := planSha(d.trial, d.spec())
	writePrivate(readinessPath(d.trial), M{"status": "ready", "plan": plan, "checked_at": util.Now(), "steps": []any{},
		"problems": []any{}, "warnings": []any{"an aws command without --profile acts in the model's account"}, "needs_ack": true})
	var cmds []*exec.Cmd
	capture := func(cmd *exec.Cmd, entry M) { cmds = append(cmds, cmd) }
	if _, err := start(d.trial, StartOpts{Actor: "agent", Via: "chat", Quote: "go"}, nil, "", capture); err == nil ||
		!strings.Contains(err.Error(), "need your acceptance") {
		t.Fatalf("start: %v", err)
	}
	if got := last(func() []M { e, _ := readEntries(d.trial, 0); return e }(), "start_blocked"); got == nil {
		t.Fatal("no start_blocked")
	}
	if _, err := start(d.trial, StartOpts{Actor: "agent", Via: "chat", Quote: "go, I accept the warning", AcceptWarnings: true},
		nil, "", capture); err != nil {
		t.Fatal(err)
	}
	_ = cmds[0].Wait()
	if code := util.ExitCode(cmds[0].ProcessState); code != 0 {
		t.Fatalf("exit %d", code)
	}
	entries, _ := readEntries(d.trial, 0)
	accepted := util.Strings(last(entries, "approved")["accepted_warnings"])
	if len(accepted) != 1 || accepted[0] != "an aws command without --profile acts in the model's account" {
		t.Fatalf("accepted %v", accepted)
	}
}

func TestACheckWithWarningsOnAnAccountTaskNeedsAcceptance(t *testing.T) {
	d := newDemo(t, "0")
	d.set("task", "identity_cmd", "aws sts get-caller-identity", 0)
	t.Setenv("AJX_FAKE_DOCTOR", "WARNING the tool shell sets AWS_PROFILE|doctor: 1 PROBLEMS")
	res, err := check(d.trial, "user")
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "ready" || res["needs_ack"] != true {
		t.Fatalf("check %v", res)
	}
	if w := util.Strings(res["warnings"]); len(w) != 1 || w[0] != "the tool shell sets AWS_PROFILE" {
		t.Fatalf("warnings %v", w)
	}
	if p := util.Strings(res["problems"]); len(p) != 1 || p[0] != "doctor: 1 PROBLEMS" {
		t.Fatalf("problems %v", p)
	}
	steps := util.Maps(res["steps"])
	if len(steps) != 2 || steps[0]["name"] != "validate" || !strings.HasPrefix(util.S(steps[1]["command"]), "ajx doctor ") {
		t.Fatalf("steps %v", steps)
	}
	view, _ := Since(d.trial, 0, true)
	found := false
	for _, s := range util.Strings(view["do_not"]) {
		if strings.HasPrefix(s, "pass --accept-warnings unless") {
			found = true
		}
	}
	if !found || util.Get(view, "readiness", "warnings_need_acceptance") != true {
		t.Fatalf("do_not %v", view["do_not"])
	}
}

func TestRunAgainHereAddsAComparableAttemptAndAFreshFolderStartsOver(t *testing.T) {
	ts := newTestServer(t, "0")
	if status, _, raw := ts.request("/api/start", M{"plan": util.Get(ts.state(), "plan", "sha")}, true, nil); status != 202 {
		t.Fatalf("start %d %s", status, raw)
	}
	until(t, 60*time.Second, func() bool { return complete(ts.state(), 1) })
	first := ts.out()
	view, _ := Since(ts.trial, 0, true)
	foundAgain := false
	for _, s := range util.Strings(view["do_not"]) {
		if strings.Contains(s, "every planned attempt finished") {
			foundAgain = true
		}
	}
	if !foundAgain {
		t.Fatalf("do_not %v", view["do_not"])
	}
	if status, _, raw := ts.request("/api/again", M{"mode": "same-folder", "plan": util.Get(ts.state(), "plan", "sha")}, true, nil); status != 202 {
		t.Fatalf("again %d %s", status, raw)
	}
	var snap M
	until(t, 60*time.Second, func() bool { snap = ts.state(); return complete(snap, 2) && !ts.s.app.checking.Load() })
	var done []string
	for _, r := range util.Maps(snap["runs"]) {
		if r["complete"] == true {
			done = append(done, util.S(r["id"]))
		}
	}
	if strings.Join(done, ",") != "demo-r1,demo-r2" {
		t.Fatalf("complete %v", done)
	}
	if status, _, raw := ts.request("/api/again", M{"mode": "fresh-folder", "plan": util.Get(ts.state(), "plan", "sha")}, true, nil); status != 202 {
		t.Fatalf("again %d %s", status, raw)
	}
	until(t, 60*time.Second, func() bool {
		snap = ts.state()
		n := 0
		for _, r := range util.Maps(snap["runs"]) {
			if r["complete"] == true {
				n++
			}
		}
		return util.Get(snap, "output", "dir") != first && util.Get(snap, "process", "state") == "idle" && n == 2 && !ts.s.app.checking.Load()
	})
	for _, r := range util.Maps(snap["runs"]) {
		if util.Get(r, "verify", "outcome") != "succeeded" {
			t.Fatalf("run %v", r)
		}
	}
	count := map[string]int{}
	for _, k := range ts.kinds() {
		count[k]++
	}
	if count["run_again"] != 2 || count["approved"] != 3 {
		t.Fatalf("kinds %v", count)
	}
	if status, _, raw := ts.request("/api/again", M{"mode": "elsewhere"}, true, nil); status != 400 ||
		decode(t, raw)["error"] != "choose where to run again: same-folder or fresh-folder" {
		t.Fatalf("bad mode %d %s", status, raw)
	}
}

func TestRestartStopsTheRunThenStartsANewAttempt(t *testing.T) {
	ts := newTestServer(t, "8")
	ts.request("/api/start", M{"plan": util.Get(ts.state(), "plan", "sha")}, true, nil)
	until(t, 30*time.Second, func() bool {
		runs := util.Maps(ts.state()["runs"])
		return len(runs) > 0 && len(util.List(runs[0]["feed"])) > 0
	})
	os.Setenv("AJX_DEMO_PACE", "0")
	if status, _, raw := ts.request("/api/restart", M{"mode": "same-folder"}, true, nil); status != 202 {
		t.Fatalf("restart %d %s", status, raw)
	}
	var snap M
	until(t, 90*time.Second, func() bool { snap = ts.state(); return complete(snap, 2) && !ts.s.app.checking.Load() })
	runs := util.Maps(snap["runs"])
	if util.Get(runs[0], "execute", "interrupted") != true || util.Get(runs[1], "verify", "outcome") != "succeeded" {
		t.Fatalf("runs %v / %v", runs[0]["execute"], runs[1]["verify"])
	}
	for _, kind := range []string{"restart_requested", "stop_requested", "run_again"} {
		if !util.Contains(ts.kinds(), kind) {
			t.Fatalf("no %s", kind)
		}
	}
}

func TestShutdownStopsTheServerButNotRuns(t *testing.T) {
	d := newDemo(t, "0")
	s, err := newServer(d.trial, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.serve(); s.finish() }()
	ts := &testServer{demo: d, s: s}
	status, _, raw := ts.request("/api/shutdown", M{}, true, nil)
	if status != 200 || !strings.Contains(util.S(decode(t, raw)["reopen"]), "ajx console open") {
		t.Fatalf("shutdown %d %s", status, raw)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("server kept running")
	}
	time.Sleep(100 * time.Millisecond)
	if exists(serverPath(d.trial)) {
		t.Fatal("server.json left behind")
	}
	k := d.kinds()
	if k[len(k)-1] != "console_stopped" || !util.Contains(k, "shutdown_requested") {
		t.Fatalf("kinds %v", k)
	}
}

func TestIdleServersStopThemselves(t *testing.T) {
	d := newDemo(t, "0")
	done := make(chan error, 1)
	go func() { done <- Serve(d.trial, ServeOpts{Idle: 300 * time.Millisecond, Announce: func(string) {}}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("idle server kept running")
	}
}

// ---------------------------------------------------------------- background consoles

func TestOpenReusesTheBackgroundServerAndStopEndsIt(t *testing.T) {
	d := newDemo(t, "0")
	first, err := OpenConsole(d.trial, OpenOpts{})
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			StopConsole(d.trial)
		}
	})
	if first["started"] != true {
		t.Fatalf("first %v", first)
	}
	again, err := OpenConsole(filepath.Dir(d.trial), OpenOpts{})
	if err != nil || again["pid"] != first["pid"] || again["started"] != nil {
		t.Fatalf("again %v %v", again, err)
	}
	if info := ServerInfo(filepath.Dir(d.trial)); info == nil || info["pid"] != first["pid"] {
		t.Fatalf("server info %v", info)
	}
	found := false
	for _, c := range ListConsoles() {
		if c["pid"] == first["pid"] {
			found = c["url"] == first["url"] && c["trial"] == d.trial
		}
	}
	if !found {
		t.Fatalf("list %v", ListConsoles())
	}
	view, _ := Since(d.trial, 0, false)
	if util.Get(view, "console", "running") != true || !strings.HasSuffix(util.S(util.Get(view, "console", "results_url")), "&tab=results") {
		t.Fatalf("since console %v", view["console"])
	}
	ok, err := StopConsole(d.trial)
	stopped = true
	if err != nil || !ok {
		t.Fatalf("stop %v %v", ok, err)
	}
	if ServerInfo(d.trial) != nil {
		t.Fatal("server still answers")
	}
	if ok, _ := StopConsole(d.trial); ok {
		t.Fatal("second stop found a server")
	}
}

func TestStopAllStopsEveryConsoleServerAndNoRun(t *testing.T) {
	tmp := t.TempDir()
	var trials []string
	var pids []int
	for _, name := range []string{"one", "two"} {
		trial, err := CopyDemo(filepath.Join(tmp, name), false)
		if err != nil {
			t.Fatal(err)
		}
		info, err := OpenConsole(trial, OpenOpts{})
		if err != nil {
			t.Fatal(err)
		}
		trials = append(trials, trial)
		pids = append(pids, util.Int(info["pid"], 0))
	}
	var ours []M
	for _, c := range ListConsoles() {
		for _, p := range pids {
			if c["pid"] == p {
				ours = append(ours, c)
			}
		}
	}
	if len(ours) != 2 {
		t.Fatalf("ours %v", ours)
	}
	saved := listConsoles
	listConsoles = func() []M { return ours } // never touch consoles outside the test
	defer func() { listConsoles = saved }()
	if n := StopAll(); n != 2 {
		t.Fatalf("stopped %d", n)
	}
	for _, trial := range trials {
		if ServerInfo(trial) != nil {
			t.Fatalf("%s still running", trial)
		}
	}
}

func TestCopyDemoRefusesANonEmptyFolder(t *testing.T) {
	tmp := t.TempDir()
	dest := filepath.Join(tmp, "d")
	trial, err := CopyDemo(dest, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"trial.toml", "task-prompt.md", "fixture/orders.json", "plugins/console_demo.py", "expected.csv"} {
		if !isFile(filepath.Join(dest, rel)) {
			t.Fatalf("missing %s", rel)
		}
	}
	if exists(filepath.Join(dest, "plugins", "__pycache__")) || exists(filepath.Join(dest, StateDirName)) {
		t.Fatal("copied caches or console state")
	}
	if _, err := CopyDemo(dest, false); err == nil || !strings.HasSuffix(err.Error(), "is not empty; choose a new folder or pass --force") {
		t.Fatalf("refusal: %v", err)
	}
	if again, err := CopyDemo(dest, true); err != nil || again != trial {
		t.Fatalf("force: %v %v", again, err)
	}
}

func TestTrialFileAndShort(t *testing.T) {
	d := newDemo(t, "0")
	if got, err := TrialFile(filepath.Dir(d.trial)); err != nil || got != d.trial {
		t.Fatalf("TrialFile(dir) %v %v", got, err)
	}
	if _, err := TrialFile(filepath.Join(d.tmp, "nope")); !isConsoleErr(err) || !strings.HasPrefix(err.Error(), "trial file not found: ") {
		t.Fatalf("missing: %v", err)
	}
	home := util.Home()
	if Short(home) != "~" || Short(filepath.Join(home, "x")) != "~/x" || Short("/elsewhere") != "/elsewhere" || Short(home+"x") != home+"x" {
		t.Fatal("Short")
	}
	st, err := os.Stat(StateDir(d.trial))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("state dir %v %v", st, err)
	}
	writePrivate(serverPath(d.trial), M{"pid": 1})
	if st, _ := os.Stat(serverPath(d.trial)); st.Mode().Perm() != 0o600 {
		t.Fatalf("server.json mode %v", st.Mode())
	}
}
