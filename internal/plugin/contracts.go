package plugin

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/royosherove/ajx/internal/util"
)

// ErrNotImplemented marks an optional contract method an adapter lacks.
var ErrNotImplemented = errors.New("not implemented")

// SettingsLayer is env a harness applies from its own settings files.
type SettingsLayer struct {
	Source string
	Env    map[string]string
}

// ---------------------------------------------------------------- harness

// Harness is an agent harness adapter (Python base.Harness).
type Harness interface {
	Base() *BaseHarness
	Name() string
	Available() bool
	// Version is the first line of the version command; "" when n/a.
	Version() string
	// CleanEnv isolates harness config for config='clean' (e.g. a per-run home).
	CleanEnv(ctx *Ctx) map[string]string
	// SettingsEnv is env the harness applies from its own settings on top of
	// the process env, lowest precedence first.
	SettingsEnv(env map[string]string, projectDir string) []SettingsLayer
	// ToolShell is the shell the agent's tool commands run in (`<shell> -c`).
	ToolShell(env map[string]string) string
	// Plan returns facts knowable before launch (e.g. a pre-chosen session id).
	Plan(ctx *Ctx) M
	Execute(ctx *Ctx) (M, error)
	// Narrate resumes the worker's session read-only; (nil, nil) = unsupported.
	Narrate(ctx *Ctx, prompt string) (M, error)
	// Report returns M{proc, text, structured} from a fresh restricted session.
	Report(ctx *Ctx, prompt string, schema M) (M, error)
	Normalize(ctx *Ctx, stage string, excludeMessageIDs []string) (M, error)
	// Progress returns recent events for live status: M{at, kind, text}.
	Progress(runDir, stage string, limit int) []M
}

// BaseHarness holds the class attributes and default behavior of a harness.
type BaseHarness struct {
	Self              Harness
	PluginName        string
	BinaryName        string
	VersionArgv       []string
	CanResume         bool // can re-enter the worker's own session to self-narrate
	CanReport         bool // fresh reporting call with restricted tool permissions
	CleanSupported    bool // can run with user skills/memory/MCP/instructions disabled
	VerifiedLive      bool // validated against a real installed CLI
	DefaultAuth       string
	StripEnv          []string // env vars (or 'PREFIX*') coupling a child to a parent agent session
	Telemetry         string   // human-readable summary for doctor/report
	ToolShellObserved bool
	ToolShellRestores []string
}

// Bind sets the Self pointer of a hand-built harness and returns it.
func Bind(h Harness) Harness {
	h.Base().Self = h
	return h
}

func (b *BaseHarness) self() Harness {
	if b.Self != nil {
		return b.Self
	}
	return b
}

func (b *BaseHarness) Base() *BaseHarness { return b }
func (b *BaseHarness) Name() string {
	if b.PluginName == "" {
		return "base"
	}
	return b.PluginName
}
func (b *BaseHarness) Available() bool {
	return b.BinaryName != "" && util.Which(b.BinaryName) != ""
}
func (b *BaseHarness) Version() string {
	if len(b.VersionArgv) == 0 || !b.self().Available() {
		return ""
	}
	return VersionOf(b.VersionArgv)
}
func (b *BaseHarness) CleanEnv(ctx *Ctx) map[string]string                   { return map[string]string{} }
func (b *BaseHarness) SettingsEnv(map[string]string, string) []SettingsLayer { return nil }
func (b *BaseHarness) Plan(ctx *Ctx) M                                       { return M{} }
func (b *BaseHarness) Narrate(ctx *Ctx, prompt string) (M, error)            { return nil, nil }
func (b *BaseHarness) Progress(runDir, stage string, limit int) []M          { return nil }
func (b *BaseHarness) Execute(ctx *Ctx) (M, error)                           { return nil, ErrNotImplemented }
func (b *BaseHarness) Normalize(ctx *Ctx, stage string, ex []string) (M, error) {
	return nil, ErrNotImplemented
}
func (b *BaseHarness) Report(ctx *Ctx, prompt string, schema M) (M, error) {
	return nil, fmt.Errorf("%s has no reporter adapter", b.self().Name())
}

// ToolShell defaults to $SHELL (assumed, not observed).
func (b *BaseHarness) ToolShell(env map[string]string) string {
	if s := env["SHELL"]; s != "" {
		return s
	}
	return "/bin/sh"
}

// DefaultAuthName is the harness's default auth type ("inherit" when unset).
func (b *BaseHarness) DefaultAuthName() string {
	if b.DefaultAuth == "" {
		return "inherit"
	}
	return b.DefaultAuth
}

// TelemetrySummary is the telemetry label ("wall clock only" when unset).
func (b *BaseHarness) TelemetrySummary() string {
	if b.Telemetry == "" {
		return "wall clock only"
	}
	return b.Telemetry
}

// RunOpts configures BaseHarness.Run.
type RunOpts struct {
	StdinData *string
	Timeout   int // seconds; 0 = ctx default (narrate: ctx.NarrateTimeout or 900)
	ExtraEnv  map[string]string
}

// Run launches argv for stage, capturing stdout/stderr under ctx.RunDir. The
// returned proc carries a redacted argv (secrets and prompts never reach
// state.json). Interruption returns util.ErrInterrupted after aborting a
// managed environment's worker.
func (b *BaseHarness) Run(ctx *Ctx, argv []string, stage string, o RunOpts) (M, error) {
	env := util.MergeEnv(ctx.Env, o.ExtraEnv)
	drop := append(append([]string{}, b.StripEnv...), ctx.UnsetEnv...)
	environment := ctx.EnvironmentProfile != nil
	mr, managed := ctx.Runner.(ManagedRunner)
	var fullEnv map[string]string
	if checked, ok := ctx.Runner.(CheckedManagedRunner); environment && ok {
		var err error
		if fullEnv, err = checked.ChildEnvChecked(env, drop); err != nil {
			return nil, err
		}
	} else if environment && managed {
		fullEnv = mr.ChildEnv(env, drop)
	} else {
		fullEnv = util.ChildEnv(env, drop)
	}
	if environment && ctx.Auth != nil {
		if problems := ctx.Auth.EnvironmentProblems(fullEnv, ctx.EnvironmentProfile); len(problems) > 0 {
			return nil, fmt.Errorf("environment authentication configuration is not ready: %s", strings.Join(problems, "; "))
		}
	}
	fullArgv, err := ctx.Runner.Wrap(argv, ctx, env)
	if err != nil {
		return nil, err
	}
	timeout := o.Timeout
	if stage == "narrate" && timeout == 0 {
		timeout = ctx.NarrateTimeout
		if timeout == 0 {
			timeout = 900
		}
	}
	if timeout == 0 {
		timeout = ctx.Timeout
	}
	proc, err := util.RunStreaming(ctx.Context(), fullArgv, ctx.Workspace,
		ctx.RunDir+"/"+stage+".raw.jsonl", ctx.RunDir+"/"+stage+".stderr.txt",
		time.Duration(timeout)*time.Second,
		util.StreamOpts{Env: fullEnv, StdinData: o.StdinData, ReapAfterExit: stage != "execute"})
	if err != nil {
		if environment && managed {
			mr.Abort(ctx)
		}
		return proc, err
	}
	if environment && managed && util.Truthy(proc["timed_out"]) {
		// Killing a Docker client does not stop the process inside its container.
		proc["environment_abort"] = mr.Abort(ctx)
	}
	var hide []string
	if o.StdinData != nil {
		hide = []string{*o.StdinData}
	}
	proc["argv"] = toAny(b.SafeArgv(ctx, argv, hide))
	return proc, nil
}

func toAny(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// SafeArgv is argv for the record: prompts replaced by a marker, auth values redacted.
func (b *BaseHarness) SafeArgv(ctx *Ctx, argv []string, hide []string) []string {
	secrets := map[string]bool{}
	if ctx != nil && ctx.Auth != nil {
		for _, v := range ctx.Auth.Env() {
			if len([]rune(v)) >= 8 {
				secrets[v] = true
			}
		}
		for _, a := range ctx.Auth.ExtraArgs() {
			if !strings.HasPrefix(a, "-") {
				secrets[a] = true
			}
		}
	}
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		switch {
		case a != "" && util.Contains(hide, a):
			out = append(out, "<prompt>")
		case secrets[a]:
			out = append(out, "<redacted>")
		default:
			out = append(out, a)
		}
	}
	return out
}

// ReportResult is the common response extraction for reporters using
// normalized event streams: returns M{proc, text, structured: nil}.
func (b *BaseHarness) ReportResult(ctx *Ctx, proc M) (M, error) {
	tel, err := b.self().Normalize(ctx, ctx.StageName, nil)
	if err != nil {
		return nil, err
	}
	var tokens any
	if usage := util.Maps(tel["usage"]); len(usage) > 0 {
		sum := 0.0
		for _, u := range usage {
			if f, ok := util.Num(u["output_tokens"]); ok {
				sum += f
			}
		}
		tokens = int64(sum)
	}
	var models any
	if ids := util.Strings(tel["model_ids"]); len(ids) > 0 {
		models = strings.Join(ids, ",")
	}
	stop := util.S(tel["stop_reason"])
	code, hasCode := proc["exit_code"]
	isErr := util.Truthy(proc["timed_out"]) || (hasCode && code != nil && util.Int(code, 0) != 0) ||
		stop == "error" || stop == "failed" || stop == "cancelled"
	proc["output_tokens"] = tokens
	proc["model_ids"] = models
	proc["result_subtype"] = tel["stop_reason"]
	proc["is_error"] = isErr
	lim := util.List(tel["limitations"])
	if lim == nil {
		lim = []any{}
	}
	proc["limitations"] = lim
	return M{"proc": proc, "text": util.S(tel["final_text"]), "structured": nil}, nil
}

// ---------------------------------------------------------------- auth

// Auth is a credential/provider profile for a harness (Python base.Auth).
type Auth interface {
	Base() *AuthBase
	Name() string
	// Env is the env to set for the worker (conf env with ${VAR} resolution).
	Env() map[string]string
	ExtraArgs() []string
	Unset() []string
	Prepare(ctx *Ctx) error
	Cleanup(ctx *Ctx) error
	// Problems checks credentials; a non-nil env checks against that explicit
	// worker environment only.
	Problems(env map[string]string) []string
	EnvironmentProblems(env map[string]string, environment M) []string
	Describe() M
	ModelVars(env map[string]string) []string
	// Identity is who the model calls authenticate as; ok=false when unknown.
	Identity(env map[string]string) (string, bool)
}

// AuthBase holds an auth profile's configuration and default behavior.
type AuthBase struct {
	Self             Auth
	PluginName       string
	Conf             M
	Profile          string
	Harness          string // set by spec.AuthFor to the cell's harness
	EnvironmentCheck func() []string
	Harnesses        []string // harness plugin names this applies to ("*" = all)
	Description      string
	RequiredEnv      []string
	IsolatesConfig   bool
	ModelEnv         []string
	identityProbe    *CommandProbe
}

// NewAuthBase builds the defaults for conf (Python Auth.__init__).
func NewAuthBase(conf M) *AuthBase {
	b := &AuthBase{Conf: util.Copy(util.MapOr(conf)), Harnesses: []string{"*"},
		Description: "use whatever the caller's environment already provides", IsolatesConfig: true}
	if v, ok := b.Conf["isolates_config"]; ok {
		b.IsolatesConfig = util.Truthy(v)
	}
	return b
}

func (a *AuthBase) self() Auth {
	if a.Self != nil {
		return a.Self
	}
	return a
}

func (a *AuthBase) Base() *AuthBase { return a }
func (a *AuthBase) Name() string {
	if a.PluginName == "" {
		return "inherit"
	}
	return a.PluginName
}

func (a *AuthBase) Env() map[string]string {
	out := map[string]string{}
	for k, v := range util.Map(a.Conf["env"]) {
		out[k] = Resolve(v)
	}
	return out
}

func (a *AuthBase) ExtraArgs() []string {
	var out []string
	for _, v := range util.List(a.Conf["args"]) {
		out = append(out, Resolve(v))
	}
	return out
}

func (a *AuthBase) Unset() []string {
	return util.Strings(a.Conf["unset"])
}

func (a *AuthBase) Prepare(ctx *Ctx) error { return nil }
func (a *AuthBase) Cleanup(ctx *Ctx) error { return nil }

func (a *AuthBase) Problems(env map[string]string) []string {
	explicit := env != nil
	if env == nil {
		env = util.MergeEnv(util.Environ(), a.self().Env())
	}
	var out []string
	for _, v := range a.RequiredEnv {
		if env[v] == "" {
			out = append(out, "missing env var "+v)
		}
	}
	confEnv := util.Map(a.Conf["env"])
	for _, key := range util.SortedKeys(confEnv) {
		if _, ok := env[key]; explicit && !ok {
			continue
		}
		for _, ref := range EnvRefs(util.Str(confEnv[key])) {
			if os.Getenv(ref) == "" {
				out = append(out, fmt.Sprintf("env %s references ${%s} which is empty in the caller's environment", key, ref))
			}
		}
	}
	return out
}

func (a *AuthBase) EnvironmentProblems(env map[string]string, environment M) []string { return nil }

func (a *AuthBase) Describe() M {
	env := a.self().Env()
	return M{"type": a.self().Name(), "env_names": toAny(util.SortedKeys(env)), "unset": toAny(nonNil(a.self().Unset())),
		"required_env": toAny(nonNil(a.RequiredEnv)), "isolates_config": a.IsolatesConfig}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (a *AuthBase) ModelVars(env map[string]string) []string {
	var out []string
	out = append(out, util.SortedKeys(util.Map(a.Conf["env"]))...)
	out = append(out, a.RequiredEnv...)
	out = append(out, a.self().Unset()...)
	out = append(out, a.ModelEnv...)
	out = append(out, util.Strings(a.Conf["model_env"])...)
	return out
}

func (a *AuthBase) Identity(env map[string]string) (string, bool) {
	return a.IdentityOf(util.Strings(a.Conf["identity_cmd"]), env)
}

// IdentityOf runs an identity command (argv) and returns its excerpted output.
func (a *AuthBase) IdentityOf(argv []string, env map[string]string) (string, bool) {
	if len(argv) == 0 {
		return "", false
	}
	if env == nil {
		env = util.ChildEnv(a.self().Env(), a.self().Unset())
	}
	p := probeCommand(argv, 30*time.Second, env)
	a.identityProbe = &p
	out := Excerpt(p.Output, 400)
	if !p.OK {
		return strings.TrimSpace("identity check failed (" + p.Failure() + "): " + out), true
	}
	return out, true
}

// ---------------------------------------------------------------- runner

// Runner is where a worker process executes (Python base.Runner).
type Runner interface {
	Base() *RunnerBase
	Name() string
	// LocalTranscripts: harness transcript files are readable on this machine.
	LocalTranscripts() bool
	Wrap(argv []string, ctx *Ctx, env map[string]string) ([]string, error)
	Describe() M
}

// ManagedRunner is a runner that owns an execution environment
// (environments.EnvironmentRunner): explicit env construction, environment-
// located commands, cancellation and release.
type ManagedRunner interface {
	Runner
	ChildEnv(overrides map[string]string, unset []string) map[string]string
	EnvironmentEnv(ctx *Ctx) map[string]string
	HostEnv(ctx *Ctx, overrides map[string]string, unset []string) map[string]string
	Shell(command string, ctx *Ctx, timeout int, env map[string]string) M
	Doctor(ctx *Ctx) M
	Plan(ctx *Ctx) (M, error)
	Prepare(ctx *Ctx) (M, error)
	Abort(ctx *Ctx) M
	Release(ctx *Ctx) (M, error)
	RestoreWorkerOwnership(ctx *Ctx) (M, error)
}

// CheckedManagedRunner is optionally implemented by a ManagedRunner whose
// environment construction and environment shell report the errors Python
// raises (environments.EnvironmentRunner does). ContextShellChecked and
// BaseHarness.Run prefer these methods; timeout is in seconds.
type CheckedManagedRunner interface {
	ChildEnvChecked(overrides map[string]string, unset []string) (map[string]string, error)
	HostEnvChecked(ctx *Ctx, overrides map[string]string, unset []string) (map[string]string, error)
	ShellChecked(command string, ctx *Ctx, timeout float64, env map[string]string) (M, error)
}

// RunnerBase holds a runner's configuration and default behavior.
type RunnerBase struct {
	Self       Runner
	PluginName string
	Conf       M
	// NoLocalTranscripts: harness transcripts live elsewhere (inverse of Python local_transcripts).
	NoLocalTranscripts bool
}

func (r *RunnerBase) self() Runner {
	if r.Self != nil {
		return r.Self
	}
	return r
}

func (r *RunnerBase) Base() *RunnerBase { return r }
func (r *RunnerBase) Name() string {
	if r.PluginName == "" {
		return "local"
	}
	return r.PluginName
}
func (r *RunnerBase) LocalTranscripts() bool { return !r.NoLocalTranscripts }
func (r *RunnerBase) Wrap(argv []string, ctx *Ctx, env map[string]string) ([]string, error) {
	return append([]string(nil), argv...), nil
}
func (r *RunnerBase) Describe() M {
	out := M{"type": r.self().Name()}
	for k, v := range r.Conf {
		if k != "type" {
			out[k] = v
		}
	}
	return out
}

// ---------------------------------------------------------------- check

// Check is a deterministic verification step: Run returns M{passed, detail, ...}.
type Check interface {
	Name() string
	Run(check M, ctx *Ctx) (M, error)
}
