package builtin

// Kiro CLI (`kiro-cli chat --output-format stream-json`). Verified live: ACP event stream.

import (
	"fmt"
	"strings"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

// restrictedAuthArgs keeps provider/model options without granting tools
// during reporting.
func restrictedAuthArgs(ctx *plugin.Ctx) []string {
	args := plugin.AuthArgs(ctx)
	var restricted []string
	for i := 0; i < len(args); i++ {
		flag, _, equals := strings.Cut(args[i], "=")
		switch {
		case flag == "--trust-tools":
			if !equals && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++ // Remove the split-form tool list as well as its flag.
			}
		case flag != "--trust-all-tools":
			restricted = append(restricted, args[i])
		}
	}
	return restricted
}

// KiroCli is the kiro-cli harness adapter.
type KiroCli struct{ plugin.BaseHarness }

func newKiroCli() plugin.Harness {
	h := &KiroCli{}
	h.BinaryName = "kiro-cli"
	h.VersionArgv = []string{"kiro-cli", "--version"}
	h.CanResume = true
	h.CanReport = true
	h.VerifiedLive = true
	h.DefaultAuth = "kiro-login"
	h.Telemetry = "tool calls (no independent total), credits, arrival timestamps; no tokens"
	return h
}

func (h *KiroCli) cleanEnv(ctx *plugin.Ctx) (map[string]string, error) { return launchEnv(ctx, h) }

// CleanEnvErr is CleanEnv with the profile error, if any.
func (h *KiroCli) CleanEnvErr(ctx *plugin.Ctx) (map[string]string, error) { return h.cleanEnv(ctx) }

func (h *KiroCli) CleanEnv(ctx *plugin.Ctx) map[string]string { return envOr(h.cleanEnv(ctx)) }

func (h *KiroCli) Execute(ctx *plugin.Ctx) (M, error) {
	argv := []string{launchBinary(ctx, h), "chat", "--output-format", "stream-json", "--trust-all-tools"}
	if m := cellStr(ctx, "model"); m != "" {
		argv = append(argv, "--model", m)
	}
	if e := cellStr(ctx, "effort"); e != "" {
		argv = append(argv, "--effort", e)
	}
	if configuration(ctx) {
		argv = append(argv, plugin.AuthArgs(ctx)...)
	}
	extra, err := launchArgs(ctx, h)
	if err != nil {
		return nil, err
	}
	argv = append(append(append(argv, cellList(ctx, "args")...), extra...), ctx.PromptText)
	env, err := h.cleanEnv(ctx)
	if err != nil {
		return nil, err
	}
	proc, err := run(h, launchContext(ctx), argv, "execute", plugin.RunOpts{ExtraEnv: env})
	if proc == nil {
		return proc, err
	}
	proc["argv"] = safeArgv(h, ctx, argv, ctx.PromptText)
	proc["session_id"] = kiroSessionID(rawPath(ctx.RunDir, "execute"))
	return proc, err
}

func kiroSessionID(raw string) any {
	for _, r := range newScope().records(raw) {
		if sid := util.MapOr(util.Or(util.MapOr(r.ev)["data"], M{}))["sessionId"]; util.Truthy(sid) {
			return sid
		}
	}
	return nil
}

func (h *KiroCli) Narrate(ctx *plugin.Ctx, prompt string) (M, error) {
	sidV := ctx.StageState("execute")["session_id"]
	if !util.Truthy(sidV) {
		return nil, nil
	}
	argv := []string{launchBinary(ctx, h), "chat", "--output-format", "stream-json", "--resume-id", util.Str(sidV), "--trust-tools="}
	if m := cellStr(ctx, "model"); m != "" {
		argv = append(argv, "--model", m)
	}
	if configuration(ctx) {
		argv = append(argv, restrictedAuthArgs(ctx)...)
	}
	extra, err := launchArgs(ctx, h)
	if err != nil {
		return nil, err
	}
	argv = append(argv, extra...)
	env, err := h.cleanEnv(ctx)
	if err != nil {
		return nil, err
	}
	full := append(append([]string{}, argv...), prompt)
	proc, err := run(h, launchContext(ctx), full, "narrate", plugin.RunOpts{ExtraEnv: env})
	if proc == nil {
		return proc, err
	}
	proc["argv"] = safeArgv(h, ctx, full, prompt)
	proc["session_id"] = sidV
	proc["tools_disabled"] = "untrusted (--trust-tools=); headless tool requests are denied"
	return proc, err
}

func (h *KiroCli) Report(ctx *plugin.Ctx, prompt string, schema M) (M, error) {
	if err := ensureReporterContext(ctx); err != nil {
		return nil, err
	}
	argv := []string{"kiro-cli", "chat", "--no-interactive", "--output-format", "stream-json", "--trust-tools="}
	if m := cellStr(ctx, "model"); m != "" {
		argv = append(argv, "--model", m)
	}
	if e := cellStr(ctx, "effort"); e != "" {
		argv = append(argv, "--effort", e)
	}
	argv = append(argv, restrictedAuthArgs(ctx)...)
	full := append(append([]string{}, argv...), prompt)
	proc, err := run(h, ctx, full, ctx.StageName, plugin.RunOpts{})
	if err != nil {
		return nil, err
	}
	proc["argv"] = safeArgv(h, ctx, full, prompt)
	proc["tools_policy"] = "no trusted tools; headless tool requests are denied"
	return h.ReportResult(ctx, proc)
}

func (h *KiroCli) Normalize(ctx *plugin.Ctx, stage string, excludeMessageIDs []string) (M, error) {
	s := newScope()
	third := "kiro loads ~/.kiro steering/agents/MCP; config cannot be made clean"
	if configuration(ctx) {
		third = "kiro profile uses isolated KIRO_HOME and explicit V2 agent resources; " +
			"built-in/managed sources and actual skill use remain unverified"
	}
	hr := M{"credits": 0.0, "credit_note": "kiro meteringUsage, summed; harness-reported"}
	out := plugin.EmptyTelemetry(M{
		"model_ids": []any{util.OrStr(cellStr(ctx, "model"), "auto")}, "tool_status": "partial",
		"harness_reported": hr,
		"timestamp_source": "ajx line-arrival timestamps (kiro stream has no event times)",
	})
	limitations := []any{"kiro-cli exposes no token counts; output tokens unavailable",
		"kiro tool calls have no independent expected total; tool_status partial", third}
	calls := map[string]M{}
	events := []any{}
	var buf []string
	var bufAt any
	var fullTexts []string
	flush := func() {
		text := strings.Join(buf, "")
		events = append(events, s.textEvent(text, bufAt, nil))
		fullTexts = append(fullTexts, text)
		buf, bufAt = nil, nil
	}
	for _, r := range s.records(rawPath(ctx.RunDir, stage)) {
		ev := r.evMap()
		if !util.Truthy(ev) {
			continue
		}
		data := util.MapOr(util.Or(ev["data"], M{}))
		out["session_id"] = util.Or(out["session_id"], data["sessionId"])
		if ev["type"] == "metadata" {
			var values []any
			for _, m := range util.List(util.Or(data["meteringUsage"], []any{})) {
				values = append(values, pyFloatOf(util.Or(util.MapOr(m)["value"], int64(0))))
			}
			hr["credits"] = toFloat(hr["credits"]) + toFloat(pySum(values))
			if v, ok := data["contextUsagePercentage"]; ok && v != nil {
				hr["context_usage_percentage_last"] = v
			}
		}
		if ev["type"] == "runFinished" {
			out["stop_reason"] = util.Or(data["stopReason"], data["status"])
			out["final_text"] = util.Or(data["finalText"], "")
			hr["final_text_truncated"] = data["finalTextTruncated"]
		}
		upd := util.MapOr(util.Or(data["update"], M{}))
		kind := upd["sessionUpdate"]
		if kind == "agent_message_chunk" {
			bufAt = util.Or(bufAt, r.at)
			text, _ := util.Or(util.MapOr(util.Or(upd["content"], M{}))["text"], "").(string)
			buf = append(buf, text)
			continue
		}
		if len(buf) > 0 && (kind == "tool_call" || kind == "tool_call_update") {
			flush()
		}
		switch kind {
		case "tool_call":
			tid := upd["toolCallId"]
			name := util.Or(util.MapOr(util.Or(util.MapOr(util.Or(upd["_meta"], M{}))["kiro"], M{}))["toolName"], upd["kind"])
			calls[idKey(tid)] = s.toolEvent(tid, name, util.Or(upd["rawInput"], upd["title"]), r.at, nil)
			events = append(events, calls[idKey(tid)])
		case "tool_call_update":
			call := calls[idKey(upd["toolCallId"])]
			if call == nil {
				continue
			}
			for _, c := range util.List(util.Or(upd["content"], []any{})) {
				text := util.MapOr(util.Or(util.MapOr(util.Or(c, M{}))["content"], M{}))["text"]
				if t, ok := text.(string); ok && t != "" {
					call["output"] = util.Tail(util.S(call["output"])+t, 1500)
				}
			}
			if st := upd["status"]; st == "completed" || st == "failed" {
				s.finishTool(call, r.at, st == "failed", nil)
			}
		}
	}
	if len(buf) > 0 {
		flush()
	}
	// kiro's runFinished.finalText concatenates every assistant message of
	// the run; the declared outcome is the last message, the narration is all
	// of them
	if len(fullTexts) > 0 {
		hr["final_text_concatenated"] = out["final_text"]
		if stage == "execute" {
			out["final_text"] = fullTexts[len(fullTexts)-1]
		} else {
			out["final_text"] = strings.Join(fullTexts, "\n\n")
		}
	}
	out["events"] = events
	if !util.Truthy(out["stop_reason"]) {
		limitations = append(limitations, fmt.Sprintf("%s: no runFinished event (killed, crashed, or timed out)", stage))
	}
	out["limitations"] = limitations
	return out, nil
}

func registerKiro() {
	plugin.RegisterHarness("kiro-cli", "builtin", newKiroCli)
	plugin.RegisterAuth("kiro-login", "builtin", func(conf M) plugin.Auth {
		a := newSimpleAuth(conf, false)
		a.Harnesses = []string{"kiro-cli"}
		a.Description = "kiro-cli login (AWS Builder ID or IAM Identity Center); `kiro-cli whoami` for identity"
		a.ModelEnv = []string{"KIRO_API_KEY"}
		a.identityCmd = []string{"kiro-cli", "whoami"}
		return a
	})
}
