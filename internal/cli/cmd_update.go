package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	ajx "github.com/royosherove/ajx"
	"github.com/royosherove/ajx/internal/update"
	"github.com/royosherove/ajx/internal/util"
	"github.com/royosherove/ajx/internal/wizard"
)

var latestRelease = func() (update.Release, error) {
	return update.Latest(update.Client(), update.Releases)
}
var chooseUpdate = wizard.UpdateChoice
var installUpdate = runUpdateInstaller

type updateInstallOptions struct {
	CLIOnly   bool
	Replace   bool
	NoSummary bool
}

func cmdUpdate(ns *Namespace) (int, error) {
	if ns.Bool("cli_only") && ns.Bool("with_skills") {
		return 2, usageErr(ns, "choose --cli-only or --with-skills, not both")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return 1, err
	}
	release, err := latestRelease()
	if err != nil {
		return 1, err
	}
	state, err := update.Inspect(release, ajx.Version, Build, home)
	if err != nil {
		return 1, err
	}
	summary := fmt.Sprintf("CLI: %s → %s.", state.CurrentVersion, release.Version)
	switch {
	case state.Ahead:
		summary = "Your CLI is newer than the latest stable release; no downgrade will be performed."
	case !state.SkillInstalled:
		summary += "\nSkill: not managed by AJX yet. CLI + skill installs it for Codex, Claude Code and Kiro."
	case state.SkillAvailable:
		summary += "\nAn updated skill bundle is available (or your installed skill differs)."
	default:
		summary += "\nYour installed skill already matches this release."
	}
	document := util.M{"schema_version": SchemaVersion, "command": "update", "ok": true,
		"exit_code": 0, "status": state, "updated": false}
	emit := func(message string) {
		if ns.Bool("json") {
			document["message"] = message
			emitJSON(document)
		} else {
			outln(message)
		}
	}
	if ns.Bool("check") || state.Ahead || (!state.CLIAvailable && !state.SkillAvailable) {
		if !state.Ahead && !state.CLIAvailable && !state.SkillAvailable {
			summary = "AJX CLI and skill are up to date (" + release.Version + ")."
		}
		emit(summary)
		return 0, nil
	}
	scope := "all"
	if ns.Bool("cli_only") {
		scope = "cli"
	} else if !ns.Bool("yes") && !ns.Bool("with_skills") {
		if ns.Bool("json") || ns.Bool("no_input") || !interactive() {
			return 2, usageErr(ns, "an update is available; choose --with-skills (or --yes) for CLI + skill, --cli-only, or --check")
		}
		scope, err = chooseUpdate(summary+"\nPrevious managed versions are preserved.", wizardIO)
		if err != nil {
			return 1, err
		}
	}
	if scope == "cancel" {
		emit("Update cancelled; nothing changed.")
		return 0, nil
	}
	if scope == "cli" && !state.CLIAvailable {
		emit("The CLI is up to date. Your skill was left unchanged; use ajx update --with-skills to update it.")
		return 0, nil
	}
	writer := Stdout
	if ns.Bool("json") {
		writer = Stderr
	}
	options := updateInstallOptions{
		CLIOnly: scope == "cli", Replace: ns.Bool("replace"), NoSummary: ns.Bool("json"),
	}
	if err := installUpdate(release, options, writer, Stderr); err != nil {
		return 1, err
	}
	document["updated"] = true
	document["scope"] = scope
	if ns.Bool("json") {
		emit("Updated AJX to " + release.Version + ". " + map[bool]string{
			true:  "Your installed skill was left unchanged.",
			false: "Start a fresh agent session to load the updated skill.",
		}[scope == "cli"])
	}
	return 0, nil
}

func runUpdateInstaller(release update.Release, options updateInstallOptions, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	// A manually copied executable cannot replace itself through the managed
	// installer. Explain the destination instead of reporting a false update.
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	versions, err := filepath.EvalSymlinks(filepath.Join(home, ".local", "share", "ajx", "versions"))
	if err != nil {
		return fmt.Errorf("this CLI is not a managed installation; use the README one-line installer first")
	}
	rel, err := filepath.Rel(versions, executable)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("this CLI is not running from the managed AJX installation; run %s update instead, or use the README installer", filepath.Join(home, ".local", "bin", "ajx"))
	}
	dir, err := os.MkdirTemp("", "ajx-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	script := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(script, []byte(ajx.Installer), 0o600); err != nil {
		return err
	}
	args := []string{script, "--version", "v" + release.Version, "--no-path", "--update", "--compact-summary"}
	if options.NoSummary {
		args = append(args, "--no-summary")
	}
	if options.CLIOnly {
		args = append(args, "--no-skills")
	}
	if options.Replace {
		args = append(args, "--replace")
	}
	command := exec.Command("/bin/bash", args...)
	command.Stdout, command.Stderr = stdout, stderr
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "BASH_ENV=") && !strings.HasPrefix(value, "ENV=") {
			command.Env = append(command.Env, value)
		}
	}
	if err := command.Run(); err != nil {
		return fmt.Errorf("AJX update did not complete: %w", err)
	}
	return nil
}
