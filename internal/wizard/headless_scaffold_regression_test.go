package wizard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHeadlessInitRejectsInvalidOptionsBeforeWriting(t *testing.T) {
	tests := map[string]Options{
		"blank product":    {Product: "   "},
		"invalid ID":       {Product: "demo", ID: "Not A Slug"},
		"mutable image":    {Product: "demo", Environment: "container", Image: "ubuntu:latest"},
		"zero timeout":     {Product: "demo", TimeoutSet: true},
		"zero repetitions": {Product: "demo", RepetitionsSet: true},
		"negative timeout": {Product: "demo", Timeout: -1},
		"malformed HTTP":   {Product: "demo", Checks: []string{"http:"}},
	}
	for name, options := range tests {
		t.Run(name, func(t *testing.T) {
			options.Dir = filepath.Join(t.TempDir(), "new-trial")
			if _, err := Write(options); err == nil {
				t.Fatal("invalid options accepted")
			}
			if _, err := os.Stat(options.Dir); !os.IsNotExist(err) {
				t.Fatalf("invalid options wrote files: %v", err)
			}
		})
	}
}

func TestHeadlessInitReportsRetainedMetadataAndMissingWork(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "trial")
	first, err := Write(Options{Dir: dir, Product: "original", ID: "original", Harness: "codex"})
	if err != nil || !first.Valid || first.TaskSet || first.VerifyChecks != 0 {
		t.Fatalf("first write: %+v %v", first, err)
	}
	before, _ := os.ReadFile(first.TrialFile)
	second, err := Write(Options{Dir: dir, Product: "replacement", ID: "replacement",
		Harness: "kiro-cli", Task: "make result", Checks: []string{"file:result"}})
	if err != nil || !second.Valid || second.ID != "original" || second.Harness != "codex" ||
		second.Reporter != "codex" || second.TaskSet || second.VerifyChecks != 0 || len(second.Kept) != 2 {
		t.Fatalf("retained metadata: %+v %v", second, err)
	}
	after, _ := os.ReadFile(first.TrialFile)
	if string(before) != string(after) {
		t.Fatal("retained trial changed")
	}
	forced, err := Write(Options{Dir: dir, Product: "replacement", ID: "replacement",
		Harness: "kiro-cli", Task: "make result", Checks: []string{"file:result"}, Force: true})
	if err != nil || !forced.Valid || forced.ID != "replacement" || !forced.TaskSet || forced.VerifyChecks != 1 {
		t.Fatalf("force: %+v %v", forced, err)
	}
}

func TestHeadlessInitEscapesControlsAndAcceptsPinnedImage(t *testing.T) {
	options := Options{Product: "Demo\x01 CLI", Dir: filepath.Join(t.TempDir(), "trial")}
	r, err := Write(options)
	if err != nil || !r.Valid {
		t.Fatalf("%+v: %+v %v", options, r, err)
	}
	// Codex's default auth supports an isolated config. No image inspection
	// or authentication runs while writing this structural scaffold.
	pinned := Options{Product: "demo", Harness: "codex", Environment: "container",
		Image: "sha256:" + strings.Repeat("a", 64), Dir: filepath.Join(t.TempDir(), "pinned")}
	r, err = Write(pinned)
	if err != nil || !r.Valid {
		t.Fatalf("pinned image scaffold: %+v %v", r, err)
	}
	// Inherited login cannot supply credentials to a fresh container HOME.
	pinned.Harness, pinned.Dir = "claude-code", filepath.Join(t.TempDir(), "inherit")
	r, err = Write(pinned)
	if err != nil || r.Valid || !strings.Contains(r.Problem, "auth 'inherit'") {
		t.Fatalf("missing isolated auth should remain invalid: %+v %v", r, err)
	}
}
