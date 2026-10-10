"""Export one run as a folder a product team can open: report.html, journey.html, asks.md.

The copies differ from the run directory in two ways.

Paths. The worker's directories recorded in state.json "paths" become neutral labels (workspace
-> /workspace, home_dir -> /agent-home, config_dir -> /agent-home/.config/harness, cache_dir ->
/agent-home/.cache), including the '-'-encoded spellings some harnesses use for project directories
(-srv-people-me-work-8b41708955) and the archived copies under the run directory. Other attempt
directories under the same workspace root (<root>/<hex>) become /agent-home/attempt, the bare root
becomes /agent-home/attempts, and the coordinator's home directory becomes ~. Every rule is applied in one pass, longest match first, so a
directory nested in another gets its own label and inserted labels are never rewritten.

Links. Links to files the bundle does not carry (run.json, verify.json, environment.json,
agent-configuration.json, ../../index.html) become their text; list items that only carry such a
link are dropped; links to pretty-journey.html follow its rename to journey.html; the brand link
loses its href.

Text-based and standard-library only. The result is a starting point for review, not a redaction:
the agent's commands and outputs, account identifiers and URLs stay as the reports recorded them.
"""

import os
import re
from pathlib import Path

from .util import read_json

FILES = (("report.html", "report.html"), ("pretty-journey.html", "journey.html"), ("asks.md", "asks.md"))
LABELS = {"workspace": "/workspace", "home_dir": "/agent-home",
          "config_dir": "/agent-home/.config/harness", "cache_dir": "/agent-home/.cache"}
ATTEMPT_LABEL = "/agent-home/attempt"
ROOT_LABEL = "/agent-home/attempts"   # the workspace root itself, when it appears without an attempt below it
HOME_LABEL = "~"
# Files the bundle does not carry: links to them become their text.
OUTSIDE = ("run.json", "verify.json", "environment.json", "agent-configuration.json", "../../index.html")

_HEX_DIR = r"[0-9a-f]{6,}"
# A path match may not continue into a longer name (/srv/people/me must not match /srv/people/meredith or
# /srv/people/me.bak) but may be followed by a sentence-ending period, a slash, a quote or whitespace.
_RAW_END = r"(?![\w-]|\.\w)"
# Encoded spellings continue with '-' (-srv-people-me-work-8b41708955-sub), so only letters and digits end them.
_ENCODED_END = r"(?!\w|\.\w)"

_OUTSIDE_HREF = "(?:" + "|".join(re.escape(name) for name in OUTSIDE) + r')(?:#[^"]*)?'
_BRAND = re.compile(r'<a class="brand" href="[^"]*">')
_LI_OUTSIDE = re.compile(r'<li\b[^>]*>(?:(?!</li>|<li\b).)*?<a\b[^>]*\bhref="' + _OUTSIDE_HREF
                         + r'"(?:(?!</li>|<li\b).)*?</li>', re.S)
_A_OUTSIDE = re.compile(r'<a\b[^>]*\bhref="' + _OUTSIDE_HREF + r'"[^>]*>(.*?)</a>', re.S)
_JOURNEY_HREF = re.compile(r'href="pretty-journey\.html')
_MD_OUTSIDE = "(?:" + "|".join(re.escape(name) for name in OUTSIDE) + r")(?:#[^)]*)?"
_MD_LI_OUTSIDE = re.compile(r"^[ \t]*[-*+][ \t]+\[[^\]]*\]\(" + _MD_OUTSIDE + r"\)[ \t]*\n?", re.M)
_MD_LINK_OUTSIDE = re.compile(r"\[([^\]]*)\]\(" + _MD_OUTSIDE + r"\)")
_MD_JOURNEY = re.compile(r"\]\(pretty-journey\.html")


class ExportError(Exception):
    """A run cannot be exported; the message says why and names the path."""


def encoded_forms(path):
    """How a harness may spell a directory as a project-directory name: '/' -> '-' (and, for Claude
    Code, every other non-alphanumeric character too). Distinct forms only."""
    forms = [path.replace("/", "-"), re.sub(r"[^A-Za-z0-9]", "-", path)]
    return list(dict.fromkeys(forms))


def _paired_forms(path, label):
    """Each encoded spelling of `path` with the matching spelling of `label` (the slash-only form
    first; the fully dashed form when the path has other punctuation)."""
    label_forms = encoded_forms(label)
    return [(form, label_forms[min(i, len(label_forms) - 1)]) for i, form in enumerate(encoded_forms(path))]


def replacement_rules(paths, home, archived=None):
    """(pattern, replacement, summary key) triples, in priority order for a single-pass rewrite:
    literal directories longest first, then sibling attempt directories under the workspace root, then
    anything shorter than that root (typically the coordinator's home)."""
    literals = []  # (literal, replacement, key)
    for source in (paths or {}, archived or {}):
        for key, label in LABELS.items():
            value = source.get(key) if isinstance(source, dict) else None
            if not isinstance(value, str) or not value.strip("/"):
                continue
            literals.append((value, label, label))
            for form, labelled in _paired_forms(value, label):
                literals.append((form, labelled, label))
    workspace = (paths or {}).get("workspace") if isinstance(paths, dict) else None
    root = str(Path(workspace).parent) if isinstance(workspace, str) and workspace.strip("/") else ""
    attempts = []
    if root.strip("/"):
        attempts.append((re.escape(root) + "/" + _HEX_DIR + _RAW_END, ATTEMPT_LABEL, ATTEMPT_LABEL))
        for form, labelled in _paired_forms(root, ATTEMPT_LABEL):
            attempts.append((re.escape(form) + "-" + _HEX_DIR + _ENCODED_END, labelled, ATTEMPT_LABEL))
        # The bare root (a truncated attempt name, a mount point in an error) names the coordinator's layout.
        attempts.append((re.escape(root) + _RAW_END, ROOT_LABEL, ROOT_LABEL))
        for form, labelled in _paired_forms(root, ROOT_LABEL):
            attempts.append((re.escape(form) + _ENCODED_END, labelled, ROOT_LABEL))
    homes = []
    if home:
        home = str(Path(home).expanduser())
        resolved = str(Path(home).resolve())
        homes = [h for h in dict.fromkeys((home, resolved)) if h.strip("/")]
    for h in homes:
        literals.append((h, HOME_LABEL, HOME_LABEL))
        for form in encoded_forms(h):
            literals.append((form, HOME_LABEL, HOME_LABEL))
    literals = list(dict.fromkeys(literals))
    literals.sort(key=lambda item: len(item[0]), reverse=True)

    def rule(literal, replacement, key):
        end = _RAW_END if literal.startswith("/") else _ENCODED_END
        return (re.escape(literal) + end, replacement, key)

    longer = [rule(*item) for item in literals if len(item[0]) > len(root)]
    shorter = [rule(*item) for item in literals if len(item[0]) <= len(root)]
    return longer + attempts + shorter


def rewrite_paths(text, rules, counts=None):
    """Apply every rule in one pass (earlier rules win at the same position); count hits per key."""
    if not rules:
        return text
    counts = counts if counts is not None else {}
    for _, _, key in rules:
        counts.setdefault(key, 0)
    pattern = re.compile("|".join(f"({src})" for src, _, _ in rules))

    def sub(match):
        _, replacement, key = rules[match.lastindex - 1]
        counts[key] += 1
        return replacement
    return pattern.sub(sub, text)


def rewrite_html_links(text):
    """Brand link without href; list items and links that lead outside the bundle reduced to text;
    pretty-journey.html links renamed."""
    text = _BRAND.sub('<a class="brand">', text)
    text = _LI_OUTSIDE.sub("", text)
    text = _A_OUTSIDE.sub(r"\1", text)
    return _JOURNEY_HREF.sub('href="journey.html', text)


def rewrite_markdown_links(text):
    """The Markdown counterpart: bullet lines that only carry an outside link are dropped, other
    outside links become their text, pretty-journey.html links renamed."""
    text = _MD_LI_OUTSIDE.sub("", text)
    text = _MD_LINK_OUTSIDE.sub(r"\1", text)
    return _MD_JOURNEY.sub("](journey.html", text)


def export_run(run_dir, dest, *, home=None):
    """Write report.html, journey.html and asks.md for one run into `dest` (created with parents).

    Returns {"files": [...], "replacements": {label: count}, "dest": str, "warnings": [...]}.
    Raises ExportError when the run directory or one of the three source files is missing; nothing is
    written in that case.
    """
    run_dir, dest = Path(run_dir), Path(dest)
    if not run_dir.is_dir():
        raise ExportError(f"run directory not found: {run_dir}")
    missing = [src for src, _ in FILES if not (run_dir / src).is_file()]
    if missing:
        raise ExportError(f"{run_dir.name}: cannot export without {', '.join(missing)} in {run_dir} "
                          "(render the run first: ajx run <trial> --stages render)")
    warnings, paths = [], None
    try:
        state = read_json(run_dir / "state.json", None)
    except ValueError:
        state = None
        warnings.append("state.json is unreadable; the worker's directories were not relabeled")
    if not isinstance(state, dict):
        if not warnings:
            warnings.append("state.json is missing; the worker's directories were not relabeled")
        state = {}
    elif isinstance(state.get("paths"), dict) and state["paths"]:
        paths = state["paths"]
    else:
        warnings.append("state.json records no worker directories (paths); only the home directory was relabeled")
    rules = replacement_rules(paths, Path.home() if home is None else home, state.get("archived"))
    counts = {}
    outputs = []
    for src, dst in FILES:
        try:
            text = (run_dir / src).read_text(encoding="utf-8")
        except UnicodeDecodeError as exc:
            raise ExportError(f"{run_dir / src} is not UTF-8 text: {exc}") from exc
        text = rewrite_paths(text, rules, counts)
        text = rewrite_markdown_links(text) if dst.endswith(".md") else rewrite_html_links(text)
        outputs.append((dst, text))
    dest.mkdir(parents=True, exist_ok=True)
    for dst, text in outputs:
        target = dest / dst
        target.write_text(text, encoding="utf-8")
        os.chmod(target, 0o644)
    return {"files": [dst for dst, _ in outputs], "replacements": counts, "dest": str(dest), "warnings": warnings}
