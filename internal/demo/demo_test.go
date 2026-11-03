package demo

// Ports PublicReportViewTest from skills/ajx/tests/test_ajx.py and compares
// the generated example with the Python generator. No agent is involved.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/render/paritytest"
	"github.com/royosherove/ajx/internal/render/pyfmt"
	"github.com/royosherove/ajx/internal/reportui"
	"github.com/royosherove/ajx/internal/util"
)

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestExampleGeneratesCompleteLinkedViewsWithoutAnAgent(t *testing.T) {
	out, err := Generate(filepath.Join(t.TempDir(), "example"))
	if err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(out, "runs", "baseline-codex-r1")
	report := read(t, filepath.Join(run, "report.html"))
	journey := read(t, filepath.Join(run, "pretty-journey.html"))
	matrix := read(t, filepath.Join(out, "index.html"))
	if strings.Index(report, `id="asks"`) >= strings.Index(report, `id="verification"`) {
		t.Error("asks should precede verification")
	}
	for _, want := range []string{`id="ASK-001"`, `href="pretty-journey.html#E-003"`, "Fictional example."} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %s", want)
		}
	}
	for _, want := range []string{`id="E-003"`, "<svg", "Table of happenings"} {
		if !strings.Contains(journey, want) {
			t.Errorf("journey missing %s", want)
		}
	}
	for _, name := range reportui.LabelNames {
		if !strings.Contains(journey, reportui.Labels[name][0]+" "+name) {
			t.Errorf("legend missing %s", name)
		}
	}
	for _, row := range util.Maps(pyfmt.ReadMap(filepath.Join(out, "matrix.json"))["runs"]) {
		if !strings.Contains(matrix, util.S(row["run_id"])) {
			t.Errorf("matrix missing %s", row["run_id"])
		}
		register := util.Maps(row["ask_register"])
		if util.Int(row["asks"], -1) != len(register) {
			t.Errorf("%s asks %v", row["run_id"], row["asks"])
		}
		for _, ask := range register {
			if !strings.Contains(matrix, util.S(ask["title"])) {
				t.Errorf("matrix missing ask %s", ask["title"])
			}
		}
	}
	if regexp.MustCompile(`<(?:script|link)[^>]+(?:src|href)="https?://`).MatchString(report + journey + matrix) {
		t.Error("network reference")
	}
	again, err := Generate(out) // refresh only its own example
	if err != nil || again != out {
		t.Fatalf("refresh: %v %s", err, again)
	}
}

func TestExampleRefusesAnExistingUnrelatedDirectory(t *testing.T) {
	tmp := t.TempDir()
	keep := filepath.Join(tmp, "keep.txt")
	os.WriteFile(keep, []byte("existing work"), 0o644)
	_, err := Generate(tmp)
	if err == nil || pyfmt.TypeName(err) != "ValueError" ||
		err.Error() != "Choose an empty directory; the example generator will not overwrite existing work." {
		t.Fatalf("err %v", err)
	}
	if read(t, keep) != "existing work" {
		t.Fatal("existing work changed")
	}
}

// TestExampleParityWithPython generates the fictional example with the
// Python CLI and with Generate and compares every file. Text files must be
// identical (the example carries no timestamps or revisions); JSON files are
// compared with sorted keys (Go writes sorted keys) and exact number spelling.
func TestExampleParityWithPython(t *testing.T) {
	paritytest.Python(t)
	tmp := t.TempDir()
	py, goDir := filepath.Join(tmp, "py"), filepath.Join(tmp, "go")
	paritytest.Run(t, tmp, filepath.Join(paritytest.Root(), "skills", "ajx", "bin", "ajx"), "example", py)
	if _, err := Generate(goDir); err != nil {
		t.Fatal(err)
	}
	paritytest.CompareTrees(t, py, goDir)
}
