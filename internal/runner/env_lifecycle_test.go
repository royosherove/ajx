package runner

// Offline integration of environment profiles with real synthetic processes:
// ports of skills/ajx/tests/test_environment_lifecycle.py. Python's
// mock.patch.object(...) becomes a spy runner, test hooks on the harness and
// auth, and the Run.Override stage map.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/environments"
	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

const probeExecute = `set -e
test "$(cat "$HOME/installed-tool")" = ready
test -z "${AJX_TEST_AMBIENT_SECRET+x}"
printf '{"home": "%s", "workspace": "%s", "tool": "ready"}' "$HOME" "$(pwd -P)" > result.json
printf 'one execution' > executions.txt
echo synthetic task completed
`

const probeNarrate = `set -e
test "$(cat "$HOME/installed-tool")" = ready
test -e "$HOME/teardown-ran"
echo same environment available for narration
`

// envProbe is a worker that needs the tool prepared in its isolated home.
type envProbe struct {
	plugin.BaseHarness
	execute func(h *envProbe, ctx *plugin.Ctx) (M, error)
	narrate func(h *envProbe, ctx *plugin.Ctx) (M, error)
	execs   int
}

func (h *envProbe) Execute(ctx *plugin.Ctx) (M, error) {
	h.execs++
	if h.execute != nil {
		return h.execute(h, ctx)
	}
	proc, err := h.Run(ctx, []string{"sh", "-c", probeExecute}, "execute", plugin.RunOpts{})
	if err != nil {
		return proc, err
	}
	proc["session_id"] = "synthetic-environment-session"
	return proc, nil
}

func (h *envProbe) Narrate(ctx *plugin.Ctx, prompt string) (M, error) {
	if h.narrate != nil {
		return h.narrate(h, ctx)
	}
	proc, err := h.Run(ctx, []string{"sh", "-c", probeNarrate}, "narrate", plugin.RunOpts{})
	if err != nil {
		return proc, err
	}
	proc["tools_disabled"] = true
	return proc, nil
}

func (h *envProbe) Normalize(ctx *plugin.Ctx, stage string, exclude []string) (M, error) {
	text := "synthetic task completed"
	if stage == "narrate" {
		text = "# Journey: synthetic environment lifecycle\n\n" +
			"This is a synthetic narration used only by an offline lifecycle test. " +
			"The worker used a tool prepared in its isolated home, wrote its result " +
			"in the workspace, and reused the same environment for narration after " +
			"verification and teardown. No model provider was called."
	}
	return plugin.EmptyTelemetry(M{"session_id": "synthetic-environment-session", "final_text": text,
		"stop_reason": "completed"}), nil
}

// copyAuth copies only the synthetic credential created by a lifecycle test.
type copyAuth struct {
	*plugin.AuthBase
	cleanups   int
	cleanupErr error
	cleanupDir []string
}

func (a *copyAuth) Prepare(ctx *plugin.Ctx) error {
	data, err := os.ReadFile(util.S(a.Conf["source"]))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(ctx.ConfigDir, "auth.json"), data, 0o600)
}

func (a *copyAuth) Cleanup(ctx *plugin.Ctx) error {
	a.cleanups++
	a.cleanupDir = append(a.cleanupDir, ctx.ConfigDir)
	if a.cleanupErr != nil {
		return a.cleanupErr
	}
	err := os.Remove(filepath.Join(ctx.ConfigDir, "auth.json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func init() {
	plugin.RegisterHarness("environment-probe", "test", func() plugin.Harness {
		return &envProbe{BaseHarness: plugin.BaseHarness{BinaryName: "sh", VersionArgv: []string{"sh", "-c", "echo probe-sh 1.0"},
			DefaultAuth: "env", CleanSupported: true, CanResume: true}}
	})
	plugin.RegisterAuth("environment-copy", "test", func(conf M) plugin.Auth {
		return &copyAuth{AuthBase: plugin.NewAuthBase(conf)}
	})
}

// spyRunner records (and can replace) the backend calls a test asserts on.
type spyRunner struct {
	*environments.EnvironmentRunner
	t                          *testing.T
	plans, prepares, releases  int
	aborts                     int
	releaseResult              M
	releaseErr                 error
	forbidPlan, forbidPrepare  bool
	forbidRelease, forbidAbort bool
	releaseStates              []M
}

func (s *spyRunner) Plan(ctx *plugin.Ctx) (M, error) {
	s.plans++
	if s.forbidPlan {
		s.t.Error("runner.Plan must not be called")
	}
	return s.EnvironmentRunner.Plan(ctx)
}

func (s *spyRunner) Prepare(ctx *plugin.Ctx) (M, error) {
	s.prepares++
	if s.forbidPrepare {
		s.t.Error("runner.Prepare must not be called")
	}
	return s.EnvironmentRunner.Prepare(ctx)
}

func (s *spyRunner) Release(ctx *plugin.Ctx) (M, error) {
	s.releases++
	s.releaseStates = append(s.releaseStates, util.DeepCopy(ctx.State).(M))
	if s.forbidRelease {
		s.t.Error("runner.Release must not be called (backend unavailable)")
		return M{"status": "failed", "confirmed": false}, nil
	}
	if s.releaseErr != nil {
		return nil, s.releaseErr
	}
	if s.releaseResult != nil {
		return util.Copy(s.releaseResult), nil
	}
	return s.EnvironmentRunner.Release(ctx)
}

func (s *spyRunner) Abort(ctx *plugin.Ctx) M {
	s.aborts++
	if s.forbidAbort {
		s.t.Error("adapter owns cancellation: runner.Abort must not be called")
	}
	return s.EnvironmentRunner.Abort(ctx)
}

func spy(t *testing.T, r *Run) *spyRunner {
	t.Helper()
	er, ok := r.Runner.(*environments.EnvironmentRunner)
	if !ok {
		t.Fatalf("runner is %T, want *environments.EnvironmentRunner", r.Runner)
	}
	s := &spyRunner{EnvironmentRunner: er, t: t}
	r.Runner = s
	return s
}

func probeOf(t *testing.T, r *Run) *envProbe {
	t.Helper()
	h, ok := r.Harness.(*envProbe)
	if !ok {
		t.Fatalf("harness is %T", r.Harness)
	}
	return h
}

func copyAuthOf(t *testing.T, r *Run) *copyAuth {
	t.Helper()
	a, ok := r.Auth.(*copyAuth)
	if !ok {
		t.Fatalf("auth is %T", r.Auth)
	}
	return a
}

func envTrial(t *testing.T, root, setup, extra string) M {
	t.Helper()
	if setup == "" {
		setup = `printf ready > "$HOME/installed-tool"`
	}
	_ = os.WriteFile(filepath.Join(root, "task.md"), []byte("Create the synthetic result file."), 0o644)
	_ = os.MkdirAll(filepath.Join(root, "fixture"), 0o755)
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	text := fmt.Sprintf(`
[trial]
id = "environment-lifecycle"
product = "synthetic"
workspace_root = %s
output_dir = "results"
timeout_seconds = 10
environment = "local"

[task]
prompt_file = "task.md"
fixture_dir = "fixture"

[environments.local]
backend = "local"

[auth.test]
type = "env"
isolates_config = true

[reporter]
harness = "codex"

[[setup]]
run = %s

[[verify]]
type = "file_exists"
name = "result"
path = "result.json"
contains = "ready"

[[teardown]]
run = 'touch "$HOME/teardown-ran"'

[[cells]]
id = "local"
harness = "environment-probe"
auth = "test"
%s
`, q(filepath.Join(root, "workspaces")), q(setup), extra)
	path := filepath.Join(root, "trial.toml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return loadSpec(t, path)
}

const syntheticLogin = `{"token": "synthetic-offline-credential"}`

func copiedAuthRun(t *testing.T, root string) (M, *Run, string) {
	t.Helper()
	sp := envTrial(t, root, "", "")
	source := filepath.Join(root, "synthetic-login.json")
	_ = os.WriteFile(source, []byte(syntheticLogin), 0o600)
	util.MapOr(sp["auth"])["test"] = M{"type": "environment-copy", "source": source}
	return sp, newRun(t, sp, nil), source
}

func freshRun(t *testing.T, sp M) *Run {
	t.Helper()
	return newRun(t, sp, nil)
}

// interruptWithEvidence writes the partial evidence an interrupted attempt
// leaves, then interrupts (cancels the run context).
func interruptWithEvidence(t *testing.T, r *Run, source string, cancel context.CancelFunc) func() error {
	return func() error {
		paths := r.paths()
		if got := readFile(t, filepath.Join(util.S(paths["config_dir"]), "auth.json")); got != syntheticLogin {
			t.Errorf("copied credential %q", got)
		}
		_ = os.WriteFile(filepath.Join(util.S(paths["workspace"]), "partial-evidence.txt"), []byte("keep the interrupted attempt"), 0o644)
		_ = os.WriteFile(filepath.Join(util.S(paths["config_dir"]), "transcript.jsonl"), []byte("{\"event\": \"synthetic partial evidence\"}\n"), 0o644)
		cached := filepath.Join(util.S(paths["home_dir"]), ".synthetic")
		_ = os.MkdirAll(cached, 0o755)
		_ = os.WriteFile(filepath.Join(cached, "credentials.json"), []byte("synthetic cached credential"), 0o600)
		cancel()
		return util.ErrInterrupted
	}
}

func assertPurgedEvidence(t *testing.T, paths M, source string) {
	t.Helper()
	if readFile(t, source) != syntheticLogin {
		t.Error("the original login changed")
	}
	if exists(filepath.Join(util.S(paths["config_dir"]), "auth.json")) {
		t.Error("copied auth.json kept")
	}
	if exists(filepath.Join(util.S(paths["home_dir"]), ".synthetic", "credentials.json")) {
		t.Error("cached credentials.json kept")
	}
	if got := readFileQuiet(filepath.Join(util.S(paths["workspace"]), "partial-evidence.txt")); got != "keep the interrupted attempt" {
		t.Errorf("partial evidence %q", got)
	}
	if got := readFileQuiet(filepath.Join(util.S(paths["config_dir"]), "transcript.jsonl")); got != "{\"event\": \"synthetic partial evidence\"}\n" {
		t.Errorf("transcript %q", got)
	}
}

func allGone(paths M) bool {
	for _, p := range paths {
		if exists(util.S(p)) {
			return false
		}
	}
	return true
}

func allDirs(paths M) bool {
	for _, p := range paths {
		if !isDir(util.S(p)) {
			return false
		}
	}
	return true
}

// interruptAt arranges for stage to be interrupted with evidence written.
func interruptAt(t *testing.T, r *Run, stage, source string, cancel context.CancelFunc) {
	hit := interruptWithEvidence(t, r, source, cancel)
	switch stage {
	case "prepare":
		commandHook = func(*Run, M) error { return hit() }
		t.Cleanup(func() { commandHook = nil })
	case "execute":
		probeOf(t, r).execute = func(*envProbe, *plugin.Ctx) (M, error) { return nil, hit() }
	case "verify":
		r.Override["verify"] = func(*Run) error { return hit() }
	case "narrate":
		probeOf(t, r).narrate = func(*envProbe, *plugin.Ctx) (M, error) { return nil, hit() }
	case "measure":
		r.Override["measure"] = func(*Run) error { return hit() }
	}
}

func TestHandledInterruptionArchivesEvidenceAndPurgesAuthWithoutReplay(t *testing.T) {
	for _, stage := range []string{"prepare", "execute", "verify", "narrate"} {
		t.Run(stage, func(t *testing.T) {
			root := resolvePath(t.TempDir())
			sp, r, source := copiedAuthRun(t, root)
			stubExtract(r)
			s := spy(t, r)
			s.forbidAbort = true
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			interruptAt(t, r, stage, source, cancel)
			_, err := r.Go(ctx, nil, false)
			commandHook = nil
			if !util.Interrupted(err) {
				t.Fatalf("want interruption, got %v", err)
			}
			if r.Status(stage) != "interrupted" {
				t.Fatalf("%s = %s: %v", stage, r.Status(stage), statuses(r.State))
			}
			if util.Get(readMapT(t, r.StatePath), "stages", stage, "status") != "interrupted" {
				t.Fatal("state.json not saved")
			}
			if !r.done("archive") || !cleanupConfirmed(r.State) {
				t.Fatalf("not archived/released: %v %v", statuses(r.State), r.State["environment_cleanup"])
			}
			assertPurgedEvidence(t, r.archived(), source)
			paths := util.DeepCopy(r.paths()).(M)
			owned := util.DeepCopy(r.State["owned_paths"])
			environment := util.DeepCopy(r.State["environment"])
			if !allGone(paths) {
				t.Fatal("original paths remain")
			}
			if stage == "prepare" {
				if r.Status("execute") != "" || r.Status("teardown") != "" {
					t.Fatalf("%v", statuses(r.State))
				}
			} else {
				if !r.done("teardown") || util.Get(r.State, "teardown", "status") != "ok" {
					t.Fatalf("teardown %v %v", statuses(r.State), r.State["teardown"])
				}
				if !exists(filepath.Join(r.RunDir, "agent-home", "teardown-ran")) {
					t.Fatal("teardown did not run in the environment")
				}
			}
			if stage == "execute" {
				ex := r.execState()
				if ex["stop_reason"] != "interrupted" || ex["interrupted"] != true || ex["stopped_at"] == nil {
					t.Fatalf("execute %v", ex)
				}
			}
			resumed := freshRun(t, sp)
			rs := spy(t, resumed)
			rs.forbidPlan, rs.forbidPrepare = true, true
			probeOf(t, resumed).execute = func(*envProbe, *plugin.Ctx) (M, error) {
				t.Error("execute must not be replayed")
				return M{}, nil
			}
			for _, st := range [][]string{{"execute"}, {"archive"}} {
				if _, err := resumed.Go(context.Background(), st, false); err != nil {
					t.Fatal(err)
				}
			}
			if !PyEqual(resumed.paths(), paths) || !PyEqual(resumed.State["owned_paths"], owned) ||
				!PyEqual(resumed.State["environment"], environment) {
				t.Fatal("resume changed the recorded attempt")
			}
			assertPurgedEvidence(t, resumed.archived(), source)
		})
	}
}

func TestHandledInterruptionKeepsRequestedWorkspaceButPurgesCredentials(t *testing.T) {
	for _, stage := range []string{"prepare", "execute"} {
		t.Run(stage, func(t *testing.T) {
			root := resolvePath(t.TempDir())
			sp, r, source := copiedAuthRun(t, root)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			interruptAt(t, r, stage, source, cancel)
			_, err := r.Go(ctx, nil, true)
			commandHook = nil
			if !util.Interrupted(err) {
				t.Fatalf("want interruption, got %v", err)
			}
			if r.Status(stage) != "interrupted" || r.Status("archive") != "" || r.State["archived"] != nil {
				t.Fatalf("%v archived=%v", statuses(r.State), r.State["archived"])
			}
			if !cleanupConfirmed(r.State) {
				t.Fatalf("release %v", r.State["environment_cleanup"])
			}
			if stage == "execute" && !r.done("teardown") {
				t.Fatalf("%v", statuses(r.State))
			}
			if util.Get(r.State, "interruption_cleanup", "steps", "credentials", "status") != "done" {
				t.Fatalf("steps %v", util.Get(r.State, "interruption_cleanup", "steps"))
			}
			if !allDirs(r.paths()) {
				t.Fatal("kept workspace removed")
			}
			assertPurgedEvidence(t, r.paths(), source)
			resumed := freshRun(t, sp)
			rs := spy(t, resumed)
			rs.forbidPlan, rs.forbidPrepare = true, true
			probeOf(t, resumed).execute = func(*envProbe, *plugin.Ctx) (M, error) {
				t.Error("execute must not be replayed")
				return M{}, nil
			}
			if _, err := resumed.Go(context.Background(), []string{stage}, true); err != nil {
				t.Fatal(err)
			}
			if resumed.State["archived"] != nil || !allDirs(resumed.paths()) {
				t.Fatal("resume with --keep-workspace archived")
			}
			assertPurgedEvidence(t, resumed.paths(), source)
		})
	}
}

func TestHandledInterruptionRetainsFailedReleasePathsAndPurgesAuthBeforeRetry(t *testing.T) {
	root := resolvePath(t.TempDir())
	sp, r, source := copiedAuthRun(t, root)
	s := spy(t, r)
	s.releaseResult = M{"status": "failed", "confirmed": false, "error": "synthetic backend outage"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interruptAt(t, r, "execute", source, cancel)
	if _, err := r.Go(ctx, nil, false); !util.Interrupted(err) {
		t.Fatalf("want interruption, got %v", err)
	}
	if s.releases != 1 || r.Status("release") != "error" || r.Status("archive") != "error" || cleanupConfirmed(r.State) ||
		r.State["archived"] != nil {
		t.Fatalf("releases=%d %v %v", s.releases, statuses(r.State), r.State["environment_cleanup"])
	}
	paths := util.DeepCopy(r.paths()).(M)
	owned, environment := util.DeepCopy(r.State["owned_paths"]), util.DeepCopy(r.State["environment"])
	if !allDirs(paths) {
		t.Fatal("paths not retained")
	}
	assertPurgedEvidence(t, paths, source)
	resumed := freshRun(t, sp)
	rs := spy(t, resumed)
	rs.forbidPlan, rs.forbidPrepare = true, true
	probeOf(t, resumed).execute = func(*envProbe, *plugin.Ctx) (M, error) { t.Error("replayed"); return M{}, nil }
	if _, err := resumed.Go(context.Background(), []string{"release", "archive"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Go(context.Background(), []string{"execute"}, false); err != nil {
		t.Fatal(err)
	}
	if rs.releases != 1 || !PyEqual(util.Get(rs.releaseStates[0], "paths"), paths) {
		t.Fatalf("release calls %d", rs.releases)
	}
	if !resumed.done("archive") || !cleanupConfirmed(resumed.State) {
		t.Fatalf("%v", statuses(resumed.State))
	}
	if !PyEqual(resumed.paths(), paths) || !PyEqual(resumed.State["owned_paths"], owned) || !PyEqual(resumed.State["environment"], environment) {
		t.Fatal("attempt record changed")
	}
	if !allGone(paths) {
		t.Fatal("original paths remain")
	}
	assertPurgedEvidence(t, resumed.archived(), source)
}

func TestCleanupInterruptionPreservesOriginalInterruptionAndPurgesKnownCredentials(t *testing.T) {
	for _, failed := range []string{"teardown", "release", "auth"} {
		t.Run(failed, func(t *testing.T) {
			root := resolvePath(t.TempDir())
			_, r, source := copiedAuthRun(t, root)
			s := spy(t, r)
			switch failed {
			case "teardown":
				r.Override["teardown"] = func(*Run) error { return util.ErrInterrupted }
			case "release":
				s.releaseErr = util.ErrInterrupted
			case "auth":
				copyAuthOf(t, r).cleanupErr = util.ErrInterrupted
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			interruptAt(t, r, "execute", source, cancel)
			if _, err := r.Go(ctx, nil, false); !util.Interrupted(err) {
				t.Fatalf("want interruption, got %v", err)
			}
			if r.Status("execute") != "interrupted" {
				t.Fatalf("%v", statuses(r.State))
			}
			switch failed {
			case "teardown":
				if r.Status("teardown") != "interrupted" || r.Status("archive") != "error" ||
					util.Get(r.State, "environment_cleanup", "status") != "deferred_teardown" || r.State["archived"] != nil {
					t.Fatalf("%v %v", statuses(r.State), r.State["environment_cleanup"])
				}
			case "release":
				if r.Status("release") != "interrupted" || r.Status("archive") != "error" || r.State["archived"] != nil ||
					cleanupConfirmed(r.State) {
					t.Fatalf("%v", statuses(r.State))
				}
			case "auth":
				if r.Status("archive") != "interrupted" ||
					util.Get(r.State, "interruption_cleanup", "steps", "credentials", "status") != "interrupted" {
					t.Fatalf("%v %v", statuses(r.State), util.Get(r.State, "interruption_cleanup", "steps"))
				}
			}
			where := r.archived()
			if len(where) == 0 {
				where = r.paths()
			}
			assertPurgedEvidence(t, where, source)
		})
	}
}

func TestFailedEnvironmentTeardownRetainsToolsAndAllowsRetryWithoutReplay(t *testing.T) {
	failures := map[string]string{
		"exit":    `test -e allow-cleanup || exit 4`,
		"timeout": `if [ ! -e allow-cleanup ]; then sleep 60; fi`,
		"auth":    `if [ ! -e allow-cleanup ]; then echo "Unable to locate credentials" >&2; fi`,
	}
	for kind, command := range failures {
		t.Run(kind, func(t *testing.T) {
			root := resolvePath(t.TempDir())
			sp, r, source := copiedAuthRun(t, root)
			td := util.Maps(sp["teardown"])[0]
			td["run"] = command + `; touch "$HOME/teardown-ran"`
			td["timeout"] = int64(1)
			var logs []string
			r.Log = func(m string) { logs = append(logs, m) }
			s := spy(t, r)
			s.forbidRelease = true
			if _, err := r.Go(context.Background(), []string{"prepare", "execute", "teardown", "release", "archive"}, false); err != nil {
				t.Fatal(err)
			}
			if util.Get(r.State, "teardown", "status") == "ok" || util.Get(r.State, "environment_cleanup", "status") != "deferred_teardown" ||
				cleanupConfirmed(r.State) || r.Status("archive") != "error" || r.State["archived"] != nil {
				t.Fatalf("%v %v %v", statuses(r.State), r.State["teardown"], r.State["environment_cleanup"])
			}
			found := false
			for _, m := range logs {
				found = found || strings.Contains(m, "teardown,release,render,archive")
			}
			if !found {
				t.Fatalf("logs %q", logs)
			}
			paths := util.DeepCopy(r.paths()).(M)
			handle := util.DeepCopy(r.State["environment"])
			if !allDirs(paths) || exists(filepath.Join(util.S(paths["config_dir"]), "auth.json")) || !exists(source) {
				t.Fatal("retained paths or purge")
			}
			if out := r.Runner.(plugin.ManagedRunner).Shell(`cat "$HOME/installed-tool"`, r.Ctx(), 30, nil); out["stdout"] != "ready" {
				t.Fatalf("environment tools gone: %v", out)
			}
			retried := freshRun(t, sp)
			rs := spy(t, retried)
			rs.forbidRelease = true
			if _, err := retried.Go(context.Background(), []string{"teardown", "release", "archive"}, false); err != nil {
				t.Fatal(err)
			}
			if util.Get(retried.State, "environment_cleanup", "status") != "deferred_teardown" {
				t.Fatalf("%v", retried.State["environment_cleanup"])
			}
			_ = os.WriteFile(filepath.Join(util.S(paths["workspace"]), "allow-cleanup"), nil, 0o644)
			rs.forbidRelease, rs.forbidPlan, rs.forbidPrepare = false, true, true
			probeOf(t, retried).execute = func(*envProbe, *plugin.Ctx) (M, error) { t.Error("replayed"); return M{}, nil }
			if _, err := retried.Go(context.Background(), []string{"teardown", "release", "archive"}, false); err != nil {
				t.Fatal(err)
			}
			if util.Get(retried.State, "teardown", "status") != "ok" || !cleanupConfirmed(retried.State) || !retried.done("archive") ||
				!PyEqual(retried.State["environment"], handle) {
				t.Fatalf("%v %v", statuses(retried.State), retried.State["environment_cleanup"])
			}
			if readFile(t, filepath.Join(retried.RunDir, "workspace", "executions.txt")) != "one execution" ||
				!exists(filepath.Join(retried.RunDir, "agent-home", "teardown-ran")) ||
				exists(filepath.Join(retried.RunDir, "harness-config", "auth.json")) || !exists(source) {
				t.Fatal("archived evidence")
			}
		})
	}
}

func TestExplicitReleaseCanDiscardEnvironmentAfterFailedTaskCleanup(t *testing.T) {
	root := resolvePath(t.TempDir())
	sp, r, source := copiedAuthRun(t, root)
	util.Maps(sp["teardown"])[0]["run"] = "exit 4"
	if _, err := r.Go(context.Background(), []string{"prepare", "execute", "teardown", "release", "archive"}, false); err != nil {
		t.Fatal(err)
	}
	if util.Get(r.State, "environment_cleanup", "status") != "deferred_teardown" {
		t.Fatalf("%v", r.State["environment_cleanup"])
	}
	retried := freshRun(t, sp)
	retried.Override["teardown"] = func(*Run) error { t.Error("teardown must not run"); return nil }
	if _, err := retried.Go(context.Background(), []string{"release", "archive"}, false); err != nil {
		t.Fatal(err)
	}
	if !cleanupConfirmed(retried.State) || util.Get(retried.State, "teardown", "status") != "failed" || !retried.done("archive") {
		t.Fatalf("%v %v", statuses(retried.State), retried.State["environment_cleanup"])
	}
	if exists(filepath.Join(retried.RunDir, "harness-config", "auth.json")) || !exists(source) {
		t.Fatal("purge")
	}
}

func TestHostOnlyCleanupFailureDoesNotRetainEnvironment(t *testing.T) {
	root := resolvePath(t.TempDir())
	sp, r, _ := copiedAuthRun(t, root)
	td := util.Copy(util.Maps(sp["teardown"])[0])
	td["run"], td["location"] = "exit 4", "host"
	sp["teardown"] = []any{td}
	if _, err := r.Go(context.Background(), []string{"prepare", "execute", "teardown", "release", "archive"}, false); err != nil {
		t.Fatal(err)
	}
	if util.Get(r.State, "teardown", "status") != "failed" || !cleanupConfirmed(r.State) || !r.done("archive") {
		t.Fatalf("%v %v", statuses(r.State), r.State["environment_cleanup"])
	}
}

func TestMixedTeardownRetainsEnvironmentUntilHostStepAlsoSucceeds(t *testing.T) {
	for _, hostFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(hostFirst), func(t *testing.T) {
			root := resolvePath(t.TempDir())
			sp, r, source := copiedAuthRun(t, root)
			env := util.Maps(sp["teardown"])[0]
			host := util.Copy(env)
			host["run"], host["location"] = "test -e allow-host-cleanup || exit 4", "host"
			if hostFirst {
				sp["teardown"] = []any{host, env}
			} else {
				sp["teardown"] = []any{env, host}
			}
			if _, err := r.Go(context.Background(), []string{"prepare", "execute", "teardown", "release", "archive"}, false); err != nil {
				t.Fatal(err)
			}
			results := readListT(t, filepath.Join(r.RunDir, "teardown.json"))
			hi, ei := 1, 0
			if hostFirst {
				hi, ei = 0, 1
			}
			if util.Int(results[ei]["exit_code"], -1) != 0 || util.Int(results[hi]["exit_code"], -1) != 4 {
				t.Fatalf("teardown.json %v", results)
			}
			if util.Get(r.State, "environment_cleanup", "status") != "deferred_teardown" || r.State["archived"] != nil {
				t.Fatalf("%v", r.State["environment_cleanup"])
			}
			if out := r.Runner.(plugin.ManagedRunner).Shell(`cat "$HOME/installed-tool"`, r.Ctx(), 30, nil); out["stdout"] != "ready" {
				t.Fatalf("environment gone: %v", out)
			}
			handle := util.DeepCopy(r.State["environment"])
			_ = os.WriteFile(filepath.Join(util.S(util.Get(r.State, "paths", "workspace")), "allow-host-cleanup"), nil, 0o644)
			retried := freshRun(t, sp)
			rs := spy(t, retried)
			rs.forbidPlan = true
			probeOf(t, retried).execute = func(*envProbe, *plugin.Ctx) (M, error) { t.Error("replayed"); return M{}, nil }
			if _, err := retried.Go(context.Background(), []string{"teardown", "release", "archive"}, false); err != nil {
				t.Fatal(err)
			}
			if util.Get(retried.State, "teardown", "status") != "ok" || !cleanupConfirmed(retried.State) || !retried.done("archive") ||
				!PyEqual(retried.State["environment"], handle) {
				t.Fatalf("%v", statuses(retried.State))
			}
			if readFile(t, filepath.Join(retried.RunDir, "workspace", "executions.txt")) != "one execution" || !exists(source) {
				t.Fatal("evidence")
			}
		})
	}
}

func TestHandledInterruptionArchivesAfterAlreadyConfirmedReleaseWithoutBackend(t *testing.T) {
	root := resolvePath(t.TempDir())
	_, r, source := copiedAuthRun(t, root)
	if _, err := r.Go(context.Background(), []string{"prepare", "execute", "verify", "teardown", "normalize", "narrate", "release"}, false); err != nil {
		t.Fatal(err)
	}
	confirmed := util.DeepCopy(r.State["environment_cleanup"])
	if !cleanupConfirmed(r.State) {
		t.Fatalf("%v %v", statuses(r.State), confirmed)
	}
	s := spy(t, r)
	s.forbidRelease = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interruptAt(t, r, "measure", source, cancel)
	if _, err := r.Go(ctx, []string{"measure"}, false); !util.Interrupted(err) {
		t.Fatalf("want interruption, got %v", err)
	}
	if s.releases != 0 || r.Status("measure") != "interrupted" || !r.done("archive") || !PyEqual(r.State["environment_cleanup"], confirmed) {
		t.Fatalf("%v", statuses(r.State))
	}
	assertPurgedEvidence(t, r.archived(), source)
}

// interruptedPrepare leaves the durable state of a coordinator that died during preparation.
func interruptedPrepare(t *testing.T, root string) (M, *Run, string) {
	t.Helper()
	sp, r, source := copiedAuthRun(t, root)
	if err := r.mark("prepare", "running", nil); err != nil {
		t.Fatal(err)
	}
	commandHook = func(*Run, M) error { return util.ErrInterrupted }
	err := r.stagePrepare()
	commandHook = nil
	if !util.Interrupted(err) || r.Status("prepare") != "running" {
		t.Fatalf("stagePrepare: %v %v", err, statuses(r.State))
	}
	if readFile(t, filepath.Join(util.S(util.Get(r.State, "paths", "config_dir")), "auth.json")) != syntheticLogin {
		t.Fatal("credential not copied")
	}
	_ = os.WriteFile(filepath.Join(util.S(util.Get(r.State, "paths", "workspace")), "partial-evidence.txt"), []byte("keep preparation evidence"), 0o644)
	_ = os.WriteFile(filepath.Join(util.S(util.Get(r.State, "paths", "home_dir")), "credentials.json"), []byte("synthetic cached credential"), 0o600)
	return sp, r, source
}

func TestInterruptedPrepareCleansOriginalCredentialsWithoutReplaying(t *testing.T) {
	for _, stages := range [][]string{nil, {"prepare"}} {
		t.Run(fmt.Sprint(stages), func(t *testing.T) {
			root := resolvePath(t.TempDir())
			sp, interrupted, source := interruptedPrepare(t, root)
			paths := util.DeepCopy(interrupted.paths()).(M)
			owned, handle := util.DeepCopy(interrupted.State["owned_paths"]), util.DeepCopy(interrupted.State["environment"])
			var logs []string
			resumed := newRun(t, sp, func(m string) { logs = append(logs, m) })
			rs := spy(t, resumed)
			rs.forbidPlan, rs.forbidPrepare = true, true
			probeOf(t, resumed).execute = func(*envProbe, *plugin.Ctx) (M, error) { t.Error("must not execute"); return M{}, nil }
			if _, err := resumed.Go(context.Background(), stages, false); err != nil {
				t.Fatal(err)
			}
			if resumed.Status("prepare") != "interrupted" || resumed.Status("execute") != "" || !resumed.done("archive") ||
				!cleanupConfirmed(resumed.State) {
				t.Fatalf("%v %v", statuses(resumed.State), resumed.State["environment_cleanup"])
			}
			if !util.Contains(logs, "[local-r1] prepare was interrupted; cleaning up the recorded attempt") {
				t.Fatalf("logs %q", logs)
			}
			if !PyEqual(resumed.paths(), paths) || !PyEqual(resumed.State["owned_paths"], owned) || !PyEqual(resumed.State["environment"], handle) {
				t.Fatal("attempt record changed")
			}
			auth := copyAuthOf(t, resumed)
			if auth.cleanups != 1 || auth.cleanupDir[0] != util.S(paths["config_dir"]) {
				t.Fatalf("auth cleanup %d %v", auth.cleanups, auth.cleanupDir)
			}
			if !allGone(paths) || exists(filepath.Join(resumed.RunDir, "harness-config", "auth.json")) ||
				exists(filepath.Join(resumed.RunDir, "agent-home", "credentials.json")) {
				t.Fatal("purge/archive")
			}
			if readFile(t, filepath.Join(resumed.RunDir, "workspace", "partial-evidence.txt")) != "keep preparation evidence" || !exists(source) {
				t.Fatal("evidence")
			}
			if _, err := resumed.Go(context.Background(), nil, false); err != nil {
				t.Fatal(err)
			}
			if auth.cleanups != 1 || rs.plans != 0 || rs.prepares != 0 {
				t.Fatal("cleanup repeated or attempt replayed")
			}
		})
	}
}

func TestInterruptedPrepareRetriesUnconfirmedCleanupWithOriginalPaths(t *testing.T) {
	root := resolvePath(t.TempDir())
	sp, interrupted, source := interruptedPrepare(t, root)
	paths := util.DeepCopy(interrupted.paths()).(M)
	resumed := freshRun(t, sp)
	rs := spy(t, resumed)
	rs.releaseResult = M{"status": "failed", "confirmed": false}
	if _, err := resumed.Go(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	if resumed.Status("prepare") != "interrupted" || resumed.Status("archive") != "error" || !PyEqual(resumed.paths(), paths) ||
		resumed.State["archived"] != nil || !allDirs(paths) {
		t.Fatalf("%v", statuses(resumed.State))
	}
	if exists(filepath.Join(util.S(paths["config_dir"]), "auth.json")) || exists(filepath.Join(util.S(paths["home_dir"]), "credentials.json")) {
		t.Fatal("credentials kept while release is unconfirmed")
	}
	if readFile(t, source) != syntheticLogin || copyAuthOf(t, resumed).cleanups != 0 {
		t.Fatal("auth hook ran before a confirmed release")
	}
	recovered := freshRun(t, sp)
	probeOf(t, recovered).execute = func(*envProbe, *plugin.Ctx) (M, error) { t.Error("must not execute"); return M{}, nil }
	if _, err := recovered.Go(context.Background(), []string{"prepare"}, false); err != nil {
		t.Fatal(err)
	}
	if !PyEqual(recovered.paths(), paths) || !recovered.done("archive") || !cleanupConfirmed(recovered.State) || !allGone(paths) ||
		exists(filepath.Join(recovered.RunDir, "harness-config", "auth.json")) {
		t.Fatalf("%v", statuses(recovered.State))
	}
}

func TestArchiveFailedReleasePurgesKnownCredentialsWithoutFollowingSymlinks(t *testing.T) {
	root := resolvePath(t.TempDir())
	_, r, source := copiedAuthRun(t, root)
	if _, err := r.Go(context.Background(), []string{"prepare", "teardown"}, false); err != nil {
		t.Fatal(err)
	}
	paths := util.DeepCopy(r.paths()).(M)
	config := util.S(paths["config_dir"])
	outside := filepath.Join(root, "outside")
	_ = os.MkdirAll(outside, 0o755)
	original := filepath.Join(outside, "auth.json")
	_ = os.WriteFile(original, []byte("synthetic original outside owned roots"), 0o600)
	nested := filepath.Join(config, "nested")
	_ = os.MkdirAll(nested, 0o755)
	_ = os.WriteFile(filepath.Join(nested, "tokens.json"), []byte("synthetic staged token"), 0o600)
	_ = os.WriteFile(filepath.Join(nested, "transcript.jsonl"), []byte("retain evidence"), 0o644)
	_ = os.Symlink(outside, filepath.Join(config, "redirected-directory"))
	_ = os.Symlink(original, filepath.Join(config, "credentials.json"))
	s := spy(t, r)
	s.releaseResult = M{"status": "failed", "confirmed": false}
	if _, err := r.Go(context.Background(), []string{"archive"}, false); err != nil {
		t.Fatal(err)
	}
	if s.releases != 1 || copyAuthOf(t, r).cleanups != 0 || r.Status("archive") != "error" || cleanupConfirmed(r.State) ||
		!PyEqual(r.paths(), paths) || r.State["archived"] != nil {
		t.Fatalf("%v releases=%d", statuses(r.State), s.releases)
	}
	if exists(filepath.Join(config, "auth.json")) || exists(filepath.Join(nested, "tokens.json")) {
		t.Fatal("known credentials kept")
	}
	if _, err := os.Lstat(filepath.Join(config, "credentials.json")); err == nil {
		t.Fatal("credential symlink kept")
	}
	if st, err := os.Lstat(filepath.Join(config, "redirected-directory")); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("directory symlink touched")
	}
	if readFile(t, filepath.Join(nested, "transcript.jsonl")) != "retain evidence" ||
		readFile(t, original) != "synthetic original outside owned roots" || readFile(t, source) != syntheticLogin {
		t.Fatal("evidence or outside files changed")
	}
	if got := util.S(util.Get(r.State, "stages", "archive", "error")); got != "RuntimeError: environment cleanup was not confirmed: failed" {
		t.Fatalf("archive error %q", got)
	}
}

func TestArchiveFailedReleasePurgeStaysAnchoredDuringDirectoryReplacement(t *testing.T) {
	for _, replacement := range []string{"root", "ancestor", "descendant"} {
		t.Run(replacement, func(t *testing.T) {
			root := resolvePath(t.TempDir())
			_, r, source := copiedAuthRun(t, root)
			if _, err := r.Go(context.Background(), []string{"prepare", "teardown"}, false); err != nil {
				t.Fatal(err)
			}
			paths := util.DeepCopy(r.paths()).(M)
			config := util.S(paths["config_dir"])
			outside := filepath.Join(root, "outside")
			_ = os.MkdirAll(outside, 0o755)
			var watched, relative string
			switch replacement {
			case "root":
				watched = config
			case "ancestor":
				watched, relative = filepath.Dir(config), filepath.Base(config)
			default:
				watched = filepath.Join(config, "nested")
				_ = os.MkdirAll(watched, 0o755)
				_ = os.WriteFile(filepath.Join(watched, "auth.json"), []byte("synthetic staged credential"), 0o600)
			}
			_ = os.WriteFile(filepath.Join(watched, relative, "transcript.jsonl"), []byte("retain evidence"), 0o644)
			target := filepath.Join(outside, relative)
			_ = os.MkdirAll(target, 0o755)
			original := filepath.Join(target, "auth.json")
			_ = os.WriteFile(original, []byte("synthetic original outside owned roots"), 0o600)
			_ = os.WriteFile(filepath.Join(target, "transcript.jsonl"), []byte("outside evidence must remain untouched"), 0o644)
			retained := watched + "-retained"
			identity, err := os.Stat(watched)
			if err != nil {
				t.Fatal(err)
			}
			swapped := false
			afterOpenDir = func(info os.FileInfo) {
				if !swapped && os.SameFile(info, identity) {
					if err := os.Rename(watched, retained); err != nil {
						t.Error(err)
					}
					if err := os.Symlink(outside, watched); err != nil {
						t.Error(err)
					}
					swapped = true
				}
			}
			defer func() { afterOpenDir = nil }()
			s := spy(t, r)
			s.releaseResult = M{"status": "failed", "confirmed": false}
			if _, err := r.Go(context.Background(), []string{"archive"}, false); err != nil {
				t.Fatal(err)
			}
			afterOpenDir = nil
			if !swapped {
				t.Fatal("the race must occur after opening the intended directory")
			}
			if copyAuthOf(t, r).cleanups != 0 || r.Status("archive") != "error" || cleanupConfirmed(r.State) ||
				!PyEqual(r.paths(), paths) || r.State["archived"] != nil {
				t.Fatalf("%v", statuses(r.State))
			}
			if exists(filepath.Join(retained, relative, "auth.json")) {
				t.Fatal("pinned directory not purged")
			}
			if readFile(t, filepath.Join(retained, relative, "transcript.jsonl")) != "retain evidence" ||
				readFile(t, original) != "synthetic original outside owned roots" ||
				readFile(t, filepath.Join(target, "transcript.jsonl")) != "outside evidence must remain untouched" ||
				readFile(t, source) != syntheticLogin {
				t.Fatal("outside or evidence files changed")
			}
		})
	}
}

func TestLocalTaskTimeoutStillRunsDefaultEnvironmentTeardown(t *testing.T) {
	root := resolvePath(t.TempDir())
	sp := envTrial(t, root, "", "")
	if util.Maps(sp["teardown"])[0]["location"] != "environment" {
		t.Fatalf("teardown location %v", util.Maps(sp["teardown"])[0]["location"])
	}
	r := freshRun(t, sp)
	probeOf(t, r).execute = func(h *envProbe, ctx *plugin.Ctx) (M, error) {
		return h.Run(ctx, []string{"sh", "-c", "printf once > task-started; sleep 60"}, "execute", plugin.RunOpts{Timeout: 1})
	}
	stages := []string{"prepare", "execute", "teardown", "release", "archive"}
	if _, err := r.Go(context.Background(), stages, false); err != nil {
		t.Fatal(err)
	}
	for _, s := range stages {
		if !r.done(s) {
			t.Fatalf("%s: %v", s, statuses(r.State))
		}
	}
	if util.Get(r.State, "execute", "stop_reason") != "timeout" || util.Get(r.State, "execute", "timed_out") != true ||
		!exists(filepath.Join(r.RunDir, "agent-home", "teardown-ran")) || util.Get(r.State, "teardown", "status") != "ok" ||
		readFile(t, filepath.Join(r.RunDir, "workspace", "task-started")) != "once" {
		t.Fatalf("%v %v", r.State["execute"], r.State["teardown"])
	}
	if readListT(t, filepath.Join(r.RunDir, "teardown.json"))[0]["location"] != "environment" || !cleanupConfirmed(r.State) || !allGone(r.paths()) {
		t.Fatal("teardown location / release / archive")
	}
	n := probeOf(t, r).execs
	if _, err := r.Go(context.Background(), []string{"execute"}, false); err != nil {
		t.Fatal(err)
	}
	if probeOf(t, r).execs != n {
		t.Fatal("executed twice")
	}
}

func TestArchiveKeepsConfirmedReleaseWhenBackendBecomesUnavailable(t *testing.T) {
	root := resolvePath(t.TempDir())
	sp, r, source := copiedAuthRun(t, root)
	if _, err := r.Go(context.Background(), []string{"prepare", "execute", "verify", "teardown", "normalize", "narrate", "release"}, false); err != nil {
		t.Fatal(err)
	}
	if !r.done("release") || !cleanupConfirmed(r.State) || !exists(filepath.Join(util.S(util.Get(r.State, "paths", "config_dir")), "auth.json")) {
		t.Fatalf("%v", statuses(r.State))
	}
	confirmed := util.DeepCopy(r.State["environment_cleanup"])
	resumed := freshRun(t, sp)
	rs := spy(t, resumed)
	rs.forbidRelease = true
	if _, err := resumed.Go(context.Background(), []string{"release", "archive"}, false); err != nil {
		t.Fatal(err)
	}
	if rs.releases != 0 || copyAuthOf(t, resumed).cleanups != 1 || !resumed.done("archive") ||
		!PyEqual(resumed.State["environment_cleanup"], confirmed) {
		t.Fatalf("%v", statuses(resumed.State))
	}
	if exists(filepath.Join(resumed.RunDir, "harness-config", "auth.json")) || !exists(source) || !allGone(resumed.paths()) {
		t.Fatal("purge/archive")
	}
}

func TestLocalLifecycleIsolatedHomeAndEnvironmentSurvivesToNarration(t *testing.T) {
	root := resolvePath(t.TempDir())
	sp := envTrial(t, root, "", "")
	r := freshRun(t, sp)
	callerHome := os.Getenv("HOME")
	t.Setenv("AJX_TEST_AMBIENT_SECRET", "synthetic-do-not-forward")
	StageOverrides["extract"] = noModelExtract
	defer delete(StageOverrides, "extract")
	if _, err := r.Go(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	for _, s := range Stages {
		if !r.done(s) {
			t.Fatalf("%s: %v %v", s, statuses(r.State), util.Get(r.State, "stages", s))
		}
	}
	if readMapT(t, filepath.Join(r.RunDir, "verify.json"))["outcome"] != "succeeded" {
		t.Fatal("verify")
	}
	result := readMapT(t, filepath.Join(r.RunDir, "workspace", "result.json"))
	if result["home"] == callerHome || result["home"] != util.Get(r.State, "paths", "home_dir") {
		t.Fatalf("home %v", result["home"])
	}
	if !exists(filepath.Join(r.RunDir, "agent-home", "teardown-ran")) || os.Getenv("HOME") != callerHome {
		t.Fatal("teardown / caller HOME")
	}
	if _, ok := readMapT(t, filepath.Join(r.RunDir, "run.json"))["environment_cleanup"]; !ok {
		t.Fatal("run.json lacks environment_cleanup")
	}
	if !util.Truthy(r.State["environment_contract_sha256"]) || !allGone(r.paths()) {
		t.Fatal("contract / archive")
	}
	if j := readFile(t, filepath.Join(r.RunDir, "journey.md")); !strings.HasPrefix(j, "<!-- provenance: self-narrated") {
		t.Fatalf("journey %q", util.Clip(j, 100))
	}
	again := freshRun(t, sp)
	probeOf(t, again).execute = func(*envProbe, *plugin.Ctx) (M, error) { t.Error("executed twice"); return M{}, nil }
	if _, err := again.Go(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := again.Go(context.Background(), []string{"release", "render", "archive"}, false); err != nil {
		t.Fatal(err)
	}
	if !again.done("release") || !again.done("archive") || !cleanupConfirmed(again.State) ||
		readFile(t, filepath.Join(r.RunDir, "workspace", "executions.txt")) != "one execution" {
		t.Fatalf("%v", statuses(again.State))
	}
}

func TestChangedProfileCannotResumeAnExistingAttempt(t *testing.T) {
	root := resolvePath(t.TempDir())
	sp := envTrial(t, root, "", "")
	r := freshRun(t, sp)
	defer func() { _ = r.stageArchive() }()
	if _, err := r.Go(context.Background(), []string{"prepare"}, false); err != nil || !r.done("prepare") {
		t.Fatalf("%v %v", err, statuses(r.State))
	}
	util.Maps(sp["cells"])[0]["args"] = []any{"changed-configuration"}
	_, err := NewRun(sp, util.Maps(sp["cells"])[0], 1, func(string) {})
	if err == nil || pyTypeName(err) != "SpecError" || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("want SpecError about a change, got %v", err)
	}
	if err.Error() != "local-r1: environment, agent configuration, or task changed; use a new trial output directory for a new comparison" {
		t.Fatalf("message %q", err.Error())
	}
}

func TestPrepareFailureReleasesOwnedStateWithoutStartingWorker(t *testing.T) {
	root := resolvePath(t.TempDir())
	sp := envTrial(t, root, "exit 9", "")
	r := freshRun(t, sp)
	probeOf(t, r).execute = func(*envProbe, *plugin.Ctx) (M, error) { t.Error("worker must not start"); return M{}, nil }
	if _, err := r.Go(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	if r.Status("prepare") != "error" || r.Status("execute") != "" || !r.done("archive") || !allGone(r.paths()) ||
		r.State["environment_cleanup"] == nil {
		t.Fatalf("%v", statuses(r.State))
	}
	original := util.DeepCopy(r.paths()).(M)
	sentinel := filepath.Join(r.RunDir, "workspace", "failure-evidence.txt")
	_ = os.WriteFile(sentinel, []byte("preserve the failed attempt"), 0o644)
	again := freshRun(t, sp)
	probeOf(t, again).execute = func(*envProbe, *plugin.Ctx) (M, error) { t.Error("failed prepare cannot execute"); return M{}, nil }
	if _, err := again.Go(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	if again.Status("execute") != "" || !PyEqual(again.paths(), original) || readFile(t, sentinel) != "preserve the failed attempt" {
		t.Fatalf("%v", statuses(again.State))
	}
}

func TestFixtureSymlinkIsRejectedWithoutReadingTarget(t *testing.T) {
	root := resolvePath(t.TempDir())
	sp := envTrial(t, root, "", "")
	secret := filepath.Join(root, "unrelated.txt")
	_ = os.WriteFile(secret, []byte("synthetic outside fixture"), 0o644)
	_ = os.Symlink(secret, filepath.Join(root, "fixture", "linked"))
	r := freshRun(t, sp)
	if _, err := r.Go(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	if r.Status("prepare") != "error" || !strings.Contains(util.S(util.Get(r.State, "stages", "prepare", "error")), "symlinks") {
		t.Fatalf("%v", util.Get(r.State, "stages", "prepare"))
	}
	if _, err := os.Lstat(filepath.Join(r.RunDir, "workspace", "linked")); err == nil {
		t.Fatal("symlink copied")
	}
	if readFile(t, secret) != "synthetic outside fixture" {
		t.Fatal("target changed")
	}
}
