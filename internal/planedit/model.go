// Package planedit ports lib/ajx/plan_edit.py: the console's structured editor
// for trial.toml.
//
// A plain model of what a test runner configures (the task, success checks,
// run settings, the reporter, the task account and cleanup, execution
// environments and workers) is read from the trial file and written back
// section by section. Only what changed is rewritten: untouched keys and
// sections keep their text and comments. Validation stays with spec.Load:
// the console loads the result before saving it, and refuses anything AJX
// itself would refuse.
//
// The model is JSON-shaped with ordered objects (Dict), so tables are written
// back in the order the Python editor writes them.
package planedit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/royosherove/ajx/internal/envcheck"
	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/render/pyfmt"
	"github.com/royosherove/ajx/internal/util"
)

// FolderEnv is the implicit "fresh folder on your machine"; '@' can never be a profile name.
const FolderEnv = "@local"

var (
	checkTypes = map[string]string{"file_exists": "file", "shell": "cmd", "http": "http"}
	checkTOML  = map[string]string{"file": "file_exists", "cmd": "shell", "http": "http"}
	// KnownModels are offered in the model fields; any other name the agent accepts still works.
	KnownModels = map[string][]string{"claude-code": {"sonnet", "opus", "haiku"}, "kiro-cli": {"claude-sonnet-4.5"}, "codex": {"gpt-5"}}
	effort      = map[string]bool{"claude-code": true, "codex": true, "kiro-cli": true} // agents that accept an effort setting
	labels      = map[string]string{"claude-code": "Claude Code", "codex": "Codex", "kiro-cli": "Kiro CLI",
		"gemini-cli": "Gemini CLI", "cursor-agent": "Cursor Agent", "copilot-cli": "Copilot CLI"}
	cellKeys    = setOf("id", "harness", "model", "models", "effort", "config", "auth", "environment", "agent_configuration")
	checkFields = map[string][]string{"file": {"path", "contains"}, "cmd": {"run", "expect_exit", "expect_stdout"},
		"http": {"url", "expect_status", "expect_body"}}
	envKeys   = setOf("backend", "image", "network", "required", "limits", "tools", "docker_host")
	modelKeys = []string{"task", "checks", "run", "reporter", "account", "envs", "workers"}
	slug      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,47}$`) // spec.load's short-lowercase-slug rule
)

// ContainerLimits are the container limits a profile gets when it names none.
func ContainerLimits() *Dict {
	return NewDict("cpus", int64(1), "memory_mb", int64(1024), "pids", int64(256), "tmpfs_mb", int64(64),
		"lifetime_seconds", int64(3600))
}

// authTemplate is a sign-in route the page can create: credentials are
// ${VAR} references to the terminal, never values.
type authTemplate struct {
	name, label, detail string
	table               func() *Dict
}

var authTemplates = map[string][]authTemplate{
	"claude-code": {{"bedrock-keys", "Bedrock keys from your terminal",
		"Adds [auth.bedrock-keys] to trial.toml. It reads AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_REGION " +
			"(and AWS_SESSION_TOKEN if set) from the terminal that started the console; no secret is written.",
		func() *Dict {
			return NewDict("type", "claude-bedrock", "env", NewDict("AWS_REGION", "${AWS_REGION}",
				"AWS_ACCESS_KEY_ID", "${AWS_ACCESS_KEY_ID}", "AWS_SECRET_ACCESS_KEY", "${AWS_SECRET_ACCESS_KEY}"))
		}}},
	"kiro-cli": {{"kiro-key", "Kiro API key from your terminal",
		"Adds [auth.kiro-key] to trial.toml. It reads KIRO_API_KEY from the terminal that started the console.",
		func() *Dict {
			return NewDict("type", "env", "isolates_config", true, "required_env", []any{"KIRO_API_KEY"})
		}}},
}

func template(harness any, name any) *authTemplate {
	h, ok := hashKey(harness)
	if !ok {
		return nil
	}
	n, ok := hashKey(name)
	if !ok {
		return nil
	}
	for i, t := range authTemplates[h] {
		if t.name == n {
			return &authTemplates[h][i]
		}
	}
	return nil
}

func setOf(items ...string) map[string]bool {
	out := map[string]bool{}
	for _, s := range items {
		out[s] = true
	}
	return out
}

// readText is Path.read_text(encoding="utf-8"): strict UTF-8 with universal newlines.
func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", &pyError{"UnicodeDecodeError", "'utf-8' codec can't decode the file " + path}
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n"), nil
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// jsonSafe refuses TOML dates and times, which the page cannot edit.
func jsonSafe(v any, where string) {
	switch t := v.(type) {
	case time.Time:
		fail("%s holds a TOML date or time, which the console cannot edit; edit trial.toml directly", where)
	case *Dict:
		for _, k := range t.keys {
			jsonSafe(t.vals[k], where+"."+k)
		}
	case []any:
		for _, x := range t {
			jsonSafe(x, where)
		}
	}
}

func commands(list any) []any {
	out := []any{}
	for _, c := range orList(list) {
		run, name := pyStr(or(get(c, "run"), "")), or(get(c, "name"), "")
		extra := NewDict()
		for _, k := range items(c).keys {
			if k != "run" && k != "name" {
				extra.Set(k, c.(*Dict).vals[k])
			}
		}
		out = append(out, NewDict("run", run, "name", name, "extra", extra))
	}
	return out
}

// shlexQuote is shlex.quote.
func shlexQuote(s string) string {
	if s == "" {
		return "''"
	}
	if regexp.MustCompile(`^[\w@%+=:,./-]+$`).MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func identity(cmd any) string {
	if l, ok := cmd.([]any); ok {
		parts := make([]string, len(l))
		for i, a := range l {
			parts[i] = shlexQuote(pyStr(a))
		}
		return strings.Join(parts, " ")
	}
	return pyStr(or(cmd, ""))
}

func canReport(cli any, adapters any) bool {
	if !truthy(cli) || has(adapters, cli) {
		return false
	}
	name, ok := cli.(string)
	if !ok {
		return false
	}
	h, err := plugin.NewHarness(name)
	return err == nil && h.Base().CanReport
}

// AutoReporter is the agent spec.Load picks when [reporter].harness is not
// set: the first worker's that can report.
func AutoReporter(workers []any, adapters any) any {
	for _, w := range workers {
		if canReport(item(w, "cli"), adapters) {
			return item(w, "cli")
		}
	}
	return "claude-code"
}

// ReadModel is the editable model of a trial file: raw values as written, not
// the defaults spec.Load adds.
func ReadModel(trial string) (*Dict, error) {
	text, err := readText(trial)
	if err != nil {
		return nil, err
	}
	return ReadModelText(trial, text)
}

// ReadModelText is ReadModel for the trial file's text.
func ReadModelText(trial, text string) (model *Dict, err error) {
	raw, err := ParseTOML(text)
	if err != nil {
		return nil, err
	}
	err = catch(func() { model = readModel(trial, raw) })
	return model, err
}

func readModel(trial string, raw *Dict) *Dict {
	jsonSafe(raw, "trial.toml")
	t, task, rep := orDict(raw.Get("trial")), orDict(raw.Get("task")), orDict(raw.Get("reporter"))
	promptFile := or(get(task, "prompt_file"), "")
	prompt := ""
	if truthy(promptFile) {
		name, ok := promptFile.(string)
		if !ok {
			raise("TypeError", "unsupported operand type(s) for /: 'PosixPath' and '%s'", typeName(promptFile))
		}
		path := name
		if !filepath.IsAbs(name) {
			path = filepath.Join(filepath.Dir(trial), name)
		}
		if isFile(path) {
			text, err := readText(path)
			if pe, ok := err.(*pyError); ok {
				panic(pe)
			} else if err != nil {
				raise("OSError", "%s", err.Error())
			}
			prompt = text
		}
	}
	profiles := orDict(raw.Get("environments"))
	configs := orDict(raw.Get("agent_configurations"))
	cells := orList(raw.Get("cells"))
	defaultEnv, defaultCfg := get(t, "environment"), get(t, "agent_configuration")
	envs := []any{}
	needFolder := !truthy(profiles)
	for _, c := range cells {
		if !truthy(or(get(c, "environment"), defaultEnv)) {
			needFolder = true
		}
	}
	if needFolder {
		envs = append(envs, NewDict("id", FolderEnv, "kind", "folder", "label", "your machine"))
	}
	for _, name := range items(profiles).Keys() {
		prof := profiles.(*Dict).Get(name)
		container := Equal(get(prof, "backend"), "container")
		kind, network := "profile", "inherit"
		limits := NewDict()
		if container {
			kind, network = "container", "none"
			limits = ContainerLimits()
		}
		limits.Update(dictOf(orDict(get(prof, "limits"))))
		tools := []any{}
		for _, x := range orList(get(prof, "tools")) {
			tools = append(tools, dictOf(x))
		}
		extra := NewDict()
		for _, k := range items(prof).keys {
			if !envKeys[k] {
				extra.Set(k, prof.(*Dict).vals[k])
			}
		}
		envs = append(envs, NewDict("id", name, "kind", kind, "image", getDef(prof, "image", ""),
			"network", getDef(prof, "network", network), "required", listOf(orList(get(prof, "required"))),
			"limits", limits, "tools", tools, "docker_host", getDef(prof, "docker_host", ""), "extra", extra))
	}
	workers := []any{}
	for _, c := range cells {
		cfg := or(get(c, "agent_configuration"), defaultCfg)
		skills := any(NewDict())
		if truthy(cfg) {
			skills = orDict(get(orDict(getAny(configs, cfg)), "skills"))
		}
		folders := "none"
		if mode := get(skills, "mode"); Equal(mode, "selected") || Equal(mode, "snapshot") {
			folders = "folders"
		}
		models := []any{}
		for _, x := range orList(get(c, "models")) {
			models = append(models, pyStr(x))
		}
		paths := []any{}
		for _, x := range orList(get(skills, "paths")) {
			paths = append(paths, pyStr(x))
		}
		extra := NewDict()
		for _, k := range items(c).keys {
			if !cellKeys[k] {
				extra.Set(k, c.(*Dict).vals[k])
			}
		}
		workers = append(workers, NewDict(
			"id", pyStr(or(get(c, "id"), "")), "cli", or(get(c, "harness"), "claude-code"), "model", or(get(c, "model"), ""),
			"models", models, "models_form", has(c, "models"),
			"effort", or(get(c, "effort"), ""), "config", or(get(c, "config"), "clean"), "auth", or(get(c, "auth"), ""),
			"env", or(or(get(c, "environment"), defaultEnv), FolderEnv),
			"skills", folders, "skillPaths", paths, "profile", or(cfg, ""), "extra", extra))
	}
	checks := []any{}
	for _, v := range orList(raw.Get("verify")) {
		kind := getDef(v, "type", "shell")
		short := kind
		if s, ok := hashKey(kind); ok {
			if c, known := checkTypes[s]; known {
				short = c
			}
		}
		var known []string
		if s, ok := short.(string); ok {
			known = checkFields[s]
		}
		skip := setOf(append([]string{"name", "type", "location"}, known...)...)
		where := "host"
		if Equal(get(v, "location"), "environment") {
			where = "worker"
		}
		extra := NewDict()
		for _, k := range items(v).keys {
			if !skip[k] {
				extra.Set(k, v.(*Dict).vals[k])
			}
		}
		check := NewDict("name", getDef(v, "name", ""), "type", short, "path", "", "contains", "", "run", "",
			"expect_exit", int64(0), "expect_stdout", "", "url", "", "expect_status", int64(200), "expect_body", "",
			"where", where, "extra", extra)
		for _, k := range known {
			if x, ok := v.(*Dict).Lookup(k); ok {
				check.Set(k, x)
			}
		}
		checks = append(checks, check)
	}
	budget := get(t, "max_budget_usd")
	if budget == nil {
		budget = ""
	}
	materials := []any{}
	for _, m := range orList(get(task, "materials")) {
		materials = append(materials, pyStr(m))
	}
	return NewDict(
		"task", NewDict("prompt", prompt, "prompt_file", promptFile, "materials", materials,
			"fixture", or(get(task, "fixture_dir"), ""), "human", or(get(task, "human"), "unavailable"),
			"setup", commands(raw.Get("setup"))),
		"checks", checks,
		"run", NewDict("minutes", pyfmt.Round(float64(pyInt(getDef(t, "timeout_seconds", int64(1800))))/60, 2),
			"attempts", pyInt(getDef(t, "repetitions", int64(1))),
			"budget", budget, "parallel", pyInt(getDef(t, "parallel", int64(1))),
			"report_minutes", pyfmt.Round(float64(pyInt(getDef(t, "narrate_timeout_seconds", int64(900))))/60, 2),
			"order_seed", pyInt(getDef(t, "order_seed", int64(0))), "workspace", getDef(t, "workspace_root", "/tmp"),
			"shell", getDef(t, "shell_startup", "inherit"), "output_dir", getDef(t, "output_dir", "")),
		"reporter", NewDict("cli", or(get(rep, "harness"), ""), // empty: the first worker's agent that can report
			"model", or(get(rep, "model"), ""), "auth", or(get(rep, "auth"), "")),
		"account", NewDict("on", truthy(or(get(task, "identity_cmd"), raw.Get("preflight"))),
			"identity", identity(get(task, "identity_cmd")),
			"preflight", commands(raw.Get("preflight")), "teardown", commands(raw.Get("teardown"))),
		"envs", envs,
		"workers", workers,
	)
}

// ---------------------------------------------------------------- Docker

var (
	dockerMu     sync.Mutex
	dockerAt     time.Time
	dockerStub   bool
	dockerCached = util.M{"images": []any{}, "host": ""}
)

// StubDocker makes DockerInfo answer info from now on without asking Docker
// (tests; Python sets plan_edit._DOCKER).
func StubDocker(images []any, host string) {
	dockerMu.Lock()
	defer dockerMu.Unlock()
	if images == nil {
		images = []any{}
	}
	dockerStub, dockerCached = true, util.M{"images": images, "host": host}
}

// DockerInfo is the local Docker images (as pinned references) and the
// active context's socket, for the container editor. Read-only and best
// effort: nothing is pulled or started; cached for ttl (60 s by default).
func DockerInfo(ttl time.Duration) util.M {
	if ttl == 0 {
		ttl = 60 * time.Second
	}
	dockerMu.Lock()
	defer dockerMu.Unlock()
	if dockerStub || (!dockerAt.IsZero() && time.Since(dockerAt) < ttl) {
		return dockerCached
	}
	images, host := []any{}, ""
	if util.Which("docker") != "" {
		ask := func(argv ...string) string {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "docker", argv...).Output() // stdin is /dev/null
			if err != nil {
				return ""
			}
			return strings.ToValidUTF8(string(out), "�")
		}
		host = pyfmt.Strip(ask("context", "inspect", "--format", "{{.Endpoints.docker.Host}}"))
		lines := pyfmt.SplitLines(ask("image", "ls", "--no-trunc", "--format", "{{.Repository}}:{{.Tag}}\t{{.ID}}"))
		if len(lines) > 40 {
			lines = lines[:40]
		}
		for _, line := range lines {
			name, ident, _ := strings.Cut(line, "\t")
			if strings.HasPrefix(ident, "sha256:") {
				images = append(images, util.M{"ref": ident, "label": name + " (" + util.Clip(ident[7:], 12) + ")"})
			}
		}
	}
	if !strings.HasPrefix(host, "unix://") && !strings.HasPrefix(host, "tcp://") && !strings.HasPrefix(host, "npipe://") {
		host = ""
	}
	dockerAt, dockerCached = time.Now(), util.M{"images": images, "host": host}
	return dockerCached
}

// ---------------------------------------------------------------- options

func authClass(conf any) plugin.Auth {
	kind := get(orDict(conf), "type")
	name, ok := kind.(string)
	if !ok || !plugin.Has("auth", name) {
		return nil
	}
	a, err := plugin.NewAuth(name, util.M{})
	if err != nil {
		return nil
	}
	return a
}

func isolates(name string, profiles *Dict) bool {
	if profiles.Has(name) {
		def := false
		if cls := authClass(profiles.Get(name)); cls != nil {
			def = cls.Base().IsolatesConfig
		}
		return truthy(getDef(profiles.Get(name), "isolates_config", def))
	}
	a, err := plugin.NewAuth(name, util.M{})
	return err == nil && a.Base().IsolatesConfig
}

func applies(a plugin.Auth, harness string) bool {
	hs := a.Base().Harnesses
	return util.Contains(hs, "*") || util.Contains(hs, harness)
}

// Options is what the editors may offer: agents (installed, verified), their
// sign-in routes, model suggestions, reporter agents and local container images.
func Options(trial string, model *Dict) (out *Dict, err error) {
	text, err := readText(trial)
	if err != nil {
		return nil, err
	}
	raw, err := ParseTOML(text)
	if err != nil {
		return nil, err
	}
	err = catch(func() { out = options(raw, model) })
	return out, err
}

func options(raw *Dict, model *Dict) *Dict {
	profiles := items(orDict(raw.Get("auth")))
	var harnesses []*Dict
	for _, name := range plugin.Names("harness") {
		h, err := plugin.NewHarness(name)
		if err != nil {
			continue
		}
		b := h.Base()
		models := []any{}
		for _, m := range KnownModels[name] {
			models = append(models, m)
		}
		label := labels[name]
		if label == "" {
			label = name
		}
		harnesses = append(harnesses, NewDict("name", name, "label", label, "installed", safeAvailable(h),
			"verified", b.VerifiedLive, "reporter", b.CanReport, "clean", b.CleanSupported,
			"default_auth", b.DefaultAuthName(), "effort", effort[name], "models", models))
	}
	for _, adapter := range orList(raw.Get("adapters")) {
		harnesses = append(harnesses, NewDict("name", adapter, "label", adapter, "installed", true, "verified", false,
			"reporter", false, "clean", false, "default_auth", "inherit", "effort", true, "models", []any{}))
	}
	auth := NewDict()
	for _, h := range harnesses {
		hname := pyStr(h.Get("name"))
		def := pyStr(h.Get("default_auth"))
		routes := []any{NewDict("value", "", "label", "Default ("+def+")", "fresh_home", isolates(def, profiles),
			"kind", "default", "detail", "Uses the "+def+" sign-in.")}
		for _, name := range plugin.Names("auth") {
			a, err := plugin.NewAuth(name, util.M{})
			if err != nil || name == "env" || !applies(a, hname) {
				continue
			}
			detail := a.Base().Description
			if detail == "" {
				detail = name
			}
			routes = append(routes, NewDict("value", name, "label", name, "fresh_home", a.Base().IsolatesConfig,
				"kind", "type", "detail", detail))
		}
		for _, name := range profiles.Keys() {
			conf := profiles.Get(name)
			if cls := authClass(conf); cls != nil && applies(cls, hname) {
				routes = append(routes, NewDict("value", name, "label", name+" (in this trial)", "kind", "profile",
					"fresh_home", isolates(name, profiles),
					"detail", "[auth."+name+"] in trial.toml, type "+pyStr(get(conf, "type"))+"."))
			}
		}
		for _, t := range authTemplates[hname] {
			if !profiles.Has(t.name) {
				routes = append(routes, NewDict("value", t.name, "label", t.label, "fresh_home", true, "kind", "template",
					"detail", t.detail))
			}
		}
		auth.Set(hname, routes)
	}
	seen := map[string][]any{}
	workers := orList(get(model, "workers"))
	for _, w := range workers {
		for _, m := range append([]any{get(w, "model")}, orList(get(w, "models"))...) {
			if !truthy(m) {
				continue
			}
			cli, _ := hashKey(item(w, "cli"))
			if !has(append([]any{}, seen[cli]...), m) {
				seen[cli] = append(seen[cli], m)
			}
		}
	}
	list := make([]any, len(harnesses))
	for i, h := range harnesses {
		models := []any{}
		for _, m := range append(h.Get("models").([]any), seen[pyStr(h.Get("name"))]...) {
			if !has(models, m) {
				models = append(models, m)
			}
		}
		h.Set("models", models)
		list[i] = h
	}
	bash := []any{}
	for _, p := range envcheck.BashOnlyPatterns() {
		bash = append(bash, []any{p[0], p[1]})
	}
	docker := DockerInfo(0)
	return NewDict("harnesses", list, "auth", auth, "images", FromM(docker["images"]), "docker_host", docker["host"],
		"auto_reporter", AutoReporter(workers, orDict(raw.Get("adapters"))),
		"bash_only", bash,
		"check_types", []any{[]any{"file", "File exists"}, []any{"cmd", "Command succeeds"}, []any{"http", "HTTP returns"}},
		"efforts", []any{"", "low", "medium", "high"})
}

func safeAvailable(h plugin.Harness) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false // an odd plugin must not break the page
		}
	}()
	return h.Available()
}
