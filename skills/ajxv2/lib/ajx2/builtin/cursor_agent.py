"""Cursor CLI (`cursor-agent -p --output-format stream-json`). NOT verified live by ajx2 tests.

Documented stream-json events: system{subtype:init,session_id,model}, user, assistant
{message{content[{type:text,text}]}}, tool_call{subtype:started|completed, call_id,
tool_call{<kind>ToolCall{args, result}}}, result{subtype,result,session_id,duration_ms,is_error}.
No token counts are exposed.
"""

from ..base import auth_args, Auth, Harness, empty_telemetry, finish_tool, stream_records, text_event, tool_event
from ..plugins import register


@register("harness", "cursor-agent")
class CursorAgent(Harness):
    binary = "cursor-agent"
    version_argv = ["cursor-agent", "--version"]
    can_resume = True
    clean_supported = False
    default_auth = "cursor-login"
    telemetry = "tool calls, assistant text, arrival timestamps; no tokens"

    def _common(self, ctx):
        args = ["--model", ctx["cell"]["model"]] if ctx["cell"].get("model") else []
        return args + auth_args(ctx)

    def execute(self, ctx):
        argv = ["cursor-agent", "-p", "--output-format", "stream-json"] + self._common(ctx)
        argv += ctx["cell"].get("permission_args") or ["--force"]
        argv += list(ctx["cell"].get("args") or []) + [ctx["prompt_text"]]
        proc = self.run(ctx, argv, "execute")
        proc["argv"] = self.safe_argv(ctx, argv, hide=(ctx["prompt_text"],))
        proc["session_id"] = _sid(ctx["run_dir"] / "execute.raw.jsonl")
        return proc

    def narrate(self, ctx, prompt):
        sid = self.stage_state(ctx, "execute").get("session_id")
        if not sid:
            return None
        argv = ["cursor-agent", "-p", "--output-format", "stream-json", "--resume", sid] + self._common(ctx)
        proc = self.run(ctx, argv + [prompt], "narrate")
        proc.update(argv=self.safe_argv(ctx, argv + [prompt], hide=(prompt,)), session_id=sid,
                    tools_disabled="no --force; headless writes/commands are not approved")
        return proc

    def normalize(self, ctx, stage, exclude_message_ids=()):
        out = empty_telemetry(timestamp_source="ajx2 line-arrival timestamps",
                              limitations=["cursor-agent adapter not verified live by ajx2 tests",
                                           "cursor-agent exposes no token counts"])
        calls, events = {}, []
        for at, ev, _ in stream_records(ctx["run_dir"] / f"{stage}.raw.jsonl"):
            if not ev:
                continue
            t = ev.get("type")
            if t == "system" and ev.get("subtype") == "init":
                out["session_id"] = ev.get("session_id")
                out["model_ids"] = [ev.get("model")] if ev.get("model") else []
            elif t == "assistant":
                text = "".join(c.get("text", "") for c in (ev.get("message") or {}).get("content") or []
                               if isinstance(c, dict))
                if text.strip():
                    events.append(text_event(text, at))
            elif t == "tool_call":
                cid = ev.get("call_id")
                body = ev.get("tool_call") or {}
                kind = next(iter(body), "tool")
                inner = body.get(kind) or {}
                if ev.get("subtype") == "started":
                    calls[cid] = tool_event(cid, kind.replace("ToolCall", ""), inner.get("args"), at)
                    events.append(calls[cid])
                elif ev.get("subtype") == "completed":
                    res = inner.get("result") or {}
                    finish_tool(calls.get(cid), at, "failure" in res or "error" in res, res)
            elif t == "result":
                out["stop_reason"] = ev.get("subtype")
                out["final_text"] = ev.get("result") or ""
                out["harness_reported"].update(duration_ms=ev.get("duration_ms"), is_error=ev.get("is_error"))
        out["events"] = events
        out["tool_status"] = "partial" if calls else "unavailable"
        if stage == "narrate" and not out["final_text"]:
            out["final_text"] = "\n".join(e["text"] for e in events if e["kind"] == "text")
        return out


def _sid(raw):
    for _, ev, _ in stream_records(raw):
        if ev and ev.get("session_id"):
            return ev["session_id"]
    return None


@register("auth", "cursor-login")
class CursorLogin(Auth):
    harnesses = ("cursor-agent",)
    description = "cursor-agent login (browser); `cursor-agent status` for identity"
    isolates_config = False

    def identity(self, env=None):
        self.conf.setdefault("identity_cmd", ["cursor-agent", "status"])
        return super().identity(env)


@register("auth", "cursor-api")
class CursorApi(Auth):
    harnesses = ("cursor-agent",)
    description = "Cursor API key (CURSOR_API_KEY) for headless/CI"
    required_env = ("CURSOR_API_KEY",)
