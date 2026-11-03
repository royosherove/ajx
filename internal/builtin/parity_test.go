package builtin

// Parity with the Python adapters: the same raw streams and transcripts are
// normalized by the Python normalizer (python3 -I) and by the Go port, and
// the JSON results must be semantically equal. Skipped without python3.

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

const pyDriver = `
import json, sys
from pathlib import Path
req = json.loads(sys.argv[1])
sys.path.insert(0, req["lib"])
from ajx import plugins
if req.get("trial_dir"):
    plugins.load(req["trial_dir"])
if req.get("adapter"):
    from ajx.builtin.generic import Declarative
    h = Declarative(req["harness"], req["adapter"])
else:
    h = plugins.get("harness", req["harness"])()
if req["mode"] == "progress":
    out = h.progress(Path(req["run_dir"]), stage=req["stage"], limit=req["limit"])
else:
    c = req["ctx"]
    ctx = {"spec": c["spec"], "cell": c["cell"], "run_dir": Path(c["run_dir"]), "workspace": Path(c["workspace"]),
           "config_dir": c["config_dir"], "state": c["state"], "env": {}, "unset_env": [], "timeout": 10,
           "prompt_text": "x", "runner": plugins.get("runner", "local")(), "auth": None}
    out = h.normalize(ctx, req["stage"], req.get("exclude") or ())
print(json.dumps(out))
`

type parityCase struct {
	name       string
	harness    string
	adapter    M
	trialDir   string
	cell       M
	state      M
	stage      string
	exclude    []string
	raw        string // stage raw stream as JSONL of the harness's own lines
	transcript string // claude transcript JSONL (written under config_dir/projects/-p/<sid>.jsonl)
	sid        string
	progress   bool
}

func python(t *testing.T) string {
	p, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	if _, err := os.Stat("../../skills/ajx/lib/ajx/builtin/claude_code.py"); err != nil {
		t.Skip("python sources unavailable")
	}
	return p
}

func semantic(t *testing.T, v any) any {
	t.Helper()
	data, err := util.MarshalIndent(v) // Python number spelling: 60.0 stays a float
	if err != nil {
		t.Fatal(err)
	}
	out, err := util.DecodeJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func runParity(t *testing.T, py string, c parityCase) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	ctx := testCtx(t, tmp, c.cell, c.state, tmp)
	lines := strings.Split(strings.TrimRight(c.raw, "\n"), "\n")
	writeRawLines(t, filepath.Join(ctx.RunDir, c.stage+".raw.jsonl"), lines)
	if c.transcript != "" {
		proj := filepath.Join(tmp, "projects", "-p")
		os.MkdirAll(proj, 0o755)
		os.WriteFile(filepath.Join(proj, c.sid+".jsonl"), []byte(c.transcript), 0o644)
	}
	var h plugin.Harness
	switch {
	case c.adapter != nil:
		h = NewDeclarative(c.harness, c.adapter)
	case c.harness == "console-demo":
		RegisterConsoleDemo()
		h = harness(t, c.harness)
	default:
		h = harness(t, c.harness)
	}
	var goOut any
	if c.progress {
		goOut = h.Progress(ctx.RunDir, c.stage, 10)
	} else {
		goOut = mustNormalize(t, h, ctx, c.stage, c.exclude)
	}
	lib, _ := filepath.Abs("../../skills/ajx/lib")
	req := M{"lib": lib, "harness": c.harness, "stage": c.stage, "exclude": anyList(c.exclude), "limit": 10,
		"run_dir": ctx.RunDir, "mode": "normalize",
		"ctx": M{"spec": ctx.Spec, "cell": c.cell, "run_dir": ctx.RunDir, "workspace": tmp, "config_dir": tmp, "state": c.state}}
	if c.progress {
		req["mode"] = "progress"
	}
	if c.adapter != nil {
		req["adapter"] = c.adapter
	}
	if c.trialDir != "" {
		abs, _ := filepath.Abs(c.trialDir)
		req["trial_dir"] = abs
	}
	cmd := exec.Command(py, "-I", "-c", pyDriver, util.Compact(req))
	cmd.Env = append(os.Environ(), "HOME="+tmp)
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("python: %v\n%s", err, stderr)
	}
	pyOut, err := util.DecodeJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	g := semantic(t, goOut)
	if !reflect.DeepEqual(g, pyOut) {
		gj, _ := util.MarshalIndent(g)
		pj, _ := util.MarshalIndent(pyOut)
		t.Fatalf("%s: Go and Python differ\n--- go\n%s\n--- python\n%s", c.name, gj, pj)
	}
}

func jsonl(events ...any) string {
	var lines []string
	for _, e := range events {
		lines = append(lines, newScope().dumps(e, true))
	}
	return strings.Join(lines, "\n") + "\n"
}

func readFixture(t *testing.T, name string) string {
	data, err := os.ReadFile(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestParityWithPythonNormalizers(t *testing.T) {
	py := python(t)
	isolated := func(sid string) M { return M{"execute": M{"session_id": sid, "config_isolated": true}} }
	fixSID := "00000000-0000-4000-8000-000000000001"
	cases := []parityCase{
		{name: "claude fixture", harness: "claude-code", cell: ccCell(), state: isolated(fixSID), stage: "execute",
			raw: readFixture(t, "claude-stream.jsonl"), transcript: readFixture(t, "claude-transcript.jsonl"), sid: fixSID},
		{name: "claude multi-result", harness: "claude-code", cell: ccCell(), state: isolated(ccSID), stage: "execute",
			raw: jsonl(multiStream()...), transcript: jsonl(multiTranscript()...), sid: ccSID},
		{name: "claude non-cumulative", harness: "claude-code", cell: ccCell(), state: isolated(ccSID), stage: "execute",
			raw: jsonl(ccInit(), ccResult(2, 1000, 0.20, 10, resultOpts{}), ccResult(1, 500, 0.15, 5, resultOpts{text: "done"}))},
		{name: "claude no session", harness: "claude-code", cell: ccCell(), state: M{}, stage: "execute",
			raw: `{"type":"system","subtype":"init","slash_commands":["ajx:run","Zed",3],"mcp_servers":[{"name":"m"}]}` + "\n"},
		{name: "claude subagents and partial", harness: "claude-code", cell: ccCell(), state: isolated(ccSID), stage: "execute",
			raw: `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Task","input":{"todos":[{"status":"pending","content":"Größe ✓ 🚧"}],"b":1.0,"a":null}}]}}
{"type":"assistant","parent_tool_use_id":"t1","message":{"content":[{"type":"tool_use","id":"t9","name":"Bash","input":{"command":["ls","-la",2,true,null,{"x":"y"}]}}]}}
{"type":"result","subtype":"success","result":"ok","usage":{"output_tokens":7.0},"subagent_stats":{"spawned":1},"modelUsage":{"zeta":{"outputTokens":3},"alpha":{"outputTokens":4}}}
`,
			transcript: `{"type":"assistant","timestamp":"2026-01-01T00:00:01Z","message":{"id":"a1","model":"m-z","usage":{"output_tokens":7},"content":[{"type":"tool_use","id":"t1","name":"Task","input":{"todos":[{"status":"pending","content":"Größe ✓ 🚧"}],"b":1.0,"a":null}},{"type":"text","text":"  \n  "},{"type":"text","text":"  héllo world  "}]}}
{"type":"assistant","isSidechain":true,"message":{"id":"side","usage":{"output_tokens":99},"content":[]}}
{"type":"user","timestamp":"2026-01-01T00:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"line1"},{"type":"image"},{"type":"text","text":"line2"}],"is_error":true}]}}
{"type":"user","timestamp":"2026-01-01T00:00:03Z","message":{"content":"plain"}}
{"bad json
`, sid: ccSID},
		{name: "claude narrate fork", harness: "claude-code", cell: ccCell(),
			state: M{"execute": M{"session_id": "orig", "config_isolated": true}, "narrate": M{"session_id": "forked"}},
			stage: "narrate", exclude: []string{"msg-example-1", "msg-example-2"},
			raw:        `{"type":"result","subtype":"success","result":"# Journey","usage":{"output_tokens":5},"modelUsage":{}}` + "\n",
			transcript: readFixture(t, "claude-transcript.jsonl") + `{"type":"assistant","timestamp":"t","message":{"id":"new","model":"m","usage":{"output_tokens":5},"content":[{"type":"text","text":"# Journey"}]}}` + "\n",
			sid:        "forked"},
		{name: "claude progress", harness: "claude-code", cell: ccCell(), state: M{}, stage: "execute",
			raw: jsonl(progressStream()...), progress: true},
		{name: "kiro fixture", harness: "kiro-cli", cell: M{"id": "k", "harness": "kiro-cli", "config": "clean", "model": "claude-haiku-4.5"},
			state: M{"execute": M{}}, stage: "execute", raw: readFixture(t, "kiro-stream.jsonl")},
		{name: "kiro narrate", harness: "kiro-cli", cell: M{"id": "k", "harness": "kiro-cli", "config": "inherit"},
			state: M{}, stage: "narrate", raw: `{"type":"metadata","data":{"sessionId":"s","meteringUsage":[{"value":0.1},{"value":"0.2"},{"value":null}]}}
{"type":"sessionUpdate","data":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"one "}}}}
{"type":"sessionUpdate","data":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"two"}}}}
{"type":"sessionUpdate","data":{"update":{"sessionUpdate":"tool_call","toolCallId":"x","title":"Reading file","kind":"read"}}}
{"type":"sessionUpdate","data":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"x","status":"failed","content":[{"content":{"text":"nope"}},null]}}}
{"type":"sessionUpdate","data":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"three"}}}}
{"type":"metadata","data":{"meteringUsage":[{"value":0.1},{"value":0.1},{"value":0.1}],"contextUsagePercentage":12.5}}
`},
		{name: "codex", harness: "codex", cell: M{"id": "x", "harness": "codex", "config": "clean"}, state: M{}, stage: "execute",
			raw: `{"type":"thread.started","thread_id":"th-1"}
{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"bash -lc ls","status":"in_progress"}}
{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"bash -lc ls","aggregated_output":"a\nb","exit_code":1,"status":"completed"}}
{"type":"item.completed","item":{"id":"i2","type":"mcp_tool_call","server":"docs","tool":"search","status":"completed","result":{"content":[{"text":"résumé"}]}}}
{"type":"item.completed","item":{"id":"i3","item_type":"file_change","changes":[{"path":"a.txt","kind":"add"}],"status":"failed"}}
{"type":"item.completed","item":{"id":"i4","type":"agent_message","text":"first"}}
{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":2,"output_tokens":30}}
{"type":"item.completed","item":{"id":"i5","type":"agent_message","text":"done"}}
{"type":"turn.completed","usage":{"output_tokens":12}}
{"type":"error","message":{"detail":"x"}}
`},
		{name: "codex narrate", harness: "codex", cell: M{"id": "x", "harness": "codex", "config": "clean", "model": "gpt-x"}, state: M{},
			stage: "narrate", raw: `{"type":"item.completed","item":{"id":"i4","type":"agent_message","text":"a"}}
{"type":"item.completed","item":{"id":"i5","type":"agent_message","text":"b"}}
`},
		{name: "gemini", harness: "gemini-cli", cell: M{"id": "g", "harness": "gemini-cli", "config": "inherit"}, state: M{}, stage: "execute",
			raw: `{"type":"init","timestamp":"2026-01-01T00:00:00Z","session_id":"gs","model":"gemini-x"}
{"type":"message","role":"assistant","content":"Hel","delta":true}
{"type":"message","role":"user","content":"ignored"}
{"type":"message","role":"assistant","content":"lo"}
{"type":"tool_use","timestamp":"2026-01-01T00:00:02Z","tool_name":"run_shell_command","tool_id":"t1","parameters":{"command":"ls"}}
{"type":"tool_result","timestamp":"2026-01-01T00:00:03Z","tool_id":"t1","status":"error","error":{"message":"denied"}}
{"type":"message","role":"assistant","content":"Done"}
{"type":"result","status":"success","stats":{"output_tokens":42,"tool_calls":1}}
`},
		{name: "gemini no result", harness: "gemini-cli", cell: M{"id": "g", "harness": "gemini-cli", "config": "inherit"}, state: M{}, stage: "narrate",
			raw: `{"type":"message","role":"assistant","content":"A"}
{"type":"tool_use","tool_name":"x","tool_id":"t1"}
{"type":"message","role":"assistant","content":"B"}
`},
		{name: "cursor", harness: "cursor-agent", cell: M{"id": "c", "harness": "cursor-agent", "config": "inherit"}, state: M{}, stage: "execute",
			raw: `{"type":"system","subtype":"init","session_id":"cs","model":"auto"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Looking"},{"type":"text","text":" now"}]}}
{"type":"tool_call","subtype":"started","call_id":"c1","tool_call":{"shellToolCall":{"args":{"command":"ls"}},"zzz":{}}}
{"type":"tool_call","subtype":"completed","call_id":"c1","tool_call":{"shellToolCall":{"args":{"command":"ls"},"result":{"error":{"msg":"naïve"}}}}}
{"type":"tool_call","subtype":"started","call_id":"c2","tool_call":{"readToolCall":{"args":{"path":"x"}}}}
{"type":"tool_call","subtype":"completed","call_id":"c2","tool_call":{"readToolCall":{"result":{"success":{"content":"ok"}}}}}
{"type":"result","subtype":"success","result":"All good","duration_ms":1234,"is_error":false}
`},
		{name: "cursor narrate", harness: "cursor-agent", cell: M{"id": "c", "harness": "cursor-agent", "config": "inherit"}, state: M{}, stage: "narrate",
			raw: `{"type":"assistant","message":{"content":[{"type":"text","text":"Para one"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Para two"}]}}
`},
		{name: "copilot", harness: "copilot-cli", cell: M{"id": "p", "harness": "copilot-cli", "config": "inherit"}, state: M{}, stage: "execute",
			raw: "line one\n{\"json\": 1}\nline three\n"},
		{name: "declarative", harness: "mytool", adapter: M{"execute": []any{"true"}}, cell: M{"id": "d", "harness": "mytool", "config": "inherit", "model": "m1"},
			state: M{"execute": M{"session_id": "s1"}}, stage: "narrate", raw: "a\nb\n"},
		{name: "console demo execute", harness: "console-demo", trialDir: "../../skills/ajx/examples/console-demo",
			cell: M{"id": "demo", "harness": "console-demo", "config": "inherit"}, state: M{}, stage: "execute", raw: demoRaw(len(demoSteps))},
		{name: "console demo interrupted", harness: "console-demo", trialDir: "../../skills/ajx/examples/console-demo",
			cell: M{"id": "demo", "harness": "console-demo", "config": "inherit"}, state: M{}, stage: "execute", raw: demoRaw(4)},
		{name: "console demo progress", harness: "console-demo", trialDir: "../../skills/ajx/examples/console-demo",
			cell: M{"id": "demo", "harness": "console-demo", "config": "inherit"}, state: M{}, stage: "execute", raw: demoRaw(len(demoSteps)), progress: true},
		{name: "console demo narrate", harness: "console-demo", trialDir: "../../skills/ajx/examples/console-demo",
			cell: M{"id": "demo", "harness": "console-demo", "config": "inherit"}, state: M{}, stage: "narrate", raw: demoJourney},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runParity(t, py, c) })
	}
}

// TestJSONLRecordsAreSplitOnNewlinesOnly is a deliberate divergence: Python's
// str.splitlines also splits on U+2028/U+2029/U+0085, which are legal raw
// characters inside JSON strings (Claude Code transcripts contain them raw),
// so the Python adapter drops such a record. The Go port keeps it.
func TestJSONLRecordsAreSplitOnNewlinesOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(path, []byte("{\"text\": \"a\u2028b\u0085c\"}\n{\"n\": 1}\r\n"), 0o644)
	rows := newScope().readJSONL(path)
	eq(t, rows, []any{M{"text": "a\u2028b\u0085c"}, M{"n": 1}}, "rows")
}

func demoRaw(n int) string {
	var lines []string
	for _, s := range demoSteps[:n] {
		lines = append(lines, s.line)
	}
	return strings.Join(lines, "\n") + "\n"
}

// TestPluginsListingMatchesPython compares the `ajx plugins` view of the
// registry (names and listed attributes) with the Python CLI's.
func TestPluginsListingMatchesPython(t *testing.T) {
	py := python(t)
	var b strings.Builder
	for _, kind := range plugin.Kinds {
		b.WriteString(kind + ":\n")
		for _, name := range plugin.Names(kind) {
			if name == "console-demo" {
				continue // registered on demand when a trial bundles console_demo.py (other tests do)
			}
			extra := ""
			switch kind {
			case "harness":
				h, _ := plugin.NewHarness(name)
				hb := h.Base()
				installed := "no"
				if h.Available() {
					installed = "yes"
				}
				extra = "  installed=" + installed + " verified_live=" + pyBool(hb.VerifiedLive) + " resume=" + pyBool(hb.CanResume) +
					" reporter=" + pyBool(hb.CanReport) + " clean=" + pyBool(hb.CleanSupported) + " default_auth=" + hb.DefaultAuthName()
			case "auth":
				a, _ := plugin.NewAuth(name, nil)
				extra = "  for=" + strings.Join(a.Base().Harnesses, ",") + "  " + a.Base().Description
			}
			b.WriteString("  " + padRight(name, 22) + extra + "\n")
		}
	}
	cmd := exec.Command(py, "-I", "../../skills/ajx/bin/ajx", "plugins")
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir()) // no ~/.config/ajx/plugins
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(out) {
		t.Fatalf("listing differs\n--- go\n%s--- python\n%s", b.String(), out)
	}
}

func pyBool(v bool) string {
	if v {
		return "True"
	}
	return "False"
}

func padRight(s string, n int) string {
	for util.RuneLen(s) < n {
		s += " "
	}
	return s
}

// TestTextHelpersMatchPython checks json.dumps (both ensure_ascii modes),
// repr/str, summarize_input, excerpt and _one_line on tricky values.
func TestTextHelpersMatchPython(t *testing.T) {
	py := python(t)
	inputs := `[{"b": 1, "a": [1.5, 1e-07, 1e+16, 100000000000000000000.0, -0.0, true, null, "x\u007f\u0085\u2028\ud83d\udea7'\"\\"]},
		"  héllo\u00a0\u001c wörld \t\n", {"command": ["ls", 1, 2.0, null, true, {"k": "v'"}]}, {"path": {"z": 1, "y": [2]}},
		{"todos": [{"status": "pending", "content": "x"}]}, "it's", "both ' and \"", "` + strings.Repeat("é", 400) + `", 12, 3.25, null]`
	s := newScope()
	vals, err := s.decode([]byte(inputs))
	if err != nil {
		t.Fatal(err)
	}
	var got []any
	for _, v := range vals.([]any) {
		str, _ := v.(string)
		got = append(got, []any{s.dumps(v, true), s.dumps(v, false), s.repr(v), s.pyStr(v), s.summarize(v), s.excerpt(v, 20),
			oneLine(str, 12)})
	}
	script := `
import json, sys
sys.path.insert(0, sys.argv[1])
from ajx.base import excerpt, summarize_input
from ajx.builtin.claude_code import _one_line
out = []
for v in json.loads(sys.stdin.read()):
    out.append([json.dumps(v), json.dumps(v, ensure_ascii=False), repr(v), str(v), summarize_input(v), excerpt(v, 20),
                _one_line(v if isinstance(v, str) else "", 12)])
print(json.dumps(out))
`
	lib, _ := filepath.Abs("../../skills/ajx/lib")
	cmd := exec.Command(py, "-I", "-c", script, lib)
	cmd.Stdin = strings.NewReader(inputs)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := util.DecodeJSON(out)
	g := semantic(t, got)
	wl, gl := want.([]any), g.([]any)
	for i := range wl {
		if !reflect.DeepEqual(gl[i], wl[i]) {
			t.Errorf("value %d:\n go     %q\n python %q", i, gl[i], wl[i])
		}
	}
}
