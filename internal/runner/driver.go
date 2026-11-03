package runner

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

// interruptSignals subscribes to the signals that interrupt one cleanup step
// at a time while interruption cleanup runs (tests replace it).
var interruptSignals = func() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	return ch, func() { signal.Stop(ch) }
}

// stepWatcher gives each interruption-cleanup step a context that the next
// signal cancels, like Python's KeyboardInterrupt landing in one cleanup step.
type stepWatcher struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	last   string
	stop   func()
	done   chan struct{}
}

func newStepWatcher() *stepWatcher {
	ch, stop := interruptSignals()
	w := &stepWatcher{stop: stop, done: make(chan struct{})}
	go func() {
		for {
			select {
			case sig := <-ch:
				w.mu.Lock()
				w.last = signalText(sig)
				if w.cancel != nil {
					w.cancel()
				}
				w.mu.Unlock()
			case <-w.done:
				return
			}
		}
	}()
	return w
}

func signalText(sig os.Signal) string {
	if s, ok := sig.(syscall.Signal); ok {
		if s == syscall.SIGINT {
			return "" // signal.default_int_handler raises KeyboardInterrupt()
		}
		return fmt.Sprintf("signal %d", int(s))
	}
	return ""
}

func (w *stepWatcher) step() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	w.mu.Lock()
	w.cancel = cancel
	w.mu.Unlock()
	return ctx, func() {
		w.mu.Lock()
		w.cancel = nil
		w.mu.Unlock()
		cancel()
	}
}

func (w *stepWatcher) lastSignal() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

func (w *stepWatcher) close() {
	w.stop()
	close(w.done)
}

// cleanupInterrupted tries each cleanup step once; keeps a failed release
// recoverable and the interruption intact.
func (r *Run) cleanupInterrupted(stage string, keepWorkspace bool) error {
	cleanup := M{"stage": stage, "at": util.Now(), "steps": M{}}
	r.State["interruption_cleanup"] = cleanup
	steps := cleanup["steps"].(M)
	watcher := newStepWatcher()
	defer watcher.close()
	outer := r.goctx
	defer func() { r.goctx = outer }()
	var saveErr error

	attempt := func(name string, action func() error) {
		tracked := util.Contains(Stages, name) && name != stage
		stepCtx, done := watcher.step()
		r.goctx = stepCtx
		err := func() error {
			if tracked {
				if err := r.mark(name, "running", nil); err != nil {
					return err
				}
			}
			return action()
		}()
		interrupted := stepCtx.Err() != nil
		done()
		r.goctx = outer
		var result M
		switch {
		case err == nil && !interrupted:
			result = M{"status": "done"}
		case util.Interrupted(err) || interrupted:
			result = M{"status": "interrupted", "error": "KeyboardInterrupt: " + watcher.lastSignal()}
		default:
			result = M{"status": "error", "error": pyErrString(err)}
		}
		steps[name] = result
		var serr error
		if tracked {
			extra := util.Copy(result)
			delete(extra, "status")
			serr = r.mark(name, util.S(result["status"]), extra)
		} else {
			serr = r.save()
		}
		if serr != nil && saveErr == nil {
			saveErr = serr
		}
	}

	stopProcesses := func() error {
		execution := r.execState()
		if execution == nil {
			return nil
		}
		if pid := util.Int(execution["pid"], 0); pid != 0 && !util.Truthy(execution["reaped"]) {
			util.KillGroup(pid)
			execution["reaped"] = true
			return r.save()
		}
		return nil
	}
	released := func() bool {
		return r.EnvironmentProfile == nil || !util.Truthy(r.State["environment"]) || cleanupConfirmed(r.State)
	}

	// Harness.Run reaps an interrupted launch and cancels container workers.
	// A completed execution can still have descendants retained for verification.
	attempt("stop_processes", stopProcesses)
	if r.done("prepare") && isTerminal(r.Status("execute")) && !r.done("teardown") && stage != "teardown" {
		attempt("teardown", func() error { return r.callStage("teardown") })
	}
	if !released() {
		attempt("release", func() error { return r.callStage("release") })
	}
	if !keepWorkspace && util.Truthy(r.State["paths"]) {
		if released() {
			attempt("archive", func() error { return r.callStage("archive") })
		} else if stage != "archive" {
			if err := r.mark("archive", "error", M{"error": "environment release was not confirmed; " +
				"retaining original owned paths for cleanup retry"}); err != nil && saveErr == nil {
				saveErr = err
			}
		}
	}
	if !r.done("archive") {
		// Failed release and --keep-workspace retain evidence roots, never copied auth.
		attempt("credentials", r.purgeCredentials)
	}
	return saveErr
}

func (r *Run) detectInterrupted() error {
	if r.EnvironmentProfile != nil && r.Status("prepare") == "running" {
		if err := r.mark("prepare", "interrupted", M{"note": "ajx stopped during preparation; keeping the original " +
			"owned paths and environment for cleanup without replay"}); err != nil {
			return err
		}
		r.Log(fmt.Sprintf("[%s] prepare was interrupted; cleaning up the recorded attempt", r.RunID))
	}
	if r.Status("execute") == "running" {
		ex := util.Copy(r.execState())
		ex["stop_reason"] = "interrupted"
		ex["interrupted"] = true
		r.State["execute"] = ex
		if err := r.mark("execute", "interrupted", M{"note": "ajx was not running when this run was resumed; the worker " +
			"may have been killed or finished unobserved"}); err != nil {
			return err
		}
		r.Log(fmt.Sprintf("[%s] execute was interrupted; keeping its evidence, not re-running the task", r.RunID))
	}
	return nil
}

func (r *Run) ready(stage string) bool {
	if stage == "execute" {
		return r.done("prepare")
	}
	for _, p := range Prereqs[stage] {
		if !isTerminal(r.Status(p)) {
			return false
		}
	}
	return true
}

func (r *Run) skip(stage string) bool {
	st := r.Status(stage)
	if stage == "prepare" && r.EnvironmentProfile != nil && isTerminal(st) {
		return true // A failed owned attempt is evidence, not permission to allocate a replacement.
	}
	if st == "interrupted" && util.Contains(RedoWhenInterrupted, stage) {
		return false // e.g. Stop during reporting: finish the report instead of leaving it half written
	}
	return st == "done" || st == "interrupted" || (stage == "execute" && st == "error")
}

// Go runs the wanted stages of this attempt (Python Run.go). ctx cancellation
// is an interruption: the worker is stopped, cleanup runs, and the returned
// error satisfies util.Interrupted.
func (r *Run) Go(ctx context.Context, stages []string, keepWorkspace bool) (M, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	r.goctx = ctx
	if util.Truthy(r.Spec["_cleanup_only"]) && !CleanupOnly(stages) {
		return r.State, spec.Errorf("saved cleanup profiles allow only teardown, release, render and archive")
	}
	// A release-only request explicitly disposes of the environment. A teardown retry
	// that also names release must still retain the environment if that retry fails.
	r.explicitRelease = util.Contains(stages, "release") && !util.Contains(stages, "teardown")
	if err := r.detectInterrupted(); err != nil {
		return r.State, err
	}
	if len(stages) > 0 {
		var unknown []string
		for _, s := range stages {
			if !util.Contains(Stages, s) {
				unknown = append(unknown, s)
			}
		}
		if len(unknown) > 0 {
			return r.State, valueErr("unknown stage(s) %s; stages are %s", pyList(unknown), pyList(Stages))
		}
	}
	if r.EnvironmentProfile != nil && r.Status("prepare") == "interrupted" {
		// As with failed preparation, release before archiving and purging credentials.
		// Retry unconfirmed cleanup against the same paths, even with --stages prepare.
		if !r.done("archive") {
			if keepWorkspace {
				if err := r.cleanupInterrupted("prepare", true); err != nil {
					return r.State, err
				}
			} else if err := r.safe("archive"); err != nil {
				return r.State, err
			}
		}
		return r.State, nil
	}
	if len(stages) > 0 {
		var blocked []string
		for _, s := range stages {
			if util.Contains(NotRedoable, s) && isTerminal(r.Status(s)) {
				blocked = append(blocked, s)
			}
		}
		if len(blocked) > 0 {
			r.Log(fmt.Sprintf("[%s] refusing to redo %s: a task runs once per repetition; add a "+
				"repetition or delete %s", r.RunID, pyList(blocked), r.RunDir))
			return r.State, nil
		}
		if util.Truthy(r.State["archived"]) {
			var late, keep []string
			for _, s := range stages {
				if util.Contains(NotRedoableAfterArchive, s) {
					late = append(late, s)
				} else {
					keep = append(keep, s)
				}
			}
			if len(late) > 0 {
				r.Log(fmt.Sprintf("[%s] refusing to redo %s after archive: the session can no longer be "+
					"resumed from its original workspace; the existing journey is kept", r.RunID, pyList(late)))
				stages = keep
			}
		} else if util.Truthy(r.State["environment_cleanup"]) && util.Contains(stages, "narrate") {
			r.Log(fmt.Sprintf("[%s] refusing to resume narration after environment release; "+
				"keeping the existing journey", r.RunID))
			var keep []string
			for _, s := range stages {
				if s != "narrate" {
					keep = append(keep, s)
				}
			}
			stages = keep
		}
		for _, s := range stages { // explicitly requested post-processing stages are redone
			delete(r.stages(), s)
		}
		if err := r.save(); err != nil {
			return r.State, err
		}
	}
	var wanted []string
	for _, s := range Stages {
		if len(stages) == 0 || util.Contains(stages, s) {
			wanted = append(wanted, s)
		}
	}
	if keepWorkspace {
		var keep []string
		for _, s := range wanted {
			if s != "archive" {
				keep = append(keep, s)
			}
		}
		wanted = keep
	}
	if isTerminal(r.Status("execute")) && !r.done("teardown") && !util.Contains(wanted, "teardown") {
		wanted = append(wanted, "teardown") // a crash between execute and teardown must still tear down
	}
	for _, stage := range Stages {
		if !util.Contains(wanted, stage) || r.skip(stage) {
			continue
		}
		if stage == "archive" && !util.Truthy(r.State["paths"]) {
			continue
		}
		if !r.ready(stage) {
			r.Log(fmt.Sprintf("[%s] skip %s: prerequisites %s not reached", r.RunID, stage, pyTuple(Prereqs[stage])))
			continue
		}
		var err error
		if r.interrupted() {
			// The interruption landed between stages: Python marks the next stage interrupted.
			err = util.ErrInterrupted
		} else if err = r.mark(stage, "running", nil); err == nil {
			err = r.callStage(stage)
			if err == nil && !r.interrupted() {
				err = r.mark(stage, "done", nil)
			}
		}
		if err == nil && !r.interrupted() {
			continue
		}
		if util.Interrupted(err) || r.interrupted() {
			return r.State, r.handleInterrupt(stage, keepWorkspace)
		}
		_ = r.mark(stage, "error", M{"error": pyErrString(err), "trace": traceText(err)})
		r.Log(fmt.Sprintf("[%s] %s failed: %s", r.RunID, stage, pyMessage(err)))
		if stage == "prepare" {
			if serr := r.safe("archive"); serr != nil {
				return r.State, serr
			}
			break
		}
	}
	return r.State, nil
}

// handleInterrupt is Python's `except BaseException` branch of Run.go.
func (r *Run) handleInterrupt(stage string, keepWorkspace bool) error {
	ie := &interruptError{}
	if stage == "execute" {
		execution := util.Copy(r.execState())
		execution["stop_reason"] = "interrupted"
		execution["interrupted"] = true
		if !util.Truthy(execution["stopped_at"]) {
			execution["stopped_at"] = util.Now()
		}
		r.State["execute"] = execution
	}
	merr := r.mark(stage, "interrupted", M{"note": "execution interrupted; keeping captured evidence"})
	cerr := r.cleanupInterrupted(stage, keepWorkspace)
	if merr != nil || cerr != nil {
		bad := merr
		if bad == nil {
			bad = cerr
		}
		ie.notes = append(ie.notes, "AJX interruption cleanup also failed: "+pyTypeName(bad))
	}
	return ie
}

// traceText stands in for Python's traceback.format_exc()[-3000:] (the Go
// CLI has no Python traceback; the last line has the same form).
func traceText(err error) string {
	return util.Tail("Traceback: not available in the Go CLI\n"+pyErrString(err)+"\n", 3000)
}

// safe runs one stage, recording a failure without raising (Python _safe).
func (r *Run) safe(stage string) error {
	if err := r.mark(stage, "running", nil); err != nil {
		return err
	}
	if err := r.callStage(stage); err != nil {
		if util.Interrupted(err) || r.interrupted() {
			return util.ErrInterrupted // Python's _safe catches Exception only
		}
		return r.mark(stage, "error", M{"error": pyMessage(err)})
	}
	return r.mark(stage, "done", nil)
}
