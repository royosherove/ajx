"""Kiro CLI (`kiro-cli chat --output-format stream-json`). Verified live: ACP event stream."""

from ..base import Auth, Harness, auth_args, empty_telemetry, finish_tool, stream_records, text_event, tool_event
from ..agent_configuration import (configuration, ensure_reporter_context, launch_args, launch_binary,
                                   launch_context, launch_env)
from ..plugins import register


def _restricted_auth_args(ctx):
    """Keep provider/model options without granting tools during reporting."""
    args, restricted, i = auth_args(ctx), [], 0
    while i < len(args):
        flag, equals, _ = args[i].partition("=")
        if flag == "--trust-tools":
            if not equals and i + 1 < len(args) and not args[i + 1].startswith("-"):
                i += 1  # Remove the split-form tool list as well as its flag.
        elif flag != "--trust-all-tools":
            restricted.append(args[i])
        i += 1
    return restricted


@register("harness", "kiro-cli")
class KiroCli(Harness):
    binary = "kiro-cli"
    version_argv = ["kiro-cli", "--version"]
    can_resume = True
    can_report = True
    clean_supported = False
    verified_live = True
    default_auth = "kiro-login"
    telemetry = "tool calls (no independent total), credits, arrival timestamps; no tokens"

    def clean_env(self, ctx):
        return launch_env(ctx, self)

    def execute(self, ctx):
        cell = ctx["cell"]
        argv = [launch_binary(ctx, self), "chat", "--output-format", "stream-json", "--trust-all-tools"]
        if cell.get("model"):
            argv += ["--model", cell["model"]]
        if cell.get("effort"):
            argv += ["--effort", cell["effort"]]
        if configuration(ctx):
            argv += auth_args(ctx)
        argv += list(cell.get("args") or []) + launch_args(ctx, self) + [ctx["prompt_text"]]
        proc = self.run(launch_context(ctx), argv, "execute", extra_env=self.clean_env(ctx))
        proc["argv"] = self.safe_argv(ctx, argv, hide=(ctx["prompt_text"],))
        proc["session_id"] = self._session_id(ctx["run_dir"] / "execute.raw.jsonl")
        return proc

    @staticmethod
    def _session_id(raw):
        for _, ev, _ in stream_records(raw):
            sid = ((ev or {}).get("data") or {}).get("sessionId")
            if sid:
                return sid
        return None

    def narrate(self, ctx, prompt):
        sid = self.stage_state(ctx, "execute").get("session_id")
        if not sid:
            return None
        argv = [launch_binary(ctx, self), "chat", "--output-format", "stream-json", "--resume-id", sid, "--trust-tools="]
        if ctx["cell"].get("model"):
            argv += ["--model", ctx["cell"]["model"]]
        if configuration(ctx):
            argv += _restricted_auth_args(ctx)
        argv += launch_args(ctx, self)
        proc = self.run(launch_context(ctx), argv + [prompt], "narrate", extra_env=self.clean_env(ctx))
        proc.update(argv=self.safe_argv(ctx, argv + [prompt], hide=(prompt,)), session_id=sid,
                    tools_disabled="untrusted (--trust-tools=); headless tool requests are denied")
        return proc

    def report(self, ctx, prompt, schema=None):
        ensure_reporter_context(ctx)
        argv = ["kiro-cli", "chat", "--no-interactive", "--output-format", "stream-json",
                "--trust-tools="]
        if ctx["cell"].get("model"):
            argv += ["--model", ctx["cell"]["model"]]
        if ctx["cell"].get("effort"):
            argv += ["--effort", ctx["cell"]["effort"]]
        argv += _restricted_auth_args(ctx)
        proc = self.run(ctx, argv + [prompt], ctx["stage_name"])
        proc.update(argv=self.safe_argv(ctx, argv + [prompt], hide=(prompt,)),
                    tools_policy="no trusted tools; headless tool requests are denied")
        return self.report_result(ctx, proc)

    def normalize(self, ctx, stage, exclude_message_ids=()):
        out = empty_telemetry(
            model_ids=[ctx["cell"].get("model") or "auto"], tool_status="partial",
            harness_reported={"credits": 0.0, "credit_note": "kiro meteringUsage, summed; harness-reported"},
            timestamp_source="ajx line-arrival timestamps (kiro stream has no event times)",
            limitations=["kiro-cli exposes no token counts; output tokens unavailable",
                         "kiro tool calls have no independent expected total; tool_status partial",
                         ("kiro profile uses isolated KIRO_HOME and explicit V2 agent resources; "
                          "built-in/managed sources and actual skill use remain unverified"
                          if configuration(ctx) else
                          "kiro loads ~/.kiro steering/agents/MCP; config cannot be made clean")])
        calls, events, buf, buf_at, full_texts = {}, [], [], None, []
        for at, ev, _ in stream_records(ctx["run_dir"] / f"{stage}.raw.jsonl"):
            if not ev:
                continue
            data = ev.get("data") or {}
            out["session_id"] = out["session_id"] or data.get("sessionId")
            if ev.get("type") == "metadata":
                out["harness_reported"]["credits"] += sum(float(m.get("value") or 0)
                                                          for m in data.get("meteringUsage") or [])
                if data.get("contextUsagePercentage") is not None:
                    out["harness_reported"]["context_usage_percentage_last"] = data["contextUsagePercentage"]
            if ev.get("type") == "runFinished":
                out["stop_reason"] = data.get("stopReason") or data.get("status")
                out["final_text"] = data.get("finalText") or ""
                out["harness_reported"]["final_text_truncated"] = data.get("finalTextTruncated")
            upd = data.get("update") or {}
            kind = upd.get("sessionUpdate")
            if kind == "agent_message_chunk":
                buf_at = buf_at or at
                buf.append((upd.get("content") or {}).get("text") or "")
                continue
            if buf and kind in ("tool_call", "tool_call_update"):
                events.append(text_event("".join(buf), buf_at))
                full_texts.append("".join(buf))
                buf, buf_at = [], None
            if kind == "tool_call":
                tid = upd.get("toolCallId")
                name = ((upd.get("_meta") or {}).get("kiro") or {}).get("toolName") or upd.get("kind")
                calls[tid] = tool_event(tid, name, upd.get("rawInput") or upd.get("title"), at)
                events.append(calls[tid])
            elif kind == "tool_call_update":
                call = calls.get(upd.get("toolCallId"))
                if call:
                    for c in upd.get("content") or []:
                        text = ((c or {}).get("content") or {}).get("text")
                        if text:
                            call["output"] = (call["output"] + text)[-1500:]
                    if upd.get("status") in ("completed", "failed"):
                        finish_tool(call, at, upd["status"] == "failed", None)
        if buf:
            events.append(text_event("".join(buf), buf_at))
            full_texts.append("".join(buf))
        # kiro's runFinished.finalText concatenates every assistant message of the run; the declared
        # outcome is the last message, the narration is all of them
        if full_texts:
            out["harness_reported"]["final_text_concatenated"] = out["final_text"]
            out["final_text"] = full_texts[-1] if stage == "execute" else "\n\n".join(full_texts)
        out["events"] = events
        if not out["stop_reason"]:
            out["limitations"].append(f"{stage}: no runFinished event (killed, crashed, or timed out)")
        return out


@register("auth", "kiro-login")
class KiroLogin(Auth):
    harnesses = ("kiro-cli",)
    description = "kiro-cli login (AWS Builder ID or IAM Identity Center); `kiro-cli whoami` for identity"
    isolates_config = False
    model_env = ("KIRO_API_KEY",)

    def identity(self, env=None):
        self.conf.setdefault("identity_cmd", ["kiro-cli", "whoami"])
        return super().identity(env)
