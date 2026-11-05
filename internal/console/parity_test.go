package console

// Side-by-side checks against the Python console (skills/ajx/lib/ajx/console.py)
// run with `python3 -I`. They use synthetic trials only and skip without python3.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/planedit"
	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

func pythonLib(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	_, file, _, _ := runtime.Caller(0)
	lib := filepath.Join(filepath.Dir(file), "..", "..", "skills", "ajx", "lib")
	if !isDir(filepath.Join(lib, "ajx")) {
		t.Skip("skills/ajx/lib not found")
	}
	return resolvePath(lib)
}

// python runs a script with the Python console importable and decodes its JSON output.
func python(t *testing.T, script string, input any) any {
	t.Helper()
	lib := pythonLib(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "parity.py")
	prelude := "import json, sys\nsys.path.insert(0, " + util.PyQuote(lib) + ")\nfrom ajx import console, plan_edit\n" +
		"plan_edit._DOCKER.update(at=float('inf'), info={'images': [], 'host': ''})  # never ask Docker in tests\n" +
		"data = json.loads(sys.stdin.read())\n"
	if err := os.WriteFile(path, []byte(prelude+script), 0o644); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(input)
	cmd := exec.Command("python3", "-I", path)
	cmd.Stdin = strings.NewReader(string(in))
	cmd.Env = append(os.Environ(), "AJX_DEMO_PACE=0")
	out, err := cmd.Output()
	if err != nil {
		msg := ""
		if ee, ok := err.(*exec.ExitError); ok {
			msg = string(ee.Stderr)
		}
		t.Fatalf("python: %v\n%s", err, msg)
	}
	v, err := util.DecodeJSON(out)
	if err != nil {
		t.Fatalf("python output: %v\n%s", err, out)
	}
	return v
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPlanShaMatchesPython(t *testing.T) {
	pythonLib(t)
	d := newDemo(t, "0")
	dir := filepath.Dir(d.trial)
	// Names whose order differs between plain string sorting and Python's
	// path-part sorting, hidden files, caches, the console's own state and a symlink.
	writeTree(t, filepath.Join(dir, "fixture"), map[string]string{
		"a/b.txt": "ab", "a-c.txt": "a-c", "a.txt": "a", ".hidden": "h", "z/__pycache__/x.pyc": "cache",
		".ajx-console/actions.jsonl": "{}", "dir/sub/deep.json": "{\"k\": 1}", "café.md": "unicode",
	})
	if err := os.Symlink("a.txt", filepath.Join(dir, "fixture", "link-to-a")); err != nil {
		t.Fatal(err)
	}
	writeTree(t, filepath.Join(dir, "plugins"), map[string]string{"extra/helper.py": "x = 1\n"})
	goSha := planSha(d.trial, d.spec())
	goNoSpec := planSha(d.trial, nil)
	goDigest, goComplete := treeDigest(filepath.Join(dir, "fixture"))
	got := util.MapOr(python(t, `
from ajx import spec as specmod
trial = console.trial_file(data["trial"])
spec = specmod.load(trial, readiness=False)
digest, complete = console._tree_digest(trial.parent / "fixture")
print(json.dumps({"sha": console.plan_sha(trial, spec), "no_spec": console.plan_sha(trial, None),
                  "digest": digest, "complete": complete, "covers": console.plan_info(trial, spec)["covers"]}))
`, M{"trial": d.trial}))
	if got["sha"] != goSha || got["no_spec"] != goNoSpec || got["digest"] != goDigest || got["complete"] != goComplete {
		t.Fatalf("plan ids differ:\n go     %s %s %s %v\n python %v %v %v %v", goSha, goNoSpec, goDigest, goComplete,
			got["sha"], got["no_spec"], got["digest"], got["complete"])
	}
	info, _ := PlanInfo(d.trial, d.spec())
	if util.PyJSON(info["covers"]) != util.PyJSON(got["covers"]) {
		t.Fatalf("covers %v vs %v", info["covers"], got["covers"])
	}
}

func TestSetValueMatchesPython(t *testing.T) {
	cases := []M{
		{"text": sample, "table": "trial", "key": "timeout_seconds", "value": 120, "index": 0},
		{"text": sample, "table": "trial", "key": "max_budget_usd", "value": 2.5, "index": 0},
		{"text": sample, "table": "trial", "key": "max_budget_usd", "value": 0.01, "index": 0},
		{"text": sample, "table": "trial", "key": "output_dir", "value": "ajx-reports/x \"q\" é", "index": 0},
		{"text": sample, "table": "cells", "key": "model", "value": "big", "index": 1},
		{"text": sample, "table": "cells", "key": "model", "value": "big", "index": 0},
		{"text": sample, "table": "cells", "key": "harness", "value": nil, "index": 0},
		{"text": sample, "table": "reporter", "key": "model", "value": "small", "index": 0},
		{"text": sample, "table": "env", "key": "FOO", "value": "bar", "index": 0},
		{"text": sample, "table": "trial", "key": "parallel", "value": true, "index": 0},
		{"text": "[trial]\nid = \"x\"\n# trailing comment\n\n", "table": "trial", "key": "repetitions", "value": 2, "index": 0},
		{"text": "trial = { id = \"x\" }\n", "table": "trial", "key": "repetitions", "value": 2, "index": 0},
		{"text": sample, "table": "cells", "key": "model", "value": "m", "index": 5},
	}
	var goOut []any
	for _, c := range cases {
		v := c["value"]
		if i, ok := v.(int); ok {
			v = int64(i)
		}
		text, err := SetValue(util.S(c["text"]), util.S(c["table"]), util.S(c["key"]), v, util.Int(c["index"], 0))
		if err != nil {
			goOut = append(goOut, M{"error": true})
		} else {
			goOut = append(goOut, M{"text": text})
		}
	}
	pyOut := util.List(python(t, `
out = []
for c in data:
    try:
        out.append({"text": console.set_value(c["text"], c["table"], c["key"], c["value"], index=c["index"])})
    except console.ConsoleError:
        out.append({"error": True})
print(json.dumps(out))
`, cases))
	for i := range cases {
		if util.PyJSON(goOut[i]) != util.PyJSON(pyOut[i]) {
			t.Errorf("case %d (%v.%v=%v):\n go     %q\n python %q", i, cases[i]["table"], cases[i]["key"], cases[i]["value"],
				util.PyJSON(goOut[i]), util.PyJSON(pyOut[i]))
		}
	}
}

func TestJournalsAreSharedWithPython(t *testing.T) {
	d := newDemo(t, "0")
	record(d.trial, "user", "message", M{"text": "Go wrote this “quote”", "about": "plan"})
	record(d.trial, "agent", "approved", M{"plan": strings.Repeat("ab", 32), "via": "chat", "quote": "go"})
	record(d.trial, "console", "job_finished", M{"job_n": 3, "pid": 1, "job": "run", "exit_code": 130})
	got := util.MapOr(python(t, `
trial = console.trial_file(data["trial"])
console.record(trial, "user", "message", text="Python wrote this", reply_to=None)
console.record(trial, "user", "settings_changed", changes=["time limit 5 -> 4 min"])
entries, cursor = console.read_entries(trial)
print(json.dumps({"cursor": cursor, "texts": [console.describe(e) for e in entries], "n": [e["n"] for e in entries],
                  "raw": open(trial.parent / ".ajx-console" / "actions.jsonl", encoding="utf-8").read().splitlines()}))
`, M{"trial": d.trial}))
	entries, cursor := readEntries(d.trial, 0)
	if cursor != util.Int(got["cursor"], -1) || cursor != 5 {
		t.Fatalf("cursor go %d python %v", cursor, got["cursor"])
	}
	var texts []any
	for _, e := range entries {
		texts = append(texts, describe(e))
	}
	if util.PyJSON(texts) != util.PyJSON(got["texts"]) {
		t.Fatalf("texts\n go     %v\n python %v", texts, got["texts"])
	}
	// Byte-identical line shape: Python's lines re-encode the same way in Go.
	for i, raw := range util.Strings(got["raw"]) {
		v, err := util.DecodeJSON([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		m := util.MapOr(v)
		if util.PyJSON(normalizeNumbers(m)) != raw {
			t.Errorf("line %d: go would write\n %s\npython wrote\n %s", i+1, util.PyJSON(normalizeNumbers(m)), raw)
		}
	}
}

// normalizeNumbers turns integral float64 values (from encoding/json) back into ints.
func normalizeNumbers(v any) any {
	switch t := v.(type) {
	case M:
		out := M{}
		for k, x := range t {
			out[k] = normalizeNumbers(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = normalizeNumbers(x)
		}
		return out
	case float64:
		if t == float64(int64(t)) {
			return int64(t)
		}
	}
	return v
}

func TestSinceMatchesPython(t *testing.T) {
	d := newDemo(t, "0")
	rd := filepath.Join(d.out(), "runs", "demo-r1")
	marks := M{}
	for _, s := range []string{"prepare", "execute", "verify", "teardown", "normalize", "narrate", "release", "extract", "measure", "render"} {
		marks[s] = M{"status": "done", "at": "2026-01-01T10:00:00Z"}
	}
	writeJSONFile(t, filepath.Join(rd, "state.json"), M{"stages": marks, "teardown": M{"status": "ok"},
		"execute": M{"exit_code": 0, "started_at": "2026-01-01T09:59:00Z", "stopped_at": "2026-01-01T10:00:00Z",
			"requested": M{"harness": "console-demo", "model": nil}}})
	writeJSONFile(t, filepath.Join(rd, "verify.json"), M{"outcome": "succeeded", "passed": 1, "total": 1, "checks": []any{}})
	writeJSONFile(t, filepath.Join(rd, "asks.json"), M{"extraction_status": "ok", "asks": []any{M{"id": "A-1", "title": "t"}}})
	writeJSONFile(t, filepath.Join(rd, "run.json"), M{"review_status": "ready"})
	os.WriteFile(filepath.Join(rd, "report.html"), []byte("<p>r</p>"), 0o644)
	sp := d.spec()
	writeJSONFile(t, filepath.Join(d.out(), "matrix-plan.json"), M{"task_prompt_sha256": util.Get(sp, "task", "prompt_sha256"), "order": []any{"demo-r1"}})
	_, _ = message(d.trial, "is it done?", nil)
	goView, err := Since(d.trial, 0, false) // journals the milestones first; Python must not repeat them
	if err != nil {
		t.Fatal(err)
	}
	pyView := util.MapOr(python(t, `
print(json.dumps(console.since(data["trial"], 0, touch=False)))
`, M{"trial": d.trial}))
	for _, key := range []string{"headline", "do_not", "messages_for_agent", "cursor", "plan", "process", "output"} {
		g, p := util.PyJSON(normalizeNumbers(goView[key])), util.PyJSON(normalizeNumbers(pyView[key]))
		if g != p {
			t.Errorf("%s:\n go     %s\n python %s", key, g, p)
		}
	}
	strip := func(v any) string {
		var out []any
		for _, c := range util.Maps(v) {
			out = append(out, M{"n": c["n"], "kind": c["kind"], "actor": c["actor"], "text": c["text"]})
		}
		return util.PyJSON(normalizeNumbers(out))
	}
	if strip(goView["changes"]) != strip(pyView["changes"]) {
		t.Errorf("changes:\n go     %s\n python %s", strip(goView["changes"]), strip(pyView["changes"]))
	}
	runKeys := []string{"id", "stage", "complete", "verified", "cleanup", "exit_code", "error", "could_not_start", "asks", "review_status", "files"}
	g, p := util.Maps(goView["runs"]), util.Maps(pyView["runs"])
	if len(g) != 1 || len(p) != 1 {
		t.Fatalf("runs go %v python %v", g, p)
	}
	for _, k := range runKeys {
		if util.PyJSON(normalizeNumbers(g[0][k])) != util.PyJSON(normalizeNumbers(p[0][k])) {
			t.Errorf("runs[0].%s: go %v python %v", k, g[0][k], p[0][k])
		}
	}
}

// diffJSON lists the paths where two JSON-shaped values differ.
func diffJSON(path string, a, b any, out *[]string) {
	a, b = normalizeNumbers(a), normalizeNumbers(b)
	am, aok := a.(M)
	bm, bok := b.(M)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		for _, k := range util.SortedKeys(keys) {
			av, ain := am[k]
			bv, bin := bm[k]
			if ain != bin {
				*out = append(*out, path+"."+k+" present only in "+map[bool]string{true: "go", false: "python"}[ain])
				continue
			}
			diffJSON(path+"."+k, av, bv, out)
		}
		return
	}
	al, alok := a.([]any)
	bl, blok := b.([]any)
	if alok && blok && len(al) == len(bl) {
		for i := range al {
			diffJSON(path+"["+py(i)+"]", al[i], bl[i], out)
		}
		return
	}
	if util.PyJSON(a) != util.PyJSON(b) {
		*out = append(*out, path+": go "+util.Clip(util.PyJSON(a), 200)+" | python "+util.Clip(util.PyJSON(b), 200))
	}
}

func TestSnapshotMatchesPython(t *testing.T) {
	d := newDemo(t, "0")
	rd := filepath.Join(d.out(), "runs", "demo-r1")
	marks := M{}
	for _, s := range []string{"prepare", "execute", "verify", "teardown", "normalize", "narrate", "release"} {
		marks[s] = M{"status": "done", "at": "2026-01-01T10:00:00Z"}
	}
	marks["extract"] = M{"status": "error", "at": "2026-01-01T10:00:01Z", "error": "RuntimeError: reporter failed"}
	writeJSONFile(t, filepath.Join(rd, "state.json"), M{"stages": marks, "teardown": M{"status": "failed"},
		"environment_cleanup": M{"status": "released", "confirmed": false},
		"execute": M{"exit_code": 1, "stop_reason": "exit_1", "started_at": "2026-01-01T09:59:00Z", "stopped_at": "2026-01-01T10:00:00Z",
			"requested": M{"harness": "console-demo", "model": "sonnet"}}})
	writeJSONFile(t, filepath.Join(rd, "verify.json"), M{"outcome": "partial", "passed": 1, "total": 2,
		"checks": []any{M{"name": "a", "passed": true, "detail": strings.Repeat("x", 250)}, M{"name": "b", "passed": false}}})
	writeJSONFile(t, filepath.Join(rd, "telemetry.execute.json"), M{"model_ids": []any{"claude-sonnet-x"},
		"events": []any{M{"kind": "tool"}}, "final_text": "  It   failed\n badly "})
	os.WriteFile(filepath.Join(rd, "execute.stderr.txt"), []byte("\nboom\n"), 0o644)
	os.WriteFile(filepath.Join(rd, "journey.md"), []byte("# j\n"), 0o644)
	_, _ = message(d.trial, "why?", "demo-r1")
	_, _ = Note(d.trial, "Looking.", 1, "agent")
	record(d.trial, "user", "approved", M{"plan": "0123456789abcdef", "via": "console"})
	goSnap, err := Snapshot(d.trial, SnapshotOpts{})
	if err != nil {
		t.Fatal(err)
	}
	pySnap := util.MapOr(python(t, `
print(json.dumps(console.snapshot(data["trial"])))
`, M{"trial": d.trial}))
	for _, s := range []M{goSnap, pySnap} {
		delete(s, "generated_at")
	}
	withoutTestHarnesses(goSnap)
	var diffs []string
	diffJSON("snapshot", goSnap, pySnap, &diffs)
	for _, d := range diffs {
		t.Error(d)
	}
}

// withoutTestHarnesses drops the harnesses only these tests register (plain-test) from a snapshot's plan options.
func withoutTestHarnesses(snap M) {
	opts := util.Map(snap["plan_options"])
	if opts == nil {
		return
	}
	var kept []any
	for _, h := range util.Maps(opts["harnesses"]) {
		if plugin.Source("harness", util.S(h["name"])) != "test" {
			kept = append(kept, h)
		} else {
			delete(util.MapOr(opts["auth"]), util.S(h["name"]))
		}
	}
	opts["harnesses"] = kept
}

// savePlanCases are Plan-tab edits written in Python; each preview runs in
// both consoles on the same trial folder with the same model JSON.
var savePlanCases = []struct {
	setup string // "", "attempt" (the folder holds an attempt), "nofixture" (and trial.toml no longer loads)
	edit  string
}{
	{"", ""},
	{"", `m["run"]["minutes"] = 3`},
	{"", `m["envs"].append({"id": "box", "kind": "container", "image": "sha256:" + "b" * 64, "network": "none"})
m["workers"][0]["env"] = "box"`},
	{"", `m["run"]["minutes"] = "soon"`},
	{"", `m["task"]["prompt"] = "New task.\nSecond line"`},
	{"", `m["task"]["prompt"] = "   "`},
	{"", `m["workers"][0]["model"] = "has space"`},
	{"", `m.clear(); m["workers"] = "x"`},
	{"", `m["workers"].append({"id": "claude", "cli": "claude-code", "model": "sonnet", "models": [], "models_form": False,
                    "effort": "", "config": "clean", "auth": "bedrock-keys", "env": "@local", "skills": "none",
                    "skillPaths": [], "profile": "", "extra": {}})`},
	{"", `m["checks"][0]["run"] = "test -s out.csv"
m["task"]["materials"] = []
m["run"]["output_dir"] = "ajx-reports/other"`},
	{"attempt", `m["workers"][0]["model"] = "other"`},
	{"attempt", `m["checks"][0]["expect_exit"] = 3`},
	{"attempt", `m["task"]["prompt"] = "A different task.\n"`},
	{"attempt", `m["reporter"]["model"] = "demo-model"`},
	{"attempt", `m["task"]["human"] = "available"
m["account"]["teardown"] = [{"run": "true", "name": "", "extra": {}}]`},
	{"attempt", `m["run"]["attempts"] = 2
w = dict(m["workers"][0]); w["id"] = "demo-two"; m["workers"].append(w)`},
	{"attempt", `m["workers"][0]["model"] = "other"
m["run"]["output_dir"] = "ajx-reports/orders-cli/fresh"`},
	{"nofixture", `m["task"]["prompt"] = "A different task.\n"`},
}

func TestSavePlanPreviewMatchesPython(t *testing.T) {
	pythonLib(t)
	var input []any
	for _, c := range savePlanCases {
		d := newDemo(t, "0")
		switch c.setup {
		case "attempt":
			d.recordAttempt()
		case "nofixture":
			d.recordAttempt()
			os.RemoveAll(filepath.Join(filepath.Dir(d.trial), "fixture"))
		}
		input = append(input, M{"trial": d.trial, "edit": c.edit})
	}
	got := util.List(python(t, `
import copy
out = []
for case in data:
    m = plan_edit.read_model(case["trial"])
    exec(case["edit"], {"m": m, "copy": copy})
    out.append({"model": json.dumps(m), "view": console.save_plan(case["trial"], m, dry_run=True)})
print(json.dumps(out))
`, input))
	for i, c := range savePlanCases {
		want := util.MapOr(got[i])
		m, err := planedit.DecodeJSON([]byte(util.S(want["model"])))
		if err != nil {
			t.Fatal(err)
		}
		view, err := savePlan(util.S(util.MapOr(input[i])["trial"]), m, nil, nil, "user", true)
		if err != nil {
			t.Errorf("case %d: %v", i, err)
			continue
		}
		var diffs []string
		diffJSON(fmt.Sprintf("case %d (%s %q)", i, c.setup, util.Clip(c.edit, 50)), view, want["view"], &diffs)
		for _, d := range diffs {
			t.Error(d)
		}
	}
}

func TestJourneyHighlightsMatchPython(t *testing.T) {
	samples := []string{
		"## Chronological account\n1. 🚧 Roadblock. It failed [E-003].\n2. I read the help [E-004].\n\n## What helped\nnothing\n",
		"# Journey\n## Chronology\n1) **Wall**: blocked E-0001 and E-12\n   continued   line\n* not a step\n" +
			"2. ↩️Detour: went around E-005x E-006.\n3. Wrong Turn it took [E-007]\n4. Delightful but not a label\n" +
			"## Other\n5. ignored\n## Chronological (again)\n10. ⏳ **Wait** — slow\r\n11. Cost\n",
		"no headings at all\n1. a step\n",
		"## Chronological account\n" + strings.Repeat("1. 🔀 Fork. "+strings.Repeat("long text ", 40)+"\n", 3),
		"## Chronology\r\n  １.\u00a0⏳ **Wait**: E-００１ E-002x éE-003\r\n\tcontinued  text\r\n" +
			"2. Waité E-004\r\n## Other\r\n3. ignored\r\n## CHRONOLOGICAL again\n4. no references\n",
		"## Chronological account\n1. \n\t**Fork**: chose E-005\n2. **Wait**\n\ncontinued E-006\n",
	}
	got := util.List(python(t, `
print(json.dumps([console.journey_highlights(s) for s in data]))
`, samples))
	for i, s := range samples {
		if g, p := util.PyJSON(JourneyHighlights(s)), util.PyJSON(got[i]); g != p {
			t.Errorf("sample %d:\n go     %s\n python %s", i, g, p)
		}
	}
}

func TestOutputsMatchPython(t *testing.T) {
	d := newDemo(t, "0")
	ws := filepath.Join(d.tmp, "ws", "attempt")
	files := map[string]string{"b.txt": "b", "a.txt": "a", "A.txt": "A", "sub/z.txt": "z", "sub/inner/y.txt": "yy",
		".git/HEAD": "x", "node_modules/x/index.js": "x", "venv/bin/python": "x", "café/é.txt": "é", ".hidden": "h"}
	for i := 0; i < 230; i++ {
		files[fmt.Sprintf("many/f%03d.txt", i)] = "x"
	}
	writeTree(t, ws, files)
	os.Symlink(filepath.Join(ws, "a.txt"), filepath.Join(ws, "link.txt"))
	os.Symlink(filepath.Join(ws, "sub"), filepath.Join(ws, "linkdir"))
	rd := filepath.Join(d.out(), "runs", "demo-r1")
	for _, st := range []M{{"paths": M{"workspace": ws}}, {"archived": M{"workspace": filepath.Join(ws, "sub")}, "paths": M{"workspace": ws}},
		{"paths": M{"workspace": filepath.Join(d.tmp, "missing")}}, {}} {
		goFiles, goTrunc := outputs(rd, st, filepath.Join(d.tmp, "fixture"))
		goOut := []any{goFiles, goTrunc}
		pyOut := python(t, `
print(json.dumps(console._outputs(data["d"], data["st"], fixture=data["fixture"])))
`, M{"d": rd, "st": st, "fixture": filepath.Join(d.tmp, "fixture")})
		if g, p := util.PyJSON(normalizeNumbers(goOut)), util.PyJSON(normalizeNumbers(pyOut)); g != p {
			t.Errorf("state %v:\n go     %s\n python %s", st, util.Clip(g, 600), util.Clip(p, 600))
		}
	}
}

func TestPlanSnapshotMatchesPython(t *testing.T) {
	d := newDemo(t, "0")
	d.write(d.text() + "\n[auth.keys]\ntype = \"claude-bedrock\"\nisolates_config = false\n\n[environments.box]\nbackend = \"container\"\n" +
		"image = \"sha256:" + strings.Repeat("d", 64) + "\"\n")
	ws := filepath.Join(d.tmp, "ws", "attempt")
	writeTree(t, ws, map[string]string{"out.csv": "id\n1\n", "sub/deep.txt": "deep", ".git/HEAD": "x"})
	rd := filepath.Join(d.out(), "runs", "demo-r1")
	writeJSONFile(t, filepath.Join(rd, "state.json"), M{"paths": M{"workspace": ws}, "journey_provenance": "SELF_NARRATED",
		"stages": M{"prepare": M{"status": "done", "at": "2026-01-01T10:00:00Z"}, "execute": M{"status": "done", "at": "2026-01-01T10:01:00Z"}}})
	os.WriteFile(filepath.Join(rd, "journey.md"), []byte("# Journey\n## Chronological account\n1. 🧱 Wall. Stuck [E-001].\n"+
		"2. ✅ **Delight**: it worked E-002\n"), 0o644)
	record(d.trial, "user", "plan_changed", M{"changes": []any{"a", "b", "c", "d", "e"}})
	record(d.trial, "user", "plan_changed", M{"changes": []any{}})
	goSnap, err := Snapshot(d.trial, SnapshotOpts{})
	if err != nil {
		t.Fatal(err)
	}
	pySnap := util.MapOr(python(t, `
print(json.dumps(console.snapshot(data["trial"])))
`, M{"trial": d.trial}))
	for _, s := range []M{goSnap, pySnap} {
		delete(s, "generated_at")
	}
	withoutTestHarnesses(goSnap)
	var diffs []string
	diffJSON("snapshot", goSnap, pySnap, &diffs)
	for _, d := range diffs {
		t.Error(d)
	}
	if len(util.List(util.Get(util.Maps(goSnap["runs"])[0], "outputs"))) != 2 {
		t.Errorf("outputs %v", util.Get(util.Maps(goSnap["runs"])[0], "outputs"))
	}
}
