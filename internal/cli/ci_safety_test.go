package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/royosherove/ajx/internal/util"
)

func TestCINonFiniteRateFailsBeforeWorkerAndWritesJSON(t *testing.T) {
	offline(t)
	dir := filepath.Join(t.TempDir(), "demo")
	copyTree(t, filepath.Join(repoRoot(t), "skills", "ajx", "examples", "console-demo"), dir)
	for _, value := range []string{"NaN", "Inf", "-Inf"} {
		t.Run(value, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out")
			code, text, stderr := run(t, "ci", dir, "--output", out, "--min-success-rate="+value, "--no-synthesis")
			if code != 2 {
				t.Fatalf("exit=%d\n%s\n%s", code, text, stderr)
			}
			raw, err := os.ReadFile(filepath.Join(out, "ci-result.json"))
			if err != nil || !json.Valid(raw) {
				t.Fatalf("invalid JSON: %v %s", err, raw)
			}
			if _, err := os.Stat(filepath.Join(out, "runs")); !os.IsNotExist(err) {
				t.Fatal("attempt created for invalid threshold")
			}
		})
	}
}

func TestCIBareNegativeInfinityIsAUsageErrorBeforeWork(t *testing.T) {
	offline(t)
	dir := filepath.Join(t.TempDir(), "demo")
	copyTree(t, filepath.Join(repoRoot(t), "skills", "ajx", "examples", "console-demo"), dir)
	out := filepath.Join(t.TempDir(), "out")
	code, text, stderr := run(t, "ci", dir, "--output", out, "--min-success-rate", "-Inf", "--no-synthesis")
	if code != 2 || !strings.Contains(stderr, "expected one argument") {
		t.Fatalf("exit=%d\n%s\n%s", code, text, stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("usage error created output or ran a worker: %v", err)
	}
}

func TestCIFailedArtifactWriteIsNotAdvertisedAsSuccess(t *testing.T) {
	offline(t)
	dir := filepath.Join(t.TempDir(), "demo")
	copyTree(t, filepath.Join(repoRoot(t), "skills", "ajx", "examples", "console-demo"), dir)
	out := filepath.Join(t.TempDir(), "out")
	badResult := filepath.Join(t.TempDir(), "result-dir")
	if err := os.Mkdir(badResult, 0o755); err != nil {
		t.Fatal(err)
	}
	code, text, stderr := run(t, "ci", dir, "--output", out, "--result", badResult, "--no-synthesis")
	if code != 2 || !strings.Contains(text, "artifact_write_error") || !strings.Contains(text, "result not written") {
		t.Fatalf("exit=%d\n%s\n%s", code, text, stderr)
	}
	if strings.Contains(text, "ci: decision passed") || strings.Contains(text, "ci: result "+badResult) {
		t.Fatalf("advertised nonexistent result: %s", text)
	}
	if _, err := os.Stat(filepath.Join(out, "runs")); !os.IsNotExist(err) {
		t.Fatal("worker ran without a provisional result")
	}
}

func TestCIPreservesCompletedResultWhenFreshnessCheckRefuses(t *testing.T) {
	offline(t)
	dir := filepath.Join(t.TempDir(), "demo")
	copyTree(t, filepath.Join(repoRoot(t), "skills", "ajx", "examples", "console-demo"), dir)
	out := filepath.Join(t.TempDir(), "out")
	if code, text, stderr := run(t, "ci", dir, "--output", out, "--no-synthesis"); code != 0 {
		t.Fatalf("first exit=%d\n%s\n%s", code, text, stderr)
	}
	path := filepath.Join(out, "ci-result.json")
	before, _ := os.ReadFile(path)
	if code, text, _ := run(t, "ci", dir, "--output", out, "--no-synthesis"); code != 2 || !strings.Contains(text, "output_not_fresh") {
		t.Fatalf("second exit=%d\n%s", code, text)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) || util.ReadMap(path)["decision"] != "passed" {
		t.Fatal("freshness refusal overwrote the prior result")
	}
}

func TestCIReuseContractRefusalPreservesArtifacts(t *testing.T) {
	offline(t)
	for _, change := range []string{"check", "prompt", "missing-state"} {
		for _, explicitOutput := range []bool{false, true} {
			t.Run(change+"/explicit-output="+strconv.FormatBool(explicitOutput), func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "demo")
				copyTree(t, filepath.Join(repoRoot(t), "skills", "ajx", "examples", "console-demo"), dir)
				out := filepath.Join(dir, "output")
				trialPath := filepath.Join(dir, "trial.toml")
				trial, err := os.ReadFile(trialPath)
				if err != nil {
					t.Fatal(err)
				}
				trial = []byte(strings.Replace(string(trial), "[trial]", "[trial]\noutput_dir = "+strconv.Quote(out), 1))
				if err := os.WriteFile(trialPath, trial, 0o644); err != nil {
					t.Fatal(err)
				}
				args := []string{"ci", dir, "--no-synthesis"}
				if explicitOutput {
					args = append(args, "--output", out)
				}
				if code, text, stderr := run(t, args...); code != 0 {
					t.Fatalf("initial exit=%d\n%s\n%s", code, text, stderr)
				}
				switch change {
				case "check":
					const original = `run = "cmp -s out.csv \"$AJX_TRIAL_DIR/expected.csv\""`
					if !strings.Contains(string(trial), original) {
						t.Fatal("synthetic verification command not found")
					}
					trial = []byte(strings.Replace(string(trial), original, `run = "false"`, 1))
					if err := os.WriteFile(trialPath, trial, 0o644); err != nil {
						t.Fatal(err)
					}
				case "prompt":
					if err := os.WriteFile(filepath.Join(dir, "task-prompt.md"), []byte("A different synthetic task.\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				case "missing-state":
					if err := os.Remove(filepath.Join(out, "runs", "demo-r1", "state.json")); err != nil {
						t.Fatal(err)
					}
				}
				before := snapshotCIArtifacts(t, out)
				code, text, stderr := run(t, append(args, "--allow-reuse")...)
				if code != 2 || !strings.Contains(text, "invalid_config") || !strings.Contains(text, "result not written") {
					t.Fatalf("refusal exit=%d\n%s\n%s", code, text, stderr)
				}
				after := snapshotCIArtifacts(t, out)
				if len(after) != len(before) {
					t.Fatalf("refusal changed the output file count: %d -> %d", len(before), len(after))
				}
				for path, old := range before {
					if current, ok := after[path]; !ok || current != old {
						t.Errorf("refusal rewrote or removed %s", path)
					}
				}
				if util.ReadMap(filepath.Join(out, "ci-result.json"))["decision"] != "passed" {
					t.Fatal("refusal replaced the previously passed summary")
				}
			})
		}
	}
}

type ciArtifactSnapshot struct {
	content string
	modTime time.Time
}

func snapshotCIArtifacts(t *testing.T, root string) map[string]ciArtifactSnapshot {
	t.Helper()
	files := map[string]ciArtifactSnapshot{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[rel] = ciArtifactSnapshot{content: string(data), modTime: info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
