"""Trial file (trial.toml) loading, validation and plugin resolution."""

import random
import re
import shlex
import tomllib
from pathlib import Path

from . import plugins
from .util import sha256_bytes, sha256_file

CONFIGS = ("clean", "user")
_ID = re.compile(r"^[a-z0-9][a-z0-9._-]{0,47}$")
_NOT_ID = re.compile(r"[^a-z0-9._-]+")


class SpecError(ValueError):
    pass


def _need(cond, msg):
    if not cond:
        raise SpecError(msg)


def load(path):
    path = Path(path).resolve()
    _need(path.exists(), f"trial file not found: {path}")
    raw = tomllib.loads(path.read_text(encoding="utf-8"))
    base = path.parent
    plugins.load(base)

    trial = dict(raw.get("trial") or {})
    _need(_ID.match(str(trial.get("id", ""))), "trial.id must be a short lowercase slug")
    _need(trial.get("product"), "trial.product is required")
    defaults = {"product_version_cmd": None, "workspace_root": "/tmp", "repetitions": 1, "parallel": 1,
                "timeout_seconds": 1800, "max_budget_usd": None, "order_seed": 0, "cache": "isolated",
                "narrate_timeout_seconds": 900}
    for k, v in defaults.items():
        trial.setdefault(k, v)
    trial.setdefault("output_dir", str(base / "ajx-reports" / trial["product"] / trial["id"]))
    _need(trial["cache"] in ("isolated", "shared"), "trial.cache must be isolated|shared")
    for key in ("repetitions", "parallel", "timeout_seconds", "narrate_timeout_seconds", "order_seed"):
        try:
            trial[key] = int(trial[key])
        except (TypeError, ValueError):
            raise SpecError(f"trial.{key} must be an integer") from None
    _need(trial["repetitions"] >= 1, "trial.repetitions must be >= 1")
    _need(trial["parallel"] >= 1, "trial.parallel must be >= 1")
    _need(trial["timeout_seconds"] >= 1 and trial["narrate_timeout_seconds"] >= 1, "timeouts must be >= 1 second")
    trial["output_dir"] = str((base / trial["output_dir"]).resolve())
    trial["workspace_root"] = str(Path(trial["workspace_root"]).expanduser().resolve())

    task = dict(raw.get("task") or {})
    _need(task.get("prompt_file"), "task.prompt_file is required")
    prompt_path = (base / task["prompt_file"]).resolve()
    _need(prompt_path.exists(), f"task prompt not found: {prompt_path}")
    prompt_bytes = prompt_path.read_bytes()
    _need(prompt_bytes.strip(), "task prompt is empty")
    task.update(prompt_path=str(prompt_path), prompt_sha256=sha256_bytes(prompt_bytes),
                prompt_text=prompt_bytes.decode("utf-8"))
    task.setdefault("materials", [])
    task.setdefault("human", "unavailable")
    # The account the worker acts in, checked by doctor in the worker's own tool shell.
    ident = task.get("identity_cmd")
    if isinstance(ident, list):
        _need(ident and all(isinstance(a, str) for a in ident), "task.identity_cmd list must hold strings")
        ident = shlex.join(ident)
    _need(ident is None or (isinstance(ident, str) and ident.strip()),
          "task.identity_cmd must be a shell command string or an argv list")
    task["identity_cmd"] = ident
    if task.get("fixture_dir"):
        fixture = (base / task["fixture_dir"]).resolve()
        _need(fixture.is_dir(), f"fixture_dir not found: {fixture}")
        task["fixture_dir"] = str(fixture)
    else:
        task["fixture_dir"] = None

    def commands(key):
        out = []
        for i, item in enumerate(raw.get(key) or []):
            _need(isinstance(item, dict), f"{key}[{i}] must be a table")
            entry = dict(item)
            entry.setdefault("type", "shell")
            try:
                entry["timeout"] = int(item.get("timeout", 600))
            except (TypeError, ValueError):
                raise SpecError(f"{key}[{i}].timeout must be an integer") from None
            if key != "verify":
                _need(entry["type"] == "shell", f"{key}[{i}].type must be shell (only [[verify]] has other check types)")
            if entry["type"] == "shell":
                _need(item.get("run"), f"{key}[{i}].run is required for type=shell")
            # credential-error scanning opt-out, for a command that expects access to be denied
            entry["allow_auth_errors"] = bool(item.get("allow_auth_errors", False))
            entry.setdefault("name", item.get("run", entry["type"])[:60] if item.get("run") else entry["type"])
            if key == "verify":
                _need(entry["type"] in plugins.names("check"),
                      f"verify[{i}].type {entry['type']!r} unknown; available {plugins.names('check')}")
            out.append(entry)
        return out

    adapters = {str(k): dict(v) for k, v in (raw.get("adapters") or {}).items()}
    for name, conf in adapters.items():
        _need(isinstance(conf.get("execute"), list) and conf["execute"],
              f"adapters.{name}.execute must be a non-empty argv list")
    auth_profiles = {str(k): dict(v) for k, v in (raw.get("auth") or {}).items()}
    for name, conf in auth_profiles.items():
        _need(conf.get("type") in plugins.names("auth"),
              f"auth.{name}.type must be one of {plugins.names('auth')}")
    runner = dict(raw.get("runner") or {"type": "local"})

    reporter = dict(raw.get("reporter") or {})
    reporter.setdefault("harness", "claude-code")
    reporter.setdefault("model", None)
    reporter.setdefault("effort", None)
    reporter.setdefault("auth", None)
    _need(reporter["harness"] == "claude-code",
          "reporter.harness must be claude-code (structured JSON output is required for extraction)")

    spec = {
        "path": str(path), "sha256": sha256_file(path), "trial": trial, "task": task,
        "setup": commands("setup"), "verify": commands("verify"), "teardown": commands("teardown"),
        "preflight": commands("preflight"),
        "reporter": reporter, "env": {str(k): str(v) for k, v in (raw.get("env") or {}).items()},
        "adapters": adapters, "auth": auth_profiles, "runner": runner, "cells": [],
    }

    seen = set()
    for i, cell in enumerate(raw.get("cells") or []):
        cell = dict(cell)
        _need(_ID.match(str(cell.get("id", ""))), f"cells[{i}].id must be a short lowercase slug")
        _need(cell.get("harness") in harness_names(spec),
              f"cells[{i}].harness {cell.get('harness')!r} unknown; available {harness_names(spec)}")
        for k, v in {"model": None, "effort": None, "config": "clean", "args": [], "env": {},
                     "auth": None, "runner": None}.items():
            cell.setdefault(k, v)
        _need(cell["config"] in CONFIGS, f"cells[{i}].config must be clean|user")
        _need(isinstance(cell["args"], list), f"cells[{i}].args must be a list")
        for one in _expand_models(cell, i):
            _need(one["id"] not in seen, f"duplicate cell id {one['id']}")
            seen.add(one["id"])
            spec["cells"].append(one)
    _need(spec["cells"], "at least one [[cells]] entry is required")
    for cell in spec["cells"]:
        auth_for(spec, cell)  # validates
    auth_for(spec, {"id": "reporter", "harness": reporter["harness"], "auth": reporter.get("auth")})
    return spec


def _expand_models(cell, i):
    """`models = ["sonnet", "opus"]` is one cell per model, ids `<id>-<model>`, grouped under `<id>`."""
    models = cell.pop("models", None)
    if models is None:
        return [cell]
    _need(isinstance(models, list) and models and all(isinstance(m, str) and m.strip() for m in models),
          f"cells[{i}].models must be a non-empty list of model names")
    _need(not cell.get("model"), f"cells[{i}]: set model or models, not both")
    _need(len(set(models)) == len(models), f"cells[{i}].models lists a model twice")
    out = []
    for model in models:
        cid = f"{cell['id']}-{_NOT_ID.sub('-', model.lower()).strip('-.')}"
        _need(_ID.match(cid), f"cells[{i}]: cell id {cid!r} derived from model {model!r} is not a short "
                              "lowercase slug; shorten the id or write a separate [[cells]] entry for this model")
        out.append({**cell, "id": cid, "model": model, "group": cell["id"]})
    return out


def harness_names(spec):
    return sorted(set(plugins.names("harness")) | set(spec.get("adapters") or {}))


def worker_env(spec, cell, auth):
    """Variables ajx sets for a worker: trial [env], then the cell's env, then the auth profile's."""
    env = dict(spec["env"])
    env.update({k: str(v) for k, v in (cell.get("env") or {}).items()})
    env.update(auth.env())
    return env


def harness_for(spec, name):
    if name in (spec.get("adapters") or {}):
        from .builtin.generic import Declarative
        return Declarative(name, spec["adapters"][name])
    return plugins.get("harness", name)()


def auth_for(spec, cell, override=None):
    """Cell auth may name a trial [auth.<profile>] or an auth plugin type directly."""
    harness = harness_for(spec, cell["harness"])
    ref = override or cell.get("auth") or harness.default_auth
    profile = (spec.get("auth") or {}).get(ref)
    if profile:
        cls, conf = plugins.get("auth", profile["type"]), profile
    else:
        _need(ref in plugins.names("auth"), f"cell {cell['id']}: auth {ref!r} is neither a [auth.*] profile "
              f"nor an auth type {plugins.names('auth')}")
        cls, conf = plugins.get("auth", ref), {}
    _need("*" in cls.harnesses or cell["harness"] in cls.harnesses or cell["harness"] in (spec.get("adapters") or {}),
          f"cell {cell['id']}: auth type {cls.plugin_name!r} is for {cls.harnesses}, not {cell['harness']}")
    auth = cls(conf)
    auth.profile = ref
    auth.harness = cell["harness"]
    return auth


def runner_for(spec, cell):
    conf = cell.get("runner") or spec.get("runner") or {"type": "local"}
    if isinstance(conf, str):
        conf = {"type": conf}
    return plugins.get("runner", conf.get("type", "local"))(conf)


def run_plan(spec, only_cells=None):
    """Interleaved, seeded order: every cell once for rep 1 (shuffled), then rep 2, ...
    `only_cells` may name a cell or the id of a `models = [...]` group."""
    known = {c["id"] for c in spec["cells"]} | {c["group"] for c in spec["cells"] if c.get("group")}
    unknown = [c for c in (only_cells or []) if c not in known]
    _need(not unknown, f"unknown cell id(s) {unknown}; cells are {sorted(known)}")
    cells = [c for c in spec["cells"] if not only_cells or c["id"] in only_cells or c.get("group") in only_cells]
    rng = random.Random(int(spec["trial"]["order_seed"]))
    plan = []
    for rep in range(1, int(spec["trial"]["repetitions"]) + 1):
        order = list(cells)
        rng.shuffle(order)
        plan += [(c, rep) for c in order]
    return plan
