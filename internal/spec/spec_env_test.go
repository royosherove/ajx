package spec

// Ports of test_environment_env_conflicts.py and the spec part of
// test_coordinator_fixes.py: environment-backed cells validated at load time.
// They exercise the real environments and builtin packages; nothing launches.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/util"
)

// clearEnv replaces the process environment for one test (Python
// mock.patch.dict(os.environ, values, clear=True)).
func clearEnv(t *testing.T, values map[string]string) {
	t.Helper()
	saved := os.Environ()
	os.Clearenv()
	for k, v := range values {
		os.Setenv(k, v)
	}
	t.Cleanup(func() {
		os.Clearenv()
		for _, kv := range saved {
			if k, v, ok := strings.Cut(kv, "="); ok {
				os.Setenv(k, v)
			}
		}
	})
}

func inline(m map[string]string) string {
	var parts []string
	for _, k := range util.SortedKeys(m) {
		b, _ := json.Marshal(m[k])
		parts = append(parts, k+" = "+string(b))
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

type conflictOpts struct {
	layer, unset, backend          string
	isolated, required, configured bool
}

func conflictTrial(t *testing.T, root string, o conflictOpts) string {
	t.Helper()
	_ = os.WriteFile(filepath.Join(root, "task.md"), []byte("Synthetic trial; no provider calls."), 0o644)
	value := map[string]string{"OPENAI_API_KEY": "${AJX_TEST_API_KEY}"}
	empty := map[string]string{}
	pick := func(layer string) map[string]string {
		if o.layer == layer {
			return value
		}
		return empty
	}
	selected := ""
	if o.isolated {
		selected = "environment = \"worker\"\n"
	}
	if o.configured {
		selected += "agent_configuration = \"plain\"\n"
	}
	image := ""
	if o.backend == "container" {
		image = "image = \"sha256:" + strings.Repeat("a", 64) + "\"\n"
	}
	ws, _ := json.Marshal(filepath.Join(root, "workspaces"))
	unset, _ := json.Marshal([]string{o.unset})
	text := "[trial]\nid = \"env-conflicts\"\nproduct = \"synthetic\"\noutput_dir = \"results\"\n" +
		"workspace_root = " + string(ws) + "\n" + selected +
		"[task]\nprompt_file = \"task.md\"\n[env]\n"
	if o.layer == "trial" {
		text += "OPENAI_API_KEY = \"${AJX_TEST_API_KEY}\"\n"
	}
	text += "[environments.worker]\nbackend = \"" + o.backend + "\"\n" + image
	if o.configured {
		text += "[agent_configurations.plain.skills]\nmode = \"none\"\n"
	}
	text += "[auth.explicit]\ntype = \"env\"\nisolates_config = true\nenv = " + inline(pick("auth")) + "\nunset = " + string(unset) + "\n"
	if o.required {
		text += "required_env = [\"OPENAI_API_KEY\"]\n"
	}
	text += "[reporter]\nharness = \"codex\"\nauth = \"inherit\"\n[[cells]]\nid = \"worker\"\nharness = \"codex\"\nauth = \"explicit\"\n" +
		"env = " + inline(pick("cell")) + "\n"
	p := filepath.Join(root, "trial.toml")
	_ = os.WriteFile(p, []byte(text), 0o644)
	return p
}

func assertRejectedWithoutPreparation(t *testing.T, o conflictOpts) {
	t.Helper()
	root := Resolve(t.TempDir())
	clearEnv(t, map[string]string{"AJX_TEST_API_KEY": "synthetic-replacement", "OPENAI_API_KEY": "synthetic-ambient"})
	_, err := Load(conflictTrial(t, root, o), LoadOpts{})
	if !IsError(err) || !strings.Contains(err.Error(), "both supplied and unset") {
		t.Fatalf("%+v: want 'both supplied and unset', got %v", o, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "OPENAI_API_KEY") || strings.Contains(msg, "synthetic-replacement") || strings.Contains(msg, "synthetic-ambient") {
		t.Fatalf("message %q", msg)
	}
	for _, d := range []string{"results", "workspaces"} {
		if _, err := os.Stat(filepath.Join(root, d)); err == nil {
			t.Fatalf("%s created", d)
		}
	}
}

func TestExactAndPrefixConflictsFailForAllLayersAndBackends(t *testing.T) {
	for _, backend := range []string{"local", "container"} {
		for _, layer := range []string{"trial", "cell", "auth"} {
			for _, unset := range []string{"OPENAI_API_KEY", "OPENAI_*"} {
				t.Run(backend+"/"+layer+"/"+unset, func(t *testing.T) {
					assertRejectedWithoutPreparation(t, conflictOpts{layer: layer, unset: unset, backend: backend, isolated: true})
				})
			}
		}
	}
}

func TestRequiredForwardedVariablesCannotBeSilentlyRemoved(t *testing.T) {
	assertRejectedWithoutPreparation(t, conflictOpts{layer: "none", unset: "OPENAI_*", backend: "local", isolated: true, required: true})
}

func TestImplicitEnvironmentForAgentConfigurationAlsoRejectsConflicts(t *testing.T) {
	assertRejectedWithoutPreparation(t, conflictOpts{layer: "auth", unset: "OPENAI_*", backend: "local", configured: true})
}

func TestDisjointUnsetIsValidAndLegacyOverridePrecedenceIsPreserved(t *testing.T) {
	root := Resolve(t.TempDir())
	clearEnv(t, map[string]string{"AJX_TEST_API_KEY": "synthetic-replacement"})
	if _, err := Load(conflictTrial(t, root, conflictOpts{layer: "auth", unset: "OTHER_PROVIDER_*", backend: "local", isolated: true}), LoadOpts{}); err != nil {
		t.Fatal(err)
	}
	sp, err := Load(conflictTrial(t, root, conflictOpts{layer: "auth", unset: "OPENAI_*", backend: "local"}), LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	cell := util.Maps(sp["cells"])[0]
	auth, err := AuthFor(sp, cell, "")
	if err != nil {
		t.Fatal(err)
	}
	values := util.ChildEnv(WorkerEnv(sp, cell, auth), auth.Unset())
	if values["OPENAI_API_KEY"] != "synthetic-replacement" {
		t.Fatalf("value %q", values["OPENAI_API_KEY"])
	}
}

func bedrockTrial(t *testing.T, root string, authEnv map[string]string, unset []string) string {
	t.Helper()
	_ = os.WriteFile(filepath.Join(root, "task.md"), []byte("Synthetic trial; no provider calls.\n"), 0o644)
	ws, _ := json.Marshal(filepath.Join(root, "workspaces"))
	if unset == nil {
		unset = []string{}
	}
	u, _ := json.Marshal(unset)
	text := "[trial]\nid = \"coordinator\"\nproduct = \"synthetic\"\noutput_dir = \"results\"\n" +
		"workspace_root = " + string(ws) + "\nenvironment = \"worker\"\n" +
		"[task]\nprompt_file = \"task.md\"\n[env]\n" +
		"[environments.worker]\nbackend = \"container\"\nimage = \"sha256:" + strings.Repeat("a", 64) + "\"\n" +
		"[auth.plain]\ntype = \"claude-bedrock\"\nenv = " + inline(authEnv) + "\nunset = " + string(u) + "\n" +
		"[reporter]\nharness = \"claude-code\"\nauth = \"inherit\"\n" +
		"[[cells]]\nid = \"plain\"\nharness = \"claude-code\"\nauth = \"plain\"\nenv = {  }\n"
	p := filepath.Join(root, "trial.toml")
	_ = os.WriteFile(p, []byte(text), 0o644)
	return p
}

var missingRefs = map[string]string{"AWS_REGION": "us-east-1", "AWS_ACCESS_KEY_ID": "${AJX_TEST_MISSING_KEY}",
	"AWS_SECRET_ACCESS_KEY": "${AJX_TEST_MISSING_SECRET}"}

func TestCleanupOnlyLoadDoesNotResolveCallerSecrets(t *testing.T) {
	root := Resolve(t.TempDir())
	_ = os.MkdirAll(filepath.Join(root, "caller-home"), 0o755)
	clearEnv(t, map[string]string{"HOME": filepath.Join(root, "caller-home")})
	path := bedrockTrial(t, root, missingRefs, nil)
	_, err := Load(path, LoadOpts{})
	if !IsError(err) || !strings.Contains(err.Error(), "not ready for the selected environment") ||
		!strings.Contains(err.Error(), "${AJX_TEST_MISSING_KEY}") {
		t.Fatalf("run load: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "cell plain: auth 'plain' is not ready") {
		t.Fatalf("prefix: %v", err)
	}
	for _, o := range []LoadOpts{{CleanupOnly: true}, {SkipReadiness: true}} {
		sp, err := Load(path, o)
		if err != nil {
			t.Fatalf("%+v: %v", o, err)
		}
		if sp["_cleanup_only"] != o.CleanupOnly || util.Get(sp, "trial", "output_dir") != filepath.Join(root, "results") {
			t.Fatalf("spec %v", sp["trial"])
		}
		cell := util.Maps(sp["cells"])[0]
		auth, err := AuthFor(sp, cell, "")
		if err != nil || auth.Base().Profile != "plain" || auth.Name() != "claude-bedrock" {
			t.Fatalf("auth %v %v", auth, err)
		}
		rep, err := AuthFor(sp, M{"id": "reporter", "harness": util.Get(sp, "reporter", "harness"),
			"auth": util.Get(sp, "reporter", "auth")}, "")
		if err != nil || rep.Name() != "inherit" {
			t.Fatalf("reporter auth %v %v", rep, err)
		}
	}
	for _, d := range []string{"results", "workspaces"} {
		if _, err := os.Stat(filepath.Join(root, d)); err == nil {
			t.Fatalf("%s created", d)
		}
	}
}

func TestCleanupOnlyLoadKeepsStructuralValidation(t *testing.T) {
	root := Resolve(t.TempDir())
	clearEnv(t, map[string]string{"HOME": root})
	path := bedrockTrial(t, root, missingRefs, []string{"AWS_REGION"})
	for _, o := range []LoadOpts{{CleanupOnly: true}, {}} {
		if _, err := Load(path, o); !IsError(err) || !strings.Contains(err.Error(), "both supplied and unset") {
			t.Fatalf("%+v: %v", o, err)
		}
	}
}
