"""Claude Code (`claude -p`). Verified live: stream-json + on-disk transcript reconciliation."""

import json
import shutil
import sys
import uuid
from pathlib import Path

from ..base import (AWS_CREDENTIAL_ENV, auth_args, Auth, Harness, empty_telemetry, excerpt, read_jsonl, stream_records,
                    summarize_input)
from ..agent_configuration import (configuration, ensure_reporter_context, launch_args, launch_binary,
                                   launch_context, launch_env)
from ..cloud_auth import CloudAuth
from ..plugins import register

MANAGED_SETTINGS = (Path("/Library/Application Support/ClaudeCode/managed-settings.json") if sys.platform == "darwin"
                    else Path("/etc/claude-code/managed-settings.json"))


def _settings_file_env(path):
    try:
        env = json.loads(Path(path).read_text(encoding="utf-8")).get("env")
    except (OSError, ValueError, AttributeError):
        return {}
    return {str(k): str(v) for k, v in env.items()} if isinstance(env, dict) else {}


# ----------------------------------------------------------------------------- result folding
# A `claude -p` session emits one `result` event per segment when the agent schedules its own
# continuation (ScheduleWakeup / self-resume). Observed live: num_turns, duration_ms and usage are
# per segment; total_cost_usd (and modelUsage costUSD) is a running total for the whole session.

def _number(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool)


def _sum_or_none(values):
    nums = [v for v in values if _number(v)]
    return sum(nums) if nums else None


def _running_total(values):
    """The last value of a running total is the figure. A series that ever decreases cannot be
    cumulative, so the segments are summed instead and the caller says so. Returns (value, summed)."""
    nums = [v for v in values if _number(v)]
    if not nums:
        return None, False
    if all(later >= earlier for earlier, later in zip(nums, nums[1:])):
        return nums[-1], False
    return sum(nums), True


_CUMULATIVE_USAGE = frozenset({"costUSD"})       # running session total, like total_cost_usd
_CONSTANT_USAGE = frozenset({"contextWindow"})   # a property of the model, not a counter


def _merge_model_usage(usages):
    """Merge per-result modelUsage maps by model id: counters (inputTokens, outputTokens, cache*,
    webSearchRequests, ...) are per segment and summed, costUSD follows _running_total, contextWindow
    is kept, non-numeric fields keep their first value. Returns (merged, cost_summed)."""
    merged, cost_series = {}, {}
    for usage in usages:
        if not isinstance(usage, dict):
            continue
        for model, fields in usage.items():
            slot = merged.setdefault(model, {})
            if not isinstance(fields, dict):
                continue
            for key, value in fields.items():
                if not _number(value):
                    slot.setdefault(key, value)
                elif key in _CUMULATIVE_USAGE:
                    cost_series.setdefault((model, key), []).append(value)
                elif key in _CONSTANT_USAGE:
                    slot[key] = max(slot.get(key, value), value)
                else:
                    slot[key] = slot.get(key, 0) + value
    summed = False
    for (model, key), values in cost_series.items():
        merged[model][key], was_summed = _running_total(values)
        summed = summed or was_summed
    return merged, summed


def _aggregate_results(results):
    """Fold the stream's result events (in order) into one record. Text, subtype and error state come
    from the last event; turns, duration, usage and spawned subagents are summed; cost is the running
    session total. With a single event every figure is that event's own."""
    last = results[-1]
    cost, cost_summed = _running_total([r.get("total_cost_usd") for r in results])
    model_usage, model_cost_summed = last.get("modelUsage"), False
    if len(results) > 1:
        model_usage, model_cost_summed = _merge_model_usage([r.get("modelUsage") for r in results])
    model_ids = []
    for r in results:
        model_ids += [m for m in (r.get("modelUsage") or {}) if m not in model_ids]
    return {"last": last, "count": len(results), "model_usage": model_usage, "model_ids": model_ids,
            "num_turns": _sum_or_none([r.get("num_turns") for r in results]),
            "duration_ms": _sum_or_none([r.get("duration_ms") for r in results]),
            "cost_estimate_usd": cost, "cost_summed": cost_summed, "model_cost_summed": model_cost_summed,
            "subagents_spawned": _sum_or_none([(r.get("subagent_stats") or {}).get("spawned") for r in results]),
            "safety_stops": _sum_or_none([r.get("safety_stops") for r in results]),
            "output_tokens": _sum_or_none([(r.get("usage") or {}).get("output_tokens") for r in results])}


def _multi_result_limitation(stage, agg):
    summed = [name for name, flag in (("total_cost_usd", agg["cost_summed"]),
                                      ("modelUsage costUSD", agg["model_cost_summed"])) if flag]
    cost = (f"{' and '.join(summed)} decreased between segments, so cost was summed from non-cumulative segments"
            if summed else "cost taken as the cumulative session total")
    return (f"{stage}: {agg['count']} result events in one session (self-resumed/scheduled continuation); "
            f"turns, duration and usage summed across segments; {cost}")


def _one_line(text, limit):
    text = " ".join(str(text).split())
    return text if len(text) <= limit else text[:limit - 3].rstrip() + "..."


@register("harness", "claude-code")
class ClaudeCode(Harness):
    binary = "claude"
    version_argv = ["claude", "--version"]
    can_resume = True
    can_report = True
    clean_supported = True
    verified_live = True
    telemetry = "per-message output tokens (reconciled), tool calls (reconciled), event timestamps"
    # Session-coupling vars present when ajx itself runs inside Claude Code. Provider/auth
    # vars are left to the auth profile.
    strip_env = ("CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_CHILD_SESSION",
                 "CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_PID", "CLAUDE_CODE_MESSAGING_SOCKET",
                 "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_EFFORT",
                 "CLAUDE_CODE_EXECPATH", "CLAUDE_CODE_SAFE_MODE", "CLAUDE_CODE_SIMPLE")

    def _isolated(self, ctx):
        if configuration(ctx):
            return True
        auth = ctx.get("auth")
        return ctx["cell"]["config"] == "clean" and (auth is None or auth.isolates_config)

    def clean_env(self, ctx):
        if configuration(ctx):
            return launch_env(ctx, self)
        env = {"DISABLE_AUTOUPDATER": "1"}
        if self._isolated(ctx):
            env["CLAUDE_CONFIG_DIR"] = ctx["config_dir"]
        return env

    managed_settings = MANAGED_SETTINGS
    tool_shell_observed = True
    tool_shell_restores = ("PATH",)   # from the shell snapshot, before every Bash call

    def settings_env(self, env, project_dir=None):
        """Claude Code applies `env` from its settings over the process env (observed with
        --safe-mode too, which is why a trial [env] value can lose): user settings in
        $CLAUDE_CONFIG_DIR (default ~/.claude), project and local settings, managed settings last."""
        user_dir = Path(env["CLAUDE_CONFIG_DIR"]) if env.get("CLAUDE_CONFIG_DIR") else Path.home() / ".claude"
        files = [user_dir / "settings.json"]
        if project_dir:
            files += [Path(project_dir) / ".claude" / "settings.json", Path(project_dir) / ".claude" / "settings.local.json"]
        files.append(Path(self.managed_settings))
        home = str(Path.home()) + "/"
        return [("~/" + str(p)[len(home):] if str(p).startswith(home) else str(p), values)
                for p in files if (values := _settings_file_env(p))]

    def tool_shell(self, env):
        """Each Bash tool call runs as `<shell> -c 'source <snapshot> && eval <cmd>'` (observed): not a
        login shell, so zsh reads ~/.zshenv; the snapshot restores functions, options and PATH only."""
        if env.get("CLAUDE_CODE_SHELL"):
            return env["CLAUDE_CODE_SHELL"]
        shell = env.get("SHELL") or ""
        if Path(shell).name in ("bash", "zsh"):
            return shell
        return shutil.which("zsh") or shutil.which("bash") or "/bin/sh"

    def _common(self, ctx):
        cell, args = ctx["cell"], []
        if cell.get("model"):
            args += ["--model", cell["model"]]
        if cell.get("effort"):
            args += ["--effort", cell["effort"]]
        if cell["config"] == "clean" and not configuration(ctx):
            args += ["--safe-mode", "--strict-mcp-config"]
        return args + auth_args(ctx) + launch_args(ctx, self)

    def plan(self, ctx):
        """Session id is chosen up front so a crash mid-run still leaves a findable transcript."""
        return {"session_id": str(uuid.uuid4()), "config_isolated": self._isolated(ctx)}

    def execute(self, ctx):
        sid = self.stage_state(ctx, "execute").get("session_id") or str(uuid.uuid4())
        argv = [launch_binary(ctx, self), "-p", "--output-format", "stream-json", "--verbose", "--session-id", sid,
                "--permission-mode", ctx["cell"].get("permission_mode") or "bypassPermissions"]
        argv += self._common(ctx)
        budget = ctx["spec"]["trial"].get("max_budget_usd")
        if budget:
            argv += ["--max-budget-usd", str(budget)]
        argv += list(ctx["cell"].get("args") or [])
        proc = self.run(launch_context(ctx), argv, "execute", stdin_data=ctx["prompt_text"], extra_env=self.clean_env(ctx))
        proc.update(session_id=sid, config_isolated=self._isolated(ctx))
        return proc

    def narrate(self, ctx, prompt):
        sid = self.stage_state(ctx, "execute").get("session_id")
        argv = [launch_binary(ctx, self), "-p", "--resume", sid, "--fork-session", "--tools", "",
                "--output-format", "stream-json", "--verbose"] + self._common(ctx)
        proc = self.run(launch_context(ctx), argv, "narrate", stdin_data=prompt, extra_env=self.clean_env(ctx))
        proc.update(tools_disabled=True)
        for _, ev, _ in stream_records(ctx["run_dir"] / "narrate.raw.jsonl"):
            if ev and ev.get("type") == "system" and ev.get("subtype") == "init":
                proc["session_id"] = ev.get("session_id")
        return proc

    def report(self, ctx, prompt, schema=None):
        ensure_reporter_context(ctx)
        argv = ["claude", "-p", "--output-format", "stream-json", "--verbose",
                "--tools", "", "--session-id", str(uuid.uuid4())] + self._common(ctx)
        if schema:
            argv += ["--json-schema", json.dumps(schema)]
        proc = self.run(ctx, argv, ctx["stage_name"], stdin_data=prompt,
                        extra_env=self.clean_env(ctx))
        text, structured = "", None
        proc.update(is_error=True, result_subtype="missing result", tools_policy="tools disabled")
        results = [ev for _, ev, _ in stream_records(ctx["run_dir"] / f"{ctx['stage_name']}.raw.jsonl")
                   if ev and ev.get("type") == "result"]
        if results:
            agg = _aggregate_results(results)
            last = agg["last"]
            text, structured = last.get("result") or "", last.get("structured_output")
            proc.update(output_tokens=agg["output_tokens"], model_ids=",".join(agg["model_ids"]) or None,
                        cost_estimate_usd=agg["cost_estimate_usd"],
                        is_error=bool(last.get("is_error")), result_subtype=last.get("subtype"))
            if agg["count"] > 1:
                proc["result_events"] = agg["count"]
        return {"proc": proc, "text": text, "structured": structured}

    def _projects_dir(self, ctx):
        isolated = self.stage_state(ctx, "execute").get("config_isolated")
        base = Path(ctx["config_dir"]) if isolated else Path.home() / ".claude"
        return base / "projects"

    def normalize(self, ctx, stage, exclude_message_ids=()):
        raw = ctx["run_dir"] / f"{stage}.raw.jsonl"
        sid = self.stage_state(ctx, stage).get("session_id")
        out = empty_telemetry(session_id=sid, timestamp_source="transcript event timestamps (harness clock)")
        stream_tool_ids, results = set(), []
        for _, ev, _ in stream_records(raw):
            if not ev:
                continue
            if ev.get("type") == "system" and ev.get("subtype") == "init":
                commands = [str(c) for c in ev.get("slash_commands") or []]
                out["harness_reported"].update({
                    "init_model": ev.get("model"), "claude_code_version": ev.get("claude_code_version"),
                    "permission_mode": ev.get("permissionMode"), "api_key_source": ev.get("apiKeySource"),
                    "skills_visible": len(commands),
                    "ajx_skills_visible": sorted(c for c in commands if "ajx" in c.lower()),
                    "mcp_servers": [m.get("name") for m in ev.get("mcp_servers") or []]})
                if out["harness_reported"]["ajx_skills_visible"]:
                    out["limitations"].append(
                        f"{stage}: AJX skill(s) {out['harness_reported']['ajx_skills_visible']} were listed in the "
                        "worker's session (user config); the run is evaluation-aware")
            if ev.get("type") == "assistant" and not ev.get("parent_tool_use_id"):
                for block in (ev.get("message") or {}).get("content") or []:
                    if isinstance(block, dict) and block.get("type") == "tool_use":
                        stream_tool_ids.add(block.get("id"))
            if ev.get("type") == "result":
                results.append(ev)
        agg = _aggregate_results(results) if results else None
        if agg:
            last = agg["last"]
            out["final_text"] = last.get("result") or ""
            out["stop_reason"] = last.get("subtype")
            out["harness_reported"].update({
                "result_subtype": last.get("subtype"), "terminal_reason": last.get("terminal_reason"),
                "num_turns": agg["num_turns"], "duration_ms": agg["duration_ms"],
                "cost_estimate_usd": agg["cost_estimate_usd"],
                "cost_note": "harness-computed estimate; not billed money",
                "model_usage": agg["model_usage"], "subagents_spawned": agg["subagents_spawned"],
                "is_error": last.get("is_error"), "result_events": agg["count"]})
            if agg["count"] > 1:
                out["limitations"].append(_multi_result_limitation(stage, agg))
            if agg["safety_stops"]:
                # Observed with Opus 5.5: a safeguard stops the response, Claude Code continues once, and
                # the continuation can be a refusal that replaces the requested output.
                out["harness_reported"]["safety_stops"] = agg["safety_stops"]
                out["limitations"].append(f"{stage}: the model's safeguards stopped {agg['safety_stops']} "
                                          "response(s); text after a stop can be a refusal rather than the "
                                          "requested output")
        else:
            out["limitations"].append(f"{stage}: no result event in stream (killed, crashed, or timed out)")

        hits = list(self._projects_dir(ctx).glob(f"*/{sid}.jsonl")) if sid else []
        if not hits:
            out["limitations"].append(f"{stage}: transcript for session {sid} not found "
                                      "(remote runner or moved config); tokens unavailable")
            return out
        out["transcript_path"] = str(hits[0])
        messages, order, pending, events = {}, [], {}, []
        excluded = set(exclude_message_ids)
        for row in read_jsonl(hits[0]):
            if row.get("isSidechain"):
                continue
            msg = row.get("message") if isinstance(row.get("message"), dict) else None
            ts = row.get("timestamp")
            if row.get("type") == "assistant" and msg and msg.get("id"):
                mid = msg["id"]
                if mid in excluded:
                    continue
                if mid not in messages:
                    messages[mid] = {"at": ts, "output_tokens": 0, "model": msg.get("model")}
                    order.append(mid)
                messages[mid]["output_tokens"] = int((msg.get("usage") or {}).get("output_tokens") or 0)
                for block in msg.get("content") or []:
                    if not isinstance(block, dict):
                        continue
                    if block.get("type") == "tool_use":
                        call = {"kind": "tool", "tool_use_id": block.get("id"), "name": block.get("name"),
                                "input": summarize_input(block.get("input")), "input_raw": block.get("input"),
                                "at": ts, "end": None, "is_error": None, "output": "", "message_id": mid}
                        pending[block.get("id")] = call
                        events.append(call)
                    elif block.get("type") == "text" and block.get("text", "").strip():
                        events.append({"kind": "text", "text": excerpt(block["text"], 1200), "at": ts,
                                       "message_id": mid})
            elif row.get("type") == "user" and msg and isinstance(msg.get("content"), list):
                for block in msg["content"]:
                    if isinstance(block, dict) and block.get("type") == "tool_result":
                        call = pending.get(block.get("tool_use_id"))
                        if call:
                            content = block.get("content")
                            if isinstance(content, list):
                                content = "\n".join(c.get("text", "") for c in content if isinstance(c, dict))
                            call.update(end=ts, is_error=bool(block.get("is_error")),
                                        output=excerpt(content, 1500))
        out["events"], out["message_ids"] = events, order
        out["usage"] = [{"id": m, "at": messages[m]["at"], "output_tokens": messages[m]["output_tokens"]}
                        for m in order]
        out["model_ids"] = sorted({m["model"] for m in messages.values() if m.get("model")})
        observed = sum(u["output_tokens"] for u in out["usage"])
        expected = agg["output_tokens"] if agg else None   # per-segment usage, summed over every result event
        if isinstance(expected, int) and expected == observed:
            out["usage_status"], out["expected_output_tokens"] = "complete", expected
        else:
            out["usage_status"] = "partial"
            out["limitations"].append(f"{stage}: transcript output tokens {observed} != stream result "
                                      f"{expected}; reported as partial")
        transcript_ids = {e["tool_use_id"] for e in events if e["kind"] == "tool"}
        if agg and stream_tool_ids == transcript_ids:
            out["tool_status"], out["expected_tool_calls"] = "complete", len(stream_tool_ids)
        else:
            out["tool_status"] = "partial"
            out["limitations"].append(f"{stage}: stream tool calls {len(stream_tool_ids)} vs transcript "
                                      f"{len(transcript_ids)}; partial")
        if out["harness_reported"].get("subagents_spawned"):
            out["limitations"].append(f"{stage}: subagents spawned; nested tokens/calls excluded from main totals")
            if out["usage_status"] == "complete":
                out.update(usage_status="partial", expected_output_tokens=None)
        return out

    def progress(self, run_dir, stage="execute", limit=10):
        """Live status from the stage's raw stream, tolerant of a file still being written (a partial
        last line parses as nothing) or not yet created ([]). Sub-agent blocks are included: for a
        status feed, what the worker is doing matters more than whose thread it is."""
        items = []
        try:
            for at, ev, _ in stream_records(Path(run_dir) / f"{stage}.raw.jsonl"):
                if not ev:
                    continue
                if ev.get("type") == "assistant":
                    for block in (ev.get("message") or {}).get("content") or []:
                        if not isinstance(block, dict):
                            continue
                        if block.get("type") == "text" and (block.get("text") or "").strip():
                            items.append({"at": at, "kind": "text", "text": _one_line(block["text"], 200)})
                        elif block.get("type") == "tool_use":
                            detail = _one_line(summarize_input(block.get("input")), 160)
                            items.append({"at": at, "kind": "tool",
                                          "text": _one_line(f"{block.get('name')}: {detail}", 200)})
                elif ev.get("type") == "result":
                    items.append({"at": at, "kind": "result",
                                  "text": f"result {ev.get('subtype')} turns={ev.get('num_turns')}"})
        except (OSError, UnicodeDecodeError):
            pass
        return items[-limit:] if limit and limit > 0 else items


# ----------------------------------------------------------------------------- auth profiles

@register("auth", "claude-subscription")
class ClaudeSubscription(Auth):
    harnesses = ("claude-code",)
    description = "Claude.ai Pro/Max/Team login (OAuth in the user's keychain/config)"
    isolates_config = False   # a fresh CLAUDE_CONFIG_DIR has no login; clean = --safe-mode only
    model_env = ("CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR")

    def unset(self):
        return ["ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
                "CLAUDE_CODE_USE_FOUNDRY"] + super().unset()


@register("auth", "anthropic-api")
class AnthropicApi(Auth):
    harnesses = ("claude-code",)
    description = "Anthropic API key (ANTHROPIC_API_KEY); optional ANTHROPIC_BASE_URL gateway"
    required_env = ("ANTHROPIC_API_KEY",)
    model_env = ("ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN")

    def unset(self):
        return ["CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"] + super().unset()


@register("auth", "claude-bedrock")
class ClaudeBedrock(CloudAuth):
    harnesses = ("claude-code",)
    description = "Amazon Bedrock (CLAUDE_CODE_USE_BEDROCK=1 + AWS credentials/region)"
    required_env = ("AWS_REGION",)
    model_env = ("CLAUDE_CODE_USE_BEDROCK", "ANTHROPIC_BEDROCK_*", "CLAUDE_CODE_SKIP_BEDROCK_AUTH") + AWS_CREDENTIAL_ENV
    provider_env = "CLAUDE_CODE_USE_BEDROCK"
    credential_routes = (("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"), ("AWS_BEARER_TOKEN_BEDROCK",))
    optional_env = ("AWS_SESSION_TOKEN",)
    unsupported_env = ("AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE",
                       "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_CONTAINER_*",
                       "CLAUDE_CODE_SKIP_BEDROCK_AUTH")
    home_credentials = "AWS profiles, SSO, and default provider-chain credentials"

    def env(self):
        return {"CLAUDE_CODE_USE_BEDROCK": "1", **super().env()}

    def unset(self):
        return ["ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"] + super().unset()


@register("auth", "claude-vertex")
class ClaudeVertex(CloudAuth):
    harnesses = ("claude-code",)
    description = "Google Vertex AI (CLAUDE_CODE_USE_VERTEX=1, CLOUD_ML_REGION, ANTHROPIC_VERTEX_PROJECT_ID)"
    required_env = ("CLOUD_ML_REGION", "ANTHROPIC_VERTEX_PROJECT_ID")
    model_env = ("CLAUDE_CODE_USE_VERTEX", "ANTHROPIC_VERTEX_*", "VERTEX_REGION_*", "GOOGLE_APPLICATION_CREDENTIALS",
                 "CLOUDSDK_*")
    provider_env = "CLAUDE_CODE_USE_VERTEX"
    unsupported_env = ("GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_CONFIG", "CLOUDSDK_AUTH_*",
                       "CLAUDE_CODE_SKIP_VERTEX_AUTH")
    home_credentials = "file-backed Google Application Default Credentials (ADC)"

    def env(self):
        return {"CLAUDE_CODE_USE_VERTEX": "1", **super().env()}

    def unset(self):
        return ["ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_FOUNDRY"] + super().unset()


@register("auth", "claude-foundry")
class ClaudeFoundry(CloudAuth):
    harnesses = ("claude-code",)
    description = "Microsoft Foundry (CLAUDE_CODE_USE_FOUNDRY=1, ANTHROPIC_FOUNDRY_RESOURCE or _BASE_URL)"
    model_env = ("CLAUDE_CODE_USE_FOUNDRY", "ANTHROPIC_FOUNDRY_*", "AZURE_*")
    provider_env = "CLAUDE_CODE_USE_FOUNDRY"
    credential_routes = (("ANTHROPIC_FOUNDRY_API_KEY",), ("AZURE_CLIENT_ID", "AZURE_TENANT_ID", "AZURE_CLIENT_SECRET"))
    endpoint_routes = ("ANTHROPIC_FOUNDRY_RESOURCE", "ANTHROPIC_FOUNDRY_BASE_URL")
    unsupported_env = ("AZURE_CONFIG_DIR", "AZURE_CLIENT_CERTIFICATE_PATH", "AZURE_FEDERATED_TOKEN_FILE",
                       "AZURE_TOKEN_CREDENTIALS", "CLAUDE_CODE_SKIP_FOUNDRY_AUTH", "ANTHROPIC_FOUNDRY_AUTH_TOKEN")
    home_credentials = "Azure CLI login and default identity credentials"

    def env(self):
        return {"CLAUDE_CODE_USE_FOUNDRY": "1", **super().env()}

    def unset(self):
        return ["ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"] + super().unset()
