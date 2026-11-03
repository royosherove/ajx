package builtin

// OpenAI Codex CLI (`codex exec --json`). NOT verified live by ajx tests.
//
// Event schema (documented JSONL): thread.started{thread_id}, turn.started,
// item.started/item.updated/item.completed{item:{id,type,...}},
// turn.completed{usage:{input_tokens,cached_input_tokens,output_tokens}},
// turn.failed{error}, error{message}. Item types: agent_message{text},
// reasoning, command_execution{command,aggregated_output,exit_code,status},
// file_change{changes}, mcp_tool_call{server,tool,status}, web_search{query}.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

var codexToolItems = []string{"command_execution", "file_change", "mcp_tool_call", "web_search", "todo_list"}

// Codex is the codex harness adapter.
type Codex struct{ plugin.BaseHarness }

func newCodex() plugin.Harness {
	h := &Codex{}
	h.BinaryName = "codex"
	h.VersionArgv = []string{"codex", "--version"}
	h.CanResume = true
	h.CanReport = true
	h.CleanSupported = true // per-run CODEX_HOME (no user AGENTS.md / config / MCP)
	h.DefaultAuth = "codex-chatgpt"
	h.Telemetry = "per-turn output tokens (turn.completed), command/file/mcp items; arrival timestamps"
	return h
}

func (h *Codex) isolated(ctx *plugin.Ctx) bool {
	return configuration(ctx) || cellConfig(ctx) == "clean"
}

func (h *Codex) cleanEnv(ctx *plugin.Ctx) (map[string]string, error) {
	if configuration(ctx) {
		return launchEnv(ctx, h)
	}
	if h.isolated(ctx) {
		return map[string]string{"CODEX_HOME": ctx.ConfigDir}, nil
	}
	return map[string]string{}, nil
}

// CleanEnvErr is CleanEnv with the profile error, if any.
func (h *Codex) CleanEnvErr(ctx *plugin.Ctx) (map[string]string, error) { return h.cleanEnv(ctx) }

func (h *Codex) CleanEnv(ctx *plugin.Ctx) map[string]string { return envOr(h.cleanEnv(ctx)) }

func (h *Codex) commonArgs(ctx *plugin.Ctx) ([]string, error) {
	var args []string
	if m := cellStr(ctx, "model"); m != "" {
		args = append(args, "--model", m)
	}
	if e := cellStr(ctx, "effort"); e != "" {
		args = append(args, "-c", "model_reasoning_effort="+e)
	}
	args = append(args, plugin.AuthArgs(ctx)...)
	extra, err := launchArgs(ctx, h)
	if err != nil {
		return nil, err
	}
	return append(args, extra...), nil
}

func (h *Codex) Execute(ctx *plugin.Ctx) (M, error) {
	argv := []string{launchBinary(ctx, h), "exec", "--json", "--skip-git-repo-check", "--cd", ctx.Workspace}
	common, err := h.commonArgs(ctx)
	if err != nil {
		return nil, err
	}
	argv = append(argv, common...)
	if perm := cellList(ctx, "permission_args"); len(perm) > 0 {
		argv = append(argv, perm...)
	} else {
		argv = append(argv, "--dangerously-bypass-approvals-and-sandbox")
	}
	argv = append(append(argv, cellList(ctx, "args")...), "-")
	env, err := h.cleanEnv(ctx)
	if err != nil {
		return nil, err
	}
	proc, err := run(h, launchContext(ctx), argv, "execute", plugin.RunOpts{StdinData: strp(ctx.PromptText), ExtraEnv: env})
	if proc != nil {
		proc["session_id"] = codexThread(rawPath(ctx.RunDir, "execute"))
	}
	return proc, err
}

func codexThread(raw string) any {
	for _, r := range newScope().records(raw) {
		if ev := r.evMap(); ev != nil && ev["type"] == "thread.started" {
			return ev["thread_id"]
		}
	}
	return nil
}

func (h *Codex) Narrate(ctx *plugin.Ctx, prompt string) (M, error) {
	sidV := ctx.StageState("execute")["session_id"]
	if !util.Truthy(sidV) {
		return nil, nil
	}
	sid := util.Str(sidV)
	var argv []string
	if configuration(ctx) {
		// --sandbox belongs to exec, not its resume subcommand in current CLIs.
		argv = []string{launchBinary(ctx, h), "exec", "--sandbox", "read-only", "resume", sid,
			"--json", "--skip-git-repo-check"}
	} else {
		argv = []string{"codex", "exec", "resume", sid, "--json", "--skip-git-repo-check", "--sandbox", "read-only"}
	}
	common, err := h.commonArgs(ctx)
	if err != nil {
		return nil, err
	}
	argv = append(append(argv, common...), "-")
	env, err := h.cleanEnv(ctx)
	if err != nil {
		return nil, err
	}
	proc, err := run(h, launchContext(ctx), argv, "narrate", plugin.RunOpts{StdinData: strp(prompt), ExtraEnv: env})
	if proc != nil {
		proc["session_id"] = sidV
		proc["tools_disabled"] = "read-only sandbox (commands may still run read-only)"
	}
	return proc, err
}

func (h *Codex) Report(ctx *plugin.Ctx, prompt string, schema M) (M, error) {
	if err := ensureReporterContext(ctx); err != nil {
		return nil, err
	}
	stage := ctx.StageName
	response := filepath.Join(ctx.RunDir, stage+".response.txt")
	_ = os.Remove(response)
	argv := []string{"codex", "exec", "--json", "--skip-git-repo-check", "--ephemeral",
		"--ignore-user-config", "--ignore-rules", "--sandbox", "read-only",
		"-c", `approval_policy="never"`, "--cd", ctx.Workspace,
		"--output-last-message", response}
	common, err := h.commonArgs(ctx)
	if err != nil {
		return nil, err
	}
	argv = append(argv, common...)
	if len(schema) > 0 {
		schemaPath := filepath.Join(ctx.RunDir, stage+".schema.json")
		if err := os.WriteFile(schemaPath, []byte(newScope().dumps(schema, true)), 0o644); err != nil {
			return nil, err
		}
		argv = append(argv, "--output-schema", schemaPath)
	}
	env, err := h.cleanEnv(ctx)
	if err != nil {
		return nil, err
	}
	proc, err := run(h, ctx, append(argv, "-"), stage, plugin.RunOpts{StdinData: strp(prompt), ExtraEnv: env})
	if err != nil {
		return nil, err
	}
	proc["tools_policy"] = "read-only sandbox; read-only commands may still run"
	result, err := h.ReportResult(ctx, proc)
	if err != nil {
		return nil, err
	}
	if data, err := os.ReadFile(response); err == nil {
		result["text"] = string(data)
	}
	return result, nil
}

func (h *Codex) Normalize(ctx *plugin.Ctx, stage string, excludeMessageIDs []string) (M, error) {
	s := newScope()
	out := plugin.EmptyTelemetry(M{"model_ids": []any{util.OrStr(cellStr(ctx, "model"), "codex default (unrecorded)")},
		"timestamp_source": "ajx line-arrival timestamps"})
	limitations := []any{"codex adapter not verified live by ajx tests"}
	hr := out["harness_reported"].(M)
	items := map[string]M{}
	events := []any{}
	usage := []any{}
	var texts []string
	turns := 0
	var stop any
	for _, r := range s.records(rawPath(ctx.RunDir, stage)) {
		ev := r.evMap()
		if !util.Truthy(ev) {
			continue
		}
		t := ev["type"]
		switch t {
		case "thread.started":
			out["session_id"] = ev["thread_id"]
		case "item.started", "item.completed":
			item := util.MapOr(util.Or(ev["item"], M{}))
			iid := item["id"]
			itype := util.Or(item["type"], item["item_type"])
			itypeS, _ := itype.(string)
			if util.Contains(codexToolItems, itypeS) {
				key := idKey(iid)
				if _, ok := items[key]; !ok {
					var pairs []any
					for _, k := range []string{"command", "changes", "server", "tool", "query"} {
						if v, present := item[k]; present && v != nil {
							pairs = append(pairs, k, v)
						}
					}
					items[key] = s.toolEvent(iid, itype, s.obj(pairs...), r.at, nil)
					events = append(events, items[key])
				}
				if t == "item.completed" {
					code, hasCode := item["exit_code"]
					failed := item["status"] == "failed" || (hasCode && code != nil && !(isNum(code) && toFloat(code) == 0))
					s.finishTool(items[key], r.at, failed, util.Or(item["aggregated_output"], item["result"]))
				}
			} else if itype == "agent_message" && t == "item.completed" {
				text, _ := util.Or(item["text"], "").(string)
				events = append(events, s.textEvent(text, r.at, nil))
				texts = append(texts, text)
			}
		case "turn.completed":
			turns++
			u := util.MapOr(util.Or(ev["usage"], M{}))
			usage = append(usage, M{"id": fmt.Sprintf("turn-%d", turns), "at": r.at,
				"output_tokens": pyIntOf(util.Or(u["output_tokens"], int64(0)))})
			tu, _ := hr["turn_usage"].([]any)
			hr["turn_usage"] = append(tu, u)
		case "turn.failed", "error":
			stop = "error"
			detail := util.Or(ev["error"], ev["message"])
			limitations = append(limitations, fmt.Sprintf("%s: %s: %s", stage, t, util.Clip(s.pyStr(detail), 200)))
		}
	}
	out["events"] = events
	out["usage"] = usage
	if len(texts) > 0 && stage == "execute" {
		out["final_text"] = texts[len(texts)-1]
	} else {
		out["final_text"] = strings.Join(texts, "\n\n")
	}
	if !util.Truthy(stop) && turns > 0 {
		stop = "completed"
	}
	out["stop_reason"] = stop
	if len(usage) > 0 {
		out["usage_status"] = "partial"
		limitations = append(limitations, "codex usage is per turn with no independent session total; partial")
	}
	if len(events) > 0 {
		out["tool_status"] = "partial"
	} else {
		out["tool_status"] = "unavailable"
	}
	out["limitations"] = limitations
	return out, nil
}

// ---------------------------------------------------------------- auth

// CodexChatGPT copies the ChatGPT sign-in (auth.json) into an isolated
// CODEX_HOME for clean runs and removes it afterwards.
type CodexChatGPT struct{ *plugin.AuthBase }

func (a *CodexChatGPT) source() string {
	home := util.Str(a.Conf["codex_home"])
	if !util.Truthy(a.Conf["codex_home"]) {
		home = filepath.Join(util.Home(), ".codex")
	}
	return filepath.Join(home, "auth.json")
}

func (a *CodexChatGPT) Prepare(ctx *plugin.Ctx) error {
	src := a.source()
	if _, err := os.Stat(src); err != nil || ctx.ConfigDir == "" {
		return nil
	}
	if err := os.MkdirAll(ctx.ConfigDir, 0o755); err != nil {
		return err
	}
	target := filepath.Join(ctx.ConfigDir, "auth.json")
	if err := copyFile(src, target); err != nil {
		return err
	}
	if runnerBackend(ctx) == "container" && geteuid() == 0 {
		info, err := os.Stat(ctx.ConfigDir)
		if err != nil {
			return err
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			return chown(target, int(st.Uid), int(st.Gid))
		}
	}
	return nil
}

func (a *CodexChatGPT) Cleanup(ctx *plugin.Ctx) error {
	if ctx.ConfigDir == "" {
		return nil
	}
	copied := filepath.Join(ctx.ConfigDir, "auth.json")
	if _, err := os.Stat(copied); err == nil {
		return os.Remove(copied)
	}
	return nil
}

func (a *CodexChatGPT) Problems(env map[string]string) []string {
	src := a.source()
	if _, err := os.Stat(src); err == nil {
		return nil
	}
	return []string{fmt.Sprintf("no codex login found at %s (run `codex login`)", src)}
}

// geteuid and chown are replaced in tests.
var geteuid, chown = os.Geteuid, os.Chown

// runnerBackend is the execution environment's backend ("container",
// "local"...) when the runner owns one; "" for legacy runners.
func runnerBackend(ctx *plugin.Ctx) string {
	if b, ok := ctx.Runner.(interface{ Backend() string }); ok {
		return b.Backend()
	}
	if _, managed := ctx.Runner.(plugin.ManagedRunner); managed {
		return util.S(ctx.EnvironmentProfile["backend"])
	}
	return ""
}

// copyFile is shutil.copy2: contents, mode and modification time.
func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, info.Mode().Perm()); err != nil {
		return err
	}
	_ = os.Chmod(dst, info.Mode().Perm())
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

// CodexProvider is Azure OpenAI, Ollama, OpenRouter, or any
// [model_providers.X] Codex supports:
//
//	[auth.azure]
//	type = "codex-provider"
//	provider = "azure"                       # passed as -c model_provider=azure
//	config = ['model_providers.azure.base_url="https://..."', 'model_providers.azure.env_key="AZURE_OPENAI_API_KEY"']
//	env = { AZURE_OPENAI_API_KEY = "${AZURE_OPENAI_API_KEY}" }
type CodexProvider struct{ *plugin.AuthBase }

func (a *CodexProvider) ExtraArgs() []string {
	var args []string
	if util.Truthy(a.Conf["provider"]) {
		args = append(args, "-c", "model_provider="+util.Str(a.Conf["provider"]))
	}
	for _, c := range util.Strings(a.Conf["config"]) {
		args = append(args, "-c", c)
	}
	return append(args, a.AuthBase.ExtraArgs()...)
}

func registerCodex() {
	plugin.RegisterHarness("codex", "builtin", newCodex)
	plugin.RegisterAuth("codex-chatgpt", "builtin", func(conf M) plugin.Auth {
		b := plugin.NewAuthBase(conf)
		b.Harnesses = []string{"codex"}
		b.Description = "ChatGPT sign-in (auth.json in ~/.codex); copied into an isolated CODEX_HOME for clean runs"
		b.ModelEnv = []string{"CODEX_HOME"}
		return &CodexChatGPT{AuthBase: b}
	})
	plugin.RegisterAuth("openai-api", "builtin", func(conf M) plugin.Auth {
		a := newSimpleAuth(conf, true)
		a.Harnesses = []string{"codex"}
		a.Description = "OpenAI API key (OPENAI_API_KEY); optional OPENAI_BASE_URL"
		a.RequiredEnv = []string{"OPENAI_API_KEY"}
		a.ModelEnv = []string{"OPENAI_*"}
		return a
	})
	plugin.RegisterAuth("codex-provider", "builtin", func(conf M) plugin.Auth {
		b := plugin.NewAuthBase(conf)
		b.Harnesses = []string{"codex"}
		b.Description = "custom/Azure/OSS model provider via `-c model_provider=...` overrides"
		return &CodexProvider{AuthBase: b}
	})
}
