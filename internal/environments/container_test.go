package environments

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

type containerFixture struct {
	tr      *tempRoot
	engine  *fakeDocker
	process *fakeProcesses
}

func newContainerFixture(t *testing.T) *containerFixture {
	tr := newTemp(t)
	engine := newFakeDocker()
	return &containerFixture{tr: tr, engine: engine, process: installFakeProcesses(t, engine)}
}

func (f *containerFixture) prepared(t *testing.T, opts util.M) (*EnvironmentRunner, *plugin.Ctx, util.M, util.M) {
	t.Helper()
	r := f.engine.runner(t, containerProfile(t, f.tr.base, opts))
	ctx := newCtx(t, f.tr.root, "attempt", true)
	handle := persist(t, r, ctx)
	report, err := r.Prepare(ctx)
	noErr(t, err)
	return r, ctx, handle, report
}

func (f *containerFixture) container(handle util.M) util.M {
	return f.engine.containers[util.S(handle["name"])]
}

func setTime(t *testing.T, value float64) {
	old := timeNow
	timeNow = func() float64 { return value }
	t.Cleanup(func() { timeNow = old })
}

func TestPlanAndDoctorOnlyInspectWithoutProvisioning(t *testing.T) {
	f := newContainerFixture(t)
	r := f.engine.runner(t, containerProfile(t, f.tr.base, nil))
	ctx := newCtx(t, f.tr.root, "attempt", false)
	doctor := r.Doctor(ctx)
	if doctor["status"] != "available" || util.Get(doctor, "capabilities", "lifetime", "state") != "unknown" {
		t.Fatal(doctor)
	}
	handle := persist(t, r, ctx)
	if util.Get(handle, "image", "id") != testImage || f.engine.count("run") != 0 {
		t.Fatal(handle["image"])
	}
	for _, key := range Roots {
		if lexists(ctxRoot(ctx, key)) {
			t.Fatal(key)
		}
	}
	if entries, _ := os.ReadDir(ctx.RunDir); len(entries) != 0 {
		t.Fatal("plan wrote into the report directory")
	}
	if strings.Contains(util.Compact(handle), imageSecret) || !util.Contains(util.Strings(util.Get(handle, "image", "environment_names")), "IMAGE_SECRET") {
		t.Fatal(handle["image"])
	}
}

func TestCreateHasOnlyOwnedMountsFixedSecurityAndIndependentLifetime(t *testing.T) {
	f := newContainerFixture(t)
	f.tr.fixture("fixtures")
	_, ctx, handle, report := f.prepared(t, util.M{
		"mounts":   []any{util.M{"source": "fixtures", "target": "/fixtures", "access": "read"}},
		"required": toAnyStrings(RequiredCapabilities)})
	var create []string
	for _, c := range f.engine.calls {
		if c[0] == "run" {
			create = c
			break
		}
	}
	for _, flag := range []string{"--rm", "--init", "--read-only", "--no-healthcheck"} {
		if !util.Contains(create, flag) {
			t.Fatal(flag)
		}
	}
	if option(create, "--network") != "none" || option(create, "--cap-drop") != "ALL" || option(create, "--user") != "0:0" ||
		option(create, "--entrypoint") != "/usr/bin/env" || !util.Contains(create, "no-new-privileges=true") {
		t.Fatal(create)
	}
	tail := create[len(create)-5 : len(create)-1]
	if strings.Join(tail, " ") != testImage+" -i PATH="+ContainerPath+" /bin/sleep" {
		t.Fatal(tail)
	}
	left, _ := strconv.Atoi(lastArg(create))
	if left <= 0 || left > 3600 {
		t.Fatal(left)
	}
	if strings.HasPrefix(util.S(handle["worker_user"]), "0:") {
		t.Fatal(handle["worker_user"])
	}
	mounts := options(create, "--mount")
	if len(mounts) != 5 {
		t.Fatal(mounts)
	}
	readonly := false
	for _, m := range mounts {
		if !strings.Contains(m, "bind-recursive=disabled") || strings.Contains(m, ctx.RunDir) || strings.Contains(m, "docker.sock") {
			t.Fatal(m)
		}
		if strings.Contains(m, "target=/fixtures") && strings.Contains(m, "readonly") {
			readonly = true
		}
	}
	if !readonly {
		t.Fatal(mounts)
	}
	for _, capability := range RequiredCapabilities {
		if util.Get(report, "capabilities", capability, "state") != "enforced" {
			t.Fatal(capability)
		}
	}
	if util.Get(report, "image", "platform") != "linux/amd64" || util.Get(report, "platform", "system") != "Linux" {
		t.Fatal(report)
	}
	if strings.Contains(util.Compact(report), imageSecret) {
		t.Fatal("image env value leaked")
	}
	envs := options(create, "--env")
	if !util.Contains(envs, "IMAGE_SECRET=") || !util.Contains(envs, "BASH_ENV=") {
		t.Fatal(envs)
	}
}

func TestSinglePersistentContainerForSetupExecuteNarrateAndRecovery(t *testing.T) {
	f := newContainerFixture(t)
	r, ctx, handle, report := f.prepared(t, util.M{"tools": []any{util.M{"name": "probe", "argv": []any{"probe", "--version"}}}})
	inventory := func() int {
		n := 0
		for _, e := range f.process.executions {
			if strings.Contains(lastArg(e["argv"].([]string)), "version") {
				n++
			}
		}
		return n
	}
	before := inventory()
	_, err := r.ShellChecked("install-user-tool", ctx, 600, nil)
	noErr(t, err)
	env, err := r.WorkerEnv(ctx, nil)
	noErr(t, err)
	execute, err := r.Wrap([]string{"probe", "execute"}, ctx, env)
	noErr(t, err)
	narrate, err := r.Wrap([]string{"probe", "narrate"}, ctx, env)
	noErr(t, err)
	identity := util.S(f.container(handle)["Id"])
	if !util.Contains(execute, identity) || !util.Contains(narrate, identity) || f.engine.count("run") != 1 {
		t.Fatal(execute)
	}
	again, err := r.Prepare(ctx)
	noErr(t, err)
	if util.Compact(again) != util.Compact(report) {
		t.Fatal("prepare is not idempotent")
	}
	recovered := f.engine.runner(t, r.Profile())
	plan, err := recovered.Plan(ctx)
	noErr(t, err)
	if util.Compact(plan) != util.Compact(handle) {
		t.Fatal("recovered plan differs")
	}
	rr, err := recovered.Prepare(ctx)
	noErr(t, err)
	if rr["status"] != "attached" || util.Maps(util.Get(rr, "inventory", "tools"))[0]["status"] != "unknown" {
		t.Fatal(rr)
	}
	if inventory() != before || f.engine.count("run") != 1 {
		t.Fatal("recovery replayed inventory or creation")
	}
	released, _ := recovered.Release(ctx)
	if released["confirmed"] != true {
		t.Fatal(released)
	}
}

func TestExecPreservesEmptyArgumentValues(t *testing.T) {
	f := newContainerFixture(t)
	r, ctx, _, _ := f.prepared(t, nil)
	t.Cleanup(func() { r.Release(ctx) })
	env, _ := r.WorkerEnv(ctx, nil)
	argv, err := r.Wrap([]string{"claude", "--tools", ""}, ctx, env)
	noErr(t, err)
	if strings.Join(argv[len(argv)-3:], "|") != "claude|--tools|" {
		t.Fatal(argv)
	}
}

func TestExecContainsEnvNamesNotValuesOrImageDefaults(t *testing.T) {
	f := newContainerFixture(t)
	r, ctx, handle, _ := f.prepared(t, nil)
	supplied := "synthetic-explicit-auth-value"
	ctx.Env = map[string]string{"DECLARED_TOKEN": supplied, "DISCARD": "synthetic"}
	ctx.UnsetEnv = []string{"DISCARD"}
	env, err := r.WorkerEnv(ctx, nil)
	noErr(t, err)
	argv, err := r.Wrap([]string{"tool", "task"}, ctx, env)
	noErr(t, err)
	if strings.Contains(strings.Join(argv, " "), supplied) {
		t.Fatal("value in argv")
	}
	envs := options(argv, "--env")
	if !util.Contains(envs, "DECLARED_TOKEN") || util.Contains(envs, "DISCARD") || util.Contains(envs, "IMAGE_SECRET") {
		t.Fatal(envs)
	}
	script := argv[len(argv)-4]
	if !strings.Contains(script, "/usr/bin/env -i") || !strings.Contains(script, `"DECLARED_TOKEN=${DECLARED_TOKEN}"`) {
		t.Fatal(script)
	}
	if argv[0] != "/synthetic/docker" || option(argv, "--host") != "unix:///var/run/docker.sock" ||
		option(argv, "--user") != handle["worker_user"] || env["DECLARED_TOKEN"] != supplied {
		t.Fatal(argv)
	}
	if option(argv, "--config") != handle["docker_config_dir"] {
		t.Fatal(argv)
	}
}

func TestWriteMountIsAttemptCopyAndReadSourceIsUntouched(t *testing.T) {
	f := newContainerFixture(t)
	source := f.tr.fixture("writable-start")
	r, ctx, handle, _ := f.prepared(t, util.M{"mounts": []any{util.M{"source": "writable-start", "target": "/output", "access": "write"}}})
	copied := util.S(util.Maps(handle["mounts"])[0]["host_source"])
	if !relativeTo(copied, ctx.Workspace) || copied == source {
		t.Fatal(copied)
	}
	if readFile(t, filepath.Join(copied, "input.txt")) != readFile(t, filepath.Join(source, "input.txt")) {
		t.Fatal("snapshot differs")
	}
	writeFile(t, filepath.Join(copied, "input.txt"), "changed in attempt")
	if readFile(t, filepath.Join(source, "input.txt")) != "synthetic starting data\n" {
		t.Fatal("source changed")
	}
	released, _ := r.Release(ctx)
	if released["confirmed"] != true || !lexists(copied) {
		t.Fatal("release must preserve evidence for the coordinator's archive")
	}
}

func TestSourceDriftBetweenPlanAndPrepareIsRejected(t *testing.T) {
	f := newContainerFixture(t)
	source := f.tr.fixture("fixtures")
	r := f.engine.runner(t, containerProfile(t, f.tr.base, util.M{"mounts": []any{util.M{"source": "fixtures", "target": "/fixtures", "access": "read"}}}))
	ctx := newCtx(t, f.tr.root, "attempt", true)
	persist(t, r, ctx)
	writeFile(t, filepath.Join(source, "input.txt"), "changed after plan")
	_, err := r.Prepare(ctx)
	wantErr(t, err, KindEnvironment, "changed after planning")
	if f.engine.count("run") != 0 {
		t.Fatal("created after drift")
	}
}

func TestRecoveryMissingOrExpiredNeverCreatesOrRestarts(t *testing.T) {
	f := newContainerFixture(t)
	r := f.engine.runner(t, containerProfile(t, f.tr.base, nil))
	ctx := newCtx(t, f.tr.root, "attempt", true)
	handle := persist(t, r, ctx)
	recovered := f.engine.runner(t, r.Profile())
	plan, err := recovered.Plan(ctx)
	noErr(t, err)
	if util.Compact(plan) != util.Compact(handle) {
		t.Fatal("plan changed")
	}
	_, err = recovered.Prepare(ctx)
	wantErr(t, err, KindEnvironment, "recovery never creates")
	if f.engine.count("run") != 0 {
		t.Fatal("recovery created")
	}
	expires, _ := util.Num(handle["expires_at"])
	setTime(t, expires+1)
	_, err = r.Prepare(ctx)
	wantErr(t, err, KindEnvironment, "expired")
	if f.engine.count("run") != 0 {
		t.Fatal("expired plan created")
	}
}

func TestStoppedOrPolicyTamperedContainerIsNeverRestarted(t *testing.T) {
	f := newContainerFixture(t)
	r, ctx, handle, _ := f.prepared(t, nil)
	data := f.container(handle)
	for _, c := range []struct {
		section, key string
		bad          any
	}{{"State", "Running", false}, {"HostConfig", "Privileged", true}, {"HostConfig", "ReadonlyRootfs", false},
		{"Config", "User", "1000:1000"}, {"HostConfig", "NetworkMode", "host"}} {
		section := data[c.section].(util.M)
		original := section[c.key]
		section[c.key] = c.bad
		env, _ := r.WorkerEnv(ctx, nil)
		_, err := r.Wrap([]string{"tool"}, ctx, env)
		wantErr(t, err, KindEnvironment, "")
		section[c.key] = original
	}
	if f.engine.count("run") != 1 {
		t.Fatal("restarted")
	}
}

func TestTimeoutStopsWorkersAndPreservesTheSameEnvironmentForTeardown(t *testing.T) {
	f := newContainerFixture(t)
	r, ctx, handle, _ := f.prepared(t, nil)
	identity := util.S(f.container(handle)["Id"])
	f.process.handler = func(argv []string) util.M { return result("partial", nil, true) }
	response, err := r.ShellChecked("sleep 600", ctx, 0.1, nil)
	noErr(t, err)
	abort := util.MapOr(response["environment_abort"])
	if response["timed_out"] != true || abort["confirmed"] != true || abort["environment_retained"] != true {
		t.Fatal(response)
	}
	var stop []string
	for _, c := range f.engine.calls {
		if c[0] == "exec" {
			stop = c
			break
		}
	}
	if option(stop, "--user") != handle["worker_user"] || option(stop, "--workdir") != "/" ||
		!util.Contains(stop, identity) || !util.Contains(stop, StopWorkers) ||
		lastArg(stop) != strings.SplitN(util.S(handle["worker_user"]), ":", 2)[0] {
		t.Fatal(stop)
	}
	if f.engine.count("container", "rm", "--force", identity) != 0 {
		t.Fatal("abort removed the retained environment")
	}
	f.process.handler = nil
	teardown, err := r.ShellChecked("perform-declared-teardown", ctx, 600, nil)
	noErr(t, err)
	if teardown["exit_code"] != int64(0) {
		t.Fatal(teardown)
	}
	argv, err := f.engine.runner(t, r.Profile()).Wrap([]string{"tool"}, ctx, map[string]string{})
	noErr(t, err)
	if !util.Contains(argv, identity) {
		t.Fatal(argv)
	}
	released, _ := r.Release(ctx)
	if released["confirmed"] != true || len(f.engine.containers) != 0 || f.engine.count("run") != 1 {
		t.Fatal(released)
	}
}

// fakeDockerScript makes the synthetic Docker binary a real executable so
// plugin.BaseHarness.Run can launch the wrapped argv.
func fakeDockerScript(t *testing.T, r *EnvironmentRunner, f *fakeDocker, dir, body string) {
	script := filepath.Join(dir, "synthetic-docker")
	writeFile(t, script, "#!/bin/sh\n"+body+"\n")
	noErr(t, os.Chmod(script, 0o755))
	r.SetDocker(script, f.call)
}

func TestHarnessTimeoutStopsWorkersWithoutReleasingCleanupEnvironment(t *testing.T) {
	f := newContainerFixture(t)
	r, ctx, _, _ := f.prepared(t, nil)
	fakeDockerScript(t, r, f.engine, f.tr.root, "sleep 30")
	ctx.Runner = r
	ctx.EnvironmentProfile = r.Profile()
	ctx.Env = r.EnvironmentEnv(ctx)
	ctx.Timeout = 1
	h := &plugin.BaseHarness{}
	plugin.Bind(h)
	response, err := h.Run(ctx, []string{"synthetic-worker"}, "execute", plugin.RunOpts{})
	noErr(t, err)
	if response["timed_out"] != true || util.Get(response, "environment_abort", "environment_retained") != true {
		t.Fatal(response)
	}
	teardown, err := r.ShellChecked("perform-declared-teardown", ctx, 600, nil)
	noErr(t, err)
	if teardown["exit_code"] != int64(0) || f.engine.count("exec") != 1 || f.engine.count("container", "rm") != 0 {
		t.Fatal(f.engine.calls)
	}
}

func TestHarnessInterruptionStopsWorkersBeforeReturningControl(t *testing.T) {
	f := newContainerFixture(t)
	r, ctx, _, _ := f.prepared(t, nil)
	fakeDockerScript(t, r, f.engine, f.tr.root, "sleep 30")
	ctx.Runner = r
	ctx.EnvironmentProfile = r.Profile()
	ctx.Env = r.EnvironmentEnv(ctx)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	ctx.Go = cancelled
	h := &plugin.BaseHarness{}
	plugin.Bind(h)
	_, err := h.Run(ctx, []string{"synthetic-worker"}, "execute", plugin.RunOpts{})
	if !util.Interrupted(err) {
		t.Fatalf("expected interruption, got %v", err)
	}
	if f.engine.count("exec") != 1 {
		t.Fatal("interruption did not stop workers")
	}
	ctx.Go = nil
	teardown, err := r.ShellChecked("perform-declared-teardown", ctx, 600, nil)
	noErr(t, err)
	if teardown["exit_code"] != int64(0) || f.engine.count("container", "rm") != 0 {
		t.Fatal(teardown)
	}
}

func TestUnconfirmedWorkerStopFallsBackToOwnedContainerRemoval(t *testing.T) {
	f := newContainerFixture(t)
	r, ctx, _, _ := f.prepared(t, nil)
	f.engine.failCancel = true
	stopped := r.Abort(ctx)
	if stopped["confirmed"] != true || stopped["environment_retained"] != false ||
		!strings.Contains(util.S(stopped["cancellation_error"]), "could not be confirmed") || len(f.engine.containers) != 0 {
		t.Fatal(stopped)
	}
}

func TestFailedStopAndRemovalNeverClaimConfirmedCleanup(t *testing.T) {
	f := newContainerFixture(t)
	r, ctx, handle, _ := f.prepared(t, nil)
	f.engine.failCancel, f.engine.failRemove = true, true
	stopped := r.Abort(ctx)
	if stopped["confirmed"] != false || stopped["environment_retained"] != false || f.container(handle) == nil {
		t.Fatal(stopped)
	}
}

func TestExpiryAtExecutionStopsOwnedContainerAndDoesNotExtendDeadline(t *testing.T) {
	f := newContainerFixture(t)
	r, ctx, handle, _ := f.prepared(t, nil)
	expires, _ := util.Num(handle["expires_at"])
	setTime(t, expires+1)
	_, err := r.Wrap([]string{"tool"}, ctx, map[string]string{})
	wantErr(t, err, KindEnvironment, "lifetime expired")
	if len(f.engine.containers) != 0 || f.engine.count("run") != 1 {
		t.Fatal(f.engine.containers)
	}
}

func TestCleanupVerifiesOwnershipThenUsesFullIDAndIsIdempotent(t *testing.T) {
	f := newContainerFixture(t)
	r, ctx, handle, _ := f.prepared(t, nil)
	identity := util.S(f.container(handle)["Id"])
	first, _ := r.Release(ctx)
	second, _ := r.Release(ctx)
	if first["confirmed"] != true || second["status"] != "already_absent" {
		t.Fatal(first, second)
	}
	var removes [][]string
	for _, c := range f.engine.calls {
		if hasPrefix(c, "container", "rm") {
			removes = append(removes, c)
		}
	}
	if len(removes) != 1 || strings.Join(removes[0], " ") != "container rm --force "+identity {
		t.Fatal(removes)
	}
}

func TestForeignSameNameContainerIsNeverRemovedOrExecuted(t *testing.T) {
	f := newContainerFixture(t)
	r, ctx, handle, _ := f.prepared(t, nil)
	f.container(handle)["Config"].(util.M)["Labels"] = util.M{}
	if r.Abort(ctx)["confirmed"] != false || f.engine.count("exec") != 0 {
		t.Fatal("abort acted on a foreign container")
	}
	cleanup, _ := r.Release(ctx)
	if cleanup["confirmed"] != false || !strings.Contains(util.S(cleanup["error"]), "ownership") || f.engine.count("container", "rm") != 0 {
		t.Fatal(cleanup)
	}
	_, err := r.Wrap([]string{"tool"}, ctx, map[string]string{})
	wantErr(t, err, KindEnvironment, "ownership")
}

func TestNullOrMissingLabelsAreNotCleanupAuthority(t *testing.T) {
	f := newContainerFixture(t)
	r, ctx, handle, _ := f.prepared(t, nil)
	f.container(handle)["Config"].(util.M)["Labels"] = nil
	cleanup, _ := r.Release(ctx)
	if cleanup["confirmed"] != false || f.engine.count("container", "rm") != 0 {
		t.Fatal(cleanup)
	}
}

func TestRecoveryCleanupDoesNotNeedOriginalFixtureSources(t *testing.T) {
	f := newContainerFixture(t)
	source := f.tr.fixture("fixtures")
	r, ctx, _, _ := f.prepared(t, util.M{"mounts": []any{util.M{"source": "fixtures", "target": "/fixtures", "access": "read"}}})
	noErr(t, os.Rename(source, filepath.Join(filepath.Dir(source), "moved-source")))
	recovered := f.engine.runner(t, r.Profile())
	if released, _ := recovered.Release(ctx); released["confirmed"] != true {
		t.Fatal(released)
	}
}

func TestInterruptedPreparationCannotBeReclassifiedAsPreparedOnRecovery(t *testing.T) {
	f := newContainerFixture(t)
	r, ctx, handle, _ := f.prepared(t, nil)
	noErr(t, r.writeMarker(ctx, handle, "preparing", false))
	recovered := f.engine.runner(t, r.Profile())
	_, err := recovered.Prepare(ctx)
	wantErr(t, err, KindEnvironment, "preparation was not confirmed")
	if f.engine.count("run") != 1 {
		t.Fatal("recreated")
	}
	if released, _ := recovered.Release(ctx); released["confirmed"] != true {
		t.Fatal(released)
	}
}

func TestReplacementBetweenInspectAndRemoveCannotDeleteNewContainer(t *testing.T) {
	f := newContainerFixture(t)
	r, ctx, handle, _ := f.prepared(t, nil)
	identity := util.S(f.container(handle)["Id"])
	f.engine.replaceAtRemove = true
	cleanup, _ := r.Release(ctx)
	if cleanup["confirmed"] != true || f.container(handle) == nil || f.container(handle)["Id"] == identity ||
		f.engine.count("container", "rm", "--force", identity) != 1 {
		t.Fatal(cleanup)
	}
}

func TestCleanupFindsRenamedOwnedContainerByLabel(t *testing.T) {
	f := newContainerFixture(t)
	r, ctx, handle, _ := f.prepared(t, nil)
	owned := f.container(handle)
	f.engine.remove(util.S(handle["name"]))
	owned["Name"] = "/renamed-by-host"
	f.engine.put("renamed-by-host", owned)
	cleanup, _ := r.Release(ctx)
	if cleanup["confirmed"] != true || len(f.engine.containers) != 0 ||
		f.engine.count("container", "rm", "--force", util.S(owned["Id"])) != 1 {
		t.Fatal(cleanup)
	}
}

func TestCleanupOutageOrRemovalFailureStaysUnconfirmed(t *testing.T) {
	f := newContainerFixture(t)
	r, ctx, handle, _ := f.prepared(t, nil)
	f.engine.unreachable = true
	failed, err := r.Release(ctx)
	noErr(t, err)
	if failed["status"] != "failed" || failed["confirmed"] != false {
		t.Fatal(failed)
	}
	f.engine.unreachable = false
	f.engine.failRemove = true
	if again, _ := r.Release(ctx); again["confirmed"] != false || f.container(handle) == nil {
		t.Fatal(again)
	}
	f.engine.failRemove = false
	if last, _ := r.Release(ctx); last["confirmed"] != true {
		t.Fatal(last)
	}
}

func TestCreateFailureAfterResourceExistsCleansItAndCannotRetry(t *testing.T) {
	f := newContainerFixture(t)
	r := f.engine.runner(t, containerProfile(t, f.tr.base, nil))
	ctx := newCtx(t, f.tr.root, "attempt", true)
	persist(t, r, ctx)
	f.engine.failCreate = true
	_, err := r.Prepare(ctx)
	e := wantErr(t, err, KindEnvironment, "creation failed")
	if !strings.Contains(strings.Join(e.Notes, "\n"), "cleanup") || len(f.engine.containers) != 0 {
		t.Fatal(e.Notes)
	}
	_, err = r.Prepare(ctx)
	wantErr(t, err, KindEnvironment, "")
	if f.engine.count("run") != 1 {
		t.Fatal("retried creation")
	}
}

func TestFailedInventoryReleasesContainer(t *testing.T) {
	f := newContainerFixture(t)
	r := f.engine.runner(t, containerProfile(t, f.tr.base, util.M{"tools": []any{util.M{"name": "missing", "argv": []any{"missing", "--version"}}}}))
	ctx := newCtx(t, f.tr.root, "attempt", true)
	persist(t, r, ctx)
	f.process.handler = func(argv []string) util.M {
		if lastArg(argv) == "missing --version" {
			return result("", int64(127), false)
		}
		return f.process.defaultResult(argv)
	}
	_, err := r.Prepare(ctx)
	wantErr(t, err, KindEnvironment, "required starting-inventory")
	if len(f.engine.containers) != 0 {
		t.Fatal("container not released")
	}
}

func TestImageVolumesPlatformAndMissingCgroupSupportFailBeforeCreate(t *testing.T) {
	f := newContainerFixture(t)
	r := f.engine.runner(t, containerProfile(t, f.tr.base, nil))
	ctx := newCtx(t, f.tr.root, "attempt", true)
	f.engine.image["Config"].(util.M)["Volumes"] = util.M{"/implicit": util.M{}}
	_, err := r.Plan(ctx)
	wantErr(t, err, KindEnvironment, "VOLUMEs")
	f.engine.image["Config"].(util.M)["Volumes"] = nil
	f.engine.image["Os"] = "windows"
	_, err = r.Plan(ctx)
	wantErr(t, err, KindEnvironment, "Linux")
	f.engine.image["Os"] = "linux"
	f.engine.info["PidsLimit"] = false
	_, err = r.Plan(ctx)
	wantErr(t, err, KindEnvironment, "PidsLimit")
	if f.engine.count("run") != 0 {
		t.Fatal("created")
	}
}

func TestManagementSubprocessEnvironmentDoesNotInheritHostOrWorkerAuth(t *testing.T) {
	tr := newTemp(t)
	r := mustNew(t, containerProfile(t, tr.base, nil))
	script := filepath.Join(tr.root, "synthetic-docker")
	record := filepath.Join(tr.root, "record")
	writeFile(t, script, "#!/bin/sh\n{ printf '%s\\n' \"$@\"; env; } > "+util.ShellQuote(record)+"\nprintf '{}'\n")
	noErr(t, os.Chmod(script, 0o755))
	r.SetDocker(script, nil)
	t.Setenv("SYNTHETIC_HOST_TOKEN", "not-for-docker")
	res, err := r.docker([]string{"info"}, nil, 30)
	noErr(t, err)
	if res.Code != 0 || res.Stdout != "{}" {
		t.Fatal(res)
	}
	text := readFile(t, record)
	if strings.Contains(text, "SYNTHETIC_HOST_TOKEN") || !strings.Contains(text, "HOME=/nonexistent") ||
		!strings.Contains(text, "--config\n") || !strings.Contains(text, "--host\nunix:///var/run/docker.sock\ninfo\n") {
		t.Fatal(text)
	}
	if !strings.Contains(text, "PATH="+managerPath+"\n") {
		t.Fatal(text)
	}
}

func TestDockerUnavailableIsAnUnconfirmedEnvironmentError(t *testing.T) {
	tr := newTemp(t)
	r := mustNew(t, containerProfile(t, tr.base, nil))
	r.SetDocker(filepath.Join(tr.root, "missing-docker"), nil)
	_, err := r.docker([]string{"info"}, nil, 30)
	e := wantErr(t, err, KindEnvironment, "Docker info unavailable")
	if e.Msg != "Docker info unavailable (FileNotFoundError); check the local daemon; ownership/cleanup is unconfirmed" {
		t.Fatal(e.Msg)
	}
}
