package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/royosherove/ajx/internal/util"
)

// Run by a disposable child test process. Killing the coordinator must leave a
// durable launch identity that another coordinator can recover without replay.
func TestCrashWorkerProcess(t *testing.T) {
	path := os.Getenv("AJX_TEST_CRASH_TRIAL")
	if path == "" {
		return
	}
	sp := loadSpec(t, path)
	if _, err := RunMatrix(context.Background(), sp, MatrixOpts{
		Stages: []string{"prepare", "execute"}, Log: func(string) {},
	}); err != nil {
		t.Fatal(err)
	}
}

func startCrashWorker(t *testing.T) (M, string, M) {
	t.Helper()
	root := resolvePath(t.TempDir())
	path := writeTrial(t, root, "[[verify]]\nname = \"made\"\ntype = \"file_exists\"\npath = \"made.txt\"\n",
		"[[cells]]\nid = \"f\"\nharness = \"fake-slow\"\n")
	sp := loadSpec(t, path)
	runDir := filepath.Join(util.S(util.Get(sp, "trial", "output_dir")), "runs", "f-r1")
	record := filepath.Join(runDir, "execute.process.json")
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=^TestCrashWorkerProcess$")
	cmd.Dir = root
	cmd.Env = []string{"HOME=" + root, "TMPDIR=" + root, "PATH=" + os.Getenv("PATH"), "AJX_TEST_CRASH_TRIAL=" + path}
	log, err := os.Create(filepath.Join(root, "coordinator.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = log.Close()
		if exists(record) {
			if err := util.StopRecordedProcess(record); err != nil {
				t.Errorf("owned worker cleanup: %v", err)
			}
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	var state M
	for {
		state = util.ReadMap(filepath.Join(runDir, "state.json"))
		workspace := util.S(util.Get(state, "paths", "workspace"))
		if workspace != "" && exists(filepath.Join(workspace, "child.pid")) &&
			len(util.ReadJSONL(filepath.Join(runDir, "execute.raw.jsonl"))) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker never became ready: %s", readFileQuiet(log.Name()))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	return sp, runDir, state
}

func TestCrashRecoveryStopsOriginalWorkerBeforeVerification(t *testing.T) {
	sp, runDir, before := startCrashWorker(t)
	record := util.ReadMap(filepath.Join(runDir, "execute.process.json"))
	owner := util.Int(record["pid"], 0)
	if active, err := util.ProcessGroupActive(owner); err != nil || !active {
		t.Fatalf("crash fixture has no live orphan: %v %v", active, err)
	}
	rawBefore := readFile(t, filepath.Join(runDir, "execute.raw.jsonl"))
	r := newRun(t, sp, nil)
	verified := false
	// The callback below forbids a second task invocation.
	r.Harness.(*fakeHarness).execErr = errors.New("task must not be replayed")
	r.Override["verify"] = func(r *Run) error {
		verified = true
		if active, err := util.ProcessGroupActive(owner); err != nil || active {
			t.Fatalf("verification raced the original worker: active=%v err=%v", active, err)
		}
		if util.Get(r.State, "worker_cleanup", "confirmed") != true || util.Get(r.State, "execute", "reaped") != true {
			t.Fatal("verification preceded durable stop confirmation")
		}
		return r.stageVerify()
	}
	stubExtract(r)
	if _, err := r.Go(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	if !verified || r.Status("execute") != "interrupted" || r.Status("teardown") != "done" ||
		r.Status("archive") != "done" || util.Get(r.State, "teardown", "status") != "ok" {
		t.Fatalf("recovery statuses %v cleanup %v", statuses(r.State), r.State["worker_cleanup"])
	}
	if readFile(t, filepath.Join(runDir, "execute.raw.jsonl")) != rawBefore {
		t.Fatal("original raw evidence changed")
	}
	if !PyEqual(r.paths(), before["paths"]) || !exists(filepath.Join(runDir, "report.html")) {
		t.Fatal("original ownership or rendered artifacts missing")
	}
	// Idempotent resume and explicit replay refusal are different operations.
	if _, err := r.Go(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	_, err := r.Go(context.Background(), []string{"execute"}, false)
	requireStageRefusal(t, err)
}

func TestStoppedOwnerRefusesRecoveryUntilItCanStopItsGroup(t *testing.T) {
	sp, runDir, _ := startCrashWorker(t)
	rec := util.ReadMap(filepath.Join(runDir, "execute.process.json"))
	owner, err := os.FindProcess(util.Int(rec["pid"], 0))
	if err != nil {
		t.Fatal(err)
	}
	// This is the fixture's live owner, not a PID obtained from an old run.
	if err := owner.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer owner.Signal(syscall.SIGCONT)
	r := newRun(t, sp, nil)
	r.Override["verify"] = func(*Run) error { t.Error("verified live worker"); return nil }
	_, err = r.Go(context.Background(), []string{"verify", "teardown", "archive"}, false)
	var recovery *ProcessRecoveryError
	if !errors.As(err, &recovery) || util.Get(r.State, "worker_cleanup", "confirmed") != false {
		t.Fatalf("want unconfirmed recovery, got %v state %v", err, r.State["worker_cleanup"])
	}
	if r.State["archived"] != nil || r.Status("execute") != "running" || r.Status("verify") != "" {
		t.Fatalf("unsafe progress: %v", statuses(r.State))
	}
	if err := owner.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	delete(r.Override, "verify")
	if _, err := r.Go(context.Background(), []string{"teardown", "archive"}, false); err != nil {
		t.Fatal(err)
	}
	if r.Status("archive") != "done" || util.Get(r.State, "worker_cleanup", "confirmed") != true {
		t.Fatalf("retry did not finish cleanup: %v", statuses(r.State))
	}
}

func TestLegacyMissingIdentityNeverSignalsOrClaimsCleanup(t *testing.T) {
	root := resolvePath(t.TempDir())
	sp := loadSpec(t, writeTrial(t, root, "", ""))
	r := newRun(t, sp, nil)
	if _, err := r.Go(context.Background(), []string{"prepare"}, true); err != nil {
		t.Fatal(err)
	}
	unrelated := exec.Command("/bin/sleep", "60")
	unrelated.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() }()
	for _, pid := range []int{0, unrelated.Process.Pid} {
		r.State["execute"] = M{"pid": int64(pid), "stop_reason": "running"}
		if err := r.mark("execute", "running", nil); err != nil {
			t.Fatal(err)
		}
		_, err := r.Go(context.Background(), []string{"verify", "teardown", "archive"}, false)
		var recovery *ProcessRecoveryError
		if !errors.As(err, &recovery) || util.Get(r.State, "worker_cleanup", "confirmed") != false {
			t.Fatalf("legacy pid %d incorrectly confirmed: %v", pid, err)
		}
		if !util.PidAlive(unrelated.Process.Pid) || r.State["archived"] != nil || r.Status("verify") != "" {
			t.Fatal("legacy recovery signaled a reused PID or progressed")
		}
	}
}

func TestNonemptyRunMissingStateCannotReexecute(t *testing.T) {
	root := resolvePath(t.TempDir())
	sp := loadSpec(t, writeTrial(t, root, "", ""))
	opts := MatrixOpts{Stages: []string{"prepare", "execute", "teardown"}, Log: func(string) {}}
	if _, err := RunMatrix(context.Background(), sp, opts); err != nil {
		t.Fatal(err)
	}
	out := util.S(util.Get(sp, "trial", "output_dir"))
	runDir := filepath.Join(out, "runs", "f-r1")
	statePath := filepath.Join(runDir, "state.json")
	meta := readFile(t, filepath.Join(out, "matrix-plan.json"))
	raw := readFile(t, filepath.Join(runDir, "execute.raw.jsonl"))
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if _, err := RunMatrix(context.Background(), sp, opts); err == nil || !strings.Contains(err.Error(), "state.json is missing") {
		t.Fatalf("missing state accepted: %v", err)
	}
	if exists(statePath) || readFile(t, filepath.Join(out, "matrix-plan.json")) != meta ||
		readFile(t, filepath.Join(runDir, "execute.raw.jsonl")) != raw {
		t.Fatal("refusal changed existing evidence")
	}
	if err := os.WriteFile(statePath, []byte("null"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRun(sp, util.Maps(sp["cells"])[0], 1, nil); err == nil {
		t.Fatal("null state accepted as an unstarted run")
	}
}

func TestChangedTaskContractsCannotRelabelExistingVerification(t *testing.T) {
	root := resolvePath(t.TempDir())
	body := `[[preflight]]
run = "true"
[[setup]]
run = "true"
[[verify]]
name = "made"
type = "file_exists"
path = "made.txt"
[[teardown]]
run = "true"
`
	sp := loadSpec(t, writeTrial(t, root, body, ""))
	opts := MatrixOpts{Stages: []string{"prepare", "execute", "verify", "teardown"}, Log: func(string) {}}
	if _, err := RunMatrix(context.Background(), sp, opts); err != nil {
		t.Fatal(err)
	}
	out := util.S(util.Get(sp, "trial", "output_dir"))
	files := []string{filepath.Join(out, "matrix-plan.json"), filepath.Join(out, "runs", "f-r1", "verify.json"),
		filepath.Join(out, "runs", "f-r1", "state.json"), filepath.Join(out, "runs", "f-r1", "execute.raw.jsonl")}
	before := make([]string, len(files))
	for i, file := range files {
		before[i] = readFile(t, file)
	}
	for _, changed := range []string{"verify", "setup", "preflight", "teardown"} {
		next := util.DeepCopy(sp).(M)
		command := util.Maps(next[changed])[0]
		if changed == "verify" {
			command["path"] = "always-missing"
		} else {
			command["run"] = "false"
		}
		if _, err := RunMatrix(context.Background(), next, opts); err == nil ||
			!strings.Contains(err.Error(), "inputs changed") {
			t.Fatalf("%s change accepted: %v", changed, err)
		}
		for i, file := range files {
			if readFile(t, file) != before[i] {
				t.Fatalf("%s changed evidence %s", changed, file)
			}
		}
	}
	_, err := RunMatrix(context.Background(), sp, MatrixOpts{Stages: []string{"execute"}, Log: func(string) {}})
	requireStageRefusal(t, err)
	for i, file := range files {
		if readFile(t, file) != before[i] {
			t.Fatalf("explicit replay refusal changed %s", file)
		}
	}
	next := util.DeepCopy(sp).(M)
	util.Map(next["trial"])["repetitions"] = int64(2)
	if _, err := RunMatrix(context.Background(), next, MatrixOpts{Log: func(string) {}, Stages: []string{"verify"}}); err != nil {
		t.Fatalf("unchanged task with added repetition refused: %v", err)
	}
}

func TestFailedSetupRunsTeardownAndNeverReplaysSetup(t *testing.T) {
	for _, failCleanup := range []bool{false, true} {
		t.Run(fmt.Sprint("cleanup-fails-", failCleanup), func(t *testing.T) {
			root := resolvePath(t.TempDir())
			resource, count, allow := filepath.Join(root, "resource"), filepath.Join(root, "setup-count"), filepath.Join(root, "allow")
			setup := fmt.Sprintf("echo x >> %s; touch %s; exit 9", util.ShellQuote(count), util.ShellQuote(resource))
			teardown := "rm -f " + util.ShellQuote(resource)
			if failCleanup {
				teardown = "test -f " + util.ShellQuote(allow) + " && " + teardown
			}
			body := fmt.Sprintf("[[setup]]\nrun = %q\n[[teardown]]\nrun = %q\n", setup, teardown)
			sp := loadSpec(t, writeTrial(t, root, body, ""))
			r := newRun(t, sp, nil)
			if _, err := r.Go(context.Background(), nil, false); err != nil {
				t.Fatal(err)
			}
			if r.Status("prepare") != "error" || r.Status("execute") != "" || !r.setupStarted() {
				t.Fatalf("unexpected setup result: %v", statuses(r.State))
			}
			if failCleanup {
				if !exists(resource) || r.State["archived"] != nil || r.Status("archive") != "error" ||
					util.Get(r.State, "teardown", "status") == "ok" {
					t.Fatal("failed teardown claimed cleanup or discarded retry paths")
				}
				if err := os.WriteFile(allow, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			resumed := newRun(t, sp, nil)
			if _, err := resumed.Go(context.Background(), nil, false); err != nil {
				t.Fatal(err)
			}
			if exists(resource) || util.Get(resumed.State, "teardown", "status") != "ok" || !resumed.done("archive") {
				t.Fatalf("cleanup did not finish: %v", statuses(resumed.State))
			}
			if readFile(t, count) != "x\n" || resumed.Status("execute") != "" {
				t.Fatal("failed setup/task replayed")
			}
		})
	}
}
