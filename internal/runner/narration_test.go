package runner

import (
	"strings"
	"testing"
	"time"

	"github.com/royosherove/ajx/internal/util"
)

// Which self-narrations can stand as the journey (mirrors
// skills/ajx/tests/test_narration.py). A safeguard can stop the account
// partway and the harness's continuation can be a refusal; a reply without
// the requested `# Journey` document is not an account either.
var narrationAccount = "# Journey: convert orders\n\n## Outcome I declared\nI wrote orders.csv [E-002].\n" +
	strings.Repeat("detail ", 40)

func TestNarrationACompleteAccountIsAccepted(t *testing.T) {
	if got := NarrationRejection(narrationAccount, M{"harness_reported": M{}}); got != "" {
		t.Fatalf("rejected: %q", got)
	}
}

func TestNarrationASafeguardStopRejectsEvenALongText(t *testing.T) {
	tel := M{"harness_reported": M{"safety_stops": int64(1)}}
	if got := NarrationRejection(narrationAccount, tel); got != "safety_stop" {
		t.Fatalf("got %q", got)
	}
}

func TestNarrationARefusalWithoutTheHeadingIsRejected(t *testing.T) {
	refusal := strings.Repeat("Understood. I won't write that account again, in any wording. ", 5)
	if got := NarrationRejection(refusal, M{}); got != "no_journey_heading" {
		t.Fatalf("got %q", got)
	}
}

func TestNarrationShortOrEmptyTextIsRejected(t *testing.T) {
	for _, text := range []string{"", "# Journey: x\nshort"} {
		if got := NarrationRejection(text, M{}); got != "empty_or_short" {
			t.Fatalf("%q: got %q", text, got)
		}
	}
}

func TestNarrationAPreambleBeforeTheHeadingIsDropped(t *testing.T) {
	text := "Here is my account.\n\n" + narrationAccount
	if got := NarrationRejection(text, M{}); got != "" {
		t.Fatalf("rejected: %q", got)
	}
	if !strings.HasPrefix(JourneyText(text), "# Journey: convert orders") {
		t.Fatal(JourneyText(text)[:40])
	}
}

func TestNarrationEveryRejectionHasALogReason(t *testing.T) {
	for _, k := range []string{"safety_stop", "no_journey_heading", "empty_or_short"} {
		if NarrationRejected[k] == "" {
			t.Fatal(k)
		}
	}
	if len(NarrationRejected) != 3 {
		t.Fatal(NarrationRejected)
	}
}

func TestTaskInputsRecordChecksCommandsAndTaskConditions(t *testing.T) {
	sp := M{"path": "/t/trial.toml", "verify": []any{M{"name": "v"}}, "setup": []any{}, "preflight": []any{}, "teardown": []any{M{"run": "true"}},
		"task": M{"materials": []any{"jq --help"}, "fixture_dir": "/t/fixture", "human": "unavailable", "identity_cmd": nil, "prompt_text": "x"}}
	got := TaskInputs(sp)
	if len(got) != 8 || got["fixture_dir"] != "fixture" || got["prompt_text"] != nil {
		t.Fatalf("%v", got)
	}
	if _, ok := got["identity_cmd"]; !ok {
		t.Fatal("identity_cmd must be recorded even when unset")
	}
}

func TestTaskInputsRecordTOMLDatesAsPythonText(t *testing.T) {
	at := func(zone string, offset int) time.Time {
		return time.Date(2026, 1, 2, 3, 4, 5, 600000000, time.FixedZone(zone, offset))
	}
	sp := M{"path": "/t/trial.toml", "verify": []any{}, "preflight": []any{}, "teardown": []any{},
		"setup": []any{M{"run": "true", "when": time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("date-local", 0)),
			"at": []any{at("time-local", 0), at("datetime-local", 0), at("", 19800), at("UTC", 0)}}},
		"task": M{"materials": []any{}, "human": "unavailable"}}
	setup := TaskInputs(sp)["setup"].([]any)[0].(M)
	want := []any{"03:04:05.600000", "2026-01-02 03:04:05.600000", "2026-01-02 03:04:05.600000+05:30", "2026-01-02 03:04:05.600000+00:00"}
	if setup["when"] != "2026-01-01" || util.PyJSON(setup["at"]) != util.PyJSON(want) {
		t.Fatalf("%v", setup)
	}
}
