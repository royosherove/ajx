package environments

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

// Hooks for time and tests (Python tests patch time.time and _tail_process).
var (
	timeNow     = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	tailProcess = runTailProcess
	// writeMarkerData writes a lifecycle marker's bytes; replaceFile publishes it.
	writeMarkerData = func(w io.Writer, data []byte) error { _, err := w.Write(data); return err }
	replaceFile     = os.Rename
)

// EnvironmentRunner is a per-attempt runner (Python EnvironmentRunner);
// normalized profiles may be shared, runner instances may not. It implements
// plugin.ManagedRunner. Name() is the backend ("local" or "container"), as
// Python's EnvironmentRunner.name reports.
type EnvironmentRunner struct {
	plugin.RunnerBase
	profile util.M
	backend string
	initErr error

	freshOwner        string
	createdOwner      string
	prepared          util.M
	preparing         bool
	preparationFailed bool
	creationFailure   util.M
	released          bool
	dockerBinary      string
	coordinatorPath   string

	docker   DockerFunc
	reportFn func(ctx *plugin.Ctx, handle util.M, fresh bool) (util.M, error)
}

var (
	_ plugin.ManagedRunner        = (*EnvironmentRunner)(nil)
	_ plugin.CheckedManagedRunner = (*EnvironmentRunner)(nil)
)

// self dispatches internal lifecycle calls (Release, Abort, Wrap) through
// RunnerBase.Self, as Python's self does, so a wrapper that embeds the
// runner and sets Self observes them.
func (r *EnvironmentRunner) self() plugin.ManagedRunner {
	if mr, ok := r.Self.(plugin.ManagedRunner); ok && mr != nil {
		return mr
	}
	return r
}

// New builds a runner from a normalized profile (Python EnvironmentRunner(profile)).
// A profile without _base_dir is normalized (and its sources inspected) relative
// to the working directory. Recovery and cleanup do not depend on fixture
// sources still existing: a normalized profile is not re-inspected.
func New(profile util.M) (*EnvironmentRunner, error) {
	r := &EnvironmentRunner{}
	r.Self = r
	r.PluginName = "environment"
	r.docker = r.realDocker
	r.reportFn = r.report
	// PATH is runtime selection, not credential inheritance. Capture before worker execution.
	if p, ok := os.LookupEnv("PATH"); ok {
		r.coordinatorPath = p
	} else {
		r.coordinatorPath = managerPath
	}
	if profile == nil {
		return r, fail("environment", "expected a normalized profile")
	}
	raw := util.DeepCopy(profile).(util.M)
	baseValue, normalized := raw["_base_dir"]
	delete(raw, "_base_dir")
	base, _ := baseValue.(string)
	if !normalized || base == "" {
		base, _ = os.Getwd()
	}
	name := "environment"
	if id, ok := raw["id"]; ok {
		s, isStr := id.(string)
		if !isStr {
			return r, fail("environments", "expected a table")
		}
		name = s
	}
	profiles, err := normalizeProfiles(util.M{name: raw}, base, !normalized)
	if err != nil {
		r.initErr = err
		r.backend = util.S(raw["backend"])
		return r, err
	}
	r.profile = profiles[name].(util.M)
	r.Conf = r.profile
	r.backend = util.S(r.profile["backend"])
	return r, nil
}

// NewEnvironmentRunner builds a runner (Python EnvironmentRunner(profile)). When
// the profile cannot be normalized, every later operation returns that error.
func NewEnvironmentRunner(profile util.M) plugin.ManagedRunner {
	r, _ := New(profile)
	return r
}

// Err is the construction error, if any.
func (r *EnvironmentRunner) Err() error { return r.initErr }

// Profile is the normalized profile, including the internal _base_dir.
func (r *EnvironmentRunner) Profile() util.M { return r.profile }

// Backend is "local" or "container".
func (r *EnvironmentRunner) Backend() string { return r.backend }

// Name is the backend, as Python's EnvironmentRunner.name reports.
func (r *EnvironmentRunner) Name() string {
	if r.backend == "" {
		return "environment"
	}
	return r.backend
}

// LocalTranscripts: harness transcripts are host files under the attempt roots.
func (r *EnvironmentRunner) LocalTranscripts() bool { return true }

// Describe is the profile without internal fields.
func (r *EnvironmentRunner) Describe() util.M {
	out := util.M{}
	for k, v := range r.profile {
		if !strings.HasPrefix(k, "_") {
			out[k] = util.DeepCopy(v)
		}
	}
	return out
}

func (r *EnvironmentRunner) limits() util.M { return util.MapOr(r.profile["limits"]) }
func (r *EnvironmentRunner) id() string     { return util.S(r.profile["id"]) }

func (r *EnvironmentRunner) limitInt(key string) int64 {
	f, _ := util.Num(r.limits()[key])
	return int64(f)
}

// ------------------------------------------------------------------ environment construction

// ChildEnvChecked is a full process environment. It never inherits the
// caller's environment, proxies, or host credentials (Python child_env).
func (r *EnvironmentRunner) ChildEnvChecked(overrides map[string]string, unset []string) (map[string]string, error) {
	if err := validateUnset(unset, ""); err != nil {
		return nil, err
	}
	extra, err := checkedEnv(overrides, "")
	if err != nil {
		return nil, err
	}
	for _, key := range util.SortedKeys(extra) {
		if isolationEnv[key] && dropped(key, unset) {
			return nil, fail("environment.unset_env", "cannot unset the isolated "+key+" path")
		}
	}
	out := map[string]string{}
	for k, v := range util.MergeEnv(baseEnv, extra) {
		if !dropped(k, unset) {
			out[k] = v
		}
	}
	return out, nil
}

// ChildEnv is ChildEnvChecked for the plugin contract. On a configuration
// error it returns an empty environment (never the caller's); Wrap then
// reports the same error.
func (r *EnvironmentRunner) ChildEnv(overrides map[string]string, unset []string) map[string]string {
	env, err := r.ChildEnvChecked(overrides, unset)
	if err != nil {
		return map[string]string{}
	}
	return env
}

// EnvironmentEnvChecked is the per-attempt path environment (HOME, XDG_*, caches, PATH).
func (r *EnvironmentRunner) EnvironmentEnvChecked(ctx *plugin.Ctx) (map[string]string, error) {
	if r.initErr != nil {
		return nil, r.initErr
	}
	paths, err := r.paths(ctx)
	if err != nil {
		return nil, err
	}
	return pathEnvironment(paths["home_dir"], paths["config_dir"], paths["cache_dir"]), nil
}

// EnvironmentEnv is EnvironmentEnvChecked for the plugin contract ({} on error).
func (r *EnvironmentRunner) EnvironmentEnv(ctx *plugin.Ctx) map[string]string {
	env, err := r.EnvironmentEnvChecked(ctx)
	if err != nil {
		return map[string]string{}
	}
	return env
}

func ctxRoot(ctx *plugin.Ctx, key string) string {
	if ctx == nil {
		return ""
	}
	switch key {
	case "workspace":
		return ctx.Workspace
	case "config_dir":
		return ctx.ConfigDir
	case "cache_dir":
		return ctx.CacheDir
	case "home_dir":
		return ctx.HomeDir
	}
	return ""
}

func ctxState(ctx *plugin.Ctx) util.M {
	if ctx == nil || ctx.State == nil {
		return util.M{}
	}
	return ctx.State
}

func ctxRunDir(ctx *plugin.Ctx) string {
	if ctx == nil {
		return ""
	}
	return ctx.RunDir
}

// workerRoots includes current, archived, original, and explicitly writable mount paths.
func (r *EnvironmentRunner) workerRoots(ctx *plugin.Ctx) (map[string]bool, error) {
	state := ctxState(ctx)
	handle := util.MapOr(state["environment"])
	var values []any
	for _, key := range Roots {
		values = append(values, ctxRoot(ctx, key))
	}
	for _, mapping := range []any{state["paths"], state["archived"], handle["paths"]} {
		m := util.MapOr(mapping)
		for _, key := range Roots {
			values = append(values, m[key])
		}
	}
	values = append(values, util.List(state["owned_paths"])...)
	for _, m := range util.Maps(handle["mounts"]) {
		if m["access"] == "write" {
			values = append(values, m["host_source"])
		}
	}
	roots := map[string]bool{}
	for _, value := range values {
		s, ok := value.(string)
		if !ok || s == "" {
			continue
		}
		if !isAbs(s) {
			return nil, fail("environment.host_env", "worker ownership paths must be absolute")
		}
		roots[filepath.Clean(s)] = true
		roots[realpath(s)] = true
	}
	return roots, nil
}

func insideWorker(path string, roots map[string]bool) bool {
	lexical := filepath.Clean(path)
	resolved := realpath(path)
	for _, candidate := range []string{lexical, resolved} {
		for root := range roots {
			if pyPath(candidate) == pyPath(root) || relativeTo(candidate, root) {
				return true
			}
		}
	}
	return false
}

// HostEnvChecked is the private host-check environment; never use worker
// HOME/PATH for trusted verification (Python host_env). It creates only owned
// empty 0700 directories under run_dir/host-environment. Explicit auth/task
// values are retained, but startup/configuration controls are rebuilt. No
// caller credential, proxy, shell-startup, or user config is inherited. The
// result is for a subprocess, not a durable record.
func (r *EnvironmentRunner) HostEnvChecked(ctx *plugin.Ctx, overrides map[string]string, unset []string) (map[string]string, error) {
	if r.initErr != nil {
		return nil, r.initErr
	}
	roots, err := r.workerRoots(ctx)
	if err != nil {
		return nil, err
	}
	runDir, err := directory(ctxRunDir(ctx), "environment.host_env.run_dir", true)
	if err != nil {
		return nil, err
	}
	private := pathJoin(runDir, "host-environment")
	for root := range roots {
		if overlaps(private, root) {
			return nil, fail("environment.host_env", "private host context overlaps a worker-owned directory")
		}
	}
	home, config, cache := pathJoin(private, "home"), pathJoin(private, "config"), pathJoin(private, "cache")
	fixed := pathEnvironment(home, config, cache)
	for key, dir := range map[string]string{
		"YARN_CACHE_FOLDER": "yarn", "PNPM_STORE_DIR": "pnpm", "POETRY_CACHE_DIR": "poetry",
		"GOMODCACHE": "go-mod", "BUN_INSTALL_CACHE_DIR": "bun", "DENO_DIR": "deno",
		"NUGET_PACKAGES": "nuget", "GRADLE_USER_HOME": "gradle",
	} {
		fixed[key] = pathJoin(cache, dir)
	}
	directories := []string{private, config, cache}
	for _, k := range util.SortedKeys(fixed) {
		if k != "PATH" {
			directories = append(directories, fixed[k])
		}
	}
	var pathEntries []string
	for _, raw := range strings.Split(r.coordinatorPath, string(os.PathListSeparator)) {
		if raw == "" || strings.ContainsAny(raw, "\x00\n\r") || !isAbs(raw) {
			continue
		}
		if insideWorker(raw, roots) {
			continue
		}
		resolved := realpath(raw)
		if !util.Contains(pathEntries, resolved) {
			pathEntries = append(pathEntries, resolved)
		}
	}
	if len(pathEntries) == 0 {
		return nil, fail("environment.host_env.PATH", "coordinator PATH has no absolute runtime directory outside worker-owned paths")
	}
	for k, v := range map[string]string{
		"PATH": strings.Join(pathEntries, string(os.PathListSeparator)), "SHELL": "/bin/sh", "ZDOTDIR": home,
		"XDG_CONFIG_DIRS": config, "XDG_DATA_DIRS": "/usr/local/share:/usr/share",
		"XDG_RUNTIME_DIR": pathJoin(cache, "runtime"), "CODEX_HOME": pathJoin(config, "codex"),
		"CLAUDE_CONFIG_DIR": pathJoin(config, "claude"), "KIRO_HOME": pathJoin(config, "kiro"),
		"DOCKER_CONFIG":  pathJoin(config, "docker"),
		"PYTHONSAFEPATH": "1", "PYTHONNOUSERSITE": "1", "PYTHONSTARTUP": "/dev/null",
		"PYTHONPYCACHEPREFIX": pathJoin(cache, "pycache"),
		"GIT_CONFIG_NOSYSTEM": "1", "GEM_HOME": pathJoin(home, ".local", "gems"),
		"GEM_PATH":         pathJoin(home, ".local", "gems"),
		"BUNDLE_USER_HOME": pathJoin(config, "bundle"), "BUNDLE_APP_CONFIG": pathJoin(config, "bundle"),
	} {
		fixed[k] = v
	}
	files := map[string]string{"GIT_CONFIG_GLOBAL": pathJoin(config, "git", "config"),
		"PIP_CONFIG_FILE":       pathJoin(config, "pip", "pip.conf"),
		"npm_config_userconfig": pathJoin(config, "npm", "npmrc"),
		"HISTFILE":              pathJoin(home, ".local", "state", "shell_history"),
		"INPUTRC":               pathJoin(config, "readline", "inputrc")}
	for k, v := range files {
		fixed[k] = v
	}
	for _, lower := range []string{"npm_config_cache", "npm_config_prefix", "npm_config_userconfig"} {
		fixed[strings.ToUpper(lower)] = fixed[lower]
	}
	for _, k := range util.SortedKeys(files) {
		directories = append(directories, parentOf(files[k]))
	}
	for _, key := range []string{"XDG_RUNTIME_DIR", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "KIRO_HOME", "DOCKER_CONFIG",
		"PYTHONPYCACHEPREFIX", "GEM_HOME", "BUNDLE_USER_HOME"} {
		directories = append(directories, fixed[key])
	}
	raw := map[string]string{}
	if ctx != nil {
		for k, v := range ctx.Env {
			raw[k] = v
		}
		for k, v := range ctx.CheckEnv {
			raw[k] = v
		}
	}
	for k, v := range overrides {
		raw[k] = v
	}
	selected := map[string]string{}
	for key, value := range raw {
		if _, isFixed := fixed[key]; isFixed || hostDrop[key] || hasAnyPrefix(key, hostDropPrefixes) {
			continue
		}
		selected[key] = value
	}
	supplied, err := checkedEnv(selected, "environment.host_env")
	if err != nil {
		return nil, err
	}
	for key := range raw {
		if !envNameRE.MatchString(key) {
			return nil, fail("environment.host_env", "invalid environment variable name")
		}
	}
	for _, key := range util.SortedKeys(supplied) {
		if !hostExternalFiles[key] || supplied[key] == "" {
			continue
		}
		value := supplied[key]
		candidates := []string{value}
		if key == "KUBECONFIG" || key == "BOTO_PATH" {
			candidates = strings.Split(value, string(os.PathListSeparator))
		}
		for _, p := range candidates {
			if !isAbs(p) || insideWorker(p, roots) {
				return nil, fail("environment.host_env."+key,
					"host checks cannot load configuration/credentials from relative or worker-owned paths; "+
						"supply a coordinator-side path or explicit credential values")
			}
		}
	}
	var drop []string
	if ctx != nil {
		drop = append(drop, ctx.UnsetEnv...)
	}
	drop = append(drop, unset...)
	if err := validateUnset(drop, "environment.host_env.unset"); err != nil {
		return nil, err
	}
	for _, key := range util.SortedKeys(fixed) {
		if dropped(key, drop) {
			return nil, fail("environment.host_env.unset", "cannot unset the private host control "+key)
		}
	}
	if err := privateHostDirectories(runDir, directories); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range util.MergeEnv(baseEnv, supplied, fixed) {
		if !dropped(k, drop) {
			out[k] = v
		}
	}
	return out, nil
}

// HostEnv is HostEnvChecked for the plugin contract. On error it returns an
// empty environment (never the caller's); use HostEnvChecked (or
// plugin.ContextShellChecked) to surface the error.
func (r *EnvironmentRunner) HostEnv(ctx *plugin.Ctx, overrides map[string]string, unset []string) map[string]string {
	env, err := r.HostEnvChecked(ctx, overrides, unset)
	if err != nil {
		return map[string]string{}
	}
	return env
}

// WorkerEnv is the worker's full process environment for ctx plus env (Python _worker_env).
func (r *EnvironmentRunner) WorkerEnv(ctx *plugin.Ctx, env map[string]string) (map[string]string, error) {
	isolated, err := r.EnvironmentEnvChecked(ctx)
	if err != nil {
		return nil, err
	}
	var ctxEnv map[string]string
	var unset []string
	if ctx != nil {
		ctxEnv, unset = ctx.Env, ctx.UnsetEnv
	}
	extra, err := checkedEnv(util.MergeEnv(ctxEnv, env), "")
	if err != nil {
		return nil, err
	}
	for _, key := range util.SortedKeys(extra) {
		if isolationEnv[key] && extra[key] != isolated[key] {
			return nil, fail("environment.env."+key, "cannot override the per-attempt path")
		}
	}
	return r.ChildEnvChecked(util.MergeEnv(isolated, extra), unset)
}

// paths validates the four private attempt roots (Python _paths).
func (r *EnvironmentRunner) paths(ctx *plugin.Ctx) (map[string]string, error) {
	paths := map[string]string{}
	for _, key := range Roots {
		p, err := directory(ctxRoot(ctx, key), "environment.ctx."+key, false)
		if err != nil {
			return nil, err
		}
		paths[key] = p
	}
	report, err := directory(ctxRunDir(ctx), "environment.ctx.run_dir", false)
	if err != nil {
		return nil, err
	}
	home := realpath(util.Home())
	base := util.S(r.profile["_base_dir"])
	for i, key := range Roots {
		path := paths[key]
		system := false
		for _, p := range systemTargets {
			if overlaps(path, p) {
				system = true
			}
		}
		if overlaps(path, report) || path == home || relativeTo(home, path) || relativeTo(base, path) || system {
			return nil, fail("environment.ctx."+key, "must be a private root disjoint from reports, system paths, and the host home")
		}
		for _, other := range Roots[:i] {
			if overlaps(path, paths[other]) {
				return nil, fail("environment.ctx."+key, "overlaps "+other+"; use separate per-attempt roots")
			}
		}
	}
	return paths, nil
}

func (r *EnvironmentRunner) profileHash() string {
	sum := sha256.Sum256([]byte(pyDumps(r.profile, true)))
	return hex.EncodeToString(sum[:])
}

func uuidHex() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:])
}

func sortedEnvNames(env map[string]string) []any { return toAnyStrings(util.SortedKeys(env)) }

func pathsM(paths map[string]string) util.M {
	out := util.M{}
	for k, v := range paths {
		out[k] = v
	}
	return out
}

// handlePaths iterates a handle's paths in the planned root order.
func handlePaths(handle util.M) [][2]string {
	m := util.MapOr(handle["paths"])
	var out [][2]string
	seen := map[string]bool{}
	for _, key := range Roots {
		if v, ok := m[key]; ok {
			out = append(out, [2]string{key, util.Str(v)})
			seen[key] = true
		}
	}
	for _, key := range util.SortedKeys(m) {
		if !seen[key] {
			out = append(out, [2]string{key, util.Str(m[key])})
		}
	}
	return out
}

func handlePath(handle util.M, key string) string { return util.S(util.Get(handle, "paths", key)) }

// ------------------------------------------------------------------ plan / handle

// Plan returns the durable ownership intent. Persist it VERBATIM in
// ctx.State["environment"] before Prepare. A persisted plan is validated and
// returned unchanged; plan never provisions anything.
func (r *EnvironmentRunner) Plan(ctx *plugin.Ctx) (util.M, error) {
	if r.initErr != nil {
		return nil, r.initErr
	}
	state := ctxState(ctx)
	if existing, ok := state["environment"]; ok {
		if _, err := r.handle(ctx, false); err != nil {
			return nil, err
		}
		return util.MapOr(util.DeepCopy(existing)), nil
	}
	paths, err := r.paths(ctx)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"home_dir", "config_dir", "cache_dir"} {
		path := paths[key]
		if lexists(path) && dirNotEmpty(path) && !callerOwned(ctx, path) {
			return nil, fail("environment.ctx."+key, "must start empty; live host configuration and warm caches are not inherited")
		}
		if lexists(path) {
			if _, err := treeManifest(path, "environment.ctx."+key, treeOpts{}); err != nil {
				return nil, err
			}
		}
	}
	workspace := paths["workspace"]
	if lexists(workspace) {
		if _, err := treeManifest(workspace, "environment.ctx.workspace", treeOpts{}); err != nil {
			return nil, err
		}
	}
	workerEnv, err := r.WorkerEnv(ctx, nil)
	if err != nil {
		return nil, err
	}
	owner := uuidHex()
	plan := util.M{"version": int64(1), "id": r.id(), "backend": r.backend, "owner": owner,
		"profile_sha256": r.profileHash(), "paths": pathsM(paths), "created_at": util.Now(),
		"env_names": sortedEnvNames(workerEnv), "mounts": []any{}}
	mounts := []any{}
	runDir := ctxRunDir(ctx)
	for i, mount := range util.Maps(r.profile["mounts"]) {
		source, tgt := util.S(mount["source"]), util.S(mount["target"])
		others := append(mapValues(paths), runDir)
		for _, p := range others {
			if overlaps(source, p) {
				return nil, fail("environments."+r.id()+".mounts["+itoa(i)+"].source", "overlaps attempt or reporting directories")
			}
		}
		for _, p := range others {
			if overlaps(tgt, p) {
				return nil, fail("environments."+r.id()+".mounts["+itoa(i)+"].target", "overlaps an attempt or report root")
			}
		}
		manifest, err := treeManifest(source, "environment.mounts["+itoa(i)+"].source", treeOpts{fixtures: true})
		if err != nil {
			return nil, err
		}
		write := mount["access"] == "write"
		projected, disposition := source, "read-only"
		if write {
			projected, disposition = pathJoin(workspace, ".ajx-mounts", itoa(i)), "attempt-copy"
		}
		entry := util.Copy(mount)
		for k, v := range manifest {
			entry[k] = v
		}
		entry["host_source"] = projected
		entry["write_disposition"] = disposition
		mounts = append(mounts, entry)
	}
	plan["mounts"] = mounts
	if r.backend == "container" {
		expires := timeNow() + float64(r.limitInt("lifetime_seconds"))
		// A whole-number float would round-trip through Go's JSON encoder as an
		// integer, changing str(expires_at) for the Python CLI's label check.
		for expires == math.Trunc(expires) {
			expires = math.Nextafter(expires, math.Inf(1))
		}
		plan["name"] = "ajx-" + owner
		plan["docker_host"] = r.profile["docker_host"]
		plan["docker_config_dir"] = pathJoin(realpath(runDir), "environment-docker-"+owner)
		plan["expires_at"] = expires
		plan["worker_user"] = workerUser()
		image, err := r.inspectImage(plan)
		if err != nil {
			return nil, err
		}
		plan["image"] = image
		daemon, err := r.daemon(plan)
		if err != nil {
			return nil, err
		}
		plan["daemon"] = daemon
		plan["labels"] = util.M{labelPrefix + "owner": owner, labelPrefix + "profile": plan["profile_sha256"],
			labelPrefix + "version": "1", labelPrefix + "expires": pyStr(expires)}
	}
	r.freshOwner = owner
	return util.MapOr(util.DeepCopy(plan)), nil
}

func workerUser() string {
	uid, gid := getuid(), getgid()
	if uid == 0 {
		uid = 1000
	}
	if gid == 0 {
		gid = 1000
	}
	return itoa(uid) + ":" + itoa(gid)
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, k := range Roots {
		if v, ok := m[k]; ok {
			out = append(out, v)
		}
	}
	return out
}

func dirNotEmpty(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	names, _ := f.Readdirnames(1)
	return len(names) > 0
}

// pyStr is Python str() for a JSON-shaped scalar.
func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case float64:
		return pyFloatRepr(t)
	case json.Number:
		return t.String()
	}
	return util.Str(v)
}

func isIntOne(v any) bool {
	switch t := v.(type) {
	case int64:
		return t == 1
	case int:
		return t == 1
	case float64:
		return t == 1
	case json.Number:
		return t.String() == "1"
	}
	return false
}

func equalPaths(recorded any, paths map[string]string) bool {
	m, ok := recorded.(util.M)
	if !ok || len(m) != len(paths) {
		return false
	}
	for k, v := range paths {
		s, isStr := m[k].(string)
		if !isStr || s != v {
			return false
		}
	}
	return true
}

func equalStringMap(recorded any, expected map[string]string) bool {
	m, ok := recorded.(util.M)
	if !ok || len(m) != len(expected) {
		return false
	}
	for k, v := range expected {
		s, isStr := m[k].(string)
		if !isStr || s != v {
			return false
		}
	}
	return true
}

// handle validates the persisted plan (Python _handle).
func (r *EnvironmentRunner) handle(ctx *plugin.Ctx, cleanup bool) (util.M, error) {
	if r.initErr != nil {
		return nil, r.initErr
	}
	handle, ok := ctxState(ctx)["environment"].(util.M)
	if !ok || !isIntOne(handle["version"]) || !ownerRE.MatchString(util.Str(handle["owner"])) ||
		!containerIDRE.MatchString(util.Str(handle["profile_sha256"])) {
		return nil, envError("Persist the environment plan in state['environment'] before preparation or execution")
	}
	if handle["backend"] != r.backend {
		return nil, envError("Persisted environment backend differs from the requested backend")
	}
	if !cleanup {
		if handle["profile_sha256"] != r.profileHash() {
			return nil, envError("Environment profile/paths changed; do not reuse or rerun the attempt")
		}
		paths, err := r.paths(ctx)
		if err != nil {
			return nil, err
		}
		if !equalPaths(handle["paths"], paths) {
			return nil, envError("Environment profile/paths changed; do not reuse or rerun the attempt")
		}
	}
	if r.backend == "container" {
		owner := util.Str(handle["owner"])
		if handle["name"] != "ajx-"+owner {
			return nil, envError("Invalid persisted container ownership name")
		}
		expected := map[string]string{labelPrefix + "owner": owner, labelPrefix + "profile": util.Str(handle["profile_sha256"]),
			labelPrefix + "version": "1", labelPrefix + "expires": pyStr(handle["expires_at"])}
		if !equalStringMap(handle["labels"], expected) || !imageIDRE.MatchString(util.S(util.Get(handle, "image", "id"))) {
			return nil, envError("Invalid persisted container ownership/image handle")
		}
		_, isBool := handle["expires_at"].(bool)
		expires, isNum := util.Num(handle["expires_at"])
		if (!isNum && !isBool) || math.IsNaN(expires) || math.IsInf(expires, 0) {
			return nil, envError("Invalid persisted container expiry")
		}
		if _, err := dockerHost(handle["docker_host"], "environment.state.docker_host"); err != nil {
			return nil, err
		}
		config := pyPath(util.S(handle["docker_config_dir"]))
		if config != pathJoin(realpath(ctxRunDir(ctx)), "environment-docker-"+owner) {
			return nil, envError("Invalid persisted Docker configuration path")
		}
		if !workerUserRE.MatchString(util.S(handle["worker_user"])) {
			return nil, envError("Invalid persisted non-root worker identity")
		}
		if !cleanup {
			declared := util.Maps(r.profile["mounts"])
			recorded := util.Maps(handle["mounts"])
			same := len(declared) == len(recorded) && len(util.List(handle["mounts"])) == len(recorded)
			for i := 0; same && i < len(declared); i++ {
				for _, k := range []string{"source", "target", "access"} {
					if recorded[i][k] != declared[i][k] {
						same = false
					}
				}
			}
			if !same {
				return nil, envError("Persisted mounts differ from the declared profile")
			}
			for i, mount := range recorded {
				expectedSource := util.S(mount["source"])
				if mount["access"] == "write" {
					expectedSource = pathJoin(handlePath(handle, "workspace"), ".ajx-mounts", itoa(i))
				}
				if mount["host_source"] != expectedSource {
					return nil, envError("Invalid persisted mount source")
				}
			}
		}
	}
	return handle, nil
}

// callerOwned: the coordinator may stage declared auth/extensions in its durably allocated fresh roots.
func callerOwned(ctx *plugin.Ctx, path string) bool {
	state := ctxState(ctx)
	owned := false
	for _, p := range util.List(state["owned_paths"]) {
		if p == path {
			owned = true
		}
	}
	if !owned {
		return false
	}
	for _, v := range util.MapOr(state["paths"]) {
		if v == path {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------ lifecycle markers

func (r *EnvironmentRunner) markerPath(ctx *plugin.Ctx, handle util.M) string {
	return pathJoin(ctxRunDir(ctx), "environment-"+util.Str(handle["owner"])+".json")
}

// MarkerPath is the attempt's lifecycle marker file.
func (r *EnvironmentRunner) MarkerPath(ctx *plugin.Ctx, handle util.M) string {
	return r.markerPath(ctx, handle)
}

func decodeNumbers(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("extra data")
	}
	return v, nil
}

func (r *EnvironmentRunner) readMarker(ctx *plugin.Ctx, handle util.M) (util.M, error) {
	marker := r.markerPath(ctx, handle)
	if isSymlink(marker) {
		return nil, envError("Environment lifecycle marker must not be a symlink")
	}
	raw, err := os.ReadFile(marker)
	var data any
	if err == nil {
		data, err = decodeNumbers(raw)
	}
	if err != nil {
		return nil, envError("Environment preparation was not confirmed; recovery will not recreate it")
	}
	m, ok := data.(util.M)
	if !ok || m["owner"] != handle["owner"] || m["profile_sha256"] != handle["profile_sha256"] {
		return nil, envError("Environment lifecycle marker ownership differs from the plan")
	}
	return m, nil
}

func (r *EnvironmentRunner) writeMarker(ctx *plugin.Ctx, handle util.M, status string, initial bool) error {
	marker := r.markerPath(ctx, handle)
	data := util.M{"owner": handle["owner"], "profile_sha256": handle["profile_sha256"], "status": status}
	if status == "prepared" {
		identity := util.M{}
		for _, kv := range handlePaths(handle) {
			info, err := pathStat(kv[1])
			if err != nil {
				return err
			}
			identity[kv[0]] = []any{info.dev, info.ino}
		}
		data["path_identity"] = identity
	}
	if !initial {
		if _, err := r.readMarker(ctx, handle); err != nil {
			return err
		}
	}
	// Each write owns a private temporary file. An abrupt process exit may
	// leave it behind, but cannot block a later lifecycle update or expose a
	// partially written authoritative marker.
	f, err := os.CreateTemp(parentOf(marker), baseName(marker)+".*.tmp")
	if err != nil {
		return err
	}
	temporary := f.Name()
	defer os.Remove(temporary)
	err = writeMarkerData(f, []byte(pyDumps(data, false)))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if initial {
		// Publish the complete record atomically, without replacing an
		// existing attempt's marker (including a symlink).
		if err := os.Link(temporary, marker); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return envError("Environment preparation was already attempted; it cannot be replayed")
			}
			return err
		}
		return nil
	}
	return replaceFile(temporary, marker)
}

func identityEquals(v any, info statInfo) bool {
	items := util.List(v)
	if len(items) != 2 {
		return false
	}
	return numberEquals(items[0], strconv.FormatInt(info.dev, 10)) &&
		numberEquals(items[1], strconv.FormatUint(info.ino, 10))
}

func numberEquals(v any, decimal string) bool {
	switch t := v.(type) {
	case json.Number:
		if t.String() == decimal {
			return true
		}
		f, err := t.Float64()
		want, _ := strconv.ParseFloat(decimal, 64)
		return err == nil && f == want
	case float64:
		want, _ := strconv.ParseFloat(decimal, 64)
		return t == want
	case int64:
		return strconv.FormatInt(t, 10) == decimal
	case uint64:
		return strconv.FormatUint(t, 10) == decimal
	}
	return false
}

func (r *EnvironmentRunner) confirmedPreparation(ctx *plugin.Ctx, handle util.M) error {
	marker, err := r.readMarker(ctx, handle)
	if err != nil {
		return err
	}
	if marker["status"] != "prepared" {
		return envError("Environment preparation was not confirmed or has been released; recovery will not rerun it")
	}
	identities := util.MapOr(marker["path_identity"])
	for _, kv := range handlePaths(handle) {
		info, err := pathStat(kv[1])
		if err != nil {
			return envError("Prepared attempt directory is missing; recovery will not recreate it")
		}
		if !identityEquals(identities[kv[0]], info) {
			return envError("Prepared attempt directory identity changed; refusing to reuse it")
		}
	}
	return nil
}

// ------------------------------------------------------------------ preparation

// mkdirParents is Path.mkdir(parents=True, exist_ok=True, mode=mode): missing
// parents get the default mode, the leaf gets mode (both subject to umask).
func mkdirParents(path string, mode os.FileMode) error {
	err := os.Mkdir(path, mode)
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		parent := parentOf(path)
		if parent == path {
			return err
		}
		if err := mkdirParents(parent, 0o777); err != nil {
			return err
		}
		err = os.Mkdir(path, mode)
		if err == nil {
			return nil
		}
	}
	if isDir(path) {
		return nil
	}
	return err
}

func (r *EnvironmentRunner) ensureRoots(ctx *plugin.Ctx, handle util.M) error {
	for _, kv := range handlePaths(handle) {
		key := kv[0]
		path, err := directory(kv[1], "environment.ctx."+key, false)
		if err != nil {
			return err
		}
		if key != "workspace" && lexists(path) && dirNotEmpty(path) && !callerOwned(ctx, path) {
			return envErrorf("Fresh environment %s is not empty; refusing configuration/cache reuse", key)
		}
		if err := mkdirParents(path, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return err
		}
	}
	for _, kv := range handlePaths(handle) {
		if _, err := treeManifest(kv[1], "environment.ctx."+kv[0], treeOpts{}); err != nil {
			return err
		}
	}
	for _, mount := range util.Maps(handle["mounts"]) {
		source := util.S(mount["source"])
		observed, err := treeManifest(source, "environment.mount.source", treeOpts{fixtures: true})
		if err != nil {
			return err
		}
		if observed["sha256"] != mount["sha256"] {
			return envError("Declared mount source changed after planning; replan a fresh attempt")
		}
		if mount["access"] == "write" {
			dest := util.S(mount["host_source"])
			if lexists(dest) {
				return envError("Writable mount snapshot already exists; refusing attempt reuse")
			}
			snapshot, err := treeManifest(source, "environment.mount.snapshot", treeOpts{fixtures: true, destination: dest})
			if err != nil {
				return err
			}
			if snapshot["sha256"] != mount["sha256"] {
				return envError("Mount source changed during snapshot copy")
			}
		}
	}
	// No host configuration is copied. Create only known empty install/cache directories.
	env, err := r.EnvironmentEnvChecked(ctx)
	if err != nil {
		return err
	}
	home, cache := handlePath(handle, "home_dir"), handlePath(handle, "cache_dir")
	// Python's dict order: a parent created first gets the default mode.
	for _, key := range pathEnvironmentKeys {
		value := env[key]
		if (strings.HasPrefix(value, home+"/") || strings.HasPrefix(value, cache+"/")) && !strings.Contains(value, ":") {
			if err := mkdirParents(value, 0o700); err != nil {
				return err
			}
		}
	}
	if err := mkdirParents(pathJoin(home, ".local", "bin"), 0o700); err != nil {
		return err
	}
	if err := r.restoreWorkerRoots(handle, nil); err != nil {
		return err
	}
	if r.backend == "container" {
		config := util.S(handle["docker_config_dir"])
		if err := os.Mkdir(config, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(pathJoin(config, "config.json"), []byte("{}\n"), 0o666); err != nil {
			return err
		}
	}
	return nil
}

func (r *EnvironmentRunner) restoreWorkerRoots(handle util.M, identities util.M) error {
	if r.backend != "container" || geteuid() != 0 {
		return nil
	}
	parts := strings.SplitN(util.S(handle["worker_user"]), ":", 2)
	uid, _ := strconv.Atoi(parts[0])
	gid := 0
	if len(parts) > 1 {
		gid, _ = strconv.Atoi(parts[1])
	}
	var roots []*dirHandle
	defer func() {
		for _, d := range roots {
			d.close()
		}
	}()
	wrap := func(err error) error {
		if _, ok := asError(err); ok {
			return err
		}
		return envErrorf("Cannot safely restore worker ownership (%s); attempt paths must remain accessible without symlinks",
			osErrorName(err))
	}
	// Open and validate every original root before changing any ownership.
	// Holding descriptors also prevents a later path/symlink swap redirecting chown.
	for _, key := range Roots {
		d, err := openDirectory(handlePath(handle, key))
		if err != nil {
			return wrap(err)
		}
		roots = append(roots, d)
		info, err := fstat(d.file)
		if err != nil {
			return wrap(err)
		}
		if identities != nil && !identityEquals(identities[key], info) {
			return envError("Prepared attempt directory identity changed; refusing ownership repair")
		}
	}
	for _, d := range roots {
		if err := chownOwnedTree(d, uid, gid); err != nil {
			return wrap(err)
		}
	}
	return nil
}

// RestoreWorkerOwnership repairs staged configuration/host setup output.
// Call after staging auth/configuration or a successful host setup command,
// before the next environment command. For a root coordinator and a container
// backend it restores the planned worker UID/GID in the four prepared roots;
// local backends and nonroot coordinators do nothing. It never provisions or
// restarts an environment. Returns (nil, nil) on success.
func (r *EnvironmentRunner) RestoreWorkerOwnership(ctx *plugin.Ctx) (util.M, error) {
	if r.backend != "container" || geteuid() != 0 {
		return nil, nil
	}
	handle, err := r.handle(ctx, false)
	if err != nil {
		return nil, err
	}
	if r.released || r.preparationFailed {
		return nil, envError("Environment is released or preparation failed; refusing ownership repair")
	}
	marker, err := r.readMarker(ctx, handle)
	if err != nil {
		return nil, err
	}
	identities, ok := marker["path_identity"].(util.M)
	if marker["status"] != "prepared" || !ok {
		return nil, envError("Environment preparation was not confirmed; refusing ownership repair")
	}
	state := ctxState(ctx)
	if _, present := state["owned_paths"]; present {
		for _, kv := range handlePaths(handle) {
			if !callerOwned(ctx, kv[1]) {
				return nil, envError("Prepared attempt paths are no longer recorded as owned; refusing ownership repair")
			}
		}
	}
	return nil, r.restoreWorkerRoots(handle, identities)
}

// Prepare establishes (fresh) or attaches to (recovered) the planned
// environment and returns its report. Only the runner instance that made a
// fresh plan can create its container, once; recovered instances never
// create, restart, or replay a missing or expired attempt.
func (r *EnvironmentRunner) Prepare(ctx *plugin.Ctx) (util.M, error) {
	handle, err := r.handle(ctx, false)
	if err != nil {
		return nil, err
	}
	if r.released {
		return nil, envError("Environment was released; create a new attempt")
	}
	if r.preparationFailed {
		return nil, envError("Environment preparation previously failed; create a new attempt")
	}
	if r.prepared != nil {
		if r.backend == "container" {
			if _, err := r.running(ctx, handle); err != nil {
				return nil, err
			}
		}
		return util.MapOr(util.DeepCopy(r.prepared)), nil
	}
	owner := util.Str(handle["owner"])
	fresh := r.freshOwner == owner
	report, err := r.prepareAttempt(ctx, handle, owner, &fresh)
	if err != nil {
		r.preparing = false
		r.preparationFailed = true
		// Only clean our own attempted create. A collision must never remove someone else's resource.
		if r.backend == "container" && r.createdOwner == owner {
			cleanup, _ := r.self().Release(ctx)
			if e, ok := asError(err); ok {
				e.AddNote("Environment preparation cleanup: " + pyDumps(cleanup, false))
			}
		}
		return nil, err
	}
	return report, nil
}

func (r *EnvironmentRunner) prepareAttempt(ctx *plugin.Ctx, handle util.M, owner string, fresh *bool) (util.M, error) {
	expires, _ := util.Num(handle["expires_at"])
	switch {
	case r.backend == "container":
		if timeNow() >= expires {
			cleanup, _ := r.self().Release(ctx)
			return nil, envError("Environment expired; it will not be recreated; cleanup " + util.S(cleanup["status"]))
		}
		existing, err := r.find(handle, "")
		if err != nil {
			return nil, err
		}
		if existing == nil {
			if !*fresh {
				return nil, envError("Owned environment is missing; recovery never creates or reruns an attempt")
			}
			// Consume before any mutations: retrying failed preparation cannot create twice.
			r.freshOwner = ""
			if err := r.writeMarker(ctx, handle, "preparing", true); err != nil {
				return nil, err
			}
			if err := r.ensureRoots(ctx, handle); err != nil {
				return nil, err
			}
			r.createdOwner = owner
			argv, err := r.createArgv(handle)
			if err != nil {
				return nil, err
			}
			created, err := r.docker(argv, handle, math.Min(30, math.Max(1, expires-timeNow())))
			if err != nil {
				return nil, err
			}
			if created.Code != 0 {
				// Keep the engine's own words: "creation failed" alone hid a VM bind-mount limit.
				r.creationFailure = dockerFailure(created)
				return nil, envError(dockerError("Docker creation failed", r.creationFailure,
					"no fallback or task retry was attempted"))
			}
			if existing, err = r.find(handle, ""); err != nil {
				return nil, err
			}
			if existing == nil {
				return nil, envError("Container disappeared during preparation (expiry or startup failure)")
			}
		} else {
			*fresh = false
			r.freshOwner = ""
			if err := r.confirmedPreparation(ctx, handle); err != nil {
				return nil, err
			}
		}
		if err := r.checkContainer(existing, handle); err != nil {
			return nil, err
		}
	case *fresh:
		r.freshOwner = ""
		if err := r.writeMarker(ctx, handle, "preparing", true); err != nil {
			return nil, err
		}
		if err := r.ensureRoots(ctx, handle); err != nil {
			return nil, err
		}
	default:
		for _, kv := range handlePaths(handle) {
			if !isDir(kv[1]) {
				return nil, envError("Local attempt directories are missing; recovery will not recreate them")
			}
		}
		if err := r.confirmedPreparation(ctx, handle); err != nil {
			return nil, err
		}
	}
	r.preparing = true
	report, err := r.reportFn(ctx, handle, *fresh)
	if err != nil {
		return nil, err
	}
	if *fresh {
		if err := r.writeMarker(ctx, handle, "prepared", false); err != nil {
			return nil, err
		}
	}
	r.prepared = util.MapOr(util.DeepCopy(report))
	r.preparing = false
	return report, nil
}

func (r *EnvironmentRunner) running(ctx *plugin.Ctx, handle util.M) (util.M, error) {
	if r.released {
		return nil, envError("Environment was released; the attempt cannot run again")
	}
	expires, _ := util.Num(handle["expires_at"])
	if timeNow() >= expires {
		cleanup, _ := r.self().Release(ctx)
		return nil, envError("Environment lifetime expired; cleanup " + util.S(cleanup["status"]))
	}
	data, err := r.find(handle, "")
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, envError("Environment is missing or expired; it will not be recreated")
	}
	if err := r.checkContainer(data, handle); err != nil {
		return nil, err
	}
	if !r.preparing {
		if err := r.confirmedPreparation(ctx, handle); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// ------------------------------------------------------------------ execution

// Wrap returns the argv that runs argv in the prepared environment. For
// wrapped launches pass EnvironmentEnv(ctx) plus declared/auth overrides to
// both Wrap and ChildEnv; the latter is the actual subprocess environment.
// Docker reads values from the CLI's explicit process env; values never enter argv.
func (r *EnvironmentRunner) Wrap(argv []string, ctx *plugin.Ctx, env map[string]string) ([]string, error) {
	var value any = argv
	if argv == nil {
		value = nil
	}
	args, err := arguments(value, "environment.argv")
	if err != nil {
		return nil, err
	}
	handle, err := r.handle(ctx, false)
	if err != nil {
		return nil, err
	}
	if r.released {
		return nil, envError("Environment was released")
	}
	fullEnv, err := r.WorkerEnv(ctx, env)
	if err != nil {
		return nil, err
	}
	if r.backend == "local" {
		if !r.preparing {
			if err := r.confirmedPreparation(ctx, handle); err != nil {
				return nil, err
			}
		}
		return args, nil
	}
	data, err := r.running(ctx, handle)
	if err != nil {
		return nil, err
	}
	prefix, err := r.dockerPrefix(handle)
	if err != nil {
		return nil, err
	}
	out := append(prefix, "exec", "--interactive", "--user", util.S(handle["worker_user"]),
		"--workdir", handlePath(handle, "workspace"))
	names := util.SortedKeys(fullEnv)
	assignments := make([]string, len(names))
	for i, key := range names {
		out = append(out, "--env", key)
		// env -i prevents image ENV/defaults from leaking. Expansion happens inside the
		// container, with quoted names generated only from validated identifiers.
		assignments[i] = `"` + key + "=${" + key + `}"`
	}
	script := "umask 077; exec /usr/bin/env -i " + strings.Join(assignments, " ") + ` "$@"`
	out = append(out, util.S(data["Id"]), "/bin/sh", "-c", script, "ajx-env")
	return append(out, args...), nil
}

// ShellChecked runs a shell command inside the environment with bounded
// output (Python shell). A timeout stops container workers (abort) and keeps
// the environment for teardown. timeout is in seconds; 0 means 600.
func (r *EnvironmentRunner) ShellChecked(command string, ctx *plugin.Ctx, timeout float64, env map[string]string) (util.M, error) {
	if strings.Contains(command, "\x00") {
		return nil, fail("environment.shell", "expected a shell command string without NUL bytes")
	}
	if timeout == 0 {
		timeout = 600
	}
	fullEnv, err := r.WorkerEnv(ctx, env)
	if err != nil {
		return nil, err
	}
	argv, err := r.self().Wrap([]string{"/bin/sh", "-c", command}, ctx, fullEnv)
	var result util.M
	if err == nil {
		var cwd string
		if ctx != nil {
			cwd = ctx.Workspace
		}
		result, err = tailProcess(ctx.Context(), argv, cwd, timeout, fullEnv)
	}
	if err != nil {
		if r.backend == "container" {
			r.self().Abort(ctx)
		}
		return result, err
	}
	result["cmd"] = command
	if util.Truthy(result["timed_out"]) {
		result["environment_abort"] = r.self().Abort(ctx)
	}
	return result, nil
}

// Shell is ShellChecked for the plugin contract. An error that Python would
// raise is returned as a failed record (exit_code nil, "error" set); use
// ShellChecked (or plugin.ContextShellChecked) to surface it.
func (r *EnvironmentRunner) Shell(command string, ctx *plugin.Ctx, timeout int, env map[string]string) util.M {
	result, err := r.ShellChecked(command, ctx, float64(timeout), env)
	if err == nil {
		return result
	}
	now := util.Now()
	if result == nil {
		result = util.M{"exit_code": nil, "timed_out": false, "stdout": "", "started_at": now, "stopped_at": now}
	}
	result["cmd"] = command
	result["stderr"] = ErrorType(err) + ": " + err.Error()
	result["error"] = ErrorType(err) + ": " + err.Error()
	if util.Interrupted(err) {
		result["interrupted"] = true
	}
	return result
}

func localPlatform() util.M {
	system, release, machine := unameFacts()
	facts := util.M{"system": system, "release": release, "machine": machine,
		"effective_uid": int64(geteuid()), "effective_gid": int64(os.Getegid())}
	if system == "Linux" {
		facts["os_release"] = osRelease()
	}
	return facts
}

// osRelease is platform.freedesktop_os_release() filtered to ID/VERSION_ID/PRETTY_NAME.
func osRelease() any {
	var data []byte
	var err error
	for _, p := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		if data, err = os.ReadFile(p); err == nil {
			break
		}
	}
	if err != nil {
		return nil
	}
	out := util.M{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(key, "#") {
			continue
		}
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
			value = strings.NewReplacer(`\"`, `"`, `\'`, `'`, `\\`, `\`, "\\$", "$", "\\`", "`").Replace(value)
		}
		if key == "ID" || key == "VERSION_ID" || key == "PRETTY_NAME" {
			out[key] = value
		}
	}
	return out
}

func (r *EnvironmentRunner) limitations() []any {
	common := []any{"Inventory covers declared probes and bootstrap/runtime identity, not every installed package.",
		"Host evidence directories have no enforced disk quota; archive after worker shutdown.",
		"Explicitly supplied authentication values and command output require the caller's redaction.",
		"System/runtime configuration in the selected OS or image may still affect installed tools."}
	if r.backend == "local" {
		return append([]any{"Local execution is observational: no filesystem, network, sudo, or root sandbox.",
			"Fresh HOME/config/cache paths do not hide other host paths or global configuration.",
			"Local process groups cannot contain a process that creates a new session; " +
				"no independent lifetime survives coordinator termination."}, common...)
	}
	return append([]any{"Container isolation depends on the local Docker daemon, kernel, and trusted image runtime.",
		"Only user-level installation into writable attempt directories is supported.",
		"The root-owned supervisor and automatic removal bound lifetime without the coordinator.",
		"Bridge networking, when selected, has no endpoint, host/LAN, or metadata allowlist.",
		"Runtime /proc, /dev and Docker-managed DNS/hosts files remain present.",
		"Read-only fixture sources are trusted inputs; host changes during a run are not prevented."}, common...)
}

func (r *EnvironmentRunner) capabilities(confirmed bool) util.M {
	container := r.backend == "container"
	state := "unsupported"
	if container {
		state = "unknown"
		if confirmed {
			state = "enforced"
		}
	}
	mech := func(text string) any {
		if container {
			return text
		}
		return nil
	}
	network := util.S(r.profile["network"])
	networkState, networkMech := "unsupported", any(nil)
	if network == "none" {
		networkState, networkMech = state, "Docker none network; loopback only"
	}
	packageState := "observed-only"
	if container {
		packageState = state
	}
	configState := "unknown"
	if confirmed {
		configState = "enforced"
	}
	perm := util.MapOr(r.profile["permissions"])
	limits := util.MapOr(util.DeepCopy(r.limits()))
	var lifetime any
	if v, ok := r.limits()["lifetime_seconds"]; ok {
		lifetime = v
	}
	return util.M{
		"filesystem_isolation": util.M{"declared": perm["filesystem"], "state": state,
			"mechanism": mech("read-only rootfs and explicit nonrecursive bind mounts")},
		"network_isolation": util.M{"declared": network, "state": networkState, "mechanism": networkMech},
		"non_root": util.M{"declared": perm["root"], "state": state,
			"mechanism": mech("numeric non-root exec user; all capabilities dropped; no-new-privileges")},
		"resource_limits": util.M{"declared": limits, "state": state,
			"mechanism": mech("Docker memory/CPU/pids limits, bounded tmpfs")},
		"lifetime": util.M{"declared": lifetime, "state": state,
			"mechanism": mech("root-owned sleep under init; auto-remove; no restart")},
		"package_install": util.M{"declared": perm["package_install"], "state": packageState,
			"mechanism": mech("user directories writable; system rootfs read-only")},
		"configuration_paths": util.M{"declared": "fresh HOME/XDG/config/cache; no inherited process environment",
			"state": configState, "mechanism": "explicit directories/env; does not disable global OS configuration"},
	}
}

const bootstrapProbe = "printf 'system='; uname -s; printf 'release='; uname -r; printf 'machine='; uname -m; " +
	"printf 'uid='; id -u; printf 'gid='; id -g; " +
	"while read -r key value rest; do case \"$key\" in CapEff:|NoNewPrivs:|Seccomp:) " +
	"printf '%s=%s\\n' \"$key\" \"$value\";; esac; done < /proc/self/status; " +
	"while read -r key value rest; do case \"$key\" in Uid:) " +
	"printf 'supervisor_uid=%s\\n' \"$value\";; esac; done < /proc/1/status; " +
	"while IFS=: read -r iface rest; do case \"$iface\" in *'|'*) continue;; esac; " +
	"set -- $iface; [ \"$#\" = 1 ] && printf 'interface=%s\\n' \"$1\"; done < /proc/net/dev; " +
	"if [ -r /etc/os-release ]; then while IFS='=' read -r key value; do " +
	"case \"$key\" in ID|VERSION_ID|PRETTY_NAME) printf 'os_%s=%s\\n' \"$key\" \"$value\";; esac; " +
	"done < /etc/os-release; fi"

func (r *EnvironmentRunner) report(ctx *plugin.Ctx, handle util.M, fresh bool) (util.M, error) {
	workerEnv, err := r.WorkerEnv(ctx, nil)
	if err != nil {
		return nil, err
	}
	status, phase := "attached", "recovered"
	if fresh {
		status, phase = "prepared", "initial"
	}
	capabilities := r.capabilities(true)
	report := util.M{"id": r.id(), "backend": r.backend, "status": status,
		"image": util.DeepCopy(handle["image"]), "mounts": util.DeepCopy(util.List(handle["mounts"])),
		"env_names": sortedEnvNames(workerEnv), "limitations": r.limitations(),
		"capabilities": capabilities,
		"inventory": util.M{"phase": phase, "tools": []any{},
			"scope": "declared probes only; not a complete software inventory"}}
	if report["mounts"] == nil {
		report["mounts"] = []any{}
	}
	if r.backend == "local" {
		report["platform"] = localPlatform()
	} else {
		// This fixed, unprivileged probe contains no task/verifier instructions or credentials.
		probe, err := r.ShellChecked(bootstrapProbe, ctx, 10, nil)
		if err != nil {
			return nil, err
		}
		if util.Truthy(probe["timed_out"]) || !isZero(probe["exit_code"]) {
			return nil, envError("Container bootstrap probe failed; image requires sh/env/sleep/uname/id")
		}
		fields := map[string]string{}
		var fieldOrder []string
		interfaces := []any{}
		for _, line := range pySplitLines(util.S(probe["stdout"])) {
			key, value, sep := strings.Cut(line, "=")
			if sep && key == "interface" {
				interfaces = append(interfaces, value)
			} else if sep {
				if _, seen := fields[key]; !seen {
					fieldOrder = append(fieldOrder, key)
				}
				fields[key] = value
			}
		}
		expectedUID := strings.SplitN(util.S(handle["worker_user"]), ":", 2)[0]
		capEff, capErr := strconv.ParseInt(orDefault(fields, "CapEff:", "-1"), 16, 64)
		if fields["uid"] != expectedUID || fields["NoNewPrivs:"] != "1" || fields["Seccomp:"] != "2" ||
			fields["supervisor_uid"] != "0" || capErr != nil || capEff != 0 {
			return nil, envError("Observed worker privileges do not match the non-root contract")
		}
		if r.profile["network"] == "none" {
			ifaces := map[string]bool{}
			for _, i := range interfaces {
				ifaces[util.S(i)] = true
			}
			if len(ifaces) != 1 || !ifaces["lo"] {
				return nil, envError("Observed interfaces do not match network='none'")
			}
		}
		platform := util.M{}
		for _, k := range []string{"system", "release", "machine", "uid", "gid"} {
			if v, ok := fields[k]; ok {
				platform[k] = v
			} else {
				platform[k] = nil
			}
		}
		osRel := util.M{}
		for _, key := range fieldOrder {
			if strings.HasPrefix(key, "os_") {
				osRel[key[3:]] = fields[key]
			}
		}
		platform["os_release"] = osRel
		report["platform"] = platform
		util.MapOr(capabilities["non_root"])["observations"] = util.M{
			"uid": fields["uid"], "effective_capabilities": fields["CapEff:"],
			"no_new_privileges": true, "seccomp_filter": true}
		util.MapOr(capabilities["lifetime"])["observations"] = util.M{"supervisor_uid": fields["supervisor_uid"]}
		util.MapOr(capabilities["network_isolation"])["observations"] = util.M{"interfaces": interfaces}
		// Probe writes using throwaway sentinels, including read mounts. Never modify real inputs.
		marker := ".ajx-probe-" + util.Str(handle["owner"])
		var commands []string
		for _, kv := range handlePaths(handle) {
			sentinel := shQuote(pathJoin(kv[1], marker))
			commands = append(commands, "(umask 077; : > "+sentinel+") && rm -f -- "+sentinel)
		}
		denied := []string{"/" + marker, pathJoin(ctxRunDir(ctx), marker)}
		for _, m := range util.Maps(handle["mounts"]) {
			if m["access"] == "read" {
				denied = append(denied, pathJoin(util.S(m["target"]), marker))
			}
		}
		for _, p := range denied {
			quoted := shQuote(p)
			commands = append(commands, "if (: > "+quoted+") 2>/dev/null; then rm -f -- "+quoted+"; exit 91; fi")
		}
		writes, err := r.ShellChecked("set -e; "+strings.Join(commands, "; "), ctx, 10, nil)
		if err != nil {
			return nil, err
		}
		if util.Truthy(writes["timed_out"]) || !isZero(writes["exit_code"]) {
			return nil, envError("Synthetic permitted/denied directory write probe failed")
		}
		util.MapOr(capabilities["filesystem_isolation"])["observations"] = util.M{
			"private_roots_writable": true, "root_and_report_writes_denied": true,
			"read_mount_writes_denied": true, "scope": "synthetic sentinels, not an exhaustive escape proof"}
	}
	inventory := util.MapOr(report["inventory"])
	tools := []any{}
	if fresh {
		for _, tool := range util.Maps(r.profile["tools"]) {
			argv := util.Strings(tool["argv"])
			result, err := r.ShellChecked(shJoin(argv), ctx, 15, nil)
			if err != nil {
				return nil, err
			}
			observed := util.M{"name": tool["name"], "required": tool["required"], "argv": toAnyStrings(argv)}
			for _, k := range []string{"exit_code", "timed_out", "stdout", "stderr"} {
				observed[k] = result[k]
			}
			env, err := r.WorkerEnv(ctx, nil)
			if err != nil {
				return nil, err
			}
			for _, stream := range []string{"stdout", "stderr"} {
				observed[stream] = redactEnvOutput(util.S(observed[stream]), env)
			}
			redacted := make([]any, len(argv))
			for i, a := range argv {
				redacted[i] = redactEnvOutput(a, env)
			}
			observed["argv"] = redacted
			observed["status"] = "unavailable"
			if isZero(result["exit_code"]) && !util.Truthy(result["timed_out"]) {
				observed["status"] = "observed"
			}
			tools = append(tools, observed)
			inventory["tools"] = tools
			if util.Truthy(tool["required"]) && observed["status"] != "observed" {
				return nil, envError("environments." + r.id() + ".tools." + util.S(tool["name"]) +
					": required starting-inventory probe failed")
			}
		}
	} else {
		for _, t := range util.Maps(r.profile["tools"]) {
			tools = append(tools, util.M{"name": t["name"], "status": "unknown",
				"reason": "recovery does not replay starting inventory commands"})
		}
	}
	inventory["tools"] = tools
	return report, nil
}

func orDefault(m map[string]string, key, def string) string {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

func isZero(v any) bool {
	if v == nil {
		return false
	}
	if _, isBool := v.(bool); isBool {
		return v == false
	}
	f, ok := util.Num(v)
	return ok && f == 0
}

// Abort stops workers while retaining cleanup access; it removes the
// container when stopping is unconfirmed. Call it immediately after a wrapped
// harness timeout: killing the Docker client alone does not kill docker-exec
// workers. Local abort relies on the caller to reap process groups.
func (r *EnvironmentRunner) Abort(ctx *plugin.Ctx) util.M {
	if r.backend == "local" {
		return util.M{"status": "caller_managed", "confirmed": false, "backend": "local",
			"worker_stop": "caller-managed",
			"limitations": []any{"The caller must reap local process groups; abort keeps the prepared " +
				"environment available for cleanup until final release."}}
	}
	data, handle, err := r.stopWorkers(ctx)
	if err != nil {
		cleanup, rerr := r.self().Release(ctx)
		if cleanup == nil {
			cleanup = util.M{"status": "failed", "confirmed": false, "backend": r.backend, "error": pyStrErr(rerr)}
		}
		out := util.Copy(cleanup)
		out["environment_retained"] = false
		out["cancellation_error"] = pyStrErr(err)
		return out
	}
	return util.M{"status": "workers_stopped", "confirmed": true, "backend": "container",
		"name": handle["name"], "container_id": data["Id"], "environment_retained": true}
}

func (r *EnvironmentRunner) stopWorkers(ctx *plugin.Ctx) (util.M, util.M, error) {
	handle, err := r.handle(ctx, true)
	if err != nil {
		return nil, nil, err
	}
	data, err := r.find(handle, "")
	if err != nil {
		return nil, nil, err
	}
	expires, _ := util.Num(handle["expires_at"])
	if data == nil || r.released || timeNow() >= expires {
		return nil, nil, envError("Environment is absent, released, or expired")
	}
	// Verify the private PID namespace and non-root identity before any signal.
	if err := r.checkContainer(data, handle); err != nil {
		return nil, nil, err
	}
	user := util.S(handle["worker_user"])
	stopped, err := r.docker([]string{"exec", "--user", user, "--workdir", "/", util.S(data["Id"]),
		"/usr/bin/env", "-i", "PATH=" + containerPath, "HOME=/nonexistent",
		"/bin/sh", "-c", StopWorkers, "ajx-stop-workers", strings.SplitN(user, ":", 2)[0]}, handle, 5)
	if err != nil {
		return nil, nil, err
	}
	if stopped.Code != 0 {
		return nil, nil, envError("Worker termination could not be confirmed")
	}
	after, err := r.find(handle, "")
	if err != nil {
		return nil, nil, err
	}
	if after == nil {
		after = util.M{}
	}
	if err := r.checkContainer(after, handle); err != nil {
		return nil, nil, err
	}
	return data, handle, nil
}

// Release removes only an ownership-verified container and retains bounded
// host evidence for the caller's archive. It is idempotent. The record is
// {status, confirmed, ...}; an unreachable daemon is not success. The error is
// non-nil only for interruption (Python lets KeyboardInterrupt propagate).
func (r *EnvironmentRunner) Release(ctx *plugin.Ctx) (util.M, error) {
	record, err := r.release(ctx)
	if err != nil {
		if !cleanupCatches(err) {
			return nil, err
		}
		record = util.M{"status": "failed", "confirmed": false, "backend": r.backend, "error": pyStrErr(err)}
	}
	if r.creationFailure != nil {
		// The one absence whose cause is known: persist the engine's exit code/stderr with the cleanup record.
		record["creation_failure"] = util.DeepCopy(r.creationFailure)
	}
	return record, nil
}

func (r *EnvironmentRunner) release(ctx *plugin.Ctx) (util.M, error) {
	handle, err := r.handle(ctx, true)
	if err != nil {
		return nil, err
	}
	r.freshOwner = ""
	if r.backend == "local" {
		if _, err := os.Stat(r.markerPath(ctx, handle)); err == nil {
			if err := r.writeMarker(ctx, handle, "released", false); err != nil {
				return nil, err
			}
		}
		r.released = true
		return util.M{"status": "released", "confirmed": true, "backend": "local", "owned_resources": []any{},
			"worker_stop": "caller-managed",
			"limitations": []any{"No local sandbox or independent process-lifetime guarantee; " +
				"caller must reap harness process groups before archiving."}}, nil
	}
	data, err := r.find(handle, "")
	if err != nil {
		return nil, err
	}
	if data == nil {
		r.released = true
		reason := "not present; expiry/startup failure cannot be distinguished"
		if r.creationFailure != nil {
			reason = "not present; container creation failed (see creation_failure)"
		}
		return util.M{"status": "already_absent", "confirmed": true, "backend": "container",
			"name": handle["name"], "reason": reason}, nil
	}
	identity := util.S(data["Id"])
	result, err := r.docker([]string{"container", "rm", "--force", identity}, handle, 15)
	if err != nil {
		return nil, err
	}
	// A concurrent --rm expiry can make rm fail; verify absence instead of trusting its exit.
	remaining, err := r.find(handle, identity)
	if err != nil {
		return nil, err
	}
	if remaining != nil {
		return util.M{"status": "failed", "confirmed": false, "backend": "container", "name": handle["name"],
			"error": "owned container remains after removal", "docker_exit_code": int64(result.Code)}, nil
	}
	r.released = true
	return util.M{"status": "released", "confirmed": true, "backend": "container",
		"name": handle["name"], "container_id": identity, "host_evidence": "preserved"}, nil
}

// Doctor checks prerequisites without attempt roots, pulls, or inventory
// commands. Container profiles inspect the pinned image and daemon read-only,
// then run one short-lived unprivileged probe container that bind-mounts an
// empty private directory created under ctx.Spec trial.workspace_root
// read-only at /ajx-probe; the container and the directory are removed. The
// probe is skipped for the local backend, without a workspace_root, or after
// a failed image/daemon check; a failed probe makes the profile unavailable,
// with the engine's stderr in report["error"]. ctx may be nil.
func (r *EnvironmentRunner) Doctor(ctx *plugin.Ctx) util.M {
	if r.initErr != nil {
		return util.M{"id": nil, "backend": r.backend, "status": "unavailable", "error": pyStrErr(r.initErr),
			"mount_probe": util.M{"status": "skipped", "reason": "image/daemon checks failed"}}
	}
	report := util.M{"id": r.id(), "backend": r.backend, "status": "available",
		"capabilities": r.capabilities(false), "limitations": r.limitations()}
	if r.backend == "local" {
		report["platform"] = localPlatform()
		report["mount_probe"] = util.M{"status": "skipped", "reason": "the local backend has no container bind mounts"}
		return report
	}
	image, err := r.inspectImage(nil)
	var daemon util.M
	if err == nil {
		daemon, err = r.daemon(nil)
	}
	if image != nil {
		report["image"] = image
	}
	if daemon != nil {
		report["daemon"] = daemon
	}
	if err != nil {
		report["status"] = "unavailable"
		report["error"] = pyStrErr(err)
		report["mount_probe"] = util.M{"status": "skipped", "reason": "image/daemon checks failed"}
		return report
	}
	probe := r.mountProbe(ctx, image)
	report["mount_probe"] = probe
	if probe["status"] == "failed" {
		report["status"] = "unavailable"
		report["error"] = probe["error"]
	}
	return report
}

func (r *EnvironmentRunner) mountProbe(ctx *plugin.Ctx, image util.M) util.M {
	var root string
	if ctx != nil {
		root, _ = util.Get(ctx.Spec, "trial", "workspace_root").(string)
	}
	if root == "" {
		return util.M{"status": "skipped", "reason": "no trial.workspace_root in the doctor context"}
	}
	if !isDir(root) {
		return util.M{"status": "skipped", "workspace_root": root,
			"reason": "trial.workspace_root " + root + " does not exist yet (ajx run creates it); " +
				"create it to probe bind mounts"}
	}
	probe := util.M{"status": "failed", "workspace_root": root, "stderr_tail": "", "hint": nil}
	var source string
	defer func() {
		if source != "" {
			os.RemoveAll(source)
		}
	}()
	err := func() error {
		parent, err := directory(root, "trial.workspace_root", true)
		if err != nil {
			return err
		}
		// A private 0o700 directory with an unpredictable name, directly under the root.
		if source, err = os.MkdirTemp(parent, "ajx-doctor-probe-"); err != nil {
			return err
		}
		if err := os.Chmod(source, 0o700); err != nil {
			return err
		}
		if _, err := pathText(source, "trial.workspace_root"); err != nil {
			return err
		}
		args := []string{"run", "--rm", "--pull", "never", "--network", "none", "--read-only", "--cap-drop", "ALL",
			"--security-opt", "no-new-privileges=true", "--user", workerUser(),
			"--ipc", "none", "--cgroupns", "private", "--no-healthcheck", "--log-driver", "none",
			"--entrypoint", "/usr/bin/env"}
		if p := util.S(r.profile["platform"]); p != "" {
			args = append(args, "--platform", p)
		}
		// As in createArgv: clear image ENV before the entrypoint, then env -i for the probe shell.
		for _, key := range clearedEnv(image) {
			args = append(args, "--env", key+"=")
		}
		args = append(args, "--mount", "type=bind,source="+source+",target="+probeTarget+","+
			"bind-propagation=rprivate,bind-recursive=disabled,readonly",
			util.S(image["id"]), "-i", "PATH="+containerPath, "/bin/sh", "-c", "test -d "+probeTarget)
		result, err := r.docker(args, nil, 20)
		if err != nil {
			return err
		}
		if result.Code != 0 {
			record := dockerFailure(result)
			probe["exit_code"] = record["exit_code"]
			probe["stderr_tail"] = record["stderr_tail"]
			probe["hint"] = record["hint"]
			probe["error"] = dockerError("Workspace bind-mount probe failed for trial.workspace_root "+root, record, "")
		} else {
			probe["status"] = "ok"
		}
		return nil
	}()
	if err != nil {
		probe["error"] = "Workspace bind-mount probe could not run for trial.workspace_root " + root + ": " + pyStrErr(err)
	}
	return probe
}

// clearedEnv names every image ENV key plus the startup controls cleared at container start.
func clearedEnv(image util.M) []string {
	return util.Uniq(append(util.Strings(image["environment_names"]), "HOME", "PATH", "ENV", "BASH_ENV"))
}

// ------------------------------------------------------------------ helpers

// shQuote is Python shlex.quote.
func shQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("@%+=:,./-_", c)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// shJoin is Python shlex.join.
func shJoin(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = shQuote(a)
	}
	return strings.Join(out, " ")
}

// pySplitLines is Python str.splitlines().
func pySplitLines(s string) []string {
	var out []string
	var cur strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch c {
		case '\r':
			out = append(out, cur.String())
			cur.Reset()
			if i+1 < len(runes) && runes[i+1] == '\n' {
				i++
			}
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', '\u2028', '\u2029':
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
