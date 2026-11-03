package builtin

// Harness-agnostic plugins: inherit/env auth, declarative harness, runners, checks.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

// ---------------------------------------------------------------- auth

// simpleAuth is an Auth with fixed extra unset names and an optional default
// identity command (the shape of most provider profiles).
type simpleAuth struct {
	*plugin.AuthBase
	baseUnset   []string // Python [...] + super().unset()
	identityCmd []string // Python conf.setdefault("identity_cmd", [...])
}

// newSimpleAuth builds the base with the class default isolates_config,
// overridden by conf["isolates_config"].
func newSimpleAuth(conf M, isolates bool) *simpleAuth {
	b := plugin.NewAuthBase(conf)
	b.IsolatesConfig = isolates
	if v, ok := b.Conf["isolates_config"]; ok {
		b.IsolatesConfig = util.Truthy(v)
	}
	return &simpleAuth{AuthBase: b}
}

func (a *simpleAuth) Unset() []string {
	out := append([]string{}, a.baseUnset...)
	return append(out, a.AuthBase.Unset()...)
}

func (a *simpleAuth) Identity(env map[string]string) (string, bool) {
	if a.identityCmd != nil {
		if _, ok := a.Conf["identity_cmd"]; !ok {
			a.Conf["identity_cmd"] = anyList(a.identityCmd)
		}
	}
	return a.AuthBase.Identity(env)
}

// providerHint is (marker var, provider label, harnesses that read it, other
// vars that provider reads).
type providerHint struct {
	marker, label string
	harnesses     []string
	vars          []string
}

var inheritHints = []providerHint{
	{"CLAUDE_CODE_USE_BEDROCK", "claude via Amazon Bedrock (AWS credentials)", []string{"claude-code"},
		append([]string{"ANTHROPIC_BEDROCK_*", "CLAUDE_CODE_SKIP_BEDROCK_AUTH"}, plugin.AWSCredentialEnv...)},
	{"CLAUDE_CODE_USE_VERTEX", "claude via Google Vertex AI", []string{"claude-code"},
		[]string{"CLOUD_ML_REGION", "ANTHROPIC_VERTEX_*", "VERTEX_REGION_*", "GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_*"}},
	{"CLAUDE_CODE_USE_FOUNDRY", "claude via Microsoft Foundry", []string{"claude-code"}, []string{"ANTHROPIC_FOUNDRY_*", "AZURE_*"}},
	{"ANTHROPIC_API_KEY", "Anthropic API key", []string{"claude-code"}, []string{"ANTHROPIC_BASE_URL"}},
	{"OPENAI_API_KEY", "OpenAI API key", []string{"codex"}, []string{"OPENAI_*"}},
	{"GEMINI_API_KEY", "Gemini API key", []string{"gemini-cli"}, nil},
	{"GOOGLE_GENAI_USE_VERTEXAI", "Gemini via Vertex AI", []string{"gemini-cli"}, []string{"GOOGLE_*", "CLOUDSDK_*"}},
	{"CURSOR_API_KEY", "Cursor API key", []string{"cursor-agent"}, nil},
	{"COPILOT_GITHUB_TOKEN", "GitHub token for Copilot", []string{"copilot-cli"}, []string{"GH_TOKEN", "GITHUB_TOKEN"}},
}

// InheritAuth is the default: the worker sees the caller's environment and
// the harness's own stored login.
//
// Config isolation is off by default because a login stored in the harness's
// home directory (Claude subscription, Kiro, Cursor...) is not visible from a
// fresh config dir. If your provider is env-driven (Bedrock, Vertex, API
// keys), set `isolates_config = true` in an [auth.*] profile of type
// "inherit" or use the matching provider type instead.
type InheritAuth struct{ *plugin.AuthBase }

func (a *InheritAuth) hints(env map[string]string) []providerHint {
	if env == nil {
		env = util.Environ()
	}
	known := plugin.Has("harness", a.Harness) // a declarative adapter may read any of them
	var out []providerHint
	for _, h := range inheritHints {
		if env[h.marker] != "" && (a.Harness == "" || !known || util.Contains(h.harnesses, a.Harness)) {
			out = append(out, h)
		}
	}
	return out
}

// ProviderHint names the providers env selects for the cell's harness.
func (a *InheritAuth) ProviderHint(env map[string]string) string {
	var labels []string
	for _, h := range a.hints(env) {
		labels = append(labels, h.label)
	}
	if len(labels) == 0 {
		return "no provider env vars set: harness stored login, if any"
	}
	return strings.Join(labels, "; ")
}

func (a *InheritAuth) Describe() M {
	out := a.AuthBase.Describe()
	out["provider_hint"] = a.ProviderHint(nil)
	return out
}

func (a *InheritAuth) ModelVars(env map[string]string) []string {
	out := a.AuthBase.ModelVars(env)
	for _, h := range a.hints(env) {
		out = append(out, h.marker)
		out = append(out, h.vars...)
	}
	return out
}

func (a *InheritAuth) Identity(env map[string]string) (string, bool) {
	if util.Truthy(a.Conf["identity_cmd"]) {
		return a.AuthBase.Identity(env)
	}
	// Only Bedrock model access has an AWS identity; an AWS_PROFILE alone belongs to the task.
	bedrock := false
	for _, h := range a.hints(env) {
		if h.marker == "CLAUDE_CODE_USE_BEDROCK" {
			bedrock = true
		}
	}
	if !bedrock || util.Which("aws") == "" {
		return "", false
	}
	source := env
	if source == nil {
		source = util.Environ()
	}
	for _, v := range []string{"AWS_BEARER_TOKEN_BEDROCK", "CLAUDE_CODE_SKIP_BEDROCK_AUTH"} {
		if source[v] != "" { // a Bedrock API key or a gateway: no SigV4 identity to show
			return fmt.Sprintf("not checked: Bedrock through %s has no STS identity", v), true
		}
	}
	return a.IdentityOf([]string{"aws", "sts", "get-caller-identity", "--output", "text"}, env)
}

// EnvAuth is the generic profile for any provider: set/unset env vars, add
// args, run an identity command.
//
//	[auth.my-gateway]
//	type = "env"
//	env = { OPENAI_BASE_URL = "https://gateway.example", OPENAI_API_KEY = "${GATEWAY_KEY}" }
//	unset = ["ANTHROPIC_API_KEY"]
//	args = []
//	required_env = ["GATEWAY_KEY"]
//	identity_cmd = ["my-cli", "whoami"]
//	isolates_config = true
//	model_env = ["MY_PROVIDER_*"]
type EnvAuth struct{ *plugin.AuthBase }

func registerGenericAuth() {
	plugin.RegisterAuth("inherit", "builtin", func(conf M) plugin.Auth {
		a := newSimpleAuth(conf, false).AuthBase
		return &InheritAuth{AuthBase: a}
	})
	plugin.RegisterAuth("env", "builtin", func(conf M) plugin.Auth {
		b := plugin.NewAuthBase(conf)
		b.Description = "generic: set/unset env vars, add CLI args, run an identity command (any provider)"
		b.RequiredEnv = util.Strings(b.Conf["required_env"])
		b.IsolatesConfig = true
		if v, ok := b.Conf["isolates_config"]; ok {
			b.IsolatesConfig = util.Truthy(v)
		}
		return &EnvAuth{AuthBase: b}
	})
}

// ---------------------------------------------------------------- declarative harness

// Declarative is any headless agent CLI described in trial.toml, no code:
//
//	[adapters.opencode]
//	execute       = ["opencode", "run", "{prompt}"]       # argv list; never a shell string
//	model_args    = ["--model", "{model}"]                 # appended when the cell sets a model
//	effort_args   = []
//	clean_args    = []                                     # present => clean supported
//	clean_env     = { XDG_CONFIG_HOME = "{config_dir}" }   # env used for config="clean"
//	resume        = ["opencode", "run", "--session", "{session_id}", "{prompt}"]  # enables self-narration
//	no_tools_args = []                                     # appended on resume to keep narration read-only
//	session_id_regex = "session[:= ]+([A-Za-z0-9_-]{8,})"  # searched in stdout+stderr
//	prompt_via    = "arg"                                  # arg | stdin
//	version       = ["opencode", "--version"]
//	strip_env     = []
//	env           = {}
//
// Placeholders: {prompt} {prompt_file} {model} {effort} {workspace}
// {config_dir} {session_id} {run_token}. {session_id} inside execute is a
// pre-generated UUID for CLIs that accept one. Telemetry: wall clock +
// stdout/stderr.
type Declarative struct {
	plugin.BaseHarness
	Conf M
}

func newDeclarative(name string, conf M) plugin.Harness {
	conf = util.MapOr(conf)
	h := &Declarative{Conf: conf}
	h.PluginName = name
	if exe := util.Strings(conf["execute"]); len(exe) > 0 {
		h.BinaryName = exe[0]
	}
	if v := util.Strings(conf["version"]); len(v) > 0 {
		h.VersionArgv = v
	}
	h.CanResume = util.Truthy(conf["resume"])
	_, cleanArgs := conf["clean_args"]
	_, cleanEnv := conf["clean_env"]
	h.CleanSupported = cleanArgs || cleanEnv
	h.StripEnv = util.Strings(conf["strip_env"])
	h.Telemetry = "wall clock and stdout only (declarative adapter)"
	return plugin.Bind(h)
}

func (h *Declarative) values(ctx *plugin.Ctx, prompt, sid string, write bool) map[string]string {
	pf := filepath.Join(ctx.RunDir, "prompt-input.txt")
	if write {
		_ = os.WriteFile(pf, []byte(prompt), 0o644)
	}
	return map[string]string{"prompt": prompt, "prompt_file": pf, "model": cellStr(ctx, "model"),
		"effort": cellStr(ctx, "effort"), "workspace": ctx.Workspace, "config_dir": ctx.ConfigDir,
		"session_id": sid, "run_token": ctx.RunToken}
}

func (h *Declarative) argv(ctx *plugin.Ctx, template []string, values map[string]string, extra []string) []string {
	argv := append([]string{}, template...)
	if cellStr(ctx, "model") != "" {
		argv = append(argv, util.Strings(h.Conf["model_args"])...)
	}
	if cellStr(ctx, "effort") != "" {
		argv = append(argv, util.Strings(h.Conf["effort_args"])...)
	}
	argv = append(argv, extra...)
	argv = append(argv, cellList(ctx, "args")...)
	return append(util.FillAll(argv, values), plugin.AuthArgs(ctx)...)
}

func (h *Declarative) env(ctx *plugin.Ctx, values map[string]string) map[string]string {
	env := map[string]string{}
	for k, v := range util.Map(h.Conf["env"]) {
		env[k] = util.Fill(util.Str(v), values)
	}
	if cellConfig(ctx) == "clean" {
		for k, v := range util.Map(h.Conf["clean_env"]) {
			env[k] = util.Fill(util.Str(v), values)
		}
	}
	return env
}

// CleanEnv is the adapter's own env/clean_env as launched, so doctor and
// prepare model the worker's env.
func (h *Declarative) CleanEnv(ctx *plugin.Ctx) map[string]string {
	return h.env(ctx, h.values(ctx, ctx.PromptText, "", false))
}

func (h *Declarative) stdin(prompt string) *string {
	if util.S(h.Conf["prompt_via"]) == "stdin" {
		return strp(prompt)
	}
	return nil
}

func (h *Declarative) findSession(ctx *plugin.Ctx, stage string, fallback any) any {
	rx := util.Str(h.Conf["session_id_regex"])
	if rx == "" {
		return fallback
	}
	s := newScope()
	var lines []string
	for _, row := range s.readJSONL(rawPath(ctx.RunDir, stage)) {
		line, _ := util.MapOr(row)["line"].(string)
		lines = append(lines, line)
	}
	text := strings.Join(lines, "\n")
	if data, err := os.ReadFile(filepath.Join(ctx.RunDir, stage+".stderr.txt")); err == nil {
		text += "\n" + validUTF8(string(data))
	} else {
		text += "\n"
	}
	re, err := regexp.Compile(rx)
	if err != nil {
		return fallback
	}
	if m := re.FindStringSubmatch(text); m != nil && len(m) > 1 {
		return m[1]
	}
	return fallback
}

func (h *Declarative) Execute(ctx *plugin.Ctx) (M, error) {
	exe := util.Strings(h.Conf["execute"])
	var pre any
	sid := ""
	if strings.Contains(strings.Join(exe, " "), "{session_id}") {
		sid = uuid4()
		pre = sid
	}
	values := h.values(ctx, ctx.PromptText, sid, true)
	var extra []string
	if cellConfig(ctx) == "clean" {
		extra = util.Strings(h.Conf["clean_args"])
	}
	argv := h.argv(ctx, exe, values, extra)
	proc, err := run(h, ctx, argv, "execute", plugin.RunOpts{StdinData: h.stdin(ctx.PromptText), ExtraEnv: h.env(ctx, values)})
	if proc == nil {
		return proc, err
	}
	proc["argv"] = safeArgv(h, ctx, argv, ctx.PromptText)
	proc["session_id"] = h.findSession(ctx, "execute", pre)
	return proc, err
}

func (h *Declarative) Narrate(ctx *plugin.Ctx, prompt string) (M, error) {
	sid := ctx.StageState("execute")["session_id"]
	if !(util.Truthy(h.Conf["resume"]) && util.Truthy(sid)) {
		return nil, nil
	}
	values := h.values(ctx, prompt, util.Str(sid), true)
	argv := h.argv(ctx, util.Strings(h.Conf["resume"]), values, util.Strings(h.Conf["no_tools_args"]))
	proc, err := run(h, ctx, argv, "narrate", plugin.RunOpts{StdinData: h.stdin(prompt), ExtraEnv: h.env(ctx, values)})
	if proc == nil {
		return proc, err
	}
	proc["argv"] = safeArgv(h, ctx, argv, prompt)
	proc["session_id"] = sid
	proc["tools_disabled"] = util.Truthy(h.Conf["no_tools_args"])
	return proc, err
}

func (h *Declarative) Normalize(ctx *plugin.Ctx, stage string, excludeMessageIDs []string) (M, error) {
	return plainTelemetry(ctx, stage, M{
		"session_id": ctx.StageState(stage)["session_id"],
		"model_ids":  []any{util.OrStr(cellStr(ctx, "model"), "harness default (unrecorded)")},
		"limitations": []any{fmt.Sprintf("%s: declarative adapter; no token or tool-call telemetry",
			h.Name())},
	}, stage != "execute"), nil
}

// plainTelemetry is the stdout-only telemetry of the declarative and copilot
// adapters: the last 80 stdout lines (or all of them when all is set).
func plainTelemetry(ctx *plugin.Ctx, stage string, overrides M, all bool) M {
	s := newScope()
	var lines []string
	for _, row := range s.readJSONL(rawPath(ctx.RunDir, stage)) {
		line, _ := util.MapOr(row)["line"].(string)
		lines = append(lines, line)
	}
	if !all && len(lines) > 80 {
		lines = lines[len(lines)-80:]
	}
	overrides["final_text"] = strings.Join(lines, "\n")
	overrides["timestamp_source"] = "ajx process start/stop and stdout arrival"
	return plugin.EmptyTelemetry(overrides)
}
