package cli

import (
	"path/filepath"
	"strings"
	"testing"

	ajx "github.com/royosherove/ajx"
)

func TestRootHelpEntryPointsShareBanner(t *testing.T) {
	offline(t)
	wantPrefix := ajx.Logo + "v" + ajx.Version + "\n\nusage: ajx"
	var baseline string
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"-h"}} {
		code, output, diagnostic := run(t, args...)
		if code != 0 || diagnostic != "" || !strings.HasPrefix(output, wantPrefix) {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, output, diagnostic)
		}
		if strings.Count(output, ajx.Logo) != 1 {
			t.Fatalf("%v: expected one logo: %q", args, output)
		}
		if baseline != "" && output != baseline {
			t.Fatalf("%v: help differs from bare ajx", args)
		}
		baseline = output
	}
}

func TestDoctorBannerStaysOutOfJSON(t *testing.T) {
	offline(t)
	// Missing tools are a deterministic failed-readiness outcome, with no
	// installed harness, credential or model commands invoked.
	t.Setenv("PATH", t.TempDir())
	code, output, diagnostic := run(t, "doctor")
	if code != 1 || diagnostic != "" ||
		!strings.HasPrefix(output, ajx.Logo+"v"+ajx.Version+"\n\n") ||
		!strings.Contains(output, "NOT READY:") || strings.Count(output, ajx.Logo) != 1 {
		t.Fatalf("human doctor: code=%d stdout=%q stderr=%q", code, output, diagnostic)
	}
	jsonCode, output, diagnostic := run(t, "doctor", "--json")
	doc := decode(t, output)
	if jsonCode != code || doc["command"] != "doctor" || doc["ready"] != false ||
		strings.Contains(output, ajx.Logo) || strings.Contains(diagnostic, ajx.Logo) {
		t.Fatalf("JSON doctor: code=%d stdout=%q stderr=%q", jsonCode, output, diagnostic)
	}
}

func TestBannerDoesNotDecorateOtherOutputOrErrors(t *testing.T) {
	offline(t)
	for _, args := range [][]string{
		{"--version"}, {"version"}, {"version", "--json"},
		{"doctor", "--help"}, {"validate", "--help", "--json"},
		{"completion", "bash"}, {"docs", "--json"},
		{"unknown-command"}, {"doctor", "--unknown"},
		{"help", "--unknown"}, {"doctor", filepath.Join(t.TempDir(), "missing.toml")},
		{"doctor", filepath.Join(t.TempDir(), "missing.toml"), "--json"},
	} {
		code, output, diagnostic := run(t, args...)
		if strings.Contains(output, ajx.Logo) || strings.Contains(diagnostic, ajx.Logo) {
			t.Fatalf("%v: banner leaked into unrelated output: code=%d stdout=%q stderr=%q",
				args, code, output, diagnostic)
		}
		if args[len(args)-1] == "--json" && !strings.Contains(strings.Join(args, " "), "--help") {
			decode(t, output)
		}
	}
}
