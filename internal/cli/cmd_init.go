package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	ajx "github.com/royosherove/ajx"
	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/res"
	"github.com/royosherove/ajx/internal/util"
	"github.com/royosherove/ajx/internal/wizard"
)

// interactive is replaceable in tests.
var interactive = Interactive

// wizardIO is the form's input/output (nil = the terminal).
var wizardIO wizard.IO

func initOptions(ns *Namespace) (wizard.Options, error) {
	o := wizard.Options{
		Dir: ns.Str("dir"), Product: ns.Str("product"), ID: ns.Str("id"), Force: ns.Bool("force"),
		Harness: ns.Str("harness"), Reporter: ns.Str("reporter"), Model: ns.Str("model"),
		ReporterModel: ns.Str("reporter_model"), Task: ns.Str("task"), Materials: ns.List("material"),
		TaskVerbatim: ns.Has("task"), CheckNames: ns.List("check_name"), ProductVersionCmd: ns.Str("product_version_cmd"),
		Checks: ns.List("check"), Timeout: ns.Int("timeout", 0), Repetitions: ns.Int("repetitions", 0),
		TimeoutSet: ns.Has("timeout"), RepetitionsSet: ns.Has("repetitions"),
		Fixture: ns.Str("fixture"), Environment: ns.Str("environment"), Image: ns.Str("image"),
	}
	if ns.Has("task_file") {
		path := ns.Str("task_file")
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return o, usageErr(ns, "--task-file must name a readable regular file")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return o, usageErr(ns, "cannot read --task-file: "+err.Error())
		}
		if !utf8.Valid(data) || strings.TrimSpace(string(data)) == "" {
			return o, usageErr(ns, "--task-file must contain nonempty UTF-8 text")
		}
		o.Task, o.TaskFile, o.TaskVerbatim = string(data), path, true
	}
	if ns.Has("max_budget_usd") {
		budget, err := strconv.ParseFloat(strings.TrimSpace(ns.Str("max_budget_usd")), 64)
		if err != nil {
			return o, usageErr(ns, "--max-budget-usd must be a finite positive number")
		}
		o.MaxBudgetUSD = &budget
	}
	return o, nil
}

func cmdInit(ns *Namespace) (int, error) {
	plugin.Load("")
	noInput := ns.Bool("no_input") || ns.Bool("yes") || ns.Bool("json")
	canAsk := !noInput && interactive()
	if ns.Bool("wizard") && !canAsk {
		return 2, usageErr(ns, "--wizard needs a terminal on stdin and stdout, CI unset and no --no-input/--yes/--json; pass the settings as flags instead")
	}
	o, err := initOptions(ns)
	if err != nil {
		return 2, err
	}
	asked := false
	if canAsk && (ns.Bool("wizard") || len(wizard.Missing(o)) > 0) {
		if err := wizard.Ask(&o, wizardIO); err != nil {
			if errors.Is(err, wizard.ErrAborted) {
				fmt.Fprintln(Stderr, err.Error())
				return 1, nil
			}
			return 1, err
		}
		asked = true
	} else if err := ns.RequireDeferred(nil); err != nil {
		return 2, err
	}
	if err := wizard.Resolve(&o); err != nil {
		return 2, err
	}
	if asked {
		ok, err := wizard.Confirm(o, wizardIO)
		if err != nil {
			return 1, err
		}
		if !ok {
			fmt.Fprintln(Stderr, wizard.ErrAborted.Error())
			return 1, nil
		}
	}
	r, err := wizard.Write(o)
	if err != nil {
		return 1, err
	}
	for _, w := range wizard.Warnings(o) {
		fmt.Fprintln(Stderr, "note: "+w)
	}
	headless := noInput || !canAsk
	missing := []string{}
	if !r.TaskSet {
		missing = append(missing, "describe the task in "+r.PromptFile)
	}
	if r.VerifyChecks == 0 {
		missing = append(missing, "add a [[verify]] check of the requested outcome to "+r.TrialFile)
	}
	lastAction, lastPath := "console", r.Dir
	if headless {
		lastAction, lastPath = "run", r.TrialFile
	}
	nextArgv := [][]string{{"ajx", "validate", r.TrialFile}, {"ajx", "doctor", r.TrialFile}, {"ajx", lastAction, lastPath}}
	nextCommands := []string{}
	for _, argv := range nextArgv {
		nextCommands = append(nextCommands, initCommand(argv[1], argv[2]))
	}
	code := 0
	var problem any
	if !r.Valid {
		code = 2
		problem = util.M{"type": "trial_file_error", "message": r.Problem}
		missing = []string{"fix trial.toml: " + r.Problem}
		nextCommands, nextArgv = []string{}, [][]string{}
		fmt.Fprintln(Stderr, "trial file error: "+r.Problem)
	}
	next := "Next: "
	if len(missing) > 0 {
		next += strings.Join(missing, " and ") + ". "
	}
	if r.Valid && headless {
		next += "Validate with " + nextCommands[0] + ", then check readiness with " + nextCommands[1] +
			". When ready and authorized, use " + nextCommands[2] + "; use " + initCommand("ci", r.TrialFile) + " instead for acceptance checks."
	} else if r.Valid {
		next += "review and start it in the browser with: " + nextCommands[2]
	}
	if ns.Bool("json") {
		emitJSON(util.M{"schema_version": SchemaVersion, "command": "init", "ok": r.Valid, "exit_code": code, "dir": r.Dir,
			"trial_file": r.TrialFile, "prompt_file": r.PromptFile, "written": strs(r.Written), "kept": strs(r.Kept),
			"prompt_sha256": r.PromptSHA256,
			"trial_id":      nilIfEmpty(r.ID), "harness": nilIfEmpty(r.Harness), "harnesses": strs(r.Harnesses),
			"reporter": nilIfEmpty(r.Reporter), "valid": r.Valid, "problem": nilIfEmpty(r.Problem), "error": problem,
			"next": nextCommands, "next_argv": nextArgv, "missing": missing,
			"run_requires": []string{"complete the task and success checks", "doctor reports ready", "the trial scope is authorized"}})
		return code, nil
	}
	for _, path := range []string{r.TrialFile, r.PromptFile} {
		if util.Contains(r.Kept, path) {
			outf("exists, not overwritten: %s\n", path)
		} else {
			outf("wrote %s\n", path)
		}
	}
	if ns.Has("task_file") {
		outf("task-prompt.md sha256: %s\n", r.PromptSHA256)
	}
	outln(next)
	if !r.Valid {
		return code, nil
	}
	if !asked {
		return 0, nil
	}
	if !wizardIO.Accessible && os.Getenv("ACCESSIBLE") == "" {
		// The full-screen form clears its Review step; keep the plan on screen.
		fmt.Fprintln(Stdout)
		fmt.Fprintln(Stdout, wizard.Summary(o, ColorEnabled(os.Stdout)))
	}
	step, err := wizard.NextStep(wizardIO)
	if err != nil {
		return 1, err
	}
	switch step {
	case wizard.NextConsole:
		return consoleOpen(r.TrialFile, consoleOpenOpts{OpenBrowser: true})
	case wizard.NextDoctor:
		return runDoctor(r.TrialFile, false, false)
	}
	return 0, nil
}

func initCommand(action, path string) string {
	// Always quote the absolute path. Quoting "~" would suppress tilde
	// expansion; an absolute path also works for argv-based callers.
	return "ajx " + action + " '" + strings.ReplaceAll(path, "'", "'\"'\"'") + "'"
}

func usageErr(ns *Namespace, msg string) error { return &UsageError{ns.Leaf, msg} }

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

// ---------------------------------------------------------------- example, version, completion

func cmdExample(ns *Namespace) (int, error) {
	dir, err := wizard.ResolvePath(ns.Str("dir"))
	if err != nil {
		return 2, err
	}
	output, err := demoGenerate(dir)
	if err != nil {
		fmt.Fprintln(Stderr, err.Error())
		return 2, nil
	}
	outf("Fictional example; no agents or model providers were called.\n%s\n", filepath.Join(output, "index.html"))
	return 0, nil
}

// Build is set by the linker (-ldflags "-X github.com/royosherove/ajx/internal/cli.Build=...").
var Build = ""

func cmdVersion(ns *Namespace) (int, error) {
	if ns.Bool("json") {
		emitJSON(util.M{"schema_version": SchemaVersion, "command": "version", "version": ajx.Version,
			"implementation": "go", "build": nilIfEmpty(Build), "skill_fingerprint": res.Fingerprint(),
			"skill_resources": res.Root()})
		return 0, nil
	}
	line := "AJX " + ajx.Version + " (go"
	if Build != "" {
		line += ", " + Build
	}
	outln(line + ")")
	outf("skill %s (%s)\n", res.Fingerprint(), res.Root())
	return 0, nil
}
