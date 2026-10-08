"""Harness-agnostic plugins: inherit/env auth, declarative harness, runners, checks."""

import os
import re
import stat
import urllib.error
import urllib.request
import uuid
from contextlib import ExitStack
from pathlib import Path

from ..base import (AWS_CREDENTIAL_ENV, Auth, Check, Harness, Runner, auth_args, empty_telemetry, fill, read_jsonl,
                    resolve, version_of, which)
from ..plugins import names as plugin_names, register
from ..util import context_shell, fill_all, run_shell


# ----------------------------------------------------------------------------- auth

@register("auth", "inherit")
class InheritAuth(Auth):
    """Default: the worker sees the caller's environment and the harness's own stored login.

    Config isolation is off by default because a login stored in the harness's home directory
    (Claude subscription, Kiro, Cursor...) is not visible from a fresh config dir. If your provider
    is env-driven (Bedrock, Vertex, API keys), set `isolates_config = true` in an [auth.*] profile
    of type "inherit" or use the matching provider type instead.
    """
    harnesses = ("*",)
    isolates_config = False

    # (marker var, provider label, harnesses that read it, other vars that provider reads)
    _HINTS = (("CLAUDE_CODE_USE_BEDROCK", "claude via Amazon Bedrock (AWS credentials)", ("claude-code",),
               ("ANTHROPIC_BEDROCK_*", "CLAUDE_CODE_SKIP_BEDROCK_AUTH") + AWS_CREDENTIAL_ENV),
              ("CLAUDE_CODE_USE_VERTEX", "claude via Google Vertex AI", ("claude-code",),
               ("CLOUD_ML_REGION", "ANTHROPIC_VERTEX_*", "VERTEX_REGION_*", "GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_*")),
              ("CLAUDE_CODE_USE_FOUNDRY", "claude via Microsoft Foundry", ("claude-code",), ("ANTHROPIC_FOUNDRY_*", "AZURE_*")),
              ("ANTHROPIC_API_KEY", "Anthropic API key", ("claude-code",), ("ANTHROPIC_BASE_URL",)),
              ("OPENAI_API_KEY", "OpenAI API key", ("codex",), ("OPENAI_*",)),
              ("GEMINI_API_KEY", "Gemini API key", ("gemini-cli",), ()),
              ("GOOGLE_GENAI_USE_VERTEXAI", "Gemini via Vertex AI", ("gemini-cli",), ("GOOGLE_*", "CLOUDSDK_*")),
              ("CURSOR_API_KEY", "Cursor API key", ("cursor-agent",), ()),
              ("COPILOT_GITHUB_TOKEN", "GitHub token for Copilot", ("copilot-cli",), ("GH_TOKEN", "GITHUB_TOKEN")))

    def _hints(self, env=None):
        env = os.environ if env is None else env
        known = self.harness in plugin_names("harness")   # a declarative adapter may read any of them
        return [h for h in self._HINTS if env.get(h[0]) and (not self.harness or not known or self.harness in h[2])]

    def provider_hint(self, env=None):
        return "; ".join(h[1] for h in self._hints(env)) or "no provider env vars set: harness stored login, if any"

    def describe(self):
        return {**super().describe(), "provider_hint": self.provider_hint()}

    def model_vars(self, env=None):
        return super().model_vars(env) + tuple(v for h in self._hints(env) for v in (h[0],) + h[3])

    def identity(self, env=None):
        if self.conf.get("identity_cmd"):
            return super().identity(env)
        # Only Bedrock model access has an AWS identity; an AWS_PROFILE alone belongs to the task.
        if not any(h[0] == "CLAUDE_CODE_USE_BEDROCK" for h in self._hints(env)) or not which("aws"):
            return None
        source = os.environ if env is None else env
        for var in ("AWS_BEARER_TOKEN_BEDROCK", "CLAUDE_CODE_SKIP_BEDROCK_AUTH"):
            if source.get(var):  # a Bedrock API key or a gateway: no SigV4 identity to show
                return f"not checked: Bedrock through {var} has no STS identity"
        return self._identity_of(["aws", "sts", "get-caller-identity", "--output", "text"], env)


@register("auth", "env")
class EnvAuth(Auth):
    """Generic profile for any provider: set/unset env vars, add args, run an identity command.

    [auth.my-gateway]
    type = "env"
    env = { OPENAI_BASE_URL = "https://gateway.example", OPENAI_API_KEY = "${GATEWAY_KEY}" }
    unset = ["ANTHROPIC_API_KEY"]
    args = []
    required_env = ["GATEWAY_KEY"]
    identity_cmd = ["my-cli", "whoami"]
    isolates_config = true
    model_env = ["MY_PROVIDER_*"]      # other vars the provider reads; [env] keys matching them are flagged
    """
    harnesses = ("*",)
    description = "generic: set/unset env vars, add CLI args, run an identity command (any provider)"

    def __init__(self, conf=None):
        super().__init__(conf)
        self.required_env = tuple(self.conf.get("required_env") or ())
        self.isolates_config = bool(self.conf.get("isolates_config", True))


# ----------------------------------------------------------------------------- declarative harness

class Declarative(Harness):
    """Any headless agent CLI described in trial.toml, no code:

    [adapters.opencode]
    execute       = ["opencode", "run", "{prompt}"]       # argv list; never a shell string
    model_args    = ["--model", "{model}"]                 # appended when the cell sets a model
    effort_args   = []
    clean_args    = []                                     # present => clean supported
    clean_env     = { XDG_CONFIG_HOME = "{config_dir}" }   # env used for config="clean"
    resume        = ["opencode", "run", "--session", "{session_id}", "{prompt}"]  # enables self-narration
    no_tools_args = []                                     # appended on resume to keep narration read-only
    session_id_regex = "session[:= ]+([A-Za-z0-9_-]{8,})"  # searched in stdout+stderr
    prompt_via    = "arg"                                  # arg | stdin
    version       = ["opencode", "--version"]
    strip_env     = []
    env           = {}

    Placeholders: {prompt} {prompt_file} {model} {effort} {workspace} {config_dir} {session_id} {run_token}.
    {session_id} inside execute is a pre-generated UUID for CLIs that accept one.
    Telemetry: wall clock + stdout/stderr. Write a Python plugin for tokens/tool calls.
    """

    def __init__(self, name, conf):
        self.plugin_name = name
        self.conf = conf
        self.binary = conf["execute"][0]
        self.version_argv = conf.get("version")
        self.can_resume = bool(conf.get("resume"))
        self.clean_supported = "clean_args" in conf or "clean_env" in conf
        self.strip_env = tuple(conf.get("strip_env") or ())
        self.telemetry = "wall clock and stdout only (declarative adapter)"

    def _values(self, ctx, prompt, sid, write=True):
        pf = ctx["run_dir"] / "prompt-input.txt"
        if write:
            pf.write_text(prompt, encoding="utf-8")
        cell = ctx["cell"]
        return {"prompt": prompt, "prompt_file": str(pf), "model": cell.get("model") or "",
                "effort": cell.get("effort") or "", "workspace": str(ctx["workspace"]),
                "config_dir": ctx["config_dir"], "session_id": sid or "", "run_token": ctx.get("run_token", "")}

    def _argv(self, ctx, template, values, extra=()):
        cell = ctx["cell"]
        argv = list(template)
        if cell.get("model"):
            argv += self.conf.get("model_args") or []
        if cell.get("effort"):
            argv += self.conf.get("effort_args") or []
        argv += list(extra) + list(cell.get("args") or [])
        return fill_all(argv, values) + auth_args(ctx)

    def _env(self, ctx, values):
        env = {k: fill(str(v), values) for k, v in (self.conf.get("env") or {}).items()}
        if ctx["cell"]["config"] == "clean":
            env.update({k: fill(str(v), values) for k, v in (self.conf.get("clean_env") or {}).items()})
        return env

    def clean_env(self, ctx):
        """The adapter's own env/clean_env as launched, so doctor and prepare model the worker's env."""
        return self._env(ctx, self._values(ctx, ctx.get("prompt_text", ""), None, write=False))

    def _stdin(self, prompt):
        return prompt if self.conf.get("prompt_via") == "stdin" else None

    def _find_session(self, ctx, stage, fallback):
        rx = self.conf.get("session_id_regex")
        if not rx:
            return fallback
        text = "\n".join(r.get("line", "") for r in read_jsonl(ctx["run_dir"] / f"{stage}.raw.jsonl"))
        err = ctx["run_dir"] / f"{stage}.stderr.txt"
        text += "\n" + (err.read_text(errors="replace") if err.exists() else "")
        m = re.search(rx, text)
        return m.group(1) if m else fallback

    def execute(self, ctx):
        pre = str(uuid.uuid4()) if "{session_id}" in " ".join(self.conf["execute"]) else None
        values = self._values(ctx, ctx["prompt_text"], pre)
        extra = (self.conf.get("clean_args") or []) if ctx["cell"]["config"] == "clean" else []
        argv = self._argv(ctx, self.conf["execute"], values, extra)
        proc = self.run(ctx, argv, "execute", stdin_data=self._stdin(ctx["prompt_text"]),
                        extra_env=self._env(ctx, values))
        proc["argv"] = self.safe_argv(ctx, argv, hide=(ctx["prompt_text"],))
        proc["session_id"] = self._find_session(ctx, "execute", pre)
        return proc

    def narrate(self, ctx, prompt):
        sid = self.stage_state(ctx, "execute").get("session_id")
        if not (self.conf.get("resume") and sid):
            return None
        values = self._values(ctx, prompt, sid)
        argv = self._argv(ctx, self.conf["resume"], values, self.conf.get("no_tools_args") or [])
        proc = self.run(ctx, argv, "narrate", stdin_data=self._stdin(prompt), extra_env=self._env(ctx, values))
        proc.update(argv=self.safe_argv(ctx, argv, hide=(prompt,)), session_id=sid,
                    tools_disabled=bool(self.conf.get("no_tools_args")))
        return proc

    def normalize(self, ctx, stage, exclude_message_ids=()):
        lines = [r.get("line", "") for r in read_jsonl(ctx["run_dir"] / f"{stage}.raw.jsonl")]
        return empty_telemetry(
            session_id=self.stage_state(ctx, stage).get("session_id"),
            model_ids=[ctx["cell"].get("model") or "harness default (unrecorded)"],
            final_text="\n".join(lines[-80:]) if stage == "execute" else "\n".join(lines),
            timestamp_source="ajx process start/stop and stdout arrival",
            limitations=[f"{self.name}: declarative adapter; no token or tool-call telemetry"])


# ----------------------------------------------------------------------------- runners

@register("runner", "local")
class LocalRunner(Runner):
    """Run the worker directly on this machine (default)."""


@register("runner", "docker")
class DockerRunner(Runner):
    """Run the worker inside a container. The workspace is bind-mounted at the same path so
    paths in transcripts stay meaningful. Env vars are forwarded by name (values never logged).

    [runner]                      # or per cell: runner = { type = "docker", image = "..." }
    type = "docker"
    image = "my-agent-image:latest"
    mounts = ["~/.claude:/root/.claude"]   # optional extra -v mounts (e.g. harness config/transcripts)
    args = ["--network", "bridge"]
    local_transcripts = false               # set true if transcripts are mounted back to the host
    """
    def __init__(self, conf=None):
        super().__init__(conf)
        self.local_transcripts = bool(self.conf.get("local_transcripts", False))

    def wrap(self, argv, ctx, env):
        ws = str(ctx["workspace"])
        out = ["docker", "run", "--rm", "-i", "-v", f"{ws}:{ws}", "-w", ws]
        for extra in (ctx.get("config_dir"), ctx.get("cache_dir")):  # keep transcripts/caches on the host
            if extra and Path(extra).exists():
                out += ["-v", f"{extra}:{extra}"]
        for m in self.conf.get("mounts") or []:
            out += ["-v", str(Path(m.split(":")[0]).expanduser()) + ":" + ":".join(m.split(":")[1:])]
        for name in sorted(env):
            out += ["-e", name]
        out += list(self.conf.get("args") or []) + [self.conf["image"]]
        return out + list(argv)


@register("runner", "wrapper")
class WrapperRunner(Runner):
    """Any argv prefix: ssh, a CI sandbox, firejail, `sandbox-exec`, a devcontainer exec...

    [runner]
    type = "wrapper"
    prefix = ["ssh", "builder@host", "cd {workspace} &&"]
    local_transcripts = false
    """
    def __init__(self, conf=None):
        super().__init__(conf)
        self.local_transcripts = bool(self.conf.get("local_transcripts", False))

    def wrap(self, argv, ctx, env):
        import shlex
        values = {"workspace": str(ctx["workspace"]), "run_token": ctx.get("run_token", "")}
        prefix = fill_all(self.conf.get("prefix") or [], values)
        # ssh and friends hand the rest to a remote shell: pass argv as ONE safely quoted string
        # whenever the prefix ends in a shell operator or the profile says shell_join = true.
        joins = self.conf.get("shell_join")
        if joins is None:
            joins = bool(prefix) and prefix[-1].rstrip().endswith(("&&", ";", "||", "|", "-c"))
        return prefix + ([shlex.join(argv)] if joins else list(argv))


# ----------------------------------------------------------------------------- checks

def _env_for_checks(ctx):
    """Same env as preflight and teardown: the caller's, minus what the auth profile unsets, plus ajx's."""
    from ..util import child_env
    return child_env(ctx.get("check_env", {}), ctx.get("unset_env") or ())


@register("check", "shell")
class ShellCheck(Check):
    """run = "<shell command>"; expect_exit (default 0); expect_stdout = "<regex>" (optional)."""

    def run(self, check, ctx):
        res = context_shell(check["run"], ctx, timeout=check["timeout"], location=check.get("location", "host"))
        passed = (not res["timed_out"]) and res["exit_code"] == int(check.get("expect_exit", 0))
        if passed and check.get("expect_stdout"):
            passed = re.search(check["expect_stdout"], res["stdout"], re.M) is not None
        detail = f"exit={res['exit_code']}" + (" timeout" if res["timed_out"] else "")
        return {**res, "passed": passed, "detail": detail}


@register("check", "file_exists")
class FileExistsCheck(Check):
    """path = "<glob relative to workspace>"; min_count (default 1); contains = "<regex>" (optional)."""

    @staticmethod
    def _matches(root_fd, relative, regex):
        """Open every component beneath the workspace without following worker-created links."""
        if not relative.parts or any(part in (".", "..") for part in relative.parts):
            return False
        try:
            with ExitStack() as opened:
                parent_fd = root_fd
                for part in relative.parts[:-1]:
                    parent_fd = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                                        dir_fd=parent_fd)
                    opened.callback(os.close, parent_fd)
                fd = os.open(relative.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK,
                             dir_fd=parent_fd)
                opened.callback(os.close, fd)
                if not stat.S_ISREG(os.fstat(fd).st_mode):
                    return False
                with os.fdopen(fd, encoding="utf-8", errors="replace", closefd=False) as stream:
                    return regex is None or regex.search(stream.read()) is not None
        except (OSError, ValueError):
            return False

    def run(self, check, ctx):
        from ..util import now
        started = now()
        workspace, pattern = Path(ctx["workspace"]), check.get("path")
        if not isinstance(pattern, str) or not pattern or "\x00" in pattern \
                or Path(pattern).is_absolute() or ".." in Path(pattern).parts:
            return {"passed": False, "detail": "file_exists requires a glob beneath the workspace",
                    "started_at": started, "stopped_at": now()}
        regex = re.compile(check["contains"], re.M) if check.get("contains") else None
        hits = []
        try:
            root_fd = os.open(workspace, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                for path in workspace.glob(pattern):
                    relative = path.relative_to(workspace)
                    if self._matches(root_fd, relative, regex):
                        hits.append(path)
            finally:
                os.close(root_fd)
        except OSError:
            pass  # Missing, replaced, or inaccessible workspaces cannot supply file evidence.
        passed = len(hits) >= int(check.get("min_count", 1))
        rel = [str(p.relative_to(workspace)) for p in hits[:20]]
        return {"passed": passed, "detail": f"{len(hits)} match(es): {rel}", "started_at": started,
                "stopped_at": now()}


@register("check", "http")
class HttpCheck(Check):
    """url = "..."; expect_status (default 200); expect_body = "<regex>" (optional)."""

    def run(self, check, ctx):
        from ..util import now
        started = now()
        try:
            with urllib.request.urlopen(resolve(check["url"]), timeout=check["timeout"]) as resp:
                status, body = resp.status, resp.read(65536).decode(errors="replace")
        except urllib.error.HTTPError as exc:
            status, body = exc.code, ""
        except Exception as exc:  # noqa: BLE001
            return {"passed": False, "detail": f"request failed: {type(exc).__name__}: {exc}",
                    "started_at": started, "stopped_at": now()}
        passed = status == int(check.get("expect_status", 200))
        if passed and check.get("expect_body"):
            passed = re.search(check["expect_body"], body) is not None
        return {"passed": passed, "detail": f"status={status}", "stdout": body[:2000],
                "started_at": started, "stopped_at": now()}


__all__ = ["Declarative", "version_of", "which"]
