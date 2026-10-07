"""Base contracts for plugins plus shared parsing helpers.

A run context `ctx` (dict) handed to plugins contains:
  spec, cell, run_dir (Path), workspace (Path), config_dir (str, per-run, empty),
  cache_dir, run_token, timeout, prompt_text, state (per-stage results so far),
  env (dict: trial env + cell env + auth env, already resolved), unset_env (list),
  runner (Runner instance), auth (Auth instance or None).

Normalized telemetry returned by Harness.normalize():
  {"session_id", "model_ids", "final_text", "stop_reason",
   "events": [{"kind": "tool", "tool_use_id", "name", "input", "input_raw", "at", "end",
               "is_error", "output", "message_id"} | {"kind": "text", "text", "at", "message_id"}],
   "usage": [{"id", "at", "output_tokens"}],  # incremental, final value per message
   "usage_status", "expected_output_tokens", "tool_status", "expected_tool_calls",
   "harness_reported": {...},  # harness estimates (cost, credits), always labeled
   "timestamp_source", "limitations": [...], "message_ids": [...]}
"""

import json
import os
import re
import shutil
import subprocess
from pathlib import Path

from .util import child_env, fill, read_jsonl, run_streaming


# --------------------------------------------------------------------------- helpers

# What the AWS SDKs and CLI read to pick credentials, account and region (not output settings
# such as AWS_PAGER): a Bedrock-backed harness shares exactly these with an AWS task.
AWS_CREDENTIAL_ENV = ("AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_ACCESS_KEY_ID",
                      "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_SECURITY_TOKEN", "AWS_CONFIG_FILE",
                      "AWS_SHARED_CREDENTIALS_FILE", "AWS_BEARER_TOKEN_*", "AWS_ROLE_ARN", "AWS_ROLE_SESSION_NAME",
                      "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_*", "AWS_EC2_METADATA_*", "AWS_ENDPOINT_URL*",
                      "AWS_SDK_LOAD_CONFIG", "AWS_STS_REGIONAL_ENDPOINTS", "AWS_CA_BUNDLE", "AWS_USE_FIPS_ENDPOINT")


def version_of(argv):
    try:
        out = subprocess.run(argv, capture_output=True, text=True, timeout=20)
        text = (out.stdout or out.stderr).strip()
        return text.splitlines()[0][:120] if text else None
    except Exception as exc:  # noqa: BLE001 - doctor/report only
        return f"unavailable ({type(exc).__name__})"


def excerpt(value, limit=600):
    if value is None:
        return ""
    if not isinstance(value, str):
        value = json.dumps(value, ensure_ascii=False)
    value = value.strip()
    return value if len(value) <= limit else value[:limit] + f"... [{len(value) - limit} more chars]"


def summarize_input(data):
    if isinstance(data, dict):
        for key in ("command", "cmd", "file_path", "path", "absolute_path", "pattern", "url",
                    "query", "description"):
            if key in data:
                val = data[key]
                if isinstance(val, list):
                    val = " ".join(map(str, val))
                return excerpt(f"{key}: {val}", 300)
    return excerpt(data, 300)


def stream_records(raw_path):
    """Yield (arrival_ts, parsed_json_or_None, raw_line) from a run_streaming capture."""
    for rec in read_jsonl(raw_path):
        line = rec.get("line", "")
        try:
            parsed = json.loads(line)
        except (json.JSONDecodeError, TypeError):
            parsed = None
        yield rec.get("at"), parsed, line


def empty_telemetry(**overrides):
    out = {"session_id": None, "model_ids": [], "final_text": "", "stop_reason": None,
           "events": [], "usage": [], "usage_status": "unavailable", "expected_output_tokens": None,
           "tool_status": "unavailable", "expected_tool_calls": None, "harness_reported": {},
           "timestamp_source": "unknown", "limitations": [], "message_ids": []}
    out.update(overrides)
    return out


def tool_event(tid, name, raw_input, at, message_id=None):
    return {"kind": "tool", "tool_use_id": tid, "name": name, "input": summarize_input(raw_input),
            "input_raw": raw_input, "at": at, "end": None, "is_error": None, "output": "",
            "message_id": message_id}


def text_event(text, at, message_id=None):
    return {"kind": "text", "text": excerpt(text, 1200), "at": at, "message_id": message_id}


def finish_tool(call, at, is_error, output):
    if call is None:
        return
    call["end"] = at
    call["is_error"] = bool(is_error)
    if output:
        call["output"] = excerpt(output if isinstance(output, str) else json.dumps(output), 1500)


# --------------------------------------------------------------------------- contracts

class Harness:
    """Agent harness adapter. Override execute/narrate/normalize; set the capability flags."""
    plugin_name = "base"
    binary = None                 # executable name, for availability/doctor
    version_argv = None
    can_resume = False            # can re-enter the worker's own session to self-narrate
    clean_supported = False       # can run with user skills/memory/MCP/instructions disabled
    verified_live = False         # adapter validated against a real installed CLI by ajx tests
    default_auth = "inherit"
    strip_env = ()                # env vars (or 'PREFIX*') coupling a child to a parent agent session
    telemetry = "wall clock only"  # human-readable summary for doctor/report

    @property
    def name(self):
        return self.plugin_name

    def available(self):
        return bool(self.binary) and shutil.which(self.binary) is not None

    def version(self):
        return version_of(self.version_argv) if self.version_argv and self.available() else None

    def clean_env(self, ctx):
        """Env vars that isolate harness config for config='clean' (e.g. a per-run home dir)."""
        return {}

    def settings_env(self, env, project_dir=None):
        """Env the harness applies from its own settings on top of the process env `env`, as
        [(source label, {name: value}), ...] lowest precedence first. Default: none."""
        return []

    tool_shell_observed = False   # True once tool_shell() matches what the real CLI was seen to run
    tool_shell_restores = ()      # env vars the harness resets before each tool command (e.g. a PATH snapshot)

    def tool_shell(self, env):
        """Shell the agent's tool commands run in (`<shell> -c <command>`). Default `$SHELL`: assumed,
        not observed; claude-code overrides it with its observed behavior."""
        return env.get("SHELL") or "/bin/sh"

    def run(self, ctx, argv, stage, stdin_data=None, timeout=None, extra_env=None):
        """Launch argv for `stage`, capturing stdout/stderr under run_dir. The returned proc dict
        carries a redacted `argv` (secrets and prompts never reach state.json)."""
        env = dict(ctx["env"])
        env.update(extra_env or {})
        drop = tuple(self.strip_env) + tuple(ctx.get("unset_env") or ())
        full_env = child_env(env, drop)
        full_argv = ctx["runner"].wrap(argv, ctx, env)
        if stage == "narrate" and timeout is None:
            timeout = ctx.get("narrate_timeout") or 900
        proc = run_streaming(full_argv, ctx["workspace"], ctx["run_dir"] / f"{stage}.raw.jsonl",
                             ctx["run_dir"] / f"{stage}.stderr.txt", timeout or ctx["timeout"],
                             env=full_env, stdin_data=stdin_data, reap_after_exit=(stage != "execute"))
        proc["argv"] = self.safe_argv(ctx, argv, hide=(stdin_data,))
        return proc

    def safe_argv(self, ctx, argv, hide=()):
        """argv for the record: prompts replaced by a marker, auth values and args redacted."""
        secrets = set()
        auth = ctx.get("auth")
        if auth:
            secrets.update(v for v in auth.env().values() if len(v) >= 8)
            secrets.update(a for a in auth.extra_args() if not a.startswith("-"))
        out = []
        for a in argv:
            if a in hide and a:
                out.append("<prompt>")
            elif a in secrets:
                out.append("<redacted>")
            else:
                out.append(a)
        return out

    @staticmethod
    def stage_state(ctx, stage):
        return (ctx.get("state") or {}).get(stage) or {}

    def plan(self, ctx):
        """Facts knowable before launch (e.g. a pre-chosen session id), saved to state first."""
        return {}

    def execute(self, ctx):
        raise NotImplementedError

    def narrate(self, ctx, prompt):
        """Resume the worker's own session read-only and ask for the journey. None = unsupported."""
        return None

    def normalize(self, ctx, stage, exclude_message_ids=()):
        raise NotImplementedError


class Auth:
    """Credential/provider profile for a harness.

    Config comes from trial.toml [auth.<profile>] with `type = "<plugin name>"`; values may
    reference the caller's environment as "${VAR}" so secrets never live in the trial file.
    """
    plugin_name = "inherit"
    harnesses = ("*",)            # harness plugin names this applies to
    description = "use whatever the caller's environment already provides"
    required_env = ()             # env vars that must resolve (names only are ever recorded)
    isolates_config = True        # whether harness clean-config isolation keeps auth working
    model_env = ()                # further vars (or 'PREFIX*') the provider reads for model access

    def __init__(self, conf=None):
        self.conf = dict(conf or {})
        if "isolates_config" in self.conf:
            self.isolates_config = bool(self.conf["isolates_config"])
        self.profile = self.plugin_name
        self.harness = None       # set by spec.auth_for to the cell's harness

    @property
    def name(self):
        return self.plugin_name

    def env(self):
        """Env vars to set for the worker. Default: conf['env'] with ${VAR} resolution."""
        return {k: resolve(v) for k, v in (self.conf.get("env") or {}).items()}

    def extra_args(self):
        """Harness CLI args this profile needs (e.g. codex -c model_provider=...)."""
        return [resolve(a) for a in (self.conf.get("args") or [])]

    def unset(self):
        """Env vars to remove so a different provider in the caller's env does not leak in."""
        return list(self.conf.get("unset") or [])

    def prepare(self, ctx):
        """Hook to copy credentials into an isolated config dir, etc. Never log secret values."""

    def cleanup(self, ctx):
        """Hook to remove what prepare() placed in the per-run dirs before they are archived."""

    def problems(self):
        env = {**os.environ, **self.env()}
        out = [f"missing env var {v}" for v in self.required_env if not env.get(v)]
        for key, raw in (self.conf.get("env") or {}).items():
            for ref in re.findall(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}", str(raw)):
                if not os.environ.get(ref):
                    out.append(f"env {key} references ${{{ref}}} which is empty in the caller's environment")
        return out

    def describe(self):
        """Non-secret record: type, var names, check command output (identity)."""
        env = self.env()
        return {"type": self.name, "env_names": sorted(env), "unset": self.unset(),
                "required_env": list(self.required_env), "isolates_config": self.isolates_config}

    def model_vars(self, env=None):
        """Env var names (or 'PREFIX*') the harness's model access depends on under this profile.
        A trial [env] or cell env key matching one shares a namespace with the model's credentials.
        `env` is the harness's effective environment, for profiles that infer their provider."""
        extra = self.conf.get("model_env") or ()
        return (tuple(self.conf.get("env") or {}) + tuple(self.required_env) + tuple(self.unset())
                + tuple(self.model_env) + ((extra,) if isinstance(extra, str) else tuple(extra)))

    def identity(self, env=None):
        """Who the model calls authenticate as. `env` is the environment the harness process really
        gets (worker env plus harness settings); default: the caller's env plus this profile's."""
        return self._identity_of(self.conf.get("identity_cmd"), env)

    def _identity_of(self, argv, env=None):
        if not argv:
            return None
        try:
            env = child_env(self.env(), self.unset()) if env is None else env
            out = subprocess.run(argv, capture_output=True, text=True, timeout=30, env=env)
            return excerpt(out.stdout or out.stderr, 400)
        except Exception as exc:  # noqa: BLE001
            return f"identity check failed: {type(exc).__name__}"


def resolve(value):
    """Expand ${VAR} references from the caller's environment."""
    if not isinstance(value, str):
        return str(value)
    return re.sub(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}", lambda m: os.environ.get(m.group(1), ""), value)


class Runner:
    """Where a worker process executes. `wrap` turns a local argv into the real argv."""
    plugin_name = "local"
    local_transcripts = True      # harness transcript files are readable on this machine

    def __init__(self, conf=None):
        self.conf = dict(conf or {})

    @property
    def name(self):
        return self.plugin_name

    def wrap(self, argv, ctx, env):
        return list(argv)

    def describe(self):
        return {"type": self.name, **{k: v for k, v in self.conf.items() if k != "type"}}


class Check:
    """Deterministic verification step. `run` returns dict(passed: bool, detail: str, ...)."""
    plugin_name = "shell"

    @property
    def name(self):
        return self.plugin_name

    def run(self, check, ctx):
        raise NotImplementedError


def auth_args(ctx):
    auth = ctx.get("auth")
    return auth.extra_args() if auth else []


def which(binary):
    return shutil.which(binary)


__all__ = ["AWS_CREDENTIAL_ENV", "Harness", "Auth", "Runner", "Check", "Path", "auth_args", "excerpt", "empty_telemetry",
           "fill", "finish_tool", "read_jsonl", "resolve", "stream_records", "summarize_input", "text_event",
           "tool_event", "version_of", "which"]
