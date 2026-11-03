package runner

import (
	"path/filepath"

	"github.com/royosherove/ajx/internal/util"
)

func (r *Run) processRecordPath() string {
	return filepath.Join(r.RunDir, "execute.process.json")
}

func (r *Run) workerStopped(status, reason string) error {
	r.State["worker_cleanup"] = M{"status": status, "confirmed": true, "reason": reason}
	if ex := r.execState(); ex != nil {
		ex["reaped"] = true
	}
	return r.save()
}

func (r *Run) workerUnconfirmed(reason string) error {
	r.State["worker_cleanup"] = M{"status": "unconfirmed", "confirmed": false, "reason": reason}
	if ex := r.execState(); ex != nil {
		ex["reaped"] = false
	}
	// A previous task-cleanup result alone cannot confirm worker termination.
	r.State["teardown"] = M{"status": "skipped", "reason": "worker termination is unconfirmed"}
	if err := r.save(); err != nil {
		return err
	}
	return &ProcessRecoveryError{RunID: r.RunID, Reason: reason}
}

func (r *Run) ensureWorkerStopped() error {
	if util.Get(r.State, "worker_cleanup", "confirmed") == true {
		return nil
	}
	ex := r.execState()
	if ex == nil && r.Status("execute") == "" {
		return nil // The task was never attempted.
	}
	if util.S(ex["process_record"]) != "" {
		// The run may have moved; the record lives beside its current state.
		if err := util.StopRecordedProcess(r.processRecordPath()); err != nil {
			return r.workerUnconfirmed(err.Error())
		}
		if r.EnvironmentProfile != nil && util.S(r.EnvironmentProfile["backend"]) != "local" {
			// A docker client exiting does not stop docker-exec. The managed
			// backend authenticates its saved container identity before acting.
			mr, err := r.needManaged()
			if err != nil {
				return r.workerUnconfirmed(err.Error())
			}
			result := mr.Abort(r.Ctx())
			ex["environment_abort"] = result
			if result["confirmed"] != true {
				return r.workerUnconfirmed("the environment did not confirm that its workers stopped")
			}
		}
		return r.workerStopped("stopped", "recorded worker process group has no live members")
	}
	// Old records cannot authorize a signal. An absent recorded group can
	// establish termination, but a live/reused PID must be left alone.
	if pid := util.Int(ex["pid"], 0); pid > 1 {
		active, err := util.ProcessGroupActive(pid)
		if err == nil && !active {
			return r.workerStopped("stopped", "legacy recorded process group has no live members; no signal sent")
		}
	}
	return r.workerUnconfirmed("launch identity is missing; the original worker cannot be safely identified")
}

func (r *Run) setupStarted() bool {
	return util.Truthy(r.State["setup_started"])
}

func (r *Run) cleanupFailedPrepare(keep bool) error {
	if r.setupStarted() && (r.Status("teardown") != "done" || util.Get(r.State, "teardown", "status") != "ok") {
		if err := r.safe("teardown"); err != nil {
			return err
		}
	}
	if !keep && !r.done("archive") {
		return r.safe("archive")
	}
	return nil
}
