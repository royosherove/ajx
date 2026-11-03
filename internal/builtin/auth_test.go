package builtin

// Auth classes alone: provider profiles, the generic inherit/env types and
// the cloud direct-env contracts of test_environment_cloud_auth.py (with
// explicit worker environments; the spec/runner halves belong elsewhere).
// No provider CLI, network, or credential discovery.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

func newAuth(t *testing.T, name string, conf M) plugin.Auth {
	t.Helper()
	a, err := plugin.NewAuth(name, conf)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func joined(items []string) string { return strings.Join(items, " | ") }

func TestAuthUnsetEnvAndDescriptions(t *testing.T) {
	sub := newAuth(t, "claude-subscription", M{"unset": []any{"EXTRA"}})
	eq(t, sub.Unset(), []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "EXTRA"}, "unset")
	if sub.Base().IsolatesConfig {
		t.Fatal("subscription login does not survive config isolation")
	}
	if !newAuth(t, "claude-subscription", M{"isolates_config": true}).Base().IsolatesConfig {
		t.Fatal("conf overrides isolates_config")
	}
	bed := newAuth(t, "claude-bedrock", M{"env": M{"AWS_REGION": "us-east-1"}})
	eq(t, bed.Env(), map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "AWS_REGION": "us-east-1", "AWS_DEFAULT_REGION": "us-east-1"}, "bedrock env + alias")
	if !util.Contains(bed.ModelVars(nil), "AWS_DEFAULT_REGION") {
		t.Fatal("alias is a model var")
	}
	// an explicit or unset alias is left alone
	eq(t, newAuth(t, "claude-bedrock", M{"env": M{"AWS_REGION": "a", "AWS_DEFAULT_REGION": "b"}}).Env()["AWS_DEFAULT_REGION"], "b", "explicit alias")
	if _, ok := newAuth(t, "claude-bedrock", M{"env": M{"AWS_REGION": "a"}, "unset": []any{"AWS_DEFAULT_*"}}).Env()["AWS_DEFAULT_REGION"]; ok {
		t.Fatal("unset alias must not be mirrored")
	}
	eq(t, newAuth(t, "gemini-vertex", nil).Env(), map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "true"}, "gemini vertex marker")
	d := bed.Describe()
	eq(t, d["type"], "claude-bedrock", "describe type")
	eq(t, d["env_names"], []any{"AWS_DEFAULT_REGION", "AWS_REGION", "CLAUDE_CODE_USE_BEDROCK"}, "env names")
	codex := newAuth(t, "codex-provider", M{"provider": "azure", "config": []any{"a=1"}, "args": []any{"--x"}})
	eq(t, codex.ExtraArgs(), []string{"-c", "model_provider=azure", "-c", "a=1", "--x"}, "codex provider args")
}

func TestAuthFlagsUnresolvedEnvReferenceAndModelEnvString(t *testing.T) {
	os.Unsetenv("AJX_DEFINITELY_UNSET")
	a := newAuth(t, "inherit", M{"env": M{"K": "${AJX_DEFINITELY_UNSET}"}})
	if !strings.Contains(joined(a.Problems(nil)), "AJX_DEFINITELY_UNSET") {
		t.Fatal(a.Problems(nil))
	}
	mv := newAuth(t, "env", M{"model_env": "MY_*"}).ModelVars(nil)
	eq(t, mv[len(mv)-1], "MY_*", "model_env string")
	env := newAuth(t, "env", M{"required_env": []any{"AJX_T_GATEWAY"}})
	eq(t, env.Problems(map[string]string{}), []string{"missing env var AJX_T_GATEWAY"}, "required_env")
	if !env.Base().IsolatesConfig || newAuth(t, "env", M{"isolates_config": false}).Base().IsolatesConfig {
		t.Fatal("env isolates_config")
	}
	if newAuth(t, "inherit", nil).Base().IsolatesConfig {
		t.Fatal("inherit does not isolate by default")
	}
}

func TestSafeArgvRedactsAuthValuesAndPrompt(t *testing.T) {
	auth := newAuth(t, "inherit", M{"env": M{"MY_KEY": "supersecretvalue"}, "args": []any{"--token", "tok-abcdefgh"}})
	h := harness(t, "copilot-cli")
	argv := []string{"tool", "--token", "tok-abcdefgh", "--key", "supersecretvalue", "the prompt text", "-v"}
	eq(t, h.Base().SafeArgv(&plugin.Ctx{Auth: auth}, argv, []string{"the prompt text"}),
		[]string{"tool", "--token", "<redacted>", "--key", "<redacted>", "<prompt>", "-v"}, "safe argv")
}

func TestInheritProviderHintsAndIdentity(t *testing.T) {
	a := newAuth(t, "inherit", nil).(*InheritAuth)
	a.Harness = "claude-code"
	env := map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "OPENAI_API_KEY": "x"}
	eq(t, a.ProviderHint(env), "claude via Amazon Bedrock (AWS credentials)", "hint scoped to harness")
	if !util.Contains(a.ModelVars(env), "AWS_PROFILE") || util.Contains(a.ModelVars(env), "OPENAI_*") {
		t.Fatal(a.ModelVars(env))
	}
	a.Harness = "some-declarative-adapter" // unknown harness: any provider may apply
	eq(t, a.ProviderHint(env), "claude via Amazon Bedrock (AWS credentials); OpenAI API key", "unknown harness")
	eq(t, a.ProviderHint(map[string]string{}), "no provider env vars set: harness stored login, if any", "none")
	var _ ProviderHinter = a
	a.Harness = "claude-code"
	// Only Bedrock model access has an AWS identity; a bearer token or gateway has none.
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "aws"), []byte("#!/bin/sh\necho acct-$AWS_PROFILE\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, v := range []string{"AWS_BEARER_TOKEN_BEDROCK", "CLAUDE_CODE_SKIP_BEDROCK_AUTH"} {
		note, ok := a.Identity(map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", v: "1"})
		if !ok || !strings.HasPrefix(note, "not checked") {
			t.Fatal(note)
		}
	}
	note, ok := a.Identity(map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "AWS_PROFILE": "p", "PATH": os.Getenv("PATH")})
	if !ok || note != "acct-p" {
		t.Fatal(note, ok)
	}
	if _, ok := a.Identity(map[string]string{"AWS_PROFILE": "task"}); ok {
		t.Fatal("an AWS_PROFILE alone belongs to the task")
	}
	// a fake kiro-cli first on PATH: the default identity command runs it, never a real CLI
	os.WriteFile(filepath.Join(bin, "kiro-cli"), []byte("#!/bin/sh\necho \"fake-user $1\"\n"), 0o755)
	kiro := newAuth(t, "kiro-login", nil)
	if note, ok := kiro.Identity(map[string]string{"PATH": os.Getenv("PATH")}); !ok || note != "fake-user whoami" {
		t.Fatal(note, ok)
	}
	eq(t, util.Strings(kiro.Base().Conf["identity_cmd"]), []string{"kiro-cli", "whoami"}, "identity default")
}

func TestCodexChatGPTCopiesAndRemovesTheLogin(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "auth.json"), []byte(`{"synthetic": "not-a-real-credential"}`), 0o600)
	a := newAuth(t, "codex-chatgpt", M{"codex_home": src})
	if p := a.Problems(nil); len(p) != 0 {
		t.Fatal(p)
	}
	cfg := filepath.Join(t.TempDir(), "cfg")
	ctx := &plugin.Ctx{ConfigDir: cfg}
	if err := a.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(cfg, "auth.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(info, err)
	}
	if err := a.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg, "auth.json")); err == nil {
		t.Fatal("copied login left behind")
	}
	if _, err := os.Stat(filepath.Join(src, "auth.json")); err != nil {
		t.Fatal("original removed")
	}
	// a copy inside a container environment is handed to the config dir's owner
	calls := []string{}
	oldEuid, oldChown := geteuid, chown
	geteuid = func() int { return 0 }
	chown = func(name string, uid, gid int) error { calls = append(calls, name); return nil }
	defer func() { geteuid, chown = oldEuid, oldChown }()
	ctx.Runner = backendRunner{"container"}
	if err := a.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, calls, []string{filepath.Join(cfg, "auth.json")}, "chown")
	ctx.Runner = backendRunner{"local"}
	a.Prepare(ctx)
	eq(t, len(calls), 1, "no chown outside containers")
	a.Cleanup(ctx)
	missing := newAuth(t, "codex-chatgpt", M{"codex_home": filepath.Join(src, "nope")})
	eq(t, missing.Problems(nil), []string{"no codex login found at " + filepath.Join(src, "nope", "auth.json") + " (run `codex login`)"}, "problems")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	eq(t, newAuth(t, "github-token", nil).Problems(nil), []string{"none of COPILOT_GITHUB_TOKEN / GH_TOKEN / GITHUB_TOKEN is set"}, "gh")
	if p := newAuth(t, "github-token", M{"env": M{"GH_TOKEN": "x"}}).Problems(nil); len(p) != 0 {
		t.Fatal(p)
	}
}

type backendRunner struct{ backend string }

func (r backendRunner) Base() *plugin.RunnerBase { return &plugin.RunnerBase{} }
func (r backendRunner) Name() string             { return "environment" }
func (r backendRunner) LocalTranscripts() bool   { return true }
func (r backendRunner) Wrap(argv []string, ctx *plugin.Ctx, env map[string]string) ([]string, error) {
	return argv, nil
}
func (r backendRunner) Describe() M     { return M{} }
func (r backendRunner) Backend() string { return r.backend }

// ------------------------------------------------------------ cloud direct-env contracts

var cloudConfig = map[string]map[string]string{
	"claude-bedrock": {"AWS_REGION": "us-east-1"},
	"claude-vertex":  {"CLOUD_ML_REGION": "us-central1", "ANTHROPIC_VERTEX_PROJECT_ID": "synthetic-project"},
	"claude-foundry": {"ANTHROPIC_FOUNDRY_RESOURCE": "synthetic-resource"},
	"gemini-vertex":  {"GOOGLE_CLOUD_PROJECT": "synthetic-project", "GOOGLE_CLOUD_LOCATION": "us-central1"},
}

var servicePrincipal = map[string]string{"AZURE_CLIENT_ID": "synthetic-client", "AZURE_TENANT_ID": "synthetic-tenant",
	"AZURE_CLIENT_SECRET": "synthetic-client-secret"}

type route struct {
	provider string
	values   map[string]string
}

func directRoutes() []route {
	m := util.MergeEnv
	return []route{
		{"claude-bedrock", m(cloudConfig["claude-bedrock"], map[string]string{"AWS_ACCESS_KEY_ID": "synthetic-access", "AWS_SECRET_ACCESS_KEY": "synthetic-secret"})},
		{"claude-bedrock", m(cloudConfig["claude-bedrock"], map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "synthetic-bedrock-token"})},
		{"claude-foundry", m(cloudConfig["claude-foundry"], map[string]string{"ANTHROPIC_FOUNDRY_API_KEY": "synthetic-foundry-key"})},
		{"claude-foundry", m(cloudConfig["claude-foundry"], servicePrincipal)},
		{"gemini-vertex", m(cloudConfig["gemini-vertex"], map[string]string{"GOOGLE_API_KEY": "synthetic-vertex-key"})},
	}
}

var markers = map[string]string{"claude-bedrock": "CLAUDE_CODE_USE_BEDROCK", "claude-vertex": "CLAUDE_CODE_USE_VERTEX",
	"claude-foundry": "CLAUDE_CODE_USE_FOUNDRY", "gemini-vertex": "GOOGLE_GENAI_USE_VERTEXAI"}

// workerEnv is the explicit worker env an environment builds: the auth's env
// (fixed marker, declared values) plus extra layers.
func workerEnv(a plugin.Auth, extra map[string]string) map[string]string {
	return util.MergeEnv(a.Env(), extra)
}

func anyMap(m map[string]string) M { return util.AnyMap(m) }

func TestCloudCompleteDirectRoutesHaveNoProblems(t *testing.T) {
	for _, r := range directRoutes() {
		a := newAuth(t, r.provider, M{"env": anyMap(r.values)})
		if p := a.EnvironmentProblems(workerEnv(a, nil), M{"backend": "local"}); len(p) != 0 {
			t.Fatalf("%s %v: %v", r.provider, r.values, p)
		}
		// declared in another layer (trial env / cell env) works the same
		a = newAuth(t, r.provider, nil)
		if p := a.EnvironmentProblems(workerEnv(a, r.values), M{"backend": "container"}); len(p) != 0 {
			t.Fatalf("%s layer: %v", r.provider, p)
		}
	}
}

func TestCloudHomeLoginsCannotSatisfyAFreshHome(t *testing.T) {
	for provider, config := range cloudConfig {
		a := newAuth(t, provider, M{"env": anyMap(config)})
		p := joined(a.EnvironmentProblems(workerEnv(a, nil), M{}))
		if !strings.Contains(p, "fresh HOME") {
			t.Fatalf("%s: %s", provider, p)
		}
	}
	p := joined(newAuth(t, "claude-vertex", M{"env": anyMap(cloudConfig["claude-vertex"])}).EnvironmentProblems(
		util.MergeEnv(map[string]string{"CLAUDE_CODE_USE_VERTEX": "1"}, cloudConfig["claude-vertex"]), M{}))
	for _, want := range []string{"no supported direct-env credential route", "type='env'", "before archival"} {
		if !strings.Contains(p, want) {
			t.Fatal(p)
		}
	}
}

func TestCloudPartialEmptyAndUnresolvedRoutesAreRejected(t *testing.T) {
	for _, r := range directRoutes() {
		for missing := range r.values {
			partial := map[string]string{}
			for k, v := range r.values {
				if k != missing {
					partial[k] = v
				}
			}
			a := newAuth(t, r.provider, nil)
			if p := joined(a.EnvironmentProblems(workerEnv(a, partial), M{})); !strings.Contains(p, missing) {
				t.Fatalf("%s missing %s: %s", r.provider, missing, p)
			}
			for _, empty := range []string{"   ", "${MISSING_SYNTHETIC_VALUE}"} {
				p := joined(a.EnvironmentProblems(workerEnv(a, util.MergeEnv(r.values, map[string]string{missing: empty})), M{}))
				if !strings.Contains(p, missing) {
					t.Fatalf("%s %s=%q: %s", r.provider, missing, empty, p)
				}
			}
		}
	}
}

func TestCloudUnsetRemovesCredentialsAndProviderSelectionMustSurvive(t *testing.T) {
	for _, r := range directRoutes() {
		for name := range r.values {
			for _, unset := range []string{name, name[:strings.LastIndex(name, "_")] + "_*"} {
				a := newAuth(t, r.provider, M{"env": anyMap(r.values), "unset": []any{unset}})
				if len(a.EnvironmentProblems(workerEnv(a, nil), M{})) == 0 {
					t.Fatalf("%s unset %s accepted", r.provider, unset)
				}
			}
		}
		marker := markers[r.provider]
		for _, override := range []string{"", "0", "false", "${MISSING_MARKER}"} {
			a := newAuth(t, r.provider, M{"env": anyMap(util.MergeEnv(r.values, map[string]string{marker: override}))})
			if p := joined(a.EnvironmentProblems(workerEnv(a, nil), M{})); !strings.Contains(p, marker) {
				t.Fatalf("%s %s=%q: %s", r.provider, marker, override, p)
			}
		}
		a := newAuth(t, r.provider, M{"env": anyMap(r.values), "unset": []any{marker}})
		if p := joined(a.EnvironmentProblems(workerEnv(a, nil), M{})); !strings.Contains(p, marker) {
			t.Fatal(p)
		}
	}
}

func TestCloudUnsupportedSelectorsAndFoundryEndpoint(t *testing.T) {
	cases := [][2]string{{"claude-bedrock", "AWS_PROFILE"}, {"claude-bedrock", "AWS_CONTAINER_CREDENTIALS_FULL_URI"},
		{"claude-bedrock", "CLAUDE_CODE_SKIP_BEDROCK_AUTH"}, {"claude-foundry", "AZURE_CONFIG_DIR"},
		{"claude-foundry", "ANTHROPIC_FOUNDRY_AUTH_TOKEN"}, {"gemini-vertex", "GOOGLE_APPLICATION_CREDENTIALS"},
		{"gemini-vertex", "GEMINI_API_KEY"}}
	routes := map[string]map[string]string{}
	for _, r := range directRoutes() {
		if _, ok := routes[r.provider]; !ok {
			routes[r.provider] = r.values
		}
	}
	for _, c := range cases {
		values := util.MergeEnv(routes[c[0]], map[string]string{c[1]: "/synthetic/private-location"})
		a := newAuth(t, c[0], M{"env": anyMap(values)})
		p := joined(a.EnvironmentProblems(workerEnv(a, nil), M{}))
		if !strings.Contains(p, c[1]) || !strings.Contains(p, "custom auth plugin") || !strings.Contains(p, "before archival") ||
			strings.Contains(p, "/synthetic/private-location") {
			t.Fatalf("%v: %s", c, p)
		}
		// an empty selector is allowed
		values[c[1]] = ""
		a = newAuth(t, c[0], M{"env": anyMap(values)})
		if p := a.EnvironmentProblems(workerEnv(a, nil), M{}); len(p) != 0 {
			t.Fatalf("%v empty: %v", c, p)
		}
	}
	for _, creds := range []map[string]string{{"ANTHROPIC_FOUNDRY_API_KEY": "k"}, servicePrincipal} {
		a := newAuth(t, "claude-foundry", M{"env": anyMap(creds)})
		p := joined(a.EnvironmentProblems(workerEnv(a, nil), M{}))
		if !strings.Contains(p, "declare ANTHROPIC_FOUNDRY_RESOURCE or ANTHROPIC_FOUNDRY_BASE_URL in the worker env") {
			t.Fatal(p)
		}
		values := util.MergeEnv(creds, map[string]string{"ANTHROPIC_FOUNDRY_BASE_URL": "https://foundry.example.test/anthropic"})
		a = newAuth(t, "claude-foundry", M{"env": anyMap(values)})
		if p := a.EnvironmentProblems(workerEnv(a, nil), M{}); len(p) != 0 {
			t.Fatal(p)
		}
	}
}

func TestCloudDescriptionsAndProblemsDoNotExposeValues(t *testing.T) {
	for _, r := range directRoutes() {
		a := newAuth(t, r.provider, M{"env": anyMap(r.values)})
		desc := util.Compact(a.Describe())
		invalid := newAuth(t, r.provider, M{"env": anyMap(util.MergeEnv(r.values, map[string]string{markers[r.provider]: "false"}))})
		problems := joined(invalid.EnvironmentProblems(workerEnv(invalid, nil), M{}))
		for _, v := range r.values {
			if strings.Contains(desc, v) || strings.Contains(problems, v) {
				t.Fatalf("%s exposes %s", r.provider, v)
			}
		}
	}
}

func TestCloudProblemsUseTheBoundEnvironmentCheck(t *testing.T) {
	a := newAuth(t, "claude-bedrock", M{"env": anyMap(cloudConfig["claude-bedrock"])})
	a.Base().EnvironmentCheck = func() []string { return []string{"bound"} }
	eq(t, a.Problems(nil), []string{"bound"}, "bound check")
	if p := a.Problems(map[string]string{"AWS_REGION": "x"}); len(p) != 0 {
		t.Fatal("an explicit env bypasses the bound check", p)
	}
	a.Base().EnvironmentCheck = nil
	t.Setenv("AWS_REGION", "")
	if p := newAuth(t, "claude-bedrock", nil).Problems(nil); !util.Contains(p, "missing env var AWS_REGION") {
		t.Fatal(p)
	}
}
