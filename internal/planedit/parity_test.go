package planedit

// Side-by-side checks against skills/ajx/lib/ajx/plan_edit.py, run with
// `python3 -I`. Models are built (and edited) in Python, sent to both as the
// same JSON text, and the results compared byte for byte, key order included.
// Synthetic trials only; Docker is stubbed on both sides. Skips without python3.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func pythonLib(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	_, file, _, _ := runtime.Caller(0)
	lib := filepath.Join(filepath.Dir(file), "..", "..", "skills", "ajx", "lib")
	if st, err := os.Stat(filepath.Join(lib, "ajx", "plan_edit.py")); err != nil || st.IsDir() {
		t.Skip("skills/ajx/lib/ajx/plan_edit.py not found")
	}
	abs, _ := filepath.Abs(lib)
	return abs
}

// python runs script with plan_edit importable (Docker stubbed) and returns its
// stdout, decoded with key order kept.
func python(t *testing.T, script string, input any) any {
	t.Helper()
	lib := pythonLib(t)
	path := filepath.Join(t.TempDir(), "parity.py")
	prelude := "import copy, json, sys\nsys.path.insert(0, " + Dumps(lib) + ")\n" +
		"from pathlib import Path\nfrom ajx import plan_edit, plugins\n" +
		"plan_edit._DOCKER.update(at=float('inf'), info={'images': [], 'host': ''})\n" +
		"data = json.loads(sys.stdin.read())\n"
	if err := os.WriteFile(path, []byte(prelude+script), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-I", path)
	cmd.Stdin = strings.NewReader(string(in))
	out, err := cmd.Output()
	if err != nil {
		msg := ""
		if ee, ok := err.(*exec.ExitError); ok {
			msg = string(ee.Stderr)
		}
		t.Fatalf("python: %v\n%s", err, msg)
	}
	v, err := DecodeJSON(out)
	if err != nil {
		t.Fatalf("python output: %v\n%s", err, out)
	}
	return v
}

func sameText(t *testing.T, what, goText, pyText string) {
	t.Helper()
	if goText != pyText {
		t.Errorf("%s differs:\n go     %s\n python %s", what, goText, pyText)
	}
}

// tomlError trims a parser's own wording from a message: only the editor's text must match.
func tomlError(msg string) string {
	for _, prefix := range []string{"could not update ", "the edit produced invalid TOML", "not valid TOML"} {
		if strings.HasPrefix(msg, prefix) && strings.Contains(msg, " (") {
			return msg[:strings.Index(msg, " (")] + " (…)"
		}
	}
	return msg
}

var tomlSamples = []string{
	"z = 1\n[trial]\nid = \"x\"\nb = 2\na = { q = 1, p = { y = 2, x = 3 } }\n",
	"[[cells]]\nid = \"a\"\nmodel = \"m\"\nharness = \"h\"\n[[cells]]\nharness = \"h2\"\nid = \"b\"\n" +
		"tools = [{ name = \"n\", argv = [\"a\"] }, { argv = [\"b\"], name = \"m\" }, { required = false }]\n" +
		"[cells.sub]\nk = 1\nj = 2\n[[cells.list]]\nw = 1\nv = 2\n[[cells.list]]\nv = 3\n",
	"[x.y]\nc = 1\n[x]\nd = 2\na.b.c = 3\na.a = 4\n\"quoted key\" = 5\n'lit' = 6\n",
	"[environments.box]\nbackend = \"container\"\nlimits = { pids = 10, cpus = 2.5 }\n[environments.box.extra]\nz = 1\na = 2\n" +
		"[environments.dev]\nbackend = \"local\"\n",
	"d = 2024-05-01\nt = 10:11:12\nl = 2024-05-01T10:11:12\no = 2024-05-01T10:11:12Z\nf = 2024-05-01T10:11:12.5+05:30\n" +
		"arr = [[1, 2], [\"a\"]]\nm = '''\nmulti\n'''\n",
}

func TestParseTOMLMatchesPython(t *testing.T) {
	samples := append([]string{}, tomlSamples...)
	for _, name := range []string{"console-demo", "environment-profiles", "aws-cloud", "jq-smoke", ""} {
		samples = append(samples, readFile(t, filepath.Join(examples(t), name, "trial.toml")))
	}
	got := python(t, `
print(json.dumps([plan_edit.tomllib.loads(t) for t in data], default=str))
`, samples)
	for i, text := range samples {
		d, err := ParseTOML(text)
		if err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		sameText(t, fmt.Sprintf("sample %d", i), Dumps(d), Dumps(got.([]any)[i]))
	}
}

func extraTrials(t *testing.T) []string {
	t.Helper()
	var trials []string
	for _, name := range []string{"console-demo", "environment-profiles", "aws-cloud", "jq-smoke"} {
		trials = append(trials, copyExample(t, name))
	}
	odd := copyExample(t, "console-demo")
	writeFile(t, odd, readFile(t, odd)+`
[[setup]]
name = "prepare"
run = "true"
env = { B = "2", A = "1" }

[[preflight]]
run = ["sh", "-c", "it's"]

[environments.box]
backend = "container"
image = "sha256:`+strings.Repeat("c", 64)+`"
limits = { pids = 64, cpus = 2, extra_limit = 3 }
tools = [{ name = "jq" }, { argv = ["python3", "-V"], name = "python3", required = false }]
zeta = "kept"

[auth.keys]
type = "claude-bedrock"

[auth.weird]
type = "no-such-auth"

[adapters.my-agent]
execute = ["sh", "-c", "true"]
`)
	editText(t, odd, "[[verify]]\n", "[[verify]]\ntimeout = 30\ntype = \"shell\"\nlocation = \"environment\"\n")
	editText(t, odd, "[task]\n", "[task]\nidentity_cmd = [\"aws\", \"sts\", \"get-caller-identity\", \"--query\", \"Arn x\"]\nhuman = 5\n")
	editText(t, odd, "harness = \"console-demo\"\n\n[[cells]]", "harness = \"console-demo\"\nmodel = \"\"\n\n[[cells]]")
	trials = append(trials, odd)
	crlf := copyExample(t, "console-demo")
	writeFile(t, crlf, strings.ReplaceAll(readFile(t, crlf), "\n", "\r\n"))
	writeFile(t, filepath.Join(filepath.Dir(crlf), "task-prompt.md"), "line one\r\nline two\r\n")
	trials = append(trials, crlf)
	dated := copyExample(t, "console-demo")
	writeFile(t, dated, strings.Replace(readFile(t, dated), "[task]", "[task]\nwritten = 2024-05-01", 1))
	return append(trials, dated)
}

func TestReadModelMatchesPython(t *testing.T) {
	trials := extraTrials(t)
	got := python(t, `
out = []
for trial in data:
    try:
        out.append({"model": plan_edit.read_model(trial)})
    except plan_edit.PlanError as exc:
        out.append({"error": str(exc)})
print(json.dumps(out))
`, trials)
	for i, trial := range trials {
		want := got.([]any)[i].(*Dict)
		m, err := ReadModel(trial)
		if err != nil {
			sameText(t, trial+" error", err.Error(), fmt.Sprint(want.Get("error")))
			continue
		}
		sameText(t, filepath.Base(filepath.Dir(trial))+" model", Dumps(m), Dumps(want.Get("model")))
	}
}

func TestOptionsMatchPython(t *testing.T) {
	trials := extraTrials(t)[:5]
	got := python(t, `
out = []
for trial in data:
    plugins.load(Path(trial).parent)
    out.append(plan_edit.options(trial, plan_edit.read_model(trial)))
print(json.dumps(out))
`, trials)
	for i, trial := range trials {
		opts, err := Options(trial, model(t, trial))
		if err != nil {
			t.Fatal(err)
		}
		sameText(t, filepath.Base(filepath.Dir(trial))+" options", Dumps(opts), Dumps(got.([]any)[i]))
	}
}

// renderCases are edits written in Python (the model is edited there and sent
// to Go as JSON). Each names the trial it starts from.
var renderCases = []struct{ trial, edit string }{
	{"console-demo", ""},
	{"console-demo", `m["workers"][0].update(cli="kiro-cli", model="opus")
m["reporter"].update(cli="codex", model="astra")`},
	{"console-demo", `m["workers"][0].update(cli="kiro-cli", models=["sonnet", "opus", "gateway/MyModel"])
m["reporter"].update(cli="codex", model="sol")`},
	{"console-demo", `m["workers"][0].update(cli="codex", model="opus")`},
	{"console-demo", `m["workers"][0]["model"] = "opus"
m["checks"].append({"name": "out.csv exists", "type": "file", "path": "out.csv", "where": "host"})
m["run"]["attempts"] = 3`},
	{"console-demo", `m["account"]["teardown"] = [{"run": 'set -eu\n[ -f out.csv ] && echo "one"\nprintf %s \\\\\nrm -rf "$AJX_RUN_TOKEN"'}]`},
	{"console-demo", `m["task"]["materials"] = ["orders-cli --help", 'say "hi"', "it's \"both\"", "  ", "tab\there"]
m["task"]["setup"] = [{"run": "echo one", "name": "first", "extra": {"env": {"Z": "1", "A": "2"}, "my key": "x", "é": 1}},
                      {"run": "  ", "name": "skipped"}]`},
	{"console-demo", `m["envs"].append({"id": "sealed", "kind": "container", "image": "sha256:" + "a" * 64, "network": "none",
                 "required": ["filesystem_isolation"], "limits": dict(plan_edit.CONTAINER_LIMITS),
                 "tools": [{"name": "python3", "required": True}]})
w = copy.deepcopy(m["workers"][0])
w.update(id="sealed-claude", cli="claude-code", env="sealed", auth="bedrock-keys", model="sonnet")
m["workers"].append(w)`},
	{"console-demo", `m["envs"].append({"id": "box", "kind": "container", "image": " img ", "network": "bridge", "docker_host": "unix:///x.sock",
                 "limits": {"cpus": "2", "memory_mb": 2048.0, "pids": 256, "custom": "7", "tmpfs_mb": ""},
                 "tools": [{"name": "jq", "argv": ["jq", "-V"], "required": False}, {"name": ""}], "extra": {"zz": 1}})
m["workers"][0]["env"] = "box"
m["workers"][0]["config"] = "default"
m["workers"][0]["skills"] = "folders"
m["workers"][0]["skillPaths"] = [" skills/a ", ""]`},
	{"console-demo", `m["workers"].append({"id": "kiro", "cli": "kiro-cli", "model": "", "models": ["claude-sonnet-4.5", "other", " "],
                    "models_form": False, "effort": "low", "config": "clean", "auth": "kiro-key", "env": "@local",
                    "skills": "none", "skillPaths": [], "profile": "", "extra": {"args": ["--x"], "id": "ignored"}})
m["reporter"].update(cli="claude-code", auth="bedrock-keys", model=" big ")`},
	{"environment-profiles", `m["workers"][0].update(skills="folders", skillPaths=["skills/csv-guide", "skills/other"])`},
	{"environment-profiles", `m["workers"] = [w for w in m["workers"] if w["id"] == "plain"]`},
	{"environment-profiles", `w = copy.deepcopy(m["workers"][1])
w["id"] = "guided-two"
m["workers"].append(w)
m["workers"][1]["skillPaths"] = ["skills/csv-guide", "skills/more"]`},
	{"environment-profiles", `m["envs"][0]["tools"] = [{"name": "python", "argv": ["python3", "-V"]}]
m["envs"].append({"id": "second", "kind": "profile", "tools": [], "extra": {}})`},
	{"environment-profiles", `m["workers"][1]["skills"] = "none"`},
	{"aws-cloud", `m["account"]["on"] = False`},
	{"aws-cloud", `m["account"]["identity"] = "aws sts get-caller-identity --profile x"
m["account"]["preflight"] = m["account"]["preflight"][:1]
m["run"].update(budget="2.5", parallel="2", order_seed=7, workspace=" /var/tmp ", shell="skip", output_dir=" out/x ")
m["task"].update(fixture=" fixture2 ", human="available", materials=[])`},
	{"aws-cloud", `m["run"].update(budget="", minutes=12.5, report_minutes="3")`},
	{"jq-smoke", `m["checks"] = [{"name": " web ", "type": "http", "url": " http://x ", "expect_status": "404", "expect_body": "ok", "where": "worker",
                 "extra": {"timeout": 5}},
                {"name": "", "type": "cmd", "run": "true", "expect_exit": " 3 ", "expect_stdout": "", "extra": {}},
                {"name": "f", "type": "file", "path": " out.csv ", "contains": "id", "extra": {"contains": "no"}},
                {"name": "odd", "type": "custom", "extra": {"script": "x"}}]`},
	{"jq-smoke", `m["task"]["prompt"] = "New task.\r\nSecond line.\n"`},
	{"jq-smoke", `m["task"]["prompt"] = "same prompt, other ending"
m["task"]["prompt_file"] = "ignored.md"`},
	{"console-demo", `m["workers"][0]["model"] = "sonnet 5.5"`},
	{"console-demo", `m["workers"][0].update(skills="folders", skillPaths=["skills/x"])`},
	{"console-demo", `m["workers"].append(dict(m["workers"][0]))`},
	{"console-demo", `m["run"].update(attempts=1.5)`},
	{"console-demo", `m["run"].update(minutes="x")`},
	{"console-demo", `m["run"].update(minutes=0.01)`},
	{"console-demo", `m["checks"][0].update(expect_exit="x")`},
	{"console-demo", `m["checks"][0].update(expect_exit=True)`},
	{"console-demo", `m["workers"][0].pop("env")`},
	{"console-demo", `m["workers"][0].pop("cli")`},
	{"console-demo", `m["workers"] = []`},
	{"console-demo", `m["workers"] = ["x"]`},
	{"console-demo", `m["workers"][0]["id"] = "Bad Name"`},
	{"console-demo", `m["workers"][0]["env"] = "nowhere"`},
	{"console-demo", `m["envs"].append({"id": "Bad", "kind": "container"})`},
	{"console-demo", `m["envs"].append({"id": "@local", "kind": "container"})`},
	{"console-demo", `m["envs"].append({"id": "@local", "kind": "folder"})`},
	{"console-demo", `m["run"]["shell"] = "zsh"`},
	{"console-demo", `m["reporter"]["model"] = "-x"`},
	{"console-demo", `m["run"]["budget"] = None`},
	{"console-demo", `m["run"]["parallel"] = 0`},
	{"console-demo", `m.pop("envs")`},
	{"console-demo", `m["checks"] = {}`},
	{"console-demo", `m["account"]["teardown"] = "x"`},
}

func TestRenderChangesAndDiffMatchPython(t *testing.T) {
	trials := map[string]string{}
	var input []any
	for i, c := range renderCases {
		trial := copyExample(t, c.trial)
		trials[fmt.Sprint(i)] = trial
		input = append(input, map[string]any{"trial": trial, "edit": c.edit})
	}
	got := python(t, `
out = []
for case in data:
    trial = Path(case["trial"])
    plugins.load(trial.parent)
    m = plan_edit.read_model(trial)
    exec(case["edit"], {"m": m, "copy": copy, "plan_edit": plan_edit})
    rec = {"model": m}
    try:
        text, prompt, added = plan_edit.render(trial, m)
        rec.update(text=text, prompt=prompt, added=added,
                   diff=plan_edit.diff_lines(trial.read_text(encoding="utf-8"), text, "trial.toml"))
    except plan_edit.PlanError as exc:
        rec["error"] = str(exc)
    try:
        rec["changes"] = plan_edit.changes(plan_edit.read_model(trial), m)
    except plan_edit.PlanError as exc:
        rec["changes_error"] = str(exc)
    out.append(rec)
print(json.dumps(out))
`, input)
	for i, c := range renderCases {
		want := got.([]any)[i].(*Dict)
		trial := trials[fmt.Sprint(i)]
		name := fmt.Sprintf("case %d (%s: %s)", i, c.trial, strings.SplitN(c.edit, "\n", 2)[0])
		// The model goes through JSON text, as from the page.
		m, err := DecodeJSON([]byte(Dumps(want.Get("model"))))
		if err != nil {
			t.Fatal(err)
		}
		out, err := Render(trial, m, RenderOpts{})
		if err != nil {
			if !want.Has("error") {
				t.Errorf("%s: go refused (%v), python wrote\n%s", name, err, want.Get("text"))
				continue
			}
			sameText(t, name+" error", tomlError(err.Error()), tomlError(want.Get("error").(string)))
		} else if want.Has("error") {
			t.Errorf("%s: python refused (%v), go wrote\n%s", name, want.Get("error"), out.Text)
		} else {
			sameText(t, name+" text", out.Text, want.Get("text").(string))
			var prompt any
			if out.Prompt != nil {
				prompt = *out.Prompt
			}
			sameText(t, name+" prompt", Dumps(prompt), Dumps(want.Get("prompt")))
			added := []any{}
			for _, a := range out.AddedAuth {
				added = append(added, a)
			}
			sameText(t, name+" added", Dumps(added), Dumps(want.Get("added")))
			diff := []any{}
			for _, l := range DiffLines(readFile(t, trial), out.Text, "trial.toml") {
				diff = append(diff, l)
			}
			sameText(t, name+" diff", Dumps(diff), Dumps(want.Get("diff")))
		}
		list, err := Changes(model(t, trial), m)
		if err != nil {
			sameText(t, name+" changes error", err.Error(), fmt.Sprint(want.Get("changes_error")))
			continue
		}
		var changes []any
		for _, ch := range list {
			changes = append(changes, NewDict("node", ch.Node, "text", ch.Text))
		}
		if changes == nil {
			changes = []any{}
		}
		sameText(t, name+" changes", Dumps(changes), Dumps(want.Get("changes")))
	}
}

func TestSetValueMatchesPython(t *testing.T) {
	text := "# top\n[trial]\nid = \"x\"\nmaterials = [\n  \"a\",\n  \"b\",\n]\nnote = '''\n[cells]\nmodel = \"no\"\n'''\n" +
		"timeout_seconds = 60   # keep\n# output_dir = \"x\"\n\n[[cells]]\nid = \"a\"\nharness = \"h\"\n\n[[cells]]\nid = \"b\"\n\n" +
		"[\"quoted\"]\nk = 1\n[env]\n\"my key\" = 1\n"
	values := []any{int64(5), 2.5, 1e16, 1e-7, true, "plain", `say "hi"`, `it's "both"`, "multi\nline", "multi\nline'", "a'''b\nc",
		"tab\tin", "ctl\x01", "é ü", []any{"a", int64(1), []any{}}, NewDict("b", int64(1), "a", NewDict()), NewDict(), nil}
	type call struct {
		table, name string
		value       any
		index       int
	}
	var calls []call
	for _, v := range values {
		calls = append(calls, call{"trial", "materials", v, 0}, call{"trial", "output_dir", v, 0}, call{"cells", "model", v, 1})
	}
	calls = append(calls, call{"trial", "note", "x", 0}, call{"trial", "timeout_seconds", int64(1), 0},
		call{"cells", "id", nil, 0}, call{"cells", "x", int64(1), 2}, call{"env", "my key", int64(2), 0},
		call{"env", "é", int64(2), 0}, call{"reporter", "model", "m", 0}, call{"quoted", "k", int64(2), 0},
		call{"trial", "materials", nil, 0})
	var input []any
	for _, c := range calls {
		input = append(input, NewDict("text", text, "table", c.table, "name", c.name, "value", c.value, "index", int64(c.index)))
	}
	got := python(t, `
out = []
for c in data:
    try:
        out.append({"text": plan_edit.set_value(c["text"], c["table"], c["name"], c["value"], c["index"])})
    except plan_edit.PlanError as exc:
        out.append({"error": str(exc)})
print(json.dumps(out))
`, input)
	for i, c := range calls {
		want := got.([]any)[i].(*Dict)
		name := fmt.Sprintf("set_value %s.%s = %s", c.table, c.name, Dumps(c.value))
		res, err := SetValue(text, c.table, c.name, c.value, c.index)
		switch {
		case err != nil && want.Has("error"):
			sameText(t, name+" error", tomlError(err.Error()), tomlError(want.Get("error").(string)))
		case err != nil:
			t.Errorf("%s: go refused (%v), python wrote\n%s", name, err, want.Get("text"))
		case want.Has("error"):
			t.Errorf("%s: python refused (%v), go wrote\n%s", name, want.Get("error"), res)
		default:
			sameText(t, name, res, want.Get("text").(string))
		}
	}
}

func TestDiffLinesMatchPython(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	words := []string{"a = 1", "b = 2", "", "[cells]", "id = \"x\"", "# note", "c = 3"}
	var pairs []any
	for n := 0; n < 40; n++ {
		size := 1 + rng.Intn(30)
		if n%10 == 9 {
			size = 250 + rng.Intn(60) // autojunk: popular lines in long files
		}
		var before []string
		for i := 0; i < size; i++ {
			before = append(before, words[rng.Intn(len(words))])
		}
		after := append([]string{}, before...)
		for e := 0; e < 1+rng.Intn(6); e++ {
			i := rng.Intn(len(after) + 1)
			switch rng.Intn(3) {
			case 0:
				after = append(after[:i], append([]string{fmt.Sprintf("new %d", e)}, after[i:]...)...)
			case 1:
				if i < len(after) {
					after = append(after[:i], after[i+1:]...)
				}
			default:
				if i < len(after) {
					after[i] = words[rng.Intn(len(words))] + " changed"
				}
			}
		}
		a, b := strings.Join(before, "\n"), strings.Join(after, "\n")
		if n%3 == 0 {
			a, b = a+"\n", b+"\n"
		}
		if n%7 == 0 {
			b += "\r\ntrailing\x0cform"
		}
		pairs = append(pairs, []any{a, b})
	}
	got := python(t, `
print(json.dumps([plan_edit.diff_lines(a, b, "f.toml") for a, b in data]))
`, pairs)
	for i, p := range pairs {
		pair := p.([]any)
		diff := []any{}
		for _, l := range DiffLines(pair[0].(string), pair[1].(string), "f.toml") {
			diff = append(diff, l)
		}
		sameText(t, fmt.Sprintf("diff %d", i), Dumps(diff), Dumps(got.([]any)[i]))
	}
}
