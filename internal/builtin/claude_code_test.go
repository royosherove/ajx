package builtin

// Offline tests for the Claude Code stream adapter (ports of
// test_claude_code_stream.py and the Claude tests in test_ajx.py): sessions
// that emit several `result` events, reporter result folding, the live
// progress hook, transcript reconciliation and forked narration. Every stream
// and transcript is synthetic; nothing launches a real `claude`.

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

const (
	ccSID          = "00000000-0000-4000-8000-000000000011"
	ccModel        = "claude-haiku-4-5"
	multiResultTag = "result events in one session"
)

func ccCell() M { return M{"id": "c", "harness": "claude-code", "config": "clean", "model": "haiku"} }

func ccExecuteCtx(t *testing.T, tmp string) *plugin.Ctx {
	return testCtx(t, tmp, ccCell(), M{"execute": M{"session_id": ccSID, "config_isolated": true}}, tmp)
}

func ccInit() M {
	return M{"type": "system", "subtype": "init", "session_id": ccSID, "cwd": "/tmp/synthetic", "model": ccModel,
		"claude_code_version": "0.0.0-fixture", "permissionMode": "bypassPermissions", "apiKeySource": "none",
		"slash_commands": []any{}, "mcp_servers": []any{}}
}

func ccAssistant(mid string, block M, ts string, outputTokens int64, parent string) M {
	ev := M{"type": "assistant", "session_id": ccSID, "timestamp": ts,
		"message": M{"id": mid, "model": ccModel, "role": "assistant", "usage": M{"output_tokens": outputTokens},
			"content": []any{block}}}
	if parent != "" {
		ev["parent_tool_use_id"] = parent
	}
	return ev
}

func ccToolUse(tid, command string) M {
	return M{"type": "tool_use", "id": tid, "name": "Bash", "input": M{"command": command}}
}

func ccText(text string) M { return M{"type": "text", "text": text} }

func ccToolResult(tid, ts, content string) M {
	return M{"type": "user", "session_id": ccSID, "timestamp": ts,
		"message": M{"role": "user", "content": []any{M{"type": "tool_result", "tool_use_id": tid,
			"content": content, "is_error": false}}}}
}

type resultOpts struct {
	modelCost any
	text      string
	spawned   int64
	subtype   string
}

func ccResult(numTurns, durationMS int64, cost float64, outputTokens int64, o resultOpts) M {
	if o.text == "" {
		o.text = "segment done"
	}
	if o.subtype == "" {
		o.subtype = "success"
	}
	var modelCost any = cost
	if o.modelCost != nil {
		modelCost = o.modelCost
	}
	return M{"type": "result", "subtype": o.subtype, "session_id": ccSID, "result": o.text, "is_error": false,
		"num_turns": numTurns, "duration_ms": durationMS, "total_cost_usd": cost,
		"usage": M{"output_tokens": outputTokens},
		"modelUsage": M{ccModel: M{"inputTokens": int64(100), "outputTokens": outputTokens, "cacheReadInputTokens": int64(50),
			"costUSD": modelCost, "contextWindow": int64(200000)}},
		"terminal_reason": "completed", "subagent_stats": M{"spawned": o.spawned}}
}

// Three segments of one session: the agent scheduled its own continuation
// twice. Per-segment usage is 10, 20 and 5 output tokens; total_cost_usd is a
// running total (0.10 -> 0.25 -> 0.30).
func multiStream() []any {
	return []any{
		ccInit(),
		ccAssistant("msg-a1", ccToolUse("tool-a1", "echo one"), T[1], 6, ""), ccToolResult("tool-a1", T[2], "one"),
		ccAssistant("msg-a2", ccText("First segment."), T[3], 4, ""),
		ccResult(2, 1000, 0.10, 10, resultOpts{text: "First segment."}),
		ccAssistant("msg-b1", ccToolUse("tool-b1", "echo two"), T[5], 20, ""), ccToolResult("tool-b1", T[6], "two"),
		ccResult(3, 2000, 0.25, 20, resultOpts{text: "Second segment."}),
		ccAssistant("msg-c1", ccText("All done."), T[8], 5, ""),
		ccResult(1, 500, 0.30, 5, resultOpts{text: "All done."}),
	}
}

func multiTranscript() []any {
	return []any{
		M{"type": "user", "sessionId": ccSID, "timestamp": T[0], "message": M{"role": "user", "content": "Run the task."}},
		ccAssistant("msg-a1", ccToolUse("tool-a1", "echo one"), T[1], 6, ""), ccToolResult("tool-a1", T[2], "one"),
		ccAssistant("msg-a2", ccText("First segment."), T[3], 4, ""),
		ccAssistant("msg-b1", ccToolUse("tool-b1", "echo two"), T[5], 20, ""), ccToolResult("tool-b1", T[6], "two"),
		ccAssistant("msg-c1", ccText("All done."), T[8], 5, ""),
	}
}

func ccNormalize(t *testing.T, stream []any, transcript []any) M {
	tmp := t.TempDir()
	ctx := ccExecuteCtx(t, tmp)
	writeRaw(t, filepath.Join(ctx.RunDir, "execute.raw.jsonl"), stream, "")
	if transcript != nil {
		writeTranscript(t, tmp, transcript, ccSID)
	}
	return mustNormalize(t, harness(t, "claude-code"), ctx, "execute", nil)
}

func TestClaudeSegmentsAreSummedAndCostIsTheRunningTotal(t *testing.T) {
	tel := ccNormalize(t, multiStream(), multiTranscript())
	hr := tel["harness_reported"].(M)
	eq(t, hr["result_events"], 3, "result_events")
	eq(t, hr["num_turns"], 6, "num_turns")
	eq(t, hr["duration_ms"], 3500, "duration_ms")
	eq(t, hr["cost_estimate_usd"], 0.30, "cost")
	eq(t, hr["subagents_spawned"], 0, "spawned")
	eq(t, hr["model_usage"], M{ccModel: M{"inputTokens": 300, "outputTokens": 35, "cacheReadInputTokens": 150,
		"costUSD": 0.30, "contextWindow": 200000}}, "model_usage")
	eq(t, tel["final_text"], "All done.", "final_text")
	eq(t, tel["stop_reason"], "success", "stop_reason")
	eq(t, hr["result_subtype"], "success", "result_subtype")
	eq(t, tel["usage_status"], "complete", "usage_status")
	eq(t, tel["expected_output_tokens"], 35, "expected")
	sum := 0.0
	for _, u := range util.Maps(tel["usage"]) {
		sum += num(u["output_tokens"])
	}
	eq(t, sum, 35, "usage sum")
	eq(t, tel["tool_status"], "complete", "tool_status")
	eq(t, tel["expected_tool_calls"], 2, "expected tools")
	lims := strs(tel["limitations"])
	notes := containsAny(lims, multiResultTag)
	if len(notes) != 1 {
		t.Fatalf("notes %v", lims)
	}
	for _, want := range []string{"execute: 3 result events in one session (self-resumed/scheduled continuation)",
		"turns, duration and usage summed across segments", "cost taken as the cumulative session total"} {
		if !strings.Contains(notes[0], want) {
			t.Fatalf("%q missing %q", notes[0], want)
		}
	}
	if len(containsAny(lims, "!= stream result")) > 0 {
		t.Fatal(lims)
	}
}

func TestClaudeNonCumulativeCostFallsBackToTheSum(t *testing.T) {
	tel := ccNormalize(t, []any{ccInit(), ccResult(2, 1000, 0.20, 10, resultOpts{}), ccResult(1, 500, 0.15, 5, resultOpts{text: "done"})}, nil)
	hr := tel["harness_reported"].(M)
	if math.Abs(num(hr["cost_estimate_usd"])-0.35) > 1e-9 {
		t.Fatal(hr["cost_estimate_usd"])
	}
	if math.Abs(num(hr["model_usage"].(M)[ccModel].(M)["costUSD"])-0.35) > 1e-9 {
		t.Fatal(hr["model_usage"])
	}
	eq(t, hr["result_events"], 2, "result_events")
	notes := containsAny(strs(tel["limitations"]), multiResultTag)
	if len(notes) != 1 || !strings.Contains(notes[0], "summed from non-cumulative segments") ||
		!strings.Contains(notes[0], "total_cost_usd and modelUsage costUSD decreased between segments") ||
		strings.Contains(notes[0], "cumulative session total") {
		t.Fatal(notes)
	}
}

func TestClaudeSingleResultSessionIsUnchanged(t *testing.T) {
	tmp := t.TempDir()
	sid := "00000000-0000-4000-8000-000000000001"
	ctx := testCtx(t, tmp, ccCell(), M{"execute": M{"session_id": sid, "config_isolated": true}}, tmp)
	writeRawLines(t, filepath.Join(ctx.RunDir, "execute.raw.jsonl"), fixtureLines(t, "claude-stream.jsonl"))
	var rows []any
	for _, l := range fixtureLines(t, "claude-transcript.jsonl") {
		v, _ := newScope().decode([]byte(l))
		rows = append(rows, v)
	}
	writeTranscript(t, tmp, rows, sid)
	tel := mustNormalize(t, harness(t, "claude-code"), ctx, "execute", nil)
	hr := tel["harness_reported"].(M)
	eq(t, hr["result_events"], 1, "result_events")
	eq(t, []any{hr["num_turns"], hr["duration_ms"], hr["cost_estimate_usd"]}, []any{2, 4000, 0.025}, "figures")
	eq(t, hr["model_usage"], M{"claude-haiku-4-5": M{"outputTokens": 131}}, "model_usage kept verbatim")
	eq(t, hr["subagents_spawned"], 0, "spawned")
	eq(t, tel["usage_status"], "complete", "usage_status")
	eq(t, tel["expected_output_tokens"], 131, "expected")
	eq(t, tel["final_text"], "It printed `hi`.", "final_text")
	if len(containsAny(strs(tel["limitations"]), multiResultTag)) > 0 {
		t.Fatal(tel["limitations"])
	}
}

func TestClaudeLastResultDecidesTextSubtypeAndErrorState(t *testing.T) {
	last := ccResult(1, 500, 0.12, 3, resultOpts{subtype: "error_during_execution"})
	last["result"] = ""
	last["is_error"] = true
	tel := ccNormalize(t, []any{ccInit(), ccResult(2, 1000, 0.10, 10, resultOpts{text: "fine"}), last}, nil)
	eq(t, tel["stop_reason"], "error_during_execution", "stop_reason")
	eq(t, tel["final_text"], "", "final_text")
	eq(t, tel["harness_reported"].(M)["is_error"], true, "is_error")
	eq(t, tel["harness_reported"].(M)["num_turns"], 3, "num_turns")
}

func TestClaudeSubagentsSpawnedSumsAcrossSegments(t *testing.T) {
	tel := ccNormalize(t, []any{ccInit(), ccResult(2, 1000, 0.10, 10, resultOpts{spawned: 1}),
		ccResult(1, 500, 0.12, 3, resultOpts{spawned: 2})}, nil)
	eq(t, tel["harness_reported"].(M)["subagents_spawned"], 3, "spawned")
}

func TestClaudeResultFoldingHelpers(t *testing.T) {
	v, summed := runningTotal([]any{0.2596, 0.3177, 1.1042, 1.1259})
	eq(t, []any{v, summed}, []any{1.1259, false}, "monotonic")
	v, summed = runningTotal([]any{0.1, 0.1})
	eq(t, []any{v, summed}, []any{0.1, false}, "free segment keeps the total")
	v, summed = runningTotal([]any{0.025})
	eq(t, []any{v, summed}, []any{0.025, false}, "single")
	v, summed = runningTotal([]any{0.2, nil, 0.15})
	if math.Abs(num(v)-0.35) > 1e-9 || !summed {
		t.Fatal(v, summed)
	}
	v, summed = runningTotal([]any{nil, "n/a", true})
	if v != nil || summed {
		t.Fatal(v, summed)
	}
	eq(t, sumOrNone([]any{int64(22), int64(5), int64(40), int64(1)}), 68, "sum")
	eq(t, sumOrNone([]any{nil, int64(4)}), 4, "sum skip")
	if sumOrNone([]any{nil, "x", false}) != nil {
		t.Fatal("expected nil")
	}
	if _, ok := sumOrNone([]any{int64(2), int64(3)}).(int64); !ok {
		t.Fatal("int sums stay int")
	}
}

func TestClaudeMergeModelUsage(t *testing.T) {
	s := newScope()
	merged, summed := s.mergeModelUsage([]any{
		M{"m1": M{"inputTokens": int64(1), "outputTokens": int64(10), "costUSD": 0.1, "contextWindow": int64(200000), "tier": "a"}},
		M{"m1": M{"inputTokens": int64(2), "outputTokens": int64(20), "costUSD": 0.3, "contextWindow": int64(200000), "tier": "b"},
			"m2": M{"outputTokens": int64(7), "webSearchRequests": int64(1), "costUSD": 0.05}},
		nil,
		M{"m1": M{"outputTokens": int64(5), "costUSD": 0.35, "cacheCreationInputTokens": int64(9)}},
	})
	if summed {
		t.Fatal("not summed")
	}
	eq(t, merged["m1"], M{"inputTokens": 3, "outputTokens": 35, "costUSD": 0.35, "contextWindow": 200000,
		"tier": "a", "cacheCreationInputTokens": 9}, "m1")
	eq(t, merged["m2"], M{"outputTokens": 7, "webSearchRequests": 1, "costUSD": 0.05}, "m2")
	merged, summed = s.mergeModelUsage([]any{M{"m1": M{"costUSD": 0.2}}, M{"m1": M{"costUSD": 0.15}}})
	if !summed || math.Abs(num(merged["m1"].(M)["costUSD"])-0.35) > 1e-9 {
		t.Fatal(merged, summed)
	}
}

func TestClaudeAggregateResultsMatchesTheObservedLiveShape(t *testing.T) {
	turns := []int64{22, 5, 40, 1}
	durations := []int64{277755, 19003, 412990, 4831}
	costs := []float64{0.2596, 0.3177, 1.1042, 1.1259}
	tokens := []int64{9000, 2000, 10626, 207}
	var results []M
	for i := range turns {
		results = append(results, ccResult(turns[i], durations[i], costs[i], tokens[i], resultOpts{}))
	}
	agg := newScope().aggregateResults(results)
	eq(t, []any{agg.count, agg.numTurns, agg.durationMS}, []any{4, 68, 714579}, "counts")
	eq(t, agg.costEstimate, 1.1259, "cost")
	eq(t, agg.outputTokens, 21833, "tokens")
	eq(t, agg.modelUsage.(M)[ccModel].(M)["outputTokens"], 21833, "model tokens")
	eq(t, agg.modelIDs, []string{ccModel}, "model ids")
	if util.Compact(agg.last) != util.Compact(results[3]) || agg.costSummed || agg.modelCostSummed {
		t.Fatal("last/summed")
	}
}

func ccReport(t *testing.T, events []any) M {
	tmp := t.TempDir()
	ctx := testCtx(t, tmp, ccCell(), M{}, tmp)
	ctx.StageName = "extract"
	f := &fakeRun{write: func(ctx *plugin.Ctx, argv []string, stage string) {
		writeRaw(t, filepath.Join(ctx.RunDir, stage+".raw.jsonl"), events, "")
	}}
	f.install(t)
	res, err := harness(t, "claude-code").Report(ctx, "Synthetic evidence only.", M{"type": "object"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(util.Compact(f.argvs), "Synthetic evidence only.") {
		t.Fatal("prompt leaked into argv")
	}
	argv := f.argvs[0]
	if argv[0] != "claude" || argv[indexOf(argv, "--tools")+1] != "" || argv[indexOf(argv, "--json-schema")+1] != `{"type": "object"}` {
		t.Fatal(argv)
	}
	if f.opts[0].StdinData == nil || *f.opts[0].StdinData != "Synthetic evidence only." {
		t.Fatal("prompt goes via stdin")
	}
	return res
}

func indexOf(items []string, s string) int {
	for i, x := range items {
		if x == s {
			return i
		}
	}
	return -1
}

func TestClaudeReportSingleResult(t *testing.T) {
	data := M{"asks": []any{}}
	text := newScope().dumps(data, true)
	res := ccReport(t, []any{M{"type": "result", "subtype": "success", "result": text, "structured_output": data,
		"is_error": false, "usage": M{"output_tokens": int64(12)}, "total_cost_usd": 0.02, "modelUsage": M{"fake": M{}}}})
	eq(t, res["structured"], data, "structured")
	eq(t, res["text"], text, "text")
	proc := res["proc"].(M)
	eq(t, []any{proc["output_tokens"], proc["model_ids"], proc["cost_estimate_usd"]}, []any{12, "fake", 0.02}, "proc")
	eq(t, proc["is_error"], false, "is_error")
	eq(t, proc["result_subtype"], "success", "subtype")
	eq(t, proc["tools_policy"], "tools disabled", "tools_policy")
	if _, ok := proc["result_events"]; ok {
		t.Fatal("result_events")
	}
}

func TestClaudeReportMultipleResultsUseTheLastTextAndFoldUsage(t *testing.T) {
	data := M{"asks": []any{M{"id": "a1"}}}
	text := newScope().dumps(data, true)
	res := ccReport(t, []any{
		M{"type": "result", "subtype": "success", "result": "thinking out loud", "is_error": false,
			"usage": M{"output_tokens": int64(12)}, "total_cost_usd": 0.10, "modelUsage": M{"m1": M{"outputTokens": int64(12)}}},
		M{"type": "result", "subtype": "success", "result": text, "structured_output": data,
			"is_error": false, "usage": M{"output_tokens": int64(8)}, "total_cost_usd": 0.30, "modelUsage": M{"m2": M{"outputTokens": int64(8)}}},
	})
	eq(t, res["structured"], data, "structured")
	eq(t, res["text"], text, "text")
	proc := res["proc"].(M)
	eq(t, []any{proc["output_tokens"], proc["model_ids"], proc["cost_estimate_usd"]}, []any{20, "m1,m2", 0.30}, "proc")
	eq(t, proc["result_events"], 2, "result_events")
	eq(t, proc["is_error"], false, "is_error")
}

func TestClaudeReportMissingResultIsStillAnError(t *testing.T) {
	res := ccReport(t, []any{ccInit()})
	proc := res["proc"].(M)
	eq(t, proc["is_error"], true, "is_error")
	eq(t, proc["result_subtype"], "missing result", "subtype")
	eq(t, []any{res["text"], res["structured"]}, []any{"", nil}, "text/structured")
}

func progressStream() []any {
	return []any{
		ccInit(),
		ccAssistant("m1", ccText("Let me look.\n\nSecond   line."), T[1], 3, ""),
		ccAssistant("m2", ccToolUse("t1", "echo hi && \\\n  echo there"), T[2], 3, ""),
		ccToolResult("t1", T[3], "hi\nthere"),
		ccAssistant("m3", M{"type": "thinking", "thinking": "private reasoning is not progress"}, T[4], 3, ""),
		ccAssistant("m4", M{"type": "tool_use", "id": "t2", "name": "Read", "input": M{"file_path": "/tmp/x.py", "limit": int64(5)}},
			T[5], 3, "t0"),
		ccResult(2, 4000, 0.01, 9, resultOpts{text: "Done"}),
	}
}

func ccProgress(t *testing.T, events []any, trailing string, limit int) []M {
	runDir := filepath.Join(t.TempDir(), "run")
	os.MkdirAll(runDir, 0o755)
	writeRaw(t, filepath.Join(runDir, "execute.raw.jsonl"), events, trailing)
	return harness(t, "claude-code").Progress(runDir, "execute", limit)
}

func kinds(items []M) []string {
	var out []string
	for _, i := range items {
		out = append(out, util.S(i["kind"]))
	}
	return out
}

func TestClaudeProgressKindsOrderAndOneLineText(t *testing.T) {
	items := ccProgress(t, progressStream(), "", 10)
	eq(t, kinds(items), []string{"text", "tool", "tool", "result"}, "kinds")
	eq(t, items[0], M{"at": "2026-01-01T00:00:01.000Z", "kind": "text", "text": "Let me look. Second line."}, "text")
	eq(t, items[1]["text"], "Bash: command: echo hi && \\ echo there", "tool")
	eq(t, items[2]["text"], "Read: file_path: /tmp/x.py", "sub-agent tool")
	eq(t, items[3], M{"at": "2026-01-01T00:00:06.000Z", "kind": "result", "text": "result success turns=2"}, "result")
	for _, i := range items {
		if strings.Contains(util.S(i["text"]), "\n") || util.RuneLen(util.S(i["text"])) > 200 {
			t.Fatal(i)
		}
	}
}

func TestClaudeProgressLimitKeepsTheMostRecentItems(t *testing.T) {
	events := []any{ccInit()}
	for i := 0; i < 15; i++ {
		events = append(events, ccAssistant("m", ccText("step "+itoa(i)), T[i%20], 1, ""))
	}
	items := ccProgress(t, events, "", 10)
	var texts []string
	for _, i := range items {
		texts = append(texts, util.S(i["text"]))
	}
	var want []string
	for i := 5; i < 15; i++ {
		want = append(want, "step "+itoa(i))
	}
	eq(t, texts, want, "last ten")
	eq(t, kinds(ccProgress(t, progressStream(), "", 2)), []string{"tool", "result"}, "limit 2")
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestClaudeProgressLongInputsAreTruncated(t *testing.T) {
	items := ccProgress(t, []any{ccInit(), ccAssistant("m1", ccToolUse("t1", strings.Repeat("x", 500)), T[1], 1, ""),
		ccAssistant("m2", ccText(strings.Repeat("y", 1000)), T[2], 1, "")}, "", 10)
	first := util.S(items[0]["text"])
	if !strings.HasPrefix(first, "Bash: command: xxx") || !strings.HasSuffix(first, "...") || util.RuneLen(first) > 160+len("Bash: ") {
		t.Fatal(first)
	}
	second := util.S(items[1]["text"])
	if util.RuneLen(second) != 200 || !strings.HasSuffix(second, "...") {
		t.Fatal(second)
	}
}

func TestClaudeProgressToleratesAPartialTrailingLineAndAMissingFile(t *testing.T) {
	half := `{"at": "2026-01-01T00:00:09.000Z", "line": "{\"type\":\"assistant\",\"message\":{\"con`
	eq(t, kinds(ccProgress(t, progressStream(), half, 10)), []string{"text", "tool", "tool", "result"}, "kinds")
	tmp := t.TempDir()
	h := harness(t, "claude-code")
	if got := h.Progress(filepath.Join(tmp, "nope"), "execute", 10); len(got) != 0 {
		t.Fatal(got)
	}
	if got := h.Progress(tmp, "narrate", 10); len(got) != 0 {
		t.Fatal(got)
	}
	var base plugin.BaseHarness
	if got := base.Progress(tmp, "execute", 10); len(got) != 0 {
		t.Fatal(got)
	}
}

// ------------------------------------------------------------ test_ajx.py

func TestClaudeNormalizeReconcilesTokensAndTools(t *testing.T) {
	tmp := t.TempDir()
	sid := "00000000-0000-4000-8000-000000000001"
	ctx := testCtx(t, tmp, ccCell(), M{"execute": M{"session_id": sid, "config_isolated": true}}, tmp)
	proj := filepath.Join(tmp, "projects", "-private-tmp-x")
	os.MkdirAll(proj, 0o755)
	data, _ := os.ReadFile(filepath.Join(fixtures, "claude-transcript.jsonl"))
	os.WriteFile(filepath.Join(proj, sid+".jsonl"), data, 0o644)
	writeRawLines(t, filepath.Join(ctx.RunDir, "execute.raw.jsonl"), fixtureLines(t, "claude-stream.jsonl"))
	tel := mustNormalize(t, harness(t, "claude-code"), ctx, "execute", nil)
	eq(t, tel["usage_status"], "complete", "usage_status")
	eq(t, tel["expected_output_tokens"], 131, "expected")
	eq(t, tel["tool_status"], "complete", "tool_status")
	eq(t, tel["expected_tool_calls"], 1, "tools")
	var tools []M
	for _, e := range util.Maps(tel["events"]) {
		if e["kind"] == "tool" {
			tools = append(tools, e)
		}
	}
	if tools[0]["name"] != "Bash" || !strings.Contains(util.S(tools[0]["input"]), "echo hi") ||
		tools[0]["is_error"] != false || !strings.Contains(util.S(tools[0]["output"]), "hi") {
		t.Fatal(tools[0])
	}
	eq(t, tel["final_text"], "It printed `hi`.", "final_text")
	if !strings.Contains(strs(tel["model_ids"])[0], "haiku") {
		t.Fatal(tel["model_ids"])
	}
	eq(t, tel["harness_reported"].(M)["cost_note"], "harness-computed estimate; not billed money", "cost_note")
	eq(t, tel["transcript_path"], filepath.Join(proj, sid+".jsonl"), "transcript_path")
}

func TestClaudeForkedNarrateTranscriptCountsOnlyNewMessages(t *testing.T) {
	// A --fork-session transcript repeats the task's messages (same ids);
	// narrate telemetry must exclude them so reporter tokens never leak into
	// the task's count and vice versa.
	tmp := t.TempDir()
	var rows []any
	var taskIDs []string
	for _, l := range fixtureLines(t, "claude-transcript.jsonl") {
		v, _ := util.DecodeJSON([]byte(l))
		rows = append(rows, v)
		if m := v.(M); m["type"] == "assistant" {
			taskIDs = append(taskIDs, util.S(m["message"].(M)["id"]))
		}
	}
	text := "# Journey: x\n" + strings.Repeat("y", 300)
	newMsg := M{"type": "assistant", "timestamp": "2026-01-01T00:05:00.000Z", "message": M{
		"id": "msg_new_narration", "model": "claude-haiku", "usage": M{"output_tokens": int64(1197)},
		"content": []any{M{"type": "text", "text": text}}}}
	ctx := testCtx(t, tmp, ccCell(), M{"execute": M{"session_id": "orig", "config_isolated": true},
		"narrate": M{"session_id": "forked"}}, tmp)
	proj := filepath.Join(tmp, "projects", "-slug")
	os.MkdirAll(proj, 0o755)
	var lines []string
	for _, r := range append(rows, newMsg) {
		lines = append(lines, util.Compact(r))
	}
	os.WriteFile(filepath.Join(proj, "forked.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	result := M{"type": "result", "subtype": "success", "result": text, "usage": M{"output_tokens": int64(1197)}, "modelUsage": M{}}
	writeRaw(t, filepath.Join(ctx.RunDir, "narrate.raw.jsonl"), []any{result}, "")
	tel := mustNormalize(t, harness(t, "claude-code"), ctx, "narrate", taskIDs)
	var ids []string
	for _, u := range util.Maps(tel["usage"]) {
		ids = append(ids, util.S(u["id"]))
	}
	eq(t, ids, []string{"msg_new_narration"}, "usage ids")
	eq(t, tel["usage_status"], "complete", "usage_status")
	eq(t, tel["expected_output_tokens"], 1197, "expected")
	eq(t, kinds(util.Maps(tel["events"])), []string{"text"}, "the task's tool calls are not re-counted")
	if !strings.HasPrefix(util.S(tel["final_text"]), "# Journey") {
		t.Fatal(tel["final_text"])
	}
}

func TestClaudeMissingTranscriptAndAJXSkillLimitations(t *testing.T) {
	init := ccInit()
	init["slash_commands"] = []any{"ajx", "review", "AJX:run"}
	init["mcp_servers"] = []any{M{"name": "x"}}
	tel := ccNormalize(t, []any{init}, nil)
	lims := strs(tel["limitations"])
	want := []string{
		"execute: AJX skill(s) ['AJX:run', 'ajx'] were listed in the worker's session (user config); the run is evaluation-aware",
		"execute: no result event in stream (killed, crashed, or timed out)",
		"execute: transcript for session " + ccSID + " not found (remote runner or moved config); tokens unavailable",
	}
	eq(t, lims, want, "limitations")
	hr := tel["harness_reported"].(M)
	eq(t, hr["skills_visible"], 3, "skills_visible")
	eq(t, hr["mcp_servers"], []any{"x"}, "mcp_servers")
	eq(t, tel["usage_status"], "unavailable", "usage_status")
}

// ------------------------------------------------------------ launch argv

func TestClaudeExecuteAndNarrateArgv(t *testing.T) {
	tmp := t.TempDir()
	cell := M{"id": "c", "harness": "claude-code", "config": "clean", "model": "sonnet", "effort": "high", "args": []any{"--extra"}}
	ctx := testCtx(t, tmp, cell, M{"execute": M{"session_id": "sid-1", "config_isolated": true}}, filepath.Join(tmp, "cfg"))
	ctx.Spec = M{"trial": M{"max_budget_usd": 2.5}}
	ctx.PromptText = "do the task"
	f := &fakeRun{write: func(ctx *plugin.Ctx, argv []string, stage string) {
		if stage == "narrate" {
			writeRaw(t, filepath.Join(ctx.RunDir, "narrate.raw.jsonl"), []any{M{"type": "system", "subtype": "init", "session_id": "forked-1"}}, "")
		}
	}}
	f.install(t)
	h := harness(t, "claude-code")
	proc, err := h.Execute(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, f.argvs[0], []string{"claude", "-p", "--output-format", "stream-json", "--verbose", "--session-id", "sid-1",
		"--permission-mode", "bypassPermissions", "--model", "sonnet", "--effort", "high", "--safe-mode", "--strict-mcp-config",
		"--max-budget-usd", "2.5", "--extra"}, "execute argv")
	eq(t, f.opts[0].ExtraEnv, map[string]string{"DISABLE_AUTOUPDATER": "1", "CLAUDE_CONFIG_DIR": filepath.Join(tmp, "cfg")}, "clean env")
	eq(t, *f.opts[0].StdinData, "do the task", "stdin prompt")
	eq(t, []any{proc["session_id"], proc["config_isolated"]}, []any{"sid-1", true}, "proc")
	proc, err = h.Narrate(ctx, "tell me")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, f.argvs[1], []string{"claude", "-p", "--resume", "sid-1", "--fork-session", "--tools", "",
		"--output-format", "stream-json", "--verbose", "--model", "sonnet", "--effort", "high", "--safe-mode", "--strict-mcp-config"}, "narrate argv")
	eq(t, []any{proc["session_id"], proc["tools_disabled"]}, []any{"forked-1", true}, "narrate proc")
	plan := h.Plan(ctx)
	if len(util.S(plan["session_id"])) != 36 || plan["config_isolated"] != true {
		t.Fatal(plan)
	}
	// a subscription login is not visible from a fresh config dir: clean = --safe-mode only
	auth, _ := plugin.NewAuth("claude-subscription", nil)
	ctx.Auth = auth
	if env := h.CleanEnv(ctx); env["CLAUDE_CONFIG_DIR"] != "" {
		t.Fatal(env)
	}
	args, ok, err := CommonArgs(h, ctx)
	if !ok || err != nil || args[indexOf(args, "--model")+1] != "sonnet" {
		t.Fatal(args, ok, err)
	}
}

func TestClaudeSettingsEnvAndToolShell(t *testing.T) {
	tmp := t.TempDir()
	cfg := filepath.Join(tmp, "cfg")
	os.MkdirAll(cfg, 0o755)
	os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"env": {"A": "1", "N": 2, "B": true, "F": 1.5}}`), 0o644)
	proj := filepath.Join(tmp, "proj", ".claude")
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, "settings.local.json"), []byte(`{"env": {"L": "x"}}`), 0o644)
	os.WriteFile(filepath.Join(proj, "settings.json"), []byte(`not json`), 0o644)
	managed := filepath.Join(tmp, "managed.json")
	os.WriteFile(managed, []byte(`{"env": {"M": "m"}}`), 0o644)
	old := ClaudeManagedSettings
	ClaudeManagedSettings = managed
	defer func() { ClaudeManagedSettings = old }()
	layers := harness(t, "claude-code").SettingsEnv(map[string]string{"CLAUDE_CONFIG_DIR": cfg}, filepath.Join(tmp, "proj"))
	if len(layers) != 3 {
		t.Fatal(layers)
	}
	eq(t, layers[0].Env, map[string]string{"A": "1", "N": "2", "B": "True", "F": "1.5"}, "str() values")
	eq(t, []string{layers[0].Source, layers[1].Source, layers[2].Source},
		[]string{filepath.Join(cfg, "settings.json"), filepath.Join(proj, "settings.local.json"), managed}, "sources")
	h := harness(t, "claude-code")
	eq(t, h.ToolShell(map[string]string{"CLAUDE_CODE_SHELL": "/x/fish", "SHELL": "/bin/zsh"}), "/x/fish", "explicit")
	eq(t, h.ToolShell(map[string]string{"SHELL": "/opt/bin/bash"}), "/opt/bin/bash", "bash")
	if sh := h.ToolShell(map[string]string{"SHELL": "/usr/bin/fish"}); !strings.HasSuffix(sh, "zsh") && !strings.HasSuffix(sh, "bash") && sh != "/bin/sh" {
		t.Fatal(sh)
	}
	b := h.Base()
	if !b.ToolShellObserved || util.Compact(b.ToolShellRestores) != `["PATH"]` {
		t.Fatal(b)
	}
}

// A safeguard can stop a response; Claude Code then continues once, and the
// continuation may be a refusal. The count is recorded as a harness report
// with a limitation, so the runner can reject the text.
func TestClaudeSafetyStopsAreRecordedWithALimitation(t *testing.T) {
	stopped := ccResult(2, 900, 0.2, 40, resultOpts{text: "Understood. I won't write that account again."})
	stopped["safety_stops"] = int64(1)
	tel := ccNormalize(t, []any{ccInit(), stopped}, nil)
	eq(t, tel["harness_reported"].(M)["safety_stops"], 1, "safety_stops")
	want := "execute: the model's safeguards stopped 1 response(s); text after a stop can be a refusal rather than the requested output"
	if indexOf(strs(tel["limitations"]), want) < 0 {
		t.Fatalf("limitation missing: %v", tel["limitations"])
	}
}

func TestClaudeNoSafetyStopKeyWithoutAStop(t *testing.T) {
	ok := ccResult(1, 10, 0.1, 5, resultOpts{})
	ok["safety_stops"] = int64(0)
	tel := ccNormalize(t, []any{ccInit(), ok}, nil)
	if _, has := tel["harness_reported"].(M)["safety_stops"]; has {
		t.Fatal("safety_stops recorded without a stop")
	}
	if len(containsAny(strs(tel["limitations"]), "safeguards")) > 0 {
		t.Fatal(tel["limitations"])
	}
}
