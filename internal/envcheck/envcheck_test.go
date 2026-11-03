package envcheck

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/builtin"
	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

// guardAWS shadows the caller's real aws CLI: the suite stays offline.
func guardAWS(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"$@\" >> \"$(dirname \"$0\")/aws.calls\"\necho \"ajx tests: real aws blocked\" >&2\nexit 97\n"
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, v := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
}

// fakeShell is a shell whose startup file sets a variable (like a ~/.zshenv export), then behaves as sh.
func fakeShell(t *testing.T, dir, name, exports string) string {
	t.Helper()
	p := filepath.Join(dir, "bin", name)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+exports+"\nexec /bin/sh \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func trialAt(t *testing.T, dir, body, cells string) string {
	t.Helper()
	if cells == "" {
		cells = "[[cells]]\nid = \"a\"\nharness = \"claude-code\"\n"
	}
	_ = os.WriteFile(filepath.Join(dir, "task-prompt.md"), []byte("x\n"), 0o644)
	p := filepath.Join(dir, "trial.toml")
	text := "[trial]\nid = \"t\"\nproduct = \"p\"\nworkspace_root = \"" + dir + "/ws\"\n[task]\nprompt_file = \"task-prompt.md\"\n" + body + "\n" + cells
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

type fixture struct {
	tmp   string
	shell string
}

func newEnvFixture(t *testing.T) *fixture {
	guardAWS(t)
	tmp := spec.Resolve(t.TempDir())
	cfg := filepath.Join(tmp, "claude-config")
	_ = os.MkdirAll(cfg, 0o755)
	settings, _ := json.Marshal(M{"env": M{"AJX_T_SETTINGSVAR": "from-settings", "AJX_T_PROFILE": "from-settings-profile"}})
	_ = os.WriteFile(filepath.Join(cfg, "settings.json"), settings, 0o644)
	shell := fakeShell(t, tmp, "zsh", "export AJX_T_SHELLVAR=from-rc")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("CLAUDE_CODE_SHELL", shell)
	old := builtin.ClaudeManagedSettings
	builtin.ClaudeManagedSettings = filepath.Join(tmp, "no-managed-settings.json")
	t.Cleanup(func() { builtin.ClaudeManagedSettings = old })
	return &fixture{tmp: tmp, shell: shell}
}

func load(t *testing.T, path string) M {
	t.Helper()
	sp, err := spec.Load(path, spec.LoadOpts{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return sp
}

func (f *fixture) inspect(t *testing.T, envBlock, cells string, identities bool) (M, M, plugin.Auth) {
	t.Helper()
	sp := load(t, trialAt(t, f.tmp, envBlock, cells))
	cell := util.Maps(sp["cells"])[0]
	auth, err := spec.AuthFor(sp, cell, "")
	if err != nil {
		t.Fatal(err)
	}
	rn, err := spec.RunnerFor(sp, cell)
	if err != nil {
		t.Fatal(err)
	}
	h, err := spec.HarnessFor(sp, util.Str(cell["harness"]))
	if err != nil {
		t.Fatal(err)
	}
	ws, err := ScratchWorkspace(sp, f.tmp)
	if err != nil {
		t.Fatal(err)
	}
	checks, view := InspectCell(sp, cell, h, auth, rn, ws, PlaceholderCtx(sp, cell, auth, rn, f.tmp), InspectOpts{Identities: identities})
	return checks, view, auth
}

func TestOverridesByHarnessSettingsAndShellStartup(t *testing.T) {
	f := newEnvFixture(t)
	checks, view, auth := f.inspect(t, "[env]\nAJX_T_SETTINGSVAR = \"trial\"\nAJX_T_SHELLVAR = \"trial\"\nAJX_T_PLAIN = \"trial\"\n"+
		"AJX_T_PROFILE = \"trial\"\n", "[[cells]]\nid = \"u\"\nharness = \"claude-code\"\nconfig = \"user\"\n", false)
	found := map[string]M{}
	for _, o := range util.Maps(checks["overrides"]) {
		found[util.Str(o["name"])] = o
	}
	names := util.SortedKeys(found)
	if !reflect.DeepEqual(names, []string{"AJX_T_PROFILE", "AJX_T_SETTINGSVAR", "AJX_T_SHELLVAR"}) {
		t.Fatalf("overrides %v", names)
	}
	sv := found["AJX_T_SETTINGSVAR"]
	if sv["model_affected"] != true || !strings.HasSuffix(util.Str(sv["settings_source"]), "settings.json") || sv["tool_shell"] != "from-settings" {
		t.Fatalf("settings var %v", sv)
	}
	sh := found["AJX_T_SHELLVAR"]
	if sh["model_affected"] != false || sh["shell_startup"] != true || sh["tool_shell"] != "from-rc" {
		t.Fatalf("shell var %v", sh)
	}
	if checks["tool_shell"] != f.shell+" -c" {
		t.Fatalf("tool_shell %v", checks["tool_shell"])
	}
	text := strings.Join(Warnings(checks, auth, view), " ")
	for _, want := range []string{`[env] sets AJX_T_PROFILE="trial", but the harness applies "from-settings-profile" from harness settings`,
		"[env] sets AJX_T_SETTINGSVAR=<value hidden>"} {
		if !strings.Contains(text, want) {
			t.Fatalf("warnings lack %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, `"from-settings"`) {
		t.Fatal("secret-looking value printed")
	}
	rec, _ := json.Marshal(Record(checks))
	if strings.Contains(string(rec), "from-settings") {
		t.Fatalf("record holds values: %s", rec)
	}
}

func TestIsolatedConfigDirIgnoresUserSettings(t *testing.T) {
	f := newEnvFixture(t)
	checks, _, _ := f.inspect(t, "[env]\nAJX_T_SETTINGSVAR = \"trial\"\n[auth.bed]\ntype = \"claude-bedrock\"\n",
		"[[cells]]\nid = \"c\"\nharness = \"claude-code\"\nauth = \"bed\"\n", false)
	if len(util.List(checks["harness_settings"])) != 0 || len(util.List(checks["overrides"])) != 0 {
		t.Fatalf("settings %v overrides %v", checks["harness_settings"], checks["overrides"])
	}
}

func TestTaskEnvCollidingWithModelCredentials(t *testing.T) {
	f := newEnvFixture(t)
	env := "[env]\nAWS_PROFILE = \"default\"\nAWS_REGION = \"us-east-1\"\nMY_FLAG = \"1\"\n"
	cases := []struct {
		extra, harness, auth string
		caller               map[string]string
		want                 []string
	}{
		{"[auth.bed]\ntype = \"claude-bedrock\"\n", "claude-code", "auth = \"bed\"", nil, []string{"AWS_PROFILE", "AWS_REGION"}},
		{"", "claude-code", "", map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}, []string{"AWS_PROFILE", "AWS_REGION"}},
		{"", "claude-code", "", map[string]string{"CLAUDE_CODE_USE_BEDROCK": ""}, []string{}},
		{"[auth.key]\ntype = \"anthropic-api\"\n", "claude-code", "auth = \"key\"", nil, []string{}},
		{"", "kiro-cli", "auth = \"inherit\"", map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}, []string{}},
	}
	for _, c := range cases {
		for k, v := range c.caller {
			t.Setenv(k, v)
		}
		checks, view, auth := f.inspect(t, env+c.extra, "[[cells]]\nid = \"c\"\nharness = \""+c.harness+"\"\n"+c.auth+"\n", false)
		got := util.Strings(checks["collisions"])
		if got == nil {
			got = []string{}
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %s %v: collisions %v want %v", c.harness, c.auth, c.caller, got, c.want)
		}
		if len(c.want) > 0 && !strings.Contains(CollisionText(c.want, auth, view), "--profile") {
			t.Errorf("collision text lacks --profile")
		}
		for k := range c.caller {
			os.Unsetenv(k)
		}
	}
}

func TestTaskIdentityInToolShellAndCheckShell(t *testing.T) {
	f := newEnvFixture(t)
	checks, _, _ := f.inspect(t, "identity_cmd = \"echo acct-$AJX_T_SHELLVAR\"\n[env]\nAJX_T_SHELLVAR = \"trial\"\n",
		"[[cells]]\nid = \"u\"\nharness = \"claude-code\"\nconfig = \"user\"\n", true)
	ti := util.MapOr(checks["task_identity"])
	if ti["ok"] != true || ti["output"] != "acct-from-rc" {
		t.Fatalf("task identity %v", ti)
	}
	sp := load(t, filepath.Join(f.tmp, "trial.toml"))
	cell := util.Maps(sp["cells"])[0]
	auth, _ := spec.AuthFor(sp, cell, "")
	checkEnv := CommandEnv(spec.WorkerEnv(sp, cell, auth), auth.Unset(), "t", nil)
	ctx := context.Background()
	if got := IdentityResult(ctx, util.Str(util.Get(sp, "task", "identity_cmd")), f.tmp, checkEnv, ""); got["output"] != "acct-trial" {
		t.Fatalf("check shell identity %v", got)
	}
	failing := IdentityResult(ctx, `echo "An error occurred (NoCredentials): Unable to locate credentials" >&2; exit 253`, f.tmp, checkEnv, "")
	if failing["ok"] != false || util.Int(failing["exit_code"], 0) != 253 || !strings.Contains(util.Str(failing["output"]), "NoCredentials") {
		t.Fatalf("failing %v", failing)
	}
	if hidden := IdentityResult(ctx, `echo "Unable to locate credentials" >&2; true`, f.tmp, checkEnv, ""); hidden["ok"] != false {
		t.Fatal("exit 0 is not enough")
	}
	if failing["shell"] != "/bin/sh -c" {
		t.Fatal(failing["shell"])
	}
}

func TestAuthErrorScanner(t *testing.T) {
	for _, line := range []string{
		`aws: [ERROR]: An error occurred (NoCredentials): Unable to locate credentials. You can configure credentials by running "aws login".`,
		"An error occurred (ExpiredToken) when calling the DescribeInstances operation",
		"The config profile (sandbox) could not be found", "Please run 'az login' to setup account.",
		"error: You must be logged in to the server (Unauthorized)", "HTTP 401: Bad credentials",
		"npm ERR! code E401", "Error: No valid credential sources found",
		"CredentialsProviderError: Could not load credentials from any providers",
		"Unable to resolve AWS account to use. It must be either configured when you define your CDK Stack",
		"failed to refresh cached credentials, no EC2 IMDS role found",
		"botocore.exceptions.NoCredentialsError: Unable to locate credentials",
		"Run `gh auth login` to authenticate", "AADSTS700016: Application not found",
	} {
		if len(AuthErrorLines(line)) == 0 {
			t.Errorf("missed %q", line)
		}
	}
	for _, line := range []string{"running=0", "sg=0 kp=0", "run the login flow later", "Authentication succeeded",
		"Token refreshed", "app.py:1:1: E401 multiple imports on one line"} {
		if len(AuthErrorLines(line)) != 0 {
			t.Errorf("false positive %q", line)
		}
	}
	long := strings.Repeat("x", 300) + " AccessDenied"
	lines := AuthErrorLines("a AccessDenied\r\nb AccessDenied\n\n  a AccessDenied  \nc AccessDenied\nd AccessDenied\ne AccessDenied\nf AccessDenied", long)
	if len(lines) != 5 || lines[0] != "a AccessDenied" || lines[1] != "b AccessDenied" {
		t.Fatalf("lines %v", lines)
	}
	if got := AuthErrorLinesN(10, long); util.RuneLen(got[0]) != 240 {
		t.Fatal(got)
	}
}

func TestShownHidesSecrets(t *testing.T) {
	for _, c := range []struct {
		name  string
		value any
		want  string
	}{{"AWS_PROFILE", "dev", `"dev"`}, {"AWS_SESSION_TOKEN", "x", "<value hidden>"}, {"MY_VAR", "x", "<value hidden>"},
		{"AWS_PROFILE", nil, "<unset>"}, {"aws_region", "é", `"\u00e9"`}, {"LANG", strings.Repeat("a", 81), `"` + strings.Repeat("a", 77) + `..."`},
		{"ANTHROPIC_CUSTOM_HEADERS", "x-api-key: sk", "<value hidden>"}, {"GOOGLE_CLOUD_PROJECT", "p", `"p"`}} {
		if got := Shown(c.name, c.value); got != c.want {
			t.Errorf("Shown(%s) = %s want %s", c.name, got, c.want)
		}
	}
}

func TestAdapterLaunchEnvIsModeled(t *testing.T) {
	f := newEnvFixture(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(f.tmp, "empty-config"))
	t.Setenv("CLAUDE_CODE_SHELL", "/bin/sh")
	t.Setenv("SHELL", "/bin/sh")
	checks, view, auth := f.inspect(t, "[env]\nAJX_T_PROFILE = \"trial-value\"\n[adapters.mytool]\nexecute = [\"true\"]\n"+
		"env = { AJX_T_PROFILE = \"adapter-value\" }\n", "[[cells]]\nid = \"d\"\nharness = \"mytool\"\n", false)
	if Effective(view)["AJX_T_PROFILE"] != "adapter-value" {
		t.Fatalf("effective %v", Effective(view)["AJX_T_PROFILE"])
	}
	want := `[env] sets AJX_T_PROFILE="trial-value", but the harness applies "adapter-value" from the harness's launch env`
	if text := strings.Join(Warnings(checks, auth, view), " "); !strings.Contains(text, want) {
		t.Fatalf("warnings %s", text)
	}
}

func TestProbeIgnoresLocaleCoercionAndFlagsPathSnapshot(t *testing.T) {
	f := newEnvFixture(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(f.tmp, "empty-config"))
	t.Setenv("CLAUDE_CODE_SHELL", "/bin/sh")
	checks, view, auth := f.inspect(t, "[env]\nLC_CTYPE = \"C\"\nPATH = \"/opt/x/bin:/usr/bin:/bin\"\n",
		"[[cells]]\nid = \"c\"\nharness = \"claude-code\"\n", false)
	if o := util.List(checks["overrides"]); len(o) != 0 {
		t.Fatalf("overrides %v", o)
	}
	if !reflect.DeepEqual(util.Strings(checks["snapshot_restored"]), []string{"PATH"}) {
		t.Fatal(checks["snapshot_restored"])
	}
	if !strings.Contains(strings.Join(Warnings(checks, auth, view), " "), "PATH is reset before every tool command") {
		t.Fatal("no snapshot warning")
	}
}

func TestDefaultAWSIdentityShowsWhatACommandWithoutProfileActsAs(t *testing.T) {
	f := newEnvFixture(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(f.tmp, "empty-config"))
	t.Setenv("CLAUDE_CODE_SHELL", "/bin/sh")
	t.Setenv("SHELL", "/bin/sh")
	bin := filepath.Join(f.tmp, "awsbin")
	_ = os.MkdirAll(bin, 0o755)
	_ = os.WriteFile(filepath.Join(bin, "aws"), []byte("#!/bin/sh\np=\"$AWS_PROFILE\"\nwhile [ $# -gt 0 ]; do [ \"$1\" = \"--profile\" ] && p=\"$2\"; shift; done\necho \"acct-$p\"\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	checks, view, auth := f.inspect(t, "identity_cmd = \"aws sts get-caller-identity --profile task-prof\"\n"+
		"[auth.model]\ntype = \"claude-bedrock\"\nenv = { AWS_PROFILE = \"model-prof\", AWS_REGION = \"us-east-1\" }\n"+
		"identity_cmd = [\"aws\", \"sts\", \"get-caller-identity\", \"--profile\", \"model-prof\"]\n",
		"[[cells]]\nid = \"c\"\nharness = \"claude-code\"\nauth = \"model\"\n", true)
	if !UsesAWS(load(t, filepath.Join(f.tmp, "trial.toml")), auth, view) {
		t.Fatal("uses aws")
	}
	if got := util.Get(checks, "task_identity", "output"); got != "acct-task-prof" {
		t.Fatalf("task identity %v", got)
	}
	if got := util.Get(checks, "default_aws_identity", "output"); got != "acct-model-prof" {
		t.Fatalf("default identity %v", got)
	}
	if checks["auth_identity"] != "acct-model-prof" {
		t.Fatalf("auth identity %v", checks["auth_identity"])
	}
	w := DefaultIdentityWarning(checks)
	if !strings.Contains(w, "omits the task's --profile acts as acct-model-prof (the model's account)") {
		t.Fatalf("warning %q", w)
	}
}

func TestStartupSourceAndShellSkip(t *testing.T) {
	home := t.TempDir()
	_ = os.WriteFile(filepath.Join(home, ".zshenv"), []byte("# x\n  export AWS_PROFILE=dev\n"), 0o644)
	if got := StartupSource("AWS_PROFILE", map[string]string{"HOME": home}); got != "~/.zshenv line 2" {
		t.Fatal(got)
	}
	bashEnv := filepath.Join(home, "bashenv")
	_ = os.WriteFile(bashEnv, []byte("typeset -x FOO=1\n"), 0o644)
	if got := StartupSource("FOO", map[string]string{"HOME": "/nowhere", "BASH_ENV": bashEnv}); got != bashEnv+" line 1" {
		t.Fatal(got)
	}
	if got := StartupSource("NOPE", map[string]string{"HOME": home}); got != "" {
		t.Fatal(got)
	}
	// shell_startup = "skip" empties ZDOTDIR and drops BASH_ENV/ENV for the placeholder run
	guardAWS(t)
	dir := spec.Resolve(t.TempDir())
	sp := load(t, trialAt(t, dir, "", "[[cells]]\nid = \"a\"\nharness = \"claude-code\"\nshell_startup = \"skip\"\n"))
	cell := util.Maps(sp["cells"])[0]
	auth, _ := spec.AuthFor(sp, cell, "")
	rn, _ := spec.RunnerFor(sp, cell)
	ctx := PlaceholderCtx(sp, cell, auth, rn, dir)
	if ctx.Env["ZDOTDIR"] != filepath.Join(dir, "no-shell-startup") || !util.Contains(ctx.UnsetEnv, "BASH_ENV") ||
		!util.Contains(ctx.UnsetEnv, "ENV") || ctx.RunToken != "doctor" || ctx.CheckEnv["AJX_RUN_TOKEN"] != "doctor" ||
		ctx.CheckEnv["AJX_TRIAL_DIR"] != dir {
		t.Fatalf("ctx %+v", ctx)
	}
}

func TestShellSyntaxWarnings(t *testing.T) {
	sp := M{"setup": []any{M{"type": "shell", "run": "diff <(ls) <(ls)", "name": "cmp"}},
		"verify":   []any{M{"type": "http", "run": "[[ x ]]"}, M{"type": "shell", "run": "[[ -f x ]] && echo {1..3}", "name": "v"}},
		"teardown": []any{M{"type": "shell", "run": "source env.sh", "name": "t"}, M{"type": "shell", "run": ". ./env.sh; ok"}}}
	got := ShellSyntaxWarnings(sp)
	want := []string{
		"[[setup]] 1 (cmp) uses process substitution <(...), which /bin/sh may not support; AJX runs it with /bin/sh -c. Use POSIX syntax or wrap it in bash -c '...'",
		"[[verify]] 2 (v) uses [[ ... ]] tests, brace ranges {a..b}, which /bin/sh may not support; AJX runs it with /bin/sh -c. Use POSIX syntax or wrap it in bash -c '...'",
		"[[teardown]] 1 (t) uses source (use . instead), which /bin/sh may not support; AJX runs it with /bin/sh -c. Use POSIX syntax or wrap it in bash -c '...'",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestRunPreflightAndSummaries(t *testing.T) {
	dir := t.TempDir()
	cmds := []M{
		{"name": "ok", "run": "echo fine", "timeout": int64(10)},
		{"name": "denied as expected", "run": `echo "An error occurred (AccessDenied)" >&2; true`, "timeout": int64(10), "allow_auth_errors": true},
		{"name": "creds", "run": "echo Unable to locate credentials >&2; exit 253", "timeout": int64(10)},
		{"name": "after", "run": "true", "timeout": int64(10)},
	}
	env := CommandEnv(map[string]string{}, nil, "tok", nil)
	res, err := RunPreflight(context.Background(), cmds, dir, env, PreflightOpts{})
	if err != nil {
		t.Fatal(err)
	}
	var passed []bool
	for _, r := range res {
		passed = append(passed, r["passed"].(bool))
	}
	if !reflect.DeepEqual(passed, []bool{true, true, false, true}) {
		t.Fatalf("passed %v", passed)
	}
	if got := PreflightFailure(res[2]); got != "exit 253: Unable to locate credentials" {
		t.Fatal(got)
	}
	res, _ = RunPreflight(context.Background(), cmds, dir, env, PreflightOpts{StopOnFailure: true})
	if len(res) != 3 {
		t.Fatalf("stop on failure ran %d", len(res))
	}
	res, _ = RunPreflight(context.Background(), cmds[:1], dir, env, PreflightOpts{Executor: func(cmd M) M {
		return M{"cmd": cmd["run"], "exit_code": nil, "timed_out": true, "started_at": "a", "stopped_at": "b",
			"stdout": "", "stderr": "", "location": "environment"}
	}})
	if res[0]["location"] != "environment" || res[0]["passed"] != false || PreflightFailure(res[0]) != "timed out" {
		t.Fatalf("executor %v", res[0])
	}
	if got := PreflightFailure(M{"exit_code": float64(2), "timed_out": false, "stderr": "", "stdout": " boom "}); got != "exit 2: boom" {
		t.Fatal(got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RunPreflight(ctx, cmds[:1], dir, env, PreflightOpts{}); !util.Interrupted(err) {
		t.Fatalf("interrupt %v", err)
	}
	ok := M{"exit_code": int64(0), "timed_out": false, "auth_errors": []any{}}
	bad := M{"exit_code": int64(4), "timed_out": false, "auth_errors": []any{}}
	auth := M{"exit_code": int64(0), "timed_out": false, "auth_errors": []any{"AccessDenied"}}
	for _, c := range []struct {
		results []M
		defined int
		ran     bool
		status  string
	}{{nil, 0, true, "ok"}, {nil, 2, false, "skipped"}, {[]M{ok, bad}, 2, true, "failed"},
		{[]M{bad, auth}, 2, true, "auth_errors"}, {[]M{ok}, 1, true, "ok"}} {
		s := TeardownSummary(c.results, c.defined, c.ran)
		if s["status"] != c.status {
			t.Errorf("%v: status %v want %s", c, s["status"], c.status)
		}
	}
	s := TeardownSummary([]M{ok, bad}, 2, true)
	if b, _ := json.Marshal(s); string(b) != `{"auth_errors":[],"commands":2,"failed":[1],"status":"failed"}` {
		t.Fatal(string(b))
	}
}

func TestRecordedNotes(t *testing.T) {
	env := M{"env_checks": M{"overrides": []any{
		M{"name": "AWS_PROFILE", "origin": "[env]", "settings_source": "harness settings x.json", "shell_startup": true, "model_affected": true},
		M{"name": "FOO", "origin": "cell a env", "settings_source": nil, "shell_startup": true, "model_affected": false}},
		"collisions": []any{"AWS_PROFILE", "AWS_REGION"}}}
	want := []string{
		"[env] AWS_PROFILE was replaced by harness settings x.json and the tool shell's startup files; the harness (model calls) and the agent's commands did not see the value ajx set",
		"cell a env FOO was replaced by the tool shell's startup files; the agent's commands did not see the value ajx set",
		"AWS_PROFILE, AWS_REGION set for the task also drive the harness's model access; one of the two did not get its intended value",
	}
	if got := RecordedNotes(env); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	if got := RecordedNotes(nil); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestScratchWorkspaceCopiesFixture(t *testing.T) {
	src := t.TempDir()
	_ = os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "sub", "a.sh"), []byte("echo"), 0o755)
	tmp := t.TempDir()
	ws, err := ScratchWorkspace(M{"task": M{"fixture_dir": src}}, tmp)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(ws, "sub", "a.sh"))
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("copy %v %v", st, err)
	}
	if ws2, err := ScratchWorkspace(M{"task": M{"fixture_dir": nil}}, t.TempDir()); err != nil || !isDir(ws2) {
		t.Fatal(err)
	}
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func TestShellEnvProbe(t *testing.T) {
	dir := t.TempDir()
	shell := fakeShell(t, dir, "zsh", "export AJX_T_SHELLVAR=from-rc")
	vars := ShellEnv(context.Background(), shell, map[string]string{"PATH": "/usr/bin:/bin", "A": "multi\nline"}, dir)
	if vars == nil || vars["AJX_T_SHELLVAR"] != "from-rc" || vars["A"] != "multi\nline" {
		t.Fatalf("vars %v", vars)
	}
	if ShellEnv(context.Background(), filepath.Join(dir, "missing-shell"), map[string]string{}, dir) != nil {
		t.Fatal("missing shell probed")
	}
	keys := util.SortedKeys(vars)
	sort.Strings(keys)
}
