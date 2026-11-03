package render

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/render/paritytest"
	"github.com/royosherove/ajx/internal/render/pyfmt"
	"github.com/royosherove/ajx/internal/util"
)

func fakeHarness(name string, clean bool) plugin.Harness {
	return plugin.Bind(&plugin.BaseHarness{PluginName: name, CleanSupported: clean})
}

// renderFixture renders a parity fixture directory the way render_py.py does.
func renderFixture(t *testing.T, dir string, withClusters bool) {
	t.Helper()
	data := pyfmt.ReadMap(filepath.Join(dir, "cases.json"))
	sp := util.MapOr(data["spec"])
	util.MapOr(sp["trial"])["output_dir"] = dir
	for _, c := range util.Maps(data["cases"]) {
		rd := filepath.Join(dir, "runs", util.S(c["dir"]))
		state := pyfmt.ReadMap(filepath.Join(rd, "state.json"))
		h := fakeHarness(util.S(c["harness"]), util.Truthy(c["clean_supported"]))
		if _, err := WriteRunJSON(sp, util.MapOr(c["cell"]), util.Int(c["rep"], 1), rd, state, h, nil, nil); err != nil {
			t.Fatalf("%s: WriteRunJSON: %v", c["dir"], err)
		}
		if err := RunReport(rd); err != nil {
			t.Fatalf("%s: RunReport: %v", c["dir"], err)
		}
	}
	clustersV, _ := pyfmt.ReadJSON(filepath.Join(dir, "clusters.json"), nil)
	saved := synthesize
	synthesize = func(context.Context, M, string, []M, func(string)) ([]M, error) { return util.Maps(clustersV), nil }
	defer func() { synthesize = saved }()
	if err := MatrixReport(sp, MatrixOpts{Synthesize: withClusters, Log: func(string) {}}); err != nil {
		t.Fatalf("MatrixReport: %v", err)
	}
}

// TestRenderParityWithPython renders identical synthetic run directories with
// the Python and Go implementations and compares every output file.
func TestRenderParityWithPython(t *testing.T) {
	paritytest.Python(t)
	here, _ := os.Getwd()
	lib := filepath.Join(paritytest.Root(), "skills", "ajx", "lib")
	for _, withClusters := range []bool{false, true} {
		name := "plain"
		if withClusters {
			name = "clusters"
		}
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			fixture := filepath.Join(tmp, "fixture")
			paritytest.Run(t, here, filepath.Join(here, "testdata", "parity", "build.py"), fixture)
			py, goDir := filepath.Join(tmp, "py"), filepath.Join(tmp, "go")
			paritytest.CopyTree(t, fixture, py)
			paritytest.CopyTree(t, fixture, goDir)
			args := []string{filepath.Join(here, "testdata", "parity", "render_py.py"), lib, py}
			if withClusters {
				args = append(args, "--clusters")
			}
			paritytest.Run(t, here, args...)
			renderFixture(t, goDir, withClusters)
			paritytest.CompareTrees(t, py, goDir)
		})
	}
}

// TestExampleReportParityWithPython re-renders copies of docs/example-report
// with both implementations.
func TestExampleReportParityWithPython(t *testing.T) {
	paritytest.Python(t)
	tmp := t.TempDir()
	src := filepath.Join(paritytest.Root(), "docs", "example-report")
	py, goDir := filepath.Join(tmp, "py"), filepath.Join(tmp, "go")
	paritytest.CopyTree(t, src, py)
	paritytest.CopyTree(t, src, goDir)
	paritytest.Run(t, tmp, "-c", `import sys; sys.path.insert(0, sys.argv[1])
from pathlib import Path
from ajx import render
for d in sorted(Path(sys.argv[2]).iterdir()):
    render.run_report(d)`, filepath.Join(paritytest.Root(), "skills", "ajx", "lib"), filepath.Join(py, "runs"))
	entries, err := os.ReadDir(filepath.Join(goDir, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := RunReport(filepath.Join(goDir, "runs", e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	paritytest.CompareTrees(t, py, goDir)
}

// TestMatrixEdgeParityWithPython covers an empty trial and runs that only
// have a state.json (interrupted before any report was written).
func TestMatrixEdgeParityWithPython(t *testing.T) {
	paritytest.Python(t)
	lib := filepath.Join(paritytest.Root(), "skills", "ajx", "lib")
	for name, setup := range map[string]func(dir string){
		"empty": func(string) {},
		"state-only": func(dir string) {
			pyfmt.WriteJSON(filepath.Join(dir, "runs", "a-r1", "state.json"), M{"stages": M{"prepare": M{"status": "error", "error": "SpecError: bad"}}})
			pyfmt.WriteJSON(filepath.Join(dir, "runs", "a-r2", "state.json"), M{})
			pyfmt.WriteJSON(filepath.Join(dir, "runs", "b-r10", "state.json"), M{"stages": M{}})
			pyfmt.WriteJSON(filepath.Join(dir, "runs", "b-r9", "state.json"), M{"stages": M{}})
			pyfmt.WriteJSON(filepath.Join(dir, "runs", "b-r9", "asks.json"), M{"asks": []any{M{"id": "ASK-001", "title": "T"}}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			fixture := filepath.Join(tmp, "fixture")
			os.MkdirAll(fixture, 0o755)
			setup(fixture)
			py, goDir := filepath.Join(tmp, "py"), filepath.Join(tmp, "go")
			paritytest.CopyTree(t, fixture, py)
			paritytest.CopyTree(t, fixture, goDir)
			spec := func(dir string) M {
				return M{"trial": M{"id": "edge", "product": "Edge", "output_dir": dir, "repetitions": int64(3),
					"parallel": int64(1), "order_seed": int64(0)}, "task": M{"prompt_sha256": "c0ffee" + strings.Repeat("0", 58)}}
			}
			paritytest.Run(t, tmp, "-c", `import json, sys; sys.path.insert(0, sys.argv[1])
from ajx import render
render.matrix_report(json.loads(sys.argv[2]), synthesize=False, log=lambda m: None)`, lib, pyfmt.Dumps(spec(py), pyfmt.DumpOpts{}))
			if err := MatrixReport(spec(goDir), MatrixOpts{Log: func(string) {}}); err != nil {
				t.Fatal(err)
			}
			paritytest.CompareTrees(t, py, goDir)
		})
	}
}
