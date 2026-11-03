package builtin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

const fixtures = "../../skills/ajx/tests/fixtures"

// T are synthetic arrival timestamps.
var T = func() []string {
	out := make([]string, 20)
	for i := range out {
		out[i] = fmt.Sprintf("2026-01-01T00:00:%02d.000Z", i)
	}
	return out
}()

func testCtx(t *testing.T, tmp string, cell M, state M, configDir string) *plugin.Ctx {
	t.Helper()
	runDir := filepath.Join(tmp, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := plugin.NewRunner("local", nil)
	if err != nil {
		t.Fatal(err)
	}
	return &plugin.Ctx{Spec: M{"trial": M{"max_budget_usd": nil}}, Cell: cell, RunDir: runDir, Workspace: tmp,
		ConfigDir: configDir, State: state, Env: map[string]string{}, Timeout: 10, PromptText: "x", Runner: r}
}

func harness(t *testing.T, name string) plugin.Harness {
	t.Helper()
	h, err := plugin.NewHarness(name)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// writeRaw writes a run_streaming capture: one {"at", "line"} record per
// event (plus an optional unterminated tail, as while the worker is still
// writing).
func writeRaw(t *testing.T, path string, events []any, trailing string) {
	t.Helper()
	var lines []string
	for i, ev := range events {
		lines = append(lines, util.Compact(M{"at": T[i%len(T)], "line": newScope().dumps(ev, true)}))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"+trailing), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeRawLines wraps fixture lines verbatim.
func writeRawLines(t *testing.T, path string, lines []string) {
	t.Helper()
	var out []string
	for i, l := range lines {
		out = append(out, util.Compact(M{"at": T[i%len(T)], "line": l}))
	}
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixtureLines(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func writeTranscript(t *testing.T, base string, rows []any, sid string) string {
	t.Helper()
	proj := filepath.Join(base, "projects", "-synthetic-workspace")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, r := range rows {
		lines = append(lines, newScope().dumps(r, true))
	}
	path := filepath.Join(proj, sid+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustNormalize(t *testing.T, h plugin.Harness, ctx *plugin.Ctx, stage string, exclude []string) M {
	t.Helper()
	tel, err := h.Normalize(ctx, stage, exclude)
	if err != nil {
		t.Fatal(err)
	}
	return tel
}

func eq(t *testing.T, got, want any, what string) {
	t.Helper()
	if util.Compact(got) != util.Compact(want) {
		t.Fatalf("%s: got %s, want %s", what, util.Compact(got), util.Compact(want))
	}
}

func strs(v any) []string { return util.Strings(v) }

func containsAny(items []string, sub string) []string {
	var out []string
	for _, s := range items {
		if strings.Contains(s, sub) {
			out = append(out, s)
		}
	}
	return out
}

func num(v any) float64 {
	f, _ := util.Num(v)
	return f
}

// fakeRun replaces BaseHarness.Run: it records argv and writes events as the
// stage's raw stream.
type fakeRun struct {
	argvs [][]string
	opts  []plugin.RunOpts
	write func(ctx *plugin.Ctx, argv []string, stage string)
}

func (f *fakeRun) install(t *testing.T) {
	t.Helper()
	RunHook = func(h plugin.Harness, ctx *plugin.Ctx, argv []string, stage string, o plugin.RunOpts) (M, error) {
		f.argvs = append(f.argvs, argv)
		f.opts = append(f.opts, o)
		if f.write != nil {
			f.write(ctx, argv, stage)
		}
		return M{"exit_code": int64(0), "timed_out": false}, nil
	}
	t.Cleanup(func() { RunHook = nil })
}
