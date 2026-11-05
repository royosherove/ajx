"""Harness model suggestions shared with the native CLI, not an access allowlist."""

import json

from .util import SKILL_ROOT

CATALOG = json.loads((SKILL_ROOT / "assets" / "model-catalog.json").read_text(encoding="utf-8"))["harnesses"]
KNOWN_MODELS = {name: entry["models"] for name, entry in CATALOG.items()}


class InvalidModelName(ValueError):
    """A value does not have the supported CLI model-name shape."""

    def __init__(self, name):
        self.name = name
        super().__init__(name)


def parse_name(value, harness=""):
    """Normalize and resolve a CLI model name, returning None for the default."""
    name = str(value or "").strip()
    if not name:
        return None
    if any(ch.isspace() for ch in name) or name.startswith("-") or len(name) > 200:
        raise InvalidModelName(name)
    return resolve(harness, name)


def hint(harness):
    return CATALOG.get(harness, {}).get(
        "hint", "Use a model ID accepted by this agent, or leave it empty for the agent's default.")


def resolve(harness, model):
    model = str(model or "").strip()
    entry = CATALOG.get(harness)
    if not entry or not model:
        return model
    key = model.lower()
    if key in entry["aliases"]:
        return entry["aliases"][key]
    if any(key in other["aliases"] for other in CATALOG.values()):
        raise ValueError(
            f'"{model}" is not a model alias for {harness}. Choose a model for {harness}, '
            "enter its exact model ID, or leave it empty for the agent's default")
    return model
