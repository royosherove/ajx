package ci

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/runner"
	"github.com/royosherove/ajx/internal/util"
)

func TestPolicyRejectsWrongTypesAndNonFiniteNumbers(t *testing.T) {
	for _, text := range []string{
		"acceptance = true", "acceptance = []", "[[acceptance]]\nrequire_cleanup = true",
		"schema_version = 1.0", "schema_version = 1.9", "schema_version = true", `schema_version = "1"`,
		"[acceptance]\nmin_repetitions = 1.9", "[acceptance]\nmin_repetitions = 1.0",
		"[acceptance]\nmin_repetitions = -1", "[acceptance]\nmin_repetitions = true",
		"[acceptance]\nmin_success_rate = nan", "[acceptance]\nmin_success_rate = +inf",
		"[acceptance]\nmin_success_rate = -inf", "[acceptance]\nmin_success_rate = true",
		"[acceptance]\nmin_success_rate = 1.1",
	} {
		t.Run(text, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "acceptance.toml")
			mustWrite(t, path, text)
			if _, err := LoadPolicy(path); err == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
	for _, text := range []string{
		"", "schema_version = 1", "[acceptance]\nmin_repetitions = 0\nmin_success_rate = 0",
		"[acceptance]\nmin_repetitions = 2\nmin_success_rate = 1.0\nrequire_cleanup = true",
	} {
		t.Run("valid:"+text, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "acceptance.toml")
			mustWrite(t, path, text)
			if _, err := LoadPolicy(path); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInvalidRateNeverRunsAndWritesStandardJSON(t *testing.T) {
	for _, rate := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		t.Run(fmt.Sprint(rate), func(t *testing.T) {
			trial := writeTrial(t, 1, oneCheck)
			out := filepath.Join(t.TempDir(), "out")
			h := mustNotRun(t)
			s := New(Options{Trial: trial, Output: out, MinSuccessRate: &rate}, h)
			if code := s.Run(context.Background()); code != ExitInvalid {
				t.Fatalf("exit=%d result=%v", code, s.Result())
			}
			raw, err := os.ReadFile(filepath.Join(out, "ci-result.json"))
			if err != nil || !json.Valid(raw) {
				t.Fatalf("error result is not valid JSON: %v %s", err, raw)
			}
			if _, err := os.Stat(filepath.Join(out, ".lock")); !os.IsNotExist(err) {
				t.Fatalf("lock left behind: %v", err)
			}
		})
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustNotRun(t *testing.T) Hooks {
	t.Helper()
	h := hooks(0, nil)
	h.Doctor = func(string) (int, []string, error) {
		t.Error("doctor ran after an invalid policy/output")
		return 0, nil, nil
	}
	h.Run = func(context.Context, util.M, runner.MatrixOpts, *runner.MatrixLock) (util.M, error) {
		t.Error("worker ran after an invalid policy/output")
		return nil, nil
	}
	return h
}

func TestRefusedOutputPreservesPriorArtifacts(t *testing.T) {
	for _, kind := range []string{"missing-state", "locked"} {
		t.Run(kind, func(t *testing.T) {
			trial := writeTrial(t, 1, oneCheck)
			out := filepath.Join(t.TempDir(), "out")
			mustWrite(t, filepath.Join(out, "ci-result.json"), `{"decision":"passed","marker":"prior"}`)
			mustWrite(t, filepath.Join(out, "junit.xml"), "<prior/>")
			mustWrite(t, filepath.Join(out, "runs", "a-r1", "verify.json"), `{"outcome":"succeeded"}`)
			if kind == "locked" {
				lock, err := runner.AcquireMatrixLock(out)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			s := New(Options{Trial: trial, Output: out, AllowReuse: kind == "locked"}, mustNotRun(t))
			want := ExitInvalid
			if kind == "locked" {
				want = ExitLocked
			}
			if code := s.Run(context.Background()); code != want {
				t.Fatalf("exit=%d result=%v", code, s.Result())
			}
			for name, want := range map[string]string{
				"ci-result.json": `{"decision":"passed","marker":"prior"}`, "junit.xml": "<prior/>",
			} {
				got, err := os.ReadFile(filepath.Join(out, name))
				if err != nil || string(got) != want {
					t.Fatalf("%s changed: %v %s", name, err, got)
				}
			}
			if result, junit := s.Paths(); result != "" || junit != "" {
				t.Fatalf("refused invocation claims it wrote artifacts: %s %s", result, junit)
			}
		})
	}
}

func TestInvalidReusePolicyPreservesPriorArtifacts(t *testing.T) {
	trial := writeTrial(t, 1, oneCheck)
	out := filepath.Join(t.TempDir(), "out")
	mustWrite(t, filepath.Join(out, "runs", "a-r1", "state.json"), "{}")
	const prior = `{"decision":"passed","marker":"prior"}`
	mustWrite(t, filepath.Join(out, "ci-result.json"), prior)
	mustWrite(t, filepath.Join(out, "junit.xml"), "<prior/>")
	rate := math.NaN()
	s := New(Options{Trial: trial, Output: out, AllowReuse: true, MinSuccessRate: &rate}, mustNotRun(t))
	if code := s.Run(context.Background()); code != ExitInvalid {
		t.Fatalf("exit=%d", code)
	}
	for name, want := range map[string]string{"ci-result.json": prior, "junit.xml": "<prior/>"} {
		got, err := os.ReadFile(filepath.Join(out, name))
		if err != nil || string(got) != want {
			t.Fatalf("invalid reuse replaced %s: %v %s", name, err, got)
		}
	}
}

func TestOutputLockCoversDoctorAndWorker(t *testing.T) {
	trial := writeTrial(t, 1, oneCheck)
	out := filepath.Join(t.TempDir(), "out")
	checkLock := func() {
		t.Helper()
		lock, err := runner.AcquireMatrixLock(out)
		if err == nil {
			lock.Close()
			t.Fatal("another invocation could acquire the output")
		}
	}
	h := hooks(0, nil)
	h.Doctor = func(string) (int, []string, error) {
		checkLock()
		return 0, []string{"synthetic doctor"}, nil
	}
	h.Run = func(ctx context.Context, sp util.M, o runner.MatrixOpts, lock *runner.MatrixLock) (util.M, error) {
		checkLock()
		if lock == nil {
			t.Fatal("runner did not receive the acquired lock")
		}
		return fakeRun(passedAttempts(), nil)(ctx, sp, o, lock)
	}
	code, _, _ := evaluate(t, Options{Trial: trial, Output: out}, h)
	if code != ExitPassed {
		t.Fatalf("exit=%d", code)
	}
	lock, err := runner.AcquireMatrixLock(out)
	if err != nil {
		t.Fatalf("completed invocation retained its lock: %v", err)
	}
	lock.Close()
}

func passedAttempts() map[string]attempt {
	return map[string]attempt{"a-r1": {outcome: "succeeded", teardown: "ok"}, "b-r1": {outcome: "succeeded", teardown: "ok"}}
}

func TestProvisionalWriteFailureStopsBeforeWork(t *testing.T) {
	for _, dest := range []string{"result", "junit"} {
		t.Run(dest, func(t *testing.T) {
			trial := writeTrial(t, 1, oneCheck)
			out := filepath.Join(t.TempDir(), "out")
			blocked := filepath.Join(t.TempDir(), "file", "artifact")
			mustWrite(t, filepath.Dir(blocked), "not a directory")
			o := Options{Trial: trial, Output: out}
			if dest == "result" {
				o.Result = blocked
			} else {
				o.JUnit = blocked
			}
			s := New(o, mustNotRun(t))
			if code := s.Run(context.Background()); code != ExitInvalid {
				t.Fatalf("exit=%d result=%v", code, s.Result())
			}
			if !strings.Contains(codes(s.Result()), "artifact_write_error") {
				t.Fatalf("write error not recorded: %v", s.Result())
			}
			resultPath, junitPath := s.Paths()
			if dest == "result" && resultPath != "" || dest == "junit" && junitPath != "" {
				t.Fatalf("unwritten path advertised: %s %s", resultPath, junitPath)
			}
			if resultPath != "" {
				if util.ReadMap(resultPath)["decision"] == Passed {
					t.Fatal("JSON passed after a required artifact failed")
				}
			}
			if junitPath != "" {
				raw, _ := os.ReadFile(junitPath)
				if !strings.Contains(string(raw), `type="artifact_write_error"`) {
					t.Fatalf("JUnit omitted write failure: %s", raw)
				}
			}
		})
	}
}

func TestFinalWriteFailureCannotPass(t *testing.T) {
	for _, name := range []string{"ci-result.json", "junit.xml"} {
		t.Run(name, func(t *testing.T) {
			trial := writeTrial(t, 1, oneCheck)
			out := filepath.Join(t.TempDir(), "out")
			h := hooks(0, func(ctx context.Context, sp util.M, o runner.MatrixOpts, lock *runner.MatrixLock) (util.M, error) {
				m, err := fakeRun(passedAttempts(), nil)(ctx, sp, o, lock)
				path := filepath.Join(out, name)
				if e := os.Remove(path); e != nil {
					t.Fatal(e)
				}
				if e := os.Mkdir(path, 0o755); e != nil {
					t.Fatal(e)
				}
				return m, err
			})
			s := New(Options{Trial: trial, Output: out}, h)
			if code := s.Run(context.Background()); code != ExitInconclusive || s.Result()["decision"] != Inconclusive {
				t.Fatalf("exit=%d result=%v", code, s.Result())
			}
			if !strings.Contains(codes(s.Result()), "artifact_write_error") {
				t.Fatal("missing artifact error")
			}
			if name == "junit.xml" && util.ReadMap(filepath.Join(out, "ci-result.json"))["decision"] == Passed {
				t.Fatal("JSON passed after final JUnit write failure")
			}
		})
	}
}

func TestDoctorArtifactFailureStopsBeforeWorker(t *testing.T) {
	trial := writeTrial(t, 1, oneCheck)
	out := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(filepath.Join(out, "ci-doctor.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := mustNotRun(t)
	h.Doctor = func(string) (int, []string, error) { return 0, []string{"ready"}, nil }
	s := New(Options{Trial: trial, Output: out}, h)
	if code := s.Run(context.Background()); code != ExitInconclusive || !strings.Contains(codes(s.Result()), "artifact_write_error") {
		t.Fatalf("exit=%d result=%v", code, s.Result())
	}
}

type runHook = func(context.Context, util.M, runner.MatrixOpts, *runner.MatrixLock) (util.M, error)

func mutateRun(t *testing.T, change func(dir string, state util.M)) runHook {
	return func(ctx context.Context, sp util.M, o runner.MatrixOpts, lock *runner.MatrixLock) (util.M, error) {
		m, err := fakeRun(passedAttempts(), nil)(ctx, sp, o, lock)
		dir := filepath.Join(util.S(util.Get(sp, "trial", "output_dir")), "runs", "a-r1")
		state := util.ReadMap(filepath.Join(dir, "state.json"))
		change(dir, state)
		if e := util.WriteJSON(filepath.Join(dir, "state.json"), state); e != nil {
			t.Fatal(e)
		}
		return m, err
	}
}

func TestRequiredReportsUseActualArtifacts(t *testing.T) {
	for _, defect := range []string{"none", "report.html", "pretty-journey.html", "journey.md", "asks.md", "run.json", "asks.json", "stub", "stale-run"} {
		t.Run(defect, func(t *testing.T) {
			trial := writeTrial(t, 1, oneCheck)
			policy := filepath.Join(t.TempDir(), "policy.toml")
			mustWrite(t, policy, "[acceptance]\nrequire_reports = true\n")
			h := hooks(0, mutateRun(t, func(dir string, state util.M) {
				switch defect {
				case "none":
				case "stub":
					state["journey_provenance"] = "unavailable"
				case "stale-run":
					util.WriteJSON(filepath.Join(dir, "run.json"), util.M{"review_status": "incomplete"})
				default:
					if err := os.Remove(filepath.Join(dir, defect)); err != nil {
						t.Fatal(err)
					}
				}
			}))
			code, res, junit := evaluate(t, Options{Trial: trial, Policy: policy}, h)
			if defect == "none" {
				if code != ExitPassed {
					t.Fatalf("complete reports rejected: %d %s", code, codes(res))
				}
			} else if code != ExitInconclusive || !strings.Contains(junit, `type="reporting_incomplete"`) ||
				util.Truthy(util.Maps(res["runs"])[0]["report_complete"]) {
				t.Fatalf("incomplete report passed: %d %s\n%s", code, codes(res), junit)
			}
		})
	}
}

func TestUntrustedVerificationCannotBecomeProductFailure(t *testing.T) {
	for _, outcome := range []string{"succeeded", "failed", "partial"} {
		t.Run(outcome, func(t *testing.T) {
			trial := writeTrial(t, 1, oneCheck)
			runs := passedAttempts()
			runs["a-r1"] = attempt{outcome: outcome, teardown: "ok", authErrs: true}
			code, res, junit := evaluate(t, Options{Trial: trial}, hooks(0, fakeRun(runs, nil)))
			if code != ExitInconclusive || util.Int(util.Get(res, "coverage", "failed"), -1) != 0 ||
				!strings.Contains(junit, `type="verification_untrusted"`) {
				t.Fatalf("%d %s\n%s", code, codes(res), junit)
			}
		})
	}
}

func TestInterruptedExecutionRemainsInconclusiveOnResume(t *testing.T) {
	for _, stage := range []string{"execute", "narrate"} {
		for _, outcome := range []string{"succeeded", "failed"} {
			t.Run(stage+"-"+outcome, func(t *testing.T) {
				trial := writeTrial(t, 1, oneCheck)
				h := hooks(0, mutateRun(t, func(dir string, state util.M) {
					util.MapOr(state["stages"])[stage] = util.M{"status": "interrupted"}
					util.WriteJSON(filepath.Join(dir, "verify.json"), util.M{"outcome": outcome, "passed": 0, "total": 1})
				}))
				code, res, _ := evaluate(t, Options{Trial: trial}, h)
				want := ExitInconclusive
				if stage == "narrate" {
					want = ExitPassed
					if outcome == "failed" {
						want = ExitFailed
					}
				}
				if code != want {
					t.Fatalf("%d, want %d: %s", code, want, codes(res))
				}
			})
		}
	}
}

func TestFailedPreparationDoesNotConfirmUnattemptedCleanup(t *testing.T) {
	trial := writeTrial(t, 1, oneCheck+"[[setup]]\nrun = \"true\"\n")
	h := hooks(0, mutateRun(t, func(dir string, state util.M) {
		state["stages"] = util.M{"prepare": util.M{"status": "error", "error": "synthetic setup failed"}}
		delete(state, "teardown")
		delete(state, "execute")
		os.Remove(filepath.Join(dir, "verify.json"))
	}))
	code, res, _ := evaluate(t, Options{Trial: trial, RequireCleanup: true}, h)
	if code != ExitFailed || util.Truthy(util.Get(util.Maps(res["runs"])[0], "cleanup", "confirmed")) ||
		!strings.Contains(codes(res), "cleanup_not_confirmed/failure") {
		t.Fatalf("%d %s", code, codes(res))
	}
}

func TestUnconfirmedWorkerOverridesSuccessfulTaskCleanup(t *testing.T) {
	for _, teardown := range []bool{true, false} {
		t.Run(fmt.Sprint(teardown), func(t *testing.T) {
			body := oneCheck
			if !teardown {
				body = "[[verify]]\nrun = \"true\"\n"
			}
			trial := writeTrial(t, 1, body)
			h := hooks(0, mutateRun(t, func(_ string, state util.M) {
				state["worker_cleanup"] = util.M{"confirmed": false, "status": "unconfirmed"}
			}))
			code, res, _ := evaluate(t, Options{Trial: trial, RequireCleanup: true}, h)
			if code != ExitFailed || util.Truthy(util.Get(util.Maps(res["runs"])[0], "cleanup", "confirmed")) {
				t.Fatalf("worker cleanup ignored: %d %s", code, codes(res))
			}
		})
	}
}

func TestEarlyInvalidInputDoesNotOverwriteStandaloneResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prior.json")
	const prior = `{"decision":"passed","marker":"prior"}`
	mustWrite(t, path, prior)
	rate := math.NaN()
	s := New(Options{Result: path, MinSuccessRate: &rate}, mustNotRun(t))
	if code := s.Run(context.Background()); code != ExitInvalid {
		t.Fatalf("exit=%d", code)
	}
	if data, _ := os.ReadFile(path); string(data) != prior {
		t.Fatalf("overwrote standalone artifact: %s", data)
	}
}

func TestIdenticalArtifactPathsAreRejectedBeforeWork(t *testing.T) {
	trial := writeTrial(t, 1, oneCheck)
	out := filepath.Join(t.TempDir(), "out")
	path := filepath.Join(out, "both")
	s := New(Options{Trial: trial, Output: out, Result: path, JUnit: path}, mustNotRun(t))
	if code := s.Run(context.Background()); code != ExitInvalid {
		t.Fatalf("exit=%d", code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("conflicting artifact was created: %v", err)
	}
}
