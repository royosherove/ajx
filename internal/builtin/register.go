package builtin

import "github.com/royosherove/ajx/internal/plugin"

func init() {
	plugin.RegisterHarness("claude-code", "builtin", newClaudeCode)
	registerClaudeAuth()
	registerCodex()
	registerKiro()
	registerGemini()
	registerCursor()
	registerCopilot()
	registerGenericAuth()
	registerRunners()
	registerChecks()
	// The console demo trial bundles plugins/console_demo.py; the Go CLI
	// registers this native port instead when it loads that trial.
	plugin.NativeEquivalents["console_demo.py"] = true
	plugin.NativeRegistrars["console_demo.py"] = RegisterConsoleDemo
}
