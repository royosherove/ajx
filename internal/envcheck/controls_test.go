package envcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/builtin"
	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

func TestPreparedControlsDoNotClaimObservedReads(t *testing.T) {
	f := newEnvFixture(t)
	checks, _, _ := f.inspect(t, "", "[[cells]]\nid = \"u\"\nharness = \"claude-code\"\nconfig = \"clean\"\nshell_startup = \"skip\"\n", false)
	config := util.MapOr(checks["configuration"])
	if config["auth_allows_isolation"] != false || !strings.Contains(util.Str(config["note"]), "requires caller configuration") {
		t.Fatalf("inherited authentication must explain retained configuration: %v", config)
	}
	shell := util.MapOr(checks["shell_startup"])
	if shell["evidence_scope"] != "doctor_probe" || shell["startup_files_read"] != nil {
		t.Fatalf("a doctor probe must not become worker file-read evidence: %v", shell)
	}
	if util.Compact(shell["configured_env_names"]) != `["ZDOTDIR"]` || util.Compact(shell["unset_env_names"]) != `["BASH_ENV","ENV"]` {
		t.Fatalf("missing prepared controls: %v", shell)
	}
	record := Record(checks)
	if util.Compact(record["shell_startup"]) != util.Compact(shell) || util.Compact(record["configuration"]) != util.Compact(config) {
		t.Fatalf("environment record lost control provenance: %v", record)
	}
}

func TestShellControlsOnlyRecordPreparedAdjustments(t *testing.T) {
	cell := M{"shell_startup": "skip"}
	ctx := &plugin.Ctx{Env: map[string]string{}, RunToken: "run"}
	shell := ShellStartupControls(cell, ctx)
	if len(util.List(shell["configured_env_names"])) != 0 || len(util.List(shell["unset_env_names"])) != 0 ||
		!strings.Contains(util.Str(shell["note"]), "only the listed") {
		t.Fatalf("requested settings were presented as prepared controls: %v", shell)
	}
	ctx.RunDir = "/synthetic/run"
	ctx.Env["ZDOTDIR"] = "/synthetic/user-config"
	shell = ShellStartupControls(cell, ctx)
	if len(util.List(shell["configured_env_names"])) != 0 {
		t.Fatalf("a user-supplied ZDOTDIR was mistaken for AJX redirection: %v", shell)
	}
	ctx.EnvironmentProfile = M{"backend": "local"}
	ctx.Env["ZDOTDIR"] = "/synthetic/quiet"
	ctx.UnsetEnv = []string{"BASH_ENV", "ENV"}
	shell = ShellStartupControls(cell, ctx)
	if len(util.List(shell["configured_env_names"])) != 0 || !strings.Contains(util.Str(shell["note"]), "not applied") {
		t.Fatalf("legacy controls must not be claimed for explicit environments: %v", shell)
	}
}

func TestSettingsSourcesFollowEffectivePrecedence(t *testing.T) {
	f := newEnvFixture(t)
	user := filepath.Join(f.tmp, "claude-config", "settings.json")
	managed := filepath.Join(f.tmp, "managed.json")
	for path, content := range map[string]string{
		user:    `{"env":{"AWS_PROFILE":"discarded-profile","AWS_REGION":"synthetic-region"}}`,
		managed: `{"env":{"AWS_PROFILE":"effective-profile"}}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	builtin.ClaudeManagedSettings = managed
	checks, _, _ := f.inspect(t, "", "[[cells]]\nid = \"u\"\nharness = \"claude-code\"\nconfig = \"user\"\n", false)
	got := checks["aws_settings_sources"]
	want := []any{
		M{"source": "harness settings " + user, "names": []any{"AWS_REGION"}},
		M{"source": "harness settings " + managed, "names": []any{"AWS_PROFILE"}},
	}
	if util.Compact(got) != util.Compact(want) {
		t.Fatalf("discarded settings must not claim effective provenance: %v", got)
	}
	if text := util.Compact(Record(checks)); strings.Contains(text, "discarded-profile") || strings.Contains(text, "effective-profile") {
		t.Fatal("configuration values entered the environment record")
	}
}

func TestDefaultIdentityWarningIncludesSettingsProvenance(t *testing.T) {
	sources := awsSettingsSources(M{"source": map[string]string{
		"AWS_PROFILE": "harness settings fixture.json", "AWS_REGION": "harness settings fixture.json",
		"AWS_PAGER": "harness settings unrelated.json", "OTHER": "harness settings unrelated.json",
	}})
	if len(sources) != 1 || util.Compact(util.MapOr(sources[0])["names"]) != `["AWS_PROFILE","AWS_REGION"]` {
		t.Fatalf("source records must contain only relevant names: %v", sources)
	}
	report := M{"default_aws_identity": M{"ok": true, "output": "synthetic-model"},
		"auth_identity": "synthetic-model", "aws_settings_sources": sources}
	text := DefaultIdentityWarning(report)
	for _, want := range []string{"fixture.json", "AWS_PROFILE, AWS_REGION", "does not disable these harness settings"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, "unrelated.json") || strings.Contains(text, "AWS_PAGER") {
		t.Fatalf("noncredential setting leaked into provenance: %s", text)
	}
}
