package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/royosherove/ajx/internal/util"
)

type coordinatorChecks struct {
	localIPC          func() error
	processInspection func() error
}

// Replaceable in tests without starting a guardian or worker.
var doctorCoordinatorChecks = coordinatorChecks{
	localIPC:          func() error { return coordinatorUnixSocket("") },
	processInspection: coordinatorProcessInspection,
}

const coordinatorRemediation = "Permit local Unix socket IPC and /bin/ps process inspection for the coordinator, " +
	"or run the coordinator outside that sandbox. Workers can retain their environment restrictions."

func (checks coordinatorChecks) run(r *report) bool {
	problems := []string{}
	results := []any{}
	for _, check := range []struct {
		name  string
		probe func() error
	}{
		{"local_ipc", checks.localIPC},
		{"process_inspection", checks.processInspection},
	} {
		problem := ""
		if err := check.probe(); err != nil {
			problem = err.Error()
			problems = append(problems, check.name+": "+problem)
			r.line("    FAILED coordinator %s: %s", check.name, problem)
		}
		results = append(results, util.M{"name": check.name, "ready": problem == "", "problem": nilIfEmpty(problem)})
	}
	ready := len(problems) == 0
	doc := util.M{"ready": ready, "checks": results, "problems": strs(problems), "remediation": nil}
	if !ready {
		doc["remediation"] = coordinatorRemediation
		r.line("    %s", coordinatorRemediation)
	}
	r.doc["coordinator"] = doc
	return ready
}

// Match the guardian's socket filename, directory-prefix length and default
// TMPDIR fallback. Explicit parents are kept for targeted cleanup tests.
// All traffic stays on this private local socket; no worker is launched.
func coordinatorUnixSocket(parent string) (err error) {
	dir, err := os.MkdirTemp(parent, ".ajx-doctor-")
	if err != nil {
		return fmt.Errorf("create temporary directory for local IPC: %w", err)
	}
	defer func() {
		if dir != "" {
			if cleanupErr := os.RemoveAll(dir); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("remove local IPC probe: %w", cleanupErr))
			}
		}
	}()
	// Keep this rule aligned with runRecordedStreaming in util/proc.go:
	// macOS allows 104 bytes and Linux 108, so the guardian uses 104 on both.
	if parent == "" && len(filepath.Join(dir, "control.sock")) >= 104 {
		if err := os.Remove(dir); err != nil {
			return fmt.Errorf("remove overlong local IPC probe directory: %w", err)
		}
		dir, err = os.MkdirTemp("/tmp", ".ajx-doctor-")
		if err != nil {
			return fmt.Errorf("create local IPC fallback directory in /tmp (coordinator needs write access): %w", err)
		}
	}
	path := filepath.Join(dir, "control.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listen on local Unix socket (coordinator needs local IPC permission): %w", err)
	}
	defer listener.Close()
	deadline := time.Now().Add(time.Second)
	if err := listener.SetDeadline(deadline); err != nil {
		return fmt.Errorf("set local IPC accept deadline: %w", err)
	}
	client, err := net.DialTimeout("unix", path, time.Until(deadline))
	if err != nil {
		return fmt.Errorf("connect to local Unix socket: %w", err)
	}
	defer client.Close()
	server, err := listener.AcceptUnix()
	if err != nil {
		return fmt.Errorf("accept local Unix socket connection: %w", err)
	}
	defer server.Close()
	for _, conn := range []net.Conn{client, server} {
		if err := conn.SetDeadline(deadline); err != nil {
			return fmt.Errorf("set local IPC transfer deadline: %w", err)
		}
	}
	// Exercise request and reply permissions without a goroutine or service.
	for _, pair := range [][2]net.Conn{{client, server}, {server, client}} {
		if _, err := pair[0].Write([]byte{1}); err != nil {
			return fmt.Errorf("write local Unix socket: %w", err)
		}
		var reply [1]byte
		if _, err := io.ReadFull(pair[1], reply[:]); err != nil {
			return fmt.Errorf("read local Unix socket: %w", err)
		}
		if reply[0] != 1 {
			return errors.New("local Unix socket probe received an unexpected reply")
		}
	}
	return nil
}

func coordinatorProcessInspection() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Match the guardian's inventory command. Do not expose the process
	// inventory in doctor output; only verify that our own group is visible.
	cmd := exec.CommandContext(ctx, "/bin/ps", "-axo", "pgid=,stat=")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	cmd.WaitDelay = 100 * time.Millisecond
	data, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("/bin/ps process inspection failed: %w", err)
	}
	group := syscall.Getpgrp()
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pgid, err := strconv.Atoi(fields[0])
		if err == nil && pgid == group && !strings.HasPrefix(fields[1], "Z") {
			return nil
		}
	}
	return errors.New("/bin/ps cannot observe the coordinator's own process group")
}
