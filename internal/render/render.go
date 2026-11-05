// Package render ports lib/ajx/render.py: deterministic rendering of
// run.json, asks.md, report.html and matrix.json/md/index.html.
//
// Records are read with exact JSON numbers (an int stays 3, a float stays
// 3.0) and written with Python's number spelling, so the text a Python run
// renders and the text this package renders from the same files match.
// Dictionaries iterate in sorted key order, except state.json stages, which
// follow the runner's stage order.
package render

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	ajx "github.com/royosherove/ajx"
	"github.com/royosherove/ajx/internal/envcheck"
	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/render/pyfmt"
	"github.com/royosherove/ajx/internal/reporter"
	"github.com/royosherove/ajx/internal/reportui"
	"github.com/royosherove/ajx/internal/res"
	"github.com/royosherove/ajx/internal/util"
)

type M = util.M

// Stages is the runner's stage order (Python runner.STAGES); stage records
// are listed in this order.
var Stages = []string{"prepare", "execute", "verify", "teardown", "normalize", "narrate", "release", "extract",
	"measure", "render", "archive"}

// TeardownProblems maps a teardown status to the problem it reports.
var TeardownProblems = map[string]string{
	"auth_errors": "teardown printed credential or permission errors",
	"failed":      "a teardown command failed or timed out",
	"skipped":     "teardown did not run (the workspace was missing)",
}

// ---------------------------------------------------------------- helpers

func rec(v any) M {
	if m, ok := v.(M); ok && m != nil {
		return m
	}
	return M{}
}

func getd(m M, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// fmtv is render._fmt: "n/a" for None, floats rounded to one decimal.
func fmtv(v any, unit string) string {
	if v == nil {
		return "n/a"
	}
	if f, ok := v.(float64); ok {
		v = pyfmt.Round(f, 1)
	}
	return pyfmt.Str(v) + unit
}

// cell is a Markdown table cell: no pipes or newlines, optional truncation.
func cell(v any, limit int) string {
	s := ""
	if v != nil {
		s = pyfmt.Str(v)
	}
	text := pyfmt.Strip(strings.ReplaceAll(pyfmt.CollapseSpace(s), "|", "\\|"))
	if limit <= 0 || util.RuneLen(text) <= limit {
		return text
	}
	return util.Clip(text, limit-1) + "…"
}

func runSortKey(name string) (string, int) {
	i := strings.LastIndex(name, "-r")
	c, rep := "", name
	if i >= 0 {
		c, rep = name[:i], name[i+2:]
	}
	n := 0
	if rep != "" {
		ok := true
		for _, r := range rep {
			if !pyfmt.IsDigit(r) {
				ok = false
				break
			}
		}
		if ok {
			n, _ = strconv.Atoi(rep)
		}
	}
	return c, n
}

func stripPrefix(version, name any) any {
	v, vok := version.(string)
	n, nok := name.(string)
	if vok && nok && v != "" && n != "" && strings.HasPrefix(strings.ToLower(v), strings.ToLower(n)) {
		r := []rune(v)
		k := util.RuneLen(n)
		if k > len(r) {
			k = len(r)
		}
		return pyfmt.Strip(string(r[k:]))
	}
	return version
}

func mm(v any) string {
	f, ok := pyfmt.Number(v)
	if v == nil || !ok {
		return "n/a"
	}
	return fmt.Sprintf("%sm %02ds", pyfmt.IntStr(pyfmt.FloorDiv(f, 60)), int64(pyfmt.Mod(f, 60)))
}

func readMap(sc *pyfmt.Scope, path string, def M) (M, error) {
	v, err := sc.ReadJSON(path, nil)
	if err != nil {
		return nil, pyfmt.Errorf("JSONDecodeError", "%s: %v", path, err)
	}
	if v == nil {
		if _, statErr := os.Stat(path); statErr != nil {
			return def, nil
		}
	}
	return util.MapOr(v), nil
}

func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

func strs(v any) []string {
	var out []string
	for _, x := range util.List(v) {
		out = append(out, pyfmt.Str(x))
	}
	return out
}

func join(v any, sep string) string { return strings.Join(strs(v), sep) }

// stageKeys lists a stages record in the runner's order, then any others sorted.
func stageKeys(stages M) []string {
	var out []string
	for _, s := range Stages {
		if _, ok := stages[s]; ok {
			out = append(out, s)
		}
	}
	for _, k := range util.SortedKeys(stages) {
		if !util.Contains(Stages, k) {
			out = append(out, k)
		}
	}
	return out
}

// ---------------------------------------------------------------- run.json

// These are report records, not launch contexts. Reports still require review
// before sharing: dropping private fields does not sanitize arbitrary output.
var privateProfileFields = map[string]bool{
	"env": true, "environment_variables": true, "environment_values": true, "settings": true, "settings_json": true,
	"config_values": true, "launch": true, "launch_details": true, "launch_controls": true, "runtime": true, "private": true,
	"credentials": true, "credential": true, "secret": true, "secrets": true, "password": true, "token": true, "api_key": true,
	"authorization": true, "headers": true, "argv": true, "args": true,
}

func publicProfile(sc *pyfmt.Scope, v any) any {
	switch t := v.(type) {
	case M:
		out := M{}
		var keys []string
		for _, k := range pyfmt.Keys(t) {
			x := t[k]
			low := strings.ReplaceAll(strings.ToLower(k), "-", "_")
			lower := strings.ToLower(k)
			if strings.HasPrefix(k, "_") || privateProfileFields[low] {
				continue
			}
			private := false
			for _, suffix := range []string{"_env", "_token", "_secret", "_password", "_api_key"} {
				if strings.HasSuffix(lower, suffix) {
					private = true
				}
			}
			if !private {
				out[k] = publicProfile(sc, x)
				keys = append(keys, k)
			}
		}
		sc.Register(out, keys)
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = publicProfile(sc, x)
		}
		return out
	}
	return v
}

func profileID(v any) any {
	if m, ok := v.(M); ok {
		return util.Or(m["profile_id"], m["id"], m["name"])
	}
	return v
}

var planKeys = []string{"id", "profile_id", "name", "backend", "ownership", "owner", "status", "resource_id",
	"environment_id", "container_id", "profile_sha256", "created_at", "expires_at", "limitations"}

func profileRecords(sc *pyfmt.Scope, runDir string, state, cellM, existing M) (M, error) {
	cellM, existing = rec(cellM), rec(existing)
	snapshot, err := readMap(sc, filepath.Join(runDir, "environment.json"), M{})
	if err != nil {
		return nil, err
	}
	actual := snapshot["execution_environment"]
	source := "environment.json"
	if _, ok := actual.(M); !ok {
		actual, source = state["environment_report"], "state.environment_report"
	}
	recorded := actual
	if _, ok := actual.(M); !ok {
		recorded = existing["execution_environment"]
	}
	environment := M{}
	if r, ok := recorded.(M); ok {
		environment = publicProfile(sc, r).(M)
	}
	if plan, ok := state["environment"].(M); ok {
		picked := M{}
		var keys []string
		for _, k := range planKeys {
			if v, present := plan[k]; present {
				picked[k] = v
				keys = append(keys, k)
			}
		}
		sc.Register(picked, keys)
		environment["plan"] = publicProfile(sc, picked)
	}
	previousEnvironment := rec(existing["execution_environment"])
	if declared := profileID(getd(cellM, "environment", previousEnvironment["declared_profile_id"])); declared != nil {
		environment["declared_profile_id"] = declared
	}
	if _, ok := actual.(M); ok {
		environment["report_source"] = source
	}
	manifest, ok := state["agent_configuration"].(M)
	if !ok {
		v, err := sc.ReadJSON(filepath.Join(runDir, "agent-configuration.json"), nil)
		if err != nil {
			return nil, pyfmt.Errorf("JSONDecodeError", "%v", err)
		}
		manifest, ok = v.(M)
	}
	if !ok {
		manifest, ok = existing["agent_configuration"].(M)
	}
	configuration := M{}
	if ok {
		configuration = publicProfile(sc, manifest).(M)
	}
	previousConfiguration := rec(existing["agent_configuration"])
	if declared := profileID(getd(cellM, "agent_configuration", previousConfiguration["declared_profile_id"])); declared != nil {
		configuration["declared_profile_id"] = declared
	}
	cleanup := getd(state, "environment_cleanup", getd(snapshot, "environment_cleanup", existing["environment_cleanup"]))
	out := M{"execution_environment": nil, "agent_configuration": nil, "environment_cleanup": publicProfile(sc, cleanup)}
	if len(environment) > 0 {
		out["execution_environment"] = environment
	}
	if len(configuration) > 0 {
		out["agent_configuration"] = configuration
	}
	return out, nil
}

// ProfileRecords keeps actual reports separate from durable intent; old runs need neither.
//
// execution_environment retains the capability report, with a separate plan
// and declared_profile_id. agent_configuration retains the sanitized
// manifest. Absent records and usage states are never converted to negative
// observations. Unreadable records are treated as absent.
func ProfileRecords(runDir string, state, cell, existing M) M {
	sc := pyfmt.NewScope()
	defer sc.Close()
	out, err := profileRecords(sc, runDir, rec(state), cell, existing)
	if err != nil {
		out, _ = profileRecords(sc, filepath.Join(runDir, "\x00unreadable"), rec(state), cell, existing)
	}
	return out
}

// ProfileArtifacts names the profile evidence files present in the run directory.
func ProfileArtifacts(runDir string) M {
	out := M{}
	for _, a := range [][2]string{{"environment", "environment.json"}, {"agent_configuration", "agent-configuration.json"}} {
		if isFile(filepath.Join(runDir, a[1])) {
			out[a[0]] = a[1]
		}
	}
	return out
}

func harnessFacts(h plugin.Harness) (string, bool) {
	if h == nil {
		return "None", false
	}
	return h.Name(), h.Base().CleanSupported
}

// WriteRunJSON assembles and writes run.json for a run.
func WriteRunJSON(sp, cellM M, rep int, runDir string, state M, h plugin.Harness, auth plugin.Auth, rn plugin.Runner) (run M, err error) {
	defer pyfmt.Recover(&err)
	sc := pyfmt.NewScope()
	defer sc.Close()
	state = rec(state)
	env, err := readMap(sc, filepath.Join(runDir, "environment.json"), M{})
	if err != nil {
		return nil, err
	}
	verify, err := readMap(sc, filepath.Join(runDir, "verify.json"), M{})
	if err != nil {
		return nil, err
	}
	tel, err := readMap(sc, filepath.Join(runDir, "telemetry.execute.json"), M{})
	if err != nil {
		return nil, err
	}
	meas, err := readMap(sc, filepath.Join(runDir, "measurements.json"), M{})
	if err != nil {
		return nil, err
	}
	setupV, err := sc.ReadJSON(filepath.Join(runDir, "setup.json"), []any{})
	if err != nil {
		return nil, pyfmt.Errorf("JSONDecodeError", "%v", err)
	}
	ex := rec(getd(state, "execute", M{}))
	harnessName, cleanSupported := harnessFacts(h)
	envAuth := rec(getd(env, "auth", M{}))
	limitations := append([]any{}, util.List(tel["limitations"])...)
	for _, p := range util.List(envAuth["problems"]) {
		limitations = append(limitations, "auth: "+pyfmt.Str(p))
	}
	if !cleanSupported && pyfmt.Equal(pyfmt.Index(cellM, "config"), "clean") && !util.Truthy(state["agent_configuration"]) {
		limitations = append(limitations, harnessName+" cannot disable user config; 'clean' here means only no AJX material was supplied")
	}
	if flags := util.List(tel["isolation_flags"]); len(flags) > 0 {
		limitations = append(limitations, fmt.Sprintf("isolation flags on %d tool call(s); see telemetry.execute.json", len(flags)))
	}
	hr := rec(tel["harness_reported"])
	ajxVisible := util.Truthy(hr["ajx_skills_visible"]) || util.Truthy(hr["ajx_slash_commands_visible"])
	if ajxVisible {
		limitations = append(limitations, "AJX extensions were listed in the harness session inventory; "+
			"listing does not establish their origin, activation, or exposure to evaluation instructions")
	}
	inventory := M{}
	for _, key := range []string{"skills_visible", "skills_inventory", "slash_commands_visible", "ajx_skills_visible", "ajx_slash_commands_visible"} {
		if value, ok := hr[key]; ok {
			inventory[key] = value
		}
	}
	for _, l := range CredentialLimitations(env, verify, rec(state["teardown"])) {
		limitations = append(limitations, l)
	}
	profiles, err := profileRecords(sc, runDir, state, cellM, nil)
	if err != nil {
		return nil, err
	}
	for _, l := range reportui.ProfileLimitations(profiles) {
		limitations = append(limitations, l)
	}
	if reportui.CleanupProblem(profiles["environment_cleanup"]) {
		limitations = append(limitations, "environment cleanup not confirmed; owned resources may still exist (see run.json)")
	}
	var prior string
	switch {
	case profiles["agent_configuration"] != nil:
		prior = "fresh session; pinned agent profile in isolated directories; " +
			"managed/built-in discovery and observed use are recorded separately"
	case profiles["execution_environment"] != nil:
		prior = "fresh session and per-attempt environment; harness discovery controls depend on its " +
			"adapter; no explicit agent configuration profile selected"
	default:
		prior = "fresh session; no AJX material; unnamed workspace; "
		if util.Truthy(ex["config_isolated"]) {
			prior += "isolated harness config"
		} else {
			prior += "harness user config loaded"
		}
	}
	task := rec(pyfmt.Index(sp, "task"))
	trial := rec(pyfmt.Index(sp, "trial"))
	mode := "human"
	if pyfmt.Equal(task["human"], "unavailable") {
		mode = "ci"
	}
	settings := M{}
	candidates := M{"effort": cellM["effort"], "config": pyfmt.Index(cellM, "config"),
		"permission_mode": hr["permission_mode"], "args": cellM["args"]}
	for k, v := range candidates {
		if util.Truthy(v) {
			settings[k] = v
		}
	}
	var modelID any
	if ids := strs(tel["model_ids"]); len(ids) > 0 && strings.Join(ids, ",") != "" {
		modelID = strings.Join(ids, ",")
	}
	authRec := M{}
	for k, v := range envAuth {
		if k != "identity" {
			authRec[k] = v
		}
	}
	authRec["identity_recorded"] = util.Truthy(envAuth["identity"])
	identity := env["task_identity"]
	probes := rec(getd(env, "probes", M{}))
	var tools []any
	for _, k := range pyfmt.Keys(probes) {
		if k != "os" && util.Truthy(probes[k]) {
			tools = append(tools, k+": "+pyfmt.Str(probes[k]))
		}
	}
	if tools == nil {
		tools = []any{}
	}
	capabilities := []any{"shell", "file edits"}
	if harnessName != "copilot-cli" {
		capabilities = append(capabilities, "web")
	}
	setup := []any{}
	for _, s := range util.List(setupV) {
		setup = append(setup, pyfmt.Index(s, "cmd"))
	}
	var declared any
	if text := util.Clip(util.S(tel["final_text"]), 2000); text != "" {
		declared = text
	}
	scope := "no checks defined"
	if checks := util.List(getd(verify, "checks", []any{})); util.Truthy(verify["checks"]) {
		var names []string
		for _, c := range checks {
			names = append(names, pyfmt.Str(pyfmt.Index(c, "name")))
		}
		scope = fmt.Sprintf("%s/%s ajx checks passed; ", pyfmt.Str(getd(verify, "passed", int64(0))),
			pyfmt.Str(getd(verify, "total", int64(0)))) + strings.Join(names, "; ")
	}
	reporterRec := M{}
	rp := rec(pyfmt.Index(sp, "reporter"))
	for _, k := range []string{"harness", "model", "effort"} {
		reporterRec[k] = rp[k]
	}
	timestamp := any("process window only")
	if util.Truthy(tel["events"]) {
		timestamp = tel["timestamp_source"]
	}
	stages := M{}
	for k, v := range rec(getd(state, "stages", M{})) {
		stages[k] = rec(v)["status"]
	}
	var measurements, verifyArt any
	if len(meas) > 0 {
		measurements = "measurements.json"
	}
	if len(verify) > 0 {
		verifyArt = "verify.json"
	}
	artifacts := M{"journey": "journey.md", "pretty_journey": "pretty-journey.html", "asks": "asks.md", "report": "report.html",
		"measurements": measurements, "digest": "digest.md", "events": "events.jsonl", "verify": verifyArt}
	for k, v := range ProfileArtifacts(runDir) {
		artifacts[k] = v
	}
	review, err := reviewStatus(sc, runDir, state)
	if err != nil {
		return nil, err
	}
	transcript := util.Or(tel["transcript_path"], pyfmt.CleanPath(runDir)+"/execute.raw.jsonl")
	run = M{
		"schema_version":            int64(1),
		"ajx_version":               ajx.Version,
		"run_id":                    fmt.Sprintf("%s-r%d", pyfmt.Str(pyfmt.Index(cellM, "id")), rep),
		"mode":                      mode,
		"task_prompt":               M{"path": pyfmt.Index(task, "prompt_path"), "sha256": pyfmt.Index(task, "prompt_sha256")},
		"task_amendments":           []any{},
		"evaluation_wrapper_sha256": pyfmt.Index(sp, "sha256"),
		"ajx_skill_revision":        res.Fingerprint(),
		"invocation_timing":         "before_task",
		"report_instructions_visible_during_task": nil,
		"reported_extension_inventory":            inventory,
		"product": M{"name": pyfmt.Index(trial, "product"), "version": env["product_version_before"],
			"version_after": env["product_version_after"]},
		"agent": M{
			"model_id": modelID, "model_requested": getd(rec(ex["requested"]), "model", cellM["model"]), "model_version": nil,
			"harness_name": harnessName, "harness_version": rec(getd(env, "harness", M{}))["version_before"],
			"harness_version_after": env["harness_version_after"], "settings": settings,
		},
		"auth":          authRec,
		"task_identity": M{"checked": util.Truthy(identity), "ok": rec(identity)["ok"]},
		"teardown":      TeardownRecord(state, sp),
		"runner":        env["runner"],
		"starting_conditions": M{
			"workspace_revision": nil, "os": probes["os"], "tools_and_versions": tools,
			"capabilities": capabilities, "prior_context": prior, "preinstalled_setup": setup,
			"cache_state": env["cache_state"],
			"credential_scope_description": fmt.Sprintf("auth profile %s (%s); env var names only recorded",
				pyfmt.Str(envAuth["profile"]), pyfmt.Str(envAuth["type"])),
			"customer_materials": task["materials"],
		},
		"constraints": M{"human_available": !pyfmt.Equal(task["human"], "unavailable"),
			"time_limit_seconds":    pyfmt.Index(trial, "timeout_seconds"),
			"spending_limit":        trial["max_budget_usd"],
			"authorized_operations": []any{"all tool calls auto-approved in an unnamed workspace"}},
		"task_started_at": ex["started_at"], "task_stopped_at": ex["stopped_at"],
		"task_stop_reason":      ex["stop_reason"],
		"task_outcome_declared": declared,
		"task_outcome_verified": verify["outcome"],
		"verification_scope":    scope,
		"journey_provenance":    state["journey_provenance"],
		"narration_failure":     state["narration_failure"],
		"reporter":              reporterRec,
		"review_status":         review,
		"evidence": M{
			"transcript_path":    transcript,
			"timestamp_coverage": timestamp,
			"usage_coverage":     getd(tel, "usage_status", "unavailable"),
			"tool_call_coverage": getd(tel, "tool_status", "unavailable"),
		},
		"stages":      stages,
		"artifacts":   artifacts,
		"limitations": limitations,
	}
	for k, v := range profiles {
		run[k] = v
	}
	if err := pyfmt.WriteJSON(filepath.Join(runDir, "run.json"), run); err != nil {
		return nil, err
	}
	return run, nil
}

// TeardownRecord summarizes the task teardown for run.json.
func TeardownRecord(state, sp M) M {
	if !util.Truthy(sp["teardown"]) {
		return M{"status": "none defined"}
	}
	summary := rec(state["teardown"])
	if !util.Truthy(state["teardown"]) {
		ran := pyfmt.Equal(rec(rec(getd(state, "stages", M{}))["teardown"])["status"], "done")
		if ran {
			return M{"status": "unchecked"}
		}
		return M{"status": "not run"}
	}
	return M{"status": summary["status"], "commands": summary["commands"], "failed": summary["failed"],
		"auth_error_lines": int64(len(util.List(summary["auth_errors"])))}
}

// CredentialLimitations are limitations for anything that makes the recorded
// environment, verification or cleanup untrustworthy.
func CredentialLimitations(env, verify, teardown M) []string {
	var out []string
	if problem, ok := TeardownProblems[util.S(rec(teardown)["status"])]; ok {
		out = append(out, "cleanup: "+problem+"; resources the task created outside the workspace may still exist "+
			"(see teardown.json; redo with --stages teardown,render)")
	}
	if names := strs(rec(verify)["checks_with_auth_errors"]); len(names) > 0 {
		out = append(out, fmt.Sprintf("verify: %d check(s) printed credential or permission errors (%s); "+
			"their pass/fail reflects ajx's verification credentials, not only the agent's work", len(names), strings.Join(names, "; ")))
	}
	for _, note := range envcheck.RecordedNotes(rec(env)) {
		out = append(out, "env: "+note)
	}
	if ident := rec(env)["task_identity"]; util.Truthy(ident) && !util.Truthy(rec(ident)["ok"]) {
		out = append(out, "task identity check ("+pyfmt.Str(rec(ident)["cmd"])+") failed in the worker's tool shell before the task started")
	}
	return out
}

func reviewStatus(sc *pyfmt.Scope, runDir string, state M) (string, error) {
	asks, err := readMap(sc, filepath.Join(runDir, "asks.json"), M{})
	if err != nil {
		return "", err
	}
	_, statErr := os.Stat(filepath.Join(runDir, "journey.md"))
	jp := state["journey_provenance"]
	journeyOK := statErr == nil && jp != nil && !pyfmt.Equal(jp, "unavailable")
	if journeyOK && pyfmt.Equal(asks["extraction_status"], "ok") {
		return "ready", nil
	}
	return "incomplete", nil
}

// ReviewStatus is "ready" when both primary artifacts are usable; a stub
// journey or a failed extraction is not.
func ReviewStatus(runDir string, state M) string {
	s, err := reviewStatus(nil, runDir, rec(state))
	if err != nil {
		return "incomplete"
	}
	return s
}

// ---------------------------------------------------------------- asks.md

func rank(asks []M, key, label string) string {
	type val struct {
		id string
		v  any
	}
	var ranked []val
	var missing []string
	for _, a := range asks {
		id := pyfmt.Str(pyfmt.Index(a, "id"))
		v := rec(pyfmt.Index(a, "measured"))[key]
		if v == nil {
			missing = append(missing, id)
		} else {
			ranked = append(ranked, val{id, v})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		a, _ := pyfmt.Number(ranked[i].v)
		b, _ := pyfmt.Number(ranked[j].v)
		return a > b
	})
	var parts []string
	for _, r := range ranked {
		parts = append(parts, r.id+" ("+pyfmt.Str(r.v)+")")
	}
	line := strings.Join(parts, ", ")
	if line == "" {
		line = "none measurable"
	}
	if len(missing) > 0 {
		line += "; unranked (no data): " + strings.Join(missing, ", ")
	}
	return "- **" + label + "** (measured, incurred): " + line
}

// ProfileMD is literal profile evidence in Markdown, including inside pipe tables.
func ProfileMD(v any) string {
	s := "unknown"
	if v != nil {
		s = pyfmt.Str(v)
	}
	text := pyfmt.Strip(pyfmt.CollapseSpace(s))
	var b strings.Builder
	for _, r := range text {
		if strings.ContainsRune("\\`*[]_|", r) {
			fmt.Fprintf(&b, "&#%d;", r)
		} else {
			b.WriteString(pyfmt.Escape(string(r), true))
		}
	}
	return b.String()
}

// comparabilityMD formats inventories as values, not escaped Python list
// representations. Markdown metacharacters and HTML remain literal evidence.
func comparabilityMD(v any) string {
	if items, ok := v.([]any); ok {
		if len(items) == 0 {
			return "none"
		}
		values := make([]string, len(items))
		for i, item := range items {
			values[i] = comparabilityMD(item)
		}
		return strings.Join(values, ", ")
	}
	if items, ok := v.([]string); ok {
		values := make([]any, len(items))
		for i, item := range items {
			values[i] = item
		}
		return comparabilityMD(values)
	}
	s := "unknown"
	if v != nil {
		s = pyfmt.Str(v)
	}
	text := pyfmt.Escape(pyfmt.Strip(pyfmt.CollapseSpace(s)), false)
	var b strings.Builder
	for _, r := range text {
		if strings.ContainsRune("\\`*[]_|", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ConfigurationMarkdown is the asks.md configuration section.
func ConfigurationMarkdown(run M) []string {
	lines := []string{"## Execution environment and agent configuration", ""}
	for _, v := range reportui.ConfigurationValues(run) {
		lines = append(lines, "- **"+v[0]+":** "+ProfileMD(v[1]))
	}
	lines = append(lines, "", "Declared settings express intent; enforced controls and observed checks or activity are separate evidence. "+
		"A private HOME or isolated harness configuration is not a filesystem sandbox. "+
		"Enabled skills do not prove use; missing loaded/invoked telemetry remains unknown, not false.", "",
		"Capability coverage: "+ProfileMD(reportui.ProfileCoverage(run))+".", "")
	if states := reportui.ExtensionStateRows(rec(util.Or(run["agent_configuration"], M{}))); len(states) > 0 {
		lines = append(lines, "| "+strings.Join(reportui.ExtensionHeadings, " | ")+" |",
			"|"+strings.Repeat("---|", len(reportui.ExtensionHeadings)))
		for _, row := range states {
			cells := make([]string, len(row))
			for i, v := range row {
				cells[i] = ProfileMD(v)
			}
			lines = append(lines, "| "+strings.Join(cells, " | ")+" |")
		}
		lines = append(lines, "")
	} else {
		lines = append(lines, "No extension entries recorded. Loaded / invoked telemetry is unknown.", "")
	}
	for _, note := range reportui.ProfileLimitations(run) {
		lines = append(lines, "- "+ProfileMD(note))
	}
	if run["environment_cleanup"] != nil {
		lines = append(lines, "", "Environment cleanup record: "+ProfileMD(pyfmt.Dumps(run["environment_cleanup"], pyfmt.DumpOpts{Ordered: true}))+".",
			"Environment release is separate from task teardown and task verification.", "")
	}
	if reportui.CleanupProblem(run["environment_cleanup"]) {
		lines = append(lines, "**Environment cleanup not confirmed.** Owned resources may still exist; inspect `run.json`.", "")
	}
	artifacts := rec(run["artifacts"])
	for _, a := range [][3]string{{"environment", "environment.json", "Environment evidence"},
		{"agent_configuration", "agent-configuration.json", "Agent configuration manifest"}} {
		if pyfmt.Equal(artifacts[a[0]], a[1]) {
			lines = append(lines, "- ["+a[2]+"]("+a[1]+")")
		}
	}
	return append(lines, "")
}

// AsksMarkdown renders asks.md. Missing required fields fail like Python's
// KeyError.
func AsksMarkdown(run, asksData, tel M) (md string, err error) {
	defer pyfmt.Recover(&err)
	ix := pyfmt.Index
	agent, product := ix(run, "agent"), ix(run, "product")
	asks := util.Maps(asksData["asks"])
	lines := []string{"# Asks: " + pyfmt.Str(ix(product, "name")) + " (" + pyfmt.Str(ix(run, "run_id")) + ")", "",
		"Harness " + pyfmt.Str(ix(agent, "harness_name")) + " " + pyfmt.Str(util.Or(ix(agent, "harness_version"), "")) +
			", model " + pyfmt.Str(util.Or(ix(agent, "model_id"), ix(agent, "model_requested"), "default")) + ", " +
			"product version " + pyfmt.Str(util.Or(ix(product, "version"), "unknown")) + ". Task outcome declared by the agent is recorded in run.json; " +
			"verified outcome: **" + pyfmt.Str(ix(run, "task_outcome_verified")) + "** (" + pyfmt.Str(ix(run, "verification_scope")) + ").", "",
		"Every ask below was extracted from `journey.md` and checked against `events.jsonl`. Measured costs are " +
			"sums over the referenced events, computed by ajx, per ask; events shared between asks are listed so the " +
			"overlap is visible (run-level totals in measurements.json count each event once). Priorities are " +
			"rationales, not scores.", ""}
	status := pyfmt.Str(getd(asksData, "extraction_status", "unknown"))
	switch {
	case !pyfmt.Equal(getd(asksData, "extraction_status", "unknown"), "ok"):
		lines = append(lines, "## Extraction did not complete", "", "Status: **"+status+"**. No asks were extracted; this is a reporting "+
			"failure, not evidence of a clean run. Rerun with `ajx run <trial> --stages extract,measure,render`.", "")
	case util.Truthy(asksData["no_obstacles_observed"]) && len(asks) == 0:
		lines = append(lines, "## No obstacles observed", "", "The journey and telemetry show no supported ask. This is a legitimate result.", "")
	case len(asks) == 0:
		lines = append(lines, "## No asks extracted", "", "The extraction pass returned an empty register without declaring "+
			"`no_obstacles_observed`; treat as needs review.", "")
	}
	if len(asks) > 0 {
		lines = append(lines, "## Prioritized register", "",
			"| ID | Ask | Status | Labels | Events | Tool calls (failed) | Span | Tokens | Priority rationale |",
			"|---|---|---|---|---|---|---|---|---|")
		for _, a := range asks {
			m := ix(a, "measured")
			refs := join(a["event_refs"], ", ")
			if refs == "" {
				refs = join(a["journey_anchors"], ", ")
				if refs == "" {
					refs = "none"
				}
			}
			lines = append(lines, "| "+pyfmt.Str(ix(a, "id"))+" | "+cell(ix(a, "title"), 0)+" | "+pyfmt.Str(ix(a, "evidence_status"))+" | "+
				cell(join(a["labels"], ", "), 0)+" | "+cell(refs, 0)+" | "+
				pyfmt.Str(ix(m, "tool_calls"))+" ("+pyfmt.Str(ix(m, "failed_tool_calls"))+") | "+fmtv(ix(m, "span_seconds"), " s")+" | "+
				fmtv(ix(m, "output_tokens"), "")+" | "+cell(ix(a, "priority_rationale"), 0)+" |")
		}
		lines = append(lines, "", "Default order: as prioritized by the extraction pass using task impact and supported cost. Alternative rankings:", "",
			rank(asks, "span_seconds", "Time span"), rank(asks, "output_tokens", "Output tokens"),
			rank(asks, "tool_calls", "Tool calls"), rank(asks, "failed_tool_calls", "Failed calls"), "")
		gates := util.List(asksData["gates"])
		gateLine := fmt.Sprintf("- **Human intervention**: %d gate(s) recorded", len(gates))
		if len(gates) > 0 {
			var triggers []string
			for _, g := range gates {
				triggers = append(triggers, pyfmt.Str(ix(g, "trigger")))
			}
			gateLine += ": " + strings.Join(triggers, "; ")
		}
		lines = append(lines, gateLine, "", "## Detail", "")
		for _, a := range asks {
			m := ix(a, "measured")
			version := pyfmt.Str(util.Or(ix(product, "version"), "unknown"))
			evidenceLine := "- **Evidence:** events " + orStr(join(a["event_refs"], ", "), "none") + "; journey anchors " + orStr(join(a["journey_anchors"], ", "), "none")
			if util.Truthy(a["invalid_refs"]) {
				evidenceLine += "; invalid refs dropped: " + join(ix(a, "invalid_refs"), ", ")
			}
			cost := "- **Measured cost:** " + pyfmt.Str(ix(m, "tool_calls")) + " tool call(s), " + pyfmt.Str(ix(m, "failed_tool_calls")) + " failed, tool time " +
				fmtv(ix(m, "tool_seconds"), " s") + ", span " + fmtv(ix(m, "span_seconds"), " s") + ", output tokens " + fmtv(ix(m, "output_tokens"), "")
			if util.Truthy(rec(m)["shared_events"]) {
				cost += "; shared events: " + join(ix(m, "shared_events"), ", ")
			}
			cost += ". Basis: " + pyfmt.Str(ix(m, "basis")) + "."
			modeled := "not modeled"
			if util.Truthy(a["modeled_saving"]) {
				s := ix(a, "modeled_saving")
				modeled = pyfmt.Str(ix(s, "claim")) + " (comparator: " + pyfmt.Str(ix(s, "comparator")) + "; assumption: " +
					pyfmt.Str(ix(s, "assumption")) + "; uncertainty: " + pyfmt.Str(ix(s, "uncertainty")) + ")"
			}
			lines = append(lines, "### "+pyfmt.Str(ix(a, "id"))+": "+pyfmt.Str(ix(a, "title")), "",
				"- **Requested behavior:** "+pyfmt.Str(ix(a, "requested_behavior")),
				"- **Status:** "+pyfmt.Str(ix(a, "evidence_status"))+"; product version "+version,
				"- **How observed:** "+pyfmt.Str(ix(a, "how_observed")),
				"- **Consequence:** "+pyfmt.Str(ix(a, "consequence")),
				evidenceLine, cost,
				"- **Modeled value:** "+modeled,
				"- **Workaround:** "+pyfmt.Str(util.Or(a["workaround"], "none recorded")),
				"- **Priority:** "+pyfmt.Str(ix(a, "priority_rationale")),
				"- **Verification:** "+pyfmt.Str(ix(a, "verification")), "")
		}
	}
	if strengths := util.List(asksData["strengths"]); len(strengths) > 0 {
		lines = append(lines, "## Strengths observed", "")
		for _, s := range strengths {
			lines = append(lines, "- **"+pyfmt.Str(ix(s, "title"))+"**: "+pyfmt.Str(ix(s, "how_observed"))+" ("+orStr(join(util.Get(s, "event_refs"), ", "), "no anchor")+")")
		}
		lines = append(lines, "")
	}
	if gates := util.List(asksData["gates"]); len(gates) > 0 {
		lines = append(lines, "## Gates", "")
		for _, g := range gates {
			lines = append(lines, "- "+pyfmt.Str(ix(g, "trigger"))+" ; agent: "+pyfmt.Str(ix(g, "agent_action"))+" ; resolved: "+
				pyfmt.Str(ix(g, "resolved"))+" ("+join(util.Get(g, "event_refs"), ", ")+")")
		}
		lines = append(lines, "")
	}
	if errata := util.List(asksData["journey_errata"]); len(errata) > 0 {
		lines = append(lines, "## Journey corrections from the evidence check", "")
		for _, e := range errata {
			lines = append(lines, "- Journey said: "+pyfmt.Str(ix(e, "journey_claim"))+" ; evidence: "+pyfmt.Str(ix(e, "evidence"))+
				" ("+join(util.Get(e, "event_refs"), ", ")+")")
		}
		lines = append(lines, "")
	}
	lines = append(lines, ConfigurationMarkdown(run)...)
	evidence := ix(run, "evidence")
	lines = append(lines, "## Method and limits", "",
		"- Journey provenance: "+pyfmt.Str(run["journey_provenance"]),
		"- Extraction: "+status+"; fresh reporter session ("+pyfmt.Str(rec(getd(asksData, "extractor", M{}))["model_ids"])+
			"), structured output validated against schema; event references validated against the run's events.",
		"- Telemetry coverage: usage "+pyfmt.Str(ix(evidence, "usage_coverage"))+", tool calls "+pyfmt.Str(ix(evidence, "tool_call_coverage"))+
			", timestamps "+pyfmt.Str(ix(evidence, "timestamp_coverage"))+".",
		"- Limitations:")
	for _, l := range util.List(run["limitations"]) {
		lines = append(lines, "  - "+ProfileMD(l))
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ---------------------------------------------------------------- run report

func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

// RunReport renders asks.md, report.html and pretty-journey.html for a run
// and refreshes run.json's profile records, artifacts and review status.
func RunReport(runDir string) (err error) {
	defer pyfmt.Recover(&err)
	sc := pyfmt.NewScope()
	defer sc.Close()
	run, err := readMap(sc, filepath.Join(runDir, "run.json"), M{})
	if err != nil {
		return err
	}
	asksData, err := readMap(sc, filepath.Join(runDir, "asks.json"), M{"asks": []any{}})
	if err != nil {
		return err
	}
	tel, err := readMap(sc, filepath.Join(runDir, "telemetry.execute.json"), M{"events": []any{}})
	if err != nil {
		return err
	}
	meas, err := readMap(sc, filepath.Join(runDir, "measurements.json"), M{})
	if err != nil {
		return err
	}
	state, err := readMap(sc, filepath.Join(runDir, "state.json"), M{})
	if err != nil {
		return err
	}
	profiles, err := profileRecords(sc, runDir, state, nil, run)
	if err != nil {
		return err
	}
	for k, v := range profiles {
		run[k] = v
	}
	artifacts, ok := run["artifacts"].(M)
	if !ok {
		if _, present := run["artifacts"]; present {
			return pyfmt.Errorf("AttributeError", "'%s' object has no attribute 'pop'", pyfmt.TypeOf(run["artifacts"]))
		}
		artifacts = M{}
		run["artifacts"] = artifacts
	}
	delete(artifacts, "environment")
	delete(artifacts, "agent_configuration")
	for k, v := range ProfileArtifacts(runDir) {
		artifacts[k] = v
	}
	md, err := AsksMarkdown(run, asksData, tel)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(runDir, "asks.md"), []byte(md), 0o644); err != nil {
		return err
	}
	review, err := reviewStatus(sc, runDir, M{"journey_provenance": run["journey_provenance"]})
	if err != nil {
		return err
	}
	run["review_status"] = review
	artifacts["pretty_journey"] = "pretty-journey.html"
	if err := pyfmt.WriteJSON(filepath.Join(runDir, "run.json"), run); err != nil {
		return err
	}
	journey, err := readText(filepath.Join(runDir, "journey.md"))
	if err != nil {
		return err
	}
	digest, err := readText(filepath.Join(runDir, "digest.md"))
	if err != nil {
		return err
	}
	summary := rec(meas["task_summary"])
	agent, product := rec(run["agent"]), rec(run["product"])
	subtitle := pyfmt.Str(util.Or(product["version"], "Unknown product version")) + " / " + pyfmt.Str(agent["harness_name"]) + " / " +
		pyfmt.Str(util.Or(agent["model_id"], agent["model_requested"], "default model")) + " / " + pyfmt.Str(run["run_id"])
	example := util.Truthy(run["example"])
	body := reportui.RunBody(run, asksData, summary, digest, journey != "")
	page := reportui.Page(pyfmt.Str(product["name"])+": product asks", body, subtitle, "report", example)
	if err := os.WriteFile(filepath.Join(runDir, "report.html"), []byte(page), 0o644); err != nil {
		return err
	}
	body = reportui.JourneyBody(run, asksData, tel, summary, journey)
	page = reportui.Page(pyfmt.Str(product["name"])+": agent journey", body, subtitle, "journey", example)
	return os.WriteFile(filepath.Join(runDir, "pretty-journey.html"), []byte(page), 0o644)
}

// ---------------------------------------------------------------- matrix

func cellRow(sc *pyfmt.Scope, runDir string) (M, error) {
	run, err := readMap(sc, filepath.Join(runDir, "run.json"), M{})
	if err != nil {
		return nil, err
	}
	meas, err := readMap(sc, filepath.Join(runDir, "measurements.json"), M{})
	if err != nil {
		return nil, err
	}
	asks, err := readMap(sc, filepath.Join(runDir, "asks.json"), M{"asks": []any{}})
	if err != nil {
		return nil, err
	}
	state, err := readMap(sc, filepath.Join(runDir, "state.json"), M{"stages": M{}})
	if err != nil {
		return nil, err
	}
	verify, err := readMap(sc, filepath.Join(runDir, "verify.json"), M{})
	if err != nil {
		return nil, err
	}
	s := rec(getd(meas, "task_summary", M{}))
	hr := rec(s["harness_reported"])
	agent := rec(getd(run, "agent", M{}))
	register := util.List(asks["asks"])
	defects := 0
	for _, a := range register {
		if pyfmt.Equal(util.Get(a, "evidence_status"), "verified_defect") {
			defects++
		}
	}
	var provenance any
	jp := run["journey_provenance"]
	switch {
	case strings.HasPrefix(util.S(jp), "self"):
		provenance = "self"
	case pyfmt.Equal(jp, "unavailable"):
		provenance = "unavailable"
	case util.Truthy(jp):
		provenance = "reconstruction"
	}
	stagesRec := rec(getd(state, "stages", M{}))
	stages := M{}
	errs := []any{}
	keys, known := pyfmt.KnownKeys(stagesRec)
	if !known {
		keys = stageKeys(stagesRec)
	}
	for _, k := range keys {
		v := rec(stagesRec[k])
		stages[k] = v["status"]
		if pyfmt.Equal(v["status"], "error") {
			errs = append(errs, k+": "+pyfmt.Str(v["error"]))
		}
	}
	profiles, err := profileRecords(sc, runDir, state, nil, run)
	if err != nil {
		return nil, err
	}
	if register == nil {
		register = []any{}
	}
	row := M{
		"run_id": util.Or(run["run_id"], filepath.Base(runDir)), "dir": filepath.Base(runDir),
		"harness":         agent["harness_name"],
		"harness_version": stripPrefix(agent["harness_version"], agent["harness_name"]),
		"model_requested": agent["model_requested"], "model_id": agent["model_id"],
		"config": rec(getd(agent, "settings", M{}))["config"], "auth": rec(getd(run, "auth", M{}))["type"],
		"runner":          run["runner"],
		"product_version": rec(getd(run, "product", M{}))["version"],
		"declared":        util.Clip(pyfmt.Str(util.Or(run["task_outcome_declared"], "")), 160), "verified": run["task_outcome_verified"],
		"stop_reason": run["task_stop_reason"], "elapsed_seconds": s["elapsed_seconds"],
		"tool_calls": s["tool_calls"], "tool_status": s["tool_status"], "failed_tool_calls": s["failed_tool_calls"],
		"retries": s["retries_after_error"], "output_tokens": s["output_tokens"], "usage_status": s["usage_status"],
		"cost_estimate_usd": hr["cost_estimate_usd"], "credits": hr["credits"],
		"asks": int64(len(util.List(asks["asks"]))), "gates": int64(len(util.List(asks["gates"]))),
		"ask_register":          register,
		"no_obstacles_observed": getd(asks, "no_obstacles_observed", false),
		"defects":               int64(defects),
		"journey_provenance":    provenance,
		"extraction_status":     asks["extraction_status"],
		"review_status":         run["review_status"], "stages": stages,
		"errors":             errs,
		"teardown":           rec(util.Or(run["teardown"], M{}))["status"],
		"artifacts":          ProfileArtifacts(runDir),
		"verify_auth_errors": int64(len(util.List(verify["checks_with_auth_errors"]))),
		"limitations":        int64(len(util.List(run["limitations"]))),
	}
	for k, v := range profiles {
		row[k] = v
	}
	return row, nil
}

func variance(rows []M, key string) any {
	var vals []any
	for _, r := range rows {
		if key == "output_tokens" && !pyfmt.Equal(r["usage_status"], "complete") {
			continue // partial subtotals are not totals; only reconciled counts aggregate
		}
		if r[key] != nil {
			vals = append(vals, r[key])
		}
	}
	if len(vals) == 0 {
		return nil
	}
	sorted := append([]any{}, vals...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, _ := pyfmt.Number(sorted[i])
		b, _ := pyfmt.Number(sorted[j])
		return a < b
	})
	n := len(sorted)
	var median any
	if n%2 == 1 {
		median = sorted[n/2]
	} else {
		a, _ := pyfmt.Number(sorted[n/2-1])
		b, _ := pyfmt.Number(sorted[n/2])
		median = (a + b) / 2
	}
	lo, hi := vals[0], vals[0]
	for _, x := range vals[1:] {
		f, _ := pyfmt.Number(x)
		l, _ := pyfmt.Number(lo)
		h, _ := pyfmt.Number(hi)
		if f < l {
			lo = x
		}
		if f > h {
			hi = x
		}
	}
	return M{"n": int64(n), "min": lo, "median": median, "max": hi}
}

// profileVariants retains differing manifests within repetitions, including missing evidence.
func profileVariants(rows []M, key string) []any {
	seen := map[string]bool{}
	out := []any{}
	for _, r := range rows {
		k := pyfmt.Dumps(r[key], pyfmt.DumpOpts{ASCII: true})
		if !seen[k] {
			seen[k] = true
			out = append(out, r[key])
		}
	}
	return out
}

func sortedSet(items []string) []any {
	out := []any{}
	for _, s := range util.Uniq(items) {
		out = append(out, s)
	}
	return out
}

func listRuns(runsDir string) ([]string, error) {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		p := filepath.Join(runsDir, e.Name())
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			if _, err := os.Stat(filepath.Join(p, "state.json")); err == nil {
				dirs = append(dirs, p)
			}
		}
	}
	sort.SliceStable(dirs, func(i, j int) bool {
		ci, ri := runSortKey(filepath.Base(dirs[i]))
		cj, rj := runSortKey(filepath.Base(dirs[j]))
		if ci != cj {
			return ci < cj
		}
		return ri < rj
	})
	return dirs, nil
}

// MatrixOpts are MatrixReport's optional arguments.
type MatrixOpts struct {
	Synthesize bool
	Log        func(string)    // nil = print to stdout
	Ctx        context.Context // cancels the synthesis reporter call
}

// MatrixReport writes matrix.json, matrix.md and index.html for the trial.
func MatrixReport(sp M, o MatrixOpts) error {
	_, err := MatrixData(sp, o)
	return err
}

// MatrixData is MatrixReport returning the matrix record it wrote.
func MatrixData(sp M, o MatrixOpts) (matrix M, err error) {
	defer pyfmt.Recover(&err)
	sc := pyfmt.NewScope()
	defer sc.Close()
	log := o.Log
	if log == nil {
		log = func(s string) { fmt.Println(s) }
	}
	goCtx := o.Ctx
	if goCtx == nil {
		goCtx = context.Background()
	}
	ix := pyfmt.Index
	trial := ix(sp, "trial")
	out := pyfmt.Str(ix(trial, "output_dir"))
	runsDir := filepath.Join(out, "runs")
	dirs, err := listRuns(runsDir)
	if err != nil {
		return nil, err
	}
	var rows []M
	for _, d := range dirs {
		row, err := cellRow(sc, d)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	plan, err := readMap(sc, filepath.Join(out, "matrix-plan.json"), M{})
	if err != nil {
		return nil, err
	}
	var cellOrder []string
	byCell := map[string][]M{}
	for _, r := range rows {
		id := pyfmt.Str(r["run_id"])
		if i := strings.LastIndex(id, "-r"); i >= 0 {
			id = id[:i]
		}
		if _, ok := byCell[id]; !ok {
			cellOrder = append(cellOrder, id)
		}
		byCell[id] = append(byCell[id], r)
	}
	var cells []M
	for _, cid := range cellOrder {
		cr := byCell[cid]
		var models, verified []any
		var ids []string
		for _, r := range cr {
			if util.Truthy(r["model_id"]) {
				ids = append(ids, pyfmt.Str(r["model_id"]))
			}
			verified = append(verified, r["verified"])
		}
		models = sortedSet(ids)
		cells = append(cells, M{"cell": cid, "runs": int64(len(cr)), "harness": cr[0]["harness"], "model_requested": cr[0]["model_requested"],
			"model_ids": models, "config": cr[0]["config"],
			"execution_environments": profileVariants(cr, "execution_environment"),
			"agent_configurations":   profileVariants(cr, "agent_configuration"),
			"verified":               verified,
			"elapsed_seconds":        variance(cr, "elapsed_seconds"), "tool_calls": variance(cr, "tool_calls"),
			"failed_tool_calls": variance(cr, "failed_tool_calls"), "output_tokens": variance(cr, "output_tokens"),
			"asks": variance(cr, "asks")})
	}
	var productVersions, harnessVersions, envProfiles, envBackends, agentProfiles []string
	for _, r := range rows {
		productVersions = append(productVersions, pyfmt.Str(r["product_version"]))
		harnessVersions = append(harnessVersions, pyfmt.Str(r["harness"])+" "+pyfmt.Str(r["harness_version"]))
		values := reportui.ConfigurationValues(r)
		envProfiles = append(envProfiles, values[0][1])
		envBackends = append(envBackends, values[1][1])
		agentProfiles = append(agentProfiles, values[2][1])
	}
	consistency := M{
		"prompt_sha256":                ix(ix(sp, "task"), "prompt_sha256"),
		"product_versions":             sortedSet(productVersions),
		"harness_versions":             sortedSet(harnessVersions),
		"environment_profiles":         sortedSet(envProfiles),
		"environment_backends":         sortedSet(envBackends),
		"agent_configuration_profiles": sortedSet(agentProfiles),
		"wall_clock_comparable":        getd(plan, "wall_clock_comparable", true),
		"configuration_note": "Compare resolved environments, capabilities, extension modes, content hashes, and states. " +
			"A shared profile name does not establish identical configuration. Enabled extensions do not prove use; " +
			"missing telemetry remains unknown. Configuration differences do not establish a measured improvement.",
		"token_note": "output tokens use each model's tokenizer; compare across models as resource usage, not reasoning; " +
			"per-cell token variance includes only runs with complete (reconciled) usage",
		"cost_note": "cost_estimate_usd (Claude Code) and credits (Kiro) are harness estimates in different units; never summed or sorted across harnesses",
	}
	var clusters []M
	clustersSet := false
	if o.Synthesize && len(rows) > 1 {
		c, err := synthesize(goCtx, sp, out, rows, log)
		if err != nil {
			if util.Interrupted(err) {
				return nil, err
			}
			log("synthesis skipped: " + err.Error())
		} else if c != nil {
			clusters, clustersSet = c, true
		}
	}
	trialRec := M{}
	for _, k := range []string{"id", "product", "repetitions", "parallel", "order_seed"} {
		trialRec[k] = ix(trial, k)
	}
	var clustersVal any
	if clustersSet {
		cl := make([]any, len(clusters))
		for i, c := range clusters {
			cl[i] = c
		}
		clustersVal = cl
	}
	rowsAny := make([]any, len(rows))
	for i, r := range rows {
		rowsAny[i] = r
	}
	cellsAny := make([]any, len(cells))
	for i, c := range cells {
		cellsAny[i] = c
	}
	matrix = M{"schema_version": int64(1), "trial": trialRec, "plan": plan, "consistency": consistency,
		"runs": rowsAny, "cells": cellsAny, "clusters": clustersVal, "generated_at": util.Now()}
	if err := pyfmt.WriteJSON(filepath.Join(out, "matrix.json"), matrix); err != nil {
		return nil, err
	}

	wall := ""
	if !util.Truthy(consistency["wall_clock_comparable"]) {
		wall = " (**concurrent runs: wall clock not comparable**)"
	}
	md := []string{"# AJX matrix: " + pyfmt.Str(ix(trial, "product")) + " / " + pyfmt.Str(ix(trial, "id")), "",
		"Task prompt sha256 `" + util.Clip(pyfmt.Str(consistency["prompt_sha256"]), 16) + "…`; " +
			fmt.Sprintf("%d run(s) across %d cell(s); ", len(rows), len(cells)) +
			"order seed " + pyfmt.Str(ix(trial, "order_seed")) + "; parallel " + pyfmt.Str(ix(trial, "parallel")) + wall + ".", "",
		"## Runs", "",
		"| Run | Harness | Model (observed) | Config | Environment / backend | Agent profile / skills / plugins / hooks (declared) | Environment cleanup | Verified | Declared (excerpt) | Stop | Wall clock | Tool calls | Failed | Retries | Output tokens | Asks (defects) | Gates | Journey | Report |",
		"|" + strings.Repeat("---|", 19)}
	for _, r := range rows {
		var profiles []string
		for _, v := range reportui.ConfigurationValues(r) {
			profiles = append(profiles, v[1])
		}
		md = append(md, "| "+cell(r["run_id"], 0)+" | "+cell(r["harness"], 0)+" "+cell(r["harness_version"], 0)+" | "+
			cell(util.Or(r["model_id"], r["model_requested"], "default"), 0)+" | "+pyfmt.Str(r["config"])+" | "+
			ProfileMD(strings.Join(profiles[:2], " / "))+" | "+ProfileMD(strings.Join(profiles[2:6], " / "))+" | "+ProfileMD(profiles[6])+" | "+
			"**"+pyfmt.Str(r["verified"])+"** | "+cell(r["declared"], 90)+" | "+pyfmt.Str(r["stop_reason"])+" | "+mm(r["elapsed_seconds"])+" | "+
			fmtv(r["tool_calls"], "")+" ("+pyfmt.Str(r["tool_status"])+") | "+fmtv(r["failed_tool_calls"], "")+" | "+fmtv(r["retries"], "")+" | "+
			fmtv(r["output_tokens"], "")+" ("+pyfmt.Str(r["usage_status"])+") | "+
			pyfmt.Str(r["asks"])+" ("+pyfmt.Str(r["defects"])+") | "+pyfmt.Str(r["gates"])+" | "+pyfmt.Str(util.Or(r["journey_provenance"], "missing"))+
			" | [report](runs/"+pyfmt.Quote(util.S(r["dir"]), "")+"/report.html) |")
	}
	anyErrors := false
	for _, r := range rows {
		if util.Truthy(r["errors"]) {
			anyErrors = true
		}
	}
	if anyErrors {
		md = append(md, "", "Stage errors:", "")
		for _, r := range rows {
			if util.Truthy(r["errors"]) {
				md = append(md, "- "+pyfmt.Str(r["run_id"])+": "+join(r["errors"], "; "))
			}
		}
	}
	var cleanupRows []M
	for _, r := range rows {
		if _, ok := TeardownProblems[util.S(r["teardown"])]; ok {
			cleanupRows = append(cleanupRows, r)
		}
	}
	if len(cleanupRows) > 0 {
		md = append(md, "", "Cleanup not confirmed (resources the task created may still exist; see each run's teardown.json):", "")
		for _, r := range cleanupRows {
			md = append(md, "- "+pyfmt.Str(r["run_id"])+": "+TeardownProblems[util.S(r["teardown"])])
		}
	}
	var envCleanup []M
	for _, r := range rows {
		if reportui.CleanupProblem(r["environment_cleanup"]) {
			envCleanup = append(envCleanup, r)
		}
	}
	if len(envCleanup) > 0 {
		md = append(md, "", "Environment cleanup not confirmed (owned resources may still exist; see each run's run.json):", "")
		for _, r := range envCleanup {
			md = append(md, "- "+ProfileMD(r["run_id"])+": "+ProfileMD(reportui.CleanupStatus(r["environment_cleanup"])))
		}
	}
	var creds []M
	for _, r := range rows {
		if util.Truthy(r["verify_auth_errors"]) {
			creds = append(creds, r)
		}
	}
	if len(creds) > 0 {
		md = append(md, "", "Verify checks that hit credential errors (their result reflects ajx's credentials, not the agent's work):", "")
		for _, r := range creds {
			md = append(md, "- "+pyfmt.Str(r["run_id"])+": "+pyfmt.Str(r["verify_auth_errors"])+" check(s); see verify.json")
		}
	}
	if repetitions(ix(trial, "repetitions")) > 1 {
		md = append(md, "", "## Per-cell variance (min / median / max)", "",
			"| Cell | Runs | Verified outcomes | Wall clock s | Tool calls | Failed | Output tokens | Asks |", "|---|---|---|---|---|---|---|---|")
		v := func(x any) string {
			m, ok := x.(M)
			if !ok || len(m) == 0 {
				return "n/a"
			}
			return fmtv(m["min"], "") + " / " + fmtv(m["median"], "") + " / " + fmtv(m["max"], "")
		}
		for _, c := range cells {
			md = append(md, "| "+pyfmt.Str(c["cell"])+" | "+pyfmt.Str(c["runs"])+" | "+join(c["verified"], ", ")+" | "+v(c["elapsed_seconds"])+" | "+
				v(c["tool_calls"])+" | "+v(c["failed_tool_calls"])+" | "+v(c["output_tokens"])+" | "+v(c["asks"])+" |")
		}
	}
	md = append(md, "", "## Configuration evidence", "")
	for _, r := range rows {
		prefix := "runs/" + pyfmt.Quote(util.S(r["dir"]), "") + "/"
		md = append(md, "### "+ProfileMD(r["run_id"]), "",
			"Capability coverage: "+ProfileMD(reportui.ProfileCoverage(r))+".",
			"Enabled extensions do not prove use; missing loaded/invoked telemetry remains unknown.", "")
		for _, note := range reportui.ProfileLimitations(r) {
			md = append(md, "- "+ProfileMD(note))
		}
		if states := reportui.ExtensionStateRows(rec(util.Or(r["agent_configuration"], M{}))); len(states) > 0 {
			md = append(md, "", "| "+strings.Join(reportui.ExtensionHeadings, " | ")+" |",
				"|"+strings.Repeat("---|", len(reportui.ExtensionHeadings)))
			for _, row := range states {
				c := make([]string, len(row))
				for i, x := range row {
					c[i] = ProfileMD(x)
				}
				md = append(md, "| "+strings.Join(c, " | ")+" |")
			}
		}
		md = append(md, "", "[Complete run evidence]("+prefix+"run.json)")
		artifacts := rec(r["artifacts"])
		for _, k := range []string{"environment", "agent_configuration"} {
			if f, ok := artifacts[k]; ok {
				md = append(md, "["+pyfmt.Str(f)+"]("+prefix+pyfmt.Str(f)+")")
			}
		}
		md = append(md, "")
	}
	md = append(md, "", "## Comparability", "")
	for _, k := range reportui.ConsistencyOrder(consistency) {
		md = append(md, "- "+k+": "+comparabilityMD(consistency[k]))
	}
	if len(clusters) > 0 {
		md = append(md, "", "## Asks across runs (clustered; clustering is inferred by the reporter model)", "",
			"| Cluster | Ask | Status | Observed in | Members |", "|---|---|---|---|---|")
		for _, c := range clusters {
			var members []string
			for _, m := range util.List(ix(c, "members")) {
				members = append(members, "["+pyfmt.Str(ix(m, "run_id"))+"/"+pyfmt.Str(ix(m, "ask_id"))+"](runs/"+pyfmt.Str(ix(m, "run_id"))+"/asks.md)")
			}
			md = append(md, "| "+pyfmt.Str(ix(c, "id"))+" | "+pyfmt.Str(ix(c, "title"))+" | "+pyfmt.Str(ix(c, "evidence_status"))+" | "+
				pyfmt.Str(ix(c, "observed_in"))+fmt.Sprintf("/%d runs | ", len(rows))+strings.Join(members, ", ")+" |")
		}
		md = append(md, "", "Representative observations:", "")
		for _, c := range clusters {
			obs := ix(c, "representative_observation")
			md = append(md, "- **"+pyfmt.Str(ix(c, "id"))+" "+pyfmt.Str(ix(c, "title"))+"** ("+pyfmt.Str(ix(obs, "run_id"))+"): "+pyfmt.Str(ix(obs, "text")))
		}
	} else {
		md = append(md, "", "## Asks per run", "")
		for _, r := range rows {
			a, err := readMap(sc, filepath.Join(runsDir, util.S(r["dir"]), "asks.json"), M{"asks": []any{}})
			if err != nil {
				return nil, err
			}
			var listing []string
			for _, x := range util.List(a["asks"]) {
				listing = append(listing, pyfmt.Str(ix(x, "id"))+" "+pyfmt.Str(ix(x, "title")))
			}
			tag := ""
			if !pyfmt.Equal(a["extraction_status"], "ok") {
				tag = " [extraction " + pyfmt.Str(getd(a, "extraction_status", "missing")) + "]"
			}
			md = append(md, fmt.Sprintf("- **%s** (%d)%s: %s", pyfmt.Str(r["run_id"]), len(util.List(a["asks"])), tag, orStr(strings.Join(listing, "; "), "none")))
		}
	}
	mdText := strings.Join(md, "\n")
	if err := os.WriteFile(filepath.Join(out, "matrix.md"), []byte(mdText+"\n"), 0o644); err != nil {
		return nil, err
	}
	registers := map[string]M{}
	for _, r := range rows {
		a, err := readMap(sc, filepath.Join(runsDir, util.S(r["dir"]), "asks.json"), M{"asks": []any{}})
		if err != nil {
			return nil, err
		}
		registers[util.S(r["dir"])] = a
	}
	body := reportui.MatrixBody(sp, rows, cells, consistency, registers, mdText, clusters)
	page := reportui.Page(pyfmt.Str(ix(trial, "product"))+": compare runs", body,
		fmt.Sprintf("%s / %d runs / %d configurations", pyfmt.Str(ix(trial, "id")), len(rows), len(cells)),
		"matrix", util.Truthy(rec(trial)["example"]))
	if err := os.WriteFile(filepath.Join(out, "index.html"), []byte(page), 0o644); err != nil {
		return nil, err
	}
	return matrix, nil
}

func repetitions(v any) int {
	if f, ok := util.Num(v); ok {
		return int(f)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(util.S(v)))
	return n
}

// synthesize is the matrix's clustering step; tests replace it.
var synthesize = SynthesizeClusters

// SynthesizeClusters asks the reporter to group related asks across runs.
// It returns nil when fewer than two runs have asks to compare.
func SynthesizeClusters(goCtx context.Context, sp M, out string, rows []M, log func(string)) (clusters []M, err error) {
	defer pyfmt.Recover(&err)
	var sc *pyfmt.Scope
	ix := pyfmt.Index
	var registers []string
	for _, r := range rows {
		a, err := readMap(sc, filepath.Join(out, "runs", util.S(r["dir"]), "asks.json"), M{"asks": []any{}})
		if err != nil {
			return nil, err
		}
		if !util.Truthy(a["asks"]) {
			continue
		}
		var lines []string
		for _, x := range util.List(ix(a, "asks")) {
			lines = append(lines, "- "+pyfmt.Str(ix(x, "id"))+": "+pyfmt.Str(ix(x, "title"))+" ["+pyfmt.Str(ix(x, "evidence_status"))+"] requested: "+
				pyfmt.Str(ix(x, "requested_behavior"))+" observed: "+util.Clip(pyfmt.Str(ix(x, "how_observed")), 400))
		}
		registers = append(registers, "## Run "+pyfmt.Str(r["run_id"])+" ("+pyfmt.Str(r["harness"])+" / "+
			pyfmt.Str(util.Or(r["model_id"], r["model_requested"]))+")\n\n"+strings.Join(lines, "\n"))
	}
	if len(registers) < 2 {
		return nil, nil // clustering needs at least two registers to compare
	}
	schema, err := reporter.LoadSchema("clusters.schema.json")
	if err != nil {
		return nil, err
	}
	prompt, err := reporter.Prompt("synthesize.md", "product", pyfmt.Str(util.Get(sp, "trial", "product")),
		"registers", strings.Join(registers, "\n\n"))
	if err != nil {
		return nil, err
	}
	synDir := filepath.Join(out, "synthesis")
	if err := os.Mkdir(synDir, 0o777); err != nil && !os.IsExist(err) {
		return nil, err
	}
	res, err := reporter.Call(goCtx, sp, synDir, "synthesize", prompt, schema)
	if err != nil {
		return nil, err
	}
	proc := rec(res["proc"])
	if util.Truthy(proc["is_error"]) {
		return nil, pyfmt.Errorf("ValueError", "reporter returned an invalid clustering response")
	}
	data := rec(res["structured"])
	known := map[string]bool{}
	for _, r := range rows {
		a, err := readMap(sc, filepath.Join(out, "runs", util.S(r["dir"]), "asks.json"), M{})
		if err != nil {
			return nil, err
		}
		for _, x := range util.List(getd(a, "asks", []any{})) {
			known[pyfmt.Repr(r["run_id"])+"\x00"+pyfmt.Repr(ix(x, "id"))] = true
		}
	}
	clusters = []M{}
	for i, c := range util.List(data["clusters"]) {
		cm := rec(c)
		runsSeen := map[string]bool{}
		for _, m := range util.List(ix(c, "members")) {
			if !known[pyfmt.Repr(ix(m, "run_id"))+"\x00"+pyfmt.Repr(ix(m, "ask_id"))] {
				return nil, pyfmt.Errorf("ValueError", "cluster references an unknown run or ask")
			}
			runsSeen[pyfmt.Repr(ix(m, "run_id"))] = true
		}
		if !runsSeen[pyfmt.Repr(ix(ix(c, "representative_observation"), "run_id"))] {
			return nil, pyfmt.Errorf("ValueError", "cluster observation is not from a member run")
		}
		entry := M{"id": fmt.Sprintf("AJX-%03d", i+1)}
		for k, v := range cm {
			entry[k] = v
		}
		entry["observed_in"] = int64(len(runsSeen))
		clusters = append(clusters, entry)
	}
	cl := make([]any, len(clusters))
	for i, c := range clusters {
		cl[i] = c
	}
	if err := pyfmt.WriteJSON(filepath.Join(synDir, "clusters.json"), M{"clusters": cl, "reporter": proc}); err != nil {
		return nil, err
	}
	return clusters, nil
}
