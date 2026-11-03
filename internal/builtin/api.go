// Package builtin ports lib/ajx/builtin/*.py: the built-in harness adapters
// (claude-code, codex, kiro-cli, gemini-cli, cursor-agent, copilot-cli), their
// auth profiles, the generic inherit/env auth types, the declarative
// [adapters.*] harness, the local/docker/wrapper runners, the shell,
// file_exists and http checks, and the native console demo harness. Every
// plugin registers itself in init() under the Python name (source "builtin").
package builtin

import (
	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

// RunHook, when set (tests only), replaces BaseHarness.Run for every builtin
// adapter (Python tests patch Harness.run). It receives the adapter, the
// context the adapter launched with, the unwrapped argv, the stage and options.
var RunHook func(h plugin.Harness, ctx *plugin.Ctx, argv []string, stage string, o plugin.RunOpts) (util.M, error)

// ProviderHinter is implemented by auth types that can name the model
// provider an environment selects (Python InheritAuth.provider_hint). A nil
// env means the caller's environment.
type ProviderHinter interface {
	ProviderHint(env map[string]string) string
}

// CleanEnvChecker is implemented by builtin adapters whose clean env can
// fail (a prepared agent configuration that belongs to another harness).
// CleanEnv returns an empty map in that case; CleanEnvErr returns the error
// that Python's clean_env raises, for doctor to report.
type CleanEnvChecker interface {
	CleanEnvErr(ctx *plugin.Ctx) (map[string]string, error)
}

// CommonArgs returns the model/effort/clean/auth/profile args a builtin
// harness adds to every launch (Python Harness._common); ok=false when h has
// no such notion.
func CommonArgs(h plugin.Harness, ctx *plugin.Ctx) (args []string, ok bool, err error) {
	if c, is := h.(interface {
		commonArgs(*plugin.Ctx) ([]string, error)
	}); is {
		a, err := c.commonArgs(ctx)
		return a, true, err
	}
	return nil, false, nil
}

// NewDeclarative builds the harness an [adapters.<name>] table describes.
func NewDeclarative(name string, conf util.M) plugin.Harness {
	return newDeclarative(name, conf)
}
