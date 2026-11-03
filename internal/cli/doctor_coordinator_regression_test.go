package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/util"
)

func TestCoordinatorUnixSocketRoundTripAndCleanup(t *testing.T) {
	// Keep this path short on macOS, which limits Unix socket paths to 104
	// bytes; ordinary t.TempDir names include this long test name.
	parent, err := os.MkdirTemp("/tmp", "ajx-ipc-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	if err := coordinatorUnixSocket(parent); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("probe left temporary files: %v %v", entries, err)
	}
	// A listen failure must remove its temporary directory too.
	longParent := filepath.Join(parent, strings.Repeat("x", 100))
	if err := os.Mkdir(longParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := coordinatorUnixSocket(longParent); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("expected socket-path failure, got %v", err)
	}
	entries, err = os.ReadDir(longParent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed probe left temporary files: %v %v", entries, err)
	}
	if err := coordinatorUnixSocket(filepath.Join(parent, "absent")); err == nil {
		t.Fatal("missing temporary parent accepted")
	}
}

func TestCoordinatorProcessInspectionSeesOwnGroup(t *testing.T) {
	if err := coordinatorProcessInspection(); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorCoordinatorLongTMPDIRUsesRuntimeFallback(t *testing.T) {
	offline(t)
	trial := headlessDoctorFixture(t, `['/bin/echo','synthetic-identity']`, "#!/bin/sh\necho synthetic-v1\n", "", "")
	longTMP := filepath.Join(t.TempDir(), strings.Repeat("x", 110))
	if err := os.Mkdir(longTMP, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", longTMP)
	// Exercise the actual default probe, including the guardian's /tmp
	// fallback, rather than supplying an explicit short parent.
	code, out, stderr := run(t, "doctor", trial, "--json")
	doc := decode(t, out)
	if code != 0 || doc["ready"] != true || util.Get(doc, "coordinator", "ready") != true || stderr != "" {
		t.Fatalf("long TMPDIR should remain ready when /tmp is allowed: %d %s %s", code, out, stderr)
	}
	entries, err := os.ReadDir(longTMP)
	if err != nil || len(entries) != 0 {
		t.Fatalf("long TMPDIR retained probe files: %v %v", entries, err)
	}
}

func TestCoordinatorReadinessInjectedFailures(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ipc, process error
		wantProblems int
	}{
		{"ready", nil, nil, 0},
		{"IPC denied", errors.New("local socket permission denied"), nil, 1},
		{"inspection denied", nil, errors.New("/bin/ps permission denied"), 1},
		{"both denied", errors.New("local socket denied"), errors.New("/bin/ps denied"), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ipcCalls, processCalls := 0, 0
			checks := coordinatorChecks{
				localIPC:          func() error { ipcCalls++; return tc.ipc },
				processInspection: func() error { processCalls++; return tc.process },
			}
			r := newReport("doctor", true)
			ready := checks.run(r)
			doc := util.MapOr(r.doc["coordinator"])
			if ready != (tc.wantProblems == 0) || doc["ready"] != ready ||
				len(util.List(doc["problems"])) != tc.wantProblems || ipcCalls != 1 || processCalls != 1 {
				t.Fatalf("readiness=%v document=%v calls=%d/%d", ready, doc, ipcCalls, processCalls)
			}
			if ready {
				if len(r.lines) != 0 || doc["remediation"] != nil {
					t.Fatalf("successful probe changed text output: %v", r)
				}
			} else if doc["remediation"] != coordinatorRemediation ||
				!strings.Contains(strings.Join(r.lines, "\n"), "Workers can retain their environment restrictions") {
				t.Fatalf("missing actionable coordinator remediation: %v", r)
			}
		})
	}
}

func TestDoctorCoordinatorFailurePreventsReady(t *testing.T) {
	offline(t)
	original := doctorCoordinatorChecks
	t.Cleanup(func() { doctorCoordinatorChecks = original })
	doctorCoordinatorChecks = coordinatorChecks{
		localIPC:          func() error { return errors.New("synthetic IPC denial") },
		processInspection: func() error { return nil },
	}
	trial := headlessDoctorFixture(t, `['/bin/echo','synthetic-identity']`, "#!/bin/sh\necho synthetic-v1\n", "", "")
	code, out, stderr := run(t, "doctor", trial, "--json")
	doc := decode(t, out)
	if code != 1 || doc["ready"] != false || util.Get(doc, "coordinator", "ready") != false ||
		!strings.Contains(out, coordinatorRemediation) || stderr != "" {
		t.Fatalf("doctor claimed readiness: %d %s %s", code, out, stderr)
	}
}
