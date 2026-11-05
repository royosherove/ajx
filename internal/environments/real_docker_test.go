package environments

// Real Docker boundary tests: they run only when AJX_TEST_CONTAINER_IMAGE is a
// preloaded immutable Linux image ID/digest with Python 3, /bin/sh,
// /usr/bin/env, /bin/sleep, id and uname (for example a CI-resolved
// python:3-alpine). They never pull images, use credentials, or call an agent.

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

func realImage(t *testing.T) string {
	image := os.Getenv("AJX_TEST_CONTAINER_IMAGE")
	if image == "" {
		t.Skip("set AJX_TEST_CONTAINER_IMAGE to opt into real Docker")
	}
	return image
}

func makeReal(t *testing.T, tr *tempRoot, name string, opts util.M) (*EnvironmentRunner, *plugin.Ctx, util.M, util.M) {
	t.Helper()
	all := util.M{"image": realImage(t)}
	for k, v := range opts {
		all[k] = v
	}
	r := mustNew(t, containerProfile(t, tr.base, all))
	ctx := newCtx(t, tr.root, name, true)
	handle := persist(t, r, ctx)
	t.Cleanup(func() {
		cleanup, err := r.Release(ctx)
		if err != nil || cleanup["confirmed"] != true {
			t.Errorf("release: %v %v", cleanup, err)
		}
	})
	report, err := r.Prepare(ctx)
	noErr(t, err)
	return r, ctx, handle, report
}

func shell(t *testing.T, r *EnvironmentRunner, ctx *plugin.Ctx, command string, timeout float64) util.M {
	t.Helper()
	res, err := r.ShellChecked(command, ctx, timeout, nil)
	noErr(t, err)
	return res
}

func TestRealPersistentToolsWriteAndNetworkBoundariesTwoAttemptsAndRelease(t *testing.T) {
	realImage(t)
	tr := newTemp(t)
	fixture := tr.fixture("fixtures")
	unrelated := filepath.Join(tr.root, "unrelated-host-directory")
	noErr(t, os.Mkdir(unrelated, 0o755))
	sentinel := filepath.Join(unrelated, "private-sentinel.txt")
	writeFile(t, sentinel, "synthetic unrelated host data")
	opts := util.M{"mounts": []any{util.M{"source": "fixtures", "target": "/fixtures", "access": "read"}},
		"tools":    []any{util.M{"name": "python", "argv": []any{"python3", "--version"}}},
		"required": toAnyStrings(RequiredCapabilities)}
	first, a, planA, report := makeReal(t, tr, "first", opts)
	second, b, planB, _ := makeReal(t, tr, "second", opts)
	if planA["owner"] == planB["owner"] || util.Maps(util.Get(report, "inventory", "tools"))[0]["status"] != "observed" {
		t.Fatal(report["inventory"])
	}
	setup := shell(t, first, a, `mkdir -p "$HOME/.local/bin"; `+
		`printf '#!/bin/sh\nprintf "installed-tool-ok\\n"\n' > "$HOME/.local/bin/ajx-probe"; `+
		`chmod +x "$HOME/.local/bin/ajx-probe"; printf persistent > installed-during-setup.txt`, 600)
	if setup["exit_code"] != int64(0) {
		t.Fatal(setup)
	}
	execute := shell(t, first, a, "ajx-probe; cat installed-during-setup.txt", 600)
	if execute["exit_code"] != int64(0) || execute["stdout"] != "installed-tool-ok\npersistent" {
		t.Fatal(execute)
	}
	checks := map[string]string{
		"read fixture":           "test -r /fixtures/input.txt",
		"read-only fixture":      "if printf bad >> /fixtures/input.txt 2>/dev/null; then exit 90; fi",
		"read-only root":         "if printf bad > /ajx-forbidden-root 2>/dev/null; then exit 90; fi",
		"unrelated host absent":  "test ! -e " + util.ShellQuote(sentinel),
		"report absent":          "test ! -e " + util.ShellQuote(a.RunDir),
		"other workspace absent": "test ! -e " + util.ShellQuote(b.Workspace),
		"cannot stop supervisor": "if kill -STOP 1 2>/dev/null; then exit 90; fi",
		"non-root identity":      `test "$(id -u)" != 0`,
	}
	for label, command := range checks {
		if res := shell(t, first, a, command, 600); res["exit_code"] != int64(0) {
			t.Fatalf("%s: %v", label, res)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	noErr(t, err)
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	script := "import errno,os,socket\n" +
		"assert sorted(os.listdir('/sys/class/net')) == ['lo']\n" +
		"s=socket.socket();s.settimeout(1)\n" +
		"try:s.connect(('127.0.0.1'," + strconv.Itoa(port) + "))\n" +
		"except OSError:pass\n" +
		"else:raise SystemExit('host listener unexpectedly reachable')\n"
	if res := shell(t, first, a, "python3 -c "+util.ShellQuote(script), 600); res["exit_code"] != int64(0) {
		t.Fatal(res)
	}
	isolated := shell(t, second, b, `test ! -e installed-during-setup.txt; test ! -e "$HOME/.local/bin/ajx-probe"`, 600)
	if isolated["exit_code"] != int64(0) || readFile(t, filepath.Join(fixture, "input.txt")) != "synthetic starting data\n" {
		t.Fatal(isolated)
	}
	for _, pair := range []struct {
		r   *EnvironmentRunner
		ctx *plugin.Ctx
		h   util.M
	}{{first, a, planA}, {second, b, planB}} {
		released, _ := pair.r.Release(pair.ctx)
		found, err := pair.r.find(pair.h, "")
		if released["confirmed"] != true || err != nil || found != nil {
			t.Fatal(released, err)
		}
	}
	if readFile(t, filepath.Join(a.Workspace, "installed-during-setup.txt")) != "persistent" {
		t.Fatal("host evidence lost")
	}
}

func TestRealTimeoutStopsDetachedWorkersAndKeepsEnvironmentForTeardown(t *testing.T) {
	realImage(t)
	tr := newTemp(t)
	r, ctx, handle, _ := makeReal(t, tr, "timeout", nil)
	worker := "from pathlib import Path; import time\nwhile True:\n with Path('heartbeat').open('a') as out: out.write('alive\\n')\n time.sleep(0.02)\n"
	launch := "import subprocess,time\nfrom pathlib import Path\n" +
		"subprocess.Popen(['python3','-c'," + pyRepr(worker) + "],start_new_session=True)\n" +
		"Path('before-timeout').write_text('evidence')\ntime.sleep(120)\n"
	response := shell(t, r, ctx, "python3 -c "+util.ShellQuote(launch), 1)
	abort := util.MapOr(response["environment_abort"])
	if response["timed_out"] != true || abort["confirmed"] != true || abort["environment_retained"] != true {
		t.Fatal(response)
	}
	if found, err := r.find(handle, ""); err != nil || found == nil {
		t.Fatal("environment not retained")
	}
	heartbeat := readFile(t, filepath.Join(ctx.Workspace, "heartbeat"))
	time.Sleep(100 * time.Millisecond)
	if heartbeat == "" || readFile(t, filepath.Join(ctx.Workspace, "heartbeat")) != heartbeat ||
		readFile(t, filepath.Join(ctx.Workspace, "before-timeout")) != "evidence" {
		t.Fatal("detached worker still running")
	}
	if res := shell(t, r, ctx, `printf cleaned > "$HOME/teardown-ran"`, 600); res["exit_code"] != int64(0) ||
		readFile(t, filepath.Join(ctx.HomeDir, "teardown-ran")) != "cleaned" {
		t.Fatal(res)
	}
}

func TestRealHarnessTimeoutRetainsPreparedToolsForTeardown(t *testing.T) {
	realImage(t)
	tr := newTemp(t)
	r, ctx, _, _ := makeReal(t, tr, "harness-timeout", nil)
	ctx.Runner, ctx.EnvironmentProfile, ctx.Env, ctx.Timeout = r, r.Profile(), r.EnvironmentEnv(ctx), 1
	if res := shell(t, r, ctx, `printf ready > "$HOME/prepared-tool"`, 600); res["exit_code"] != int64(0) {
		t.Fatal(res)
	}
	h := &plugin.BaseHarness{}
	plugin.Bind(h)
	response, err := h.Run(ctx, []string{"/bin/sh", "-c", "printf evidence > before-timeout; sleep 120 & wait"}, "execute", plugin.RunOpts{})
	noErr(t, err)
	if response["timed_out"] != true || util.Get(response, "environment_abort", "environment_retained") != true {
		t.Fatal(response)
	}
	cleanup := shell(t, r, ctx, `test "$(cat "$HOME/prepared-tool")" = ready && printf cleaned > "$HOME/teardown-ran"`, 600)
	if cleanup["exit_code"] != int64(0) || readFile(t, filepath.Join(ctx.Workspace, "before-timeout")) != "evidence" {
		t.Fatal(cleanup)
	}
}

func TestRealLifetimeExpiryWithoutReleaseOrACoordinatorTimer(t *testing.T) {
	realImage(t)
	tr := newTemp(t)
	r, ctx, handle, _ := makeReal(t, tr, "expiry", util.M{"limits": util.M{"lifetime_seconds": int64(12)}})
	env, err := r.WorkerEnv(ctx, nil)
	noErr(t, err)
	argv, err := r.Wrap([]string{"/bin/sh", "-c", "printf started > began; sleep 120"}, ctx, env)
	noErr(t, err)
	started := time.Now()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = ctx.Workspace, util.EnvList(env)
	err = cmd.Run()
	if time.Since(started) > 20*time.Second || err == nil {
		t.Fatal("worker outlived the lifetime", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	remaining, _ := r.find(handle, "")
	for remaining != nil && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		remaining, _ = r.find(handle, "")
	}
	if remaining != nil {
		t.Fatal("container survived its lifetime")
	}
	if _, err := mustNew(t, r.Profile()).Prepare(ctx); err == nil {
		t.Fatal("recovery recreated an expired attempt")
	}
	if !strings.Contains(readFile(t, filepath.Join(ctx.Workspace, "began")), "started") {
		t.Fatal("worker did not start")
	}
}
