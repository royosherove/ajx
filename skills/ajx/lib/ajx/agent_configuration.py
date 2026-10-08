"""Explicit, per-attempt harness extension profiles (standard library only).

Public trial schema::

    [agent_configurations.product]
    skills = { mode = "selected", paths = ["skills/product"], pins = { product = "<sha256>" } }
    plugins = { mode = "none" }
    hooks = { mode = "none" }
    require_absence = false

All three categories default to ``none``. ``snapshot`` also takes explicit skill
directories, never a user's configuration root, a glob, or an implicit search.
Pins are optional at input: normalization always pins the complete content.
Plugins/hooks other than none are unsupported and rejected.

Integration:
* normalize_profiles(raw_profiles, base_dir) returns JSON-safe normalized tables.
  ``_source`` and ``_identity`` are private resolution data; do not publish them.
* resume_contract(profile) retains profile semantics and source paths while
  excluding source filesystem identities from the saved attempt comparison.
* validate_for_cell(profile, cell, harness, auth, environment_profile=None)
  raises ValueError without reading credentials or launching a provider.
* prepare(profile, ctx, harness) runs only backend help/version capability checks,
  copies pinned snapshots, sets ctx["agent_configuration"], and returns a
  JSON-safe manifest. The caller persists that exact result in
  state["agent_configuration"] and agent-configuration.json before execution.
* launch_args/launch_env/launch_binary read that manifest (including from state)
  and verify installed content and configuration before worker/narrator launches.
  Paths in the persisted launch description use context-directory placeholders.
  It contains no auth values, source paths, or caller configuration.

This controls optional activation, not host filesystem access. Managed policy
and built-in features are explicitly unknown; require_absence is rejected.
Reporter contexts must be independent and must not carry a worker manifest.

Mechanisms checked against official docs and local help on 2026-10-07:
https://developers.openai.com/codex/skills
https://developers.openai.com/codex/config-schema.json
https://developers.openai.com/codex/config-reference
https://code.claude.com/docs/en/cli-reference
https://code.claude.com/docs/en/settings-reference
https://kiro.dev/docs/reference/settings/
https://kiro.dev/docs/custom-agents/configuration-reference/
https://kiro.dev/docs/getting-started/authentication/
"""

import copy
import fnmatch
import hashlib
import json
import os
import posixpath
import re
import shlex
import shutil
import stat
import subprocess
import tempfile
from pathlib import Path
from urllib.parse import unquote, urlsplit

_ID = re.compile(r"^[a-z0-9][a-z0-9-]{0,62}$")
_HASH = re.compile(r"^[a-f0-9]{64}$")
_MODES = ("none", "selected", "snapshot")
_HARNESSES = ("codex", "claude-code", "kiro-cli")
_MAX_SKILLS, _MAX_FILES, _MAX_BYTES, _MAX_FILE, _MAX_DEPTH = 32, 2048, 32 * 1024**2, 8 * 1024**2, 16
_DENIED_NAMES = frozenset({
    "node_modules", "__pycache__", "venv", "env", "cache", "caches",
    "credentials", "credentials.json", "auth.json", "token", "tokens", "id_rsa", "id_ed25519",
})
_ROOTS = ("home_dir", "config_dir", "cache_dir", "workspace")
_ENV_EXACT = frozenset({
    "HOME", "USERPROFILE", "ZDOTDIR", "BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS",
    "NODE_OPTIONS", "NODE_PATH", "PYTHONPATH", "PYTHONHOME", "PYTHONSTARTUP",
    "CDPATH", "GLOBIGNORE", "PROMPT_COMMAND", "LD_PRELOAD", "LD_LIBRARY_PATH",
    "DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH",
    "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
})
_ENV_PREFIXES = ("CODEX_", "CLAUDE_", "KIRO_", "Q_", "XDG_", "LD_", "DYLD_", "BASH_FUNC_")
# Auth/provider settings do not choose discovery scopes or install extensions.
_PROVIDER_ENV = frozenset({
    "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
    "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_SKIP_BEDROCK_AUTH", "KIRO_API_KEY",
})
_COMMON_ENV = {
    "HOME": "{home_dir}", "XDG_CONFIG_HOME": "{config_dir}",
    "XDG_DATA_HOME": "{home_dir}/.local/share", "XDG_STATE_HOME": "{home_dir}/.local/state",
    "XDG_CACHE_HOME": "{cache_dir}", "ZDOTDIR": "{home_dir}",
}
_CODEX_CONFIG = {
    "features.plugins": False, "features.hooks": False, "features.apps": False,
    "features.skill_mcp_dependency_install": False, "skills.bundled.enabled": False,
    "notify": [], "allow_login_shell": False, "cli_auth_credentials_store": "file",
}
_CLAUDE_SETTINGS = {
    "disableAllHooks": True, "disableBundledSkills": True, "skillOverrides": {"doctor": "off"},
    "enabledPlugins": {}, "extraKnownMarketplaces": {}, "autoMemoryEnabled": False,
    "disableClaudeAiConnectors": True,
}
# We do not inspect these files' contents: policy may contain credentials.
_SYSTEM_PATHS = {
    "codex": ("/etc/codex/skills", "/etc/codex/config.toml", "/etc/codex/managed_config.toml",
              "/etc/codex/requirements.toml"),
    "claude-code": ("/Library/Application Support/ClaudeCode/managed-settings.json",
                    "/Library/Application Support/ClaudeCode/managed-settings.d",
                    "/etc/claude-code/managed-settings.json", "/etc/claude-code/managed-settings.d"),
    "kiro-cli": (),
}


def _need(condition, message):
    if not condition:
        raise ValueError(message)


def _name(harness):
    return harness if isinstance(harness, str) else harness.name


def _json_hash(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def _identity(st):
    return [st.st_dev, st.st_ino, st.st_mode, st.st_size, st.st_mtime_ns, st.st_ctime_ns]


def _safe_component(name):
    return (not name.startswith(".") and name.casefold() not in _DENIED_NAMES
            and not name.lower().endswith((".pem", ".key", ".p12", ".pfx", ".pyc"))
            and not any(ord(c) < 32 for c in name))


def _read_tree(path):
    """Read via no-follow directory descriptors; no symlink or special file is read.

    Retain identities to reject even a changed-and-restored source between
    resolution and copy. Content hashes themselves are independent of timestamps.
    """
    path = Path(path)
    records, identities, content, folded = [], {}, {}, set()
    size = 0
    flags = os.O_RDONLY | os.O_NOFOLLOW

    def walk(fd, prefix="", depth=0):
        nonlocal size
        _need(depth <= _MAX_DEPTH, "skill tree exceeds maximum directory depth")
        before = os.fstat(fd)
        names = sorted(os.listdir(fd))
        for name in names:
            rel = f"{prefix}/{name}" if prefix else name
            _need(_safe_component(name), f"skill contains disallowed hidden, secret, or cache entry: {rel}")
            _need(rel.casefold() not in folded, f"skill has a case-insensitive path collision: {rel}")
            folded.add(rel.casefold())
            _need(len(folded) <= _MAX_FILES, "skill tree has too many entries")
            st = os.stat(name, dir_fd=fd, follow_symlinks=False)
            _need(not stat.S_ISLNK(st.st_mode), f"skill symlinks are unsupported: {rel}; supply real files")
            _need(stat.S_ISDIR(st.st_mode) or stat.S_ISREG(st.st_mode),
                  f"skill contains a special file: {rel}")
            _need(not st.st_mode & (stat.S_ISUID | stat.S_ISGID), f"skill has privileged mode bits: {rel}")
            identities[rel] = _identity(st)
            child = os.open(name, flags | (os.O_DIRECTORY if stat.S_ISDIR(st.st_mode) else 0), dir_fd=fd)
            try:
                _need(_identity(os.fstat(child)) == _identity(st), f"skill changed while reading: {rel}")
                if stat.S_ISDIR(st.st_mode):
                    records.append({"path": rel, "type": "directory"})
                    walk(child, rel, depth + 1)
                else:
                    _need(st.st_nlink == 1, f"hard-linked skill file is unsupported: {rel}")
                    _need(st.st_size <= _MAX_FILE, f"skill file exceeds size limit: {rel}")
                    chunks, length = [], 0
                    while chunk := os.read(child, min(65536, _MAX_FILE + 1 - length)):
                        chunks.append(chunk)
                        length += len(chunk)
                        _need(length <= _MAX_FILE, f"skill file exceeds size limit: {rel}")
                    data = b"".join(chunks)
                    size += len(data)
                    _need(size <= _MAX_BYTES, "skill content exceeds snapshot size limit")
                    content[rel] = data
                    records.append({"path": rel, "type": "file", "bytes": len(data),
                                    "executable": bool(st.st_mode & 0o111),
                                    "sha256": hashlib.sha256(data).hexdigest()})
                _need(_identity(os.fstat(child)) == _identity(st), f"skill changed while reading: {rel}")
                _need(_identity(os.stat(name, dir_fd=fd, follow_symlinks=False)) == _identity(st),
                      f"skill changed while reading: {rel}")
            finally:
                os.close(child)
        _need(_identity(os.fstat(fd)) == _identity(before), "skill directory changed while reading")

    try:
        st = path.lstat()
        _need(stat.S_ISDIR(st.st_mode) and not stat.S_ISLNK(st.st_mode),
              "each skill path must be a real directory containing SKILL.md")
        fd = os.open(path, flags | os.O_DIRECTORY)
        try:
            _need(_identity(os.fstat(fd)) == _identity(st), "skill directory changed while opening")
            identities[""] = _identity(st)
            walk(fd)
        finally:
            os.close(fd)
        _need(_identity(path.lstat()) == _identity(st), "skill source directory changed while reading")
    except OSError as exc:
        raise ValueError(f"cannot safely read skill snapshot ({type(exc).__name__}); check files and permissions") from None
    _need("SKILL.md" in content, "each explicit skill directory must contain a regular SKILL.md")
    records.sort(key=lambda item: item["path"])
    return {"sha256": _json_hash(records), "files": records, "bytes": size,
            "_identity": identities, "_content": content}


def _frontmatter(data, folder_name):
    """Validate a deliberately small, portable subset without rewriting YAML.

    Unknown execution/dependency fields require a real harness-specific resolver,
    rather than guessing YAML semantics or silently enabling a hook.
    """
    try:
        text = data.decode("utf-8")
    except UnicodeError:
        raise ValueError("SKILL.md must be UTF-8") from None
    lines = text.splitlines()
    _need(lines and lines[0] == "---", "SKILL.md requires YAML frontmatter with name and description")
    try:
        end = lines.index("---", 1)
    except ValueError:
        raise ValueError("SKILL.md frontmatter needs a closing ---") from None
    allowed = {"name", "description", "license", "compatibility", "metadata",
               "disable-model-invocation", "user-invocable", "argument-hint"}
    fields, active = {}, None
    for line in lines[1:end]:
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if line[0].isspace():
            _need(active in ("description", "compatibility", "metadata"),
                  "unsupported nested SKILL.md frontmatter; only descriptive metadata may be nested")
            continue
        match = re.fullmatch(r"([a-z][a-z0-9-]*):(?:[ \t]+(.*)|[ \t]*)", line)
        _need(match is not None, "unsupported SKILL.md frontmatter syntax; use plain, quoted, or block fields")
        key, value = match.group(1), (match.group(2) or "").strip()
        _need(key in allowed, f"unsupported SKILL.md field {key!r}; hooks, tool grants, and dependencies need a resolver")
        _need(key not in fields, f"duplicate SKILL.md field: {key}")
        _need(not value.startswith(("&", "*", "!", "{", "[")), f"unsupported YAML value for SKILL.md {key}")
        fields[key], active = value, key
    name = fields.get("name", "")
    if name.startswith(("'", '"')):
        _need(len(name) >= 2 and name[-1] == name[0], "SKILL.md name has mismatched quotes")
        name = name[1:-1]
    _need(bool(_ID.fullmatch(name)) and "--" not in name,
          "SKILL.md name must be a lowercase skill slug (up to 63 characters)")
    _need(not name.endswith("-"), "SKILL.md name cannot end in a hyphen")
    _need(name == folder_name, "SKILL.md name must exactly match its directory name")
    _need(bool(fields.get("description")), "SKILL.md description is required")
    for key in ("disable-model-invocation", "user-invocable"):
        if key in fields:
            _need(fields[key] in ("true", "false"), f"SKILL.md {key} must be true or false")
    return {"name": name, "automatic_invocation": fields.get("disable-model-invocation") != "true",
            "user_invocable": fields.get("user-invocable") != "false",
            "invocation_fields": [k for k in ("disable-model-invocation", "user-invocable") if k in fields]}


def _check_skill_configuration(tree):
    # These files can add dependencies, hooks, permissions, agents, or extra skills.
    for entry in tree["files"]:
        rel = entry["path"]
        parts = rel.split("/")
        _need(parts[0] not in ("agents", "hooks", "plugins", "skills")
              and rel not in ("plugin.json", "hooks.json", "mcp.json", "settings.json"),
              f"unsupported skill configuration or dependency at {rel}; provide a standalone portable skill")
    # Validate explicit local Markdown links. Do not chase links outside the chosen
    # root or fetch remote content. Prose/shell code is not a dependency language.
    known = {item["path"] for item in tree["files"]} | {"."}
    for rel, data in tree["_content"].items():
        if Path(rel).suffix.lower() not in (".md", ".markdown"):
            continue
        try:
            text = data.decode("utf-8")
        except UnicodeError:
            continue
        links = re.findall(r"!?\[[^\]\n]*\]\(\s*<?([^\s)>]+)", text)
        links += re.findall(r"(?m)^\s{0,3}\[[^\]\n]+\]:\s*<?([^\s>]+)", text)
        for link in links:
            try:
                uri = urlsplit(link)
            except ValueError:
                raise ValueError(f"invalid link in skill file {rel}") from None
            if uri.scheme and uri.scheme not in ("file", "skill"):
                continue
            raw = unquote(uri.path)
            if not raw:
                continue  # An anchor in the same document.
            target = posixpath.normpath(posixpath.join(posixpath.dirname(rel), raw))
            _need(not uri.netloc and not raw.startswith(("/", "~", "\\")) and "\\" not in raw
                  and target != ".." and not target.startswith("../"),
                  f"local link in {rel} escapes the skill snapshot; bundle the referenced asset in the skill directory")
            _need(target in known, f"local link in {rel} references content missing from the skill snapshot")


def _category(raw, category, label):
    raw = {"mode": raw} if isinstance(raw, str) else raw
    _need(isinstance(raw, dict), f"{label}.{category} must be a table")
    unknown = set(raw) - {"mode", "paths", "pins"}
    _need(not unknown, f"{label}.{category}: unsupported fields {sorted(unknown)}")
    mode = raw.get("mode", "none")
    _need(mode in _MODES, f"{label}.{category}.mode must be none, selected, or snapshot")
    paths, pins = raw.get("paths", []), raw.get("pins", {})
    _need(isinstance(paths, list) and all(isinstance(p, str) and p.strip() for p in paths),
          f"{label}.{category}.paths must be an explicit list of local skill directories")
    _need(isinstance(pins, dict) and all(isinstance(k, str) and isinstance(v, str) and _HASH.fullmatch(v)
                                       for k, v in pins.items()), f"{label}.{category}.pins must map names to SHA256")
    _need(mode != "none" or not (paths or pins), f"{label}.{category}: mode none cannot include paths or pins")
    if category != "skills":
        _need(mode == "none", f"{label}.{category}: selected/snapshot {category} are unsupported; use mode none")
    else:
        _need(mode == "none" or paths, f"{label}.skills.{mode} requires explicit paths, one per SKILL.md directory")
    return {"mode": mode, "paths": paths, "pins": pins}


def normalize_profiles(raw_profiles, base_dir):
    """Resolve and hash explicit source trees once; never discover ambient skills."""
    _need(isinstance(raw_profiles, dict), "agent_configurations must be a table of named profiles")
    out = {}
    for name, raw in raw_profiles.items():
        label = f"agent_configurations.{name}"
        _need(isinstance(name, str) and bool(_ID.fullmatch(name)), "agent configuration names must be lowercase slugs")
        _need(isinstance(raw, dict), f"{label} must be a table")
        _need(not set(raw) - {"skills", "plugins", "hooks", "require_absence"},
              f"{label}: unsupported profile fields {sorted(set(raw) - {'skills', 'plugins', 'hooks', 'require_absence'})}")
        _need(isinstance(raw.get("require_absence", False), bool), f"{label}.require_absence must be boolean")
        profile = {"name": name, "require_absence": raw.get("require_absence", False)}
        for category in ("skills", "plugins", "hooks"):
            profile[category] = _category(raw.get(category, {}), category, label)
        skills, sources, names, total = profile["skills"], [], set(), 0
        _need(len(skills["paths"]) <= _MAX_SKILLS, f"{label}: too many skill sources (limit {_MAX_SKILLS})")
        for raw_path in skills.pop("paths"):
            _need(not any(c in raw_path for c in "*?[]{}\x00") and "://" not in raw_path,
                  f"{label}: skill paths must be local directories, without globs or variables")
            path = Path(raw_path).expanduser()
            if not path.is_absolute():
                path = Path(base_dir) / path
            _need(not path.is_symlink(), f"{label}: skill directory symlinks are unsupported")
            path = path.resolve()
            _need(path != Path(path.anchor) and path != Path.home().resolve() and _safe_component(path.name),
                  f"{label}: choose one bounded skill directory, not a home/configuration root")
            _need((path / "SKILL.md").is_file() and not (path / "SKILL.md").is_symlink(),
                  f"{label}: each path must name one directory with its own regular SKILL.md; collection roots are unsupported")
            tree = _read_tree(path)
            metadata = _frontmatter(tree["_content"]["SKILL.md"], path.name)
            _check_skill_configuration(tree)
            _need(metadata["name"].casefold() not in names, f"{label}: duplicate skill name {metadata['name']}")
            names.add(metadata["name"].casefold())
            pin = skills["pins"].get(metadata["name"])
            _need(pin is None or pin == tree["sha256"], f"{label}: SHA256 pin mismatch for {metadata['name']}")
            total += tree["bytes"]
            _need(total <= _MAX_BYTES, f"{label}: combined skill content exceeds snapshot size limit")
            sources.append({**metadata, "_source": str(path), "_identity": tree["_identity"],
                            "sha256": tree["sha256"], "files": tree["files"], "bytes": tree["bytes"]})
        _need(not set(skills.pop("pins")) - names, f"{label}: pins include an unselected skill")
        skills["entries"] = sources
        for category in ("plugins", "hooks"):
            profile[category] = {"mode": "none", "entries": []}
        out[name] = profile
    return out


def resume_contract(profile):
    """Keep profile semantics stable across source touches or identical checkouts.

    Filesystem identities protect the interval between resolution and copying.
    They are not part of a saved attempt's content contract; retain them on the
    original profile for _install's before/after integrity checks.
    """
    if profile is None:
        return None
    contract = copy.deepcopy(profile)
    for entry in contract["skills"]["entries"]:
        entry.pop("_identity", None)
    return contract


def _controlled_env(key):
    return key in _ENV_EXACT or (key.startswith(_ENV_PREFIXES) and key not in _PROVIDER_ENV)


def _validate_env(values, label):
    for key in values:
        _need(not _controlled_env(key), f"{label}.{key} conflicts with agent_configuration controls; remove the override")


def _validate_args(args, harness, label):
    """Allow only known harmless/model options; arbitrary flags are not a control boundary."""
    _need(isinstance(args, (list, tuple)) and all(isinstance(a, str) for a in args), f"{label} must be an argv list")
    values = {
        "codex": {"--model", "-m", "--sandbox", "-s", "--color"},
        "claude-code": {"--model", "--effort", "--max-budget-usd", "--permission-mode"},
        "kiro-cli": {"--model", "--effort", "--trust-tools"},
    }[harness]
    switches = {
        "codex": {"--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check"},
        "claude-code": {"--dangerously-skip-permissions", "--allow-dangerously-skip-permissions"},
        "kiro-cli": {"--trust-all-tools", "--no-interactive"},
    }[harness]
    i = 0
    while i < len(args):
        arg, sep, value = args[i].partition("=")
        # Codex provider args are necessary for custom endpoints and env-key auth.
        if harness == "codex" and (arg in ("-c", "--config") or args[i].startswith("-c")):
            if arg in ("-c", "--config") and not sep:
                i += 1
                _need(i < len(args), f"{label}: missing config assignment")
                value = args[i]
            elif arg not in ("-c", "--config"):
                value = args[i][2:]
            key, equals, _ = value.partition("=")
            _need(equals and "\n" not in value and "\r" not in value
                  and (key.strip() in ("model", "model_reasoning_effort", "model_provider")
                       or re.fullmatch(r"model_providers\.[A-Za-z0-9_-]+\.[A-Za-z0-9_]+", key.strip())),
                  f"{label}: config override conflicts with the explicit agent profile; only model/provider keys are allowed")
        elif arg in values:
            if not sep:
                i += 1
                _need(i < len(args) and not args[i].startswith("-"), f"{label}: missing value for {arg}")
        else:
            safe_flag = arg if re.fullmatch(r"--?[a-zA-Z0-9-]{1,64}", arg) else "<argument>"
            _need(arg in switches and not sep,
                  f"{label}: {safe_flag!r} is unsupported with an explicit agent profile; remove configuration/extension overrides")
        i += 1


def validate_for_cell(profile, cell, harness, auth, environment_profile=None):
    """Validate requested controls, override conflicts, and authentication compatibility."""
    if profile is None:
        return
    name = _name(harness)
    _need(name in _HARNESSES, f"agent_configuration is unsupported for harness {name!r}")
    _need(not profile.get("require_absence"),
          "agent_configuration.require_absence is unsupported: a fresh HOME disables discovery but cannot hide host "
          "skill paths or remove managed/built-in content; use activation controls or a qualified isolation backend")
    for category in ("plugins", "hooks"):
        _need(profile[category]["mode"] == "none" and not profile[category].get("entries"),
              f"{name}: selected {category} and their dependencies are unsupported; use mode none")
    for entry in profile["skills"]["entries"]:
        _need(name == "claude-code" or not entry["invocation_fields"],
              f"{name}: SKILL.md invocation controls for {entry['name']} are Claude-specific; semantics cannot be preserved")
    _validate_args(cell.get("args") or [], name, "cell.args")
    _validate_args(cell.get("permission_args") or [], name, "cell.permission_args")
    _validate_env(cell.get("env") or {}, "cell.env")
    runner = cell.get("runner") or {}
    runner_type = runner if isinstance(runner, str) else runner.get("type", "local")
    backend = (environment_profile or {}).get("backend", (environment_profile or {}).get("type", "local"))
    backend = backend.get("type", "local") if isinstance(backend, dict) else backend
    _need(runner_type in ("local", "container") and backend in ("local", "container"),
          "agent_configuration supports local or container EnvironmentRunner backends; "
          "use an explicit [environments.NAME] profile instead of a legacy Docker/wrapper runner")
    if auth is not None:
        _validate_args(auth.extra_args(), name, "auth.args")
        _validate_env((getattr(auth, "conf", {}) or {}).get("env") or {}, "auth.env")
        for pattern in auth.unset():
            _need(not any(fnmatch.fnmatch(k, pattern) for k in (*_COMMON_ENV, "CODEX_HOME", "CLAUDE_CONFIG_DIR", "KIRO_HOME")),
                  f"auth.unset {pattern!r} conflicts with isolated agent directories")
        _need(auth.isolates_config, f"{name}: auth {auth.name!r} requires caller configuration; use isolated env/API-key "
              "auth (Codex's copied auth.json is supported) for an explicit agent profile")
        if name == "claude-code":
            conf = getattr(auth, "conf", {}) or {}
            _need("CLAUDE_CODE_OAUTH_TOKEN" not in (conf.get("env") or {})
                  and "CLAUDE_CODE_OAUTH_TOKEN" not in (conf.get("required_env") or ()),
                  "claude-code profiles require API-key or cloud-provider auth; OAuth can sync unpinned account skills")
    if name == "kiro-cli":
        conf = getattr(auth, "conf", {}) or {}
        declared = set(conf.get("env") or {}) | set(conf.get("required_env") or ()) | set(cell.get("env") or {})
        _need(auth is not None and "KIRO_API_KEY" in declared,
              "kiro-cli profiles require an isolated env auth profile declaring KIRO_API_KEY in env or required_env; "
              "stored kiro-login cannot be reused with a fresh HOME")


def _roots(ctx):
    roots = {}
    for key in _ROOTS:
        value = ctx.get(key)
        _need(value and Path(value).is_absolute(), f"agent_configuration requires a fresh absolute ctx.{key}")
        path = Path(value)
        _need(path.is_dir() and not path.is_symlink(), f"ctx.{key} must be a real per-attempt directory")
        _need(not any("{" + other + "}" in str(path) for other in _ROOTS),
              f"ctx.{key} contains a reserved launch placeholder")
        roots[key] = path.resolve()
    for i, a in enumerate(_ROOTS):
        for b in _ROOTS[i + 1:]:
            _need(not roots[a].is_relative_to(roots[b]) and not roots[b].is_relative_to(roots[a]),
                  f"ctx.{a} and ctx.{b} must be separate directories, not nested or reused")
    _need(roots["home_dir"] != Path.home().resolve(), "agent_configuration must never use the caller's HOME")
    return roots


def _backend(ctx):
    runner = ctx.get("runner")
    return getattr(runner, "backend", getattr(runner, "name", "local"))


def _probe_context(ctx):
    """A backend probe never inherits auth or the worker's provider environment."""
    runner = ctx["runner"]
    env = dict(runner.environment_env(ctx))
    return {**ctx, "env": env, "check_env": env, "auth": None, "unset_env": []}


def _backend_shell(command, ctx, env=None):
    probe = _probe_context(ctx)
    return ctx["runner"].shell(command, probe, timeout=15, env=env or probe["env"])


def _scope_guard(ctx, harness):
    """Check in the execution backend, without reading any ambient settings."""
    workspace = Path(ctx["workspace"]).resolve()
    scopes = {
        "codex": (".agents/skills", ".codex/config.toml", ".codex/hooks.json", ".codex/skills"),
        "claude-code": (".claude/settings.json", ".claude/settings.local.json", ".claude/skills",
                        ".claude/commands", ".claude/plugins", ".claude/agents"),
        "kiro-cli": (".kiro/agents", ".kiro/skills", ".kiro/settings.json", ".kiro/mcp.json",
                     ".kiro/hooks", ".kiro/powers", ".agents/skills", ".claude/skills"),
    }[harness]
    checks = [(str(parent / rel), "workspace:" + rel) for parent in (workspace, *workspace.parents) for rel in scopes]
    checks += [(value, "admin-configuration") for value in _SYSTEM_PATHS[harness]]
    if hasattr(ctx.get("runner"), "shell"):
        script = "\n".join(
            f"if [ -e {shlex.quote(path)} ] || [ -L {shlex.quote(path)} ]; then "
            f"printf '%s\\n' {shlex.quote(label)}; exit 42; fi" for path, label in checks) + "\nexit 0"
        result = _backend_shell(script, ctx)
        _need(not result.get("timed_out") and result.get("exit_code") in (0, 42),
              f"{harness}: could not inspect discovery scopes inside the {_backend(ctx)} backend")
        found = result.get("stdout", "").strip() if result.get("exit_code") else ""
        _need(not found or found in {label for _, label in checks},
              f"{harness}: backend returned an invalid discovery-scope response")
    else:
        _need(_backend(ctx) == "local", "non-local profile probes require EnvironmentRunner.shell")
        found = next((label for path, label in checks if os.path.lexists(path)), "")
    if found == "admin-configuration":
        raise ValueError(f"{harness}: installed admin configuration/skills prevent verifying optional extension "
                         "controls; use an environment without that policy, or omit the explicit profile")
    _need(not found, f"{harness}: workspace or ancestor contains a discovery/configuration source "
          f"({found}), which can change explicit profile discovery; use a neutral workspace and fixture")


def _contained_path(location, ctx):
    """Resolve a recorded attempt-relative location without following replacement symlinks."""
    match = re.fullmatch(r"\{(home_dir|config_dir|cache_dir|workspace)\}/(.+)", location)
    _need(match is not None, "invalid recorded agent configuration location")
    relative = Path(match.group(2))
    _need(not relative.is_absolute() and ".." not in relative.parts, "unsafe recorded agent configuration location")
    path = Path(ctx[match.group(1)])
    for part in relative.parts:
        path /= part
        _need(not path.is_symlink(), "prepared agent configuration path was replaced by a symlink")
    return path


def _discovery_guard(ctx, manifest):
    """Check configured discovery roots for extra entries added by setup or a previous turn."""
    harness = manifest["harness"]
    names = {s["name"] for s in manifest["skills"]["entries"]}
    if harness == "codex":
        roots = {"{home_dir}/.agents/skills": names, "{config_dir}/skills": {".system"}}
        forbidden = ["{config_dir}/config.toml", "{config_dir}/hooks.json",
                     "{home_dir}/.codex/config.toml", "{home_dir}/.codex/skills"]
    elif harness == "claude-code":
        roots = {"{config_dir}/skills": names}
        forbidden = ["{config_dir}/commands", "{config_dir}/plugins", "{config_dir}/agents",
                     "{home_dir}/.claude/skills", "{home_dir}/.claude/commands",
                     "{config_dir}/settings.local.json", "{config_dir}/hooks.json"]
    else:
        roots = {"{config_dir}/skills": names, "{config_dir}/agents": {"ajx-profile.json"}}
        forbidden = ["{config_dir}/settings.json", "{config_dir}/mcp.json", "{config_dir}/hooks",
                     "{config_dir}/powers", "{home_dir}/.kiro/skills", "{home_dir}/.kiro/agents"]
    for location, expected in roots.items():
        path = _contained_path(location, ctx)
        if path.exists():
            _need(path.is_dir(), "prepared skill discovery root is no longer a directory")
            actual = {p.name for p in path.iterdir()}
            _need(actual <= expected, f"{harness}: additional configuration/skills appeared after preparation; "
                  "cannot launch the promised profile")
    for location in forbidden:
        _need(not os.path.lexists(_contained_path(location, ctx)),
              f"{harness}: additional harness configuration appeared after preparation")


def _validate_runtime_env(ctx, launch):
    expected = _expand(launch["env"], ctx)
    if "CLAUDE_CONFIG_DIR" in expected:
        _need("CLAUDE_CODE_OAUTH_TOKEN" not in (ctx.get("env") or {}),
              "claude-code profiles require API-key or cloud-provider auth; OAuth can sync unpinned account skills")
    for key, value in (ctx.get("env") or {}).items():
        if _controlled_env(key):
            _need(key in expected and str(value) == expected[key],
                  f"worker env {key} conflicts with the prepared agent profile")
    for pattern in ctx.get("unset_env") or ():
        _need(not any(fnmatch.fnmatch(k, pattern) for k in expected),
              f"worker unset_env {pattern!r} removes profile controls")


def _expand(value, ctx):
    if isinstance(value, str):
        for key in _ROOTS:
            value = value.replace("{" + key + "}", str(ctx[key]))
    elif isinstance(value, list):
        return [_expand(v, ctx) for v in value]
    elif isinstance(value, dict):
        return {k: _expand(v, ctx) for k, v in value.items()}
    return value


def _toml(value):
    if isinstance(value, dict):
        return "{" + ", ".join(json.dumps(k) + "=" + _toml(v) for k, v in value.items()) + "}"
    if isinstance(value, list):
        return "[" + ", ".join(_toml(v) for v in value) + "]"
    return json.dumps(value, ensure_ascii=True)


def _probe(harness, ctx):
    """Only help/version/config parsing, inside the chosen execution backend.

    Local discovery uses trusted caller PATH, not a synthetic worker PATH.
    A container resolves its own executable. The resulting absolute path is
    persisted and used verbatim, including when a worker later changes PATH.
    """
    binary = "kiro-cli-chat" if harness == "kiro-cli" else {"codex": "codex", "claude-code": "claude"}[harness]
    backend = _backend(ctx)
    runner = ctx.get("runner")
    if backend == "container":
        _need(hasattr(runner, "shell"), "container agent profiles require EnvironmentRunner.shell")
        result = _backend_shell("command -v " + shlex.quote(binary), ctx)
        if result.get("exit_code") and harness == "kiro-cli":
            result = _backend_shell("command -v kiro-cli", ctx)
        resolved = result.get("stdout", "").strip() if result.get("exit_code") == 0 else ""
    else:
        resolved = shutil.which(binary, path=os.environ.get("PATH", ""))
    _need(resolved and resolved.startswith("/") and "\n" not in resolved and "\0" not in resolved,
          f"{harness}: no supported CLI executable in the {backend} backend; install it in the pinned image "
          "or on the local host before preparing a profile"
          + (" (Kiro's separate chat companion is needed when its launcher depends on the original HOME)"
             if harness == "kiro-cli" else ""))
    help_args = {"codex": ["exec", "--help"], "claude-code": ["--help"], "kiro-cli": ["chat", "--help"]}[harness]
    required = {
        "codex": ("--strict-config", "--config", "--disable"),
        "claude-code": ("--setting-sources", "--settings", "--disable-slash-commands", "--strict-mcp-config"),
        "kiro-cli": ("--agent", "--agent-engine", "--no-interactive"),
    }[harness]
    with tempfile.TemporaryDirectory(prefix="capabilities-", dir=ctx["cache_dir"]) as scratch:
        if hasattr(runner, "shell"):
            # EnvironmentRunner enforces its directory paths. Preparation precedes
            # auth.prepare, so these are empty harness directories, not a login home.
            env = {**runner.environment_env(ctx), "CODEX_HOME": str(ctx["config_dir"]),
                   "CLAUDE_CONFIG_DIR": str(ctx["config_dir"]), "KIRO_HOME": str(ctx["config_dir"]), "TERM": "dumb"}
        else:
            env = {"PATH": os.path.dirname(resolved) + os.pathsep + "/usr/bin:/bin", "HOME": scratch,
                   "CODEX_HOME": scratch, "CLAUDE_CONFIG_DIR": scratch, "KIRO_HOME": scratch,
                   "XDG_CONFIG_HOME": scratch, "XDG_DATA_HOME": scratch, "XDG_CACHE_HOME": scratch, "TERM": "dumb"}

        def run(args, filter_lines=()):
            if hasattr(runner, "shell"):
                command = shlex.join([resolved, *args])
                if filter_lines:
                    # Runner output is deliberately bounded. Filter long help text
                    # inside the backend so flags at its beginning are not lost.
                    patterns = "|".join("*" + shlex.quote(text) + "*" for text in filter_lines)
                    command = (
                        f"ajx_probe_text=$({command}); ajx_probe_status=$?; "
                        '[ "$ajx_probe_status" -eq 0 ] || exit "$ajx_probe_status"; '
                        'printf \'%s\\n\' "$ajx_probe_text" | while IFS= read -r ajx_probe_line; do '
                        f'case "$ajx_probe_line" in {patterns}) printf \'%s\\n\' "$ajx_probe_line";; esac; done')
                result = _backend_shell(command, ctx, env)
                _need(not result.get("timed_out") and result.get("exit_code") == 0,
                      f"{harness}: {backend} help/config check rejected required controls or could not run "
                      "the executable with the environment's PATH")
                return result.get("stdout", "")
            try:
                result = subprocess.run([resolved, *args], cwd=scratch, env=env, capture_output=True,
                                        text=True, timeout=15)
            except (OSError, subprocess.TimeoutExpired):
                raise ValueError(f"{harness}: local capability check failed; no worker was launched") from None
            _need(result.returncode == 0, f"{harness}: local help/config check rejected required controls")
            return result.stdout

        version = run(["--version"]).strip()
        match = re.search(r"(?<![0-9])([0-9]+)\.([0-9]+)\.([0-9]+)", version)
        _need(match, f"{harness}: cannot determine CLI version for profile controls")
        actual = tuple(int(v) for v in match.groups())
        minimum = {"codex": (0, 160, 1), "claude-code": (2, 1, 293), "kiro-cli": (2, 28, 0)}[harness]
        _need(actual >= minimum and actual[0] == minimum[0],
              f"{harness}: profile controls require version {'.'.join(map(str, minimum))} or newer in the same major")
        help_text = run(help_args, required)
        _need(all(flag in help_text for flag in required),
              f"{harness}: installed CLI help is missing required profile controls ({', '.join(required)})")
        if harness == "codex":
            # This CLI supports --strict-config for exec/resume, not `features`.
            # Inspect feature values here; exec/resume apply strict parsing later.
            args = []
            for key, value in _CODEX_CONFIG.items():
                args += ["-c", key + "=" + _toml(value)]
            features = ("plugins", "hooks", "apps", "skill_mcp_dependency_install")
            flags = run([*args, "features", "list"], features)
            for feature in features:
                _need(re.search(rf"(?m)^{feature}\s+.*\bfalse\s*$", flags),
                      f"codex: cannot verify disabled feature {feature}; unsupported CLI")
    return {"binary": resolved, "version": ".".join(match.groups()), "backend": backend,
            "method": "backend --version/--help" if hasattr(runner, "shell") else "local --version/--help",
            "config_parse_checked": False, "feature_overrides_checked": harness == "codex", "provider_called": False,
            "live_extension_activation_verified": False}


def _bootstrap_guard(ctx, roots, allowed_auth):
    """Allow only the runner's known empty bootstrap directories before copying skills."""
    home = roots["home_dir"]
    allowed_dirs = set()
    runner = ctx.get("runner")
    if hasattr(runner, "environment_env"):
        values = runner.environment_env(ctx).values()
        for value in values:
            candidate = Path(value)
            if candidate.is_absolute() and candidate.is_relative_to(home) and ":" not in value:
                allowed_dirs.add(candidate)
                allowed_dirs.update(p for p in candidate.parents if p.is_relative_to(home))
        allowed_dirs.add(home / ".local/bin")
        allowed_dirs.add(home / ".local")
    for current, dirs, files in os.walk(home, followlinks=False):
        _need(not files, "agent_configuration requires a fresh empty home_dir (bootstrap directories only)")
        for name in dirs:
            path = Path(current) / name
            _need(not path.is_symlink() and path in allowed_dirs,
                  "agent_configuration home_dir contains unexpected configuration; only empty bootstrap directories are allowed")
    _need({p.name for p in roots["config_dir"].iterdir()} <= allowed_auth,
          "agent_configuration requires a fresh config_dir (only copied Codex auth.json is allowed)")


def _write_file(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("xb") as stream:
        stream.write(data)
    path.chmod(0o600)


def _install(entry, target):
    before = _read_tree(entry["_source"])
    _need(before["sha256"] == entry["sha256"] and before["_identity"] == entry["_identity"],
          f"skill {entry['name']} changed after resolution; reload the trial to pin the new content")
    target.mkdir(parents=True, exist_ok=False)
    for item in before["files"]:
        dst = target / item["path"]
        if item["type"] == "directory":
            dst.mkdir(parents=True, exist_ok=True)
        else:
            _write_file(dst, before["_content"][item["path"]])
            dst.chmod(0o700 if item["executable"] else 0o600)
    after = _read_tree(entry["_source"])
    _need(after["sha256"] == entry["sha256"] and after["_identity"] == entry["_identity"],
          f"skill {entry['name']} changed during copy; reload the trial")
    copied = _read_tree(target)
    _need(copied["sha256"] == entry["sha256"], f"copied skill {entry['name']} failed content verification")


def _container_ownership(ctx):
    """Honor the runner's non-root UID when the host coordinator happens to be root."""
    if _backend(ctx) != "container" or os.geteuid() != 0:
        return
    for key in ("home_dir", "config_dir"):
        root = Path(ctx[key])
        owner = root.stat()
        for current, dirs, files in os.walk(root, followlinks=False):
            for name in dirs + files:
                path = Path(current) / name
                _need(not path.is_symlink(), "agent configuration changed to a symlink during preparation")
                os.chown(path, owner.st_uid, owner.st_gid, follow_symlinks=False)


def prepare(profile, ctx, harness):
    """Prepare isolated snapshots and return the manifest to persist before setup."""
    if profile is None:
        return {}
    name = _name(harness)
    validate_for_cell(profile, ctx["cell"], harness, ctx.get("auth"), ctx.get("environment_profile"))
    backend = _backend(ctx)
    _need(backend in ("local", "container"),
          "agent_configuration requires a local or container EnvironmentRunner backend")
    _validate_env((ctx.get("spec") or {}).get("env") or {}, "trial.env")
    roots = _roots(ctx)
    # auth.prepare may have copied Codex's auth.json; do not read it or archive its values.
    allowed = {"auth.json"} if name == "codex" and getattr(ctx.get("auth"), "name", None) == "codex-chatgpt" else set()
    _bootstrap_guard(ctx, roots, allowed)
    _scope_guard(ctx, name)
    capability = _probe(name, ctx)
    env = dict(_COMMON_ENV)
    launch = {"binary": capability["binary"], "args": [], "env": env, "config": {}}
    if name == "codex":
        env["CODEX_HOME"] = "{config_dir}"
        launch["args"] = ["--strict-config"]
        launch["config"] = dict(_CODEX_CONFIG)
        skill_root = "{home_dir}/.agents/skills"
    elif name == "claude-code":
        env.update(CLAUDE_CONFIG_DIR="{config_dir}", DISABLE_AUTOUPDATER="1")
        launch["args"] = ["--setting-sources", "user", "--settings", "{config_dir}/settings.json", "--strict-mcp-config"]
        if profile["skills"]["mode"] == "none":
            launch["args"] += ["--disable-slash-commands"]
        skill_root = "{config_dir}/skills"
    else:
        env["KIRO_HOME"] = "{config_dir}"
        launch["args"] = ["--agent-engine", "v2", "--agent", "ajx-profile", "--no-interactive"]
        skill_root = "{config_dir}/skills"
    _validate_runtime_env(ctx, launch)
    skills, files, owned = [], [], []
    try:
        for entry in profile["skills"]["entries"]:
            location = skill_root + "/" + entry["name"]
            target = Path(_expand(location, ctx))
            owned.append(target)
            _install(entry, target)
            skills.append({k: entry[k] for k in ("name", "sha256", "bytes", "files", "automatic_invocation", "user_invocable")})
            skills[-1].update(id="skill:" + entry["name"], source=entry["name"], location=location,
                              installed=True, discoverable="unknown", discovery_configured=True, enabled=True,
                              enabled_basis="launch configuration", loaded="unknown", invoked="unknown",
                              discovery_scope="isolated user" if name != "kiro-cli" else "explicit agent resource")
        configs = {}
        if name == "codex":
            launch["config"]["skills.config"] = [
                {"path": skill["location"] + "/SKILL.md", "enabled": True} for skill in skills]
        elif name == "claude-code":
            configs["{config_dir}/settings.json"] = _CLAUDE_SETTINGS
        else:
            configs["{config_dir}/agents/ajx-profile.json"] = {
                "name": "ajx-profile", "description": "Per-attempt skill profile",
                "tools": ["@builtin"], "allowedTools": [], "mcpServers": {}, "includeMcpJson": False,
                "resources": ["skill://" + _expand(s["location"], ctx) + "/SKILL.md" for s in skills], "hooks": {},
            }
        for location, conf in configs.items():
            path = Path(_expand(location, ctx))
            data = (json.dumps(conf, sort_keys=True, indent=2) + "\n").encode()
            _write_file(path, data)
            owned.append(path)
            files.append({"location": location, "sha256": hashlib.sha256(data).hexdigest()})
        _container_ownership(ctx)
    except Exception:
        for path in reversed(owned):
            if path.is_dir():
                shutil.rmtree(path)
            else:
                path.unlink(missing_ok=True)
        raise
    manifest = {
        "schema_version": 1, "name": profile["name"], "harness": name, "backend": backend,
        "boundary": "optional activation", "content_absence": "unsupported",
        "capabilities": capability, "skills": {"mode": profile["skills"]["mode"], "entries": skills},
        "plugins": {"mode": "none", "entries": [], "optional_activation": "disabled", "invoked": "unknown"},
        "hooks": {"mode": "none", "entries": [], "optional_activation": "disabled", "invoked": "unknown"},
        "uncontrolled_sources": [
            {"id": "harness-builtins", "installed": "unknown", "discoverable": "unknown", "enabled": "unknown",
             "loaded": "unknown", "invoked": "unknown", "control": "mandatory-or-unknown"},
            {"id": "managed-policy", "installed": "unknown", "discoverable": "unknown", "enabled": "unknown",
             "loaded": "unknown", "invoked": "unknown", "control": "unknown"},
        ],
        "limitations": [
            ("Local isolation does not hide absolute host skill paths from shell/file tools." if backend == "local"
             else "Container filesystem controls hide unmounted host paths; skills present in the pinned image "
             "or declared mounts can remain readable. Content absence is not verified by this profile."),
            "Managed policy and built-in features may remain; their absence is not verified.",
            "Harness instructions, model knowledge, and task files are not removed by extension controls.",
            "All files within selected skill directories are pinned. External prose/runtime dependencies and "
            "remote resources are not resolved or snapshotted.",
            "No model was called to verify activation; loaded/invoked remain unknown without telemetry.",
            "Installed content/configuration is checked again before execution and narration; writes during a turn "
            "are not prevented by this profile.",
        ],
        "narration": "retains worker launch configuration and verifies pinned snapshots",
        "configuration_files": files, "launch": launch,
    }
    ctx["agent_configuration"] = manifest
    return manifest


def configuration(ctx):
    """Return only an explicitly prepared profile, never infer from config=clean."""
    return ctx.get("agent_configuration") or (ctx.get("state") or {}).get("agent_configuration")


def _verified(ctx, harness):
    manifest = configuration(ctx)
    if not manifest:
        _need(not ctx.get("cell", {}).get("agent_configuration"),
              "agent_configuration was selected but not prepared; prepare and persist it before launching")
        return None
    name = _name(harness)
    _need(manifest.get("schema_version") == 1 and manifest.get("harness") == name,
          "agent configuration manifest does not match this harness; prepare a new attempt")
    _roots(ctx)
    _scope_guard(ctx, name)
    _validate_args(ctx.get("cell", {}).get("args") or [], name, "cell.args")
    _validate_args(ctx.get("cell", {}).get("permission_args") or [], name, "cell.permission_args")
    _validate_env(ctx.get("cell", {}).get("env") or {}, "cell.env")
    _validate_env((ctx.get("spec") or {}).get("env") or {}, "trial.env")
    auth = ctx.get("auth")
    if auth:
        _validate_args(auth.extra_args(), name, "auth.args")
        _validate_env((getattr(auth, "conf", {}) or {}).get("env") or {}, "auth.env")
    _validate_runtime_env(ctx, manifest["launch"])
    _discovery_guard(ctx, manifest)
    for entry in manifest["skills"]["entries"]:
        snapshot = _read_tree(_contained_path(entry["location"], ctx))
        _need(snapshot["sha256"] == entry["sha256"],
              f"installed skill {entry['name']} changed after preparation; cannot retain the worker profile")
    for entry in manifest["configuration_files"]:
        path = _contained_path(entry["location"], ctx)
        _need(not path.is_symlink() and path.is_file(), "prepared harness configuration is missing or replaced")
        _need(hashlib.sha256(path.read_bytes()).hexdigest() == entry["sha256"],
              "prepared harness configuration changed; cannot launch with the promised profile")
    return manifest


def launch_args(ctx, harness):
    manifest = _verified(ctx, harness)
    if not manifest:
        return []
    launch = manifest["launch"]
    args = _expand(launch["args"], ctx)
    for key, value in launch["config"].items():
        args += ["-c", key + "=" + _toml(_expand(value, ctx))]
    return args


def launch_env(ctx, harness):
    # clean_env is also used by doctor before prepare; legacy behavior can apply there.
    manifest = configuration(ctx)
    if not manifest:
        return {}
    _need(manifest["harness"] == _name(harness), "profile environment belongs to a different harness")
    return _expand(manifest["launch"]["env"], ctx)


def launch_binary(ctx, harness):
    manifest = configuration(ctx)
    return manifest["launch"]["binary"] if manifest else harness.binary


def launch_context(ctx):
    """Drop inherited extension/shell injection variables only for profiled launches.

    Harness.run reapplies launch_env after these inherited values are dropped.
    Preserve provider variables, except Claude OAuth (account skill syncing).
    """
    if not configuration(ctx):
        return ctx
    if hasattr(ctx.get("runner"), "child_env"):
        # EnvironmentRunner constructs a full environment without inheritance and
        # deliberately rejects unsetting its HOME/XDG controls.
        return ctx
    drop = sorted(_ENV_EXACT | {k for k in os.environ if _controlled_env(k)} | {"CLAUDE_CODE_OAUTH_TOKEN"})
    return {**ctx, "unset_env": list(ctx.get("unset_env") or []) + drop}


def ensure_reporter_context(ctx):
    _need(not configuration(ctx) and not ctx.get("cell", {}).get("agent_configuration"),
          "reporter must use a separate fixed context without the worker agent_configuration")


__all__ = ["normalize_profiles", "resume_contract", "validate_for_cell", "prepare", "configuration",
           "launch_args", "launch_env", "launch_binary", "launch_context", "ensure_reporter_context"]
