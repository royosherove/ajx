package console

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/royosherove/ajx/internal/runner"
	"github.com/royosherove/ajx/internal/util"
)

// onLaunch receives a command the caller launched (the server reaps its children).
type onLaunch func(cmd *exec.Cmd, entry M)

func asConsoleError(err error) (*Error, bool) {
	var ce *Error
	ok := errors.As(err, &ce)
	return ce, ok
}

func ensureIdle(trial string, entries []M) (M, error) {
	sp, _ := load(trial)
	if entries == nil {
		entries, _ = readEntries(trial, 0)
	}
	if job := activeJob(trial, entries, outputDir(trial, sp)); job != nil {
		cmd := util.OrStr(pstr(job["command"]), "a command")
		return nil, Conflict("%s is still running (pid %s)", cmd, py(job["pid"]))
	}
	return sp, nil
}

func planMismatch(plan *string, current string) (string, bool) {
	if plan == nil {
		return "", false
	}
	given := strings.ToLower(strings.TrimSpace(*plan))
	if util.RuneLen(given) < 7 || !strings.HasPrefix(current, given) {
		shown := util.Clip(given, 12)
		if shown == "" {
			shown = "nothing"
		}
		return shown, true
	}
	return "", false
}

// PreflightStart runs the checks that need no waiting; the caller then checks
// readiness and launches. plan may be the full plan id or a prefix of at least
// 7 characters (as `since` prints it); an empty plan is refused as a mismatch.
func PreflightStart(trial, plan string) error {
	_, _, err := preflightStart(trial, &plan)
	return err
}

func preflightStart(trial string, plan *string) (M, string, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, "", err
	}
	sp, loadErr := load(trial)
	if loadErr != "" {
		return nil, "", Errorf("the trial file has an error: %s", loadErr)
	}
	info, err := PlanInfo(trial, sp)
	if err != nil {
		return nil, "", err
	}
	current := util.S(info["sha"])
	if shown, bad := planMismatch(plan, current); bad {
		return nil, "", Conflict("plan mismatch: you passed %s, the current plan is %s. "+
			"The trial file, task, starting files or plugins changed after review, or the id is mistyped. "+
			"`ajx console since` shows the current plan and what changed.", shown, util.Clip(current, 12))
	}
	if info["complete"] != true {
		return nil, "", Errorf("the starting files or the trial's plugins are too large to fingerprint for approval " +
			"(over 2000 files or 64 MB); start this trial with `ajx run` instead")
	}
	if _, err := ensureIdle(trial, nil); err != nil {
		return nil, "", err
	}
	out := outputDir(trial, sp)
	if blocked := outputView(sp, out, runViews(sp, out, 0)); blocked["blocker"] != nil {
		return nil, "", Errorf("%s", py(blocked["blocker"]))
	}
	return sp, current, nil
}

// Start checks readiness when it is missing for this exact plan, records the
// approval and launches `ajx run --expect-plan <plan>`. The run itself refuses
// to start if the plan changes after approval. On a task that acts in an
// account, readiness warnings must be accepted explicitly. Returns the
// job_started journal entry.
func Start(trial string, o StartOpts) (M, error) {
	plan := o.Plan
	return start(trial, o, &plan, "", nil)
}

func start(trial string, o StartOpts, plan *string, again string, launched onLaunch) (M, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	cmd, entry, err := startInner(trial, o, plan, again)
	if err != nil {
		if _, ok := asConsoleError(err); ok {
			record(trial, o.Actor, "start_blocked", M{"reason": clip(err.Error(), 300)})
		}
		return nil, err
	}
	if launched != nil {
		launched(cmd, entry)
	}
	return entry, nil
}

func startInner(trial string, o StartOpts, plan *string, again string) (*exec.Cmd, M, error) {
	_, current, err := preflightStart(trial, plan)
	if err != nil {
		return nil, nil, err
	}
	ready := readiness(trial, current)
	if !(ready["current"] == true && ready["status"] == "ready") {
		if ready, err = check(trial, o.Actor); err != nil {
			return nil, nil, err
		}
	}
	if ready["status"] != "ready" {
		return nil, nil, Errorf("not ready; the readiness check lists what to fix")
	}
	warnings := util.List(ready["warnings"])
	if util.Truthy(ready["needs_ack"]) && len(warnings) > 0 && !o.AcceptWarnings {
		var shown []string
		for i, w := range warnings {
			if i >= 3 {
				break
			}
			shown = append(shown, clip(w, 200))
		}
		return nil, nil, Errorf("the readiness check passed with warnings that need your acceptance before this task "+
			"acts in an account: %s", strings.Join(shown, " | "))
	}
	sp, _ := load(trial)
	if planSha(trial, sp) != current {
		return nil, nil, Conflict("the plan changed during the readiness check; review it and start again")
	}
	entries, _ := readEntries(trial, 0)
	if approval(entries, current)["state"] != "valid" {
		fields := M{"plan": current, "via": o.Via}
		if o.Quote != "" {
			fields["quote"] = clip(o.Quote, 500)
		}
		if again != "" {
			fields["again"] = again
		}
		if o.AcceptWarnings {
			accepted := []any{}
			for _, w := range warnings {
				accepted = append(accepted, clip(w, 300))
			}
			fields["accepted_warnings"] = accepted
		}
		record(trial, o.Actor, "approved", fields)
	}
	cmd, entry, err := launch(trial, "run", []string{"run", trial, "--expect-plan", current}, o.Actor,
		M{"plan": util.Clip(current, 12), "via": o.Via})
	if err != nil {
		return nil, nil, err
	}
	return cmd, entry, nil
}

// RunAgain starts a new attempt of the same task: "same-folder" adds one
// attempt to this output folder (the settings must be unchanged, so the
// attempts stay comparable); "fresh-folder" points output_dir at a new, empty
// folder. Finished and stopped attempts are kept, never run again.
func RunAgain(trial string, o AgainOpts) (M, error) {
	plan := o.Plan
	return runAgain(trial, o, &plan, nil)
}

func runAgain(trial string, o AgainOpts, plan *string, launched onLaunch) (M, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	if !util.Contains(AgainModes, o.Mode) {
		return nil, Errorf("run again needs a mode: %s", strings.Join(AgainModes, " or "))
	}
	sp, loadErr := load(trial)
	if loadErr != "" {
		return nil, Errorf("the trial file has an error: %s", loadErr)
	}
	current := planSha(trial, sp)
	if shown, bad := planMismatch(plan, current); bad {
		return nil, Conflict("plan mismatch: you passed %s, the current plan is %s; "+
			"`ajx console since` shows what changed", shown, util.Clip(current, 12))
	}
	if _, err := ensureIdle(trial, nil); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(trial)
	if err != nil {
		return nil, err
	}
	baseSha := sha256Hex(data)
	if o.Mode == "same-folder" {
		out := outputDir(trial, sp)
		view := outputView(sp, out, runViews(sp, out, 0))
		if view["blocker_kind"] == "changed" {
			return nil, Errorf("%s", py(view["blocker"]))
		}
		if _, err := addAttempt(trial, baseSha, o.Actor); err != nil {
			return nil, err
		}
	} else if _, err := freshOutput(trial, baseSha, o.Actor); err != nil {
		return nil, err
	}
	record(trial, o.Actor, "run_again", M{"mode": o.Mode, "from_plan": util.Clip(current, 12)})
	return start(trial, StartOpts{Actor: o.Actor, Via: o.Via, Quote: o.Quote, AcceptWarnings: o.AcceptWarnings},
		nil, o.Mode, launched)
}

// Stop sends SIGINT to the running `ajx run`: AJX stops the worker, runs task
// cleanup, releases the environment and archives the evidence. Returns M{pid}.
func Stop(trial, actor string) (M, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	sp, _ := load(trial)
	entries, _ := readEntries(trial, 0)
	job := activeJob(trial, entries, outputDir(trial, sp))
	if job == nil || (job["job"] != "run" && job["job"] != "stages") {
		return nil, Errorf("no run is in progress")
	}
	pid, _ := intOf(job["pid"])
	if l := last(entries, "stop_requested"); l != nil {
		if lp, _ := intOf(l["pid"]); lp == pid && eq(l["job_n"], job["n"]) {
			return nil, Conflict("already stopping: AJX is stopping the worker and cleaning up, and a second signal could cut " +
				"that cleanup short")
		}
	}
	parallel := util.Int(util.Get(sp, "trial", "parallel"), 1)
	if job["job"] == "run" && parallel > 1 {
		return nil, Errorf("Stop is unavailable when trial.parallel is above 1: AJX would stop only after every " +
			"running attempt ends, which can take until their time limits")
	}
	// SIGINT takes the runner's handled interruption: stop the worker, tear down, release, archive.
	if err := syscall.Kill(pid, syscall.SIGINT); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil, Errorf("the run already ended")
		}
		return nil, err
	}
	record(trial, actor, "stop_requested", M{"pid": pid, "job_n": job["n"]})
	return M{"pid": pid}, nil
}

var runIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*-r[0-9]+$`)
var runSuffixRe = regexp.MustCompile(`-r[0-9]+$`)

func runStages(trial, actor string, runID, stageSet any, launched onLaunch) (M, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	set, isStr := stageSet.(string)
	if _, ok := StageSets[set]; !ok || !isStr {
		return nil, Errorf("unsupported stage set %s", pyRepr(stageSet))
	}
	sp, err := ensureIdle(trial, nil)
	if err != nil {
		return nil, err
	}
	out := outputDir(trial, sp)
	rid := pstr(runID)
	if _, isS := runID.(string); !isS || !runIDRe.MatchString(rid) || !isFile(filepath.Join(out, "runs", rid, "state.json")) {
		return nil, Errorf("run %s not found", pyRepr(runID))
	}
	cell := runSuffixRe.ReplaceAllString(rid, "")
	cmd, entry, err := launch(trial, "stages", []string{"run", trial, "--cells", cell, "--stages", set}, actor,
		M{"run": rid, "stages": set})
	if err != nil {
		return nil, err
	}
	if launched != nil {
		launched(cmd, entry)
	}
	return entry, nil
}

func rebuildReports(trial, actor string, launched onLaunch) (M, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	if _, err := ensureIdle(trial, nil); err != nil {
		return nil, err
	}
	cmd, entry, err := launch(trial, "report", []string{"report", trial, "--no-synthesis"}, actor, nil)
	if err != nil {
		return nil, err
	}
	if launched != nil {
		launched(cmd, entry)
	}
	return entry, nil
}

func exportRun(trial, actor string, runID any) (M, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	sp, _ := load(trial)
	out := outputDir(trial, sp)
	rid, isStr := runID.(string)
	if !isStr || !runIDRe.MatchString(rid) {
		return nil, Errorf("export needs a run id")
	}
	res := runCLI([]string{"export", trial, "--run", rid, "--force"}, 120*time.Second, filepath.Dir(trial))
	if c, ok := intOf(res["exit_code"]); res["exit_code"] == nil || !ok || c != 0 {
		return nil, Errorf("%s", util.OrStr(strings.TrimSpace(util.S(res["output"])), "export failed"))
	}
	dest := filepath.Join(out, "share", rid)
	record(trial, actor, "exported", M{"run": rid})
	files := []any{}
	for _, n := range []string{"report.html", "journey.html", "asks.md"} {
		if isFile(filepath.Join(dest, n)) {
			files = append(files, "share/"+rid+"/"+n)
		}
	}
	return M{"output": res["output"], "dest": Short(dest), "files": files}, nil
}

func message(trial string, text, about any) (M, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	t := strings.TrimSpace(pstr(text))
	if !util.Truthy(text) {
		t = ""
	}
	if t == "" {
		return nil, Errorf("write a message first")
	}
	fields := M{"text": clip(t, maxText)}
	if util.Truthy(about) {
		fields["about"] = clip(about, 120)
	}
	return record(trial, "user", "message", fields), nil
}

func pyRepr(v any) string { return runner.PyRepr(v) }
