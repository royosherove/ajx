package modelcatalog

import (
	"slices"
	"strings"
	"testing"
)

func TestResolveUsesTheSelectedHarness(t *testing.T) {
	for _, tc := range []struct{ harness, input, want string }{
		{"claude-code", "opus", "opus"},
		{"kiro-cli", " Opus ", "claude-opus-5.5"},
		{"copilot-cli", "haiku", "claude-haiku-5.5"},
		{"codex", "sol", "gpt-6.1-sol"},
		{"codex", "Astra", "gpt-6-astra"},
		{"codex", "terra", "gpt-6.1-terra"},
		{"codex", "luna", "gpt-6-luna"},
		{"kiro-cli", "sol", "gpt-5.6-sol"},
		{"cursor-agent", "terra", "gpt-5.6-terra"},
		{"codex", "", ""},
		{"claude-code", "gateway/MyCustomModel-v2", "gateway/MyCustomModel-v2"},
		{"custom-harness", "opus", "opus"},
	} {
		t.Run(tc.harness+"/"+tc.input, func(t *testing.T) {
			got, err := Resolve(tc.harness, tc.input)
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, tc := range []struct{ harness, model string }{
		{"codex", "opus"}, {"claude-code", "astra"}, {"gemini-cli", "sonnet"}, {"kiro-cli", "astra"},
	} {
		if _, err := Resolve(tc.harness, tc.model); err == nil || !strings.Contains(err.Error(), tc.harness) {
			t.Fatalf("expected actionable mismatch for %s / %s; got %v", tc.harness, tc.model, err)
		}
	}
}

func TestCatalogChoicesAreUniqueAndAliasesPointToOfferedModels(t *testing.T) {
	for _, harness := range []string{"claude-code", "codex", "kiro-cli", "gemini-cli", "cursor-agent", "copilot-cli"} {
		entry := entries[harness]
		if len(entry.Models) < 5 || len(entry.Sources) == 0 || entry.Hint == "" {
			t.Fatalf("missing model choices or guidance for %s", harness)
		}
		seen := map[string]bool{}
		for _, model := range entry.Models {
			if seen[model] || strings.ContainsAny(model, " \t\n") {
				t.Fatalf("duplicate or invalid model %s / %q", harness, model)
			}
			seen[model] = true
		}
		for alias, model := range entry.Aliases {
			if !slices.Contains(entry.Models, model) {
				t.Fatalf("%s alias %s points to an unlisted model %s", harness, alias, model)
			}
		}
	}
}
