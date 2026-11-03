package builtin

import (
	"strings"

	"github.com/royosherove/ajx/internal/cloudauth"
	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

// ================================================================ gemini-cli
// Google Gemini CLI (`gemini -p --output-format stream-json`). NOT verified
// live by ajx tests. Documented stream-json events (each with `timestamp`):
// init{session_id,model}, message{role,content,delta},
// tool_use{tool_name,tool_id,parameters}, tool_result{tool_id,status,output,error},
// error{severity,message},
// result{status,stats{total_tokens,input_tokens,output_tokens,duration_ms,tool_calls}}.

// GeminiCli is the gemini-cli harness adapter.
type GeminiCli struct{ plugin.BaseHarness }

func newGeminiCli() plugin.Harness {
	h := &GeminiCli{}
	h.BinaryName = "gemini"
	h.VersionArgv = []string{"gemini", "--version"}
	h.CanResume = true
	h.DefaultAuth = "gemini-login" // ~/.gemini settings/GEMINI.md/extensions are always read
	h.Telemetry = "session output tokens + tool-call count from result.stats; event timestamps"
	return h
}

func (h *GeminiCli) commonArgs(ctx *plugin.Ctx) ([]string, error) {
	var args []string
	if m := cellStr(ctx, "model"); m != "" {
		args = append(args, "--model", m)
	}
	return append(args, plugin.AuthArgs(ctx)...), nil
}

func (h *GeminiCli) Execute(ctx *plugin.Ctx) (M, error) {
	common, _ := h.commonArgs(ctx)
	argv := append([]string{"gemini", "--output-format", "stream-json"}, common...)
	if perm := cellList(ctx, "permission_args"); len(perm) > 0 {
		argv = append(argv, perm...)
	} else {
		argv = append(argv, "--yolo")
	}
	argv = append(append(argv, cellList(ctx, "args")...), "-p", ctx.PromptText)
	proc, err := run(h, ctx, argv, "execute", plugin.RunOpts{})
	if proc == nil {
		return proc, err
	}
	proc["argv"] = safeArgv(h, ctx, argv, ctx.PromptText)
	proc["session_id"] = firstOf(rawPath(ctx.RunDir, "execute"), func(ev M) (any, bool) {
		return ev["session_id"], ev["type"] == "init"
	})
	return proc, err
}

// firstOf returns pick(ev) for the first event that pick accepts.
func firstOf(raw string, pick func(M) (any, bool)) any {
	for _, r := range newScope().records(raw) {
		if ev := r.evMap(); util.Truthy(ev) {
			if v, ok := pick(ev); ok {
				return v
			}
		}
	}
	return nil
}

func (h *GeminiCli) Narrate(ctx *plugin.Ctx, prompt string) (M, error) {
	sidV := ctx.StageState("execute")["session_id"]
	if !util.Truthy(sidV) {
		return nil, nil
	}
	common, _ := h.commonArgs(ctx)
	argv := append([]string{"gemini", "--output-format", "stream-json", "--resume", util.Str(sidV),
		"--approval-mode", "default"}, common...)
	full := append(argv, "-p", prompt)
	proc, err := run(h, ctx, full, "narrate", plugin.RunOpts{})
	if proc == nil {
		return proc, err
	}
	proc["argv"] = safeArgv(h, ctx, full, prompt)
	proc["session_id"] = sidV
	proc["tools_disabled"] = "approval-mode default; headless tool requests are not approved"
	return proc, err
}

func (h *GeminiCli) Normalize(ctx *plugin.Ctx, stage string, excludeMessageIDs []string) (M, error) {
	s := newScope()
	out := plugin.EmptyTelemetry(M{"timestamp_source": "gemini event timestamps (fallback: arrival)"})
	limitations := []any{"gemini-cli adapter not verified live by ajx tests",
		"gemini-cli reads ~/.gemini config; cannot be made clean"}
	calls := map[string]M{}
	events := []any{}
	var buf []string
	var bufAt any
	var result M
	toolIDs := map[string]bool{}
	var lastAt any
	for _, r := range s.records(rawPath(ctx.RunDir, stage)) {
		lastAt = r.at
		ev := r.evMap()
		if !util.Truthy(ev) {
			continue
		}
		var ts any = r.at
		if t, ok := ev["timestamp"].(string); ok {
			ts = t
		}
		t := ev["type"]
		if t == "init" {
			out["session_id"] = ev["session_id"]
			if util.Truthy(ev["model"]) {
				out["model_ids"] = []any{ev["model"]}
			}
		} else if t == "message" && ev["role"] == "assistant" {
			bufAt = util.Or(bufAt, ts)
			text, _ := util.Or(ev["content"], "").(string)
			buf = append(buf, text)
			continue
		}
		if len(buf) > 0 && t != "message" {
			events = append(events, s.textEvent(strings.Join(buf, ""), bufAt, nil))
			buf, bufAt = nil, nil
		}
		switch t {
		case "tool_use":
			tid := ev["tool_id"]
			toolIDs[idKey(tid)] = true
			calls[idKey(tid)] = s.toolEvent(tid, ev["tool_name"], ev["parameters"], ts, nil)
			events = append(events, calls[idKey(tid)])
		case "tool_result":
			st := ev["status"]
			s.finishTool(calls[idKey(ev["tool_id"])], ts, !(st == "success" || st == nil), util.Or(ev["output"], ev["error"]))
		case "result":
			result = ev
		}
	}
	if len(buf) > 0 {
		events = append(events, s.textEvent(strings.Join(buf, ""), bufAt, nil))
	}
	out["events"] = events
	var texts []string
	for _, e := range events {
		if em := e.(M); em["kind"] == "text" {
			texts = append(texts, util.S(em["text"]))
		}
	}
	switch {
	case len(texts) == 0:
		out["final_text"] = ""
	case stage == "execute":
		out["final_text"] = texts[len(texts)-1]
	default:
		out["final_text"] = strings.Join(texts, "\n")
	}
	if result != nil {
		stats := util.MapOr(util.Or(result["stats"], M{}))
		out["stop_reason"] = result["status"]
		out["harness_reported"].(M)["stats"] = stats
		if tokens := stats["output_tokens"]; isInt(tokens) {
			out["usage"] = []any{M{"id": "session", "at": util.Or(result["timestamp"], lastAt), "output_tokens": tokens}}
			out["usage_status"] = "complete"
			out["expected_output_tokens"] = tokens
			limitations = append(limitations, "gemini tokens are a session total assigned to the result event")
		}
		if calls := stats["tool_calls"]; isInt(calls) && toInt64(calls) == int64(len(toolIDs)) {
			out["tool_status"] = "complete"
			out["expected_tool_calls"] = int64(len(toolIDs))
		} else if len(toolIDs) > 0 {
			out["tool_status"] = "partial"
		} else {
			out["tool_status"] = "unavailable"
		}
	} else {
		if len(toolIDs) > 0 {
			out["tool_status"] = "partial"
		} else {
			out["tool_status"] = "unavailable"
		}
		limitations = append(limitations, stage+": no result event")
	}
	out["limitations"] = limitations
	return out, nil
}

func registerGemini() {
	plugin.RegisterHarness("gemini-cli", "builtin", newGeminiCli)
	plugin.RegisterAuth("gemini-login", "builtin", func(conf M) plugin.Auth {
		a := newSimpleAuth(conf, false)
		a.Harnesses = []string{"gemini-cli"}
		a.Description = "Login with Google (cached OAuth in ~/.gemini)"
		a.baseUnset = []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENAI_USE_VERTEXAI"}
		return a
	})
	plugin.RegisterAuth("gemini-api", "builtin", func(conf M) plugin.Auth {
		a := newSimpleAuth(conf, true)
		a.Harnesses = []string{"gemini-cli"}
		a.Description = "Gemini API key from AI Studio (GEMINI_API_KEY)"
		a.RequiredEnv = []string{"GEMINI_API_KEY"}
		a.baseUnset = []string{"GOOGLE_GENAI_USE_VERTEXAI"}
		return a
	})
	plugin.RegisterAuth("gemini-vertex", "builtin", func(conf M) plugin.Auth {
		a := cloudauth.New(conf)
		a.Harnesses = []string{"gemini-cli"}
		a.Description = "Vertex AI (GOOGLE_GENAI_USE_VERTEXAI=true, GOOGLE_CLOUD_PROJECT, GOOGLE_CLOUD_LOCATION; ADC or GOOGLE_API_KEY)"
		a.RequiredEnv = []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION"}
		a.ModelEnv = []string{"GOOGLE_*", "CLOUDSDK_*"}
		a.ProviderEnv = "GOOGLE_GENAI_USE_VERTEXAI"
		a.CredentialRoutes = [][]string{{"GOOGLE_API_KEY"}}
		a.UnsupportedEnv = []string{"GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_CONFIG", "CLOUDSDK_AUTH_*", "GEMINI_API_KEY"}
		a.HomeCredentials = "Google Application Default Credentials (ADC)"
		a.FixedEnv = map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "true"}
		return a
	})
}

// ================================================================ cursor-agent
// Cursor CLI (`cursor-agent -p --output-format stream-json`). NOT verified
// live by ajx tests. Documented stream-json events:
// system{subtype:init,session_id,model}, user,
// assistant{message{content[{type:text,text}]}},
// tool_call{subtype:started|completed, call_id, tool_call{<kind>ToolCall{args, result}}},
// result{subtype,result,session_id,duration_ms,is_error}. No token counts.

// CursorAgent is the cursor-agent harness adapter.
type CursorAgent struct{ plugin.BaseHarness }

func newCursorAgent() plugin.Harness {
	h := &CursorAgent{}
	h.BinaryName = "cursor-agent"
	h.VersionArgv = []string{"cursor-agent", "--version"}
	h.CanResume = true
	h.DefaultAuth = "cursor-login"
	h.Telemetry = "tool calls, assistant text, arrival timestamps; no tokens"
	return h
}

func (h *CursorAgent) commonArgs(ctx *plugin.Ctx) ([]string, error) {
	var args []string
	if m := cellStr(ctx, "model"); m != "" {
		args = append(args, "--model", m)
	}
	return append(args, plugin.AuthArgs(ctx)...), nil
}

func (h *CursorAgent) Execute(ctx *plugin.Ctx) (M, error) {
	common, _ := h.commonArgs(ctx)
	argv := append([]string{"cursor-agent", "-p", "--output-format", "stream-json"}, common...)
	if perm := cellList(ctx, "permission_args"); len(perm) > 0 {
		argv = append(argv, perm...)
	} else {
		argv = append(argv, "--force")
	}
	argv = append(append(argv, cellList(ctx, "args")...), ctx.PromptText)
	proc, err := run(h, ctx, argv, "execute", plugin.RunOpts{})
	if proc == nil {
		return proc, err
	}
	proc["argv"] = safeArgv(h, ctx, argv, ctx.PromptText)
	proc["session_id"] = firstOf(rawPath(ctx.RunDir, "execute"), func(ev M) (any, bool) {
		return ev["session_id"], util.Truthy(ev["session_id"])
	})
	return proc, err
}

func (h *CursorAgent) Narrate(ctx *plugin.Ctx, prompt string) (M, error) {
	sidV := ctx.StageState("execute")["session_id"]
	if !util.Truthy(sidV) {
		return nil, nil
	}
	common, _ := h.commonArgs(ctx)
	argv := append([]string{"cursor-agent", "-p", "--output-format", "stream-json", "--resume", util.Str(sidV)}, common...)
	full := append(argv, prompt)
	proc, err := run(h, ctx, full, "narrate", plugin.RunOpts{})
	if proc == nil {
		return proc, err
	}
	proc["argv"] = safeArgv(h, ctx, full, prompt)
	proc["session_id"] = sidV
	proc["tools_disabled"] = "no --force; headless writes/commands are not approved"
	return proc, err
}

func (h *CursorAgent) Normalize(ctx *plugin.Ctx, stage string, excludeMessageIDs []string) (M, error) {
	s := newScope()
	out := plugin.EmptyTelemetry(M{"timestamp_source": "ajx line-arrival timestamps",
		"limitations": []any{"cursor-agent adapter not verified live by ajx tests", "cursor-agent exposes no token counts"}})
	calls := map[string]M{}
	events := []any{}
	for _, r := range s.records(rawPath(ctx.RunDir, stage)) {
		ev := r.evMap()
		if !util.Truthy(ev) {
			continue
		}
		switch ev["type"] {
		case "system":
			if ev["subtype"] == "init" {
				out["session_id"] = ev["session_id"]
				if util.Truthy(ev["model"]) {
					out["model_ids"] = []any{ev["model"]}
				} else {
					out["model_ids"] = []any{}
				}
			}
		case "assistant":
			var parts []string
			for _, c := range util.List(util.Or(util.MapOr(util.Or(ev["message"], M{}))["content"], []any{})) {
				if cm, ok := c.(M); ok {
					t, _ := util.Or(cm["text"], "").(string)
					parts = append(parts, t)
				}
			}
			if text := strings.Join(parts, ""); pyStrip(text) != "" {
				events = append(events, s.textEvent(text, r.at, nil))
			}
		case "tool_call":
			cid := ev["call_id"]
			body := util.MapOr(util.Or(ev["tool_call"], M{}))
			kind := "tool"
			if ks := s.keys(body); len(ks) > 0 {
				kind = ks[0]
			}
			inner := util.MapOr(util.Or(body[kind], M{}))
			switch ev["subtype"] {
			case "started":
				calls[idKey(cid)] = s.toolEvent(cid, strings.ReplaceAll(kind, "ToolCall", ""), inner["args"], r.at, nil)
				events = append(events, calls[idKey(cid)])
			case "completed":
				res := util.Or(inner["result"], M{})
				var failed bool
				switch rv := res.(type) {
				case M:
					_, f1 := rv["failure"]
					_, f2 := rv["error"]
					failed = f1 || f2
				case string:
					failed = strings.Contains(rv, "failure") || strings.Contains(rv, "error")
				case []any:
					for _, x := range rv {
						if x == "failure" || x == "error" {
							failed = true
						}
					}
				}
				s.finishTool(calls[idKey(cid)], r.at, failed, res)
			}
		case "result":
			out["stop_reason"] = ev["subtype"]
			out["final_text"] = util.Or(ev["result"], "")
			hr := out["harness_reported"].(M)
			hr["duration_ms"] = ev["duration_ms"]
			hr["is_error"] = ev["is_error"]
		}
	}
	out["events"] = events
	if len(calls) > 0 {
		out["tool_status"] = "partial"
	} else {
		out["tool_status"] = "unavailable"
	}
	if stage == "narrate" && !util.Truthy(out["final_text"]) {
		var texts []string
		for _, e := range events {
			if em := e.(M); em["kind"] == "text" {
				texts = append(texts, util.S(em["text"]))
			}
		}
		out["final_text"] = strings.Join(texts, "\n")
	}
	return out, nil
}

func registerCursor() {
	plugin.RegisterHarness("cursor-agent", "builtin", newCursorAgent)
	plugin.RegisterAuth("cursor-login", "builtin", func(conf M) plugin.Auth {
		a := newSimpleAuth(conf, false)
		a.Harnesses = []string{"cursor-agent"}
		a.Description = "cursor-agent login (browser); `cursor-agent status` for identity"
		a.identityCmd = []string{"cursor-agent", "status"}
		return a
	})
	plugin.RegisterAuth("cursor-api", "builtin", func(conf M) plugin.Auth {
		a := newSimpleAuth(conf, true)
		a.Harnesses = []string{"cursor-agent"}
		a.Description = "Cursor API key (CURSOR_API_KEY) for headless/CI"
		a.RequiredEnv = []string{"CURSOR_API_KEY"}
		return a
	})
}

// ================================================================ copilot-cli
// GitHub Copilot CLI (`copilot -p`). NOT verified live by ajx tests. Headless
// output is plain text, so telemetry is wall clock + stdout.

// CopilotCli is the copilot-cli harness adapter.
type CopilotCli struct{ plugin.BaseHarness }

func newCopilotCli() plugin.Harness {
	h := &CopilotCli{}
	h.BinaryName = "copilot"
	h.VersionArgv = []string{"copilot", "--version"}
	// resume needs an interactive picker or a session id we cannot read
	h.DefaultAuth = "github-login"
	h.Telemetry = "wall clock and stdout only"
	return h
}

func (h *CopilotCli) Execute(ctx *plugin.Ctx) (M, error) {
	argv := []string{"copilot", "-p", ctx.PromptText}
	if m := cellStr(ctx, "model"); m != "" {
		argv = append(argv, "--model", m)
	}
	if perm := cellList(ctx, "permission_args"); len(perm) > 0 {
		argv = append(argv, perm...)
	} else {
		argv = append(argv, "--allow-all-tools")
	}
	argv = append(append(argv, plugin.AuthArgs(ctx)...), cellList(ctx, "args")...)
	proc, err := run(h, ctx, argv, "execute", plugin.RunOpts{})
	if proc == nil {
		return proc, err
	}
	proc["argv"] = safeArgv(h, ctx, argv, ctx.PromptText)
	proc["session_id"] = nil
	return proc, err
}

func (h *CopilotCli) Normalize(ctx *plugin.Ctx, stage string, excludeMessageIDs []string) (M, error) {
	return plainTelemetry(ctx, stage, M{
		"model_ids": []any{util.OrStr(cellStr(ctx, "model"), "copilot default (unrecorded)")},
		"limitations": []any{"copilot-cli adapter not verified live by ajx tests",
			"copilot-cli headless output has no tool/token telemetry",
			"no self-narration: journey is a labeled reconstruction"},
	}, false), nil
}

// GitHubToken accepts any of the three token variables.
type GitHubToken struct{ *plugin.AuthBase }

func (a *GitHubToken) Problems(env map[string]string) []string {
	e := util.MergeEnv(util.Environ(), a.Env())
	for _, v := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if e[v] != "" {
			return nil
		}
	}
	return []string{"none of COPILOT_GITHUB_TOKEN / GH_TOKEN / GITHUB_TOKEN is set"}
}

func registerCopilot() {
	plugin.RegisterHarness("copilot-cli", "builtin", newCopilotCli)
	plugin.RegisterAuth("github-login", "builtin", func(conf M) plugin.Auth {
		a := newSimpleAuth(conf, false)
		a.Harnesses = []string{"copilot-cli"}
		a.Description = "Copilot CLI /login (GitHub OAuth)"
		return a
	})
	plugin.RegisterAuth("github-token", "builtin", func(conf M) plugin.Auth {
		b := plugin.NewAuthBase(conf)
		b.Harnesses = []string{"copilot-cli"}
		b.Description = "Fine-grained PAT with Copilot Requests permission (COPILOT_GITHUB_TOKEN, GH_TOKEN or GITHUB_TOKEN)"
		b.ModelEnv = []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}
		return &GitHubToken{AuthBase: b}
	})
}
