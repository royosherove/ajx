"""Google Gemini CLI (`gemini -p --output-format stream-json`). NOT verified live by ajx tests.

Documented stream-json events (each with `timestamp`): init{session_id,model},
message{role,content,delta}, tool_use{tool_name,tool_id,parameters},
tool_result{tool_id,status,output,error}, error{severity,message},
result{status,stats{total_tokens,input_tokens,output_tokens,duration_ms,tool_calls}}.
"""

from ..base import auth_args, Auth, Harness, empty_telemetry, finish_tool, stream_records, text_event, tool_event
from ..cloud_auth import CloudAuth
from ..plugins import register


@register("harness", "gemini-cli")
class GeminiCli(Harness):
    binary = "gemini"
    version_argv = ["gemini", "--version"]
    can_resume = True
    clean_supported = False       # ~/.gemini settings/GEMINI.md/extensions are always read
    default_auth = "gemini-login"
    telemetry = "session output tokens + tool-call count from result.stats; event timestamps"

    def _common(self, ctx):
        args = ["--model", ctx["cell"]["model"]] if ctx["cell"].get("model") else []
        return args + auth_args(ctx)

    def execute(self, ctx):
        argv = ["gemini", "--output-format", "stream-json"] + self._common(ctx)
        argv += ctx["cell"].get("permission_args") or ["--yolo"]
        argv += list(ctx["cell"].get("args") or []) + ["-p", ctx["prompt_text"]]
        proc = self.run(ctx, argv, "execute")
        proc["argv"] = self.safe_argv(ctx, argv, hide=(ctx["prompt_text"],))
        proc["session_id"] = _init_session(ctx["run_dir"] / "execute.raw.jsonl")
        return proc

    def narrate(self, ctx, prompt):
        sid = self.stage_state(ctx, "execute").get("session_id")
        if not sid:
            return None
        argv = ["gemini", "--output-format", "stream-json", "--resume", sid,
                "--approval-mode", "default"] + self._common(ctx)
        proc = self.run(ctx, argv + ["-p", prompt], "narrate")
        proc.update(argv=self.safe_argv(ctx, argv + ["-p", prompt], hide=(prompt,)), session_id=sid,
                    tools_disabled="approval-mode default; headless tool requests are not approved")
        return proc

    def normalize(self, ctx, stage, exclude_message_ids=()):
        out = empty_telemetry(timestamp_source="gemini event timestamps (fallback: arrival)",
                              limitations=["gemini-cli adapter not verified live by ajx tests",
                                           "gemini-cli reads ~/.gemini config; cannot be made clean"])
        calls, events, buf, buf_at, result, tool_ids = {}, [], [], None, None, set()
        for at, ev, _ in stream_records(ctx["run_dir"] / f"{stage}.raw.jsonl"):
            if not ev:
                continue
            ts = ev.get("timestamp") if isinstance(ev.get("timestamp"), str) else at
            t = ev.get("type")
            if t == "init":
                out["session_id"] = ev.get("session_id")
                if ev.get("model"):
                    out["model_ids"] = [ev["model"]]
            elif t == "message" and ev.get("role") == "assistant":
                buf_at = buf_at or ts
                buf.append(ev.get("content") or "")
                continue
            if buf and t != "message":
                events.append(text_event("".join(buf), buf_at))
                buf, buf_at = [], None
            if t == "tool_use":
                tid = ev.get("tool_id")
                tool_ids.add(tid)
                calls[tid] = tool_event(tid, ev.get("tool_name"), ev.get("parameters"), ts)
                events.append(calls[tid])
            elif t == "tool_result":
                finish_tool(calls.get(ev.get("tool_id")), ts, ev.get("status") not in ("success", None),
                            ev.get("output") or ev.get("error"))
            elif t == "result":
                result = ev
        if buf:
            events.append(text_event("".join(buf), buf_at))
        out["events"] = events
        texts = [e["text"] for e in events if e["kind"] == "text"]
        out["final_text"] = (texts[-1] if stage == "execute" else "\n".join(texts)) if texts else ""
        if result:
            stats = result.get("stats") or {}
            out["stop_reason"] = result.get("status")
            out["harness_reported"].update(stats=stats)
            if isinstance(stats.get("output_tokens"), int):
                out["usage"] = [{"id": "session", "at": result.get("timestamp") or at,
                                 "output_tokens": stats["output_tokens"]}]
                out["usage_status"], out["expected_output_tokens"] = "complete", stats["output_tokens"]
                out["limitations"].append("gemini tokens are a session total assigned to the result event")
            if isinstance(stats.get("tool_calls"), int) and stats["tool_calls"] == len(tool_ids):
                out["tool_status"], out["expected_tool_calls"] = "complete", len(tool_ids)
            else:
                out["tool_status"] = "partial" if tool_ids else "unavailable"
        else:
            out["tool_status"] = "partial" if tool_ids else "unavailable"
            out["limitations"].append(f"{stage}: no result event")
        return out


def _init_session(raw):
    for _, ev, _ in stream_records(raw):
        if ev and ev.get("type") == "init":
            return ev.get("session_id")
    return None


@register("auth", "gemini-login")
class GeminiLogin(Auth):
    harnesses = ("gemini-cli",)
    description = "Login with Google (cached OAuth in ~/.gemini)"
    isolates_config = False

    def unset(self):
        return ["GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENAI_USE_VERTEXAI"] + super().unset()


@register("auth", "gemini-api")
class GeminiApi(Auth):
    harnesses = ("gemini-cli",)
    description = "Gemini API key from AI Studio (GEMINI_API_KEY)"
    required_env = ("GEMINI_API_KEY",)

    def unset(self):
        return ["GOOGLE_GENAI_USE_VERTEXAI"] + super().unset()


@register("auth", "gemini-vertex")
class GeminiVertex(CloudAuth):
    harnesses = ("gemini-cli",)
    description = "Vertex AI (GOOGLE_GENAI_USE_VERTEXAI=true, GOOGLE_CLOUD_PROJECT, GOOGLE_CLOUD_LOCATION; ADC or GOOGLE_API_KEY)"
    required_env = ("GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION")
    model_env = ("GOOGLE_*", "CLOUDSDK_*")
    provider_env = "GOOGLE_GENAI_USE_VERTEXAI"
    credential_routes = (("GOOGLE_API_KEY",),)
    unsupported_env = ("GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_CONFIG", "CLOUDSDK_AUTH_*", "GEMINI_API_KEY")
    home_credentials = "Google Application Default Credentials (ADC)"

    def env(self):
        return {"GOOGLE_GENAI_USE_VERTEXAI": "true", **super().env()}
