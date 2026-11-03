package mdlite

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/render/paritytest"
)

func TestRenderEscapesAndTables(t *testing.T) {
	html := Render("# T\n\n| a | b |\n|---|---|\n| 1 | <x> |\n\n- `c` **b** [l](http://x)\n\n```sh\necho <hi>\n```")
	for _, want := range []string{"<table>", "&lt;x&gt;", "<code>c</code>", `<a href="http://x">l</a>`, "echo &lt;hi&gt;"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in %s", want, html)
		}
	}
}

func TestNeutralizesJavascriptLinks(t *testing.T) {
	html := Render("[x](javascript:alert(1)) [y](data:text/html,zz)")
	if strings.Contains(html, "javascript:") || strings.Contains(html, "data:") || !strings.Contains(html, `href="#"`) {
		t.Fatal(html)
	}
	for _, url := range []string{"javascript:alert", "data:text/html,x", "vbscript:fake", "http://[bad"} {
		if got := Inline("[unsafe](" + url + ")"); !strings.Contains(got, `href="#"`) {
			t.Errorf("%s: %s", url, got)
		}
	}
}

func load(t *testing.T) ([]string, []map[string]string) {
	t.Helper()
	var cases []string
	var expected []map[string]string
	for name, v := range map[string]any{"cases.json": &cases, "expected.json": &expected} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, v); err != nil {
			t.Fatal(err)
		}
	}
	return cases, expected
}

// TestGoldenOutputFromPython checks outputs recorded from the Python mdlite.
func TestGoldenOutputFromPython(t *testing.T) {
	cases, expected := load(t)
	for i, c := range cases {
		if got := Render(c); got != expected[i]["render"] {
			t.Errorf("render %q:\n got %q\nwant %q", c, got, expected[i]["render"])
		}
		if got := Inline(c); got != expected[i]["inline"] {
			t.Errorf("inline %q:\n got %q\nwant %q", c, got, expected[i]["inline"])
		}
	}
}

// TestParityWithPython runs the live Python mdlite on the same cases.
func TestParityWithPython(t *testing.T) {
	paritytest.Python(t)
	cases, _ := load(t)
	extra := []string{"* a*b *c* d*", "x *y*z* *w*", "**", "***", "*a\n*", "[a](b)[c](javascript:d)",
		"`x` and `y`", "- [ ] task\n- [x] done", "1. a\n\n2. b", "> > nested\n> q", "|a|b|\n|-|-|\n|1|2|"}
	cases = append(cases, extra...)
	data, _ := json.Marshal(cases)
	in := filepath.Join(t.TempDir(), "cases.json")
	if err := os.WriteFile(in, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out := paritytest.Run(t, ".", "-c", `import json, sys; sys.path.insert(0, sys.argv[1])
from ajx import mdlite
cases = json.load(open(sys.argv[2], encoding="utf-8"))
print(json.dumps([{"render": mdlite.render(c), "inline": mdlite.inline(c)} for c in cases]))`,
		filepath.Join(paritytest.Root(), "skills", "ajx", "lib"), in)
	var expected []map[string]string
	if err := json.Unmarshal([]byte(out), &expected); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		if got := Render(c); got != expected[i]["render"] {
			t.Errorf("render %q:\n got %q\nwant %q", c, got, expected[i]["render"])
		}
		if got := Inline(c); got != expected[i]["inline"] {
			t.Errorf("inline %q:\n got %q\nwant %q", c, got, expected[i]["inline"])
		}
	}
}
