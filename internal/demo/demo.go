// Package demo ports lib/ajx/demo.py: fictional report fixtures generated
// offline through the production renderer (`ajx example`).
package demo

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	ajx "github.com/royosherove/ajx"
	"github.com/royosherove/ajx/internal/evidence"
	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/render"
	"github.com/royosherove/ajx/internal/render/pyfmt"
	"github.com/royosherove/ajx/internal/reporter"
	"github.com/royosherove/ajx/internal/util"
)

type M = util.M

// Task is the fictional task prompt.
const Task = "Export the 100 supplied fictional records as a CSV file and verify the row count."

var start = time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

func at(seconds int) string {
	return start.Add(time.Duration(seconds)*time.Second).Format("2006-01-02T15:04:05") + "Z"
}

func strList(items ...string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

func ask(title, request, observation, consequence string, refs, labels []string, priority, verification string, workaround any) M {
	return M{"title": title, "requested_behavior": request, "evidence_status": "observed_friction",
		"how_observed": observation, "consequence": consequence, "event_refs": strList(refs...),
		"journey_anchors": []any{}, "labels": strList(labels...), "priority_rationale": priority,
		"verification": verification, "workaround": workaround, "modeled_saving": nil}
}

type record struct {
	start  int
	end    int // -1: an agent note, not a tool call
	action string
	failed bool
	output string
}

func runVariant(out, variant string, sp M) error {
	baseline, gated := variant == "baseline", variant == "headless"
	harness := "codex"
	if gated {
		harness = "kiro-cli"
	}
	version := "1.3.0"
	if baseline {
		version = "1.2.0"
	}
	rid := fmt.Sprintf("%s-%s-r1", variant, harness)
	rd := filepath.Join(out, "runs", rid)
	if err := os.MkdirAll(rd, 0o777); err != nil {
		return err
	}
	// Every command and response is invented here, never executed.
	var records []record
	var steps []string
	var asks []M
	var strengths, gates []any
	var seconds, passed int
	switch {
	case baseline:
		records = []record{
			{0, 2, "demo-export --help", false, "Commands: export, dump. Both write records to a file."},
			{8, -1, "I could use export or dump. The help does not explain which produces CSV.", false, ""},
			{16, 18, "demo-export export --format csv --out report.csv", true, "Unknown option: --out"},
			{29, 31, "demo-export export --help", false, "Use --output FILE. The quick start still shows --out."},
			{38, 85, "demo-export export --format csv --output report.csv", false, "Export started.\nComplete: 100 records."},
			{57, -1, "The export has been quiet. I cannot tell whether it is still working.", false, ""},
			{90, -1, "I trust the command help over the older quick start after the flag was rejected.", false, ""},
			{96, 98, "demo-export inspect report.csv", false, "Format: CSV. Data rows: 100. Validation: passed."},
			{105, -1, "I exported the records and verified that the CSV contains 100 data rows.", false, ""},
		}
		steps = []string{
			"1. 🔀 Fork. I read the help [E-001] and found two export commands without a selection rule [E-002].",
			"2. 🚧 Roadblock. I copied the quick-start flag. It failed [E-003]. The command help supplied a working alternative [E-004].",
			"3. ⏳ Wait. I started the corrected export [E-005]. While it ran, I recorded that there was no progress signal [E-006].",
			"4. 🧭 Trust Decision. I switched from the quick start to command help after the mismatch [E-007].",
			"5. ✅ Delight. The inspect command gave a machine-readable row count and a validation result [E-008].",
			"6. I declared the export complete after the row-count check [E-009].",
		}
		asks = []M{
			ask("Make the quick-start export command executable",
				"Use the supported --output flag in the quick start, with a copyable CSV example.",
				"The documented --out flag failed. Command help showed --output, which worked.",
				"The first attempt failed and required a help lookup before retrying.", []string{"E-003", "E-004", "E-005"},
				[]string{"Roadblock", "Misdirection"}, "Fix the broken first attempt before reducing optional friction.",
				"Run the documented command in a fresh workspace; it must produce a 100-row CSV.",
				"Replace --out with --output."),
			ask("Expose progress while a CSV export is running",
				"Emit bounded progress on stderr and a final structured completion result.",
				"Only start and completion messages appeared during the 47-second export.",
				"The agent could not distinguish productive work from a stalled command.", []string{"E-005", "E-006"},
				[]string{"Wait", "Blind Spot"}, "Make long-running work observable without extra polling.",
				"Run a delayed export; progress must remain separate from CSV stdout.", nil),
			ask("Explain when to choose export instead of dump",
				"State the output formats and intended use of both commands in top-level help.",
				"Both commands were described as writing records; the required CSV path was unclear.",
				"The agent had to choose without a selection signal.", []string{"E-001", "E-002"},
				[]string{"Fork"}, "Remove the ambiguous choice at the start of the task.",
				"A user reading only top-level help can identify the CSV command.", nil),
		}
		strengths = []any{M{"title": "A verifiable row count", "how_observed": "Inspect confirmed CSV format and 100 data rows.",
			"event_refs": strList("E-008")}}
		gates, seconds, passed = []any{}, 108, 2
	case gated:
		records = []record{
			{0, 2, "demo-export export --help", false, "Use --output FILE. Exports require workspace approval."},
			{8, 11, "demo-export export --format csv --output report.csv", true, "Open the approval page to continue."},
			{17, -1, "I cannot open an approval session or ask a human in this headless task.", false, ""},
			{24, -1, "The export is blocked by required approval. I did not create the CSV.", false, ""},
		}
		steps = []string{
			"1. I read the documented command [E-001].",
			"2. 🚪 Gate. The export required interactive approval [E-002]. I had no human available [E-003].",
			"3. 🧱 Wall. I stopped without creating the CSV because no authorized unattended path was available [E-004].",
		}
		asks = []M{ask("Check headless authorization before starting an export",
			"Provide a non-interactive authorization preflight and document the required human approval.",
			"The command requested browser approval after the agent had started the task.",
			"The unattended task stopped without an output file.", []string{"E-002", "E-003", "E-004"},
			[]string{"Gate", "Wall"}, "Expose the prerequisite early so teams can prepare an authorized run.",
			"In headless mode, preflight reports the missing approval before an export begins.", nil)}
		strengths = []any{}
		gates = []any{M{"trigger": "Workspace approval requires a human", "agent_action": "Stopped and reported the prerequisite.",
			"resolved": false, "event_refs": strList("E-002", "E-003")}}
		seconds, passed = 25, 0
	default:
		records = []record{
			{0, 2, "demo-export --help", false, "Use export for CSV; dump writes a JSON snapshot."},
			{6, 26, "demo-export export --format csv --output report.csv", false, "Export started.\nComplete: 100 records."},
			{15, -1, "The export is running, but I have no intermediate progress signal.", false, ""},
			{29, 31, "demo-export inspect report.csv", false, "Format: CSV. Data rows: 100. Validation: passed."},
			{33, -1, "I created the CSV and verified all 100 data rows.", false, ""},
		}
		steps = []string{
			"1. ✅ Delight. Help explicitly directed me to the CSV export command [E-001].",
			"2. ⏳ Wait. The documented command ran [E-002], but I still had no intermediate progress signal [E-003].",
			"3. ✅ Delight. Inspect confirmed the format and row count [E-004].",
			"4. I declared the requested export complete [E-005].",
		}
		asks = []M{ask("Expose progress while a CSV export is running",
			"Emit bounded progress on stderr and a final structured completion result.",
			"Only start and completion messages appeared during the 20-second export.",
			"The agent could not observe intermediate progress.", []string{"E-002", "E-003"},
			[]string{"Wait"}, "The remaining observed friction is visibility during execution.",
			"Run a delayed export; progress must remain separate from CSV stdout.", nil)}
		strengths = []any{
			M{"title": "The help identifies the CSV path", "how_observed": "Top-level help explains export versus dump.", "event_refs": strList("E-001")},
			M{"title": "A verifiable row count", "how_observed": "Inspect confirms the requested output.", "event_refs": strList("E-004")}}
		gates, seconds, passed = []any{}, 34, 2
	}
	usageStatus := "partial"
	if gated {
		usageStatus = "unavailable"
	}
	tel := plugin.EmptyTelemetry(M{"model_ids": strList("example-model"), "usage_status": usageStatus,
		"tool_status": "partial", "timestamp_source": "synthetic timestamps",
		"limitations": strList("Fictional fixture. No product or model was run.")})
	var events, usage []any
	for i, r := range records {
		mid := fmt.Sprintf("example-message-%d", i)
		var event M
		if r.end < 0 {
			event = plugin.TextEvent(r.action, at(r.start), mid)
		} else {
			event = plugin.ToolEvent(fmt.Sprintf("example-tool-%d", i), "shell", M{"command": r.action}, at(r.start), mid)
			plugin.FinishTool(event, at(r.end), r.failed, r.output)
		}
		events = append(events, event)
		if !gated {
			usage = append(usage, M{"id": mid, "at": at(r.start), "output_tokens": int64(130 + i*47)})
		}
	}
	tel["events"] = events
	if usage != nil {
		tel["usage"] = usage
	}
	tel["final_text"] = records[len(records)-1].action
	tel = evidence.AssignIDs(tel)
	for i, a := range asks {
		a["id"] = fmt.Sprintf("ASK-%03d", i+1)
	}
	asks = evidence.AskCosts(asks, tel)
	reporter.NormalizeMeasured(asks)
	askList := make([]any, len(asks))
	for i, a := range asks {
		askList[i] = a
	}
	data := M{"asks": askList, "strengths": strengths, "gates": gates, "journey_errata": []any{},
		"extraction_status": "ok", "no_obstacles_observed": false, "extractor": M{"model_ids": "fictional reporter"}}
	stages := M{}
	for _, s := range []string{"execute", "verify", "narrate", "extract", "measure", "render"} {
		stages[s] = M{"status": "done"}
	}
	state := M{"execute": M{"started_at": at(0), "stopped_at": at(seconds), "exit_code": int64(0),
		"stop_reason": "completed", "timed_out": false}, "stages": stages}
	summary := evidence.TaskSummary(state, tel)
	outcome := "failed"
	if passed > 0 {
		outcome = "succeeded"
	}
	var checks []any
	for _, name := range []string{"CSV exists", "CSV contains 100 data rows"} {
		checks = append(checks, M{"name": name, "passed": passed > 0, "type": "synthetic"})
	}
	verify := M{"passed": int64(passed), "total": int64(2), "outcome": outcome, "checks_with_auth_errors": []any{},
		"checks": checks}
	run := M{"schema_version": int64(1), "ajx_version": ajx.Version, "example": true, "run_id": rid,
		"product": M{"name": util.Get(sp, "trial", "product"), "version": version},
		"agent": M{"harness_name": harness, "harness_version": "synthetic fixture", "model_id": "example-model",
			"model_requested": "example-model", "settings": M{"config": "clean"}},
		"auth": M{"type": "synthetic; no credentials"}, "runner": M{"type": "offline example"},
		"task_prompt":               M{"path": "task-prompt.md", "sha256": util.Get(sp, "task", "prompt_sha256")},
		"evaluation_wrapper_sha256": util.Sha256Bytes([]byte("AJX fictional example")), "ajx_skill_revision": "example",
		"task_outcome_verified": outcome, "task_outcome_declared": tel["final_text"],
		"verification_scope": fmt.Sprintf("%d/2 fictional checks passed", passed), "task_stop_reason": "completed",
		"task_started_at": at(0), "task_stopped_at": at(seconds),
		"journey_provenance": "FICTIONAL FIRST-PERSON ACCOUNT: hand-authored demonstration, not a real agent recollection.",
		"review_status":      "ready", "teardown": M{"status": "none defined"},
		"evidence": M{"usage_coverage": tel["usage_status"], "tool_call_coverage": "partial",
			"timestamp_coverage": "synthetic timestamps"},
		"limitations": strList("All content and metrics are invented for this example.",
			"A change across these fictional runs demonstrates the report, not a measured improvement.",
			"Token subtotals are partial; the headless example intentionally has no usage telemetry.")}
	journey := "# Journey: export 100 records to CSV\n\n" +
		"> Fictional first-person account. No agent performed this task.\n\n" +
		"## Outcome I declared\n\n" + util.S(tel["final_text"]) + "\n\n## Chronological account\n\n" +
		strings.Join(steps, "\n\n") + "\n\n## What I still do not know\n\n" +
		"These fictional observations do not establish behavior for any real product or agent.\n"
	for _, f := range []struct {
		name  string
		value any
	}{{"run.json", run}, {"state.json", state}, {"asks.json", data}, {"verify.json", verify},
		{"telemetry.execute.json", tel}, {"measurements.json", M{"schema_version": int64(1), "task_summary": summary}}} {
		if err := pyfmt.WriteJSON(filepath.Join(rd, f.name), f.value); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(rd, "journey.md"), []byte(journey), 0o644); err != nil {
		return err
	}
	var lines strings.Builder
	for _, e := range util.List(tel["events"]) {
		lines.WriteString(pyfmt.Dumps(e, pyfmt.DumpOpts{}) + "\n")
	}
	if err := os.WriteFile(filepath.Join(rd, "events.jsonl"), []byte(lines.String()), 0o644); err != nil {
		return err
	}
	digest := evidence.BuildDigest(sp, M{"harness": harness, "model": "example-model", "config": "clean"},
		state, tel, verify, M{}, evidence.DigestOpts{})
	if err := os.WriteFile(filepath.Join(rd, "digest.md"),
		[]byte("> Fictional example; every event below is synthetic.\n\n"+digest), 0o644); err != nil {
		return err
	}
	return render.RunReport(rd)
}

// Generate creates or refreshes only an AJX-owned example directory; it
// never calls an agent. It returns the example directory.
func Generate(output string) (string, error) {
	out := pyfmt.CleanPath(output)
	marker := filepath.Join(out, "example.json")
	if st, err := os.Stat(out); err == nil {
		if !st.IsDir() {
			return "", pyfmt.Errorf("NotADirectoryError", "[Errno 20] Not a directory: %s", pyfmt.StrRepr(out))
		}
		entries, err := os.ReadDir(out)
		if err != nil {
			return "", err
		}
		if len(entries) > 0 {
			v, err := pyfmt.ReadJSON(marker, M{})
			if err != nil {
				return "", pyfmt.Errorf("JSONDecodeError", "%v", err)
			}
			if !pyfmt.Equal(util.MapOr(v)["generator"], "ajx-fictional-example") {
				return "", pyfmt.Errorf("ValueError", "Choose an empty directory; the example generator will not overwrite existing work.")
			}
		}
	}
	if err := os.MkdirAll(out, 0o777); err != nil {
		return "", err
	}
	if err := pyfmt.WriteJSON(marker, M{"generator": "ajx-fictional-example", "synthetic": true}); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(out, "task-prompt.md"), []byte(Task+"\n"), 0o644); err != nil {
		return "", err
	}
	sp := M{"trial": M{"id": "csv-export-example", "product": "Demo Export CLI", "example": true,
		"output_dir": out, "repetitions": int64(1), "parallel": int64(1), "order_seed": int64(0)},
		"task": M{"prompt_sha256": util.Sha256Bytes([]byte(Task + "\n"))}}
	if err := pyfmt.WriteJSON(filepath.Join(out, "matrix-plan.json"),
		M{"wall_clock_comparable": true, "note": "Synthetic, sequential examples."}); err != nil {
		return "", err
	}
	for _, variant := range []string{"baseline", "revised", "headless"} {
		if err := runVariant(out, variant, sp); err != nil {
			return "", err
		}
	}
	if err := render.MatrixReport(sp, render.MatrixOpts{Synthesize: false}); err != nil {
		return "", err
	}
	readme := "# Fictional AJX example\n\nOpen `index.html` in a browser for the comparison. " +
		"Each run includes `report.html` and `pretty-journey.html`.\n\n" +
		"Every event, command, version, model name, and cost is invented. No agent or product " +
		"was executed. These pages demonstrate the renderer, not benchmark results.\n"
	if err := os.WriteFile(filepath.Join(out, "README.md"), []byte(readme), 0o644); err != nil {
		return "", err
	}
	return out, nil
}
