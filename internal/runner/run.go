package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/royosherove/ajx/internal/agentconfig"
	"github.com/royosherove/ajx/internal/envcheck"
	"github.com/royosherove/ajx/internal/evidence"
	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/render"
	"github.com/royosherove/ajx/internal/reporter"
	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

// StageFunc replaces a stage implementation (tests stub the reporter's LLM
// call this way, like assigning run.stage_extract in Python).
type StageFunc func(r *Run) error

// StageOverrides replaces stages for every Run (Python patches on the Run
// class). Tests only; guarded by no lock, so set it before running.
var StageOverrides = map[string]StageFunc{}

// Run is one (cell, repetition) attempt (Python runner.Run).
type Run struct {
	Spec, Cell M
	Rep        int
	Log        func(string)
	RunID      string
	OutDir     string
	RunDir     string
	StatePath  string
	State      M

	Harness              plugin.Harness
	Auth                 plugin.Auth
	Runner               plugin.Runner
	EnvironmentProfile   M
	ConfigurationProfile M
	ContractSHA256       string

	// Override replaces stages of this run only (Python run.stage_<name> = fn).
	Override map[string]StageFunc

	explicitRelease bool
	goctx           context.Context
}

// NewRun loads (or starts) the state of one attempt.
func NewRun(sp, cell M, rep int, log func(string)) (*Run, error) {
	return loadRun(sp, cell, rep, log, false)
}

// loadRun also supports input validation without creating directories,
// constructing plugins, or persisting a legacy fingerprint upgrade.
func loadRun(sp, cell M, rep int, log func(string), readOnly bool) (*Run, error) {
	if log == nil {
		log = func(m string) { fmt.Println(m) }
	}
	r := &Run{Spec: sp, Cell: cell, Rep: rep, Log: log, Override: map[string]StageFunc{}, goctx: context.Background()}
	r.RunID = fmt.Sprintf("%s-r%d", util.Str(cell["id"]), rep)
	r.OutDir = util.S(util.Get(sp, "trial", "output_dir"))
	r.RunDir = filepath.Join(r.OutDir, "runs", r.RunID)
	// A missing state file is not permission to replay a partially recorded
	// attempt. Inspect before creating anything or replacing its evidence.
	if entries, err := os.ReadDir(r.RunDir); err == nil && len(entries) > 0 {
		if _, err := os.Stat(filepath.Join(r.RunDir, "state.json")); errors.Is(err, os.ErrNotExist) {
			return nil, spec.Errorf("%s: state.json is missing from a nonempty run directory; "+
				"preserve this evidence and use a new output directory", r.RunID)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if !readOnly {
		if err := os.MkdirAll(r.RunDir, 0o755); err != nil {
			return nil, err
		}
	}
	r.StatePath = filepath.Join(r.RunDir, "state.json")
	st, err := ReadJSON(r.StatePath, nil)
	if err != nil {
		return nil, err
	}
	if st == nil {
		if exists(r.StatePath) {
			return nil, spec.Errorf("%s: state.json contains no run state; preserve the evidence and use a new output directory", r.RunID)
		}
		st = M{"stages": M{}}
	}
	state, ok := st.(M)
	if !ok {
		return nil, runtimeErr("%s: state.json is not an object", r.RunID)
	}
	stages := util.Map(state["stages"])
	if stages == nil {
		return nil, spec.Errorf("%s: state.json has no valid stages object; refusing to treat it as an unstarted attempt", r.RunID)
	}
	for name, record := range stages {
		status := util.S(util.Get(record, "status"))
		if status != "running" && !isTerminal(status) {
			return nil, spec.Errorf("%s: state.json has an invalid %s stage record; preserve the original attempt", r.RunID, name)
		}
	}
	if util.Truthy(state["execute"]) && stages["execute"] == nil ||
		util.Truthy(state["paths"]) && stages["prepare"] == nil {
		return nil, spec.Errorf("%s: state.json is missing stage history for an existing attempt; refusing to replay it", r.RunID)
	}
	r.State = state
	if !readOnly {
		if r.Harness, err = spec.HarnessFor(sp, util.S(cell["harness"])); err != nil {
			return nil, err
		}
		if r.Auth, err = spec.AuthFor(sp, cell, ""); err != nil {
			return nil, err
		}
		if r.Runner, err = spec.RunnerFor(sp, cell); err != nil {
			return nil, err
		}
	}
	r.EnvironmentProfile = spec.EnvironmentFor(sp, cell)
	r.ConfigurationProfile = spec.AgentConfigurationFor(sp, cell)
	contract := M{"environment": nilIfEmpty(r.EnvironmentProfile),
		"agent_configuration": nilIfEmpty(agentconfig.ResumeContract(r.ConfigurationProfile)),
		"cell":                cell, "prompt_sha256": util.Get(sp, "task", "prompt_sha256")}
	r.ContractSHA256 = util.Sha256Bytes([]byte(PyDumps(contract)))
	if saved := util.S(r.State["environment_contract_sha256"]); saved != "" {
		if !contractMatches(saved, contract) {
			// Upgrade old fingerprints only when every original field still matches.
			legacy := util.Copy(contract)
			legacy["agent_configuration"] = nilIfEmpty(r.ConfigurationProfile)
			if !contractMatches(saved, legacy) {
				return nil, spec.Errorf("%s: environment, agent configuration, or task changed; "+
					"use a new trial output directory for a new comparison", r.RunID)
			}
		}
		if saved != r.ContractSHA256 && !readOnly {
			r.State["environment_contract_sha256"] = r.ContractSHA256
			if err := r.save(); err != nil {
				return nil, err
			}
		}
	} else if r.EnvironmentProfile != nil && util.Truthy(r.State["paths"]) {
		return nil, spec.Errorf("%s: cannot add environment profiles to an existing attempt; "+
			"use a new trial output directory", r.RunID)
	}
	return r, nil
}

// contractMatches compares a saved fingerprint with the exact Python bytes,
// then with integral numbers normalized (profiles reloaded from JSON).
func contractMatches(saved string, contract M) bool {
	return saved == util.Sha256Bytes([]byte(PyDumps(contract))) ||
		saved == util.Sha256Bytes([]byte(CanonicalJSON(contract, true)))
}

func nilIfEmpty(m M) any {
	if m == nil {
		return nil
	}
	return m
}

// ------------------------------------------------------------------ state

func (r *Run) save() error { return util.WriteJSON(r.StatePath, r.State) }

func (r *Run) stages() M {
	st := util.Map(r.State["stages"])
	if st == nil {
		st = M{}
		r.State["stages"] = st
	}
	return st
}

// Status is the recorded status of a stage ("" when it never ran).
func (r *Run) Status(stage string) string {
	return util.S(util.Get(r.stages(), stage, "status"))
}

func (r *Run) statusRepr(stage string) string {
	if s := r.Status(stage); s != "" {
		return s
	}
	return "None"
}

func (r *Run) done(stage string) bool { return r.Status(stage) == "done" }

func (r *Run) mark(stage, status string, extra M) error {
	rec := M{"status": status, "at": util.Now()}
	for k, v := range extra {
		rec[k] = v
	}
	r.stages()[stage] = rec
	if err := r.save(); err != nil {
		return err
	}
	r.Log(fmt.Sprintf("[%s] stage %s: %s", r.RunID, stage, status))
	return nil
}

// syncRunRecord updates derived metadata after all stage and cleanup work.
// A failed metadata write must not prevent teardown or archival.
func (r *Run) syncRunRecord() error {
	path := filepath.Join(r.RunDir, "run.json")
	if !exists(path) {
		return nil
	}
	value, err := ReadJSON(path, nil)
	if err != nil {
		return err
	}
	record := util.Map(value)
	if record == nil {
		return runtimeErr("cannot update invalid run.json")
	}
	stages := M{}
	for name, value := range r.stages() {
		stages[name] = util.MapOr(value)["status"]
	}
	record["stages"] = stages
	return util.WriteJSON(path, record)
}

func (r *Run) execState() M { return util.Map(r.State["execute"]) }

func (r *Run) paths() M    { return util.MapOr(r.State["paths"]) }
func (r *Run) archived() M { return util.MapOr(r.State["archived"]) }

// Ctx builds the plugin run context (Python Run.ctx()).
func (r *Run) Ctx() *plugin.Ctx {
	paths, archived := r.paths(), r.archived()
	env := spec.WorkerEnv(r.Spec, r.Cell, r.Auth)
	if env == nil {
		env = map[string]string{}
	}
	var unset []string
	if r.Auth != nil {
		unset = append(unset, r.Auth.Unset()...)
	}
	if r.EnvironmentProfile == nil && util.S(r.Cell["shell_startup"]) == "skip" {
		quiet := filepath.Join(r.RunDir, "no-shell-startup")
		_ = os.MkdirAll(quiet, 0o755)
		for k, v := range spec.ShellStartupSkipEnv {
			env[k] = v
		}
		env["ZDOTDIR"] = quiet
		unset = append(unset, spec.ShellStartupDrop...)
	}
	trial := util.MapOr(r.Spec["trial"])
	if util.S(trial["cache"]) == "isolated" && util.S(paths["cache_dir"]) != "" {
		for _, v := range CacheVars {
			env[v] = filepath.Join(util.S(paths["cache_dir"]), strings.ToLower(v))
		}
	}
	workspace := util.OrStr(util.S(archived["workspace"]), util.S(paths["workspace"]), filepath.Join(r.RunDir, "workspace"))
	configDir := util.OrStr(util.S(archived["config_dir"]), util.S(paths["config_dir"]))
	runToken := util.S(r.State["run_token"])
	ctx := &plugin.Ctx{
		Go: r.goctx, Spec: r.Spec, Cell: r.Cell, RunDir: r.RunDir, Workspace: workspace,
		ConfigDir: configDir, CacheDir: util.S(paths["cache_dir"]),
		HomeDir:  util.OrStr(util.S(archived["home_dir"]), util.S(paths["home_dir"])),
		RunToken: runToken,
		Timeout:  util.Int(trial["timeout_seconds"], 0), NarrateTimeout: util.Int(trial["narrate_timeout_seconds"], 0),
		PromptText: util.S(util.Get(r.Spec, "task", "prompt_text")), State: r.State, Env: env,
		UnsetEnv: unset, Runner: r.Runner, Auth: r.Auth,
		CheckEnv:                  util.MergeEnv(env, envcheck.CheckVars(r.Spec, runToken)),
		EnvironmentProfile:        r.EnvironmentProfile,
		AgentConfigurationProfile: r.ConfigurationProfile,
	}
	if r.EnvironmentProfile != nil && ctx.HomeDir != "" && len(archived) == 0 {
		if mr, ok := r.Runner.(plugin.ManagedRunner); ok {
			declared, hasPath := env["PATH"]
			for k, v := range mr.EnvironmentEnv(ctx) {
				env[k] = v
			}
			if hasPath {
				env["PATH"] = declared
			}
			ctx.Env = env
			ctx.CheckEnv = util.MergeEnv(env, envcheck.CheckVars(r.Spec, runToken))
		}
	}
	return ctx
}

func (r *Run) managed() plugin.ManagedRunner {
	mr, _ := r.Runner.(plugin.ManagedRunner)
	return mr
}

func (r *Run) needManaged() (plugin.ManagedRunner, error) {
	if mr := r.managed(); mr != nil {
		return mr, nil
	}
	return nil, runtimeErr("environment profile %s needs an environment runner, got %s",
		util.Str(r.Cell["environment"]), r.Runner.Name())
}

func (r *Run) shellEnv(ctx *plugin.Ctx) map[string]string {
	if r.EnvironmentProfile != nil {
		if mr := r.managed(); mr != nil {
			return mr.ChildEnv(ctx.CheckEnv, ctx.UnsetEnv)
		}
	}
	return envcheck.CommandEnv(ctx.Env, ctx.UnsetEnv, ctx.RunToken, ctx.Spec)
}

// interrupted reports whether the run's context was cancelled.
func (r *Run) interrupted() bool { return r.goctx != nil && r.goctx.Err() != nil }

// checkInterrupt turns a cancelled context into Python's KeyboardInterrupt.
func (r *Run) checkInterrupt() error {
	if r.interrupted() {
		return util.ErrInterrupted
	}
	return nil
}

// commandHook replaces lifecycle commands in tests (Python patches Run._command).
var commandHook func(r *Run, cmd M) error

// command runs a lifecycle command at its declared location (Python _command).
func (r *Run) command(cmd M, ctx *plugin.Ctx, defaultLocation string) (M, error) {
	if commandHook != nil {
		if err := commandHook(r, cmd); err != nil {
			return M{"cmd": cmd["run"], "exit_code": nil, "timed_out": false, "stdout": "", "stderr": ""}, err
		}
	}
	location := util.S(cmd["location"])
	if _, ok := cmd["location"]; !ok || cmd["location"] == nil {
		location = defaultLocation
	}
	res := plugin.ContextShell(util.S(cmd["run"]), ctx, util.Int(cmd["timeout"], 600), location)
	if util.Truthy(res["interrupted"]) || ctx.Context().Err() != nil {
		return res, util.ErrInterrupted
	}
	return res, nil
}

func (r *Run) contextShell(command string, ctx *plugin.Ctx, timeout int, location string) (M, error) {
	res := plugin.ContextShell(command, ctx, timeout, location)
	if util.Truthy(res["interrupted"]) || ctx.Context().Err() != nil {
		return res, util.ErrInterrupted
	}
	return res, nil
}

func (r *Run) readMap(name string) (M, error) {
	v, err := ReadJSON(filepath.Join(r.RunDir, name), nil)
	if err != nil {
		return nil, err
	}
	return util.MapOr(v), nil
}

func (r *Run) writeJSON(name string, data any) error {
	return util.WriteJSON(filepath.Join(r.RunDir, name), data)
}

// ------------------------------------------------------------------ stages

func (r *Run) callStage(stage string) error {
	if f := r.Override[stage]; f != nil {
		return f(r)
	}
	if f := StageOverrides[stage]; f != nil {
		return f(r)
	}
	switch stage {
	case "prepare":
		return r.stagePrepare()
	case "execute":
		return r.stageExecute()
	case "verify":
		return r.stageVerify()
	case "teardown":
		return r.stageTeardown()
	case "normalize":
		return r.stageNormalize()
	case "narrate":
		return r.stageNarrate()
	case "release":
		return r.stageRelease()
	case "extract":
		return r.stageExtract()
	case "measure":
		return r.stageMeasure()
	case "render":
		return r.stageRender()
	case "archive":
		return r.stageArchive()
	}
	return valueErr("unknown stage %s", pyStrRepr(stage))
}

func binaryRepr(h plugin.Harness) string {
	if b := h.Base().BinaryName; b != "" {
		return pyStrRepr(b)
	}
	return "None"
}

func (r *Run) stagePrepare() error {
	if util.Truthy(r.Spec["_cleanup_only"]) {
		return spec.Errorf("saved cleanup profiles cannot prepare a new attempt; reload the trial normally")
	}
	if r.EnvironmentProfile == nil && r.Runner.Name() == "local" && !r.Harness.Available() {
		return runtimeErr("harness binary %s not found on PATH", binaryRepr(r.Harness))
	}
	root := util.S(util.Get(r.Spec, "trial", "workspace_root"))
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	// Unnamed random dirs: nothing in the path reveals an evaluation, a cell, or a sibling run.
	keys := []string{"workspace", "config_dir", "cache_dir"}
	paths := M{}
	for _, k := range keys {
		paths[k] = resolvePath(filepath.Join(root, randHex()))
	}
	if r.EnvironmentProfile != nil {
		keys = append(keys, "home_dir")
		paths["home_dir"] = resolvePath(filepath.Join(root, randHex()))
		r.State["environment_contract_sha256"] = r.ContractSHA256
	}
	r.State["paths"] = paths
	owned := []any{}
	r.State["owned_paths"] = owned
	if err := r.save(); err != nil {
		return err
	}
	for _, k := range keys {
		p := util.S(paths[k])
		if err := os.Mkdir(p, 0o700); err != nil {
			return err
		}
		owned = append(owned, p)
		r.State["owned_paths"] = owned
		if err := r.save(); err != nil {
			return err
		}
	}
	// A retry after a failed prepare (say, a preflight) must not reuse the first attempt's archive:
	// Ctx() prefers archived paths (this worker would land inside the report dir), and a done
	// archive stage would leave these new dirs, and any copied credentials, in workspace_root.
	delete(r.State, "archived")
	delete(r.stages(), "archive")
	r.State["run_token"] = randHex()
	if err := r.save(); err != nil {
		return err
	}
	workspace := util.S(paths["workspace"])
	if fixture := util.S(util.Get(r.Spec, "task", "fixture_dir")); fixture != "" {
		if r.EnvironmentProfile != nil {
			err := filepath.WalkDir(fixture, func(p string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if p == fixture {
					return nil
				}
				if t := d.Type(); !(t.IsRegular() || t.IsDir()) {
					return runtimeErr("environment fixtures must contain only regular files and directories; " +
						"symlinks and special files are not copied")
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		if err := copyTree(fixture, workspace); err != nil {
			return err
		}
	}
	ctx := r.Ctx()
	var problems []string
	if r.Auth != nil {
		problems = r.Auth.Problems(nil)
	}
	if len(problems) > 0 && r.EnvironmentProfile != nil {
		return runtimeErr("environment authentication configuration is not ready: %s", strings.Join(problems, "; "))
	}
	if r.EnvironmentProfile != nil {
		mr, err := r.needManaged()
		if err != nil {
			return err
		}
		planned, err := mr.Plan(ctx)
		if err != nil {
			return err
		}
		r.State["environment"] = planned
		if err := r.save(); err != nil { // Durable ownership intent precedes any backend side effect.
			return err
		}
		ctx = r.Ctx()
		report, err := mr.Prepare(ctx)
		if err != nil {
			return err
		}
		r.State["environment_report"] = report
		if err := r.writeJSON("environment.json", M{"execution_environment": report,
			"agent_configuration": r.State["agent_configuration"]}); err != nil {
			return err
		}
		if err := r.save(); err != nil {
			return err
		}
	}
	if r.ConfigurationProfile != nil {
		manifest, err := agentconfig.Prepare(r.ConfigurationProfile, ctx, r.Harness)
		if err != nil {
			return err
		}
		r.State["agent_configuration"] = manifest
		if err := r.writeJSON("agent-configuration.json", manifest); err != nil {
			return err
		}
		if err := r.save(); err != nil {
			return err
		}
		ctx = r.Ctx()
	}
	if r.Auth != nil {
		if err := r.Auth.Prepare(ctx); err != nil {
			return err
		}
	}
	if r.EnvironmentProfile != nil {
		if _, err := r.managed().RestoreWorkerOwnership(ctx); err != nil {
			return err
		}
	}
	if err := r.checkInterrupt(); err != nil {
		return err
	}
	if pre := util.Maps(r.Spec["preflight"]); len(pre) > 0 {
		// Same shell and env as verify/teardown: a run that cannot clean up never starts.
		var interruptedCmd bool
		preflight, err := envcheck.RunPreflight(ctx.Context(), pre, ctx.Workspace, r.shellEnv(ctx), envcheck.PreflightOpts{
			StopOnFailure: true,
			Executor: func(cmd M) M {
				res, err := r.command(cmd, ctx, "environment")
				if err != nil {
					interruptedCmd = true
				}
				return res
			},
		})
		if err != nil {
			return err
		}
		if interruptedCmd || r.interrupted() {
			return util.ErrInterrupted
		}
		if err := r.writeJSON("preflight.json", mapsList(preflight)); err != nil {
			return err
		}
		for _, c := range preflight {
			if !util.Truthy(c["passed"]) {
				return runtimeErr("preflight %s failed (%s); the worker was not started",
					PyRepr(c["name"]), envcheck.PreflightFailure(c))
			}
		}
	}
	setupResults := []any{}
	for _, cmd := range util.Maps(r.Spec["setup"]) {
		// A setup command can mutate resources before it fails or AJX dies.
		// Persist that cleanup is owed before invoking the first command.
		r.State["setup_started"] = true
		if err := r.save(); err != nil {
			return err
		}
		res, err := r.command(cmd, ctx, "environment")
		if err != nil {
			return err
		}
		setupResults = append(setupResults, pick(res, "cmd", "exit_code", "timed_out", "started_at",
			"stopped_at", "stdout", "stderr", "location"))
		if err := r.writeJSON("setup.json", setupResults); err != nil {
			return err
		}
		if !exitZero(res) {
			return runtimeErr("setup command failed: %s exit=%s", PyRepr(cmd["run"]), pyStr(res["exit_code"]))
		}
		if r.EnvironmentProfile != nil && util.S(cmd["location"]) == "host" {
			if _, err := r.managed().RestoreWorkerOwnership(ctx); err != nil {
				return err
			}
		}
	}
	if err := r.writeJSON("setup.json", setupResults); err != nil {
		return err
	}
	var probes M
	var harnessFacts M
	if r.EnvironmentProfile != nil {
		report := util.MapOr(r.State["environment_report"])
		actual := util.MapOr(report["platform"])
		var parts []string
		for _, key := range []string{"system", "release", "machine"} {
			parts = append(parts, pyOrEmpty(actual[key]))
		}
		probes = M{"os": strings.TrimSpace(strings.Join(parts, " "))}
		for _, tool := range util.Maps(util.Get(report, "inventory", "tools")) {
			if util.S(tool["status"]) == "observed" {
				text := util.OrStr(util.S(tool["stdout"]), util.S(tool["stderr"]))
				probes[util.Str(tool["name"])] = util.Clip(strings.TrimSpace(text), 160)
			}
		}
		var err error
		if harnessFacts, err = r.harnessFacts(ctx, false); err != nil {
			return err
		}
		if !util.Truthy(harnessFacts["available"]) {
			return runtimeErr("harness %s is unavailable in the selected environment", binaryRepr(r.Harness))
		}
	} else {
		probes = M{}
		for _, k := range util.SortedKeys(DefaultProbes) {
			probes[k] = probe(DefaultProbes[k])
		}
		probes["os"] = platformText()
		harnessFacts = M{"version_before": r.Harness.Version(), "available": r.Harness.Available()}
	}
	checks, view := envcheck.InspectCell(r.Spec, r.Cell, r.Harness, r.Auth, r.Runner, ctx.Workspace, ctx,
		envcheck.InspectOpts{Identities: true})
	checks, view = util.MapOr(checks), util.MapOr(view)
	if err := r.checkInterrupt(); err != nil {
		return err
	}
	productVersion, err := r.productVersion(ctx)
	if err != nil {
		return err
	}
	hb := r.Harness.Base()
	harnessRec := M{"name": r.Harness.Name()}
	for k, v := range harnessFacts {
		harnessRec[k] = v
	}
	harnessRec["verified_live"] = hb.VerifiedLive
	harnessRec["clean_supported"] = hb.CleanSupported
	harnessRec["can_resume"] = hb.CanResume
	harnessRec["telemetry"] = hb.TelemetrySummary()
	authRec := M{}
	if r.Auth != nil {
		for k, v := range r.Auth.Describe() {
			authRec[k] = v
		}
		authRec["profile"] = r.Auth.Base().Profile
	}
	authRec["problems"] = anyList(problems)
	authRec["identity"] = checks["auth_identity"]
	if hinter, ok := r.Auth.(envcheck.ProviderHinter); ok && r.Auth != nil {
		authRec["provider_hint"] = hinter.ProviderHint(envMap(view["effective"]))
	}
	var callerNames []string
	for k := range util.Environ() {
		for _, h := range SensitiveNameHints {
			if strings.Contains(k, h) {
				callerNames = append(callerNames, k)
				break
			}
		}
	}
	sort.Strings(callerNames)
	cacheState := "shared caller caches (may be warm)"
	if util.S(util.Get(r.Spec, "trial", "cache")) == "isolated" {
		cacheState = "isolated empty per-run package caches"
	}
	snapshot := M{
		"taken_at": util.Now(), "probes": probes, "harness": harnessRec,
		"product_version_before": productVersion,
		"auth":                   authRec,
		"task_identity":          checks["task_identity"],
		"default_aws_identity":   checks["default_aws_identity"],
		"env_checks":             envcheck.Record(checks),
		"runner":                 r.Runner.Describe(),
		"worker_env_names":       anyList(util.SortedKeys(ctx.Env)),
		"caller_env_names":       anyList(callerNames),
		"cache_state":            cacheState,
		"config":                 r.Cell["config"],
		"execution_environment":  r.State["environment_report"],
		"agent_configuration":    r.State["agent_configuration"],
	}
	if err := r.writeJSON("environment.json", snapshot); err != nil {
		return err
	}
	if len(problems) > 0 {
		r.Log(fmt.Sprintf("[%s] auth warnings: %s", r.RunID, pyList(problems)))
	}
	for _, warning := range append(envcheck.Warnings(checks, r.Auth, view), envcheck.DefaultIdentityWarning(checks)) {
		if warning != "" {
			r.Log(fmt.Sprintf("[%s] env warning: %s", r.RunID, warning))
		}
	}
	if ti := util.Map(checks["task_identity"]); len(ti) > 0 && !util.Truthy(ti["ok"]) {
		r.Log(fmt.Sprintf("[%s] task identity check failed in the worker's tool shell: %s", r.RunID, pyStr(ti["output"])))
	}
	return nil
}

// pyOrEmpty is str(value or "").
func pyOrEmpty(v any) string {
	if !util.Truthy(v) {
		return ""
	}
	return pyStr(v)
}

func (r *Run) productVersion(ctx *plugin.Ctx) (any, error) {
	cmd := util.S(util.Get(r.Spec, "trial", "product_version_cmd"))
	if cmd == "" {
		return nil, nil
	}
	res, err := r.contextShell(cmd, ctx, 60, "environment")
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(util.OrStr(util.S(res["stdout"]), util.S(res["stderr"])))
	if exitZero(res) && text != "" {
		return firstLine(text, 160), nil
	}
	return fmt.Sprintf("unavailable (exit %s)", pyStr(res["exit_code"])), nil
}

func (r *Run) harnessFacts(ctx *plugin.Ctx, refresh bool) (M, error) {
	manifest := util.Map(r.State["agent_configuration"])
	if len(manifest) > 0 && !refresh {
		return M{"available": true, "version_before": util.Get(manifest, "capabilities", "version"),
			"checked_in": "agent environment during profile capability checks"}, nil
	}
	binary := r.Harness.Base().BinaryName
	if len(manifest) > 0 {
		binary = util.S(util.Get(manifest, "capabilities", "binary"))
	}
	if binary == "" {
		return M{"available": false, "version_before": nil}, nil
	}
	available, err := r.contextShell("command -v "+shellQuote(binary), ctx, 20, "environment")
	if err != nil {
		return nil, err
	}
	var version any
	if va := r.Harness.Base().VersionArgv; exitZero(available) && len(va) > 0 {
		argv := append([]string{binary}, va[1:]...)
		result, err := r.contextShell(shellJoin(argv), ctx, 20, "environment")
		if err != nil {
			return nil, err
		}
		text := strings.TrimSpace(util.OrStr(util.S(result["stdout"]), util.S(result["stderr"])))
		if exitZero(result) && text != "" {
			version = firstLine(text, 160)
		}
	}
	return M{"available": exitZero(available), "version_before": version, "checked_in": "agent environment"}, nil
}

func (r *Run) stageExecute() error {
	ctx := r.Ctx()
	model := "harness default"
	if util.Truthy(r.Cell["model"]) {
		model = pyStr(r.Cell["model"])
	}
	r.Log(fmt.Sprintf("[%s] executing task with %s model=%s", r.RunID, r.Harness.Name(), model))
	// Record what we can before launching so a crash mid-run still leaves a findable session.
	planned := r.Harness.Plan(ctx)
	requested := M{}
	for _, k := range []string{"harness", "model", "effort", "config", "auth"} {
		requested[k] = r.Cell[k]
	}
	exec := M{}
	for k, v := range planned {
		exec[k] = v
	}
	exec["requested"] = requested
	exec["started_at"] = util.Now()
	exec["stopped_at"] = nil
	exec["stop_reason"] = "running"
	exec["process_record"] = r.processRecordPath()
	r.State["execute"] = exec
	if err := r.save(); err != nil {
		return err
	}
	ctx = r.Ctx()
	ctx.Go = util.WithProcessRecord(ctx.Context(), r.processRecordPath())
	proc, executeErr := r.Harness.Execute(ctx)
	if proc == nil {
		proc = M{}
	}
	stop := "exit"
	if util.Truthy(proc["timed_out"]) {
		stop = "timeout"
	} else if !exitZero(proc) {
		stop = "exit_" + pyStr(proc["exit_code"])
	}
	merged := util.Copy(r.execState())
	for k, v := range proc {
		merged[k] = v
	}
	merged["stop_reason"] = stop
	r.State["execute"] = merged
	if !exists(r.processRecordPath()) {
		// The live call returned without entering the process launcher.
		delete(merged, "process_record")
		r.State["worker_cleanup"] = M{"status": "not_started", "confirmed": true,
			"reason": "harness returned without launching a tracked process"}
	}
	if err := r.save(); err != nil {
		return err
	}
	return executeErr
}

func (r *Run) stageVerify() error {
	ctx := r.Ctx()
	results := []M{}
	started := util.Now()
	for _, check := range util.Maps(r.Spec["verify"]) {
		plug, err := plugin.NewCheck(util.S(check["type"]))
		if err != nil {
			return err // Python: plugins.get raises outside the check's own error handling
		}
		res, err := runCheck(plug, check, ctx)
		if util.Interrupted(err) || r.interrupted() {
			return util.ErrInterrupted
		}
		if err != nil {
			res = M{"passed": false, "detail": "check crashed: " + pyErrString(err)}
		}
		if res == nil {
			res = M{}
		}
		// A check that could not authenticate says nothing about the agent's work, pass or fail
		// (`! aws ... | grep -q i-` passes when aws has no credentials). stderr only: stdout is
		// often the product's own output (a 401 page, CloudTrail records, lint codes).
		var errs []string
		if !util.Truthy(check["allow_auth_errors"]) {
			errs = envcheck.AuthErrorLines(util.S(res["stderr"]))
		}
		definition := M{}
		for k, v := range check {
			if k != "name" {
				definition[k] = v
			}
		}
		rec := M{"name": check["name"], "type": check["type"], "scope": check["scope"], "definition": definition}
		for k, v := range res {
			rec[k] = v
		}
		if len(errs) > 0 {
			rec["auth_errors"] = anyList(errs)
		}
		results = append(results, rec)
	}
	passed := 0
	withAuthErrors := []any{}
	for _, rec := range results {
		if util.Truthy(rec["passed"]) {
			passed++
		}
		if util.Truthy(rec["auth_errors"]) {
			withAuthErrors = append(withAuthErrors, rec["name"])
		}
	}
	total := len(results)
	outcome := "failed"
	switch {
	case total == 0:
		outcome = "not_run"
	case passed == total:
		outcome = "succeeded"
	case passed > 0:
		outcome = "partial"
	}
	return r.writeJSON("verify.json", M{
		"window": M{"start": started, "end": util.Now()}, "passed": int64(passed), "total": int64(total),
		"outcome":                 outcome,
		"note":                    "ajx reporter verification, run after the task agent stopped; not part of the task",
		"checks_with_auth_errors": withAuthErrors,
		"checks":                  mapsList(results)})
}

// runCheck runs a check plugin, turning a panic into a crash record (Python
// catches every exception a check raises).
func runCheck(plug plugin.Check, check M, ctx *plugin.Ctx) (res M, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = runtimeErr("%v", p)
		}
	}()
	return plug.Run(check, ctx)
}

func (r *Run) stageTeardown() error {
	if err := r.ensureWorkerStopped(); err != nil {
		return err
	}
	ctx := r.Ctx()
	results := []M{}
	teardown := util.Maps(r.Spec["teardown"])
	ran := exists(ctx.Workspace)
	if ran {
		for _, cmd := range teardown {
			res, err := r.command(cmd, ctx, "environment")
			if err != nil {
				return err
			}
			rec := pick(res, "cmd", "exit_code", "timed_out", "started_at", "stopped_at", "stdout", "stderr", "location")
			if util.Truthy(cmd["allow_auth_errors"]) {
				rec["auth_errors"] = []any{}
			} else {
				rec["auth_errors"] = anyList(envcheck.AuthErrorLines(util.S(res["stdout"]), util.S(res["stderr"])))
			}
			results = append(results, rec)
		}
	}
	if err := r.writeJSON("teardown.json", mapsList(results)); err != nil {
		return err
	}
	r.State["teardown"] = envcheck.TeardownSummary(results, len(teardown), ran)
	if err := r.save(); err != nil {
		return err
	}
	if status := util.S(util.Get(r.State, "teardown", "status")); status != "ok" {
		retry := "teardown,render"
		if r.EnvironmentProfile != nil {
			retry = "teardown,release,render,archive"
		}
		r.Log(fmt.Sprintf("[%s] teardown %s: resources the task created may still exist; see teardown.json, "+
			"then `ajx run <trial> --cells %s --stages %s`", r.RunID, pyStr(util.Get(r.State, "teardown", "status")),
			util.Str(r.Cell["id"]), retry))
	}
	env, err := r.readMap("environment.json")
	if err != nil {
		return err
	}
	if r.EnvironmentProfile != nil {
		facts, err := r.harnessFacts(ctx, true)
		if util.Interrupted(err) {
			return err
		}
		if err != nil {
			env["harness_version_after"] = nil
		} else {
			env["harness_version_after"] = facts["version_before"]
		}
	} else {
		env["harness_version_after"] = r.Harness.Version()
	}
	env["product_version_after"] = nil
	if exists(ctx.Workspace) {
		pv, err := r.productVersion(ctx)
		if err != nil {
			return err
		}
		env["product_version_after"] = pv
	}
	return r.writeJSON("environment.json", env)
}

func (r *Run) stageNormalize() error {
	ctx := r.Ctx()
	tel, err := r.Harness.Normalize(ctx, "execute", nil)
	if err != nil {
		return err
	}
	if tel == nil {
		tel = M{}
	}
	if out := evidence.AssignIDs(tel); out != nil {
		tel = out
	}
	flags := evidence.IsolationScan(tel, r.Spec, r.State)
	if flags == nil {
		flags = []any{}
	}
	tel["isolation_flags"] = flags
	if r.Status("execute") != "done" {
		lim := util.List(tel["limitations"])
		tel["limitations"] = append(append([]any{}, lim...),
			fmt.Sprintf("execute stage status %s: evidence may be partial", r.statusRepr("execute")))
	}
	if err := r.writeJSON("telemetry.execute.json", tel); err != nil {
		return err
	}
	var b strings.Builder
	for _, ev := range util.Maps(tel["events"]) {
		row := M{}
		for k, v := range ev {
			if k != "input_raw" {
				row[k] = v
			}
		}
		b.WriteString(util.PyJSON(row))
		b.WriteString("\n")
	}
	if err := writeText(filepath.Join(r.RunDir, "events.jsonl"), b.String()); err != nil {
		return err
	}
	verify, err := r.readMap("verify.json")
	if err != nil {
		return err
	}
	environment, err := r.readMap("environment.json")
	if err != nil {
		return err
	}
	if err := writeText(filepath.Join(r.RunDir, "digest.md"),
		evidence.BuildDigest(r.Spec, r.Cell, r.State, tel, verify, environment, evidence.DigestOpts{})); err != nil {
		return err
	}
	return writeText(filepath.Join(r.RunDir, "digest.narrator.md"),
		evidence.BuildDigest(r.Spec, r.Cell, r.State, tel, verify, environment, evidence.DigestOpts{OmitVerification: true}))
}

func (r *Run) writeJourney(text, provenance string) error {
	header := "<!-- provenance: " + provenance + " -->\n"
	if err := writeText(filepath.Join(r.RunDir, "journey.md"), header+strings.TrimSpace(text)+"\n"); err != nil {
		return err
	}
	r.State["journey_provenance"] = provenance
	return r.save()
}

func (r *Run) stageNarrate() error {
	ctx := r.Ctx()
	telV, err := ReadJSON(filepath.Join(r.RunDir, "telemetry.execute.json"), nil)
	if err != nil {
		return err
	}
	tel := util.Map(telV)
	if st := r.Status("execute"); (st != "done" && st != "interrupted") || telV == nil {
		return r.writeJourney(fmt.Sprintf("# Journey unavailable\n\nThe task never ran to a recordable stopping point "+
			"(execute stage status: %s). There is no agent experience to narrate; see state.json and digest.md.",
			r.statusRepr("execute")), JourneyUnavailable)
	}
	digestPath := filepath.Join(r.RunDir, "digest.narrator.md")
	if !exists(digestPath) {
		digestPath = filepath.Join(r.RunDir, "digest.md")
	}
	digest, err := readText(digestPath)
	if err != nil {
		return err
	}
	prompt, err := reporter.NarratePrompt(r.Spec, r.Cell, r.RunID, digest)
	if err != nil {
		return err
	}
	if err := writeText(filepath.Join(r.RunDir, "narrate.prompt.md"), prompt); err != nil {
		return err
	}
	var proc M
	text := ""
	narrateTel := M{}
	delete(r.State, "narration_failure")
	failure := M{"category": "resume_unsupported", "message": "the harness does not support session resumption"}
	if r.Harness.Base().CanResume {
		failure = M{"category": "session_unavailable", "message": "the execution has no resumable session identifier"}
	}
	if r.Harness.Base().CanResume && util.Truthy(util.Get(r.State, "execute", "session_id")) {
		failure = M{"category": "resume_no_result", "message": "the resumed harness returned no narration result"}
		p, err := r.Harness.Narrate(ctx, prompt)
		if util.Interrupted(err) || r.interrupted() {
			return util.ErrInterrupted
		}
		if err != nil {
			r.Log(fmt.Sprintf("[%s] self-narration failed: %s", r.RunID, pyMessage(err)))
			failure = M{"category": "resume_error", "message": pyErrString(err)}
			p = nil
		}
		proc = p
		if len(proc) > 0 {
			r.State["narrate"] = proc
			if err := r.save(); err != nil {
				return err
			}
			ntel, err := r.Harness.Normalize(ctx, "narrate", util.Strings(tel["message_ids"]))
			if err != nil {
				return err
			}
			if ntel == nil {
				ntel = M{}
			}
			if err := r.writeJSON("telemetry.narrate.json", ntel); err != nil {
				return err
			}
			narrateTel = ntel
			text = strings.TrimSpace(util.S(ntel["final_text"]))
		}
	}
	rejected := ""
	if len(proc) > 0 {
		rejected = NarrationRejection(text, narrateTel)
	}
	if len(proc) > 0 && rejected == "" {
		restriction := "tool restrictions unrecorded"
		if proc != nil {
			switch v := proc["tools_disabled"].(type) {
			case bool:
				if v {
					restriction = "tools disabled"
				}
			default:
				if util.Truthy(v) {
					restriction = pyStr(v)
				}
			}
		}
		return r.writeJourney(JourneyText(text), "self-narrated: the original task agent, resumed after the task; "+restriction)
	}
	if len(proc) > 0 {
		r.Log(fmt.Sprintf("[%s] %s; falling back to a labeled reconstruction", r.RunID, NarrationRejected[rejected]))
		failure = M{"category": rejected, "message": NarrationRejected[rejected]}
		if reason := narrateTel["stop_reason"]; reason != nil {
			failure["stop_reason"] = reason
		}
	}
	r.State["narration_failure"] = failure
	if err := r.save(); err != nil {
		return err
	}
	result, err := reporter.Reconstruct(ctx.Context(), r.Spec, r.RunDir)
	if util.Interrupted(err) || r.interrupted() {
		return util.ErrInterrupted
	}
	if err != nil {
		if jerr := r.writeJourney(fmt.Sprintf("# Journey unavailable\n\nSelf-narration was not possible and the reporter "+
			"reconstruction failed: %s. The evidence digest is in digest.md.", pyErrString(err)), JourneyUnavailable); jerr != nil {
			return jerr
		}
		return err
	}
	narrate := util.Copy(util.MapOr(result["proc"]))
	narrate["reconstruction"] = true
	if rejected != "" {
		narrate["self_narration_rejected"] = rejected
	}
	r.State["narrate"] = narrate
	if strings.TrimSpace(util.S(result["text"])) != "" {
		// The full message remains structured in state/run.json. Keep the
		// provenance comment on one line and prevent closing its HTML comment.
		reason := narrationFailureSummary(util.S(failure["message"]))
		return r.writeJourney(util.S(result["text"]),
			"EDITORIAL RECONSTRUCTION by the reporter agent from recorded evidence; the original "+
				"agent could not narrate ("+util.S(failure["category"])+": "+reason+"). No recollection is claimed.")
	}
	return r.writeJourney("# Journey unavailable\n\nThe reporter returned no text.", JourneyUnavailable)
}

func narrationFailureSummary(message string) string {
	const maxCharacters = 320
	const truncated = "… [truncated; see narration_failure.message]"
	reason := strings.ReplaceAll(strings.Join(strings.Fields(message), " "), "--", "—")
	chars := []rune(reason)
	if len(chars) > maxCharacters {
		return string(chars[:maxCharacters-len([]rune(truncated))]) + truncated
	}
	return reason
}

func (r *Run) stageExtract() error {
	if util.S(r.State["journey_provenance"]) == JourneyUnavailable {
		return reporter.WriteAsksStub(r.RunDir, "skipped: no journey to extract from", nil)
	}
	var missing []string
	for _, n := range []string{"journey.md", "digest.md"} {
		if !isFile(filepath.Join(r.RunDir, n)) {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return reporter.WriteAsksStub(r.RunDir, "skipped: "+strings.Join(missing, " and ")+
			" missing (narration did not finish)", nil)
	}
	result, err := reporter.Extract(r.goctx, r.Spec, r.RunDir)
	if err != nil {
		return err
	}
	r.State["extract"] = util.MapOr(result)["proc"]
	return r.save()
}

// teardownNeedsEnvironment: a teardown retry repeats every command, including
// previously successful environment steps.
func (r *Run) teardownNeedsEnvironment() bool {
	if r.EnvironmentProfile == nil || (!r.done("prepare") && !r.setupStarted()) || !isTerminal(r.Status("teardown")) {
		return false
	}
	required := false
	for _, cmd := range util.Maps(r.Spec["teardown"]) {
		if util.S(cmd["location"]) == "environment" {
			required = true
		}
	}
	if !required {
		return false
	}
	return r.Status("teardown") != "done" || util.S(util.Get(r.State, "teardown", "status")) != "ok"
}

func cleanupConfirmed(state M) bool {
	v, ok := util.Get(state, "environment_cleanup", "confirmed").(bool)
	return ok && v
}

// stageRelease releases the worker after narration; reporting consumes exported evidence.
func (r *Run) stageRelease() error {
	if err := r.ensureWorkerStopped(); err != nil {
		return err
	}
	if r.EnvironmentProfile == nil || !util.Truthy(r.State["environment"]) {
		return nil
	}
	if cleanupConfirmed(r.State) {
		return nil
	}
	var result M
	if r.teardownNeedsEnvironment() && !r.explicitRelease {
		result = M{
			"status": "deferred_teardown", "confirmed": false, "backend": r.EnvironmentProfile["backend"],
			"reason": "Task teardown needs its original environment. Retry teardown,release,render,archive; " +
				"explicit release,render,archive discards the environment without confirming task cleanup.",
			"expires_at": util.Get(r.State, "environment", "expires_at"),
		}
	} else {
		mr, err := r.needManaged()
		if err != nil {
			return err
		}
		if result, err = mr.Release(r.Ctx()); err != nil {
			return err
		}
		if result == nil {
			result = M{}
		}
	}
	r.State["environment_cleanup"] = result
	if err := r.save(); err != nil {
		return err
	}
	environment, err := r.readMap("environment.json")
	if err != nil {
		return err
	}
	environment["environment_cleanup"] = result
	if err := r.writeJSON("environment.json", environment); err != nil {
		return err
	}
	if c, ok := result["confirmed"].(bool); !ok || !c {
		status := "unknown"
		if v, present := result["status"]; present {
			status = pyStr(v)
		}
		return runtimeErr("environment cleanup was not confirmed: %s", status)
	}
	return nil
}

func (r *Run) stageMeasure() error {
	_, err := evidence.MeasureRun(r.Spec, r.Cell, r.RunDir, r.State)
	return err
}

func (r *Run) stageRender() error {
	if _, err := render.WriteRunJSON(r.Spec, r.Cell, r.Rep, r.RunDir, r.State, r.Harness, r.Auth, r.Runner); err != nil {
		return err
	}
	return render.RunReport(r.RunDir)
}

// ownedPath reports whether p may be touched: owned_paths absent (old
// states) or listing it.
func (r *Run) ownedPath(p string) bool {
	owned, present := r.State["owned_paths"]
	if !present || owned == nil {
		return true
	}
	return util.Contains(util.Strings(owned), p)
}

// purgeCredentials purges known files through owned directory descriptors;
// auth cleanup hooks are deferred until a confirmed release.
func (r *Run) purgeCredentials() error {
	paths, archived := r.paths(), r.archived()
	confirmed := (r.EnvironmentProfile == nil || !util.Truthy(r.State["environment"]) || cleanupConfirmed(r.State)) &&
		util.Get(r.State, "worker_cleanup", "confirmed") != false
	type rootRec struct {
		path string
		dir  *pinnedDir
	}
	roots := map[string]rootRec{}
	var errs []string
	defer func() {
		for _, rr := range roots {
			rr.dir.Close()
		}
	}()
	for _, key := range []string{"config_dir", "home_dir"} {
		original := util.S(paths[key])
		if original == "" || !r.ownedPath(original) {
			continue
		}
		root := util.OrStr(util.S(archived[key]), original)
		dir, err := openDirectoryNofollow(root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			var pe *pyError
			if errors.As(err, &pe) {
				return err // Python raises RuntimeError here, outside the OSError handler.
			}
			errs = append(errs, key+": "+pyTypeName(err))
			continue
		}
		roots[key] = rootRec{root, dir}
	}
	var hookErr error
	// Hooks take filesystem paths, not fds. Never give a still-active worker
	// the opportunity to redirect those paths during unconfirmed release.
	if cfg, ok := roots["config_dir"]; confirmed && ok && len(errs) == 0 && r.Auth != nil {
		ctx := r.Ctx()
		ctx.ConfigDir = cfg.path
		ctx.HomeDir = ""
		if home, ok := roots["home_dir"]; ok {
			ctx.HomeDir = home.path
		}
		if err := callCleanup(r.Auth, ctx); err != nil {
			if util.Interrupted(err) || r.interrupted() {
				hookErr = util.ErrInterrupted
			} else {
				r.Log(fmt.Sprintf("[%s] auth cleanup failed: %s", r.RunID, pyMessage(err)))
			}
		}
	}
	// Also runs if an auth hook is interrupted; descriptors stay pinned if a
	// worker replaces a root, ancestor, or descendant with a symlink.
	for _, key := range []string{"config_dir", "home_dir"} {
		rr, ok := roots[key]
		if !ok {
			continue
		}
		if err := purgeCredentialTree(rr.dir.root); err != nil {
			errs = append(errs, key+": "+pyTypeName(err))
		}
	}
	if hookErr != nil {
		return hookErr
	}
	if len(errs) > 0 {
		return runtimeErr("credential purge could not safely complete: %s", strings.Join(errs, "; "))
	}
	return nil
}

func callCleanup(a plugin.Auth, ctx *plugin.Ctx) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = runtimeErr("%v", p)
		}
	}()
	return a.Cleanup(ctx)
}

// stageArchive moves the workspace and harness config under the run dir so
// later workers cannot discover them; purges copied credentials; drops
// package caches; repoints evidence paths.
func (r *Run) stageArchive() error {
	if err := r.ensureWorkerStopped(); err != nil {
		return err
	}
	if r.setupStarted() && !r.done("prepare") && util.Get(r.State, "teardown", "status") != "ok" {
		_ = r.purgeCredentials()
		return runtimeErr("setup may have changed resources and teardown is not confirmed; retaining original paths for cleanup")
	}
	paths := r.paths()
	moved := util.Copy(r.archived())
	// A confirmed release is durable; archiving no longer needs the backend to be available.
	if r.EnvironmentProfile != nil && util.Truthy(r.State["environment"]) && !cleanupConfirmed(r.State) {
		if err := r.callStage("release"); err != nil {
			// Keep the original ownership and paths for retry, but never retain known
			// staged credentials just because the backend cannot confirm release.
			_ = r.purgeCredentials()
			return err
		}
	}
	if err := r.purgeCredentials(); err != nil {
		return err
	}
	for _, pair := range [][2]string{{"workspace", "workspace"}, {"config_dir", "harness-config"}, {"home_dir", "agent-home"}} {
		key, dest := pair[0], pair[1]
		src := util.S(paths[key])
		if src != "" && r.ownedPath(src) && exists(src) {
			target := filepath.Join(r.RunDir, dest)
			if _, err := os.Lstat(target); err == nil {
				if err := os.RemoveAll(target); err != nil {
					return err
				}
			}
			if err := moveTree(src, target); err != nil {
				return err
			}
			moved[key] = target
			r.State["archived"] = util.Copy(moved)
			if err := r.save(); err != nil { // Keep moved evidence findable even if a later cleanup step fails.
				return err
			}
		}
	}
	if cache := util.S(paths["cache_dir"]); cache != "" && exists(cache) && r.ownedPath(cache) {
		_ = os.RemoveAll(cache)
	}
	hostContext := filepath.Join(r.RunDir, "host-environment")
	if r.EnvironmentProfile != nil && exists(hostContext) {
		if st, err := os.Lstat(hostContext); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return runtimeErr("host check context was replaced by a symlink; refusing to archive it")
		}
		if err := os.RemoveAll(hostContext); err != nil {
			return err
		}
	}
	r.State["archived"] = moved
	if err := r.save(); err != nil {
		return err
	}
	// evidence pointers recorded before the move
	configDir, movedConfig := util.S(paths["config_dir"]), util.S(moved["config_dir"])
	for _, name := range []string{"telemetry.execute.json", "telemetry.narrate.json", "run.json"} {
		v, err := ReadJSON(filepath.Join(r.RunDir, name), nil)
		if err != nil {
			return err
		}
		data := util.Map(v)
		if len(data) == 0 {
			continue
		}
		changed := false
		holders := []M{data}
		if ev := util.Map(data["evidence"]); len(ev) > 0 {
			holders = append(holders, ev)
		}
		for _, holder := range holders {
			old := util.S(holder["transcript_path"])
			if old != "" && configDir != "" && strings.HasPrefix(old, configDir) && movedConfig != "" {
				holder["transcript_path"] = movedConfig + old[len(configDir):]
				changed = true
			}
		}
		if changed {
			if err := r.writeJSON(name, data); err != nil {
				return err
			}
		}
	}
	return nil
}

// journeyHeading finds the `# Journey` heading the narration prompt asks for.
var journeyHeading = regexp.MustCompile(`(?m)^# Journey\b`)

// NarrationRejected maps a rejection reason to its log text (Python
// runner.NARRATION_REJECTED).
var NarrationRejected = map[string]string{
	"safety_stop":        "self-narration was stopped by the model's safeguards",
	"no_journey_heading": "self-narration did not include the # Journey account",
	"empty_or_short":     "self-narration empty or short",
}

// NarrationRejection says why a self-narration cannot stand as the journey,
// or "" when it can. A safeguard stop can leave a refusal in place of the
// account, and an answer without the requested `# Journey` document is not one.
func NarrationRejection(text string, ntel M) string {
	if util.Truthy(util.Get(ntel, "harness_reported", "safety_stops")) {
		return "safety_stop"
	}
	if !journeyHeading.MatchString(text) {
		if util.RuneLen(text) >= 200 {
			return "no_journey_heading"
		}
		return "empty_or_short"
	}
	if util.RuneLen(JourneyText(text)) < 200 {
		return "empty_or_short"
	}
	return ""
}

// JourneyText is the account from its `# Journey` heading on, without any
// preamble before it.
func JourneyText(text string) string {
	if loc := journeyHeading.FindStringIndex(text); loc != nil {
		return text[loc[0]:]
	}
	return text
}
