package export

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/render/paritytest"
	"github.com/royosherove/ajx/internal/render/pyfmt"
	"github.com/royosherove/ajx/internal/util"
)

// TestExportParityWithPython exports the same synthetic run with both
// implementations and compares the three files and the summary.
func TestExportParityWithPython(t *testing.T) {
	paritytest.Python(t)
	tmp := t.TempDir()
	l := layout(tmp)
	l.paths["cache_dir"] = filepath.Join(l.paths["home_dir"], "cache dir.v2") // punctuation: two encoded forms
	runDir := makeRunDir(t, filepath.Join(tmp, "out"), l, true)
	state := pyfmt.ReadMap(filepath.Join(runDir, "state.json"))
	state["archived"] = util.M{"workspace": filepath.Join(runDir, "workspace")}
	if err := pyfmt.WriteJSON(filepath.Join(runDir, "state.json"), state); err != nil {
		t.Fatal(err)
	}
	extra := "\n<p>" + l.paths["cache_dir"] + "/x " + strings.NewReplacer("/", "-", " ", "-", ".", "-").Replace(l.paths["cache_dir"]) +
		"-y " + l.root + "/abcdef12_x " + l.root + "-" + hexSibling + "-sub " + filepath.Join(runDir, "workspace") + "/f " +
		l.home + "é " + l.home + ".bak " + l.home + "-é</p>\n"
	for _, name := range []string{"report.html", "pretty-journey.html", "asks.md"} {
		data, _ := os.ReadFile(filepath.Join(runDir, name))
		writeFile(t, filepath.Join(runDir, name), string(data)+extra)
	}
	pyDest, goDest := filepath.Join(tmp, "py"), filepath.Join(tmp, "go")
	out := paritytest.Run(t, tmp, "-c", `import json, sys; sys.path.insert(0, sys.argv[1])
from ajx import export
s = export.export_run(sys.argv[2], sys.argv[3], home=sys.argv[4])
print(json.dumps({"files": s["files"], "order": list(s["replacements"]), "counts": s["replacements"], "warnings": s["warnings"]}))`,
		filepath.Join(paritytest.Root(), "skills", "ajx", "lib"), runDir, pyDest, l.home)
	var py struct {
		Files    []string       `json:"files"`
		Order    []string       `json:"order"`
		Counts   map[string]int `json:"counts"`
		Warnings []string       `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &py); err != nil {
		t.Fatal(err, out)
	}
	summary, err := ExportRun(runDir, goDest, ExportOpts{Home: l.home})
	if err != nil {
		t.Fatal(err)
	}
	if got := util.Strings(summary["replacement_order"]); strings.Join(got, "|") != strings.Join(py.Order, "|") {
		t.Errorf("order: go %v python %v", got, py.Order)
	}
	for k, n := range py.Counts {
		if util.Int(util.MapOr(summary["replacements"])[k], -1) != n {
			t.Errorf("count %s: go %v python %d", k, util.MapOr(summary["replacements"])[k], n)
		}
	}
	if strings.Join(util.Strings(summary["warnings"]), "|") != strings.Join(py.Warnings, "|") {
		t.Errorf("warnings: go %v python %v", summary["warnings"], py.Warnings)
	}
	for _, name := range py.Files {
		a, _ := os.ReadFile(filepath.Join(pyDest, name))
		b, _ := os.ReadFile(filepath.Join(goDest, name))
		if string(a) != string(b) {
			t.Errorf("%s differs:\n%s", name, paritytest.FirstDiff(string(a), string(b)))
		}
	}
}
