package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ajx "github.com/royosherove/ajx"
	"github.com/royosherove/ajx/internal/update"
	"github.com/royosherove/ajx/internal/wizard"
)

func TestUpdateScopeCheckCancellationAndHeadlessContract(t *testing.T) {
	offline(t)
	oldLatest, oldInstall, oldChoice, oldInteractive := latestRelease, installUpdate, chooseUpdate, interactive
	defer func() {
		latestRelease, installUpdate, chooseUpdate, interactive = oldLatest, oldInstall, oldChoice, oldInteractive
	}()
	latestRelease = func() (update.Release, error) {
		return update.Release{Schema: 1, Version: ajx.Version, Commit: strings.Repeat("a", 40), SkillHash: strings.Repeat("b", 64)}, nil
	}
	interactive = func() bool { return false }
	var called, cliOnly, replace bool
	installUpdate = func(r update.Release, options updateInstallOptions, stdout, stderr io.Writer) error {
		called, cliOnly, replace = true, options.CLIOnly, options.Replace
		fmt.Fprintln(stdout, "synthetic installer")
		return nil
	}
	for _, tc := range []struct {
		args                  []string
		code                  int
		install, cli, replace bool
	}{
		{[]string{"update", "--check", "--json"}, 0, false, false, false},
		{[]string{"update", "--json"}, 2, false, false, false},
		{[]string{"update", "--no-input"}, 2, false, false, false},
		{[]string{"update", "--cli-only", "--json"}, 0, true, true, false},
		{[]string{"update", "--with-skills", "--json", "--replace"}, 0, true, false, true},
		{[]string{"update", "--yes"}, 0, true, false, false},
		{[]string{"update", "--cli-only", "--with-skills"}, 2, false, false, false},
	} {
		called, cliOnly, replace = false, false, false
		code, output, errors := run(t, tc.args...)
		if code != tc.code || called != tc.install || cliOnly != tc.cli || replace != tc.replace {
			t.Fatalf("%v: %d %v %v %v %s %s", tc.args, code, called, cliOnly, replace, output, errors)
		}
		if strings.Contains(strings.Join(tc.args, " "), "--json") {
			doc := decode(t, output)
			if doc["command"] != "update" {
				t.Fatalf("invalid JSON output: %s", output)
			}
		}
	}
	interactive = func() bool { return true }
	for _, choice := range []string{"cancel", "cli", "all"} {
		called = false
		chooseUpdate = func(summary string, x wizard.IO) (string, error) {
			if !strings.Contains(summary, "Skill:") {
				t.Errorf("missing skill state: %s", summary)
			}
			return choice, nil
		}
		code, out, errors := run(t, "update")
		if code != 0 || called != (choice != "cancel") || (called && cliOnly != (choice == "cli")) {
			t.Fatalf("%s: %d %s %s", choice, code, out, errors)
		}
	}
}

func TestUpdateFailureDoesNotReportSuccess(t *testing.T) {
	offline(t)
	oldLatest := latestRelease
	defer func() { latestRelease = oldLatest }()
	latestRelease = func() (update.Release, error) { return update.Release{}, fmt.Errorf("synthetic offline failure") }
	code, out, _ := run(t, "update", "--check", "--json")
	if code != 1 || decode(t, out)["ok"] != false {
		t.Fatalf("failed check reported success: %d %s", code, out)
	}
}

func TestUpdateStillOffersSkillWhenCLIIsCurrent(t *testing.T) {
	home := offline(t)
	path := filepath.Join(home, ".local/share/ajx/current/skill")
	for _, part := range []string{"prompts", "schemas", "references", "assets", "examples"} {
		if err := os.MkdirAll(filepath.Join(path, part), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte("previous instructions"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldLatest, oldInstall, oldChoice, oldInteractive, oldBuild := latestRelease, installUpdate, chooseUpdate, interactive, Build
	defer func() {
		latestRelease, installUpdate, chooseUpdate, interactive, Build = oldLatest, oldInstall, oldChoice, oldInteractive, oldBuild
	}()
	Build = strings.Repeat("a", 40)
	release := update.Release{Schema: 1, Version: ajx.Version, Commit: Build, SkillHash: strings.Repeat("b", 64)}
	latestRelease = func() (update.Release, error) { return release, nil }
	interactive = func() bool { return true }
	called, prompted, choice := false, false, "cli"
	chooseUpdate = func(summary string, x wizard.IO) (string, error) {
		prompted = true
		if !strings.Contains(summary, "updated skill bundle") {
			t.Errorf("missing skill update explanation: %s", summary)
		}
		return choice, nil
	}
	installUpdate = func(r update.Release, options updateInstallOptions, stdout, stderr io.Writer) error {
		called = true
		return nil
	}
	code, out, _ := run(t, "update")
	if code != 0 || !prompted || called || !strings.Contains(out, "skill was left unchanged") {
		t.Fatalf("CLI-only current version: code=%d prompted=%v called=%v %s", code, prompted, called, out)
	}
	choice = "all"
	code, out, _ = run(t, "update")
	if code != 0 || !called {
		t.Fatalf("skill-only update was not applied: %d %s", code, out)
	}
	release.SkillHash, _ = update.SkillHash(os.DirFS(path))
	called, prompted = false, false
	code, out, _ = run(t, "update")
	if code != 0 || called || prompted || !strings.Contains(out, "up to date") {
		t.Fatalf("up-to-date installation prompted or changed: %d %s", code, out)
	}
}

func TestUpdateCompletionDelegatesHumanOutputAndPreservesJSON(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cliOnly bool
	}{
		{"CLI and skills", false},
		{"CLI only", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			offline(t)
			oldLatest, oldInstall, oldBuild := latestRelease, installUpdate, Build
			t.Cleanup(func() { latestRelease, installUpdate, Build = oldLatest, oldInstall, oldBuild })
			Build = "previous-build"
			release := update.Release{Schema: 1, Version: ajx.Version, Commit: strings.Repeat("a", 40), SkillHash: strings.Repeat("b", 64)}
			latestRelease = func() (update.Release, error) { return release, nil }
			jsonOutput := false
			installUpdate = func(r update.Release, options updateInstallOptions, stdout, stderr io.Writer) error {
				if options.CLIOnly != tc.cliOnly || options.NoSummary != jsonOutput {
					t.Errorf("incorrect installer options: %+v", options)
				}
				fmt.Fprintln(stdout, "Downloading the update...")
				fmt.Fprintln(stdout, "Preserved previous installation: synthetic-backup")
				if !options.NoSummary {
					fmt.Fprintln(stdout, "Update complete: synthetic installer summary")
				}
				return nil
			}
			flag := "--with-skills"
			if tc.cliOnly {
				flag = "--cli-only"
			}
			code, output, errors := run(t, "update", flag)
			if code != 0 || errors != "" {
				t.Fatalf("update failed: %d %s %s", code, output, errors)
			}
			for _, want := range []string{"Update complete: synthetic installer summary",
				"Preserved previous installation: synthetic-backup"} {
				if !strings.Contains(output, want) {
					t.Errorf("missing %q in completion:\n%s", want, output)
				}
			}
			if strings.Count(output, "Update complete") != 1 {
				t.Errorf("expected one completion summary:\n%s", output)
			}
			for _, unwanted := range []string{release.Commit, release.SkillHash, "PATH", "Custom Kiro", "\x1b["} {
				if strings.Contains(output, unwanted) {
					t.Errorf("unexpected %q in human completion:\n%s", unwanted, output)
				}
			}

			jsonOutput = true
			code, output, errors = run(t, "update", flag, "--json")
			doc := decode(t, output) // Rejects extra logo, progress, or boxed text on stdout.
			if code != 0 || doc["ok"] != true || doc["updated"] != true {
				t.Fatalf("invalid JSON result: %d %s", code, output)
			}
			scope := "all"
			if tc.cliOnly {
				scope = "cli"
			}
			if doc["scope"] != scope {
				t.Errorf("incorrect JSON scope: %s", output)
			}
			status := doc["status"].(map[string]any)
			details := status["release"].(map[string]any)
			if details["commit"] != release.Commit || details["skill_sha256"] != release.SkillHash {
				t.Errorf("agent metadata was lost: %s", output)
			}
			if !strings.Contains(errors, "synthetic-backup") || strings.Contains(output, "Update complete") {
				t.Errorf("installer diagnostics must remain separate from JSON: %s %s", output, errors)
			}
		})
	}
}

func TestFailedInstallerNeverPrintsCompletion(t *testing.T) {
	offline(t)
	oldLatest, oldInstall, oldBuild := latestRelease, installUpdate, Build
	t.Cleanup(func() { latestRelease, installUpdate, Build = oldLatest, oldInstall, oldBuild })
	Build = "previous-build"
	latestRelease = func() (update.Release, error) {
		return update.Release{Schema: 1, Version: ajx.Version, Commit: strings.Repeat("a", 40), SkillHash: strings.Repeat("b", 64)}, nil
	}
	installUpdate = func(r update.Release, options updateInstallOptions, stdout, stderr io.Writer) error {
		fmt.Fprintln(stderr, "synthetic installation error")
		return fmt.Errorf("installer failed")
	}
	for _, args := range [][]string{{"update", "--yes"}, {"update", "--yes", "--json"}} {
		code, output, errors := run(t, args...)
		if code != 1 || !strings.Contains(errors, "synthetic installation error") ||
			strings.Contains(output, "Update complete") || strings.Contains(output, "is ready") {
			t.Fatalf("failed installer reported success: %d %s %s", code, output, errors)
		}
		if args[len(args)-1] == "--json" && decode(t, output)["ok"] != false {
			t.Fatalf("failed installer returned success JSON: %s", output)
		}
	}
}
