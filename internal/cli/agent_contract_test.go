package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/runner"
	"github.com/royosherove/ajx/internal/util"
)

func TestRunRefusalsHaveActionableErrorTypes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
		kind string
	}{
		{&runner.StageRefusalError{RunID: "demo-r1", Stages: []string{"execute"}}, 2, "stage_refused"},
		{&runner.ProcessRecoveryError{RunID: "demo-r1", Reason: "owner unavailable"}, 1, "cleanup_unconfirmed"},
	} {
		code, kind, message := ExitCode(tc.err)
		if code != tc.code || kind != tc.kind || !strings.Contains(message, "demo-r1") {
			t.Fatalf("error mapping: %d %s %s", code, kind, message)
		}
	}
}

func TestJSONUsageErrorsAreMachineReadable(t *testing.T) {
	offline(t)
	cases := [][]string{
		{"validate", "--json"},
		{"init", "--no-input", "--json"},
		{"init", "unused", "--product", "Demo", "--timeout", "abc", "--json"},
		{"plugins", "--oops", "--json"},
		{"init", "unused", "--product", "Demo", "--wizard", "--json"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, text, diagnostic := run(t, args...)
			doc := decode(t, text)
			if code != 2 || doc["ok"] != false || doc["command"] != args[0] ||
				util.Int(doc["exit_code"], 0) != 2 || util.Get(doc, "error", "type") != "usage" ||
				!strings.Contains(diagnostic, "error:") {
				t.Fatalf("code %d; document %s; stderr %s", code, text, diagnostic)
			}
		})
	}
	code, text, _ := run(t, "validate", "--help", "--json")
	if code != 0 || !strings.HasPrefix(text, "usage:") {
		t.Fatalf("help must remain help: %d %s", code, text)
	}
	code, text, _ = run(t, "plugins", "--", "--json")
	if code != 2 || text != "" {
		t.Fatalf("literal --json after delimiter is not a flag: %d %s", code, text)
	}
}

func TestStatusRejectsMissingAndUnrelatedDirectories(t *testing.T) {
	offline(t)
	dir := t.TempDir()
	for _, path := range []string{dir, filepath.Join(dir, "missing")} {
		code, text, _ := run(t, "status", path, "--json")
		doc := decode(t, text)
		if code != 2 || doc["ok"] != false || util.Get(doc, "error", "type") != "trial_file_error" {
			t.Fatalf("unrecognized path reported success: %d %s", code, text)
		}
	}
	trial := filepath.Join(t.TempDir(), "unstarted")
	code, _, errText := run(t, "init", trial, "--product", "Demo", "--no-input")
	if code != 0 {
		t.Fatal(errText)
	}
	code, text, _ := run(t, "status", trial, "--json")
	if code != 0 || len(util.List(decode(t, text)["runs"])) != 0 {
		t.Fatalf("valid unstarted trial must remain queryable: %d %s", code, text)
	}
}

func TestStatusCleanupAndStaleRunnerAreHonest(t *testing.T) {
	offline(t)
	out := t.TempDir()
	dir := filepath.Join(out, "runs", "fake-r1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	save := func(state util.M) {
		t.Helper()
		data, err := util.MarshalIndent(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	save(util.M{"stages": util.M{"execute": util.M{"status": "running"}}})
	code, text, _ := run(t, "status", out, "--json")
	rows := util.Maps(decode(t, text)["runs"])
	if code != 0 || len(rows) != 1 || rows[0]["cleanup_confirmed"] != false {
		t.Fatalf("cleanup has not happened: %d %s", code, text)
	}
	host, _ := os.Hostname()
	lock, _ := util.MarshalIndent(util.M{"host": host, "pid": 2147483647})
	if err := os.WriteFile(filepath.Join(out, ".lock"), lock, 0o644); err != nil {
		t.Fatal(err)
	}
	code, text, _ = run(t, "status", out, "--json")
	doc := decode(t, text)
	rows = util.Maps(doc["runs"])
	if code != 1 || util.Get(doc, "runner", "state") != "stale" ||
		rows[0]["runner_state"] != "stale" || util.Get(rows[0], "stages", "execute") != "interrupted" {
		t.Fatalf("dead runner still shown live: %d %s", code, text)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"running"`) {
		t.Fatal("status must not rewrite recorded evidence")
	}
	save(util.M{"stages": util.M{"execute": util.M{"status": "done"}, "teardown": util.M{"status": "done"}},
		"teardown": util.M{"status": "ok"}})
	_, text, _ = run(t, "status", out, "--json")
	if util.Maps(decode(t, text)["runs"])[0]["cleanup_confirmed"] != true {
		t.Fatal("completed cleanup not recognized:", text)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	code, text, _ = run(t, "status", out, "--json")
	if code != 2 || decode(t, text)["ok"] != false {
		t.Fatalf("missing attempt state reported success: %d %s", code, text)
	}
}
