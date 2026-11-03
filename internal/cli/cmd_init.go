package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

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

func initOptions(ns *Namespace) wizard.Options {
	return wizard.Options{
		Dir: ns.Str("dir"), Product: ns.Str("product"), ID: ns.Str("id"), Force: ns.Bool("force"),
		Harness: ns.Str("harness"), Reporter: ns.Str("reporter"), Model: ns.Str("model"),
		ReporterModel: ns.Str("reporter_model"), Task: ns.Str("task"), Materials: ns.List("material"),
		Checks: ns.List("check"), Timeout: ns.Int("timeout", 0), Repetitions: ns.Int("repetitions", 0),
		Fixture: ns.Str("fixture"), Environment: ns.Str("environment"), Image: ns.Str("image"),
	}
}

func cmdInit(ns *Namespace) (int, error) {
	plugin.Load("")
	noInput := ns.Bool("no_input") || ns.Bool("yes") || ns.Bool("json")
	canAsk := !noInput && interactive()
	if ns.Bool("wizard") && !canAsk {
		return 2, usageErr(ns, "--wizard needs a terminal on stdin and stdout, CI unset and no --no-input/--yes/--json; pass the settings as flags instead")
	}
	o := initOptions(ns)
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
	short := shortHome(r.Dir)
	extended := o.Task != "" || len(o.Checks) > 0
	next := fmt.Sprintf("Next: describe the task in task-prompt.md and add a [[verify]] check, then review and start it in the "+
		"browser with: ajx console %s", short)
	switch {
	case o.Task != "" && len(o.Checks) > 0:
		next = "Next: review and start it in the browser with: ajx console " + short
	case o.Task != "":
		next = "Next: add a [[verify]] check of the requested outcome to trial.toml, then review and start it in the browser with: ajx console " + short
	case len(o.Checks) > 0:
		next = "Next: describe the task in task-prompt.md, then review and start it in the browser with: ajx console " + short
	}
	if ns.Bool("json") {
		emitJSON(util.M{"schema_version": SchemaVersion, "command": "init", "ok": true, "dir": r.Dir,
			"trial_file": r.TrialFile, "prompt_file": r.PromptFile, "written": strs(r.Written), "kept": strs(r.Kept),
			"trial_id": r.ID, "harness": o.Harness, "reporter": o.Reporter, "valid": r.Valid, "problem": nilIfEmpty(r.Problem),
			"next": []any{"ajx validate " + r.TrialFile, "ajx doctor " + r.TrialFile, "ajx console " + short}})
		return 0, nil
	}
	for _, path := range []string{r.TrialFile, r.PromptFile} {
		if util.Contains(r.Kept, path) {
			outf("exists, not overwritten: %s\n", path)
		} else {
			outf("wrote %s\n", path)
		}
	}
	outln(next)
	if !r.Valid && (extended || asked) {
		fmt.Fprintf(Stderr, "note: the trial does not load yet: %s\n", r.Problem)
	}
	if !asked {
		return 0, nil
	}
	if !r.Valid {
		// Never hand an unloadable trial to the console or doctor as if it were ready.
		fmt.Fprintf(Stderr, "Fix trial.toml first (or run ajx init %s --force again), then: ajx console %s\n", short, short)
		return 1, nil
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
		return runDoctor(r.TrialFile, false)
	}
	return 0, nil
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
