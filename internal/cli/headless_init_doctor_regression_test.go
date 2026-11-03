package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/util"
)

func TestHeadlessInitInvalidOptionsAndRetainedInvalidTrial(t *testing.T) {
	offline(t)
	for _, flags := range [][]string{
		{"--product", "   "}, {"--product", "demo", "--id", "Not A Slug"},
		{"--product", "demo", "--timeout", "0"}, {"--product", "demo", "--repetitions", "0"},
		{"--product", "demo", "--environment", "container", "--image", "ubuntu:latest"},
		{"--product", "demo", "--check", "http:"},
	} {
		dir := filepath.Join(t.TempDir(), "invalid")
		args := append([]string{"init", dir, "--json", "--no-input"}, flags...)
		code, out, stderr := run(t, args...)
		doc := decode(t, out)
		if code != 2 || doc["ok"] != false || doc["exit_code"] != float64(2) {
			t.Fatalf("%v: %d %s %s", args, code, out, stderr)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("invalid init wrote files: %v", err)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "trial.toml"), []byte("[trial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := run(t, "init", dir, "--product", "demo", "--json")
	doc := decode(t, out)
	if code != 2 || doc["ok"] != false || doc["valid"] != false || len(util.List(doc["next"])) != 0 ||
		util.Get(doc, "error", "type") != "trial_file_error" || !strings.Contains(stderr, "trial file error") {
		t.Fatalf("invalid retained trial: %d %s %s", code, out, stderr)
	}
}

func TestHeadlessInitNextCommandsRoundTripAndUseEffectiveMetadata(t *testing.T) {
	offline(t)
	dir := filepath.Join(t.TempDir(), "a trial's path")
	code, out, stderr := run(t, "init", dir, "--product", "demo", "--harness", "codex", "--id", "original", "--json")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
	doc := decode(t, out)
	if len(util.List(doc["missing"])) != 2 || len(util.List(doc["run_requires"])) == 0 {
		t.Fatalf("missing prerequisites: %s", out)
	}
	// Capture arguments with a shell function: no worker or doctor is invoked.
	for i, command := range util.Strings(doc["next"]) {
		captured, err := exec.Command("/bin/sh", "-c", "ajx() { printf '%s\\n' \"$@\"; }; "+command).Output()
		argv := util.Strings(util.List(doc["next_argv"])[i])
		if err != nil || string(captured) != strings.Join(argv[1:], "\n")+"\n" {
			t.Fatalf("command does not round trip: %q => %q, %v", command, captured, err)
		}
	}
	if !strings.HasPrefix(util.Strings(doc["next"])[2], "ajx run ") {
		t.Fatalf("headless continuation missing: %s", out)
	}
	code, out, _ = run(t, "init", dir, "--product", "replacement", "--harness", "kiro-cli", "--id", "replacement",
		"--task", "make result", "--check", "file:result", "--json")
	doc = decode(t, out)
	if code != 0 || doc["trial_id"] != "original" || doc["harness"] != "codex" ||
		doc["reporter"] != "codex" || len(util.List(doc["missing"])) != 2 {
		t.Fatalf("retained metadata: %d %s", code, out)
	}
	code, out, _ = run(t, "init", filepath.Join(t.TempDir(), "complete"), "--product", "demo", "--no-input",
		"--task", "make result", "--check", "file:result")
	if code != 0 || !strings.Contains(out, "ajx run ") || !strings.Contains(out, "ajx ci ") || strings.Contains(out, "browser") {
		t.Fatalf("headless text continuation: %d %s", code, out)
	}
}

func headlessDoctorFixture(t *testing.T, identity, versionBody, workspace, output string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte(versionBody), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin:/bin")
	if workspace == "" {
		workspace = filepath.Join(dir, "work")
	}
	if output == "" {
		output = filepath.Join(dir, "output")
	}
	// Paths come from t.TempDir, and contain no TOML delimiters.
	text := "[trial]\nid='doctor'\nproduct='demo'\nworkspace_root='" + workspace + "'\noutput_dir='" + output + `'
[task]
prompt_file='task-prompt.md'
[auth.synthetic]
type='env'
identity_cmd=` + identity + `
[reporter]
harness='codex'
auth='synthetic'
[[cells]]
id='worker'
harness='codex'
auth='synthetic'
[[verify]]
type='file_exists'
path='result'
`
	if err := os.WriteFile(filepath.Join(dir, "task-prompt.md"), []byte("make result"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "trial.toml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHeadlessDoctorUsesStructuredFailures(t *testing.T) {
	offline(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	for _, tc := range []struct {
		name, identity, version string
		ready                   bool
	}{
		{"successful", `['/bin/sh','-c','echo synthetic-identity']`, "#!/bin/sh\necho synthetic-v1\n", true},
		{"silent auth failure", `['/bin/sh','-c','exit 7']`, "#!/bin/sh\necho synthetic-v1\n", false},
		{"text auth failure", `['/bin/sh','-c','echo login-needed; exit 7']`, "#!/bin/sh\necho synthetic-v1\n", false},
		{"credential error at exit zero", `['/bin/sh','-c','echo AccessDenied']`, "#!/bin/sh\necho synthetic-v1\n", false},
		{"missing auth executable", `['/nonexistent/ajx-test-identity']`, "#!/bin/sh\necho synthetic-v1\n", false},
		{"broken interpreter", `['/bin/sh','-c','echo synthetic-identity']`, "#!/nonexistent/ajx-test-interpreter\n", false},
		{"failed version command", `['/bin/sh','-c','echo synthetic-identity']`, "#!/bin/sh\necho unavailable\nexit 9\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := headlessDoctorFixture(t, tc.identity, tc.version, "", "")
			code, out, stderr := run(t, "doctor", path, "--json")
			doc := decode(t, out)
			if (code == 0) != tc.ready || doc["ready"] != tc.ready || (code != 0 && code != 1) {
				t.Fatalf("%d %s %s", code, out, stderr)
			}
			if tc.name == "silent auth failure" {
				probe := util.MapOr(util.Get(doc, "reporter", "auth_identity_probe"))
				if probe["ok"] != false || probe["exit_code"] != float64(7) || probe["output"] != "" {
					t.Fatalf("lost structured auth failure: %v", probe)
				}
				cells := util.Maps(doc["cells"])
				if len(cells) != 1 || util.Get(cells[0], "auth_identity_probe", "ok") != false {
					t.Fatalf("worker failure lost: %v", cells)
				}
			}
		})
	}
}

func TestHeadlessDoctorRejectsUnusableDirectoriesWithoutLeavingProbes(t *testing.T) {
	offline(t)
	root := t.TempDir()
	file := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(file, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][2]string{{file, ""}, {"", file}, {filepath.Join(file, "nested"), ""}} {
		path := headlessDoctorFixture(t, `['/bin/sh','-c','echo synthetic-identity']`, "#!/bin/sh\necho synthetic-v1\n", paths[0], paths[1])
		code, out, stderr := run(t, "doctor", path, "--json")
		doc := decode(t, out)
		if code != 1 || doc["ready"] != false || len(util.List(doc["paths"])) != 2 {
			t.Fatalf("%d %s %s", code, out, stderr)
		}
	}
	if problem := doctorDirectory(filepath.Join(root, "not-created", "nested")); problem != "" {
		t.Fatal(problem)
	}
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 1 {
		t.Fatalf("directory probe left files behind: %v %v", files, err)
	}
	link := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(root, "missing"), link); err != nil {
		t.Fatal(err)
	}
	if problem := doctorDirectory(link); !strings.Contains(problem, "dangling") {
		t.Fatalf("dangling path accepted: %q", problem)
	}
	if os.Geteuid() != 0 {
		readOnly := filepath.Join(root, "read-only")
		if err := os.Mkdir(readOnly, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })
		if problem := doctorDirectory(readOnly); !strings.Contains(problem, "cannot write") {
			t.Fatalf("unwritable path accepted: %q", problem)
		}
	}
}
