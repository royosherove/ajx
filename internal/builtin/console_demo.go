package builtin

// Native port of skills/ajx/examples/console-demo/plugins/console_demo.py:
// a simulated worker for the AJX Console demo. It calls no agent, model or
// network. The worker prints a scripted session for a fictional `orders-cli`
// while it writes out.csv, and the narration and reporting calls return
// canned text. AJX_DEMO_PACE scales the pauses (default 1, about a minute).
// The processes are POSIX sh (sleep, printf, cat) instead of Python.

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

// demoWorker prints each scripted event after its pause, writing out.csv on
// the step that writes it. Args: csv, then (seconds, writes, line) triples.
const demoWorker = `csv=$1; shift
while [ $# -ge 3 ]; do
  sleep "$1"
  if [ "$2" = 1 ]; then printf '%s' "$csv" > out.csv; fi
  printf '%s\n' "$3"
  shift 3
done`

const demoEcho = `sleep "$1"; cat`
const demoSleep = `sleep "$1"; echo ok`

func demoPace(ctx *plugin.Ctx) float64 {
	value := ""
	if ctx != nil {
		value = ctx.Env["AJX_DEMO_PACE"]
	}
	if value == "" {
		value = os.Getenv("AJX_DEMO_PACE")
	}
	if value == "" {
		value = "1"
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(value), "_", ""), 64)
	if err != nil {
		return 1.0
	}
	if math.IsNaN(f) || f < 0 {
		return 0.0
	}
	return f
}

func seconds(f float64) string {
	if math.IsInf(f, 1) {
		f = 1e9
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// ConsoleDemo is the simulated console-demo harness.
type ConsoleDemo struct{ plugin.BaseHarness }

func newConsoleDemo() plugin.Harness {
	h := &ConsoleDemo{}
	h.BinaryName = "sh"
	h.CanResume = true
	h.CanReport = true
	h.CleanSupported = true
	h.Telemetry = "simulated events (no tokens)"
	return h
}

func (h *ConsoleDemo) Version() string { return "console-demo 1 (simulated)" }

func (h *ConsoleDemo) Plan(ctx *plugin.Ctx) M { return M{"session_id": demoSession} }

func (h *ConsoleDemo) Execute(ctx *plugin.Ctx) (M, error) {
	pace := demoPace(ctx)
	argv := []string{"sh", "-c", demoWorker, "console-demo", demoCSV}
	for _, step := range demoSteps {
		writes := "0"
		if step.writes {
			writes = "1"
		}
		argv = append(argv, seconds(step.pause*pace), writes, step.line)
	}
	proc, err := run(h, ctx, argv, "execute", plugin.RunOpts{})
	if proc != nil {
		proc["session_id"] = demoSession
	}
	return proc, err
}

func (h *ConsoleDemo) Progress(runDir, stage string, limit int) []M {
	if stage == "" {
		stage = "execute"
	}
	s := newScope()
	items := []M{}
	for _, r := range s.records(rawPath(runDir, stage)) {
		ev := r.evMap()
		if ev == nil {
			continue
		}
		switch ev["kind"] {
		case "text":
			items = append(items, M{"at": r.at, "kind": "text", "text": util.Or(ev["text"], "")})
		case "tool":
			text := s.pyStr(ev["name"]) + ": " + s.pyStr(ev["command"])
			if util.Truthy(ev["error"]) {
				text += "  ->  " + s.pyStr(ev["output"])
			}
			items = append(items, M{"at": r.at, "kind": "tool", "text": text, "error": util.Truthy(ev["error"])})
		}
	}
	return lastN(items, limit)
}

func (h *ConsoleDemo) Normalize(ctx *plugin.Ctx, stage string, excludeMessageIDs []string) (M, error) {
	s := newScope()
	rows := s.records(rawPath(ctx.RunDir, stage))
	tel := plugin.EmptyTelemetry(M{"session_id": demoSession, "model_ids": []any{"simulated"},
		"timestamp_source": "arrival", "limitations": []any{demoLimitation}})
	if stage != "execute" {
		lines := make([]string, len(rows))
		for i, r := range rows {
			lines[i] = r.line
		}
		tel["final_text"] = strings.Join(lines, "\n")
		return tel, nil
	}
	var parsed []rec
	for _, r := range rows {
		if r.evMap() != nil {
			parsed = append(parsed, r)
		}
	}
	events := []any{}
	ids := []any{}
	var final any = ""
	tools := 0
	for i, r := range parsed {
		ev := r.evMap()
		mid := fmt.Sprintf("m%d", i+1)
		ids = append(ids, mid)
		if ev["kind"] == "tool" {
			began := r.at
			if i > 0 {
				began = parsed[i-1].at // the call ran during the pause before its output
			}
			call := s.toolEvent(fmt.Sprintf("t%d", i+1), util.Or(ev["name"], "Bash"), M{"command": ev["command"]}, began, mid)
			s.finishTool(call, r.at, util.Truthy(ev["error"]), util.Or(ev["output"], ""))
			events = append(events, call)
			tools++
		} else {
			text, _ := util.Or(ev["text"], "").(string)
			events = append(events, s.textEvent(text, r.at, mid))
			final = util.Or(ev["text"], final)
		}
	}
	stop := "interrupted"
	if len(parsed) == len(demoSteps) {
		stop = "success"
	}
	tel["events"] = events
	tel["message_ids"] = ids
	tel["final_text"] = final
	tel["tool_status"] = "complete"
	tel["expected_tool_calls"] = int64(tools)
	tel["stop_reason"] = stop
	return tel, nil
}

func (h *ConsoleDemo) Narrate(ctx *plugin.Ctx, prompt string) (M, error) {
	tel := util.ReadMap(filepath.Join(ctx.RunDir, "telemetry.execute.json"))
	events := util.Maps(tel["events"])
	text := demoJourney
	if len(events) != len(demoSteps) {
		text = stoppedJourney(events)
	}
	argv := []string{"sh", "-c", demoEcho, "console-demo", seconds(2 * demoPace(ctx))}
	proc, err := run(h, ctx, argv, "narrate", plugin.RunOpts{StdinData: strp(text), Timeout: 60})
	if proc != nil {
		proc["session_id"] = demoSession
		proc["tools_disabled"] = true
	}
	return proc, err
}

func (h *ConsoleDemo) Report(ctx *plugin.Ctx, prompt string, schema M) (M, error) {
	stage := ctx.StageName
	if stage == "" {
		stage = "report"
	}
	argv := []string{"sh", "-c", demoSleep, "console-demo", seconds(3 * demoPace(ctx))}
	proc, err := run(h, ctx, argv, stage, plugin.RunOpts{Timeout: 60})
	if err != nil {
		return nil, err
	}
	s := newScope()
	var data M
	if _, ok := util.MapOr(schema["properties"])["asks"]; ok {
		tel := util.ReadMap(filepath.Join(ctx.RunDir, "telemetry.execute.json"))
		known := map[string]bool{}
		for _, e := range util.Maps(tel["events"]) {
			known[idKey(e["eid"])] = true
		}
		within := func(item M) bool {
			for _, ref := range util.List(item["event_refs"]) {
				if !known[idKey(ref)] {
					return false
				}
			}
			return true
		}
		pick := func(js string) []any {
			all, _ := s.decode([]byte(js))
			out := []any{}
			for _, item := range util.Maps(all) {
				if within(item) {
					out = append(out, item)
				}
			}
			return out
		}
		data = s.obj("asks", pick(demoAsksJSON), "strengths", pick(demoStrengthsJSON),
			"journey_errata", []any{}, "gates", []any{}, "no_obstacles_observed", false)
	}
	subtype := "unsupported"
	if data != nil {
		subtype = "success"
	}
	proc["model_ids"] = "simulated"
	proc["output_tokens"] = nil
	proc["result_subtype"] = subtype
	proc["is_error"] = data == nil
	if data == nil {
		return M{"proc": proc, "text": "", "structured": nil}, nil
	}
	return M{"proc": proc, "text": s.dumps(data, true), "structured": data}, nil
}

func stoppedJourney(events []M) string {
	s := newScope()
	lines := []string{"# Journey: convert three orders from JSON to CSV with orders-cli (stopped early)", "",
		"## Outcome I declared", "I was stopped before I finished, so I declared no outcome.", "",
		"## What I asked for first", "- Nothing yet; I stopped too early to say.", "",
		"## Chronological account"}
	for i, ev := range events {
		var what string
		if ev["kind"] == "text" {
			what = s.pyStr(ev["text"])
		} else {
			what = "I ran `" + s.pyStr(ev["input"]) + "`."
		}
		lines = append(lines, fmt.Sprintf("%d. %s [%s]", i+1, what, s.pyStr(ev["eid"])))
	}
	if len(events) == 0 {
		lines = append(lines, "1. I was stopped before my first step.")
	}
	lines = append(lines, "", "## What I still do not know", "Whether the conversion would have worked. The run was stopped "+
		"from the AJX Console before I finished, and this account was written from the recorded steps.")
	return strings.Join(lines, "\n") + "\n"
}

var consoleDemoOnce sync.Once

// RegisterConsoleDemo registers the native console-demo harness. It runs when
// a trial's plugins directory holds console_demo.py (so `ajx plugins` lists
// exactly what the Python CLI lists) and may be called directly.
func RegisterConsoleDemo() {
	consoleDemoOnce.Do(func() {
		plugin.RegisterHarness("console-demo", "builtin", newConsoleDemo)
	})
}
