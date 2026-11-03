// Package plugin ports lib/ajx/base.py and lib/ajx/plugins.py: the four
// extension contracts (harness, auth, runner, check), their default
// behavior, the run context, and the registry.
//
// Go has no inheritance, so each contract has a Base struct holding the
// Python class attributes and default methods. A Base keeps a Self pointer to
// the outer value so default methods dispatch to overrides (as Python's self
// does). Registry constructors bind Self automatically; when constructing an
// adapter by hand, call Bind.
package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/royosherove/ajx/internal/util"
)

type M = util.M

// Kinds of plugins, in registry order.
var Kinds = []string{"harness", "auth", "runner", "check"}

// Ctx is the run context handed to plugins (Python's ctx dict).
type Ctx struct {
	Go                        context.Context // cancellation (SIGINT/SIGTERM/SIGHUP)
	Spec                      M
	Cell                      M
	RunDir                    string
	Workspace                 string
	ConfigDir                 string
	CacheDir                  string
	HomeDir                   string
	RunToken                  string
	Timeout                   int // seconds
	NarrateTimeout            int // seconds
	PromptText                string
	State                     M // per-stage results so far (state.json "stages" parent record)
	Env                       map[string]string
	UnsetEnv                  []string
	CheckEnv                  map[string]string
	Runner                    Runner
	Auth                      Auth // may be nil
	EnvironmentProfile        M    // nil for legacy runners
	AgentConfigurationProfile M    // nil when no profile is selected
	AgentConfiguration        M    // prepared manifest (ctx["agent_configuration"]); nil until prepared
	StageName                 string
	Extra                     M // anything else a plugin needs to stash
}

// Context returns the cancellation context (never nil).
func (c *Ctx) Context() context.Context {
	if c == nil || c.Go == nil {
		return context.Background()
	}
	return c.Go
}

// Clone returns a shallow copy (Python {**ctx, ...}).
func (c *Ctx) Clone() *Ctx {
	cp := *c
	return &cp
}

// StageState is ctx["state"][stage] (Python Harness.stage_state).
func (c *Ctx) StageState(stage string) M {
	return util.MapOr(util.Get(c.State, stage))
}

// AWSCredentialEnv is what the AWS SDKs and CLI read to pick credentials,
// account and region (not output settings such as AWS_PAGER).
var AWSCredentialEnv = []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION",
	"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_SECURITY_TOKEN", "AWS_CONFIG_FILE",
	"AWS_SHARED_CREDENTIALS_FILE", "AWS_BEARER_TOKEN_*", "AWS_ROLE_ARN", "AWS_ROLE_SESSION_NAME",
	"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_*", "AWS_EC2_METADATA_*", "AWS_ENDPOINT_URL*",
	"AWS_SDK_LOAD_CONFIG", "AWS_STS_REGIONAL_ENDPOINTS", "AWS_CA_BUNDLE", "AWS_USE_FIPS_ENDPOINT"}

// ---------------------------------------------------------------- helpers

// VersionOf runs argv and returns the first output line (<=120 chars); "" when
// the binary printed nothing; "unavailable (<ErrType>)" when it failed to run.
func VersionOf(argv []string) string {
	out, err := util.Output(argv, 20*time.Second, nil)
	if err != nil && out == "" {
		return fmt.Sprintf("unavailable (%T)", err)
	}
	if out == "" {
		return ""
	}
	return util.Clip(strings.SplitN(out, "\n", 2)[0], 120)
}

// Excerpt trims a value (JSON-encoded when not a string) to limit characters.
func Excerpt(value any, limit int) string {
	if value == nil {
		return ""
	}
	s, ok := value.(string)
	if !ok {
		s = util.PyJSON(value)
	}
	s = strings.TrimSpace(s)
	n := util.RuneLen(s)
	if n <= limit {
		return s
	}
	return util.Clip(s, limit) + fmt.Sprintf("... [%d more chars]", n-limit)
}

// SummarizeInput is a one-line summary of a tool input.
func SummarizeInput(data any) string {
	if m, ok := data.(M); ok {
		for _, key := range []string{"command", "cmd", "file_path", "path", "absolute_path", "pattern", "url", "query", "description"} {
			if val, present := m[key]; present {
				var s string
				if l, isList := val.([]any); isList {
					parts := make([]string, len(l))
					for i, x := range l {
						parts[i] = util.Str(x)
					}
					s = strings.Join(parts, " ")
				} else if str, isStr := val.(string); isStr {
					s = str
				} else {
					s = pyStr(val)
				}
				return Excerpt(key+": "+s, 300)
			}
		}
	}
	return Excerpt(data, 300)
}

// pyStr approximates Python str() for composite values in f-strings.
func pyStr(v any) string {
	switch v.(type) {
	case M, []any:
		return util.PyJSON(v)
	}
	return util.Str(v)
}

// StreamRecord is one line of a RunStreaming capture.
type StreamRecord struct {
	At     string
	Parsed any // decoded JSON, or nil when the line was not JSON
	Line   string
}

// StreamRecords reads a *.raw.jsonl capture.
func StreamRecords(rawPath string) []StreamRecord {
	var out []StreamRecord
	for _, rec := range util.ReadJSONL(rawPath) {
		m := util.MapOr(rec)
		line := util.S(m["line"])
		parsed, err := util.DecodeJSON([]byte(line))
		if err != nil {
			parsed = nil
		}
		out = append(out, StreamRecord{At: util.S(m["at"]), Parsed: parsed, Line: line})
	}
	return out
}

// EmptyTelemetry is the normalized-telemetry skeleton with overrides applied.
func EmptyTelemetry(overrides M) M {
	out := M{"session_id": nil, "model_ids": []any{}, "final_text": "", "stop_reason": nil,
		"events": []any{}, "usage": []any{}, "usage_status": "unavailable", "expected_output_tokens": nil,
		"tool_status": "unavailable", "expected_tool_calls": nil, "harness_reported": M{},
		"timestamp_source": "unknown", "limitations": []any{}, "message_ids": []any{}}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// ToolEvent is a normalized tool call.
func ToolEvent(tid any, name string, rawInput any, at any, messageID any) M {
	return M{"kind": "tool", "tool_use_id": tid, "name": name, "input": SummarizeInput(rawInput),
		"input_raw": rawInput, "at": at, "end": nil, "is_error": nil, "output": "", "message_id": messageID}
}

// TextEvent is a normalized assistant message.
func TextEvent(text string, at any, messageID any) M {
	return M{"kind": "text", "text": Excerpt(text, 1200), "at": at, "message_id": messageID}
}

// FinishTool records a tool call's end.
func FinishTool(call M, at any, isError bool, output any) {
	if call == nil {
		return
	}
	call["end"] = at
	call["is_error"] = isError
	if util.Truthy(output) {
		s, ok := output.(string)
		if !ok {
			s = util.Compact(output)
		}
		call["output"] = Excerpt(s, 1500)
	}
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Resolve expands ${VAR} references from the caller's environment.
func Resolve(value any) string {
	s, ok := value.(string)
	if !ok {
		return util.Str(value)
	}
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		return os.Getenv(m[2 : len(m)-1])
	})
}

// EnvRefs lists the ${VAR} names referenced by value.
func EnvRefs(value string) []string {
	var out []string
	for _, m := range envRef.FindAllStringSubmatch(value, -1) {
		out = append(out, m[1])
	}
	return out
}

// AuthArgs is the auth profile's extra CLI args for ctx (none without auth).
func AuthArgs(ctx *Ctx) []string {
	if ctx == nil || ctx.Auth == nil {
		return nil
	}
	return ctx.Auth.ExtraArgs()
}

// ContextShell runs a trusted lifecycle/check command at its declared
// location ("host" or "environment"). Host verification is independent of the
// worker's process and container; legacy runners keep host-side behavior.
// An environment error (Python raises it) is returned as a failed record
// with "error" set; use ContextShellChecked to receive it as an error.
func ContextShell(cmd string, ctx *Ctx, timeout int, location string) M {
	res, err := ContextShellChecked(cmd, ctx, timeout, location)
	if err != nil && res != nil {
		if util.Interrupted(err) {
			res["interrupted"] = true
		}
		return res
	}
	if err != nil {
		now := util.Now()
		res = M{"cmd": cmd, "exit_code": nil, "timed_out": false, "stdout": "", "started_at": now, "stopped_at": now,
			"stderr": err.Error(), "error": err.Error(), "location": location}
		if location == "" {
			res["location"] = "host"
		}
		if util.Interrupted(err) {
			res["interrupted"] = true
		}
	}
	return res
}

// ContextShellChecked is ContextShell returning the error a managed
// environment raises (an invalid or released environment, unsafe host-check
// variables) instead of running the command, as Python's context_shell does.
func ContextShellChecked(cmd string, ctx *Ctx, timeout int, location string) (M, error) {
	if location == "" {
		location = "host"
	}
	extra := ctx.CheckEnv
	if len(extra) == 0 {
		extra = ctx.Env
	}
	extra = util.MergeEnv(extra)
	managed := ctx.EnvironmentProfile != nil
	mr, isManaged := ctx.Runner.(ManagedRunner)
	checked, isChecked := ctx.Runner.(CheckedManagedRunner)
	if managed && isManaged && location == "environment" {
		var res M
		if isChecked {
			var err error
			if res, err = checked.ShellChecked(cmd, ctx, float64(timeout), extra); err != nil {
				if res != nil {
					res["location"] = "environment"
				}
				return res, err
			}
		} else {
			res = mr.Shell(cmd, ctx, timeout, extra)
		}
		res["location"] = "environment"
		return res, nil
	}
	var env map[string]string
	switch {
	case managed && isChecked:
		var err error
		if env, err = checked.HostEnvChecked(ctx, extra, ctx.UnsetEnv); err != nil {
			return nil, err
		}
	case managed && isManaged:
		env = mr.HostEnv(ctx, extra, ctx.UnsetEnv)
	default:
		env = util.ChildEnv(extra, ctx.UnsetEnv)
	}
	res := util.RunShell(ctx.Context(), cmd, ctx.Workspace, time.Duration(timeout)*time.Second, util.ShellOpts{Env: env})
	res["location"] = "host"
	return res, nil
}

// ---------------------------------------------------------------- registry

type harnessFactory func() Harness
type authFactory func(conf M) Auth
type runnerFactory func(conf M) Runner
type checkFactory func() Check

var (
	mu        sync.RWMutex
	harnesses = map[string]harnessFactory{}
	auths     = map[string]authFactory{}
	runners   = map[string]runnerFactory{}
	checks    = map[string]checkFactory{}
	sources   = map[string]map[string]string{"harness": {}, "auth": {}, "runner": {}, "check": {}}
	loaded    = map[string]bool{}
	loadNotes []string
)

// RegisterHarness registers a harness adapter constructor. Later wins.
func RegisterHarness(name, source string, f func() Harness) {
	mu.Lock()
	defer mu.Unlock()
	harnesses[name] = func() Harness {
		h := f()
		h.Base().PluginName = name
		h.Base().Self = h
		return h
	}
	sources["harness"][name] = source
}

// RegisterAuth registers an auth type.
func RegisterAuth(name, source string, f func(conf M) Auth) {
	mu.Lock()
	defer mu.Unlock()
	auths[name] = func(conf M) Auth {
		a := f(conf)
		b := a.Base()
		b.PluginName = name
		b.Self = a
		if b.Profile == "" {
			b.Profile = name
		}
		return a
	}
	sources["auth"][name] = source
}

// RegisterRunner registers a runner type.
func RegisterRunner(name, source string, f func(conf M) Runner) {
	mu.Lock()
	defer mu.Unlock()
	runners[name] = func(conf M) Runner {
		r := f(conf)
		r.Base().PluginName = name
		r.Base().Self = r
		return r
	}
	sources["runner"][name] = source
}

// RegisterCheck registers a check type.
func RegisterCheck(name, source string, f func() Check) {
	mu.Lock()
	defer mu.Unlock()
	checks[name] = f
	sources["check"][name] = source
}

// NotFound is returned for an unknown plugin name.
type NotFound struct {
	Kind, Name string
	Known      []string
}

func (e *NotFound) Error() string {
	return fmt.Sprintf("no %s plugin named %q; available: %s", e.Kind, e.Name, pyList(e.Known))
}

func pyList(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = "'" + s + "'"
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// NewHarness constructs a registered harness adapter.
func NewHarness(name string) (Harness, error) {
	mu.RLock()
	f, ok := harnesses[name]
	mu.RUnlock()
	if !ok {
		return nil, &NotFound{"harness", name, Names("harness")}
	}
	return f(), nil
}

// NewAuth constructs a registered auth type with its configuration.
func NewAuth(name string, conf M) (Auth, error) {
	mu.RLock()
	f, ok := auths[name]
	mu.RUnlock()
	if !ok {
		return nil, &NotFound{"auth", name, Names("auth")}
	}
	return f(conf), nil
}

// NewRunner constructs a registered runner type.
func NewRunner(name string, conf M) (Runner, error) {
	mu.RLock()
	f, ok := runners[name]
	mu.RUnlock()
	if !ok {
		return nil, &NotFound{"runner", name, Names("runner")}
	}
	return f(conf), nil
}

// NewCheck constructs a registered check type.
func NewCheck(name string) (Check, error) {
	mu.RLock()
	f, ok := checks[name]
	mu.RUnlock()
	if !ok {
		return nil, &NotFound{"check", name, Names("check")}
	}
	return f(), nil
}

// Has reports whether kind/name is registered.
func Has(kind, name string) bool {
	for _, n := range Names(kind) {
		if n == name {
			return true
		}
	}
	return false
}

// Names lists registered names of a kind, sorted.
func Names(kind string) []string {
	mu.RLock()
	defer mu.RUnlock()
	var keys []string
	switch kind {
	case "harness":
		keys = util.SortedKeys(harnesses)
	case "auth":
		keys = util.SortedKeys(auths)
	case "runner":
		keys = util.SortedKeys(runners)
	case "check":
		keys = util.SortedKeys(checks)
	}
	return keys
}

// Source is where kind/name was registered from ("builtin", a file path...).
func Source(kind, name string) string {
	mu.RLock()
	defer mu.RUnlock()
	return sources[kind][name]
}

// PluginDirs are searched for plugin files: ~/.config/ajx/plugins and <trial>/plugins.
func PluginDirs(trialDir string) []string {
	dirs := []string{filepath.Join(util.Home(), ".config", "ajx", "plugins")}
	if trialDir != "" {
		dirs = append(dirs, filepath.Join(trialDir, "plugins"))
	}
	return dirs
}

var pyRegister = regexp.MustCompile(`@register\(\s*["'](harness|auth|runner|check)["']\s*,\s*["']([^"']+)["']\s*\)`)

// NativeEquivalents maps a Python plugin file name (e.g. "console_demo.py") to
// true when a Go builtin already provides the names it registers.
var NativeEquivalents = map[string]bool{}

// NativeRegistrars maps a Python plugin file name to a function that
// registers its native Go equivalent. Load calls it when it finds that file,
// so on-demand builtins (the console demo) are only listed once a trial
// that bundles them is loaded, as in the Python CLI.
var NativeRegistrars = map[string]func(){}

// Load scans plugin directories. The Go CLI cannot execute Python plugins: a
// *.py plugin whose names have no native Go equivalent registers a
// placeholder that fails with a clear message when used, so validate and
// doctor can name the problem. Returns notes describing what was found.
func Load(trialDir string) []string {
	var notes []string
	for _, d := range PluginDirs(trialDir) {
		abs, err := filepath.Abs(d)
		if err != nil {
			continue
		}
		mu.Lock()
		seen := loaded[abs]
		loaded[abs] = true
		mu.Unlock()
		if seen {
			continue
		}
		files, _ := filepath.Glob(filepath.Join(abs, "*.py"))
		sort.Strings(files)
		for _, f := range files {
			if reg := NativeRegistrars[filepath.Base(f)]; reg != nil {
				reg()
			}
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			for _, m := range pyRegister.FindAllStringSubmatch(string(data), -1) {
				kind, name := m[1], m[2]
				if NativeEquivalents[filepath.Base(f)] && Has(kind, name) {
					continue
				}
				if Has(kind, name) && Source(kind, name) != "python:"+f {
					notes = append(notes, fmt.Sprintf("%s plugin %q in %s overrides a built-in in the Python CLI; the Go CLI keeps its built-in", kind, name, f))
					continue
				}
				registerPythonPlaceholder(kind, name, f)
				notes = append(notes, fmt.Sprintf("%s plugin %q is defined in Python (%s); the Go CLI cannot run Python plugins", kind, name, f))
			}
		}
	}
	mu.Lock()
	loadNotes = append(loadNotes, notes...)
	mu.Unlock()
	return notes
}

// LoadNotes returns every note produced by Load so far.
func LoadNotes() []string {
	mu.RLock()
	defer mu.RUnlock()
	return append([]string(nil), loadNotes...)
}

// PythonPluginError is returned by placeholder plugins.
type PythonPluginError struct{ Kind, Name, Path string }

func (e *PythonPluginError) Error() string {
	return fmt.Sprintf("%s plugin %q is a Python plugin (%s); the Go CLI cannot load it. Declare it with "+
		"[adapters.%s] in trial.toml, or run this trial with the Python CLI", e.Kind, e.Name, e.Path, e.Name)
}

func registerPythonPlaceholder(kind, name, path string) {
	perr := &PythonPluginError{kind, name, path}
	src := "python:" + path
	switch kind {
	case "harness":
		RegisterHarness(name, src, func() Harness { return &pythonHarness{err: perr} })
	case "auth":
		RegisterAuth(name, src, func(conf M) Auth { return &pythonAuth{AuthBase: NewAuthBase(conf), err: perr} })
	case "runner":
		RegisterRunner(name, src, func(conf M) Runner { return &pythonRunner{RunnerBase: RunnerBase{Conf: conf}, err: perr} })
	case "check":
		RegisterCheck(name, src, func() Check { return &pythonCheck{name: name, err: perr} })
	}
}

type pythonHarness struct {
	BaseHarness
	err error
}

func (h *pythonHarness) Available() bool                   { return false }
func (h *pythonHarness) Execute(ctx *Ctx) (M, error)       { return nil, h.err }
func (h *pythonHarness) Report(*Ctx, string, M) (M, error) { return nil, h.err }
func (h *pythonHarness) Normalize(*Ctx, string, []string) (M, error) {
	return nil, h.err
}

type pythonAuth struct {
	*AuthBase
	err error
}

func (a *pythonAuth) Problems(env map[string]string) []string { return []string{a.err.Error()} }

type pythonRunner struct {
	RunnerBase
	err error
}

func (r *pythonRunner) Wrap([]string, *Ctx, map[string]string) ([]string, error) { return nil, r.err }

type pythonCheck struct {
	name string
	err  error
}

func (c *pythonCheck) Name() string { return c.name }
func (c *pythonCheck) Run(M, *Ctx) (M, error) {
	return M{"passed": false, "detail": c.err.Error()}, nil
}
