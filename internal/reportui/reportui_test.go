package reportui

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/render/paritytest"
	"github.com/royosherove/ajx/internal/render/pyfmt"
	"github.com/royosherove/ajx/internal/util"
)

func TestAnnotationsAreCitedAndNarrativeTakesPrecedence(t *testing.T) {
	events := []M{{"eid": "E-001"}, {"eid": "E-002"}, {"eid": "E-003"}}
	markdown := "# J\n\n## Chronological account\n\n1. 🔀 Fork. I chose a path [E-001].\n\n2. I continued [E-002].\n"
	data := M{"asks": []any{M{"id": "ASK-001", "event_refs": []any{"E-001", "E-002", "E-999"}, "labels": []any{"Wait"}}},
		"strengths": []any{M{"event_refs": []any{"E-003"}}}}
	marks := Annotations(markdown, data, events)
	check := func(id string, want ...string) {
		if got := marks[id].Labels; strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: %v, want %v", id, got, want)
		}
	}
	check("E-001", "Fork")
	check("E-002", "Wait")
	check("E-003", "Delight")
	if marks["E-999"] != nil {
		t.Error("E-999 annotated")
	}
}

func TestUntrustedContentAndMissingEvidenceStaySafeAndExplicit(t *testing.T) {
	attack := `<script>alert("fake")</script>`
	ask := M{"id": "ASK-001", "title": attack, "event_refs": []any{`E-001" onclick="x`}, "measured": M{"span_seconds": nil}}
	rendered := AskCard(ask, 0, "", false)
	if strings.Contains(rendered, "<script>") || strings.Contains(rendered, `onclick="x"`) {
		t.Fatal(rendered)
	}
	if !strings.Contains(rendered, "&lt;script&gt;") || !strings.Contains(rendered, "Unavailable") {
		t.Fatal(rendered)
	}
	if Number(0.003) != "0.003" {
		t.Fatal(Number(0.003))
	}
	if !strings.Contains(EmptyRegister(M{"extraction_status": "failed"}), "incomplete") {
		t.Fatal("empty register")
	}
}

func TestPageIsOfflineAndMarksExamples(t *testing.T) {
	page := Page("T <x>", "<p>body</p>", "sub", "report", true)
	if !strings.Contains(page, "Fictional example.") || !strings.Contains(page, "Synthetic example") || !strings.Contains(page, "T &lt;x&gt;") {
		t.Fatal("example page")
	}
	if regexp.MustCompile(`<(?:script|link)[^>]+(?:src|href)="https?://`).MatchString(page) {
		t.Fatal("network reference")
	}
	matrix := Page("T", "", "", "matrix", false)
	if !strings.Contains(matrix, `<a class="brand" href="index.html">`) || !strings.Contains(matrix, "Generated ") {
		t.Fatal("matrix page")
	}
}

func TestCleanupProblem(t *testing.T) {
	for _, c := range []any{M{"status": "failed"}, M{"status": "ok", "confirmed": false}, M{"failed": int64(2)},
		M{"failed": true}, M{"errors": []any{"x"}}, "timed_out"} {
		if !CleanupProblem(c) {
			t.Errorf("%v should be a problem", c)
		}
	}
	for _, c := range []any{nil, M{"status": "released", "confirmed": true}, M{"failed": int64(0)}, "already_absent"} {
		if CleanupProblem(c) {
			t.Errorf("%v should not be a problem", c)
		}
	}
}

// TestFormattingParityWithPython compares number/duration/esc/value helpers
// with report_ui on the same values.
func TestFormattingParityWithPython(t *testing.T) {
	paritytest.Python(t)
	values := `[null, 0, 1, 59, 60, 61, 3599, 3600, 1234567, -5, 0.003, 0.0001234, 1e-05, 2.0, 59.9996, 60.0, 125.5, 1234.5678,
		1234567.0, 123456789.123, 1e16, 1.5e300, true, false, "x", "", "<a & 'b' \"c\">", [1, "a", null], {"k": [true, 2.5]}]`
	out := paritytest.Run(t, ".", "-c", `import json, sys; sys.path.insert(0, sys.argv[1])
from ajx import report_ui as r
vals = json.loads(sys.argv[2])
def safe(f, v):
    try:
        return f(v)
    except Exception as exc:
        return "ERR " + type(exc).__name__
print(json.dumps([[safe(r.number, v), safe(r.duration, v), r.esc(v), r._value(v), r.status(v) if not isinstance(v, (list, dict)) else ""] for v in vals]))`,
		filepath.Join(paritytest.Root(), "skills", "ajx", "lib"), values)
	var want [][]string
	if err := json.Unmarshal([]byte(out), &want); err != nil {
		t.Fatal(err)
	}
	vals, err := pyfmt.Decode([]byte(values))
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range util.List(vals) {
		got := []string{Number(v), Duration(v), Esc(v), Value(v), ""}
		switch v.(type) {
		case []any, M:
		default:
			got[4] = Status(v)
		}
		for j := range got {
			if strings.HasPrefix(want[i][j], "ERR ") {
				continue // Python raises for this input; Go degrades
			}
			if got[j] != want[i][j] {
				t.Errorf("value %v helper %d: go %q python %q", v, j, got[j], want[i][j])
			}
		}
	}
}
