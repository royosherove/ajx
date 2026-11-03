package environments

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/util"
)

func hostRunner(t *testing.T, base, coordinatorPath string) *EnvironmentRunner {
	t.Helper()
	t.Setenv("PATH", coordinatorPath)
	r := mustNew(t, containerProfile(t, base, nil))
	return r
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	noErr(t, err)
	return st.Mode().Perm()
}

func TestHostRuntimeAndConfigIgnoreWorkerPlantedStartupFiles(t *testing.T) {
	tr := newTemp(t)
	ctx := newCtx(t, tr.root, "attempt", true)
	workerBin := filepath.Join(ctx.HomeDir, ".local", "bin")
	workerConfig := filepath.Join(ctx.HomeDir, ".config")
	for _, p := range []string{workerBin, workerConfig, ctx.ConfigDir} {
		noErr(t, os.MkdirAll(p, 0o755))
		writeFile(t, filepath.Join(p, "python3"), "#!/bin/sh\nprintf poisoned-worker-runtime\n")
		noErr(t, os.Chmod(filepath.Join(p, "python3"), 0o755))
		writeFile(t, filepath.Join(p, "settings.json"), `{"source":"worker"}`)
	}
	writeFile(t, filepath.Join(ctx.Workspace, "sitecustomize.py"), "raise SystemExit('worker startup executed')\n")
	runtimeDir := filepath.Join(tr.root, "coordinator-runtime")
	noErr(t, os.Mkdir(runtimeDir, 0o755))
	noErr(t, os.Symlink("/bin/sh", filepath.Join(runtimeDir, "coordinator-sh")))
	alias := filepath.Join(tr.root, "worker-bin-alias")
	noErr(t, os.Symlink(workerBin, alias))
	coordinatorPath := ":relative-tools:" + workerBin + ":" + workerConfig + ":" + ctx.ConfigDir + ":" + alias + ":" +
		runtimeDir + ":/usr/bin:/bin"
	r := hostRunner(t, tr.base, coordinatorPath)
	checkEnv := r.EnvironmentEnv(ctx)
	for k, v := range map[string]string{"PATH": workerBin + ":" + workerConfig,
		"CODEX_HOME": ctx.ConfigDir, "CLAUDE_CONFIG_DIR": ctx.ConfigDir, "KIRO_HOME": ctx.ConfigDir,
		"ZDOTDIR": ctx.HomeDir, "PYTHONPATH": ctx.Workspace,
		"PYTHONSTARTUP":       filepath.Join(ctx.Workspace, "sitecustomize.py"),
		"EXPLICIT_TEST_TOKEN": "synthetic-declared-value"} {
		checkEnv[k] = v
	}
	ctx.CheckEnv = checkEnv
	t.Setenv("AJX_HOST_AMBIENT_SECRET", "synthetic-do-not-inherit")
	t.Setenv("PATH", workerBin)
	env, err := r.HostEnvChecked(ctx, ctx.CheckEnv, nil)
	noErr(t, err)
	entries := strings.Split(env["PATH"], ":")
	if entries[0] != runtimeDir {
		t.Fatal(env["PATH"])
	}
	roots, err := r.workerRoots(ctx)
	noErr(t, err)
	for _, entry := range entries {
		if !isAbs(entry) || insideWorker(entry, roots) {
			t.Fatalf("unsafe PATH entry %s", entry)
		}
	}
	for _, key := range []string{"PYTHONPATH", "AJX_HOST_AMBIENT_SECRET"} {
		if _, ok := env[key]; ok {
			t.Fatal(key)
		}
	}
	if env["PYTHONSAFEPATH"] != "1" || env["PYTHONNOUSERSITE"] != "1" || env["PYTHONSTARTUP"] != "/dev/null" {
		t.Fatal(env)
	}
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "TMPDIR", "PYTHONUSERBASE",
		"npm_config_prefix", "CARGO_HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "KIRO_HOME"} {
		if !relativeTo(env[key], ctx.RunDir) {
			t.Fatalf("%s=%s is outside the report directory", key, env[key])
		}
		if m := mode(t, env[key]); m != 0o700 {
			t.Fatalf("%s mode %o", key, m)
		}
	}
	settings := filepath.Join(env["XDG_CONFIG_HOME"], "settings.json")
	writeFile(t, settings, `{"source":"coordinator"}`)
	cmd := exec.Command("/bin/sh", "-c", `command -v coordinator-sh; cat "$XDG_CONFIG_HOME/settings.json"; printf '|%s|%s' "$EXPLICIT_TEST_TOKEN" "$HOME"`)
	cmd.Dir = ctx.Workspace
	cmd.Env = util.EnvList(env)
	out, err := cmd.Output()
	noErr(t, err)
	want := filepath.Join(runtimeDir, "coordinator-sh") + "\n" + `{"source":"coordinator"}` + "|synthetic-declared-value|" + env["HOME"]
	if string(out) != want {
		t.Fatalf("got %q want %q", out, want)
	}
	again, err := r.HostEnvChecked(ctx, nil, nil)
	noErr(t, err)
	if again["HOME"] != env["HOME"] || env["HOME"] == ctx.HomeDir {
		t.Fatal("host context must be stable and private within an attempt")
	}
}

func TestHostPackageCachesCannotLoadWorkerInstalledPackages(t *testing.T) {
	tr := newTemp(t)
	ctx := newCtx(t, tr.root, "attempt", true)
	keys := []string{"npm_config_cache", "YARN_CACHE_FOLDER", "PNPM_STORE_DIR", "UV_CACHE_DIR",
		"PIP_CACHE_DIR", "POETRY_CACHE_DIR", "GOCACHE", "GOMODCACHE", "BUN_INSTALL_CACHE_DIR",
		"DENO_DIR", "NUGET_PACKAGES", "GRADLE_USER_HOME"}
	workerCaches := map[string]string{}
	for _, key := range keys {
		p := filepath.Join(ctx.CacheDir, strings.ToLower(key))
		noErr(t, os.Mkdir(p, 0o755))
		writeFile(t, filepath.Join(p, "package-startup"), "worker-controlled code")
		workerCaches[key] = p
	}
	ctx.Env = util.MergeEnv(workerCaches)
	ctx.CheckEnv = util.MergeEnv(workerCaches)
	r := hostRunner(t, tr.base, "/usr/bin:/bin")
	env, err := r.HostEnvChecked(ctx, workerCaches, nil)
	noErr(t, err)
	for _, key := range keys {
		p := env[key]
		if p == workerCaches[key] || !relativeTo(p, env["XDG_CACHE_HOME"]) ||
			!relativeTo(p, filepath.Join(ctx.RunDir, "host-environment")) || mode(t, p) != 0o700 ||
			lexists(filepath.Join(p, "package-startup")) {
			t.Fatalf("%s=%s", key, p)
		}
		if readFile(t, filepath.Join(workerCaches[key], "package-startup")) != "worker-controlled code" {
			t.Fatal(key)
		}
	}
	_, err = r.HostEnvChecked(ctx, map[string]string{}, []string{"GRADLE_USER_HOME"})
	wantErr(t, err, KindValue, "cannot unset")
}

func TestPrivateHostRootsRejectSymlinksWithoutWritingTheTarget(t *testing.T) {
	tr := newTemp(t)
	ctx := newCtx(t, tr.root, "attempt", true)
	outside := filepath.Join(tr.root, "unrelated")
	noErr(t, os.Mkdir(outside, 0o755))
	private := filepath.Join(ctx.RunDir, "host-environment")
	noErr(t, os.Symlink(outside, private))
	r := hostRunner(t, tr.base, "/usr/bin:/bin")
	_, err := r.HostEnvChecked(ctx, nil, nil)
	wantErr(t, err, KindValue, "without symlinks")
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatal("wrote through a symlink")
	}
	noErr(t, os.Remove(private))
	noErr(t, os.Mkdir(private, 0o700))
	noErr(t, os.Symlink(outside, filepath.Join(private, "cache")))
	_, err = r.HostEnvChecked(ctx, nil, nil)
	wantErr(t, err, KindValue, "without symlinks")
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatal("wrote through a symlink")
	}
	// The plugin contract never falls back to the caller's environment.
	if env := r.HostEnv(ctx, nil, nil); len(env) != 0 {
		t.Fatal(env)
	}
}

func TestHostFilterIncludesWriteMountsAndPreservesExplicitAuthUnset(t *testing.T) {
	tr := newTemp(t)
	ctx := newCtx(t, tr.root, "attempt", true)
	extraWrite := filepath.Join(tr.root, "extra-write-mount")
	noErr(t, os.Mkdir(extraWrite, 0o755))
	ctx.State["environment"] = util.M{"mounts": []any{util.M{"access": "write", "host_source": extraWrite}}}
	r := hostRunner(t, tr.base, extraWrite+"::relative:/usr/bin:/bin")
	env, err := r.HostEnvChecked(ctx, map[string]string{"DECLARED_TOKEN": "synthetic", "DROP_TOKEN": "synthetic"}, []string{"DROP_*"})
	noErr(t, err)
	if strings.Contains(env["PATH"], extraWrite) {
		t.Fatal(env["PATH"])
	}
	if _, ok := env["DROP_TOKEN"]; ok || env["DECLARED_TOKEN"] != "synthetic" {
		t.Fatal(env)
	}
	_, err = r.HostEnvChecked(ctx, map[string]string{}, []string{"HOME"})
	wantErr(t, err, KindValue, "cannot unset")
	_, err = r.HostEnvChecked(ctx, map[string]string{"AWS_CONFIG_FILE": filepath.Join(ctx.ConfigDir, "config")}, nil)
	wantErr(t, err, KindValue, "AWS_CONFIG_FILE")
	trusted := filepath.Join(tr.root, "coordinator-configuration")
	env, err = r.HostEnvChecked(ctx, map[string]string{"AWS_CONFIG_FILE": trusted}, nil)
	noErr(t, err)
	if env["AWS_CONFIG_FILE"] != trusted {
		t.Fatal(env["AWS_CONFIG_FILE"])
	}
}

func TestSeparateAttemptsGetSeparatePrivateHostContexts(t *testing.T) {
	tr := newTemp(t)
	r := hostRunner(t, tr.base, "/usr/bin:/bin")
	a, b := newCtx(t, tr.root, "a", true), newCtx(t, tr.root, "b", true)
	first, err := r.HostEnvChecked(a, nil, nil)
	noErr(t, err)
	second, err := r.HostEnvChecked(b, nil, nil)
	noErr(t, err)
	if first["HOME"] == second["HOME"] {
		t.Fatal("attempts share a host context")
	}
	writeFile(t, filepath.Join(first["HOME"], "private-file"), "coordinator-only")
	if lexists(filepath.Join(second["HOME"], "private-file")) {
		t.Fatal("host contexts leak between attempts")
	}
	local := localRunner(t, tr.base, nil)
	if util.Get(local.Doctor(nil), "capabilities", "filesystem_isolation", "state") != "unsupported" {
		t.Fatal("local backend has no OS sandbox")
	}
}
