"""AJX Console: a local web page for reviewing, starting and watching one trial.

`ajx console <trial>` serves the page on 127.0.0.1 and opens it. The console runs as the user who
started it and adds no execution path of its own: every action runs an existing `ajx` command
(validate, doctor, run, report, export) or edits the trial files shown on the page. Its state is
the trial's own files plus a small journal in <trial-dir>/.ajx-console/, so the page and a
coordinating agent see the same trial, and closing either one never stops a run.
`ajx console since|wait|note|start` give an agent the same view without the server.
"""

import hashlib
import hmac
import http.server
import json
import os
import re
import secrets
import shlex
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
import tomllib
import urllib.parse
import urllib.request
import webbrowser
from datetime import datetime
from pathlib import Path

from . import base, plugins, runner, spec as specmod
from .util import SKILL_ROOT, now, parse_ts, read_json, sha256_bytes

AJX_BIN = SKILL_ROOT / "bin" / "ajx"
ASSETS = SKILL_ROOT / "assets"
DEMO_DIR = SKILL_ROOT / "examples" / "console-demo"
STATE_DIR = ".ajx-console"
SCHEMA = 1
IDLE_SECONDS = 30 * 60
MAX_TEXT = 4000

# Recovery commands the page offers for one run. Anything else stays a CLI decision.
STAGE_SETS = {
    "teardown,render": "Retry task cleanup",
    "teardown,release,render,archive": "Retry task cleanup in the kept environment",
    "release,render": "Release the environment",
    "extract,measure,render": "Re-run the analysis (makes reporter model calls)",
}

# Advisory only: words that can turn the worker's task into a guided or observed one.
PROMPT_LINT = (
    (r"\bAJX\b", "names AJX"),
    (r"\b(evaluat\w*|benchmark\w*|usability (?:test|study|review))\b", "says the task is an evaluation"),
    (r"\b(this is a test|you are being (?:tested|evaluated|observed|watched))\b", "tells the worker it is observed"),
    (r"\b(journey|friction|product asks?)\b", "uses AJX reporting vocabulary"),
    (r"\b(?:known|expected) (?:bugs?|issues?|problems?|errors?)\b", "mentions an expected problem"),
    (r"\bwork-?arounds?\b", "suggests a workaround"),
)

CONTENT_TYPES = {".html": "text/html; charset=utf-8", ".md": "text/plain; charset=utf-8",
                 ".txt": "text/plain; charset=utf-8", ".log": "text/plain; charset=utf-8",
                 ".jsonl": "text/plain; charset=utf-8", ".csv": "text/plain; charset=utf-8",
                 ".json": "application/json; charset=utf-8", ".svg": "image/svg+xml", ".png": "image/png"}
STATIC = {"console.js": "text/javascript; charset=utf-8", "console.css": "text/css; charset=utf-8"}
PAGE_CSP = ("default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; "
            "base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
REPORT_CSP = ("sandbox allow-scripts allow-popups allow-popups-to-escape-sandbox allow-downloads; default-src 'none'; "
              "style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'")


class ConsoleError(Exception):
    status = 400


class Conflict(ConsoleError):
    status = 409


# --------------------------------------------------------------------------- paths and files

def trial_file(path):
    p = Path(path).expanduser()
    if p.is_dir():
        p = p / "trial.toml"
    p = p.resolve()
    if not p.is_file():
        raise ConsoleError(f"trial file not found: {p}")
    return p


def state_dir(trial):
    d = Path(trial).parent / STATE_DIR
    d.mkdir(mode=0o700, exist_ok=True)
    return d


def _write_private(path, data):
    """JSON readable only by the user (the server file carries the page's session key)."""
    path = Path(path)
    tmp = path.with_name(f".{path.name}.{os.getpid()}.tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as fh:
        fh.write(json.dumps(data, indent=2) + "\n")
    os.replace(tmp, path)


def _clip(text, limit):
    text = "" if text is None else str(text)
    return text if len(text) <= limit else text[:limit - 1] + "\u2026"


def _short(path):
    text = str(path)
    home = str(Path.home())
    return "~" + text[len(home):] if text == home or text.startswith(home + os.sep) else text


def _alive(pid):
    try:
        return bool(pid) and runner._pid_alive(int(pid))
    except (TypeError, ValueError):
        return False


def _is_ajx(pid, verb):
    """Whether pid is still an `ajx <verb>` process, so a reused pid is never signalled or shown as
    running. None when `ps` cannot tell."""
    try:
        res = subprocess.run(["ps", "-o", "command=", "-p", str(int(pid))], capture_output=True, text=True, timeout=5)
    except (OSError, ValueError, subprocess.TimeoutExpired):
        return None
    command = f" {res.stdout.strip()} "
    if res.returncode != 0 and not res.stdout.strip():
        return False
    return "ajx" in command and f" {verb} " in command


def _seconds_since(at):
    try:
        return max(0, int(time.time() - parse_ts(str(at)).timestamp()))
    except (TypeError, ValueError):
        return None


def _raw(trial):
    try:
        return tomllib.loads(Path(trial).read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, tomllib.TOMLDecodeError):
        return {}


def load(trial):
    """(spec, error). The page shows the trial as written, without the caller's secrets."""
    try:
        return specmod.load(trial, readiness=False), None
    except Exception as exc:  # noqa: BLE001 - an invalid trial is something to show, not a crash
        return None, str(exc) if isinstance(exc, specmod.SpecError) else f"{type(exc).__name__}: {exc}"


def output_dir(trial, spec=None):
    if spec:
        return Path(spec["trial"]["output_dir"])
    raw = _raw(trial).get("trial") or {}
    base_dir = Path(trial).parent
    default = base_dir / "ajx-reports" / str(raw.get("product") or "product") / str(raw.get("id") or "trial")
    return (base_dir / str(raw.get("output_dir") or default)).resolve()


def prompt_path(trial, spec=None):
    if spec:
        return Path(spec["task"]["prompt_path"])
    name = (_raw(trial).get("task") or {}).get("prompt_file")
    return (Path(trial).parent / str(name)).resolve() if name else None


_DIGESTS = {}  # root -> (file stats, digest, complete): polling must not re-read unchanged files


def _tree_digest(root, max_files=2000, max_bytes=64 << 20):
    """(digest, complete) over a directory's files, excluding the console's own state and caches."""
    root = Path(root)
    entries, total = [], 0
    for p in sorted(root.rglob("*")):
        parts = p.relative_to(root).parts
        if STATE_DIR in parts or "__pycache__" in parts:
            continue
        if p.is_symlink():
            entries.append(("link", str(p.relative_to(root)), os.readlink(p)))
        elif p.is_file():
            st = p.stat()
            total += st.st_size
            entries.append(("file", str(p.relative_to(root)), st.st_size, st.st_mtime_ns))
            if len(entries) > max_files or total > max_bytes:
                return hashlib.sha256(b"<too large>").hexdigest(), False
    stats = tuple(entries)
    cached = _DIGESTS.get(str(root))
    if cached and cached[0] == stats:
        return cached[1], cached[2]
    h = hashlib.sha256()
    for item in entries:
        if item[0] == "link":
            h.update(b"link\0" + item[1].encode() + b"\0" + item[2].encode())
        else:
            h.update(b"file\0" + item[1].encode() + b"\0" + hashlib.sha256((root / item[1]).read_bytes()).digest())
    _DIGESTS[str(root)] = (stats, h.hexdigest(), True)
    return h.hexdigest(), True


def plan_info(trial, spec=None):
    """What an approval covers, byte for byte: trial.toml, the task prompt, the starting files and
    the trial's own plugins. `complete` is False when those files are too large to fingerprint."""
    h = hashlib.sha256(b"ajx-console-plan-2\0trial\0" + Path(trial).read_bytes())
    pp = prompt_path(trial, spec)
    h.update(b"\0prompt\0" + (pp.read_bytes() if pp and pp.is_file() else b""))
    complete, covers = True, ["trial.toml", "the task prompt"]
    fixture = (spec or {}).get("task", {}).get("fixture_dir")
    for label, root in (("the starting files", fixture), ("the trial's plugins", Path(trial).parent / "plugins")):
        if root and Path(root).is_dir():
            digest, ok = _tree_digest(root)
            h.update(b"\0" + label.encode() + b"\0" + digest.encode())
            complete &= ok
            covers.append(label)
    return {"sha": h.hexdigest(), "complete": complete, "covers": covers}


def plan_sha(trial, spec=None):
    return plan_info(trial, spec)["sha"]


def lint_prompt(text):
    found = []
    for pattern, why in PROMPT_LINT:
        m = re.search(pattern, text or "", re.IGNORECASE)
        if m:
            found.append({"match": m.group(0), "why": why})
    return found


# --------------------------------------------------------------------------- journal and presence

def _journal(trial):
    return state_dir(trial) / "actions.jsonl"


def record(trial, actor, kind, **fields):
    """Append one action and return it with its line number `n`. A single O_APPEND write keeps lines
    whole across processes."""
    entry = {"at": now(), "actor": actor, "kind": kind, **{k: v for k, v in fields.items() if v is not None}}
    raw = json.dumps(entry, ensure_ascii=False, sort_keys=True).encode("utf-8")
    path = _journal(trial)
    fd = os.open(path, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
    try:
        os.write(fd, raw + b"\n")
    finally:
        os.close(fd)
    lines = path.read_bytes().split(b"\n")[:-1]
    entry["n"] = next((i for i in range(len(lines), 0, -1) if lines[i - 1] == raw), len(lines))
    return entry


def read_entries(trial, cursor=0):
    """(entries after `cursor`, new cursor). The cursor counts complete lines; each entry carries
    its 1-based line number `n`. A line still being written is left for the next read."""
    path = _journal(trial)
    data = path.read_bytes() if path.is_file() else b""
    complete = data.split(b"\n")[:-1]
    out = []
    for n, raw in enumerate(complete, 1):
        if n <= cursor:
            continue
        try:
            entry = json.loads(raw)
        except ValueError:
            entry = {"kind": "unreadable"}
        entry["n"] = n
        out.append(entry)
    return out, len(complete)


def touch_agent(trial, mode, until=None):
    _write_private(state_dir(trial) / "agent.json", {"last_seen": now(), "mode": mode, "until": until})


def presence(trial):
    info = read_json(state_dir(trial) / "agent.json", {}) or {}
    if not info.get("last_seen"):
        return {"state": "never", "last_seen": None}
    if info.get("mode") == "watching" and (info.get("until") or 0) > time.time():
        return {"state": "watching", "last_seen": info["last_seen"]}
    return {"state": "seen", "last_seen": info["last_seen"]}


# --------------------------------------------------------------------------- jobs

def _job_env():
    return {**os.environ, "PYTHONUNBUFFERED": "1"}


def _display(args):
    """The equivalent command line for people: the home directory as ~ (unquoted, to stay readable)
    and plan ids shortened."""
    def one(a):
        a = _short(a)
        if re.fullmatch(r"[0-9a-f]{40,}", a):
            return a[:12] + "\u2026"
        return a if re.fullmatch(r"[\w@%+=:,./~-]+", a) else shlex.quote(a)
    return "ajx " + " ".join(one(a) for a in args)


def _jobs(entries):
    """Launched commands keyed by their `job_started` line number (pids can be reused)."""
    started = [e for e in entries if e.get("kind") == "job_started" and e.get("pid")]
    finished = {e.get("job_n") for e in entries if e.get("kind") == "job_finished"}
    return started, finished


def _job_alive(e):
    verb = "report" if e.get("job") == "report" else "run"
    return e.get("host") == socket.gethostname() and _alive(e["pid"]) and _is_ajx(e["pid"], verb) is not False


def finish_job(trial, job_n, pid, exit_code=None, job=None):
    """Record a launched command's end once, whichever process notices first."""
    marker = state_dir(trial) / f"finished-{int(job_n)}"
    try:
        os.close(os.open(marker, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600))
    except FileExistsError:
        return False
    record(trial, "console", "job_finished", job_n=int(job_n), pid=int(pid), job=job, exit_code=exit_code)
    return True


def reconcile(trial, entries, skip=()):
    """Record the end of commands whose process is gone. The server records its own children with
    their exit codes; others wait a few quiet seconds so a reaper can record the code first."""
    started, finished = _jobs(entries)
    changed = False
    for e in started:
        if e["n"] in finished or int(e["pid"]) in skip or _job_alive(e):
            continue
        log = state_dir(trial) / "logs" / Path(str(e.get("log") or "none")).name
        if log.exists() and time.time() - log.stat().st_mtime < 3:
            continue
        changed |= finish_job(trial, e["n"], e["pid"], None, e.get("job"))
    return changed


def _lock(out):
    info = read_json(Path(out) / ".lock", None)
    if not isinstance(info, dict):
        return None
    same = info.get("host") == socket.gethostname()
    alive = same and _alive(info.get("pid")) and _is_ajx(info.get("pid"), "run") is not False
    return {**info, "alive": alive, "same_host": same}


def active_job(trial, entries, out):
    """The command still running for this trial: one the console or an agent launched, or an
    `ajx run` started elsewhere that holds the output directory's lock."""
    started, finished = _jobs(entries)
    for e in reversed(started):
        if e["n"] not in finished and _job_alive(e):
            return e
    lock = _lock(out)
    if lock and lock["alive"]:
        return {"job": "run", "pid": lock["pid"], "at": lock.get("at"), "actor": "outside",
                "command": "ajx run (started outside the console)", "outside": True}
    return None


def launch(trial, job, args, actor, **fields):
    logs = state_dir(trial) / "logs"
    logs.mkdir(exist_ok=True)
    log = logs / f"{datetime.now().strftime('%Y%m%d-%H%M%S')}-{job}.log"
    with open(log, "ab") as fh:
        proc = subprocess.Popen([sys.executable, str(AJX_BIN), *args], cwd=str(Path(trial).parent),
                                stdin=subprocess.DEVNULL, stdout=fh, stderr=subprocess.STDOUT,
                                start_new_session=True, env=_job_env())
    entry = record(trial, actor, "job_started", job=job, pid=proc.pid, host=socket.gethostname(),
                   command=_display(args), log=log.name, **fields)
    return proc, entry


def _run_cli(args, timeout, cwd):
    argv = [sys.executable, str(AJX_BIN), *args]
    try:
        p = subprocess.run(argv, cwd=str(cwd), capture_output=True, text=True, timeout=timeout,
                           stdin=subprocess.DEVNULL, env=_job_env())
        output, code, timed_out = (p.stdout or "") + (p.stderr or ""), p.returncode, False
    except subprocess.TimeoutExpired as exc:
        parts = [x.decode("utf-8", "replace") if isinstance(x, bytes) else (x or "") for x in (exc.stdout, exc.stderr)]
        output, code, timed_out = "".join(parts) + f"\n(timed out after {timeout}s)", None, True
    return {"command": _display(args), "exit_code": code, "timed_out": timed_out, "output": _clip(output, 40000)}


# --------------------------------------------------------------------------- readiness

def _readiness_path(trial):
    return state_dir(trial) / "readiness.json"


def readiness(trial, current_plan=None):
    r = dict(read_json(_readiness_path(trial), {}) or {})
    if not r:
        return {"status": "unknown", "current": False}
    if r.get("status") == "checking" and not _alive(r.get("pid")):
        r["status"] = "unknown"
    r["current"] = bool(current_plan) and r.get("plan") == current_plan
    return r


_PROBLEM = re.compile(r"NOT READY|FAILED|PROBLEMS|WARNING|trial file error|error:", re.IGNORECASE)


def check(trial, actor="user"):
    """`ajx validate` then `ajx doctor`, recorded for the plan they checked."""
    trial = trial_file(trial)
    spec, _ = load(trial)
    plan = plan_sha(trial, spec)
    _write_private(_readiness_path(trial), {"status": "checking", "plan": plan, "started_at": now(), "pid": os.getpid()})
    record(trial, actor, "check_started", plan=plan[:12])
    steps, ok = [], True
    for name, timeout in (("validate", 120), ("doctor", 300)):
        res = _run_cli([name, str(trial)], timeout, trial.parent)
        steps.append({"name": name, **res})
        if res["exit_code"] != 0:
            ok = False
            break
    problems = [line.strip() for s in steps for line in s["output"].splitlines() if _PROBLEM.search(line)][:12]
    result = {"status": "ready" if ok else "not_ready", "plan": plan, "checked_at": now(), "steps": steps,
              "problems": problems}
    _write_private(_readiness_path(trial), result)
    record(trial, actor, "checked", plan=plan[:12], status=result["status"])
    return result


# --------------------------------------------------------------------------- run views

def _harness_for_run(spec, run_dir):
    name = None
    try:
        for cell in (spec or {}).get("cells") or []:
            prefix = f"{cell['id']}-r"
            if run_dir.name.startswith(prefix) and run_dir.name[len(prefix):].isdigit():
                return specmod.harness_for(spec, cell["harness"]), None
        name = (((read_json(run_dir / "run.json", {}) or {}).get("agent") or {}).get("harness_name")
                or ((read_json(run_dir / "environment.json", {}) or {}).get("harness") or {}).get("name"))
        if not name:
            return None, "The agent for this run is not known yet."
        return plugins.get("harness", name)(), None
    except Exception as exc:  # noqa: BLE001 - a status view must not fail on an odd run dir
        return None, f"agent {name!r} unavailable: {type(exc).__name__}"


def _feed(spec, run_dir, limit):
    harness, why = _harness_for_run(spec, run_dir)
    if harness is None:
        return [], why
    if type(harness).progress is base.Harness.progress:
        return [], f"{harness.name} does not report live progress. The report lists every event after the run."
    try:
        items = list(harness.progress(run_dir, "execute", limit) or [])[-limit:]
    except Exception as exc:  # noqa: BLE001
        return [], f"live progress unavailable: {type(exc).__name__}"
    return [{"at": i.get("at"), "kind": i.get("kind"), "text": _clip(i.get("text"), 240), "error": bool(i.get("error"))}
            for i in items if isinstance(i, dict)], None


def _asks_view(asks, marks):
    if asks is None:
        extract = (marks.get("extract") or {}).get("status")
        return {"status": "generating" if extract == "running" else "pending"}
    status = asks.get("extraction_status") or "ok"
    if status == "in progress":
        return {"status": "generating"}
    if status != "ok":
        return {"status": "incomplete", "reason": status}
    items = asks.get("asks") or []
    return {"status": "ready", "count": len(items), "no_obstacles_observed": bool(asks.get("no_obstacles_observed")),
            "strengths": len(asks.get("strengths") or []), "gates": len(asks.get("gates") or []),
            "top": [{"id": a.get("id"), "title": a.get("title"), "labels": a.get("labels") or [],
                     "event_refs": a.get("event_refs") or [], "evidence_status": a.get("evidence_status"),
                     "requested_behavior": _clip(a.get("requested_behavior"), 400),
                     "consequence": _clip(a.get("consequence"), 300)} for a in items[:8]]}


def _recovery(st, marks, teardown, env_cleanup):
    out = []
    cleanup = env_cleanup or {}
    if (marks.get("teardown") or {}).get("status") == "done" and teardown in ("failed", "auth_errors", "skipped"):
        stages = "teardown,release,render,archive" if cleanup.get("status") == "deferred_teardown" else "teardown,render"
        out.append({"stages": stages, "label": STAGE_SETS[stages],
                    "why": "Cleanup was not confirmed; resources the task created may still exist."})
    elif cleanup and cleanup.get("confirmed") is not True and cleanup.get("status") != "deferred_teardown":
        out.append({"stages": "release,render", "label": STAGE_SETS["release,render"],
                    "why": "The worker's environment was not confirmed removed."})
    if (marks.get("extract") or {}).get("status") == "error":
        out.append({"stages": "extract,measure,render", "label": STAGE_SETS["extract,measure,render"],
                    "why": "The analysis failed; the evidence is kept."})
    return out


def _cell_for_run(spec, rid):
    for cell in (spec or {}).get("cells") or []:
        prefix = f"{cell['id']}-r"
        if rid.startswith(prefix) and rid[len(prefix):].isdigit():
            return cell
    return None


def _progress_text(rid, stage, status, d, st, mark):
    if status == "error":
        return f"{rid}: {stage} failed: {_clip(mark.get('error'), 200)}"
    if stage == "prepare":
        return f"{rid}: stopped before the worker started" if status == "interrupted" else None
    if stage == "execute":
        return {"running": f"{rid}: the worker started", "done": f"{rid}: the worker finished",
                "interrupted": f"{rid}: the worker was stopped; its evidence is kept"}.get(status)
    if status != "done":
        return None
    if stage == "verify":
        v = read_json(d / "verify.json", {}) or {}
        return f"{rid}: verification {v.get('outcome')} ({v.get('passed')}/{v.get('total')} checks passed)"
    if stage == "teardown":
        td = (st.get("teardown") or {}).get("status")
        return f"{rid}: task cleanup " + {"ok": "ok", "failed": "failed; resources may still exist",
                                           "auth_errors": "hit credential errors; resources may still exist",
                                           "skipped": "did not run"}.get(td, "had nothing to do")
    if stage == "narrate":
        made_up = str(st.get("journey_provenance") or "").startswith("EDITORIAL")
        return f"{rid}: journey written" + (" (a labeled reconstruction)" if made_up else "")
    if stage == "extract":
        a = read_json(d / "asks.json", {}) or {}
        if a.get("extraction_status") == "ok":
            return f"{rid}: {len(a.get('asks') or [])} ask(s) extracted"
        return f"{rid}: ask extraction {a.get('extraction_status') or 'incomplete'}"
    return f"{rid}: report ready" if stage == "render" else None


def journal_progress(trial, out):
    """Journal each run's milestones once (worker started or stopped, verification, cleanup, journey,
    asks, report, any stage error), so the page and `ajx console since` read one story."""
    runs = Path(out) / "runs"
    if not runs.is_dir():
        return False
    seen = state_dir(trial) / "seen"
    seen.mkdir(exist_ok=True)
    changed = False
    for d in sorted(p for p in runs.iterdir() if p.is_dir()):
        st = read_json(d / "state.json", {}) or {}
        marks = st.get("stages") or {}
        for stage in runner.STAGES:
            mark = marks.get(stage) or {}
            text = _progress_text(d.name, stage, mark.get("status"), d, st, mark) if mark.get("status") else None
            if not text:
                continue
            key = re.sub(r"[^A-Za-z0-9._-]", "_", f"{d.name}.{stage}.{mark['status']}.{mark.get('at')}")
            try:
                os.close(os.open(seen / key, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600))
            except FileExistsError:
                continue
            record(trial, "runner", "progress", run=d.name, stage=stage, status=mark["status"],
                   happened_at=mark.get("at"), text=text)
            changed = True
    return changed


def _run_view(spec, out, rid, planned, feed_limit):
    d = Path(out) / "runs" / rid
    st = read_json(d / "state.json", {}) or {}
    marks = st.get("stages") or {}
    stages = [{"name": s, "status": (marks.get(s) or {}).get("status"), "at": (marks.get(s) or {}).get("at"),
               "error": _clip((marks.get(s) or {}).get("error"), 300) or None} for s in runner.STAGES]
    started = any(s["status"] for s in stages)
    current = next((s["name"] for s in stages if s["status"] == "running"), None)
    execution = st.get("execute") or {}
    exec_mark = marks.get("execute") or {}
    exec_started = execution.get("started_at") or (exec_mark.get("at") if exec_mark.get("status") == "running" else None)
    verify = read_json(d / "verify.json", None)
    run_json = read_json(d / "run.json", None) or {}
    teardown = (st.get("teardown") or {}).get("status")
    env_cleanup = st.get("environment_cleanup")
    cell = _cell_for_run(spec, rid)
    owned_env = bool(cell and specmod.environment_for(spec, cell))
    prepared = (marks.get("prepare") or {}).get("status")
    # Finished means a resume can do nothing more: rendered, or a preparation that cannot be retried.
    never_ran = prepared == "interrupted" or (prepared == "error" and owned_env)
    complete = (marks.get("render") or {}).get("status") == "done" or never_ran
    feed, note = (_feed(spec, d, feed_limit) if exec_mark.get("status") and feed_limit > 0 else ([], None))
    view = {
        "id": rid, "planned": planned, "started": started, "has_state": (d / "state.json").is_file(),
        "complete": complete, "never_ran": never_ran, "current": current,
        "stages": stages,
        "execute": {"status": exec_mark.get("status"), "started_at": exec_started,
                    "stopped_at": execution.get("stopped_at"),
                    "elapsed": _seconds_since(exec_started) if exec_mark.get("status") == "running" else None,
                    "timeout_seconds": (spec or {}).get("trial", {}).get("timeout_seconds"),
                    "timed_out": execution.get("timed_out"), "stop_reason": execution.get("stop_reason"),
                    "interrupted": bool(execution.get("interrupted"))},
        "verify": None if verify is None else {
            "outcome": verify.get("outcome"), "passed": verify.get("passed"), "total": verify.get("total"),
            "auth_errors": verify.get("checks_with_auth_errors") or [],
            "checks": [{"name": c.get("name"), "passed": c.get("passed"), "detail": _clip(c.get("detail"), 200)}
                       for c in verify.get("checks") or []]},
        "teardown": teardown,
        "environment_cleanup": None if not env_cleanup else {"status": env_cleanup.get("status"),
                                                             "confirmed": env_cleanup.get("confirmed")},
        "asks": _asks_view(read_json(d / "asks.json", None), marks),
        "review_status": run_json.get("review_status"),
        "declared": _clip(run_json.get("task_outcome_declared"), 400) or None,
        "journey_provenance": st.get("journey_provenance"),
        "feed": feed, "feed_note": note,
        "links": {key: f"runs/{rid}/{name}" for key, name in (
            ("report", "report.html"), ("journey", "pretty-journey.html"), ("asks", "asks.md"),
            ("journey_md", "journey.md")) if (d / name).is_file()},
        "recovery": _recovery(st, marks, teardown, env_cleanup),
    }
    return view


def _runs(spec, out, feed_limit):
    planned = [f"{c['id']}-r{r}" for c, r in specmod.run_plan(spec)] if spec else []
    meta = read_json(Path(out) / "matrix-plan.json", {}) or {}
    runs_dir = Path(out) / "runs"
    existing = sorted(d.name for d in runs_dir.iterdir() if d.is_dir()) if runs_dir.is_dir() else []
    ids = []
    for rid in list(meta.get("order") or []) + existing + planned:
        if rid not in ids:
            ids.append(rid)
    return [_run_view(spec, out, rid, rid in planned, feed_limit) for rid in ids]


def _output(spec, out, runs):
    meta = read_json(Path(out) / "matrix-plan.json", {}) or {}
    started = [r for r in runs if r["started"]]
    planned = [r for r in runs if r["planned"]]
    done = [r for r in planned if r["complete"]]
    blocker, kind = None, None
    # The runner refuses a changed task or profile as soon as any attempt has a state file.
    if spec and any(r["has_state"] for r in runs):
        current_profiles = {"schema_version": 1, "environments": spec.get("environments", {}),
                            "agent_configurations": spec.get("agent_configurations", {})}
        if meta.get("task_prompt_sha256") and meta["task_prompt_sha256"] != spec["task"]["prompt_sha256"]:
            kind = "changed"
            blocker = ("This output folder holds attempts of a different task prompt. Use a fresh output folder "
                       "to run the current task; the recorded attempts keep their original prompt.")
        elif meta.get("normalized_profiles") and \
                runner._profile_contract(meta["normalized_profiles"]) != runner._profile_contract(current_profiles):
            kind = "changed"
            blocker = ("The environment or agent profiles changed after this output folder was started. "
                       "Use a fresh output folder.")
        elif planned and len(done) == len(planned):
            kind = "complete"
            blocker = ("Every planned attempt in this output folder has finished. Add another attempt (same folder, "
                       "comparable conditions) or use a fresh output folder.")
    return {"dir": str(out), "short": _short(out), "exists": Path(out).is_dir(), "started": len(started),
            "planned": len(planned), "complete": len(done), "blocker": blocker, "blocker_kind": kind,
            "resumable": bool(started) and not blocker,
            "index": "index.html" if (Path(out) / "index.html").is_file() else None}


def _place(spec, cell):
    profile = specmod.environment_for(spec, cell)
    if profile and profile.get("backend") == "container":
        limits = profile.get("limits") or {}
        facts = [f"image {str(profile.get('image') or '')[:19]}\u2026",
                 "network: " + ("none" if profile.get("network") == "none" else "bridge (can reach your network and the internet)"),
                 "runs as a non-root user; files outside its folders are not visible"]
        if limits:
            facts.append(f"limits: {limits.get('cpus')} CPU, {limits.get('memory_mb')} MB, "
                         f"{limits.get('lifetime_seconds')} s lifetime")
        return {"kind": "container", "label": "In a container on your machine",
                "summary": "The worker runs inside a local Docker container with the restrictions below.",
                "facts": facts, "required": profile.get("required") or [], "sandbox": True}
    if profile:
        return {"kind": "local-profile", "label": "On your machine, with a fresh profile",
                "summary": "Fresh home, configuration and workspace folders. Not a sandbox: the worker runs as you "
                           "and can still reach your files and network.",
                "facts": [], "required": [], "sandbox": False}
    conf = cell.get("runner") or spec.get("runner") or {"type": "local"}
    kind = conf if isinstance(conf, str) else conf.get("type", "local")
    if kind == "local":
        return {"kind": "local", "label": "On your machine",
                "summary": f"The worker runs as you in a fresh folder under {_short(spec['trial']['workspace_root'])}. "
                           "Not a sandbox: it can read and change what you can, and use your network.",
                "facts": [], "required": [], "sandbox": False}
    return {"kind": kind, "label": f"Through the {kind} runner",
            "summary": "A legacy runner wraps the worker; see the trial file for its isolation.", "facts": [],
            "required": [], "sandbox": False}


def _signin(spec, cell):
    try:
        auth = specmod.auth_for(spec, cell)
    except Exception as exc:  # noqa: BLE001
        return {"profile": cell.get("auth"), "type": None, "description": f"unavailable: {exc}"}
    desc = getattr(type(auth), "description", "") or ""
    if auth.plugin_name == "inherit":
        desc = "Uses the login and settings this agent CLI already has on your machine."
    return {"profile": auth.profile, "type": auth.plugin_name, "description": desc,
            "isolates_config": bool(auth.isolates_config)}


def _check_view(c):
    t = c.get("type")
    if t == "shell":
        detail = c.get("run")
    elif t == "file_exists":
        detail = str(c.get("path")) + (f" containing /{c['contains']}/" if c.get("contains") else "")
    elif t == "http":
        detail = str(c.get("url") or "")
    else:
        detail = json.dumps({k: v for k, v in c.items() if k not in ("name", "type")}, default=str)[:200]
    return {"name": c.get("name"), "type": t, "location": c.get("location"), "detail": _clip(detail, 400)}


def _trial_view(trial, spec, error, out):
    text = Path(trial).read_text(encoding="utf-8", errors="replace")
    raw = _raw(trial)
    view = {"path": str(trial), "short": _short(trial), "dir": str(Path(trial).parent), "sha256": sha256_bytes(text.encode()),
            "error": error, "text_size": len(text)}
    pp = prompt_path(trial, spec)
    prompt_bytes = pp.read_bytes() if pp and pp.is_file() else b""
    prompt_text = prompt_bytes.decode("utf-8", "replace")
    view["prompt"] = {"path": str(pp) if pp else None, "short": _short(pp) if pp else None, "text": prompt_text,
                      "sha256": sha256_bytes(prompt_bytes), "lint": lint_prompt(prompt_text)}
    if not spec:
        t = raw.get("trial") or {}
        view.update(id=t.get("id"), product=t.get("product"), output_dir=str(out), cells=[], checks=[])
        return view
    t, task = spec["trial"], spec["task"]
    raw_cells = raw.get("cells") or []
    cells = []
    for c in spec["cells"]:
        source = c.get("group") or c["id"]
        index = next((i for i, rc in enumerate(raw_cells) if rc.get("id") == source), None)
        harness = specmod.harness_for(spec, c["harness"])
        cells.append({"id": c["id"], "harness": c["harness"], "model": c.get("model"), "config": c.get("config"),
                      "raw_index": index, "model_editable": index is not None and "models" not in raw_cells[index],
                      "verified_live": bool(harness.verified_live), "installed": _safe_available(harness),
                      "place": _place(spec, c), "signin": _signin(spec, c),
                      "budget_enforced": c["harness"] == "claude-code"})
    rep = spec["reporter"]
    view.update({
        "id": t["id"], "product": t["product"], "output_dir": t["output_dir"], "workspace_root": t["workspace_root"],
        "timeout_seconds": t["timeout_seconds"], "narrate_timeout_seconds": t["narrate_timeout_seconds"],
        "repetitions": t["repetitions"], "parallel": t["parallel"], "max_budget_usd": t.get("max_budget_usd"),
        "materials": task.get("materials") or [], "human": task.get("human"),
        "fixture_dir": _short(task["fixture_dir"]) if task.get("fixture_dir") else None,
        "identity_cmd": task.get("identity_cmd"),
        "checks": [_check_view(c) for c in spec["verify"]],
        "setup": [c.get("name") for c in spec["setup"]], "teardown": [c.get("name") for c in spec["teardown"]],
        "preflight": [c.get("name") for c in spec["preflight"]],
        "cells": cells, "reporter": {"harness": rep.get("harness"), "model": rep.get("model"), "auth": rep.get("auth")},
    })
    return view


def _safe_available(harness):
    try:
        return bool(harness.available())
    except Exception:  # noqa: BLE001
        return False


def _log_tail(trial, name, lines=40):
    if not name:
        return []
    path = state_dir(trial) / "logs" / Path(str(name)).name
    try:
        with open(path, "rb") as fh:
            fh.seek(0, os.SEEK_END)
            fh.seek(max(0, fh.tell() - 16384))
            data = fh.read().decode("utf-8", "replace")
    except OSError:
        return []
    return data.splitlines()[-lines:]


def _last(entries, *kinds):
    return next((e for e in reversed(entries) if e.get("kind") in kinds), None)


def _approval(entries, plan):
    last = _last(entries, "approved")
    if not last:
        return {"state": "none"}
    return {"state": "valid" if last.get("plan") == plan else "stale", "at": last.get("at"), "via": last.get("via"),
            "actor": last.get("actor"), "quote": last.get("quote"), "plan": str(last.get("plan") or "")[:12]}


def _pending_start(entries, ready):
    """A start the user asked for that is still checking readiness. A request nothing followed (the
    server stopped mid-check) stops counting once its check is no longer running."""
    req = _last(entries, "start_requested")
    if not req or any(e["n"] > req["n"] and e.get("kind") in ("start_blocked", "job_started") for e in entries):
        return None
    age = _seconds_since(req.get("at"))
    if ready.get("status") == "checking" or (age is not None and age < 15):
        return {"at": req.get("at"), "actor": req.get("actor")}
    return None


def _process(trial, entries, job, out, ready, parallel=1):
    lock = _lock(out)
    last_job = _last(entries, "job_started")
    view = {"state": "idle", "stale_lock": bool(lock and not lock["alive"]), "parallel": parallel,
            "log": (job or last_job or {}).get("log"), "log_tail": _log_tail(trial, (job or last_job or {}).get("log"))}
    if job:
        stop = _last(entries, "stop_requested")
        stopping = bool(stop and int(stop.get("pid") or 0) == int(job.get("pid") or -1)
                        and stop.get("job_n") == job.get("n"))
        view.update(state="stopping" if stopping else ("running" if job.get("job") in ("run", "stages") else "busy"),
                    job=job.get("job"), pid=job.get("pid"), since=job.get("at"), by=job.get("actor"),
                    command=job.get("command"), outside=bool(job.get("outside")))
    elif ready.get("status") == "checking":
        view["state"] = "checking"
    elif _pending_start(entries, ready):
        view["state"] = "starting"
    return view


def describe(e):
    """One line a person can read, shared by the page and `ajx console since`."""
    k = e.get("kind")
    if k == "approved":
        text = f"Approved plan {str(e.get('plan'))[:7]} " + ("in chat" if e.get("via") == "chat" else "in the console")
        return text + (f": \u201c{e['quote']}\u201d" if e.get("quote") else "")
    if k == "job_started":
        job = e.get("job")
        if job == "run":
            return "Started the run" + (f" ({e['via']})" if e.get("via") == "chat" else "")
        if job == "stages":
            return f"Started: {STAGE_SETS.get(e.get('stages'), e.get('stages'))} for {e.get('run')}"
        return "Rebuilding the reports (no model calls)" if job == "report" else f"Started {job}"
    if k == "job_finished":
        code = e.get("exit_code")
        what = {"run": "The run", "stages": "The recovery command", "report": "Rebuilding reports"}.get(e.get("job"), "A command")
        if code is None:
            return f"{what} ended"
        return f"{what} finished" if code == 0 else (f"{what} was stopped" if code in (130, -2) else f"{what} ended with exit {code}")
    labels = {
        "stop_requested": "Asked the run to stop. AJX cleans up (teardown, release, archive) before it exits.",
        "check_started": "Checking readiness (ajx validate, then ajx doctor)",
        "start_requested": "Asked to check readiness, then start",
        "prompt_edited": "Changed the task prompt",
        "trial_edited": "Edited trial.toml",
        "console_started": "Opened the console",
        "console_stopped": "Closed the console",
    }
    if k in labels:
        return labels[k]
    if k == "checked":
        return "Readiness: " + ("ready" if e.get("status") == "ready" else "not ready")
    if k == "start_blocked":
        return f"Did not start: {e.get('reason')}"
    if k == "settings_changed":
        return "Changed settings: " + ", ".join(e.get("changes") or [])
    if k == "fresh_output":
        return f"Switched to a fresh output folder: {e.get('output_dir')}"
    if k in ("message", "note", "progress"):
        return str(e.get("text") or "")  # the actor says who wrote it
    if k == "exported":
        return f"Exported {e.get('run')} for sharing"
    return str(k)


def _entry_view(e, texts=None):
    view = {"n": e.get("n"), "at": e.get("at"), "actor": e.get("actor"), "kind": e.get("kind"),
            "text": _clip(describe(e), 600), "reply_to": e.get("reply_to")}
    if e.get("reply_to") and texts:
        view["reply_to_text"] = _clip(texts.get(e["reply_to"]), 160) or None
    return view


def snapshot(trial, *, feed_limit=14, skip=()):
    trial = trial_file(trial)
    spec, error = load(trial)
    out = output_dir(trial, spec)
    journal_progress(trial, out)
    entries, _ = read_entries(trial)
    if reconcile(trial, entries, skip=skip):
        entries, _ = read_entries(trial)
    plan = plan_info(trial, spec)
    ready = readiness(trial, plan["sha"])
    job = active_job(trial, entries, out)
    runs = _runs(spec, out, feed_limit)
    output = _output(spec, out, runs)
    process = _process(trial, entries, job, out, ready, int(((spec or {}).get("trial") or {}).get("parallel") or 1))
    started = any(r["has_state"] for r in runs)
    lock_reason = ("Locked while a run or command is in progress." if job else
                   "This output folder already holds attempts of this task. Use a fresh output folder to change "
                   "the task; recorded attempts keep their original prompt." if started else None)
    return {
        "schema": SCHEMA, "generated_at": now(),
        "trial": _trial_view(trial, spec, error, out),
        "plan": {"sha": plan["sha"], "short": plan["sha"][:12], "complete": plan["complete"], "covers": plan["covers"],
                 "approval": _approval(entries, plan["sha"])},
        "readiness": ready,
        "process": process,
        "output": output,
        "runs": runs,
        "journal": {"cursor": entries[-1]["n"] if entries else 0,
                    "entries": [_entry_view(e, {m["n"]: m.get("text") for m in entries if m.get("kind") == "message"})
                                for e in entries[-80:]]},
        "agent": presence(trial),
        "console": _console_view(trial),
        "pending_start": _pending_start(entries, ready),
        "locks": {"prompt": lock_reason, "settings": "Locked while a run or command is in progress." if job else None},
    }


def _console_view(trial):
    info = read_json(_server_path(trial), {}) or {}
    return {"started_at": info.get("started_at"), "pid": info.get("pid")}


# --------------------------------------------------------------------------- agent view

def _headline(snap):
    proc, out, runs = snap["process"], snap["output"], snap["runs"]
    if proc["state"] in ("running", "stopping"):
        active = [f"{r['id']} at {r['current']}" for r in runs if r.get("current")]
        head = ("Stopping the run" if proc["state"] == "stopping" else "A run is in progress") + \
               (": " + ", ".join(active) if active else "")
    elif proc["state"] in ("checking", "starting"):
        head = "Checking readiness" + (" before starting" if proc["state"] == "starting" else "")
    elif proc["state"] == "busy":
        head = "Rebuilding the reports"
    elif out["started"]:
        parts = []
        for r in runs:
            if not r["started"]:
                continue
            verified = (r.get("verify") or {}).get("outcome") or "not verified"
            asks = r["asks"]
            parts.append(f"{r['id']}: {verified}, " + (f"{asks['count']} asks" if asks["status"] == "ready" else f"asks {asks['status']}"))
        head = f"{out['complete']} of {out['planned']} attempts finished" + ("; " + "; ".join(parts) if parts else "")
    elif snap["plan"]["approval"]["state"] == "valid":
        head = "Approved but not started"
    else:
        head = "Not started. The user reviews and starts the trial in the console."
    return head


def since(trial, cursor=0, touch=True):
    """What changed after `cursor`, for an agent catching up before it replies."""
    trial = trial_file(trial)
    snap = snapshot(trial, feed_limit=3)
    entries, _ = read_entries(trial, cursor)
    new_cursor = snap["journal"]["cursor"]
    if touch:
        touch_agent(trial, "seen")
    all_entries, _ = read_entries(trial)
    replied = {e.get("reply_to") for e in all_entries if e.get("kind") == "note"}
    messages = [{"n": e["n"], "at": e.get("at"), "text": e.get("text"), "about": e.get("about")}
                for e in all_entries if e.get("kind") == "message" and e["n"] not in replied]
    where, short = _short(trial.parent), snap["plan"]["short"]
    do_not = [f"approve, start, stop or retry for the user. If they approve in chat, run `ajx console start {where} "
              f"--plan {short} --quote \"<their words>\"`"]
    kind = snap["output"]["blocker_kind"]
    if snap["process"]["state"] in ("running", "stopping", "busy", "starting", "checking"):
        do_not.append("start another run or edit the trial while this one is in progress")
    elif kind == "complete":
        do_not.append("start again: every planned attempt finished. Offer another attempt or a fresh output folder "
                      "(both are buttons on the Plan tab)")
    elif kind == "changed":
        do_not.append("start in this output folder: the task or profiles changed. Offer a fresh output folder")
    if snap["output"]["started"] and kind != "changed":
        do_not.append("edit the task prompt for this output folder; offer a fresh output folder instead")
    info = server_info(trial)
    out = Path(snap["output"]["dir"])
    return {
        "schema": SCHEMA, "trial": str(trial), "since": cursor, "cursor": new_cursor,
        "headline": _headline(snap).rstrip(".") + "." + (f" {len(messages)} message(s) for you." if messages else ""),
        "changes": [{"n": e["n"], "at": e.get("at"), "actor": e.get("actor"), "kind": e.get("kind"),
                     "text": _clip(describe(e), 600)} for e in entries],
        "messages_for_agent": messages,
        "plan": {"sha": short, "complete": snap["plan"]["complete"], "approval": snap["plan"]["approval"]},
        "readiness": {"status": snap["readiness"].get("status"), "current": snap["readiness"].get("current"),
                      "problems": snap["readiness"].get("problems") or []},
        "process": {k: snap["process"].get(k) for k in ("state", "job", "pid", "since", "by", "command")},
        "output": {k: snap["output"][k] for k in ("dir", "started", "planned", "complete", "blocker", "index")},
        "runs": [{"id": r["id"], "stage": r["current"], "complete": r["complete"],
                  "verified": (r.get("verify") or {}).get("outcome"), "cleanup": r["teardown"],
                  "asks": r["asks"].get("count") if r["asks"]["status"] == "ready" else r["asks"]["status"],
                  "review_status": r["review_status"],
                  "files": {k: str(out / v) for k, v in r["links"].items()}} for r in snap["runs"] if r["started"]],
        "console": {"running": info is not None, "url": info["url"] if info else None,
                    "results_url": info["url"] + "&tab=results" if info else None},
        "do_not": do_not,
    }


def wait(trial, cursor=0, timeout=90):
    """Block until the journal gains an entry after `cursor` (an action, or a run milestone such as
    verification or a finished report), or `timeout` seconds pass; then return `since`."""
    trial = trial_file(trial)
    timeout = max(1, min(int(timeout), 3600))
    deadline = time.time() + timeout
    touch_agent(trial, "watching", until=deadline)
    woke = "timeout"
    try:
        while time.time() < deadline:
            spec, _ = load(trial)
            journal_progress(trial, output_dir(trial, spec))
            reconcile(trial, read_entries(trial)[0])
            if read_entries(trial)[1] > cursor:
                woke = "journal"
                break
            time.sleep(1)
    finally:
        touch_agent(trial, "seen")
    result = since(trial, cursor, touch=False)
    result["woke"] = woke
    return result


def note(trial, text, reply_to=None, actor="agent"):
    trial = trial_file(trial)
    text = " ".join(str(text).split())
    if not text:
        raise ConsoleError("a note needs text")
    if reply_to is not None and not any(e["n"] == reply_to and e.get("kind") == "message"
                                        for e in read_entries(trial)[0]):
        raise ConsoleError(f"#{reply_to} is not a message from the user; `ajx console since` lists those under "
                           "messages_for_agent")
    entry = record(trial, actor, "note", text=_clip(text, MAX_TEXT), reply_to=reply_to)
    touch_agent(trial, "seen")
    return entry


# --------------------------------------------------------------------------- actions

def _ensure_idle(trial, entries=None):
    spec, _ = load(trial)
    if entries is None:
        entries, _ = read_entries(trial)
    job = active_job(trial, entries, output_dir(trial, spec))
    if job:
        raise Conflict(f"{job.get('command') or 'a command'} is still running (pid {job.get('pid')})")
    return spec


def preflight_start(trial, plan=None):
    """Checks that need no waiting; the caller then checks readiness and launches. `plan` may be the
    full plan id or a prefix of at least 7 characters (as `since` prints it)."""
    trial = trial_file(trial)
    spec, error = load(trial)
    if error:
        raise ConsoleError(f"the trial file has an error: {error}")
    info = plan_info(trial, spec)
    current = info["sha"]
    given = str(plan or "").strip().lower()
    if plan is not None and (len(given) < 7 or not current.startswith(given)):
        raise Conflict(f"plan mismatch: you passed {given[:12] or 'nothing'}, the current plan is {current[:12]}. "
                       "The trial file, task, starting files or plugins changed after review, or the id is mistyped. "
                       "`ajx console since` shows the current plan and what changed.")
    if not info["complete"]:
        raise ConsoleError("the starting files or the trial's plugins are too large to fingerprint for approval "
                           "(over 2000 files or 64 MB); start this trial with `ajx run` instead")
    _ensure_idle(trial)
    out = output_dir(trial, spec)
    blocked = _output(spec, out, _runs(spec, out, 0))
    if blocked["blocker"]:
        raise ConsoleError(blocked["blocker"])
    return spec, current


def start(trial, *, actor, via, plan=None, quote=None, on_launch=None):
    """Check readiness when it is missing for this exact plan, record approval, launch `ajx run`.
    The run itself refuses to start if the plan changes after approval (--expect-plan)."""
    trial = trial_file(trial)
    try:
        _, current = preflight_start(trial, plan)
        ready = readiness(trial, current)
        if not (ready["current"] and ready.get("status") == "ready"):
            ready = check(trial, actor)
        if ready.get("status") != "ready":
            raise ConsoleError("not ready; the readiness check lists what to fix")
        if plan_sha(trial, load(trial)[0]) != current:
            raise Conflict("the plan changed during the readiness check; review it and start again")
        entries, _ = read_entries(trial)
        if _approval(entries, current)["state"] != "valid":
            record(trial, actor, "approved", plan=current, via=via, quote=_clip(quote, 500) if quote else None)
        proc, entry = launch(trial, "run", ["run", str(trial), "--expect-plan", current], actor,
                             plan=current[:12], via=via)
    except ConsoleError as exc:
        record(trial, actor, "start_blocked", reason=_clip(str(exc), 300))
        raise
    if on_launch:
        on_launch(proc, entry)
    return entry


def stop(trial, actor):
    trial = trial_file(trial)
    spec, _ = load(trial)
    entries, _ = read_entries(trial)
    job = active_job(trial, entries, output_dir(trial, spec))
    if not job or job.get("job") not in ("run", "stages"):
        raise ConsoleError("no run is in progress")
    pid = int(job["pid"])
    last = _last(entries, "stop_requested")
    if last and int(last.get("pid") or 0) == pid and last.get("job_n") == job.get("n"):
        raise Conflict("already stopping: AJX is stopping the worker and cleaning up, and a second signal could cut "
                       "that cleanup short")
    if job.get("job") == "run" and int(((spec or {}).get("trial") or {}).get("parallel") or 1) > 1:
        raise ConsoleError("Stop is unavailable when trial.parallel is above 1: AJX would stop only after every "
                           "running attempt ends, which can take until their time limits")
    # SIGINT takes the runner's handled interruption: stop the worker, tear down, release, archive.
    try:
        os.kill(pid, signal.SIGINT)
    except ProcessLookupError:
        raise ConsoleError("the run already ended") from None
    record(trial, actor, "stop_requested", pid=pid, job_n=job.get("n"))
    return {"pid": pid}


def run_stages(trial, actor, run_id, stages, on_launch=None):
    trial = trial_file(trial)
    if stages not in STAGE_SETS:
        raise ConsoleError(f"unsupported stage set {stages!r}")
    spec = _ensure_idle(trial)
    out = output_dir(trial, spec)
    if not re.fullmatch(r"[a-z0-9][a-z0-9._-]*-r\d+", str(run_id)) or not (out / "runs" / run_id / "state.json").is_file():
        raise ConsoleError(f"run {run_id!r} not found")
    cell = re.sub(r"-r\d+$", "", run_id)
    proc, entry = launch(trial, "stages", ["run", str(trial), "--cells", cell, "--stages", stages], actor,
                         run=run_id, stages=stages)
    if on_launch:
        on_launch(proc, entry)
    return entry


def rebuild_reports(trial, actor, on_launch=None):
    trial = trial_file(trial)
    _ensure_idle(trial)
    proc, entry = launch(trial, "report", ["report", str(trial), "--no-synthesis"], actor)
    if on_launch:
        on_launch(proc, entry)
    return entry


def export(trial, actor, run_id):
    trial = trial_file(trial)
    spec, _ = load(trial)
    out = output_dir(trial, spec)
    if not re.fullmatch(r"[a-z0-9][a-z0-9._-]*-r\d+", str(run_id)):
        raise ConsoleError("export needs a run id")
    res = _run_cli(["export", str(trial), "--run", run_id, "--force"], 120, trial.parent)
    if res["exit_code"] != 0:
        raise ConsoleError(res["output"].strip() or "export failed")
    dest = out / "share" / run_id
    record(trial, actor, "exported", run=run_id)
    return {"output": res["output"], "dest": _short(dest),
            "files": [f"share/{run_id}/{n}" for n in ("report.html", "journey.html", "asks.md") if (dest / n).is_file()]}


def message(trial, text, about=None):
    trial = trial_file(trial)
    text = str(text or "").strip()
    if not text:
        raise ConsoleError("write a message first")
    return record(trial, "user", "message", text=_clip(text, MAX_TEXT), about=_clip(about, 120) if about else None)


# --------------------------------------------------------------------------- editing trial files

_HEADER = re.compile(r"^\s*(\[\[?)([^\[\]]+?)(\]\]?)\s*(#.*)?$")


def _sections(lines):
    """[(name, is_array, header_index, end_index)]; the root section has name None. Quoted or
    dotted headers end a section but never match a plain table name."""
    out, current = [], (None, False, -1)
    for i, line in enumerate(lines):
        m = _HEADER.match(line)
        if m and (m.group(1) == "[[") == (m.group(3) == "]]"):
            out.append((*current, i))
            current = (m.group(2).strip(), m.group(1) == "[[", i)
    out.append((*current, len(lines)))
    return out


def _toml_value(value):
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, (int, float)):
        return repr(value)
    return json.dumps(str(value), ensure_ascii=False)


def _trailing_comment(line, key):
    for i, ch in enumerate(line):
        if ch == "#":
            try:
                if key in tomllib.loads(line[:i]):
                    return "  " + line[i:].strip()
            except tomllib.TOMLDecodeError:
                continue
    return ""


def set_value(text, table, key, value, index=0):
    """Set (or with value None, remove) `key` in `[table]`, or in the index-th `[[cells]]`, keeping
    the rest of the file as written. The result is re-parsed and compared before it is used."""
    lines = text.split("\n")
    sections = [s for s in _sections(lines) if s[0] == table and s[1] == (table == "cells")]
    rendered = None if value is None else f"{key} = {_toml_value(value)}"
    if table == "cells" and index >= len(sections):
        raise ConsoleError(f"cells[{index}] not found")
    section = sections[index if table == "cells" else 0] if sections else None
    if section:
        start, end = section[2] + 1, section[3]
        live = re.compile(rf"^\s*{re.escape(key)}\s*=")
        commented = re.compile(rf"^\s*#\s*{re.escape(key)}\s*=")
        hit = next((i for i in range(start, end) if live.match(lines[i])), None)
        if hit is not None:
            if rendered is None:
                del lines[hit]
            else:
                lines[hit] = rendered + _trailing_comment(lines[hit], key)
        elif rendered is not None:
            hint = next((i for i in range(start, end) if commented.match(lines[i])), None)
            if hint is not None:
                lines[hint] = rendered  # replace the scaffold's commented-out example in place
            else:
                at = end
                while at > start and (not lines[at - 1].strip() or lines[at - 1].lstrip().startswith("#")):
                    at -= 1
                lines.insert(at, rendered)
    elif rendered is not None:
        while lines and lines[-1] == "":
            lines.pop()
        lines += ["", f"[{table}]", rendered, ""]
    new = "\n".join(lines)
    try:
        parsed = tomllib.loads(new)
    except tomllib.TOMLDecodeError as exc:
        raise ConsoleError(f"could not update {table}.{key} safely ({exc}); edit trial.toml directly") from None
    holder = (parsed.get("cells") or [{}])[index] if table == "cells" else (parsed.get(table) or {})
    if (value is None and key in holder) or (value is not None and holder.get(key) != value):
        raise ConsoleError(f"could not update {table}.{key} safely; edit trial.toml directly")
    return new


def _write_trial(trial, text, base_sha, actor, kind, **fields):
    current = Path(trial).read_bytes()
    if base_sha and base_sha != sha256_bytes(current):
        raise Conflict("trial.toml changed on disk (by your agent or an editor). Reload to see the new version.")
    try:
        tomllib.loads(text)
    except tomllib.TOMLDecodeError as exc:
        raise ConsoleError(f"not valid TOML: {exc}") from None
    tmp = Path(trial).with_name(f".{Path(trial).name}.console-{os.getpid()}.tmp")
    tmp.write_text(text, encoding="utf-8")
    try:
        specmod.load(tmp, readiness=False)
    except Exception as exc:  # noqa: BLE001 - reject the edit, keep the file as it was
        tmp.unlink(missing_ok=True)
        raise ConsoleError(f"not saved: {exc}") from None
    os.replace(tmp, trial)
    new_sha = sha256_bytes(text.encode("utf-8"))
    record(trial, actor, kind, before=sha256_bytes(current)[:12], after=new_sha[:12], **fields)
    return new_sha


def save_trial_text(trial, text, base_sha, actor="user"):
    trial = trial_file(trial)
    _ensure_idle(trial)
    return {"sha256": _write_trial(trial, str(text).replace("\r\n", "\n"), base_sha, actor, "trial_edited")}


def save_settings(trial, changes, base_sha, actor="user"):
    """Edit the few values the page exposes. `changes` keys: timeout_minutes, repetitions,
    max_budget_usd, reporter_model, cell_models ({raw cell index: model or ""})."""
    trial = trial_file(trial)
    _ensure_idle(trial)
    text, notes = Path(trial).read_text(encoding="utf-8"), []
    if "timeout_minutes" in changes:
        minutes = _number(changes["timeout_minutes"], "time limit", 1, 24 * 60)
        text = set_value(text, "trial", "timeout_seconds", int(round(minutes * 60)))
        notes.append(f"time limit {minutes:g} min")
    if "repetitions" in changes:
        reps = _number(changes["repetitions"], "attempts", 1, 50)
        if reps != int(reps):
            raise ConsoleError("attempts must be a whole number")
        reps = int(reps)
        text = set_value(text, "trial", "repetitions", reps)
        notes.append(f"attempts {reps}")
    if "max_budget_usd" in changes:
        raw = changes["max_budget_usd"]
        budget = None if raw in (None, "") else _number(raw, "budget", 0.01, 10000)
        text = set_value(text, "trial", "max_budget_usd", budget)
        notes.append("budget " + (f"${budget:g}" if budget is not None else "removed"))
    if "reporter_model" in changes:
        model = str(changes["reporter_model"] or "").strip() or None
        text = set_value(text, "reporter", "model", model)
        notes.append(f"reporter model {model or 'default'}")
    for index, model in (changes.get("cell_models") or {}).items():
        model = str(model or "").strip() or None
        text = set_value(text, "cells", "model", model, index=int(index))
        notes.append(f"cell {int(index) + 1} model {model or 'default'}")
    if not notes:
        raise ConsoleError("nothing to change")
    return {"sha256": _write_trial(trial, text, base_sha, actor, "settings_changed", changes=notes)}


def _number(value, label, low, high):
    try:
        number = float(value)
    except (TypeError, ValueError):
        raise ConsoleError(f"{label} must be a number") from None
    if not low <= number <= high:
        raise ConsoleError(f"{label} must be between {low:g} and {high:g}")
    return number


def fresh_output(trial, base_sha, actor="user"):
    trial = trial_file(trial)
    spec = _ensure_idle(trial)
    current = output_dir(trial, spec)
    name = re.sub(r"-\d{8}-\d{6}$", "", current.name) + datetime.now().strftime("-%Y%m%d-%H%M%S")
    target = current.parent / name
    try:
        value = os.path.relpath(target, trial.parent)
    except ValueError:
        value = str(target)
    text = set_value(Path(trial).read_text(encoding="utf-8"), "trial", "output_dir", value)
    return {"sha256": _write_trial(trial, text, base_sha, actor, "fresh_output", output_dir=value), "output_dir": value}


def add_attempt(trial, base_sha, actor="user"):
    spec, error = load(trial_file(trial))
    if error:
        raise ConsoleError(error)
    return save_settings(trial, {"repetitions": spec["trial"]["repetitions"] + 1}, base_sha, actor)


def save_prompt(trial, text, base_sha, actor="user"):
    trial = trial_file(trial)
    spec, _ = load(trial)
    path = prompt_path(trial, spec)
    if not path:
        raise ConsoleError("task.prompt_file is not set in trial.toml")
    snap_lock = snapshot(trial, feed_limit=0)["locks"]["prompt"]
    if snap_lock:
        raise ConsoleError(snap_lock)
    current = path.read_bytes() if path.is_file() else b""
    if base_sha != sha256_bytes(current):
        raise Conflict("the task prompt changed on disk (by your agent or an editor). Reload to see it.")
    text = str(text).replace("\r\n", "\n")
    if not text.strip():
        raise ConsoleError("the task prompt cannot be empty")
    text = text if text.endswith("\n") else text + "\n"
    tmp = path.with_name(f".{path.name}.console-{os.getpid()}.tmp")
    tmp.write_text(text, encoding="utf-8")
    os.replace(tmp, path)
    new = sha256_bytes(text.encode("utf-8"))
    record(trial, actor, "prompt_edited", before=sha256_bytes(current)[:12], after=new[:12])
    return {"sha256": new}


# --------------------------------------------------------------------------- server

class App:
    def __init__(self, trial, token, idle):
        self.trial, self.token, self.idle = trial, token, idle
        self.port = None
        self.children = {}
        self.last_request = time.time()
        self.lock = threading.Lock()
        self.checking = threading.Event()

    def watch(self, proc, entry):
        self.children[proc.pid] = proc

        def reap():
            code = proc.wait()
            finish_job(self.trial, entry["n"], proc.pid, code, entry.get("job"))
            self.children.pop(proc.pid, None)
        threading.Thread(target=reap, daemon=True).start()

    def busy(self):
        return any(p.poll() is None for p in list(self.children.values())) or self.checking.is_set()

    def background(self, fn):
        def run():
            try:
                fn()
            except ConsoleError:
                pass  # already recorded in the journal for the page
            except Exception as exc:  # noqa: BLE001
                record(self.trial, "console", "start_blocked", reason=f"{type(exc).__name__}: {_clip(exc, 200)}")
        threading.Thread(target=run, daemon=True).start()


def _handler(app):
    class Handler(http.server.BaseHTTPRequestHandler):
        server_version = "AJXConsole/1"  # HTTP/1.0: one request per connection, no unread-body reuse

        def log_message(self, fmt, *args):  # quiet; the journal records actions
            pass

        def do_GET(self):
            self._dispatch("GET")

        def do_POST(self):
            self._dispatch("POST")

        def _dispatch(self, method):
            app.last_request = time.time()
            try:
                if not self._host_ok():
                    return self._json(403, {"error": "unexpected Host header"})
                path = urllib.parse.urlsplit(self.path).path
                if method == "GET" and path in ("/", "/index.html"):
                    return self._static(ASSETS / "console.html", "text/html; charset=utf-8", csp=True)
                if method == "GET" and path.startswith("/static/") and path[8:] in STATIC:
                    return self._static(ASSETS / path[8:], STATIC[path[8:]])
                if method == "POST" and path == "/api/session":
                    return self._session()
                if not self._authorized(method):
                    return self._json(403, {"error": "this page needs the link printed by `ajx console` (it carries a key)"})
                if method == "GET" and path.startswith("/files/"):
                    return self._file(path[len("/files/"):])
                if method == "GET" and path.startswith("/logs/"):
                    return self._log(path[len("/logs/"):])
                body = self._body() if method == "POST" else {}
                result = self._route(method, path, body)
                if result is None:
                    return self._json(404, {"error": "not found"})
                status, data = result
                return self._json(status, data)
            except ConsoleError as exc:
                return self._json(exc.status, {"error": str(exc)})
            except (BrokenPipeError, ConnectionResetError):
                return None
            except Exception as exc:  # noqa: BLE001 - report, keep serving
                return self._json(500, {"error": f"{type(exc).__name__}: {exc}"})

        def _route(self, method, path, body):
            t = app.trial
            if method == "GET":
                if path == "/api/ping":
                    return 200, {"ok": True, "trial": str(t), "pid": os.getpid()}
                if path == "/api/state":
                    return 200, snapshot(t, skip=set(app.children))
                if path == "/api/trial-text":
                    text = Path(t).read_text(encoding="utf-8", errors="replace")
                    return 200, {"text": text, "sha256": sha256_bytes(Path(t).read_bytes())}
                return None
            if path == "/api/check":
                if app.checking.is_set():
                    raise Conflict("a readiness check is already running")
                _ensure_idle(t)
                app.checking.set()

                def run_check():
                    try:
                        check(t, "user")
                    finally:
                        app.checking.clear()
                app.background(run_check)
                return 202, {"ok": True}
            if path == "/api/start":
                with app.lock:
                    if app.busy():
                        raise Conflict("a readiness check or command is already running")
                    preflight_start(t, body.get("plan"))
                    record(t, "user", "start_requested", plan=str(body.get("plan") or "")[:12])
                    app.checking.set()

                def run_start():
                    try:
                        start(t, actor="user", via="console", plan=body.get("plan"), on_launch=app.watch)
                    finally:
                        app.checking.clear()
                app.background(run_start)
                return 202, {"ok": True}
            if path == "/api/stop":
                return 200, stop(t, "user")
            if path == "/api/stages":
                return 200, {"ok": True, "entry": run_stages(t, "user", body.get("run"), body.get("stages"), app.watch)}
            if path == "/api/report":
                return 200, {"ok": True, "entry": rebuild_reports(t, "user", app.watch)}
            if path == "/api/export":
                return 200, export(t, "user", body.get("run"))
            if path == "/api/prompt":
                return 200, save_prompt(t, body.get("text", ""), body.get("base"))
            if path == "/api/settings":
                return 200, save_settings(t, body.get("changes") or {}, body.get("base"))
            if path == "/api/trial-text":
                return 200, save_trial_text(t, body.get("text", ""), body.get("base"))
            if path == "/api/fresh-output":
                return 200, fresh_output(t, body.get("base"))
            if path == "/api/add-attempt":
                return 200, add_attempt(t, body.get("base"))
            if path == "/api/message":
                return 200, {"entry": message(t, body.get("text"), body.get("about"))}
            return None

        # -- helpers
        def _host_ok(self):
            host = (self.headers.get("Host") or "").lower()
            return host in (f"127.0.0.1:{app.port}", f"localhost:{app.port}")

        def _cookie(self):
            for part in (self.headers.get("Cookie") or "").split(";"):
                name, _, value = part.strip().partition("=")
                if name == f"ajx_console_{app.port}":
                    return value
            return ""

        def _authorized(self, method):
            header = self.headers.get("X-AJX-Token") or ""
            if hmac.compare_digest(header.encode(), app.token.encode()):
                return True
            # Opened links (reports, logs) carry the session cookie; actions always need the header.
            return method == "GET" and hmac.compare_digest(self._cookie().encode(), app.token.encode())

        def _session(self):
            header = self.headers.get("X-AJX-Token") or ""
            if not hmac.compare_digest(header.encode(), app.token.encode()):
                return self._json(403, {"error": "this page needs the link printed by `ajx console` (it carries a key)"})
            self._json(200, {"ok": True}, cookie=f"ajx_console_{app.port}={app.token}; HttpOnly; SameSite=Lax; Path=/")

        def _body(self):
            if "application/json" not in (self.headers.get("Content-Type") or ""):
                raise ConsoleError("expected a JSON body")
            try:
                length = int(self.headers.get("Content-Length") or 0)
            except ValueError:
                raise ConsoleError("malformed Content-Length") from None
            if not 0 <= length <= 2_000_000:
                raise ConsoleError("request too large or malformed")
            try:
                data = json.loads(self.rfile.read(length) or b"{}")
            except ValueError:
                raise ConsoleError("the request body is not valid JSON") from None
            if not isinstance(data, dict):
                raise ConsoleError("expected a JSON object")
            return data

        def _send(self, status, data, ctype, extra=None):
            self.send_response(status)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(data)))
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Content-Type-Options", "nosniff")
            self.send_header("Referrer-Policy", "no-referrer")
            for k, v in (extra or {}).items():
                self.send_header(k, v)
            self.end_headers()
            self.wfile.write(data)

        def _json(self, status, payload, cookie=None):
            extra = {"Set-Cookie": cookie} if cookie else None
            self._send(status, json.dumps(payload, ensure_ascii=False).encode("utf-8"),
                       "application/json; charset=utf-8", extra)

        def _static(self, path, ctype, csp=False):
            self._send(200, Path(path).read_bytes(), ctype, {"Content-Security-Policy": PAGE_CSP} if csp else None)

        def _file(self, rel):
            spec, _ = load(app.trial)
            root = output_dir(app.trial, spec).resolve()
            target = (root / urllib.parse.unquote(rel)).resolve()
            if not target.is_relative_to(root) or not target.is_file() or target.suffix not in CONTENT_TYPES:
                return self._json(404, {"error": "not found"})
            # Reports quote worker output: run them as an opaque origin, without the console's cookie.
            extra = {"Content-Security-Policy": REPORT_CSP} if target.suffix in (".html", ".svg") else None
            self._send(200, target.read_bytes(), CONTENT_TYPES[target.suffix], extra)

        def _log(self, name):
            target = state_dir(app.trial) / "logs" / Path(urllib.parse.unquote(name)).name
            if not target.is_file():
                return self._json(404, {"error": "not found"})
            self._send(200, target.read_bytes(), "text/plain; charset=utf-8")

    return Handler


def _server_path(trial):
    return state_dir(trial) / "server.json"


def _ping(info, timeout=2):
    req = urllib.request.Request(f"http://127.0.0.1:{info['port']}/api/ping", headers={"X-AJX-Token": info["token"]})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as res:
            return json.loads(res.read()).get("ok") is True
    except (OSError, ValueError):
        return False


def server_info(trial):
    try:
        trial = trial_file(trial)
    except ConsoleError:
        return None
    info = read_json(_server_path(trial), None)
    if not isinstance(info, dict) or not _alive(info.get("pid")) or not info.get("port"):
        return None
    return info if _ping(info) else None


def make_server(trial, port=0, idle=IDLE_SECONDS):
    """(httpd, app, info) bound to 127.0.0.1; `serve` runs it until shutdown or idle."""
    trial = trial_file(trial)
    app = App(trial, secrets.token_urlsafe(24), idle)
    httpd = http.server.ThreadingHTTPServer(("127.0.0.1", int(port)), _handler(app))
    httpd.daemon_threads = True
    app.port = httpd.server_address[1]
    url = f"http://127.0.0.1:{app.port}/#token={app.token}"
    info = {"pid": os.getpid(), "port": app.port, "token": app.token, "url": url, "started_at": now(),
            "trial": str(trial)}
    _write_private(_server_path(trial), info)
    record(trial, "console", "console_started", port=app.port)
    return httpd, app, info


def serve(trial, port=0, open_browser=False, idle=IDLE_SECONDS, announce=print):
    trial = trial_file(trial)
    httpd, app, info = make_server(trial, port, idle)
    url = info["url"]

    def shutdown(*_):
        threading.Thread(target=httpd.shutdown, daemon=True).start()

    if threading.current_thread() is threading.main_thread():
        signal.signal(signal.SIGTERM, shutdown)
        signal.signal(signal.SIGHUP, shutdown)

    def watchdog():
        while True:
            time.sleep(15)
            if time.time() - app.last_request > app.idle and not app.busy():
                shutdown()
                return
    threading.Thread(target=watchdog, daemon=True).start()
    announce(url)
    if open_browser:
        webbrowser.open(url)
    try:
        httpd.serve_forever(poll_interval=0.5)
    except KeyboardInterrupt:
        pass
    finally:
        httpd.server_close()
        if (read_json(_server_path(trial), {}) or {}).get("pid") == os.getpid():
            _server_path(trial).unlink(missing_ok=True)
        record(trial, "console", "console_stopped")
    return 0


_SPAWNED = {}  # servers this process started, so it can reap them when it stops one


def open_console(trial, *, port=0, open_browser=True, timeout=15):
    """Reuse this trial's console or start one in the background; return its server info."""
    trial = trial_file(trial)
    info = server_info(trial)
    if not info:
        _server_path(trial).unlink(missing_ok=True)
        log = state_dir(trial) / "server.log"
        with open(log, "ab") as fh:
            proc = subprocess.Popen([sys.executable, str(AJX_BIN), "console", "serve", str(trial), "--port", str(port)],
                                    cwd=str(trial.parent), stdin=subprocess.DEVNULL, stdout=fh, stderr=subprocess.STDOUT,
                                    start_new_session=True, env=_job_env())
        _SPAWNED[proc.pid] = proc
        deadline = time.time() + timeout
        while True:
            info = read_json(_server_path(trial), None)
            if isinstance(info, dict) and info.get("pid") == proc.pid and _ping(info):
                info["started"] = True
                break
            if proc.poll() is not None or time.time() > deadline:
                tail = log.read_text(encoding="utf-8", errors="replace").splitlines()[-8:] if log.exists() else []
                raise ConsoleError("the console did not start" + (":\n" + "\n".join(tail) if tail else ""))
            time.sleep(0.1)
    if open_browser:
        webbrowser.open(info["url"])
    return info


def stop_console(trial, timeout=5):
    trial = trial_file(trial)
    info = read_json(_server_path(trial), None)
    if not isinstance(info, dict) or not _alive(info.get("pid")):
        _server_path(trial).unlink(missing_ok=True)
        return False
    os.kill(int(info["pid"]), signal.SIGTERM)
    own = _SPAWNED.pop(int(info["pid"]), None)
    if own:
        own.wait(timeout)
        return True
    deadline = time.time() + timeout
    while time.time() < deadline and _alive(info["pid"]):
        time.sleep(0.1)
    return True


def copy_demo(dest, force=False):
    dest = Path(dest).expanduser().resolve()
    if dest.exists() and any(dest.iterdir()) and not force:
        raise ConsoleError(f"{dest} is not empty; choose a new folder or pass --force")
    shutil.copytree(DEMO_DIR, dest, dirs_exist_ok=True,
                    ignore=shutil.ignore_patterns("__pycache__", "*.pyc", "ajx-reports", STATE_DIR))
    return dest / "trial.toml"
