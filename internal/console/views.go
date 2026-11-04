package console

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/royosherove/ajx/internal/modelcatalog"
	"github.com/royosherove/ajx/internal/planedit"
	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/runner"
	"github.com/royosherove/ajx/internal/util"
)

// ---------------------------------------------------------------- run views

func typeName(v any) string {
	t := reflect.TypeOf(v)
	if t == nil {
		return "error"
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Name() != "" {
		return t.Name()
	}
	return t.String()
}

func runNumberOf(name, prefix string) bool {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok || rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func harnessForRun(sp M, runDir string) (h plugin.Harness, why string) {
	name := ""
	defer func() {
		if r := recover(); r != nil {
			h, why = nil, fmt.Sprintf("agent %s unavailable: %s", runner.PyRepr(name), typeName(r))
		}
	}()
	base := filepath.Base(runDir)
	for _, cell := range util.Maps(util.Get(sp, "cells")) {
		if runNumberOf(base, py(cell["id"])+"-r") {
			hh, err := specHarnessFor(sp, util.S(cell["harness"]))
			if err != nil {
				return nil, fmt.Sprintf("agent %s unavailable: %s", runner.PyRepr(cell["harness"]), typeName(err))
			}
			return hh, ""
		}
	}
	name = util.S(util.Get(readMap(filepath.Join(runDir, "run.json")), "agent", "harness_name"))
	if name == "" {
		name = util.S(util.Get(readMap(filepath.Join(runDir, "environment.json")), "harness", "name"))
	}
	if name == "" {
		return nil, "The agent for this run is not known yet."
	}
	hh, err := specHarnessFor(nil, name)
	if err != nil {
		return nil, fmt.Sprintf("agent %s unavailable: %s", runner.PyRepr(name), typeName(err))
	}
	return hh, ""
}

func safeProgress(h plugin.Harness, runDir string, limit int) (items []M, err error) {
	defer func() {
		if r := recover(); r != nil {
			items, err = nil, fmt.Errorf("%s", typeName(r))
		}
	}()
	return h.Progress(runDir, "execute", limit), nil
}

func feed(sp M, runDir string, limit int) ([]any, any) {
	h, why := harnessForRun(sp, runDir)
	if h == nil {
		return []any{}, why
	}
	if !hasProgress(h) {
		return []any{}, h.Name() + " does not report live progress. The report lists every event after the run."
	}
	items, err := safeProgress(h, runDir, limit)
	if err != nil {
		return []any{}, "live progress unavailable: " + err.Error()
	}
	if len(items) > limit {
		items = items[len(items)-limit:]
	}
	out := []any{}
	for _, i := range items {
		if i == nil {
			continue
		}
		out = append(out, M{"at": i["at"], "kind": i["kind"], "text": clip(i["text"], 240), "error": util.Truthy(i["error"])})
	}
	return out, nil
}

func listOr(v any) []any {
	if l := util.List(v); l != nil {
		return l
	}
	return []any{}
}

func asksView(asks M, marks M) M {
	if asks == nil {
		if util.Get(marks, "extract", "status") == "running" {
			return M{"status": "generating"}
		}
		return M{"status": "pending"}
	}
	status := "ok"
	if util.Truthy(asks["extraction_status"]) {
		status = py(asks["extraction_status"])
	}
	if status == "in progress" {
		return M{"status": "generating"}
	}
	if status != "ok" {
		return M{"status": "incomplete", "reason": status}
	}
	items := util.Maps(asks["asks"])
	top := []any{}
	for i, a := range items {
		if i >= 8 {
			break
		}
		top = append(top, M{"id": a["id"], "title": a["title"], "labels": listOr(a["labels"]),
			"event_refs": listOr(a["event_refs"]), "evidence_status": a["evidence_status"],
			"requested_behavior": clip(a["requested_behavior"], 400), "consequence": clip(a["consequence"], 300)})
	}
	return M{"status": "ready", "count": len(listOr(asks["asks"])), "no_obstacles_observed": util.Truthy(asks["no_obstacles_observed"]),
		"strengths": len(listOr(asks["strengths"])), "gates": len(listOr(asks["gates"])), "top": top}
}

func recovery(marks M, teardown any, envCleanup M) []any {
	out := []any{}
	cleanup := envCleanup
	if cleanup == nil {
		cleanup = M{}
	}
	td := pstr(teardown)
	if util.Get(marks, "teardown", "status") == "done" && teardown != nil && (td == "failed" || td == "auth_errors" || td == "skipped") {
		stagesKey := "teardown,render"
		if cleanup["status"] == "deferred_teardown" {
			stagesKey = "teardown,release,render,archive"
		}
		out = append(out, M{"stages": stagesKey, "label": StageSets[stagesKey],
			"why": "Cleanup was not confirmed; resources the task created may still exist."})
	} else if len(cleanup) > 0 && cleanup["confirmed"] != true && cleanup["status"] != "deferred_teardown" {
		out = append(out, M{"stages": "release,render", "label": StageSets["release,render"],
			"why": "The worker's environment was not confirmed removed."})
	}
	if util.Get(marks, "extract", "status") == "error" {
		out = append(out, M{"stages": "extract,measure,render", "label": StageSets["extract,measure,render"],
			"why": "The analysis failed; the evidence is kept."})
	}
	return out
}

func cellForRun(sp M, rid string) M {
	for _, cell := range util.Maps(util.Get(sp, "cells")) {
		if runNumberOf(rid, py(cell["id"])+"-r") {
			return cell
		}
	}
	return nil
}

type execCache struct {
	key    [4]int64
	result M
}

var (
	executionsMu sync.Mutex
	executions   = map[string]execCache{} // run dir -> (file mtimes, summary): finished runs do not change
)

func mtimes(d string, names ...string) [4]int64 {
	var out [4]int64
	for i, n := range names {
		if st, err := os.Stat(filepath.Join(d, n)); err == nil {
			out[i] = st.ModTime().UnixNano()
		}
	}
	return out
}

// execution is how the worker stopped, in words: exit code, stop reason, the
// error it printed, and whether it never got going (a non-zero exit within two
// minutes and no tool calls, such as a rejected model).
func execution(sp M, d string, st M) M {
	key := mtimes(d, "state.json", "telemetry.execute.json", "execute.raw.jsonl", "execute.stderr.txt")
	executionsMu.Lock()
	cached, ok := executions[d]
	executionsMu.Unlock()
	if ok && cached.key == key {
		return cached.result
	}
	result := executionUncached(sp, d, st)
	executionsMu.Lock()
	executions[d] = execCache{key, result}
	executionsMu.Unlock()
	return result
}

func executionUncached(sp M, d string, st M) M {
	ex := util.MapOr(st["execute"])
	code, timedOut := ex["exit_code"], util.Truthy(ex["timed_out"])
	tel := readMapOrNil(filepath.Join(d, "telemetry.execute.json"))
	var items []M
	if sp != nil {
		if h, _ := harnessForRun(sp, d); h != nil && hasProgress(h) {
			items, _ = safeProgress(h, d, 0)
		}
	}
	tools := 0
	if len(tel) > 0 {
		for _, e := range util.Maps(tel["events"]) {
			if e["kind"] == "tool" {
				tools++
			}
		}
	} else {
		for _, i := range items {
			if i != nil && i["kind"] == "tool" {
				tools++
			}
		}
	}
	var errText any
	nonzero := code != nil && !eq(code, 0)
	if timedOut {
		errText = "it reached the time limit"
	} else if nonzero && !util.Truthy(ex["interrupted"]) {
		candidates := []string{strings.TrimSpace(pstr(tel["final_text"]))}
		for i := len(items) - 1; i >= 0; i-- {
			if items[i] != nil && items[i]["kind"] == "text" {
				candidates = append(candidates, strings.TrimSpace(pstr(items[i]["text"])))
				break
			}
		}
		if data, err := os.ReadFile(filepath.Join(d, "execute.stderr.txt")); err == nil {
			for _, line := range splitLines(strings.ToValidUTF8(string(data), "�")) {
				if strings.TrimSpace(line) != "" {
					candidates = append(candidates, strings.TrimSpace(line))
					break
				}
			}
		}
		for _, c := range candidates {
			if c != "" {
				errText = strings.Join(strings.Fields(c), " ")
				break
			}
		}
		if errText == nil {
			errText = "exit " + py(code)
		}
	}
	var duration *float64
	if a, b := util.S(ex["started_at"]), util.S(ex["stopped_at"]); a != "" && b != "" {
		ta, err1 := util.ParseTS(a)
		tb, err2 := util.ParseTS(b)
		if err1 == nil && err2 == nil {
			v := tb.Sub(ta).Seconds()
			duration = &v
		}
	}
	couldNotStart := errText != nil && !timedOut && tools == 0 && duration != nil && *duration < 120
	var errOut any
	if errText != nil {
		errOut = clip(errText, 300)
	}
	return M{"exit_code": code, "stop_reason": ex["stop_reason"], "timed_out": timedOut,
		"interrupted": util.Truthy(ex["interrupted"]), "tool_calls": tools, "error": errOut,
		"could_not_start": couldNotStart, "model": util.Get(ex, "requested", "model")}
}

func progressText(rid, stage, status, d string, st, mark, sp M) string {
	if status == "error" {
		return fmt.Sprintf("%s: %s failed: %s", rid, stage, clip(mark["error"], 200))
	}
	switch stage {
	case "prepare":
		if status == "interrupted" {
			return rid + ": stopped before the worker started"
		}
		return ""
	case "execute":
		switch status {
		case "running":
			return rid + ": the worker started"
		case "interrupted":
			return rid + ": the worker was stopped; its evidence is kept"
		case "done":
		default:
			return ""
		}
		ex := execution(sp, d, st)
		switch {
		case util.Truthy(ex["timed_out"]):
			return rid + ": the worker reached the time limit"
		case util.Truthy(ex["could_not_start"]):
			return rid + ": the worker could not start: " + py(ex["error"])
		case util.Truthy(ex["error"]):
			return fmt.Sprintf("%s: the worker stopped with an error (%s): %s", rid, py(ex["stop_reason"]), py(ex["error"]))
		}
		return rid + ": the worker finished"
	}
	if status != "done" {
		return ""
	}
	switch stage {
	case "verify":
		v := readMap(filepath.Join(d, "verify.json"))
		if len(v) == 0 {
			return rid + ": verification recorded no results"
		}
		return fmt.Sprintf("%s: verification %s (%s/%s checks passed)", rid, py(v["outcome"]), py(v["passed"]), py(v["total"]))
	case "teardown":
		words := map[string]string{"ok": "ok", "failed": "failed; resources may still exist",
			"auth_errors": "hit credential errors; resources may still exist", "skipped": "did not run"}
		td := util.Get(st, "teardown", "status")
		w, ok := words[util.S(td)]
		if !ok || td == nil {
			w = "had nothing to do"
		}
		return rid + ": task cleanup " + w
	case "narrate":
		madeUp := strings.HasPrefix(pstr(st["journey_provenance"]), "EDITORIAL")
		if madeUp {
			return rid + ": journey written (a labeled reconstruction)"
		}
		return rid + ": journey written"
	case "extract":
		a := readMap(filepath.Join(d, "asks.json"))
		if a["extraction_status"] == "ok" {
			return fmt.Sprintf("%s: %d ask(s) extracted", rid, len(listOr(a["asks"])))
		}
		return rid + ": ask extraction " + util.OrStr(pstr(a["extraction_status"]), "incomplete")
	case "render":
		run := readMap(filepath.Join(d, "run.json"))
		if run["review_status"] == nil || run["review_status"] == "ready" {
			return rid + ": report ready"
		}
		asks := readMap(filepath.Join(d, "asks.json"))
		why := "see the report"
		if es := asks["extraction_status"]; es != nil && es != "ok" {
			why = py(es)
		}
		return fmt.Sprintf("%s: report written, but incomplete (%s)", rid, why)
	}
	return ""
}

var seenKeyRe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func runDirs(out string) []string {
	entries, err := os.ReadDir(filepath.Join(out, "runs"))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if isDir(filepath.Join(out, "runs", e.Name())) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// journalProgress journals each run's milestones once (worker started or
// stopped, verification, cleanup, journey, asks, report, any stage error), so
// the page and `ajx console since` read one story. The once-markers in
// .ajx-console/seen/ use Python's names, so a Go and a Python console share them.
func journalProgress(trial, out string, sp M) bool {
	if !isDir(filepath.Join(out, "runs")) {
		return false
	}
	seen := filepath.Join(StateDir(trial), "seen")
	_ = os.Mkdir(seen, 0o755)
	changed := false
	for _, name := range runDirs(out) {
		d := filepath.Join(out, "runs", name)
		st := readMap(filepath.Join(d, "state.json"))
		marks := util.MapOr(st["stages"])
		for _, stage := range stages {
			mark := util.MapOr(marks[stage])
			status := pstr(mark["status"])
			if !util.Truthy(mark["status"]) {
				continue
			}
			text := progressText(name, stage, status, d, st, mark, sp)
			if text == "" {
				continue
			}
			key := seenKeyRe.ReplaceAllString(fmt.Sprintf("%s.%s.%s.%s", name, stage, status, py(mark["at"])), "_")
			f, err := os.OpenFile(filepath.Join(seen, key), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				continue
			}
			f.Close()
			record(trial, "runner", "progress", M{"run": name, "stage": stage, "status": mark["status"],
				"happened_at": mark["at"], "text": text})
			changed = true
		}
	}
	return changed
}

func runView(sp M, out, rid string, planned bool, feedLimit int) M {
	d := filepath.Join(out, "runs", rid)
	st := readMap(filepath.Join(d, "state.json"))
	marks := util.MapOr(st["stages"])
	stageViews := []any{}
	started := false
	var current any
	for _, s := range stages {
		m := util.MapOr(marks[s])
		stageViews = append(stageViews, M{"name": s, "status": m["status"], "at": m["at"], "error": clipOrNil(m["error"], 300)})
		if util.Truthy(m["status"]) {
			started = true
		}
		if current == nil && m["status"] == "running" {
			current = s
		}
	}
	execution_ := util.MapOr(st["execute"])
	execMark := util.MapOr(marks["execute"])
	execStatus := execMark["status"]
	execStarted := execution_["started_at"]
	if !util.Truthy(execStarted) {
		execStarted = nil
		if execStatus == "running" {
			execStarted = execMark["at"]
		}
	}
	verify := readMapOrNil(filepath.Join(d, "verify.json"))
	runJSON := readMap(filepath.Join(d, "run.json"))
	teardown := util.Get(st, "teardown", "status")
	envCleanup := util.Map(st["environment_cleanup"])
	cell := cellForRun(sp, rid)
	ownedEnv := cell != nil && specEnvironmentFor(sp, cell) != nil
	prepared := util.Get(marks, "prepare", "status")
	// Finished means a resume can do nothing more: rendered, or a preparation that cannot be retried.
	neverRan := prepared == "interrupted" || (prepared == "error" && ownedEnv)
	complete := util.Get(marks, "render", "status") == "done" || neverRan
	feedItems, note := []any{}, any(nil)
	if util.Truthy(execStatus) && feedLimit > 0 {
		feedItems, note = feed(sp, d, feedLimit)
	}
	asksData := readMapOrNil(filepath.Join(d, "asks.json"))
	var elapsed any
	if execStatus == "running" {
		elapsed = secondsSince(execStarted)
	}
	var verifyView any
	if verify != nil {
		checks := []any{}
		for _, c := range util.Maps(verify["checks"]) {
			location := c["location"]
			if !util.Truthy(location) {
				location = util.Get(c, "definition", "location")
			}
			checks = append(checks, M{"name": c["name"], "passed": c["passed"], "detail": clip(c["detail"], 200),
				"location": location})
		}
		verifyView = M{"outcome": verify["outcome"], "passed": verify["passed"], "total": verify["total"],
			"auth_errors": listOr(verify["checks_with_auth_errors"]), "checks": checks}
	}
	var envView any
	if len(envCleanup) > 0 {
		envView = M{"status": envCleanup["status"], "confirmed": envCleanup["confirmed"],
			"reason": clipOrNil(envCleanup["reason"], 300), "error": clipOrNil(envCleanup["error"], 300)}
	}
	teardownCommands := []any{}
	for _, c := range util.Maps(readJSON(filepath.Join(d, "teardown.json"), nil)) {
		if len(teardownCommands) >= 20 {
			break
		}
		teardownCommands = append(teardownCommands, M{"cmd": clip(c["cmd"], 200), "exit_code": c["exit_code"],
			"timed_out": util.Truthy(c["timed_out"]), "location": c["location"], "auth_errors": util.Truthy(c["auth_errors"])})
	}
	var result any
	if isTerminal(pstr(execStatus)) {
		result = execution(sp, d, st)
	}
	var model any
	switch {
	case util.Truthy(util.Get(st, "execute", "requested")):
		model = util.Get(st, "execute", "requested", "model")
	case len(runJSON) > 0:
		model = util.Get(runJSON, "agent", "model_requested")
	default:
		model = util.MapOr(cell)["model"]
	}
	strengths, gates := []any{}, []any{}
	for i, x := range util.Maps(util.Get(asksData, "strengths")) {
		if i >= 8 {
			break
		}
		strengths = append(strengths, M{"title": x["title"], "how_observed": clip(x["how_observed"], 300),
			"event_refs": listOr(x["event_refs"])})
	}
	for i, x := range util.Maps(util.Get(asksData, "gates")) {
		if i >= 8 {
			break
		}
		gates = append(gates, M{"trigger": clip(x["trigger"], 200), "agent_action": clip(x["agent_action"], 200),
			"resolved": x["resolved"], "event_refs": listOr(x["event_refs"])})
	}
	links := M{}
	for _, l := range [][2]string{{"report", "report.html"}, {"journey", "pretty-journey.html"}, {"asks", "asks.md"},
		{"journey_md", "journey.md"}} {
		if isFile(filepath.Join(d, l[1])) {
			links[l[0]] = "runs/" + rid + "/" + l[1]
		}
	}
	var timeout any
	if sp != nil {
		timeout = util.Get(sp, "trial", "timeout_seconds")
	}
	files, truncated := []any{}, false
	if started && feedLimit > 0 {
		files, truncated = outputs(d, st, util.S(util.Get(sp, "task", "fixture_dir")))
	}
	var journey any
	if feedLimit > 0 {
		journey = journeyView(d, st)
	}
	return M{
		"id": rid, "planned": planned, "started": started, "has_state": isFile(filepath.Join(d, "state.json")),
		"complete": complete, "never_ran": neverRan, "current": current,
		"stages": stageViews,
		"execute": M{"status": execStatus, "started_at": execStarted, "stopped_at": execution_["stopped_at"],
			"elapsed": elapsed, "timeout_seconds": timeout, "timed_out": execution_["timed_out"],
			"stop_reason": execution_["stop_reason"], "interrupted": util.Truthy(execution_["interrupted"])},
		"verify":              verifyView,
		"teardown":            teardown,
		"teardown_commands":   teardownCommands,
		"environment_cleanup": envView,
		"cell":                util.MapOr(cell)["id"],
		"group":               util.MapOr(cell)["group"],
		"attempt":             attemptOf(rid),
		"outputs_truncated":   truncated,
		"asks":                asksView(asksData, marks),
		"result":              result,
		"model":               model,
		"models_seen":         listOr(readMap(filepath.Join(d, "telemetry.execute.json"))["model_ids"]),
		"strengths":           strengths,
		"gates":               gates,
		"review_status":       runJSON["review_status"],
		"outputs":             files,
		"journey":             journey,
		"declared":            clipOrNil(runJSON["task_outcome_declared"], 400),
		"journey_provenance":  st["journey_provenance"],
		"feed":                feedItems, "feed_note": note,
		"links":    links,
		"recovery": recovery(marks, teardown, envCleanup),
	}
}

func plannedIDs(sp M) []string {
	if sp == nil {
		return nil
	}
	items, err := specRunPlan(sp)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, fmt.Sprintf("%s-r%d", py(it.Cell["id"]), it.Rep))
	}
	return out
}

// runViews lists every run: the saved order, then the run directories, then
// the planned ones. feedLimit <= 0 carries no live feed.
func runViews(sp M, out string, feedLimit int) []M {
	planned := plannedIDs(sp)
	meta := readMap(filepath.Join(out, "matrix-plan.json"))
	var ids []string
	seen := map[string]bool{}
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, id := range util.List(meta["order"]) {
		add(pstr(id))
	}
	for _, id := range runDirs(out) {
		add(id)
	}
	for _, id := range planned {
		add(id)
	}
	views := make([]M, 0, len(ids))
	for _, id := range ids {
		views = append(views, runView(sp, out, id, util.Contains(planned, id), feedLimit))
	}
	return views
}

func outputView(sp M, out string, runs []M) M {
	meta := readMap(filepath.Join(out, "matrix-plan.json"))
	var started, planned, done []M
	attempts := false
	for _, r := range runs {
		if util.Truthy(r["started"]) {
			started = append(started, r)
		}
		if util.Truthy(r["planned"]) {
			planned = append(planned, r)
			if util.Truthy(r["complete"]) {
				done = append(done, r)
			}
		}
		if util.Truthy(r["has_state"]) {
			attempts = true
		}
	}
	var blocker, kind any
	// The runner refuses a changed task or profile as soon as any attempt has a state file.
	if sp != nil && attempts {
		currentProfiles := M{"schema_version": 1, "environments": util.MapOr(sp["environments"]),
			"agent_configurations": util.MapOr(sp["agent_configurations"])}
		savedContract := util.Map(meta["cells_contract"])
		switch {
		case util.Truthy(meta["task_prompt_sha256"]) && !eq(meta["task_prompt_sha256"], util.Get(sp, "task", "prompt_sha256")):
			kind = "changed"
			blocker = "This output folder holds attempts of a different task prompt. Use a fresh output folder " +
				"to run the current task; the recorded attempts keep their original prompt."
		case util.Truthy(meta["normalized_profiles"]) &&
			runner.ProfileContract(util.MapOr(meta["normalized_profiles"])) != runner.ProfileContract(currentProfiles):
			kind = "changed"
			blocker = "The environment or agent profiles changed after this output folder was started. " +
				"Use a fresh output folder."
		case len(savedContract) > 0 && len(runner.ContractChanges(savedContract, runner.CellsContract(sp))) > 0:
			changes := runner.ContractChanges(savedContract, runner.CellsContract(sp))
			if len(changes) > 3 {
				changes = changes[:3]
			}
			kind = "changed"
			blocker = "These attempts were recorded with other settings (" + strings.Join(changes, "; ") +
				"). Run again in a fresh output folder so the results stay comparable."
		case len(planned) > 0 && len(done) == len(planned):
			kind = "complete"
			blocker = "Every planned attempt in this output folder has finished. Add another attempt (same folder, " +
				"comparable conditions) or use a fresh output folder."
		}
	}
	pending, unstarted := []any{}, []any{}
	for _, r := range started {
		if !util.Truthy(r["complete"]) && isTerminal(pstr(util.Get(r, "execute", "status"))) {
			pending = append(pending, r["id"])
		}
	}
	for _, r := range planned {
		if !util.Truthy(r["has_state"]) {
			unstarted = append(unstarted, r["id"])
		}
	}
	var index any
	if isFile(filepath.Join(out, "index.html")) {
		index = "index.html"
	}
	return M{"dir": out, "short": Short(out), "exists": isDir(out), "started": len(started),
		"planned": len(planned), "complete": len(done), "blocker": blocker, "blocker_kind": kind,
		"resumable": len(started) > 0 && blocker == nil, "pending_reports": pending, "unstarted": unstarted,
		"attempts_exist": attempts, "index": index}
}

func place(sp, cell M) M {
	profile := specEnvironmentFor(sp, cell)
	if profile != nil && profile["backend"] == "container" {
		limits := util.MapOr(profile["limits"])
		network := "bridge (can reach your network and the internet)"
		if profile["network"] == "none" {
			network = "none"
		}
		facts := []any{"image " + util.Clip(pstr(util.Or(profile["image"], "")), 19) + "…",
			"network: " + network,
			"runs as a non-root user; files outside its folders are not visible"}
		if len(limits) > 0 {
			facts = append(facts, fmt.Sprintf("limits: %s CPU, %s MB, %s s lifetime", py(limits["cpus"]),
				py(limits["memory_mb"]), py(limits["lifetime_seconds"])))
		}
		return M{"kind": "container", "label": "In a container on your machine",
			"summary": "The worker runs inside a local Docker container with the restrictions below.",
			"facts":   facts, "required": listOr(profile["required"]), "sandbox": true}
	}
	if profile != nil {
		return M{"kind": "local-profile", "label": "On your machine, with a fresh profile",
			"summary": "Fresh home, configuration and workspace folders. Not a sandbox: the worker runs as you " +
				"and can still reach your files and network.",
			"facts": []any{}, "required": []any{}, "sandbox": false}
	}
	conf := util.Or(cell["runner"], sp["runner"])
	kind := "local"
	switch t := conf.(type) {
	case string:
		kind = t
	case M:
		if v, ok := t["type"]; ok {
			kind = pstr(v)
		}
	}
	if kind == "local" {
		return M{"kind": "local", "label": "On your machine",
			"summary": "The worker runs as you in a fresh folder under " + Short(pstr(util.Get(sp, "trial", "workspace_root"))) +
				". Not a sandbox: it can read and change what you can, and use your network.",
			"facts": []any{}, "required": []any{}, "sandbox": false}
	}
	return M{"kind": kind, "label": "Through the " + kind + " runner",
		"summary": "A legacy runner wraps the worker; see the trial file for its isolation.", "facts": []any{},
		"required": []any{}, "sandbox": false}
}

func signin(sp, cell M) (view M) {
	defer func() {
		if r := recover(); r != nil {
			view = M{"profile": cell["auth"], "type": nil, "description": fmt.Sprintf("unavailable: %v", r)}
		}
	}()
	auth, err := specAuthFor(sp, cell)
	if err != nil || auth == nil {
		msg := "unknown auth"
		if err != nil {
			msg = err.Error()
		}
		return M{"profile": cell["auth"], "type": nil, "description": "unavailable: " + msg}
	}
	b := auth.Base()
	desc := b.Description
	if auth.Name() == "inherit" {
		desc = "Uses the login and settings this agent CLI already has on your machine."
	}
	return M{"profile": b.Profile, "type": auth.Name(), "description": desc, "isolates_config": b.IsolatesConfig}
}

func checkView(c M) M {
	t := c["type"]
	var detail any
	switch t {
	case "shell":
		detail = c["run"]
	case "file_exists":
		s := py(c["path"])
		if util.Truthy(c["contains"]) {
			s += " containing /" + py(c["contains"]) + "/"
		}
		detail = s
	case "http":
		detail = pstr(c["url"])
	default:
		rest := M{}
		for k, v := range c {
			if k != "name" && k != "type" {
				rest[k] = v
			}
		}
		detail = util.Clip(util.PyJSON(rest), 200)
	}
	return M{"name": c["name"], "type": t, "location": c["location"], "detail": clip(detail, 400)}
}

func safeAvailable(h plugin.Harness) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return h.Available()
}

func names(items any) []any {
	out := []any{}
	for _, c := range util.Maps(items) {
		out = append(out, c["name"])
	}
	return out
}

func modelSuggestions(harness string, seen map[string][]any) []any {
	out := []any{}
	for _, m := range modelcatalog.Models(harness) {
		out = append(out, m)
	}
	return append(out, seen[harness]...)
}

func trialView(trial string, sp M, loadErr string, out string) M {
	data, _ := os.ReadFile(trial)
	text := strings.ToValidUTF8(string(data), "�")
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n") // read_text's universal newlines
	raw := rawTOML(trial)
	var errV any
	if loadErr != "" {
		errV = loadErr
	}
	view := M{"path": trial, "short": Short(trial), "dir": filepath.Dir(trial),
		"sha256": sha256Hex(data), // what writeTrial checks (CRLF files too)
		"error":  errV, "text_size": util.RuneLen(text)}
	pp := promptPath(trial, sp)
	var promptBytes []byte
	if pp != "" && isFile(pp) {
		promptBytes, _ = os.ReadFile(pp)
	}
	promptText := strings.ToValidUTF8(string(promptBytes), "�")
	var ppV, ppShort any
	if pp != "" {
		ppV, ppShort = pp, Short(pp)
	}
	view["prompt"] = M{"path": ppV, "short": ppShort, "text": promptText, "sha256": sha256Hex(promptBytes),
		"lint": lintPrompt(promptText)}
	if sp == nil {
		t := util.MapOr(raw["trial"])
		view["id"], view["product"], view["output_dir"] = t["id"], t["product"], out
		view["cells"], view["checks"] = []any{}, []any{}
		return view
	}
	t, task := util.MapOr(sp["trial"]), util.MapOr(sp["task"])
	rawCells := util.Maps(raw["cells"])
	cells := []M{}
	for _, c := range util.Maps(sp["cells"]) {
		source := util.Or(c["group"], c["id"])
		var index any
		editable := false
		for i, rc := range rawCells {
			if eq(rc["id"], source) {
				index = i
				_, hasModels := rc["models"]
				editable = !hasModels
				break
			}
		}
		harnessName := util.S(c["harness"])
		verified, installed := false, false
		if h, err := specHarnessFor(sp, harnessName); err == nil && h != nil {
			verified, installed = h.Base().VerifiedLive, safeAvailable(h)
		}
		cells = append(cells, M{"id": c["id"], "harness": c["harness"], "model": c["model"], "config": c["config"],
			"raw_index": index, "model_editable": editable, "verified_live": verified, "installed": installed,
			"place": place(sp, c), "signin": signin(sp, c), "budget_enforced": harnessName == "claude-code"})
	}
	seen := map[string][]any{}
	for _, name := range globAll(filepath.Join(out, "runs")) {
		d := filepath.Join(out, "runs", name)
		harnessName := util.S(util.Get(readMap(filepath.Join(d, "state.json")), "execute", "requested", "harness"))
		for _, mid := range util.List(readMap(filepath.Join(d, "telemetry.execute.json"))["model_ids"]) {
			if harnessName != "" && util.Truthy(mid) && !containsAny(seen[harnessName], mid) {
				seen[harnessName] = append(seen[harnessName], mid)
			}
		}
	}
	cellList := make([]any, len(cells))
	for i, cell := range cells {
		cell["model_suggestions"] = modelSuggestions(util.S(cell["harness"]), seen)
		cell["model_hint"] = modelcatalog.Hint(util.S(cell["harness"]))
		cellList[i] = cell
	}
	rep := util.MapOr(sp["reporter"])
	checks := []any{}
	for _, c := range util.Maps(sp["verify"]) {
		checks = append(checks, checkView(c))
	}
	var fixture any
	if util.Truthy(task["fixture_dir"]) {
		fixture = Short(pstr(task["fixture_dir"]))
	}
	materials := listOr(task["materials"])
	extra := M{
		"id": t["id"], "product": t["product"], "output_dir": t["output_dir"], "workspace_root": t["workspace_root"],
		"timeout_seconds": t["timeout_seconds"], "narrate_timeout_seconds": t["narrate_timeout_seconds"],
		"repetitions": t["repetitions"], "parallel": t["parallel"], "max_budget_usd": t["max_budget_usd"],
		"materials": materials, "human": task["human"], "fixture_dir": fixture, "identity_cmd": task["identity_cmd"],
		"checks": checks, "setup": names(sp["setup"]), "teardown": names(sp["teardown"]), "preflight": names(sp["preflight"]),
		"cells": cellList,
		"reporter": M{"harness": rep["harness"], "model": rep["model"], "auth": rep["auth"],
			"model_suggestions": modelSuggestions(util.S(rep["harness"]), seen),
			"model_hint":        modelcatalog.Hint(util.S(rep["harness"]))},
		"shell_startup": t["shell_startup"],
		"account_task":  util.Truthy(task["identity_cmd"]) || util.Truthy(sp["preflight"]),
	}
	for k, v := range extra {
		view[k] = v
	}
	return view
}

func containsAny(list []any, v any) bool {
	for _, x := range list {
		if eq(x, v) {
			return true
		}
	}
	return false
}

// globAll lists every entry name in dir, sorted (Python sorted(dir.glob("*"))).
func globAll(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func logTail(trial string, name any, lines int) []any {
	out := []any{}
	if !util.Truthy(name) {
		return out
	}
	path := filepath.Join(StateDir(trial), "logs", filepath.Base(pstr(name)))
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return out
	}
	start := st.Size() - 16384
	if start < 0 {
		start = 0
	}
	buf := make([]byte, st.Size()-start)
	n, _ := f.ReadAt(buf, start)
	all := splitLines(strings.ToValidUTF8(string(buf[:n]), "�"))
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	for _, l := range all {
		out = append(out, l)
	}
	return out
}

func approval(entries []M, plan string) M {
	l := last(entries, "approved")
	if l == nil {
		return M{"state": "none"}
	}
	state := "stale"
	if util.S(l["plan"]) == plan && l["plan"] != nil {
		state = "valid"
	}
	return M{"state": state, "at": l["at"], "via": l["via"], "actor": l["actor"], "quote": l["quote"],
		"plan": util.Clip(pstr(l["plan"]), 12)}
}

// pendingStart is a start the user asked for that is still checking readiness.
// A request nothing followed (the server stopped mid-check) stops counting once
// its check is no longer running.
func pendingStart(entries []M, ready M) any {
	req := last(entries, "start_requested")
	if req == nil {
		return nil
	}
	for _, e := range entries {
		if entryN(e) > entryN(req) && (e["kind"] == "start_blocked" || e["kind"] == "job_started") {
			return nil
		}
	}
	age := secondsSince(req["at"])
	if ready["status"] == "checking" || (age != nil && age.(int) < 15) {
		return M{"at": req["at"], "actor": req["actor"]}
	}
	return nil
}

func processView(trial string, entries []M, job M, out string, ready M, parallel int) M {
	lock := lockInfo(out)
	lastJob := last(entries, "job_started")
	src := job
	if src == nil {
		src = lastJob
	}
	logName := util.MapOr(src)["log"]
	view := M{"state": "idle", "stale_lock": lock != nil && util.Truthy(lock["same_host"]) && !util.Truthy(lock["alive"]), "parallel": parallel,
		"log": logName, "log_tail": logTail(trial, logName, 40)}
	if job != nil {
		stop := last(entries, "stop_requested")
		jobPid, ok := intOf(job["pid"])
		if !ok {
			jobPid = -1
		}
		stopPid, _ := intOf(stop["pid"])
		stopping := stop != nil && stopPid == jobPid && eq(stop["job_n"], job["n"])
		state := "busy"
		switch {
		case stopping:
			state = "stopping"
		case job["job"] == "run" || job["job"] == "stages":
			state = "running"
		}
		view["state"], view["job"], view["pid"], view["since"], view["by"] = state, job["job"], job["pid"], job["at"], job["actor"]
		view["command"], view["outside"] = job["command"], util.Truthy(job["outside"])
		restart := last(entries, "restart_requested")
		view["restarting"] = restart != nil && eq(restart["job_n"], job["n"])
	} else if ready["status"] == "checking" {
		view["state"] = "checking"
	} else if pendingStart(entries, ready) != nil {
		view["state"] = "starting"
	}
	return view
}

// describe is one line a person can read, shared by the page and `ajx console since`.
func describe(e M) string {
	k := e["kind"]
	switch k {
	case "approved":
		where := "in the console"
		if e["via"] == "chat" {
			where = "in chat"
		}
		text := "Approved plan " + util.Clip(py(e["plan"]), 7) + " " + where
		if util.Truthy(e["quote"]) {
			text += ": “" + py(e["quote"]) + "”"
		}
		return text
	case "job_started":
		switch e["job"] {
		case "run":
			if e["via"] == "chat" {
				return "Started the run (" + py(e["via"]) + ")"
			}
			return "Started the run"
		case "stages":
			label, ok := StageSets[util.S(e["stages"])]
			if !ok {
				label = py(e["stages"])
			}
			return "Started: " + label + " for " + py(e["run"])
		case "report":
			return "Rebuilding the reports (no model calls)"
		}
		return "Started " + py(e["job"])
	case "job_finished":
		what := map[string]string{"run": "The run", "stages": "The recovery command", "report": "Rebuilding reports"}[util.S(e["job"])]
		if what == "" {
			what = "A command"
		}
		if util.Truthy(e["summary"]) {
			return what + " " + py(e["summary"])
		}
		code := e["exit_code"]
		if code == nil {
			return what + " ended"
		}
		switch {
		case eq(code, 0):
			return what + " finished"
		case eq(code, 130) || eq(code, -2):
			return what + " was stopped"
		}
		return what + " ended with exit " + py(code)
	case "run_again":
		if e["mode"] == "same-folder" {
			return "Run again: one more attempt in this output folder"
		}
		return "Run again: a fresh output folder"
	case "restart_requested":
		return "Restart: stop this run (AJX cleans up first), then run again"
	}
	fixed := map[string]string{
		"stop_requested":     "Asked the run to stop. AJX cleans up (teardown, release, archive) before it exits.",
		"check_started":      "Checking readiness (ajx validate, then ajx doctor)",
		"start_requested":    "Asked to check readiness, then start",
		"prompt_edited":      "Changed the task prompt",
		"trial_edited":       "Edited trial.toml",
		"console_started":    "Opened the console",
		"console_stopped":    "Closed the console",
		"shutdown_requested": "Asked the console to stop (runs keep going)",
	}
	if s, ok := k.(string); ok {
		if text, ok := fixed[s]; ok {
			return text
		}
	}
	switch k {
	case "checked":
		if e["status"] == "ready" {
			return "Readiness: ready"
		}
		return "Readiness: not ready"
	case "start_blocked":
		return "Did not start: " + py(e["reason"])
	case "settings_changed":
		var parts []string
		for _, c := range util.List(e["changes"]) {
			parts = append(parts, py(c))
		}
		return "Changed settings: " + strings.Join(parts, ", ")
	case "plan_changed":
		items := util.List(e["changes"])
		if len(items) == 0 {
			return "Changed the plan: no visible change"
		}
		var parts []string
		for i, c := range items {
			if i < 3 {
				parts = append(parts, py(c))
			}
		}
		text := "Changed the plan: " + strings.Join(parts, "; ")
		if len(items) > 3 {
			text += fmt.Sprintf("; and %d more", len(items)-3)
		}
		return text
	case "fresh_output":
		return "Switched to a fresh output folder: " + py(e["output_dir"])
	case "message", "note", "progress":
		return pstr(e["text"]) // the actor says who wrote it
	case "exported":
		return "Exported " + py(e["run"]) + " for sharing"
	}
	return py(k)
}

func entryView(e M, texts map[int]any, replied map[int]bool) M {
	view := M{"n": e["n"], "at": e["at"], "actor": e["actor"], "kind": e["kind"], "text": clip(describe(e), 600),
		"reply_to": e["reply_to"]}
	if util.Truthy(e["reply_to"]) && len(texts) > 0 {
		rt, _ := intOf(e["reply_to"])
		view["reply_to_text"] = clipOrNil(texts[rt], 160)
	}
	if e["kind"] == "message" {
		view["answered"] = replied[entryN(e)]
	}
	return view
}

// Snapshot is everything the page shows, as JSON (`ajx console state`).
func Snapshot(trial string, o SnapshotOpts) (M, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	feedLimit := o.FeedLimit
	if feedLimit == 0 {
		feedLimit = 14
	}
	skip := map[int]bool{}
	for _, p := range o.Skip {
		skip[p] = true
	}
	return snapshot(trial, feedLimit, skip)
}

func snapshot(trial string, feedLimit int, skip map[int]bool) (M, error) {
	sp, loadErr := load(trial)
	out := outputDir(trial, sp)
	entries, _ := readEntries(trial, 0)
	if reconcile(trial, entries, skip) {
		entries, _ = readEntries(trial, 0)
	}
	plan, err := PlanInfo(trial, sp)
	if err != nil {
		return nil, err
	}
	sha := util.S(plan["sha"])
	ready := readiness(trial, sha)
	job := activeJob(trial, entries, out)
	runs := runViews(sp, out, feedLimit)
	// Capture milestones after the run records: a fast worker may finish
	// while this snapshot is being assembled. Its completed report must not
	// be shown with a journal cursor that predates that report.
	journalProgress(trial, out, sp)
	entries, _ = readEntries(trial, 0)
	output := outputView(sp, out, runs)
	parallel := util.Int(util.Get(sp, "trial", "parallel"), 1)
	if parallel == 0 {
		parallel = 1
	}
	process := processView(trial, entries, job, out, ready, parallel)
	started := false
	for _, r := range runs {
		if util.Truthy(r["has_state"]) {
			started = true
		}
	}
	var lockReason, settingsLock, lockKind any
	switch {
	case job != nil:
		lockReason = "Locked while a run or command is in progress."
		settingsLock, lockKind = lockReason, "job"
	case started:
		lockKind = "attempts"
		lockReason = "This output folder already holds attempts of this task. Use a fresh output folder to change " +
			"the task; recorded attempts keep their original prompt."
		settingsLock = "Locked for this output folder: its attempts used these settings. To change them, run again " +
			"in a fresh output folder."
	}
	texts := map[int]any{}
	replied := map[int]bool{}
	for _, m := range entries {
		switch m["kind"] {
		case "message":
			texts[entryN(m)] = m["text"]
		case "note":
			if n, ok := intOf(m["reply_to"]); ok {
				replied[n] = true
			}
		}
	}
	cursor := 0
	if len(entries) > 0 {
		cursor = entryN(entries[len(entries)-1])
	}
	views := []any{}
	from := len(entries) - 80
	if from < 0 {
		from = 0
	}
	for _, e := range entries[from:] {
		views = append(views, entryView(e, texts, replied))
	}
	runList := make([]any, len(runs))
	for i, r := range runs {
		runList[i] = r
	}
	planModel, planOptions := planEditor(trial)
	return M{
		"schema": Schema, "generated_at": util.Now(),
		"trial": trialView(trial, sp, loadErr, out),
		"plan": M{"sha": sha, "short": util.Clip(sha, 12), "complete": plan["complete"], "covers": plan["covers"],
			"approval": approval(entries, sha)},
		"readiness":     ready,
		"process":       process,
		"output":        output,
		"runs":          runList,
		"journal":       M{"cursor": cursor, "entries": views},
		"agent":         presence(trial),
		"console":       consoleView(trial),
		"pending_start": pendingStart(entries, ready),
		"locks":         M{"prompt": lockReason, "settings": settingsLock, "kind": lockKind},
		"plan_model":    planModel,
		"plan_options":  planOptions,
		"vocabulary":    M{"labels": pairs(labels), "statuses": pairs(statuses), "meanings": labelMeanings()},
	}, nil
}

// planEditor is the Plan tab's model and what its editors may offer; nil for
// a trial file the editor cannot read (the page shows its error instead).
func planEditor(trial string) (any, any) {
	model, err := planedit.ReadModel(trial)
	if err != nil {
		return nil, nil
	}
	options, err := planedit.Options(trial, model)
	if err != nil {
		return nil, nil
	}
	lint := []any{}
	for _, l := range promptLint {
		lint = append(lint, []any{strings.TrimPrefix(l.re.String(), "(?i)"), l.why})
	}
	options.Set("prompt_lint", lint)
	return planedit.ToM(model), planedit.ToM(options)
}

func consoleView(trial string) M {
	info := readMap(serverPath(trial))
	return M{"started_at": info["started_at"], "pid": info["pid"]}
}

func serverPath(trial string) string { return filepath.Join(StateDir(trial), "server.json") }

var attemptSuffix = regexp.MustCompile(`^.+-r(\d+)$`)

// attemptOf is N of a <cell>-rN run id, or nil.
func attemptOf(rid string) any {
	if m := attemptSuffix.FindStringSubmatch(rid); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return int64(n)
		}
	}
	return nil
}
