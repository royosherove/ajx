"""OpenAI Codex CLI (`codex exec --json`). NOT verified live by ajx tests on this machine.

Event schema (documented JSONL): thread.started{thread_id}, turn.started, item.started/
item.updated/item.completed{item:{id,type,...}}, turn.completed{usage:{input_tokens,
cached_input_tokens,output_tokens}}, turn.failed{error}, error{message}.
Item types: agent_message{text}, reasoning, command_execution{command,aggregated_output,
exit_code,status}, file_change{changes}, mcp_tool_call{server,tool,status}, web_search{query}.
"""

import json
import os
import shutil
from pathlib import Path

from ..base import auth_args, Auth, Harness, empty_telemetry, finish_tool, stream_records, text_event, tool_event
from ..agent_configuration import (configuration, ensure_reporter_context, launch_args, launch_binary,
                                   launch_context, launch_env)
from ..plugins import register

_TOOL_ITEMS = ("command_execution", "file_change", "mcp_tool_call", "web_search", "todo_list")


@register("harness", "codex")
class Codex(Harness):
    binary = "codex"
    version_argv = ["codex", "--version"]
    can_resume = True
    can_report = True
    clean_supported = True        # per-run CODEX_HOME (no user AGENTS.md / config / MCP)
    default_auth = "codex-chatgpt"
    telemetry = "per-turn output tokens (turn.completed), command/file/mcp items; arrival timestamps"

    def _isolated(self, ctx):
        return bool(configuration(ctx)) or ctx["cell"]["config"] == "clean"

    def clean_env(self, ctx):
        if configuration(ctx):
            return launch_env(ctx, self)
        return {"CODEX_HOME": ctx["config_dir"]} if self._isolated(ctx) else {}

    def _common(self, ctx):
        cell, args = ctx["cell"], []
        if cell.get("model"):
            args += ["--model", cell["model"]]
        if cell.get("effort"):
            args += ["-c", f"model_reasoning_effort={cell['effort']}"]
        return args + auth_args(ctx) + launch_args(ctx, self)

    def execute(self, ctx):
        argv = [launch_binary(ctx, self), "exec", "--json", "--skip-git-repo-check",
                "--cd", str(ctx["workspace"])] + self._common(ctx)
        argv += ctx["cell"].get("permission_args") or ["--dangerously-bypass-approvals-and-sandbox"]
        argv += list(ctx["cell"].get("args") or []) + ["-"]
        proc = self.run(launch_context(ctx), argv, "execute", stdin_data=ctx["prompt_text"], extra_env=self.clean_env(ctx))
        proc["session_id"] = self._thread(ctx["run_dir"] / "execute.raw.jsonl")
        return proc

    @staticmethod
    def _thread(raw):
        for _, ev, _ in stream_records(raw):
            if ev and ev.get("type") == "thread.started":
                return ev.get("thread_id")
        return None

    def narrate(self, ctx, prompt):
        sid = self.stage_state(ctx, "execute").get("session_id")
        if not sid:
            return None
        if configuration(ctx):
            # --sandbox belongs to exec, not its resume subcommand in current CLIs.
            argv = [launch_binary(ctx, self), "exec", "--sandbox", "read-only", "resume", sid,
                    "--json", "--skip-git-repo-check"]
        else:
            argv = ["codex", "exec", "resume", sid, "--json", "--skip-git-repo-check", "--sandbox", "read-only"]
        argv += self._common(ctx) + ["-"]
        proc = self.run(launch_context(ctx), argv, "narrate", stdin_data=prompt, extra_env=self.clean_env(ctx))
        proc.update(session_id=sid, tools_disabled="read-only sandbox (commands may still run read-only)")
        return proc

    def report(self, ctx, prompt, schema=None):
        ensure_reporter_context(ctx)
        stage = ctx["stage_name"]
        response = ctx["run_dir"] / f"{stage}.response.txt"
        response.unlink(missing_ok=True)
        argv = ["codex", "exec", "--json", "--skip-git-repo-check", "--ephemeral",
                "--ignore-user-config", "--ignore-rules", "--sandbox", "read-only",
                "-c", 'approval_policy="never"', "--cd", str(ctx["workspace"]),
                "--output-last-message", str(response)] + self._common(ctx)
        if schema:
            schema_path = ctx["run_dir"] / f"{stage}.schema.json"
            schema_path.write_text(json.dumps(schema), encoding="utf-8")
            argv += ["--output-schema", str(schema_path)]
        proc = self.run(ctx, argv + ["-"], stage, stdin_data=prompt, extra_env=self.clean_env(ctx))
        proc["tools_policy"] = "read-only sandbox; read-only commands may still run"
        result = self.report_result(ctx, proc)
        if response.exists():
            result["text"] = response.read_text(encoding="utf-8")
        return result

    def normalize(self, ctx, stage, exclude_message_ids=()):
        out = empty_telemetry(model_ids=[ctx["cell"].get("model") or "codex default (unrecorded)"],
                              timestamp_source="ajx line-arrival timestamps",
                              limitations=["codex adapter not verified live by ajx tests"])
        items, events, usage, texts, turns_completed = {}, [], [], [], 0
        for at, ev, _ in stream_records(ctx["run_dir"] / f"{stage}.raw.jsonl"):
            if not ev:
                continue
            t = ev.get("type")
            if t == "thread.started":
                out["session_id"] = ev.get("thread_id")
            elif t in ("item.started", "item.completed"):
                item = ev.get("item") or {}
                iid, itype = item.get("id"), item.get("type") or item.get("item_type")
                if itype in _TOOL_ITEMS:
                    if iid not in items:
                        raw = {k: item.get(k) for k in ("command", "changes", "server", "tool", "query")
                               if item.get(k) is not None}
                        items[iid] = tool_event(iid, itype, raw, at)
                        events.append(items[iid])
                    if t == "item.completed":
                        failed = item.get("status") == "failed" or (item.get("exit_code") not in (None, 0))
                        finish_tool(items[iid], at, failed, item.get("aggregated_output") or item.get("result"))
                elif itype == "agent_message" and t == "item.completed":
                    events.append(text_event(item.get("text") or "", at))
                    texts.append(item.get("text") or "")
            elif t == "turn.completed":
                turns_completed += 1
                u = ev.get("usage") or {}
                usage.append({"id": f"turn-{turns_completed}", "at": at,
                              "output_tokens": int(u.get("output_tokens") or 0)})
                out["harness_reported"].setdefault("turn_usage", []).append(u)
            elif t in ("turn.failed", "error"):
                out["stop_reason"] = "error"
                out["limitations"].append(f"{stage}: {t}: {str(ev.get('error') or ev.get('message'))[:200]}")
        out["events"], out["usage"] = events, usage
        out["final_text"] = texts[-1] if texts and stage == "execute" else "\n\n".join(texts)
        out["stop_reason"] = out["stop_reason"] or ("completed" if turns_completed else None)
        if usage:
            out["usage_status"] = "partial"
            out["limitations"].append("codex usage is per turn with no independent session total; partial")
        out["tool_status"] = "partial" if events else "unavailable"
        return out


@register("auth", "codex-chatgpt")
class CodexChatGPT(Auth):
    harnesses = ("codex",)
    description = "ChatGPT sign-in (auth.json in ~/.codex); copied into an isolated CODEX_HOME for clean runs"
    model_env = ("CODEX_HOME",)

    def prepare(self, ctx):
        src = Path(self.conf.get("codex_home") or Path.home() / ".codex") / "auth.json"
        if src.exists() and ctx.get("config_dir"):
            Path(ctx["config_dir"]).mkdir(parents=True, exist_ok=True)
            target = Path(ctx["config_dir"]) / "auth.json"
            shutil.copy2(src, target)
            if getattr(ctx.get("runner"), "backend", None) == "container" and os.geteuid() == 0:
                owner = Path(ctx["config_dir"]).stat()
                os.chown(target, owner.st_uid, owner.st_gid)

    def cleanup(self, ctx):
        copied = Path(ctx.get("config_dir") or "") / "auth.json"
        if ctx.get("config_dir") and copied.exists():
            copied.unlink()

    def problems(self):
        src = Path(self.conf.get("codex_home") or Path.home() / ".codex") / "auth.json"
        return [] if src.exists() else [f"no codex login found at {src} (run `codex login`)"]


@register("auth", "openai-api")
class OpenAIApi(Auth):
    harnesses = ("codex",)
    description = "OpenAI API key (OPENAI_API_KEY); optional OPENAI_BASE_URL"
    required_env = ("OPENAI_API_KEY",)
    model_env = ("OPENAI_*",)


@register("auth", "codex-provider")
class CodexProvider(Auth):
    """Azure OpenAI, Ollama, OpenRouter, or any [model_providers.X] Codex supports.

    [auth.azure]
    type = "codex-provider"
    provider = "azure"                       # passed as -c model_provider=azure
    config = ['model_providers.azure.base_url="https://..."', 'model_providers.azure.env_key="AZURE_OPENAI_API_KEY"']
    env = { AZURE_OPENAI_API_KEY = "${AZURE_OPENAI_API_KEY}" }
    """
    harnesses = ("codex",)
    description = "custom/Azure/OSS model provider via `-c model_provider=...` overrides"

    def extra_args(self):
        args = ["-c", f"model_provider={self.conf['provider']}"] if self.conf.get("provider") else []
        for c in self.conf.get("config") or []:
            args += ["-c", c]
        return args + super().extra_args()
