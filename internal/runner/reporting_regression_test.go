package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/reporter"
	"github.com/royosherove/ajx/internal/util"
)

func TestRunRecordFollowsFinalStageFailure(t *testing.T) {
	for _, stage := range []string{"render", "archive"} {
		t.Run(stage, func(t *testing.T) {
			tmp := t.TempDir()
			r := newRun(t, loadSpec(t, writeTrial(t, tmp, "", "")), nil)
			r.Override["extract"] = noModelExtract
			r.Override[stage] = func(r *Run) error {
				if stage == "render" {
					if err := r.stageRender(); err != nil {
						return err
					}
				}
				return runtimeErr("synthetic final-stage failure")
			}
			state, err := r.Go(context.Background(), nil, false)
			if err != nil {
				t.Fatal(err)
			}
			run := readMapT(t, filepath.Join(r.RunDir, "run.json"))
			if got := util.Get(run, "stages", stage); got != "error" {
				t.Fatalf("run.json says %s=%v, state=%v", stage, got, statuses(state))
			}
			if util.Compact(run["stages"]) != util.Compact(statuses(state)) {
				t.Fatalf("run.json stages differ from final state: %v / %v", run["stages"], statuses(state))
			}
		})
	}
}

func TestFinalRecordWriteFailureDoesNotPreventCleanup(t *testing.T) {
	tmp := t.TempDir()
	r := newRun(t, loadSpec(t, writeTrial(t, tmp, "", "")), nil)
	r.Override["extract"] = noModelExtract
	r.Override["render"] = func(r *Run) error {
		// A broken derived record must not block archival or credential cleanup.
		return os.Mkdir(filepath.Join(r.RunDir, "run.json"), 0o755)
	}
	state, err := r.Go(context.Background(), nil, false)
	if err == nil {
		t.Fatal("expected the derived-record write error")
	}
	// Archive's final evidence-pointer rewrite also encounters the broken
	// record, so the stage is an error; its preceding cleanup must still run.
	if util.Get(state, "stages", "teardown", "status") != "done" ||
		!exists(filepath.Join(r.RunDir, "workspace", "made.txt")) ||
		exists(util.S(util.Get(state, "paths", "workspace"))) {
		t.Fatalf("metadata failure prevented archival: %v", state["stages"])
	}
}

type narrationFailureHarness struct {
	fakeHarness
	failure error
	tel     M
}

func (h *narrationFailureHarness) Narrate(*plugin.Ctx, string) (M, error) {
	if h.failure != nil {
		return nil, h.failure
	}
	return M{"exit_code": 0}, nil
}

func (h *narrationFailureHarness) Normalize(*plugin.Ctx, string, []string) (M, error) {
	return h.tel, nil
}

func TestNarrationFailureSurvivesReconstruction(t *testing.T) {
	saved := reporter.Call
	reporter.Call = func(context.Context, M, string, string, string, M) (M, error) {
		return M{"text": "# Journey: synthetic reconstruction", "proc": M{"exit_code": 0}}, nil
	}
	defer func() { reporter.Call = saved }()
	for _, tc := range []struct {
		name, category, message string
		canResume, session      bool
		failure                 error
		tel                     M
	}{
		{"safeguard", "safety_stop", "model's safeguards", true, true, nil,
			M{"final_text": "", "harness_reported": M{"safety_stops": 1}}},
		{"invocation", "resume_error", "synthetic resume failure", true, true,
			runtimeErr("synthetic resume failure"), nil},
		{"comment-terminator", "resume_error", "synthetic —> failure", true, true,
			runtimeErr("synthetic -->\nfailure"), nil},
		{"long-message", "resume_error", "[truncated; see narration_failure.message]", true, true,
			runtimeErr("%s", "synthetic -->\n"+strings.Repeat("λ detail ", 10000)), nil},
		{"unsupported", "resume_unsupported", "does not support", false, true, nil, nil},
		{"missing-session", "session_unavailable", "session identifier", true, false, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			r := newRun(t, loadSpec(t, writeTrial(t, tmp, "", "")), nil)
			r.Harness = &narrationFailureHarness{
				fakeHarness: fakeHarness{BaseHarness: plugin.BaseHarness{CanResume: tc.canResume}},
				failure:     tc.failure, tel: tc.tel,
			}
			r.State["execute"] = M{}
			if tc.session {
				r.State["execute"] = M{"session_id": "synthetic-session"}
			}
			if err := r.mark("execute", "done", nil); err != nil {
				t.Fatal(err)
			}
			if err := util.WriteJSON(filepath.Join(r.RunDir, "telemetry.execute.json"), M{}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(r.RunDir, "digest.md"), []byte("Synthetic evidence."), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := r.stageNarrate(); err != nil {
				t.Fatal(err)
			}
			if got := util.Get(r.State, "narration_failure", "category"); got != tc.category {
				t.Fatalf("failure category %v, want %s", got, tc.category)
			}
			journey := readFile(t, filepath.Join(r.RunDir, "journey.md"))
			if !strings.Contains(journey, tc.message) || strings.Contains(journey, "harness cannot resume or resume failed") {
				t.Fatalf("provenance lost failure: %s", journey)
			}
			header := strings.SplitN(journey, "\n", 2)[0]
			if len([]rune(header)) > 600 || strings.Count(header, "-->") != 1 {
				t.Fatalf("provenance header is unbounded or closes early: %.650s", header)
			}
			if tc.failure != nil && util.Get(r.State, "narration_failure", "message") != pyErrString(tc.failure) {
				t.Fatal("structured state lost the full narration failure")
			}
			if err := r.stageRender(); err != nil {
				t.Fatal(err)
			}
			run := readMapT(t, filepath.Join(r.RunDir, "run.json"))
			if util.Get(run, "narration_failure", "category") != tc.category {
				t.Fatalf("run.json lost failure: %v", run["narration_failure"])
			}
			if tc.failure != nil && util.Get(run, "narration_failure", "message") != pyErrString(tc.failure) {
				t.Fatal("run.json lost the full narration failure")
			}
		})
	}
}
