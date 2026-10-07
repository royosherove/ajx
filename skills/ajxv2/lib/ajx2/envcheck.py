"""Environment and credential checks: what a worker really sees, and whether cleanup can work.

A cloud task's credentials pass through three environments that can disagree:

  harness process   ajx2's worker env (trial [env], cell env, auth env), then the harness's own
                    settings env on top (Claude Code applies `env` from settings.json over its
                    process env). The model calls use this.
  agent tool shell  the harness process env, then the startup files of `<shell> -c` (Claude Code
                    runs every Bash call that way, so zsh reads ~/.zshenv). The agent's commands
                    use this, including the model's credentials it inherits.
  check shell       verify, teardown and preflight: ajx2's env under /bin/sh, with no harness
                    settings and no startup files.

doctor and validate compare them before a run, prepare records them, and teardown, preflight
and verify output is scanned for credential errors so an exit 0 cannot hide a cleanup that
never authenticated.
"""

import inspect
import json
import re
import shlex
import shutil
import subprocess
import sys
from pathlib import Path

from . import spec as specmod
from .base import excerpt
from .util import child_env, run_shell

# Values are printed only for names that are routinely non-secret; anything else shows as hidden.
SHOWN_SUFFIXES = ("_PROFILE", "_REGION", "_LOCATION", "_PROJECT", "_PROJECT_ID", "_SUBSCRIPTION_ID", "_ACCOUNT_ID")
SHOWN_NAMES = ("CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "AWS_PAGER",
               "AWS_DEFAULT_OUTPUT", "LANG", "LC_ALL", "LC_CTYPE", "TZ")
SENSITIVE_HINTS = ("KEY", "TOKEN", "SECRET", "PASSWORD", "PASS", "PWD", "CREDENTIAL", "SESSION", "COOKIE", "AUTH",
                   "HEADER", "PRIVATE", "CERT")

# Heuristic. Phrases that cloud/SaaS CLIs and SDKs print when they cannot authenticate...
CREDENTIAL_PATTERNS = (
    r"\b(NoCredentials|NoCredentialsError|NoCredentialProviders|CredentialsError|CredentialsProviderError|"
    r"ExpiredToken|ExpiredTokenException|InvalidClientTokenId|UnrecognizedClientException|InvalidAccessKeyId|"
    r"SignatureDoesNotMatch|AuthFailure|InvalidIdentityToken|ExpiredAuthenticationToken|"
    r"InvalidAuthenticationToken|UNAUTHENTICATED|ENEEDAUTH)\b",
    r"Unable to locate credentials",
    r"The config profile \(.+\) could not be found",
    r"(?i)\bcould not load credentials from any providers\b",
    r"(?i)\bno valid credential sources found\b",
    r"(?i)\bunable to resolve AWS account to use\b",
    r"(?i)\bfailed to refresh cached credentials\b",
    r"(?i)\bsecurity token included in the request is (invalid|expired)\b",
    r"(?i)\b(token|credentials?)( has| have| is)? expired\b",
    r"(?i)\bSSO session\b.*\b(expired|invalid)\b",
    r"(?i)\breauthentication (failed|required)\b",
    r"(?i)\bdo not currently have an active account\b",
    r"(?i)\b(run|running|use)\b[:\s`'\"$]*(aws (sso )?login|aws configure|gcloud auth|az login|gh auth login|"
    r"(vercel|netlify|firebase|heroku|npm|docker|fly|flyctl|wrangler|doctl|supabase|railway) login)\b",
    r"(?i)\bno (existing |valid )?credentials (found|provided|available)\b",
    r"(?i)could not (automatically determine|find default) credentials",
    r"(?i)\byou must be logged in to the server\b",
    r"\bnpm (ERR!|error) code E401\b",
    r"(?i)\bbad credentials\b",
    r"\bAADSTS\d{5,}\b",
)
# ...and when they are authenticated but not allowed (cleanup is just as incomplete).
PERMISSION_PATTERNS = (
    r"\b(AccessDenied|AccessDeniedException|UnauthorizedOperation|AuthorizationError|AuthorizationFailed|"
    r"PERMISSION_DENIED)\b",
    r"(?i)\bis not authorized to perform\b",
    r"(?i)\bnot (logged|signed) in\b",
    r"(?i)\bauthentication (failed|required)\b",
    r"(?i)\b(401 Unauthorized|403 Forbidden|HTTP 40[13])\b",
)
AUTH_ERROR_PATTERNS = tuple(re.compile(p) for p in CREDENTIAL_PATTERNS + PERMISSION_PATTERNS)

FIX_COLLISION = (
    "The worker has one environment for both, so the task's value either breaks model calls or is replaced "
    "before the agent sees it. Give the model an explicit, isolated auth profile of its own, and give the task "
    "its account without [env]: name it in the task prompt and pass it explicitly in [task] identity_cmd, "
    "[[preflight]], [[verify]] and [[teardown]].")
FIX_COLLISION_AWS = (
    " For Bedrock: [auth.<name>] type = \"claude-bedrock\", env = { AWS_PROFILE = \"<model profile>\", "
    "AWS_REGION = \"<region>\" }; for the task, --profile <task profile> (an explicit --profile beats AWS_PROFILE "
    "in the environment). AWS_CONFIG_FILE and AWS_SHARED_CREDENTIALS_FILE are read by the model's AWS SDK too; "
    "put them in [env] only when the model does not authenticate through AWS.")


def auth_error_lines(*texts, limit=5):
    """Lines of command output that look like a credential or permission failure (heuristic)."""
    hits = []
    for text in texts:
        for line in (text or "").splitlines():
            line = line.strip()
            if line and line[:240] not in hits and any(p.search(line) for p in AUTH_ERROR_PATTERNS):
                hits.append(line[:240])
    return hits[:limit]


def shown(name, value):
    """A value for terminal output and logs: only allowlisted, non-secret-looking names show a value."""
    if value is None:
        return "<unset>"
    up = name.upper()
    if any(h in up for h in SENSITIVE_HINTS) or not (up in SHOWN_NAMES or up.endswith(SHOWN_SUFFIXES)):
        return "<value hidden>"
    return json.dumps(value if len(value) <= 80 else value[:77] + "...")


def _matches(name, patterns):
    return any(name == p or (p.endswith("*") and name.startswith(p[:-1])) for p in patterns)


def command_env(env, unset, run_token):
    """Env of verify/teardown/preflight commands, which ajx2 runs under /bin/sh."""
    return child_env({**env, "AJX2_RUN_TOKEN": run_token}, unset)


def scratch_workspace(spec, tmp):
    """A stand-in workspace for doctor/validate: the fixture, as prepare would copy it, in a temp dir."""
    ws = Path(tmp) / "workspace"
    if spec["task"].get("fixture_dir"):
        shutil.copytree(spec["task"]["fixture_dir"], ws, dirs_exist_ok=True)
    ws.mkdir(exist_ok=True)
    return ws


def placeholder_ctx(spec, cell, auth, runner, tmp):
    """A full run ctx (see base.py) for doctor/validate, pointing at scratch dirs instead of a run."""
    tmp = Path(tmp)
    env = specmod.worker_env(spec, cell, auth)
    return {"spec": spec, "cell": cell, "run_dir": tmp, "workspace": tmp / "workspace",
            "config_dir": str(tmp / "harness-config"),
            "cache_dir": str(tmp / "cache"), "run_token": "doctor", "timeout": int(spec["trial"]["timeout_seconds"]),
            "narrate_timeout": int(spec["trial"]["narrate_timeout_seconds"]), "prompt_text": spec["task"]["prompt_text"],
            "state": {}, "env": env, "unset_env": auth.unset(), "runner": runner, "auth": auth,
            "check_env": {**env, "AJX2_RUN_TOKEN": "doctor"}}


def worker_view(spec, harness, auth, ctx):
    """The env a worker's harness process gets (as Harness.run builds it from `ctx`), the harness's own
    settings env on top of it, and the shell its tool commands run in. Holds secrets: never record it."""
    errors = []
    try:
        extra = harness.clean_env(ctx) or {}
    except Exception as exc:  # noqa: BLE001 - a plugin's clean_env must not break doctor or prepare
        extra, errors = {}, [f"{harness.name}.clean_env failed: {type(exc).__name__}: {exc}"]
    process = child_env({**ctx["env"], **extra}, tuple(harness.strip_env) + tuple(auth.unset()))
    layers = harness.settings_env(process, spec["task"].get("fixture_dir"))
    effective, source = dict(process), {}
    for label, values in layers:
        effective.update(values)
        source.update(dict.fromkeys(values, f"harness settings {label}"))
    return {"intended": dict(ctx["env"]), "launch": dict(extra), "process": process, "effective": effective,
            "settings": layers, "source": source, "shell": harness.tool_shell(effective), "errors": errors}


_MARK = "__AJX2_ENV__"


def shell_env(shell, env, cwd, timeout=30):
    """Exported variables a command sees inside `<shell> -c`, after the shell's startup files.
    `env -0` first (Python would coerce a C locale); a Python dump where env has no -0."""
    dumpers = [shlex.quote(shutil.which("env") or "/usr/bin/env") + " -0",
               f"{shlex.quote(sys.executable)} -I -c 'import os, sys; sys.stdout.write(chr(0).join(k + \"=\" + v for k, v in os.environ.items()))'"]
    for dump in dumpers:
        try:
            out = subprocess.run([shell, "-c", f"printf %s {_MARK}; exec {dump}"], cwd=str(cwd), env=env,
                                 stdin=subprocess.DEVNULL, capture_output=True, text=True, errors="replace",
                                 timeout=timeout)
        except (OSError, subprocess.TimeoutExpired):
            return None
        if out.returncode == 0 and _MARK in out.stdout:
            pairs = (item.partition("=") for item in out.stdout.rsplit(_MARK, 1)[1].split("\0") if item)
            return {k: v for k, _, v in pairs}
    return None


def env_findings(spec, cell, auth, view, shell_vars):
    """Variables ajx2 sets that the harness or the agent's tool shell will not see as set."""
    origin = {}
    for label, values in (("[env]", spec["env"]), (f"cell {cell['id']} env", cell.get("env") or {}),
                          (f"auth {auth.profile}", auth.env())):
        origin.update(dict.fromkeys(values, label))
    out = []
    for name, label in origin.items():
        want, harness_val = view["intended"].get(name), view["effective"].get(name)
        tool_val = shell_vars.get(name) if shell_vars is not None else harness_val
        if harness_val != want or tool_val != harness_val:
            source = view["source"].get(name) or ("the harness's launch env" if name in view["launch"] else None)
            out.append({"name": name, "origin": label, "set": want, "harness": harness_val, "tool_shell": tool_val,
                        "settings_source": source if harness_val != want else None,
                        "shell_startup": tool_val != harness_val, "model_affected": harness_val != want})
    return out


def override_text(finding, shell):
    n = finding["name"]
    text = f"{finding['origin']} sets {n}={shown(n, finding['set'])}, but "
    if finding["model_affected"]:
        text += (f"the harness applies {shown(n, finding['harness'])} from {finding['settings_source']} over it "
                 "(model calls use that); ")
    text += f"the agent's tool shell ({shell}) sees {shown(n, finding['tool_shell'])}"
    if finding["shell_startup"]:
        text += " (set by that shell's startup files)"
    if not finding["model_affected"] and finding["origin"].startswith("auth "):
        text += "; model calls keep the profile's value"
    return text


def provider_label(auth, env):
    hint = getattr(auth, "provider_hint", None)
    return f"auth {auth.profile}: {hint(env) if hint else auth.description}"


def collision_findings(spec, cell, auth, view):
    """Trial [env] / cell env keys that are also the harness's model credentials under this auth."""
    patterns = auth.model_vars(view["effective"])
    keys = list(dict.fromkeys([*spec["env"], *(cell.get("env") or {})]))
    return [k for k in keys if _matches(k, patterns)]


def collision_text(names, auth, view):
    text = (f"{', '.join(names)} from [env]/cell env also drive the harness's model access "
            f"({provider_label(auth, view['effective'])}). " + FIX_COLLISION)
    return text + (FIX_COLLISION_AWS if any(n.startswith("AWS_") for n in names) else "")


def auth_identity(auth, env):
    """auth.identity(env); identity() for plugins written before identity took the effective env."""
    try:
        takes_env = bool(inspect.signature(auth.identity).parameters)
    except (TypeError, ValueError):
        takes_env = False
    return auth.identity(env) if takes_env else auth.identity()


def identity_result(cmd, cwd, env, shell=None, timeout=60):
    """Run an identity command as `<shell> -c` (shell None: /bin/sh, like verify and teardown)."""
    res = run_shell(cmd, cwd, timeout=timeout, env=env, shell=shell)
    errors = auth_error_lines(res["stdout"], res["stderr"])
    ok = res["exit_code"] == 0 and not res["timed_out"] and not errors
    text = res["stdout"].strip() if ok else "\n".join(errors) or (res["stderr"] or res["stdout"]).strip()
    return {"cmd": cmd, "shell": f"{shell or '/bin/sh'} -c", "exit_code": res["exit_code"],
            "timed_out": res["timed_out"], "ok": ok, "output": excerpt(text, 400), "auth_errors": errors}


DEFAULT_AWS_IDENTITY = "aws sts get-caller-identity --output text"


def uses_aws(spec, auth, view):
    """The model reads AWS credentials (Bedrock) or the task identity is an aws command."""
    return bool(shutil.which("aws", path=view["effective"].get("PATH"))) and (
        _matches("AWS_PROFILE", auth.model_vars(view["effective"]))
        or (spec["task"].get("identity_cmd") or "").lstrip().startswith("aws "))


def inspect_cell(spec, cell, harness, auth, runner, cwd, ctx, identities=False):
    """What doctor, validate and prepare report about one cell's environment. Returns (report, view);
    the report holds no env values except identity output, the view holds secrets."""
    local = runner.name == "local"
    view = worker_view(spec, harness, auth, ctx)
    if not local:  # the worker's env is rebuilt elsewhere; its harness settings and shell are unknown here
        view.update(effective=dict(view["process"]), settings=[], source={})
    shell_vars = shell_env(view["shell"], view["effective"], cwd) if local else None
    observed = getattr(harness, "tool_shell_observed", False)
    report = {
        "local_runner": local,
        "harness_settings": [{"source": s, "names": sorted(v)} for s, v in view["settings"]],
        "tool_shell": (f"{view['shell']} -c" + ("" if observed else " (assumed)")) if local else None,
        "tool_shell_probed": shell_vars is not None,
        "overrides": env_findings(spec, cell, auth, view, shell_vars) if shell_vars is not None else [],
        "collisions": collision_findings(spec, cell, auth, view),
        "snapshot_restored": sorted(n for n in (*spec["env"], *(cell.get("env") or {}))
                                    if n in getattr(harness, "tool_shell_restores", ())),
        "errors": view["errors"],
    }
    if identities:
        report["auth_identity"] = auth_identity(auth, view["effective"])
        cmd = spec["task"].get("identity_cmd")
        shell = view["shell"]
        report["task_identity"] = identity_result(cmd, cwd, view["effective"], shell) if cmd and local else None
        # What a command that omits the task's --profile acts as: with Bedrock, the model's own account.
        report["default_aws_identity"] = (identity_result(DEFAULT_AWS_IDENTITY, cwd, view["effective"], shell)
                                          if local and uses_aws(spec, auth, view) else None)
    return report, view


def warnings(report, auth, view):
    out = [f"could not model the worker's env: {e}" for e in report.get("errors") or []]
    out += [override_text(f, report["tool_shell"]) for f in report["overrides"]]
    if report["collisions"]:
        out.append(collision_text(report["collisions"], auth, view))
    for name in report.get("snapshot_restored") or []:
        out.append(f"{name} is reset before every tool command from the harness's shell snapshot (taken from a login "
                   "shell with your rc files), so the agent may not see the value ajx2 sets; not probed")
    if report["local_runner"] and not report["tool_shell_probed"]:
        out.append(f"could not probe the agent's tool shell ({view['shell']} -c); [env] overrides are unchecked")
    return out


def default_identity_warning(report):
    """Warn when an aws command without the task's --profile would act as someone else."""
    default, task = report.get("default_aws_identity"), report.get("task_identity")
    if not default or not default["ok"] or (task and task["ok"] and task["output"] == default["output"]):
        return None
    whose = " (the model's account)" if default["output"] == (report.get("auth_identity") or "").strip() else ""
    return (f"an aws command or SDK call in the agent's tool shell that omits the task's --profile acts as "
            f"{default['output']}{whose}, not as the task identity; teardown does not look there")


def record(report):
    """environment.json form: names and sources only."""
    return {"harness_settings": report["harness_settings"], "tool_shell": report["tool_shell"],
            "tool_shell_probed": report["tool_shell_probed"], "collisions": report["collisions"],
            "snapshot_restored": report["snapshot_restored"], "errors": report["errors"],
            "overrides": [{k: f[k] for k in ("name", "origin", "settings_source", "shell_startup", "model_affected")}
                          for f in report["overrides"]]}


def recorded_notes(environment):
    """One sentence per recorded override or collision (environment.json), for reports and the digest."""
    checks = (environment or {}).get("env_checks") or {}
    out = []
    for o in checks.get("overrides") or []:
        where = ([o["settings_source"]] if o.get("settings_source") else []) \
            + (["the tool shell's startup files"] if o.get("shell_startup") else [])
        who = "the harness (model calls) and the agent's commands" if o.get("model_affected") else "the agent's commands"
        out.append(f"{o['origin']} {o['name']} was replaced by {' and '.join(where)}; {who} did not see the value ajx2 set")
    if checks.get("collisions"):
        out.append(f"{', '.join(checks['collisions'])} set for the task also drive the harness's model access; "
                   "one of the two did not get its intended value")
    return out


def run_preflight(commands, cwd, env, stop_on_failure=False):
    """A preflight passes when it exits 0 within its timeout and prints no credential errors (unless the
    entry sets allow_auth_errors, e.g. for a check that expects access to be denied)."""
    results = []
    for cmd in commands:
        res = run_shell(cmd["run"], cwd, timeout=cmd["timeout"], env=env)
        rec = {"name": cmd["name"], **{k: res[k] for k in ("cmd", "exit_code", "timed_out", "started_at",
                                                            "stopped_at", "stdout", "stderr")},
               "auth_errors": [] if cmd.get("allow_auth_errors") else auth_error_lines(res["stdout"], res["stderr"])}
        rec["passed"] = res["exit_code"] == 0 and not res["timed_out"] and not rec["auth_errors"]
        results.append(rec)
        if stop_on_failure and not rec["passed"]:
            break
    return results


def preflight_failure(rec):
    why = "timed out" if rec["timed_out"] else f"exit {rec['exit_code']}"
    detail = "; ".join(rec["auth_errors"]) or excerpt((rec["stderr"] or rec["stdout"]).strip(), 300)
    return f"{why}" + (f": {detail}" if detail else "")


def teardown_summary(results, defined, ran):
    """status: ok | failed (a command exited non-zero or timed out) | auth_errors (credential or
    permission errors printed, whatever the exit code) | skipped (no workspace, so nothing ran)."""
    failed = [i for i, r in enumerate(results) if r["exit_code"] != 0 or r["timed_out"]]
    errors = [line for r in results for line in r.get("auth_errors") or []]
    status = ("ok" if not defined else "skipped" if not ran else "auth_errors" if errors
              else "failed" if failed else "ok")
    return {"status": status, "commands": len(results), "failed": failed, "auth_errors": errors[:10]}
