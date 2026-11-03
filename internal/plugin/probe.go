package plugin

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/royosherove/ajx/internal/util"
)

// CommandProbe preserves process status separately from its human-readable
// output. An unknown probe is not evidence that authentication succeeded.
// It intentionally contains neither argv nor environment values.
type CommandProbe struct {
	Checked  bool   `json:"checked"`
	OK       bool   `json:"ok"`
	ExitCode *int   `json:"exit_code"`
	TimedOut bool   `json:"timed_out"`
	Output   string `json:"output"`
	Error    string `json:"error,omitempty"`
}

// Failure describes process failure without interpreting output as a status.
func (p CommandProbe) Failure() string {
	switch {
	case p.TimedOut:
		return "timed out"
	case p.Error != "":
		return p.Error
	case p.ExitCode != nil && *p.ExitCode != 0:
		return fmt.Sprintf("exit %d", *p.ExitCode)
	}
	return ""
}

type probeBuffer struct{ data []byte }

func (b *probeBuffer) Write(p []byte) (int, error) {
	n := len(p)
	const limit = 8000
	if len(p) >= limit {
		b.data = append(b.data[:0], p[len(p)-limit:]...)
	} else {
		b.data = append(b.data, p...)
		if len(b.data) > limit {
			b.data = b.data[len(b.data)-limit:]
		}
	}
	return n, nil
}

// probeCommand is headless and bounded, including when descendants retain
// stdout after the initial process exits.
func probeCommand(argv []string, timeout time.Duration, env map[string]string) CommandProbe {
	if len(argv) == 0 {
		return CommandProbe{}
	}
	p := CommandProbe{Checked: true}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	binary := util.WhichIn(argv[0], env)
	if binary == "" {
		p.Error = "executable not found: " + argv[0]
		return p
	}
	cmd := exec.CommandContext(ctx, binary, argv[1:]...)
	if env != nil {
		cmd.Env = util.EnvList(env)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 250 * time.Millisecond
	var stdout, stderr probeBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if cmd.Process != nil {
		// Version/identity commands must not leave background descendants.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	p.TimedOut = ctx.Err() == context.DeadlineExceeded
	if cmd.ProcessState != nil {
		code := util.ExitCode(cmd.ProcessState)
		p.ExitCode = &code
	}
	p.OK = err == nil && !p.TimedOut
	if err != nil && (p.ExitCode == nil || *p.ExitCode == 0) {
		p.Error = err.Error()
	}
	text := stdout.data
	if len(bytes.TrimSpace(text)) == 0 {
		text = stderr.data
	}
	p.Output = strings.TrimSpace(string(bytes.ToValidUTF8(text, []byte("�"))))
	return p
}

// ProbeHarnessVersion invokes only the adapter's declared version command.
// Synthetic/custom adapters without such a command retain their Version text,
// with checked=false; we never run an arbitrary worker command as a probe.
func ProbeHarnessVersion(h Harness) CommandProbe {
	if len(h.Base().VersionArgv) == 0 {
		return CommandProbe{Output: h.Version()}
	}
	p := probeCommand(h.Base().VersionArgv, 20*time.Second, nil)
	p.Output = util.Clip(strings.SplitN(p.Output, "\n", 2)[0], 120)
	return p
}

// ProbeAuthIdentity dispatches the existing Identity method so built-in
// defaults (for example Kiro's whoami) and custom implementations keep working.
// IdentityOf records the structured result on the per-cell auth instance.
// A descriptive Identity implementation that runs no recorded command remains
// unknown rather than being promoted to a successful authentication check.
func ProbeAuthIdentity(a Auth, env map[string]string) CommandProbe {
	b := a.Base()
	b.identityProbe = nil
	text, _ := a.Identity(env)
	if b.identityProbe != nil {
		p := *b.identityProbe
		p.Output = Excerpt(p.Output, 400)
		return p
	}
	return CommandProbe{Output: text}
}
