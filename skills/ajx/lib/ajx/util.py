"""Shared helpers: time, hashing, json io, placeholder filling, subprocess with timestamps."""

import hashlib
import json
import os
import re
import signal
import subprocess
import threading
import time
from datetime import datetime, timezone
from pathlib import Path

SKILL_ROOT = Path(__file__).resolve().parents[2]
# Only these parts of the skill define its behavior; examples, tests and generated reports do not.
FINGERPRINT_PARTS = ("SKILL.md", "bin", "lib", "prompts", "schemas", "references")


def now():
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def parse_ts(value):
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def seconds_between(a, b):
    if not a or not b:
        return None
    return round((parse_ts(b) - parse_ts(a)).total_seconds(), 3)


def sha256_bytes(data):
    return hashlib.sha256(data).hexdigest()


def sha256_file(path):
    return sha256_bytes(Path(path).read_bytes())


def skill_fingerprint():
    """Content hash of the skill's behavior-defining files, used as ajx_skill_revision."""
    digest = hashlib.sha256()
    for part in FINGERPRINT_PARTS:
        root = SKILL_ROOT / part
        paths = [root] if root.is_file() else sorted(p for p in root.rglob("*") if p.is_file()) if root.exists() else []
        for path in paths:
            if "__pycache__" in path.parts:
                continue
            digest.update(str(path.relative_to(SKILL_ROOT)).encode())
            digest.update(path.read_bytes())
    return "sha256:" + digest.hexdigest()[:16]


def read_json(path, default=None):
    path = Path(path)
    if not path.exists():
        return default
    return json.loads(path.read_text(encoding="utf-8"))


def write_json(path, data):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    tmp.replace(path)


def read_jsonl(path):
    path = Path(path)
    if not path.exists():
        return []
    rows = []
    for line in path.read_text(encoding="utf-8").splitlines():
        if line.strip():
            try:
                rows.append(json.loads(line))
            except json.JSONDecodeError:
                rows.append({"_unparsed": line})
    return rows


_PLACEHOLDER = re.compile(r"\{([a-z_]+)\}")


def fill(template, values):
    """Replace {name} placeholders that are in `values`; leave every other brace untouched
    (unlike str.format, a literal '{' in a user's argv template is not an error)."""
    return _PLACEHOLDER.sub(lambda m: str(values[m.group(1)]) if m.group(1) in values else m.group(0), template)


def fill_all(items, values):
    return [fill(str(item), values) for item in items]


def child_env(extra=None, drop=()):
    """Inherit the caller's environment (credentials are the auth profile's concern), minus
    variables an adapter or auth profile declares. Entries in `drop` ending in '*' are prefixes."""
    exact = {d for d in drop if not d.endswith("*")}
    prefixes = tuple(d[:-1] for d in drop if d.endswith("*"))
    env = {k: v for k, v in os.environ.items()
           if k not in exact and not (prefixes and k.startswith(prefixes))}
    env.update(extra or {})
    return env


def _killpg(pid, sig):
    try:
        os.killpg(pid, sig)
    except (ProcessLookupError, PermissionError):
        pass


def kill_group(pid):
    """SIGTERM then SIGKILL a process group left behind by a worker (dev servers, watchers)."""
    _killpg(pid, signal.SIGTERM)
    time.sleep(1)
    _killpg(pid, signal.SIGKILL)


def run_streaming(argv, cwd, out_path, err_path, timeout, env=None, stdin_data=None, grace=3.0,
                  reap_after_exit=True):
    """Run a process in its own process group, stamping each stdout line with its arrival time.

    Writes {"at": ts, "line": raw} records to out_path. Kills the whole group on timeout, and
    (when reap_after_exit) again after a normal exit so leftovers do not outlive the stage. The
    execute stage passes reap_after_exit=False so a server the agent started is still up for
    verification; the runner reaps it afterwards. stdin is fed from a thread so a large prompt
    cannot deadlock against a chatty child. Returns dict(exit_code, timed_out, pid, started_at,
    stopped_at).
    """
    started = now()
    out_path, err_path = Path(out_path), Path(err_path)
    with out_path.open("w", encoding="utf-8") as out, err_path.open("w", encoding="utf-8") as err:
        proc = subprocess.Popen(
            argv, cwd=str(cwd), env=env, stdout=subprocess.PIPE, stderr=err,
            stdin=subprocess.PIPE if stdin_data is not None else subprocess.DEVNULL,
            text=True, encoding="utf-8", errors="replace", start_new_session=True)
        timed_out = threading.Event()

        def killer():
            timed_out.set()
            _killpg(proc.pid, signal.SIGTERM)
            time.sleep(5)
            _killpg(proc.pid, signal.SIGKILL)

        def feeder():
            try:
                proc.stdin.write(stdin_data)
                proc.stdin.close()
            except (BrokenPipeError, OSError):
                pass

        def reader():
            try:
                for line in proc.stdout:
                    out.write(json.dumps({"at": now(), "line": line.rstrip("\n")}) + "\n")
                    out.flush()
            except ValueError:  # output file closed after the stage ended; a leftover child kept writing
                pass

        timer = threading.Timer(timeout, killer) if timeout else None
        threads = [threading.Thread(target=reader, daemon=True)]
        if stdin_data is not None:
            threads.append(threading.Thread(target=feeder, daemon=True))
        try:
            if timer:
                timer.start()
            for t in threads:
                t.start()
            code = proc.wait()
            # The pipe stays open while grandchildren hold it; give them a moment, then reap
            # (or leave them for the runner when the stage wants survivors kept alive).
            threads[0].join(grace)
            if reap_after_exit:
                _killpg(proc.pid, signal.SIGTERM)
                threads[0].join(grace)
                if threads[0].is_alive():
                    _killpg(proc.pid, signal.SIGKILL)
                    threads[0].join(grace)
        finally:
            if timer:
                timer.cancel()
            for stream in (proc.stdout, proc.stdin):
                if stream:
                    try:
                        stream.close()
                    except OSError:
                        pass
    return {"exit_code": code, "timed_out": timed_out.is_set(), "pid": proc.pid,
            "started_at": started, "stopped_at": now()}


def run_shell(cmd, cwd, timeout=600, env=None, shell=None):
    """`/bin/sh -c cmd` (or `<shell> -c cmd`), non-interactive, output tails kept."""
    started = now()
    try:
        proc = subprocess.run(cmd, shell=True, executable=shell, cwd=str(cwd), capture_output=True, text=True,
                              timeout=timeout, env=env, errors="replace", stdin=subprocess.DEVNULL)
        code, out, err, timed_out = proc.returncode, proc.stdout, proc.stderr, False
    except subprocess.TimeoutExpired as exc:
        code, timed_out = None, True
        out = (exc.stdout or b"").decode(errors="replace") if isinstance(exc.stdout, bytes) else (exc.stdout or "")
        err = (exc.stderr or b"").decode(errors="replace") if isinstance(exc.stderr, bytes) else (exc.stderr or "")
    except OSError as exc:  # missing shell or cwd
        code, timed_out, out, err = None, False, "", f"{type(exc).__name__}: {exc}"
    return {"cmd": cmd, "exit_code": code, "timed_out": timed_out, "stdout": out[-8000:],
            "stderr": err[-4000:], "started_at": started, "stopped_at": now()}
