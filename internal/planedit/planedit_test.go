package planedit

// Ports skills/ajx/tests/test_plan_edit.py (RoundTripTest, EditTest): reading
// the model, writing it back section by section. Synthetic copies of the
// bundled examples only; Docker is never asked.

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "github.com/royosherove/ajx/internal/builtin"
	_ "github.com/royosherove/ajx/internal/cloudauth"
	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

func TestMain(m *testing.M) {
	StubDocker(nil, "") // never ask Docker in tests
	os.Exit(m.Run())
}

func examples(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "skills", "ajx", "examples")
}

// copyExample copies a bundled example (without reports, console state or caches) and loads its plugins.
func copyExample(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join(examples(t), name)
	dst := filepath.Join(resolved(t.TempDir()), name)
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() && (d.Name() == "ajx-reports" || d.Name() == ".ajx-console" || d.Name() == "__pycache__") {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, _ := d.Info()
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
	plugin.Load(dst)
	return filepath.Join(dst, "trial.toml")
}

func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeFile(t *testing.T, p, text string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func editText(t *testing.T, trial, old, new string) {
	t.Helper()
	text := readFile(t, trial)
	if strings.Count(text, old) != 1 {
		t.Fatalf("%q appears %d times", old, strings.Count(text, old))
	}
	writeFile(t, trial, strings.Replace(text, old, new, 1))
}

func model(t *testing.T, trial string) *Dict {
	t.Helper()
	m, err := ReadModel(trial)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func sub(d *Dict, key string) *Dict { return d.Get(key).(*Dict) }

func list(d *Dict, key string) []any { return d.Get(key).([]any) }

func worker(m *Dict, i int) *Dict { return list(m, "workers")[i].(*Dict) }

func render(t *testing.T, trial string, m *Dict) *Rendered {
	t.Helper()
	out, err := Render(trial, m, RenderOpts{})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// write renders m, and loads the result beside the trial as AJX would.
func write(t *testing.T, trial string, m *Dict) (string, util.M, []string) {
	t.Helper()
	out := render(t, trial, m)
	edited := filepath.Join(filepath.Dir(trial), "edited.toml")
	writeFile(t, edited, out.Text)
	sp, err := spec.Load(edited, spec.LoadOpts{SkipReadiness: true})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.Text)
	}
	return out.Text, sp, out.AddedAuth
}

func parsed(t *testing.T, text string) *Dict {
	t.Helper()
	d, err := ParseTOML(text)
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	return d
}

func changeTexts(t *testing.T, old, new *Dict) []string {
	t.Helper()
	list, err := Changes(old, new)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range list {
		out = append(out, c.Text)
	}
	return out
}

// ---------------------------------------------------------------- round trip

func TestAnUnchangedModelWritesBackTheSameBytes(t *testing.T) {
	for _, name := range []string{"console-demo", "environment-profiles", "aws-cloud", "jq-smoke"} {
		trial := copyExample(t, name)
		out := render(t, trial, model(t, trial))
		if out.Text != readFile(t, trial) || len(out.AddedAuth) != 0 {
			t.Errorf("%s: rewritten\n%s", name, out.Text)
		}
	}
}

func TestTheModelReadsLayersFromTheFile(t *testing.T) {
	m := model(t, copyExample(t, "environment-profiles"))
	envs := list(m, "envs")
	if len(envs) != 1 || envs[0].(*Dict).Get("id") != "developer" || envs[0].(*Dict).Get("kind") != "profile" {
		t.Fatalf("envs %s", Dumps(envs))
	}
	got := map[string][2]any{}
	for _, w := range list(m, "workers") {
		d := w.(*Dict)
		got[d.Get("id").(string)] = [2]any{d.Get("env"), d.Get("skills")}
	}
	if got["plain"] != [2]any{"developer", "none"} || got["guided"] != [2]any{"developer", "folders"} {
		t.Fatalf("workers %v", got)
	}
	if !Equal(worker(m, 1).Get("skillPaths"), []any{"skills/csv-guide"}) {
		t.Fatalf("paths %v", worker(m, 1).Get("skillPaths"))
	}
}

func TestAProfileNamedLocalDoesNotCollideWithTheFolderOnYourMachine(t *testing.T) {
	trial := copyExample(t, "console-demo")
	writeFile(t, trial, readFile(t, trial)+"\n[environments.local]\nbackend = \"local\"\n")
	m := model(t, trial)
	var ids []string
	for _, e := range list(m, "envs") {
		ids = append(ids, e.(*Dict).Get("id").(string))
	}
	if strings.Join(ids, ",") != FolderEnv+",local" {
		t.Fatalf("envs %v", ids)
	}
	sub(m, "run").Set("attempts", int64(2))
	if got := sub(parsed(t, render(t, trial, m).Text), "trial").Get("repetitions"); !Equal(got, 2) {
		t.Fatalf("repetitions %v", got)
	}
}

func TestTOMLDatesKeepThePageWorkingWithoutTheEditor(t *testing.T) {
	trial := copyExample(t, "console-demo")
	writeFile(t, trial, strings.Replace(readFile(t, trial), "[task]", "[task]\nwritten = 2024-05-01", 1))
	if _, err := ReadModel(trial); err == nil || !strings.Contains(err.Error(), "date") {
		t.Fatalf("err %v", err)
	}
}

func TestModelAliasesResolveForWorkerComparisonsAndReporter(t *testing.T) {
	trial := copyExample(t, "console-demo")
	m := model(t, trial)
	worker(m, 0).Set("cli", "kiro-cli")
	worker(m, 0).Set("model", "opus")
	sub(m, "reporter").Set("cli", "codex")
	sub(m, "reporter").Set("model", "astra")
	raw := parsed(t, render(t, trial, m).Text)
	if got := list(raw, "cells")[0].(*Dict).Get("model"); got != "claude-opus-5.5" {
		t.Fatalf("worker model: %v", got)
	}
	if got := sub(raw, "reporter").Get("model"); got != "gpt-6-astra" {
		t.Fatalf("reporter model: %v", got)
	}
	worker(m, 0).Set("models", []any{"sonnet", "opus", "gateway/MyModel"})
	raw = parsed(t, render(t, trial, m).Text)
	if got := list(raw, "cells")[0].(*Dict).Get("models"); !Equal(got, []any{"claude-sonnet-5.5", "claude-opus-5.5", "gateway/MyModel"}) {
		t.Fatalf("comparison models: %v", got)
	}
	worker(m, 0).Set("cli", "codex")
	if _, err := Render(trial, m, RenderOpts{}); err == nil || !strings.Contains(err.Error(), "not a model alias for codex") {
		t.Fatalf("cross-harness model: %v", err)
	}
}

// ---------------------------------------------------------------- edits

func TestAModelChangeRewritesOnlyItsSectionsAndKeepsOtherComments(t *testing.T) {
	trial := copyExample(t, "console-demo")
	m := model(t, trial)
	worker(m, 0).Set("model", "opus")
	m.Set("checks", append(list(m, "checks"), NewDict("name", "out.csv exists", "type", "file", "path", "out.csv", "where", "host")))
	sub(m, "run").Set("attempts", int64(3))
	text, sp, _ := write(t, trial, m)
	if util.Get(util.Maps(sp["cells"])[0], "model") != "opus" || util.Int(util.Get(sp, "trial", "repetitions"), 0) != 3 {
		t.Fatalf("spec %v", sp["cells"])
	}
	checks := util.Maps(sp["verify"])
	if checks[len(checks)-1]["name"] != "out.csv exists" {
		t.Fatalf("checks %v", checks)
	}
	if !strings.Contains(text, "# AJX Console demo.") { // the file's own comments outside the edits survive
		t.Fatalf("comment lost\n%s", text)
	}
	changes := strings.Join(changeTexts(t, model(t, trial), m), "\n")
	for _, want := range []string{"worker demo · model: default -> opus", "run settings · attempts: 1 -> 3"} {
		if !strings.Contains(changes, want) {
			t.Errorf("changes lack %q:\n%s", want, changes)
		}
	}
}

func TestMultiLineCommandsStayReadableAndExact(t *testing.T) {
	trial := copyExample(t, "console-demo")
	m := model(t, trial)
	script := "set -eu\n[ -f out.csv ] && echo \"one\"\nprintf %s \\\\\nrm -rf \"$AJX_RUN_TOKEN\""
	sub(m, "account").Set("teardown", []any{NewDict("run", script)})
	text, sp, _ := write(t, trial, m)
	if !strings.Contains(text, "'''\n"+script+"'''") || util.Maps(sp["teardown"])[0]["run"] != script {
		t.Fatalf("teardown\n%s", text)
	}
	writeFile(t, trial, text) // a header-like line inside the command is not a section
	again := model(t, trial)
	sub(again, "run").Set("minutes", int64(4))
	if _, sp, _ := write(t, trial, again); util.Maps(sp["teardown"])[0]["run"] != script {
		t.Fatalf("teardown after another edit %v", sp["teardown"])
	}
}

func TestMultiLineValuesAndSubtablesAreEditedWhole(t *testing.T) {
	trial := copyExample(t, "console-demo")
	editText(t, trial, `materials = ["orders-cli --help"]`, "materials = [\n  \"orders-cli --help\",\n  \"README.md\",\n]")
	writeFile(t, trial, readFile(t, trial)+"\n[[setup]]\nrun = \"true\"\n\n[setup.env]\nA = \"1\"\n")
	m := model(t, trial)
	sub(m, "task").Set("materials", []any{"orders-cli --help"})
	sub(m, "task").Set("setup", append(list(sub(m, "task"), "setup"), NewDict("run", "echo two", "name", "", "extra", NewDict())))
	text, sp, _ := write(t, trial, m)
	if !Equal(FromM(util.Get(sp, "task", "materials")), []any{"orders-cli --help"}) {
		t.Fatalf("materials %v", util.Get(sp, "task", "materials"))
	}
	want := []any{NewDict("run", "true", "env", NewDict("A", "1")), NewDict("run", "echo two")}
	if got := parsed(t, text).Get("setup"); !Equal(got, want) {
		t.Fatalf("setup %s", Dumps(got))
	}
}

func TestASingleModelListKeepsItsFormSoCellIDsDoNotChange(t *testing.T) {
	trial := copyExample(t, "console-demo")
	editText(t, trial, "id = \"demo\"\nharness = \"console-demo\"", "id = \"demo\"\nharness = \"console-demo\"\nmodels = [\"demo-model\"]")
	m := model(t, trial)
	worker(m, 0).Set("effort", "high")
	_, sp, _ := write(t, trial, m)
	if ids := util.Maps(sp["cells"]); len(ids) != 1 || ids[0]["id"] != "demo-demo-model" {
		t.Fatalf("cells %v", sp["cells"])
	}
}

func TestAContainerWorkerGetsAnEnvironmentAndASignInRouteButNoNeedlessProfile(t *testing.T) {
	trial := copyExample(t, "console-demo")
	m := model(t, trial)
	m.Set("envs", append(list(m, "envs"), NewDict("id", "sealed", "kind", "container", "image", "sha256:"+strings.Repeat("a", 64),
		"network", "none", "required", []any{"filesystem_isolation"}, "limits", ContainerLimits(),
		"tools", []any{NewDict("name", "python3", "required", true)})))
	w := DeepCopy(worker(m, 0)).(*Dict)
	for k, v := range map[string]string{"id": "sealed-claude", "cli": "claude-code", "env": "sealed", "auth": "bedrock-keys", "model": "sonnet"} {
		w.Set(k, v)
	}
	m.Set("workers", append(list(m, "workers"), w))
	out := render(t, trial, m)
	if strings.Join(out.AddedAuth, ",") != "bedrock-keys" {
		t.Fatalf("added %v", out.AddedAuth)
	}
	raw := parsed(t, out.Text)
	sealed := sub(sub(raw, "environments"), "sealed")
	if sealed.Get("backend") != "container" || sealed.Has("limits") { // the defaults stay implicit
		t.Fatalf("environment %s", Dumps(sealed))
	}
	cells := list(raw, "cells")
	cell := cells[len(cells)-1].(*Dict)
	if cell.Get("environment") != "sealed" || cell.Get("config") != "clean" || cell.Has("agent_configuration") || raw.Has("agent_configurations") {
		t.Fatalf("cell %s", Dumps(cell))
	}
	if sub(sub(raw, "auth"), "bedrock-keys").Get("type") != "claude-bedrock" || strings.Contains(out.Text, "AKIA") {
		t.Fatalf("auth\n%s", out.Text) // credentials are references to the terminal, never values
	}
}

func TestSkillsGetTheirOwnProfileWithoutReplacingAnotherWorkers(t *testing.T) {
	trial := copyExample(t, "environment-profiles")
	m := model(t, trial)
	worker(m, 0).Set("skills", "folders")
	worker(m, 0).Set("skillPaths", []any{"skills/csv-guide", "skills/other"})
	raw := parsed(t, render(t, trial, m).Text)
	configs := sub(raw, "agent_configurations")
	if !Equal(sub(sub(configs, "plain"), "skills").Get("paths"), []any{"skills/csv-guide", "skills/other"}) ||
		!Equal(sub(sub(configs, "guided"), "skills").Get("paths"), []any{"skills/csv-guide"}) {
		t.Fatalf("configurations %s", Dumps(configs))
	}
}

func TestRemovingAWorkerKeepsItsProfileSoRecordedAttemptsStayComparable(t *testing.T) {
	trial := copyExample(t, "environment-profiles")
	before := sub(parsed(t, readFile(t, trial)), "agent_configurations").Keys()
	m := model(t, trial)
	m.Set("workers", []any{worker(m, 0)})
	_, sp, _ := write(t, trial, m)
	cells := util.Maps(sp["cells"])
	if len(cells) != 1 || cells[0]["id"] != "plain" {
		t.Fatalf("cells %v", cells)
	}
	if got := util.SortedKeys(util.MapOr(sp["agent_configurations"])); strings.Join(got, ",") != strings.Join(sortedStrings(before), ",") {
		t.Fatalf("configurations %v, want %v", got, before)
	}
}

func TestEditsThatCannotWorkAreRefusedWithAReason(t *testing.T) {
	trial := copyExample(t, "console-demo")
	cases := []struct {
		edit   func(m *Dict)
		reason string
	}{
		{func(m *Dict) { worker(m, 0).Set("model", "sonnet 5.5") }, "not a model name"},
		{func(m *Dict) {
			worker(m, 0).Set("skills", "folders")
			worker(m, 0).Set("skillPaths", []any{"skills/x"})
		}, "fresh-profile or container"},
		{func(m *Dict) { m.Set("workers", append(list(m, "workers"), worker(m, 0).Copy())) }, "unique"},
		{func(m *Dict) { sub(m, "run").Set("attempts", 1.5) }, "whole number"},
		{func(m *Dict) { sub(m, "run").Set("minutes", "x") }, "must be a number"},
		{func(m *Dict) { list(m, "checks")[0].(*Dict).Set("expect_exit", "x") }, "must be a number"},
		{func(m *Dict) { worker(m, 0).Delete("env") }, "cannot write"},
		{func(m *Dict) { m.Set("workers", []any{}) }, "at least one worker"},
	}
	for _, c := range cases {
		m := model(t, trial)
		c.edit(m)
		_, err := Render(trial, m, RenderOpts{})
		if _, ok := err.(*Error); !ok || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("want %q, got %v", c.reason, err)
		}
	}
}

func TestAnOddValueTheUserDidNotTouchNeverBlocksAnEdit(t *testing.T) {
	trial := copyExample(t, "console-demo")
	editText(t, trial, "timeout_seconds = 300", "timeout_seconds = 5")
	m := model(t, trial)
	sub(m, "run").Set("attempts", int64(2))
	if text := render(t, trial, m).Text; !strings.Contains(text, "timeout_seconds = 5") {
		t.Fatalf("\n%s", text)
	}
}

func TestValuesSpecAcceptsButTheEditorWouldRefuseBlockOnlyTheirOwnEdit(t *testing.T) {
	trial := copyExample(t, "console-demo")
	editText(t, trial, "id = \"demo\"\nharness = \"console-demo\"", "id = \"demo\"\nharness = \"console-demo\"\nmodel = \"my model\"")
	m := model(t, trial)
	sub(m, "run").Set("attempts", int64(2))
	text := render(t, trial, m).Text
	if !strings.Contains(text, `model = "my model"`) || !strings.Contains(text, "repetitions = 2") {
		t.Fatalf("\n%s", text)
	}
	worker(m, 0).Set("effort", "high") // touching that worker validates it again
	if _, err := Render(trial, m, RenderOpts{}); err == nil || !strings.Contains(err.Error(), "not a model name") {
		t.Fatalf("err %v", err)
	}
}

func TestAnUntouchedCheckValueSpecAcceptsNeverBlocksAnEdit(t *testing.T) {
	trial := copyExample(t, "console-demo")
	editText(t, trial, `run = "cmp -s out.csv`, "expect_exit = true\nrun = \"cmp -s out.csv")
	m := model(t, trial)
	sub(m, "run").Set("minutes", int64(3))
	text := render(t, trial, m).Text
	if !strings.Contains(text, "expect_exit = true") || !strings.Contains(text, "timeout_seconds = 180") {
		t.Fatalf("\n%s", text)
	}
}

func TestUnchangedWorkersKeepTheirCellTablesExactly(t *testing.T) {
	trial := copyExample(t, "console-demo")
	editText(t, trial, "id = \"demo\"\nharness = \"console-demo\"", "id = \"demo\"\nharness = \"console-demo\"\neffort = \"\"")
	m := model(t, trial)
	extra := DeepCopy(worker(m, 0)).(*Dict)
	extra.Set("id", "demo-two")
	m.Set("workers", append(list(m, "workers"), extra))
	cells := list(parsed(t, render(t, trial, m).Text), "cells")
	if Dumps(cells[0]) != `{"id": "demo", "harness": "console-demo", "effort": ""}` || cells[1].(*Dict).Get("id") != "demo-two" {
		t.Fatalf("cells %s", Dumps(cells))
	}
}

func TestAReporterLeftAutomaticStaysAutomatic(t *testing.T) {
	trial := copyExample(t, "console-demo")
	editText(t, trial, "[reporter]\nharness = \"console-demo\"\n", "")
	m := model(t, trial)
	if sub(m, "reporter").Get("cli") != "" {
		t.Fatalf("reporter %s", Dumps(sub(m, "reporter")))
	}
	sub(m, "reporter").Set("model", "demo-model")
	if got := Dumps(sub(parsed(t, render(t, trial, m).Text), "reporter")); got != `{"model": "demo-model"}` {
		t.Fatalf("reporter %s", got)
	}
}

// ---------------------------------------------------------------- pieces

func TestOptionsOfferAgentsSignInRoutesAndTheAutomaticReporter(t *testing.T) {
	trial := copyExample(t, "console-demo")
	m := model(t, trial)
	opts, err := Options(trial, m)
	if err != nil {
		t.Fatal(err)
	}
	names := map[any]bool{}
	for _, h := range list(opts, "harnesses") {
		names[h.(*Dict).Get("name")] = true
	}
	if !names["claude-code"] || !names["console-demo"] || opts.Get("auto_reporter") != "console-demo" {
		t.Fatalf("options %s", Dumps(opts))
	}
	if len(list(opts, "bash_only")) == 0 {
		t.Fatal("no bash-only patterns")
	}
	var templates []string
	for _, r := range sub(opts, "auth").Get("claude-code").([]any) {
		if r.(*Dict).Get("kind") == "template" {
			templates = append(templates, r.(*Dict).Get("value").(string))
		}
	}
	if strings.Join(templates, ",") != "bedrock-keys" {
		t.Fatalf("templates %v", templates)
	}
}

func TestStringLinesAndSections(t *testing.T) {
	lines := strings.Split("[trial]\nx = '''\n[cells]\n'''\n[[cells]]\ny = \"\"\"a\\\"\"\"\"\n[\"q\"]\n", "\n")
	inside := StringLines(lines)
	if !inside[2] || !inside[3] || inside[4] || inside[5] {
		t.Fatalf("inside %v", inside)
	}
	var got []string
	for _, s := range Sections(lines) {
		got = append(got, s.Name+":"+map[bool]string{true: "a", false: "t"}[s.Array])
	}
	if strings.Join(got, " ") != ":t trial:t cells:a \"q\":t" {
		t.Fatalf("sections %v", got)
	}
}

func TestDiffLinesShowOneLineOfContext(t *testing.T) {
	got := DiffLines("a\nb\nc\nd\ne\n", "a\nb\nC\nd\ne\n", "trial.toml")
	want := []string{"--- trial.toml (now)", "+++ trial.toml (after saving)", "@@ -2,3 +2,3 @@", " b", "-c", "+C", " d"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("diff %q", got)
	}
	if got := DiffLines("same\n", "same\n", "x"); len(got) != 0 {
		t.Fatalf("no change %q", got)
	}
}
