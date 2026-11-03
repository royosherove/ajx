package ci

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/runner"
	"github.com/royosherove/ajx/internal/util"

	_ "github.com/royosherove/ajx/internal/builtin"
)

const trialBody = `[trial]
id = "t"
product = "p"
workspace_root = "%s"
repetitions = %d
[task]
prompt_file = "task-prompt.md"
[adapters.fake]
execute = ["/bin/sh", "-c", "true"]
%s
[reporter]
harness = "claude-code"
[[cells]]
id = "a"
harness = "fake"
[[cells]]
id = "b"
harness = "fake"
`

const oneCheck = "[[verify]]\nname = \"ok\"\nrun = \"true\"\n[[teardown]]\nrun = \"true\"\n"

func writeTrial(t *testing.T, reps int, extra string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.WriteFile(filepath.Join(dir, "task-prompt.md"), []byte("x\n"), 0o644)
	path := filepath.Join(dir, "trial.toml")
	os.WriteFile(path, []byte(fmt.Sprintf(trialBody, filepath.Join(dir, "ws"), reps, extra)), 0o644)
	return path
}

// fakeRun records attempts as a real run would: state.json, verify.json.
type attempt struct {
	outcome  string // succeeded | failed | "" (no verify.json)
	teardown string // ok | auth_errors | ""
	errStage string
	authErrs bool
}

func fakeRun(attempts map[string]attempt, err error) func(context.Context, util.M, runner.MatrixOpts) (util.M, error) {
	return func(ctx context.Context, sp util.M, o runner.MatrixOpts) (util.M, error) {
		out := util.S(util.Get(sp, "trial", "output_dir"))
		for id, a := range attempts {
			dir := filepath.Join(out, "runs", id)
			stages := util.M{}
			for _, s := range runner.Stages {
				stages[s] = util.M{"status": "done"}
			}
			if a.errStage != "" {
				stages[a.errStage] = util.M{"status": "error", "error": "RuntimeError: boom"}
			}
			st := util.M{"stages": stages, "execute": util.M{"started_at": "2026-01-01T00:00:00Z", "stopped_at": "2026-01-01T00:00:05Z"}}
			if a.teardown != "" {
				st["teardown"] = util.M{"status": a.teardown}
			}
			util.WriteJSON(filepath.Join(dir, "state.json"), st)
			if a.outcome != "" {
				v := util.M{"outcome": a.outcome, "passed": 1, "total": 1, "checks": []any{util.M{"name": "ok", "passed": a.outcome == "succeeded"}},
					"checks_with_auth_errors": []any{}}
				if a.authErrs {
					v["checks_with_auth_errors"] = []any{"ok"}
				}
				util.WriteJSON(filepath.Join(dir, "verify.json"), v)
			}
		}
		return util.M{}, err
	}
}

func hooks(doctorCode int, run func(context.Context, util.M, runner.MatrixOpts) (util.M, error)) Hooks {
	return Hooks{
		Validate: func(trial string, cells []string) ([]string, util.M, error) { return []string{"ok"}, util.M{}, nil },
		Doctor:   func(trial string) (int, []string, error) { return doctorCode, []string{"doctor"}, nil },
		Run:      run,
	}
}

func evaluate(t *testing.T, o Options, h Hooks) (int, util.M, string) {
	t.Helper()
	if o.Output == "" {
		o.Output = filepath.Join(t.TempDir(), "out")
	}
	s := New(o, h)
	code := s.Run(context.Background())
	result := util.ReadMap(filepath.Join(o.Output, "ci-result.json"))
	if o.Result != "" {
		result = util.ReadMap(o.Result)
	}
	junit, _ := os.ReadFile(filepath.Join(o.Output, "junit.xml"))
	if len(junit) > 0 {
		var v any
		if err := xml.Unmarshal(junit, &v); err != nil {
			t.Fatalf("junit.xml is not XML: %v\n%s", err, junit)
		}
	}
	if util.Int(result["exit_code"], -1) != code {
		t.Fatalf("result exit_code %v != %d", result["exit_code"], code)
	}
	return code, result, string(junit)
}

func codes(result util.M) string {
	var out []string
	for _, r := range util.Maps(result["reasons"]) {
		out = append(out, util.S(r["code"])+"/"+util.S(r["severity"]))
	}
	return strings.Join(out, ",")
}

func TestPassedOnlyWhenEveryPlannedRunVerified(t *testing.T) {
	trial := writeTrial(t, 1, oneCheck)
	code, res, junit := evaluate(t, Options{Trial: trial}, hooks(0, fakeRun(map[string]attempt{"a-r1": {outcome: "succeeded", teardown: "ok"},
		"b-r1": {outcome: "succeeded", teardown: "ok"}}, nil)))
	if code != ExitPassed || res["decision"] != Passed {
		t.Fatalf("%d %v %s", code, res["decision"], codes(res))
	}
	if util.Int(util.Get(res, "coverage", "passed"), 0) != 2 || !strings.Contains(junit, `name="a-r1"`) {
		t.Fatalf("coverage/junit: %v\n%s", res["coverage"], junit)
	}
	if util.Get(res, "plan", "sha256") == nil || util.Get(res, "trial", "sha256") == nil {
		t.Fatalf("hashes missing: %v %v", res["plan"], res["trial"])
	}
}

func TestFailedVerificationFailsTheCell(t *testing.T) {
	trial := writeTrial(t, 2, oneCheck)
	runs := map[string]attempt{"a-r1": {outcome: "succeeded", teardown: "ok"}, "a-r2": {outcome: "failed", teardown: "ok"},
		"b-r1": {outcome: "succeeded", teardown: "ok"}, "b-r2": {outcome: "succeeded", teardown: "ok"}}
	code, res, junit := evaluate(t, Options{Trial: trial}, hooks(0, fakeRun(runs, nil)))
	if code != ExitFailed || !strings.Contains(codes(res), "success_rate_below_target/failure") {
		t.Fatalf("%d %s", code, codes(res))
	}
	if !strings.Contains(junit, `<failure type="verification_failed"`) {
		t.Fatalf("junit:\n%s", junit)
	}
	half := 0.5
	code, res, _ = evaluate(t, Options{Trial: trial, MinSuccessRate: &half}, hooks(0, fakeRun(runs, nil)))
	if code != ExitPassed {
		t.Fatalf("rate 0.5: %d %s", code, codes(res))
	}
}

func TestMissingEvidenceIsInconclusiveNotPassed(t *testing.T) {
	trial := writeTrial(t, 1, oneCheck)
	code, res, _ := evaluate(t, Options{Trial: trial}, hooks(0, fakeRun(map[string]attempt{
		"a-r1": {outcome: "succeeded", teardown: "ok"}, "b-r1": {errStage: "execute", teardown: "ok"}}, nil)))
	if code != ExitInconclusive || !strings.Contains(codes(res), "stage_error/inconclusive") ||
		!strings.Contains(codes(res), "insufficient_evidence") {
		t.Fatalf("%d %s", code, codes(res))
	}
	code, res, _ = evaluate(t, Options{Trial: trial}, hooks(0, fakeRun(map[string]attempt{"a-r1": {outcome: "succeeded", teardown: "ok"}}, nil)))
	if code != ExitInconclusive || !strings.Contains(codes(res), "run_missing") {
		t.Fatalf("missing run: %d %s", code, codes(res))
	}
	code, res, _ = evaluate(t, Options{Trial: trial}, hooks(0, fakeRun(map[string]attempt{
		"a-r1": {outcome: "succeeded", teardown: "ok", authErrs: true}, "b-r1": {outcome: "succeeded", teardown: "ok"}}, nil)))
	if code != ExitInconclusive || !strings.Contains(codes(res), "verification_untrusted") {
		t.Fatalf("auth errors: %d %s", code, codes(res))
	}
	noChecks := writeTrial(t, 1, "")
	code, res, _ = evaluate(t, Options{Trial: noChecks}, hooks(0, fakeRun(map[string]attempt{
		"a-r1": {outcome: "not_run"}, "b-r1": {outcome: "not_run"}}, nil)))
	if code != ExitInconclusive || !strings.Contains(codes(res), "no_checks") {
		t.Fatalf("no checks: %d %s", code, codes(res))
	}
}

func TestCleanupPolicy(t *testing.T) {
	trial := writeTrial(t, 1, oneCheck)
	runs := map[string]attempt{"a-r1": {outcome: "succeeded", teardown: "auth_errors"}, "b-r1": {outcome: "succeeded", teardown: "ok"}}
	code, res, _ := evaluate(t, Options{Trial: trial}, hooks(0, fakeRun(runs, nil)))
	if code != ExitPassed || !strings.Contains(codes(res), "cleanup_not_confirmed/warning") {
		t.Fatalf("not required: %d %s", code, codes(res))
	}
	code, res, junit := evaluate(t, Options{Trial: trial, RequireCleanup: true}, hooks(0, fakeRun(runs, nil)))
	if code != ExitFailed || !strings.Contains(codes(res), "cleanup_not_confirmed/failure") || !strings.Contains(junit, `<failure type="cleanup_not_confirmed"`) {
		t.Fatalf("required: %d %s\n%s", code, codes(res), junit)
	}
	policy := filepath.Join(t.TempDir(), "acceptance.toml")
	os.WriteFile(policy, []byte("schema_version = 1\n[acceptance]\nrequire_cleanup = true\nmin_success_rate = 1.0\n"), 0o644)
	code, res, _ = evaluate(t, Options{Trial: trial, Policy: policy}, hooks(0, fakeRun(runs, nil)))
	if code != ExitFailed || util.Get(res, "policy", "sha256") == nil {
		t.Fatalf("policy: %d %s %v", code, codes(res), res["policy"])
	}
}

func TestStopsEarlyAndMapsExitCodes(t *testing.T) {
	trial := writeTrial(t, 1, oneCheck)
	called := false
	runFn := func(ctx context.Context, sp util.M, o runner.MatrixOpts) (util.M, error) {
		called = true
		return nil, nil
	}
	code, res, _ := evaluate(t, Options{Trial: trial}, hooks(1, runFn))
	if code != ExitInconclusive || called || !strings.Contains(codes(res), "not_ready") {
		t.Fatalf("doctor: %d %v %s", code, called, codes(res))
	}
	code, res, _ = evaluate(t, Options{Trial: trial}, hooks(0, fakeRun(nil, &runner.LockedError{Msg: "held"})))
	if code != ExitLocked || !strings.Contains(codes(res), "locked") {
		t.Fatalf("locked: %d %s", code, codes(res))
	}
	code, res, _ = evaluate(t, Options{Trial: trial}, hooks(0, fakeRun(map[string]attempt{"a-r1": {outcome: "succeeded", teardown: "ok"}}, util.ErrInterrupted)))
	if code != ExitInterrupted || !strings.Contains(codes(res), "interrupted") {
		t.Fatalf("interrupted: %d %s", code, codes(res))
	}
	out := filepath.Join(t.TempDir(), "out")
	evaluate(t, Options{Trial: trial, Output: out}, hooks(0, fakeRun(map[string]attempt{"a-r1": {outcome: "succeeded", teardown: "ok"}, "b-r1": {outcome: "succeeded", teardown: "ok"}}, nil)))
	code, res, _ = evaluate(t, Options{Trial: trial, Output: out}, hooks(0, fakeRun(nil, nil)))
	if code != ExitInvalid || !strings.Contains(codes(res), "output_not_fresh") {
		t.Fatalf("reuse: %d %s", code, codes(res))
	}
	code, _, _ = evaluate(t, Options{Trial: trial, Output: out, AllowReuse: true}, hooks(0, fakeRun(nil, nil)))
	if code != ExitPassed {
		t.Fatalf("allow reuse: %d", code)
	}
	resultPath := filepath.Join(t.TempDir(), "r.json")
	badHooks := hooks(0, nil)
	badHooks.Validate = func(string, []string) ([]string, util.M, error) { return nil, nil, fmt.Errorf("broken") }
	code, res, _ = evaluate(t, Options{Trial: trial, Result: resultPath}, badHooks)
	if code != ExitInvalid || !strings.Contains(codes(res), "invalid_config") {
		t.Fatalf("invalid: %d %s", code, codes(res))
	}
	policy := filepath.Join(t.TempDir(), "acceptance.toml")
	os.WriteFile(policy, []byte("[acceptance]\nrequired_cells = [\"zzz\"]\n"), 0o644)
	code, res, _ = evaluate(t, Options{Trial: trial, Policy: policy}, hooks(0, nil))
	if code != ExitInvalid || !strings.Contains(util.S(util.Maps(res["reasons"])[0]["message"]), "zzz") {
		t.Fatalf("required cell: %d %s", code, codes(res))
	}
	os.WriteFile(policy, []byte("[acceptance]\nmin_sucess_rate = 1\n"), 0o644)
	if _, err := LoadPolicy(policy); err == nil || !strings.Contains(err.Error(), "min_sucess_rate") {
		t.Fatalf("typo accepted: %v", err)
	}
}

func TestAbortWritesAnInconclusiveResult(t *testing.T) {
	trial := writeTrial(t, 1, oneCheck)
	out := filepath.Join(t.TempDir(), "out")
	block := make(chan struct{})
	var s *Session
	runFn := func(ctx context.Context, sp util.M, o runner.MatrixOpts) (util.M, error) {
		if code := s.Abort(5); code != ExitInterrupted {
			t.Errorf("abort code %d", code)
		}
		close(block)
		return nil, util.ErrInterrupted
	}
	s = New(Options{Trial: trial, Output: out}, hooks(0, runFn))
	code := s.Run(context.Background())
	<-block
	res := util.ReadMap(filepath.Join(out, "ci-result.json"))
	if code != ExitInterrupted || res["decision"] != Inconclusive || !strings.Contains(codes(res), "cleanup_not_confirmed") {
		t.Fatalf("%d %v %s", code, res["decision"], codes(res))
	}
}
