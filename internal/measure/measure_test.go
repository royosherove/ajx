package measure

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/royosherove/ajx/internal/util"
)

func canon(t *testing.T, v any) any {
	t.Helper()
	b, err := util.MarshalIndent(v) // Python number spelling: 60.0 stays a float
	if err != nil {
		t.Fatal(err)
	}
	out, err := util.DecodeJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestPythonCases replays manifests recorded from measure.py (synthetic)
// and compares results and error messages.
func TestPythonCases(t *testing.T) {
	data, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	cases, err := decodeNumbers(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range cases.([]any) {
		c := raw.(util.M)
		t.Run(c["name"].(string), func(t *testing.T) {
			res, err := Measure(c["manifest"], util.List(c["events"]))
			if want := c["error"]; want != nil {
				if err == nil || err.Error() != want {
					t.Fatalf("error = %v, want %q", err, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if g, w := canon(t, res), canon(t, c["result"]); !reflect.DeepEqual(g, w) {
				t.Fatalf("result differs\n got %v\nwant %v", g, w)
			}
		})
	}
}

func TestInProcessFloatCountsAreIntegers(t *testing.T) {
	man := util.M{"sessions": []any{util.M{"id": "s", "start": "2026-01-01T00:00:00Z", "end": "2026-01-01T00:00:10Z",
		"phases":       []util.M{{"id": "s", "start": "2026-01-01T00:00:00Z", "end": "2026-01-01T00:00:10Z", "scope": "task"}},
		"usage_status": "complete", "expected_output_tokens": float64(3), "tool_status": "unavailable"}}}
	ev := []any{util.M{"id": "u", "session_id": "s", "at": "2026-01-01T00:00:01Z", "kind": "usage", "output_tokens": float64(3)}}
	res, err := Measure(man, ev)
	if err != nil {
		t.Fatal(err)
	}
	s := util.Maps(res["sessions"])[0]
	if s["output_tokens"] != int64(3) || s["output_tokens_reconciled"] != true || s["tool_calls"] != nil {
		t.Fatalf("session %v", s)
	}
}

func TestParseISO(t *testing.T) {
	for in, want := range map[string]string{
		"2024-01-01T00:00:00Z":           "2024-01-01T00:00:00+00:00",
		"2024-01-01T00:00:00.1Z":         "2024-01-01T00:00:00.100000+00:00",
		"2024-01-01T00:00:00.1234567Z":   "2024-01-01T00:00:00.123456+00:00",
		"2024-01-01 00:00:00+0200":       "2024-01-01T00:00:00+02:00",
		"2024-01-01T00:00Z":              "2024-01-01T00:00:00+00:00",
		"2024-01-01":                     "2024-01-01T00:00:00",
		"20240101T000000Z":               "2024-01-01T00:00:00+00:00",
		"2024-01-01T00:00:00.12+05:30":   "2024-01-01T00:00:00.120000+05:30",
		"2024-01-01T00:00:00,5Z":         "2024-01-01T00:00:00.500000+00:00",
		"2024-01-01T24:00:00Z":           "2024-01-02T00:00:00+00:00",
		"2024-01-01T05Z":                 "2024-01-01T05:00:00+00:00",
		"2024-01-01T00:00:00-00:00":      "2024-01-01T00:00:00+00:00",
		"2024-01-01T00:00:00+01:30:15.5": "2024-01-01T00:00:00+01:30:15",
		"2024-01-01T000000.5":            "2024-01-01T00:00:00.500000",
	} {
		tm, aware, err := ParseISO(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got := ISOFormat(tm, aware); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
	for _, in := range []string{"2024-01-01T5:00:00Z", "2024-01-01T05:0Z", "2024-1-01T00:00:00Z", "2024-01-01T00:00:00.Z",
		"2024-01-01T00:00:00+2:00", "2024-01-01T", "2024-01-01T00:00:00 ", " 2024-01-01T00:00:00"} {
		if _, _, err := ParseISO(in); err == nil || !strings.HasPrefix(err.Error(), "Invalid isoformat string: ") {
			t.Errorf("%q: err %v", in, err)
		}
	}
	tm, _, _ := ParseISO("2024-01-01T00:00:00.5Z")
	if !tm.Equal(time.Date(2024, 1, 1, 0, 0, 0, 5e8, time.UTC)) {
		t.Fatal(tm)
	}
}

func TestMainWritesAndRefusesToOverwriteInputs(t *testing.T) {
	tmp := t.TempDir()
	man := filepath.Join(tmp, "m.json")
	evs := filepath.Join(tmp, "e.jsonl")
	_ = os.WriteFile(man, []byte(`{"sessions": [{"id": "s", "start": "2026-01-01T00:00:00Z", "end": "2026-01-01T00:00:10Z",
	 "phases": [{"id": "s", "start": "2026-01-01T00:00:00Z", "end": "2026-01-01T00:00:10Z", "scope": "task"}],
	 "usage_status": "complete", "expected_output_tokens": 2, "tool_status": "unavailable"}]}`), 0o644)
	_ = os.WriteFile(evs, []byte("{\"id\": \"u\", \"session_id\": \"s\", \"at\": \"2026-01-01T00:00:01Z\", \"kind\": \"usage\", \"output_tokens\": 2}\n\n"), 0o644)
	var out, errb bytes.Buffer
	if code := Main([]string{man, evs}, &out, &errb); code != 0 {
		t.Fatalf("code %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), `"output_tokens_reconciled": true`) {
		t.Fatal(out.String())
	}
	out.Reset()
	errb.Reset()
	if code := Main([]string{man, evs, "--output", man}, &out, &errb); code != 1 ||
		errb.String() != "AJX measurement failed: Output must not overwrite an input\n" {
		t.Fatalf("code %d: %q", code, errb.String())
	}
	dest := filepath.Join(tmp, "out.json")
	if code := Main([]string{"--output=" + dest, man, evs}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(evs, []byte("{\"id\": \"u\"\n"), 0o644)
	errb.Reset()
	if code := Main([]string{man, evs}, &out, &errb); code != 1 || errb.String() != "AJX measurement failed: e.jsonl:1: invalid JSON\n" {
		t.Fatalf("code %d: %q", code, errb.String())
	}
	// Python json.loads keeps 2.0 a float: not a count
	_ = os.WriteFile(evs, []byte("{\"id\": \"u\", \"session_id\": \"s\", \"at\": \"2026-01-01T00:00:01Z\", \"kind\": \"usage\", \"output_tokens\": 2.0}\n"), 0o644)
	errb.Reset()
	if code := Main([]string{man, evs}, &out, &errb); code != 1 || errb.String() != "AJX measurement failed: s/u must be a nonnegative integer\n" {
		t.Fatalf("code %d: %q", code, errb.String())
	}
	errb.Reset()
	if code := Main([]string{filepath.Join(tmp, "missing.json"), evs}, &out, &errb); code != 1 ||
		!strings.Contains(errb.String(), "No such file or directory") {
		t.Fatalf("code %d: %q", code, errb.String())
	}
	if code := Main([]string{man}, &out, &errb); code != 2 {
		t.Fatalf("code %d", code)
	}
}
