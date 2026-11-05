package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	ajx "github.com/royosherove/ajx"
	"github.com/royosherove/ajx/internal/envcheck"
	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

// report collects human lines (printed unless JSON) and a JSON document.
type report struct {
	json  bool
	lines []string
	doc   util.M
}

func newReport(command string, json bool) *report {
	return &report{json: json, doc: util.M{"schema_version": SchemaVersion, "command": command}}
}

func (r *report) line(format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	r.lines = append(r.lines, s)
	if !r.json {
		fmt.Fprintln(Stdout, s)
	}
}

func (r *report) list(key string, v any) {
	r.doc[key] = append(util.List(r.doc[key]), v)
}

func (r *report) emit() {
	if r.json {
		r.doc["lines"] = strs(r.lines)
		emitJSON(r.doc)
	}
}

func cmdDoctor(ns *Namespace) (int, error) {
	return runDoctor(ns.Str("trial"), ns.Bool("json"))
}

// Doctor runs `ajx doctor` and returns its exit code, lines and document.
func Doctor(trial string, json bool) (int, []string, util.M, error) {
	r := newReport("doctor", json)
	code, err := doctor(trial, r)
	return code, r.lines, r.doc, err
}

func runDoctor(trial string, json bool) (int, error) {
	r := newReport("doctor", json)
	code, err := doctor(trial, r)
	if err != nil {
		return code, err
	}
	r.doc["ready"] = code == 0
	r.doc["exit_code"] = code
	r.emit()
	return code, nil
}

func yesNo(b bool, no string) string {
	if b {
		return "yes"
	}
	return no
}

func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

func doctor(trial string, r *report) (int, error) {
	var sp util.M
	if trial != "" {
		var err error
		sp, err = spec.Load(trial, spec.LoadOpts{})
		if err != nil {
			return 2, err
		}
		plugin.Load(filepath.Dir(util.S(sp["path"])))
		r.doc["trial"] = sp["path"]
	} else {
		plugin.Load("")
		r.doc["trial"] = nil
	}
	r.doc["implementation"] = "go"
	r.doc["version"] = ajx.Version
	r.line("go %s (ajx %s)", strings.TrimPrefix(runtime.Version(), "go"), ajx.Version)
	var names []string
	wanted, hostRequired := map[string]bool{}, map[string]bool{}
	if sp != nil {
		names = spec.HarnessNames(sp)
		rep := util.S(util.Get(sp, "reporter", "harness"))
		wanted[rep], hostRequired[rep] = true, true
		for _, c := range util.Maps(sp["cells"]) {
			h := util.S(c["harness"])
			wanted[h] = true
			if util.S(util.Get(spec.EnvironmentFor(sp, c), "backend")) != "container" {
				hostRequired[h] = true
			}
		}
	} else {
		names = plugin.Names("harness")
		for _, n := range names {
			wanted[n], hostRequired[n] = true, true
		}
	}
	ok := doctorCoordinatorChecks.run(r)
	r.doc["harnesses"] = []any{}
	for _, name := range names {
		if !wanted[name] {
			continue
		}
		if !hostRequired[name] {
			r.line("harness %-14s availability will be checked inside the selected container during prepare", name)
			r.list("harnesses", util.M{"name": name, "checked": "container"})
			continue
		}
		var h plugin.Harness
		var err error
		if sp != nil {
			h, err = spec.HarnessFor(sp, name)
		} else {
			h, err = plugin.NewHarness(name)
		}
		if err != nil {
			return 1, err
		}
		b := h.Base()
		avail := h.Available()
		probe := plugin.CommandProbe{}
		if avail {
			probe = plugin.ProbeHarnessVersion(h)
		}
		version := probe.Output
		problems := []string{}
		if probe.Checked && !probe.OK {
			problems = append(problems, "version command failed: "+probe.Failure())
			if version == "" {
				version = "unavailable (" + probe.Failure() + ")"
			}
		}
		r.line("harness %-14s installed=%s version=%s verified_live=%s resume=%s clean=%s telemetry: %s",
			name, yesNo(avail, "NO "), util.OrStr(version, "n/a"), pyBool(b.VerifiedLive), pyBool(b.CanResume),
			pyBool(b.CleanSupported), b.TelemetrySummary())
		r.list("harnesses", util.M{"name": name, "checked": "host", "installed": avail, "version": nilIfEmpty(version),
			"verified_live": b.VerifiedLive, "resume": b.CanResume, "reporter": b.CanReport, "clean": b.CleanSupported,
			"telemetry": b.TelemetrySummary(), "version_probe": commandProbeDoc(probe), "problems": strs(problems)})
		for _, problem := range problems {
			r.line("    FAILED %s: %s", name, problem)
		}
		ok = ok && avail && len(problems) == 0
	}
	if sp != nil {
		warnings := envcheck.ShellSyntaxWarnings(sp)
		for _, w := range warnings {
			r.line("    WARNING %s", w)
		}
		r.doc["shell_syntax_warnings"] = strs(warnings)
		tmp, err := os.MkdirTemp("", "ajx-doctor-")
		if err != nil {
			return 1, err
		}
		cellsOK, err := doctorCells(sp, tmp, r)
		os.RemoveAll(tmp)
		if err != nil {
			return 1, err
		}
		ok = ok && cellsOK
		r.doc["verify"] = []any{}
		for _, cmd := range util.Maps(sp["verify"]) {
			r.line("verify  %-10s %s", util.Str(cmd["type"]), util.Str(cmd["name"]))
			r.list("verify", util.M{"type": cmd["type"], "name": cmd["name"], "location": cmd["location"]})
		}
		r.line("workspace_root=%s output_dir=%s", util.Str(util.Get(sp, "trial", "workspace_root")),
			util.Str(util.Get(sp, "trial", "output_dir")))
		r.doc["workspace_root"] = util.Get(sp, "trial", "workspace_root")
		r.doc["output_dir"] = util.Get(sp, "trial", "output_dir")
		r.doc["paths"] = []any{}
		for _, key := range []string{"workspace_root", "output_dir"} {
			path := util.S(util.Get(sp, "trial", key))
			problem := doctorDirectory(path)
			r.list("paths", util.M{"field": "trial." + key, "path": path,
				"ready": problem == "", "problem": nilIfEmpty(problem)})
			if problem != "" {
				r.line("    FAILED trial.%s: %s", key, problem)
				ok = false
			}
		}
		r.line("Review the declared permissions and TASK identity before running. Model authentication and " +
			"task access are separate. Environment profiles report which restrictions are enforced.")
	}
	r.doc["ready"] = ok
	if ok {
		r.doc["exit_code"] = 0
		r.line("READY: the checked prerequisites passed. Run only within the authorized scope.")
		return 0, nil
	}
	r.doc["exit_code"] = 1
	r.line("NOT READY: fix the FAILED/PROBLEMS or missing harnesses above before `ajx run`.")
	return 1, nil
}

func why(res util.M) string {
	if util.Truthy(res["timed_out"]) {
		return "timed out"
	}
	if code, isNum := util.Num(res["exit_code"]); !isNum || code != 0 {
		return "exit " + spec.PyStr(res["exit_code"])
	}
	return "credential errors in its output"
}

func envSig(env map[string]string) string {
	keys := util.SortedKeys(env)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(env[k])
		b.WriteByte(0)
	}
	return b.String()
}

type cmdGroup struct {
	env   map[string]string
	cells []string
	tool  []util.M
}

// doctorCells ports _doctor_cells: per cell model credentials, the worker's
// tool-shell identity and env warnings; then per distinct verify/teardown
// environment, the same identity under /bin/sh and the [[preflight]] results.
func doctorCells(sp util.M, tmp string, r *report) (bool, error) {
	ctx := context.Background()
	ok := true
	memo := map[string]any{}
	once := func(key string, fn func() any) any {
		if v, seen := memo[key]; seen {
			return v
		}
		v := fn()
		memo[key] = v
		return v
	}
	probeAuth := func(auth plugin.Auth, env map[string]string) plugin.CommandProbe {
		return once("auth\x00"+auth.Base().Harness+"\x00"+auth.Base().Profile+"\x00"+envSig(env), func() any {
			return plugin.ProbeAuthIdentity(auth, env)
		}).(plugin.CommandProbe)
	}
	var groupOrder []string
	groups := map[string]*cmdGroup{}
	rep := util.MapOr(sp["reporter"])
	reporterCell := util.M{"id": "reporter", "harness": rep["harness"], "auth": rep["auth"]}
	reporterAuth, err := spec.AuthFor(sp, reporterCell, "")
	if err != nil {
		return false, err
	}
	r.line("reporter harness=%s model=%s auth=%s", util.Str(rep["harness"]), util.OrStr(util.S(rep["model"]), "harness default"),
		reporterAuth.Base().Profile)
	reporterProblems := reporterAuth.Problems(nil)
	reporterProbe := probeAuth(reporterAuth, util.ChildEnv(reporterAuth.Env(), reporterAuth.Unset()))
	if failure := authProbeFailure(reporterProbe); failure != "" {
		reporterProblems = append(reporterProblems, "model identity check failed: "+failure)
	}
	if len(reporterProblems) > 0 {
		r.line("    PROBLEMS: %s", spec.ReprList(reporterProblems))
		ok = false
	}
	r.doc["reporter"] = util.M{"harness": rep["harness"], "model": rep["model"], "auth": reporterAuth.Base().Profile,
		"problems": strs(reporterProblems), "auth_identity_probe": commandProbeDoc(reporterProbe)}
	identCmd := util.S(util.Get(sp, "task", "identity_cmd"))
	ws, err := envcheck.ScratchWorkspace(sp, tmp)
	if err != nil {
		return false, err
	}
	r.doc["cells"] = []any{}
	for _, cell := range util.Maps(sp["cells"]) {
		cellDoc := util.M{"id": cell["id"], "harness": cell["harness"], "model": cell["model"], "config": cell["config"]}
		var cellProblems []string
		harness, err := spec.HarnessFor(sp, util.S(cell["harness"]))
		if err != nil {
			return false, err
		}
		auth, err := spec.AuthFor(sp, cell, "")
		if err != nil {
			return false, err
		}
		rn, err := spec.RunnerFor(sp, cell)
		if err != nil {
			return false, err
		}
		probs := append(auth.Problems(nil), pythonPluginProblems(sp, cell)...)
		profile := spec.EnvironmentFor(sp, cell)
		model := util.OrStr(util.S(cell["model"]), "harness default")
		if profile != nil {
			r.line("cell %-16s harness=%s model=%s", util.Str(cell["id"]), util.Str(cell["harness"]), model)
			r.line("    environment=%s backend=%s agent_configuration=%s", util.OrStr(util.S(cell["environment"]), "ajx-local"),
				util.Str(profile["backend"]), util.OrStr(util.S(cell["agent_configuration"]), "legacy clean settings"))
			var check util.M
			if mr, managed := rn.(plugin.ManagedRunner); managed {
				check = mr.Doctor(&plugin.Ctx{Go: ctx, Spec: sp, Cell: cell})
			} else {
				check = util.M{"status": "available"}
			}
			healthy := util.S(check["status"]) == "available"
			r.line("    backend prerequisites: %s", map[bool]string{true: "available", false: "FAILED"}[healthy])
			probe := util.MapOr(check["mount_probe"])
			switch util.S(probe["status"]) {
			case "ok":
				r.line("    workspace bind-mount probe: ok (%s)", spec.PyStr(probe["workspace_root"]))
			case "skipped":
				r.line("    workspace bind-mount probe: skipped (%s)", spec.PyStr(probe["reason"]))
			default:
				if len(probe) > 0 {
					r.line("    workspace bind-mount probe: FAILED (%s)", spec.PyStr(probe["workspace_root"]))
				}
			}
			if e := util.Str(check["error"]); e != "" && util.Truthy(check["error"]) {
				r.line("    %s", e)
				cellProblems = append(cellProblems, e)
			}
			ok = ok && healthy
			if len(probs) > 0 {
				r.line("    PROBLEMS: %s", spec.ReprList(probs))
				ok = false
			}
			r.line("    Profile schema and adapter controls validated. Runtime inventory, permissions, and " +
				"environment-located preflight commands are checked during prepare.")
			r.line("    Setup/teardown use their declared location; verification defaults to the host.")
			cellDoc["environment"] = util.OrStr(util.S(cell["environment"]), "ajx-local")
			cellDoc["backend"] = profile["backend"]
			cellDoc["agent_configuration"] = cell["agent_configuration"]
			cellDoc["backend_prerequisites"] = map[bool]string{true: "available", false: "failed"}[healthy]
			cellDoc["mount_probe"] = nilIfEmptyMap(probe)
			cellDoc["problems"] = strs(append(cellProblems, probs...))
			cellDoc["warnings"] = []any{}
			r.list("cells", cellDoc)
			continue
		}
		pctx := envcheck.PlaceholderCtx(sp, cell, auth, rn, tmp)
		pctx.Go = ctx
		checks, view := envcheck.InspectCell(sp, cell, harness, auth, rn, ws, pctx, envcheck.InspectOpts{})
		eff := envcheck.Effective(view)
		shell := envcheck.ViewShell(view)
		ab := auth.Base()
		authProbe := probeAuth(auth, eff)
		checks["auth_identity"] = authProbe.Output
		r.line("cell %-16s harness=%s model=%s config=%s", util.Str(cell["id"]), util.Str(cell["harness"]), model, util.Str(cell["config"]))
		r.line("    auth=%s (%s) env=%s unset=%s isolates_config=%s", ab.Profile, auth.Name(),
			spec.ReprList(util.SortedKeys(auth.Env())), spec.ReprList(auth.Unset()), pyBool(ab.IsolatesConfig))
		r.line("    configuration: %s", util.Str(util.Get(checks, "configuration", "note")))
		r.line("    shell startup: %s", util.Str(util.Get(checks, "shell_startup", "note")))
		for _, s := range util.Maps(checks["harness_settings"]) {
			r.line("    harness settings env applied over the worker env: %s sets %s", util.Str(s["source"]),
				strings.Join(util.Strings(s["names"]), ", "))
		}
		authIdent := util.Str(checks["auth_identity"])
		r.line("    harness auth identity (model credentials): %s", util.OrStr(authIdent, "n/a"))
		var authOK any
		if authProbe.Checked {
			authOK = authProbe.OK
		}
		if failure := authProbeFailure(authProbe); failure != "" {
			r.line("    FAILED model identity check: %s", failure)
			ok, authOK = false, false
			cellProblems = append(cellProblems, "model identity check failed: "+failure)
		}
		checks["task_identity"] = nil
		local := util.Truthy(checks["local_runner"])
		if identCmd != "" && local {
			ti := once("tool\x00"+identCmd+"\x00"+shell+"\x00"+envSig(eff), func() any {
				return envcheck.IdentityResult(ctx, identCmd, ws, eff, shell)
			}).(util.M)
			checks["task_identity"] = ti
			label := fmt.Sprintf("task identity (worker tool shell, %s): ", util.Str(checks["tool_shell"]))
			if util.Truthy(ti["ok"]) {
				r.line("    %s%s", label, util.Str(ti["output"]))
			} else {
				r.line("    %sFAILED (%s): %s", label, why(ti), util.Str(ti["output"]))
				ok = false
				cellProblems = append(cellProblems, "task identity failed in the worker tool shell")
			}
		} else if identCmd != "" {
			r.line("    task identity: not checked here (runner %s runs the worker elsewhere)", rn.Name())
		} else {
			r.line("    task identity: not checked ([task] identity_cmd unset; set it for any task that uses an account)")
		}
		checks["default_aws_identity"] = nil
		checks["aws_profile_source"] = nil
		if local {
			for _, name := range []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE"} {
				if source := envcheck.StartupSource(name, eff); source != "" {
					checks["aws_profile_source"] = source
					break
				}
			}
		}
		if local && envcheck.UsesAWS(sp, auth, view) {
			d := once("tool\x00"+envcheck.DefaultAWSIdentity+"\x00"+shell+"\x00"+envSig(eff), func() any {
				return envcheck.IdentityResult(ctx, envcheck.DefaultAWSIdentity, ws, eff, shell)
			}).(util.M)
			checks["default_aws_identity"] = d
			text := ""
			if util.Truthy(d["ok"]) {
				text = util.Str(d["output"])
			} else {
				text = fmt.Sprintf("none (%s: %s)", why(d), util.Str(d["output"]))
			}
			r.line("    default AWS identity in the agent's tool shell (any aws/SDK call without --profile): %s", text)
		}
		ws2 := envcheck.Warnings(checks, auth, view)
		if w := envcheck.DefaultIdentityWarning(checks); w != "" {
			ws2 = append(ws2, w)
		}
		for _, w := range ws2 {
			r.line("    WARNING %s", w)
		}
		if len(probs) > 0 {
			r.line("    PROBLEMS: %s", spec.ReprList(probs))
			ok = false
		}
		r.line("    runner=%s %s", rn.Name(), spec.Repr(rn.Describe()))
		env := envcheck.CommandEnv(spec.WorkerEnv(sp, cell, auth), auth.Unset(), "doctor", sp)
		key := envSig(env)
		if groups[key] == nil {
			groups[key] = &cmdGroup{env: env}
			groupOrder = append(groupOrder, key)
		}
		groups[key].cells = append(groups[key].cells, util.Str(cell["id"]))
		ti, _ := checks["task_identity"].(util.M)
		groups[key].tool = append(groups[key].tool, ti)

		cellDoc["auth"] = util.M{"profile": ab.Profile, "type": auth.Name(), "env_names": strs(util.SortedKeys(auth.Env())),
			"unset": strs(auth.Unset()), "isolates_config": ab.IsolatesConfig}
		cellDoc["harness_settings"] = util.Or(checks["harness_settings"], []any{})
		cellDoc["configuration"] = checks["configuration"]
		cellDoc["shell_startup"] = checks["shell_startup"]
		cellDoc["aws_settings_sources"] = checks["aws_settings_sources"]
		cellDoc["tool_shell"] = checks["tool_shell"]
		cellDoc["auth_identity"] = nilIfEmpty(authIdent)
		cellDoc["auth_identity_ok"] = authOK
		cellDoc["auth_identity_probe"] = commandProbeDoc(authProbe)
		cellDoc["task_identity"] = identityDoc(checks["task_identity"])
		cellDoc["default_aws_identity"] = identityDoc(checks["default_aws_identity"])
		cellDoc["aws_profile_source"] = checks["aws_profile_source"]
		cellDoc["warnings"] = strs(ws2)
		cellDoc["problems"] = strs(append(cellProblems, probs...))
		cellDoc["runner"] = rn.Describe()
		r.list("cells", cellDoc)
	}
	r.doc["command_environments"] = []any{}
	if identCmd == "" && len(util.List(sp["preflight"])) == 0 {
		return ok, nil
	}
	for _, key := range groupOrder {
		g := groups[key]
		gdoc := util.M{"cells": strs(g.cells), "task_identity": nil, "preflight": []any{}, "warnings": []any{}}
		r.line("verify/teardown/preflight shell (/bin/sh -c, ajx env only; no harness settings, no shell startup "+
			"files) for cells %s", strings.Join(g.cells, ", "))
		if identCmd != "" {
			res := envcheck.IdentityResult(ctx, identCmd, ws, g.env, "")
			gdoc["task_identity"] = identityDoc(res)
			if !util.Truthy(res["ok"]) {
				r.line("    task identity: FAILED (%s): %s", why(res), util.Str(res["output"]))
				ok = false
			} else {
				differs := false
				for _, t := range g.tool {
					if t != nil && util.Truthy(t["ok"]) && util.Str(t["output"]) != util.Str(res["output"]) {
						differs = true
					}
				}
				r.line("    task identity: %s", util.Str(res["output"]))
				if differs {
					w := "this differs from the worker's tool shell identity: verify and teardown act as a " +
						"different identity than the agent and may not see what it created"
					r.line("    WARNING %s", w)
					gdoc["warnings"] = []any{w}
				}
			}
		}
		recs, err := envcheck.RunPreflight(ctx, util.Maps(sp["preflight"]), ws, g.env, envcheck.PreflightOpts{})
		if err != nil {
			return false, err
		}
		for _, rec := range recs {
			passed := util.Truthy(rec["passed"])
			text := "ok"
			if !passed {
				text = "FAILED (" + envcheck.PreflightFailure(rec) + ")"
			}
			r.line("    preflight %s: %s", util.Str(rec["name"]), text)
			gdoc["preflight"] = append(util.List(gdoc["preflight"]), util.M{"name": rec["name"], "passed": passed,
				"failure": map[bool]any{true: nil, false: envcheck.PreflightFailure(rec)}[passed]})
			ok = ok && passed
		}
		r.list("command_environments", gdoc)
	}
	return ok, nil
}

func nilIfEmptyMap(m util.M) any {
	if len(m) == 0 {
		return nil
	}
	return m
}

func identityDoc(v any) any {
	m, isMap := v.(util.M)
	if !isMap || m == nil {
		return nil
	}
	return util.M{"ok": util.Truthy(m["ok"]), "output": m["output"], "exit_code": m["exit_code"],
		"timed_out": util.Truthy(m["timed_out"]), "shell": m["shell"]}
}

func commandProbeDoc(p plugin.CommandProbe) util.M {
	var ok, code any
	if p.Checked {
		ok = p.OK
	}
	if p.ExitCode != nil {
		code = *p.ExitCode
	}
	return util.M{"checked": p.Checked, "ok": ok, "exit_code": code,
		"timed_out": p.TimedOut, "output": p.Output, "error": nilIfEmpty(p.Error)}
}

func authProbeFailure(p plugin.CommandProbe) string {
	if p.Checked && !p.OK {
		return p.Failure()
	}
	if len(envcheck.AuthErrorLines(p.Output)) > 0 {
		return "credential errors in identity output"
	}
	return ""
}

// doctorDirectory checks the existing directory or nearest existing parent.
// It does not create the configured tree; the write probe is removed at once.
func doctorDirectory(path string) string {
	for dir := path; ; dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err == nil {
			if !info.IsDir() {
				return fmt.Sprintf("%s is not a directory", dir)
			}
			probe, err := os.CreateTemp(dir, ".ajx-doctor-*")
			if err != nil {
				return fmt.Sprintf("cannot write beneath %s: %v", dir, err)
			}
			name := probe.Name()
			closeErr := probe.Close()
			removeErr := os.Remove(name)
			if closeErr != nil {
				return fmt.Sprintf("directory write probe failed: %v", closeErr)
			}
			if removeErr != nil {
				return fmt.Sprintf("cannot remove directory write probe %s: %v", name, removeErr)
			}
			return ""
		}
		if !os.IsNotExist(err) || filepath.Dir(dir) == dir {
			return fmt.Sprintf("cannot access %s: %v", dir, err)
		}
		if _, linkErr := os.Lstat(dir); linkErr == nil {
			return fmt.Sprintf("%s is a dangling link", dir)
		}
	}
}
