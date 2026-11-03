package util

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrInterrupted reports that a stage was cancelled (SIGINT/SIGTERM/SIGHUP).
// It plays the role of Python's KeyboardInterrupt: callers clean up and
// propagate it; the CLI exits 130.
var ErrInterrupted = errors.New("interrupted")

// Interrupted reports whether err is (or wraps) ErrInterrupted or a context cancellation.
func Interrupted(err error) bool {
	return errors.Is(err, ErrInterrupted) || errors.Is(err, context.Canceled)
}

func killpg(pid int, sig syscall.Signal) {
	if pid > 0 {
		_ = syscall.Kill(-pid, sig)
	}
}

// KillGroup sends SIGTERM, waits a second, then SIGKILL to a process group
// left behind by a worker (dev servers, watchers).
func KillGroup(pid int) {
	killpg(pid, syscall.SIGTERM)
	time.Sleep(time.Second)
	killpg(pid, syscall.SIGKILL)
}

// PidAlive reports whether a process with pid exists (signal 0).
func PidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ExitCode converts a finished process state to Python's returncode
// convention: negative signal number when killed by a signal.
func ExitCode(ps *os.ProcessState) int {
	if ps == nil {
		return -1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -int(ws.Signal())
	}
	return ps.ExitCode()
}

// StreamOpts configures RunStreaming.
type StreamOpts struct {
	Env       map[string]string // full child environment (nil = inherit)
	StdinData *string           // nil: stdin is /dev/null
	Grace     time.Duration     // default 3s
	// ReapAfterExit kills the process group after a normal exit so leftovers
	// do not outlive the stage. execute passes false so a server the agent
	// started is still up for verification; the runner reaps it afterwards.
	ReapAfterExit bool
}

// RunStreaming runs argv in its own process group (session), stamping each
// stdout line with its arrival time as {"at": ts, "line": raw} records in
// outPath; stderr goes to errPath. On timeout the whole group gets SIGTERM,
// then SIGKILL after 5s. Cancelling ctx kills the group and returns
// ErrInterrupted (with the partial record). Returns
// M{exit_code, timed_out, pid, started_at, stopped_at}.
func RunStreaming(ctx context.Context, argv []string, cwd, outPath, errPath string, timeout time.Duration, o StreamOpts) (M, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if record, ok := ctx.Value(processRecordKey{}).(string); ok {
		return runRecordedStreaming(ctx, record, argv, cwd, outPath, errPath, timeout, o)
	}
	if o.Grace == 0 {
		o.Grace = 3 * time.Second
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty argv")
	}
	started := Now()
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return nil, err
	}
	out, err := os.Create(outPath)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	errf, err := os.Create(errPath)
	if err != nil {
		return nil, err
	}
	defer errf.Close()

	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	if o.Env != nil {
		cmd.Env = EnvList(o.Env)
	}
	cmd.Stdout = pw
	cmd.Stderr = errf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var stdin io.WriteCloser
	if o.StdinData != nil {
		stdin, err = cmd.StdinPipe()
		if err != nil {
			pr.Close()
			pw.Close()
			return nil, err
		}
	}
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, err
	}
	pw.Close()
	pid := cmd.Process.Pid

	var mu sync.Mutex
	closed := false
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		r := bufio.NewReaderSize(pr, 1<<16)
		for {
			line, err := r.ReadString('\n')
			if len(line) > 0 {
				line = trimNewline(line)
				rec := Compact(M{"at": Now(), "line": line})
				mu.Lock()
				if !closed {
					out.WriteString(rec + "\n")
				}
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	if stdin != nil {
		data := *o.StdinData
		go func() {
			_, _ = io.WriteString(stdin, data)
			_ = stdin.Close()
		}()
	}

	var timedOut bool
	var timedMu sync.Mutex
	var timer *time.Timer
	if timeout > 0 {
		timer = time.AfterFunc(timeout, func() {
			timedMu.Lock()
			timedOut = true
			timedMu.Unlock()
			killpg(pid, syscall.SIGTERM)
			time.Sleep(5 * time.Second)
			killpg(pid, syscall.SIGKILL)
		})
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	var interrupted bool
	select {
	case <-waitDone:
	case <-ctx.Done():
		interrupted = true
		killpg(pid, syscall.SIGTERM)
		killpg(pid, syscall.SIGKILL)
		<-waitDone
	}
	if timer != nil {
		timer.Stop()
	}
	join := func(d time.Duration) bool {
		select {
		case <-readerDone:
			return true
		case <-time.After(d):
			return false
		}
	}
	if !interrupted {
		// The pipe stays open while grandchildren hold it; give them a moment,
		// then reap (or leave them for the runner when survivors are wanted).
		if !join(o.Grace) && o.ReapAfterExit {
			killpg(pid, syscall.SIGTERM)
			if !join(o.Grace) {
				killpg(pid, syscall.SIGKILL)
				join(o.Grace)
			}
		} else if o.ReapAfterExit {
			killpg(pid, syscall.SIGTERM)
		}
	} else {
		join(o.Grace)
	}
	mu.Lock()
	closed = true
	mu.Unlock()
	pr.Close()

	timedMu.Lock()
	to := timedOut
	timedMu.Unlock()
	res := M{"exit_code": int64(ExitCode(cmd.ProcessState)), "timed_out": to, "pid": int64(pid),
		"started_at": started, "stopped_at": Now()}
	if interrupted {
		return res, ErrInterrupted
	}
	return res, nil
}

func trimNewline(s string) string {
	if n := len(s); n > 0 && s[n-1] == '\n' {
		return s[:n-1]
	}
	return s
}

// ShellOpts configures RunShell.
type ShellOpts struct {
	Env   map[string]string // full environment (nil = inherit)
	Shell string            // default /bin/sh
}

// RunShell runs `<shell> -c cmd` non-interactively (stdin /dev/null) in its
// own process group, keeping output tails (stdout 8000, stderr 4000 chars).
// A timeout kills the group and records exit_code nil and timed_out true.
// Returns M{cmd, exit_code, timed_out, stdout, stderr, started_at, stopped_at}.
func RunShell(ctx context.Context, command, cwd string, timeout time.Duration, o ShellOpts) M {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = 600 * time.Second
	}
	shell := o.Shell
	if shell == "" {
		shell = "/bin/sh"
	}
	started := Now()
	cmd := exec.Command(shell, "-c", command)
	cmd.Dir = cwd
	if o.Env != nil {
		cmd.Env = EnvList(o.Env)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Do not wait forever for grandchildren holding the pipes after exit.
	cmd.WaitDelay = 5 * time.Second
	res := M{"cmd": command, "exit_code": nil, "timed_out": false, "stdout": "", "stderr": "",
		"started_at": started}
	if err := cmd.Start(); err != nil {
		res["stderr"] = fmt.Sprintf("%T: %v", err, err)
		if pe, ok := err.(*os.PathError); ok {
			res["stderr"] = "FileNotFoundError: " + pe.Error()
		}
		res["stopped_at"] = Now()
		return res
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var interrupted bool
	select {
	case <-done:
		res["exit_code"] = int64(ExitCode(cmd.ProcessState))
	case <-time.After(timeout):
		killpg(cmd.Process.Pid, syscall.SIGKILL)
		<-done
		res["timed_out"] = true
	case <-ctx.Done():
		interrupted = true
		killpg(cmd.Process.Pid, syscall.SIGTERM)
		killpg(cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	res["stdout"] = Tail(toValidUTF8(stdout.String()), 8000)
	res["stderr"] = Tail(toValidUTF8(stderr.String()), 4000)
	res["stopped_at"] = Now()
	if interrupted {
		res["interrupted"] = true
	}
	return res
}

func toValidUTF8(s string) string { return string(bytes.ToValidUTF8([]byte(s), []byte("�"))) }

// Which finds an executable on PATH ("" when absent).
func Which(binary string) string {
	if binary == "" {
		return ""
	}
	p, err := exec.LookPath(binary)
	if err != nil {
		return ""
	}
	return p
}

// WhichIn finds an executable using the PATH of env (nil = caller's PATH).
func WhichIn(binary string, env map[string]string) string {
	if env == nil {
		return Which(binary)
	}
	if filepath.IsAbs(binary) {
		if st, err := os.Stat(binary); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return binary
		}
		return ""
	}
	for _, dir := range filepath.SplitList(env["PATH"]) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, binary)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// Output runs argv with a timeout and returns combined stdout (or stderr when
// stdout is empty), trimmed, and the error.
func Output(argv []string, timeout time.Duration, env map[string]string) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("empty argv")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if env != nil {
		cmd.Env = EnvList(env)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	text := stdout.String()
	if text == "" {
		text = stderr.String()
	}
	return string(bytes.TrimSpace([]byte(toValidUTF8(text)))), err
}

// Recorded workers use a private control socket and a live session leader.
// Recovery never sends a signal to a PID read from disk: the authenticated
// leader signals its own group. This avoids the check-then-kill PID reuse race.
// The leader remains alive after the task exits so task-created servers can be
// verified, then stopped at teardown. An unavailable leader with live group
// members is an explicit, recoverable uncertainty, not permission to signal.
type processRecordKey struct{}

func WithProcessRecord(ctx context.Context, path string) context.Context {
	return context.WithValue(ctx, processRecordKey{}, path)
}

type ProcessRecord struct {
	Version int    `json:"version"`
	PID     int    `json:"pid"`
	Host    string `json:"host"`
	Token   string `json:"token"`
	Socket  string `json:"socket"`
	Stopped bool   `json:"stopped"`
}

type processReply struct {
	PID   int    `json:"pid"`
	Error string `json:"error,omitempty"`
}

type processIdentityError struct{ message string }

func (e *processIdentityError) Error() string { return e.message }

type processResult struct {
	ExitCode  int    `json:"exit_code"`
	PID       int    `json:"worker_pid"`
	StartedAt string `json:"started_at"`
	StoppedAt string `json:"stopped_at"`
	Error     string `json:"error,omitempty"`
}

const processGuardArg = "__ajx_owned_process_v1"
const processGuardEnv = "_AJX_OWNED_PROCESS_GUARD"

// This private helper mode works in the native CLI and in Go test binaries.
// No shell is used to encode worker argv, environment, or standard input.
func init() {
	if len(os.Args) >= 5 && os.Args[1] == processGuardArg && os.Getenv(processGuardEnv) == "1" {
		os.Unsetenv(processGuardEnv)
		serveRecordedProcess(os.Args[2], os.Args[3], os.Args[4:])
		os.Exit(1)
	}
}

func writeProcessJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".process-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func readProcessJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func processCall(rec ProcessRecord, action string) (processReply, error) {
	return processCallUntil(rec, action, time.Now().Add(2*time.Second))
}

func processCallUntil(rec ProcessRecord, action string, deadline time.Time) (processReply, error) {
	var reply processReply
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return reply, context.DeadlineExceeded
	}
	c, err := net.DialTimeout("unix", rec.Socket, min(300*time.Millisecond, remaining))
	if err != nil {
		return reply, err
	}
	defer c.Close()
	if perCall := time.Now().Add(2 * time.Second); perCall.Before(deadline) {
		deadline = perCall
	}
	_ = c.SetDeadline(deadline)
	if err = json.NewEncoder(c).Encode(map[string]string{"token": rec.Token, "action": action}); err != nil {
		return reply, err
	}
	if err = json.NewDecoder(c).Decode(&reply); err != nil {
		return reply, err
	}
	if reply.Error != "" {
		return reply, &processIdentityError{reply.Error}
	}
	if reply.PID <= 1 || (rec.PID != 0 && reply.PID != rec.PID) {
		return reply, &processIdentityError{"worker control identity does not match its launch record"}
	}
	return reply, nil
}

// ProcessGroupActive excludes zombies, which cannot act. A failed inventory is
// uncertainty. It must never be treated as proof that a group has stopped.
func ProcessGroupActive(pid int) (bool, error) {
	if pid <= 1 {
		return false, errors.New("invalid recorded process group")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/ps", "-axo", "pgid=,stat=")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	data, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("cannot inspect recorded process group: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pgid, err := strconv.Atoi(fields[0])
		if err == nil && pgid == pid && !strings.HasPrefix(fields[1], "Z") {
			return true, nil
		}
	}
	return false, nil
}

// StopRecordedProcess requests termination from the recorded owner, then proves
// that no live group members remain. Missing identity or a lost owner with live
// descendants fails closed, without signaling any potentially reused PID.
func StopRecordedProcess(path string) error {
	var rec ProcessRecord
	if err := readProcessJSON(path, &rec); err != nil {
		return fmt.Errorf("cannot read worker launch identity: %w", err)
	}
	if rec.Version != 1 || len(rec.Token) != 64 || filepath.Base(rec.Socket) != "control.sock" {
		return errors.New("invalid worker launch identity")
	}
	if rec.Stopped {
		return nil
	}
	host, err := os.Hostname()
	if err != nil || host == "" || rec.Host != host {
		return errors.New("worker launch host is missing or differs from this host; cleanup is unconfirmed")
	}
	// A durable PID can precede the owner's listening socket. Cancellation
	// during that window must wait for authenticated control, not abandon a
	// live owner after one failed connection. Never fall back to PID signals.
	deadline := time.Now().Add(5 * time.Second)
	authenticated, stopRequested := false, false
	for {
		if !stopRequested {
			reply, contactErr := processCallUntil(rec, "status", deadline)
			var identityErr *processIdentityError
			if errors.As(contactErr, &identityErr) {
				return contactErr
			}
			if contactErr == nil {
				authenticated = true
				if rec.PID == 0 {
					rec.PID = reply.PID
					// Preserve recovered identity before the stop request:
					// this coordinator can also die during recovery.
					if err := writeProcessJSON(path, rec); err != nil {
						return err
					}
				}
				_, stopErr := processCallUntil(rec, "stop", deadline)
				if errors.As(stopErr, &identityErr) {
					return stopErr
				}
				stopRequested = stopErr == nil
			}
		}
		if rec.PID > 1 {
			active, err := ProcessGroupActive(rec.PID)
			if err != nil {
				return err
			}
			if !active {
				rec.Stopped = true
				if err := writeProcessJSON(path, rec); err != nil {
					return err
				}
				// A stale/corrupted record cannot authorize filesystem deletion.
				if authenticated {
					removeProcessControl(rec)
				}
				return nil
			}
		}
		if time.Now().After(deadline) {
			if rec.PID <= 1 {
				return errors.New("worker launch was not acknowledged; cannot establish whether its owner stopped")
			}
			if !stopRequested {
				return errors.New("worker owner is unreachable and its group is still active; no process was signaled")
			}
			return errors.New("worker process group did not stop; cleanup is unconfirmed")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func removeProcessControl(rec ProcessRecord) {
	dir := filepath.Dir(rec.Socket)
	if !strings.HasPrefix(filepath.Base(dir), ".ajx-worker-") {
		return
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0700 {
		return
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Geteuid() {
		return
	}
	// Pin the checked directory. Remove only direct children through this
	// descriptor, so a path replacement cannot redirect the unlink operations.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return
	}
	defer root.Close()
	pinned, err := root.Stat(".")
	if err != nil || !os.SameFile(st, pinned) {
		return
	}
	for _, name := range []string{"control.sock", "result.json"} {
		info, err := root.Lstat(name)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(owner.Uid) != os.Geteuid() {
			continue
		}
		if name == "control.sock" && info.Mode()&os.ModeSocket == 0 ||
			name == "result.json" && !info.Mode().IsRegular() {
			continue
		}
		_ = root.Remove(name)
	}
	// Non-recursive removal cannot erase any unrelated child if the path was
	// changed after validation.
	_ = os.Remove(dir)
}

func serveRecordedProcess(socket, token string, argv []string) {
	if len(token) != 64 || len(argv) == 0 {
		return
	}
	// A caught disposition resets to default in the exec'ed worker. The owner
	// itself must survive TERM long enough to escalate its own group to KILL.
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		for range sigs {
		}
	}()
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return
	}
	defer ln.Close()
	pid := os.Getpid()
	stop := func() {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		time.Sleep(100 * time.Millisecond)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	// If the launching coordinator dies before granting permission, no worker
	// starts and the idle owner cannot remain forever.
	idle := time.AfterFunc(10*time.Second, stop)
	started := false
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		var req map[string]string
		reply := processReply{PID: pid}
		stopNow := false
		if err := json.NewDecoder(c).Decode(&req); err != nil || req["token"] != token {
			reply.Error = "worker control identity rejected"
		} else {
			switch req["action"] {
			case "status":
			case "stop":
				stopNow = true
			case "start":
				if !started {
					started = true
					idle.Stop()
					child := exec.Command(argv[0], argv[1:]...)
					child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
					began := Now()
					if err := child.Start(); err != nil {
						_ = writeProcessJSON(filepath.Join(filepath.Dir(socket), "result.json"), processResult{ExitCode: -1, StartedAt: began, StoppedAt: Now(), Error: err.Error()})
					} else {
						go func() {
							_ = child.Wait()
							_ = writeProcessJSON(filepath.Join(filepath.Dir(socket), "result.json"), processResult{ExitCode: ExitCode(child.ProcessState), PID: child.Process.Pid, StartedAt: began, StoppedAt: Now()})
						}()
					}
					// The owner must not keep the worker's output pipe open.
					os.Stdin.Close()
					os.Stdout.Close()
					os.Stderr.Close()
				}
			default:
				reply.Error = "unknown worker control action"
			}
		}
		_ = json.NewEncoder(c).Encode(reply)
		c.Close()
		if stopNow {
			idle.Stop()
			stop()
			return
		}
	}
}

func runRecordedStreaming(ctx context.Context, path string, argv []string, cwd, outPath, errPath string, timeout time.Duration, o StreamOpts) (result M, retErr error) {
	if len(argv) == 0 {
		return nil, errors.New("empty argv")
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, errors.New("refusing to replace an existing worker launch record")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", ".ajx-worker-")
	if err != nil {
		return nil, err
	}
	// Unix socket paths are limited to 104 bytes on macOS (108 on Linux).
	// Test and CI temporary roots can already exceed that before our suffix.
	if len(filepath.Join(dir, "control.sock")) >= 104 {
		_ = os.Remove(dir)
		dir, err = os.MkdirTemp("/tmp", ".ajx-worker-")
		if err != nil {
			return nil, err
		}
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		os.Remove(dir)
		return nil, err
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		os.Remove(dir)
		return nil, errors.New("cannot identify worker launch host")
	}
	rec := ProcessRecord{Version: 1, Host: host, Token: hex.EncodeToString(nonce[:]), Socket: filepath.Join(dir, "control.sock")}
	if err := writeProcessJSON(path, rec); err != nil {
		os.Remove(dir)
		return nil, err
	}
	launched, keepOwner := false, false
	defer func() {
		if !launched {
			// No child can act: retain that positive evidence even if creating
			// capture files or spawning the owner failed.
			rec.Stopped = true
			retErr = errors.Join(retErr, writeProcessJSON(path, rec))
			_ = os.Remove(dir)
		} else if !keepOwner {
			stopErr := StopRecordedProcess(path)
			retErr = errors.Join(retErr, stopErr)
			if stopErr == nil && result != nil && result["stopped_at"] == nil {
				result["stopped_at"] = Now()
			}
		}
	}()
	out, err := os.Create(outPath)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	errFile, err := os.Create(errPath)
	if err != nil {
		return nil, err
	}
	defer errFile.Close()
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer pr.Close()
	defer pw.Close()
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := append([]string{processGuardArg, rec.Socket, rec.Token}, argv...)
	cmd := exec.Command(exe, args...)
	cmd.Dir = cwd
	env := o.Env
	if env == nil {
		env = Environ()
	}
	cmd.Env = append(EnvList(env), processGuardEnv+"=1")
	cmd.Stdout, cmd.Stderr = pw, errFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var stdin io.WriteCloser
	if o.StdinData != nil {
		stdin, err = cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
	}
	began, startedAt := time.Now(), Now()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	launched = true
	pw.Close()
	ownerDone := make(chan struct{})
	go func() { _ = cmd.Wait(); close(ownerDone) }()
	rec.PID = cmd.Process.Pid
	result = M{"pid": int64(rec.PID), "worker_pid": int64(0), "exit_code": int64(-1),
		"timed_out": false, "started_at": startedAt, "stopped_at": nil, "process_record": path}
	if err := writeProcessJSON(path, rec); err != nil {
		return result, err
	}
	// Grant start only after the PID, endpoint and nonce are durable.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ctx.Err() != nil {
			result["exit_code"] = int64(-int(syscall.SIGKILL))
			return result, ErrInterrupted
		}
		if timeout > 0 && time.Since(began) >= timeout {
			result["exit_code"], result["timed_out"] = int64(-int(syscall.SIGKILL)), true
			return result, nil
		}
		_, err = processCall(rec, "start")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return result, fmt.Errorf("worker owner did not start: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		r := bufio.NewReaderSize(pr, 1<<16)
		for {
			line, err := r.ReadString('\n')
			if len(line) > 0 {
				_, _ = out.WriteString(Compact(M{"at": Now(), "line": trimNewline(line)}) + "\n")
			}
			if err != nil {
				return
			}
		}
	}()
	if stdin != nil {
		go func() { _, _ = io.WriteString(stdin, *o.StdinData); _ = stdin.Close() }()
	}
	var proc processResult
	interrupted, timedOut := false, false
	for {
		if err := readProcessJSON(filepath.Join(dir, "result.json"), &proc); err == nil {
			break
		}
		if ctx.Err() != nil {
			interrupted = true
			break
		}
		if timeout > 0 && time.Since(began) >= timeout {
			timedOut = true
			break
		}
		select {
		case <-ownerDone:
			return nil, errors.New("worker owner exited before reporting the task result")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if interrupted || timedOut || o.ReapAfterExit {
		if interrupted || timedOut {
			result["exit_code"], result["timed_out"] = int64(-int(syscall.SIGKILL)), timedOut
		}
		if err := StopRecordedProcess(path); err != nil {
			return result, err
		}
	}
	if o.Grace <= 0 {
		o.Grace = 3 * time.Second
	}
	select {
	case <-readerDone:
	case <-time.After(o.Grace):
	}
	pr.Close()
	<-readerDone // do not race capture writes with closing the output file
	result = M{"pid": int64(rec.PID), "worker_pid": int64(proc.PID), "exit_code": int64(proc.ExitCode), "timed_out": timedOut, "started_at": proc.StartedAt, "stopped_at": proc.StoppedAt, "process_record": path}
	if interrupted || timedOut {
		result["exit_code"] = int64(-int(syscall.SIGKILL))
		result["started_at"] = startedAt
		result["stopped_at"] = Now()
	}
	if interrupted {
		return result, ErrInterrupted
	}
	if proc.Error != "" {
		return result, errors.New(proc.Error)
	}
	keepOwner = !timedOut && !o.ReapAfterExit
	return result, nil
}
