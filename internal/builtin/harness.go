package builtin

import (
	"path/filepath"

	"github.com/royosherove/ajx/internal/agentconfig"
	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

// run launches argv through BaseHarness.Run (or RunHook in tests).
func run(h plugin.Harness, ctx *plugin.Ctx, argv []string, stage string, o plugin.RunOpts) (M, error) {
	if RunHook != nil {
		return RunHook(h, ctx, argv, stage, o)
	}
	return h.Base().Run(ctx, argv, stage, o)
}

func strp(s string) *string { return &s }

// safeArgv is Harness.safe_argv as a JSON list.
func safeArgv(h plugin.Harness, ctx *plugin.Ctx, argv []string, hide ...string) []any {
	return anyList(h.Base().SafeArgv(ctx, argv, hide))
}

// cellStr is str(cell[key]) ("" when unset or falsy).
func cellStr(ctx *plugin.Ctx, key string) string {
	v := ctx.Cell[key]
	if !util.Truthy(v) {
		return ""
	}
	return util.Str(v)
}

// cellList is list(cell.get(key) or []).
func cellList(ctx *plugin.Ctx, key string) []string {
	return util.Strings(ctx.Cell[key])
}

func cellConfig(ctx *plugin.Ctx) string { return util.S(ctx.Cell["config"]) }

func rawPath(runDir, stage string) string { return filepath.Join(runDir, stage+".raw.jsonl") }

// ------------------------------------------------------------ agent configuration

func configuration(ctx *plugin.Ctx) bool { return len(agentconfig.Configuration(ctx)) > 0 }

func launchBinary(ctx *plugin.Ctx, h plugin.Harness) string { return agentconfig.LaunchBinary(ctx, h) }

func launchContext(ctx *plugin.Ctx) *plugin.Ctx { return agentconfig.LaunchContext(ctx) }

func launchArgs(ctx *plugin.Ctx, h plugin.Harness) ([]string, error) {
	return agentconfig.LaunchArgs(ctx, h)
}

func launchEnv(ctx *plugin.Ctx, h plugin.Harness) (map[string]string, error) {
	return agentconfig.LaunchEnv(ctx, h)
}

func ensureReporterContext(ctx *plugin.Ctx) error { return agentconfig.EnsureReporterContext(ctx) }

// envOr is CleanEnv's view of a clean env that may fail: Python lets
// clean_env raise to doctor; the Go contract has no error, so the modeled env
// is empty and the launch itself (which checks the error) fails instead.
func envOr(env map[string]string, err error) map[string]string {
	if err != nil || env == nil {
		return map[string]string{}
	}
	return env
}
