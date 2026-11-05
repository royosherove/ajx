package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

func TestInitTaskFilePreservesBytesAndReportsSavedHash(t *testing.T) {
	offline(t)
	root := t.TempDir()
	source := filepath.Join(root, "source.md")
	for _, task := range []string{"Write result.json.", "  Keep café.\r\n\r\nSecond line.\n\n"} {
		if err := os.WriteFile(source, []byte(task), 0o600); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(t.TempDir(), "trial")
		code, out, diagnostic := run(t, "init", dir, "--product", "demo", "--task-file", source, "--json")
		if code != 0 {
			t.Fatalf("%d %s %s", code, out, diagnostic)
		}
		saved, err := os.ReadFile(filepath.Join(dir, "task-prompt.md"))
		if err != nil || !bytes.Equal(saved, []byte(task)) {
			t.Fatalf("task file changed: %q, %v", saved, err)
		}
		if got := decode(t, out)["prompt_sha256"]; got != util.Sha256Bytes(saved) {
			t.Fatalf("wrong saved prompt hash: %v", got)
		}
		replacement := []byte("Replace the original.\n")
		if err := os.WriteFile(source, replacement, 0o600); err != nil {
			t.Fatal(err)
		}
		code, out, diagnostic = run(t, "init", dir, "--product", "demo", "--task-file", source, "--json")
		if code != 0 || decode(t, out)["prompt_sha256"] != util.Sha256Bytes(saved) {
			t.Fatalf("kept prompt hash must describe the destination: %d %s %s", code, out, diagnostic)
		}
		code, out, diagnostic = run(t, "init", dir, "--product", "demo", "--task-file", source, "--force", "--no-input")
		if code != 0 || !strings.Contains(out, "task-prompt.md sha256: "+util.Sha256Bytes(replacement)) {
			t.Fatalf("forced prompt/hash: %d %s %s", code, out, diagnostic)
		}
		after, err := os.ReadFile(filepath.Join(dir, "task-prompt.md"))
		if err != nil || !bytes.Equal(after, replacement) {
			t.Fatalf("forced prompt changed: %q, %v", after, err)
		}
	}
}

func TestInitRejectsInvalidTaskFilesBeforeWriting(t *testing.T) {
	offline(t)
	root := t.TempDir()
	empty, invalid, valid := filepath.Join(root, "empty"), filepath.Join(root, "invalid"), filepath.Join(root, "valid")
	for path, content := range map[string][]byte{empty: []byte(" \r\n\t"), invalid: {0xff, 0xfe}, valid: []byte("Task.\n")} {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"--task-file", filepath.Join(root, "missing")},
		{"--task-file", root},
		{"--task-file", empty},
		{"--task-file", invalid},
		{"--task-file", ""},
		{"--task-file", valid, "--task", ""},
		{"--task", "Task.", "--task-file", valid},
	} {
		dir := filepath.Join(t.TempDir(), "trial")
		code, out, diagnostic := run(t, append([]string{"init", dir, "--product", "demo", "--json"}, args...)...)
		if code != 2 {
			t.Fatalf("invalid task input accepted: %v => %d %s %s", args, code, out, diagnostic)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("invalid input wrote files: %v", err)
		}
	}
}

func TestInitBudgetWritesAValidLimitAndRejectsInvalidValues(t *testing.T) {
	offline(t)
	for _, raw := range []string{"0.01", "2.5", "300", "3e2"} {
		dir := filepath.Join(t.TempDir(), "trial")
		code, out, diagnostic := run(t, "init", dir, "--product", "demo", "--max-budget-usd", raw, "--json")
		if code != 0 {
			t.Fatalf("%s: %d %s %s", raw, code, out, diagnostic)
		}
		sp, err := spec.Load(filepath.Join(dir, "trial.toml"), spec.LoadOpts{})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]float64{"0.01": 0.01, "2.5": 2.5, "300": 300, "3e2": 300}[raw]
		if got, ok := util.Num(util.Get(sp, "trial", "max_budget_usd")); !ok || got != want {
			t.Fatalf("budget %q was not written: %v", raw, got)
		}
	}
	for _, raw := range []string{"", "0", "-1", "NaN", "Inf", "-Inf", "1e309", "many"} {
		dir := filepath.Join(t.TempDir(), "trial")
		code, out, diagnostic := run(t, "init", dir, "--product", "demo", "--max-budget-usd="+raw, "--json")
		if code != 2 {
			t.Fatalf("invalid budget %q accepted: %d %s %s", raw, code, out, diagnostic)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("invalid budget wrote files: %v", err)
		}
	}
}
