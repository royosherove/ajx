package render

// Ports the rendering tests of skills/ajx/tests/test_ajx.py (RenderTest,
// token variance) and skills/ajx/tests/test_profile_reports.py. Synthetic
// records only; no worker, provider, or network calls.

import (
	"context"
	"encoding/json"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/render/pyfmt"
	"github.com/royosherove/ajx/internal/util"
)

func TestAsksTableEscapesPipesAndReportsExtractionFailure(t *testing.T) {
	run := M{"product": M{"name": "p", "version": "1"}, "run_id": "r1",
		"agent":                 M{"harness_name": "h", "harness_version": "1", "model_id": "m", "model_requested": "m"},
		"task_outcome_verified": "failed", "verification_scope": "0/1", "journey_provenance": "self",
		"evidence": M{"usage_coverage": "x", "tool_call_coverage": "y", "timestamp_coverage": "z"}, "limitations": []any{}}
	asks := M{"asks": []any{M{"id": "ASK-001", "title": "a | b", "evidence_status": "verified_defect", "labels": []any{},
		"event_refs": []any{"E-001"}, "journey_anchors": []any{}, "priority_rationale": "x|y", "how_observed": "o",
		"requested_behavior": "r", "consequence": "c", "verification": "v",
		"measured": M{"tool_calls": int64(1), "failed_tool_calls": int64(0), "span_seconds": 1.0, "output_tokens": nil,
			"tool_seconds": 1.0, "shared_events": []any{}, "basis": "b"}}},
		"extraction_status": "ok", "strengths": []any{}, "gates": []any{}, "journey_errata": []any{}, "extractor": M{}}
	md, err := AsksMarkdown(run, asks, M{"events": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, `a \| b`) || !strings.Contains(md, `x\|y`) || !strings.Contains(md, "| 1.0 s |") {
		t.Fatal(md)
	}
	failed := util.Copy(asks)
	failed["asks"], failed["extraction_status"] = []any{}, "failed: timeout"
	md2, err := AsksMarkdown(run, failed, M{"events": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md2, "Extraction did not complete") || strings.Contains(md2, "No obstacles observed") {
		t.Fatal(md2)
	}
	delete(run, "product")
	if _, err := AsksMarkdown(run, asks, M{}); err == nil || pyfmt.TypeName(err) != "KeyError" || err.Error() != "'product'" {
		t.Fatalf("missing product: %v", err)
	}
}

func TestTokenVarianceExcludesPartialRuns(t *testing.T) {
	rows := []M{{"output_tokens": int64(100), "usage_status": "complete"}, {"output_tokens": int64(5), "usage_status": "partial"}}
	if v := util.MapOr(variance(rows, "output_tokens")); util.Int(v["n"], 0) != 1 {
		t.Fatal(v)
	}
	if variance(rows, "tool_calls") != nil {
		t.Fatal("tool_calls variance")
	}
	even := variance([]M{{"x": int64(2)}, {"x": int64(4)}}, "x").(M)
	if even["median"] != 3.0 || even["min"] != int64(2) {
		t.Fatal(even) // statistics.median of an even count is a float
	}
}

func TestReviewStatusAndTeardownRecord(t *testing.T) {
	dir := t.TempDir()
	if ReviewStatus(dir, M{"journey_provenance": "self"}) != "incomplete" {
		t.Fatal("no journey")
	}
	os.WriteFile(filepath.Join(dir, "journey.md"), []byte("# j"), 0o644)
	pyfmt.WriteJSON(filepath.Join(dir, "asks.json"), M{"extraction_status": "ok"})
	if ReviewStatus(dir, M{"journey_provenance": "self"}) != "ready" || ReviewStatus(dir, M{"journey_provenance": "unavailable"}) != "incomplete" {
		t.Fatal("review status")
	}
	if got := TeardownRecord(M{}, M{"teardown": []any{}}); got["status"] != "none defined" {
		t.Fatal(got)
	}
	ran := M{"stages": M{"teardown": M{"status": "done"}}}
	if got := TeardownRecord(ran, M{"teardown": []any{"x"}}); got["status"] != "unchecked" {
		t.Fatal(got)
	}
	got := TeardownRecord(M{"teardown": M{"status": "failed", "commands": int64(2), "failed": int64(1), "auth_errors": []any{"a"}}}, M{"teardown": []any{"x"}})
	if got["status"] != "failed" || got["auth_error_lines"] != int64(1) {
		t.Fatal(got)
	}
	lim := CredentialLimitations(M{"task_identity": M{"ok": false, "cmd": "id"}}, M{"checks_with_auth_errors": []any{"c1"}}, M{"status": "skipped"})
	if len(lim) != 3 || !strings.HasPrefix(lim[0], "cleanup: teardown did not run") || !strings.Contains(lim[2], "task identity check (id) failed") {
		t.Fatal(lim)
	}
}

// ---------------------------------------------------------------- profile reports

var (
	tagRe    = regexp.MustCompile(`<([a-zA-Z][a-zA-Z0-9-]*)((?:[^>"']|"[^"]*"|'[^']*')*)>`)
	attrRe   = regexp.MustCompile(`([^\s=/>]+)(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+))?`)
	rawText  = regexp.MustCompile(`(?s)<(script|style)\b[^>]*>.*?</(script|style)>`)
	rowRe    = regexp.MustCompile(`(?s)<tr\b[^>]*>(.*?)</tr>`)
	cellRe   = regexp.MustCompile(`(?s)<t[dh]\b[^>]*>(.*?)</t[dh]>`)
	anyTagRe = regexp.MustCompile(`<[^>]*>`)
)

type tag struct {
	name  string
	attrs []string
}

// htmlRecords is a small stand-in for the Python test's HTMLParser: the tags
// (with attribute names) and table rows (cell text) of a page.
func htmlRecords(text string) ([]tag, [][]string) {
	var tags []tag
	stripped := rawText.ReplaceAllStringFunc(text, func(m string) string {
		end := strings.Index(m, ">") + 1
		return m[:end] + "</" + strings.Trim(m[1:4], " ") + ">"
	})
	for _, m := range tagRe.FindAllStringSubmatch(stripped, -1) {
		tg := tag{name: strings.ToLower(m[1])}
		for _, a := range attrRe.FindAllStringSubmatch(m[2], -1) {
			tg.attrs = append(tg.attrs, strings.ToLower(a[1]))
		}
		tags = append(tags, tg)
	}
	var rows [][]string
	for _, r := range rowRe.FindAllStringSubmatch(stripped, -1) {
		var row []string
		for _, c := range cellRe.FindAllStringSubmatch(r[1], -1) {
			row = append(row, html.UnescapeString(anyTagRe.ReplaceAllString(c[1], "")))
		}
		rows = append(rows, row)
	}
	return tags, rows
}

func environment(profile, backend string) M {
	return M{
		"profile_id": profile, "backend": backend,
		"inventory": M{"os": "synthetic OS", "tools": []any{M{"name": "sample-tool", "version": "1.0"}}},
		"capabilities": M{
			"filesystem": M{"declared": "workspace only", "enforcement": "unsupported", "observed": nil},
			"network":    M{"state": "observed-only", "observed": "synthetic probe", "evidence": []any{"probe-1"}},
		},
		"limitations": []any{"Host permissions apply; directory isolation is not enforced."},
	}
}

func configuration(profile, mode, digest string) M {
	entries := []any{}
	if mode == "selected" {
		entries = []any{M{
			"name": "product-guide", "content_hash": digest, "discovery_scope": "attempt",
			"states": M{"installed": true, "discoverable": true, "enabled": true,
				"loaded_into_context": nil, "invoked": "unknown"},
			"contributions": M{"tools": []any{"sample-inspect"}, "hashes": M{"instructions": digest}},
		}}
	}
	return M{
		"profile_id": profile,
		"skills":     M{"mode": mode, "entries": entries},
		"plugins":    M{"mode": "none"}, "hooks": M{"mode": "none"},
		"capabilities": M{"skill_discovery": M{"enforcement": "enforced", "observed": nil}},
		"limitations":  []any{"The adapter does not report skill invocation events."},
	}
}

type profiles struct {
	t   *testing.T
	out string
	sp  M
}

func newProfiles(t *testing.T) *profiles {
	out := t.TempDir()
	saved := synthesize
	synthesize = func(context.Context, M, string, []M, func(string)) ([]M, error) {
		t.Fatal("No provider calls")
		return nil, nil
	}
	t.Cleanup(func() { synthesize = saved })
	return &profiles{t: t, out: out, sp: M{
		"sha256": strings.Repeat("f", 64), "teardown": []any{}, "reporter": M{},
		"trial": M{"id": "profiles", "product": "Synthetic CLI", "timeout_seconds": int64(60),
			"output_dir": out, "repetitions": int64(2), "parallel": int64(1), "order_seed": int64(0)},
		"task": M{"prompt_path": "task.md", "prompt_sha256": strings.Repeat("b", 64), "human": "unavailable"},
	}}
}

type runOpts struct {
	actual, manifest, plan, cleanup, references, stateExtra M
	cleanupSet, noArtifacts                                 bool
}

func deepCopy(m M) M { return util.DeepCopy(m).(M) }

func (p *profiles) makeRun(cellID string, rep int, o runOpts) (string, M, M) {
	t := p.t
	t.Helper()
	rd := filepath.Join(p.out, "runs", cellID+"-r"+itoa(rep))
	if err := os.MkdirAll(rd, 0o755); err != nil {
		t.Fatal(err)
	}
	cellM := M{"id": cellID, "config": "clean"}
	for k, v := range o.references {
		cellM[k] = v
	}
	state := M{
		"journey_provenance": "self-narrated synthetic account",
		"stages":             M{"execute": M{"status": "done"}, "render": M{"status": "done"}},
		"execute": M{"started_at": "2026-01-01T00:00:00Z", "stopped_at": "2026-01-01T00:00:02Z",
			"stop_reason": "completed"},
	}
	for k, v := range o.stateExtra {
		state[k] = v
	}
	snapshot := M{"harness": M{"version_before": "synthetic-1"}, "product_version_before": "1.0"}
	if o.plan != nil {
		state["environment"] = o.plan
	}
	if o.actual != nil {
		state["environment_report"] = o.actual
		snapshot["execution_environment"] = o.actual
	}
	if o.manifest != nil {
		state["agent_configuration"] = o.manifest
	}
	if o.cleanup != nil {
		state["environment_cleanup"] = o.cleanup
	}
	write := func(name string, v any) {
		if err := pyfmt.WriteJSON(filepath.Join(rd, name), v); err != nil {
			t.Fatal(err)
		}
	}
	if !o.noArtifacts {
		write("environment.json", snapshot)
		if o.manifest != nil {
			write("agent-configuration.json", o.manifest)
		}
	}
	write("state.json", state)
	write("verify.json", M{"outcome": "succeeded", "passed": int64(1), "total": int64(1), "checks": []any{M{"name": "synthetic output"}}})
	write("telemetry.execute.json", M{"events": []any{}, "usage_status": "unavailable"})
	write("measurements.json", M{"task_summary": M{"elapsed_seconds": int64(2), "output_tokens": nil, "usage_status": "unavailable"}})
	write("asks.json", M{"extraction_status": "ok", "asks": []any{M{
		"id": "ASK-001", "title": "Explain the synthetic option",
		"requested_behavior": "Describe the argument.", "evidence_status": "observed_friction",
		"how_observed": "Synthetic evidence.", "consequence": "Extra lookup.",
		"priority_rationale": "Make the option discoverable.", "verification": "Read help.",
		"measured": M{"tool_calls": nil, "failed_tool_calls": nil, "span_seconds": nil,
			"tool_seconds": nil, "output_tokens": nil, "basis": "unavailable"},
	}}})
	os.WriteFile(filepath.Join(rd, "journey.md"), []byte("# Synthetic journey\n\nNo live task was run.\n"), 0o644)
	run, err := WriteRunJSON(p.sp, cellM, rep, rd, state, fakeHarness("synthetic", true), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := RunReport(rd); err != nil {
		t.Fatal(err)
	}
	return rd, run, state
}

func itoa(n int) string { return pyfmt.Str(int64(n)) }

func (p *profiles) matrix() M {
	p.t.Helper()
	if _, err := MatrixData(p.sp, MatrixOpts{Synthesize: false, Log: func(string) {}}); err != nil {
		p.t.Fatal(err)
	}
	// Re-read to see exactly what was written.
	return pyfmt.ReadMap(filepath.Join(p.out, "matrix.json"))
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func expectIn(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("missing %q", w)
		}
	}
}

func before(t *testing.T, text, a, b string) {
	t.Helper()
	i, j := strings.Index(text, a), strings.Index(text, b)
	if i < 0 || j < 0 || i >= j {
		t.Errorf("%q should come before %q (%d, %d)", a, b, i, j)
	}
}

func jsonEqual(a, b any) bool {
	return pyfmt.Dumps(a, pyfmt.DumpOpts{}) == pyfmt.Dumps(b, pyfmt.DumpOpts{})
}

func TestActualReportAndDeclaredPlanRemainDistinct(t *testing.T) {
	p := newProfiles(t)
	actual, manifest := environment("resolved-local", "local"), configuration("product-guide", "selected", strings.Repeat("a", 64))
	plan := M{"profile_id": "requested-local", "backend": "local", "ownership": "borrowed",
		"capabilities": M{"filesystem": "enforced"}}
	rd, run, _ := p.makeRun("sample", 1, runOpts{actual: actual, manifest: manifest, plan: plan,
		references: M{"environment": "requested-local", "agent_configuration": "product-guide"}})
	env := util.MapOr(run["execution_environment"])
	if env["profile_id"] != "resolved-local" || env["declared_profile_id"] != "requested-local" ||
		util.Get(env, "plan", "ownership") != "borrowed" || !jsonEqual(env["capabilities"], actual["capabilities"]) ||
		!jsonEqual(env["inventory"], actual["inventory"]) || !jsonEqual(util.Get(run, "agent_configuration", "skills"), manifest["skills"]) {
		t.Fatal(run)
	}
	for _, f := range []string{"report.html", "pretty-journey.html"} {
		expectIn(t, read(t, filepath.Join(rd, f)), "resolved-local (declared: requested-local)", "local (reported)",
			"unsupported", "observed-only", "not a filesystem sandbox", `href="environment.json"`, `href="agent-configuration.json"`)
	}
	before(t, read(t, filepath.Join(rd, "report.html")), `id="ASK-001"`, `id="configuration"`)
	md := read(t, filepath.Join(rd, "asks.md"))
	before(t, md, "## Prioritized register", "## Execution environment")
	expectIn(t, md, "requested-local")
}

func TestUnknownUseIsNotInferredFromEnabledOrDisabled(t *testing.T) {
	p := newProfiles(t)
	manifest := configuration("product-guide", "selected", strings.Repeat("a", 64))
	skills := util.MapOr(manifest["skills"])
	skills["entries"] = append(util.List(skills["entries"]),
		M{"name": "disabled", "states": M{"enabled": false}},
		M{"name": "observed", "states": M{"loaded": true, "invoked": false}})
	rd, _, _ := p.makeRun("sample", 1, runOpts{manifest: manifest})
	for _, f := range []string{"report.html", "pretty-journey.html"} {
		_, rows := htmlRecords(read(t, filepath.Join(rd, f)))
		find := func(name string) []string {
			for _, r := range rows {
				if len(r) > 0 && r[0] == name {
					return r
				}
			}
			t.Fatalf("%s: no row %s", f, name)
			return nil
		}
		if got := strings.Join(find("skills: product-guide")[2:], ","); got != "true,true,true,unknown,unknown" {
			t.Error(got)
		}
		if got := strings.Join(find("skills: disabled")[2:], ","); got != "unknown,unknown,false,unknown,unknown" {
			t.Error(got)
		}
		if r := find("skills: observed"); strings.Join(r[len(r)-2:], ",") != "true,false" {
			t.Error(r)
		}
	}
	saved := util.Maps(util.Get(pyfmt.ReadMap(filepath.Join(rd, "run.json")), "agent_configuration", "skills", "entries"))
	if v, ok := util.MapOr(saved[0]["states"])["loaded_into_context"]; !ok || v != nil {
		t.Error("loaded_into_context should stay null")
	}
	if _, ok := util.MapOr(saved[1]["states"])["invoked"]; ok {
		t.Error("invoked must not be inferred")
	}
	expectIn(t, read(t, filepath.Join(rd, "asks.md")), "unknown, not false")
}

func TestProfileModesBackendHashesAndLimitsSurviveMatrix(t *testing.T) {
	p := newProfiles(t)
	p.makeRun("plain", 1, runOpts{actual: environment("local-observational", "local"), manifest: configuration("plain", "none", strings.Repeat("a", 64))})
	p.makeRun("guided", 1, runOpts{actual: environment("container-profile", "container"), manifest: configuration("product-guide", "selected", strings.Repeat("a", 64))})
	matrix := p.matrix()
	rows := map[string]M{}
	for _, r := range util.Maps(matrix["runs"]) {
		rows[util.S(r["run_id"])] = r
	}
	if util.Get(rows["plain-r1"], "agent_configuration", "skills", "mode") != "none" ||
		util.Get(rows["guided-r1"], "execution_environment", "backend") != "container" ||
		util.Get(util.Maps(util.Get(rows["guided-r1"], "agent_configuration", "skills", "entries"))[0], "content_hash") != strings.Repeat("a", 64) ||
		rows["guided-r1"]["output_tokens"] != nil {
		t.Fatal(rows)
	}
	expectIn(t, strings.Join(util.Strings(util.Get(matrix, "consistency", "environment_profiles")), " "), "container-profile")
	for _, f := range []string{"index.html", "matrix.md"} {
		expectIn(t, read(t, filepath.Join(p.out, f)), "product-guide", "container-profile", "unsupported",
			"does not report skill invocation events", "agent-configuration.json")
	}
	text := read(t, filepath.Join(p.out, "index.html"))
	before(t, text, "Explain the synthetic option", "Configuration coverage and limits")
	before(t, text, "Asks by configuration", "Technical comparison")
}

func TestRepetitionsWithSameProfileNameKeepDistinctResolvedContent(t *testing.T) {
	p := newProfiles(t)
	for rep, digest := range map[int]string{1: strings.Repeat("a", 64), 2: strings.Repeat("b", 64)} {
		p.makeRun("paired", rep, runOpts{actual: environment("local-observational", "local"), manifest: configuration("product-guide", "selected", digest)})
	}
	matrix := p.matrix()
	cells := util.Maps(matrix["cells"])
	if len(cells) != 1 {
		t.Fatal(cells)
	}
	hashes := map[string]bool{}
	for _, v := range util.Maps(cells[0]["agent_configurations"]) {
		hashes[util.S(util.Maps(util.Get(v, "skills", "entries"))[0]["content_hash"])] = true
	}
	if !hashes[strings.Repeat("a", 64)] || !hashes[strings.Repeat("b", 64)] || len(hashes) != 2 {
		t.Fatal(hashes)
	}
	expectIn(t, util.S(util.Get(matrix, "consistency", "configuration_note")), "shared profile name")
	expectIn(t, read(t, filepath.Join(p.out, "matrix.md")), strings.Repeat("b", 64))
}

func TestLaunchValuesAreExcludedWithoutLosingManifestEvidence(t *testing.T) {
	p := newProfiles(t)
	manifest := configuration("product-guide", "selected", strings.Repeat("a", 64))
	manifest["launch"] = M{"env": M{"EXAMPLE_KEY": "private-launch-value"}}
	manifest["settings"] = M{"private": "private-settings-value"}
	manifest["credentials"] = M{"session_token": "private-credential-value"}
	manifest["adapter_evidence"] = M{"control": "isolated discovery", "settings": M{"key": "nested-private-value"}}
	plan := M{"backend": "local", "ownership": "borrowed", "env": M{"KEY": "private-plan-value"}}
	beforeCopy := deepCopy(manifest)
	rd, run, state := p.makeRun("sample", 1, runOpts{manifest: manifest, plan: plan})
	matrix := p.matrix()
	if !jsonEqual(state["agent_configuration"], beforeCopy) {
		t.Fatal("state manifest was modified")
	}
	if !jsonEqual(util.Get(run, "agent_configuration", "skills"), beforeCopy["skills"]) ||
		!jsonEqual(util.Get(run, "agent_configuration", "adapter_evidence"), M{"control": "isolated discovery"}) {
		t.Fatal(run["agent_configuration"])
	}
	data, _ := json.Marshal(matrix)
	outputs := string(data) + read(t, filepath.Join(rd, "run.json")) + read(t, filepath.Join(rd, "report.html")) + read(t, filepath.Join(rd, "asks.md"))
	for _, v := range []string{"private-launch-value", "private-settings-value", "private-credential-value", "nested-private-value", "private-plan-value"} {
		if strings.Contains(outputs, v) {
			t.Errorf("%s leaked", v)
		}
	}
}

func TestUntrustedProfilesCapabilitiesStatesAndCleanupAreEscaped(t *testing.T) {
	p := newProfiles(t)
	attack := `<img src=x onerror="attack()"><script>attack()</script> | [bad](javascript:bad) & "`
	actual, manifest := environment(attack, attack), configuration(attack, "selected", strings.Repeat("a", 64))
	util.MapOr(actual["capabilities"])[attack] = M{"declared": attack, "observed": attack}
	actual["limitations"] = []any{attack}
	entry := util.Maps(util.Get(manifest, "skills", "entries"))[0]
	entry["name"], entry["content_hash"] = attack, attack
	util.MapOr(entry["states"])["invoked"] = attack
	manifest["limitations"] = []any{attack}
	rd, _, _ := p.makeRun("sample", 1, runOpts{actual: actual, manifest: manifest, cleanup: M{"status": "failed", "error": attack}})
	p.matrix()
	network := regexp.MustCompile(`<(?:script|link)[^>]+(?:src|href)="https?://`)
	for _, path := range []string{filepath.Join(rd, "report.html"), filepath.Join(rd, "pretty-journey.html"), filepath.Join(p.out, "index.html")} {
		text := read(t, path)
		tags, _ := htmlRecords(text)
		scripts := 0
		for _, tg := range tags {
			if tg.name == "img" {
				t.Errorf("%s: img tag", path)
			}
			if tg.name == "script" {
				scripts++
			}
			for _, a := range tg.attrs {
				if strings.HasPrefix(a, "on") {
					t.Errorf("%s: event attribute %s", path, a)
				}
			}
		}
		if scripts != 1 { // bundled offline controls
			t.Errorf("%s: %d script tags", path, scripts)
		}
		expectIn(t, text, "&lt;img")
		if strings.Contains(text, `href="javascript:`) || network.MatchString(text) {
			t.Errorf("%s: unsafe link", path)
		}
	}
	for _, path := range []string{filepath.Join(rd, "asks.md"), filepath.Join(p.out, "matrix.md")} {
		text := read(t, path)
		if strings.Contains(text, "<img") || strings.Contains(text, "[bad](javascript:bad)") {
			t.Errorf("%s: unescaped", path)
		}
	}
}

func TestCleanupFailureIsVisibleEvenWithVerifiedSuccess(t *testing.T) {
	p := newProfiles(t)
	rd, run, _ := p.makeRun("sample", 1, runOpts{actual: environment("local-observational", "local"),
		cleanup:    M{"status": "failed", "error": "synthetic release failure"},
		stateExtra: M{"stages": M{"release": M{"status": "error", "error": "release failed"}}}})
	matrix := p.matrix()
	row := util.Maps(matrix["runs"])[0]
	if run["task_outcome_verified"] != "succeeded" || util.Get(run, "stages", "release") != "error" ||
		util.Get(row, "environment_cleanup", "status") != "failed" || row["verified"] != "succeeded" {
		t.Fatal(run, row)
	}
	for _, path := range []string{filepath.Join(rd, "report.html"), filepath.Join(rd, "pretty-journey.html"), filepath.Join(rd, "asks.md"),
		filepath.Join(p.out, "index.html"), filepath.Join(p.out, "matrix.md")} {
		expectIn(t, read(t, path), "Environment cleanup not confirmed")
	}
	expectIn(t, read(t, filepath.Join(rd, "report.html")), "synthetic release failure")
	if util.Get(run, "teardown", "status") != "none defined" {
		t.Fatal(run["teardown"])
	}
}

func TestMatrixComparabilityUsesReadableMarkdownValues(t *testing.T) {
	p := newProfiles(t)
	p.makeRun("sample", 1, runOpts{})
	p.matrix()
	text := read(t, filepath.Join(p.out, "matrix.md"))
	if !strings.Contains(text, "- product_versions: 1.0\n") {
		t.Fatalf("comparability should list versions without encoded list syntax:\n%s", text)
	}
}

func TestInventoryDoesNotEstablishInstructionVisibility(t *testing.T) {
	p := newProfiles(t)
	rd, _, state := p.makeRun("sample", 1, runOpts{})
	inventory := M{"skills_visible": int64(2), "slash_commands_visible": int64(5),
		"ajx_skills_visible": []any{"ajx"}, "ajx_slash_commands_visible": []any{"ajx"}}
	if err := pyfmt.WriteJSON(filepath.Join(rd, "telemetry.execute.json"),
		M{"events": []any{}, "harness_reported": inventory}); err != nil {
		t.Fatal(err)
	}
	run, err := WriteRunJSON(p.sp, M{"id": "sample", "config": "clean"}, 1, rd, state,
		fakeHarness("synthetic", true), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if run["report_instructions_visible_during_task"] != nil {
		t.Fatalf("inventory was mistaken for instruction visibility: %v", run)
	}
	if util.Compact(run["reported_extension_inventory"]) != util.Compact(inventory) {
		t.Fatalf("inventory evidence lost: %v", run["reported_extension_inventory"])
	}
	limitations := util.Compact(run["limitations"])
	if strings.Contains(limitations, "evaluation-aware") || strings.Contains(limitations, "(user config)") ||
		!strings.Contains(limitations, "listing does not establish") {
		t.Fatalf("inventory limitation overclaims: %s", limitations)
	}
}

func TestLegacyRecordsAndMissingArtifactsRemainRenderable(t *testing.T) {
	p := newProfiles(t)
	rd, _, _ := p.makeRun("sample", 1, runOpts{noArtifacts: true})
	old := pyfmt.ReadMap(filepath.Join(rd, "run.json"))
	for _, k := range []string{"execution_environment", "agent_configuration", "environment_cleanup"} {
		delete(old, k)
	}
	old["runner"] = M{"type": "local"}
	artifacts := util.MapOr(old["artifacts"])
	artifacts["environment"], artifacts["agent_configuration"] = "javascript:bad", "missing.json"
	pyfmt.WriteJSON(filepath.Join(rd, "run.json"), old)
	if err := RunReport(rd); err != nil {
		t.Fatal(err)
	}
	matrix := p.matrix()
	row := util.Maps(matrix["runs"])[0]
	if row["agent_configuration"] != nil || row["environment_cleanup"] != nil {
		t.Fatal(row)
	}
	for _, f := range []string{"report.html", "pretty-journey.html"} {
		text := read(t, filepath.Join(rd, f))
		expectIn(t, text, "legacy runner; capabilities unknown", "Loaded / invoked telemetry is unknown")
		for _, bad := range []string{`href="environment.json"`, `href="agent-configuration.json"`, "Environment cleanup not confirmed", `href="javascript:`} {
			if strings.Contains(text, bad) {
				t.Errorf("%s: %s", f, bad)
			}
		}
	}
}

func TestDeclaredOnlyProfileNeverBecomesCapabilityEvidence(t *testing.T) {
	p := newProfiles(t)
	rd, run, _ := p.makeRun("sample", 1, runOpts{
		references:  M{"environment": "unprepared", "agent_configuration": "requested"},
		plan:        M{"id": "unprepared", "backend": "local", "capabilities": M{"filesystem": "enforced"}},
		noArtifacts: true})
	if _, ok := util.MapOr(run["execution_environment"])["capabilities"]; ok {
		t.Fatal(run["execution_environment"])
	}
	expectIn(t, read(t, filepath.Join(rd, "report.html")), "unprepared (declared only)", "requested (declared only)",
		"local (declared only)", "Capability coverage unknown")
}

func TestStateReportAndArtifactOnlyRecordsAreSupported(t *testing.T) {
	p := newProfiles(t)
	actual, manifest := environment("local-observational", "local"), configuration("product-guide", "selected", strings.Repeat("a", 64))
	rd, run, _ := p.makeRun("sample", 1, runOpts{actual: actual, manifest: manifest, noArtifacts: true})
	if util.Get(run, "execution_environment", "report_source") != "state.environment_report" ||
		!jsonEqual(util.Get(run, "execution_environment", "capabilities"), actual["capabilities"]) {
		t.Fatal(run["execution_environment"])
	}
	pyfmt.WriteJSON(filepath.Join(rd, "state.json"), M{})
	pyfmt.WriteJSON(filepath.Join(rd, "environment.json"), M{"execution_environment": actual,
		"environment_cleanup": M{"status": "failed", "error": "release retry needed"}})
	pyfmt.WriteJSON(filepath.Join(rd, "agent-configuration.json"), manifest)
	old := pyfmt.ReadMap(filepath.Join(rd, "run.json"))
	for _, k := range []string{"execution_environment", "agent_configuration", "environment_cleanup"} {
		delete(old, k)
	}
	pyfmt.WriteJSON(filepath.Join(rd, "run.json"), old)
	if err := RunReport(rd); err != nil {
		t.Fatal(err)
	}
	refreshed := pyfmt.ReadMap(filepath.Join(rd, "run.json"))
	if !jsonEqual(refreshed["agent_configuration"], manifest) ||
		util.Get(refreshed, "execution_environment", "report_source") != "environment.json" ||
		util.Get(refreshed, "environment_cleanup", "status") != "failed" {
		t.Fatal(refreshed)
	}
	expectIn(t, read(t, filepath.Join(rd, "report.html")), "Environment cleanup not confirmed")
}

func TestBooleanCapabilitySupportDoesNotClaimEnforcement(t *testing.T) {
	p := newProfiles(t)
	actual := environment("local-observational", "local")
	actual["capabilities"] = M{"shell": true, "network": false, "filesystem": nil}
	rd, _, _ := p.makeRun("sample", 1, runOpts{actual: actual})
	_, rows := htmlRecords(read(t, filepath.Join(rd, "report.html")))
	var capabilities [][]string
	for _, r := range rows {
		if len(r) > 0 && strings.HasPrefix(r[0], "Environment: ") {
			capabilities = append(capabilities, r)
		}
	}
	if len(capabilities) != 3 {
		t.Fatal(capabilities)
	}
	details := map[string]bool{}
	for _, r := range capabilities {
		if strings.Join(r[1:4], ",") != "unknown,unknown,unknown" {
			t.Error(r)
		}
		details[r[4]] = true
	}
	if !details["true"] || !details["false"] || !details["unknown"] {
		t.Error(details)
	}
}

func TestCurrentAdapterShapesAndUncontrolledSourcesKeepEvidence(t *testing.T) {
	p := newProfiles(t)
	manifest := M{
		"name": "adapter-profile", "boundary": "optional activation", "content_absence": "unsupported",
		"skills": M{"mode": "snapshot", "entries": []any{
			M{"id": "skill:guide", "name": "guide", "sha256": strings.Repeat("c", 64),
				"installed": true, "discoverable": true, "enabled": true,
				"loaded": "unknown", "invoked": "unknown"}}},
		"plugins": M{"mode": "none", "optional_activation": "disabled", "invoked": "unknown"},
		"hooks":   M{"mode": "none", "optional_activation": "disabled", "invoked": "unknown"},
		"capabilities": M{"version": "1.2.3", "provider_called": false,
			"live_extension_activation_verified": false},
		"uncontrolled_sources": []any{
			M{"id": "managed-policy", "installed": "unknown", "enabled": "unknown", "control": "unknown"}},
		"configuration_files": []any{M{"location": "{config_dir}/settings.json", "sha256": strings.Repeat("d", 64)}},
		"launch":              M{"config": M{"private_option": "private-launch-config"}, "env": M{}},
	}
	actual := M{"id": "observed-container", "backend": "container", "capabilities": M{
		"non_root": M{"declared": "denied", "state": "enforced", "observations": M{"uid": "1000"}}}}
	cleanup := M{"status": "already_absent", "confirmed": true, "limitations": []any{"Expiry cause unknown."}}
	rd, run, _ := p.makeRun("sample", 1, runOpts{actual: actual, manifest: manifest, cleanup: cleanup,
		plan: M{"id": "observed-container", "backend": "container", "owner": "synthetic-owner",
			"profile_sha256": strings.Repeat("e", 64)}})
	if !jsonEqual(util.Get(run, "agent_configuration", "configuration_files"), manifest["configuration_files"]) ||
		util.Get(run, "execution_environment", "plan", "owner") != "synthetic-owner" {
		t.Fatal(run)
	}
	text := read(t, filepath.Join(rd, "report.html"))
	_, rows := htmlRecords(text)
	expectIn(t, text, "adapter-profile", "snapshot; guide", strings.Repeat("c", 64), "Expiry cause unknown")
	var observed, uncontrolled []string
	for _, r := range rows {
		if len(r) == 0 {
			continue
		}
		switch r[0] {
		case "Environment: non_root":
			observed = r
		case "uncontrolled sources: managed-policy":
			uncontrolled = r
		}
	}
	if len(observed) < 4 || observed[2] != "enforced" || observed[3] != `{"uid": "1000"}` {
		t.Fatal(observed)
	}
	for _, v := range uncontrolled[2:] {
		if v != "unknown" {
			t.Error(uncontrolled)
		}
	}
	for _, bad := range []string{"private-launch-config", "Environment cleanup not confirmed"} {
		if strings.Contains(text, bad) {
			t.Error(bad)
		}
	}
	matrix := p.matrix()
	if util.Get(util.Maps(matrix["runs"])[0], "environment_cleanup", "status") != "already_absent" {
		t.Fatal(matrix["runs"])
	}
	expectIn(t, read(t, filepath.Join(p.out, "index.html")), "Expiry cause unknown")
}
