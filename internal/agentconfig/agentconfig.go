// Package agentconfig ports lib/ajx/agent_configuration.py: explicit,
// per-attempt harness extension profiles ([agent_configurations.NAME]).
//
// Public trial schema:
//
//	[agent_configurations.product]
//	skills = { mode = "selected", paths = ["skills/product"], pins = { product = "<sha256>" } }
//	plugins = { mode = "none" }
//	hooks = { mode = "none" }
//	require_absence = false
//
// All three categories default to none. snapshot also takes explicit skill
// directories, never a user's configuration root, a glob, or an implicit
// search. Pins are optional at input: normalization always pins the complete
// content. Plugins/hooks other than none are unsupported and rejected.
// SKILL.md allowed-tools and version are recorded as declared metadata, never
// enforced; empty .gitkeep/.keep placeholders are copied and hashed like any
// other file so the recorded hash matches the shipped artifact.
//
// Integration:
//   - NormalizeProfiles returns JSON-safe normalized tables. "_source" and
//     "_identity" are private resolution data; do not publish them.
//   - ResumeContract retains profile semantics and source paths while
//     excluding source filesystem identities from the saved attempt comparison.
//   - ValidateForCell fails without reading credentials or launching a provider.
//   - Prepare runs only backend help/version capability checks, copies pinned
//     snapshots, sets ctx.AgentConfiguration, and returns a JSON-safe manifest.
//     The caller persists that exact result in state["agent_configuration"]
//     and agent-configuration.json before execution.
//   - LaunchArgs/LaunchEnv/LaunchBinary read that manifest (including from
//     state) and verify installed content and configuration before worker and
//     narrator launches. Paths in the persisted launch description use
//     context-directory placeholders. It contains no auth values, source
//     paths, or caller configuration.
//
// This controls optional activation, not host filesystem access. Managed
// policy and built-in features are explicitly unknown; require_absence is
// rejected. Reporter contexts must be independent and must not carry a worker
// manifest.
package agentconfig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

// Error is a profile validation failure (Python ValueError). spec maps it to
// spec.Error; operating-system failures are returned as plain errors.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// TypeName is the Python exception class name recorded for this failure.
func (e *Error) TypeName() string { return "ValueError" }

// IsValue marks the error as a validation failure (spec maps it to spec.Error).
func (e *Error) IsValue() bool { return true }

// Harnesses are the harness plugins that support explicit profiles.
var Harnesses = []string{"codex", "claude-code", "kiro-cli"}

var modes = []string{"none", "selected", "snapshot"}

// Bounds (variables so tests can lower them, as the Python tests patch them).
var (
	maxSkills       = 32
	maxFiles        = 2048
	maxBytes  int64 = 32 * 1024 * 1024
	maxFile   int64 = 8 * 1024 * 1024
	maxDepth        = 16
	maxGrants       = 64
	maxScalar       = 256
)

var placeholderNames = map[string]bool{".gitkeep": true, ".keep": true}

var deniedNames = setOf("node_modules", "__pycache__", "venv", "env", "cache", "caches",
	"credentials", "credentials.json", "auth.json", "token", "tokens", "id_rsa", "id_ed25519")

var rootKeys = []string{"home_dir", "config_dir", "cache_dir", "workspace"}

var envExact = setOf("HOME", "USERPROFILE", "ZDOTDIR", "BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS",
	"NODE_OPTIONS", "NODE_PATH", "PYTHONPATH", "PYTHONHOME", "PYTHONSTARTUP",
	"CDPATH", "GLOBIGNORE", "PROMPT_COMMAND", "LD_PRELOAD", "LD_LIBRARY_PATH",
	"DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH",
	"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME")

var envPrefixes = []string{"CODEX_", "CLAUDE_", "KIRO_", "Q_", "XDG_", "LD_", "DYLD_", "BASH_FUNC_"}

// providerEnv: auth/provider settings do not choose discovery scopes or install extensions.
var providerEnv = setOf("CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_SKIP_BEDROCK_AUTH", "KIRO_API_KEY")

var commonEnvKeys = []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "ZDOTDIR"}

func commonEnv() util.M {
	return util.M{"HOME": "{home_dir}", "XDG_CONFIG_HOME": "{config_dir}",
		"XDG_DATA_HOME": "{home_dir}/.local/share", "XDG_STATE_HOME": "{home_dir}/.local/state",
		"XDG_CACHE_HOME": "{cache_dir}", "ZDOTDIR": "{home_dir}"}
}

// codexConfigKeys is _CODEX_CONFIG's order; launch args follow it (then skills.config).
var codexConfigKeys = []string{"features.plugins", "features.hooks", "features.apps",
	"features.skill_mcp_dependency_install", "skills.bundled.enabled", "notify", "allow_login_shell",
	"cli_auth_credentials_store"}

var launchConfigOrder = append(append([]string{}, codexConfigKeys...), "skills.config")

func codexConfig() util.M {
	return util.M{"features.plugins": false, "features.hooks": false, "features.apps": false,
		"features.skill_mcp_dependency_install": false, "skills.bundled.enabled": false,
		"notify": []any{}, "allow_login_shell": false, "cli_auth_credentials_store": "file"}
}

func claudeSettings() util.M {
	return util.M{"disableAllHooks": true, "disableBundledSkills": true, "skillOverrides": util.M{"doctor": "off"},
		"enabledPlugins": util.M{}, "extraKnownMarketplaces": util.M{}, "autoMemoryEnabled": false,
		"disableClaudeAiConnectors": true}
}

// systemPaths are admin configuration locations. Their contents are never
// inspected: policy may contain credentials.
var systemPaths = map[string][]string{
	"codex": {"/etc/codex/skills", "/etc/codex/config.toml", "/etc/codex/managed_config.toml",
		"/etc/codex/requirements.toml"},
	"claude-code": {"/Library/Application Support/ClaudeCode/managed-settings.json",
		"/Library/Application Support/ClaudeCode/managed-settings.d",
		"/etc/claude-code/managed-settings.json", "/etc/claude-code/managed-settings.d"},
	"kiro-cli": {},
}

// Seams the tests replace (the Python tests patch the same functions).
var (
	probeFn     = probe
	writeFileFn = writeFile
	whichFn     = func(binary, pathEnv string) string {
		return util.WhichIn(binary, map[string]string{"PATH": pathEnv})
	}
	runLocalFn = runLocal
	geteuidFn  = os.Geteuid
	lchownFn   = os.Lchown
	homeFn     = callerHome
)

func setOf(items ...string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, it := range items {
		out[it] = true
	}
	return out
}

// resolvedHome is Path.home().resolve() ("" when the home is unknown).
func resolvedHome() string {
	if h := homeFn(); h != "" {
		return resolvePath(h)
	}
	return ""
}

func callerHome() string {
	if h := util.Home(); h != "" {
		return h
	}
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return ""
}

// ---------------------------------------------------------------- paths

// purePath normalizes like pathlib (duplicate slashes and "." removed, ".."
// kept), so lexical comparisons match Python's Path objects.
func purePath(p string) string {
	if p == "" {
		return "."
	}
	abs := strings.HasPrefix(p, "/")
	var parts []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	out := strings.Join(parts, "/")
	if abs {
		return "/" + out
	}
	if out == "" {
		return "."
	}
	return out
}

func joinPath(base, rel string) string {
	if strings.HasPrefix(rel, "/") {
		return purePath(rel)
	}
	if base == "" {
		return purePath(rel)
	}
	return purePath(base + "/" + rel)
}

// isRelativeTo is PurePath.is_relative_to (lexical).
func isRelativeTo(p, base string) bool {
	if p == base || base == "/" && strings.HasPrefix(p, "/") {
		return true
	}
	return strings.HasPrefix(p, strings.TrimSuffix(base, "/")+"/")
}

// parents is PurePath.parents of an absolute path.
func parents(p string) []string {
	var out []string
	for cur := p; cur != "/" && cur != "."; {
		cur = filepath.Dir(cur)
		out = append(out, cur)
	}
	return out
}

// resolvePath is Path.resolve(strict=False).
func resolvePath(p string) string {
	if !filepath.IsAbs(p) {
		if wd, err := os.Getwd(); err == nil {
			p = wd + "/" + p
		}
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	clean := filepath.Clean(p)
	rest := ""
	for cur := clean; ; {
		parent := filepath.Dir(cur)
		if rest == "" {
			rest = filepath.Base(cur)
		} else {
			rest = filepath.Base(cur) + "/" + rest
		}
		if parent == cur {
			return clean
		}
		if r, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(r, rest)
		}
		cur = parent
	}
}

// expanduser is Path.expanduser.
func expanduser(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	first, rest, _ := strings.Cut(p, "/")
	var home string
	if first == "~" {
		home = homeFn()
	} else if u, err := user.Lookup(first[1:]); err == nil {
		home = u.HomeDir
	}
	must(home != "", "Could not determine home directory.")
	if rest == "" {
		return home
	}
	return strings.TrimSuffix(home, "/") + "/" + rest
}

func isSymlink(p string) bool {
	fi, err := os.Lstat(purePath(p))
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

func lexists(p string) bool {
	_, err := os.Lstat(purePath(p))
	return err == nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// ---------------------------------------------------------------- normalize

type category struct {
	mode  string
	paths []string
	pins  map[string]string
}

func normalizeCategory(raw any, name, label string) category {
	if s, ok := raw.(string); ok {
		raw = util.M{"mode": s}
	}
	m, ok := raw.(util.M)
	must(ok, fmt.Sprintf("%s.%s must be a table", label, name))
	var unknown []string
	for k := range m {
		if k != "mode" && k != "paths" && k != "pins" {
			unknown = append(unknown, k)
		}
	}
	must(len(unknown) == 0, fmt.Sprintf("%s.%s: unsupported fields %s", label, name, pyListRepr(sortedStrings(unknown))))
	var modeValue any = "none"
	if v, ok := m["mode"]; ok {
		modeValue = v
	}
	mode, isStr := modeValue.(string)
	must(isStr && util.Contains(modes, mode), fmt.Sprintf("%s.%s.mode must be none, selected, or snapshot", label, name))
	c := category{mode: mode, paths: []string{}, pins: map[string]string{}}
	pathsOK := true
	if v, ok := m["paths"]; ok {
		list := util.List(v)
		pathsOK = list != nil
		for _, p := range list {
			s, isStr := p.(string)
			if !isStr || pyStrip(s) == "" {
				pathsOK = false
				break
			}
			c.paths = append(c.paths, s)
		}
	}
	must(pathsOK, fmt.Sprintf("%s.%s.paths must be an explicit list of local skill directories", label, name))
	pinsOK := true
	if v, ok := m["pins"]; ok {
		pins, isMap := v.(util.M)
		pinsOK = isMap
		for k, pv := range pins {
			s, isStr := pv.(string)
			if !isStr || !hashPattern.MatchString(s) {
				pinsOK = false
				break
			}
			c.pins[k] = s
		}
	}
	must(pinsOK, fmt.Sprintf("%s.%s.pins must map names to SHA256", label, name))
	must(mode != "none" || (len(c.paths) == 0 && len(c.pins) == 0),
		fmt.Sprintf("%s.%s: mode none cannot include paths or pins", label, name))
	if name != "skills" {
		must(mode == "none", fmt.Sprintf("%s.%s: selected/snapshot %s are unsupported; use mode none", label, name, name))
	} else {
		must(mode == "none" || len(c.paths) > 0,
			fmt.Sprintf("%s.skills.%s requires explicit paths, one per SKILL.md directory", label, mode))
	}
	return c
}

// NormalizeProfiles resolves and hashes explicit source trees once; it never
// discovers ambient skills. The result keeps private "_source"/"_identity"
// resolution data on each skill entry; do not publish them. Profiles are
// processed in name order (Python: declaration order).
func NormalizeProfiles(raw util.M, baseDir string) (out util.M, err error) {
	defer catch(&err)
	out = util.M{}
	for _, name := range util.SortedKeys(raw) {
		out[name] = normalizeProfile(name, raw[name], baseDir)
	}
	return out, nil
}

var profileFields = []string{"skills", "plugins", "hooks", "require_absence"}

func normalizeProfile(name string, rawValue any, baseDir string) util.M {
	label := "agent_configurations." + name
	must(idPattern.MatchString(name), "agent configuration names must be lowercase slugs")
	raw, ok := rawValue.(util.M)
	must(ok, label+" must be a table")
	var unknown []string
	for k := range raw {
		if !util.Contains(profileFields, k) {
			unknown = append(unknown, k)
		}
	}
	must(len(unknown) == 0, fmt.Sprintf("%s: unsupported profile fields %s", label, pyListRepr(sortedStrings(unknown))))
	var absence any = false
	if v, ok := raw["require_absence"]; ok {
		absence = v
	}
	_, isBool := absence.(bool)
	must(isBool, label+".require_absence must be boolean")
	profile := util.M{"name": name, "require_absence": absence}
	cats := map[string]category{}
	for _, c := range []string{"skills", "plugins", "hooks"} {
		var v any = util.M{}
		if rv, ok := raw[c]; ok {
			v = rv
		}
		cats[c] = normalizeCategory(v, c, label)
	}
	skills := cats["skills"]
	must(len(skills.paths) <= maxSkills, fmt.Sprintf("%s: too many skill sources (limit %d)", label, maxSkills))
	sources := []any{}
	names := map[string]bool{}
	var total int64
	for _, rawPath := range skills.paths {
		must(!strings.ContainsAny(rawPath, "*?[]{}\x00") && !strings.Contains(rawPath, "://"),
			label+": skill paths must be local directories, without globs or variables")
		p := expanduser(rawPath)
		if !filepath.IsAbs(p) {
			p = joinPath(baseDir, p)
		}
		must(!isSymlink(p), label+": skill directory symlinks are unsupported")
		p = resolvePath(p)
		must(p != "/" && p != resolvedHome() && safeComponent(filepath.Base(p)),
			label+": choose one bounded skill directory, not a home/configuration root")
		skillMD := filepath.Join(p, "SKILL.md")
		must(isFile(skillMD) && !isSymlink(skillMD),
			label+": each path must name one directory with its own regular SKILL.md; collection roots are unsupported")
		t := readTree(p)
		metadata := frontmatter(t.content["SKILL.md"], filepath.Base(p))
		checkSkillConfiguration(t)
		skillName := util.S(metadata["name"])
		must(!names[casefold(skillName)], label+": duplicate skill name "+skillName)
		names[casefold(skillName)] = true
		if pin, ok := skills.pins[skillName]; ok {
			must(pin == t.sha256, label+": SHA256 pin mismatch for "+skillName)
		}
		total += t.bytes
		must(total <= maxBytes, label+": combined skill content exceeds snapshot size limit")
		entry := util.Copy(metadata)
		entry["_source"] = p
		entry["_identity"] = t.identity
		entry["sha256"] = t.sha256
		entry["files"] = t.records()
		entry["bytes"] = t.bytes
		sources = append(sources, entry)
	}
	for pin := range skills.pins {
		must(names[pin], label+": pins include an unselected skill")
	}
	profile["skills"] = util.M{"mode": skills.mode, "entries": sources}
	for _, c := range []string{"plugins", "hooks"} {
		profile[c] = util.M{"mode": "none", "entries": []any{}}
	}
	return profile
}

// ResumeContract keeps profile semantics stable across source touches or
// identical checkouts. Filesystem identities protect the interval between
// resolution and copying; they are not part of a saved attempt's content
// contract, so they are retained only on the original profile. Returns nil
// for nil; the input is not modified.
func ResumeContract(profile util.M) util.M {
	if profile == nil {
		return nil
	}
	contract := util.MapOr(util.DeepCopy(profile))
	for _, entry := range util.Maps(util.Get(contract, "skills", "entries")) {
		delete(entry, "_identity")
		// Descriptive fields added after the first saved attempts; omit them when
		// undeclared so earlier matrix plans and attempt fingerprints still match.
		if entry["version"] == nil {
			delete(entry, "version")
		}
		if !util.Truthy(entry["declared_tool_grants"]) {
			delete(entry, "declared_tool_grants")
		}
	}
	return contract
}

// ---------------------------------------------------------------- validation

func controlledEnv(key string) bool {
	if envExact[key] {
		return true
	}
	for _, p := range envPrefixes {
		if strings.HasPrefix(key, p) {
			return !providerEnv[key]
		}
	}
	return false
}

// ControlledEnv reports whether an env var conflicts with profile controls.
func ControlledEnv(key string) bool { return controlledEnv(key) }

// orEmpty is Python's `value or []`/`value or {}` for optional fields.
func orEmpty(v any) any {
	if !util.Truthy(v) {
		return nil
	}
	return v
}

func envKeys(values any) []string {
	switch t := values.(type) {
	case nil:
		return nil
	case util.M:
		return util.SortedKeys(t)
	case map[string]string:
		return util.SortedKeys(t)
	}
	return util.Strings(values)
}

func validateEnv(values any, label string) {
	for _, key := range envKeys(values) {
		must(!controlledEnv(key), label+"."+key+" conflicts with agent_configuration controls; remove the override")
	}
}

func argvList(v any) ([]string, bool) {
	switch t := v.(type) {
	case nil:
		return nil, true
	case []string:
		return t, true
	case []any:
		out := make([]string, len(t))
		for i, x := range t {
			s, ok := x.(string)
			if !ok {
				return nil, false
			}
			out[i] = s
		}
		return out, true
	}
	return nil, false
}

var (
	argValues = map[string]map[string]bool{
		"codex":       setOf("--model", "-m", "--sandbox", "-s", "--color"),
		"claude-code": setOf("--model", "--effort", "--max-budget-usd", "--permission-mode"),
		"kiro-cli":    setOf("--model", "--effort", "--trust-tools"),
	}
	argSwitches = map[string]map[string]bool{
		"codex":       setOf("--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check"),
		"claude-code": setOf("--dangerously-skip-permissions", "--allow-dangerously-skip-permissions"),
		"kiro-cli":    setOf("--trust-all-tools", "--no-interactive"),
	}
	providerKey = regexp.MustCompile(`^model_providers\.[A-Za-z0-9_-]+\.[A-Za-z0-9_]+$`)
	flagPattern = regexp.MustCompile(`^--?[a-zA-Z0-9-]{1,64}$`)
)

// validateArgs allows only known harmless/model options; arbitrary flags are
// not a control boundary.
func validateArgs(v any, harness, label string) {
	args, ok := argvList(v)
	must(ok, label+" must be an argv list")
	values, switches := argValues[harness], argSwitches[harness]
	for i := 0; i < len(args); i++ {
		arg, value, sep := strings.Cut(args[i], "=")
		// Codex provider args are necessary for custom endpoints and env-key auth.
		if harness == "codex" && (arg == "-c" || arg == "--config" || strings.HasPrefix(args[i], "-c")) {
			if (arg == "-c" || arg == "--config") && !sep {
				i++
				must(i < len(args), label+": missing config assignment")
				value = args[i]
			} else if arg != "-c" && arg != "--config" {
				value = args[i][2:]
			}
			key, _, equals := strings.Cut(value, "=")
			key = pyStrip(key)
			must(equals && !strings.ContainsAny(value, "\n\r") &&
				(key == "model" || key == "model_reasoning_effort" || key == "model_provider" || providerKey.MatchString(key)),
				label+": config override conflicts with the explicit agent profile; only model/provider keys are allowed")
		} else if values[arg] {
			if !sep {
				i++
				must(i < len(args) && !strings.HasPrefix(args[i], "-"), label+": missing value for "+arg)
			}
		} else {
			safe := "<argument>"
			if flagPattern.MatchString(arg) {
				safe = arg
			}
			must(switches[arg] && !sep, label+": "+pyRepr(safe)+
				" is unsupported with an explicit agent profile; remove configuration/extension overrides")
		}
	}
}

// present reports whether an interface holds a non-nil value.
func present(v any) bool {
	if v == nil {
		return false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface, reflect.Func:
		return !rv.IsNil()
	}
	return true
}

func authConf(a plugin.Auth) util.M {
	if !present(a) || a.Base() == nil {
		return util.M{}
	}
	return util.MapOr(a.Base().Conf)
}

func harnessName(h plugin.Harness) string {
	if !present(h) {
		return ""
	}
	return h.Name()
}

// ValidateForCell validates requested controls, override conflicts and
// authentication compatibility without reading credentials or launching anything.
func ValidateForCell(profile, cell util.M, h plugin.Harness, auth plugin.Auth, environmentProfile util.M) (err error) {
	defer catch(&err)
	validateForCell(profile, cell, harnessName(h), auth, environmentProfile)
	return nil
}

func validateForCell(profile, cell util.M, name string, auth plugin.Auth, environmentProfile util.M) {
	if profile == nil {
		return
	}
	must(util.Contains(Harnesses, name), "agent_configuration is unsupported for harness "+pyRepr(name))
	must(!util.Truthy(profile["require_absence"]),
		"agent_configuration.require_absence is unsupported: a fresh HOME disables discovery but cannot hide host "+
			"skill paths or remove managed/built-in content; use activation controls or a qualified isolation backend")
	for _, c := range []string{"plugins", "hooks"} {
		table := util.MapOr(profile[c])
		must(util.S(table["mode"]) == "none" && !util.Truthy(table["entries"]),
			fmt.Sprintf("%s: selected %s and their dependencies are unsupported; use mode none", name, c))
	}
	for _, entry := range util.Maps(util.Get(profile, "skills", "entries")) {
		must(name == "claude-code" || !util.Truthy(entry["invocation_fields"]),
			fmt.Sprintf("%s: SKILL.md invocation controls for %s are Claude-specific; semantics cannot be preserved",
				name, util.Str(entry["name"])))
	}
	validateArgs(orEmpty(cell["args"]), name, "cell.args")
	validateArgs(orEmpty(cell["permission_args"]), name, "cell.permission_args")
	validateEnv(orEmpty(cell["env"]), "cell.env")
	runnerType := "local"
	switch r := cell["runner"].(type) {
	case string:
		if r != "" {
			runnerType = r
		}
	case util.M:
		if t, ok := r["type"]; ok {
			runnerType = util.S(t)
		}
	}
	env := util.MapOr(environmentProfile)
	var backend any = "local"
	if v, ok := env["backend"]; ok {
		backend = v
	} else if v, ok := env["type"]; ok {
		backend = v
	}
	if m, ok := backend.(util.M); ok {
		backend = "local"
		if t, ok := m["type"]; ok {
			backend = t
		}
	}
	b := util.S(backend)
	must((runnerType == "local" || runnerType == "container") && (b == "local" || b == "container"),
		"agent_configuration supports local or container EnvironmentRunner backends; "+
			"use an explicit [environments.NAME] profile instead of a legacy Docker/wrapper runner")
	conf := authConf(auth)
	if present(auth) {
		validateArgs(auth.ExtraArgs(), name, "auth.args")
		validateEnv(orEmpty(conf["env"]), "auth.env")
		isolated := append(append([]string{}, commonEnvKeys...), "CODEX_HOME", "CLAUDE_CONFIG_DIR", "KIRO_HOME")
		for _, pattern := range auth.Unset() {
			for _, k := range isolated {
				must(!fnmatch(k, pattern), "auth.unset "+pyRepr(pattern)+" conflicts with isolated agent directories")
			}
		}
		must(auth.Base().IsolatesConfig, fmt.Sprintf("%s: auth %s requires caller configuration; use isolated env/API-key "+
			"auth (Codex's copied auth.json is supported) for an explicit agent profile", name, pyRepr(auth.Name())))
		if name == "claude-code" {
			must(!util.Contains(envKeys(orEmpty(conf["env"])), "CLAUDE_CODE_OAUTH_TOKEN") &&
				!util.Contains(util.Strings(conf["required_env"]), "CLAUDE_CODE_OAUTH_TOKEN"),
				"claude-code profiles require API-key or cloud-provider auth; OAuth can sync unpinned account skills")
		}
	}
	if name == "kiro-cli" {
		declared := append(append(envKeys(orEmpty(conf["env"])), util.Strings(conf["required_env"])...),
			envKeys(orEmpty(cell["env"]))...)
		must(present(auth) && util.Contains(declared, "KIRO_API_KEY"),
			"kiro-cli profiles require an isolated env auth profile declaring KIRO_API_KEY in env or required_env; "+
				"stored kiro-login cannot be reused with a fresh HOME")
	}
}

// ---------------------------------------------------------------- context

func ctxRoot(ctx *plugin.Ctx, key string) string {
	switch key {
	case "home_dir":
		return ctx.HomeDir
	case "config_dir":
		return ctx.ConfigDir
	case "cache_dir":
		return ctx.CacheDir
	case "workspace":
		return ctx.Workspace
	}
	return ""
}

func ctxRoots(ctx *plugin.Ctx) map[string]string {
	roots := map[string]string{}
	for _, key := range rootKeys {
		value := ctxRoot(ctx, key)
		must(value != "" && filepath.IsAbs(value), "agent_configuration requires a fresh absolute ctx."+key)
		must(isDir(value) && !isSymlink(value), "ctx."+key+" must be a real per-attempt directory")
		for _, other := range rootKeys {
			must(!strings.Contains(purePath(value), "{"+other+"}"), "ctx."+key+" contains a reserved launch placeholder")
		}
		roots[key] = resolvePath(value)
	}
	for i, a := range rootKeys {
		for _, b := range rootKeys[i+1:] {
			must(!isRelativeTo(roots[a], roots[b]) && !isRelativeTo(roots[b], roots[a]),
				fmt.Sprintf("ctx.%s and ctx.%s must be separate directories, not nested or reused", a, b))
		}
	}
	must(roots["home_dir"] != resolvedHome(), "agent_configuration must never use the caller's HOME")
	return roots
}

func managedRunner(ctx *plugin.Ctx) (plugin.ManagedRunner, bool) {
	if ctx == nil || !present(ctx.Runner) {
		return nil, false
	}
	mr, ok := ctx.Runner.(plugin.ManagedRunner)
	return mr, ok
}

// backendOf is the runner's backend (EnvironmentRunner) or its name.
func backendOf(ctx *plugin.Ctx) string {
	if ctx == nil || !present(ctx.Runner) {
		return "local"
	}
	if b, ok := ctx.Runner.(interface{ Backend() string }); ok {
		return b.Backend()
	}
	return ctx.Runner.Name()
}

// probeContext: a backend probe never inherits auth or the worker's provider environment.
func probeContext(ctx *plugin.Ctx, mr plugin.ManagedRunner) *plugin.Ctx {
	env := util.MergeEnv(mr.EnvironmentEnv(ctx))
	p := ctx.Clone()
	p.Env, p.CheckEnv, p.Auth, p.UnsetEnv = env, env, nil, []string{}
	return p
}

func backendShell(command string, ctx *plugin.Ctx, env map[string]string) util.M {
	mr, _ := managedRunner(ctx)
	p := probeContext(ctx, mr)
	if len(env) == 0 {
		env = p.Env
	}
	result := util.MapOr(mr.Shell(command, p, 15, env))
	if ctx.Context().Err() != nil || util.Truthy(result["interrupted"]) {
		fail(util.ErrInterrupted) // Python: KeyboardInterrupt propagates out of runner.shell
	}
	return result
}

func exitCode(res util.M) (int, bool) {
	v, ok := res["exit_code"]
	if !ok || v == nil {
		return 0, false
	}
	if b, isBool := v.(bool); isBool {
		if b {
			return 1, true
		}
		return 0, true
	}
	f, isNum := util.Num(v)
	return int(f), isNum
}

type checkPath struct{ path, label string }

var discoveryScopes = map[string][]string{
	"codex": {".agents/skills", ".codex/config.toml", ".codex/hooks.json", ".codex/skills"},
	"claude-code": {".claude/settings.json", ".claude/settings.local.json", ".claude/skills",
		".claude/commands", ".claude/plugins", ".claude/agents"},
	"kiro-cli": {".kiro/agents", ".kiro/skills", ".kiro/settings.json", ".kiro/mcp.json",
		".kiro/hooks", ".kiro/powers", ".agents/skills", ".claude/skills"},
}

// scopeGuard checks in the execution backend, without reading any ambient settings.
func scopeGuard(ctx *plugin.Ctx, harness string) {
	workspace := resolvePath(ctx.Workspace)
	var checks []checkPath
	for _, parent := range append([]string{workspace}, parents(workspace)...) {
		for _, rel := range discoveryScopes[harness] {
			checks = append(checks, checkPath{joinPath(parent, rel), "workspace:" + rel})
		}
	}
	for _, p := range systemPaths[harness] {
		checks = append(checks, checkPath{p, "admin-configuration"})
	}
	found := ""
	if _, ok := managedRunner(ctx); ok {
		lines := make([]string, len(checks))
		labels := map[string]bool{}
		for i, c := range checks {
			q := util.ShellQuote(c.path)
			lines[i] = fmt.Sprintf("if [ -e %s ] || [ -L %s ]; then printf '%%s\\n' %s; exit 42; fi", q, q, util.ShellQuote(c.label))
			labels[c.label] = true
		}
		result := backendShell(strings.Join(lines, "\n")+"\nexit 0", ctx, nil)
		code, has := exitCode(result)
		must(!util.Truthy(result["timed_out"]) && has && (code == 0 || code == 42),
			fmt.Sprintf("%s: could not inspect discovery scopes inside the %s backend", harness, backendOf(ctx)))
		if code != 0 {
			found = pyStrip(util.S(result["stdout"]))
		}
		must(found == "" || labels[found], harness+": backend returned an invalid discovery-scope response")
	} else {
		must(backendOf(ctx) == "local", "non-local profile probes require EnvironmentRunner.shell")
		for _, c := range checks {
			if lexists(c.path) {
				found = c.label
				break
			}
		}
	}
	if found == "admin-configuration" {
		fail(&Error{Msg: harness + ": installed admin configuration/skills prevent verifying optional extension " +
			"controls; use an environment without that policy, or omit the explicit profile"})
	}
	must(found == "", fmt.Sprintf("%s: workspace or ancestor contains a discovery/configuration source "+
		"(%s), which can change explicit profile discovery; use a neutral workspace and fixture", harness, found))
}

var locationPattern = regexp.MustCompile(`^\{(home_dir|config_dir|cache_dir|workspace)\}/(.+)$`)

// containedPath resolves a recorded attempt-relative location without
// following replacement symlinks.
func containedPath(location string, ctx *plugin.Ctx) string {
	m := locationPattern.FindStringSubmatch(location)
	must(m != nil, "invalid recorded agent configuration location")
	var parts []string
	for _, part := range strings.Split(m[2], "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	must(!strings.HasPrefix(m[2], "/") && !util.Contains(parts, ".."), "unsafe recorded agent configuration location")
	p := ctxRoot(ctx, m[1])
	for _, part := range parts {
		p = joinPath(p, part)
		must(!isSymlink(p), "prepared agent configuration path was replaced by a symlink")
	}
	return p
}

type discoveryRoot struct {
	location string
	expected map[string]bool
}

// discoveryGuard checks configured discovery roots for extra entries added by
// setup or a previous turn.
func discoveryGuard(ctx *plugin.Ctx, manifest util.M) {
	harness := util.S(manifest["harness"])
	names := map[string]bool{}
	for _, s := range util.Maps(util.Get(manifest, "skills", "entries")) {
		names[util.Str(s["name"])] = true
	}
	var roots []discoveryRoot
	var forbidden []string
	switch harness {
	case "codex":
		roots = []discoveryRoot{{"{home_dir}/.agents/skills", names}, {"{config_dir}/skills", setOf(".system")}}
		forbidden = []string{"{config_dir}/config.toml", "{config_dir}/hooks.json",
			"{home_dir}/.codex/config.toml", "{home_dir}/.codex/skills"}
	case "claude-code":
		roots = []discoveryRoot{{"{config_dir}/skills", names}}
		forbidden = []string{"{config_dir}/commands", "{config_dir}/plugins", "{config_dir}/agents",
			"{home_dir}/.claude/skills", "{home_dir}/.claude/commands",
			"{config_dir}/settings.local.json", "{config_dir}/hooks.json"}
	default:
		roots = []discoveryRoot{{"{config_dir}/skills", names}, {"{config_dir}/agents", setOf("ajx-profile.json")}}
		forbidden = []string{"{config_dir}/settings.json", "{config_dir}/mcp.json", "{config_dir}/hooks",
			"{config_dir}/powers", "{home_dir}/.kiro/skills", "{home_dir}/.kiro/agents"}
	}
	for _, r := range roots {
		p := containedPath(r.location, ctx)
		if _, err := os.Stat(p); err == nil {
			must(isDir(p), "prepared skill discovery root is no longer a directory")
			entries, err := os.ReadDir(p)
			check(err)
			for _, e := range entries {
				must(r.expected[e.Name()], harness+": additional configuration/skills appeared after preparation; "+
					"cannot launch the promised profile")
			}
		}
	}
	for _, location := range forbidden {
		must(!lexists(containedPath(location, ctx)), harness+": additional harness configuration appeared after preparation")
	}
}

func expandStr(s string, ctx *plugin.Ctx) string {
	for _, key := range rootKeys {
		s = strings.ReplaceAll(s, "{"+key+"}", ctxRoot(ctx, key))
	}
	return s
}

// expand replaces context-directory placeholders in strings, lists and tables.
func expand(v any, ctx *plugin.Ctx) any {
	switch t := v.(type) {
	case string:
		return expandStr(t, ctx)
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = expand(x, ctx)
		}
		return out
	case []string:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = expandStr(x, ctx)
		}
		return out
	case util.M:
		out := make(util.M, len(t))
		for k, x := range t {
			out[k] = expand(x, ctx)
		}
		return out
	case map[string]string:
		out := make(util.M, len(t))
		for k, x := range t {
			out[k] = expandStr(x, ctx)
		}
		return out
	}
	return v
}

func expandEnv(v any, ctx *plugin.Ctx) map[string]string {
	out := map[string]string{}
	for k, x := range util.MapOr(expand(v, ctx)) {
		out[k] = util.Str(x)
	}
	return out
}

func validateRuntimeEnv(ctx *plugin.Ctx, launch util.M) {
	expected := expandEnv(launch["env"], ctx)
	if _, ok := expected["CLAUDE_CONFIG_DIR"]; ok {
		_, has := ctx.Env["CLAUDE_CODE_OAUTH_TOKEN"]
		must(!has, "claude-code profiles require API-key or cloud-provider auth; OAuth can sync unpinned account skills")
	}
	for _, key := range util.SortedKeys(ctx.Env) {
		if controlledEnv(key) {
			want, ok := expected[key]
			must(ok && ctx.Env[key] == want, "worker env "+key+" conflicts with the prepared agent profile")
		}
	}
	for _, pattern := range ctx.UnsetEnv {
		for k := range expected {
			must(!fnmatch(k, pattern), "worker unset_env "+pyRepr(pattern)+" removes profile controls")
		}
	}
}

// toml renders a launch config value as a TOML inline value (Python _toml).
func toml(v any) string {
	switch t := v.(type) {
	case util.M:
		parts := make([]string, 0, len(t))
		for _, k := range orderedKeys(t, []string{"path", "enabled"}) {
			parts = append(parts, pyJSONString(k, true)+"="+toml(t[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []any:
		parts := make([]string, len(t))
		for i, x := range t {
			parts[i] = toml(x)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []string:
		return toml(anyList(t))
	}
	return pyDumps(v, jsonOpts{ascii: true})
}

// ---------------------------------------------------------------- probe

var (
	versionPattern = regexp.MustCompile(`([0-9]+)\.([0-9]+)\.([0-9]+)`)
	minimumVersion = map[string][3]int64{"codex": {0, 160, 1}, "claude-code": {2, 1, 293}, "kiro-cli": {2, 28, 0}}
	helpArgs       = map[string][]string{"codex": {"exec", "--help"}, "claude-code": {"--help"}, "kiro-cli": {"chat", "--help"}}
	requiredFlags  = map[string][]string{
		"codex":       {"--strict-config", "--config", "--disable"},
		"claude-code": {"--setting-sources", "--settings", "--disable-slash-commands", "--strict-mcp-config"},
		"kiro-cli":    {"--agent", "--agent-engine", "--no-interactive"},
	}
	codexFeatures = []string{"plugins", "hooks", "apps", "skill_mcp_dependency_install"}
)

func binaryFor(harness string) string {
	switch harness {
	case "kiro-cli":
		return "kiro-cli-chat"
	case "claude-code":
		return "claude"
	}
	return "codex"
}

// disabledFeature is Python's (?m)^{feature}\s+.*\bfalse\s*$ (Unicode \s, \b).
func disabledFeature(feature string) *regexp.Regexp {
	ws := "[" + pyWS + "]"
	return regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(feature) + ws + `+(?:.*[^\pL\pN_\n])?false` + ws + `*$`)
}

func parseVersionPart(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 1<<63 - 1
	}
	return n
}

// errLocalProbe is a local capability command that could not run or timed out.
var errLocalProbe = errors.New("local capability check failed")

// runLocal runs a help/version command with a scratch environment; it
// returns stdout and the exit code, or an error when the command could not run.
func runLocal(parent context.Context, argv []string, cwd string, env map[string]string, timeout time.Duration) (string, int, error) {
	c, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(c, argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = util.EnvList(env)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if parent.Err() != nil {
		return "", -1, util.ErrInterrupted
	}
	if c.Err() != nil {
		return "", -1, errLocalProbe
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return stdout.String(), exit.ExitCode(), nil
	}
	if err != nil {
		return "", -1, err
	}
	return stdout.String(), 0, nil
}

// probe runs only help/version/config parsing, inside the chosen execution
// backend. Local discovery uses the trusted caller PATH, not a synthetic
// worker PATH. A container resolves its own executable. The resulting absolute
// path is persisted and used verbatim, including when a worker later changes PATH.
func probe(harness string, ctx *plugin.Ctx) util.M {
	binary := binaryFor(harness)
	backend := backendOf(ctx)
	mr, managed := managedRunner(ctx)
	var resolved string
	if backend == "container" {
		must(managed, "container agent profiles require EnvironmentRunner.shell")
		result := backendShell("command -v "+util.ShellQuote(binary), ctx, nil)
		if code, _ := exitCode(result); code != 0 && harness == "kiro-cli" {
			result = backendShell("command -v kiro-cli", ctx, nil)
		}
		if code, has := exitCode(result); has && code == 0 {
			resolved = pyStrip(util.S(result["stdout"]))
		}
	} else {
		resolved = whichFn(binary, os.Getenv("PATH"))
	}
	hint := ""
	if harness == "kiro-cli" {
		hint = " (Kiro's separate chat companion is needed when its launcher depends on the original HOME)"
	}
	must(resolved != "" && strings.HasPrefix(resolved, "/") && !strings.ContainsAny(resolved, "\n\x00"),
		fmt.Sprintf("%s: no supported CLI executable in the %s backend; install it in the pinned image "+
			"or on the local host before preparing a profile", harness, backend)+hint)
	scratch, err := os.MkdirTemp(ctx.CacheDir, "capabilities-")
	check(err)
	defer os.RemoveAll(scratch)
	var env map[string]string
	if managed {
		// EnvironmentRunner enforces its directory paths. Preparation precedes
		// auth.prepare, so these are empty harness directories, not a login home.
		env = util.MergeEnv(mr.EnvironmentEnv(ctx), map[string]string{"CODEX_HOME": ctx.ConfigDir,
			"CLAUDE_CONFIG_DIR": ctx.ConfigDir, "KIRO_HOME": ctx.ConfigDir, "TERM": "dumb"})
	} else {
		env = map[string]string{"PATH": filepath.Dir(resolved) + ":/usr/bin:/bin", "HOME": scratch,
			"CODEX_HOME": scratch, "CLAUDE_CONFIG_DIR": scratch, "KIRO_HOME": scratch,
			"XDG_CONFIG_HOME": scratch, "XDG_DATA_HOME": scratch, "XDG_CACHE_HOME": scratch, "TERM": "dumb"}
	}
	run := func(args []string, filter []string) string {
		argv := append([]string{resolved}, args...)
		if managed {
			command := util.ShellJoin(argv)
			if len(filter) > 0 {
				// Runner output is deliberately bounded. Filter long help text
				// inside the backend so flags at its beginning are not lost.
				patterns := make([]string, len(filter))
				for i, text := range filter {
					patterns[i] = "*" + util.ShellQuote(text) + "*"
				}
				command = "ajx_probe_text=$(" + command + "); ajx_probe_status=$?; " +
					`[ "$ajx_probe_status" -eq 0 ] || exit "$ajx_probe_status"; ` +
					`printf '%s\n' "$ajx_probe_text" | while IFS= read -r ajx_probe_line; do ` +
					`case "$ajx_probe_line" in ` + strings.Join(patterns, "|") +
					`) printf '%s\n' "$ajx_probe_line";; esac; done`
			}
			result := backendShell(command, ctx, env)
			code, has := exitCode(result)
			must(!util.Truthy(result["timed_out"]) && has && code == 0,
				fmt.Sprintf("%s: %s help/config check rejected required controls or could not run "+
					"the executable with the environment's PATH", harness, backend))
			return util.S(result["stdout"])
		}
		out, code, err := runLocalFn(ctx.Context(), argv, scratch, env, 15*time.Second)
		if err != nil {
			if util.Interrupted(err) {
				fail(err)
			}
			fail(&Error{Msg: harness + ": local capability check failed; no worker was launched"})
		}
		must(code == 0, harness+": local help/config check rejected required controls")
		return out
	}
	version := pyStrip(run([]string{"--version"}, nil))
	m := versionPattern.FindStringSubmatch(version)
	must(m != nil, harness+": cannot determine CLI version for profile controls")
	actual := [3]int64{parseVersionPart(m[1]), parseVersionPart(m[2]), parseVersionPart(m[3])}
	minimum := minimumVersion[harness]
	atLeast := actual[0] > minimum[0] || (actual[0] == minimum[0] &&
		(actual[1] > minimum[1] || (actual[1] == minimum[1] && actual[2] >= minimum[2])))
	must(atLeast && actual[0] == minimum[0], fmt.Sprintf("%s: profile controls require version %d.%d.%d or newer in the same major",
		harness, minimum[0], minimum[1], minimum[2]))
	required := requiredFlags[harness]
	helpText := run(helpArgs[harness], required)
	for _, flag := range required {
		must(strings.Contains(helpText, flag), fmt.Sprintf("%s: installed CLI help is missing required profile controls (%s)",
			harness, strings.Join(required, ", ")))
	}
	if harness == "codex" {
		// This CLI supports --strict-config for exec/resume, not `features`.
		// Inspect feature values here; exec/resume apply strict parsing later.
		var args []string
		config := codexConfig()
		for _, key := range codexConfigKeys {
			args = append(args, "-c", key+"="+toml(config[key]))
		}
		flags := run(append(args, "features", "list"), codexFeatures)
		for _, feature := range codexFeatures {
			must(disabledFeature(feature).MatchString(flags), "codex: cannot verify disabled feature "+feature+"; unsupported CLI")
		}
	}
	method := "local --version/--help"
	if managed {
		method = "backend --version/--help"
	}
	return util.M{"binary": resolved, "version": m[1] + "." + m[2] + "." + m[3], "backend": backend,
		"method": method, "config_parse_checked": false, "feature_overrides_checked": harness == "codex",
		"provider_called": false, "live_extension_activation_verified": false}
}

// ---------------------------------------------------------------- prepare

// walkTopDown is os.walk(top, followlinks=False): symlinks to directories are
// listed as directories but not entered; unreadable directories are skipped.
func walkTopDown(top string, fn func(current string, dirs, files []string)) {
	entries, err := os.ReadDir(top)
	if err != nil {
		return
	}
	var dirs, files []string
	for _, e := range entries {
		p := joinPath(top, e.Name())
		if e.IsDir() || (e.Type()&os.ModeSymlink != 0 && isDir(p)) {
			dirs = append(dirs, e.Name())
		} else {
			files = append(files, e.Name())
		}
	}
	fn(top, dirs, files)
	for _, d := range dirs {
		if p := joinPath(top, d); !isSymlink(p) {
			walkTopDown(p, fn)
		}
	}
}

// bootstrapGuard allows only the runner's known empty bootstrap directories
// before skills are copied.
func bootstrapGuard(ctx *plugin.Ctx, roots map[string]string, allowedAuth map[string]bool) {
	home := roots["home_dir"]
	allowed := map[string]bool{}
	if mr, ok := managedRunner(ctx); ok {
		for _, value := range mr.EnvironmentEnv(ctx) {
			candidate := purePath(value)
			if filepath.IsAbs(value) && isRelativeTo(candidate, home) && !strings.Contains(value, ":") {
				allowed[candidate] = true
				for p := filepath.Dir(candidate); isRelativeTo(p, home); p = filepath.Dir(p) {
					allowed[p] = true
					if p == filepath.Dir(p) {
						break
					}
				}
			}
		}
		allowed[joinPath(home, ".local/bin")] = true
		allowed[joinPath(home, ".local")] = true
	}
	walkTopDown(home, func(current string, dirs, files []string) {
		must(len(files) == 0, "agent_configuration requires a fresh empty home_dir (bootstrap directories only)")
		for _, name := range dirs {
			p := joinPath(current, name)
			must(!isSymlink(p) && allowed[p],
				"agent_configuration home_dir contains unexpected configuration; only empty bootstrap directories are allowed")
		}
	})
	entries, err := os.ReadDir(roots["config_dir"])
	check(err)
	for _, e := range entries {
		must(allowedAuth[e.Name()], "agent_configuration requires a fresh config_dir (only copied Codex auth.json is allowed)")
	}
}

// writeFile creates a new private file (never replacing one).
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return os.Chmod(path, 0o600)
}

func install(entry util.M, target string) {
	name := util.Str(entry["name"])
	source := util.S(entry["_source"])
	before := readTree(source)
	must(before.sha256 == util.S(entry["sha256"]) && sameIdentity(before.identity, entry["_identity"]),
		"skill "+name+" changed after resolution; reload the trial to pin the new content")
	check(os.MkdirAll(filepath.Dir(target), 0o777))
	check(os.Mkdir(target, 0o777))
	for _, item := range before.files {
		rel := util.S(item["path"])
		dst := joinPath(target, rel)
		if util.S(item["type"]) == "directory" {
			check(os.MkdirAll(dst, 0o777))
			continue
		}
		check(writeFileFn(dst, before.content[rel]))
		mode := os.FileMode(0o600)
		if util.Truthy(item["executable"]) {
			mode = 0o700
		}
		check(os.Chmod(dst, mode))
	}
	after := readTree(source)
	must(after.sha256 == util.S(entry["sha256"]) && sameIdentity(after.identity, entry["_identity"]),
		"skill "+name+" changed during copy; reload the trial")
	copied := readTree(target)
	must(copied.sha256 == util.S(entry["sha256"]), "copied skill "+name+" failed content verification")
}

// containerOwnership honors the runner's non-root UID when the host
// coordinator happens to be root.
func containerOwnership(ctx *plugin.Ctx) {
	if backendOf(ctx) != "container" || geteuidFn() != 0 {
		return
	}
	for _, key := range []string{"home_dir", "config_dir"} {
		root := ctxRoot(ctx, key)
		fi, err := os.Stat(root)
		check(err)
		owner := statOf(fi)
		walkTopDown(root, func(current string, dirs, files []string) {
			for _, name := range append(append([]string{}, dirs...), files...) {
				p := joinPath(current, name)
				must(!isSymlink(p), "agent configuration changed to a symlink during preparation")
				check(lchownFn(p, owner.uid, owner.gid))
			}
		})
	}
}

func grantLimitations(skills []any) []any {
	out := []any{}
	for _, s := range util.Maps(skills) {
		if util.Truthy(s["declared_tool_grants"]) {
			out = append(out, "skill "+util.Str(s["name"])+" declares tool grants (allowed-tools); AJX records them as "+
				"declared_tool_grants but does not enforce or restrict them, and a worker running with permission "+
				"prompts bypassed is not restricted by them.")
		}
	}
	return out
}

// Prepare copies pinned snapshots into the attempt's isolated directories,
// stores the manifest in ctx.AgentConfiguration and returns it. The caller
// persists the exact result before setup. A nil profile returns an empty
// manifest and leaves ctx unchanged.
func Prepare(profile util.M, ctx *plugin.Ctx, h plugin.Harness) (manifest util.M, err error) {
	if profile == nil {
		return util.M{}, nil
	}
	defer catch(&err)
	return prepare(profile, ctx, harnessName(h)), nil
}

type configFile struct {
	location string
	conf     util.M
}

func prepare(profile util.M, ctx *plugin.Ctx, name string) util.M {
	validateForCell(profile, ctx.Cell, name, ctx.Auth, ctx.EnvironmentProfile)
	backend := backendOf(ctx)
	must(backend == "local" || backend == "container",
		"agent_configuration requires a local or container EnvironmentRunner backend")
	validateEnv(orEmpty(util.Get(ctx.Spec, "env")), "trial.env")
	roots := ctxRoots(ctx)
	// auth.prepare may have copied Codex's auth.json; do not read it or archive its values.
	allowed := map[string]bool{}
	if name == "codex" && present(ctx.Auth) && ctx.Auth.Name() == "codex-chatgpt" {
		allowed["auth.json"] = true
	}
	bootstrapGuard(ctx, roots, allowed)
	scopeGuard(ctx, name)
	capability := probeFn(name, ctx)
	env := commonEnv()
	launch := util.M{"binary": capability["binary"], "args": []any{}, "env": env, "config": util.M{}}
	var skillRoot string
	mode := util.S(util.Get(profile, "skills", "mode"))
	switch name {
	case "codex":
		env["CODEX_HOME"] = "{config_dir}"
		launch["args"] = []any{"--strict-config"}
		launch["config"] = codexConfig()
		skillRoot = "{home_dir}/.agents/skills"
	case "claude-code":
		env["CLAUDE_CONFIG_DIR"] = "{config_dir}"
		env["DISABLE_AUTOUPDATER"] = "1"
		args := []any{"--setting-sources", "user", "--settings", "{config_dir}/settings.json", "--strict-mcp-config"}
		if mode == "none" {
			args = append(args, "--disable-slash-commands")
		}
		launch["args"] = args
		skillRoot = "{config_dir}/skills"
	default:
		env["KIRO_HOME"] = "{config_dir}"
		launch["args"] = []any{"--agent-engine", "v2", "--agent", "ajx-profile", "--no-interactive"}
		skillRoot = "{config_dir}/skills"
	}
	validateRuntimeEnv(ctx, launch)
	skills, files := []any{}, []any{}
	var owned []string
	func() {
		defer func() {
			if r := recover(); r != nil {
				for i := len(owned) - 1; i >= 0; i-- {
					if isDir(owned[i]) {
						os.RemoveAll(owned[i])
					} else {
						os.Remove(owned[i])
					}
				}
				panic(r)
			}
		}()
		scope := "isolated user"
		if name == "kiro-cli" {
			scope = "explicit agent resource"
		}
		for _, entry := range util.Maps(util.Get(profile, "skills", "entries")) {
			skillName := util.Str(entry["name"])
			location := skillRoot + "/" + skillName
			target := expandStr(location, ctx)
			owned = append(owned, target)
			install(entry, target)
			skill := util.M{}
			for _, k := range []string{"name", "sha256", "bytes", "files", "automatic_invocation", "user_invocable"} {
				skill[k] = util.DeepCopy(entry[k])
			}
			// Saved profiles from earlier attempts may predate these descriptive fields.
			skill["version"] = entry["version"]
			grants := util.List(util.DeepCopy(entry["declared_tool_grants"]))
			if grants == nil {
				grants = []any{}
			}
			skill["declared_tool_grants"] = grants
			states := util.M{"id": "skill:" + skillName, "source": skillName, "location": location,
				"installed": true, "discoverable": "unknown", "discovery_configured": true, "enabled": true,
				"enabled_basis": "launch configuration", "loaded": "unknown", "invoked": "unknown",
				"discovery_scope": scope}
			for k, v := range states {
				skill[k] = v
			}
			skills = append(skills, skill)
		}
		var configs []configFile
		switch name {
		case "codex":
			entries := make([]any, 0, len(skills))
			for _, s := range util.Maps(skills) {
				entries = append(entries, util.M{"path": util.S(s["location"]) + "/SKILL.md", "enabled": true})
			}
			util.MapOr(launch["config"])["skills.config"] = entries
		case "claude-code":
			configs = append(configs, configFile{"{config_dir}/settings.json", claudeSettings()})
		default:
			resources := make([]any, 0, len(skills))
			for _, s := range util.Maps(skills) {
				resources = append(resources, "skill://"+expandStr(util.S(s["location"]), ctx)+"/SKILL.md")
			}
			configs = append(configs, configFile{"{config_dir}/agents/ajx-profile.json", util.M{
				"name": "ajx-profile", "description": "Per-attempt skill profile",
				"tools": []any{"@builtin"}, "allowedTools": []any{}, "mcpServers": util.M{}, "includeMcpJson": false,
				"resources": resources, "hooks": util.M{},
			}})
		}
		for _, c := range configs {
			p := expandStr(c.location, ctx)
			data := []byte(pyDumps(c.conf, jsonOpts{ascii: true, indent: 2, sorted: true}) + "\n")
			check(writeFileFn(p, data))
			owned = append(owned, p)
			files = append(files, util.M{"location": c.location, "sha256": util.Sha256Bytes(data)})
		}
		containerOwnership(ctx)
	}()
	first := "Local isolation does not hide absolute host skill paths from shell/file tools."
	if backend != "local" {
		first = "Container filesystem controls hide unmounted host paths; skills present in the pinned image " +
			"or declared mounts can remain readable. Content absence is not verified by this profile."
	}
	limitations := append([]any{first,
		"Managed policy and built-in features may remain; their absence is not verified.",
		"Harness instructions, model knowledge, and task files are not removed by extension controls.",
		"All files within selected skill directories are pinned. External prose/runtime dependencies and " +
			"remote resources are not resolved or snapshotted.",
		"No model was called to verify activation; loaded/invoked remain unknown without telemetry.",
		"Installed content/configuration is checked again before execution and narration; writes during a turn " +
			"are not prevented by this profile.",
	}, grantLimitations(skills)...)
	unknownSource := func(id, control string) util.M {
		return util.M{"id": id, "installed": "unknown", "discoverable": "unknown", "enabled": "unknown",
			"loaded": "unknown", "invoked": "unknown", "control": control}
	}
	manifest := util.M{
		"schema_version": int64(1), "name": profile["name"], "harness": name, "backend": backend,
		"boundary": "optional activation", "content_absence": "unsupported",
		"capabilities": capability, "skills": util.M{"mode": mode, "entries": skills},
		"plugins": util.M{"mode": "none", "entries": []any{}, "optional_activation": "disabled", "invoked": "unknown"},
		"hooks":   util.M{"mode": "none", "entries": []any{}, "optional_activation": "disabled", "invoked": "unknown"},
		"uncontrolled_sources": []any{
			unknownSource("harness-builtins", "mandatory-or-unknown"),
			unknownSource("managed-policy", "unknown"),
		},
		"limitations":         limitations,
		"narration":           "retains worker launch configuration and verifies pinned snapshots",
		"configuration_files": files, "launch": launch,
	}
	ctx.AgentConfiguration = manifest
	return manifest
}

// ---------------------------------------------------------------- launch

// Configuration returns only an explicitly prepared profile
// (ctx.AgentConfiguration, else ctx.State["agent_configuration"]); it never
// infers one from config=clean. nil when none.
func Configuration(ctx *plugin.Ctx) util.M {
	if ctx == nil {
		return nil
	}
	if len(ctx.AgentConfiguration) > 0 {
		return ctx.AgentConfiguration
	}
	if m := util.Map(util.Get(ctx.State, "agent_configuration")); len(m) > 0 {
		return m
	}
	return nil
}

func verified(ctx *plugin.Ctx, name string) util.M {
	manifest := Configuration(ctx)
	if manifest == nil {
		must(!util.Truthy(ctx.Cell["agent_configuration"]),
			"agent_configuration was selected but not prepared; prepare and persist it before launching")
		return nil
	}
	schema, isNum := util.Num(manifest["schema_version"])
	must(isNum && schema == 1 && util.S(manifest["harness"]) == name,
		"agent configuration manifest does not match this harness; prepare a new attempt")
	ctxRoots(ctx)
	scopeGuard(ctx, name)
	validateArgs(orEmpty(ctx.Cell["args"]), name, "cell.args")
	validateArgs(orEmpty(ctx.Cell["permission_args"]), name, "cell.permission_args")
	validateEnv(orEmpty(ctx.Cell["env"]), "cell.env")
	validateEnv(orEmpty(util.Get(ctx.Spec, "env")), "trial.env")
	if present(ctx.Auth) {
		validateArgs(ctx.Auth.ExtraArgs(), name, "auth.args")
		validateEnv(orEmpty(authConf(ctx.Auth)["env"]), "auth.env")
	}
	validateRuntimeEnv(ctx, util.MapOr(manifest["launch"]))
	discoveryGuard(ctx, manifest)
	for _, entry := range util.Maps(util.Get(manifest, "skills", "entries")) {
		snapshot := readTree(containedPath(util.S(entry["location"]), ctx))
		must(snapshot.sha256 == util.S(entry["sha256"]),
			"installed skill "+util.Str(entry["name"])+" changed after preparation; cannot retain the worker profile")
	}
	for _, entry := range util.Maps(manifest["configuration_files"]) {
		p := containedPath(util.S(entry["location"]), ctx)
		must(!isSymlink(p) && isFile(p), "prepared harness configuration is missing or replaced")
		data, err := os.ReadFile(p)
		check(err)
		must(util.Sha256Bytes(data) == util.S(entry["sha256"]),
			"prepared harness configuration changed; cannot launch with the promised profile")
	}
	return manifest
}

// LaunchArgs verifies the prepared profile (roots, discovery scopes, override
// conflicts, runtime env, installed snapshots and configuration files) and
// returns its extra launch arguments; none without a profile.
func LaunchArgs(ctx *plugin.Ctx, h plugin.Harness) (args []string, err error) {
	defer catch(&err)
	manifest := verified(ctx, harnessName(h))
	if manifest == nil {
		return []string{}, nil
	}
	launch := util.MapOr(manifest["launch"])
	args = util.Strings(expand(launch["args"], ctx))
	if args == nil {
		args = []string{}
	}
	config := util.MapOr(launch["config"])
	for _, key := range orderedKeys(config, launchConfigOrder) {
		args = append(args, "-c", key+"="+toml(expand(config[key], ctx)))
	}
	return args, nil
}

// LaunchEnv is the profile's launch environment (empty without a profile).
// clean_env is also used by doctor before prepare; legacy behavior applies there.
func LaunchEnv(ctx *plugin.Ctx, h plugin.Harness) (env map[string]string, err error) {
	defer catch(&err)
	manifest := Configuration(ctx)
	if manifest == nil {
		return map[string]string{}, nil
	}
	must(util.S(manifest["harness"]) == harnessName(h), "profile environment belongs to a different harness")
	return expandEnv(util.Get(manifest, "launch", "env"), ctx), nil
}

// LaunchBinary is the profile's persisted absolute binary, else h's binary name.
func LaunchBinary(ctx *plugin.Ctx, h plugin.Harness) string {
	if manifest := Configuration(ctx); manifest != nil {
		return util.Str(util.Get(manifest, "launch", "binary"))
	}
	if !present(h) {
		return ""
	}
	return h.Base().BinaryName
}

// LaunchContext drops inherited extension/shell injection variables only for
// profiled launches. Harness.Run reapplies LaunchEnv after these inherited
// values are dropped. Provider variables are preserved, except Claude OAuth
// (account skill syncing). Managed runners (EnvironmentRunner) construct a full
// environment without inheritance and reject unsetting their HOME/XDG
// controls, so their ctx is returned unchanged, as is ctx without a profile.
func LaunchContext(ctx *plugin.Ctx) *plugin.Ctx {
	if Configuration(ctx) == nil {
		return ctx
	}
	if _, ok := managedRunner(ctx); ok {
		return ctx
	}
	drop := map[string]bool{"CLAUDE_CODE_OAUTH_TOKEN": true}
	for k := range envExact {
		drop[k] = true
	}
	for k := range util.Environ() {
		if controlledEnv(k) {
			drop[k] = true
		}
	}
	out := ctx.Clone()
	out.UnsetEnv = append(append([]string{}, ctx.UnsetEnv...), util.SortedKeys(drop)...)
	return out
}

// EnsureReporterContext rejects a reporter context that carries a worker
// profile: reporters must use a separate fixed context.
func EnsureReporterContext(ctx *plugin.Ctx) (err error) {
	defer catch(&err)
	var cell util.M
	if ctx != nil {
		cell = ctx.Cell
	}
	must(Configuration(ctx) == nil && !util.Truthy(cell["agent_configuration"]),
		"reporter must use a separate fixed context without the worker agent_configuration")
	return nil
}
