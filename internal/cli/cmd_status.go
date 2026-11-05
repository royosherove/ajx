package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/render"
	"github.com/royosherove/ajx/internal/runner"
	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

// TeardownMarks are the status-table marks for unconfirmed cleanup.
var TeardownMarks = map[string]string{"auth_errors": "AUTH", "failed": "FAIL", "skipped": "SKIP"}

var stageMarks = map[string]string{"done": "ok", "error": "ERR", "running": "run", "interrupted": "INT"}

func cmdStatus(ns *Namespace) (int, error) {
	tail := ns.Int("tail", 0)
	if ns.Bool("json") {
		code, doc, err := statusDoc(ns.Str("trial"), &tail)
		if err != nil {
			return 2, err
		}
		emitJSON(doc)
		return code, nil
	}
	return status(ns.Str("trial"), tail, false)
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// outputOf resolves a trial file (loaded read-only) or a matrix directory.
// A trial directory (holding trial.toml and no runs/) is read as its trial
// file, the way `ajx console` accepts either.
func outputOf(path string) (string, util.M, error) {
	if isDir(path) && !isDir(filepath.Join(path, "runs")) && isFile(filepath.Join(path, "trial.toml")) {
		path = filepath.Join(path, "trial.toml")
	}
	if isFile(path) {
		sp, err := spec.Load(path, spec.LoadOpts{CleanupOnly: true})
		if err != nil {
			return "", nil, err
		}
		return util.S(util.Get(sp, "trial", "output_dir")), sp, nil
	}
	if !isDir(path) {
		return "", nil, spec.Errorf("status input does not exist or is not readable: %s; pass a trial.toml or matrix output directory", path)
	}
	if !isDir(filepath.Join(path, "runs")) && !isFile(filepath.Join(path, "matrix-plan.json")) && !isFile(filepath.Join(path, "matrix.json")) {
		return "", nil, spec.Errorf("not a trial or matrix output directory: %s; pass the trial.toml for a trial that has not started", path)
	}
	return path, nil, nil
}

// statusRunDirs distinguishes a valid, unstarted trial from unreadable output.
func statusRunDirs(out string, sp util.M) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(out, "runs"))
	if os.IsNotExist(err) && (sp != nil || isFile(filepath.Join(out, "matrix-plan.json"))) {
		return nil, nil
	}
	if err != nil {
		return nil, spec.Errorf("cannot read matrix runs in %s: %v", out, err)
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(out, "runs", entry.Name()))
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// statusRunner reports only observable liveness. A lock on another host or
// running stages without a lock are unknown, never proof that a worker is dead.
func statusRunner(out string) util.M {
	path := filepath.Join(out, ".lock")
	value, err := util.ReadJSON(path, nil)
	if err != nil {
		return util.M{"state": "unknown", "problem": err.Error()}
	}
	if value == nil {
		return util.M{"state": "unlocked"}
	}
	info := util.MapOr(value)
	host, _ := os.Hostname()
	state := "unknown"
	if info["host"] == host && util.Int(info["pid"], 0) > 0 {
		state = "stale"
		if runner.PidAlive(util.Int(info["pid"], 0)) {
			state = "running"
		}
	}
	return util.M{"state": state, "pid": info["pid"], "host": info["host"], "since": info["at"]}
}

// runDirs lists <out>/runs/* directories, sorted by name.
func runDirs(out string) []string {
	entries, err := os.ReadDir(filepath.Join(out, "runs"))
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		p := filepath.Join(out, "runs", e.Name())
		if isDir(p) {
			dirs = append(dirs, p)
		}
	}
	sort.Strings(dirs)
	return dirs
}

type runRow struct {
	dir      string
	state    util.M
	marks    []string
	stages   util.M
	verified string
	outcome  any
	authErrs []string
	teardown string
	bad      bool
	liveness string
}

func readRun(d string) (runRow, []string, error) {
	row := runRow{dir: d}
	st, err := util.ReadJSON(filepath.Join(d, "state.json"), nil)
	if err != nil {
		return row, nil, fmt.Errorf("%s: %w", filepath.Join(d, "state.json"), err)
	}
	row.state = util.MapOr(st)
	if row.state == nil || row.state["stages"] == nil {
		return row, nil, spec.Errorf("missing or invalid run state: %s; preserve this output and recover it before starting another attempt", filepath.Join(d, "state.json"))
	}
	stages := util.MapOr(row.state["stages"])
	row.stages = util.M{}
	row.liveness = util.S(statusRunner(filepath.Dir(filepath.Dir(d)))["state"])
	var cleanup []string
	td := util.S(util.Get(row.state, "teardown", "status"))
	row.teardown = td
	for _, s := range runner.Stages {
		v := util.S(util.Get(stages, s, "status"))
		if v == "running" && row.liveness == "stale" {
			v = "interrupted"
		}
		row.stages[s] = nilIfEmpty(v)
		mark, known := stageMarks[v]
		if !known {
			mark = util.OrStr(v, "-")
		}
		if s == "teardown" && v == "done" {
			if m, problem := TeardownMarks[td]; problem {
				mark = m
				cleanup = append(cleanup, fmt.Sprintf("%s: %s; see %s", filepath.Base(d), render.TeardownProblems[td],
					filepath.Join(d, "teardown.json")))
			}
		}
		row.marks = append(row.marks, mark)
		if v == "error" || v == "interrupted" || mark == "AUTH" || mark == "FAIL" || mark == "SKIP" {
			row.bad = true
		}
	}
	verify := util.ReadMap(filepath.Join(d, "verify.json"))
	outcome := verify["outcome"]
	if !util.Truthy(outcome) {
		outcome = util.ReadMap(filepath.Join(d, "run.json"))["task_outcome_verified"]
	}
	row.outcome = outcome
	if util.Truthy(outcome) {
		row.verified = spec.PyStr(outcome)
	}
	if errs := util.Strings(verify["checks_with_auth_errors"]); len(errs) > 0 {
		row.authErrs = errs
		row.verified += fmt.Sprintf(" (%d check(s) hit credential errors)", len(errs))
	}
	return row, cleanup, nil
}

func countBad(rows []runRow) int {
	n := 0
	for _, r := range rows {
		for i, s := range runner.Stages {
			v := util.S(r.stages[s])
			if v == "error" || v == "interrupted" || (r.marks[i] == "AUTH" || r.marks[i] == "FAIL" || r.marks[i] == "SKIP") {
				n++
			}
		}
	}
	return n
}

func clip7(s string) string { return util.Clip(s, 7) }

func status(path string, tail int, _ bool) (int, error) {
	out, sp, err := outputOf(path)
	if err != nil {
		return 2, err
	}
	dirs, err := statusRunDirs(out, sp)
	if err != nil {
		return 2, err
	}
	header := fmt.Sprintf("%-20s ", "run")
	var cols []string
	for _, s := range runner.Stages {
		cols = append(cols, fmt.Sprintf("%7s", clip7(s)))
	}
	outln(header + strings.Join(cols, " ") + "  verified")
	var rows []runRow
	var cleanup []string
	for _, d := range dirs {
		row, c, err := readRun(d)
		if err != nil {
			return 1, err
		}
		rows = append(rows, row)
		cleanup = append(cleanup, c...)
		var marks []string
		for _, m := range row.marks {
			marks = append(marks, fmt.Sprintf("%7s", m))
		}
		outln(fmt.Sprintf("%-20s ", filepath.Base(d)) + strings.Join(marks, " ") + "  " + row.verified)
		for _, active := range activeStages(row, time.Now()) {
			elapsed := "unknown"
			if seconds, ok := util.Num(active["elapsed_seconds"]); ok {
				elapsed = fmt.Sprintf("%.1fs", seconds)
			}
			outf("  %s: started %s; elapsed %s (runner: %s)\n",
				util.S(active["stage"]), util.Str(util.Or(active["started_at"], "unknown")), elapsed, row.liveness)
		}
	}
	for _, line := range cleanup {
		outln("CLEANUP NOT CONFIRMED " + line)
	}
	if len(cleanup) > 0 {
		outln("Resources the task created may still exist. After fixing credentials: " +
			"ajx run <trial> --cells <cell> --stages teardown,render")
	}
	if tail > 0 {
		statusTail(sp, rows, tail)
	}
	if countBad(rows) > 0 {
		return 1, nil
	}
	return 0, nil
}

// statusDoc is the --json form of status (tail nil = no events).
func statusDoc(path string, tail *int) (int, util.M, error) {
	out, sp, err := outputOf(path)
	if err != nil {
		return 2, nil, err
	}
	dirs, err := statusRunDirs(out, sp)
	if err != nil {
		return 2, nil, err
	}
	doc := util.M{"schema_version": SchemaVersion, "command": "status", "output_dir": out, "stages": strs(runner.Stages),
		"runner": statusRunner(out)}
	var runs []any
	var cleanup []any
	var rows []runRow
	for _, d := range dirs {
		row, c, err := readRun(d)
		if err != nil {
			return 1, nil, err
		}
		rows = append(rows, row)
		for _, line := range c {
			cleanup = append(cleanup, line)
		}
		confirmed := row.teardown == "ok" && row.stages["teardown"] == "done" &&
			row.stages["execute"] != "running" &&
			(row.stages["execute"] != "interrupted" || util.Get(row.state, "worker_cleanup", "confirmed") == true)
		if util.Get(row.state, "worker_cleanup", "confirmed") == false {
			confirmed = false
		}
		if util.Truthy(row.state["environment"]) && util.Get(row.state, "environment_cleanup", "confirmed") != true {
			confirmed = false
		}
		rec := util.M{"run_id": filepath.Base(d), "dir": d, "stages": row.stages, "marks": strs(row.marks),
			"verified": row.outcome, "checks_with_auth_errors": strs(row.authErrs), "teardown_status": nilIfEmpty(row.teardown),
			"cleanup_confirmed": confirmed, "runner_state": row.liveness, "stage_errors": stageErrors(row.state),
			"active_stages": activeStages(row, time.Now())}
		if tail != nil && *tail > 0 {
			rec["events"] = tailEvents(sp, row, *tail)
		}
		runs = append(runs, rec)
	}
	if runs == nil {
		runs = []any{}
	}
	if cleanup == nil {
		cleanup = []any{}
	}
	code := 0
	if countBad(rows) > 0 {
		code = 1
	}
	doc["runs"] = runs
	doc["cleanup_not_confirmed"] = cleanup
	doc["ok"] = code == 0
	doc["exit_code"] = code
	return code, doc, nil
}

// The stage marker is the only progress signal shared by every adapter.
// Elapsed time measures that marker's age, not model progress or completion.
// Liveness is reported separately; a stale lock is never shown as active.
func activeStages(row runRow, now time.Time) []util.M {
	active := []util.M{}
	for _, stage := range runner.Stages {
		if row.stages[stage] != "running" {
			continue
		}
		record := util.MapOr(util.Get(row.state, "stages", stage))
		start := util.Or(record["started_at"], record["at"])
		item := util.M{"stage": stage, "started_at": start, "elapsed_seconds": nil}
		if at, err := util.ParseTS(util.S(start)); err == nil && !at.After(now) {
			item["elapsed_seconds"] = float64(now.Sub(at).Milliseconds()) / 1000
		}
		active = append(active, item)
	}
	return active
}

func stageErrors(state util.M) []any {
	out := []any{}
	stages := util.MapOr(state["stages"])
	for _, s := range runner.Stages {
		rec := util.MapOr(stages[s])
		if util.S(rec["status"]) == "error" {
			out = append(out, util.M{"stage": s, "error": rec["error"]})
		}
	}
	return out
}

// runHarness is the run's harness adapter: the trial cell's when a trial
// file was given, else the name the run recorded.
func runHarness(sp util.M, runDir string) (plugin.Harness, string) {
	name := ""
	base := filepath.Base(runDir)
	if sp != nil {
		for _, cell := range util.Maps(sp["cells"]) {
			prefix := util.Str(cell["id"]) + "-r"
			if rest, ok := strings.CutPrefix(base, prefix); ok && isDigits(rest) {
				h, err := spec.HarnessFor(sp, util.S(cell["harness"]))
				if err != nil {
					return nil, fmt.Sprintf("harness %s unavailable: %s", spec.Repr(util.S(cell["harness"])), err)
				}
				return h, ""
			}
		}
	}
	name = util.S(util.Get(util.ReadMap(filepath.Join(runDir, "run.json")), "agent", "harness_name"))
	if name == "" {
		name = util.S(util.Get(util.ReadMap(filepath.Join(runDir, "environment.json")), "harness", "name"))
	}
	if name == "" {
		return nil, "harness unknown: no trial cell matches this run and it records no harness yet"
	}
	h, err := plugin.NewHarness(name)
	if err != nil {
		return nil, fmt.Sprintf("harness %s unavailable: KeyError: %s", spec.Repr(name), err)
	}
	return h, ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.Atoi(s)
	return err == nil && !strings.ContainsAny(s, "+- ")
}

func clock(at any) string {
	s := util.Str(at)
	if s == "" {
		return "--:--:--"
	}
	t, err := util.ParseTS(s)
	if err != nil {
		return "--:--:--"
	}
	return t.Local().Format("15:04:05")
}

func progress(h plugin.Harness, runDir string, limit int) (events []util.M, problem string) {
	defer func() {
		if r := recover(); r != nil {
			events, problem = nil, fmt.Sprintf("progress unavailable: %v", r)
		}
	}()
	evs := h.Progress(runDir, "execute", limit)
	if len(evs) > limit {
		evs = evs[len(evs)-limit:]
	}
	return evs, ""
}

func tailTargets(rows []runRow) []runRow {
	var running []runRow
	for _, r := range rows {
		if util.S(util.Get(r.state, "stages", "execute", "status")) == "running" {
			running = append(running, r)
		}
	}
	if len(running) > 0 {
		return running
	}
	return rows
}

func statusTail(sp util.M, rows []runRow, limit int) {
	for _, r := range tailTargets(rows) {
		outf("\n%s: last %d worker events (execute)\n", filepath.Base(r.dir), limit)
		h, why := runHarness(sp, r.dir)
		if h == nil {
			outf("  (%s)\n", why)
			continue
		}
		events, problem := progress(h, r.dir, limit)
		if problem != "" {
			outf("  (%s)\n", problem)
			continue
		}
		if len(events) == 0 {
			outln("  (no worker events recorded)")
		}
		for _, ev := range events {
			kind := util.Clip(util.Str(util.Or(ev["kind"], "")), 5)
			outf("  %s %-5s %s\n", clock(ev["at"]), kind, util.OneLine(util.Str(ev["text"]), 200))
		}
	}
}

func tailEvents(sp util.M, r runRow, limit int) any {
	h, why := runHarness(sp, r.dir)
	if h == nil {
		return util.M{"unavailable": why}
	}
	events, problem := progress(h, r.dir, limit)
	if problem != "" {
		return util.M{"unavailable": problem}
	}
	out := []any{}
	for _, ev := range events {
		out = append(out, util.M{"at": ev["at"], "kind": ev["kind"], "text": ev["text"]})
	}
	return out
}
