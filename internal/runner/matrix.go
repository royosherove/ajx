package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/royosherove/ajx/internal/render"
	"github.com/royosherove/ajx/internal/res"
	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

// MatrixOpts are RunMatrix's optional arguments (Python run_matrix keywords).
// Synthesize mirrors Python's synthesize=True default only when set: callers
// pass Synthesize: !noSynthesis.
type MatrixOpts struct {
	OnlyCells     []string
	Stages        []string
	KeepWorkspace bool
	DryRun        bool
	Synthesize    bool
	Log           func(string) // nil = print to stdout
}

// RunMatrix runs (or resumes) every planned attempt of a trial and renders the
// matrix report. It returns matrix-plan.json's content. Cancelling ctx
// interrupts the runs (worker stopped, teardown, release, credential purge,
// archive) and returns an error satisfying util.Interrupted; the lock is
// always released.
func RunMatrix(ctx context.Context, sp M, o MatrixOpts) (M, error) {
	if util.Truthy(sp["_cleanup_only"]) && !isDir(util.S(util.Get(sp, "trial", "output_dir"))) {
		return nil, spec.Errorf("cleanup requires an existing matrix output directory")
	}
	l, err := AcquireMatrixLock(util.S(util.Get(sp, "trial", "output_dir")))
	if err != nil {
		return nil, err
	}
	defer l.Close()
	return RunMatrixLocked(ctx, sp, o, l)
}

// matrixInspection is a read-only snapshot taken while the caller holds the
// output lock. Both validation and execution use this same contract check.
type matrixInspection struct {
	trial, meta, profiles M
	out                   string
	plan                  []spec.PlanItem
	parallel              int
	recovery              bool
}

func inspectMatrix(sp M, o MatrixOpts) (*matrixInspection, error) {
	recovery := util.Truthy(sp["_cleanup_only"])
	if recovery && !CleanupOnly(o.Stages) {
		return nil, spec.Errorf("saved cleanup profiles allow only teardown, release, render and archive")
	}
	trial := util.MapOr(sp["trial"])
	out := util.S(trial["output_dir"])
	if recovery && !isDir(out) {
		return nil, spec.Errorf("cleanup requires an existing matrix output directory")
	}
	var unknown []string
	for _, s := range o.Stages {
		if !util.Contains(Stages, s) {
			unknown = append(unknown, s)
		}
	}
	if len(unknown) > 0 {
		return nil, spec.Errorf("unknown stage(s) %s; stages are %s", pyList(unknown), pyList(Stages))
	}
	plan, err := spec.RunPlan(sp, o.OnlyCells)
	if err != nil {
		return nil, err
	}
	parallel := util.Int(trial["parallel"], 1)
	metaV, err := ReadJSON(filepath.Join(out, "matrix-plan.json"), nil)
	if err != nil {
		return nil, err
	}
	meta := util.MapOr(metaV)
	profiles := M{"schema_version": int64(1), "environments": valueOr(sp, "environments", M{}),
		"agent_configurations": valueOr(sp, "agent_configurations", M{})}
	states, _ := filepath.Glob(filepath.Join(out, "runs", "*", "state.json"))
	hasStates := len(states) > 0
	if !recovery && hasStates {
		saved, recorded := meta["task_inputs"]
		if !recorded {
			return nil, spec.Errorf("the existing attempts have no recorded task-input contract; " +
				"use a new output directory, or request only cleanup stages")
		}
		if !PyEqual(saved, TaskInputs(sp)) {
			return nil, spec.Errorf("verification, setup, preflight, teardown or task inputs changed; " +
				"use a new output directory, or request only cleanup stages to recover the original attempts")
		}
	}
	if !recovery && util.Truthy(meta["normalized_profiles"]) && hasStates {
		// A subset run must not replace the only saved inputs for another cell's cleanup.
		if ProfileContract(util.MapOr(meta["normalized_profiles"])) != ProfileContract(profiles) ||
			!PyEqual(meta["task_prompt_sha256"], util.Get(sp, "task", "prompt_sha256")) {
			return nil, spec.Errorf("matrix profiles or task changed; use a new trial output directory " +
				"or request only cleanup stages to recover the original attempts")
		}
	}
	if !recovery && util.Truthy(meta["cells_contract"]) && hasStates {
		if changed := ContractChanges(util.MapOr(meta["cells_contract"]), CellsContract(sp)); len(changed) > 0 {
			if len(changed) > 6 {
				changed = changed[:6]
			}
			return nil, spec.Errorf("this output folder holds attempts made with other settings (%s); use a new trial "+
				"output directory so the attempts stay comparable, or request only cleanup stages", joinSemi(changed))
		}
	}
	if recovery {
		// Unstarted repetitions own nothing; never create new attempts during recovery.
		var kept []spec.PlanItem
		for _, it := range plan {
			if isFile(filepath.Join(out, "runs", fmt.Sprintf("%s-r%d", util.Str(it.Cell["id"]), it.Rep), "state.json")) {
				kept = append(kept, it)
			}
		}
		plan = kept
		if len(plan) == 0 {
			return nil, spec.Errorf("no recorded attempts were found for cleanup")
		}
	}
	// Refuse the entire explicit replay request before updating provenance or
	// starting any newly added cells/repetitions in the same matrix.
	for _, it := range plan {
		run, err := loadRun(sp, it.Cell, it.Rep, nil, true)
		if err != nil {
			return nil, err
		}
		if o.DryRun {
			continue
		}
		var blocked []string
		for _, stage := range o.Stages {
			if util.Contains(NotRedoable, stage) && isTerminal(run.Status(stage)) &&
				!(run.setupStarted() && !run.done("prepare")) {
				blocked = append(blocked, stage)
			}
		}
		if len(blocked) > 0 {
			return nil, &StageRefusalError{RunID: run.RunID, Stages: blocked}
		}
	}
	return &matrixInspection{trial: trial, meta: meta, profiles: profiles,
		out: out, plan: plan, parallel: parallel, recovery: recovery}, nil
}

func runMatrixLocked(ctx context.Context, sp M, o MatrixOpts) (M, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	log := o.Log
	if log == nil {
		log = func(m string) { fmt.Println(m) }
	}
	checked, err := inspectMatrix(sp, o)
	if err != nil {
		return nil, err
	}
	trial, out, plan := checked.trial, checked.out, checked.plan
	meta, profiles := checked.meta, checked.profiles
	parallel, recovery := checked.parallel, checked.recovery
	if recovery {
		log("Recovering original attempts with cleanup and offline rendering only.")
	}
	logf := log
	if parallel > 1 {
		var mu sync.Mutex
		logf = func(m string) {
			mu.Lock()
			defer mu.Unlock()
			log(m)
		}
	}
	var runs []*Run
	if !o.DryRun {
		for _, it := range plan {
			run, err := NewRun(sp, it.Cell, it.Rep, logf)
			if err != nil {
				return nil, err
			}
			runs = append(runs, run)
		}
	}
	if !recovery {
		order := make([]any, len(plan))
		for i, it := range plan {
			order[i] = fmt.Sprintf("%s-r%d", util.Str(it.Cell["id"]), it.Rep)
		}
		configurations := []any{}
		for _, c := range util.Maps(sp["cells"]) {
			envProfile := spec.EnvironmentFor(sp, c)
			confProfile := spec.AgentConfigurationFor(sp, c)
			rec := M{"cell": c["id"], "environment": c["environment"], "agent_configuration": c["agent_configuration"],
				"environment_profile_sha256": nil, "agent_configuration_sha256": nil}
			if len(envProfile) > 0 {
				rec["environment_profile_sha256"] = util.Sha256Bytes([]byte(PyDumps(envProfile)))
			}
			if len(confProfile) > 0 {
				rec["agent_configuration_sha256"] = util.Sha256Bytes([]byte(PyDumps(confProfile)))
			}
			configurations = append(configurations, rec)
		}
		task := util.MapOr(sp["task"])
		update := M{
			"trial_id": trial["id"], "product": trial["product"], "trial_file": sp["path"],
			"trial_sha256": sp["sha256"], "task_prompt_sha256": task["prompt_sha256"],
			"skill_revision": res.Fingerprint(), "order_seed": trial["order_seed"],
			"order": order, "parallel": int64(parallel),
			"cells_contract":        CellsContract(sp),
			"task_inputs":           TaskInputs(sp),
			"wall_clock_comparable": parallel == 1, "planned_at": util.Now(),
			"normalized_profiles": profiles,
			"configurations":      configurations,
		}
		for k, v := range update {
			meta[k] = v
		}
		if err := util.WriteJSON(filepath.Join(out, "matrix-plan.json"), meta); err != nil {
			return nil, err
		}
		if err := copyFile(util.S(task["prompt_path"]), filepath.Join(out, "task-prompt.md"), nil); err != nil {
			return nil, err
		}
	}
	if o.DryRun {
		return meta, nil
	}
	if err := runAll(ctx, runs, parallel, o.Stages, o.KeepWorkspace); err != nil {
		return meta, err
	}
	if ctx.Err() != nil {
		return meta, &interruptError{}
	}
	if err := render.MatrixReport(sp, render.MatrixOpts{Synthesize: o.Synthesize && !recovery, Log: log, Ctx: ctx}); err != nil {
		if util.Interrupted(err) || ctx.Err() != nil {
			return meta, &interruptError{}
		}
		return meta, err
	}
	if ctx.Err() != nil {
		return meta, &interruptError{}
	}
	return meta, nil
}

// runAll runs attempts in plan order, or with up to parallel at once
// (Python ThreadPoolExecutor.map). An interruption stops new attempts from
// starting; started ones clean up and finish. The first error in plan order is
// returned, an interruption first.
func runAll(ctx context.Context, runs []*Run, parallel int, stages []string, keep bool) error {
	if parallel <= 1 {
		for _, run := range runs {
			if ctx.Err() != nil {
				return &interruptError{}
			}
			if _, err := run.Go(ctx, stages, keep); err != nil {
				return err
			}
		}
		return nil
	}
	errs := make([]error, len(runs))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, run := range runs {
		sem <- struct{}{}
		if ctx.Err() != nil {
			<-sem
			break
		}
		wg.Add(1)
		go func(i int, run *Run) {
			defer wg.Done()
			defer func() { <-sem }()
			_, errs[i] = run.Go(ctx, stages, keep)
		}(i, run)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil && util.Interrupted(err) {
			return err
		}
	}
	if ctx.Err() != nil {
		return &interruptError{}
	}
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func valueOr(m M, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

func joinSemi(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}
