package pyfmt

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/util"
)

func TestDecodeKeepsIntAndFloatApart(t *testing.T) {
	v, err := Decode([]byte(`{"a": 1, "b": 1.0, "c": 1e3, "d": 123456789012345678901234567890, "e": [2, 2.5]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := v.(util.M)
	if _, ok := m["a"].(int64); !ok {
		t.Errorf("a %T", m["a"])
	}
	for _, k := range []string{"b", "c", "d"} {
		if _, ok := m[k].(float64); !ok {
			t.Errorf("%s %T", k, m[k])
		}
	}
	if got := Dumps(v, DumpOpts{}); got != `{"a": 1, "b": 1.0, "c": 1000.0, "d": 1.2345678901234568e+29, "e": [2, 2.5]}` {
		t.Error(got)
	}
	if _, err := Decode([]byte(`{} x`)); err == nil {
		t.Error("extra data accepted")
	}
}

func TestScopeKeepsInsertionOrder(t *testing.T) {
	sc := NewScope()
	v, err := sc.DecodeOrdered([]byte(`{"z": 1, "a": {"y": 1, "b": 2}, "m": 3}`))
	if err != nil {
		t.Fatal(err)
	}
	m := v.(util.M)
	if got := strings.Join(Keys(m), ","); got != "z,a,m" {
		t.Error(got)
	}
	m["c"] = 1
	delete(m, "a")
	if got := strings.Join(Keys(m), ","); got != "z,m,c" {
		t.Error(got)
	}
	if got := Dumps(v, DumpOpts{Ordered: true}); got != `{"z": 1, "m": 3, "c": 1}` {
		t.Error(got)
	}
	sc.Close()
	if got := strings.Join(Keys(m), ","); got != "c,m,z" {
		t.Error("closed scope still ordering:", got)
	}
}

func TestPaths(t *testing.T) {
	for in, want := range map[string]string{"/a/b/": "/a", "/a//b": "/a", "a": ".", "/": "/", "/a": "/", "a/b/./c": "a/b"} {
		if got := Parent(in); got != want {
			t.Errorf("Parent(%q) = %q, want %q", in, got, want)
		}
	}
	if CleanPath("/a//b/./c/") != "/a/b/c" || CleanPath("") != "." {
		t.Error("CleanPath")
	}
}

// TestParityWithPython compares the formatting helpers with CPython.
func TestParityWithPython(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	floats := []float64{0, 1, -1, 0.1, 0.25, 0.35, 2.675, 1e-05, 0.0001, 123456.5, 1234567.5, 1e15, 1e16, 1.5e16,
		1234567890123456.7, 3.14159265358979, 47.0, 1e300, -0.0, 59.95, 2.5, 3.5}
	strs := []string{"plain", "it's", `say "hi"`, `both ' and "`, "tab\tnl\n", "\x00\x7f\u0085\u00a0\u200b", "\u00e9\U0001F600", `back\slash`}
	payload, _ := json.Marshal(map[string]any{"floats": floats, "strs": strs})
	cmd := exec.Command(py, "-I", "-c", `import json, sys, urllib.parse
d = json.loads(sys.argv[1])
fl = [float(x) for x in d["floats"]]
print(json.dumps({
  "repr": [repr(x) for x in fl],
  "g6": [format(x, ",.6g") for x in fl],
  "g": [format(x, "g") for x in fl],
  "round1": [repr(round(x, 1)) for x in fl],
  "srepr": [repr(s) for s in d["strs"]],
  "dumps": json.dumps({"s": d["strs"], "f": fl[:8]}, sort_keys=True),
  "dumps_raw": json.dumps({"s": d["strs"]}, ensure_ascii=False, sort_keys=True),
  "dumps_ascii": json.dumps({"s": d["strs"]}, ensure_ascii=True, sort_keys=True),
  "indent": json.dumps({"a": [], "b": {}, "c": [1, {"d": None}]}, indent=2, sort_keys=True),
  "quote": [urllib.parse.quote(s, safe="") for s in d["strs"]],
}))`, string(payload))
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var want struct {
		Repr, G6, G, Round1, Srepr, Quote []string
		Dumps, DumpsASCII, Indent         string
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(out, &raw)
	_ = json.Unmarshal(raw["repr"], &want.Repr)
	_ = json.Unmarshal(raw["g6"], &want.G6)
	_ = json.Unmarshal(raw["g"], &want.G)
	_ = json.Unmarshal(raw["round1"], &want.Round1)
	_ = json.Unmarshal(raw["srepr"], &want.Srepr)
	_ = json.Unmarshal(raw["quote"], &want.Quote)
	_ = json.Unmarshal(raw["dumps"], &want.Dumps)
	_ = json.Unmarshal(raw["dumps_ascii"], &want.DumpsASCII)
	_ = json.Unmarshal(raw["indent"], &want.Indent)
	var dumpsRaw string
	_ = json.Unmarshal(raw["dumps_raw"], &dumpsRaw)
	if got := Dumps(util.M{"s": util.List(strs)}, DumpOpts{}); got != dumpsRaw {
		t.Errorf("dumps ensure_ascii=False:\n%s\n%s", got, dumpsRaw)
	}
	for i, f := range floats {
		if got := FloatRepr(f); got != want.Repr[i] {
			t.Errorf("repr(%v): %q want %q", f, got, want.Repr[i])
		}
		if got := FormatG(f, 6, true); got != want.G6[i] {
			t.Errorf("format(%v, ',.6g'): %q want %q", f, got, want.G6[i])
		}
		if got := FormatG(f, 6, false); got != want.G[i] {
			t.Errorf("format(%v, 'g'): %q want %q", f, got, want.G[i])
		}
		if got := FloatRepr(Round(f, 1)); got != want.Round1[i] {
			t.Errorf("round(%v, 1): %q want %q", f, got, want.Round1[i])
		}
	}
	for i, s := range strs {
		if got := StrRepr(s); got != want.Srepr[i] {
			t.Errorf("repr(%q): %q want %q", s, got, want.Srepr[i])
		}
		if got := Quote(s, ""); got != want.Quote[i] {
			t.Errorf("quote(%q): %q want %q", s, got, want.Quote[i])
		}
	}
	fl := make([]any, 8)
	for i := range fl {
		fl[i] = floats[i]
	}
	if got := Dumps(util.M{"s": util.List(strs), "f": fl}, DumpOpts{ASCII: true}); got != want.Dumps {
		t.Errorf("dumps:\n%s\n%s", got, want.Dumps)
	}
	if got := Dumps(util.M{"s": util.List(strs)}, DumpOpts{ASCII: true}); got != want.DumpsASCII {
		t.Errorf("dumps ascii:\n%s\n%s", got, want.DumpsASCII)
	}
	if got := Dumps(util.M{"a": []any{}, "b": util.M{}, "c": []any{int64(1), util.M{"d": nil}}}, DumpOpts{Indent: 2}); got != want.Indent {
		t.Errorf("indent:\n%s\n%s", got, want.Indent)
	}
}
