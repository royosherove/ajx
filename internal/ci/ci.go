// Package ci is `ajx ci`: a headless evaluation for pipelines. It validates
// the trial, checks readiness (doctor), runs the planned matrix in a fresh
// output folder, and decides passed, failed or inconclusive from the
// recorded evidence (verify.json, state.json, cleanup records), writing
// ci-result.json and junit.xml. It never waits for input.
//
// The decision uses the deterministic task checks only: an empty asks list
// or a zero worker exit code is never treated as success.
package ci

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	ajx "github.com/royosherove/ajx"
	"github.com/royosherove/ajx/internal/console"
	"github.com/royosherove/ajx/internal/runner"
	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

// SchemaVersion of ci-result.json.
const SchemaVersion = 1

// Decisions.
const (
	Passed       = "passed"
	Failed       = "failed"
	Inconclusive = "inconclusive"
)

// Exit codes.
const (
	ExitPassed       = 0
	ExitFailed       = 1
	ExitInvalid      = 2
	ExitLocked       = 3
	ExitInconclusive = 4
	ExitInterrupted  = 130
)

// Reason severities: a failure fails the decision, an inconclusive reason
// makes it inconclusive unless something already failed, a warning is
// recorded only.
const (
	SevFailure      = "failure"
	SevInconclusive = "inconclusive"
	SevWarning      = "warning"
)

// Options configure one evaluation.
type Options struct {
	Trial          string
	Output         string // overrides trial.output_dir
	JUnit          string // default <output>/junit.xml
	Result         string // default <output>/ci-result.json
	Policy         string
	MinSuccessRate *float64
	RequireCleanup bool
	AllowReuse     bool
	Cells          []string
	NoSynthesis    bool
	SkipDoctor     bool
	Log            func(string) // run progress; nil = discard
	Say            func(string) // ci summary lines; nil = discard
}

// Hooks are the CLI commands the evaluation reuses (so `ajx ci` checks
// exactly what `ajx validate` and `ajx doctor` check).
type Hooks struct {
	Validate func(trial string, cells []string) (lines []string, doc util.M, err error)
	Doctor   func(trial string) (code int, lines []string, err error)
	Run      func(ctx context.Context, sp util.M, o runner.MatrixOpts) (util.M, error)
}

// Session is one `ajx ci` evaluation.
type Session struct {
	o      Options
	h      Hooks
	mu     sync.Mutex
	result util.M
	paths  struct{ result, junit, out string }
	sp     util.M
	policy Policy
	plan   []spec.PlanItem
	done   bool
}

// New creates a session.
func New(o Options, h Hooks) *Session {
	if o.Log == nil {
		o.Log = func(string) {}
	}
	if o.Say == nil {
		o.Say = func(string) {}
	}
	s := &Session{o: o, h: h}
	s.result = util.M{
		"schema_version": SchemaVersion,
		"tool":           util.M{"name": "ajx", "implementation": "go", "version": ajx.Version},
		"started_at":     util.Now(),
		"decision":       Inconclusive,
		"reasons":        []any{},
		"runs":           []any{},
		"cells":          []any{},
		"exit_code":      ExitInconclusive,
		"artifacts":      util.M{},
	}
	return s
}

func reason(code, sev, msg string, extra util.M) util.M {
	r := util.M{"code": code, "severity": sev, "message": msg}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

// Run performs the evaluation and returns the exit code.
func (s *Session) Run(ctx context.Context) int {
	code := s.run(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = true
	return code
}

func (s *Session) finish(decision string, code int, reasons ...util.M) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return util.Int(s.result["exit_code"], code)
	}
	s.result["decision"] = decision
	s.result["exit_code"] = code
	list := util.List(s.result["reasons"])
	for _, r := range reasons {
		list = append(list, r)
	}
	s.result["reasons"] = list
	s.result["finished_at"] = util.Now()
	s.writeLocked()
	return code
}

// Abort records an interruption that outlived the grace period and writes
// the result now; safe to call from a signal goroutine.
func (s *Session) Abort(graceSeconds int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return util.Int(s.result["exit_code"], ExitInterrupted)
	}
	s.done = true
	s.result["decision"] = Inconclusive
	s.result["exit_code"] = ExitInterrupted
	s.result["reasons"] = append(util.List(s.result["reasons"]),
		reason("interrupted", SevInconclusive, fmt.Sprintf("cancelled; cleanup had not finished %ds after the signal, so ajx ci exited without waiting", graceSeconds), nil),
		reason("cleanup_not_confirmed", SevInconclusive, "cleanup was still running when ajx ci exited; check the output folder and rerun the same ajx command to finish it", nil))
	s.result["finished_at"] = util.Now()
	s.writeLocked()
	return ExitInterrupted
}

func (s *Session) writeLocked() {
	if s.paths.result != "" {
		if err := util.WriteJSON(s.paths.result, s.result); err != nil {
			s.o.Say("ci: cannot write " + s.paths.result + ": " + err.Error())
		}
	}
	if s.paths.junit != "" {
		if err := os.MkdirAll(filepath.Dir(s.paths.junit), 0o755); err == nil {
			if err := util.WriteFileAtomic(s.paths.junit, []byte(JUnit(s.result)), 0o644); err != nil {
				s.o.Say("ci: cannot write " + s.paths.junit + ": " + err.Error())
			}
		}
	}
}

func abs(p string) string {
	if p == "" {
		return ""
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// Result returns a copy of the current result document.
func (s *Session) Result() util.M {
	s.mu.Lock()
	defer s.mu.Unlock()
	return util.DeepCopy(s.result).(util.M)
}

// Paths returns where the result and JUnit files are written ("" = not written).
func (s *Session) Paths() (result, junit string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paths.result, s.paths.junit
}

func (s *Session) set(key string, v any) {
	s.mu.Lock()
	s.result[key] = v
	s.mu.Unlock()
}

func hasAttempts(out string) bool {
	matches, _ := filepath.Glob(filepath.Join(out, "runs", "*", "state.json"))
	return len(matches) > 0
}

func (s *Session) run(ctx context.Context) int {
	o := s.o
	// Where the result goes is known before anything can fail.
	s.paths.result, s.paths.junit = abs(o.Result), abs(o.JUnit)
	if o.Output != "" {
		if s.paths.result == "" {
			s.paths.result = filepath.Join(abs(o.Output), "ci-result.json")
		}
		if s.paths.junit == "" {
			s.paths.junit = filepath.Join(abs(o.Output), "junit.xml")
		}
	}
	pol, err := LoadPolicy(o.Policy)
	if err != nil {
		return s.finish(Inconclusive, ExitInvalid, reason("invalid_config", SevInconclusive, "acceptance policy: "+err.Error(), nil))
	}
	if o.MinSuccessRate != nil {
		if *o.MinSuccessRate < 0 || *o.MinSuccessRate > 1 {
			return s.finish(Inconclusive, ExitInvalid, reason("invalid_config", SevInconclusive, "--min-success-rate must be between 0 and 1", nil))
		}
		pol.MinSuccessRate = *o.MinSuccessRate
	}
	if o.RequireCleanup {
		pol.RequireCleanup = true
	}
	s.policy = pol
	s.set("policy", pol.Doc())

	lines, vdoc, err := s.h.Validate(o.Trial, o.Cells)
	if err != nil {
		var se *spec.Error
		msg := err.Error()
		if !errors.As(err, &se) {
			msg = "validate: " + msg
		}
		return s.finish(Inconclusive, ExitInvalid, reason("invalid_config", SevInconclusive, msg, nil))
	}
	sp, err := spec.Load(o.Trial, spec.LoadOpts{})
	if err != nil {
		return s.finish(Inconclusive, ExitInvalid, reason("invalid_config", SevInconclusive, err.Error(), nil))
	}
	s.sp = sp
	out := abs(o.Output)
	if out == "" {
		out = util.S(util.Get(sp, "trial", "output_dir"))
	}
	util.MapOr(sp["trial"])["output_dir"] = out
	s.paths.out = out
	s.mu.Lock()
	if s.paths.result == "" {
		s.paths.result = filepath.Join(out, "ci-result.json")
	}
	if s.paths.junit == "" {
		s.paths.junit = filepath.Join(out, "junit.xml")
	}
	s.mu.Unlock()
	plan, err := spec.RunPlan(sp, o.Cells)
	if err != nil {
		return s.finish(Inconclusive, ExitInvalid, reason("invalid_config", SevInconclusive, err.Error(), nil))
	}
	s.plan = plan
	planSha, _ := console.PlanSha(util.S(sp["path"]), sp)
	t := util.MapOr(sp["trial"])
	task := util.MapOr(sp["task"])
	var planned []any
	cellsInPlan := map[string]bool{}
	for _, it := range plan {
		cellsInPlan[util.Str(it.Cell["id"])] = true
		planned = append(planned, fmt.Sprintf("%s-r%d", util.Str(it.Cell["id"]), it.Rep))
	}
	s.set("trial", util.M{"path": sp["path"], "id": t["id"], "product": t["product"], "sha256": sp["sha256"],
		"prompt_sha256": task["prompt_sha256"]})
	s.set("plan", util.M{"sha256": nilIfEmpty(planSha), "runs": planned, "cells": cellsInOrder(plan),
		"verify_checks": len(util.List(sp["verify"])), "only_cells": strs(o.Cells)})
	s.set("output_dir", out)
	s.set("validate", util.M{"ok": true, "warnings": util.Or(vdoc["warnings"], []any{}), "lines": strs(lines)})
	for _, c := range pol.RequiredCells {
		if !cellsInPlan[c] {
			return s.finish(Inconclusive, ExitInvalid, reason("invalid_config", SevInconclusive,
				fmt.Sprintf("the policy requires cell %s, which this plan does not run", c), util.M{"cell": c}))
		}
	}
	if hasAttempts(out) && !o.AllowReuse {
		return s.finish(Inconclusive, ExitInvalid, reason("output_not_fresh", SevInconclusive,
			fmt.Sprintf("%s already holds attempts; pass a fresh --output for a new evaluation, or --allow-reuse to resume them", out),
			util.M{"output_dir": out}))
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return s.finish(Inconclusive, ExitInvalid, reason("invalid_config", SevInconclusive, err.Error(), nil))
	}
	s.mu.Lock()
	s.result["reasons"] = []any{reason("incomplete", SevInconclusive,
		"the evaluation has not finished; this file is replaced when it does", nil)}
	s.writeLocked()
	s.result["reasons"] = []any{}
	s.mu.Unlock()
	s.o.Say(fmt.Sprintf("ci: validate ok (%d run(s) planned, plan %s)", len(plan), util.Clip(planSha, 12)))

	if ctx.Err() != nil {
		return s.finish(Inconclusive, ExitInterrupted, reason("interrupted", SevInconclusive, "cancelled before the run started", nil))
	}
	code, dlines, err := s.h.Doctor(o.Trial)
	doctorFile := filepath.Join(out, "ci-doctor.txt")
	_ = util.WriteFileAtomic(doctorFile, []byte(strings.Join(dlines, "\n")+"\n"), 0o644)
	s.set("doctor", util.M{"ready": err == nil && code == 0, "exit_code": code, "lines": strs(dlines), "file": doctorFile})
	if err != nil {
		return s.finish(Inconclusive, ExitInvalid, reason("invalid_config", SevInconclusive, "doctor: "+err.Error(), nil))
	}
	if code != 0 {
		if !o.SkipDoctor {
			s.o.Say("ci: doctor reported problems; see " + doctorFile)
			return s.finish(Inconclusive, ExitInconclusive, reason("not_ready", SevInconclusive,
				"ajx doctor reported problems (harness, credentials, identity or preflight); nothing was run", util.M{"file": doctorFile}))
		}
		s.o.Say("ci: doctor reported problems (--skip-doctor: running anyway)")
	} else {
		s.o.Say("ci: doctor ready")
	}

	_, runErr := s.h.Run(ctx, sp, runner.MatrixOpts{OnlyCells: o.Cells, Synthesize: !o.NoSynthesis, Log: o.Log})
	var extra []util.M
	exitOverride := -1
	if runErr != nil {
		var le *runner.LockedError
		var se *spec.Error
		switch {
		case errors.As(runErr, &le):
			return s.finish(Inconclusive, ExitLocked, reason("locked", SevInconclusive, runErr.Error(), nil))
		case errors.As(runErr, &se):
			return s.finish(Inconclusive, ExitInvalid, reason("invalid_config", SevInconclusive, runErr.Error(), nil))
		case util.Interrupted(runErr):
			extra = append(extra, reason("interrupted", SevInconclusive, "the run was cancelled; cleanup ran before ajx ci exited", nil))
			exitOverride = ExitInterrupted
		default:
			extra = append(extra, reason("run_error", SevInconclusive, runErr.Error(), nil))
		}
	}
	decision, code, reasons := s.evaluate()
	reasons = append(extra, reasons...)
	if exitOverride >= 0 {
		decision, code = Inconclusive, exitOverride
	} else if len(extra) > 0 && decision == Passed {
		decision, code = Inconclusive, ExitInconclusive
	}
	return s.finish(decision, code, reasons...)
}

func cellsInOrder(plan []spec.PlanItem) []any {
	seen := map[string]bool{}
	var out []string
	for _, it := range plan {
		id := util.Str(it.Cell["id"])
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return strs(out)
}

func strs(items []string) []any {
	out := make([]any, 0, len(items))
	for _, s := range items {
		out = append(out, s)
	}
	return out
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------------------------------------------------------------- evaluation

// RunRecord is one planned attempt as recorded on disk.
type RunRecord struct {
	ID, Cell     string
	Rep          int
	Dir          string
	Outcome      string // passed | failed | error | missing | interrupted | untrusted | not_run
	Verify       util.M
	StageErrors  []util.M
	Stages       util.M
	Cleanup      string
	CleanupOK    bool
	ReportDone   bool
	Seconds      any
	Harness      any
	Model        any
	VerifyReason string
}

func readRecord(sp util.M, out string, it spec.PlanItem) RunRecord {
	id := fmt.Sprintf("%s-r%d", util.Str(it.Cell["id"]), it.Rep)
	dir := filepath.Join(out, "runs", id)
	r := RunRecord{ID: id, Cell: util.Str(it.Cell["id"]), Rep: it.Rep, Dir: dir, Stages: util.M{},
		Harness: it.Cell["harness"], Model: it.Cell["model"]}
	stPath := filepath.Join(dir, "state.json")
	if _, err := os.Stat(stPath); err != nil {
		r.Outcome = "missing"
		r.Cleanup, r.CleanupOK = "not run", true
		return r
	}
	st := util.ReadMap(stPath)
	stages := util.MapOr(st["stages"])
	interrupted := false
	for _, name := range runner.Stages {
		rec := util.MapOr(stages[name])
		status := util.S(rec["status"])
		r.Stages[name] = nilIfEmpty(status)
		switch status {
		case "error":
			r.StageErrors = append(r.StageErrors, util.M{"stage": name, "error": rec["error"]})
		case "interrupted":
			interrupted = true
		}
	}
	if d := util.SecondsBetween(util.S(util.Get(st, "execute", "started_at")), util.S(util.Get(st, "execute", "stopped_at"))); d != nil {
		r.Seconds = *d
	}
	// cleanup
	switch {
	case len(util.List(sp["teardown"])) == 0:
		r.Cleanup = "none defined"
	case util.Truthy(st["teardown"]):
		r.Cleanup = util.S(util.Get(st, "teardown", "status"))
	case util.S(util.Get(stages, "teardown", "status")) == "done":
		r.Cleanup = "unchecked"
	default:
		r.Cleanup = "not run"
	}
	executeStarted := util.Truthy(stages["execute"])
	r.CleanupOK = r.Cleanup == "ok" || r.Cleanup == "none defined" || (r.Cleanup == "not run" && !executeStarted)
	r.ReportDone = util.S(util.Get(stages, "render", "status")) == "done" && util.S(util.Get(stages, "extract", "status")) == "done"
	verify := util.ReadMap(filepath.Join(dir, "verify.json"))
	r.Verify = verify
	outcome := util.S(verify["outcome"])
	switch {
	case outcome == "succeeded" && len(util.List(verify["checks_with_auth_errors"])) > 0:
		r.Outcome = "untrusted"
	case outcome == "succeeded":
		r.Outcome = "passed"
	case outcome == "failed" || outcome == "partial":
		r.Outcome = "failed"
	case outcome == "not_run":
		r.Outcome = "not_run"
	case interrupted:
		r.Outcome = "interrupted"
	default:
		r.Outcome = "error"
	}
	return r
}

func (r RunRecord) doc() util.M {
	checks := []any{}
	for _, c := range util.Maps(r.Verify["checks"]) {
		checks = append(checks, util.M{"name": c["name"], "type": c["type"], "passed": util.Truthy(c["passed"]),
			"detail": c["detail"]})
	}
	errs := []any{}
	for _, e := range r.StageErrors {
		errs = append(errs, e)
	}
	art := util.M{}
	for key, name := range map[string]string{"report": "report.html", "journey": "pretty-journey.html", "asks": "asks.md",
		"run_json": "run.json", "verify_json": "verify.json", "state_json": "state.json", "measurements": "measurements.json"} {
		if p := filepath.Join(r.Dir, name); fileExists(p) {
			art[key] = p
		}
	}
	return util.M{"run_id": r.ID, "cell": r.Cell, "repetition": r.Rep, "harness": r.Harness, "model": r.Model,
		"outcome": r.Outcome, "stages": r.Stages, "stage_errors": errs, "execute_seconds": r.Seconds,
		"verification": util.M{"outcome": nilIfEmpty(util.S(r.Verify["outcome"])), "passed": r.Verify["passed"],
			"total": r.Verify["total"], "checks": checks,
			"checks_with_auth_errors": util.Or(r.Verify["checks_with_auth_errors"], []any{})},
		"cleanup": util.M{"status": r.Cleanup, "confirmed": r.CleanupOK}, "report_complete": r.ReportDone,
		"artifacts": art}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

var unknownReason = map[string][2]string{
	"missing":     {"run_missing", "no attempt was recorded for this planned run"},
	"error":       {"stage_error", "the attempt stopped with a stage error before verification produced an outcome"},
	"interrupted": {"interrupted", "the attempt was interrupted before verification produced an outcome"},
	"untrusted":   {"verification_untrusted", "the checks passed but printed credential or permission errors"},
	"not_run":     {"verification_missing", "no verification checks ran for this attempt"},
}

func (s *Session) evaluate() (string, int, []util.M) {
	pol := s.policy
	var reasons []util.M
	byCell := map[string][]RunRecord{}
	var cellOrder []string
	var runs []any
	cov := util.M{"planned": len(s.plan), "recorded": 0, "verified": 0, "passed": 0, "failed": 0, "unknown": 0,
		"cleanup_confirmed": 0, "reports_complete": 0}
	inc := func(k string) { cov[k] = util.Int(cov[k], 0) + 1 }
	for _, it := range s.plan {
		r := readRecord(s.sp, s.paths.out, it)
		if _, seen := byCell[r.Cell]; !seen {
			cellOrder = append(cellOrder, r.Cell)
		}
		byCell[r.Cell] = append(byCell[r.Cell], r)
		runs = append(runs, r.doc())
		if r.Outcome != "missing" {
			inc("recorded")
		}
		switch r.Outcome {
		case "passed":
			inc("verified")
			inc("passed")
		case "failed":
			inc("verified")
			inc("failed")
			reasons = append(reasons, reason("verification_failed", SevWarning,
				fmt.Sprintf("%s: %s of %s check(s) passed", r.ID, util.Str(r.Verify["passed"]), util.Str(r.Verify["total"])),
				util.M{"run_id": r.ID, "cell": r.Cell}))
		default:
			inc("unknown")
			why := unknownReason[r.Outcome]
			msg := why[1]
			if len(r.StageErrors) > 0 {
				first := r.StageErrors[0]
				msg += fmt.Sprintf(" (%s: %s)", util.Str(first["stage"]), util.OneLine(util.Str(first["error"]), 300))
			}
			reasons = append(reasons, reason(why[0], SevInconclusive, r.ID+": "+msg, util.M{"run_id": r.ID, "cell": r.Cell}))
		}
		if r.CleanupOK {
			inc("cleanup_confirmed")
		} else {
			sev := SevWarning
			if pol.RequireCleanup {
				sev = SevFailure
			}
			reasons = append(reasons, reason("cleanup_not_confirmed", sev,
				fmt.Sprintf("%s: cleanup is %s; resources the task created may still exist", r.ID, r.Cleanup),
				util.M{"run_id": r.ID, "cell": r.Cell}))
		}
		if r.ReportDone {
			inc("reports_complete")
		} else if r.Outcome != "missing" {
			sev := SevWarning
			if pol.RequireReports {
				sev = SevInconclusive
			}
			reasons = append(reasons, reason("reporting_incomplete", sev,
				r.ID+": the report was not completed (extract or render did not finish)", util.M{"run_id": r.ID, "cell": r.Cell}))
		}
	}
	if len(util.List(s.sp["verify"])) == 0 {
		reasons = append(reasons, reason("no_checks", SevInconclusive,
			"the trial has no [[verify]] checks, so no attempt can show the task was done", nil))
	}
	var cells []any
	for _, cell := range cellOrder {
		rs := byCell[cell]
		n := len(rs)
		passed, failed, unknown := 0, 0, 0
		for _, r := range rs {
			switch r.Outcome {
			case "passed":
				passed++
			case "failed":
				failed++
			default:
				unknown++
			}
		}
		rate := float64(passed) / float64(n)
		best := float64(passed+unknown) / float64(n)
		status := Passed
		switch {
		case best+1e-9 < pol.MinSuccessRate:
			status = Failed
			reasons = append(reasons, reason("success_rate_below_target", SevFailure,
				fmt.Sprintf("cell %s: %d of %d attempt(s) verified succeeded; the target is %s", cell, passed, n, rateText(pol.MinSuccessRate)),
				util.M{"cell": cell}))
		case rate+1e-9 < pol.MinSuccessRate:
			status = Inconclusive
			reasons = append(reasons, reason("insufficient_evidence", SevInconclusive,
				fmt.Sprintf("cell %s: %d of %d attempt(s) verified succeeded and %d have no trustworthy outcome; the target %s cannot be decided",
					cell, passed, n, unknown, rateText(pol.MinSuccessRate)), util.M{"cell": cell}))
		}
		if pol.MinRepetitions > 0 && passed+failed < pol.MinRepetitions {
			if status == Passed {
				status = Inconclusive
			}
			reasons = append(reasons, reason("insufficient_repetitions", SevInconclusive,
				fmt.Sprintf("cell %s: %d verified attempt(s); the policy needs %d", cell, passed+failed, pol.MinRepetitions),
				util.M{"cell": cell}))
		}
		cells = append(cells, util.M{"cell": cell, "planned": n, "passed": passed, "failed": failed, "unknown": unknown,
			"success_rate": util.Round(rate, 4), "target": pol.MinSuccessRate, "status": status})
	}
	if cells == nil {
		cells = []any{}
	}
	if runs == nil {
		runs = []any{}
	}
	s.set("runs", runs)
	s.set("cells", cells)
	s.set("coverage", cov)
	s.set("artifacts", util.M{"output_dir": s.paths.out, "matrix_report": existing(filepath.Join(s.paths.out, "index.html")),
		"matrix_json":     existing(filepath.Join(s.paths.out, "matrix.json")),
		"matrix_markdown": existing(filepath.Join(s.paths.out, "matrix.md")),
		"result":          s.paths.result, "junit": s.paths.junit, "doctor": existing(filepath.Join(s.paths.out, "ci-doctor.txt"))})
	decision, code := Passed, ExitPassed
	for _, r := range reasons {
		switch util.S(r["severity"]) {
		case SevFailure:
			decision, code = Failed, ExitFailed
		case SevInconclusive:
			if decision != Failed {
				decision, code = Inconclusive, ExitInconclusive
			}
		}
	}
	return decision, code, reasons
}

func existing(p string) any {
	if fileExists(p) {
		return p
	}
	return nil
}

func rateText(r float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", r), "0"), ".")
}

// Summary is the one-line decision for logs.
func Summary(result util.M) string {
	var parts []string
	for _, r := range util.Maps(result["reasons"]) {
		if util.S(r["severity"]) != SevWarning {
			parts = append(parts, util.S(r["code"]))
		}
	}
	line := "ci: decision " + util.Str(result["decision"])
	if len(parts) > 0 {
		line += " (" + strings.Join(uniq(parts), ", ") + ")"
	}
	return line
}

func uniq(items []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range items {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
