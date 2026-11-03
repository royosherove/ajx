package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadinessProbePreservesProcessFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "/usr/bin:/bin")
	broken := filepath.Join(t.TempDir(), "broken")
	if err := os.WriteFile(broken, []byte("#!/nonexistent/ajx-test-interpreter\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		argv []string
		code *int
		ok   bool
	}{
		{"success", []string{"/bin/sh", "-c", "printf identity"}, intPtr(0), true},
		{"silent failure", []string{"/bin/sh", "-c", "exit 7"}, intPtr(7), false},
		{"text failure", []string{"/bin/sh", "-c", "echo login-needed; exit 7"}, intPtr(7), false},
		{"stderr failure", []string{"/bin/sh", "-c", "echo failed >&2; exit 9"}, intPtr(9), false},
		{"missing binary", []string{"/nonexistent/ajx-test-probe"}, nil, false},
		{"missing interpreter", []string{broken}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := probeCommand(tc.argv, time.Second, nil)
			if !p.Checked || p.OK != tc.ok || p.TimedOut {
				t.Fatalf("probe: %+v", p)
			}
			if (p.ExitCode == nil) != (tc.code == nil) || (tc.code != nil && *p.ExitCode != *tc.code) {
				t.Fatalf("exit: %+v", p)
			}
			if !tc.ok && p.Failure() == "" {
				t.Fatalf("missing failure: %+v", p)
			}
		})
	}
}

func intPtr(n int) *int { return &n }

func TestReadinessProbeTimeoutAndRetainedPipeAreBounded(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	start := time.Now()
	p := probeCommand([]string{"/bin/sh", "-c", "sleep 30"}, 30*time.Millisecond, nil)
	if !p.TimedOut || p.OK || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout probe: %+v", p)
	}
	start = time.Now()
	p = probeCommand([]string{"/bin/sh", "-c", "sleep 30 &"}, time.Second, nil)
	if p.OK || p.Failure() == "" || time.Since(start) > 3*time.Second {
		t.Fatalf("retained pipe probe: %+v", p)
	}
}

type readinessAuth struct {
	*AuthBase
	unknown bool
}

func (a *readinessAuth) Identity(env map[string]string) (string, bool) {
	if a.unknown {
		return "not checked: synthetic", true
	}
	return a.IdentityOf([]string{"/bin/sh", "-c", "echo login-needed; exit 7"}, env)
}

func TestReadinessAuthProbeDispatchesAdapterAndDoesNotReuseFailure(t *testing.T) {
	a := &readinessAuth{AuthBase: NewAuthBase(M{})}
	a.Self = a
	p := ProbeAuthIdentity(a, map[string]string{"PATH": "/usr/bin:/bin"})
	if p.OK || !p.Checked || p.ExitCode == nil || *p.ExitCode != 7 || p.Output != "login-needed" {
		t.Fatalf("adapter failure lost: %+v", p)
	}
	a.unknown = true
	p = ProbeAuthIdentity(a, nil)
	if p.Checked || p.OK || p.ExitCode != nil || !strings.Contains(p.Output, "not checked") {
		t.Fatalf("unknown identity promoted to success: %+v", p)
	}
}
