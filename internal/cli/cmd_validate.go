package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/royosherove/ajx/internal/envcheck"
	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

func cmdValidate(ns *Namespace) (int, error) {
	r := newReport("validate", ns.Bool("json"))
	code, err := validate(ns.Str("trial"), splitList(ns.Str("cells")), r)
	if err != nil {
		return code, err
	}
	r.doc["ok"] = true
	r.doc["exit_code"] = code
	r.emit()
	return code, nil
}

// Validate runs `ajx validate` and returns its lines and document.
func Validate(trial string, cells []string) ([]string, util.M, error) {
	r := newReport("validate", true)
	_, err := validate(trial, cells, r)
	return r.lines, r.doc, err
}

func runID(cell util.M, rep int) string { return fmt.Sprintf("%s-r%d", util.Str(cell["id"]), rep) }

func validate(trial string, cells []string, r *report) (int, error) {
	sp, err := spec.Load(trial, spec.LoadOpts{})
	if err != nil {
		return 2, err
	}
	plan, err := spec.RunPlan(sp, cells)
	if err != nil {
		return 2, err
	}
	if problems := pythonPluginProblems(sp, nil); len(problems) > 0 {
		return 2, spec.Errorf("%s", strings.Join(problems, "; "))
	}
	t := util.MapOr(sp["trial"])
	task := util.MapOr(sp["task"])
	sha := util.S(sp["sha256"])
	r.line("trial %s product=%s trial_sha256=%s", util.Str(t["id"]), util.Str(t["product"]), util.Clip(sha, 16))
	r.line("prompt %s sha256=%s", util.Str(task["prompt_path"]), util.Str(task["prompt_sha256"]))
	r.line("cells=%d repetitions=%s runs=%d parallel=%s", len(util.List(sp["cells"])), util.Str(t["repetitions"]), len(plan),
		util.Str(t["parallel"]))
	r.doc["trial"] = util.M{"id": t["id"], "product": t["product"], "path": sp["path"], "sha256": sha}
	r.doc["prompt"] = util.M{"path": task["prompt_path"], "sha256": task["prompt_sha256"]}
	r.doc["repetitions"] = t["repetitions"]
	r.doc["parallel"] = t["parallel"]
	r.doc["runs"] = []any{}
	wanted := map[string]bool{}
	for _, item := range plan {
		c := item.Cell
		wanted[util.Str(c["id"])] = true
		line := fmt.Sprintf("  %s: %s model=%s config=%s auth=%s", runID(c, item.Rep), util.Str(c["harness"]),
			util.OrStr(util.S(c["model"]), "harness default"), util.Str(c["config"]), util.OrStr(util.S(c["auth"]), "default"))
		env := spec.EnvironmentFor(sp, c)
		if env != nil {
			line += fmt.Sprintf(" environment=%s agent_configuration=%s", util.OrStr(util.S(c["environment"]), "ajx-local"),
				util.OrStr(util.S(c["agent_configuration"]), "legacy"))
		}
		r.line("%s", line)
		r.list("runs", util.M{"run_id": runID(c, item.Rep), "cell": c["id"], "repetition": item.Rep, "harness": c["harness"],
			"model": c["model"], "config": c["config"], "auth": c["auth"], "environment": c["environment"],
			"agent_configuration": c["agent_configuration"], "backend": util.Get(env, "backend")})
	}
	identity := "unset"
	if util.Truthy(task["identity_cmd"]) {
		identity = "set"
	}
	r.line("verify checks=%d setup=%d teardown=%d preflight=%d task identity_cmd=%s", len(util.List(sp["verify"])),
		len(util.List(sp["setup"])), len(util.List(sp["teardown"])), len(util.List(sp["preflight"])), identity)
	r.line("output_dir=%s", util.Str(t["output_dir"]))
	r.doc["cells"] = len(util.List(sp["cells"]))
	r.doc["verify_checks"] = len(util.List(sp["verify"]))
	r.doc["setup"] = len(util.List(sp["setup"]))
	r.doc["teardown"] = len(util.List(sp["teardown"]))
	r.doc["preflight"] = len(util.List(sp["preflight"]))
	r.doc["identity_cmd_set"] = identity == "set"
	r.doc["output_dir"] = t["output_dir"]
	r.doc["warnings"] = []any{}
	for _, w := range envcheck.ShellSyntaxWarnings(sp) {
		r.line("WARNING %s", w)
		r.list("warnings", util.M{"cell": nil, "text": w})
	}
	r.doc["profiles"] = []any{}
	tmp, err := os.MkdirTemp("", "ajx-validate-")
	if err != nil {
		return 1, err
	}
	defer os.RemoveAll(tmp)
	ws, err := envcheck.ScratchWorkspace(sp, tmp)
	if err != nil {
		return 1, err
	}
	for _, cell := range util.Maps(sp["cells"]) {
		id := util.Str(cell["id"])
		if !wanted[id] {
			continue
		}
		if spec.EnvironmentFor(sp, cell) != nil {
			r.line("PROFILE %s: configuration validated; actual environment checks run during prepare", id)
			r.list("profiles", id)
			continue
		}
		auth, err := spec.AuthFor(sp, cell, "")
		if err != nil {
			return 2, err
		}
		rn, err := spec.RunnerFor(sp, cell)
		if err != nil {
			return 2, err
		}
		h, err := spec.HarnessFor(sp, util.S(cell["harness"]))
		if err != nil {
			return 2, err
		}
		pctx := envcheck.PlaceholderCtx(sp, cell, auth, rn, tmp)
		pctx.Go = context.Background()
		checks, view := envcheck.InspectCell(sp, cell, h, auth, rn, ws, pctx, envcheck.InspectOpts{})
		for _, w := range envcheck.Warnings(checks, auth, view) {
			r.line("WARNING %s: %s", id, w)
			r.list("warnings", util.M{"cell": id, "text": w})
		}
	}
	return 0, nil
}
