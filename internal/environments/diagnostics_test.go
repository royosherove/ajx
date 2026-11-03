package environments

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

func diagnosticFixture(t *testing.T) *containerFixture {
	f := newContainerFixture(t)
	f.engine.diagnostic = true
	return f
}

func doctorCtx(root string) *plugin.Ctx {
	return &plugin.Ctx{Spec: util.M{"trial": util.M{"workspace_root": root}}, Cell: util.M{"id": "c"}}
}

func cleanupNote(t *testing.T, e *Error) util.M {
	t.Helper()
	joined := strings.Join(e.Notes, "\n")
	_, text, ok := strings.Cut(joined, "Environment preparation cleanup: ")
	if !ok {
		t.Fatal(e.Notes)
	}
	var out util.M
	noErr(t, json.Unmarshal([]byte(text), &out))
	return out
}

func TestCreationFailureNamesExitCodeStderrTailAndVMHint(t *testing.T) {
	f := diagnosticFixture(t)
	r := f.engine.runner(t, containerProfile(t, f.tr.base, nil))
	ctx := newCtx(t, f.tr.root, "attempt", true)
	persist(t, r, ctx)
	f.engine.failCreate = true
	_, err := r.Prepare(ctx)
	e := wantErr(t, err, KindEnvironment, "Docker creation failed")
	for _, part := range []string{"Docker creation failed (exit code 125): docker: Error response from daemon: " +
		`invalid mount config for type "bind": bind source path does not exist: /private/tmp/ajx-example/workspace.`,
		"; no fallback or task retry was attempted", "; hint: " + VMMountHint} {
		if !strings.Contains(e.Msg, part) {
			t.Fatalf("%q missing %q", e.Msg, part)
		}
	}
	if strings.Contains(e.Msg, "\n") {
		t.Fatal("multi-line error")
	}
	cleanup := cleanupNote(t, e)
	failure := util.MapOr(cleanup["creation_failure"])
	if cleanup["status"] != "already_absent" || cleanup["reason"] != "not present; container creation failed (see creation_failure)" ||
		failure["exit_code"] != 125.0 || failure["hint"] != VMMountHint ||
		!strings.Contains(util.S(failure["stderr_tail"]), "bind source path does not exist: /private/tmp/ajx-example/workspace") {
		t.Fatal(cleanup)
	}
	if len(f.engine.containers) != 0 {
		t.Fatal(f.engine.containers)
	}
	released, _ := r.Release(ctx)
	if released["status"] != "already_absent" || util.Compact(roundTrip(t, released)["creation_failure"]) != util.Compact(failure) {
		t.Fatal(released)
	}
	_, err = r.Prepare(ctx)
	wantErr(t, err, KindEnvironment, "")
	if f.engine.count("run") != 1 {
		t.Fatal(f.engine.count("run"))
	}
}

func TestCreationFailureWithoutAKnownEngineLimitHasNoHint(t *testing.T) {
	f := diagnosticFixture(t)
	r := f.engine.runner(t, containerProfile(t, f.tr.base, nil))
	ctx := newCtx(t, f.tr.root, "attempt", true)
	persist(t, r, ctx)
	f.engine.failCreate = true
	f.engine.createStderr = "docker: Error response from daemon: OCI runtime create failed: synthetic\n"
	_, err := r.Prepare(ctx)
	e := wantErr(t, err, KindEnvironment, "")
	if e.Msg != "Docker creation failed (exit code 125): docker: Error response from daemon: "+
		"OCI runtime create failed: synthetic; no fallback or task retry was attempted" {
		t.Fatal(e.Msg)
	}
	released, _ := r.Release(ctx)
	if _, ok := util.MapOr(released["creation_failure"])["hint"]; ok {
		t.Fatal(released)
	}
}

func TestSuccessfulLifecycleRecordsNoCreationFailure(t *testing.T) {
	f := diagnosticFixture(t)
	r := f.engine.runner(t, containerProfile(t, f.tr.base, nil))
	ctx := newCtx(t, f.tr.root, "attempt", true)
	persist(t, r, ctx)
	f.engine.failCreate = true
	_, err := r.Prepare(ctx)
	wantErr(t, err, KindEnvironment, "")
	fresh := f.engine.runner(t, containerProfile(t, f.tr.base, nil))
	freshCtx := newCtx(t, f.tr.root, "second", true)
	persist(t, fresh, freshCtx)
	f.engine.failCreate = false
	released, _ := fresh.Release(freshCtx)
	if _, ok := released["creation_failure"]; ok || released["status"] != "already_absent" ||
		released["reason"] != "not present; expiry/startup failure cannot be distinguished" {
		t.Fatal(released)
	}
}

func TestStderrTailIsOneBoundedPrintableLine(t *testing.T) {
	var rows []string
	for i := 0; i < 20; i++ {
		rows = append(rows, "line "+itoa(i))
	}
	noisy := strings.Join(rows, "\n") + "\r\n\n\tlast\tline\n"
	if got := stderrTail(noisy); got != "line 15 line 16 line 17 line 18 line 19 last line" {
		t.Fatal(got)
	}
	bounded := stderrTail(strings.Repeat("x", 1000))
	if util.RuneLen(bounded) != 600 || !strings.HasSuffix(bounded, "...") {
		t.Fatal(len(bounded))
	}
	if stderrTail("") != "" || stderrTail("\x1b[31mred\x1b[0m") != "[31mred[0m" {
		t.Fatal(stderrTail("\x1b[31mred\x1b[0m"))
	}
	if got := dockerError("Docker image inspect failed", util.M{"exit_code": int64(1), "stderr_tail": ""}, "check the daemon"); got !=
		"Docker image inspect failed (exit code 1): (no stderr); check the daemon" {
		t.Fatal(got)
	}
	for _, text := range []string{"Error: mounts denied: the path /x is not shared from the host",
		"bind source path does not exist: /private/tmp/x"} {
		if dockerFailure(DockerResult{Code: 125, Stderr: text})["hint"] != VMMountHint {
			t.Fatal(text)
		}
	}
	if _, ok := dockerFailure(DockerResult{Code: 125, Stderr: "no space left on device"})["hint"]; ok {
		t.Fatal("unexpected hint")
	}
}

func TestImageInspectFailureIncludesTheEngineStderr(t *testing.T) {
	f := diagnosticFixture(t)
	r := f.engine.runner(t, containerProfile(t, f.tr.base, nil))
	f.engine.override = func(args []string) (DockerResult, bool) {
		if hasPrefix(args, "image", "inspect") {
			return DockerResult{Code: 1, Stdout: "[]\n", Stderr: "Error response from daemon: No such image: " + testImage + "\n"}, true
		}
		return DockerResult{}, false
	}
	report := r.Doctor(doctorCtx(f.tr.root))
	if report["status"] != "unavailable" || report["error"] != "Docker image inspect failed (exit code 1): Error response from daemon: "+
		"No such image: "+testImage+"; check the daemon and preloaded pinned image" {
		t.Fatal(report["error"])
	}
	if util.Compact(report["mount_probe"]) != `{"reason":"image/daemon checks failed","status":"skipped"}` || len(f.engine.probeRuns) != 0 {
		t.Fatal(report["mount_probe"])
	}
	_, err := r.Plan(newCtx(t, f.tr.root, "attempt", true))
	wantErr(t, err, KindEnvironment, "No such image")
}

func TestOKProbeUsesAPrivateDirectoryUnderWorkspaceRootInAHardenedContainer(t *testing.T) {
	f := diagnosticFixture(t)
	r := f.engine.runner(t, containerProfile(t, f.tr.base, util.M{"platform": "linux/amd64"}))
	workspaceRoot := filepath.Join(f.tr.root, "work")
	noErr(t, os.Mkdir(workspaceRoot, 0o755))
	report := r.Doctor(doctorCtx(workspaceRoot))
	if report["status"] != "available" || report["error"] != nil {
		t.Fatal(report)
	}
	if util.Compact(report["mount_probe"]) != util.Compact(util.M{"status": "ok", "workspace_root": workspaceRoot, "stderr_tail": "", "hint": nil}) {
		t.Fatal(report["mount_probe"])
	}
	if len(f.engine.probeRuns) != 1 {
		t.Fatal(f.engine.probeRuns)
	}
	run := f.engine.probeRuns[0]
	source := util.S(run["source"])
	if run["existed"] != true || run["mode"] != 0o700 || filepath.Dir(source) != workspaceRoot ||
		!strings.HasPrefix(filepath.Base(source), "ajx-doctor-probe-") || lexists(source) {
		t.Fatal(run)
	}
	if entries, _ := os.ReadDir(workspaceRoot); len(entries) != 0 {
		t.Fatal("probe directory left behind")
	}
	args := run["args"].([]string)
	padded := " " + strings.Join(args, " ") + " "
	for _, flags := range []string{"--rm", "--pull never", "--network none", "--read-only", "--cap-drop ALL",
		"--security-opt no-new-privileges=true", "--ipc none", "--log-driver none",
		"--platform linux/amd64", "--entrypoint /usr/bin/env"} {
		if !strings.Contains(padded, " "+flags+" ") {
			t.Fatal(flags)
		}
	}
	for _, flag := range []string{"--detach", "--privileged", "--cap-add", "--volume", "-v", "--device", "--name"} {
		if util.Contains(args, flag) {
			t.Fatal(flag)
		}
	}
	if !regexp.MustCompile(`^[1-9][0-9]*:[1-9][0-9]*$`).MatchString(option(args, "--user")) {
		t.Fatal(option(args, "--user"))
	}
	if util.Compact(run["mount"]) != util.Compact(map[string]any{"type": "bind", "source": source, "target": "/ajx-probe",
		"bind-propagation": "rprivate", "bind-recursive": "disabled", "readonly": true}) {
		t.Fatal(run["mount"])
	}
	at := 0
	for i, a := range args {
		if a == testImage {
			at = i
		}
	}
	if strings.Join(args[at+1:], "|") != "-i|PATH="+ContainerPath+"|/bin/sh|-c|test -d /ajx-probe" || !util.Contains(args, "IMAGE_SECRET=") {
		t.Fatal(args)
	}
}

func TestFailedProbeMarksTheProfileUnavailableWithStderrAndHint(t *testing.T) {
	f := diagnosticFixture(t)
	r := f.engine.runner(t, containerProfile(t, f.tr.base, nil))
	stderr := bindError
	f.engine.probeStderr = &stderr
	report := r.Doctor(doctorCtx(f.tr.root))
	probe := util.MapOr(report["mount_probe"])
	if report["status"] != "unavailable" || probe["status"] != "failed" || probe["workspace_root"] != f.tr.root ||
		probe["exit_code"] != int64(125) || probe["hint"] != VMMountHint || report["error"] != probe["error"] ||
		!strings.Contains(util.S(probe["stderr_tail"]), "bind source path does not exist: /private/tmp/ajx-example/workspace") {
		t.Fatal(report)
	}
	msg := util.S(report["error"])
	if !strings.HasPrefix(msg, "Workspace bind-mount probe failed for trial.workspace_root "+f.tr.root+" (exit code 125): "+
		"docker: Error response from daemon: invalid mount config") || !strings.HasSuffix(msg, "; hint: "+VMMountHint) ||
		strings.Contains(msg, "\n") {
		t.Fatal(msg)
	}
	if lexists(util.S(f.engine.probeRuns[0]["source"])) || util.Get(report, "image", "id") != testImage {
		t.Fatal(report)
	}
}

func TestFailedProbeWithoutAKnownEngineLimitHasNoHint(t *testing.T) {
	f := diagnosticFixture(t)
	r := f.engine.runner(t, containerProfile(t, f.tr.base, nil))
	stderr := "docker: Error response from daemon: synthetic runtime failure\n"
	f.engine.probeStderr = &stderr
	report := r.Doctor(doctorCtx(f.tr.root))
	if report["status"] != "unavailable" || util.Get(report, "mount_probe", "hint") != nil ||
		report["error"] != "Workspace bind-mount probe failed for trial.workspace_root "+f.tr.root+
			" (exit code 125): docker: Error response from daemon: synthetic runtime failure" {
		t.Fatal(report)
	}
}

func TestProbeClientFailureIsReportedAndTheDirectoryIsStillRemoved(t *testing.T) {
	f := diagnosticFixture(t)
	r := f.engine.runner(t, containerProfile(t, f.tr.base, nil))
	f.engine.probeError = "Docker run unavailable (TimeoutExpired); check the local daemon"
	before, _ := os.ReadDir(f.tr.root)
	report := r.Doctor(doctorCtx(f.tr.root))
	after, _ := os.ReadDir(f.tr.root)
	if report["status"] != "unavailable" || util.Get(report, "mount_probe", "status") != "failed" ||
		!strings.Contains(util.S(report["error"]), "Workspace bind-mount probe could not run") ||
		!strings.Contains(util.S(report["error"]), "TimeoutExpired") || len(before) != len(after) {
		t.Fatal(report)
	}
}

func TestProbeIsSkippedWithoutAWorkspaceRootForAMissingRootAndForLocal(t *testing.T) {
	f := diagnosticFixture(t)
	r := f.engine.runner(t, containerProfile(t, f.tr.base, nil))
	for _, ctx := range []*plugin.Ctx{nil, {}, {Spec: util.M{}}, {Spec: util.M{"trial": util.M{}}}} {
		report := r.Doctor(ctx)
		if report["status"] != "available" ||
			util.Compact(report["mount_probe"]) != `{"reason":"no trial.workspace_root in the doctor context","status":"skipped"}` {
			t.Fatal(report["mount_probe"])
		}
	}
	report := r.Doctor(doctorCtx(filepath.Join(f.tr.root, "missing")))
	if report["status"] != "available" || util.Get(report, "mount_probe", "status") != "skipped" ||
		!strings.Contains(util.S(util.Get(report, "mount_probe", "reason")), "does not exist yet") || len(f.engine.probeRuns) != 0 {
		t.Fatal(report)
	}
	local := localRunner(t, f.tr.base, nil)
	report = local.Doctor(doctorCtx(f.tr.root))
	if report["status"] != "available" ||
		util.Compact(report["mount_probe"]) != `{"reason":"the local backend has no container bind mounts","status":"skipped"}` {
		t.Fatal(report)
	}
}

// ------------------------------------------------------------------ re-inspection

type reinspection struct {
	root, source, field, destination string
}

func newReinspection(t *testing.T) *reinspection {
	tr := newTemp(t)
	source := filepath.Join(tr.base, "fixtures")
	noErr(t, os.MkdirAll(filepath.Join(source, "sub"), 0o755))
	writeFile(t, filepath.Join(source, "input.txt"), "synthetic starting data\n")
	writeFile(t, filepath.Join(source, "sub", "nested.txt"), "nested synthetic data\n")
	return &reinspection{root: tr.root, source: source, field: "environment.mounts[0].source",
		destination: filepath.Join(tr.root, "attempt", "workspace", ".ajx-mounts", "0")}
}

// alterFstat makes one fstat of target per inspection pass report a changed
// attribute. A regular file is fstat'ed twice per pass (after open and after
// the read); a directory once. Only the chosen position of the listed passes
// (1-based) is altered.
func alterFstat(t *testing.T, target, attribute string, failing map[int]bool, perPass, position int) {
	st, err := pathStat(target)
	noErr(t, err)
	seen := 0
	old := fstat
	fstat = func(f *os.File) (statInfo, error) {
		info, err := realFstat(f)
		if err != nil || info.ino != st.ino {
			return info, err
		}
		seen++
		pass, index := (seen-1)/perPass, (seen-1)%perPass
		if index == position && failing[pass+1] {
			switch attribute {
			case "ctime":
				info.ctimeNs++
			case "mtime":
				info.mtimeNs++
			case "ino":
				info.ino++
			}
		}
		return info, nil
	}
	t.Cleanup(func() { fstat = old })
}

func noSleep(t *testing.T) *[]float64 {
	var calls []float64
	old := sleep
	sleep = func(s float64) { calls = append(calls, s) }
	t.Cleanup(func() { sleep = old })
	return &calls
}

func listTree(root string) []string {
	var out []string
	filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err == nil && p != root {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func TestChangeNamesEntryAndAttributeThenAFreshPassSucceeds(t *testing.T) {
	ri := newReinspection(t)
	clean, err := treeManifest(ri.source, ri.field, treeOpts{fixtures: true})
	noErr(t, err)
	type discard struct {
		partial []string
		removed bool
		exists  bool
	}
	var discarded []discard
	old := discardPartialCopy
	discardPartialCopy = func(dest string) bool {
		partial := listTree(dest)
		removed := realDiscardPartialCopy(dest)
		discarded = append(discarded, discard{partial, removed, lexists(dest)})
		return removed
	}
	defer func() { discardPartialCopy = old }()
	alterFstat(t, filepath.Join(ri.source, "sub", "nested.txt"), "ctime", map[int]bool{1: true}, 2, 1)
	sleeps := noSleep(t)
	got, err := treeManifest(ri.source, "environment.mount.snapshot", treeOpts{fixtures: true, destination: ri.destination})
	noErr(t, err)
	if util.Compact(got) != util.Compact(clean) || len(*sleeps) != 1 || (*sleeps)[0] != reinspectDelay {
		t.Fatal(got, *sleeps)
	}
	// Pass one had copied everything before nested.txt's post-read check failed; the retry started over.
	if len(discarded) != 1 || strings.Join(discarded[0].partial, ",") != "input.txt,sub,sub/nested.txt" ||
		!discarded[0].removed || discarded[0].exists {
		t.Fatal(discarded)
	}
	if strings.Join(listTree(ri.destination), ",") != "input.txt,sub,sub/nested.txt" {
		t.Fatal(listTree(ri.destination))
	}
	for _, name := range []string{"input.txt", "sub/nested.txt"} {
		if readFile(t, filepath.Join(ri.destination, name)) != readFile(t, filepath.Join(ri.source, name)) {
			t.Fatal(name)
		}
	}
}

func TestReproducedChangeFailsWithTheEntryAttributeAndReinspectionMarker(t *testing.T) {
	ri := newReinspection(t)
	alterFstat(t, filepath.Join(ri.source, "input.txt"), "mtime", map[int]bool{1: true, 2: true}, 2, 1)
	sleeps := noSleep(t)
	_, err := treeManifest(ri.source, ri.field, treeOpts{fixtures: true, destination: ri.destination})
	e := wantErr(t, err, KindValue, "")
	if e.TypeName() != KindInspectionChanged || e.Msg != ri.field+": starting directory changed while being inspected: "+
		"input.txt (mtime) (reproduced on re-inspection)" || len(*sleeps) != 1 {
		t.Fatal(e.Msg)
	}
}

func TestDirectoryIdentityChangeNamesTheDirectory(t *testing.T) {
	ri := newReinspection(t)
	alterFstat(t, filepath.Join(ri.source, "sub"), "ino", map[int]bool{1: true, 2: true}, 1, 0)
	noSleep(t)
	_, err := treeManifest(ri.source, ri.field, treeOpts{fixtures: true})
	e := wantErr(t, err, KindValue, "")
	if e.Msg != ri.field+": starting directory changed while being inspected: sub/ (dev/ino) (reproduced on re-inspection)" {
		t.Fatal(e.Msg)
	}
}

func TestSeveralChangedAttributesAreNamedAndACleanSecondPassValidates(t *testing.T) {
	ri := newReinspection(t)
	base := filepath.Dir(ri.source)
	conf := util.M{"test": util.M{"backend": "container", "image": testImage, "mounts": []any{
		util.M{"source": "fixtures", "target": "/fixtures", "access": "read"}}}}
	func() {
		old := fstat
		defer func() { fstat = old }()
		alterFstat(t, filepath.Join(ri.source, "input.txt"), "ctime", map[int]bool{1: true}, 2, 1)
		noSleep(t)
		out, err := NormalizeProfiles(conf, base)
		noErr(t, err)
		if util.Get(util.Maps(util.Get(out, "test", "mounts"))[0], "source") != ri.source {
			t.Fatal(out)
		}
	}()
	alterFstat(t, filepath.Join(ri.source, "input.txt"), "ctime", map[int]bool{1: true, 2: true}, 2, 1)
	_, err := NormalizeProfiles(conf, base)
	wantErr(t, err, KindValue, `^environments\.test\.mounts\[0\]\.source: starting directory changed while being inspected: `+
		`input\.txt \(ctime\) \(reproduced on re-inspection\)$`)
	before, err := pathStat(filepath.Join(ri.source, "input.txt"))
	noErr(t, err)
	after := before
	after.size++
	after.mtimeNs++
	if got := statChanges(before, after, "size", "mtime", "ctime", "nlink"); strings.Join(got, ",") != "size,mtime" {
		t.Fatal(got)
	}
	if got := statChanges(before, before, "dev/ino", "type", "size", "nlink"); len(got) != 0 {
		t.Fatal(got)
	}
}
