"""Plugin registry. Four extension points, one mechanism:

  harness  - how to launch, resume (for narration) and parse an agent CLI     -> base.Harness
  auth     - provider/credential profile for a harness (env, config, checks)  -> base.Auth
  runner   - where a worker executes (local, docker, custom wrapper)          -> base.Runner
  check    - deterministic verification step types                           -> base.Check

Register with the decorator:

    from ajx.plugins import register
    from ajx.base import Harness

    @register("harness", "my-agent")
    class MyAgent(Harness): ...

Discovery order (later wins on name clash): built-ins (ajx/builtin/*.py), the skill's
plugins/ folder, ~/.config/ajx/plugins/, and <trial dir>/plugins/. Trial files can also
declare harnesses ([adapters.<name>]) and auth profiles ([auth.<name>]) without code.
"""

import importlib
import importlib.util
import pkgutil
import sys
from pathlib import Path

KINDS = ("harness", "auth", "runner", "check")
_REGISTRY = {kind: {} for kind in KINDS}
_SOURCES = {kind: {} for kind in KINDS}
_loaded_dirs = set()
_builtins_loaded = False


def register(kind, name):
    if kind not in KINDS:
        raise ValueError(f"unknown plugin kind {kind!r}; expected one of {KINDS}")

    def deco(obj):
        obj.plugin_name = name
        _REGISTRY[kind][name] = obj
        _SOURCES[kind][name] = getattr(sys.modules.get(obj.__module__), "__file__", obj.__module__)
        return obj
    return deco


def _load_builtins():
    global _builtins_loaded
    if _builtins_loaded:
        return
    from . import builtin
    for mod in pkgutil.iter_modules(builtin.__path__):
        importlib.import_module(f"{builtin.__name__}.{mod.name}")
    _builtins_loaded = True


def plugin_dirs(trial_dir=None):
    from .util import SKILL_ROOT
    dirs = [SKILL_ROOT / "plugins", Path.home() / ".config" / "ajx" / "plugins"]
    if trial_dir:
        dirs.append(Path(trial_dir) / "plugins")
    return dirs


def load(trial_dir=None):
    _load_builtins()
    for d in plugin_dirs(trial_dir):
        d = Path(d).resolve()
        if d in _loaded_dirs or not d.is_dir():
            continue
        _loaded_dirs.add(d)
        for path in sorted(d.glob("*.py")):
            spec = importlib.util.spec_from_file_location(f"ajx_plugin_{path.stem}", path)
            mod = importlib.util.module_from_spec(spec)
            sys.modules[spec.name] = mod
            spec.loader.exec_module(mod)


def get(kind, name):
    _load_builtins()
    try:
        return _REGISTRY[kind][name]
    except KeyError:
        raise KeyError(f"no {kind} plugin named {name!r}; available: {sorted(_REGISTRY[kind])}") from None


def names(kind):
    _load_builtins()
    return sorted(_REGISTRY[kind])


def source(kind, name):
    return _SOURCES[kind].get(name)
