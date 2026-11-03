package builtin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/royosherove/ajx/internal/util"
)

func TestDemoClustersKeepActualAttemptEvidence(t *testing.T) {
	out := t.TempDir()
	for i, run := range []string{"demo-r1", "demo-r2", "demo-r3"} {
		dir := filepath.Join(out, "runs", run)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		asks := []any{}
		if i < 2 {
			asks = append(asks, M{"id": "ASK-001", "title": "Explain a missing flag", "requested_behavior": "Show valid columns",
				"evidence_status": "observed_friction", "how_observed": "The demo retried the command."})
		}
		data, _ := util.MarshalIndent(M{"asks": asks})
		if err := os.WriteFile(filepath.Join(dir, "asks.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	clusters := util.Maps(demoClusters(out))
	if len(clusters) != 1 || len(util.List(clusters[0]["members"])) != 2 {
		t.Fatalf("expected one shared ask with two actual members: %v", clusters)
	}
	for _, member := range util.Maps(clusters[0]["members"]) {
		if member["run_id"] == "demo-r3" {
			t.Fatal("invented evidence for interrupted attempt")
		}
	}
}
