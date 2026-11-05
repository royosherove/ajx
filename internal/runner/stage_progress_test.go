package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/util"
)

func TestStageProgressPrecedesWorkAndReportsFailure(t *testing.T) {
	tmp := t.TempDir()
	var logs []string
	r := newRun(t, loadSpec(t, writeTrial(t, tmp, "", "")), func(line string) {
		logs = append(logs, line)
	})
	r.Override["extract"] = func(r *Run) error {
		if len(logs) == 0 || !strings.HasSuffix(logs[len(logs)-1], "stage extract: running") {
			t.Fatalf("reporting work started without progress: %v", logs)
		}
		state := readMapT(t, r.StatePath)
		if util.Get(state, "stages", "extract", "status") != "running" {
			t.Fatal("progress was emitted before the stage state was saved")
		}
		return runtimeErr("synthetic reporting failure")
	}
	if _, err := r.Go(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	output := strings.Join(logs, "\n")
	for _, message := range []string{"stage extract: error", "stage render: running",
		"stage render: done", "stage archive: done"} {
		if !strings.Contains(output, message) {
			t.Fatalf("missing %q in progress:\n%s", message, output)
		}
	}
	if strings.Contains(output, "stage extract: done") {
		t.Fatal("a failed stage was reported as completed")
	}
}

func TestStageProgressDoesNotClaimAnUnsavedTransition(t *testing.T) {
	var logs []string
	r := newRun(t, loadSpec(t, writeTrial(t, t.TempDir(), "", "")), func(line string) {
		logs = append(logs, line)
	})
	// A directory cannot be replaced with the state file.
	r.StatePath = filepath.Join(t.TempDir(), "state.json")
	if err := os.Mkdir(r.StatePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.mark("extract", "done", nil); err == nil {
		t.Fatal("expected persistence to fail")
	}
	if len(logs) != 0 {
		t.Fatalf("claimed a transition that was not saved: %v", logs)
	}
}
