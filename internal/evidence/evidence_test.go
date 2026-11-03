package evidence

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

// canon round-trips v through JSON so int64/float64 and []M/[]any compare equal.
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

func jsonEq(t *testing.T, label string, got, want any) {
	t.Helper()
	g, w := canon(t, got), canon(t, want)
	if !reflect.DeepEqual(g, w) {
		gb, _ := json.MarshalIndent(g, "", " ")
		wb, _ := json.MarshalIndent(w, "", " ")
		t.Errorf("%s differs\n got: %s\nwant: %s", label, gb, wb)
	}
}

func tool(id, name string, input M, at, mid string) M {
	return plugin.ToolEvent(id, name, input, at, mid)
}

func sampleTel() M {
	tel := plugin.EmptyTelemetry(M{"usage_status": "complete", "expected_output_tokens": int64(30),
		"tool_status": "complete", "expected_tool_calls": int64(2)})
	t1 := tool("a", "Bash", M{"command": "ls"}, "2026-01-01T00:00:01Z", "m1")
	plugin.FinishTool(t1, "2026-01-01T00:00:03Z", true, "boom")
	t2 := tool("b", "Bash", M{"command": "ls -a"}, "2026-01-01T00:00:10Z", "m2")
	plugin.FinishTool(t2, "2026-01-01T00:00:11Z", false, "ok")
	tel["events"] = []any{plugin.TextEvent("hello", "2026-01-01T00:00:00Z", "m0"), t1, t2}
	tel["usage"] = []any{M{"id": "m0", "at": "2026-01-01T00:00:00Z", "output_tokens": int64(10)},
		M{"id": "m1", "at": "2026-01-01T00:00:01Z", "output_tokens": int64(5)},
		M{"id": "m2", "at": "2026-01-01T00:00:10Z", "output_tokens": int64(15)}}
	tel["message_ids"] = []any{"m0", "m1", "m2"}
	return AssignIDs(tel)
}

func eids(tel M) []string {
	var out []string
	for _, e := range util.Maps(tel["events"]) {
		out = append(out, util.Str(e["eid"]))
	}
	return out
}

func TestIDsCostsAndRetries(t *testing.T) {
	tel := sampleTel()
	if got := eids(tel); !reflect.DeepEqual(got, []string{"E-001", "E-002", "E-003"}) {
		t.Fatalf("eids %v", got)
	}
	asks := []M{{"id": "ASK-001", "event_refs": []any{"E-002", "E-003", "E-999"}}, {"id": "ASK-002", "event_refs": []any{"E-003"}}}
	AskCosts(asks, tel)
	m := util.MapOr(asks[0]["measured"])
	jsonEq(t, "tool_calls", m["tool_calls"], 2)
	jsonEq(t, "failed", m["failed_tool_calls"], 1)
	jsonEq(t, "tokens", m["output_tokens"], 20)
	jsonEq(t, "span", m["span_seconds"], 10.0)
	jsonEq(t, "shared", m["shared_events"], []any{"E-003"})
	jsonEq(t, "invalid", asks[0]["invalid_refs"], []any{"E-999"})
	state := M{"execute": M{"started_at": "2026-01-01T00:00:00Z", "stopped_at": "2026-01-01T00:00:20Z", "stop_reason": "exit"}}
	s := TaskSummary(state, tel)
	jsonEq(t, "retries", s["retries_after_error"], 1)
	jsonEq(t, "elapsed", s["elapsed_seconds"], 20.0)
	jsonEq(t, "tokens", s["output_tokens"], 30)
}

func TestIsolationScan(t *testing.T) {
	tel := sampleTel()
	extra := AssignIDs(M{"events": []any{tool("c", "Read", M{"file_path": "/tmp/example-home/.claude/skills/ajx/SKILL.md"}, "2026-01-01T00:00:12Z", "")}})
	tel["events"] = append(util.List(tel["events"]), util.List(extra["events"])[0])
	sp := M{"trial": M{"output_dir": "/out", "workspace_root": "/tmp"}, "path": "/trial/trial.toml"}
	flags := IsolationScan(tel, sp, M{"paths": M{"workspace": "/tmp/abc123def"}})
	if len(flags) != 1 || !util.Contains(util.Strings(util.MapOr(flags[0])["reasons"]), "skills dir") {
		t.Fatalf("flags %v", flags)
	}
}

func TestIsolationScanIgnoresOwnPathsContainingAjx(t *testing.T) {
	tel := AssignIDs(M{"events": []any{tool("c", "shell", M{"command": "ls", "cwd": "/tmp/ajx-ws/abc123def"}, "2026-01-01T00:00:12Z", "")}})
	sp := M{"trial": M{"output_dir": "/out", "workspace_root": "/tmp/ajx-ws"}, "path": "/trial/trial.toml"}
	if flags := IsolationScan(tel, sp, M{"paths": M{"workspace": "/tmp/ajx-ws/abc123def"}}); len(flags) != 0 {
		t.Fatalf("flags %v", flags)
	}
}

func TestIsolationScanScrubsOwnPathsBeforeNeedles(t *testing.T) {
	trialDir := "/trials/t1"
	tel := AssignIDs(M{"events": []any{tool("c", "shell", M{"command": "ls", "cwd": trialDir + "/ws/abc123def"}, "2026-01-01T00:00:12Z", "")}})
	sp := M{"trial": M{"output_dir": trialDir + "/ajx-reports", "workspace_root": trialDir + "/ws"}, "path": trialDir + "/trial.toml"}
	if flags := IsolationScan(tel, sp, M{"paths": M{"workspace": trialDir + "/ws/abc123def"}}); len(flags) != 0 {
		t.Fatalf("flags %v", flags)
	}
}

func TestIsolationScanIgnoresTempfileDirsUnderRoot(t *testing.T) {
	tel := AssignIDs(M{"events": []any{tool("c", "Bash", M{"command": "ls /tmp/tmpk3j2h1ab /tmp/0123456789"}, "2026-01-01T00:00:12Z", "")}})
	sp := M{"trial": M{"output_dir": "/out", "workspace_root": "/tmp"}, "path": "/trial/trial.toml"}
	flags := IsolationScan(tel, sp, M{"paths": M{"workspace": "/tmp/abcdef0123"}})
	if len(flags) != 1 {
		t.Fatalf("flags %v", flags)
	}
	jsonEq(t, "reasons", util.MapOr(flags[0])["reasons"], []any{"another run's workspace root entry"})
	// a longer hex-like name is not a run entry (negative lookahead)
	tel = AssignIDs(M{"events": []any{tool("c", "Bash", M{"command": "ls /tmp/0123456789abc /tmp/0123456789-x"}, "2026-01-01T00:00:12Z", "")}})
	if flags := IsolationScan(tel, sp, M{"paths": M{"workspace": "/tmp/abcdef0123"}}); len(flags) != 0 {
		t.Fatalf("flags %v", flags)
	}
}

func TestMeasureRunReconcilesPhases(t *testing.T) {
	tmp := t.TempDir()
	tel := sampleTel()
	if err := util.WriteJSON(filepath.Join(tmp, "telemetry.execute.json"), tel); err != nil {
		t.Fatal(err)
	}
	state := M{"execute": M{"started_at": "2026-01-01T00:00:00Z", "stopped_at": "2026-01-01T00:00:20Z", "stop_reason": "exit"},
		"narrate": M{"started_at": "2026-01-01T00:01:00Z", "stopped_at": "2026-01-01T00:02:00Z",
			"reconstruction": true, "output_tokens": int64(500)}}
	_ = util.WriteJSON(filepath.Join(tmp, "verify.json"), M{"window": M{"start": "2026-01-01T00:00:30Z", "end": "2026-01-01T00:00:40Z"}})
	out, err := MeasureRun(M{"trial": M{}}, M{}, tmp, state)
	if err != nil {
		t.Fatal(err)
	}
	if out["measurement_error"] != nil {
		t.Fatalf("measurement_error %v", out["measurement_error"])
	}
	sessions := map[string]M{}
	for _, s := range util.Maps(util.Get(out, "phase_measurements", "sessions")) {
		sessions[util.Str(s["id"])] = s
	}
	jsonEq(t, "tokens", sessions["execute"]["output_tokens"], 30)
	jsonEq(t, "tools", sessions["execute"]["tool_calls"], 2)
	jsonEq(t, "scope", util.Maps(sessions["execute"]["phases"])[0]["scope"], "task")
	jsonEq(t, "narrate", sessions["narrate"]["usage_status"], "partial")
	jsonEq(t, "verify scope", util.Maps(sessions["verify"]["phases"])[0]["scope"], "verification")
	if _, err := os.Stat(filepath.Join(tmp, "measurements.json")); err != nil {
		t.Fatal(err)
	}
}

func TestDigestIsBoundedForLongTasks(t *testing.T) {
	var events []any
	for i := 0; i < 500; i++ {
		at := fmt.Sprintf("2026-01-01T00:%02d:%02dZ", i/60, i%60)
		e := tool(fmt.Sprintf("t%d", i), "Bash", M{"command": fmt.Sprintf("cmd %d", i)}, at, fmt.Sprintf("m%d", i))
		plugin.FinishTool(e, at, i == 250, "out")
		events = append(events, e)
	}
	tel := AssignIDs(plugin.EmptyTelemetry(M{"events": events}))
	shown := DigestEvents(util.List(tel["events"]))
	if len(shown) >= 300 {
		t.Fatalf("len %d", len(shown))
	}
	omitted, found := 0, false
	for _, e := range util.Maps(shown) {
		if e["kind"] == "omitted" {
			omitted++
		}
		if e["eid"] == "E-251" {
			found = true
		}
	}
	if omitted != 1 || !found {
		t.Fatalf("omitted=%d found=%v", omitted, found)
	}
	digest := BuildDigest(M{"trial": M{}}, M{"harness": "fake", "config": "clean"},
		M{"execute": M{"started_at": "2026-01-01T00:00:00Z", "stopped_at": "2026-01-01T00:09:00Z"}}, tel, M{}, M{}, DigestOpts{})
	if !strings.Contains(digest, "routine events omitted") {
		t.Fatal("no omitted row")
	}
}

func TestGapMeasuredFromEndOfRunningTool(t *testing.T) {
	tl := tool("a", "Bash", M{"command": "npm install"}, "2026-01-01T00:00:00Z", "m1")
	plugin.FinishTool(tl, "2026-01-01T00:01:30Z", false, "ok")
	tel := AssignIDs(plugin.EmptyTelemetry(M{"events": []any{tl, plugin.TextEvent("done", "2026-01-01T00:01:31Z", "m2")}}))
	s := TaskSummary(M{"execute": M{}}, tel)
	jsonEq(t, "gaps", s["gaps_over_60s"], []any{})
}

func TestNarratorDigestOmitsVerification(t *testing.T) {
	tel := AssignIDs(plugin.EmptyTelemetry(M{"events": []any{}, "final_text": "```sh\necho hi\n```"}))
	verify := M{"checks": []any{M{"passed": false, "name": "x", "type": "shell", "detail": "exit=1"}}, "passed": int64(0),
		"total": int64(1), "outcome": "failed"}
	full := BuildDigest(M{"trial": M{}}, M{"harness": "h", "config": "clean"}, M{"execute": M{}}, tel, verify, M{}, DigestOpts{})
	narr := BuildDigest(M{"trial": M{}}, M{"harness": "h", "config": "clean"}, M{"execute": M{}}, tel, verify, M{}, DigestOpts{OmitVerification: true})
	if !strings.Contains(full, "FAIL: x") || strings.Contains(narr, "FAIL") ||
		strings.Contains(strings.ToLower(narr), "verification") || !strings.Contains(full, "````text") {
		t.Fatalf("full:\n%s\nnarr:\n%s", full, narr)
	}
}

// TestPythonGolden compares against outputs recorded from the Python
// implementation (testdata/digest_cases.json, synthetic data).
func TestPythonGolden(t *testing.T) {
	data, err := os.ReadFile("testdata/digest_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := util.DecodeJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range util.Maps(raw) {
		name := util.Str(c["name"])
		t.Run(name, func(t *testing.T) {
			tel := AssignIDs(util.DeepCopy(c["tel"]).(M))
			cell, state := util.MapOr(c["cell"]), util.MapOr(c["state"])
			verify, env := util.MapOr(c["verify"]), util.MapOr(c["environment"])
			if got := BuildDigest(M{"trial": M{}}, cell, state, tel, verify, env, DigestOpts{}); got != c["digest"] {
				t.Errorf("digest differs\n--- got\n%s\n--- want\n%s", got, c["digest"])
			}
			if got := BuildDigest(M{"trial": M{}}, cell, state, tel, verify, env, DigestOpts{OmitVerification: true}); got != c["narrator_digest"] {
				t.Errorf("narrator digest differs\n--- got\n%s\n--- want\n%s", got, c["narrator_digest"])
			}
			jsonEq(t, "summary", TaskSummary(state, tel), c["summary"])
			asks := []M{{"id": "ASK-001", "event_refs": []any{"E-002", "E-003", "E-999"}},
				{"id": "ASK-002", "event_refs": []any{"E-003", "E-004"}}, {"id": "ASK-003"}}
			AskCosts(asks, tel)
			jsonEq(t, "asks", asks, c["asks"])
			tmp := t.TempDir()
			if err := util.WriteJSON(filepath.Join(tmp, "telemetry.execute.json"), tel); err != nil {
				t.Fatal(err)
			}
			_ = util.WriteJSON(filepath.Join(tmp, "verify.json"), M{"window": M{"start": "2026-01-01T00:04:00Z", "end": "2026-01-01T00:04:10Z"}})
			meas, err := MeasureRun(M{"trial": M{}}, M{}, tmp, util.MapOr(c["measure_state"]))
			if err != nil {
				t.Fatal(err)
			}
			jsonEq(t, "measurements", meas, c["measurements"])
			jsonEq(t, "manifest", util.ReadMap(filepath.Join(tmp, "measure.manifest.json")), c["manifest"])
			jsonEq(t, "events", util.ReadJSONL(filepath.Join(tmp, "measure.events.jsonl")), c["measure_events"])
		})
	}
}
