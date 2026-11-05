"""The console's structured editor for trial.toml.

A plain model of what a test runner configures (the task, success checks, run settings, the
reporter, the task account and cleanup, execution environments and workers) is read from the
trial file and written back section by section. Only what changed is rewritten: untouched keys and
sections keep their text and comments. Validation stays with `ajx.spec`: the console loads the result
before saving it, and refuses anything AJX itself would refuse.
"""

import contextlib
import contextvars
import datetime
import difflib
import json
import re
import shlex
import shutil
import subprocess
import time
import tomllib
from pathlib import Path

from . import plugins
from . import model_catalog
from .spec import _ID as SLUG  # the same short-lowercase-slug rule spec.load applies to every name

FOLDER_ENV = "@local"  # the implicit "fresh folder on your machine"; '@' can never be a profile name
CHECK_TYPES = {"file_exists": "file", "shell": "cmd", "http": "http"}
CHECK_TOML = {v: k for k, v in CHECK_TYPES.items()}
KNOWN_MODELS = model_catalog.KNOWN_MODELS
EFFORT = {"claude-code", "codex", "kiro-cli"}  # agents that accept an effort setting
LABELS = {"claude-code": "Claude Code", "codex": "Codex", "kiro-cli": "Kiro CLI", "gemini-cli": "Gemini CLI",
          "cursor-agent": "Cursor Agent", "copilot-cli": "Copilot CLI"}
# Sign-in routes the page can create: credentials are ${VAR} references to the terminal, never values.
AUTH_TEMPLATES = {
    "claude-code": {"bedrock-keys": (
        "Bedrock keys from your terminal",
        "Adds [auth.bedrock-keys] to trial.toml. It reads AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_REGION "
        "(and AWS_SESSION_TOKEN if set) from the terminal that started the console; no secret is written.",
        {"type": "claude-bedrock", "env": {"AWS_REGION": "${AWS_REGION}", "AWS_ACCESS_KEY_ID": "${AWS_ACCESS_KEY_ID}",
                                           "AWS_SECRET_ACCESS_KEY": "${AWS_SECRET_ACCESS_KEY}"}})},
    "kiro-cli": {"kiro-key": (
        "Kiro API key from your terminal",
        "Adds [auth.kiro-key] to trial.toml. It reads KIRO_API_KEY from the terminal that started the console.",
        {"type": "env", "isolates_config": True, "required_env": ["KIRO_API_KEY"]})},
}
CELL_KEYS = {"id", "harness", "model", "models", "effort", "config", "auth", "environment", "agent_configuration"}
CHECK_FIELDS = {"file": ("path", "contains"), "cmd": ("run", "expect_exit", "expect_stdout"),
                "http": ("url", "expect_status", "expect_body")}
CHECK_KEYS = {"name", "type", "location"} | {k for keys in CHECK_FIELDS.values() for k in keys}
ENV_KEYS = {"backend", "image", "network", "required", "limits", "tools", "docker_host"}
CONTAINER_LIMITS = {"cpus": 1, "memory_mb": 1024, "pids": 256, "tmpfs_mb": 64, "lifetime_seconds": 3600}
MODEL_KEYS = ("task", "checks", "run", "reporter", "account", "envs", "workers")


class PlanError(ValueError):
    pass


# --------------------------------------------------------------------------- TOML text editing

_HEADER = re.compile(r"^\s*(\[\[?)([^\[\]]+?)(\]\]?)\s*(#.*)?$")
_BARE = re.compile(r"[A-Za-z0-9_-]+")


def _string_end(line, i, delim):
    """Index just past the multi-line string delimiter `delim` closing at or after `i`, or -1."""
    while True:
        j = line.find(delim, i)
        if j < 0:
            return -1
        if delim == '"""':
            k = j - 1
            while k >= 0 and line[k] == "\\":
                k -= 1
            if (j - 1 - k) % 2:  # an escaped quote does not close the string
                i = j + 1
                continue
        end = j + 3
        while end < len(line) and line[end] == delim[0] and end - j < 5:  # up to two quotes may precede it
            end += 1
        return end


def string_lines(lines):
    """For each line, whether it begins inside a multi-line string (it is then neither a key nor a header)."""
    inside, state = [], None
    for line in lines:
        inside.append(state is not None)
        i, n = 0, len(line)
        while i < n:
            if state:
                end = _string_end(line, i, state)
                if end < 0:
                    break
                state, i = None, end
                continue
            ch = line[i]
            if ch == "#":
                break
            if line.startswith('"""', i) or line.startswith("'''", i):
                state, i = line[i:i + 3], i + 3
                continue
            if ch == '"':
                j = i + 1
                while j < n and line[j] != '"':
                    j += 2 if line[j] == "\\" else 1
                i = j + 1
                continue
            if ch == "'":
                j = line.find("'", i + 1)
                i = n if j < 0 else j + 1
                continue
            i += 1
    return inside


def sections(lines):
    """[(name, is_array, header_index, end_index)]; the root section has name None. Quoted or dotted
    headers end a section but never match a plain table name; lines inside strings are never headers."""
    out, current = [], (None, False, -1)
    for i, (line, quoted) in enumerate(zip(lines, string_lines(lines))):
        m = None if quoted else _HEADER.match(line)
        if m and (m.group(1) == "[[") == (m.group(3) == "]]"):
            out.append((*current, i))
            current = (m.group(2).strip(), m.group(1) == "[[", i)
    out.append((*current, len(lines)))
    return out


def key(name):
    return name if _BARE.fullmatch(str(name)) else json.dumps(str(name))


def value(v):
    """TOML for a value: scalars, arrays and inline tables."""
    if isinstance(v, bool):
        return "true" if v else "false"
    if isinstance(v, (int, float)):
        return repr(v)
    if isinstance(v, (list, tuple)):
        return "[" + ", ".join(value(x) for x in v) + "]"
    if isinstance(v, dict):
        return "{ " + ", ".join(f"{key(k)} = {value(x)}" for k, x in v.items()) + " }" if v else "{}"
    text = str(v)
    if "\n" in text and "'''" not in text and not text.endswith("'") and "\r" not in text \
            and not any(ord(c) < 32 and c not in "\n\t" for c in text):
        return "'''\n" + text + "'''"  # literal multi-line string: commands stay readable and exact
    if '"' in text and "'" not in text and not any(ord(c) < 32 for c in text):
        return "'" + text + "'"
    return json.dumps(text, ensure_ascii=False)


def _trailing_comment(line, name):
    for i, ch in enumerate(line):
        if ch == "#":
            try:
                if name in tomllib.loads(line[:i]):
                    return "  " + line[i:].strip()
            except tomllib.TOMLDecodeError:
                continue
    return ""


def _value_end(lines, start, end):
    """The index after the last line of the key/value that starts at `start` (arrays and strings can span lines)."""
    for j in range(start + 1, end + 1):
        try:
            tomllib.loads("\n".join(lines[start:j]))
            return j
        except tomllib.TOMLDecodeError:
            continue
    return start + 1


def set_value(text, table, name, new, index=0):
    """Set (or with None, remove) `name` in `[table]`, or in the index-th `[[cells]]`, keeping the rest of
    the file as written. The result is parsed and compared before it is returned."""
    lines = text.split("\n")
    quoted = string_lines(lines)
    found = [s for s in sections(lines) if s[0] == table and s[1] == (table == "cells")]
    rendered = None if new is None else f"{key(name)} = {value(new)}"
    if table == "cells" and index >= len(found):
        raise PlanError(f"cells[{index}] not found")
    section = found[index if table == "cells" else 0] if found else None
    if section:
        start, end = section[2] + 1, section[3]
        live = re.compile(rf"^\s*{re.escape(key(name))}\s*=")
        commented = re.compile(rf"^\s*#\s*{re.escape(name)}\s*=")
        hit = next((i for i in range(start, end) if not quoted[i] and live.match(lines[i])), None)
        if hit is not None:
            stop = _value_end(lines, hit, end)
            comment = _trailing_comment(lines[hit], name) if stop == hit + 1 else ""
            lines[hit:stop] = [] if rendered is None else [rendered + comment]
        elif rendered is not None:
            hint = next((i for i in range(start, end) if not quoted[i] and commented.match(lines[i])), None)
            if hint is not None:
                lines[hint] = rendered  # replace the scaffold's commented-out example in place
            else:
                at = end
                while at > start and (not lines[at - 1].strip() or lines[at - 1].lstrip().startswith("#")) \
                        and not quoted[at - 1]:
                    at -= 1
                lines.insert(at, rendered)
    elif rendered is not None:
        while lines and lines[-1] == "":
            lines.pop()
        lines += ["", f"[{table}]", rendered, ""]
    result = "\n".join(lines)
    try:
        parsed = tomllib.loads(result)
    except tomllib.TOMLDecodeError as exc:
        raise PlanError(f"could not update {table}.{name} safely ({exc}); edit trial.toml directly") from None
    holder = (parsed.get("cells") or [{}])[index] if table == "cells" else (parsed.get(table) or {})
    if (new is None and name in holder) or (new is not None and holder.get(name) != new):
        raise PlanError(f"could not update {table}.{name} safely; edit trial.toml directly")
    return result


def block(header, mapping, array=False):
    lines = [f"[[{header}]]" if array else f"[{header}]"]
    lines += [f"{key(k)} = {value(v)}" for k, v in mapping.items() if v is not None]
    return lines + [""]


def replace_sections(text, belongs, new_lines):
    """Replace every section for which `belongs(name, is_array)` holds with `new_lines`, placed where the
    first of them was (or at the end). Comment lines that end a removed section stay: they usually
    introduce the section that follows."""
    lines = text.split("\n")
    quoted = string_lines(lines)
    doomed = [s for s in sections(lines) if s[0] is not None and belongs(s[0], s[1])]
    if not doomed and not new_lines:
        return text
    drop = set()
    for _, _, start, end in doomed:
        last = start
        for i in range(start + 1, end):
            if quoted[i] or (lines[i].strip() and not lines[i].lstrip().startswith("#")):
                last = i
        drop.update(range(start, last + 1))
        if last + 1 < end and not lines[last + 1].strip():
            drop.add(last + 1)
    out, placed = [], False
    for i, line in enumerate(lines):
        if doomed and i == doomed[0][2] and not placed:
            out += new_lines
            placed = True
        if i not in drop:
            out.append(line)
    if not placed and new_lines:
        while out and out[-1] == "":
            out.pop()
        out += [""] + new_lines
    return "\n".join(out).rstrip("\n") + "\n"


def _group(name):
    """Sections of the array of tables `name`, with their subtables such as [verify.headers]."""
    return lambda n, array: (array and n == name) or n.startswith(name + ".")


# --------------------------------------------------------------------------- model

def _json_safe(v, where="trial.toml"):
    if isinstance(v, (datetime.date, datetime.time)):
        raise PlanError(f"{where} holds a TOML date or time, which the console cannot edit; edit trial.toml directly")
    if isinstance(v, dict):
        for k, x in v.items():
            _json_safe(x, f"{where}.{k}")
    elif isinstance(v, list):
        for x in v:
            _json_safe(x, where)


def _commands(items):
    return [{"run": str(c.get("run") or ""), "name": c.get("name") or "",
             "extra": {k: v for k, v in c.items() if k not in ("run", "name")}} for c in items or []]


def _identity(cmd):
    if isinstance(cmd, list):
        return shlex.join(str(a) for a in cmd)
    return str(cmd or "")


def _can_report(cli, adapters):
    if not cli or cli in adapters:
        return False
    try:
        return bool(getattr(plugins.get("harness", cli), "can_report", False))
    except KeyError:
        return False


def auto_reporter(workers, adapters=()):
    """The agent spec.load picks when [reporter].harness is not set: the first worker's that can report."""
    return next((w["cli"] for w in workers if _can_report(w["cli"], adapters)), "claude-code")


def read_model(trial, text=None):
    """The editable model of a trial file: raw values as written, not the defaults spec.load adds."""
    trial = Path(trial)
    raw = tomllib.loads(trial.read_text(encoding="utf-8") if text is None else text)
    _json_safe(raw)
    t, task, rep = raw.get("trial") or {}, raw.get("task") or {}, raw.get("reporter") or {}
    prompt_file = task.get("prompt_file") or ""
    prompt_path = (trial.parent / prompt_file) if prompt_file else None
    prompt = prompt_path.read_text(encoding="utf-8") if prompt_path and prompt_path.is_file() else ""
    profiles = raw.get("environments") or {}
    configs = raw.get("agent_configurations") or {}
    cells = raw.get("cells") or []
    default_env, default_cfg = t.get("environment"), t.get("agent_configuration")
    envs = []
    if any(not (c.get("environment") or default_env) for c in cells) or not profiles:
        envs.append({"id": FOLDER_ENV, "kind": "folder", "label": "your machine"})
    for name, prof in profiles.items():
        container = prof.get("backend") == "container"
        envs.append({"id": name, "kind": "container" if container else "profile",
                     "image": prof.get("image", ""), "network": prof.get("network", "none" if container else "inherit"),
                     "required": list(prof.get("required") or []),
                     "limits": {**(CONTAINER_LIMITS if container else {}), **dict(prof.get("limits") or {})},
                     "tools": [dict(x) for x in prof.get("tools") or []], "docker_host": prof.get("docker_host", ""),
                     "extra": {k: v for k, v in prof.items() if k not in ENV_KEYS}})
    workers = []
    for c in cells:
        cfg = c.get("agent_configuration") or default_cfg
        skills = ((configs.get(cfg) or {}).get("skills") or {}) if cfg else {}
        workers.append({
            "id": str(c.get("id") or ""), "cli": c.get("harness") or "claude-code", "model": c.get("model") or "",
            "models": [str(x) for x in c.get("models") or []], "models_form": "models" in c,
            "effort": c.get("effort") or "", "config": c.get("config") or "clean", "auth": c.get("auth") or "",
            "env": c.get("environment") or default_env or FOLDER_ENV,
            "skills": "folders" if skills.get("mode") in ("selected", "snapshot") else "none",
            "skillPaths": [str(x) for x in skills.get("paths") or []], "profile": cfg or "",
            "extra": {k: v for k, v in c.items() if k not in CELL_KEYS}})
    checks = []
    for v in raw.get("verify") or []:
        kind = v.get("type", "shell")
        short = CHECK_TYPES.get(kind, kind)
        known = CHECK_FIELDS.get(short, ())
        check = {"name": v.get("name", ""), "type": short, "path": "", "contains": "", "run": "", "expect_exit": 0,
                 "expect_stdout": "", "url": "", "expect_status": 200, "expect_body": "",
                 "where": "worker" if v.get("location") == "environment" else "host",
                 "extra": {k: x for k, x in v.items() if k not in {"name", "type", "location", *known}}}
        check.update({k: v[k] for k in known if k in v})
        checks.append(check)
    budget = t.get("max_budget_usd")
    return {
        "task": {"prompt": prompt, "prompt_file": prompt_file, "materials": [str(m) for m in task.get("materials") or []],
                 "fixture": task.get("fixture_dir") or "", "human": task.get("human") or "unavailable",
                 "setup": _commands(raw.get("setup"))},
        "checks": checks,
        "run": {"minutes": round(int(t.get("timeout_seconds", 1800)) / 60, 2), "attempts": int(t.get("repetitions", 1)),
                "budget": "" if budget is None else budget, "parallel": int(t.get("parallel", 1)),
                "report_minutes": round(int(t.get("narrate_timeout_seconds", 900)) / 60, 2),
                "order_seed": int(t.get("order_seed", 0)), "workspace": t.get("workspace_root", "/tmp"),
                "shell": t.get("shell_startup", "inherit"), "output_dir": t.get("output_dir", "")},
        "reporter": {"cli": rep.get("harness") or "",  # empty: the first worker's agent that can report
                     "model": rep.get("model") or "", "auth": rep.get("auth") or ""},
        "account": {"on": bool(task.get("identity_cmd") or raw.get("preflight")),
                    "identity": _identity(task.get("identity_cmd")),
                    "preflight": _commands(raw.get("preflight")), "teardown": _commands(raw.get("teardown"))},
        "envs": envs,
        "workers": workers,
    }


_DOCKER = {"at": 0.0, "info": {"images": [], "host": ""}}


def docker_info(ttl=60):
    """Local Docker images (as pinned references) and the active context's socket, for the container
    editor. Read-only and best effort: nothing is pulled or started; cached for a minute."""
    if time.time() - _DOCKER["at"] < ttl:
        return _DOCKER["info"]
    images, host = [], ""
    if shutil.which("docker"):
        def ask(*argv):
            try:
                res = subprocess.run(["docker", *argv], capture_output=True, text=True, timeout=5, stdin=subprocess.DEVNULL)
            except (OSError, subprocess.TimeoutExpired):
                return ""
            return res.stdout if res.returncode == 0 else ""
        host = ask("context", "inspect", "--format", "{{.Endpoints.docker.Host}}").strip()
        for line in ask("image", "ls", "--no-trunc", "--format", "{{.Repository}}:{{.Tag}}\t{{.ID}}").splitlines()[:40]:
            name, _, ident = line.partition("\t")
            if ident.startswith("sha256:"):
                images.append({"ref": ident, "label": f"{name} ({ident[7:19]})"})
    _DOCKER.update(at=time.time(), info={"images": images, "host": host if host.startswith(("unix://", "tcp://", "npipe://")) else ""})
    return _DOCKER["info"]


def _auth_class(conf):
    kind = (conf or {}).get("type")
    return plugins.get("auth", kind) if kind in plugins.names("auth") else None


def _isolates(name, profiles):
    if name in profiles:
        cls = _auth_class(profiles[name])
        return bool(profiles[name].get("isolates_config", getattr(cls, "isolates_config", False) if cls else False))
    try:
        return bool(getattr(plugins.get("auth", name), "isolates_config", False))
    except KeyError:
        return False


def options(trial, model):
    """What the editors may offer: agents (installed, verified), their sign-in routes, model suggestions,
    reporter agents and local container images."""
    raw = tomllib.loads(Path(trial).read_text(encoding="utf-8"))
    profiles = raw.get("auth") or {}
    harnesses = []
    for name in plugins.names("harness"):
        cls = plugins.get("harness", name)
        try:
            installed = bool(cls().available())
        except Exception:  # noqa: BLE001 - an odd plugin must not break the page
            installed = False
        harnesses.append({"name": name, "label": LABELS.get(name, name), "installed": installed,
                          "verified": bool(getattr(cls, "verified_live", False)),
                          "reporter": bool(getattr(cls, "can_report", False)),
                          "clean": bool(getattr(cls, "clean_supported", False)),
                          "default_auth": getattr(cls, "default_auth", "inherit"), "effort": name in EFFORT,
                          "models": KNOWN_MODELS.get(name, []), "model_hint": model_catalog.hint(name)})
    for adapter in raw.get("adapters") or {}:
        harnesses.append({"name": adapter, "label": adapter, "installed": True, "verified": False, "reporter": False,
                          "clean": False, "default_auth": "inherit", "effort": True, "models": [],
                          "model_hint": model_catalog.hint(adapter)})
    auth = {}
    for h in harnesses:
        default = h["default_auth"]
        routes = [{"value": "", "label": f"Default ({default})", "fresh_home": _isolates(default, profiles),
                   "kind": "default", "detail": f"Uses the {default} sign-in."}]
        for name in plugins.names("auth"):
            cls = plugins.get("auth", name)
            if name != "env" and ("*" in cls.harnesses or h["name"] in cls.harnesses):
                routes.append({"value": name, "label": name, "fresh_home": bool(getattr(cls, "isolates_config", False)),
                               "kind": "type", "detail": getattr(cls, "description", "") or name})
        for name, conf in profiles.items():
            cls = _auth_class(conf)
            if cls and ("*" in cls.harnesses or h["name"] in cls.harnesses):
                routes.append({"value": name, "label": f"{name} (in this trial)", "kind": "profile",
                               "fresh_home": _isolates(name, profiles),
                               "detail": f"[auth.{name}] in trial.toml, type {conf.get('type')}."})
        for name, (label, detail, _) in AUTH_TEMPLATES.get(h["name"], {}).items():
            if name not in profiles:
                routes.append({"value": name, "label": label, "fresh_home": True, "kind": "template", "detail": detail})
        auth[h["name"]] = routes
    seen = {}
    for w in model.get("workers") or []:
        for m in [w.get("model"), *(w.get("models") or [])]:
            if m and m not in seen.setdefault(w["cli"], []):
                seen[w["cli"]].append(m)
    for h in harnesses:
        h["models"] = list(dict.fromkeys(h["models"] + seen.get(h["name"], [])))
    from .envcheck import _BASH_ONLY
    docker = docker_info()
    return {"harnesses": harnesses, "auth": auth, "images": docker["images"], "docker_host": docker["host"],
            "auto_reporter": auto_reporter(model.get("workers") or [], raw.get("adapters") or {}),
            "bash_only": [[pattern.pattern, label] for pattern, label in _BASH_ONLY],
            "check_types": [["file", "File exists"], ["cmd", "Command succeeds"], ["http", "HTTP returns"]],
            "efforts": ["", "low", "medium", "high"]}


# --------------------------------------------------------------------------- model -> TOML

# Values already in the file are rebuilt leniently: only what an edit changes is validated, so an
# odd value the user did not touch (one spec.load accepts) never blocks an unrelated edit.
_LENIENT = contextvars.ContextVar("plan_edit_lenient", default=False)


@contextlib.contextmanager
def _lenient(on=True):
    token = _LENIENT.set(bool(on))
    try:
        yield
    finally:
        _LENIENT.reset(token)


def _num(v, label, low=None):
    try:
        return _num_strict(v, label, low)
    except PlanError:
        if _LENIENT.get():
            return v
        raise


def _num_strict(v, label, low=None):
    if isinstance(v, bool):
        raise PlanError(f"{label} must be a number")
    try:
        n = float(v)
    except (TypeError, ValueError):
        raise PlanError(f"{label} must be a number") from None
    if n != n or n in (float("inf"), float("-inf")):
        raise PlanError(f"{label} must be a number")
    if low is not None and n < low:
        raise PlanError(f"{label} must be at least {low:g}")
    return n


def _whole(v, label, low=None):
    try:
        n = _num_strict(v, label, low)
        if n != int(n):
            raise PlanError(f"{label} must be a whole number")
    except PlanError:
        if _LENIENT.get():
            return v
        raise
    return int(n)


def _model_name(v, harness=""):
    if _LENIENT.get():
        return str(v or "").strip() or None
    try:
        return model_catalog.parse_name(v, harness)
    except model_catalog.InvalidModelName as exc:
        raise PlanError(f"{exc.name!r} is not a model name an agent CLI accepts: use an alias such as sonnet, or the "
                        "provider's model id (no spaces)") from None
    except ValueError as exc:
        raise PlanError(str(exc)) from exc


def _command_tables(items):
    out = []
    for c in items or []:
        run = str(c.get("run") or "")
        if run.strip():
            entry = {"name": c.get("name") or None, "run": run}
            entry.update({k: v for k, v in (c.get("extra") or {}).items() if v is not None and k not in ("name", "run")})
            out.append({k: v for k, v in entry.items() if v is not None})
    return out


def _check_table(c):
    short = c.get("type") or "cmd"
    entry = {"name": str(c.get("name") or "").strip() or None, "type": CHECK_TOML.get(short, short)}
    if short == "file":
        entry.update(path=str(c.get("path") or "").strip(), contains=c.get("contains") or None)
    elif short == "cmd":
        code = c.get("expect_exit", "")
        entry.update(run=str(c.get("run") or "").strip(),
                     expect_exit=None if str(code).strip() in ("", "0") else _whole(_stripped(code), "expected exit code"),
                     expect_stdout=c.get("expect_stdout") or None)
    elif short == "http":
        status = c.get("expect_status", "")
        entry.update(url=str(c.get("url") or "").strip(),
                     expect_status=None if str(status).strip() in ("", "200")
                     else _whole(_stripped(status), "expected status", 100),
                     expect_body=c.get("expect_body") or None)
    if c.get("where") == "worker":
        entry["location"] = "environment"
    entry.update({k: v for k, v in (c.get("extra") or {}).items() if v is not None and k not in entry})
    return {k: v for k, v in entry.items() if v is not None}


def _stripped(v):
    """A typed number as entered; the file's own value (any type) is kept as it is."""
    return v.strip() if isinstance(v, str) else v


def _same_number(a, b):
    try:
        return float(a) == float(b)
    except (TypeError, ValueError):
        return False


def _env_table(e):
    if e.get("kind") == "container":
        limits = {}
        for k, v in (e.get("limits") or {}).items():
            if v in (None, "") or (k in CONTAINER_LIMITS and _same_number(v, CONTAINER_LIMITS[k])):
                continue
            limits[k] = _num(v, f"{e['id']} {k}", 0) if k == "cpus" else _whole(v, f"{e['id']} {k}", 0)
        entry = {"backend": "container", "image": str(e.get("image") or "").strip(), "network": e.get("network") or "none",
                 "required": list(e.get("required") or []) or None, "limits": limits or None,
                 "docker_host": str(e.get("docker_host") or "").strip() or None}
    else:
        entry = {"backend": "local"}
    tools = [t for t in e.get("tools") or [] if t.get("name")]
    if tools:
        entry["tools"] = [{"name": t["name"], "argv": list(t.get("argv") or [t["name"], "--version"]),
                           "required": bool(t.get("required", True))} for t in tools]
    entry.update({k: v for k, v in (e.get("extra") or {}).items() if v is not None and k not in entry})
    return {k: v for k, v in entry.items() if v is not None}


DEFAULTS = {("trial", "timeout_seconds"): 1800, ("trial", "repetitions"): 1, ("trial", "max_budget_usd"): None,
            ("trial", "parallel"): 1, ("trial", "narrate_timeout_seconds"): 900, ("trial", "order_seed"): 0,
            ("trial", "workspace_root"): "/tmp", ("trial", "shell_startup"): "inherit", ("trial", "output_dir"): None,
            ("task", "materials"): None, ("task", "fixture_dir"): None, ("task", "human"): "unavailable",
            ("task", "identity_cmd"): None, ("reporter", "harness"): None, ("reporter", "model"): None,
            ("reporter", "auth"): None}


def _shell(m):
    if m["run"].get("shell") not in ("inherit", "skip"):
        raise PlanError("shell startup must be inherit or skip")
    return m["run"]["shell"]


# (trial.toml key, the model fields it comes from, how to compute it). Only keys whose model fields
# changed are converted and written, so an odd value the user did not touch never blocks an edit.
SCALARS = (
    (("trial", "timeout_seconds"), (("run", "minutes"),),
     lambda m: int(round(_num(m["run"]["minutes"], "time limit", 0.1) * 60))),
    (("trial", "repetitions"), (("run", "attempts"),), lambda m: _whole(m["run"]["attempts"], "attempts", 1)),
    (("trial", "max_budget_usd"), (("run", "budget"),),
     lambda m: None if str(m["run"].get("budget", "")).strip() == "" else _num(m["run"]["budget"], "budget", 0.01)),
    (("trial", "parallel"), (("run", "parallel"),), lambda m: _whole(m["run"].get("parallel", 1), "parallel", 1)),
    (("trial", "narrate_timeout_seconds"), (("run", "report_minutes"),),
     lambda m: int(round(_num(m["run"]["report_minutes"], "reporting time limit", 0.1) * 60))),
    (("trial", "order_seed"), (("run", "order_seed"),), lambda m: _whole(m["run"].get("order_seed", 0), "order seed")),
    (("trial", "workspace_root"), (("run", "workspace"),), lambda m: str(m["run"].get("workspace") or "/tmp").strip()),
    (("trial", "shell_startup"), (("run", "shell"),), _shell),
    (("trial", "output_dir"), (("run", "output_dir"),), lambda m: str(m["run"].get("output_dir") or "").strip() or None),
    (("task", "materials"), (("task", "materials"),),
     lambda m: [x for x in (str(y).strip() for y in m["task"].get("materials") or []) if x] or None),
    (("task", "fixture_dir"), (("task", "fixture"),), lambda m: str(m["task"].get("fixture") or "").strip() or None),
    (("task", "human"), (("task", "human"),), lambda m: m["task"].get("human") or "unavailable"),
    (("task", "identity_cmd"), (("account", "identity"), ("account", "on")),
     lambda m: (str(m["account"].get("identity") or "").strip() if m["account"].get("on") else "") or None),
    (("reporter", "harness"), (("reporter", "cli"),), lambda m: m["reporter"].get("cli") or None),
    (("reporter", "model"), (("reporter", "model"), ("reporter", "cli")),
     lambda m: _model_name(m["reporter"].get("model"),
                          m["reporter"].get("cli") or auto_reporter(m["workers"]))),
    (("reporter", "auth"), (("reporter", "auth"),), lambda m: m["reporter"].get("auth") or None),
)


def _known(item, known):
    """Lenient while building an item the edit left exactly as the file has it (or already lenient)."""
    return _lenient(_LENIENT.get() or (known is not None and item in known))


def _groups(m, known=None):
    """The command and check groups; `known` is the file's own model (its unchanged checks are not
    validated again)."""
    checks = []
    for c in m["checks"]:
        with _known(c, known["checks"] if known else None):
            checks.append(_check_table(c))
    return {"setup": _command_tables(m["task"].get("setup")),
            "preflight": _command_tables(m["account"].get("preflight")) if m["account"].get("on") else [],
            "teardown": _command_tables(m["account"].get("teardown")),
            "verify": checks}


def _env_tables(m, known=None):
    out = {}
    for e in m["envs"]:
        if e["kind"] != "folder":
            with _known(e, known["envs"] if known else None):
                out[e["id"]] = _env_table(e)
    return out


def _skills_of(table):
    skills = (table or {}).get("skills") or {}
    return skills.get("mode") in ("selected", "snapshot"), [str(x) for x in skills.get("paths") or []]


def _config_name(base, table, configs, wanted):
    """A name for a new agent configuration that does not replace a different existing one."""
    name, n, stem = base, 2, base[:44]
    while (name in configs and configs[name] != table) or (name in wanted and wanted[name] != table):
        name, n = f"{stem}-{n}", n + 1
    return name


def _cells(m, configs, profiles, adapters=(), known=None, raw=None):
    """([[cells]] tables, {agent configuration name: table}, [(sign-in profile to create, its agent)]).
    `known` is the file's own model and `raw` the parsed file: a worker the edit left unchanged keeps
    its [[cells]] table exactly as written (never validated or normalized again), with the trial-level
    environment/agent_configuration defaults made explicit because a cell rewrite removes them."""
    old_workers = {w["id"]: w for w in (known or {}).get("workers") or []}
    raw = raw or {}
    raw_cells = {str(c.get("id") or ""): c for c in raw.get("cells") or []}
    defaults = {k: v for k, v in (raw.get("trial") or {}).items() if k in ("environment", "agent_configuration") and v}
    cells, wanted, added = [], {}, []
    for w in m["workers"]:
        unchanged = known is not None and old_workers.get(w["id"]) == w
        if unchanged and w["id"] in raw_cells:
            cell = {**{k: v for k, v in defaults.items() if k not in raw_cells[w["id"]]}, **raw_cells[w["id"]]}
            name = cell.get("agent_configuration")
            if name and name in configs:
                wanted[name] = configs[name]
            cells.append(cell)
            continue
        with _lenient(_LENIENT.get() or unchanged):
            _cell(w, m, configs, profiles, cells, wanted, added)
    rep = m["reporter"]
    rcli = rep.get("cli") or auto_reporter(m["workers"], adapters)
    if AUTH_TEMPLATES.get(rcli, {}).get(rep.get("auth") or "") and rep["auth"] not in profiles \
            and rep["auth"] not in [a for a, _ in added]:
        added.append((rep["auth"], rcli))
    return cells, wanted, added


def _cell(w, m, configs, profiles, cells, wanted, added):
    envs = {e["id"]: e for e in m["envs"]}
    users = {}
    for x in m["workers"]:
        if x.get("profile"):
            users.setdefault(x["profile"], []).append(x["id"])
    env = envs.get(w["env"])
    if env is None:
        if not _LENIENT.get():
            raise PlanError(f"worker {w['id']} runs in an unknown environment {w['env']!r}")
        env = {"kind": "folder" if w["env"] == FOLDER_ENV else "container"}
    explicit = env["kind"] != "folder"
    models = [_model_name(x, w["cli"]) for x in w.get("models") or [] if str(x).strip()]
    cell = {"id": w["id"], "harness": w["cli"]}
    if len(models) > 1 or (models and w.get("models_form")):
        cell["models"] = models  # the cell ids stay <id>-<model>, as recorded attempts know them
    else:
        cell["model"] = models[0] if models else _model_name(w.get("model"), w["cli"])
    cell.update(effort=w.get("effort") or None, config="clean" if explicit else (w.get("config") or "clean"),
                auth=w.get("auth") or None)
    if explicit:
        cell["environment"] = w["env"]
    wants = w.get("skills") == "folders"
    paths = [x for x in (str(y).strip() for y in w.get("skillPaths") or []) if x]
    if wants and not explicit and not _LENIENT.get():
        raise PlanError(f"worker {w['id']}: giving skills needs a fresh-profile or container environment")
    if wants and not paths and not _LENIENT.get():
        raise PlanError(f"worker {w['id']}: choose at least one skill folder, or select none")
    own = configs.get(w.get("profile") or "")
    if own is not None and _skills_of(own) == (wants, paths if wants else []):
        name, table = w["profile"], own  # unchanged: the configuration stays exactly as written
    elif wants or own is not None:
        base = own if own is not None else {"plugins": {"mode": "none"}, "hooks": {"mode": "none"}}
        table = {**base, "skills": {"mode": "selected", "paths": paths} if wants else {"mode": "none"}}
        shared = own is not None and len(users.get(w["profile"], [])) > 1
        same = next((k for k, v in configs.items() if v == table), None)  # reuse, so recorded profiles stay
        name = w["profile"] if own is not None and not shared else same or _config_name(w["id"], table, configs, wanted)
    else:
        name, table = None, None  # no configuration was there and none is needed
    if name:
        if name in wanted and wanted[name] != table:
            name = _config_name(w["id"], table, configs, wanted)
        wanted[name] = table
        cell["agent_configuration"] = name
    cell.update({k: v for k, v in (w.get("extra") or {}).items() if v is not None and k not in CELL_KEYS})
    cells.append({k: v for k, v in cell.items() if v is not None})
    if AUTH_TEMPLATES.get(w["cli"], {}).get(w.get("auth") or "") and w["auth"] not in profiles \
            and w["auth"] not in [a for a, _ in added]:
        added.append((w["auth"], w["cli"]))


def _check_names(m, current):
    ids = [w["id"] for w in m["workers"]]
    if not ids:
        raise PlanError("add at least one worker")
    old = {w["id"] for w in current["workers"]}
    if len(set(ids)) != len(ids):
        raise PlanError("worker names must be unique")
    for i in ids:
        if i not in old and not SLUG.fullmatch(i or ""):
            raise PlanError(f"worker name {i!r}: use a short lowercase name (letters, digits, . _ -)")
    env_ids = [e["id"] for e in m["envs"]]
    old_envs = {e["id"] for e in current["envs"]}
    if len(set(env_ids)) != len(env_ids):
        raise PlanError("environment names must be unique")
    for e in m["envs"]:
        if e["kind"] != "folder" and e["id"] not in old_envs and not SLUG.fullmatch(e["id"] or ""):
            raise PlanError(f"environment name {e['id']!r}: use a short lowercase name (letters, digits, . _ -)")
        if e["kind"] != "folder" and e["id"] == FOLDER_ENV:
            raise PlanError("name the environment before changing what kind it is")
    if sum(1 for e in m["envs"] if e["kind"] == "folder") > 1:
        raise PlanError("only one 'fresh folder on your machine' environment is possible")


def _shape(model):
    if not isinstance(model, dict) or any(not isinstance(model.get(k), list if k in ("checks", "envs", "workers")
                                                         else dict) for k in MODEL_KEYS):
        raise PlanError("the plan is incomplete; reload the page")


def render(trial, model, text=None, current=None):
    """(new trial.toml text, new prompt text or None, sign-in profiles added) for `model`. Only what
    differs from the file's own model is rewritten; everything else keeps its text and comments."""
    trial = Path(trial)
    text = trial.read_text(encoding="utf-8") if text is None else text
    current = read_model(trial, text) if current is None else current
    _shape(model)
    try:
        return _render(text, model, current)
    except PlanError:
        raise
    except (KeyError, TypeError, ValueError, AttributeError, IndexError) as exc:
        raise PlanError(f"the plan has a value the console cannot write ({type(exc).__name__}: {exc}); "
                        "reload the page") from None


def _render(text, model, current):
    raw = tomllib.loads(text)
    _check_names(model, current)
    for (table, name), sources, compute in SCALARS:
        if all(current[a].get(b) == model[a].get(b) for a, b in sources):
            continue
        target = compute(model)
        now = (raw.get(table) or {}).get(name, DEFAULTS[(table, name)])
        if table == "task" and name == "identity_cmd":
            now = _identity(now) or None
        if target == now or (target == DEFAULTS[(table, name)] and name not in (raw.get(table) or {})):
            continue
        text = set_value(text, table, name, target)
    with _lenient():
        now_groups, now_envs = _groups(current), _env_tables(current)
    new_groups, new_envs = _groups(model, current), _env_tables(model, current)
    for name, entries in new_groups.items():
        if entries != now_groups[name]:
            lines = [line for e in entries for line in block(name, e, array=True)]
            text = replace_sections(text, _group(name), lines)
    if now_envs != new_envs:
        lines = [line for name, entry in new_envs.items() for line in block(f"environments.{name}", entry)]
        text = replace_sections(text, lambda n, a: n == "environments" or n.startswith("environments."), lines)
    configs = {k: dict(v) for k, v in (raw.get("agent_configurations") or {}).items()}
    profiles = raw.get("auth") or {}
    adapters = raw.get("adapters") or {}
    with _lenient():
        now_cells, now_wanted, _ = _cells(current, configs, profiles, adapters, known=current, raw=raw)
    cells, wanted, added = _cells(model, configs, profiles, adapters, known=current, raw=raw)
    t = raw.get("trial") or {}
    if cells != now_cells:
        lines = [line for c in cells for line in block("cells", c, array=True)]
        text = replace_sections(text, _group("cells"), lines)
        for name in ("environment", "agent_configuration"):
            if t.get(name) is not None:
                text = set_value(text, "trial", name, None)
    if wanted != now_wanted:
        # Existing configurations stay even when no worker uses them any more: an output folder's
        # attempts recorded them, and dropping one would lock that folder. Only new or edited ones change.
        keep = dict(configs)
        keep.update(wanted)
        if keep != configs:
            lines = [line for name, entry in keep.items() for line in block(f"agent_configurations.{name}", entry)]
            text = replace_sections(text, lambda n, a: n == "agent_configurations"
                                    or n.startswith("agent_configurations."), lines)
    for name, harness in added:
        text = text.rstrip("\n") + "\n\n" + "\n".join(block(f"auth.{name}", AUTH_TEMPLATES[harness][name][2]))
    try:
        parsed = tomllib.loads(text)
    except tomllib.TOMLDecodeError as exc:
        raise PlanError(f"the edit produced invalid TOML ({exc}); edit trial.toml directly") from None
    for name, entries in new_groups.items():  # what was written reads back as intended
        if entries != now_groups[name] and [dict(x) for x in parsed.get(name) or []] != entries:
            raise PlanError(f"could not write [[{name}]] safely; edit trial.toml directly")
    prompt = model["task"].get("prompt")
    prompt = None if prompt is None else str(prompt).replace("\r\n", "\n")
    return text, prompt, [name for name, _ in added]


# --------------------------------------------------------------------------- changes, preview

SECTION = {"task": "task", "run": "run settings", "reporter": "reporter", "account": "task account"}
FIELD_NAMES = {"minutes": "time limit (min)", "attempts": "attempts", "budget": "budget", "cli": "agent",
               "model": "model", "models": "models compared", "config": "starting setup", "auth": "sign-in",
               "env": "runs in", "skills": "skills", "skillPaths": "skill folders", "kind": "kind", "image": "image",
               "network": "network", "required": "protections", "limits": "limits", "tools": "tools",
               "shell": "shell startup", "workspace": "workspace folder", "prompt": "prompt", "materials": "materials",
               "fixture": "starting files", "human": "human help", "setup": "setup commands", "on": "uses an account",
               "identity": "account check", "preflight": "preflight", "teardown": "cleanup", "parallel": "parallel",
               "report_minutes": "reporting time limit", "output_dir": "output folder", "effort": "effort",
               "order_seed": "order seed", "docker_host": "Docker socket", "extra": "other settings",
               "name": "name", "type": "type", "path": "path", "contains": "contains", "run": "command",
               "expect_exit": "expected exit code", "expect_stdout": "output pattern", "url": "URL",
               "expect_status": "expected status", "expect_body": "body pattern", "where": "runs on"}
_SKIP = {"id", "profile", "models_form", "label", "prompt_file"}


def _flat(model):
    out = {}
    for part in ("task", "run", "reporter", "account"):
        for k, v in model[part].items():
            if k not in _SKIP:
                out[(part, SECTION[part], k)] = v
    for i, c in enumerate(model["checks"]):
        for k, v in c.items():
            out[("checks", f"check {i + 1}", k)] = v
    for e in model["envs"]:
        for k, v in e.items():
            if k not in _SKIP:
                out[(f"env-{e['id']}", f"environment {e['id']}", k)] = v
    for w in model["workers"]:
        for k, v in w.items():
            if k not in _SKIP:
                out[(f"worker-{w['id']}", f"worker {w['id']}", k)] = v
    return out


def _shown(v):
    if v in ("", None, []):
        return "default"
    if isinstance(v, list):
        return ", ".join(str(x.get("run") or x.get("name") if isinstance(x, dict) else x) for x in v) or "none"
    if isinstance(v, dict):
        return "\u2026"
    text = str(v)
    return text if len(text) <= 60 else text[:57] + "\u2026"


def changes(old, new):
    """[{node, text}] in words: what the edit changes, for the unsaved bar and the journal."""
    try:
        a, b = _flat(old), _flat(new)
        out = []
        old_w, new_w = {w["id"] for w in old["workers"]}, {w["id"] for w in new["workers"]}
        old_e, new_e = {e["id"] for e in old["envs"]}, {e["id"] for e in new["envs"]}
        out += [{"node": f"worker-{i}", "text": f"worker {i} added"} for i in sorted(new_w - old_w)]
        out += [{"node": "map", "text": f"worker {i} removed"} for i in sorted(old_w - new_w)]
        out += [{"node": f"env-{i}", "text": f"environment {i} added"} for i in sorted(new_e - old_e)]
        out += [{"node": "map", "text": f"environment {i} removed"} for i in sorted(old_e - new_e)]
        if len(new["checks"]) != len(old["checks"]):
            out.append({"node": "checks", "text": f"success checks: {len(old['checks'])} -> {len(new['checks'])}"})
        for path in sorted(set(a) | set(b), key=str):
            node, label, field = path
            if (node.startswith("worker-") and node[7:] not in old_w & new_w) or \
                    (node.startswith("env-") and node[4:] not in old_e & new_e):
                continue
            if node == "checks" and len(new["checks"]) != len(old["checks"]):
                continue
            if a.get(path) != b.get(path):
                if field == "prompt":
                    if str(a.get(path) or "").rstrip("\n") != str(b.get(path) or "").rstrip("\n"):
                        out.append({"node": node, "text": "task prompt edited"})  # saved with one final newline
                else:
                    out.append({"node": node, "text": f"{label} \u00b7 {FIELD_NAMES.get(field, field)}: "
                                                      f"{_shown(a.get(path))} -> {_shown(b.get(path))}"})
        return out
    except (KeyError, TypeError, AttributeError) as exc:
        raise PlanError(f"the plan is incomplete ({exc}); reload the page") from None


def diff_lines(before, after, name):
    return [line.rstrip("\n") for line in difflib.unified_diff(before.splitlines(True), after.splitlines(True),
                                                               f"{name} (now)", f"{name} (after saving)", n=1)]
