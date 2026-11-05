package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/royosherove/ajx/internal/util"
)

func TestStatusShowsReportingStageElapsedWithoutWorkerEvents(t *testing.T) {
	offline(t)
	out := t.TempDir()
	dir := filepath.Join(out, "runs", "synthetic-r1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339Nano)
	if err := util.WriteJSON(filepath.Join(dir, "state.json"), util.M{"stages": util.M{
		"execute": util.M{"status": "done"},
		"extract": util.M{"status": "running", "at": start},
	}}); err != nil {
		t.Fatal(err)
	}
	code, text, errText := run(t, "status", out)
	if code != 0 || !strings.Contains(text, "extract") || !strings.Contains(text, "elapsed") || !strings.Contains(text, start) {
		t.Fatalf("status lacks reporting progress: code=%d\n%s\n%s", code, text, errText)
	}
	code, text, _ = run(t, "status", out, "--json")
	if code != 0 {
		t.Fatal(text)
	}
	doc := decode(t, text)
	record := util.Maps(doc["runs"])[0]
	active := util.Maps(record["active_stages"])
	elapsed := float64(-1)
	if len(active) == 1 {
		elapsed, _ = util.Num(active[0]["elapsed_seconds"])
	}
	if len(active) != 1 || active[0]["stage"] != "extract" || active[0]["started_at"] != start ||
		elapsed < 120 {
		t.Fatalf("status JSON lacks truthful stage progress: %v", record)
	}
}

func TestStatusStageAgeHandlesMissingAndFutureClocks(t *testing.T) {
	now := time.Now().UTC()
	for _, at := range []any{nil, "invalid", now.Add(time.Hour).Format(time.RFC3339)} {
		row := runRow{
			stages: util.M{"narrate": "running"},
			state:  util.M{"stages": util.M{"narrate": util.M{"status": "running", "at": at}}},
		}
		active := activeStages(row, now)
		if len(active) != 1 || active[0]["elapsed_seconds"] != nil {
			t.Fatalf("invalid clock produced a duration: %v", active)
		}
	}
	row := runRow{stages: util.M{"extract": "interrupted"}}
	if active := activeStages(row, now); len(active) != 0 {
		t.Fatalf("interrupted stage reported active: %v", active)
	}
}
