package util

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
