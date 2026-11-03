package spec

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

func canon(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	out, err := util.DecodeJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

const head = "[trial]\nid = \"t1\"\nproduct = \"p\"\n[task]\nprompt_file = \"task-prompt.md\"\n[[cells]]\nid = \"a\"\nharness = \"claude-code\"\n"

func write(t *testing.T, dir, extra string) string {
	t.Helper()
	_ = os.WriteFile(filepath.Join(dir, "task-prompt.md"), []byte("do the thing\n"), 0o644)
	p := filepath.Join(dir, "trial.toml")
	if err := os.WriteFile(p, []byte(head+extra), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// trialAt is test_ajx.py's _trial helper.
func trialAt(t *testing.T, dir, body, cells string) string {
	t.Helper()
	if cells == "" {
		cells = "[[cells]]\nid = \"a\"\nharness = \"claude-code\"\n"
	}
	_ = os.WriteFile(filepath.Join(dir, "task-prompt.md"), []byte("x\n"), 0o644)
	p := filepath.Join(dir, "trial.toml")
	text := "[trial]\nid = \"t\"\nproduct = \"p\"\nworkspace_root = \"" + dir + "/ws\"\n[task]\nprompt_file = \"task-prompt.md\"\n" + body + "\n" + cells
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustLoad(t *testing.T, path string) M {
	t.Helper()
	sp, err := Load(path, LoadOpts{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return sp
}

func wantSpecError(t *testing.T, path string, contains string) {
	t.Helper()
	_, err := Load(path, LoadOpts{})
	if !IsError(err) {
		t.Fatalf("want spec.Error, got %v", err)
	}
	if contains != "" && !strings.Contains(err.Error(), contains) {
		t.Fatalf("error %q lacks %q", err, contains)
	}
}

func cellIDs(sp M) []string {
	var out []string
	for _, c := range util.Maps(sp["cells"]) {
		out = append(out, util.Str(c["id"]))
	}
	return out
}

func TestLoadAndPlan(t *testing.T) {
	sp := mustLoad(t, write(t, t.TempDir(), "[[cells]]\nid = \"b\"\nharness = \"kiro-cli\"\n[trial.x]\n"))
	if len(util.List(sp["cells"])) != 2 || len(util.Str(util.Get(sp, "task", "prompt_sha256"))) != 64 {
		t.Fatalf("cells %v", sp["cells"])
	}
	util.MapOr(sp["trial"])["repetitions"] = int64(2)
	plan, err := RunPlan(sp, nil)
	if err != nil {
		t.Fatal(err)
	}
	var reps []int
	for _, p := range plan {
		reps = append(reps, p.Rep)
	}
	if !reflect.DeepEqual(reps, []int{1, 1, 2, 2}) {
		t.Fatalf("reps %v", reps)
	}
}

func TestRejectsUnknownHarnessAndBadAuth(t *testing.T) {
	wantSpecError(t, write(t, t.TempDir(), "[[cells]]\nid = \"b\"\nharness = \"nope\"\n"), "unknown")
	wantSpecError(t, write(t, t.TempDir(), "[[cells]]\nid = \"b\"\nharness = \"claude-code\"\nauth = \"openai-api\"\n"),
		"auth type 'openai-api' is for ('codex',), not claude-code")
}

func TestDeclarativeAdapterAndAuthProfile(t *testing.T) {
	t.Setenv("AJX_TEST_KEY", "sekret")
	sp := mustLoad(t, write(t, t.TempDir(), `
[adapters.mytool]
execute = ["mytool", "run", "{prompt}"]
[auth.gw]
type = "env"
env = { MY_KEY = "${AJX_TEST_KEY}" }
[[cells]]
id = "d"
harness = "mytool"
auth = "gw"
`))
	cell := util.Maps(sp["cells"])[1]
	auth, err := AuthFor(sp, cell, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(auth.Env(), map[string]string{"MY_KEY": "sekret"}) {
		t.Fatalf("env %v", auth.Env())
	}
	if got := canon(t, auth.Describe()["env_names"]); !reflect.DeepEqual(got, []any{"MY_KEY"}) {
		t.Fatalf("env_names %v", got)
	}
	if auth.Base().Profile != "gw" || auth.Base().Harness != "mytool" {
		t.Fatalf("profile %q harness %q", auth.Base().Profile, auth.Base().Harness)
	}
	h, err := HarnessFor(sp, "mytool")
	if err != nil {
		t.Fatal(err)
	}
	if h.Name() != "mytool" || h.Base().CanResume {
		t.Fatalf("harness %s resume=%v", h.Name(), h.Base().CanResume)
	}
	if !util.Contains(HarnessNames(sp), "mytool") || !util.Contains(HarnessNames(sp), "claude-code") {
		t.Fatal(HarnessNames(sp))
	}
}

func TestModelsExpandAndPlanByGroup(t *testing.T) {
	dir := t.TempDir()
	sp := mustLoad(t, trialAt(t, dir, "", "[[cells]]\nid = \"cc\"\nharness = \"claude-code\"\nmodels = [\"sonnet\", \"opus\"]\n"+
		"[[cells]]\nid = \"codex\"\nharness = \"codex\"\nmodels = [\"example-model-a\", \"example-model-b\"]\n"))
	var got [][3]string
	for _, c := range util.Maps(sp["cells"]) {
		got = append(got, [3]string{util.Str(c["id"]), util.Str(c["model"]), util.Str(c["group"])})
		if _, has := c["models"]; has {
			t.Fatal("models key kept")
		}
	}
	want := [][3]string{{"cc-sonnet", "sonnet", "cc"}, {"cc-opus", "opus", "cc"},
		{"codex-example-model-a", "example-model-a", "codex"}, {"codex-example-model-b", "example-model-b", "codex"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cells %v", got)
	}
	plan, err := RunPlan(sp, []string{"cc"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range plan {
		ids = append(ids, util.Str(p.Cell["id"]))
	}
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, []string{"cc-opus", "cc-sonnet"}) {
		t.Fatal(ids)
	}
	plan, _ = RunPlan(sp, []string{"codex-example-model-b"})
	if len(plan) != 1 || plan[0].Cell["id"] != "codex-example-model-b" {
		t.Fatal(plan)
	}
	if _, err := RunPlan(sp, []string{"zz"}); !IsError(err) ||
		err.Error() != "unknown cell id(s) ['zz']; cells are ['cc', 'cc-opus', 'cc-sonnet', 'codex', 'codex-example-model-a', 'codex-example-model-b']" {
		t.Fatalf("err %v", err)
	}
}

func TestModelsRejectAmbiguousOrBadCells(t *testing.T) {
	for _, cell := range []string{"id = \"cc\"\nharness = \"claude-code\"\nmodel = \"opus\"\nmodels = [\"sonnet\"]",
		"id = \"cc\"\nharness = \"claude-code\"\nmodels = []",
		"id = \"cc\"\nharness = \"claude-code\"\nmodels = [\"opus\", \"opus\"]",
		"id = \"cc\"\nharness = \"claude-code\"\nmodels = [\"" + strings.Repeat("x", 60) + "\"]"} {
		wantSpecError(t, trialAt(t, t.TempDir(), "", "[[cells]]\n"+cell+"\n"), "")
	}
	wantSpecError(t, trialAt(t, t.TempDir(), "", "[[cells]]\nid = \"cc-opus\"\nharness = \"claude-code\"\n"+
		"[[cells]]\nid = \"cc\"\nharness = \"claude-code\"\nmodels = [\"opus\"]\n"), "duplicate cell id cc-opus")
}

func TestTaskIdentityCmdAndPreflightParse(t *testing.T) {
	dir := t.TempDir()
	sp := mustLoad(t, trialAt(t, dir, "identity_cmd = [\"aws\", \"sts\", \"get-caller-identity\", \"--profile\", \"my sb\"]\n"+
		"[[preflight]]\nrun = \"true\"\n", ""))
	if got := util.Get(sp, "task", "identity_cmd"); got != "aws sts get-caller-identity --profile 'my sb'" {
		t.Fatalf("identity_cmd %v", got)
	}
	if util.Maps(sp["preflight"])[0]["run"] != "true" {
		t.Fatal(sp["preflight"])
	}
	wantSpecError(t, trialAt(t, dir, "identity_cmd = 3\n", ""), "identity_cmd")
}

func TestRejectsNonShellPreflight(t *testing.T) {
	wantSpecError(t, trialAt(t, t.TempDir(), "[[preflight]]\ntype = \"http\"\nurl = \"http://x\"\n", ""),
		"preflight[0].type must be shell")
}

func TestDefaultReporterFollowsFirstCapableCellAndAuth(t *testing.T) {
	sp := mustLoad(t, trialAt(t, t.TempDir(), "[auth.model]\ntype = \"openai-api\"\n",
		"[[cells]]\nid = \"a\"\nharness = \"codex\"\nauth = \"model\"\n"))
	if util.Get(sp, "reporter", "harness") != "codex" || util.Get(sp, "reporter", "auth") != "model" {
		t.Fatal(sp["reporter"])
	}
}

func TestExampleTrialsLoad(t *testing.T) {
	root := "../../skills/ajx/examples"
	var paths []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Name() == "trial.toml" {
			paths = append(paths, p)
		}
		return nil
	})
	if len(paths) == 0 {
		t.Skip("no examples")
	}
	for _, p := range paths {
		sp, err := Load(p, LoadOpts{SkipReadiness: true})
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if len(util.List(sp["cells"])) == 0 {
			t.Errorf("%s: no cells", p)
		}
	}
	aws, err := Load(filepath.Join(root, "aws-cloud", "trial.toml"), LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cellIDs(aws), []string{"cc-sonnet", "cc-opus"}) {
		t.Fatal(cellIDs(aws))
	}
	if len(util.List(aws["preflight"])) == 0 || len(util.List(aws["teardown"])) == 0 ||
		!strings.Contains(util.Str(util.Get(aws, "task", "identity_cmd")), "--profile") {
		t.Fatal("aws example lost its cleanup or identity")
	}
	for k := range util.MapOr(aws["env"]) {
		if strings.HasPrefix(k, "AWS_") {
			t.Fatalf("AWS var in [env]: %s", k)
		}
	}
}

func TestEnvironmentAndConfigurationLookup(t *testing.T) {
	sp := M{"trial": M{"environment": "box", "agent_configuration": nil},
		"environments": M{"box": M{"backend": "docker"}}, "agent_configurations": M{"cfg": M{"x": int64(1)}},
		"_configuration_environment": M{"backend": "local"}}
	if EnvironmentFor(sp, M{})["backend"] != "docker" {
		t.Fatal("trial default environment")
	}
	// a cell key present with None overrides the trial default (dict.get semantics)
	if EnvironmentFor(sp, M{"environment": nil}) != nil {
		t.Fatal("explicit None")
	}
	if EnvironmentFor(sp, M{"environment": nil, "agent_configuration": "cfg"})["backend"] != "local" {
		t.Fatal("configuration environment")
	}
	if AgentConfigurationFor(sp, M{"agent_configuration": "cfg"}) == nil || AgentConfigurationFor(sp, M{}) != nil {
		t.Fatal("agent configuration")
	}
}

func TestWorkerEnvLayersAndRunnerFor(t *testing.T) {
	dir := t.TempDir()
	sp := mustLoad(t, trialAt(t, dir, "[env]\nA = \"trial\"\nB = 1\n[auth.k]\ntype = \"env\"\nenv = { A = \"auth\" }\n",
		"[[cells]]\nid = \"a\"\nharness = \"claude-code\"\nauth = \"k\"\nenv = { B = 2, C = true }\nrunner = \"local\"\n"))
	cell := util.Maps(sp["cells"])[0]
	auth, err := AuthFor(sp, cell, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := WorkerEnv(sp, cell, auth); !reflect.DeepEqual(got, map[string]string{"A": "auth", "B": "2", "C": "True"}) {
		t.Fatalf("env %v", got)
	}
	r, err := RunnerFor(sp, cell)
	if err != nil || r.Name() != "local" {
		t.Fatalf("runner %v %v", r, err)
	}
	if _, err := AuthFor(sp, cell, "nope"); !IsError(err) {
		t.Fatalf("override %v", err)
	}
	inherit, err := AuthFor(sp, cell, "inherit")
	if err != nil || inherit.Name() != "inherit" {
		t.Fatal(err)
	}
}

func TestSavedProfileContractForCleanup(t *testing.T) {
	dir := t.TempDir()
	path := trialAt(t, dir, "", "")
	sp := mustLoad(t, path)
	out := util.Str(util.Get(sp, "trial", "output_dir"))
	prompt := []byte("the original prompt\n")
	_ = os.MkdirAll(out, 0o755)
	_ = os.WriteFile(filepath.Join(out, "task-prompt.md"), prompt, 0o644)
	plan := M{"trial_file": util.Str(sp["path"]), "trial_id": "t", "product": "p",
		"task_prompt_sha256":  util.Sha256Bytes(prompt),
		"normalized_profiles": M{"schema_version": 1, "environments": M{}, "agent_configurations": M{}}}
	_ = util.WriteJSON(filepath.Join(out, "matrix-plan.json"), plan)
	_ = os.Remove(filepath.Join(dir, "task-prompt.md"))
	got, err := Load(path, LoadOpts{CleanupOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if util.Get(got, "task", "prompt_text") != "the original prompt\n" || got["_cleanup_only"] != true {
		t.Fatal(got["task"])
	}
	plan["task_prompt_sha256"] = "x"
	_ = util.WriteJSON(filepath.Join(out, "matrix-plan.json"), plan)
	if _, err := Load(path, LoadOpts{CleanupOnly: true}); !IsError(err) || !strings.Contains(err.Error(), "saved task prompt does not match") {
		t.Fatal(err)
	}
	plan["trial_id"] = "other"
	_ = util.WriteJSON(filepath.Join(out, "matrix-plan.json"), plan)
	if _, err := Load(path, LoadOpts{CleanupOnly: true}); !IsError(err) || !strings.Contains(err.Error(), "belongs to a different trial") {
		t.Fatal(err)
	}
	plan["normalized_profiles"] = M{"schema_version": 2}
	_ = util.WriteJSON(filepath.Join(out, "matrix-plan.json"), plan)
	if _, err := Load(path, LoadOpts{CleanupOnly: true}); !IsError(err) || !strings.Contains(err.Error(), "saved profile contract is invalid") {
		t.Fatal(err)
	}
}

// TestShuffleMatchesPython checks the run order against random.Random(seed).shuffle.
func TestShuffleMatchesPython(t *testing.T) {
	data, err := os.ReadFile("testdata/shuffle.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var cases []struct {
		Seed   json.Number
		N      int
		Orders [][]int
	}
	if err := dec.Decode(&cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		seed, err := strconv.ParseInt(c.Seed.String(), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		rng := newPyRandom(seed)
		for rep, want := range c.Orders {
			order := make([]int, c.N)
			for i := range order {
				order[i] = i
			}
			shuffle(rng, order)
			if !reflect.DeepEqual(order, want) {
				t.Fatalf("seed %d n %d rep %d: got %v want %v", seed, c.N, rep, order, want)
			}
		}
	}
	// and through RunPlan
	var cells []any
	for i := 0; i < 5; i++ {
		cells = append(cells, M{"id": strconv.Itoa(i)})
	}
	plan, err := RunPlan(M{"cells": cells, "trial": M{"order_seed": int64(42), "repetitions": int64(3)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, p := range plan {
		n, _ := strconv.Atoi(util.Str(p.Cell["id"]))
		got = append(got, n)
	}
	for _, c := range cases {
		if c.Seed.String() == "42" && c.N == 5 {
			var want []int
			for _, o := range c.Orders {
				want = append(want, o...)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("plan %v want %v", got, want)
			}
		}
	}
}

// TestLoadMatchesPython replays trial files recorded against spec.load
// (testdata/load_cases.json): the same errors, and the same loaded spec.
func TestLoadMatchesPython(t *testing.T) {
	data, err := os.ReadFile("testdata/load_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := util.DecodeJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range util.Maps(raw) {
		c := c
		t.Run(util.Str(c["name"]), func(t *testing.T) {
			dir := t.TempDir()
			real := Resolve(dir)
			for rel, content := range util.MapOr(c["files"]) {
				p := filepath.Join(dir, rel)
				_ = os.MkdirAll(filepath.Dir(p), 0o755)
				if err := os.WriteFile(p, []byte(util.Str(content)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(dir)
			opts := util.MapOr(c["opts"])
			sp, err := Load(filepath.Join(dir, util.Str(c["trial"])), LoadOpts{CleanupOnly: util.Truthy(opts["cleanup_only"])})
			kind := util.Str(c["kind"])
			if kind != "" {
				if err == nil {
					t.Fatalf("want %s %q, loaded", kind, c["error"])
				}
				switch kind {
				case "SpecError":
					if !IsError(err) {
						t.Fatalf("want SpecError, got %T %v", err, err)
					}
				case "TOMLDecodeError":
					if _, ok := err.(*DecodeError); !ok {
						t.Fatalf("want DecodeError, got %T %v", err, err)
					}
					return // parser messages differ
				default:
					pe, ok := err.(*PyError)
					if !ok || pe.TypeName() != kind {
						t.Fatalf("want %s, got %T %v", kind, err, err)
					}
				}
				got := strings.ReplaceAll(err.Error(), real, "<TMP>")
				// The Go CLI ships the console demo harness natively (Python loads it as a plugin).
				got = strings.ReplaceAll(got, "'console-demo', ", "")
				if got != c["error"] {
					t.Fatalf("error\n got %q\nwant %q", got, c["error"])
				}
				return
			}
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			keep := M{}
			for _, k := range []string{"path", "sha256", "trial", "task", "setup", "verify", "teardown", "preflight",
				"reporter", "env", "adapters", "auth", "runner", "cells", "_cleanup_only"} {
				keep[k] = sp[k]
			}
			b, _ := json.Marshal(keep)
			got, _ := util.DecodeJSON([]byte(strings.ReplaceAll(string(b), real, "<TMP>")))
			want := canon(t, c["spec"])
			for _, k := range util.SortedKeys(util.MapOr(want)) {
				if !reflect.DeepEqual(util.MapOr(got)[k], util.MapOr(want)[k]) {
					gb, _ := json.MarshalIndent(util.MapOr(got)[k], "", " ")
					wb, _ := json.MarshalIndent(util.MapOr(want)[k], "", " ")
					t.Errorf("%s differs\n got %s\nwant %s", k, gb, wb)
				}
			}
		})
	}
}

func TestReprMatchesPython(t *testing.T) {
	for _, c := range []struct {
		v    any
		want string
	}{{"a'b", `"a'b"`}, {`a"b`, `'a"b'`}, {"a'\"b", `'a\'"b'`}, {nil, "None"}, {true, "True"},
		{[]any{1.0, nil, true, "x"}, "[1.0, None, True, 'x']"}, {M{"a": []any{int64(1)}}, "{'a': [1]}"},
		{1e16, "1e+16"}, {1e-5, "1e-05"}, {123456789012345678.0, "1.2345678901234568e+17"}, {0.1, "0.1"}} {
		if got := Repr(c.v); got != c.want {
			t.Errorf("Repr(%v) = %s, want %s", c.v, got, c.want)
		}
	}
	if got := tupleRepr([]string{"claude-code"}); got != "('claude-code',)" {
		t.Fatal(got)
	}
	_ = plugin.Names
}
