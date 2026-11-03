package util

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func recordedFixture(t *testing.T) (string, string, StreamOpts) {
	t.Helper()
	root := t.TempDir()
	record := filepath.Join(root, "process.json")
	t.Cleanup(func() {
		if _, err := os.Stat(record); err == nil {
			// Cleanup is independent of the production assertion below: a
			// regression must not leave the test's delayed owner behind.
			deadline := time.Now().Add(5 * time.Second)
			for {
				err := StopRecordedProcess(record)
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Errorf("test owner cleanup: %v", err)
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
		}
	})
	return root, record, StreamOpts{Env: map[string]string{"PATH": "/usr/bin:/bin", "HOME": root}, Grace: 25 * time.Millisecond}
}

func TestRecordedStreamingCancelledDuringStartupHasCompleteResult(t *testing.T) {
	root, record, opts := recordedFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := RunStreaming(WithProcessRecord(ctx, record),
		[]string{"/bin/sh", "-c", "echo unexpected > started"}, root,
		filepath.Join(root, "out"), filepath.Join(root, "err"), time.Second, opts)
	if !Interrupted(err) || result == nil {
		t.Fatalf("startup cancellation lost its result: %v, %v", result, err)
	}
	if result["timed_out"] != false || Int(result["exit_code"], 0) != -int(syscall.SIGKILL) ||
		S(result["started_at"]) == "" || S(result["stopped_at"]) == "" {
		t.Fatalf("incomplete startup cancellation: %v", result)
	}
	assertRecordedStopped(t, record)
	if _, err := os.Stat(filepath.Join(root, "started")); !os.IsNotExist(err) {
		t.Fatal("worker started after cancellation")
	}
}

func TestRecordedStreamingParallelStartupCancellation(t *testing.T) {
	for i := 0; i < 16; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			root, record, opts := recordedFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timer := time.AfterFunc(time.Duration(i%8+1)*time.Millisecond, cancel)
			defer timer.Stop()
			result, err := RunStreaming(WithProcessRecord(ctx, record),
				[]string{"/bin/sh", "-c", "sleep 60 & echo running; wait"}, root,
				filepath.Join(root, "out"), filepath.Join(root, "err"), 5*time.Second, opts)
			if !Interrupted(err) || result == nil || result["timed_out"] != false ||
				Int(result["exit_code"], 0) != -int(syscall.SIGKILL) ||
				S(result["started_at"]) == "" || S(result["stopped_at"]) == "" {
				t.Fatalf("startup cancellation: result=%v err=%v", result, err)
			}
			// Assert before fixture cleanup: retries in t.Cleanup must never
			// mask a live owner returned by the production path.
			assertRecordedStopped(t, record)
		})
	}
}

func TestStopRecordedProcessWaitsForDelayedOwnerSocket(t *testing.T) {
	root, record, _ := recordedFixture(t)
	control, err := os.MkdirTemp("/tmp", ".ajx-worker-")
	if err != nil {
		t.Fatal(err)
	}
	rec := ProcessRecord{Version: 1, Token: strings.Repeat("e", 64), Socket: filepath.Join(control, "control.sock")}
	rec.Host, _ = os.Hostname()
	exe, _ := os.Executable()
	// exec preserves the PID/session leader after a deterministic startup
	// delay. Recovery starts while no control socket exists.
	cmd := exec.Command("/bin/sh", "-c", `sleep 0.25; exec "$@"`, "sh",
		exe, processGuardArg, rec.Socket, rec.Token, "/bin/echo", "must-not-run")
	cmd.Dir = root
	cmd.Env = []string{processGuardEnv + "=1", "PATH=/usr/bin:/bin", "HOME=" + root}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	rec.PID = cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	if err := writeProcessJSON(record, rec); err != nil {
		t.Fatal(err)
	}
	if err := StopRecordedProcess(record); err != nil {
		t.Fatalf("owner startup raced recovery: %v", err)
	}
	assertRecordedStopped(t, record)
}

func assertRecordedStopped(t *testing.T, path string) ProcessRecord {
	t.Helper()
	var rec ProcessRecord
	if err := readProcessJSON(path, &rec); err != nil || !rec.Stopped {
		t.Fatalf("stop record %+v: %v", rec, err)
	}
	if rec.PID > 1 {
		if active, err := ProcessGroupActive(rec.PID); err != nil || active {
			t.Fatalf("recorded group %d still active=%v: %v", rec.PID, active, err)
		}
	}
	return rec
}

func TestRecordedStreamingPreservesStdioAndArguments(t *testing.T) {
	root, record, opts := recordedFixture(t)
	input := "stdin ' \" $ ` \nsecond line\n"
	opts.StdinData = &input
	opts.Env["SYNTHETIC"] = "env value"
	script := `cat; printf '%s\n' "$1" "$SYNTHETIC" "$PWD"; printf 'stderr\n' >&2; exit 7`
	arg := `literal $() " ' ;`
	res, err := RunStreaming(WithProcessRecord(context.Background(), record),
		[]string{"/bin/sh", "-c", script, "sh", arg}, root,
		filepath.Join(root, "out.jsonl"), filepath.Join(root, "err.txt"), 5*time.Second, opts)
	if err != nil || Int(res["exit_code"], 0) != 7 || res["timed_out"] != false {
		t.Fatalf("result %v: %v", res, err)
	}
	var lines []string
	for _, row := range Maps(ReadJSONL(filepath.Join(root, "out.jsonl"))) {
		lines = append(lines, S(row["line"]))
	}
	physicalRoot, _ := filepath.EvalSymlinks(root)
	want := input + arg + "\nenv value\n" + physicalRoot + "\n"
	if got := strings.Join(lines, "\n") + "\n"; got != want {
		t.Fatalf("stdout %q, want %q", got, want)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "err.txt")); string(data) != "stderr\n" {
		t.Fatalf("stderr %q", data)
	}
	if S(res["started_at"]) == "" || S(res["stopped_at"]) == "" || Int(res["worker_pid"], 0) <= 1 {
		t.Fatalf("missing launch/result facts: %v", res)
	}
	if err := StopRecordedProcess(record); err != nil {
		t.Fatal(err)
	}
	assertRecordedStopped(t, record)
}

func TestRecordedStreamingTimeoutAndCancellationStopDescendants(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "cancel"}[cancelRun], func(t *testing.T) {
			root, record, opts := recordedFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timeout := 100 * time.Millisecond
			if cancelRun {
				timeout = 5 * time.Second
				timer := time.AfterFunc(100*time.Millisecond, cancel)
				defer timer.Stop()
			}
			res, err := RunStreaming(WithProcessRecord(ctx, record),
				[]string{"/bin/sh", "-c", "sleep 60 & echo started; wait"}, root,
				filepath.Join(root, "out"), filepath.Join(root, "err"), timeout, opts)
			if cancelRun && !Interrupted(err) || !cancelRun && err != nil {
				t.Fatalf("result %v: %v", res, err)
			}
			if res["timed_out"] != !cancelRun || Int(res["exit_code"], 0) != -int(syscall.SIGKILL) ||
				S(res["started_at"]) == "" || S(res["stopped_at"]) == "" {
				t.Fatalf("timeout/cancel facts: %v", res)
			}
			assertRecordedStopped(t, record)
		})
	}
}

func TestRecordedStreamingSpawnAndCaptureFailuresLeaveNoOwner(t *testing.T) {
	for _, failure := range []string{"command", "cwd", "capture"} {
		t.Run(failure, func(t *testing.T) {
			root, record, opts := recordedFixture(t)
			argv, cwd, out := []string{"/bin/echo", "unexpected"}, root, filepath.Join(root, "out")
			switch failure {
			case "command":
				argv[0] = filepath.Join(root, "missing-command")
			case "cwd":
				cwd = filepath.Join(root, "missing-cwd")
			case "capture":
				out = filepath.Join(root, "missing-dir", "out")
			}
			_, err := RunStreaming(WithProcessRecord(context.Background(), record),
				argv, cwd, out, filepath.Join(root, "err"), time.Second, opts)
			if err == nil {
				t.Fatal("expected launch failure")
			}
			assertRecordedStopped(t, record)
		})
	}
}

func TestRecordedStreamingUsesShortSocketUnderLongTempRoot(t *testing.T) {
	root, record, opts := recordedFixture(t)
	long := filepath.Join(root, strings.Repeat("long", 30))
	if err := os.Mkdir(long, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", long)
	_, err := RunStreaming(WithProcessRecord(context.Background(), record),
		[]string{"/bin/echo", "ok"}, root, filepath.Join(root, "out"), filepath.Join(root, "err"), time.Second, opts)
	if err != nil {
		t.Fatal(err)
	}
	var rec ProcessRecord
	if err := readProcessJSON(record, &rec); err != nil || len(rec.Socket) >= 104 {
		t.Fatalf("socket path %q: %v", rec.Socket, err)
	}
	if st, err := os.Stat(filepath.Dir(rec.Socket)); err != nil || st.Mode().Perm() != 0700 {
		t.Fatalf("control directory permissions: %v %v", st, err)
	}
}

func TestRecordedStopRejectsWrongIdentityWithoutSignaling(t *testing.T) {
	root, record, opts := recordedFixture(t)
	res, err := RunStreaming(WithProcessRecord(context.Background(), record),
		[]string{"/bin/sh", "-c", "sleep 60 & echo ready"}, root,
		filepath.Join(root, "out"), filepath.Join(root, "err"), time.Second, opts)
	if err != nil {
		t.Fatal(err)
	}
	var original ProcessRecord
	if err := readProcessJSON(record, &original); err != nil {
		t.Fatal(err)
	}
	unrelated := exec.Command("/bin/sleep", "60")
	unrelated.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() }()
	defer func() { _ = writeProcessJSON(record, original) }()
	for _, failure := range []string{"wrong-pid", "wrong-token", "unreachable"} {
		bad := original
		switch failure {
		case "wrong-pid":
			bad.PID = unrelated.Process.Pid
		case "wrong-token":
			bad.Token = strings.Repeat("0", 64)
			bad.PID = 1 << 29 // an empty group must not override rejected identity
		case "unreachable":
			bad.Socket = filepath.Join(root, "missing", "control.sock")
		}
		if err := writeProcessJSON(record, bad); err != nil {
			t.Fatal(err)
		}
		if err := StopRecordedProcess(record); err == nil {
			t.Fatalf("%s accepted", failure)
		}
		if !PidAlive(unrelated.Process.Pid) {
			t.Fatalf("%s stopped unrelated process", failure)
		}
		if _, err := processCall(original, "status"); err != nil {
			t.Fatalf("%s stopped real owner %v: %v", failure, res, err)
		}
	}
}

func TestOwnerKilledBeforeStartGrantNeverStartsWorker(t *testing.T) {
	root, record, _ := recordedFixture(t)
	control, err := os.MkdirTemp("/tmp", ".ajx-guard-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(control)
	rec := ProcessRecord{Version: 1, Token: strings.Repeat("a", 64), Socket: filepath.Join(control, "control.sock")}
	rec.Host, _ = os.Hostname()
	if err := writeProcessJSON(record, rec); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	cmd := exec.Command(exe, processGuardArg, rec.Socket, rec.Token,
		"/bin/sh", "-c", "echo wrong > started")
	cmd.Dir = root
	cmd.Env = []string{processGuardEnv + "=1", "PATH=/usr/bin:/bin", "HOME=" + root}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	rec.PID = cmd.Process.Pid
	if err := writeProcessJSON(record, rec); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := processCall(rec, "status"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owner did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Merely spawning the owner is not authorization to execute the task.
	if _, err := os.Stat(filepath.Join(root, "started")); !os.IsNotExist(err) {
		t.Fatal("worker ran before durable start grant")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := StopRecordedProcess(record); err != nil {
		t.Fatal(err)
	}
	assertRecordedStopped(t, record)
}

func TestMissingLaunchAcknowledgementRemainsUnconfirmed(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "process.json")
	rec := ProcessRecord{Version: 1, Token: strings.Repeat("b", 64), Socket: filepath.Join(root, "control.sock")}
	rec.Host, _ = os.Hostname()
	if err := writeProcessJSON(path, rec); err != nil {
		t.Fatal(err)
	}
	if err := StopRecordedProcess(path); err == nil {
		t.Fatal("missing owner identity was treated as stopped")
	}
	data, _ := os.ReadFile(path)
	var saved ProcessRecord
	_ = json.Unmarshal(data, &saved)
	if saved.Stopped {
		t.Fatal("unconfirmed record was marked stopped")
	}
}

func TestUnreachableRecordCannotDeleteUnrelatedControlFiles(t *testing.T) {
	root := t.TempDir()
	foreign := filepath.Join(root, "unrelated")
	if err := os.Mkdir(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"control.sock", "result.json"} {
		if err := os.WriteFile(filepath.Join(foreign, name), []byte("retain"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "process.json")
	rec := ProcessRecord{Version: 1, PID: 1 << 29, Token: strings.Repeat("c", 64),
		Socket: filepath.Join(foreign, "control.sock")}
	rec.Host, _ = os.Hostname()
	if err := writeProcessJSON(path, rec); err != nil {
		t.Fatal(err)
	}
	if err := StopRecordedProcess(path); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"control.sock", "result.json"} {
		if data, err := os.ReadFile(filepath.Join(foreign, name)); err != nil || string(data) != "retain" {
			t.Fatalf("unreachable record changed unrelated %s: %q %v", name, data, err)
		}
	}
	link := filepath.Join(root, ".ajx-worker-symlink")
	if err := os.Symlink(foreign, link); err != nil {
		t.Fatal(err)
	}
	rec.Socket = filepath.Join(link, "control.sock")
	removeProcessControl(rec)
	if data, err := os.ReadFile(filepath.Join(foreign, "result.json")); err != nil || string(data) != "retain" {
		t.Fatal("symlink control directory was followed")
	}
}

func TestForeignWorkerHostCannotBeConfirmedFromLocalPID(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "process.json")
	rec := ProcessRecord{Version: 1, PID: 1 << 29, Host: "unrelated.invalid", Token: strings.Repeat("d", 64),
		Socket: filepath.Join(root, "control.sock")}
	if err := writeProcessJSON(path, rec); err != nil {
		t.Fatal(err)
	}
	if err := StopRecordedProcess(path); err == nil {
		t.Fatal("local process absence was used to confirm a foreign worker stopped")
	}
}
