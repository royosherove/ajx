// Package modelcatalog shares harness-specific model suggestions and aliases
// between the wizard, console, and Python implementation.
package modelcatalog

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/royosherove/ajx/internal/res"
)

type Entry struct {
	Models  []string          `json:"models"`
	Aliases map[string]string `json:"aliases"`
	Hint    string            `json:"hint"`
	Sources []string          `json:"sources"`
}

var entries = load()

func load() map[string]Entry {
	var catalog struct {
		Harnesses map[string]Entry `json:"harnesses"`
	}
	if err := json.Unmarshal([]byte(res.MustText("assets/model-catalog.json")), &catalog); err != nil {
		panic("invalid bundled model catalog: " + err.Error())
	}
	return catalog.Harnesses
}

func Models(harness string) []string {
	return append([]string{}, entries[harness].Models...)
}

func Hint(harness string) string {
	if hint := entries[harness].Hint; hint != "" {
		return hint
	}
	return "Use a model ID accepted by this agent, or leave it empty for the agent's default."
}

// Resolve expands only documented family aliases for a known harness. Exact
// IDs and custom provider/gateway IDs are kept, including their original case.
// The catalog is a set of suggestions, never an account entitlement allowlist.
func Resolve(harness, model string) (string, error) {
	model = strings.TrimSpace(model)
	entry, known := entries[harness]
	if !known || model == "" {
		return model, nil
	}
	key := strings.ToLower(model)
	if id, ok := entry.Aliases[key]; ok {
		return id, nil
	}
	for _, other := range entries {
		if _, alias := other.Aliases[key]; alias {
			return "", fmt.Errorf("%q is not a model alias for %s. Choose a model for %s, enter its exact model ID, or leave it empty for the agent's default", model, harness, harness)
		}
	}
	return model, nil
}
