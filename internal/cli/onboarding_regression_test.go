package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

func TestInitPreservesTaskArgumentBytes(t *testing.T) {
	offline(t)
	for _, task := range []string{"Write result.json.", "  Keep this spacing.\n\nAnd this line.\n\n", "Use café data.\r\n"} {
		dir := filepath.Join(t.TempDir(), "trial")
		code, out, diagnostic := run(t, "init", dir, "--product", "demo", "--task", task,
			"--material", "demo --help", "--no-input", "--json")
		if code != 0 {
			t.Fatalf("%d %s %s", code, out, diagnostic)
		}
		got, err := os.ReadFile(filepath.Join(dir, "task-prompt.md"))
		if err != nil || string(got) != task {
			t.Fatalf("task argument changed: got %q, want %q, error %v", got, task, err)
		}
		sp, err := spec.Load(filepath.Join(dir, "trial.toml"), spec.LoadOpts{})
		if err != nil || len(util.List(util.Get(sp, "task", "materials"))) != 1 {
			t.Fatalf("materials must remain recorded separately: %v %v", sp, err)
		}
	}
}

func TestInitNamesChecksAndRecordsProductVersionCommand(t *testing.T) {
	offline(t)
	dir := filepath.Join(t.TempDir(), "trial")
	code, out, diagnostic := run(t, "init", dir, "--product", "demo", "--task", "Write result.json.",
		"--check", `test -s result.json`, "--check-name", `result "exists"`,
		"--check", "file:result.json", "--check-name", "saved result",
		"--product-version-cmd", "demo --version", "--no-input", "--json")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, diagnostic)
	}
	sp, err := spec.Load(filepath.Join(dir, "trial.toml"), spec.LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	checks := util.Maps(sp["verify"])
	if len(checks) != 2 || checks[0]["name"] != `result "exists"` || checks[1]["name"] != "saved result" ||
		util.Get(sp, "trial", "product_version_cmd") != "demo --version" {
		t.Fatalf("wrong scaffold: %v", sp)
	}
	text, _ := os.ReadFile(filepath.Join(dir, "trial.toml"))
	if !strings.Contains(string(text), "type = \"shell\"\nrun = ") {
		t.Fatalf("shell check type must be explicit: %s", text)
	}
}

func TestInitRejectsAmbiguousCheckNames(t *testing.T) {
	offline(t)
	for _, args := range [][]string{
		{"--check-name", "unused"},
		{"--check", "file:one", "--check", "file:two", "--check-name", "only one"},
		{"--check", "file:one", "--check-name", "  "},
		{"--task", "  "},
	} {
		dir := filepath.Join(t.TempDir(), "invalid")
		code, out, diagnostic := run(t, append([]string{"init", dir, "--product", "demo", "--no-input", "--json"}, args...)...)
		if code != 2 {
			t.Fatalf("invalid arguments accepted: %v => %d %s %s", args, code, out, diagnostic)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("invalid arguments wrote files: %v", err)
		}
	}
}

func TestDoctorEndsWithExplicitVerdict(t *testing.T) {
	offline(t)
	for _, identity := range []string{`['/bin/sh','-c','echo synthetic-identity']`, `['/bin/sh','-c','exit 7']`} {
		path := headlessDoctorFixture(t, identity, "#!/bin/sh\necho synthetic-v1\n", "", "")
		code, text, diagnostic := run(t, "doctor", path)
		lines := strings.Split(strings.TrimSpace(text), "\n")
		last := lines[len(lines)-1]
		verdict := "READY:"
		if code != 0 {
			verdict = "NOT READY:"
		}
		if !strings.HasPrefix(last, verdict) {
			t.Fatalf("missing final verdict: %d %s %s", code, text, diagnostic)
		}
		code, _, doc, err := Doctor(path, true)
		if err != nil || doc["ready"] != (code == 0) {
			t.Fatalf("doctor API lacks matching readiness: %d %v %v", code, doc, err)
		}
	}
}

func TestDoctorIncludesAWSStartupSourceAndRemedy(t *testing.T) {
	home := offline(t)
	t.Setenv("ZDOTDIR", home)
	if err := os.WriteFile(filepath.Join(home, ".zshenv"), []byte("export AWS_PROFILE=synthetic-default\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := headlessDoctorFixture(t, `['/bin/sh','-c','echo synthetic-identity']`, "#!/bin/sh\necho synthetic-v1\n", "", "")
	text, _ := os.ReadFile(path)
	text = []byte(strings.Replace(string(text), "[task]\n", "[task]\nidentity_cmd='aws sts get-caller-identity --profile task'\n", 1))
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/sh\ncase \"$*\" in *--profile*) echo synthetic-task;; *) echo synthetic-default;; esac\n"
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "aws"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	code, out, diagnostic := run(t, "doctor", path, "--json")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, diagnostic)
	}
	cells := util.Maps(decode(t, out)["cells"])
	if len(cells) != 1 || !strings.Contains(util.S(cells[0]["aws_profile_source"]), ".zshenv line 1") {
		t.Fatalf("missing startup source: %v", cells)
	}
	warnings := strings.Join(util.Strings(cells[0]["warnings"]), "\n")
	if !strings.Contains(warnings, ".zshenv line 1") || !strings.Contains(warnings, `shell_startup = "skip"`) {
		t.Fatalf("warning lacks source and remedy: %s", warnings)
	}
}

func TestBundledDocsAreDiscoverableAndReadableOffline(t *testing.T) {
	offline(t)
	_, help, _ := run(t, "--help")
	if !strings.Contains(help, "docs") || !strings.Contains(help, "https://ajx.dev/agents/") {
		t.Fatalf("help lacks working documentation entry points: %s", help)
	}
	code, out, diagnostic := run(t, "docs", "trial", "--json")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, diagnostic)
	}
	doc := decode(t, out)
	if doc["topic"] != "trial" || !strings.Contains(util.S(doc["content"]), "[[verify]]") {
		t.Fatalf("trial reference was not readable: %s", out)
	}
	code, text, _ := run(t, "docs", "checks")
	if code != 0 || !strings.Contains(text, "--check") {
		t.Fatalf("check reference was not readable: %d %s", code, text)
	}
	if code, _, _ := run(t, "docs", "../private"); code != 2 {
		t.Fatalf("unknown documentation topic accepted: %d", code)
	}
}
