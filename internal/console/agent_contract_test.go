package console

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/util"
)

func TestAgentViewKeepsTrialErrorsInSinceAndWait(t *testing.T) {
	d := newDemo(t, "0")
	if err := os.WriteFile(d.trial, []byte("[trial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, wait := range []bool{false, true} {
		var doc M
		var err error
		if wait {
			doc, err = Wait(context.Background(), d.trial, 0, 1)
		} else {
			doc, err = Since(d.trial, 0, false)
		}
		if err != nil {
			t.Fatal(err)
		}
		if !util.Truthy(doc["trial_error"]) || util.Get(doc, "readiness", "status") != "invalid" ||
			len(util.List(util.Get(doc, "readiness", "problems"))) == 0 ||
			util.Get(doc, "output", "blocker_kind") != "invalid_trial" ||
			!strings.Contains(util.S(doc["headline"]), "invalid") {
			t.Fatalf("invalid trial hidden in agent view: %v", doc)
		}
	}
}

func TestAgentViewKeepsStaleRunnerWarning(t *testing.T) {
	d := newDemo(t, "0")
	host, _ := os.Hostname()
	writeJSONFile(t, filepath.Join(d.out(), ".lock"), M{"pid": 2147483647, "host": host})
	doc, err := Since(d.trial, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if util.Get(doc, "process", "stale_lock") != true || !strings.Contains(util.S(doc["headline"]), "no longer active") {
		t.Fatalf("stale runner warning hidden: %v", doc)
	}
}
