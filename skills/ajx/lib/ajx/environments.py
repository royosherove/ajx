"""Explicit local and container worker environments (standard library only).

Public contract
---------------
normalize_profiles(raw_profiles, base_dir) -> {name: normalized_profile}
validate_for_cell(profile, cell) -> None; configuration errors are ValueError.
EnvironmentRunner(normalized_profile) extends base.Runner and exposes:
  backend, plan(ctx), prepare(ctx), doctor(ctx=None), describe(),
  environment_env(ctx), child_env(overrides=None, unset=()),
  host_env(ctx, overrides=None, unset=()),
  wrap(argv, ctx, env), shell(command, ctx, timeout=600, env=None),
  abort(ctx), release(ctx).

Persist plan() VERBATIM in ctx["state"]["environment"] before prepare(). Store
prepare() separately, for example in state["environment_report"]. Its fields
include status, image, platform, inventory, capabilities, mounts, env_names,
and limitations. Inventory has phase ("initial" or "recovered") and tools;
each declared tool probe records its exit status and bounded output. Capability
entries distinguish declared policy from state/mechanism/observations.

Profiles under [environments.NAME] accept only:
  id (optional; must equal NAME), backend ("local" or "container"),
  image (container only: full sha256 image ID or repository@sha256:digest),
  platform (optional Linux os/arch[/variant]), network,
  tools = [{name, argv = ["tool", "--version"], required = true}],
  mounts = [{source, target, access = "read" | "write"}],
  required = [capability names],
  permissions = {filesystem, root, package_install},
  limits = {cpus, memory_mb, pids, tmpfs_mb, lifetime_seconds},
  docker_host (optional local unix:// socket; never a remote daemon).
The internal normalized _base_dir field is not a TOML setting.

Local defaults: network="inherit", permissions={filesystem="observed",
root="inherit", package_install="unrestricted"}, limits={}. It is NOT a
filesystem, network, or sudo sandbox. Required isolation is rejected.
Container defaults: network="none" (or explicitly "bridge"), permissions=
{filesystem="isolated", root="forbid", package_install="user"}, limits=
{cpus=1, memory_mb=1024, pids=256, tmpfs_mb=64, lifetime_seconds=3600}.
Supported required names: filesystem_isolation, network_isolation, non_root,
resource_limits, lifetime. Bridge is unrestricted outbound networking, including
reachable host/LAN/metadata services; it is not a public-only policy.

Directory sources must be bounded fixture directories beneath base_dir, never
the trial root, credentials, reports, symlinks, sockets, or special files.
Read mounts bind declared fixtures read-only, excluding recursive submounts.
Write mounts COPY the declared starting directory into the attempt's workspace;
their writes are archived with that workspace and never change the source.
All four ctx directories (workspace/config_dir/cache_dir/home_dir) are distinct
private roots, disjoint from run_dir. HOME/config/cache start empty unless the
caller has explicitly staged files in roots recorded in state["owned_paths"].
No automatic
host configuration, credential, sibling-workspace, or engine-socket mounts exist.

Images must already be present. Tags are rejected, so matrix cells cannot resolve
a changing tag differently. plan() only performs read-only image/daemon inspection.
prepare() starts one persistent container with a non-root worker, root-owned
bounded supervisor, --init, --rm, read-only rootfs, no capabilities, and
no-new-privileges. Installed USER tools persist across setup/execute/narration.
Root/system installation and a ban on installing files in writable space are
unsupported. The image's OS files are trusted runtime input, not a sandbox proof.

Only the runner instance that made a fresh plan can create its container, once.
Recovered instances may attach to an existing, matching, running container; they
NEVER create, restart, or replay a missing/expired attempt. Ownership labels are
checked before removal, then removal uses the full immutable container ID.
Call abort(ctx) immediately after a wrapped harness timeout: killing the Docker
client alone does not kill docker-exec workers. shell() does this automatically.
Local abort relies on the caller to reap process groups and keeps the prepared
environment available for cleanup commands until final release().
For wrapped launches, pass environment_env(ctx) plus declared/auth overrides to
both wrap() and child_env(); the latter is the actual subprocess environment.
Independent supervisor expiry stops workers if the coordinator disappears.
release() is idempotent and preserves host evidence for the caller's archive.
Cleanup returns {status, confirmed, ...}; an unreachable daemon is not success.
Local process escape and coordinator-death cleanup cannot be guaranteed.

host_env() is for trusted host checks/lifecycle commands. It retains only
explicit ctx env/check_env/overrides, rebuilds configuration and package paths
under run_dir/host-environment (0700, never worker-mounted), and uses the
coordinator PATH captured when this runner was constructed. Relative/empty
entries and paths resolving inside worker-owned directories are excluded.
Worker config/startup/loader overrides cannot change those controls. Standard
Python cwd/user-site discovery is disabled. Explicit credential/config file
variables must point outside worker-writable roots. This is environment
separation, not a host sandbox: trusted checks must not execute/source submitted
task code or opt back into a tool's project-level startup/configuration discovery.
"""

import copy
import hashlib
import json
import math
import os
import platform as host_platform
import re
import shlex
import shutil
import signal
import stat
import subprocess
import tempfile
import threading
import time
import uuid
from pathlib import Path, PurePosixPath

from .base import Runner
from .util import now


_NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,63}\Z")
_ENV_NAME = re.compile(r"[A-Za-z_][A-Za-z0-9_]*\Z")
_IMAGE_ID = re.compile(r"sha256:[0-9a-f]{64}\Z")
_IMAGE = re.compile(r"(?:sha256:[0-9a-f]{64}|[a-z0-9][a-z0-9._:/-]*@sha256:[0-9a-f]{64})\Z")
_CONTAINER_ID = re.compile(r"[0-9a-f]{64}\Z")
_OWNER = re.compile(r"[0-9a-f]{32}\Z")
_PLATFORM = re.compile(r"linux/(?:amd64|arm64|arm|386|ppc64le|s390x|riscv64)(?:/v[0-9]+)?\Z")
_PATH = "/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin"
_MANAGER_PATH = "/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin"
_BASE_ENV = {"PATH": _PATH, "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8",
             "TZ": "UTC", "SHELL": "/bin/sh", "TERM": "dumb", "HOME": "/nonexistent"}
_ROOTS = ("workspace", "config_dir", "cache_dir", "home_dir")
_LIMITS = {"cpus": 1.0, "memory_mb": 1024, "pids": 256, "tmpfs_mb": 64, "lifetime_seconds": 3600}
_REQUIRED = {"filesystem_isolation", "network_isolation", "non_root", "resource_limits", "lifetime"}
_PROFILE_KEYS = {"id", "backend", "image", "platform", "network", "tools", "mounts",
                 "required", "permissions", "limits", "docker_host"}
_ISOLATION_ENV = {"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
                  "TMPDIR", "TMP", "TEMP", "PYTHONUSERBASE", "npm_config_cache", "npm_config_prefix",
                  "CARGO_HOME", "RUSTUP_HOME", "GOPATH", "GOCACHE", "PIP_CACHE_DIR", "UV_CACHE_DIR"}
_UNSAFE_ENV = {"ENV", "BASH_ENV", "CDPATH", "GLOBIGNORE", "SHELLOPTS", "BASHOPTS", "PROMPT_COMMAND",
               "PYTHONHOME", "PYTHONPATH", "SSH_AUTH_SOCK", "SSH_AGENT_PID"}
_UNSAFE_PREFIXES = ("LD_", "DYLD_", "DOCKER_", "BASH_FUNC_")
_HOST_DROP = _UNSAFE_ENV | {
    "PYTHONSTARTUP", "PYTHONINSPECT", "PYTHONSAFEPATH", "PYTHONNOUSERSITE", "PYTHONPYCACHEPREFIX",
    "NODE_OPTIONS", "NODE_PATH", "RUBYOPT", "RUBYLIB", "PERL5OPT", "PERL5LIB",
    "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_CONFIG_NOSYSTEM",
    "GIT_SSH", "GIT_SSH_COMMAND", "GIT_ASKPASS", "SSH_ASKPASS", "GIT_PROXY_COMMAND",
    "VIRTUAL_ENV", "CONDA_PREFIX", "CONDA_DEFAULT_ENV", "PIP_TARGET", "PIP_PREFIX",
    "BUNDLE_GEMFILE", "R_PROFILE", "R_PROFILE_USER", "R_ENVIRON", "R_ENVIRON_USER",
    "OPENSSL_CONF", "OPENSSL_MODULES", "OPENSSL_ENGINES",
}
_HOST_DROP_PREFIXES = _UNSAFE_PREFIXES + ("GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_", "_PYTHON_")
_HOST_EXTERNAL_FILES = {
    "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_WEB_IDENTITY_TOKEN_FILE",
    "GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_CONFIG", "AZURE_CONFIG_DIR",
    "BOTO_CONFIG", "BOTO_PATH", "KUBECONFIG", "NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE",
    "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
}
_FORBIDDEN_NAMES = {
    ".aws", ".ssh", ".docker", ".kube", ".azure", ".gcloud", ".config", ".cache",
    ".codex", ".claude", ".kiro", ".git", ".ajx-local", ".env", ".netrc",
    "ajx-reports", "reports", "credentials", "credentials.json", "auth.json",
    "state.json", "run.json", "report.html", "measurements.json",
}
_SYSTEM_TARGETS = ("/bin", "/sbin", "/usr", "/lib", "/lib64", "/etc", "/proc", "/sys",
                   "/dev", "/run", "/root", "/boot")
_MAX_FILES = 10000
_MAX_BYTES = 64 * 1024 * 1024
_LABEL_PREFIX = "io.ajx.environment."


class EnvironmentError(RuntimeError):
    """An environment could not be established or safely operated."""


def _fail(field, message):
    raise ValueError(f"{field}: {message}")


def _table(value, field, allowed):
    if not isinstance(value, dict) or any(not isinstance(k, str) for k in value):
        _fail(field, "expected a table")
    extra = set(value) - set(allowed)
    if extra:
        _fail(f"{field}.{sorted(extra)[0]}", "unknown field; unsupported constraints are not ignored")
    return value


def _choice(value, options, field):
    if not isinstance(value, str) or value not in options:
        _fail(field, "expected " + " | ".join(sorted(options)))
    return value


def _strings(value, field):
    if not isinstance(value, list) or any(not isinstance(v, str) or not v or "\0" in v for v in value):
        _fail(field, "expected an array of nonempty strings without NUL bytes")
    return list(value)


def _arguments(value, field):
    if not isinstance(value, (list, tuple)) or any(not isinstance(v, str) or "\0" in v for v in value):
        _fail(field, "expected an argument array of strings without NUL bytes")
    if not value or not value[0] or value[0].startswith("-"):
        _fail(field, "expected an executable followed by optional arguments")
    # Empty argument values are significant, for example Claude's --tools "".
    return list(value)


def _number(value, field, minimum, maximum, integer=True):
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        _fail(field, "expected a number")
    if not minimum <= value <= maximum or not math.isfinite(value) or (integer and int(value) != value):
        _fail(field, f"expected {'an integer' if integer else 'a number'} between {minimum} and {maximum}")
    return int(value) if integer else float(value)


def _overlaps(a, b):
    a, b = Path(a), Path(b)
    return a == b or a.is_relative_to(b) or b.is_relative_to(a)


def _path_text(value, field):
    if not isinstance(value, (str, os.PathLike)):
        _fail(field, "expected a directory path")
    text = os.fspath(value)
    if not text or any(c in text for c in '\0\n\r,":') or "\\" in text:
        _fail(field, "path is empty or contains unsupported control/mount-syntax characters")
    if ".." in PurePosixPath(text).parts or text.startswith("~") or "$" in text:
        _fail(field, "path traversal and shell/environment expansion are not permitted")
    return text


def _directory(value, field, *, exists=False):
    """Reject user-controlled symlink components, including missing-leaf parents."""
    path = Path(_path_text(value, field))
    if not path.is_absolute():
        _fail(field, "expected an absolute path")
    for part in (path, *path.parents):
        # Standard macOS aliases are not user-selected fixture symlinks.
        if (host_platform.system() == "Darwin" and str(part) in ("/tmp", "/var", "/etc")
                and str(part.resolve()) == "/private" + str(part)):
            continue
        if part.is_symlink():
            _fail(field, "symlink directory components are not permitted")
    path = path.resolve()
    if path.exists() and not path.is_dir():
        _fail(field, "expected a directory, not a file, socket, or device")
    if exists and not path.is_dir():
        _fail(field, "directory does not exist")
    return path


def _forbidden(path):
    return any(p.lower() in _FORBIDDEN_NAMES or p.lower().endswith((".sock", ".pem", ".key"))
               or p.lower().startswith(".env.") for p in Path(path).parts)


def _open_directory(path):
    """Open each component relative to an already open parent, never following symlinks."""
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
    fd = os.open("/", flags)
    try:
        for component in Path(path).parts[1:]:
            child = os.open(component, flags, dir_fd=fd)
            os.close(fd)
            fd = child
        return fd
    except BaseException:
        os.close(fd)
        raise


def _tree_manifest(root, field, *, fixtures=True, destination=None):
    """Bound/hash and optionally copy a tree using no-follow descriptor-relative reads."""
    root = _directory(root, field, exists=True)
    entries, total = [], 0
    dir_flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW

    def visit(directory_fd, prefix="", destination_fd=None):
        nonlocal total
        for name in sorted(os.listdir(directory_fd)):
            rel = prefix + name
            if fixtures and _forbidden(rel):
                _fail(field, "fixture tree contains a credential/configuration/report path")
            info = os.stat(name, dir_fd=directory_fd, follow_symlinks=False)
            if stat.S_ISLNK(info.st_mode):
                _fail(field, "symlinks in starting directories are not permitted")
            if stat.S_ISDIR(info.st_mode):
                entries.append([rel + "/", "directory"])
                if rel.count("/") >= 64 or len(entries) > _MAX_FILES:
                    _fail(field, "starting directory exceeds the nesting/entry limit")
                child_fd, output_fd = None, None
                try:
                    child_fd = os.open(name, dir_flags, dir_fd=directory_fd)
                    opened = os.fstat(child_fd)
                    if (opened.st_dev, opened.st_ino) != (info.st_dev, info.st_ino):
                        _fail(field, "starting directory changed while being inspected")
                    if destination_fd is not None:
                        os.mkdir(name, mode=0o700, dir_fd=destination_fd)
                        output_fd = os.open(name, dir_flags, dir_fd=destination_fd)
                    visit(child_fd, rel + "/", output_fd)
                finally:
                    if child_fd is not None:
                        os.close(child_fd)
                    if output_fd is not None:
                        os.close(output_fd)
            elif stat.S_ISREG(info.st_mode):
                if info.st_nlink != 1:
                    _fail(field, "hardlinked files are not permitted")
                total += info.st_size
                if total > _MAX_BYTES:
                    _fail(field, f"starting directory exceeds {_MAX_BYTES} bytes")
                fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory_fd)
                with os.fdopen(fd, "rb") as stream:
                    opened = os.fstat(stream.fileno())
                    if ((opened.st_dev, opened.st_ino, opened.st_size) != (info.st_dev, info.st_ino, info.st_size)
                            or not stat.S_ISREG(opened.st_mode) or opened.st_nlink != 1):
                        _fail(field, "starting directory changed while being inspected")
                    digest = hashlib.sha256()
                    count = 0
                    output = None
                    try:
                        if destination_fd is not None:
                            output_fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                                                0o700 if info.st_mode & 0o111 else 0o600, dir_fd=destination_fd)
                            output = os.fdopen(output_fd, "wb")
                        for chunk in iter(lambda: stream.read(65536), b""):
                            count += len(chunk)
                            if count > info.st_size:
                                _fail(field, "starting directory changed while being inspected")
                            digest.update(chunk)
                            if output is not None:
                                output.write(chunk)
                        final = os.fstat(stream.fileno())
                        if (count != info.st_size or final.st_mtime_ns != info.st_mtime_ns
                                or final.st_ctime_ns != info.st_ctime_ns or final.st_nlink != 1):
                            _fail(field, "starting directory changed while being inspected")
                    finally:
                        if output is not None:
                            output.close()
                entries.append([rel, info.st_size, bool(info.st_mode & 0o111), digest.hexdigest()])
            else:
                _fail(field, "sockets, devices, FIFOs, and other special files are not permitted")
            if len(entries) > _MAX_FILES:
                _fail(field, f"starting directory exceeds {_MAX_FILES} entries")
    input_fd, output_fd = None, None
    try:
        input_fd = _open_directory(root)
        if destination is not None:
            destination = Path(destination)
            destination.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            destination.mkdir(mode=0o700, exist_ok=False)
            output_fd = _open_directory(destination)
        visit(input_fd, destination_fd=output_fd)
    except OSError as exc:
        _fail(field, f"cannot safely inspect/copy directory ({type(exc).__name__}); it may have changed")
    finally:
        if input_fd is not None:
            os.close(input_fd)
        if output_fd is not None:
            os.close(output_fd)
    return {"sha256": hashlib.sha256(json.dumps(sorted(entries), separators=(",", ":")).encode()).hexdigest(),
            "entries": len(entries), "bytes": total}


def _target(value, field):
    text = _path_text(value, field)
    path = PurePosixPath(text)
    if not path.is_absolute() or str(path) != text or text in ("/", "/tmp", "/var", "/home", "/opt"):
        _fail(field, "expected a normalized absolute directory target, not a filesystem root")
    if any(_overlaps(text, p) for p in _SYSTEM_TARGETS) or _forbidden(text):
        _fail(field, "target overlaps a system, credential, engine, or report path")
    return text


def _docker_host(value, field):
    if not isinstance(value, str) or not value.startswith("unix:///"):
        _fail(field, "only an explicit local unix:/// Docker socket is supported")
    text = value[len("unix://"):]
    _path_text(text, field)
    if "?" in text or "#" in text or text == "/":
        _fail(field, "expected a local Unix socket path")
    return "unix://" + str(Path(text))


def normalize_profiles(raw_profiles, base_dir):
    """Validate portable declarations without provisioning, probing tools, or contacting Docker."""
    return _normalize_profiles(raw_profiles, base_dir, inspect_sources=True)


def _normalize_profiles(raw_profiles, base_dir, *, inspect_sources):
    if raw_profiles is None:
        _fail("environments", "expected a table")
    _table(raw_profiles, "environments", raw_profiles)
    base = _directory(Path(base_dir).absolute(), "environments base_dir", exists=True)
    out = {}
    for name, raw in raw_profiles.items():
        field = f"environments.{name}"
        if not _NAME.fullmatch(name):
            _fail(field, "profile name must contain 1-64 letters, digits, dots, underscores, or hyphens")
        conf = _table(raw, field, _PROFILE_KEYS)
        if conf.get("id", name) != name:
            _fail(field + ".id", "must match the named profile")
        backend = _choice(conf.get("backend"), {"local", "container"}, field + ".backend")
        container = backend == "container"
        item = {"id": name, "backend": backend, "_base_dir": str(base)}
        item["network"] = _choice(conf.get("network", "none" if container else "inherit"),
                                  {"none", "bridge"} if container else {"inherit"}, field + ".network")
        defaults = {"filesystem": "isolated", "root": "forbid", "package_install": "user"} if container else {
            "filesystem": "observed", "root": "inherit", "package_install": "unrestricted"}
        perm = _table(conf.get("permissions", {}), field + ".permissions", defaults)
        for key, expected in defaults.items():
            if perm.get(key, expected) != expected:
                _fail(field + ".permissions." + key,
                      f"{backend} supports only {expected!r}; the requested restriction cannot be enforced")
        item["permissions"] = defaults
        limits = _table(conf.get("limits", {}), field + ".limits", _LIMITS)
        if limits and not container:
            _fail(field + ".limits", "local cannot enforce independent resource or environment lifetime limits")
        item["limits"] = {}
        if container:
            for key, default in _LIMITS.items():
                low, high = {"cpus": (0.01, 1024), "memory_mb": (16, 1048576), "pids": (16, 65536),
                             "tmpfs_mb": (1, 1048576), "lifetime_seconds": (1, 86400)}[key]
                item["limits"][key] = _number(limits.get(key, default), field + ".limits." + key,
                                               low, high, integer=key != "cpus")
            if item["limits"]["tmpfs_mb"] > item["limits"]["memory_mb"]:
                _fail(field + ".limits.tmpfs_mb", "cannot exceed memory_mb")
            image = conf.get("image")
            if not isinstance(image, str) or not _IMAGE.fullmatch(image):
                _fail(field + ".image", "use a full sha256 image ID or repository@sha256:digest; "
                      "pull explicitly and pin it once for the matrix (mutable tags are rejected)")
            item["image"] = image
            if "platform" in conf:
                if not isinstance(conf["platform"], str) or not _PLATFORM.fullmatch(conf["platform"]):
                    _fail(field + ".platform", "expected linux/architecture[/variant]")
                item["platform"] = conf["platform"]
            item["docker_host"] = _docker_host(conf.get("docker_host", "unix:///var/run/docker.sock"),
                                                field + ".docker_host")
        else:
            for key in ("image", "platform", "docker_host"):
                if key in conf:
                    _fail(field + "." + key, "supported only by the container backend")
        required = _strings(conf.get("required", []), field + ".required")
        if len(set(required)) != len(required):
            _fail(field + ".required", "duplicate capability")
        for capability in required:
            if capability not in _REQUIRED:
                _fail(field + ".required", f"unsupported capability {capability!r}")
            if not container or (capability == "network_isolation" and item["network"] != "none"):
                _fail(field + ".required", f"{backend}/{item['network']} cannot enforce {capability}")
        item["required"] = required
        tools = conf.get("tools", [])
        if not isinstance(tools, list):
            _fail(field + ".tools", "expected an array of tool probe tables")
        item["tools"], seen = [], set()
        for i, tool in enumerate(tools):
            tf = f"{field}.tools[{i}]"
            _table(tool, tf, {"name", "argv", "required"})
            name_ = tool.get("name")
            if not isinstance(name_, str) or not _NAME.fullmatch(name_) or name_ in seen:
                _fail(tf + ".name", "expected a unique tool name")
            argv = _arguments(tool.get("argv"), tf + ".argv")
            if not isinstance(tool.get("required", True), bool):
                _fail(tf + ".required", "expected a boolean")
            seen.add(name_)
            item["tools"].append({"name": name_, "argv": argv, "required": tool.get("required", True)})
        mounts = conf.get("mounts", [])
        if not isinstance(mounts, list):
            _fail(field + ".mounts", "expected an array of directory mounts")
        if mounts and not container:
            _fail(field + ".mounts", "local does not implement directory mount boundaries")
        item["mounts"] = []
        for i, mount in enumerate(mounts):
            mf = f"{field}.mounts[{i}]"
            _table(mount, mf, {"source", "target", "access"})
            source = Path(_path_text(mount.get("source"), mf + ".source"))
            source = source if source.is_absolute() else base / source
            if inspect_sources:
                source = _directory(source, mf + ".source", exists=True)
            if source == base or not source.is_relative_to(base) or _forbidden(source.relative_to(base)):
                _fail(mf + ".source", "must be a dedicated fixture directory inside the trial directory; "
                      "host configuration, credentials, reports, and sibling paths cannot be mounted")
            target = _target(mount.get("target"), mf + ".target")
            access = _choice(mount.get("access"), {"read", "write"}, mf + ".access")
            for previous in item["mounts"]:
                if _overlaps(source, previous["source"]) or _overlaps(target, previous["target"]):
                    _fail(mf, "mount sources and targets must not overlap other declared mounts")
            if inspect_sources:
                _tree_manifest(source, mf + ".source")
            item["mounts"].append({"source": str(source), "target": target, "access": access})
        out[name] = item
    return out


def validate_for_cell(profile, cell):
    """Reject cell-level controls that bypass the explicit environment contract."""
    field = f"cells.{cell.get('id', '<unnamed>')}"
    if cell.get("runner"):
        _fail(field + ".runner", "cannot combine a legacy runner with an explicit environment")
    env = cell.get("env", {})
    if not isinstance(env, dict):
        _fail(field + ".env", "expected a table")
    for key in env:
        if key in _ISOLATION_ENV:
            _fail(field + ".env." + key, "controlled by the per-attempt environment")
    _checked_env(env, field + ".env")
    drops = cell.get("unset_env", [])
    if drops:
        _validate_unset(drops, field + ".unset_env")
        for key in _ISOLATION_ENV:
            if _dropped(key, drops):
                _fail(field + ".unset_env", f"cannot unset the isolated {key} path")


def _checked_env(overrides, field="environment.env"):
    if overrides is None:
        return {}
    if not isinstance(overrides, dict):
        _fail(field, "expected a mapping of explicitly supplied environment variables")
    result = {}
    for key, value in overrides.items():
        if not isinstance(key, str) or not _ENV_NAME.fullmatch(key):
            _fail(field, "invalid environment variable name")
        if key in _UNSAFE_ENV or key.startswith(_UNSAFE_PREFIXES):
            _fail(field + "." + key, "process-loader, shell-startup, agent socket, or Docker control override is unsupported")
        if not isinstance(value, str) or "\0" in value:
            _fail(field + "." + key, "expected a string without NUL bytes")
        result[key] = value
    return result


def _validate_unset(unset, field="environment.unset_env"):
    if not isinstance(unset, (list, tuple)) or any(
            not isinstance(v, str) or not _ENV_NAME.fullmatch(v[:-1] if v.endswith("*") else v) for v in unset):
        _fail(field, "expected variable names or trailing-star prefixes")


def _dropped(key, unset):
    return any(key.startswith(v[:-1]) if v.endswith("*") else key == v for v in unset)


def _redact_env_output(text, env):
    """Inventory is a durable record: mask known supplied values, never serialize its env."""
    replacements = []
    for key, value in env.items():
        if key in _ISOLATION_ENV or key in _BASE_ENV or not value:
            continue
        if len(value) >= 8 or re.search(r"TOKEN|SECRET|PASSWORD|CREDENTIAL|(?:^|_)KEY$", key, re.I):
            replacements.append((value, f"<redacted env:{key}>"))
    for value, replacement in sorted(replacements, key=lambda pair: len(pair[0]), reverse=True):
        text = text.replace(value, replacement)
    return text


def _path_environment(home, config, cache):
    """Path-only environment shared by isolated workers and separate host check contexts."""
    home, config, cache = Path(home), Path(config), Path(cache)
    local = home / ".local"
    return {
        "HOME": str(home), "XDG_CONFIG_HOME": str(config), "XDG_CACHE_HOME": str(cache),
        "XDG_DATA_HOME": str(local / "share"), "XDG_STATE_HOME": str(local / "state"),
        "TMPDIR": str(cache / "tmp"), "TMP": str(cache / "tmp"), "TEMP": str(cache / "tmp"),
        "PYTHONUSERBASE": str(local), "npm_config_cache": str(cache / "npm"),
        "npm_config_prefix": str(local), "CARGO_HOME": str(home / ".cargo"),
        "RUSTUP_HOME": str(home / ".rustup"), "GOPATH": str(home / "go"),
        "GOCACHE": str(cache / "go-build"), "PIP_CACHE_DIR": str(cache / "pip"),
        "UV_CACHE_DIR": str(cache / "uv"),
        "PATH": ":".join((str(local / "bin"), str(home / ".cargo" / "bin"),
                          str(home / "go" / "bin"), _PATH)),
    }


def _private_host_directories(run_dir, paths):
    """Create/check private directories relative to an open report root, without symlink traversal."""
    anchor = _open_directory(run_dir)
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
    try:
        for path in sorted(set(Path(p) for p in paths)):
            relative = path.relative_to(run_dir)
            current = os.dup(anchor)
            try:
                for component in relative.parts:
                    try:
                        os.mkdir(component, mode=0o700, dir_fd=current)
                    except FileExistsError:
                        pass
                    child = os.open(component, flags, dir_fd=current)
                    os.close(current)
                    current = child
                    if os.fstat(current).st_uid != os.getuid():
                        _fail("environment.host_env", "private check-context directories must be coordinator-owned")
                    os.fchmod(current, 0o700)
            finally:
                os.close(current)
    except OSError as exc:
        _fail("environment.host_env", "private check-context roots must be directories without symlinks "
              f"({type(exc).__name__})")
    finally:
        os.close(anchor)


def _tail_process(argv, cwd, timeout, env):
    """Process-group timeout plus bounded in-memory output (never shell=True)."""
    started = now()
    result = {"exit_code": None, "timed_out": False, "stdout": "", "stderr": "",
              "started_at": started, "stopped_at": None}
    if not isinstance(timeout, (int, float)) or isinstance(timeout, bool) or not math.isfinite(timeout) or timeout <= 0:
        _fail("environment.timeout", "expected a finite positive number of seconds")
    tails = [bytearray(), bytearray()]
    try:
        proc = subprocess.Popen(argv, cwd=str(cwd), env=env, stdin=subprocess.DEVNULL,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
    except OSError as exc:
        result.update(stderr=f"Cannot start environment command ({type(exc).__name__})", stopped_at=now())
        return result

    def read(stream, tail, limit):
        try:
            while chunk := stream.read(4096):
                tail.extend(chunk)
                del tail[:-limit]
        except (OSError, ValueError):
            pass

    readers = [threading.Thread(target=read, args=(proc.stdout, tails[0], 8000), daemon=True),
               threading.Thread(target=read, args=(proc.stderr, tails[1], 4000), daemon=True)]
    for thread in readers:
        thread.start()
    try:
        result["exit_code"] = proc.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        result["timed_out"] = True
    finally:
        # Even after a normal shell exit, do not leave local shell descendants.
        # This kills the Docker client only; the runner separately aborts its container.
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except (ProcessLookupError, PermissionError):
            pass
        proc.wait()
        for thread in readers:
            thread.join(1)
        for stream in (proc.stdout, proc.stderr):
            if not any(t.is_alive() for t in readers):
                stream.close()
    result.update(stdout=bytes(tails[0]).decode(errors="replace"),
                  stderr=bytes(tails[1]).decode(errors="replace"), stopped_at=now())
    return result


class EnvironmentRunner(Runner):
    """A per-attempt runner; normalized profiles may be shared, runner instances may not."""

    plugin_name = "environment"
    local_transcripts = True

    def __init__(self, profile):
        raw = copy.deepcopy(profile)
        if not isinstance(raw, dict):
            _fail("environment", "expected a normalized profile")
        normalized = "_base_dir" in raw
        base = raw.pop("_base_dir", Path.cwd())
        name = raw.get("id", "environment")
        # Recovery/cleanup must not depend on the continued existence of fixture sources.
        # Fresh plans still recheck/hash every source before any provisioning.
        self.profile = _normalize_profiles({name: raw}, base, inspect_sources=not normalized)[name]
        super().__init__(self.profile)
        self.backend = self.profile["backend"]
        self._fresh_owner = None
        self._created_owner = None
        self._prepared = None
        self._preparing = False
        self._preparation_failed = False
        self._released = False
        self._docker_binary = None
        # PATH is runtime selection, not credential inheritance. Capture before worker execution.
        self._coordinator_path = os.environ.get("PATH", _MANAGER_PATH)

    @property
    def name(self):
        return self.backend

    def describe(self):
        return {k: copy.deepcopy(v) for k, v in self.profile.items() if not k.startswith("_")}

    def child_env(self, overrides=None, unset=()):
        """A full process environment. Never inherit os.environ, proxies, or host credentials."""
        _validate_unset(unset)
        extra = _checked_env(overrides)
        for key in _ISOLATION_ENV & set(extra):
            if _dropped(key, unset):
                _fail("environment.unset_env", f"cannot unset the isolated {key} path")
        return {k: v for k, v in {**_BASE_ENV, **extra}.items() if not _dropped(k, unset)}

    def environment_env(self, ctx):
        paths = self._paths(ctx)
        return _path_environment(paths["home_dir"], paths["config_dir"], paths["cache_dir"])

    def _worker_roots(self, ctx):
        """Include current, archived, original, and explicitly writable mount paths."""
        state = ctx.get("state") or {}
        handle = state.get("environment") or {}
        paths = [ctx.get(key) for key in _ROOTS]
        for mapping in (state.get("paths"), state.get("archived"), handle.get("paths")):
            paths += [(mapping or {}).get(key) for key in _ROOTS]
        paths += list(state.get("owned_paths") or [])
        paths += [m.get("host_source") for m in handle.get("mounts", []) if m.get("access") == "write"]
        roots = set()
        for value in paths:
            if not isinstance(value, (str, os.PathLike)) or not value:
                continue
            path = Path(value)
            if not path.is_absolute():
                _fail("environment.host_env", "worker ownership paths must be absolute")
            roots.add(Path(os.path.normpath(path)))
            try:
                roots.add(path.resolve())
            except (OSError, RuntimeError):
                _fail("environment.host_env", "cannot resolve a worker ownership path safely")
        return roots

    @staticmethod
    def _inside_worker(path, roots):
        lexical = Path(os.path.normpath(path))
        try:
            resolved = path.resolve()
        except (OSError, RuntimeError):
            return True
        return any(candidate == root or candidate.is_relative_to(root)
                   for candidate in (lexical, resolved) for root in roots)

    def host_env(self, ctx, overrides=None, unset=()):
        """Private host-check environment; never use worker HOME/PATH for trusted verification.

        This creates only owned empty 0700 directories under run_dir. Explicit
        auth/task values are retained, but startup/configuration controls are
        rebuilt. No caller credential, proxy, shell-startup, or user config is
        inherited. The returned dict is for a subprocess, not a durable record.
        """
        roots = self._worker_roots(ctx)
        run_dir = _directory(ctx.get("run_dir"), "environment.host_env.run_dir", exists=True)
        private = run_dir / "host-environment"
        if any(_overlaps(private, root) for root in roots):
            _fail("environment.host_env", "private host context overlaps a worker-owned directory")
        home, config, cache = private / "home", private / "config", private / "cache"
        fixed = _path_environment(home, config, cache)
        fixed.update({key: str(cache / directory) for key, directory in {
            "YARN_CACHE_FOLDER": "yarn", "PNPM_STORE_DIR": "pnpm", "POETRY_CACHE_DIR": "poetry",
            "GOMODCACHE": "go-mod", "BUN_INSTALL_CACHE_DIR": "bun", "DENO_DIR": "deno",
            "NUGET_PACKAGES": "nuget", "GRADLE_USER_HOME": "gradle",
        }.items()})
        directories = [private, config, cache, *[Path(v) for k, v in fixed.items() if k != "PATH"]]
        path_entries = []
        for raw in self._coordinator_path.split(os.pathsep):
            if not raw or any(c in raw for c in "\0\n\r") or not Path(raw).is_absolute():
                continue
            path = Path(raw)
            if self._inside_worker(path, roots):
                continue
            resolved = str(path.resolve())
            if resolved not in path_entries:
                path_entries.append(resolved)
        if not path_entries:
            _fail("environment.host_env.PATH", "coordinator PATH has no absolute runtime directory outside worker-owned paths")
        fixed.update(PATH=os.pathsep.join(path_entries), SHELL="/bin/sh", ZDOTDIR=str(home),
                     XDG_CONFIG_DIRS=str(config), XDG_DATA_DIRS="/usr/local/share:/usr/share",
                     XDG_RUNTIME_DIR=str(cache / "runtime"), CODEX_HOME=str(config / "codex"),
                     CLAUDE_CONFIG_DIR=str(config / "claude"), KIRO_HOME=str(config / "kiro"),
                     DOCKER_CONFIG=str(config / "docker"),
                     PYTHONSAFEPATH="1", PYTHONNOUSERSITE="1", PYTHONSTARTUP="/dev/null",
                     PYTHONPYCACHEPREFIX=str(cache / "pycache"),
                     GIT_CONFIG_NOSYSTEM="1", GEM_HOME=str(home / ".local" / "gems"),
                     GEM_PATH=str(home / ".local" / "gems"),
                     BUNDLE_USER_HOME=str(config / "bundle"), BUNDLE_APP_CONFIG=str(config / "bundle"))
        files = {"GIT_CONFIG_GLOBAL": config / "git" / "config", "PIP_CONFIG_FILE": config / "pip" / "pip.conf",
                 "npm_config_userconfig": config / "npm" / "npmrc",
                 "HISTFILE": home / ".local" / "state" / "shell_history", "INPUTRC": config / "readline" / "inputrc"}
        fixed.update({key: str(value) for key, value in files.items()})
        for lower in ("npm_config_cache", "npm_config_prefix", "npm_config_userconfig"):
            fixed[lower.upper()] = fixed[lower]
        directories += [Path(value).parent for value in files.values()]
        directories += [Path(fixed[key]) for key in (
            "XDG_RUNTIME_DIR", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "KIRO_HOME", "DOCKER_CONFIG",
            "PYTHONPYCACHEPREFIX", "GEM_HOME", "BUNDLE_USER_HOME")]
        raw = {}
        for source in (ctx.get("env"), ctx.get("check_env"), overrides):
            if source is not None:
                if not isinstance(source, dict):
                    _fail("environment.host_env", "expected mappings of declared auth/task variables")
                raw.update(source)
        supplied = _checked_env({
            key: value for key, value in raw.items()
            if isinstance(key, str) and key not in fixed and key not in _HOST_DROP
            and not key.startswith(_HOST_DROP_PREFIXES)
        }, "environment.host_env")
        if any(not isinstance(key, str) or not _ENV_NAME.fullmatch(key) for key in raw):
            _fail("environment.host_env", "invalid environment variable name")
        for key in _HOST_EXTERNAL_FILES & set(supplied):
            value = supplied[key]
            if not value:
                continue
            candidates = value.split(os.pathsep) if key in ("KUBECONFIG", "BOTO_PATH") else [value]
            if any(not Path(path).is_absolute() or self._inside_worker(Path(path), roots) for path in candidates):
                _fail("environment.host_env." + key,
                      "host checks cannot load configuration/credentials from relative or worker-owned paths; "
                      "supply a coordinator-side path or explicit credential values")
        drop = tuple(ctx.get("unset_env") or ()) + tuple(unset)
        _validate_unset(drop, "environment.host_env.unset")
        for key in fixed:
            if _dropped(key, drop):
                _fail("environment.host_env.unset", f"cannot unset the private host control {key}")
        _private_host_directories(run_dir, directories)
        return {key: value for key, value in {**_BASE_ENV, **supplied, **fixed}.items() if not _dropped(key, drop)}

    def _worker_env(self, ctx, env=None):
        isolated = self.environment_env(ctx)
        extra = _checked_env({**(ctx.get("env") or {}), **(env or {})})
        for key in _ISOLATION_ENV & set(extra):
            if extra[key] != isolated[key]:
                _fail("environment.env." + key, "cannot override the per-attempt path")
        return self.child_env({**isolated, **extra}, tuple(ctx.get("unset_env") or ()))

    def _paths(self, ctx):
        paths = {key: str(_directory(ctx.get(key), "environment.ctx." + key)) for key in _ROOTS}
        report = _directory(ctx.get("run_dir"), "environment.ctx.run_dir")
        for i, key in enumerate(_ROOTS):
            path = Path(paths[key])
            home = Path.home().resolve()
            if (_overlaps(path, report) or path == home or home.is_relative_to(path)
                    or Path(self.profile["_base_dir"]).is_relative_to(path)
                    or any(_overlaps(path, p) for p in _SYSTEM_TARGETS)):
                _fail("environment.ctx." + key, "must be a private root disjoint from reports, system paths, and the host home")
            for other in _ROOTS[:i]:
                if _overlaps(path, paths[other]):
                    _fail("environment.ctx." + key, f"overlaps {other}; use separate per-attempt roots")
        return paths

    def _profile_hash(self):
        return hashlib.sha256(json.dumps(self.profile, sort_keys=True, separators=(",", ":")).encode()).hexdigest()

    def _docker_prefix(self, ctx=None, handle=None):
        if self._docker_binary is None:
            binary = shutil.which("docker", path=_MANAGER_PATH)
            if binary is None:
                raise EnvironmentError("Docker CLI not found on the fixed management PATH; install Docker explicitly")
            self._docker_binary = str(Path(binary).resolve())
        config = handle.get("docker_config_dir") if handle else None
        if config is None:
            # Deliberately absent: read-only Docker operations must not load ~/.docker.
            config = str(Path(tempfile.gettempdir()) / ("ajx-docker-empty-" + uuid.uuid4().hex))
        host = handle.get("docker_host") if handle else self.profile["docker_host"]
        _docker_host(host, "environment.docker_host")
        return [self._docker_binary, "--config", config, "--host", host]

    def _docker(self, args, *, ctx=None, handle=None, timeout=30):
        """Small fakeable command interface; management never receives worker/host secrets."""
        env = {**_BASE_ENV, "PATH": _MANAGER_PATH, "HOME": "/nonexistent"}
        try:
            proc = subprocess.run(self._docker_prefix(ctx, handle) + list(args), capture_output=True,
                                  text=True, errors="replace", env=env, stdin=subprocess.DEVNULL, timeout=timeout)
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise EnvironmentError(f"Docker {args[0]} unavailable ({type(exc).__name__}); "
                                   "check the local daemon; ownership/cleanup is unconfirmed") from None
        return {"code": proc.returncode, "stdout": proc.stdout, "stderr": proc.stderr}

    def _docker_json(self, args, *, ctx=None, handle=None):
        result = self._docker(args, ctx=ctx, handle=handle)
        if result["code"]:
            raise EnvironmentError(f"Docker {' '.join(args[:2])} failed; check the daemon and preloaded pinned image")
        try:
            return json.loads(result["stdout"])
        except (ValueError, TypeError):
            raise EnvironmentError("Docker returned invalid inspection data") from None

    def _inspect_image(self, ctx=None, handle=None):
        rows = self._docker_json(["image", "inspect", self.profile["image"]], ctx=ctx, handle=handle)
        if not isinstance(rows, list) or len(rows) != 1 or not isinstance(rows[0], dict):
            raise EnvironmentError("Docker image inspect did not return exactly one image")
        image = rows[0]
        image_id = image.get("Id", "")
        if not _IMAGE_ID.fullmatch(image_id) or image.get("Os") != "linux":
            raise EnvironmentError("Environment requires a resolved Linux sha256 image")
        if _IMAGE_ID.fullmatch(self.profile["image"]) and image_id != self.profile["image"]:
            raise EnvironmentError("Resolved image ID differs from the pinned profile")
        config = image.get("Config") or {}
        if config.get("Volumes"):
            raise EnvironmentError("environments." + self.profile["id"] +
                                   ".image: images with declared VOLUMEs are unsupported (implicit writable mounts)")
        image_platform = "/".join(str(image[k]) for k in ("Os", "Architecture", "Variant") if image.get(k))
        requested = self.profile.get("platform")
        if requested and requested != image_platform:
            raise EnvironmentError("Pinned image platform does not match environments." + self.profile["id"] + ".platform")
        names = []
        for entry in config.get("Env") or []:
            if not isinstance(entry, str) or "=" not in entry or not _ENV_NAME.fullmatch(entry.split("=", 1)[0]):
                raise EnvironmentError("Image contains an unsupported environment-variable name")
            names.append(entry.split("=", 1)[0])
        return {"requested": self.profile["image"], "id": image_id, "platform": image_platform,
                "repo_digests": list(image.get("RepoDigests") or []),
                "environment_names": sorted(set(names)), "environment_values_inherited": False}

    def _daemon(self, ctx=None, handle=None):
        info = self._docker_json(["info", "--format", "{{json .}}"], ctx=ctx, handle=handle)
        if not isinstance(info, dict) or info.get("OSType") != "linux":
            raise EnvironmentError("A local Linux Docker daemon is required")
        supported = {k: info.get(k) is True for k in ("MemoryLimit", "SwapLimit", "CpuCfsQuota", "PidsLimit")}
        if not all(supported.values()):
            missing = ", ".join(k for k, enabled in supported.items() if not enabled)
            raise EnvironmentError("Docker cannot enforce the declared resource limits: " + missing)
        security = info.get("SecurityOptions") or []
        if not any("seccomp" in str(v) and "unconfined" not in str(v) for v in security):
            raise EnvironmentError("Docker must provide its seccomp syscall boundary; unconfined daemons are unsupported")
        return {"os": info["OSType"], "architecture": info.get("Architecture"),
                "server_version": info.get("ServerVersion"), "resource_support": supported,
                "security_options": security}

    def doctor(self, ctx=None):
        """Read-only checks: no roots, containers, pulls, or inventory commands are created/run."""
        report = {"id": self.profile["id"], "backend": self.backend, "status": "available",
                  "capabilities": self._capabilities(confirmed=False), "limitations": self._limitations()}
        if self.backend == "local":
            report["platform"] = self._local_platform()
        else:
            try:
                report["image"] = self._inspect_image(ctx)
                report["daemon"] = self._daemon(ctx)
            except EnvironmentError as exc:
                report.update(status="unavailable", error=str(exc))
        return report

    def plan(self, ctx):
        state = ctx.get("state") or {}
        existing = state.get("environment")
        if "environment" in state:
            self._handle(ctx)
            return copy.deepcopy(existing)
        paths = self._paths(ctx)
        for key in ("home_dir", "config_dir", "cache_dir"):
            path = Path(paths[key])
            if path.exists() and any(path.iterdir()) and not self._caller_owned(ctx, path):
                _fail("environment.ctx." + key, "must start empty; live host configuration and warm caches are not inherited")
            if path.exists():
                _tree_manifest(path, "environment.ctx." + key, fixtures=False)
        workspace = Path(paths["workspace"])
        if workspace.exists():
            _tree_manifest(workspace, "environment.ctx.workspace", fixtures=False)
        owner = uuid.uuid4().hex
        plan = {"version": 1, "id": self.profile["id"], "backend": self.backend, "owner": owner,
                "profile_sha256": self._profile_hash(), "paths": paths, "created_at": now(),
                "env_names": sorted(self._worker_env(ctx)), "mounts": []}
        for i, mount in enumerate(self.profile["mounts"]):
            if any(_overlaps(mount["source"], p) for p in [*paths.values(), str(ctx["run_dir"])]):
                _fail(f"environments.{self.profile['id']}.mounts[{i}].source", "overlaps attempt or reporting directories")
            if any(_overlaps(mount["target"], p) for p in [*paths.values(), str(ctx["run_dir"])]):
                _fail(f"environments.{self.profile['id']}.mounts[{i}].target", "overlaps an attempt or report root")
            manifest = _tree_manifest(mount["source"], f"environment.mounts[{i}].source")
            projected = (str(workspace / ".ajx-mounts" / str(i)) if mount["access"] == "write" else mount["source"])
            plan["mounts"].append({**mount, **manifest, "host_source": projected,
                                   "write_disposition": "attempt-copy" if mount["access"] == "write" else "read-only"})
        if self.backend == "container":
            plan.update(name="ajx-" + owner, docker_host=self.profile["docker_host"],
                        docker_config_dir=str(Path(ctx["run_dir"]).resolve() / ("environment-docker-" + owner)),
                        expires_at=time.time() + self.profile["limits"]["lifetime_seconds"],
                        worker_user=f"{os.getuid() or 1000}:{os.getgid() or 1000}")
            plan["image"] = self._inspect_image(ctx, plan)
            plan["daemon"] = self._daemon(ctx, plan)
            plan["labels"] = {_LABEL_PREFIX + "owner": owner, _LABEL_PREFIX + "profile": plan["profile_sha256"],
                              _LABEL_PREFIX + "version": "1", _LABEL_PREFIX + "expires": str(plan["expires_at"])}
        self._fresh_owner = owner
        return copy.deepcopy(plan)

    def _handle(self, ctx, *, cleanup=False):
        handle = (ctx.get("state") or {}).get("environment")
        if (not isinstance(handle, dict) or type(handle.get("version")) is not int or handle.get("version") != 1
                or not _OWNER.fullmatch(str(handle.get("owner", "")))
                or not _CONTAINER_ID.fullmatch(str(handle.get("profile_sha256", "")))):
            raise EnvironmentError("Persist the environment plan in state['environment'] before preparation or execution")
        if handle.get("backend") != self.backend:
            raise EnvironmentError("Persisted environment backend differs from the requested backend")
        if not cleanup and (handle.get("profile_sha256") != self._profile_hash() or handle.get("paths") != self._paths(ctx)):
            raise EnvironmentError("Environment profile/paths changed; do not reuse or rerun the attempt")
        if self.backend == "container":
            if handle.get("name") != "ajx-" + handle["owner"]:
                raise EnvironmentError("Invalid persisted container ownership name")
            expected = {_LABEL_PREFIX + "owner": handle["owner"], _LABEL_PREFIX + "profile": handle.get("profile_sha256"),
                        _LABEL_PREFIX + "version": "1", _LABEL_PREFIX + "expires": str(handle.get("expires_at"))}
            if handle.get("labels") != expected or not _IMAGE_ID.fullmatch((handle.get("image") or {}).get("id", "")):
                raise EnvironmentError("Invalid persisted container ownership/image handle")
            if (not isinstance(handle.get("expires_at"), (int, float))
                    or not math.isfinite(handle["expires_at"])):
                raise EnvironmentError("Invalid persisted container expiry")
            _docker_host(handle.get("docker_host"), "environment.state.docker_host")
            config = Path(handle.get("docker_config_dir", ""))
            if config != Path(ctx["run_dir"]).resolve() / ("environment-docker-" + handle["owner"]):
                raise EnvironmentError("Invalid persisted Docker configuration path")
            user = handle.get("worker_user", "")
            if not re.fullmatch(r"[1-9][0-9]*:[1-9][0-9]*", user):
                raise EnvironmentError("Invalid persisted non-root worker identity")
            if not cleanup:
                declared = [(m["source"], m["target"], m["access"]) for m in self.profile["mounts"]]
                recorded = [(m.get("source"), m.get("target"), m.get("access")) for m in handle.get("mounts", [])]
                if recorded != declared:
                    raise EnvironmentError("Persisted mounts differ from the declared profile")
                for i, mount in enumerate(handle["mounts"]):
                    expected_source = (str(Path(handle["paths"]["workspace"]) / ".ajx-mounts" / str(i))
                                       if mount["access"] == "write" else mount["source"])
                    if mount.get("host_source") != expected_source:
                        raise EnvironmentError("Invalid persisted mount source")
        return handle

    def _find(self, handle, ctx, *, identifier=None):
        ident = identifier or handle["name"]
        result = self._docker(["container", "inspect", ident], ctx=ctx, handle=handle)
        if result["code"]:
            # Do not confuse daemon/auth/transport failure with "already removed".
            by_id = bool(_CONTAINER_ID.fullmatch(ident))
            listing = self._docker(["container", "ls", "--all", "--no-trunc",
                                    "--filter", "id=" + ident if by_id else
                                    "label=" + _LABEL_PREFIX + "owner=" + handle["owner"],
                                    "--format", "{{.ID}}"], ctx=ctx, handle=handle)
            if listing["code"] == 0 and not listing["stdout"].strip():
                return None
            if listing["code"] == 0 and not by_id:
                candidates = listing["stdout"].split()
                if len(candidates) == 1 and _CONTAINER_ID.fullmatch(candidates[0]):
                    # A host administrator may rename a container. Labels remain our durable handle.
                    return self._find(handle, ctx, identifier=candidates[0])
            raise EnvironmentError("Cannot inspect container; its presence and ownership are unknown")
        try:
            rows = json.loads(result["stdout"])
            data = rows[0] if isinstance(rows, list) and len(rows) == 1 else None
            if not isinstance(data, dict) or not _CONTAINER_ID.fullmatch(data.get("Id", "")):
                raise ValueError
        except (ValueError, TypeError):
            raise EnvironmentError("Docker returned invalid container inspection data") from None
        labels = (data.get("Config") or {}).get("Labels") or {}
        if not isinstance(labels, dict) or any(labels.get(k) != v for k, v in handle["labels"].items()):
            raise EnvironmentError("Container ownership labels do not match; refusing to execute or remove it")
        return data

    def _expected_mounts(self, handle):
        mounts = [{"source": path, "target": path, "access": "write"} for path in handle["paths"].values()]
        mounts.extend({"source": m["host_source"], "target": m["target"], "access": m["access"]}
                      for m in handle["mounts"])
        return mounts

    @staticmethod
    def _caller_owned(ctx, path):
        """The coordinator may stage declared auth/extensions in its durably allocated fresh roots."""
        state = ctx.get("state") or {}
        paths = state.get("paths") or {}
        return str(path) in state.get("owned_paths", []) and str(path) in paths.values()

    def _marker_path(self, ctx, handle):
        return Path(ctx["run_dir"]) / ("environment-" + handle["owner"] + ".json")

    def _read_marker(self, ctx, handle):
        marker = self._marker_path(ctx, handle)
        if marker.is_symlink():
            raise EnvironmentError("Environment lifecycle marker must not be a symlink")
        try:
            data = json.loads(marker.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            raise EnvironmentError("Environment preparation was not confirmed; recovery will not recreate it") from None
        if (not isinstance(data, dict) or data.get("owner") != handle["owner"]
                or data.get("profile_sha256") != handle["profile_sha256"]):
            raise EnvironmentError("Environment lifecycle marker ownership differs from the plan")
        return data

    def _write_marker(self, ctx, handle, status, *, initial=False):
        marker = self._marker_path(ctx, handle)
        data = {"owner": handle["owner"], "profile_sha256": handle["profile_sha256"], "status": status}
        if status == "prepared":
            data["path_identity"] = {key: [Path(path).stat().st_dev, Path(path).stat().st_ino]
                                     for key, path in handle["paths"].items()}
        if initial:
            try:
                with marker.open("x", encoding="utf-8") as stream:
                    json.dump(data, stream)
                    stream.flush()
                    os.fsync(stream.fileno())
                marker.chmod(0o600)
            except FileExistsError:
                raise EnvironmentError("Environment preparation was already attempted; it cannot be replayed") from None
        else:
            self._read_marker(ctx, handle)
            temporary = marker.with_suffix(".tmp")
            with temporary.open("x", encoding="utf-8") as stream:
                json.dump(data, stream)
                stream.flush()
                os.fsync(stream.fileno())
            temporary.chmod(0o600)
            temporary.replace(marker)

    def _confirmed_preparation(self, ctx, handle):
        marker = self._read_marker(ctx, handle)
        if marker.get("status") != "prepared":
            raise EnvironmentError("Environment preparation was not confirmed or has been released; recovery will not rerun it")
        for key, path in handle["paths"].items():
            try:
                info = Path(path).stat()
            except OSError:
                raise EnvironmentError("Prepared attempt directory is missing; recovery will not recreate it") from None
            if marker.get("path_identity", {}).get(key) != [info.st_dev, info.st_ino]:
                raise EnvironmentError("Prepared attempt directory identity changed; refusing to reuse it")

    def _check_container(self, data, handle):
        config, host = data.get("Config") or {}, data.get("HostConfig") or {}
        limits = self.profile["limits"]
        checks = {
            "image": data.get("Image") == handle["image"]["id"],
            "supervisor": config.get("User") == "0:0" and config.get("Entrypoint") == ["/usr/bin/env"]
                          and (config.get("Cmd") or [])[:3] == ["-i", "PATH=" + _PATH, "/bin/sleep"],
            "read_only": host.get("ReadonlyRootfs") is True,
            "unprivileged": host.get("Privileged") is False and not host.get("CapAdd") and
                            any(str(v).lower() == "all" for v in host.get("CapDrop") or []),
            "no_new_privileges": any(str(v) in ("no-new-privileges", "no-new-privileges=true")
                                     for v in host.get("SecurityOpt") or []),
            "lifetime": host.get("AutoRemove") is True and host.get("Init") is True
                        and (host.get("RestartPolicy") or {}).get("Name", "no") in ("", "no"),
            "network": host.get("NetworkMode") == self.profile["network"]
                       and not host.get("PortBindings") and not host.get("PublishAllPorts"),
            "namespaces": host.get("PidMode", "") == "" and host.get("IpcMode") == "none"
                          and host.get("CgroupnsMode") == "private",
            "resources": host.get("Memory") == limits["memory_mb"] * 1024 * 1024
                         and host.get("MemorySwap") == limits["memory_mb"] * 1024 * 1024
                         and host.get("PidsLimit") == limits["pids"]
                         and host.get("NanoCpus") == round(limits["cpus"] * 1000000000),
            "no_extra_host_access": not any(host.get(k) for k in ("Binds", "VolumesFrom", "Devices", "DeviceRequests",
                                                                 "ExtraHosts", "Links", "GroupAdd")),
        }
        cmd = config.get("Cmd") or []
        checks["supervisor"] = checks["supervisor"] and len(cmd) == 4 and str(cmd[3]).isdigit() and 0 < int(cmd[3]) <= limits["lifetime_seconds"]
        wanted = {(m["source"], m["target"], m["access"] == "write") for m in self._expected_mounts(handle)}
        observed = {(m.get("Source"), m.get("Destination"), m.get("RW")) for m in data.get("Mounts") or []
                    if m.get("Type") == "bind"}
        checks["mounts"] = wanted == observed and all(
            m.get("Type") == "bind" or (m.get("Type") == "tmpfs" and m.get("Destination") == "/tmp")
            for m in data.get("Mounts") or [])
        checks["tmpfs"] = host.get("Tmpfs") == {
            "/tmp": f"rw,nosuid,nodev,noexec,size={limits['tmpfs_mb']}m,mode=1777"}
        configured = host.get("Mounts") or []
        checks["nonrecursive_mounts"] = len(configured) == len(wanted) and all(
            m.get("Type") == "bind" and (m.get("BindOptions") or {}).get("NonRecursive") is True
            and (m.get("BindOptions") or {}).get("Propagation") == "rprivate" for m in configured)
        failed = [key for key, passed in checks.items() if not passed]
        if failed:
            raise EnvironmentError("Container configuration differs from the owned plan: " + ", ".join(failed))
        state = data.get("State") or {}
        if not state.get("Running") or state.get("Paused") or state.get("Restarting"):
            raise EnvironmentError("Owned environment is not running; it will not be restarted or recreated")

    def _ensure_roots(self, ctx, handle):
        for key, value in handle["paths"].items():
            path = _directory(value, "environment.ctx." + key)
            if key != "workspace" and path.exists() and any(path.iterdir()) and not self._caller_owned(ctx, path):
                raise EnvironmentError(f"Fresh environment {key} is not empty; refusing configuration/cache reuse")
            path.mkdir(parents=True, exist_ok=True, mode=0o700)
            path.chmod(0o700)
        for key, path in handle["paths"].items():
            _tree_manifest(path, "environment.ctx." + key, fixtures=False)
        for mount in handle["mounts"]:
            observed = _tree_manifest(mount["source"], "environment.mount.source")
            if observed["sha256"] != mount["sha256"]:
                raise EnvironmentError("Declared mount source changed after planning; replan a fresh attempt")
            if mount["access"] == "write":
                dest = Path(mount["host_source"])
                if dest.exists() or dest.is_symlink():
                    raise EnvironmentError("Writable mount snapshot already exists; refusing attempt reuse")
                snapshot = _tree_manifest(mount["source"], "environment.mount.snapshot", destination=dest)
                if snapshot["sha256"] != mount["sha256"]:
                    raise EnvironmentError("Mount source changed during snapshot copy")
        # No host configuration is copied. Create only known empty install/cache directories.
        for value in self.environment_env(ctx).values():
            if value.startswith(handle["paths"]["home_dir"] + "/") or value.startswith(handle["paths"]["cache_dir"] + "/"):
                if ":" not in value:
                    Path(value).mkdir(parents=True, exist_ok=True, mode=0o700)
        (Path(handle["paths"]["home_dir"]) / ".local" / "bin").mkdir(parents=True, exist_ok=True, mode=0o700)
        if self.backend == "container" and os.getuid() == 0:
            uid, gid = (int(v) for v in handle["worker_user"].split(":"))
            for root in handle["paths"].values():
                for current, dirs, files in os.walk(root, followlinks=False):
                    os.chown(current, uid, gid)
                    for name in dirs + files:
                        path = Path(current) / name
                        if path.is_symlink():
                            raise EnvironmentError("Starting tree changed to a symlink before preparation")
                        os.chown(path, uid, gid, follow_symlinks=False)
        if self.backend == "container":
            config = Path(handle["docker_config_dir"])
            config.mkdir(mode=0o700, exist_ok=False)
            (config / "config.json").write_text("{}\n", encoding="utf-8")

    def _create_argv(self, handle):
        left = min(self.profile["limits"]["lifetime_seconds"], int(handle["expires_at"] - time.time()))
        if left <= 0:
            raise EnvironmentError("Environment plan has expired; no worker will be started")
        limits = self.profile["limits"]
        args = ["run", "--detach", "--rm", "--pull", "never", "--name", handle["name"],
                "--init", "--user", "0:0", "--read-only", "--cap-drop", "ALL",
                "--security-opt", "no-new-privileges=true", "--network", self.profile["network"],
                "--ipc", "none", "--cgroupns", "private", "--no-healthcheck", "--log-driver", "none",
                "--cpus", str(limits["cpus"]), "--memory", str(limits["memory_mb"]) + "m",
                "--memory-swap", str(limits["memory_mb"]) + "m", "--pids-limit", str(limits["pids"]),
                "--ulimit", "core=0:0", "--stop-timeout", "1",
                "--tmpfs", f"/tmp:rw,nosuid,nodev,noexec,size={limits['tmpfs_mb']}m,mode=1777",
                "--workdir", handle["paths"]["workspace"], "--entrypoint", "/usr/bin/env"]
        if self.profile.get("platform"):
            args.extend(["--platform", self.profile["platform"]])
        for key, value in sorted(handle["labels"].items()):
            args.extend(["--label", key + "=" + value])
        # Clear all image ENV, including loader/startup injection, before any entrypoint starts.
        for key in sorted(set(handle["image"]["environment_names"]) | {"HOME", "PATH", "ENV", "BASH_ENV"}):
            args.extend(["--env", key + "="])
        for mount in self._expected_mounts(handle):
            value = f"type=bind,source={mount['source']},target={mount['target']},bind-propagation=rprivate,bind-recursive=disabled"
            if mount["access"] == "read":
                value += ",readonly"
            args.extend(["--mount", value])
        args.extend([handle["image"]["id"], "-i", "PATH=" + _PATH, "/bin/sleep", str(left)])
        return args

    def prepare(self, ctx):
        handle = self._handle(ctx)
        if self._released:
            raise EnvironmentError("Environment was released; create a new attempt")
        if self._preparation_failed:
            raise EnvironmentError("Environment preparation previously failed; create a new attempt")
        if self._prepared is not None:
            if self.backend == "container":
                self._running(ctx, handle)
            return copy.deepcopy(self._prepared)
        fresh = self._fresh_owner == handle["owner"]
        try:
            if self.backend == "container":
                if time.time() >= handle["expires_at"]:
                    cleanup = self.release(ctx)
                    raise EnvironmentError("Environment expired; it will not be recreated; cleanup " + cleanup["status"])
                existing = self._find(handle, ctx)
                if existing is None:
                    if not fresh:
                        raise EnvironmentError("Owned environment is missing; recovery never creates or reruns an attempt")
                    # Consume before any mutations: retrying failed preparation cannot create twice.
                    self._fresh_owner = None
                    self._write_marker(ctx, handle, "preparing", initial=True)
                    self._ensure_roots(ctx, handle)
                    self._created_owner = handle["owner"]
                    created = self._docker(self._create_argv(handle), ctx=ctx, handle=handle,
                                           timeout=min(30, max(1, handle["expires_at"] - time.time())))
                    if created["code"]:
                        raise EnvironmentError("Docker creation failed; no fallback or task retry was attempted")
                    existing = self._find(handle, ctx)
                    if existing is None:
                        raise EnvironmentError("Container disappeared during preparation (expiry or startup failure)")
                else:
                    fresh = False
                    self._fresh_owner = None
                    self._confirmed_preparation(ctx, handle)
                self._check_container(existing, handle)
            elif fresh:
                self._fresh_owner = None
                self._write_marker(ctx, handle, "preparing", initial=True)
                self._ensure_roots(ctx, handle)
            elif any(not Path(p).is_dir() for p in handle["paths"].values()):
                raise EnvironmentError("Local attempt directories are missing; recovery will not recreate them")
            else:
                self._confirmed_preparation(ctx, handle)
            self._preparing = True
            report = self._report(ctx, handle, fresh=fresh)
            if fresh:
                self._write_marker(ctx, handle, "prepared")
            self._prepared = copy.deepcopy(report)
            self._preparing = False
            return report
        except BaseException as exc:
            self._preparing = False
            self._preparation_failed = True
            # Only clean our own attempted create. A collision must never remove someone else's resource.
            if self.backend == "container" and self._created_owner == handle["owner"]:
                cleanup = self.release(ctx)
                if isinstance(exc, (EnvironmentError, ValueError)):
                    exc.add_note("Environment preparation cleanup: " + json.dumps(cleanup, sort_keys=True))
            raise

    def _running(self, ctx, handle):
        if self._released:
            raise EnvironmentError("Environment was released; the attempt cannot run again")
        if time.time() >= handle["expires_at"]:
            cleanup = self.release(ctx)
            raise EnvironmentError("Environment lifetime expired; cleanup " + cleanup["status"])
        data = self._find(handle, ctx)
        if data is None:
            raise EnvironmentError("Environment is missing or expired; it will not be recreated")
        self._check_container(data, handle)
        if not self._preparing:
            self._confirmed_preparation(ctx, handle)
        return data

    def wrap(self, argv, ctx, env):
        argv = _arguments(argv, "environment.argv")
        handle = self._handle(ctx)
        if self._released:
            raise EnvironmentError("Environment was released")
        full_env = self._worker_env(ctx, env)
        if self.backend == "local":
            if not self._preparing:
                self._confirmed_preparation(ctx, handle)
            return argv
        data = self._running(ctx, handle)
        args = self._docker_prefix(ctx, handle) + ["exec", "--interactive", "--user", handle["worker_user"],
                                                  "--workdir", handle["paths"]["workspace"]]
        for key in sorted(full_env):
            # Docker reads values from the CLI's explicit process env. Values never enter argv.
            args.extend(["--env", key])
        # env -i prevents image ENV/defaults from leaking. Expansion happens inside the container,
        # with quoted names generated only from validated identifiers; never interpolate values.
        assignments = " ".join(f'"{key}=${{{key}}}"' for key in sorted(full_env))
        script = "umask 077; exec /usr/bin/env -i " + assignments + ' "$@"'
        return args + [data["Id"], "/bin/sh", "-c", script, "ajx-env", *argv]

    def shell(self, command, ctx, timeout=600, env=None):
        if not isinstance(command, str) or "\0" in command:
            _fail("environment.shell", "expected a shell command string without NUL bytes")
        full_env = self._worker_env(ctx, env)
        try:
            result = _tail_process(self.wrap(["/bin/sh", "-c", command], ctx, full_env),
                                   ctx["workspace"], timeout, full_env)
        except BaseException:
            if self.backend == "container":
                self.abort(ctx)
            raise
        result["cmd"] = command
        if result["timed_out"]:
            result["environment_abort"] = self.abort(ctx)
        return result

    @staticmethod
    def _local_platform():
        facts = {"system": host_platform.system(), "release": host_platform.release(),
                 "machine": host_platform.machine(), "effective_uid": os.geteuid(), "effective_gid": os.getegid()}
        if facts["system"] == "Linux":
            try:
                release = host_platform.freedesktop_os_release()
                facts["os_release"] = {key: release[key] for key in ("ID", "VERSION_ID", "PRETTY_NAME") if key in release}
            except OSError:
                facts["os_release"] = None
        return facts

    def _limitations(self):
        common = ["Inventory covers declared probes and bootstrap/runtime identity, not every installed package.",
                  "Host evidence directories have no enforced disk quota; archive after worker shutdown.",
                  "Explicitly supplied authentication values and command output require the caller's redaction.",
                  "System/runtime configuration in the selected OS or image may still affect installed tools."]
        if self.backend == "local":
            return ["Local execution is observational: no filesystem, network, sudo, or root sandbox.",
                    "Fresh HOME/config/cache paths do not hide other host paths or global configuration.",
                    "Local process groups cannot contain a process that creates a new session; "
                    "no independent lifetime survives coordinator termination."] + common
        return ["Container isolation depends on the local Docker daemon, kernel, and trusted image runtime.",
                "Only user-level installation into writable attempt directories is supported.",
                "The root-owned supervisor and automatic removal bound lifetime without the coordinator.",
                "Bridge networking, when selected, has no endpoint, host/LAN, or metadata allowlist.",
                "Runtime /proc, /dev and Docker-managed DNS/hosts files remain present.",
                "Read-only fixture sources are trusted inputs; host changes during a run are not prevented."] + common

    def _capabilities(self, *, confirmed):
        container = self.backend == "container"
        state = "enforced" if container and confirmed else "unknown" if container else "unsupported"
        return {
            "filesystem_isolation": {"declared": self.profile["permissions"]["filesystem"], "state": state,
                                     "mechanism": "read-only rootfs and explicit nonrecursive bind mounts" if container else None},
            "network_isolation": {"declared": self.profile["network"],
                                  "state": state if self.profile["network"] == "none" else "unsupported",
                                  "mechanism": "Docker none network; loopback only" if self.profile["network"] == "none" else None},
            "non_root": {"declared": self.profile["permissions"]["root"], "state": state,
                         "mechanism": "numeric non-root exec user; all capabilities dropped; no-new-privileges" if container else None},
            "resource_limits": {"declared": copy.deepcopy(self.profile["limits"]), "state": state,
                                "mechanism": "Docker memory/CPU/pids limits, bounded tmpfs" if container else None},
            "lifetime": {"declared": self.profile["limits"].get("lifetime_seconds"), "state": state,
                         "mechanism": "root-owned sleep under init; auto-remove; no restart" if container else None},
            "package_install": {"declared": self.profile["permissions"]["package_install"],
                                "state": state if container else "observed-only",
                                "mechanism": "user directories writable; system rootfs read-only" if container else None},
            "configuration_paths": {"declared": "fresh HOME/XDG/config/cache; no inherited process environment",
                                    "state": "enforced" if confirmed else "unknown",
                                    "mechanism": "explicit directories/env; does not disable global OS configuration"},
        }

    def _report(self, ctx, handle, *, fresh):
        report = {"id": self.profile["id"], "backend": self.backend, "status": "prepared" if fresh else "attached",
                  "image": copy.deepcopy(handle.get("image")), "mounts": copy.deepcopy(handle["mounts"]),
                  "env_names": sorted(self._worker_env(ctx)), "limitations": self._limitations(),
                  "capabilities": self._capabilities(confirmed=True),
                  "inventory": {"phase": "initial" if fresh else "recovered", "tools": [],
                                "scope": "declared probes only; not a complete software inventory"}}
        if self.backend == "local":
            report["platform"] = self._local_platform()
        else:
            # This fixed, unprivileged probe contains no task/verifier instructions or credentials.
            probe = self.shell(
                "printf 'system='; uname -s; printf 'release='; uname -r; printf 'machine='; uname -m; "
                "printf 'uid='; id -u; printf 'gid='; id -g; "
                "while read -r key value rest; do case \"$key\" in CapEff:|NoNewPrivs:|Seccomp:) "
                "printf '%s=%s\\n' \"$key\" \"$value\";; esac; done < /proc/self/status; "
                "while read -r key value rest; do case \"$key\" in Uid:) "
                "printf 'supervisor_uid=%s\\n' \"$value\";; esac; done < /proc/1/status; "
                "while IFS=: read -r iface rest; do case \"$iface\" in *'|'*) continue;; esac; "
                "set -- $iface; [ \"$#\" = 1 ] && printf 'interface=%s\\n' \"$1\"; done < /proc/net/dev; "
                "if [ -r /etc/os-release ]; then while IFS='=' read -r key value; do "
                "case \"$key\" in ID|VERSION_ID|PRETTY_NAME) printf 'os_%s=%s\\n' \"$key\" \"$value\";; esac; "
                "done < /etc/os-release; fi",
                ctx, timeout=10)
            if probe["timed_out"] or probe["exit_code"] != 0:
                raise EnvironmentError("Container bootstrap probe failed; image requires sh/env/sleep/uname/id")
            fields, interfaces = {}, []
            for line in probe["stdout"].splitlines():
                key, sep, value = line.partition("=")
                if sep and key == "interface":
                    interfaces.append(value)
                elif sep:
                    fields[key] = value
            expected_uid = handle["worker_user"].split(":")[0]
            if (fields.get("uid") != expected_uid or fields.get("NoNewPrivs:") != "1"
                    or fields.get("Seccomp:") != "2" or fields.get("supervisor_uid") != "0"
                    or int(fields.get("CapEff:", "-1"), 16) != 0):
                raise EnvironmentError("Observed worker privileges do not match the non-root contract")
            if self.profile["network"] == "none" and set(interfaces) != {"lo"}:
                raise EnvironmentError("Observed interfaces do not match network='none'")
            report["platform"] = {k: fields.get(k) for k in ("system", "release", "machine", "uid", "gid")}
            report["platform"]["os_release"] = {key[3:]: value for key, value in fields.items() if key.startswith("os_")}
            report["capabilities"]["non_root"]["observations"] = {
                "uid": fields["uid"], "effective_capabilities": fields["CapEff:"],
                "no_new_privileges": True, "seccomp_filter": True}
            report["capabilities"]["lifetime"]["observations"] = {"supervisor_uid": fields["supervisor_uid"]}
            report["capabilities"]["network_isolation"]["observations"] = {"interfaces": interfaces}
            # Probe writes using throwaway sentinels, including read mounts. Never modify real inputs.
            marker = ".ajx-probe-" + handle["owner"]
            commands = []
            for path in handle["paths"].values():
                sentinel = shlex.quote(str(Path(path) / marker))
                commands.append(f"(umask 077; : > {sentinel}) && rm -f -- {sentinel}")
            denied = ["/" + marker, str(Path(ctx["run_dir"]) / marker)]
            denied += [str(PurePosixPath(m["target"]) / marker) for m in handle["mounts"] if m["access"] == "read"]
            for path in denied:
                quoted = shlex.quote(path)
                commands.append(f"if (: > {quoted}) 2>/dev/null; then rm -f -- {quoted}; exit 91; fi")
            writes = self.shell("set -e; " + "; ".join(commands), ctx, timeout=10)
            if writes["timed_out"] or writes["exit_code"] != 0:
                raise EnvironmentError("Synthetic permitted/denied directory write probe failed")
            report["capabilities"]["filesystem_isolation"]["observations"] = {
                "private_roots_writable": True, "root_and_report_writes_denied": True,
                "read_mount_writes_denied": True, "scope": "synthetic sentinels, not an exhaustive escape proof"}
        if fresh:
            for tool in self.profile["tools"]:
                result = self.shell(shlex.join(tool["argv"]), ctx, timeout=15)
                observed = {"name": tool["name"], "required": tool["required"], "argv": list(tool["argv"]),
                            **{k: result[k] for k in ("exit_code", "timed_out", "stdout", "stderr")}}
                for stream in ("stdout", "stderr"):
                    observed[stream] = _redact_env_output(observed[stream], self._worker_env(ctx))
                observed["argv"] = [_redact_env_output(arg, self._worker_env(ctx)) for arg in observed["argv"]]
                observed["status"] = "observed" if result["exit_code"] == 0 and not result["timed_out"] else "unavailable"
                report["inventory"]["tools"].append(observed)
                if tool["required"] and observed["status"] != "observed":
                    raise EnvironmentError(f"environments.{self.profile['id']}.tools.{tool['name']}: "
                                           "required starting-inventory probe failed")
        else:
            report["inventory"]["tools"] = [{"name": t["name"], "status": "unknown",
                                             "reason": "recovery does not replay starting inventory commands"}
                                            for t in self.profile["tools"]]
        return report

    def abort(self, ctx):
        """Stop container workers; local callers reap processes while retaining cleanup access."""
        if self.backend == "local":
            return {"status": "caller_managed", "confirmed": False, "backend": "local",
                    "worker_stop": "caller-managed",
                    "limitations": ["The caller must reap local process groups; abort keeps the prepared "
                                    "environment available for cleanup until final release."]}
        return self.release(ctx)

    def release(self, ctx):
        """Remove only an ownership-verified container; retain bounded host evidence for archive."""
        try:
            handle = self._handle(ctx, cleanup=True)
            self._fresh_owner = None
            if self.backend == "local":
                marker = self._marker_path(ctx, handle)
                if marker.exists():
                    self._write_marker(ctx, handle, "released")
                self._released = True
                return {"status": "released", "confirmed": True, "backend": "local", "owned_resources": [],
                        "worker_stop": "caller-managed",
                        "limitations": ["No local sandbox or independent process-lifetime guarantee; "
                                        "caller must reap harness process groups before archiving."]}
            data = self._find(handle, ctx)
            if data is None:
                self._released = True
                return {"status": "already_absent", "confirmed": True, "backend": "container",
                        "name": handle["name"], "reason": "not present; expiry/startup failure cannot be distinguished"}
            identity = data["Id"]
            result = self._docker(["container", "rm", "--force", identity], ctx=ctx, handle=handle, timeout=15)
            # A concurrent --rm expiry can make rm fail; verify absence instead of trusting its exit.
            remaining = self._find(handle, ctx, identifier=identity)
            if remaining is not None:
                return {"status": "failed", "confirmed": False, "backend": "container", "name": handle["name"],
                        "error": "owned container remains after removal", "docker_exit_code": result["code"]}
            self._released = True
            return {"status": "released", "confirmed": True, "backend": "container",
                    "name": handle["name"], "container_id": identity, "host_evidence": "preserved"}
        except (EnvironmentError, ValueError, OSError) as exc:
            return {"status": "failed", "confirmed": False, "backend": self.backend, "error": str(exc)}


__all__ = ["EnvironmentError", "EnvironmentRunner", "normalize_profiles", "validate_for_cell"]
