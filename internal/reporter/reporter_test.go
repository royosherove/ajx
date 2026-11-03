package reporter_test

// Ports the reporter tests of skills/ajx/tests/test_ajx.py (errata,
// PortableReporterTest's cleanup/schema/re-extraction cases). The reporter
// harness is a fake registered here; no agent or model is called.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/render"
	"github.com/royosherove/ajx/internal/render/paritytest"
	"github.com/royosherove/ajx/internal/render/pyfmt"
	"github.com/royosherove/ajx/internal/reporter"
	"github.com/royosherove/ajx/internal/util"
)

type M = util.M

type fakeHarness struct {
	plugin.BaseHarness
}

var (
	fakeMu     sync.Mutex
	fakeReport func(ctx *plugin.Ctx, prompt string, schema M) (M, error)
	prepared   int
	cleaned    int
)

func (h *fakeHarness) Report(ctx *plugin.Ctx, prompt string, schema M) (M, error) {
	fakeMu.Lock()
	f := fakeReport
	fakeMu.Unlock()
	return f(ctx, prompt, schema)
}

type countingAuth struct{ *plugin.AuthBase }

func (a *countingAuth) Prepare(ctx *plugin.Ctx) error { prepared++; return nil }
func (a *countingAuth) Cleanup(ctx *plugin.Ctx) error { cleaned++; return nil }

func init() {
	plugin.RegisterHarness("fake-reporter", "test", func() plugin.Harness {
		return &fakeHarness{plugin.BaseHarness{CanReport: true}}
	})
	plugin.RegisterAuth("fake-counting", "test", func(conf M) plugin.Auth { return &countingAuth{plugin.NewAuthBase(conf)} })
}

func testSpec() M {
	return M{"reporter": M{"harness": "fake-reporter", "auth": "fake-counting", "model": nil, "effort": nil},
		"trial": M{"product": "Synthetic CLI", "narrate_timeout_seconds": int64(30)}}
}

func register() M {
	return M{"asks": []any{}, "journey_errata": []any{}, "strengths": []any{}, "gates": []any{}, "no_obstacles_observed": true}
}

func setReport(t *testing.T, f func(ctx *plugin.Ctx, prompt string, schema M) (M, error)) {
	fakeMu.Lock()
	fakeReport = f
	fakeMu.Unlock()
	t.Cleanup(func() { fakeMu.Lock(); fakeReport = nil; fakeMu.Unlock() })
}

func TestErrataBlockIsReplacedNotAppended(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "journey.md"), []byte("# Journey\n\nbody\n"), 0o644)
	if err := reporter.WriteErrata(dir, []M{{"journey_claim": "a", "evidence": "b", "event_refs": []any{"E-001"}}}); err != nil {
		t.Fatal(err)
	}
	if err := reporter.WriteErrata(dir, []M{{"journey_claim": "c", "evidence": "d", "event_refs": []any{}}}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "journey.md"))
	text := string(data)
	if strings.Count(text, "Corrections added") != 1 || !strings.Contains(text, "c -> evidence: d") ||
		strings.Contains(text, "a -> evidence: b") || !strings.HasPrefix(text, "# Journey\n\nbody") {
		t.Fatal(text)
	}
	if err := reporter.WriteErrata(dir, nil); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "journey.md"))
	if string(data) != "# Journey\n\nbody\n" {
		t.Fatalf("%q", data)
	}
}

func TestReporterCleansUpOnExceptionAndRejectsBadSchema(t *testing.T) {
	schema, err := reporter.LoadSchema("asks.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	prepared, cleaned = 0, 0
	setReport(t, func(*plugin.Ctx, string, M) (M, error) { return nil, errors.New("fake failure") })
	if _, err := reporter.ReporterCall(context.Background(), testSpec(), dir, "extract", "x", schema); err == nil {
		t.Fatal("no error")
	}
	if prepared != 1 || cleaned != 1 {
		t.Fatalf("prepare %d cleanup %d", prepared, cleaned)
	}
	bad := register()
	bad["gates"] = []any{M{"trigger": "fake", "resolved": "yes"}}
	setReport(t, func(*plugin.Ctx, string, M) (M, error) {
		return M{"proc": M{"exit_code": int64(0)}, "structured": bad, "text": pyfmt.Dumps(bad, pyfmt.DumpOpts{})}, nil
	})
	res, err := reporter.ReporterCall(context.Background(), testSpec(), dir, "extract", "x", schema)
	if err != nil {
		t.Fatal(err)
	}
	proc := util.MapOr(res["proc"])
	if proc["is_error"] != true || res["structured"] != nil || proc["result_subtype"] != "invalid structured response" ||
		proc["role"] != "reporter" || proc["harness"] != "fake-reporter" {
		t.Fatal(res)
	}
	if _, err := os.Stat(filepath.Join(dir, "extract.reporter", "config")); err != nil {
		t.Fatal("scratch config dir not created")
	}
}

func TestStructuredResponseFromFencedTextIsValidated(t *testing.T) {
	schema, _ := reporter.LoadSchema("asks.schema.json")
	good := pyfmt.Dumps(register(), pyfmt.DumpOpts{})
	var gotCtx *plugin.Ctx
	setReport(t, func(ctx *plugin.Ctx, prompt string, s M) (M, error) {
		gotCtx = ctx
		return M{"proc": M{"exit_code": int64(0)}, "structured": nil, "text": "  ```json\n" + good + "\n```  "}, nil
	})
	res, err := reporter.ReporterCall(context.Background(), testSpec(), t.TempDir(), "extract", "x", schema)
	if err != nil {
		t.Fatal(err)
	}
	if !util.Truthy(util.Get(res, "structured", "no_obstacles_observed")) || util.Get(res, "proc", "is_error") != false {
		t.Fatal(res)
	}
	if gotCtx.Timeout != 30 || gotCtx.Cell["config"] != "clean" || gotCtx.Cell["id"] != "reporter" || gotCtx.StageName != "extract" {
		t.Fatal(gotCtx)
	}
	setReport(t, func(*plugin.Ctx, string, M) (M, error) {
		return M{"proc": M{"exit_code": int64(3)}, "structured": register(), "text": ""}, nil
	})
	res, _ = reporter.ReporterCall(context.Background(), testSpec(), t.TempDir(), "extract", "x", schema)
	if util.Get(res, "proc", "is_error") != true || res["structured"] == nil {
		t.Fatal("a failed exit marks an error but keeps a valid register", res)
	}
}

func TestFailedReextractionInvalidatesPreviousRegister(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "journey.md"), []byte("# Synthetic journey"), 0o644)
	os.WriteFile(filepath.Join(dir, "digest.md"), []byte("Synthetic evidence"), 0o644)
	reg := register()
	reg["extraction_status"] = "ok"
	pyfmt.WriteJSON(filepath.Join(dir, "asks.json"), reg)
	saved := reporter.Call
	reporter.Call = func(context.Context, M, string, string, string, M) (M, error) {
		return nil, pyfmt.Errorf("RuntimeError", "fake failure")
	}
	defer func() { reporter.Call = saved }()
	if _, err := reporter.Extract(context.Background(), testSpec(), dir); err == nil {
		t.Fatal("no error")
	}
	status := util.S(pyfmt.ReadMap(filepath.Join(dir, "asks.json"))["extraction_status"])
	if status != "failed: reporter call raised RuntimeError" {
		t.Fatal(status)
	}
	if render.ReviewStatus(dir, M{"journey_provenance": "self"}) != "incomplete" {
		t.Fatal("review should be incomplete")
	}
}

func TestExtractAssignsIDsCostsAndErrata(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "journey.md"), []byte("# J\n\nbody\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "digest.md"), []byte("digest"), 0o644)
	tel := M{"events": []any{
		M{"eid": "E-001", "kind": "tool", "at": "2026-01-01T00:00:00Z", "end": "2026-01-01T00:00:02Z", "duration_s": 2.0, "is_error": true, "message_id": "m1"},
		M{"eid": "E-002", "kind": "text", "at": "2026-01-01T00:00:05Z", "message_id": "m2"}},
		"usage": []any{M{"id": "m1", "output_tokens": int64(10)}, M{"id": "m2", "output_tokens": int64(5)}}}
	pyfmt.WriteJSON(filepath.Join(dir, "telemetry.execute.json"), tel)
	ask := M{"title": "t", "requested_behavior": "r", "evidence_status": "observed_friction", "how_observed": "o",
		"consequence": "c", "priority_rationale": "p", "verification": "v", "event_refs": []any{"E-001", "E-002", "E-404"}, "labels": []any{}}
	var prompts []string
	saved := reporter.Call
	reporter.Call = func(_ context.Context, sp M, runDir, stage, prompt string, schema M) (M, error) {
		prompts = append(prompts, prompt)
		return M{"proc": M{"is_error": false, "model_ids": "rep-1", "output_tokens": int64(42)}, "text": "",
			"structured": M{"asks": []any{ask}, "journey_errata": []any{M{"journey_claim": "x", "evidence": "y", "event_refs": []any{"E-001"}}},
				"strengths": []any{}, "gates": []any{}, "no_obstacles_observed": false}}, nil
	}
	defer func() { reporter.Call = saved }()
	res, err := reporter.Extract(context.Background(), testSpec(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompts[0], "E-001, E-002") || !strings.Contains(prompts[0], "Synthetic CLI") {
		t.Fatal(prompts[0])
	}
	if p, _ := os.ReadFile(filepath.Join(dir, "extract.prompt.md")); string(p) != prompts[0] {
		t.Fatal("extract.prompt.md")
	}
	text, _ := os.ReadFile(filepath.Join(dir, "asks.json"))
	for _, want := range []string{`"id": "ASK-001"`, `"journey_anchors": []`, `"invalid_refs": [`, `"span_seconds": 5.0`,
		`"tool_seconds": 2.0`, `"output_tokens": 15`, `"extraction_status": "ok"`, `"model_ids": "rep-1"`} {
		if !strings.Contains(string(text), want) {
			t.Errorf("asks.json missing %s:\n%s", want, text)
		}
	}
	if util.Get(res, "asks", "extraction_status") != "ok" {
		t.Fatal(res)
	}
	journey, _ := os.ReadFile(filepath.Join(dir, "journey.md"))
	if !strings.Contains(string(journey), "- x -> evidence: y (E-001)") {
		t.Fatal(string(journey))
	}
	// An invalid register becomes a failed stub naming the reason.
	reporter.Call = func(context.Context, M, string, string, string, M) (M, error) {
		return M{"proc": M{"is_error": true, "timed_out": true}, "text": "", "structured": nil}, nil
	}
	res, err = reporter.Extract(context.Background(), testSpec(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := util.Get(res, "asks", "extraction_status"); got != "failed: reporter returned no valid register (timeout)" {
		t.Fatal(got)
	}
}

func TestSchemaValid(t *testing.T) {
	schema, _ := reporter.LoadSchema("asks.schema.json")
	if !reporter.SchemaValid(register(), schema) {
		t.Fatal("valid register rejected")
	}
	for _, bad := range []any{nil, "x", []any{}, M{"asks": []any{}}} {
		if reporter.SchemaValid(bad, schema) {
			t.Errorf("%v accepted", bad)
		}
	}
	s := M{"type": "object", "properties": M{"n": M{"type": "integer"}, "f": M{"type": "number"},
		"e": M{"enum": []any{"a", int64(1)}}, "p": M{"type": "string", "pattern": "^E-[0-9]{3,}$", "maxLength": int64(6)},
		"l": M{"type": "array", "minItems": int64(1), "items": M{"type": "boolean"}}, "o": M{"anyOf": []any{M{"type": "null"}, M{"type": "string"}}}},
		"required": []any{"n"}, "additionalProperties": false}
	ok := []M{{"n": int64(1)}, {"n": int64(1), "f": 2.5, "e": 1.0, "p": "E-001\n", "l": []any{true}, "o": nil}, {"n": int64(2), "e": true}}
	bad := []M{{}, {"n": 1.0}, {"n": true}, {"n": int64(1), "x": 1}, {"n": int64(1), "e": "b"}, {"n": int64(1), "p": "E-01"},
		{"n": int64(1), "p": "E-00001"}, {"n": int64(1), "l": []any{}}, {"n": int64(1), "f": false}, {"n": int64(1), "o": int64(3)}}
	for _, v := range ok {
		if !reporter.SchemaValid(v, s) {
			t.Errorf("%v rejected", v)
		}
	}
	for _, v := range bad {
		if reporter.SchemaValid(v, s) {
			t.Errorf("%v accepted", v)
		}
	}
}

// TestPromptParityWithPython compares the prompt text (including the schema
// JSON in the reporter call) with the Python reporter.
func TestPromptParityWithPython(t *testing.T) {
	paritytest.Python(t)
	dir := t.TempDir()
	lib := filepath.Join(paritytest.Root(), "skills", "ajx", "lib")
	out := paritytest.Run(t, dir, "-c", `import json, sys
from pathlib import Path
from unittest import mock
sys.path.insert(0, sys.argv[1])
from ajx import plugins, reporter
spec = {"trial": {"product": "Synthetic {{digest}} CLI"}, "reporter": {"harness": "claude-code", "auth": "inherit"}}
seen = {}
def fake(self, ctx, prompt, schema=None):
    seen["prompt"] = prompt
    return {"proc": {"exit_code": 0}, "text": "", "structured": None}
schema = json.loads((reporter.SCHEMAS / "clusters.schema.json").read_text(encoding="utf-8"))
with mock.patch.object(plugins.get("harness", "claude-code"), "report", fake):
    reporter._reporter_call(spec, Path(sys.argv[2]), "synthesize", "Body é", schema)
print(json.dumps({
  "narrate": reporter.narrate_prompt(spec, {}, "c-r1", "DIGEST {{product}}"),
  "extract": reporter._prompt("extract.md", journey="J", digest="D", product="P", valid_ids="E-001"),
  "call": seen["prompt"]}))`, lib, dir)
	want := pyfmt.ReadMap(writeTemp(t, out))
	sp := M{"trial": M{"product": "Synthetic {{digest}} CLI"}, "reporter": M{"harness": "fake-reporter", "auth": "fake-counting"}}
	narrate, err := reporter.NarratePrompt(sp, M{}, "c-r1", "DIGEST {{product}}")
	if err != nil || narrate != want["narrate"] {
		t.Errorf("narrate prompt differs: %v\n%s", err, paritytest.FirstDiff(util.S(want["narrate"]), narrate))
	}
	extract, _ := reporter.Prompt("extract.md", "journey", "J", "digest", "D", "product", "P", "valid_ids", "E-001")
	if extract != want["extract"] {
		t.Errorf("extract prompt differs:\n%s", paritytest.FirstDiff(util.S(want["extract"]), extract))
	}
	var seen string
	setReport(t, func(_ *plugin.Ctx, prompt string, _ M) (M, error) {
		seen = prompt
		return M{"proc": M{"exit_code": int64(0)}, "text": "", "structured": nil}, nil
	})
	schema, _ := reporter.LoadSchema("clusters.schema.json")
	if _, err := reporter.ReporterCall(context.Background(), sp, dir, "synthesize", "Body é", schema); err != nil {
		t.Fatal(err)
	}
	if seen != want["call"] {
		t.Errorf("reporter call prompt differs:\n%s", paritytest.FirstDiff(util.S(want["call"]), seen))
	}
}

func writeTemp(t *testing.T, text string) string {
	p := filepath.Join(t.TempDir(), "out.json")
	os.WriteFile(p, []byte(text), 0o644)
	return p
}

func TestReconstructAndInterruptedExtract(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "digest.md"), []byte("digest text"), 0o644)
	saved := reporter.Call
	defer func() { reporter.Call = saved }()
	reporter.Call = func(_ context.Context, _ M, _, stage, prompt string, schema M) (M, error) {
		if stage != "reconstruct" || schema != nil || !strings.Contains(prompt, "digest text") {
			t.Fatalf("%s %v", stage, schema)
		}
		return M{"proc": M{"is_error": false}, "text": "# Journey"}, nil
	}
	res, err := reporter.Reconstruct(context.Background(), testSpec(), dir)
	if err != nil || res["text"] != "# Journey" {
		t.Fatal(res, err)
	}
	reporter.Call = func(context.Context, M, string, string, string, M) (M, error) {
		return M{"proc": M{"is_error": true, "result_subtype": "max_turns"}, "text": ""}, nil
	}
	if _, err := reporter.Reconstruct(context.Background(), testSpec(), dir); err == nil ||
		err.Error() != "reporter reconstruction failed (max_turns)" || pyfmt.TypeName(err) != "RuntimeError" {
		t.Fatal(err)
	}
	// An interruption is not a failed extraction: the register stays "in progress".
	os.WriteFile(filepath.Join(dir, "journey.md"), []byte("# J"), 0o644)
	reporter.Call = func(context.Context, M, string, string, string, M) (M, error) { return nil, util.ErrInterrupted }
	if _, err := reporter.Extract(context.Background(), testSpec(), dir); !errors.Is(err, util.ErrInterrupted) {
		t.Fatal(err)
	}
	if s := pyfmt.ReadMap(filepath.Join(dir, "asks.json"))["extraction_status"]; s != "in progress" {
		t.Fatal(s)
	}
}
