package agentconfig

// Port of skills/ajx/tests/test_agent_configuration.py. Synthetic skill trees,
// fake harnesses and a fake EnvironmentRunner: no agent, provider, Docker or
// network is used. Launches are exercised through the Launch* helpers the
// built-in harness adapters call (execute/narrate/report themselves belong to
// internal/builtin).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

// ---------------------------------------------------------------- fakes

type fakeHarness struct{ plugin.BaseHarness }

func newHarness(name string) plugin.Harness {
	bin := map[string]string{"codex": "codex", "claude-code": "claude", "kiro-cli": "kiro-cli"}[name]
	return plugin.Bind(&fakeHarness{plugin.BaseHarness{PluginName: name, BinaryName: bin}})
}

func allHarnesses() []plugin.Harness {
	return []plugin.Harness{newHarness("codex"), newHarness("claude-code"), newHarness("kiro-cli")}
}

func newAuth(name string, conf util.M, isolates bool) plugin.Auth {
	a := plugin.NewAuthBase(conf)
	a.PluginName = name
	a.Profile = name
	a.IsolatesConfig = isolates
	a.RequiredEnv = util.Strings(conf["required_env"])
	a.Self = a
	return a
}

func envAuth(conf util.M) plugin.Auth { return newAuth("env", conf, true) }

func localRunner() plugin.Runner { return &plugin.RunnerBase{PluginName: "local"} }

type shellCall struct {
	command string
	ctx     *plugin.Ctx
	env     map[string]string
}

// fakeEnvRunner exercises the EnvironmentRunner contract without Docker.
type fakeEnvRunner struct {
	plugin.RunnerBase
	backend        string
	calls          []shellCall
	scopeViolation string
}

func newEnvRunner(backend string) *fakeEnvRunner {
	r := &fakeEnvRunner{backend: backend}
	r.Self = r
	return r
}

func (r *fakeEnvRunner) Name() string    { return r.backend }
func (r *fakeEnvRunner) Backend() string { return r.backend }
func (r *fakeEnvRunner) EnvironmentEnv(ctx *plugin.Ctx) map[string]string {
	return map[string]string{"HOME": ctx.HomeDir, "XDG_CONFIG_HOME": ctx.ConfigDir,
		"XDG_DATA_HOME": ctx.HomeDir + "/.local/share", "XDG_STATE_HOME": ctx.HomeDir + "/.local/state",
		"XDG_CACHE_HOME": ctx.CacheDir, "PATH": "/usr/local/bin:/usr/bin:/bin",
		"PYTHONUSERBASE": ctx.HomeDir + "/.local"}
}
func (r *fakeEnvRunner) ChildEnv(overrides map[string]string, unset []string) map[string]string {
	return util.MergeEnv(overrides)
}
func (r *fakeEnvRunner) HostEnv(ctx *plugin.Ctx, overrides map[string]string, unset []string) map[string]string {
	return util.MergeEnv(overrides)
}
func (r *fakeEnvRunner) Doctor(*plugin.Ctx) util.M                          { return util.M{} }
func (r *fakeEnvRunner) Plan(*plugin.Ctx) (util.M, error)                   { return util.M{}, nil }
func (r *fakeEnvRunner) Prepare(*plugin.Ctx) (util.M, error)                { return util.M{}, nil }
func (r *fakeEnvRunner) Abort(*plugin.Ctx) util.M                           { return util.M{} }
func (r *fakeEnvRunner) Release(*plugin.Ctx) (util.M, error)                { return util.M{}, nil }
func (r *fakeEnvRunner) RestoreWorkerOwnership(*plugin.Ctx) (util.M, error) { return util.M{}, nil }

const fakeHelp = "--strict-config --config --disable --setting-sources --settings --disable-slash-commands " +
	"--strict-mcp-config --agent --agent-engine --no-interactive"

func fakeFeatures() string {
	var lines []string
	for _, k := range codexFeatures {
		lines = append(lines, k+" stable false")
	}
	return strings.Join(lines, "\n")
}

func (r *fakeEnvRunner) Shell(command string, ctx *plugin.Ctx, timeout int, env map[string]string) util.M {
	r.calls = append(r.calls, shellCall{command, ctx, env})
	ok := func(text string) util.M { return util.M{"exit_code": int64(0), "stdout": text, "timed_out": false} }
	if strings.HasPrefix(command, "ajx_probe_text=$(") {
		if strings.Contains(command, "features list") {
			return ok(fakeFeatures())
		}
		return ok(fakeHelp)
	}
	if strings.HasPrefix(command, "if [ -e ") {
		code := int64(0)
		if r.scopeViolation != "" {
			code = 42
		}
		return util.M{"exit_code": code, "stdout": r.scopeViolation, "timed_out": false}
	}
	args := strings.Fields(command)
	if len(args) == 3 && args[0] == "command" && args[1] == "-v" {
		return ok("/container/bin/" + args[2] + "\n")
	}
	versions := map[string]string{"codex": "0.160.1", "claude": "2.1.293", "kiro-cli-chat": "2.28.0"}
	if len(args) == 2 && args[1] == "--version" {
		return ok(versions[filepath.Base(args[0])])
	}
	panic("Non-capability command in fake environment: " + command)
}

// ---------------------------------------------------------------- fixture

type fixture struct {
	t       *testing.T
	root    string
	source  string
	counter int
}

func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeText(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

const skillText = "---\nname: product-guide\ndescription: Use the fictional widget tool.\n---\n" +
	"Read assets/guide.txt and run scripts/check.sh when needed.\n"

func fakeProbe(name string, ctx *plugin.Ctx) util.M {
	return util.M{"binary": map[string]string{"codex": "codex", "claude-code": "claude", "kiro-cli": "kiro-cli-chat"}[name],
		"version": "synthetic", "provider_called": false, "live_extension_activation_verified": false}
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{t: t, root: realTempDir(t)}
	f.source = filepath.Join(f.root, "sources", "product-guide")
	writeText(t, filepath.Join(f.source, "SKILL.md"), skillText)
	writeText(t, filepath.Join(f.source, "assets", "guide.txt"), "Fictional reference material.\n")
	writeText(t, filepath.Join(f.source, "scripts", "check.sh"), "#!/bin/sh\nprintf 'synthetic\\n'\n")
	if err := os.Chmod(filepath.Join(f.source, "scripts", "check.sh"), 0o700); err != nil {
		t.Fatal(err)
	}
	savedSystem, savedProbe := systemPaths, probeFn
	systemPaths = map[string][]string{"codex": nil, "claude-code": nil, "kiro-cli": nil}
	probeFn = fakeProbe
	t.Cleanup(func() { systemPaths, probeFn = savedSystem, savedProbe })
	return f
}

func (f *fixture) normalize(raw util.M) (util.M, error) { return NormalizeProfiles(raw, f.root) }

func (f *fixture) profileErr(mode string, extra util.M) (util.M, error) {
	skills := util.M{"mode": mode}
	if mode != "none" {
		rel, _ := filepath.Rel(f.root, f.source)
		skills["paths"] = []any{rel}
	}
	raw := util.M{"skills": skills}
	for k, v := range extra {
		raw[k] = v
	}
	out, err := f.normalize(util.M{"product": raw})
	if err != nil {
		return nil, err
	}
	return util.Map(out["product"]), nil
}

func (f *fixture) profile(mode string, extra util.M) util.M {
	f.t.Helper()
	p, err := f.profileErr(mode, extra)
	if err != nil {
		f.t.Fatalf("normalize: %v", err)
	}
	return p
}

func (f *fixture) ctx(h plugin.Harness, auth plugin.Auth) *plugin.Ctx {
	f.counter++
	base := filepath.Join(f.root, "attempt-"+itoa(f.counter))
	paths := map[string]string{}
	for _, name := range []string{"workspace", "config_dir", "cache_dir", "home_dir", "run_dir"} {
		paths[name] = filepath.Join(base, name)
		if err := os.MkdirAll(paths[name], 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
	if auth == nil {
		if h.Name() == "kiro-cli" {
			auth = envAuth(util.M{"required_env": []any{"KIRO_API_KEY"}})
		} else {
			auth = envAuth(nil)
		}
	}
	return &plugin.Ctx{Workspace: paths["workspace"], ConfigDir: paths["config_dir"], CacheDir: paths["cache_dir"],
		HomeDir: paths["home_dir"], RunDir: paths["run_dir"],
		Cell: util.M{"id": "synthetic", "harness": h.Name(), "config": "clean", "agent_configuration": "product",
			"args": []any{}, "env": util.M{}},
		Spec: util.M{"trial": util.M{}, "env": util.M{}}, Env: map[string]string{}, Auth: auth, Runner: localRunner(),
		State:    util.M{"execute": util.M{"session_id": "synthetic-session", "config_isolated": true}},
		UnsetEnv: []string{}, Timeout: 2, PromptText: "Use the fictional widget."}
}

func itoa(n int) string { return strings.TrimSpace(util.Str(int64(n))) }

func (f *fixture) prepared(h plugin.Harness, mode string, auth plugin.Auth) (*plugin.Ctx, util.M) {
	f.t.Helper()
	ctx := f.ctx(h, auth)
	manifest, err := Prepare(f.profile(mode, nil), ctx, h)
	if err != nil {
		f.t.Fatalf("prepare %s: %v", h.Name(), err)
	}
	ctx.State["agent_configuration"] = manifest
	return ctx, manifest
}

// launched is what a built-in adapter passes to Harness.Run.
type launched struct {
	ctx  *plugin.Ctx
	argv []string
	env  map[string]string
}

// launch builds a worker/narrator launch the way the built-in adapters do.
func launch(h plugin.Harness, ctx *plugin.Ctx) (launched, error) {
	args, err := LaunchArgs(ctx, h)
	if err != nil {
		return launched{}, err
	}
	env := map[string]string{}
	if Configuration(ctx) != nil {
		if env, err = LaunchEnv(ctx, h); err != nil {
			return launched{}, err
		}
	}
	argv := append([]string{LaunchBinary(ctx, h)}, util.Strings(ctx.Cell["args"])...)
	argv = append(argv, args...)
	return launched{ctx: LaunchContext(ctx), argv: argv, env: env}, nil
}

func mustLaunch(t *testing.T, h plugin.Harness, ctx *plugin.Ctx) launched {
	t.Helper()
	l, err := launch(h, ctx)
	if err != nil {
		t.Fatalf("launch %s: %v", h.Name(), err)
	}
	return l
}

func wantErr(t *testing.T, err error, pattern string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error matching %q, got nil", pattern)
	}
	var ve *Error
	if !errors.As(err, &ve) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if !regexp.MustCompile(pattern).MatchString(err.Error()) {
		t.Fatalf("error %q does not match %q", err.Error(), pattern)
	}
}

func contains(items []string, s string) bool { return util.Contains(items, s) }

func indexOf(items []string, s string) int {
	for i, x := range items {
		if x == s {
			return i
		}
	}
	return -1
}

func firstEntry(p util.M) util.M { return util.Maps(util.Get(p, "skills", "entries"))[0] }

// ---------------------------------------------------------------- tests

func TestSchemaDefaultsAndCompletePins(t *testing.T) {
	f := newFixture(t)
	out, err := f.normalize(util.M{"plain": util.M{}})
	if err != nil {
		t.Fatal(err)
	}
	plain := util.Map(out["plain"])
	if util.Get(plain, "skills", "mode") != "none" {
		t.Fatalf("default skills mode: %v", util.Get(plain, "skills"))
	}
	if got := util.PyJSON(plain["plugins"]); got != `{"entries": [], "mode": "none"}` {
		t.Fatalf("plugins: %s", got)
	}
	entry := firstEntry(f.profile("selected", nil))
	if entry["name"] != "product-guide" {
		t.Fatalf("name %v", entry["name"])
	}
	var files []string
	for _, rec := range util.Maps(entry["files"]) {
		if rec["type"] == "file" {
			files = append(files, util.S(rec["path"]))
		}
		if rec["path"] == "scripts/check.sh" && rec["executable"] != true {
			t.Fatal("check.sh should be executable")
		}
	}
	if strings.Join(files, ",") != "SKILL.md,assets/guide.txt,scripts/check.sh" {
		t.Fatalf("files %v", files)
	}
	pinned := util.M{"skills": util.M{"mode": "snapshot", "paths": []any{f.source},
		"pins": util.M{"product-guide": entry["sha256"]}}}
	got, err := f.normalize(util.M{"pinned": pinned})
	if err != nil || firstEntry(util.Map(got["pinned"]))["sha256"] != entry["sha256"] {
		t.Fatalf("pinned: %v %v", err, got)
	}
	util.Map(pinned["skills"])["pins"] = util.M{"product-guide": strings.Repeat("0", 64)}
	_, err = f.normalize(util.M{"pinned": pinned})
	wantErr(t, err, "pin mismatch")
}

func TestHashCoversAllContentAndExecutableBit(t *testing.T) {
	f := newFixture(t)
	first := firstEntry(f.profile("selected", nil))["sha256"]
	script := filepath.Join(f.source, "scripts", "check.sh")
	if err := os.WriteFile(script, []byte(readText(t, script)+"# changed\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	second := firstEntry(f.profile("selected", nil))["sha256"]
	if first == second {
		t.Fatal("content change must change the hash")
	}
	os.Chmod(script, 0o600)
	if second == firstEntry(f.profile("selected", nil))["sha256"] {
		t.Fatal("executable bit must change the hash")
	}
	writeText(t, filepath.Join(f.source, "assets", "guide.txt"), "Another reference.")
	if first == firstEntry(f.profile("selected", nil))["sha256"] {
		t.Fatal("asset change must change the hash")
	}
}

func TestLocalMarkdownDependenciesMustBeBundledAndCannotEscape(t *testing.T) {
	f := newFixture(t)
	skill := filepath.Join(f.source, "SKILL.md")
	writeText(t, skill, skillText+"\n[Guide](assets/guide.txt)\n")
	f.profile("selected", nil)
	for _, link := range []string{"../outside.txt", "/synthetic/private.md", "assets/missing.md", "file:///synthetic/private.md"} {
		writeText(t, skill, skillText+"\n[Reference]("+link+")\n")
		_, err := f.profileErr("selected", nil)
		wantErr(t, err, "skill snapshot")
	}
	writeText(t, skill, skillText+"\n[Public documentation](https://example.invalid/reference)\n")
	f.profile("selected", nil)
}

func TestClaudeInvocationSemanticsArePreservedWithoutRewriting(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.source, "SKILL.md")
	writeText(t, path, strings.Replace(skillText, "description:", "disable-model-invocation: true\nuser-invocable: false\ndescription:", 1))
	profile := f.profile("selected", nil)
	claude := newHarness("claude-code")
	ctx := f.ctx(claude, nil)
	manifest, err := Prepare(profile, ctx, claude)
	if err != nil {
		t.Fatal(err)
	}
	skill := firstEntry(manifest)
	if skill["automatic_invocation"] != false || skill["user_invocable"] != false {
		t.Fatalf("invocation: %v", skill)
	}
	if readText(t, containedPath(util.S(skill["location"]), ctx)+"/SKILL.md") != readText(t, path) {
		t.Fatal("SKILL.md must be installed verbatim")
	}
	for _, h := range []plugin.Harness{newHarness("codex"), newHarness("kiro-cli")} {
		c := f.ctx(h, nil)
		wantErr(t, ValidateForCell(profile, c.Cell, h, c.Auth, nil), "semantics cannot be preserved")
	}
}

func TestRejectsUnboundedOrImplicitSnapshotPaths(t *testing.T) {
	f := newFixture(t)
	for _, paths := range [][]any{{}, {"/"}, {"sources/*"}, {"sources"}, {"https://example.invalid/skill"}} {
		_, err := f.normalize(util.M{"test": util.M{"skills": util.M{"mode": "snapshot", "paths": paths}}})
		if err == nil {
			t.Fatalf("paths %v should be rejected", paths)
		}
	}
	saved := homeFn
	homeFn = func() string { return f.source }
	defer func() { homeFn = saved }()
	_, err := f.profileErr("snapshot", nil)
	wantErr(t, err, "root")
}

func TestRejectsUnknownControlsDependenciesAndCollisions(t *testing.T) {
	f := newFixture(t)
	bad := []util.M{
		{"unknown": true}, {"plugins": util.M{"mode": "selected", "paths": []any{"x"}}},
		{"hooks": util.M{"mode": "snapshot", "paths": []any{"x"}}},
		{"skills": util.M{"mode": "none", "paths": []any{f.source}}},
		{"skills": util.M{"mode": "selected", "paths": []any{f.source}, "dependencies": []any{"plugin"}}},
		{"skills": util.M{"mode": "selected", "paths": []any{f.source, f.source}}},
		{"require_absence": "yes"},
	}
	for _, raw := range bad {
		if _, err := f.normalize(util.M{"test": raw}); err == nil {
			t.Fatalf("%v should be rejected", raw)
		} else if !errors.As(err, new(*Error)) {
			t.Fatalf("%v: %T", raw, err)
		}
	}
	skill := filepath.Join(f.source, "SKILL.md")
	for _, field := range []string{"hooks: {}", "context: fork", "dependencies: plugin"} {
		writeText(t, skill, strings.Replace(skillText, "description:", field+"\ndescription:", 1))
		_, err := f.profileErr("selected", nil)
		wantErr(t, err, "unsupported")
	}
	writeText(t, skill, skillText)
	writeText(t, filepath.Join(f.source, "agents", "openai.yaml"), "dependencies:\n  tools: []\n")
	_, err := f.profileErr("selected", nil)
	wantErr(t, err, "dependency")
}

func TestRejectsNameMismatchAndCasePathCollisions(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.source, "SKILL.md")
	writeText(t, path, strings.Replace(skillText, "name: product-guide", "name: other", 1))
	_, err := f.profileErr("selected", nil)
	wantErr(t, err, "match")
	writeText(t, path, skillText)
	writeText(t, filepath.Join(f.source, "Guide.txt"), "one")
	if _, err := os.Stat(filepath.Join(f.source, "guide.txt")); err != nil { // case-sensitive filesystems
		writeText(t, filepath.Join(f.source, "guide.txt"), "two")
		_, err := f.profileErr("selected", nil)
		wantErr(t, err, "collision")
	}
}

func TestRejectsSymlinksSpecialFilesHiddenFilesAndCaches(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(f.root, "outside.txt")
	writeText(t, outside, "Synthetic private content.")
	link := filepath.Join(f.source, "link")
	os.Symlink(outside, link)
	_, err := f.profileErr("selected", nil)
	wantErr(t, err, "symlink")
	os.Remove(link)
	inside := filepath.Join(f.source, "inside-link")
	os.Symlink(filepath.Join(f.source, "SKILL.md"), inside)
	_, err = f.profileErr("selected", nil)
	wantErr(t, err, "symlink")
	os.Remove(inside)
	fifo := filepath.Join(f.source, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = f.profileErr("selected", nil)
	wantErr(t, err, "special file")
	os.Remove(fifo)
	for _, name := range []string{".env", ".git", "__pycache__", "node_modules", "credentials.json", "private.key"} {
		p := filepath.Join(f.source, name)
		writeText(t, p, "Synthetic secret/cache placeholder.")
		_, err := f.profileErr("selected", nil)
		wantErr(t, err, "disallowed")
		os.Remove(p)
	}
}

const grantNote = "skill product-guide declares tool grants (allowed-tools); AJX records them as declared_tool_grants " +
	"but does not enforce or restrict them, and a worker running with permission prompts bypassed is not " +
	"restricted by them."

func TestAllowedToolsAndVersionAreRecordedNotEnforced(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.source, "SKILL.md")
	writeText(t, path, strings.Replace(skillText, "description:",
		"allowed-tools: Bash(aws:*) Bash(npm:*) Read Grep\nversion: 1.2.3\ndescription:", 1))
	grants := `["Bash(aws:*)", "Bash(npm:*)", "Read", "Grep"]`
	entry := firstEntry(f.profile("selected", nil))
	if util.PyJSON(entry["declared_tool_grants"]) != grants || entry["version"] != "1.2.3" {
		t.Fatalf("entry %v", entry)
	}
	for _, h := range allHarnesses() {
		ctx, manifest := f.prepared(h, "selected", nil)
		skill := firstEntry(manifest)
		if util.PyJSON(skill["declared_tool_grants"]) != grants || skill["version"] != "1.2.3" {
			t.Fatalf("%s: %v", h.Name(), skill)
		}
		// The shipped SKILL.md is installed verbatim: no fork, so the hash stays the artifact's.
		if readText(t, containedPath(util.S(skill["location"]), ctx)+"/SKILL.md") != readText(t, path) {
			t.Fatal("SKILL.md must be installed verbatim")
		}
		if !contains(util.Strings(manifest["limitations"]), grantNote) {
			t.Fatalf("%s: limitations %v", h.Name(), manifest["limitations"])
		}
		written := ""
		for _, item := range util.Maps(manifest["configuration_files"]) {
			written += readText(t, expandStr(util.S(item["location"]), ctx))
		}
		if strings.Contains(util.PyJSON(manifest["launch"])+written, "Bash(aws:*)") {
			t.Fatalf("%s: grants must not reach launch configuration", h.Name())
		}
	}
	writeText(t, path, skillText)
	plain := firstEntry(f.profile("selected", nil))
	if util.PyJSON(plain["declared_tool_grants"]) != "[]" || plain["version"] != nil {
		t.Fatalf("plain %v", plain)
	}
	_, manifest := f.prepared(newHarness("codex"), "selected", nil)
	for _, line := range util.Strings(manifest["limitations"]) {
		if strings.Contains(line, "allowed-tools") {
			t.Fatalf("unexpected grant limitation: %s", line)
		}
	}
	header := "---\nname: product-guide\n%s\ndescription: d\n---\n"
	parse := func(field string) (m util.M, err error) {
		defer catch(&err)
		return frontmatter([]byte(strings.Replace(header, "%s", field, 1)), "product-guide"), nil
	}
	m, err := parse(`allowed-tools: "Bash(git add:*), Read,Grep"`)
	if err != nil || util.PyJSON(m["declared_tool_grants"]) != `["Bash(git add:*)", "Read", "Grep"]` {
		t.Fatalf("comma form: %v %v", m, err)
	}
	if m, err := parse("allowed-tools:"); err != nil || util.PyJSON(m["declared_tool_grants"]) != "[]" {
		t.Fatalf("empty grants: %v %v", m, err)
	}
	if m, err := parse("version: '2'"); err != nil || m["version"] != "2" {
		t.Fatalf("quoted version: %v %v", m, err)
	}
	for _, bad := range []string{"allowed-tools: Bash(aws:*", "allowed-tools: Read)", "allowed-tools: Read # comment",
		"allowed-tools: [Read]", "version: \x1b[0m"} {
		if _, err := parse(bad); err == nil {
			t.Fatalf("%q should be rejected", bad)
		}
	}
}

func TestExecutionAndDependencyFieldsStayRejectedWithTheExistingMessage(t *testing.T) {
	f := newFixture(t)
	skill := filepath.Join(f.source, "SKILL.md")
	for _, field := range []string{"hooks: {}", "dependencies: plugin", "context: fork", "scripts: run.sh"} {
		key := strings.SplitN(field, ":", 2)[0]
		writeText(t, skill, strings.Replace(skillText, "description:", field+"\ndescription:", 1))
		_, err := f.profileErr("selected", nil)
		wantErr(t, err, regexp.QuoteMeta("unsupported SKILL.md field '"+key+"'"))
	}
}

func TestEmptyGitkeepPlaceholdersAreCopiedButOtherHiddenEntriesStayRejected(t *testing.T) {
	f := newFixture(t)
	writeText(t, filepath.Join(f.source, "scripts", ".gitkeep"), "")
	writeText(t, filepath.Join(f.source, "assets", ".keep"), "")
	entry := firstEntry(f.profile("selected", nil))
	found := map[string]bool{}
	for _, rec := range util.Maps(entry["files"]) {
		p := util.S(rec["path"])
		if strings.HasSuffix(p, ".gitkeep") || strings.HasSuffix(p, ".keep") {
			found[p] = true
			if rec["type"] != "file" || rec["bytes"] != int64(0) || rec["sha256"] != util.Sha256Bytes(nil) {
				t.Fatalf("placeholder record %v", rec)
			}
		}
	}
	if !found["scripts/.gitkeep"] || !found["assets/.keep"] || len(found) != 2 {
		t.Fatalf("placeholders %v", found)
	}
	ctx, manifest := f.prepared(newHarness("codex"), "selected", nil)
	installed := containedPath(util.S(firstEntry(manifest)["location"]), ctx)
	for _, rel := range []string{"scripts/.gitkeep", "assets/.keep"} {
		if fi, err := os.Stat(filepath.Join(installed, rel)); err != nil || fi.Size() != 0 {
			t.Fatalf("%s not installed empty: %v", rel, err)
		}
	}
	if readTree(installed).sha256 != entry["sha256"] {
		t.Fatal("installed hash mismatch")
	}
	gitkeep := filepath.Join(f.source, "scripts", ".gitkeep")
	writeText(t, gitkeep, "not empty\n")
	_, err := f.profileErr("selected", nil)
	wantErr(t, err, `disallowed.*\(\.gitkeep/\.keep placeholders must be empty\)`)
	os.Remove(gitkeep)
	os.Mkdir(gitkeep, 0o755)
	_, err = f.profileErr("selected", nil)
	wantErr(t, err, "disallowed")
	os.Remove(gitkeep)
	for _, name := range []string{".env", ".gitignore", ".DS_Store", ".gitkeep.bak"} {
		p := filepath.Join(f.source, name)
		writeText(t, p, "") // Empty, but only the exact names .gitkeep/.keep are placeholders.
		_, err := f.profileErr("selected", nil)
		wantErr(t, err, "disallowed")
		os.Remove(p)
	}
}

func TestBoundsApplyBeforeReadingOversizeContent(t *testing.T) {
	f := newFixture(t)
	savedFiles, savedFile, savedBytes := maxFiles, maxFile, maxBytes
	defer func() { maxFiles, maxFile, maxBytes = savedFiles, savedFile, savedBytes }()
	maxFiles = 2
	_, err := f.profileErr("selected", nil)
	wantErr(t, err, "too many")
	maxFiles, maxFile = savedFiles, 10
	_, err = f.profileErr("selected", nil)
	wantErr(t, err, "size limit")
	maxFile, maxBytes = savedFile, 10
	_, err = f.profileErr("selected", nil)
	wantErr(t, err, "size limit")
}

func TestSourceMutationBeforePrepareIsRejected(t *testing.T) {
	f := newFixture(t)
	profile := f.profile("selected", nil)
	writeText(t, filepath.Join(f.source, "assets", "guide.txt"), "Changed after resolution.")
	codex := newHarness("codex")
	_, err := Prepare(profile, f.ctx(codex, nil), codex)
	wantErr(t, err, "changed after resolution")
}

func TestResumeContractKeepsLiveSourceIntegrityChecks(t *testing.T) {
	f := newFixture(t)
	profile := f.profile("selected", nil)
	original := util.PyJSON(profile)
	contract := ResumeContract(profile)
	if ResumeContract(nil) != nil {
		t.Fatal("nil contract")
	}
	if util.PyJSON(profile) != original {
		t.Fatal("ResumeContract must not modify its input")
	}
	entry := firstEntry(contract)
	if _, ok := entry["_identity"]; ok {
		t.Fatal("_identity must be excluded")
	}
	if entry["_source"] != f.source {
		t.Fatalf("_source %v", entry["_source"])
	}
	source := filepath.Join(f.source, "SKILL.md")
	fi, _ := os.Stat(source)
	os.Chtimes(source, time.Now(), fi.ModTime().Add(time.Second))
	touched := f.profile("selected", nil)
	if sameIdentity(firstEntry(profile)["_identity"], firstEntry(touched)["_identity"]) {
		t.Fatal("touching the source must change its identity")
	}
	if util.PyJSON(ResumeContract(touched)) != util.PyJSON(contract) {
		t.Fatal("resume contract must ignore source identities")
	}
	// A fresh resume contract must not weaken resolution-to-copy checks.
	codex := newHarness("codex")
	_, err := Prepare(profile, f.ctx(codex, nil), codex)
	wantErr(t, err, "changed after resolution")
}

func TestResumeContractMatchesAttemptsSavedBeforeDescriptiveFields(t *testing.T) {
	f := newFixture(t)
	profile := f.profile("selected", nil)
	saved := util.MapOr(util.DeepCopy(profile))
	for _, entry := range util.Maps(util.Get(saved, "skills", "entries")) { // saved before these fields existed
		delete(entry, "version")
		delete(entry, "declared_tool_grants")
	}
	if util.PyJSON(ResumeContract(saved)) != util.PyJSON(ResumeContract(profile)) {
		t.Fatal("contracts should match")
	}
	if _, ok := firstEntry(ResumeContract(profile))["version"]; ok {
		t.Fatal("undeclared version must be omitted")
	}
	path := filepath.Join(f.source, "SKILL.md")
	writeText(t, path, strings.Replace(skillText, "description:", "allowed-tools: Read\nversion: 2\ndescription:", 1))
	declared := firstEntry(ResumeContract(f.profile("selected", nil)))
	if util.PyJSON(declared["declared_tool_grants"]) != `["Read"]` || declared["version"] != "2" {
		t.Fatalf("declared %v", declared)
	}
	if util.PyJSON(ResumeContract(profile)) == util.PyJSON(ResumeContract(f.profile("selected", nil))) {
		t.Fatal("declared fields must change the contract")
	}
}

func TestSourceMutationDuringCopyIsRejectedAndPartialCopyRemoved(t *testing.T) {
	f := newFixture(t)
	profile := f.profile("selected", nil)
	codex := newHarness("codex")
	ctx := f.ctx(codex, nil)
	original := writeFileFn
	writeFileFn = func(path string, data []byte) error {
		err := original(path, data)
		if filepath.Base(path) == "SKILL.md" {
			writeText(t, filepath.Join(f.source, "assets", "guide.txt"), "Changed during copy.")
		}
		return err
	}
	defer func() { writeFileFn = original }()
	_, err := Prepare(profile, ctx, codex)
	wantErr(t, err, "changed during copy")
	if lexists(filepath.Join(ctx.HomeDir, ".agents/skills/product-guide")) {
		t.Fatal("partial copy must be removed")
	}
	if ctx.AgentConfiguration != nil {
		t.Fatal("failed prepare must not set the manifest")
	}
}

func TestSelectedLaunchConfigurationForEveryHarness(t *testing.T) {
	f := newFixture(t)
	for _, h := range allHarnesses() {
		ctx, manifest := f.prepared(h, "selected", nil)
		l := mustLaunch(t, h, ctx)
		if l.env["HOME"] != ctx.HomeDir {
			t.Fatalf("%s HOME %q", h.Name(), l.env["HOME"])
		}
		skill := firstEntry(manifest)
		installed := containedPath(util.S(skill["location"]), ctx)
		for _, rel := range []string{"SKILL.md", "scripts/check.sh"} {
			if readText(t, filepath.Join(installed, rel)) != readText(t, filepath.Join(f.source, rel)) {
				t.Fatalf("%s: %s differs", h.Name(), rel)
			}
		}
		if readTree(installed).sha256 != skill["sha256"] {
			t.Fatal("installed hash")
		}
		switch h.Name() {
		case "codex":
			if l.env["CODEX_HOME"] != ctx.ConfigDir {
				t.Fatal("CODEX_HOME")
			}
			for _, want := range []string{"features.plugins=false", "features.hooks=false", "skills.bundled.enabled=false"} {
				if !contains(l.argv, want) {
					t.Fatalf("codex argv missing %s: %v", want, l.argv)
				}
			}
			ok := false
			for _, a := range l.argv {
				ok = ok || (strings.Contains(a, "skills.config=") && strings.Contains(a, "product-guide/SKILL.md"))
			}
			if !ok {
				t.Fatalf("skills.config missing: %v", l.argv)
			}
		case "claude-code":
			if contains(l.argv, "--safe-mode") || contains(l.argv, "--disable-slash-commands") {
				t.Fatalf("claude argv %v", l.argv)
			}
			if l.argv[indexOf(l.argv, "--setting-sources")+1] != "user" {
				t.Fatal("setting sources")
			}
			var settings util.M
			json.Unmarshal([]byte(readText(t, filepath.Join(ctx.ConfigDir, "settings.json"))), &settings)
			if settings["disableAllHooks"] != true || settings["disableBundledSkills"] != true ||
				util.PyJSON(settings["enabledPlugins"]) != "{}" {
				t.Fatalf("settings %v", settings)
			}
		default:
			if l.argv[0] != "kiro-cli-chat" || l.env["KIRO_HOME"] != ctx.ConfigDir {
				t.Fatalf("kiro launch %v %v", l.argv, l.env)
			}
			if l.argv[indexOf(l.argv, "--agent-engine")+1] != "v2" {
				t.Fatal("agent engine")
			}
			var agent util.M
			json.Unmarshal([]byte(readText(t, filepath.Join(ctx.ConfigDir, "agents/ajx-profile.json"))), &agent)
			if util.PyJSON(agent["resources"]) != util.PyJSON([]any{"skill://" + installed + "/SKILL.md"}) ||
				util.PyJSON(agent["hooks"]) != "{}" || agent["includeMcpJson"] != false {
				t.Fatalf("agent %v", agent)
			}
		}
	}
}

func TestNoneUsesRealControlsAndInstallsNoOptionalSkills(t *testing.T) {
	f := newFixture(t)
	for _, h := range allHarnesses() {
		ctx, manifest := f.prepared(h, "none", nil)
		l := mustLaunch(t, h, ctx)
		if len(util.List(util.Get(manifest, "skills", "entries"))) != 0 {
			t.Fatal("no skills expected")
		}
		switch h.Name() {
		case "codex":
			if !contains(l.argv, "skills.config=[]") || !contains(l.argv, "skills.bundled.enabled=false") {
				t.Fatalf("codex argv %v", l.argv)
			}
		case "claude-code":
			if !contains(l.argv, "--disable-slash-commands") {
				t.Fatalf("claude argv %v", l.argv)
			}
		default:
			var conf util.M
			json.Unmarshal([]byte(readText(t, filepath.Join(ctx.ConfigDir, "agents/ajx-profile.json"))), &conf)
			if util.PyJSON(conf["resources"]) != "[]" {
				t.Fatalf("resources %v", conf["resources"])
			}
		}
	}
}

func TestManifestIsSafeAndUnobservedUsageStaysUnknown(t *testing.T) {
	f := newFixture(t)
	for _, h := range allHarnesses() {
		_, manifest := f.prepared(h, "selected", nil)
		encoded := util.PyJSON(manifest)
		for _, bad := range []string{f.source, f.root, `"_source"`, `"_identity"`} {
			if strings.Contains(encoded, bad) {
				t.Fatalf("%s manifest contains %s", h.Name(), bad)
			}
		}
		if manifest["content_absence"] != "unsupported" {
			t.Fatal("content_absence")
		}
		for _, e := range util.Maps(util.Get(manifest, "skills", "entries")) {
			if e["installed"] != true || e["discoverable"] != "unknown" || e["discovery_configured"] != true ||
				e["enabled"] != true || e["loaded"] != "unknown" || e["invoked"] != "unknown" {
				t.Fatalf("entry states %v", e)
			}
		}
		if util.Get(manifest, "hooks", "invoked") != "unknown" || util.Get(manifest, "plugins", "invoked") != "unknown" ||
			!util.Truthy(manifest["uncontrolled_sources"]) {
			t.Fatal("unknown states")
		}
	}
}

// persisted round-trips a manifest through JSON the way state.json does.
func persisted(t *testing.T, manifest util.M) util.M {
	t.Helper()
	data, err := util.MarshalIndent(manifest)
	if err != nil {
		t.Fatal(err)
	}
	v, err := util.DecodeJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	return util.Map(v)
}

func TestNarrationRetainsPersistedProfileWithoutSource(t *testing.T) {
	f := newFixture(t)
	profile := f.profile("selected", nil)
	var last *plugin.Ctx
	var lastH plugin.Harness
	for _, h := range allHarnesses() {
		ctx := f.ctx(h, nil)
		manifest, err := Prepare(profile, ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		workerArgs, err := LaunchArgs(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		ctx.State["agent_configuration"] = persisted(t, manifest)
		ctx.AgentConfiguration = nil
		l := mustLaunch(t, h, ctx)
		env, _ := LaunchEnv(ctx, h)
		if util.PyJSON(l.env) != util.PyJSON(env) || l.env["HOME"] != ctx.HomeDir {
			t.Fatal("narration env")
		}
		for _, v := range workerArgs {
			if !contains(l.argv, v) {
				t.Fatalf("%s: narration argv lacks %s", h.Name(), v)
			}
		}
		if strings.Join(workerArgs, "\x00") != strings.Join(l.argv[1:], "\x00") {
			t.Fatalf("%s: persisted launch args differ:\n%v\n%v", h.Name(), workerArgs, l.argv[1:])
		}
		last, lastH = ctx, h
	}
	// Normalized source locations are irrelevant after a persisted prepare.
	os.RemoveAll(f.source)
	mustLaunch(t, lastH, last)
}

func TestReporterRejectsWorkerProfile(t *testing.T) {
	f := newFixture(t)
	for _, h := range allHarnesses() {
		worker, _ := f.prepared(h, "selected", nil)
		worker.StageName = "extract"
		wantErr(t, EnsureReporterContext(worker), "separate fixed context")
		reporter := f.ctx(h, nil)
		delete(reporter.Cell, "agent_configuration")
		reporter.StageName = "extract"
		if err := EnsureReporterContext(reporter); err != nil {
			t.Fatal(err)
		}
		if LaunchContext(reporter) != reporter {
			t.Fatal("reporter ctx must be unchanged")
		}
		args, err := LaunchArgs(reporter, h)
		if err != nil || len(args) != 0 {
			t.Fatalf("reporter args %v %v", args, err)
		}
	}
	// A context carrying only the cell's profile selection is also rejected.
	h := newHarness("codex")
	c := f.ctx(h, nil)
	wantErr(t, EnsureReporterContext(c), "separate fixed context")
}

func TestCellAuthEnvAndPermissionArgumentConflicts(t *testing.T) {
	f := newFixture(t)
	conflicts := map[string][][]any{
		"codex": {{"--profile", "ambient"}, {"-c", "features.plugins=true"}, {"--enable=plugins"},
			{"--cd", "/"}, {"-cskills.config=[]"}, {"--config=model=x\nfeatures.plugins=true"}},
		"claude-code": {{"--settings", "{}"}, {"--plugin-dir", "/"}, {"--add-dir=/"},
			{"--safe-mode"}, {"--disable-slash-commands"}},
		"kiro-cli": {{"--agent", "ambient"}, {"--agent-engine=v3"}, {"--cloud"}, {"--resume-id", "other"}},
	}
	profile := f.profile("selected", nil)
	for _, h := range allHarnesses() {
		for _, args := range conflicts[h.Name()] {
			ctx := f.ctx(h, nil)
			ctx.Cell["args"] = args
			if err := ValidateForCell(profile, ctx.Cell, h, ctx.Auth, nil); err == nil {
				t.Fatalf("%s %v should be rejected", h.Name(), args)
			}
		}
		ctx := f.ctx(h, nil)
		for _, key := range []string{"HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "KIRO_HOME", "XDG_CONFIG_HOME", "NODE_OPTIONS"} {
			ctx.Cell["env"] = util.M{key: "synthetic-override"}
			wantErr(t, ValidateForCell(profile, ctx.Cell, h, ctx.Auth, nil), "conflicts")
		}
	}
	codex := newHarness("codex")
	ctx := f.ctx(codex, envAuth(util.M{"args": []any{"-c", "notify=['synthetic']"}}))
	wantErr(t, ValidateForCell(profile, ctx.Cell, codex, ctx.Auth, nil), "auth.args")
	ctx = f.ctx(codex, envAuth(util.M{"unset": []any{"HOME"}}))
	wantErr(t, ValidateForCell(profile, ctx.Cell, codex, ctx.Auth, nil), "auth.unset")
	ctx = f.ctx(codex, nil)
	ctx.Cell["permission_args"] = []any{"--enable", "plugins"}
	wantErr(t, ValidateForCell(profile, ctx.Cell, codex, ctx.Auth, nil), "permission_args")
}

func TestSupportedModelAndProviderArguments(t *testing.T) {
	f := newFixture(t)
	// CodexProvider's extra args for a custom endpoint and env-key auth.
	auth := newAuth("codex-provider", util.M{"args": []any{"-c", `model_provider="synthetic"`,
		"-c", `model_providers.synthetic.base_url="https://example.invalid"`,
		"-c", `model_providers.synthetic.env_key="SYNTHETIC_MODEL_KEY"`}}, true)
	codex := newHarness("codex")
	ctx := f.ctx(codex, auth)
	ctx.Cell["args"] = []any{"--model", "synthetic-model", "-c", `model_reasoning_effort="low"`}
	if err := ValidateForCell(f.profile("selected", nil), ctx.Cell, codex, auth, nil); err != nil {
		t.Fatal(err)
	}
}

func TestIncompatibleAuthAndStrictAbsenceRejected(t *testing.T) {
	f := newFixture(t)
	profile := f.profile("selected", nil)
	for _, pair := range []struct {
		h    plugin.Harness
		auth plugin.Auth
	}{{newHarness("claude-code"), newAuth("claude-subscription", nil, false)},
		{newHarness("kiro-cli"), newAuth("kiro-login", nil, false)}} {
		ctx := f.ctx(pair.h, pair.auth)
		wantErr(t, ValidateForCell(profile, ctx.Cell, pair.h, pair.auth, nil), "caller configuration")
	}
	kiro := newHarness("kiro-cli")
	ctx := f.ctx(kiro, envAuth(nil))
	wantErr(t, ValidateForCell(profile, ctx.Cell, kiro, ctx.Auth, nil), "KIRO_API_KEY")
	codex := newHarness("codex")
	ctx = f.ctx(codex, nil)
	wantErr(t, ValidateForCell(f.profile("selected", util.M{"require_absence": true}), ctx.Cell, codex, ctx.Auth, nil), "cannot hide")
	wantErr(t, ValidateForCell(profile, ctx.Cell, codex, ctx.Auth, util.M{"backend": "remote"}), "EnvironmentRunner")
	if err := ValidateForCell(profile, ctx.Cell, codex, ctx.Auth, util.M{"backend": "container"}); err != nil {
		t.Fatal(err)
	}
	claude := newHarness("claude-code")
	oauth := envAuth(util.M{"env": util.M{"CLAUDE_CODE_OAUTH_TOKEN": "${SYNTHETIC}"}})
	ctx = f.ctx(claude, oauth)
	wantErr(t, ValidateForCell(profile, ctx.Cell, claude, oauth, nil), "OAuth")
	if err := ValidateForCell(nil, ctx.Cell, claude, oauth, nil); err != nil {
		t.Fatal("nil profile must validate")
	}
	wantErr(t, ValidateForCell(profile, ctx.Cell, newHarness("generic"), oauth, nil), "unsupported for harness 'generic'")
}

func TestCodexCopiedAuthIsNotReadOrChanged(t *testing.T) {
	f := newFixture(t)
	codex := newHarness("codex")
	ctx := f.ctx(codex, newAuth("codex-chatgpt", nil, true))
	authFile := filepath.Join(ctx.ConfigDir, "auth.json")
	writeText(t, authFile, "synthetic-login-fixture")
	manifest, err := Prepare(f.profile("none", nil), ctx, codex)
	if err != nil {
		t.Fatal(err)
	}
	if readText(t, authFile) != "synthetic-login-fixture" || strings.Contains(util.PyJSON(manifest), "synthetic-login") {
		t.Fatal("copied auth must stay untouched and unrecorded")
	}
	// Another auth type may not leave files in config_dir.
	ctx = f.ctx(codex, nil)
	writeText(t, filepath.Join(ctx.ConfigDir, "auth.json"), "synthetic-login-fixture")
	_, err = Prepare(f.profile("none", nil), ctx, codex)
	wantErr(t, err, "fresh config_dir")
}

func TestManagedConfigDetectedWithoutReadingValues(t *testing.T) {
	f := newFixture(t)
	policy := filepath.Join(f.root, "managed-settings.json")
	writeText(t, policy, "Synthetic policy secret.")
	claude := newHarness("claude-code")
	ctx := f.ctx(claude, nil)
	systemPaths["claude-code"] = []string{policy}
	_, err := Prepare(f.profile("selected", nil), ctx, claude)
	wantErr(t, err, "admin configuration")
	if readText(t, policy) != "Synthetic policy secret." {
		t.Fatal("policy must be untouched")
	}
}

func TestFreshHomeAndConfigurationScopeRequired(t *testing.T) {
	f := newFixture(t)
	codex := newHarness("codex")
	ctx := f.ctx(codex, nil)
	writeText(t, filepath.Join(ctx.HomeDir, "ambient"), "Keep me.")
	_, err := Prepare(f.profile("selected", nil), ctx, codex)
	wantErr(t, err, "fresh empty")
	if readText(t, filepath.Join(ctx.HomeDir, "ambient")) != "Keep me." {
		t.Fatal("home content must be kept")
	}
	ctx = f.ctx(codex, nil)
	writeText(t, filepath.Join(ctx.ConfigDir, "config.toml"), "synthetic = true")
	_, err = Prepare(f.profile("selected", nil), ctx, codex)
	wantErr(t, err, "fresh config")
	ctx = f.ctx(codex, nil)
	ctx.HomeDir = ctx.ConfigDir
	_, err = Prepare(f.profile("selected", nil), ctx, codex)
	wantErr(t, err, "separate directories")
	ctx = f.ctx(codex, nil)
	ctx.CacheDir = ""
	_, err = Prepare(f.profile("selected", nil), ctx, codex)
	wantErr(t, err, "fresh absolute ctx.cache_dir")
	ctx = f.ctx(codex, nil)
	saved := homeFn
	homeFn = func() string { return ctx.HomeDir }
	_, err = Prepare(f.profile("selected", nil), ctx, codex)
	homeFn = saved
	wantErr(t, err, "caller's HOME")
}

func TestBootstrapDirectoriesCannotContainUnexpectedFiles(t *testing.T) {
	f := newFixture(t)
	codex := newHarness("codex")
	ctx := f.ctx(codex, nil)
	ctx.Runner = newEnvRunner("container")
	ctx.EnvironmentProfile = util.M{"backend": "container"}
	path := filepath.Join(ctx.HomeDir, ".local/bin/unexpected")
	writeText(t, path, "Do not change this file.")
	_, err := Prepare(f.profile("selected", nil), ctx, codex)
	wantErr(t, err, "bootstrap")
	if readText(t, path) != "Do not change this file." {
		t.Fatal("bootstrap file must be untouched")
	}
	ctx = f.ctx(codex, nil)
	ctx.Runner = newEnvRunner("container")
	ctx.EnvironmentProfile = util.M{"backend": "container"}
	os.MkdirAll(filepath.Join(ctx.HomeDir, ".config", "rogue"), 0o755)
	_, err = Prepare(f.profile("selected", nil), ctx, codex)
	wantErr(t, err, "unexpected configuration")
}

func TestRootCoordinatorAssignsCopiedFilesToContainerDirectoryOwner(t *testing.T) {
	f := newFixture(t)
	claude := newHarness("claude-code")
	ctx := f.ctx(claude, nil)
	ctx.Runner = newEnvRunner("container")
	ctx.EnvironmentProfile = util.M{"backend": "container"}
	savedProbe, savedEuid, savedChown := probeFn, geteuidFn, lchownFn
	defer func() { probeFn, geteuidFn, lchownFn = savedProbe, savedEuid, savedChown }()
	probeFn = probe
	geteuidFn = func() int { return 0 }
	var changed []string
	lchownFn = func(p string, uid, gid int) error {
		changed = append(changed, p)
		return nil
	}
	if _, err := Prepare(f.profile("selected", nil), ctx, claude); err != nil {
		t.Fatal(err)
	}
	var skill, settings bool
	for _, p := range changed {
		skill = skill || filepath.Base(p) == "SKILL.md"
		settings = settings || filepath.Base(p) == "settings.json"
		if !isRelativeTo(p, ctx.HomeDir) && !isRelativeTo(p, ctx.ConfigDir) {
			t.Fatalf("chown outside agent directories: %s", p)
		}
	}
	if !skill || !settings {
		t.Fatalf("changed %v", changed)
	}
}

func TestSetupCannotAddWorkspaceOrHomeDiscoverySources(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct{ h, rel string }{{"codex", ".agents/skills/rogue"}, {"claude-code", ".claude/skills/rogue"},
		{"kiro-cli", ".kiro/agents"}} {
		h := newHarness(c.h)
		ctx, _ := f.prepared(h, "selected", nil)
		os.MkdirAll(filepath.Join(ctx.Workspace, c.rel), 0o755)
		_, err := launch(h, ctx)
		wantErr(t, err, "workspace or ancestor")
	}
	for _, h := range allHarnesses() {
		ctx, _ := f.prepared(h, "none", nil)
		base := filepath.Join(ctx.ConfigDir, "skills")
		if h.Name() == "codex" {
			base = filepath.Join(ctx.HomeDir, ".agents/skills")
		}
		os.MkdirAll(filepath.Join(base, "rogue"), 0o755)
		_, err := launch(h, ctx)
		wantErr(t, err, "additional")
	}
	ctx, _ := f.prepared(newHarness("claude-code"), "none", nil)
	os.MkdirAll(filepath.Join(ctx.ConfigDir, "commands"), 0o755)
	_, err := launch(newHarness("claude-code"), ctx)
	wantErr(t, err, "additional harness configuration")
}

func TestMutatedSnapshotOrSettingsBlocksNarration(t *testing.T) {
	f := newFixture(t)
	for _, h := range allHarnesses() {
		ctx, manifest := f.prepared(h, "selected", nil)
		path := containedPath(util.S(firstEntry(manifest)["location"]), ctx) + "/SKILL.md"
		writeText(t, path, "Changed by synthetic worker.")
		_, err := launch(h, ctx)
		wantErr(t, err, "changed")
	}
	claude := newHarness("claude-code")
	ctx, _ := f.prepared(claude, "selected", nil)
	writeText(t, filepath.Join(ctx.ConfigDir, "settings.json"), `{"disableAllHooks":false}`)
	_, err := launch(claude, ctx)
	wantErr(t, err, "configuration changed")
}

func TestInstalledParentSymlinkReplacementRejected(t *testing.T) {
	f := newFixture(t)
	claude := newHarness("claude-code")
	ctx, _ := f.prepared(claude, "selected", nil)
	skillRoot := filepath.Join(ctx.ConfigDir, "skills")
	saved := filepath.Join(ctx.ConfigDir, "saved-skills")
	os.Rename(skillRoot, saved)
	os.Symlink(saved, skillRoot)
	_, err := launch(claude, ctx)
	wantErr(t, err, "symlink")
}

func TestInheritedControlsAreRemovedOnlyForProfiles(t *testing.T) {
	f := newFixture(t)
	claude := newHarness("claude-code")
	ctx, _ := f.prepared(claude, "none", nil)
	injected := map[string]string{"CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD": "1", "NODE_OPTIONS": "--synthetic",
		"CODEX_HOME": "synthetic", "KIRO_HOME": "synthetic", "CLAUDE_CODE_USE_BEDROCK": "1"}
	for k, v := range injected {
		t.Setenv(k, v)
	}
	l := mustLaunch(t, claude, ctx)
	for name := range injected {
		if name != "CLAUDE_CODE_USE_BEDROCK" && !contains(l.ctx.UnsetEnv, name) {
			t.Fatalf("%s should be dropped: %v", name, l.ctx.UnsetEnv)
		}
	}
	if contains(l.ctx.UnsetEnv, "CLAUDE_CODE_USE_BEDROCK") || !contains(l.ctx.UnsetEnv, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("provider variables: %v", l.ctx.UnsetEnv)
	}
	if len(ctx.UnsetEnv) != 0 {
		t.Fatal("the caller's ctx must not change")
	}
}

func TestPlainLaunchesPreserveLegacyConfiguration(t *testing.T) {
	f := newFixture(t)
	for _, h := range allHarnesses() {
		ctx := f.ctx(h, nil)
		delete(ctx.Cell, "agent_configuration")
		ctx.Cell["args"] = []any{"--synthetic-legacy-option"}
		l := mustLaunch(t, h, ctx)
		if !contains(l.argv, "--synthetic-legacy-option") || len(l.env) != 0 || l.ctx != ctx {
			t.Fatalf("%s plain launch %v %v", h.Name(), l.argv, l.env)
		}
		if env, err := LaunchEnv(ctx, h); err != nil || len(env) != 0 {
			t.Fatal("no profile env expected")
		}
		if h.Name() == "kiro-cli" && l.argv[0] != "kiro-cli" {
			t.Fatalf("kiro binary %q", l.argv[0])
		}
	}
	// Selected but never prepared is an error, not a silent legacy launch.
	codex := newHarness("codex")
	ctx := f.ctx(codex, nil)
	_, err := LaunchArgs(ctx, codex)
	wantErr(t, err, "selected but not prepared")
}

func TestRuntimeEnvConflictsAreRejected(t *testing.T) {
	f := newFixture(t)
	codex := newHarness("codex")
	ctx := f.ctx(codex, nil)
	ctx.Spec["env"] = util.M{"CODEX_HOME": "synthetic"}
	_, err := Prepare(f.profile("selected", nil), ctx, codex)
	wantErr(t, err, "trial.env")
	ctx, _ = f.prepared(codex, "selected", nil)
	ctx.Env["CODEX_HOME"] = "synthetic"
	_, err = launch(codex, ctx)
	wantErr(t, err, "worker env")
	ctx, _ = f.prepared(codex, "selected", nil)
	ctx.Env["CODEX_HOME"] = ctx.ConfigDir // the prepared value itself is fine
	mustLaunch(t, codex, ctx)
	ctx.UnsetEnv = []string{"XDG_*"}
	_, err = launch(codex, ctx)
	wantErr(t, err, "removes profile controls")
	ctx, _ = f.prepared(codex, "selected", nil)
	_, err = LaunchEnv(ctx, newHarness("claude-code"))
	wantErr(t, err, "different harness")
}

type localCall struct {
	argv []string
	cwd  string
	env  map[string]string
}

func TestLocalProbesUseOnlyHelpVersionAndConfigParseWithScratchEnv(t *testing.T) {
	f := newFixture(t)
	probeFn = probe
	codex := newHarness("codex")
	ctx := f.ctx(codex, nil)
	var commands []localCall
	savedWhich, savedRun := whichFn, runLocalFn
	defer func() { whichFn, runLocalFn = savedWhich, savedRun }()
	whichFn = func(string, string) string { return "/synthetic/bin/codex" }
	runLocalFn = func(_ context.Context, argv []string, cwd string, env map[string]string, _ time.Duration) (string, int, error) {
		commands = append(commands, localCall{argv, cwd, env})
		switch {
		case len(argv) == 2 && argv[1] == "--version":
			return "codex-cli 0.160.1", 0, nil
		case len(argv) == 3 && argv[1] == "exec" && argv[2] == "--help":
			return "--strict-config --config --disable", 0, nil
		}
		if strings.Join(argv[len(argv)-2:], " ") != "features list" {
			t.Fatalf("unexpected probe %v", argv)
		}
		return fakeFeatures(), 0, nil
	}
	result := probe("codex", ctx)
	if result["provider_called"] != false || result["binary"] != "/synthetic/bin/codex" || result["version"] != "0.160.1" ||
		result["method"] != "local --version/--help" || result["feature_overrides_checked"] != true {
		t.Fatalf("result %v", result)
	}
	if len(commands) != 3 {
		t.Fatalf("commands %v", commands)
	}
	for _, c := range commands {
		if !strings.HasPrefix(c.cwd, ctx.CacheDir) || c.env["HOME"] == ctx.HomeDir || c.env["CODEX_HOME"] == ctx.ConfigDir {
			t.Fatalf("probe env %v cwd %s", c.env, c.cwd)
		}
		if _, ok := c.env["OPENAI_API_KEY"]; ok || contains(c.argv, "login") {
			t.Fatal("probe must not carry credentials or log in")
		}
		if c.env["PATH"] != "/synthetic/bin:/usr/bin:/bin" || c.env["TERM"] != "dumb" {
			t.Fatalf("probe PATH %q", c.env["PATH"])
		}
	}
	features := commands[2].argv
	if indexOf(features, "features.plugins=false") < 0 || indexOf(features, "notify=[]") < 0 ||
		indexOf(features, `cli_auth_credentials_store="file"`) < 0 {
		t.Fatalf("features argv %v", features)
	}
	// Disabled features must really read false.
	runLocalFn = func(_ context.Context, argv []string, _ string, _ map[string]string, _ time.Duration) (string, int, error) {
		switch {
		case argv[1] == "--version":
			return "0.160.1", 0, nil
		case argv[1] == "exec":
			return "--strict-config --config --disable", 0, nil
		}
		return "plugins stable true\nhooks stable false\napps stable false\nskill_mcp_dependency_install stable false", 0, nil
	}
	_, err := func() (_ util.M, err error) { defer catch(&err); return probe("codex", ctx), nil }()
	wantErr(t, err, "cannot verify disabled feature plugins")
	runLocalFn = func(context.Context, []string, string, map[string]string, time.Duration) (string, int, error) {
		return "", -1, errors.New("exec: not found")
	}
	_, err = func() (_ util.M, err error) { defer catch(&err); return probe("codex", ctx), nil }()
	wantErr(t, err, "local capability check failed; no worker was launched")
}

func TestNoneAndSelectedInsideContainerBackend(t *testing.T) {
	f := newFixture(t)
	probeFn = probe
	savedWhich := whichFn
	defer func() { whichFn = savedWhich }()
	whichFn = func(string, string) string { panic("container must resolve internally") }
	for _, h := range allHarnesses() {
		for _, mode := range []string{"none", "selected"} {
			ctx := f.ctx(h, nil)
			backend := newEnvRunner("container")
			ctx.Runner = backend
			ctx.EnvironmentProfile = util.M{"backend": "container"}
			ctx.Env = util.MergeEnv(backend.EnvironmentEnv(ctx), map[string]string{"SYNTHETIC_PROVIDER_KEY": "do-not-forward"})
			for _, sub := range []string{".local/bin", ".local/share", ".local/state"} {
				os.MkdirAll(filepath.Join(ctx.HomeDir, sub), 0o755)
			}
			manifest, err := Prepare(f.profile(mode, nil), ctx, h)
			if err != nil {
				t.Fatalf("%s %s: %v", h.Name(), mode, err)
			}
			ctx.State["agent_configuration"] = manifest
			l := mustLaunch(t, h, ctx)
			binary := map[string]string{"codex": "codex", "claude-code": "claude", "kiro-cli": "kiro-cli-chat"}[h.Name()]
			if l.argv[0] != "/container/bin/"+binary || manifest["backend"] != "container" {
				t.Fatalf("%s binary %q", h.Name(), l.argv[0])
			}
			if l.env["XDG_CONFIG_HOME"] != ctx.ConfigDir || l.env["BASH_ENV"] != "" || l.env["ENV"] != "" {
				t.Fatalf("env %v", l.env)
			}
			if _, has := l.env["BASH_ENV"]; has {
				t.Fatal("BASH_ENV")
			}
			if l.ctx != ctx || len(l.ctx.UnsetEnv) != 0 {
				t.Fatal("managed runners keep their ctx")
			}
			for _, line := range util.Strings(manifest["limitations"]) {
				if strings.Contains(line, "Local isolation") {
					t.Fatal("container manifest must not claim local isolation")
				}
			}
			resolvedInside := false
			for _, call := range backend.calls {
				resolvedInside = resolvedInside || call.command == "command -v "+binary
				if call.ctx.Auth != nil {
					t.Fatal("probe must not carry auth")
				}
				if _, ok := call.ctx.Env["SYNTHETIC_PROVIDER_KEY"]; ok {
					t.Fatal("probe ctx env carries the provider key")
				}
				if _, ok := call.env["SYNTHETIC_PROVIDER_KEY"]; ok {
					t.Fatal("probe env carries the provider key")
				}
				if call.ctx.Env["HOME"] != ctx.HomeDir {
					t.Fatal("probe HOME")
				}
			}
			if !resolvedInside {
				t.Fatal("binary must be resolved inside the container")
			}
			caps := util.MapOr(manifest["capabilities"])
			if caps["method"] != "backend --version/--help" || caps["backend"] != "container" {
				t.Fatalf("capabilities %v", caps)
			}
			narration := mustLaunch(t, h, ctx)
			if narration.argv[0] != "/container/bin/"+binary {
				t.Fatal("narration binary")
			}
		}
	}
}

func TestContainerAdminScopeFailureIsDetectedInContainer(t *testing.T) {
	f := newFixture(t)
	probeFn = probe
	codex := newHarness("codex")
	ctx := f.ctx(codex, nil)
	backend := newEnvRunner("container")
	backend.scopeViolation = "admin-configuration"
	ctx.Runner = backend
	ctx.EnvironmentProfile = util.M{"backend": "container"}
	systemPaths["codex"] = []string{"/synthetic/admin-policy"}
	_, err := Prepare(f.profile("selected", nil), ctx, codex)
	wantErr(t, err, "admin configuration")
	if ctx.AgentConfiguration != nil {
		t.Fatal("no manifest on failure")
	}
	if !strings.Contains(backend.calls[0].command, "'/synthetic/admin-policy'") &&
		!strings.Contains(backend.calls[0].command, "/synthetic/admin-policy") {
		t.Fatal("system path must be checked inside the backend")
	}
	backend.scopeViolation = "not-a-label"
	_, err = Prepare(f.profile("selected", nil), f.ctxWith(codex, backend), codex)
	wantErr(t, err, "invalid discovery-scope response")
}

func (f *fixture) ctxWith(h plugin.Harness, r plugin.Runner) *plugin.Ctx {
	ctx := f.ctx(h, nil)
	ctx.Runner = r
	ctx.EnvironmentProfile = util.M{"backend": r.Name()}
	return ctx
}

func TestLocalBackendDiscoversFromCallerPathAndPersistsBinary(t *testing.T) {
	f := newFixture(t)
	probeFn = probe
	codex := newHarness("codex")
	ctx := f.ctx(codex, nil)
	backend := newEnvRunner("local")
	ctx.Runner = backend
	ctx.EnvironmentProfile = util.M{"backend": "local"}
	ctx.Env = backend.EnvironmentEnv(ctx)
	savedWhich := whichFn
	defer func() { whichFn = savedWhich }()
	var seenPath string
	whichFn = func(_ string, path string) string {
		seenPath = path
		return "/trusted/install/codex"
	}
	manifest, err := Prepare(f.profile("selected", nil), ctx, codex)
	if err != nil {
		t.Fatal(err)
	}
	if seenPath != os.Getenv("PATH") || seenPath == ctx.Env["PATH"] {
		t.Fatalf("which PATH %q", seenPath)
	}
	if util.Get(manifest, "launch", "binary") != "/trusted/install/codex" {
		t.Fatal("binary must be persisted")
	}
	if mustLaunch(t, codex, ctx).argv[0] != "/trusted/install/codex" {
		t.Fatal("launch binary")
	}
}

func TestUnsupportedCliCapabilityIsPreflightFailure(t *testing.T) {
	f := newFixture(t)
	probeFn = probe
	claude := newHarness("claude-code")
	ctx := f.ctx(claude, nil)
	savedWhich, savedRun := whichFn, runLocalFn
	defer func() { whichFn, runLocalFn = savedWhich, savedRun }()
	whichFn = func(string, string) string { return "/synthetic/bin/claude" }
	runLocalFn = func(context.Context, []string, string, map[string]string, time.Duration) (string, int, error) {
		return "2.0.0", 0, nil
	}
	_, err := Prepare(f.profile("selected", nil), ctx, claude)
	wantErr(t, err, "require version")
	if ctx.AgentConfiguration != nil {
		t.Fatal("no manifest on failure")
	}
	whichFn = func(string, string) string { return "" }
	_, err = Prepare(f.profile("selected", nil), f.ctx(claude, nil), claude)
	wantErr(t, err, "no supported CLI executable in the local backend")
}

func TestNonLocalLegacyRunnerIsRejected(t *testing.T) {
	f := newFixture(t)
	codex := newHarness("codex")
	ctx := f.ctx(codex, nil)
	ctx.Runner = &plugin.RunnerBase{PluginName: "docker"}
	_, err := Prepare(f.profile("selected", nil), ctx, codex)
	wantErr(t, err, "local or container EnvironmentRunner backend")
	ctx = f.ctx(codex, nil)
	ctx.Cell["runner"] = util.M{"type": "docker"}
	wantErr(t, ValidateForCell(f.profile("selected", nil), ctx.Cell, codex, ctx.Auth, nil), "legacy Docker/wrapper runner")
}
