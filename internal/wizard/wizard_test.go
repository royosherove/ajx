package wizard

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"

	_ "github.com/royosherove/ajx/internal/builtin"
)

// lineReader hands out one line per Read, like a terminal in line mode (the
// accessible form opens a new scanner for every prompt).
type lineReader struct{ lines []string }

func (r *lineReader) Read(p []byte) (int, error) {
	if len(r.lines) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.lines[0]+"\n")
	r.lines = r.lines[1:]
	return n, nil
}

func TestAccessibleWizardWritesATrialThatLoads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "/usr/bin:/bin")
	dir := filepath.Join(t.TempDir(), "orders")
	codex := 0
	for i, h := range Harnesses(false) {
		if h.Name == "codex" {
			codex = i + 1
		}
	}
	in := &lineReader{lines: []string{
		"orders-cli", // product
		dir,          // folder
		"Export the orders to orders.csv and check the row count.", // task
		"orders-cli --help",  // materials
		strconv.Itoa(codex),  // worker
		"",                   // model: harness default
		"",                   // reporter: default (the worker's)
		"",                   // reporter model: harness default
		"1",                  // check: shell
		"test -s orders.csv", // command
		"300",                // time limit
		"2",                  // attempts
		"",                   // fixture
		"1",                  // local
		"y",                  // confirm
		"3",                  // next: exit
	}}
	var out bytes.Buffer
	x := IO{In: in, Out: &out, Accessible: true}
	o := Options{}
	if err := Ask(&o, x); err != nil {
		t.Fatal(err)
	}
	if err := Resolve(&o); err != nil {
		t.Fatal(err)
	}
	ok, err := Confirm(o, x)
	if err != nil || !ok {
		t.Fatalf("confirm: %v %v\n%s", ok, err, out.String())
	}
	res, err := Write(o)
	if err != nil || !res.Valid {
		t.Fatalf("write: %v %+v", err, res)
	}
	step, err := NextStep(x)
	if err != nil || step != NextExit {
		t.Fatalf("next: %q %v", step, err)
	}
	sp, err := spec.Load(res.TrialFile, spec.LoadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	cell := util.Maps(sp["cells"])[0]
	if cell["harness"] != "codex" || util.Get(sp, "reporter", "harness") != "codex" {
		t.Fatalf("harness %v reporter %v\n%s", cell["harness"], util.Get(sp, "reporter", "harness"), out.String())
	}
	if util.Int(util.Get(sp, "trial", "timeout_seconds"), 0) != 300 || util.Int(util.Get(sp, "trial", "repetitions"), 0) != 2 {
		t.Fatalf("limits %v", sp["trial"])
	}
	if v := util.Maps(sp["verify"]); len(v) != 1 || v[0]["run"] != "test -s orders.csv" {
		t.Fatalf("verify %v", v)
	}
	prompt, _ := os.ReadFile(res.PromptFile)
	if !strings.Contains(string(prompt), "Export the orders") || !strings.Contains(string(prompt), "- orders-cli --help") {
		t.Fatalf("prompt %s", prompt)
	}
	if !strings.Contains(out.String(), "codex") || !strings.Contains(Summary(o, false), "test -s orders.csv") {
		t.Fatalf("summary/output:\n%s", out.String())
	}
}

func TestScaffoldSavesModelsForEachSelectedHarness(t *testing.T) {
	for _, tc := range []struct{ harness, model, want string }{
		{"kiro-cli", "opus", "claude-opus-5.5"},
		{"codex", "sol", "gpt-6.1-sol"},
		{"claude-code", "opus", "opus"},
	} {
		o := Options{Dir: filepath.Join(t.TempDir(), "trial"), Product: "orders-cli",
			Harness: tc.harness, Model: tc.model, Reporter: "codex", ReporterModel: "astra"}
		if err := Resolve(&o); err != nil {
			t.Fatal(err)
		}
		result, err := Write(o)
		if err != nil {
			t.Fatal(err)
		}
		sp, err := spec.Load(result.TrialFile, spec.LoadOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if got := util.Maps(sp["cells"])[0]["model"]; got != tc.want {
			t.Fatalf("%s worker model: got %v, want %s", tc.harness, got, tc.want)
		}
		if got := util.Get(sp, "reporter", "model"); got != "gpt-6-astra" {
			t.Fatalf("reporter model: %v", got)
		}
	}
	o := Options{Product: "orders-cli", Harness: "codex", Model: "opus"}
	if err := Resolve(&o); err == nil || !strings.Contains(err.Error(), "not a model alias for codex") {
		t.Fatalf("cross-harness alias: %v", err)
	}
}

func TestRenderWithoutExtrasIsThePythonScaffold(t *testing.T) {
	o := Options{Product: "jq", Harness: "codex"}
	if err := Resolve(&o); err != nil {
		t.Fatal(err)
	}
	trial, prompt, err := Render(o, "/x/My Trial")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.NewReplacer("{id}", "my-trial", "{product}", "jq", "{harness}", "codex", "{reporter}", "codex").Replace(trialTemplate)
	if trial != want {
		t.Fatalf("trial differs")
	}
	if !strings.HasPrefix(prompt, "I want to try jq.\n\n<Describe the real outcome") {
		t.Fatalf("prompt %q", prompt)
	}
}

func TestParseCheck(t *testing.T) {
	for raw, want := range map[string]string{
		"test -f x":                 "shell test -f x",
		"shell:echo a:b":            "shell echo a:b",
		"file:out/*.csv":            "file_exists out/*.csv",
		"http://127.0.0.1:8000/":    "http http://127.0.0.1:8000/",
		"http:https://example.com/": "http https://example.com/",
	} {
		c, err := ParseCheck(raw)
		if err != nil || c.Type+" "+c.Value != want {
			t.Errorf("%q: %+v %v", raw, c, err)
		}
	}
	for _, bad := range []string{"", "file:/etc/passwd", "file:../x", "shell:"} {
		if _, err := ParseCheck(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// What a user typed into the wizard once: a sentence as the product, a model
// with a space, two links on one line and a check that always passes.
func TestWizardInputsThatOnceProducedATrialThatDidNotLoad(t *testing.T) {
	product := "Vercel CLI and Supabase CLI - Set up a TODO app that allows creating and listing todo items. "
	id := Slug(product)
	if !trialSlug.MatchString(id) || len(id) > 48 || strings.HasSuffix(id, ".") {
		t.Fatalf("slug %q is not a valid trial id", id)
	}
	if got := TrialID("trials/" + strings.TrimSpace(product)); !trialSlug.MatchString(got) {
		t.Fatalf("TrialID %q is not a valid trial id", got)
	}
	if TrialID("trials/my-cli") != "my-cli" || TrialID("trials/My CLI") != "my-cli" {
		t.Fatal("valid names must keep the Python CLI's id")
	}
	if err := ModelName("sonnet 5.5"); err == nil || !strings.Contains(err.Error(), "not a model name") {
		t.Fatalf("model with a space accepted: %v", err)
	}
	if err := ModelName("us.anthropic.claude-sonnet-4-5"); err != nil {
		t.Fatal(err)
	}
	got := lines("https://vercel.com/docs/cli https://supabase.com/docs/guides/cli\nthe README at https://example.com/x")
	if len(got) != 3 || got[0] != "https://vercel.com/docs/cli" || got[2] != "the README at https://example.com/x" {
		t.Fatalf("materials %q", got)
	}
	for cmd, trivial := range map[string]bool{`echo "hello"`: true, "true": true, "test -s out.csv": false,
		`echo ok | grep ok`: false, `jq . out.json`: false} {
		if TrivialCheck(cmd) != trivial {
			t.Errorf("TrivialCheck(%q) = %v", cmd, !trivial)
		}
	}
	if err := productName(product); err == nil {
		t.Fatal("a sentence was accepted as the product name")
	}
	if len(Warnings(Options{Checks: []string{`echo "hello"`}})) != 1 {
		t.Fatal("no warning for a check that always passes")
	}
}
