// Package envcheck ports lib/ajx/envcheck.py: what a worker really sees, and
// whether cleanup can work.
//
// A cloud task's credentials pass through three environments that can disagree:
//
//	harness process   ajx's worker env (trial [env], cell env, auth env), then the harness's own
//	                  settings env on top (Claude Code applies `env` from settings.json over its
//	                  process env). The model calls use this.
//	agent tool shell  the harness process env, then the startup files of `<shell> -c` (Claude Code
//	                  runs every Bash call that way, so zsh reads ~/.zshenv). The agent's commands
//	                  use this, including the model's credentials it inherits.
//	check shell       verify, teardown and preflight: ajx's env under /bin/sh, with no harness
//	                  settings and no startup files, plus AJX_RUN_TOKEN and AJX_TRIAL_DIR.
//
// doctor and validate compare them before a run, prepare records them, and teardown, preflight
// and verify output is scanned for credential errors so an exit 0 cannot hide a cleanup that
// never authenticated.
//
// The view returned by WorkerView/InspectCell is a util.M holding secrets
// (never record it): intended, launch, process, effective and source are
// map[string]string, settings is []plugin.SettingsLayer, shell is a string
// and errors is []string. Use Effective and ViewShell to read it.
package envcheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

type M = util.M

// Values are printed only for names that are routinely non-secret; anything else shows as hidden.
var (
	ShownSuffixes = []string{"_PROFILE", "_REGION", "_LOCATION", "_PROJECT", "_PROJECT_ID", "_SUBSCRIPTION_ID", "_ACCOUNT_ID"}
	ShownNames    = []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "AWS_PAGER",
		"AWS_DEFAULT_OUTPUT", "LANG", "LC_ALL", "LC_CTYPE", "TZ"}
	SensitiveHints = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASS", "PWD", "CREDENTIAL", "SESSION", "COOKIE", "AUTH",
		"HEADER", "PRIVATE", "CERT"}
)

// CredentialPatterns are (heuristic) phrases that cloud/SaaS CLIs and SDKs
// print when they cannot authenticate...
var CredentialPatterns = []string{
	`\b(NoCredentials|NoCredentialsError|NoCredentialProviders|CredentialsError|CredentialsProviderError|` +
		`ExpiredToken|ExpiredTokenException|InvalidClientTokenId|UnrecognizedClientException|InvalidAccessKeyId|` +
		`SignatureDoesNotMatch|AuthFailure|InvalidIdentityToken|ExpiredAuthenticationToken|` +
		`InvalidAuthenticationToken|UNAUTHENTICATED|ENEEDAUTH)\b`,
	`Unable to locate credentials`,
	`The config profile \(.+\) could not be found`,
	`(?i)\bcould not load credentials from any providers\b`,
	`(?i)\bno valid credential sources found\b`,
	`(?i)\bunable to resolve AWS account to use\b`,
	`(?i)\bfailed to refresh cached credentials\b`,
	`(?i)\bsecurity token included in the request is (invalid|expired)\b`,
	`(?i)\b(token|credentials?)( has| have| is)? expired\b`,
	`(?i)\bSSO session\b.*\b(expired|invalid)\b`,
	`(?i)\breauthentication (failed|required)\b`,
	`(?i)\bdo not currently have an active account\b`,
	`(?i)\b(run|running|use)\b[:\s` + "`" + `'"$]*(aws (sso )?login|aws configure|gcloud auth|az login|gh auth login|` +
		`(vercel|netlify|firebase|heroku|npm|docker|fly|flyctl|wrangler|doctl|supabase|railway) login)\b`,
	`(?i)\bno (existing |valid )?credentials (found|provided|available)\b`,
	`(?i)could not (automatically determine|find default) credentials`,
	`(?i)\byou must be logged in to the server\b`,
	`\bnpm (ERR!|error) code E401\b`,
	`(?i)\bbad credentials\b`,
	`\bAADSTS\d{5,}\b`,
}

// PermissionPatterns are printed when they are authenticated but not allowed
// (cleanup is just as incomplete).
var PermissionPatterns = []string{
	`\b(AccessDenied|AccessDeniedException|UnauthorizedOperation|AuthorizationError|AuthorizationFailed|` +
		`PERMISSION_DENIED)\b`,
	`(?i)\bis not authorized to perform\b`,
	`(?i)\bnot (logged|signed) in\b`,
	`(?i)\bauthentication (failed|required)\b`,
	`(?i)\b(401 Unauthorized|403 Forbidden|HTTP 40[13])\b`,
}

// AuthErrorPatterns are the compiled credential and permission patterns.
var AuthErrorPatterns = compileAll(append(append([]string{}, CredentialPatterns...), PermissionPatterns...))

func compileAll(patterns []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		out[i] = regexp.MustCompile(p)
	}
	return out
}

const (
	FixCollision = "The worker has one environment for both, so the task's value either breaks model calls or is replaced " +
		"before the agent sees it. Give the model an explicit, isolated auth profile of its own, and give the task " +
		"its account without [env]: name it in the task prompt and pass it explicitly in [task] identity_cmd, " +
		"[[preflight]], [[verify]] and [[teardown]]."
	FixCollisionAWS = " For Bedrock: [auth.<name>] type = \"claude-bedrock\", env = { AWS_PROFILE = \"<model profile>\", " +
		"AWS_REGION = \"<region>\" }; for the task, --profile <task profile> (an explicit --profile beats AWS_PROFILE " +
		"in the environment). AWS_CONFIG_FILE and AWS_SHARED_CREDENTIALS_FILE are read by the model's AWS SDK too; " +
		"put them in [env] only when the model does not authenticate through AWS."
	SkipHint = "set shell_startup = \"skip\" in [trial] so the worker's tool shell does not read it"
	// DefaultAWSIdentity is what an aws call that omits --profile acts as.
	DefaultAWSIdentity = "aws sts get-caller-identity --output text"
)

// ProviderHinter is implemented by auth types that describe their model
// provider for an env (Python Auth.provider_hint).
type ProviderHinter interface {
	ProviderHint(env map[string]string) string
}

// SplitLines is Python's str.splitlines().
func SplitLines(text string) []string {
	var out []string
	start := 0
	rs := []rune(text)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, string(rs[start:i]))
			if rs[i] == '\r' && i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(rs) {
		out = append(out, string(rs[start:]))
	}
	return out
}

// AuthErrorLines are lines of command output that look like a credential or
// permission failure (heuristic), at most 5.
func AuthErrorLines(texts ...string) []string { return AuthErrorLinesN(5, texts...) }

// AuthErrorLinesN is AuthErrorLines with an explicit limit.
func AuthErrorLinesN(limit int, texts ...string) []string {
	hits := []string{}
	for _, text := range texts {
		for _, line := range SplitLines(text) {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			clipped := util.Clip(line, 240)
			if util.Contains(hits, clipped) {
				continue
			}
			for _, p := range AuthErrorPatterns {
				if p.MatchString(line) {
					hits = append(hits, clipped)
					break
				}
			}
		}
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

func hasSuffix(s string, suffixes []string) bool {
	for _, x := range suffixes {
		if strings.HasSuffix(s, x) {
			return true
		}
	}
	return false
}

// Shown is a value for terminal output and logs: only allowlisted,
// non-secret-looking names show a value. value is nil (unset) or a string.
func Shown(name string, value any) string {
	if value == nil {
		return "<unset>"
	}
	v := util.Str(value)
	up := strings.ToUpper(name)
	for _, h := range SensitiveHints {
		if strings.Contains(up, h) {
			return "<value hidden>"
		}
	}
	if !(util.Contains(ShownNames, up) || hasSuffix(up, ShownSuffixes)) {
		return "<value hidden>"
	}
	if util.RuneLen(v) > 80 {
		v = util.Clip(v, 77) + "..."
	}
	return asciiJSON(v)
}

// asciiJSON is json.dumps(s) with Python's default ensure_ascii=True.
func asciiJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20 || (r > 0x7e && r <= 0xffff):
			fmt.Fprintf(&b, `\u%04x`, r)
		case r > 0xffff:
			r -= 0x10000
			fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Matches reports whether name equals a pattern or starts with a 'PREFIX*' pattern.
func Matches(name string, patterns []string) bool {
	for _, p := range patterns {
		if name == p || (strings.HasSuffix(p, "*") && strings.HasPrefix(name, p[:len(p)-1])) {
			return true
		}
	}
	return false
}

// CheckVars are the variables ajx adds for setup, preflight, verify and
// teardown commands, never for the worker's harness: the run token that tags
// and finds a run's resources, and the directory holding trial.toml.
func CheckVars(sp M, runToken string) map[string]string {
	out := map[string]string{"AJX_RUN_TOKEN": runToken}
	if trialFile := util.Str(util.Get(sp, "path")); trialFile != "" {
		out["AJX_TRIAL_DIR"] = filepath.Dir(trialFile)
	}
	return out
}

// CommandEnv is the env of verify/teardown/preflight commands, which ajx runs under /bin/sh.
func CommandEnv(env map[string]string, unset []string, runToken string, sp M) map[string]string {
	return util.ChildEnv(util.MergeEnv(env, CheckVars(sp, runToken)), unset)
}

// ScratchWorkspace is a stand-in workspace for doctor/validate: the fixture,
// as prepare would copy it, in a temp dir. Returns the workspace path.
func ScratchWorkspace(sp M, tmp string) (string, error) {
	ws := filepath.Join(tmp, "workspace")
	if fixture := util.Str(util.Get(sp, "task", "fixture_dir")); fixture != "" {
		if err := CopyTree(fixture, ws); err != nil {
			return ws, err
		}
	}
	if err := os.Mkdir(ws, 0o755); err != nil && !os.IsExist(err) {
		return ws, err
	}
	return ws, nil
}

// CopyTree is shutil.copytree(src, dst, dirs_exist_ok=True): symlinks are
// followed, file modes and modification times are kept.
func CopyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, info.Mode().Perm()|0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	var errs []string
	for _, e := range entries {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		st, err := os.Stat(s)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if st.IsDir() {
			if err := CopyTree(s, d); err != nil {
				errs = append(errs, err.Error())
			}
			continue
		}
		if err := copyFile(s, d, st); err != nil {
			errs = append(errs, err.Error())
		}
	}
	_ = os.Chmod(dst, info.Mode().Perm())
	_ = os.Chtimes(dst, info.ModTime(), info.ModTime())
	if len(errs) > 0 {
		return fmt.Errorf("copytree %s: %s", src, strings.Join(errs, "; "))
	}
	return nil
}

func copyFile(src, dst string, st os.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm()|0o200)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	_ = os.Chmod(dst, st.Mode().Perm())
	return os.Chtimes(dst, st.ModTime(), st.ModTime())
}

// PlaceholderCtx is a full run ctx for doctor/validate, pointing at scratch
// dirs instead of a run. Set Go on the result for cancellation.
func PlaceholderCtx(sp, cell M, auth plugin.Auth, runner plugin.Runner, tmp string) *plugin.Ctx {
	env := spec.WorkerEnv(sp, cell, auth)
	var unset []string
	if auth != nil {
		unset = append(unset, auth.Unset()...)
	}
	if spec.EnvironmentFor(sp, cell) == nil && cell["shell_startup"] == "skip" {
		quiet := filepath.Join(tmp, "no-shell-startup")
		_ = os.MkdirAll(quiet, 0o755)
		for k, v := range spec.ShellStartupSkipEnv {
			env[k] = v
		}
		env["ZDOTDIR"] = quiet
		unset = append(unset, spec.ShellStartupDrop...)
	}
	if unset == nil {
		unset = []string{}
	}
	return &plugin.Ctx{
		Spec: sp, Cell: cell, RunDir: tmp, Workspace: filepath.Join(tmp, "workspace"),
		ConfigDir: filepath.Join(tmp, "harness-config"), CacheDir: filepath.Join(tmp, "cache"),
		RunToken: "doctor", Timeout: util.Int(util.Get(sp, "trial", "timeout_seconds"), 1800),
		NarrateTimeout: util.Int(util.Get(sp, "trial", "narrate_timeout_seconds"), 900),
		PromptText:     util.Str(util.Get(sp, "task", "prompt_text")),
		State:          M{}, Env: env, UnsetEnv: unset, Runner: runner, Auth: auth,
		CheckEnv: util.MergeEnv(env, CheckVars(sp, "doctor")),
	}
}

func cleanEnv(h plugin.Harness, ctx *plugin.Ctx) (extra map[string]string, errs []string) {
	defer func() {
		if r := recover(); r != nil {
			extra = map[string]string{}
			errs = []string{fmt.Sprintf("%s.clean_env failed: %v", h.Name(), r)}
		}
	}()
	// Adapters whose clean env can fail expose the error Python's clean_env raises.
	if checker, ok := h.(interface {
		CleanEnvErr(*plugin.Ctx) (map[string]string, error)
	}); ok {
		var err error
		if extra, err = checker.CleanEnvErr(ctx); err != nil {
			return map[string]string{}, []string{fmt.Sprintf("%s.clean_env failed: %s: %v", h.Name(), errTypeName(err), err)}
		}
	} else {
		extra = h.CleanEnv(ctx)
	}
	if extra == nil {
		extra = map[string]string{}
	}
	return extra, []string{}
}

// WorkerView is the env a worker's harness process gets (as Harness.Run
// builds it from ctx), the harness's own settings env on top of it, and the
// shell its tool commands run in. Holds secrets: never record it.
func WorkerView(sp M, h plugin.Harness, auth plugin.Auth, ctx *plugin.Ctx) M {
	extra, errs := cleanEnv(h, ctx)
	unset := ctx.UnsetEnv
	if unset == nil && auth != nil {
		unset = auth.Unset()
	}
	drop := append(append([]string{}, h.Base().StripEnv...), unset...)
	merged := util.MergeEnv(ctx.Env, extra)
	var process map[string]string
	if mr, ok := ctx.Runner.(plugin.ManagedRunner); ok && ctx.EnvironmentProfile != nil {
		process = mr.ChildEnv(merged, drop)
	} else {
		process = util.ChildEnv(merged, drop)
	}
	layers := h.SettingsEnv(process, ctx.Workspace)
	effective, source := util.MergeEnv(process), map[string]string{}
	for _, layer := range layers {
		for k, v := range layer.Env {
			effective[k] = v
			source[k] = "harness settings " + layer.Source
		}
	}
	if layers == nil {
		layers = []plugin.SettingsLayer{}
	}
	return M{"intended": util.MergeEnv(ctx.Env), "launch": util.MergeEnv(extra), "process": process,
		"effective": effective, "settings": layers, "source": source, "shell": h.ToolShell(effective), "errors": errs}
}

func viewMap(view M, key string) map[string]string {
	if m, ok := view[key].(map[string]string); ok {
		return m
	}
	return util.StrMap(view[key])
}

// Effective is the env the worker's harness applies (view["effective"]).
func Effective(view M) map[string]string { return viewMap(view, "effective") }

// ViewShell is the shell the agent's tool commands run in (view["shell"]).
func ViewShell(view M) string { return util.Str(view["shell"]) }

func viewLayers(view M) []plugin.SettingsLayer {
	if l, ok := view["settings"].([]plugin.SettingsLayer); ok {
		return l
	}
	return nil
}

func viewErrors(view M) []string {
	if l, ok := view["errors"].([]string); ok {
		return l
	}
	return util.Strings(view["errors"])
}

const envMark = "__AJX_ENV__"

var safeShellWord = regexp.MustCompile(`^[\w@%+=:,./-]+$`)

// ShQuote is shlex.quote.
func ShQuote(s string) string {
	if s == "" {
		return "''"
	}
	if safeShellWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// ShellEnv is the exported variables a command sees inside `<shell> -c`,
// after the shell's startup files; nil when the shell could not be probed.
// `env -0` first; a Python dump where env has no -0.
func ShellEnv(ctx context.Context, shell string, env map[string]string, cwd string) map[string]string {
	if ctx == nil {
		ctx = context.Background()
	}
	envBin := util.Which("env")
	if envBin == "" {
		envBin = "/usr/bin/env"
	}
	dumpers := []string{ShQuote(envBin) + " -0"}
	if py := util.Which("python3"); py != "" {
		dumpers = append(dumpers, ShQuote(py)+` -I -c 'import os, sys; sys.stdout.write(chr(0).join(k + "=" + v for k, v in os.environ.items()))'`)
	}
	path := shell
	if !strings.Contains(shell, "/") {
		if path = util.WhichIn(shell, env); path == "" {
			return nil
		}
	}
	for _, dump := range dumpers {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		cmd := exec.CommandContext(cctx, path, "-c", "printf %s "+envMark+"; exec "+dump)
		cmd.Args[0] = shell
		cmd.Dir = cwd
		cmd.Env = util.EnvList(env)
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = io.Discard
		cmd.WaitDelay = time.Second
		err := cmd.Run()
		timedOut := cctx.Err() != nil
		cancel()
		if timedOut {
			return nil
		}
		if err != nil {
			if _, exited := err.(*exec.ExitError); !exited {
				return nil
			}
			continue
		}
		out := string(bytes.ToValidUTF8(stdout.Bytes(), []byte("�")))
		i := strings.LastIndex(out, envMark)
		if i < 0 {
			continue
		}
		vars := map[string]string{}
		for _, item := range strings.Split(out[i+len(envMark):], "\x00") {
			if item == "" {
				continue
			}
			k, v, _ := strings.Cut(item, "=")
			vars[k] = v
		}
		return vars
	}
	return nil
}

func optGet(m map[string]string, k string) any {
	if m == nil {
		return nil
	}
	if v, ok := m[k]; ok {
		return v
	}
	return nil
}

// EnvFindings are variables ajx sets that the harness or the agent's tool
// shell will not see as set. shellVars nil means the tool shell was not probed.
func EnvFindings(sp, cell M, auth plugin.Auth, view M, shellVars map[string]string) []any {
	var names []string
	origin := map[string]string{}
	add := func(keys []string, label string) {
		for _, k := range keys {
			if _, seen := origin[k]; !seen {
				names = append(names, k)
			}
			origin[k] = label
		}
	}
	add(util.SortedKeys(util.MapOr(sp["env"])), "[env]")
	add(util.SortedKeys(util.MapOr(cell["env"])), fmt.Sprintf("cell %s env", util.Str(cell["id"])))
	add(util.SortedKeys(auth.Env()), "auth "+auth.Base().Profile)
	intended, effective, source, launch := viewMap(view, "intended"), viewMap(view, "effective"),
		viewMap(view, "source"), viewMap(view, "launch")
	out := []any{}
	for _, name := range names {
		want, harnessVal := optGet(intended, name), optGet(effective, name)
		toolVal := harnessVal
		if shellVars != nil {
			toolVal = optGet(shellVars, name)
		}
		if harnessVal == want && toolVal == harnessVal {
			continue
		}
		var src any
		if s := source[name]; s != "" {
			src = s
		} else if _, ok := launch[name]; ok {
			src = "the harness's launch env"
		}
		var settingsSource, startup any
		if harnessVal != want {
			settingsSource = src
		}
		if toolVal != harnessVal {
			if s := StartupSource(name, effective); s != "" {
				startup = s
			}
		}
		out = append(out, M{"name": name, "origin": origin[name], "set": want, "harness": harnessVal,
			"tool_shell": toolVal, "settings_source": settingsSource, "shell_startup": toolVal != harnessVal,
			"model_affected": harnessVal != want, "startup_source": startup})
	}
	return out
}

type bashOnly struct {
	re    *regexp.Regexp
	label string
}

var bashOnlySyntax = []bashOnly{
	{regexp.MustCompile(`[<>]\(`), "process substitution <(...)"},
	{regexp.MustCompile(`\[\[`), "[[ ... ]] tests"},
	{regexp.MustCompile(`\{[^{}\s]+\.\.[^{}\s]+\}`), "brace ranges {a..b}"},
	{regexp.MustCompile(`(^|[;&|]\s*|\s)source\s`), "source (use . instead)"},
}

// BashOnlyPatterns are the bash-only syntax checks as [pattern, label] pairs
// (Python [[p.pattern, label] for p, label in envcheck._BASH_ONLY]).
func BashOnlyPatterns() [][2]string {
	out := make([][2]string, len(bashOnlySyntax))
	for i, b := range bashOnlySyntax {
		out[i] = [2]string{b.re.String(), b.label}
	}
	return out
}

// ShellSyntaxWarnings flags bash-only syntax in lifecycle commands, which
// run as `/bin/sh -c`, before a run (a teardown that cannot parse would leave
// the task's resources behind).
func ShellSyntaxWarnings(sp M) []string {
	out := []string{}
	for _, key := range []string{"setup", "preflight", "verify", "teardown"} {
		for i, cmd := range util.Maps(sp[key]) {
			typ := "shell"
			if t, ok := cmd["type"]; ok {
				typ = util.Str(t)
			}
			run := util.Str(cmd["run"])
			if typ != "shell" || !util.Truthy(cmd["run"]) {
				continue
			}
			var found []string
			for _, b := range bashOnlySyntax {
				if b.re.MatchString(run) {
					found = append(found, b.label)
				}
			}
			if len(found) > 0 {
				out = append(out, fmt.Sprintf("[[%s]] %d (%s) uses %s, which /bin/sh may not "+
					"support; AJX runs it with /bin/sh -c. Use POSIX syntax or wrap it in bash -c '...'",
					key, i+1, spec.PyStr(cmd["name"]), strings.Join(found, ", ")))
			}
		}
	}
	return out
}

func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home := os.Getenv("HOME")
		if home == "" {
			home = util.Home()
		}
		return home + p[1:]
	}
	return p
}

// StartupSource is where a non-login `zsh -c` or `bash -c` would set name:
// "~/.zshenv line 3", or "" when not found.
func StartupSource(name string, env map[string]string) string {
	home := env["HOME"]
	if home == "" {
		home = util.Home()
	}
	base := env["ZDOTDIR"]
	if base == "" {
		base = home
	}
	files := []string{filepath.Join(base, ".zshenv")}
	if b := env["BASH_ENV"]; b != "" {
		files = append(files, filepath.Clean(expandUser(b)))
	}
	pattern := regexp.MustCompile(`^\s*(?:export\s+|typeset\s+-x\s+)?` + regexp.QuoteMeta(name) + `=`)
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := string(bytes.ToValidUTF8(data, []byte("�")))
		for number, line := range SplitLines(text) {
			if pattern.MatchString(line) {
				shown := path
				if strings.HasPrefix(path, home) {
					shown = strings.Replace(path, home, "~", 1)
				}
				return fmt.Sprintf("%s line %d", shown, number+1)
			}
		}
	}
	return ""
}

// OverrideText is the warning sentence for one EnvFindings entry.
func OverrideText(finding M, shell any) string {
	n := util.Str(finding["name"])
	text := fmt.Sprintf("%s sets %s=%s, but ", util.Str(finding["origin"]), n, Shown(n, finding["set"]))
	if util.Truthy(finding["model_affected"]) {
		text += fmt.Sprintf("the harness applies %s from %s over it "+
			"(model calls use that); ", Shown(n, finding["harness"]), spec.PyStr(finding["settings_source"]))
	}
	text += fmt.Sprintf("the agent's tool shell (%s) sees %s", spec.PyStr(shell), Shown(n, finding["tool_shell"]))
	if util.Truthy(finding["shell_startup"]) {
		if source := finding["startup_source"]; util.Truthy(source) {
			text += fmt.Sprintf(" (set by %s; %s)", util.Str(source), SkipHint)
		} else {
			text += " (set by that shell's startup files)"
		}
	}
	if !util.Truthy(finding["model_affected"]) && strings.HasPrefix(util.Str(finding["origin"]), "auth ") {
		text += "; model calls keep the profile's value"
	}
	return text
}

// ProviderLabel names the auth profile and its model provider.
func ProviderLabel(auth plugin.Auth, env map[string]string) string {
	if h, ok := auth.(ProviderHinter); ok {
		return fmt.Sprintf("auth %s: %s", auth.Base().Profile, h.ProviderHint(env))
	}
	return fmt.Sprintf("auth %s: %s", auth.Base().Profile, auth.Base().Description)
}

// CollisionFindings are trial [env] / cell env keys that are also the
// harness's model credentials under this auth. A key the auth profile itself
// supplies with the identical value (after ${VAR} resolution) can break
// neither side and is not reported; values are only compared, never printed.
func CollisionFindings(sp, cell M, auth plugin.Auth, view M) []string {
	patterns := auth.ModelVars(Effective(view))
	supplied := auth.Env()
	trialEnv, cellEnv := util.StrMap(sp["env"]), util.StrMap(cell["env"])
	layered := util.MergeEnv(trialEnv, cellEnv)
	keys := util.SortedKeys(trialEnv)
	for _, k := range util.SortedKeys(cellEnv) {
		if _, ok := trialEnv[k]; !ok {
			keys = append(keys, k)
		}
	}
	out := []string{}
	for _, k := range keys {
		if !Matches(k, patterns) {
			continue
		}
		if v, ok := supplied[k]; ok && v == layered[k] {
			continue
		}
		out = append(out, k)
	}
	return out
}

// CollisionText is the warning for CollisionFindings names.
func CollisionText(names []string, auth plugin.Auth, view M) string {
	text := fmt.Sprintf("%s from [env]/cell env also drive the harness's model access (%s). ",
		strings.Join(names, ", "), ProviderLabel(auth, Effective(view))) + FixCollision
	for _, n := range names {
		if strings.HasPrefix(n, "AWS_") {
			return text + FixCollisionAWS
		}
	}
	return text
}

// AuthIdentity is auth.Identity(env): who the model calls authenticate as,
// as nil (unknown) or a string, ready for the report's auth_identity.
func AuthIdentity(auth plugin.Auth, env map[string]string) any {
	if auth == nil {
		return nil
	}
	id, ok := auth.Identity(env)
	if !ok {
		return nil
	}
	return id
}

// IdentityResult runs an identity command as `<shell> -c` (shell "": /bin/sh,
// like verify and teardown) with a 60 s timeout.
func IdentityResult(ctx context.Context, cmd, cwd string, env map[string]string, shell string) M {
	res := util.RunShell(ctx, cmd, cwd, 60*time.Second, util.ShellOpts{Env: env, Shell: shell})
	stdout, stderr := util.Str(res["stdout"]), util.Str(res["stderr"])
	errs := AuthErrorLines(stdout, stderr)
	ok := isZero(res["exit_code"]) && !util.Truthy(res["timed_out"]) && len(errs) == 0
	var text string
	if ok {
		text = strings.TrimSpace(stdout)
	} else if text = strings.Join(errs, "\n"); text == "" {
		text = strings.TrimSpace(util.OrStr(stderr, stdout))
	}
	sh := shell
	if sh == "" {
		sh = "/bin/sh"
	}
	return M{"cmd": cmd, "shell": sh + " -c", "exit_code": res["exit_code"], "timed_out": res["timed_out"],
		"ok": ok, "output": plugin.Excerpt(text, 400), "auth_errors": util.List(errs)}
}

func isZero(code any) bool {
	f, ok := util.Num(code)
	return ok && f == 0
}

// UsesAWS: the model reads AWS credentials (Bedrock) or the task identity is an aws command.
func UsesAWS(sp M, auth plugin.Auth, view M) bool {
	eff := Effective(view)
	var found string
	if p, ok := eff["PATH"]; ok {
		found = util.WhichIn("aws", map[string]string{"PATH": p})
	} else {
		found = util.Which("aws")
	}
	if found == "" {
		return false
	}
	ident := strings.TrimLeft(util.Str(util.Get(sp, "task", "identity_cmd")), " \t\n\r\v\f")
	return Matches("AWS_PROFILE", auth.ModelVars(eff)) || strings.HasPrefix(ident, "aws ")
}

// InspectOpts are InspectCell's optional arguments.
type InspectOpts struct {
	// Identities also runs the auth, task and default AWS identity commands.
	Identities bool
}

// InspectCell is what doctor, validate and prepare report about one cell's
// environment. Returns (report, view): the report holds no env values except
// identity output, the view holds secrets. Commands honor ctx.Go.
func InspectCell(sp, cell M, h plugin.Harness, auth plugin.Auth, runner plugin.Runner, cwd string, ctx *plugin.Ctx, o InspectOpts) (M, M) {
	local := runner.Name() == "local" || util.Str(util.Get(ctx.EnvironmentProfile, "backend")) == "local"
	view := WorkerView(sp, h, auth, ctx)
	if !local { // the worker's env is rebuilt elsewhere; its harness settings and shell are unknown here
		view["effective"] = util.MergeEnv(viewMap(view, "process"))
		view["settings"] = []plugin.SettingsLayer{}
		view["source"] = map[string]string{}
	}
	eff := Effective(view)
	shell := ViewShell(view)
	var shellVars map[string]string
	if local {
		shellVars = ShellEnv(ctx.Context(), shell, eff, cwd)
	}
	observed := h.Base().ToolShellObserved
	settings := []any{}
	for _, l := range viewLayers(view) {
		settings = append(settings, M{"source": l.Source, "names": util.List(util.SortedKeys(l.Env))})
	}
	var toolShell any
	if local {
		ts := shell + " -c"
		if !observed {
			ts += " (assumed)"
		}
		toolShell = ts
	}
	overrides := []any{}
	if shellVars != nil {
		overrides = EnvFindings(sp, cell, auth, view, shellVars)
	}
	restored := []string{}
	restores := h.Base().ToolShellRestores
	for _, n := range append(util.SortedKeys(util.MapOr(sp["env"])), util.SortedKeys(util.MapOr(cell["env"]))...) {
		if util.Contains(restores, n) {
			restored = append(restored, n)
		}
	}
	sort.Strings(restored)
	report := M{
		"local_runner":      local,
		"harness_settings":  settings,
		"tool_shell":        toolShell,
		"tool_shell_probed": shellVars != nil,
		"overrides":         overrides,
		"collisions":        util.List(CollisionFindings(sp, cell, auth, view)),
		"snapshot_restored": util.List(restored),
		"errors":            util.List(viewErrors(view)),
	}
	if o.Identities {
		if local || ctx.EnvironmentProfile == nil {
			report["auth_identity"] = AuthIdentity(auth, eff)
		} else {
			report["auth_identity"] = nil
		}
		cmd := util.Str(util.Get(sp, "task", "identity_cmd"))
		report["task_identity"] = nil
		if cmd != "" && local {
			report["task_identity"] = IdentityResult(ctx.Context(), cmd, cwd, eff, shell)
		}
		report["default_aws_identity"] = nil
		if local && UsesAWS(sp, auth, view) {
			report["default_aws_identity"] = IdentityResult(ctx.Context(), DefaultAWSIdentity, cwd, eff, shell)
		}
		report["aws_profile_source"] = nil
		if local {
			for _, n := range []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE"} {
				if s := StartupSource(n, eff); s != "" {
					report["aws_profile_source"] = s
					break
				}
			}
		}
	}
	return report, view
}

// Warnings are the doctor/validate/prepare warnings for an InspectCell report.
func Warnings(report M, auth plugin.Auth, view M) []string {
	out := []string{}
	for _, e := range util.Strings(report["errors"]) {
		out = append(out, "could not model the worker's env: "+e)
	}
	for _, f := range util.Maps(report["overrides"]) {
		out = append(out, OverrideText(f, report["tool_shell"]))
	}
	if c := util.Strings(report["collisions"]); len(c) > 0 {
		out = append(out, CollisionText(c, auth, view))
	}
	for _, name := range util.Strings(report["snapshot_restored"]) {
		out = append(out, name+" is reset before every tool command from the harness's shell snapshot (taken from a login "+
			"shell with your rc files), so the agent may not see the value ajx sets; not probed")
	}
	if util.Truthy(report["local_runner"]) && !util.Truthy(report["tool_shell_probed"]) {
		out = append(out, fmt.Sprintf("could not probe the agent's tool shell (%s -c); [env] overrides are unchecked", ViewShell(view)))
	}
	return out
}

// DefaultIdentityWarning warns when an aws command without the task's
// --profile would act as someone else; "" when there is nothing to say.
func DefaultIdentityWarning(report M) string {
	def, task := util.Map(report["default_aws_identity"]), util.Map(report["task_identity"])
	if len(def) == 0 || !util.Truthy(def["ok"]) {
		return ""
	}
	if len(task) > 0 && util.Truthy(task["ok"]) && util.Str(task["output"]) == util.Str(def["output"]) {
		return ""
	}
	whose := ""
	if util.Str(def["output"]) == strings.TrimSpace(util.Str(report["auth_identity"])) {
		whose = " (the model's account)"
	}
	text := fmt.Sprintf("an aws command or SDK call in the agent's tool shell that omits the task's --profile acts as "+
		"%s%s, not as the task identity; teardown does not look there", util.Str(def["output"]), whose)
	if src := report["aws_profile_source"]; util.Truthy(src) {
		text += fmt.Sprintf(". %s sets the AWS profile for that shell; %s", util.Str(src), SkipHint)
	}
	return text
}

// Record is the environment.json form of a report: names and sources only.
func Record(report M) M {
	overrides := []any{}
	for _, f := range util.Maps(report["overrides"]) {
		overrides = append(overrides, M{"name": f["name"], "origin": f["origin"], "settings_source": f["settings_source"],
			"shell_startup": f["shell_startup"], "model_affected": f["model_affected"]})
	}
	return M{"harness_settings": report["harness_settings"], "tool_shell": report["tool_shell"],
		"tool_shell_probed": report["tool_shell_probed"], "collisions": report["collisions"],
		"snapshot_restored": report["snapshot_restored"], "errors": report["errors"], "overrides": overrides}
}

// RecordedNotes is one sentence per recorded override or collision
// (environment.json), for reports and the digest.
func RecordedNotes(environment M) []string {
	checks := util.MapOr(util.Get(environment, "env_checks"))
	out := []string{}
	for _, o := range util.Maps(checks["overrides"]) {
		var where []string
		if util.Truthy(o["settings_source"]) {
			where = append(where, util.Str(o["settings_source"]))
		}
		if util.Truthy(o["shell_startup"]) {
			where = append(where, "the tool shell's startup files")
		}
		who := "the agent's commands"
		if util.Truthy(o["model_affected"]) {
			who = "the harness (model calls) and the agent's commands"
		}
		out = append(out, fmt.Sprintf("%s %s was replaced by %s; %s did not see the value ajx set",
			util.Str(o["origin"]), util.Str(o["name"]), strings.Join(where, " and "), who))
	}
	if c := util.Strings(checks["collisions"]); len(c) > 0 {
		out = append(out, strings.Join(c, ", ")+" set for the task also drive the harness's model access; "+
			"one of the two did not get its intended value")
	}
	return out
}

// PreflightOpts are RunPreflight's optional arguments.
type PreflightOpts struct {
	StopOnFailure bool
	// Executor runs one command entry (the runner's location-aware shell);
	// nil runs it with util.RunShell in cwd with env.
	Executor func(cmd M) M
}

// RunPreflight runs preflight commands. One passes when it exits 0 within its
// timeout and prints no credential errors (unless the entry sets
// allow_auth_errors). Interruption returns the results so far and
// util.ErrInterrupted.
func RunPreflight(ctx context.Context, commands []M, cwd string, env map[string]string, o PreflightOpts) ([]M, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	results := []M{}
	for _, cmd := range commands {
		var res M
		if o.Executor != nil {
			res = o.Executor(cmd)
		} else {
			res = util.RunShell(ctx, util.Str(cmd["run"]), cwd, time.Duration(util.Int(cmd["timeout"], 600))*time.Second,
				util.ShellOpts{Env: env})
		}
		if util.Truthy(res["interrupted"]) || ctx.Err() != nil {
			return results, util.ErrInterrupted
		}
		rec := M{"name": cmd["name"]}
		for _, k := range []string{"cmd", "exit_code", "timed_out", "started_at", "stopped_at", "stdout", "stderr"} {
			rec[k] = res[k]
		}
		authErrors := []string{}
		if !util.Truthy(cmd["allow_auth_errors"]) {
			authErrors = AuthErrorLines(util.Str(res["stdout"]), util.Str(res["stderr"]))
		}
		rec["auth_errors"] = util.List(authErrors)
		if loc, ok := res["location"]; ok {
			rec["location"] = loc
		}
		passed := isZero(res["exit_code"]) && !util.Truthy(res["timed_out"]) && len(authErrors) == 0
		rec["passed"] = passed
		results = append(results, rec)
		if o.StopOnFailure && !passed {
			break
		}
	}
	return results, nil
}

// PreflightFailure is a one-line reason for a failed preflight record.
func PreflightFailure(rec M) string {
	why := "exit " + spec.PyStr(intish(rec["exit_code"]))
	if util.Truthy(rec["timed_out"]) {
		why = "timed out"
	}
	detail := strings.Join(util.Strings(rec["auth_errors"]), "; ")
	if detail == "" {
		detail = plugin.Excerpt(strings.TrimSpace(util.OrStr(util.Str(rec["stderr"]), util.Str(rec["stdout"]))), 300)
	}
	if detail != "" {
		return why + ": " + detail
	}
	return why
}

// intish turns an integral float64 (JSON) into int64 so it prints as Python's int.
func intish(v any) any {
	if f, ok := v.(float64); ok && f == float64(int64(f)) {
		return int64(f)
	}
	return v
}

// TeardownSummary: status ok | failed (a command exited non-zero or timed
// out) | auth_errors (credential or permission errors printed, whatever the
// exit code) | skipped (no workspace, so nothing ran). defined is the number
// of [[teardown]] entries.
func TeardownSummary(results []M, defined int, ran bool) M {
	failed := []any{}
	errs := []string{}
	for i, r := range results {
		if !isZero(r["exit_code"]) || util.Truthy(r["timed_out"]) {
			failed = append(failed, int64(i))
		}
		errs = append(errs, util.Strings(r["auth_errors"])...)
	}
	status := "ok"
	switch {
	case defined == 0:
		status = "ok"
	case !ran:
		status = "skipped"
	case len(errs) > 0:
		status = "auth_errors"
	case len(failed) > 0:
		status = "failed"
	}
	if len(errs) > 10 {
		errs = errs[:10]
	}
	return M{"status": status, "commands": int64(len(results)), "failed": failed, "auth_errors": util.List(errs)}
}

// errTypeName is the Python exception class name a typed error reports
// (TypeName), else "ValueError" for validation errors and "Exception" otherwise.
func errTypeName(err error) string {
	var named interface{ TypeName() string }
	if errors.As(err, &named) {
		return named.TypeName()
	}
	return "Exception"
}
