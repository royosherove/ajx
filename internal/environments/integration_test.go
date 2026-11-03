package environments

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

func preparedLocal(t *testing.T) (*EnvironmentRunner, *plugin.Ctx) {
	t.Helper()
	tr := newTemp(t)
	r := localRunner(t, tr.base, nil)
	ctx := newCtx(t, tr.root, "attempt", true)
	persist(t, r, ctx)
	_, err := r.Prepare(ctx)
	noErr(t, err)
	ctx.Runner = r
	ctx.EnvironmentProfile = r.Profile()
	ctx.Env = r.EnvironmentEnv(ctx)
	return r, ctx
}

func TestContextShellCheckedSurfacesEnvironmentErrors(t *testing.T) {
	r, ctx := preparedLocal(t)
	res, err := plugin.ContextShellChecked(`printf "$HOME"`, ctx, 10, "environment")
	noErr(t, err)
	if res["location"] != "environment" || res["stdout"] != ctx.HomeDir {
		t.Fatal(res)
	}
	host, err := plugin.ContextShellChecked(`printf "$HOME"`, ctx, 10, "host")
	noErr(t, err)
	if host["location"] != "host" || !strings.HasPrefix(util.S(host["stdout"]), filepath.Join(ctx.RunDir, "host-environment")) {
		t.Fatal(host)
	}
	// Host checks cannot load credentials from worker-owned paths: Python raises before running anything.
	ctx.CheckEnv = util.MergeEnv(ctx.Env, map[string]string{"AWS_CONFIG_FILE": filepath.Join(ctx.ConfigDir, "config")})
	marker := filepath.Join(ctx.Workspace, "ran")
	_, err = plugin.ContextShellChecked("touch "+shQuote(marker), ctx, 10, "host")
	wantErr(t, err, KindValue, "AWS_CONFIG_FILE")
	if record := plugin.ContextShell("touch "+shQuote(marker), ctx, 10, "host"); record["exit_code"] != nil ||
		!strings.Contains(util.S(record["error"]), "AWS_CONFIG_FILE") {
		t.Fatal(record)
	}
	if lexists(marker) {
		t.Fatal("a refused host command ran")
	}
	ctx.CheckEnv = nil
	r.Release(ctx)
	_, err = plugin.ContextShellChecked("true", ctx, 10, "environment")
	wantErr(t, err, KindEnvironment, "released")
}

func TestLocalLaunchNeverInheritsAmbientAuthOrConfig(t *testing.T) {
	_, ctx := preparedLocal(t)
	for k, v := range map[string]string{"AWS_ACCESS_KEY_ID": "synthetic-access", "ANTHROPIC_API_KEY": "synthetic-other-provider",
		"GOOGLE_APPLICATION_CREDENTIALS": "/synthetic/caller-login.json", "HTTP_PROXY": "http://proxy.example.test",
		"UNRELATED_SECRET": "synthetic-unrelated"} {
		t.Setenv(k, v)
	}
	ctx.Env["DECLARED_TOKEN"] = "synthetic-declared"
	h := &plugin.BaseHarness{}
	plugin.Bind(h)
	proc, err := h.Run(ctx, []string{"/usr/bin/env"}, "execute", plugin.RunOpts{})
	noErr(t, err)
	if proc["exit_code"] != int64(0) {
		t.Fatal(proc)
	}
	var lines []string
	for _, rec := range plugin.StreamRecords(filepath.Join(ctx.RunDir, "execute.raw.jsonl")) {
		lines = append(lines, rec.Line)
	}
	env := strings.Join(lines, "\n")
	for _, key := range []string{"AWS_ACCESS_KEY_ID", "ANTHROPIC_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS", "HTTP_PROXY", "UNRELATED_SECRET"} {
		if strings.Contains(env, key+"=") {
			t.Fatalf("%s inherited", key)
		}
	}
	if !strings.Contains(env, "DECLARED_TOKEN=synthetic-declared") || !strings.Contains(env, "HOME="+ctx.HomeDir) ||
		strings.Contains(env, "HOME="+os.Getenv("HOME")+"\n") {
		t.Fatal(env)
	}
	// An unsafe override fails the launch instead of running with a partial environment.
	ctx.Env["LD_PRELOAD"] = "/synthetic/lib.so"
	_, err = h.Run(ctx, []string{"/usr/bin/env"}, "execute", plugin.RunOpts{})
	wantErr(t, err, KindValue, "LD_PRELOAD")
}
