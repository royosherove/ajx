package agentconfig

// Side-by-side parity with skills/ajx/lib/ajx/agent_configuration.py: the
// same synthetic skill trees, SKILL.md frontmatter, argv lists, encodings and
// prepared manifests must give identical hashes, records, messages and bytes.
// Skipped when python3 is unavailable. No agent or provider is called: both
// sides use a fake capability probe.

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

const parityScript = `
import json, shlex, sys, types
from pathlib import Path
from unittest import mock
sys.dont_write_bytecode = True
sys.path.insert(0, sys.argv[1])
from ajx import agent_configuration as ac
from ajx.builtin.generic import EnvAuth, LocalRunner

req = json.loads(sys.stdin.read())
dumps = lambda v: json.dumps(v, sort_keys=True, ensure_ascii=True, separators=(",", ":"))
out = {}
profiles = ac.normalize_profiles(req["profiles"], req["base"])
out["profiles"] = dumps(profiles)
out["contracts"] = dumps({k: ac.resume_contract(v) for k, v in profiles.items()})
out["trees"] = [dumps({k: v for k, v in ac._read_tree(p).items() if k != "_content"}) for p in req["trees"]]

def attempt(fn):
    try:
        return fn()
    except ValueError as exc:
        return "ERR " + str(exc)

out["frontmatter"] = [attempt(lambda: dumps(ac._frontmatter(t.encode(), "product-guide"))) for t in req["frontmatter"]]
out["args"] = [attempt(lambda: ac._validate_args(argv, h, "cell.args") or "OK") for h, argv in req["args"]]
out["toml"] = [ac._toml(v) for v in req["toml"]]
out["dumps"] = [json.dumps(v, sort_keys=True, indent=2) + "\n" for v in req["dumps"]]
out["quote"] = [shlex.quote(s) for s in req["quote"]]
out["repr"] = [repr(s) for s in req["repr"]]
out["fnmatch"] = [__import__("fnmatch").fnmatch(n, p) for n, p in req["fnmatch"]]
out["normalize_errors"] = [attempt(lambda: dumps(ac.normalize_profiles(r, req["base"]))) for r in req["bad_profiles"]]

def fake_probe(name, ctx):
    return {"binary": "/synthetic/bin/" + name, "version": "synthetic", "provider_called": False,
            "live_extension_activation_verified": False}

prepared = []
with mock.patch.object(ac, "_SYSTEM_PATHS", {k: () for k in ac._HARNESSES}), \
        mock.patch.object(ac, "_probe", side_effect=fake_probe):
    for case in req["prepare"]:
        dirs = {k: Path(v) for k, v in case["dirs"].items()}
        harness = types.SimpleNamespace(name=case["harness"], binary=case["harness"])
        auth = EnvAuth({"required_env": ["KIRO_API_KEY"]}) if case["harness"] == "kiro-cli" else EnvAuth()
        ctx = {**dirs, "cell": {"id": "parity", "harness": case["harness"], "config": "clean",
                                "agent_configuration": case["profile"], "args": [], "env": {}},
               "spec": {"trial": {}, "env": {}}, "env": {}, "auth": auth, "runner": LocalRunner(),
               "state": {}, "unset_env": []}
        manifest = ac.prepare(profiles[case["profile"]], ctx, harness)
        files = {e["location"]: Path(ac._expand(e["location"], ctx)).read_text() for e in manifest["configuration_files"]}
        prepared.append({"manifest": dumps(manifest), "args": ac.launch_args(ctx, harness),
                         "env": ac.launch_env(ctx, harness), "files": files,
                         "installed": [dumps({k: v for k, v in ac._read_tree(ac._contained_path(s["location"], ctx)).items()
                                              if k not in ("_content", "_identity")})
                                       for s in manifest["skills"]["entries"]]})
out["prepare"] = prepared
print(json.dumps(out))
`

type parityRequest struct {
	Profiles    util.M          `json:"profiles"`
	Base        string          `json:"base"`
	Trees       []string        `json:"trees"`
	Frontmatter []string        `json:"frontmatter"`
	Args        [][2]any        `json:"args"`
	Toml        []any           `json:"toml"`
	Dumps       []any           `json:"dumps"`
	Quote       []string        `json:"quote"`
	Repr        []string        `json:"repr"`
	Fnmatch     [][2]string     `json:"fnmatch"`
	BadProfiles []util.M        `json:"bad_profiles"`
	Prepare     []parityPrepare `json:"prepare"`
}

type parityPrepare struct {
	Harness string            `json:"harness"`
	Profile string            `json:"profile"`
	Dirs    map[string]string `json:"dirs"`
}

type parityResult struct {
	Profiles        string   `json:"profiles"`
	Contracts       string   `json:"contracts"`
	Trees           []string `json:"trees"`
	Frontmatter     []string `json:"frontmatter"`
	Args            []string `json:"args"`
	Toml            []string `json:"toml"`
	Dumps           []string `json:"dumps"`
	Quote           []string `json:"quote"`
	Repr            []string `json:"repr"`
	Fnmatch         []bool   `json:"fnmatch"`
	NormalizeErrors []string `json:"normalize_errors"`
	Prepare         []struct {
		Manifest  string            `json:"manifest"`
		Args      []string          `json:"args"`
		Env       map[string]string `json:"env"`
		Files     map[string]string `json:"files"`
		Installed []string          `json:"installed"`
	} `json:"prepare"`
}

func canonical(v any) string { return pyDumps(v, jsonOpts{ascii: true, compact: true, sorted: true}) }

func runPython(t *testing.T, req parityRequest) parityResult {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	lib, err := filepath.Abs("../../skills/ajx/lib")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(lib, "ajx", "agent_configuration.py")); err != nil {
		t.Skip("Python source tree not available")
	}
	input, _ := json.Marshal(req)
	cmd := exec.Command(python, "-I", "-c", parityScript, lib)
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python parity helper failed: %v\n%s", err, stderr.String())
	}
	var res parityResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("python output: %v\n%s", err, out)
	}
	return res
}

func writeTree(t *testing.T, root string, files map[string]string, executable ...string) {
	t.Helper()
	for rel, text := range files {
		writeText(t, filepath.Join(root, rel), text)
	}
	for _, rel := range executable {
		if err := os.Chmod(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func goAttempt(fn func() string) (s string) {
	defer func() {
		if r := recover(); r != nil {
			if f, ok := r.(failure); ok {
				s = "ERR " + f.err.Error()
				return
			}
			panic(r)
		}
	}()
	return fn()
}

func TestParityWithPython(t *testing.T) {
	base := realTempDir(t)
	rich := filepath.Join(base, "skills", "parity-skill")
	writeTree(t, rich, map[string]string{
		"SKILL.md": "---\nname: parity-skill\ndescription: >\n  Multi-line synthetic description.\n" +
			"metadata:\n  author: synthetic\n# a comment\nallowed-tools: \"Bash(git add:*), Read Grep\"\n" +
			"version: 0.1.0\nlicense: MIT\nuser-invocable: false\n---\n" +
			"See [guide](assets/naïve.txt), [日本](docs/%E6%97%A5%E6%9C%AC%E8%AA%9E.md) and [top](#top).\n" +
			"![img](<assets/a-b.txt>)\n[web](https://example.invalid/x)\n\n[ref]: assets/naïve.txt\n",
		"assets/naïve.txt":   "Fictional ü content.\n",
		"assets/a-b.txt":     "dash\n",
		"assets/a/inner.txt": "nested\n",
		"assets/.gitkeep":    "",
		"docs/日本語.md":        "[back](../SKILL.md) [self](日本語.md)\n",
		"README.md":          "Readme.\n",
		"scripts/run.sh":     "#!/bin/sh\nexit 0\n",
		"Zeta.txt":           "upper\n",
	}, "scripts/run.sh")
	plain := filepath.Join(base, "skills", "product-guide")
	writeTree(t, plain, map[string]string{"SKILL.md": skillText, "assets/guide.txt": "Fictional reference material.\n",
		"scripts/check.sh": "#!/bin/sh\nprintf 'synthetic\\n'\n"}, "scripts/check.sh")

	header := "---\nname: product-guide\n%s\ndescription: d\n---\n"
	fm := func(field string) string { return strings.Replace(header, "%s", field, 1) }
	frontmatters := []string{
		skillText, fm("allowed-tools: \"Bash(git add:*), Read,Grep\""), fm("version: '2'"), fm("allowed-tools:"),
		fm("allowed-tools: Read Grep Write"), fm("allowed-tools: Bash(aws:*"), fm("allowed-tools: Read)"),
		fm("allowed-tools: Read # comment"), fm("allowed-tools: Bash('#')"), fm("allowed-tools: Bash(\x07)"),
		fm("allowed-tools: " + strings.Repeat("x", 300)), fm("version: \x7f"), fm("version: \"1.0'"),
		fm("hooks: {}"), fm("name: other"), fm("license: [MIT]"), fm("license: &anchor"), fm("license:MIT"),
		fm("disable-model-invocation: yes"), fm("disable-model-invocation: true\nuser-invocable: true"),
		fm("license: MIT\n  nested: no"), fm("metadata:\n  nested: yes"), "---\nname: product-guide\ndescription: d\n",
		"no frontmatter\n", "---\r\nname: product-guide\r\ndescription: crlf\r\n---\r\n",
		"---\nname: product--guide\ndescription: d\n---\n", "---\nname: product-guide-\ndescription: d\n---\n",
		"---\nname: product-guide\ndescription:\n---\n", "---\nname: 'product-guide'\ndescription: \"q\"\n---\n",
		"---\nname: product-guide\ndescription: v\x0btab\n---\n", "---\n name: product-guide\ndescription: d\n---\n",
		fm("license: MIT\nlicense: MIT"), fm("version: " + strings.Repeat("9", 257)),
	}
	argCases := [][2]any{
		{"codex", []any{"-c", "model=x"}}, {"codex", []any{"-cmodel=x"}}, {"codex", []any{"--config=model_providers.a-b.c_d=1"}},
		{"codex", []any{"-c"}}, {"codex", []any{"--model"}}, {"codex", []any{"--model", "-x"}}, {"codex", []any{"--weird=1"}},
		{"codex", []any{"--" + strings.Repeat("x", 70)}}, {"codex", []any{"positional"}}, {"codex", []any{"--naïve"}},
		{"codex", []any{"--sandbox=read-only", "--skip-git-repo-check"}}, {"codex", []any{"--skip-git-repo-check=1"}},
		{"codex", []any{"-c", " model =x"}}, {"codex", []any{"--config", "notify=[]"}}, {"codex", []any{"-c", "model"}},
		{"claude-code", []any{"--effort", "low"}}, {"claude-code", []any{"--dangerously-skip-permissions"}},
		{"claude-code", []any{"--permission-mode=plan", "--max-budget-usd", "1"}}, {"claude-code", []any{"-c", "model=x"}},
		{"kiro-cli", []any{"--trust-tools", "--x"}}, {"kiro-cli", []any{"--trust-all-tools"}},
		{"kiro-cli", []any{"--no-interactive=true"}}, {"kiro-cli", []any{"--trust-tools=fs_write", "--effort=low"}},
	}
	tomls := []any{[]any{util.M{"path": "/a/ü/SKILL.md", "enabled": true}}, []any{}, false, "file", "naïve\"quote\\",
		[]any{"x", []any{true}}}
	dumpCases := []any{
		util.M{"name": "ajx-profile", "resources": []any{"skill:///tmp/ü/日本/SKILL.md", "skill://\U0001F600"},
			"hooks": util.M{}, "allowedTools": []any{}, "includeMcpJson": false, "tools": []any{"@builtin"},
			"mcpServers": util.M{}, "z": "\x7f\t\"<>& "},
		claudeSettings(),
	}
	quotes := []string{"", "abc", "a b", "it's", "/path/ü", "a$b", "--x=1", "/usr/bin/codex", "@%+=:,./-_"}
	reprs := []string{"plain", "it's", "both ' and \"", "tab\t", "\x07bell", "ü", " ", "\U0001F600", `back\slash`,
		"\u200b", "\x7f", "\ufeff"}
	fnmatches := [][2]string{{"HOME", "HOME"}, {"XDG_CONFIG_HOME", "XDG_*"}, {"HOME", "H?ME"}, {"HOME", "[!H]OME"},
		{"HOME", "[A-Z]OME"}, {"HOME", "[HX]*"}, {"KIRO_HOME", "*_HOME"}, {"HOME", "[H"}, {"[H", "[H"},
		{"CODEX_HOME", "codex_home"}, {"HOME", "*"}, {"A]", "[]]*"}}
	badProfiles := []util.M{
		{"Bad": util.M{}}, {"x": "nope"}, {"x": util.M{"unknown": 1, "also": 2}}, {"x": util.M{"require_absence": "yes"}},
		{"x": util.M{"skills": "everything"}}, {"x": util.M{"skills": util.M{"mode": "selected"}}},
		{"x": util.M{"skills": util.M{"mode": "selected", "paths": []any{"  "}}}},
		{"x": util.M{"skills": util.M{"mode": "selected", "paths": []any{"skills/*"}}}},
		{"x": util.M{"skills": util.M{"mode": "selected", "paths": []any{"skills/missing"}}}},
		{"x": util.M{"skills": util.M{"mode": "selected", "paths": []any{"skills/product-guide"}, "pins": util.M{"other": strings.Repeat("a", 64)}}}},
		{"x": util.M{"skills": util.M{"mode": "selected", "paths": []any{"skills/product-guide"}, "pins": util.M{"product-guide": "short"}}}},
		{"x": util.M{"skills": util.M{"mode": "none", "pins": util.M{}, "extra": 1}}},
		{"x": util.M{"plugins": util.M{"mode": "selected", "paths": []any{"p"}}}},
		{"x": util.M{"hooks": "snapshot"}},
		{"x": util.M{"skills": util.M{"mode": "selected", "paths": []any{"skills"}}}},
	}

	mkDirs := func(prefix string) map[string]string {
		dirs := map[string]string{}
		for _, k := range []string{"workspace", "config_dir", "cache_dir", "home_dir"} {
			dirs[k] = filepath.Join(base, prefix, k)
			os.MkdirAll(dirs[k], 0o755)
		}
		return dirs
	}
	var pyCases, goCases []parityPrepare
	for i, c := range [][2]string{{"codex", "plain"}, {"claude-code", "plain"}, {"kiro-cli", "plain"},
		{"claude-code", "rich"}, {"codex", "none"}, {"claude-code", "none"}, {"kiro-cli", "none"}} {
		id := c[0] + "-" + c[1] + "-" + itoa(i)
		pyCases = append(pyCases, parityPrepare{c[0], c[1], mkDirs("py-" + id)})
		goCases = append(goCases, parityPrepare{c[0], c[1], mkDirs("go-" + id)})
	}
	rawProfiles := util.M{
		"rich":  util.M{"skills": util.M{"mode": "selected", "paths": []any{"skills/parity-skill"}}},
		"plain": util.M{"skills": util.M{"mode": "snapshot", "paths": []any{plain}}, "plugins": "none", "hooks": util.M{}},
		"none":  util.M{"require_absence": false},
	}
	// Python receives the table with its real insertion order (Go's encoder sorts keys).
	pyTomls := append([]any{json.RawMessage(`[{"path": "/a/\u00fc/SKILL.md", "enabled": true}]`)}, tomls[1:]...)
	req := parityRequest{Profiles: rawProfiles, Base: base, Trees: []string{rich, plain}, Frontmatter: frontmatters,
		Args: argCases, Toml: pyTomls, Dumps: dumpCases, Quote: quotes, Repr: reprs, Fnmatch: fnmatches,
		BadProfiles: badProfiles, Prepare: pyCases}
	py := runPython(t, req)

	t.Logf("python frontmatter results: %s", strings.Join(py.Frontmatter, " | "))
	t.Logf("python args results: %s", strings.Join(py.Args, " | "))
	t.Logf("python normalize results: %s", strings.Join(py.NormalizeErrors, " | "))
	profiles, err := NormalizeProfiles(rawProfiles, base)
	if err != nil {
		t.Fatal(err)
	}
	if got := canonical(profiles); got != py.Profiles {
		t.Errorf("normalized profiles differ (hashes, records or identities)\ngo: %s\npy: %s", got, py.Profiles)
	}
	contracts := util.M{}
	for k, v := range profiles {
		contracts[k] = ResumeContract(util.Map(v))
	}
	if got := canonical(contracts); got != py.Contracts {
		t.Errorf("resume contracts differ\ngo: %s\npy: %s", got, py.Contracts)
	}
	for i, p := range req.Trees {
		tr := readTree(p)
		got := canonical(util.M{"sha256": tr.sha256, "files": tr.records(), "bytes": tr.bytes, "_identity": tr.identity})
		if got != py.Trees[i] {
			t.Errorf("tree %s differs\ngo: %s\npy: %s", p, got, py.Trees[i])
		}
	}
	for i, text := range frontmatters {
		got := goAttempt(func() string { return canonical(frontmatter([]byte(text), "product-guide")) })
		if got != py.Frontmatter[i] {
			t.Errorf("frontmatter %q\ngo: %s\npy: %s", text, got, py.Frontmatter[i])
		}
	}
	for i, c := range argCases {
		got := goAttempt(func() string { validateArgs(c[1], c[0].(string), "cell.args"); return "OK" })
		if got != py.Args[i] {
			t.Errorf("args %v\ngo: %s\npy: %s", c, got, py.Args[i])
		}
	}
	for i, v := range tomls {
		if got := toml(v); got != py.Toml[i] {
			t.Errorf("toml %v\ngo: %s\npy: %s", v, got, py.Toml[i])
		}
	}
	for i, v := range dumpCases {
		if got := pyDumps(v, jsonOpts{ascii: true, indent: 2, sorted: true}) + "\n"; got != py.Dumps[i] {
			t.Errorf("json dumps\ngo: %s\npy: %s", got, py.Dumps[i])
		}
	}
	for i, s := range quotes {
		if got := util.ShellQuote(s); got != py.Quote[i] {
			t.Errorf("shlex.quote(%q) go %s py %s", s, got, py.Quote[i])
		}
	}
	for i, s := range reprs {
		if got := pyRepr(s); got != py.Repr[i] {
			t.Errorf("repr(%q) go %s py %s", s, got, py.Repr[i])
		}
	}
	for i, c := range fnmatches {
		if got := fnmatch(c[0], c[1]); got != py.Fnmatch[i] {
			t.Errorf("fnmatch(%q, %q) go %v py %v", c[0], c[1], got, py.Fnmatch[i])
		}
	}
	for i, raw := range badProfiles {
		got := "OK"
		out, err := NormalizeProfiles(raw, base)
		if err != nil {
			got = "ERR " + err.Error()
		} else {
			got = canonical(out)
		}
		if got != py.NormalizeErrors[i] {
			t.Errorf("normalize %v\ngo: %s\npy: %s", raw, got, py.NormalizeErrors[i])
		}
	}

	savedSystem, savedProbe := systemPaths, probeFn
	defer func() { systemPaths, probeFn = savedSystem, savedProbe }()
	systemPaths = map[string][]string{"codex": nil, "claude-code": nil, "kiro-cli": nil}
	probeFn = func(name string, ctx *plugin.Ctx) util.M {
		return util.M{"binary": "/synthetic/bin/" + name, "version": "synthetic", "provider_called": false,
			"live_extension_activation_verified": false}
	}
	for i, c := range goCases {
		h := newHarness(c.Harness)
		auth := envAuth(nil)
		if c.Harness == "kiro-cli" {
			auth = envAuth(util.M{"required_env": []any{"KIRO_API_KEY"}})
		}
		ctx := &plugin.Ctx{Workspace: c.Dirs["workspace"], ConfigDir: c.Dirs["config_dir"], CacheDir: c.Dirs["cache_dir"],
			HomeDir: c.Dirs["home_dir"], Cell: util.M{"id": "parity", "harness": c.Harness, "config": "clean",
				"agent_configuration": c.Profile, "args": []any{}, "env": util.M{}},
			Spec: util.M{"trial": util.M{}, "env": util.M{}}, Env: map[string]string{}, Auth: auth, Runner: localRunner(),
			State: util.M{}, UnsetEnv: []string{}}
		manifest, err := Prepare(util.Map(profiles[c.Profile]), ctx, h)
		if err != nil {
			t.Fatalf("%s/%s: %v", c.Harness, c.Profile, err)
		}
		want := py.Prepare[i]
		pyBase := filepath.Dir(pyCases[i].Dirs["workspace"])
		goBase := filepath.Dir(c.Dirs["workspace"])
		wantManifest := want.Manifest
		for location, text := range want.Files {
			goText := readText(t, expandStr(location, ctx))
			if strings.ReplaceAll(text, pyBase, goBase) != goText {
				t.Errorf("%s/%s %s differs\ngo: %s\npy: %s", c.Harness, c.Profile, location, goText, text)
			}
			wantManifest = strings.ReplaceAll(wantManifest, util.Sha256Bytes([]byte(text)), util.Sha256Bytes([]byte(goText)))
		}
		if got := canonical(manifest); got != wantManifest {
			t.Errorf("%s/%s manifest differs\ngo: %s\npy: %s", c.Harness, c.Profile, got, wantManifest)
		}
		args, err := LaunchArgs(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		if got, wantArgs := strings.Join(args, "\n"), strings.ReplaceAll(strings.Join(want.Args, "\n"), pyBase, goBase); got != wantArgs {
			t.Errorf("%s/%s launch args\ngo: %s\npy: %s", c.Harness, c.Profile, got, wantArgs)
		}
		env, err := LaunchEnv(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		if got, wantEnv := canonical(env), strings.ReplaceAll(canonical(want.Env), pyBase, goBase); got != wantEnv {
			t.Errorf("%s/%s launch env\ngo: %s\npy: %s", c.Harness, c.Profile, got, wantEnv)
		}
		for j, s := range util.Maps(util.Get(manifest, "skills", "entries")) {
			tr := readTree(containedPath(util.S(s["location"]), ctx))
			if got := canonical(util.M{"sha256": tr.sha256, "files": tr.records(), "bytes": tr.bytes}); got != want.Installed[j] {
				t.Errorf("%s/%s installed snapshot differs\ngo: %s\npy: %s", c.Harness, c.Profile, got, want.Installed[j])
			}
		}
	}
}
