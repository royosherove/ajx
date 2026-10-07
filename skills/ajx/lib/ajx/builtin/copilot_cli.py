"""GitHub Copilot CLI (`copilot -p`). NOT verified live by ajx tests.

Headless output is plain text, so telemetry is wall clock + stdout. Pass a session-capable
flag through cell.args if your Copilot CLI version offers JSON output; or subclass this
adapter in a plugin to parse its session logs (~/.copilot/session-state/).
"""

from ..base import auth_args, Auth, Harness, empty_telemetry, read_jsonl
from ..plugins import register


@register("harness", "copilot-cli")
class CopilotCli(Harness):
    binary = "copilot"
    version_argv = ["copilot", "--version"]
    can_resume = False            # resume needs an interactive picker or a session id we cannot read
    clean_supported = False
    default_auth = "github-login"
    telemetry = "wall clock and stdout only"

    def execute(self, ctx):
        argv = ["copilot", "-p", ctx["prompt_text"]]
        if ctx["cell"].get("model"):
            argv += ["--model", ctx["cell"]["model"]]
        argv += ctx["cell"].get("permission_args") or ["--allow-all-tools"]
        argv += auth_args(ctx) + list(ctx["cell"].get("args") or [])
        proc = self.run(ctx, argv, "execute")
        proc["argv"] = self.safe_argv(ctx, argv, hide=(ctx["prompt_text"],))
        proc["session_id"] = None
        return proc

    def normalize(self, ctx, stage, exclude_message_ids=()):
        lines = [r.get("line", "") for r in read_jsonl(ctx["run_dir"] / f"{stage}.raw.jsonl")]
        return empty_telemetry(model_ids=[ctx["cell"].get("model") or "copilot default (unrecorded)"],
                               final_text="\n".join(lines[-80:]),
                               timestamp_source="ajx process start/stop and stdout arrival",
                               limitations=["copilot-cli adapter not verified live by ajx tests",
                                            "copilot-cli headless output has no tool/token telemetry",
                                            "no self-narration: journey is a labeled reconstruction"])


@register("auth", "github-login")
class GitHubLogin(Auth):
    harnesses = ("copilot-cli",)
    description = "Copilot CLI /login (GitHub OAuth)"
    isolates_config = False


@register("auth", "github-token")
class GitHubToken(Auth):
    harnesses = ("copilot-cli",)
    description = "Fine-grained PAT with Copilot Requests permission (COPILOT_GITHUB_TOKEN, GH_TOKEN or GITHUB_TOKEN)"
    model_env = ("COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN")

    def problems(self):
        import os
        env = {**os.environ, **self.env()}
        ok = any(env.get(v) for v in ("COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"))
        return [] if ok else ["none of COPILOT_GITHUB_TOKEN / GH_TOKEN / GITHUB_TOKEN is set"]
