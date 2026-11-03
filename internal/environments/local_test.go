package environments

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/royosherove/ajx/internal/util"
)

func runWrapped(t *testing.T, argv []string, cwd string, env map[string]string) string {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = util.EnvList(env)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %v", argv, err)
	}
	return string(out)
}

func TestEmptyArgumentValuesReachTheProcessAndToolProbe(t *testing.T) {
	tr := newTemp(t)
	argv := []string{"/bin/sh", "-c", `[ "$#" = 2 ] && [ "$1" = --tools ] && [ -z "$2" ] && printf '["%s","%s"]' "$1" "$2"`,
		"ajx-test", "--tools", ""}
	r := localRunner(t, tr.base, util.M{"tools": []any{util.M{"name": "arguments", "argv": toAnyStrings(argv)}}})
	ctx := newCtx(t, tr.root, "attempt", true)
	persist(t, r, ctx)
	report, err := r.Prepare(ctx)
	noErr(t, err)
	t.Cleanup(func() { r.Release(ctx) })
	if util.Get(util.Maps(util.Get(report, "inventory", "tools"))[0], "status") != "observed" {
		t.Fatal(util.Get(report, "inventory"))
	}
	workerEnv, err := r.WorkerEnv(ctx, nil)
	noErr(t, err)
	wrapped, err := r.Wrap(argv, ctx, workerEnv)
	noErr(t, err)
	if got := runWrapped(t, wrapped, ctx.Workspace, workerEnv); got != `["--tools",""]` {
		t.Fatal(got)
	}
}

func TestInvalidCommandArgumentsAreRejected(t *testing.T) {
	tr := newTemp(t)
	r := localRunner(t, tr.base, nil)
	ctx := newCtx(t, tr.root, "attempt", true)
	for _, argv := range [][]string{nil, {}, {""}, {"--invalid"}, {"echo", "\x00"}} {
		_, err := r.Wrap(argv, ctx, map[string]string{})
		wantErr(t, err, KindValue, `environment\.argv`)
	}
}

func TestPlanHasNoSideEffectsAndRequiresDurableState(t *testing.T) {
	tr := newTemp(t)
	r := localRunner(t, tr.base, nil)
	ctx := newCtx(t, tr.root, "attempt", false)
	plan, err := r.Plan(ctx)
	noErr(t, err)
	for _, key := range Roots {
		if lexists(ctxRoot(ctx, key)) {
			t.Fatalf("plan created %s", key)
		}
	}
	if entries, _ := os.ReadDir(ctx.RunDir); len(entries) != 0 {
		t.Fatal("plan wrote into the report directory")
	}
	_, err = r.Prepare(ctx)
	wantErr(t, err, KindEnvironment, "Persist")
	ctx.State["environment"] = plan
	report, err := r.Prepare(ctx)
	noErr(t, err)
	if util.Get(report, "inventory", "phase") != "initial" {
		t.Fatal(report["inventory"])
	}
	for _, capability := range RequiredCapabilities {
		if util.Get(report, "capabilities", capability, "state") != "unsupported" {
			t.Fatal(capability)
		}
	}
	if util.Get(report, "platform", "effective_uid") == nil {
		t.Fatal(report["platform"])
	}
	released, err := r.Release(ctx)
	noErr(t, err)
	if released["worker_stop"] != "caller-managed" {
		t.Fatal(released)
	}
}

func TestIncompletePersistedPlanIsNotSilentlyReplaced(t *testing.T) {
	tr := newTemp(t)
	r := localRunner(t, tr.base, nil)
	ctx := newCtx(t, tr.root, "attempt", true)
	ctx.State["environment"] = util.M{}
	_, err := r.Plan(ctx)
	wantErr(t, err, KindEnvironment, "Persist")
	if len(ctx.State["environment"].(util.M)) != 0 {
		t.Fatal("plan replaced the persisted record")
	}
}

func TestNoAmbientEnvAndUnsetWinsOverOverrides(t *testing.T) {
	tr := newTemp(t)
	r := localRunner(t, tr.base, nil)
	for k, v := range map[string]string{"AWS_ACCESS_KEY_ID": "synthetic-host-key", "ANTHROPIC_API_KEY": "synthetic-host-token",
		"HTTP_PROXY": "http://example.invalid", "XDG_CONFIG_HOME": "/synthetic/config", "HOME": "/synthetic/home"} {
		t.Setenv(k, v)
	}
	env, err := r.ChildEnvChecked(map[string]string{"EXPLICIT_TOKEN": "synthetic-declared", "DROP_ME": "value"}, []string{"DROP_*"})
	noErr(t, err)
	for _, key := range []string{"AWS_ACCESS_KEY_ID", "ANTHROPIC_API_KEY", "HTTP_PROXY", "DROP_ME"} {
		if _, ok := env[key]; ok {
			t.Fatalf("%s inherited", key)
		}
	}
	if env["HOME"] != "/nonexistent" || env["EXPLICIT_TOKEN"] != "synthetic-declared" {
		t.Fatal(env)
	}
	for _, key := range []string{"BASH_ENV", "ENV", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "DOCKER_CONFIG", "SSH_AUTH_SOCK"} {
		_, err := r.ChildEnvChecked(map[string]string{key: "synthetic"}, nil)
		wantErr(t, err, KindValue, key)
	}
}

func TestRealLocalShellHasIsolatedHomeAndPersistentUserTool(t *testing.T) {
	tr := newTemp(t)
	r := localRunner(t, tr.base, nil)
	ctx := newCtx(t, tr.root, "attempt", true)
	ctx.Env = map[string]string{"DECLARED": "present"}
	persist(t, r, ctx)
	report, err := r.Prepare(ctx)
	noErr(t, err)
	count := 0
	for _, n := range util.Strings(report["env_names"]) {
		if n == "DECLARED" {
			count++
		}
	}
	if count != 1 {
		t.Fatal(report["env_names"])
	}
	t.Setenv("AJX_AMBIENT_TEST_SECRET", "must-not-inherit")
	setup, err := r.ShellChecked(`printf "#!/bin/sh\nprintf installed\n" > "$HOME/.local/bin/probe"; `+
		`chmod +x "$HOME/.local/bin/probe"; printf "$HOME"`, ctx, 600, nil)
	noErr(t, err)
	execute, err := r.ShellChecked(`probe; printf "|%s|%s" "$DECLARED" "${AJX_AMBIENT_TEST_SECRET-unset}"`, ctx, 600, nil)
	noErr(t, err)
	if setup["stdout"] != ctx.HomeDir {
		t.Fatal(setup)
	}
	if execute["stdout"] != "installed|present|unset" || execute["exit_code"] != int64(0) {
		t.Fatal(execute)
	}
	for _, key := range []string{"cmd", "exit_code", "timed_out", "stdout", "stderr", "started_at", "stopped_at"} {
		if _, ok := execute[key]; !ok {
			t.Fatal(key)
		}
	}
	_, err = r.ShellChecked("true", ctx, 600, map[string]string{"HOME": tr.root})
	wantErr(t, err, KindValue, "HOME")
	for i := 0; i < 2; i++ {
		released, err := r.Release(ctx)
		noErr(t, err)
		if released["confirmed"] != true {
			t.Fatal(released)
		}
	}
	if !lexists(filepath.Join(ctx.HomeDir, ".local", "bin", "probe")) {
		t.Fatal("release removed host evidence")
	}
}

func TestDeclaredPathOverrideIsSupportedWithoutAmbientEnvInheritance(t *testing.T) {
	tr := newTemp(t)
	r := localRunner(t, tr.base, nil)
	ctx := newCtx(t, tr.root, "attempt", true)
	customBin := filepath.Join(ctx.Workspace, "tools")
	noErr(t, os.Mkdir(customBin, 0o755))
	writeFile(t, filepath.Join(customBin, "custom-probe"), "#!/bin/sh\nprintf explicit-path\n")
	noErr(t, os.Chmod(filepath.Join(customBin, "custom-probe"), 0o700))
	ctx.Env = map[string]string{"PATH": customBin + ":/usr/bin:/bin"}
	noErr(t, ValidateForCell(r.Profile(), util.M{"id": "explicit", "env": ctx.Env}))
	persist(t, r, ctx)
	_, err := r.Prepare(ctx)
	noErr(t, err)
	out, err := r.ShellChecked("custom-probe", ctx, 600, nil)
	noErr(t, err)
	if out["stdout"] != "explicit-path" {
		t.Fatal(out)
	}
}

func TestInventoryRecordMasksSuppliedEnvValues(t *testing.T) {
	tr := newTemp(t)
	r := localRunner(t, tr.base, util.M{"tools": []any{util.M{"name": "echo-env",
		"argv": []any{"/bin/sh", "-c", `printf %s "$EXPLICIT_TOKEN"`}}}})
	ctx := newCtx(t, tr.root, "attempt", true)
	ctx.Env = map[string]string{"EXPLICIT_TOKEN": "synthetic-only-env-value"}
	handle := persist(t, r, ctx)
	report, err := r.Prepare(ctx)
	noErr(t, err)
	if !util.Contains(util.Strings(report["env_names"]), "EXPLICIT_TOKEN") {
		t.Fatal(report["env_names"])
	}
	if strings.Contains(util.Compact(handle), "synthetic-only-env-value") || strings.Contains(util.Compact(report), "synthetic-only-env-value") {
		t.Fatal("supplied value leaked into a durable record")
	}
	if !strings.Contains(util.S(util.Maps(util.Get(report, "inventory", "tools"))[0]["stdout"]), "<redacted env:EXPLICIT_TOKEN>") {
		t.Fatal(util.Get(report, "inventory"))
	}
}

func TestCallerCanStageDeclaredExtensionsInOwnedFreshRoots(t *testing.T) {
	tr := newTemp(t)
	r := localRunner(t, tr.base, nil)
	ctx := newCtx(t, tr.root, "attempt", true)
	writeFile(t, filepath.Join(ctx.HomeDir, "declared-skill.txt"), "synthetic selected skill")
	_, err := r.Plan(ctx)
	wantErr(t, err, KindValue, "must start empty")
	ownAll(ctx)
	persist(t, r, ctx)
	report, err := r.Prepare(ctx)
	noErr(t, err)
	if report["status"] != "prepared" {
		t.Fatal(report["status"])
	}
}

func TestRootOverlapReportAccessAndSymlinkRootsFailClosed(t *testing.T) {
	tr := newTemp(t)
	for _, pair := range [][2]string{{"home_dir", "workspace"}, {"cache_dir", "run_dir"}} {
		r := localRunner(t, tr.base, nil)
		ctx := newCtx(t, tr.root, pair[0], true)
		replacement := ctx.RunDir
		if pair[1] != "run_dir" {
			replacement = ctxRoot(ctx, pair[1])
		}
		setRoot(ctx, pair[0], replacement)
		_, err := r.Plan(ctx)
		wantErr(t, err, KindValue, "")
	}
	r := localRunner(t, tr.base, nil)
	ctx := newCtx(t, tr.root, "symlink", true)
	noErr(t, os.Remove(ctx.HomeDir))
	noErr(t, os.Symlink(ctx.CacheDir, ctx.HomeDir))
	_, err := r.Plan(ctx)
	wantErr(t, err, KindValue, "symlink")
}

func TestLocalInventoryMissingToolAndRecoveryDoNotReplay(t *testing.T) {
	tr := newTemp(t)
	r := localRunner(t, tr.base, util.M{"tools": []any{util.M{"name": "missing",
		"argv": []any{"ajx-definitely-absent-tool"}, "required": true}}})
	ctx := newCtx(t, tr.root, "attempt", true)
	persist(t, r, ctx)
	_, err := r.Prepare(ctx)
	wantErr(t, err, KindEnvironment, "required starting-inventory")
	_, err = r.Prepare(ctx)
	wantErr(t, err, KindEnvironment, "previously failed")
	_, err = mustNew(t, r.Profile()).Prepare(ctx)
	wantErr(t, err, KindEnvironment, "not confirmed")
}

func TestLocalRecoveryWithoutConfirmedPreparationIsRejected(t *testing.T) {
	tr := newTemp(t)
	r := localRunner(t, tr.base, nil)
	ctx := newCtx(t, tr.root, "attempt", true)
	persist(t, r, ctx)
	recovered := mustNew(t, r.Profile())
	plan, err := recovered.Plan(ctx)
	noErr(t, err)
	if util.Compact(plan) != util.Compact(ctx.State["environment"]) {
		t.Fatal("recovered plan differs from the persisted plan")
	}
	_, err = recovered.Prepare(ctx)
	wantErr(t, err, KindEnvironment, "preparation was not confirmed")
	_, err = r.Prepare(ctx)
	noErr(t, err)
	report, err := mustNew(t, r.Profile()).Prepare(ctx)
	noErr(t, err)
	if util.Get(report, "inventory", "phase") != "recovered" || report["status"] != "attached" {
		t.Fatal(report)
	}
}

func TestReleasedOrReplacedLocalDirectoriesCannotBeReused(t *testing.T) {
	tr := newTemp(t)
	r := localRunner(t, tr.base, nil)
	ctx := newCtx(t, tr.root, "attempt", true)
	persist(t, r, ctx)
	_, err := r.Prepare(ctx)
	noErr(t, err)
	noErr(t, os.Rename(ctx.Workspace, filepath.Join(filepath.Dir(ctx.Workspace), "original-workspace")))
	noErr(t, os.Mkdir(ctx.Workspace, 0o700))
	_, err = mustNew(t, r.Profile()).Prepare(ctx)
	wantErr(t, err, KindEnvironment, "identity changed")
	r.Release(ctx)
	_, err = mustNew(t, r.Profile()).Prepare(ctx)
	wantErr(t, err, KindEnvironment, "released")
}

func TestLocalTimeoutKillsProcessGroupAndBoundsOutput(t *testing.T) {
	tr := newTemp(t)
	r := localRunner(t, tr.base, nil)
	ctx := newCtx(t, tr.root, "attempt", true)
	persist(t, r, ctx)
	_, err := r.Prepare(ctx)
	noErr(t, err)
	out, err := r.ShellChecked(`sleep 60 & child=$!; printf '%s\n' "$child"; wait`, ctx, 0.3, nil)
	noErr(t, err)
	if out["timed_out"] != true || out["exit_code"] != nil {
		t.Fatal(out)
	}
	child, err := strconv.Atoi(strings.TrimSpace(util.S(out["stdout"])))
	noErr(t, err)
	gone := false
	for i := 0; i < 100; i++ {
		if syscall.Kill(child, 0) != nil || isZombie(child) {
			gone = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !gone {
		t.Fatal("timed-out shell left a running child")
	}
	second := localRunner(t, tr.base, nil)
	ctx2 := newCtx(t, tr.root, "bounded-output", true)
	persist(t, second, ctx2)
	_, err = second.Prepare(ctx2)
	noErr(t, err)
	out, err = second.ShellChecked("i=0; while [ $i -lt 15000 ]; do printf x; i=$((i+1)); done", ctx2, 10, nil)
	noErr(t, err)
	if len(util.S(out["stdout"])) != 8000 {
		t.Fatal(len(util.S(out["stdout"])))
	}
	_, err = second.ShellChecked("true", ctx2, -1, nil)
	wantErr(t, err, KindValue, "environment.timeout")
}

func isZombie(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
	}
	fields := strings.Fields(string(data))
	return len(fields) > 2 && fields[2] == "Z"
}

func TestLocalAbortPreservesCleanupContextUntilFinalRelease(t *testing.T) {
	tr := newTemp(t)
	r := localRunner(t, tr.base, nil)
	ctx := newCtx(t, tr.root, "attempt", true)
	persist(t, r, ctx)
	_, err := r.Prepare(ctx)
	noErr(t, err)
	t.Cleanup(func() { r.Release(ctx) })
	if outcome := r.Abort(ctx); outcome["worker_stop"] != "caller-managed" || outcome["confirmed"] != false {
		t.Fatal(outcome)
	}
	cleanup, err := r.ShellChecked(`printf cleaned > "$HOME/cleanup-result"`, ctx, 600, nil)
	noErr(t, err)
	if cleanup["exit_code"] != int64(0) {
		t.Fatal(cleanup)
	}
	recovered := mustNew(t, r.Profile())
	got, err := recovered.ShellChecked(`cat "$HOME/cleanup-result"`, ctx, 600, nil)
	noErr(t, err)
	if got["stdout"] != "cleaned" {
		t.Fatal(got)
	}
	released, _ := r.Release(ctx)
	if released["confirmed"] != true {
		t.Fatal(released)
	}
	_, err = recovered.ShellChecked("true", ctx, 600, nil)
	wantErr(t, err, KindEnvironment, "released")
	// The plugin contract reports the same failure as a record instead of running anything.
	if res := recovered.Shell("true", ctx, 600, nil); res["exit_code"] != nil || !strings.Contains(util.S(res["error"]), "released") {
		t.Fatal(res)
	}
}
