package render

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/render/pyfmt"
	"github.com/royosherove/ajx/internal/reporter"
	"github.com/royosherove/ajx/internal/util"
)

func synthFixture(t *testing.T) (string, M, []M) {
	out := t.TempDir()
	ask := func(id, title string) M {
		return M{"id": id, "title": title, "evidence_status": "observed_friction", "requested_behavior": "r", "how_observed": strings.Repeat("o", 500)}
	}
	for _, r := range []string{"a-r1", "b-r1", "c-r1"} {
		reg := M{"asks": []any{ask("ASK-001", "Title "+r)}}
		if r == "c-r1" {
			reg = M{"asks": []any{}}
		}
		if err := pyfmt.WriteJSON(filepath.Join(out, "runs", r, "asks.json"), reg); err != nil {
			t.Fatal(err)
		}
	}
	rows := []M{{"run_id": "a-r1", "dir": "a-r1", "harness": "h", "model_id": "m"},
		{"run_id": "b-r1", "dir": "b-r1", "harness": "h", "model_requested": "q"}, {"run_id": "c-r1", "dir": "c-r1"}}
	return out, M{"trial": M{"product": "P"}}, rows
}

func TestSynthesizeClustersValidatesMembers(t *testing.T) {
	out, sp, rows := synthFixture(t)
	saved := reporter.Call
	defer func() { reporter.Call = saved }()
	var prompt string
	cluster := M{"title": "Shared", "evidence_status": "observed_friction",
		"members":                    []any{M{"run_id": "a-r1", "ask_id": "ASK-001"}, M{"run_id": "b-r1", "ask_id": "ASK-001"}},
		"representative_observation": M{"run_id": "b-r1", "text": "seen"}}
	reporter.Call = func(_ context.Context, _ M, dir, stage, p string, schema M) (M, error) {
		prompt = p
		if stage != "synthesize" || dir != filepath.Join(out, "synthesis") || schema == nil {
			t.Fatalf("call %s %s", stage, dir)
		}
		return M{"proc": M{"is_error": false}, "structured": M{"clusters": []any{cluster}}}, nil
	}
	clusters, err := SynthesizeClusters(context.Background(), sp, out, rows, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 || clusters[0]["id"] != "AJX-001" || clusters[0]["observed_in"] != int64(2) {
		t.Fatal(clusters)
	}
	if !strings.Contains(prompt, "## Run a-r1 (h / m)") || !strings.Contains(prompt, "## Run b-r1 (h / q)") ||
		strings.Contains(prompt, "c-r1") || strings.Contains(prompt, strings.Repeat("o", 401)) {
		t.Fatal(prompt)
	}
	if _, err := os.Stat(filepath.Join(out, "synthesis", "clusters.json")); err != nil {
		t.Fatal(err)
	}
	cluster["members"] = []any{M{"run_id": "a-r1", "ask_id": "ASK-009"}}
	if _, err := SynthesizeClusters(context.Background(), sp, out, rows, nil); err == nil || err.Error() != "cluster references an unknown run or ask" {
		t.Fatal(err)
	}
	cluster["members"] = []any{M{"run_id": "a-r1", "ask_id": "ASK-001"}}
	if _, err := SynthesizeClusters(context.Background(), sp, out, rows, nil); err == nil || err.Error() != "cluster observation is not from a member run" {
		t.Fatal(err)
	}
	reporter.Call = func(context.Context, M, string, string, string, M) (M, error) {
		return M{"proc": M{"is_error": true}}, nil
	}
	if _, err := SynthesizeClusters(context.Background(), sp, out, rows, nil); err == nil || pyfmt.TypeName(err) != "ValueError" {
		t.Fatal(err)
	}
	if c, err := SynthesizeClusters(context.Background(), sp, out, rows[1:], nil); c != nil || err != nil {
		t.Fatal("fewer than two registers must not call the reporter", c, err)
	}
}

func TestMatrixSynthesisFailureIsLoggedButInterruptionPropagates(t *testing.T) {
	out := t.TempDir()
	for _, r := range []string{"a-r1", "b-r1"} {
		pyfmt.WriteJSON(filepath.Join(out, "runs", r, "state.json"), M{"stages": M{}})
	}
	sp := M{"trial": M{"id": "t", "product": "P", "output_dir": out, "repetitions": int64(1), "parallel": int64(1), "order_seed": int64(0)},
		"task": M{"prompt_sha256": strings.Repeat("a", 64)}}
	saved := synthesize
	defer func() { synthesize = saved }()
	synthesize = func(context.Context, M, string, []M, func(string)) ([]M, error) {
		return nil, pyfmt.Errorf("ValueError", "reporter returned an invalid clustering response")
	}
	var logs []string
	if err := MatrixReport(sp, MatrixOpts{Synthesize: true, Log: func(s string) { logs = append(logs, s) }}); err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0] != "synthesis skipped: reporter returned an invalid clustering response" {
		t.Fatal(logs)
	}
	if util.MapOr(pyfmt.ReadMap(filepath.Join(out, "matrix.json")))["clusters"] != nil {
		t.Fatal("clusters recorded")
	}
	synthesize = func(context.Context, M, string, []M, func(string)) ([]M, error) { return nil, context.Canceled }
	if err := MatrixReport(sp, MatrixOpts{Synthesize: true, Log: func(string) {}}); err == nil {
		t.Fatal("interruption swallowed")
	}
}
