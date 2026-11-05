package builtin

// Claude Code (`claude -p`). Verified live: stream-json + on-disk transcript
// reconciliation.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/royosherove/ajx/internal/cloudauth"
	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

// ClaudeManagedSettings is the managed-settings file Claude Code applies last.
// Tests point it at a synthetic path (Python patches ClaudeCode.managed_settings).
var ClaudeManagedSettings = func() string {
	if runtime.GOOS == "darwin" {
		return "/Library/Application Support/ClaudeCode/managed-settings.json"
	}
	return "/etc/claude-code/managed-settings.json"
}()

func (s *scope) settingsFileEnv(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	v, err := s.decode(data)
	if err != nil {
		return nil
	}
	env, ok := util.Map(v)["env"].(M)
	if !ok {
		return nil
	}
	out := map[string]string{}
	for k, val := range env {
		out[k] = s.pyStr(val)
	}
	return out
}

// ---------------------------------------------------------------- result folding
// A `claude -p` session emits one `result` event per segment when the agent
// schedules its own continuation (ScheduleWakeup / self-resume). Observed
// live: num_turns, duration_ms and usage are per segment; total_cost_usd (and
// modelUsage costUSD) is a running total for the whole session.

func sumOrNone(values []any) any {
	var nums []any
	for _, v := range values {
		if isNum(v) {
			nums = append(nums, v)
		}
	}
	if len(nums) == 0 {
		return nil
	}
	return pySum(nums)
}

// runningTotal: the last value of a running total is the figure. A series
// that ever decreases cannot be cumulative, so the segments are summed
// instead and the caller says so. Returns (value, summed).
func runningTotal(values []any) (any, bool) {
	var nums []any
	for _, v := range values {
		if isNum(v) {
			nums = append(nums, v)
		}
	}
	if len(nums) == 0 {
		return nil, false
	}
	for i := 1; i < len(nums); i++ {
		if pyLess(nums[i], nums[i-1]) {
			return pySum(nums), true
		}
	}
	return nums[len(nums)-1], false
}

var cumulativeUsage = map[string]bool{"costUSD": true}     // running session total, like total_cost_usd
var constantUsage = map[string]bool{"contextWindow": true} // a property of the model, not a counter

// mergeModelUsage merges per-result modelUsage maps by model id: counters are
// per segment and summed, costUSD follows runningTotal, contextWindow is kept,
// non-numeric fields keep their first value. Returns (merged, cost_summed).
func (s *scope) mergeModelUsage(usages []any) (M, bool) {
	merged := M{}
	var mergedOrder []string
	type seriesKey struct{ model, key string }
	series := map[seriesKey][]any{}
	var seriesOrder []seriesKey
	for _, usage := range usages {
		u, ok := usage.(M)
		if !ok {
			continue
		}
		for _, model := range s.keys(u) {
			slot, exists := merged[model].(M)
			if !exists {
				slot = M{}
				merged[model] = slot
				mergedOrder = append(mergedOrder, model)
				s.order[mapID(slot)] = nil
			}
			fields, ok := u[model].(M)
			if !ok {
				continue
			}
			for _, key := range s.keys(fields) {
				value := fields[key]
				_, has := slot[key]
				switch {
				case !isNum(value):
					if !has {
						slot[key] = value
						s.order[mapID(slot)] = append(s.order[mapID(slot)], key)
					}
				case cumulativeUsage[key]:
					sk := seriesKey{model, key}
					if _, seen := series[sk]; !seen {
						seriesOrder = append(seriesOrder, sk)
					}
					series[sk] = append(series[sk], value)
				case constantUsage[key]:
					cur := value
					if has {
						cur = slot[key]
					}
					if pyLess(cur, value) {
						cur = value
					}
					slot[key] = cur
					if !has {
						s.order[mapID(slot)] = append(s.order[mapID(slot)], key)
					}
				default:
					var cur any = int64(0)
					if has {
						cur = slot[key]
					}
					slot[key] = pyAdd(cur, value)
					if !has {
						s.order[mapID(slot)] = append(s.order[mapID(slot)], key)
					}
				}
			}
		}
	}
	summed := false
	for _, sk := range seriesOrder {
		slot := merged[sk.model].(M)
		v, was := runningTotal(series[sk])
		if _, has := slot[sk.key]; !has {
			s.order[mapID(slot)] = append(s.order[mapID(slot)], sk.key)
		}
		slot[sk.key] = v
		summed = summed || was
	}
	s.order[mapID(merged)] = mergedOrder
	return merged, summed
}

type aggregate struct {
	last            M
	count           int
	modelUsage      any
	modelIDs        []string
	numTurns        any
	durationMS      any
	costEstimate    any
	costSummed      bool
	modelCostSummed bool
	subagents       any
	safetyStops     any
	outputTokens    any
}

// aggregateResults folds the stream's result events (in order) into one
// record. Text, subtype and error state come from the last event; turns,
// duration, usage and spawned subagents are summed; cost is the running
// session total. With a single event every figure is that event's own.
func (s *scope) aggregateResults(results []M) aggregate {
	last := results[len(results)-1]
	collect := func(get func(M) any) []any {
		out := make([]any, len(results))
		for i, r := range results {
			out[i] = get(r)
		}
		return out
	}
	cost, costSummed := runningTotal(collect(func(r M) any { return r["total_cost_usd"] }))
	var modelUsage any = last["modelUsage"]
	modelCostSummed := false
	if len(results) > 1 {
		modelUsage, modelCostSummed = s.mergeModelUsage(collect(func(r M) any { return r["modelUsage"] }))
	}
	var modelIDs []string
	seen := map[string]bool{}
	for _, r := range results {
		if mu, ok := r["modelUsage"].(M); ok {
			for _, m := range s.keys(mu) {
				if !seen[m] {
					seen[m] = true
					modelIDs = append(modelIDs, m)
				}
			}
		}
	}
	return aggregate{last: last, count: len(results), modelUsage: modelUsage, modelIDs: modelIDs,
		numTurns:     sumOrNone(collect(func(r M) any { return r["num_turns"] })),
		durationMS:   sumOrNone(collect(func(r M) any { return r["duration_ms"] })),
		costEstimate: cost, costSummed: costSummed, modelCostSummed: modelCostSummed,
		subagents:    sumOrNone(collect(func(r M) any { return util.MapOr(r["subagent_stats"])["spawned"] })),
		safetyStops:  sumOrNone(collect(func(r M) any { return r["safety_stops"] })),
		outputTokens: sumOrNone(collect(func(r M) any { return util.MapOr(r["usage"])["output_tokens"] }))}
}

func multiResultLimitation(stage string, agg aggregate) string {
	var summed []string
	if agg.costSummed {
		summed = append(summed, "total_cost_usd")
	}
	if agg.modelCostSummed {
		summed = append(summed, "modelUsage costUSD")
	}
	cost := "cost taken as the cumulative session total"
	if len(summed) > 0 {
		cost = strings.Join(summed, " and ") + " decreased between segments, so cost was summed from non-cumulative segments"
	}
	return fmt.Sprintf("%s: %d result events in one session (self-resumed/scheduled continuation); "+
		"turns, duration and usage summed across segments; %s", stage, agg.count, cost)
}

// ---------------------------------------------------------------- harness

// ClaudeCode is the claude-code harness adapter.
type ClaudeCode struct {
	plugin.BaseHarness
}

func newClaudeCode() plugin.Harness {
	h := &ClaudeCode{}
	h.BinaryName = "claude"
	h.VersionArgv = []string{"claude", "--version"}
	h.CanResume = true
	h.CanReport = true
	h.CleanSupported = true
	h.VerifiedLive = true
	h.Telemetry = "per-message output tokens (reconciled), tool calls (reconciled), event timestamps"
	// Session-coupling vars present when ajx itself runs inside Claude Code.
	// Provider/auth vars are left to the auth profile.
	h.StripEnv = []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_CHILD_SESSION",
		"CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_PID", "CLAUDE_CODE_MESSAGING_SOCKET",
		"CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_EFFORT",
		"CLAUDE_CODE_EXECPATH", "CLAUDE_CODE_SAFE_MODE", "CLAUDE_CODE_SIMPLE"}
	h.ToolShellObserved = true
	h.ToolShellRestores = []string{"PATH"} // from the shell snapshot, before every Bash call
	return h
}

func (h *ClaudeCode) isolated(ctx *plugin.Ctx) bool {
	if configuration(ctx) {
		return true
	}
	return cellConfig(ctx) == "clean" && (ctx.Auth == nil || ctx.Auth.Base().IsolatesConfig)
}

func (h *ClaudeCode) cleanEnv(ctx *plugin.Ctx) (map[string]string, error) {
	if configuration(ctx) {
		return launchEnv(ctx, h)
	}
	env := map[string]string{"DISABLE_AUTOUPDATER": "1"}
	if h.isolated(ctx) {
		env["CLAUDE_CONFIG_DIR"] = ctx.ConfigDir
	}
	return env, nil
}

// CleanEnv isolates the config dir for clean runs (or the profile's launch env).
// CleanEnvErr is CleanEnv with the profile error, if any.
func (h *ClaudeCode) CleanEnvErr(ctx *plugin.Ctx) (map[string]string, error) { return h.cleanEnv(ctx) }

func (h *ClaudeCode) CleanEnv(ctx *plugin.Ctx) map[string]string { return envOr(h.cleanEnv(ctx)) }

// SettingsEnv: Claude Code applies `env` from its settings over the process
// env (observed with --safe-mode too, which is why a trial [env] value can
// lose): user settings in $CLAUDE_CONFIG_DIR (default ~/.claude), project and
// local settings, managed settings last.
func (h *ClaudeCode) SettingsEnv(env map[string]string, projectDir string) []plugin.SettingsLayer {
	userDir := filepath.Join(util.Home(), ".claude")
	if env["CLAUDE_CONFIG_DIR"] != "" {
		userDir = env["CLAUDE_CONFIG_DIR"]
	}
	files := []string{filepath.Join(userDir, "settings.json")}
	if projectDir != "" {
		files = append(files, filepath.Join(projectDir, ".claude", "settings.json"),
			filepath.Join(projectDir, ".claude", "settings.local.json"))
	}
	files = append(files, ClaudeManagedSettings)
	home := util.Home() + "/"
	s := newScope()
	var out []plugin.SettingsLayer
	for _, p := range files {
		values := s.settingsFileEnv(p)
		if len(values) == 0 {
			continue
		}
		label := p
		if strings.HasPrefix(p, home) {
			label = "~/" + p[len(home):]
		}
		out = append(out, plugin.SettingsLayer{Source: label, Env: values})
	}
	return out
}

// ToolShell: each Bash tool call runs as `<shell> -c 'source <snapshot> &&
// eval <cmd>'` (observed): not a login shell, so zsh reads ~/.zshenv; the
// snapshot restores functions, options and PATH only.
func (h *ClaudeCode) ToolShell(env map[string]string) string {
	if env["CLAUDE_CODE_SHELL"] != "" {
		return env["CLAUDE_CODE_SHELL"]
	}
	shell := env["SHELL"]
	if base := filepath.Base(shell); shell != "" && (base == "bash" || base == "zsh") {
		return shell
	}
	if p := util.Which("zsh"); p != "" {
		return p
	}
	if p := util.Which("bash"); p != "" {
		return p
	}
	return "/bin/sh"
}

func (h *ClaudeCode) commonArgs(ctx *plugin.Ctx) ([]string, error) {
	var args []string
	if m := cellStr(ctx, "model"); m != "" {
		args = append(args, "--model", m)
	}
	if e := cellStr(ctx, "effort"); e != "" {
		args = append(args, "--effort", e)
	}
	if cellConfig(ctx) == "clean" && !configuration(ctx) {
		args = append(args, "--safe-mode", "--strict-mcp-config")
	}
	args = append(args, plugin.AuthArgs(ctx)...)
	extra, err := launchArgs(ctx, h)
	if err != nil {
		return nil, err
	}
	return append(args, extra...), nil
}

// Plan chooses the session id up front so a crash mid-run still leaves a
// findable transcript.
func (h *ClaudeCode) Plan(ctx *plugin.Ctx) M {
	return M{"session_id": uuid4(), "config_isolated": h.isolated(ctx)}
}

func (h *ClaudeCode) Execute(ctx *plugin.Ctx) (M, error) {
	sid := util.S(ctx.StageState("execute")["session_id"])
	if sid == "" {
		sid = uuid4()
	}
	mode := cellStr(ctx, "permission_mode")
	if mode == "" {
		mode = "bypassPermissions"
	}
	argv := []string{launchBinary(ctx, h), "-p", "--output-format", "stream-json", "--verbose", "--session-id", sid,
		"--permission-mode", mode}
	common, err := h.commonArgs(ctx)
	if err != nil {
		return nil, err
	}
	argv = append(argv, common...)
	if budget := util.Get(ctx.Spec, "trial", "max_budget_usd"); util.Truthy(budget) {
		argv = append(argv, "--max-budget-usd", util.Str(budget))
	}
	argv = append(argv, cellList(ctx, "args")...)
	env, err := h.cleanEnv(ctx)
	if err != nil {
		return nil, err
	}
	proc, err := run(h, launchContext(ctx), argv, "execute", plugin.RunOpts{StdinData: strp(ctx.PromptText), ExtraEnv: env})
	if proc != nil {
		proc["session_id"] = sid
		proc["config_isolated"] = h.isolated(ctx)
	}
	return proc, err
}

func (h *ClaudeCode) Narrate(ctx *plugin.Ctx, prompt string) (M, error) {
	sid := util.Str(ctx.StageState("execute")["session_id"])
	argv := []string{launchBinary(ctx, h), "-p", "--resume", sid, "--fork-session", "--tools", "",
		"--output-format", "stream-json", "--verbose"}
	common, err := h.commonArgs(ctx)
	if err != nil {
		return nil, err
	}
	argv = append(argv, common...)
	env, err := h.cleanEnv(ctx)
	if err != nil {
		return nil, err
	}
	proc, err := run(h, launchContext(ctx), argv, "narrate", plugin.RunOpts{StdinData: strp(prompt), ExtraEnv: env})
	if proc == nil {
		return proc, err
	}
	proc["tools_disabled"] = true
	s := newScope()
	for _, r := range s.records(rawPath(ctx.RunDir, "narrate")) {
		if ev := r.evMap(); ev != nil && ev["type"] == "system" && ev["subtype"] == "init" {
			proc["session_id"] = ev["session_id"]
		}
	}
	return proc, err
}

func (h *ClaudeCode) Report(ctx *plugin.Ctx, prompt string, schema M) (M, error) {
	if err := ensureReporterContext(ctx); err != nil {
		return nil, err
	}
	argv := []string{"claude", "-p", "--output-format", "stream-json", "--verbose",
		"--tools", "", "--session-id", uuid4()}
	common, err := h.commonArgs(ctx)
	if err != nil {
		return nil, err
	}
	argv = append(argv, common...)
	s := newScope()
	if len(schema) > 0 {
		argv = append(argv, "--json-schema", s.dumps(schema, true))
	}
	env, err := h.cleanEnv(ctx)
	if err != nil {
		return nil, err
	}
	proc, err := run(h, ctx, argv, ctx.StageName, plugin.RunOpts{StdinData: strp(prompt), ExtraEnv: env})
	if err != nil {
		return nil, err
	}
	var text any = ""
	var structured any
	proc["is_error"] = true
	proc["result_subtype"] = "missing result"
	proc["tools_policy"] = "tools disabled"
	var results []M
	for _, r := range s.records(rawPath(ctx.RunDir, ctx.StageName)) {
		if ev := r.evMap(); ev != nil && ev["type"] == "result" {
			results = append(results, ev)
		}
	}
	if len(results) > 0 {
		agg := s.aggregateResults(results)
		last := agg.last
		text = util.Or(last["result"], "")
		structured = last["structured_output"]
		var models any
		if len(agg.modelIDs) > 0 {
			models = strings.Join(agg.modelIDs, ",")
		}
		proc["output_tokens"] = agg.outputTokens
		proc["model_ids"] = models
		proc["cost_estimate_usd"] = agg.costEstimate
		proc["is_error"] = util.Truthy(last["is_error"])
		proc["result_subtype"] = last["subtype"]
		if agg.count > 1 {
			proc["result_events"] = int64(agg.count)
		}
	}
	return M{"proc": proc, "text": text, "structured": structured}, nil
}

func (h *ClaudeCode) projectsDir(ctx *plugin.Ctx) string {
	if util.Truthy(ctx.StageState("execute")["config_isolated"]) {
		return filepath.Join(ctx.ConfigDir, "projects")
	}
	return filepath.Join(util.Home(), ".claude", "projects")
}

func idKey(v any) string {
	if s, ok := v.(string); ok {
		return "s:" + s
	}
	return "x:" + util.Compact(v)
}

func (h *ClaudeCode) Normalize(ctx *plugin.Ctx, stage string, excludeMessageIDs []string) (M, error) {
	s := newScope()
	sid := ctx.StageState(stage)["session_id"]
	out := plugin.EmptyTelemetry(M{"session_id": sid, "timestamp_source": "transcript event timestamps (harness clock)"})
	hr := out["harness_reported"].(M)
	var limitations []any
	streamToolIDs := map[string]bool{}
	var results []M
	for _, r := range s.records(rawPath(ctx.RunDir, stage)) {
		ev := r.evMap()
		if !util.Truthy(ev) {
			continue
		}
		if ev["type"] == "system" && ev["subtype"] == "init" {
			// Missing inventories are unknown, not empty and not interchangeable.
			skills, skillsKnown := ev["skills"].([]any)
			commands, commandsKnown := ev["slash_commands"].([]any)
			inventory := func(items []any, known bool) (any, any) {
				if !known {
					return nil, nil
				}
				ajx := []string{}
				for _, item := range items {
					name := s.pyStr(item)
					if strings.Contains(strings.ToLower(name), "ajx") {
						ajx = append(ajx, name)
					}
				}
				sort.Strings(ajx)
				return int64(len(items)), anyList(ajx)
			}
			hr["skills_visible"], hr["ajx_skills_visible"] = inventory(skills, skillsKnown)
			hr["slash_commands_visible"], hr["ajx_slash_commands_visible"] = inventory(commands, commandsKnown)
			hr["skills_visibility_note"] = "Session inventories do not establish skill ownership or activation."
			servers := []any{}
			for _, m := range util.List(util.Or(ev["mcp_servers"], []any{})) {
				servers = append(servers, util.MapOr(m)["name"])
			}
			hr["init_model"] = ev["model"]
			hr["claude_code_version"] = ev["claude_code_version"]
			hr["permission_mode"] = ev["permissionMode"]
			hr["api_key_source"] = ev["apiKeySource"]
			hr["mcp_servers"] = servers
			if util.Truthy(hr["ajx_skills_visible"]) || util.Truthy(hr["ajx_slash_commands_visible"]) {
				limitations = append(limitations, fmt.Sprintf("%s: AJX names were listed in session skill or slash-command inventories; "+
					"visibility does not prove activation or access to this trial", stage))
			}
		}
		if ev["type"] == "assistant" && !util.Truthy(ev["parent_tool_use_id"]) {
			for _, block := range util.List(util.Or(util.MapOr(ev["message"])["content"], []any{})) {
				if b, ok := block.(M); ok && b["type"] == "tool_use" {
					streamToolIDs[idKey(b["id"])] = true
				}
			}
		}
		if ev["type"] == "result" {
			results = append(results, ev)
		}
	}
	var agg *aggregate
	if len(results) > 0 {
		a := s.aggregateResults(results)
		agg = &a
		last := a.last
		out["final_text"] = util.Or(last["result"], "")
		out["stop_reason"] = last["subtype"]
		hr["result_subtype"] = last["subtype"]
		hr["terminal_reason"] = last["terminal_reason"]
		hr["num_turns"] = a.numTurns
		hr["duration_ms"] = a.durationMS
		hr["cost_estimate_usd"] = a.costEstimate
		hr["cost_note"] = "harness-computed estimate; not billed money"
		hr["model_usage"] = a.modelUsage
		hr["subagents_spawned"] = a.subagents
		hr["is_error"] = last["is_error"]
		hr["result_events"] = int64(a.count)
		if a.count > 1 {
			limitations = append(limitations, multiResultLimitation(stage, a))
		}
		if util.Truthy(a.safetyStops) {
			// Observed with Opus 5.5: a safeguard stops the response, Claude Code continues once, and
			// the continuation can be a refusal that replaces the requested output.
			hr["safety_stops"] = a.safetyStops
			limitations = append(limitations, fmt.Sprintf("%s: the model's safeguards stopped %s response(s); "+
				"text after a stop can be a refusal rather than the requested output", stage, s.pyStr(a.safetyStops)))
		}
	} else {
		limitations = append(limitations, fmt.Sprintf("%s: no result event in stream (killed, crashed, or timed out)", stage))
	}
	finish := func() M {
		if limitations == nil {
			limitations = []any{}
		}
		out["limitations"] = limitations
		return out
	}

	var hits []string
	if util.Truthy(sid) {
		hits, _ = filepath.Glob(filepath.Join(globEscape(h.projectsDir(ctx)), "*", globEscape(util.Str(sid))+".jsonl"))
	}
	if len(hits) == 0 {
		limitations = append(limitations, fmt.Sprintf("%s: transcript for session %s not found "+
			"(remote runner or moved config); tokens unavailable", stage, s.pyStr(sid)))
		return finish(), nil
	}
	out["transcript_path"] = hits[0]
	type message struct {
		at     any
		tokens int64
		model  any
	}
	messages := map[string]*message{}
	var order []string
	pending := map[string]M{}
	events := []any{}
	excluded := map[string]bool{}
	for _, id := range excludeMessageIDs {
		excluded[id] = true
	}
	for _, rowAny := range s.readJSONL(hits[0]) {
		row := util.MapOr(rowAny)
		if util.Truthy(row["isSidechain"]) {
			continue
		}
		msg, _ := row["message"].(M)
		ts := row["timestamp"]
		if row["type"] == "assistant" && msg != nil && util.Truthy(msg["id"]) {
			mid := s.pyStr(msg["id"])
			if excluded[mid] {
				continue
			}
			if _, ok := messages[mid]; !ok {
				messages[mid] = &message{at: ts, model: msg["model"]}
				order = append(order, mid)
			}
			messages[mid].tokens = pyIntOf(util.Or(util.MapOr(msg["usage"])["output_tokens"], int64(0)))
			for _, blockAny := range util.List(util.Or(msg["content"], []any{})) {
				block, ok := blockAny.(M)
				if !ok {
					continue
				}
				switch block["type"] {
				case "tool_use":
					call := s.toolEvent(block["id"], block["name"], block["input"], ts, msg["id"])
					pending[idKey(block["id"])] = call
					events = append(events, call)
				case "text":
					if t, _ := block["text"].(string); pyStrip(t) != "" {
						events = append(events, M{"kind": "text", "text": s.excerpt(t, 1200), "at": ts, "message_id": msg["id"]})
					}
				}
			}
		} else if row["type"] == "user" && msg != nil {
			content, isList := msg["content"].([]any)
			if !isList {
				continue
			}
			for _, blockAny := range content {
				block, ok := blockAny.(M)
				if !ok || block["type"] != "tool_result" {
					continue
				}
				call := pending[idKey(block["tool_use_id"])]
				if call == nil {
					continue
				}
				var res any = block["content"]
				if l, isL := res.([]any); isL {
					var parts []string
					for _, c := range l {
						if cm, ok := c.(M); ok {
							t, _ := util.Or(cm["text"], "").(string)
							parts = append(parts, t)
						}
					}
					res = strings.Join(parts, "\n")
				}
				call["end"] = ts
				call["is_error"] = nil
				if flag, ok := block["is_error"].(bool); ok {
					call["is_error"] = flag
				}
				call["output"] = s.excerpt(res, 1500)
			}
		}
	}
	out["events"] = events
	out["message_ids"] = anyList(order)
	usage := []any{}
	var observed int64
	models := map[string]bool{}
	for _, mid := range order {
		m := messages[mid]
		usage = append(usage, M{"id": mid, "at": m.at, "output_tokens": m.tokens})
		observed += m.tokens
		if util.Truthy(m.model) {
			models[s.pyStr(m.model)] = true
		}
	}
	out["usage"] = usage
	out["model_ids"] = anyList(sortedStrings(models))
	var expected any
	if agg != nil {
		expected = agg.outputTokens // per-segment usage, summed over every result event
	}
	if isInt(expected) && toInt64(expected) == observed {
		out["usage_status"] = "complete"
		out["expected_output_tokens"] = expected
	} else {
		out["usage_status"] = "partial"
		limitations = append(limitations, fmt.Sprintf("%s: transcript output tokens %d != stream result "+
			"%s; reported as partial", stage, observed, s.pyStr(expected)))
	}
	transcriptIDs := map[string]bool{}
	for _, e := range events {
		if em := e.(M); em["kind"] == "tool" {
			transcriptIDs[idKey(em["tool_use_id"])] = true
		}
	}
	if agg != nil && sameSet(streamToolIDs, transcriptIDs) {
		out["tool_status"] = "complete"
		out["expected_tool_calls"] = int64(len(streamToolIDs))
	} else {
		out["tool_status"] = "partial"
		limitations = append(limitations, fmt.Sprintf("%s: stream tool calls %d vs transcript %d; partial",
			stage, len(streamToolIDs), len(transcriptIDs)))
	}
	if util.Truthy(hr["subagents_spawned"]) {
		limitations = append(limitations, fmt.Sprintf("%s: subagents spawned; nested tokens/calls excluded from main totals", stage))
		if out["usage_status"] == "complete" {
			out["usage_status"] = "partial"
			out["expected_output_tokens"] = nil
		}
	}
	return finish(), nil
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// globEscape quotes glob metacharacters in a literal path.
func globEscape(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch r {
		case '*', '?', '[', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Progress is the live status from the stage's raw stream, tolerant of a file
// still being written (a partial last line parses as nothing) or not yet
// created (nil). Sub-agent blocks are included: for a status feed, what the
// worker is doing matters more than whose thread it is.
func (h *ClaudeCode) Progress(runDir, stage string, limit int) []M {
	if stage == "" {
		stage = "execute"
	}
	s := newScope()
	items := []M{}
	for _, r := range s.records(rawPath(runDir, stage)) {
		ev := r.evMap()
		if !util.Truthy(ev) {
			continue
		}
		switch ev["type"] {
		case "assistant":
			for _, blockAny := range util.List(util.Or(util.MapOr(ev["message"])["content"], []any{})) {
				block, ok := blockAny.(M)
				if !ok {
					continue
				}
				switch block["type"] {
				case "text":
					if t, _ := util.Or(block["text"], "").(string); pyStrip(t) != "" {
						items = append(items, M{"at": r.at, "kind": "text", "text": oneLine(t, 200)})
					}
				case "tool_use":
					detail := oneLine(s.summarize(block["input"]), 160)
					items = append(items, M{"at": r.at, "kind": "tool",
						"text": oneLine(s.pyStr(block["name"])+": "+detail, 200)})
				}
			}
		case "result":
			items = append(items, M{"at": r.at, "kind": "result",
				"text": fmt.Sprintf("result %s turns=%s", s.pyStr(ev["subtype"]), s.pyStr(ev["num_turns"]))})
		}
	}
	return lastN(items, limit)
}

// lastN is items[-limit:] if limit > 0 else items.
func lastN(items []M, limit int) []M {
	if limit > 0 && len(items) > limit {
		return items[len(items)-limit:]
	}
	return items
}

// ---------------------------------------------------------------- auth profiles

func registerClaudeAuth() {
	plugin.RegisterAuth("claude-subscription", "builtin", func(conf M) plugin.Auth {
		a := newSimpleAuth(conf, false)
		a.Harnesses = []string{"claude-code"}
		a.Description = "Claude.ai Pro/Max/Team login (OAuth in the user's keychain/config)"
		// a fresh CLAUDE_CONFIG_DIR has no login; clean = --safe-mode only
		a.ModelEnv = []string{"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR"}
		a.baseUnset = []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"}
		return a
	})
	plugin.RegisterAuth("anthropic-api", "builtin", func(conf M) plugin.Auth {
		a := newSimpleAuth(conf, true)
		a.Harnesses = []string{"claude-code"}
		a.Description = "Anthropic API key (ANTHROPIC_API_KEY); optional ANTHROPIC_BASE_URL gateway"
		a.RequiredEnv = []string{"ANTHROPIC_API_KEY"}
		a.ModelEnv = []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"}
		a.baseUnset = []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"}
		return a
	})
	plugin.RegisterAuth("claude-bedrock", "builtin", func(conf M) plugin.Auth {
		a := cloudauth.New(conf)
		a.Harnesses = []string{"claude-code"}
		a.Description = "Amazon Bedrock (CLAUDE_CODE_USE_BEDROCK=1 + AWS credentials/region)"
		a.RequiredEnv = []string{"AWS_REGION"}
		a.ModelEnv = append([]string{"CLAUDE_CODE_USE_BEDROCK", "ANTHROPIC_BEDROCK_*", "CLAUDE_CODE_SKIP_BEDROCK_AUTH"},
			plugin.AWSCredentialEnv...)
		a.ProviderEnv = "CLAUDE_CODE_USE_BEDROCK"
		a.CredentialRoutes = [][]string{{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}, {"AWS_BEARER_TOKEN_BEDROCK"}}
		a.OptionalEnv = []string{"AWS_SESSION_TOKEN"}
		a.UnsupportedEnv = []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE",
			"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_CONTAINER_*", "CLAUDE_CODE_SKIP_BEDROCK_AUTH"}
		a.HomeCredentials = "AWS profiles, SSO, and default provider-chain credentials"
		a.FixedEnv = map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}
		a.BaseUnset = []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"}
		return a
	})
	plugin.RegisterAuth("claude-vertex", "builtin", func(conf M) plugin.Auth {
		a := cloudauth.New(conf)
		a.Harnesses = []string{"claude-code"}
		a.Description = "Google Vertex AI (CLAUDE_CODE_USE_VERTEX=1, CLOUD_ML_REGION, ANTHROPIC_VERTEX_PROJECT_ID)"
		a.RequiredEnv = []string{"CLOUD_ML_REGION", "ANTHROPIC_VERTEX_PROJECT_ID"}
		a.ModelEnv = []string{"CLAUDE_CODE_USE_VERTEX", "ANTHROPIC_VERTEX_*", "VERTEX_REGION_*", "GOOGLE_APPLICATION_CREDENTIALS",
			"CLOUDSDK_*"}
		a.ProviderEnv = "CLAUDE_CODE_USE_VERTEX"
		a.UnsupportedEnv = []string{"GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_CONFIG", "CLOUDSDK_AUTH_*",
			"CLAUDE_CODE_SKIP_VERTEX_AUTH"}
		a.HomeCredentials = "file-backed Google Application Default Credentials (ADC)"
		a.FixedEnv = map[string]string{"CLAUDE_CODE_USE_VERTEX": "1"}
		a.BaseUnset = []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_FOUNDRY"}
		return a
	})
	plugin.RegisterAuth("claude-foundry", "builtin", func(conf M) plugin.Auth {
		a := cloudauth.New(conf)
		a.Harnesses = []string{"claude-code"}
		a.Description = "Microsoft Foundry (CLAUDE_CODE_USE_FOUNDRY=1, ANTHROPIC_FOUNDRY_RESOURCE or _BASE_URL)"
		a.ModelEnv = []string{"CLAUDE_CODE_USE_FOUNDRY", "ANTHROPIC_FOUNDRY_*", "AZURE_*"}
		a.ProviderEnv = "CLAUDE_CODE_USE_FOUNDRY"
		a.CredentialRoutes = [][]string{{"ANTHROPIC_FOUNDRY_API_KEY"}, {"AZURE_CLIENT_ID", "AZURE_TENANT_ID", "AZURE_CLIENT_SECRET"}}
		a.EndpointRoutes = []string{"ANTHROPIC_FOUNDRY_RESOURCE", "ANTHROPIC_FOUNDRY_BASE_URL"}
		a.UnsupportedEnv = []string{"AZURE_CONFIG_DIR", "AZURE_CLIENT_CERTIFICATE_PATH", "AZURE_FEDERATED_TOKEN_FILE",
			"AZURE_TOKEN_CREDENTIALS", "CLAUDE_CODE_SKIP_FOUNDRY_AUTH", "ANTHROPIC_FOUNDRY_AUTH_TOKEN"}
		a.HomeCredentials = "Azure CLI login and default identity credentials"
		a.FixedEnv = map[string]string{"CLAUDE_CODE_USE_FOUNDRY": "1"}
		a.BaseUnset = []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"}
		return a
	})
}
