package reportui

import (
	"reflect"
	"testing"
)

func TestChronologicalStepsPreserveLinesAndSectionBoundaries(t *testing.T) {
	markdown := "# Journey\n1. outside\n## Chronological account\r\nintro\r\n" +
		"  １.\u00a0**Wait**: delayed E-００１\r\n\tcontinued  text\r\n\r\n" +
		"2) Fork. chose E-002\r\n## Other\r\n3. ignored E-003\r\n" +
		"## CHRONOLOGY again\n4. finished E-004"
	want := [][]string{
		{"  １.\u00a0**Wait**: delayed E-００１", "\tcontinued  text", ""},
		{"2) Fork. chose E-002"},
		{"4. finished E-004"},
	}
	if got := ChronologicalSteps(markdown); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	if got := StepContent(want[0][0]); got != "Wait**: delayed E-００１" {
		t.Fatalf("step prefix changed: %q", got)
	}
	if got := ChronologicalSteps("## Other\n1. outside"); len(got) != 0 {
		t.Fatalf("steps outside chronology included: %#v", got)
	}
}

func TestJourneySyntaxUsesUnicodeBoundaries(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"Wait", true},
		{"⏳\u00a0**Wait**: delayed", true},
		{"Wait—delayed", true},
		{"Waiting", false},
		{"Waité", false},
		{"Wait_1", false},
		{"Wait１", false},
	} {
		if got := StartsWithLabel(tc.text, "⏳", "Wait"); got != tc.want {
			t.Errorf("label in %q = %v, want %v", tc.text, got, tc.want)
		}
	}
	text := "E-001 E-12 E-002x éE-003 E-004é _E-005 E-006_ (E-００７) E-001"
	want := []string{"E-001", "E-００７", "E-001"}
	if got := EventReferences(text); !reflect.DeepEqual(got, want) {
		t.Fatalf("references = %#v, want %#v", got, want)
	}
}
