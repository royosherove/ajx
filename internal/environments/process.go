package environments

import (
	"context"
	"io"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/royosherove/ajx/internal/util"
)

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, p...)
	if len(t.data) > t.limit {
		t.data = append([]byte(nil), t.data[len(t.data)-t.limit:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return decodeReplace(t.data)
}

// runTailProcess runs argv (never through a shell) in its own process group
// with a timeout and bounded in-memory output: the last 8000 bytes of stdout
// and 4000 of stderr (Python _tail_process). Even after a normal exit the
// group is killed, so no local descendants are left; this kills the Docker
// client only, and the runner separately stops container workers. Returns
// M{exit_code, timed_out, stdout, stderr, started_at, stopped_at}; exit_code
// is nil on a timeout. Cancelling ctx kills the group and returns
// util.ErrInterrupted with the partial record.
func runTailProcess(ctx context.Context, argv []string, cwd string, timeout float64, env map[string]string) (util.M, error) {
	started := util.Now()
	result := util.M{"exit_code": nil, "timed_out": false, "stdout": "", "stderr": "",
		"started_at": started, "stopped_at": nil}
	if timeout <= 0 || math.IsNaN(timeout) || math.IsInf(timeout, 0) {
		return nil, fail("environment.timeout", "expected a finite positive number of seconds")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	binary := argv[0]
	if !strings.Contains(binary, "/") {
		// Like Python's Popen(env=...): search the child's PATH.
		if found := util.WhichIn(binary, env); found != "" {
			binary = found
		}
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		result["stderr"] = "Cannot start environment command (" + osErrorName(err) + ")"
		result["stopped_at"] = util.Now()
		return result, nil
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		result["stderr"] = "Cannot start environment command (" + osErrorName(err) + ")"
		result["stopped_at"] = util.Now()
		return result, nil
	}
	cmd := exec.Command(binary, argv[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Dir = cwd
	cmd.Env = util.EnvList(env)
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = outW, errW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	startErr := cmd.Start()
	outW.Close()
	errW.Close()
	if startErr != nil {
		outR.Close()
		errR.Close()
		result["stderr"] = "Cannot start environment command (" + osErrorName(startErr) + ")"
		result["stopped_at"] = util.Now()
		return result, nil
	}
	stdout, stderr := &tailBuffer{limit: 8000}, &tailBuffer{limit: 4000}
	var readers sync.WaitGroup
	done := make([]chan struct{}, 2)
	for i, pair := range []struct {
		r *os.File
		w io.Writer
	}{{outR, stdout}, {errR, stderr}} {
		done[i] = make(chan struct{})
		readers.Add(1)
		go func(r *os.File, w io.Writer, finished chan struct{}) {
			defer readers.Done()
			defer close(finished)
			buf := make([]byte, 4096)
			for {
				n, err := r.Read(buf)
				if n > 0 {
					w.Write(buf[:n])
				}
				if err != nil {
					return
				}
			}
		}(pair.r, pair.w, done[i])
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	interrupted := false
	timer := time.NewTimer(time.Duration(timeout * float64(time.Second)))
	select {
	case <-waited:
		result["exit_code"] = int64(util.ExitCode(cmd.ProcessState))
	case <-timer.C:
		result["timed_out"] = true
	case <-ctx.Done():
		interrupted = true
	}
	timer.Stop()
	// Even after a normal shell exit, do not leave local shell descendants.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if result["exit_code"] == nil {
		<-waited
	}
	for _, d := range done {
		select {
		case <-d:
		case <-time.After(time.Second):
		}
	}
	outR.Close()
	errR.Close()
	result["stdout"] = stdout.String()
	result["stderr"] = stderr.String()
	result["stopped_at"] = util.Now()
	if interrupted {
		result["interrupted"] = true
		return result, util.ErrInterrupted
	}
	return result, nil
}
