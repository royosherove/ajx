"""Offline HTML views of the same asks, narrative, and recorded event evidence."""

import html
import json
import re
from urllib.parse import quote

from . import __version__, mdlite
from .util import SKILL_ROOT, now, seconds_between

# Same vocabulary as references/vocabulary.md. Labels are observations, not severity scores.
LABELS = {
    "Wall": ("🧱", "bad"), "Roadblock": ("🚧", "bad"), "Detour": ("↩️", "warn"),
    "Wait": ("⏳", "warn"), "Dead End": ("🚫", "bad"), "Fork": ("🔀", "warn"),
    "Wrong Turn": ("❓", "warn"), "Misdirection": ("🔴", "bad"), "Blind Spot": ("👁️", "warn"),
    "Gate": ("🚪", "warn"), "Trust Decision": ("🧭", "info"), "Cost": ("💸", "info"),
    "Delight": ("✅", "good"),
}
STATUSES = {
    "verified_defect": ("Verified defect", "bad"), "observed_friction": ("Observed friction", "warn"),
    "untested_risk": ("Untested risk", "info"), "feature_request": ("Feature request", "info"),
    "succeeded": ("Verified success", "good"), "partial": ("Partially verified", "warn"),
    "failed": ("Verification failed", "bad"), "not_run": ("Not verified", "warn"),
}
EVENT_ID = re.compile(r"^E-\d{3,}$")


def esc(value):
    return html.escape(str(value if value is not None else ""), quote=True)


def number(value):
    if value is None:
        return "Unavailable"
    if isinstance(value, (int, float)):
        return f"{value:,.6g}" if isinstance(value, float) else f"{value:,}"
    return str(value)


def duration(value):
    if value is None:
        return "Unavailable"
    return f"{int(value // 60)}m {int(value % 60):02d}s" if value >= 60 else f"{value:g}s"


def badge(text, tone=""):
    return f'<span class="badge {esc(tone)}">{esc(text)}</span>'


def status(value):
    text, tone = STATUSES.get(value, (value or "Unknown", ""))
    return badge(text, tone)


def labels(names):
    return "".join(badge(f"{LABELS[n][0]} {n}", LABELS[n][1]) for n in names if n in LABELS)


def event_links(refs, prefix="pretty-journey.html"):
    return " ".join(f'<a class="mono" href="{esc(prefix)}#{ref}">{ref}</a>'
                    for ref in dict.fromkeys(refs or []) if isinstance(ref, str) and EVENT_ID.fullmatch(ref))


def page(title, body, subtitle="", view="report", example=False):
    css = (SKILL_ROOT / "assets" / "report.css").read_text(encoding="utf-8")
    js = (SKILL_ROOT / "assets" / "report.js").read_text(encoding="utf-8")
    navigation = ([("index.html", "Compare runs", "matrix"), ("matrix.md", "Markdown", "markdown"),
                   ("matrix.json", "Data", "data")] if view == "matrix" else
                  [("report.html", "Product asks", "report"), ("pretty-journey.html", "Agent journey", "journey"),
                   ("../../index.html", "Compare runs", "matrix"), ("asks.md", "Markdown asks", "markdown"),
                   ("run.json", "Run data", "data")])
    nav = "".join(f'<a href="{href}"' + (' aria-current="page"' if key == view else "") + f'>{label}</a>'
                  for href, label, key in navigation)
    demo = ('<div class="example"><strong>Fictional example.</strong> All events, versions, costs, and '
            'outcomes below are synthetic. This is a report demonstration, not a product or agent benchmark.</div>') if example else ""
    generated = "Synthetic example" if example else "Generated " + now()
    return f"""<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'">
<title>{esc(title)}</title><style>{css}</style></head><body>
<a class="skip" href="#content">Skip to report</a>
<aside class="rail"><a class="brand" href="{'index.html' if view == 'matrix' else '../../index.html'}">AJX<small>Agent Journey Experience</small></a>
<nav aria-label="Report views">{nav}</nav><p class="rail-note">Follow an ask to its evidence.<br>Keep measured cost and modeled savings separate.</p></aside>
<main id="content"><header class="page-head"><h1>{esc(title)}</h1><p class="subtitle">{esc(subtitle)}</p></header>
{demo}{body}
<footer class="page-footer">AJX {__version__}. {esc(generated)}. Recorded costs describe the observed run.
Provider prices are estimates; modeled savings are hypotheses. Missing measurements remain unavailable.
This report opens offline and makes no network requests.</footer></main><script>{js}</script></body></html>"""


def summary_strip(run, summary):
    items = [
        ("Task outcome", status(run.get("task_outcome_verified")), run.get("verification_scope") or "No verification recorded"),
        ("Product version", esc(run.get("product", {}).get("version") or "Unknown"), run.get("product", {}).get("name")),
        ("Task wall clock", esc(duration(summary.get("elapsed_seconds"))), "Execution window only"),
        ("Output tokens", esc(number(summary.get("output_tokens"))), summary.get("usage_status") or "Coverage unavailable"),
    ]
    return '<dl class="summary-strip">' + "".join(
        f"<div><dt>{esc(k)}</dt><dd>{v}<small>{esc(note)}</small></dd></div>" for k, v, note in items) + "</dl>"


def _record(value):
    return value if isinstance(value, dict) else {}


def _value(value):
    if value is None or value == "":
        return "unknown"
    if isinstance(value, (dict, list, bool)):
        return json.dumps(value, ensure_ascii=False, sort_keys=True)
    return str(value)


def _profile_name(record):
    return record.get("profile_id") or record.get("id") or record.get("name")


def _profile_label(record):
    actual, declared = _profile_name(record), record.get("declared_profile_id")
    if actual:
        return f"{actual} (declared: {declared})" if declared and actual != declared else str(actual)
    declared = declared or _profile_name(_record(record.get("plan")))
    return f"{declared} (declared only)" if declared else "unknown"


def extension_entries(configuration, kind):
    """Accept named selections and resolved entries without inferring their states."""
    section = configuration.get(kind)
    if isinstance(section, dict):
        entries = next((section[key] for key in ("entries", "items", "selected", "enabled")
                        if isinstance(section.get(key), (list, dict))), [])
    else:
        entries = section if isinstance(section, list) else []
    if isinstance(entries, dict):
        return [dict(_record(value), name=name) for name, value in entries.items()]
    return [entry if isinstance(entry, dict) else {"name": str(entry)} for entry in entries]


def extension_summary(configuration, kind):
    section = _record(configuration.get(kind))
    mode = section.get("mode", configuration.get(kind + "_mode"))
    selected = next((section[key] for key in ("selected", "enabled")
                     if isinstance(section.get(key), list)), None)
    entries = selected if selected is not None else extension_entries(configuration, kind)
    names = [_value(entry.get("name") or entry.get("id")) if isinstance(entry, dict) else str(entry)
             for entry in entries]
    return _value(mode) + ("; " + ", ".join(names) if names else
                          ("; selection unknown" if mode in ("selected", "snapshot") else ""))


def cleanup_status(cleanup):
    return _value(cleanup.get("status")) if isinstance(cleanup, dict) else _value(cleanup)


def cleanup_problem(cleanup):
    record = _record(cleanup)
    failed = record.get("failed")
    return (cleanup_status(cleanup).lower() in (
        "failed", "error", "auth_errors", "skipped", "not_run", "not run", "timeout", "timed_out", "partial")
        or record.get("ok") is False or record.get("confirmed") is False
        or bool(record.get("errors")) or (isinstance(failed, (int, float)) and failed > 0))


def configuration_values(run):
    environment = _record(run.get("execution_environment"))
    configuration = _record(run.get("agent_configuration"))
    backend = environment.get("backend")
    if backend is not None:
        backend = f"{_value(backend)} (reported)"
    elif _record(environment.get("plan")).get("backend") is not None:
        backend = f"{_value(environment['plan']['backend'])} (declared only)"
    elif _record(run.get("runner")).get("type"):
        backend = f"{run['runner']['type']} (legacy runner; capabilities unknown)"
    return [
        ("Environment profile", _profile_label(environment)),
        ("Backend", backend or "unknown"),
        ("Agent profile", _profile_label(configuration)),
        ("Skills mode / selection (declared)", extension_summary(configuration, "skills")),
        ("Plugins mode / selection (declared)", extension_summary(configuration, "plugins")),
        ("Hooks mode / selection (declared)", extension_summary(configuration, "hooks")),
        ("Environment cleanup", cleanup_status(run.get("environment_cleanup"))),
    ]


def profile_limitations(run):
    notes = []
    environment = _record(run.get("execution_environment"))
    configuration = _record(run.get("agent_configuration"))
    sources = [("execution environment", environment),
               ("environment plan", _record(environment.get("plan"))),
               ("agent configuration", configuration),
               ("environment cleanup", _record(run.get("environment_cleanup")))]
    sources += [(kind, _record(configuration.get(kind))) for kind in ("skills", "plugins", "hooks")]
    for label, record in sources:
        limitations = record.get("limitations")
        for note in limitations if isinstance(limitations, list) else [limitations] if limitations else []:
            notes.append(f"{label}: {_value(note)}")
    if environment and not environment.get("capabilities"):
        notes.append("execution environment: capability coverage unknown")
    return list(dict.fromkeys(notes))


def profile_links(run, prefix=""):
    artifacts = _record(run.get("artifacts"))
    # Only known local artifacts are linkable; manifest strings never become URLs.
    return " ".join(f'<a href="{esc(prefix + filename)}">{label}</a>' for key, filename, label in (
        ("environment", "environment.json", "Environment evidence"),
        ("agent_configuration", "agent-configuration.json", "Agent configuration manifest"))
        if artifacts.get(key) == filename)


def configuration_overview(run):
    values = configuration_values(run)
    text = " · ".join(f"{label}: {value}" for label, value in values[:4])
    return f'<p class="small">{esc(text)}. <a href="#configuration">Configuration and coverage</a></p>'


def capability_rows(record, scope):
    capabilities = record.get("capabilities")
    if isinstance(capabilities, dict):
        items = capabilities.items()
    elif isinstance(capabilities, list):
        items = [(entry.get("name") or entry.get("id") or "unnamed", entry)
                 if isinstance(entry, dict) else (str(entry), None) for entry in capabilities]
    else:
        return []
    rows = []
    for name, value in items:
        entry = _record(value)
        enforcement = entry.get("enforcement", entry.get("state", entry.get("status")))
        if not entry and isinstance(value, str):
            enforcement = value
        if enforcement not in ("enforced", "observed-only", "observed_only", "unsupported", "unknown", "declared"):
            enforcement = "unknown"
        rows.append((f"{scope}: {name}", _value(entry.get("declared")),
                     str(enforcement).replace("_", "-"), _value(entry.get("observed", entry.get("observations"))),
                     _value(value)))
    return rows


EXTENSION_HEADINGS = ("Category / entry", "Version / content hash", "Installed", "Discoverable", "Enabled",
                      "Loaded into context", "Invoked / executed")


def extension_state_rows(configuration):
    rows = []
    for kind in ("skills", "plugins", "hooks", "uncontrolled_sources"):
        for entry in extension_entries(configuration, kind):
            states = _record(entry.get("states"))

            def state_value(*keys):
                for key in keys:
                    if key in states:
                        return _value(states[key])
                    if key in entry:
                        return _value(entry[key])
                return "unknown"

            identity = _value(entry.get("name") or entry.get("id"))
            revision = entry.get("content_hash") or entry.get("content_sha256") or entry.get("sha256") or entry.get("hash")
            rows.append([f"{kind.replace('_', ' ')}: {identity}", f"{_value(entry.get('version'))} / {_value(revision)}",
                         state_value("installed"), state_value("discoverable"), state_value("enabled"),
                         state_value("loaded_into_context", "loaded"), state_value("invoked", "executed")])
    return rows


def profile_coverage(run):
    rows = capability_rows(_record(run.get("execution_environment")), "Environment")
    rows += capability_rows(_record(run.get("agent_configuration")), "Agent adapter")
    descriptions = []
    for name, declared, enforcement, observed, detail in rows:
        description = f"{name}: declared: {declared}; enforcement: {enforcement}; observed: {observed}"
        if declared == enforcement == observed == "unknown" and detail != "unknown":
            description = f"{name}: reported: {detail}; enforcement: unknown; observed: unknown"
        descriptions.append(description)
    return "; ".join(descriptions) or "Capability coverage unknown"


def configuration_section(run):
    environment = _record(run.get("execution_environment"))
    configuration = _record(run.get("agent_configuration"))
    values = configuration_values(run)
    out = ['<section class="section" id="configuration"><h2>Execution environment and agent configuration</h2>',
           '<div class="panel"><dl class="detail-grid">' + "".join(
               f'<div><dt>{esc(label)}</dt><dd>{esc(value)}</dd></div>' for label, value in values) + "</dl>",
           '<p class="small">Declared settings express intent. Enforced describes a reported control; observed '
           'describes recorded checks or telemetry. Missing evidence remains unknown. '
           'A private HOME or isolated harness configuration is not a filesystem sandbox.</p>',
           f'<div class="links">{profile_links(run)}<a href="run.json">Complete run evidence</a></div></div>']
    capabilities = capability_rows(environment, "Environment") + capability_rows(configuration, "Agent adapter")
    out.append('<h3>Capability coverage</h3>')
    if capabilities:
        headings = ("Capability", "Declared", "Enforcement", "Observed", "Recorded detail / scope")
        out.append('<div class="table-wrap"><table><thead><tr>' + "".join(
            f'<th scope="col">{heading}</th>' for heading in headings) + "</tr></thead><tbody>")
        out += ["<tr>" + "".join(f"<td>{esc(value)}</td>" for value in row) + "</tr>" for row in capabilities]
        out.append("</tbody></table></div>")
    else:
        out.append('<p class="muted">Capability coverage unknown; no capability report was recorded.</p>')
    out.append('<h3>Extension states</h3><p class="small">A selection or enabled state does not prove use. '
               'Installed, discoverable, and enabled are recorded configuration states. Loaded into context and '
               'invoked / executed require their own telemetry; missing states remain unknown, not false.</p>')
    states = extension_state_rows(configuration)
    if states:
        out.append('<div class="table-wrap"><table><thead><tr>' + "".join(
            f'<th scope="col">{heading}</th>' for heading in EXTENSION_HEADINGS) + "</tr></thead><tbody>")
        out += ["<tr>" + "".join(f"<td>{esc(value)}</td>" for value in row) + "</tr>" for row in states]
        out.append("</tbody></table></div>")
    else:
        out.append('<p class="muted">No extension entries recorded. Loaded / invoked telemetry is unknown.</p>')
    notes = profile_limitations(run)
    if notes:
        out.append('<h3>Profile limitations</h3><ul>' + "".join(f"<li>{esc(note)}</li>" for note in notes) + "</ul>")
    cleanup = run.get("environment_cleanup")
    out.append('<h3>Environment cleanup</h3>' + badge(cleanup_status(cleanup), "bad" if cleanup_problem(cleanup) else ""))
    if cleanup is not None:
        out.append(f'<pre>{esc(json.dumps(cleanup, indent=2, ensure_ascii=False))}</pre>')
    out.append('<p class="small">Environment release is separate from task teardown and task verification.</p></section>')
    return "".join(out)


def environment_cleanup_notice(run, prefix=""):
    if not cleanup_problem(run.get("environment_cleanup")):
        return ""
    return ('<div class="notice bad"><strong>Environment cleanup not confirmed.</strong> '
            f'{esc(cleanup_status(run.get("environment_cleanup")))}. Owned resources may still exist. '
            f'<a href="{esc(prefix)}run.json">Inspect environment cleanup</a>.</div>')


def review_notice(run, data):
    out = []
    if run.get("review_status") == "incomplete":
        out.append('<div class="notice bad"><strong>Review incomplete.</strong> The journey or extraction is missing. '
                   'The number of asks cannot establish a clean result.</div>')
    if data.get("extraction_status") != "ok":
        out.append(f'<div class="notice bad"><strong>Extraction did not complete.</strong> '
                   f'{esc(data.get("extraction_status") or "No extraction recorded")}. '
                   'Regenerate extraction before using this register to prioritize fixes.</div>')
    if (run.get("teardown") or {}).get("status") in ("failed", "auth_errors", "skipped"):
        out.append('<div class="notice bad"><strong>Cleanup not confirmed.</strong> Resources may still exist. '
                   'Inspect this run’s <a href="teardown.json">teardown record</a>.</div>')
    return "".join(out) + environment_cleanup_notice(run)


def empty_register(data):
    if data.get("extraction_status") != "ok":
        text = "No usable ask register. Extraction is incomplete; this is not a clean result."
    elif data.get("no_obstacles_observed"):
        text = "No obstacles observed in this task. The evidence supports an empty register for this run."
    else:
        text = "No asks returned, but no-obstacles-observed was not confirmed. Review the extraction."
    return f'<div class="empty">{esc(text)}</div>'


def ask_card(ask, index, prefix="", compact=False):
    measured = ask.get("measured") or {}
    aid = str(ask.get("id") or f"ASK-{index + 1:03d}")
    fragment = quote(aid, safe="")
    target = f"{prefix}report.html#{fragment}" if prefix else f"#{fragment}"
    refs = event_links(ask.get("event_refs"), prefix + "pretty-journey.html")
    tags = f'<div class="badges">{status(ask.get("evidence_status"))}{labels(ask.get("labels") or [])}</div>'
    search_kinds = "|".join([ask.get("evidence_status") or "", *(ask.get("labels") or [])])
    attrs = (f'data-item data-order="{index}" data-kinds="{esc(search_kinds)}" '
             f'data-span="{esc(measured.get("span_seconds"))}" data-tokens="{esc(measured.get("output_tokens"))}" '
             f'data-calls="{esc(measured.get("tool_calls"))}" data-failed="{esc(measured.get("failed_tool_calls"))}"')
    title = f'<a href="{esc(target)}">{esc(ask.get("title") or "Untitled ask")}</a>'
    if compact:
        return f"""<article class="ask" {attrs}>
<h4 class="ask-title"><span class="mono">{esc(aid)}</span>{title}</h4>{tags}
<p class="requested">{esc(ask.get("requested_behavior"))}</p>
<p class="compact-cost">{esc(duration(measured.get("span_seconds")))} span · {esc(number(measured.get("tool_calls")))} calls · {esc(number(measured.get("output_tokens")))} output tokens</p>
<div class="links">{refs}</div></article>"""
    facts = [("Linked-event span", duration(measured.get("span_seconds"))),
             ("Tool calls / failed", f'{number(measured.get("tool_calls"))} / {number(measured.get("failed_tool_calls"))}'),
             ("Output tokens", number(measured.get("output_tokens")))]
    costs = '<dl class="ask-cost">' + "".join(f"<div><dt>{esc(k)}</dt><dd>{esc(v)}</dd></div>" for k, v in facts) + "</dl>"
    saving = ask.get("modeled_saving")
    model = (f"{saving.get('claim')}\nComparator: {saving.get('comparator')}\n"
             f"Assumption: {saving.get('assumption')}\nUncertainty: {saving.get('uncertainty')}") if saving else "Not modeled."
    detail = [
        ("Observation", ask.get("how_observed")), ("Consequence", ask.get("consequence")),
        ("Verify the change", ask.get("verification")), ("Workaround", ask.get("workaround") or "None recorded."),
        ("Measurement basis", measured.get("basis") or "No measurement basis recorded."),
        ("Modeled saving", model),
    ]
    details = '<dl class="detail-grid">' + "".join(f"<div><dt>{esc(k)}</dt><dd>{esc(v)}</dd></div>" for k, v in detail) + "</dl>"
    shared = event_links(measured.get("shared_events"))
    if shared:
        details += f'<p class="notice warn">Costs overlap with another ask at {shared}. Do not add them as independent savings.</p>'
    if ask.get("invalid_refs"):
        details += f'<p class="notice warn">Unsupported references removed: {esc(", ".join(ask["invalid_refs"]))}.</p>'
    return f"""<article class="ask" id="{esc(aid)}" {attrs}><div class="ask-main">
<div class="ask-rank">{index + 1:02d}<small>{esc(aid)}</small></div><div><h3 class="ask-title">{title}</h3>{tags}
<p class="requested">{esc(ask.get("requested_behavior"))}</p>
<p class="rationale">{esc(ask.get("priority_rationale"))}</p><div class="links">{refs or "No valid event reference"}<a href="#{fragment}-detail">Inspect evidence and verification</a></div></div>{costs}</div>
<details class="ask-details" id="{fragment}-detail"><summary>Observation, verification, and measurement basis</summary>{details}</details></article>"""


def toolbar(noun, kinds=(), sorts=(), compare=False):
    kinds_html = ('<label>Show<select data-kind><option value="">All</option>' + "".join(
        f'<option value="{esc(value)}">{esc(label)}</option>' for value, label in kinds) + "</select></label>") if kinds else ""
    sorts_html = ('<label>Order<select data-sort><option value="order">Original order</option>' + "".join(
        f'<option value="{esc(value)}">{esc(label)}</option>' for value, label in sorts) + "</select></label>") if sorts else ""
    compare_html = '<button class="primary" type="button" data-compare>Compare selected</button>' if compare else ""
    return (f'<div class="toolbar js-only"><label>Find {esc(noun)}<input type="search" data-query placeholder="Search this report"></label>'
            f'{kinds_html}{sorts_html}{compare_html}<button type="button" data-reset>Reset</button>'
            '<span class="filter-count" data-count aria-live="polite"></span></div>')


def asks_section(run, data):
    asks = data.get("asks") or []
    controls = toolbar("asks", [(k, v[0]) for k, v in STATUSES.items() if k in (
        "verified_defect", "observed_friction", "untested_risk", "feature_request")],
        [("span", "Longest linked span"), ("tokens", "Most output tokens"),
         ("calls", "Most tool calls"), ("failed", "Most failed calls")])
    return ('<section class="section" id="asks" data-filter-scope="asks"><div class="section-heading">'
            f'<h2>Product asks</h2><p>{len(asks)} requested change{"s" if len(asks) != 1 else ""}</p></div>'
            '<p class="section-intro">Ordered by the reporter’s priority rationale. Costs belong to the linked events; '
            'overlapping costs are not additive.</p>' + (controls if asks else "")
            + '<div class="asks" data-items>' + "".join(ask_card(a, i) for i, a in enumerate(asks)) + "</div>"
            + ('<p class="empty" data-empty hidden>No asks match. Reset the filters to see the complete register.</p>' if asks else empty_register(data))
            + "</section>")


def annotations(markdown, data, events):
    """Attach only cited labels. Narrative steps take precedence over broader ask labels."""
    valid = {event["eid"] for event in events}
    result = {eid: {"labels": [], "sources": [], "asks": []} for eid in valid}

    def add(refs, names, source):
        for eid in refs:
            if eid in result:
                result[eid]["labels"] = list(dict.fromkeys(result[eid]["labels"] + names))
                if source not in result[eid]["sources"]:
                    result[eid]["sources"].append(source)

    in_chronology, blocks, block = False, [], []
    for line in markdown.splitlines():
        if line.startswith("## "):
            if block:
                blocks.append("\n".join(block)); block = []
            in_chronology = "chronolog" in line.lower()
        elif in_chronology:
            if re.match(r"^\s*\d+[.)]\s+", line) and block:
                blocks.append("\n".join(block)); block = []
            if block or re.match(r"^\s*\d+[.)]\s+", line):
                block.append(line)
    if block:
        blocks.append("\n".join(block))
    for block in blocks:
        first = re.sub(r"^\s*\d+[.)]\s+", "", block).lstrip("* ")
        names = [name for name, (icon, _) in LABELS.items()
                 if re.match(rf"(?:{re.escape(icon)}\s*)?(?:\*\*)?{re.escape(name)}\b", first)]
        add(re.findall(r"\bE-\d{3,}\b", block), names, "Chronological account")
    narrated = {eid for eid, info in result.items() if info["labels"]}
    for ask in data.get("asks") or []:
        refs = [ref for ref in ask.get("event_refs") or [] if ref in valid]
        for eid in refs:
            result[eid]["asks"].append(ask.get("id", "Ask"))
        add([eid for eid in refs if eid not in narrated],
            [name for name in ask.get("labels") or [] if name in LABELS], f"Ask {ask.get('id', '')}")
    for strength in data.get("strengths") or []:
        add(strength.get("event_refs") or [], ["Delight"], "Strength register")
    for gate in data.get("gates") or []:
        add(gate.get("event_refs") or [], ["Gate"], "Gate register")
    return result


def elapsed(run, event):
    try:
        value = seconds_between(run.get("task_started_at"), event.get("at"))
        return "+" + duration(value) if value is not None else "Time unavailable"
    except (ValueError, TypeError):
        return "Time unavailable"


def journey_map(run, events, marks):
    groups = []
    for i, event in enumerate(events):
        kinds = marks[event["eid"]]["labels"]
        if i not in (0, len(events) - 1) and not kinds and not event.get("is_error"):
            continue
        primary = kinds[0] if kinds else None
        if groups and primary and primary == groups[-1]["label"] and i == groups[-1]["end"] + 1:
            groups[-1]["refs"].append(event["eid"])
            groups[-1]["end"] = i
        else:
            groups.append({"event": event, "label": primary, "refs": [event["eid"]], "end": i})
    shown = groups[:24]
    if not shown:
        return '<div class="empty">No event map is available. Read the narrative and its evidence limits below.</div>'
    width = max(660, len(shown) * 164 + 30)
    parts = [f'<div class="journey-map" tabindex="0" role="region" aria-label="Scrollable recorded event map">'
             f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="178" viewBox="0 0 {width} 178" role="img" aria-labelledby="map-title map-desc">'
             '<title id="map-title">Recorded agent journey</title><desc id="map-desc">Selected events in recorded order. '
             'Each node links to the complete event table. Arrows indicate order, not proven causation.</desc>'
             '<defs><marker id="arrow" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="5" markerHeight="5" orient="auto">'
             '<path d="M 0 0 L 10 5 L 0 10 z" fill="#9eb4d6"/></marker></defs>']
    for i, group in enumerate(shown):
        event = group["event"]
        x, eid = 25 + i * 164, event["eid"]
        kinds = marks[eid]["labels"]
        label = kinds[0] if kinds else ("Failed call" if event.get("is_error") else "Event")
        icon, tone = LABELS.get(label, ("●", "bad" if event.get("is_error") else ""))
        if i:
            parts.append(f'<path class="route" d="M{x - 25} 71 H{x - 3}" marker-end="url(#arrow)"/>')
        action = re.sub(r"\s+", " ", str(event.get("input") or event.get("text") or event.get("name") or "Recorded event"))
        short = action[:22] + ("…" if len(action) > 22 else "")
        shape = (f'<path class="node {tone}" d="M{x + 12} 33 H{x + 123} L{x + 142} 71 L{x + 123} 109 H{x + 12} L{x - 7} 71 Z"/>'
                 if "Fork" in kinds else f'<rect class="node {tone}" x="{x}" y="33" width="140" height="76" rx="9"/>')
        references = eid if len(group["refs"]) == 1 else eid + "–" + group["refs"][-1]
        parts += [f'<a href="#{eid}" aria-label="{esc(references)}: {esc(label)}"><title>{esc(action)}</title>{shape}',
                  f'<text x="{x + 10}" y="56" class="step-label">{icon} {esc(label)}</text>',
                  f'<text x="{x + 10}" y="77" class="step-meta">{esc(references)}</text>',
                  f'<text x="{x + 10}" y="96" class="step-title">{esc(short)}</text>',
                  f'<text x="{x + 10}" y="131" class="step-meta">{esc(elapsed(run, event))}</text></a>']
    parts.append("</svg></div>")
    count = sum(len(group["refs"]) for group in shown)
    parts.append(f'<p class="small muted">{len(shown)} steps summarize {count} of {len(events)} recorded events'
                 + ("; map limited to the first 24 steps" if len(groups) > 24 else "")
                 + ". Consecutive events with the same annotation share a node. Arrows show recorded order.</p>")
    return "".join(parts)


def happenings(run, events, marks):
    rows = []
    for i, event in enumerate(events):
        eid, info = event["eid"], marks[event["eid"]]
        kinds = info["labels"]
        group = kinds + (["Obstacles"] if any(k != "Delight" for k in kinds) or event.get("is_error") else [])
        tags = labels(kinds) or badge("Agent note" if event.get("kind") == "text" else "Tool call")
        action = event.get("input") or event.get("text") or event.get("name") or "No event detail"
        output = event.get("output")
        outcome = ("Failed" if event.get("is_error") else "Completed") if event.get("is_error") is not None else "Outcome not recorded"
        details = (f'<details><summary>{esc(outcome)} · inspect response</summary><pre>{esc(output)}</pre></details>'
                   if output else f'<small>{esc(outcome if event.get("kind") == "tool" else "Recorded agent text")}</small>')
        links = "".join(f'<a class="mono" href="report.html#{quote(str(aid), safe="")}">{esc(aid)}</a>' for aid in dict.fromkeys(info["asks"])) or '<span class="muted">—</span>'
        rows.append(f'<tr id="{eid}" data-item data-order="{i}" data-kinds="{esc("|".join(group))}">'
                    f'<td class="event-identity"><a class="mono" href="#{eid}">{eid}</a><small>{esc(elapsed(run, event))}</small>'
                    f'<small>{esc(event.get("name") or event.get("kind"))}</small></td>'
                    f'<td class="event-kind">{tags}<small>{esc("; ".join(info["sources"]))}</small></td>'
                    f'<td class="event-action"><code>{esc(action)}</code>{details}</td><td class="event-asks">{links}</td></tr>')
    controls = toolbar("events", [("Obstacles", "Obstacles"), *[(key, key) for key in LABELS]])
    return ('<section class="section" id="happenings" data-filter-scope="events"><div class="section-heading">'
            '<h2>Table of happenings</h2><p>Every recorded event, with evidence links</p></div>' + controls
            + '<div class="table-wrap"><table class="happenings"><thead><tr><th scope="col">Event / elapsed</th>'
            '<th scope="col">Annotation</th><th scope="col">Action and response</th><th scope="col">Linked asks</th>'
            '</tr></thead><tbody data-items>' + "".join(rows) + "</tbody></table></div>"
            '<p class="empty" data-empty hidden>No matching events. Reset the filters to restore the full timeline.</p></section>')


def journey_body(run, data, tel, summary, markdown):
    events = [event for event in tel.get("events") or [] if isinstance(event.get("eid"), str) and EVENT_ID.fullmatch(event["eid"])]
    marks = annotations(markdown, data, events)
    legend = "".join(f'<span>{icon} {esc(name)}</span>' for name, (icon, _) in LABELS.items())
    # Link citations only when the referenced event is available in the current evidence.
    cited = re.sub(r"\[(E-\d{3,})\](?!\()", lambda m: f"[{m[1]}](#{m[1]})" if m[1] in marks else m[0], markdown)
    return (review_notice(run, data) + summary_strip(run, summary) + configuration_overview(run)
            + '<section class="section"><div class="section-heading"><h2>Journey map</h2>'
            '<a href="#account">Read the first-person account</a></div>' + journey_map(run, events, marks)
            + f'<details><summary>AJX annotation legend</summary><div class="legend">{legend}</div>'
            '<p class="small muted">A label describes an observation. It does not assign a severity or prove a cause.</p></details></section>'
            + (happenings(run, events, marks) if events else '<div class="notice warn">No normalized event evidence is available. '
               'A narrative alone cannot supply measured event timing or costs.</div>')
            + '<section class="section" id="account"><h2>First-person account</h2>'
            f'<p class="section-intro">{esc(run.get("journey_provenance") or "Provenance unavailable")}</p>'
            '<article class="panel narrative">' + mdlite.render(cited or "No journey was recorded.") + "</article></section>"
            + configuration_section(run))


def run_body(run, data, summary, digest, journey_exists):
    evidence = run.get("evidence") or {}
    coverage = [("Usage", evidence.get("usage_coverage")), ("Tool calls", evidence.get("tool_call_coverage")),
                ("Timestamps", evidence.get("timestamp_coverage")), ("Narrative", run.get("journey_provenance")),
                ("Prompt SHA-256", (run.get("task_prompt") or {}).get("sha256")),
                ("Trial SHA-256", run.get("evaluation_wrapper_sha256")), ("Skill revision", run.get("ajx_skill_revision"))]
    coverage_html = '<dl class="coverage">' + "".join(f"<dt>{esc(k)}</dt><dd>{esc(v or 'Unavailable')}</dd>" for k, v in coverage) + "</dl>"
    agent = run.get("agent") or {}
    checks = ('<h3>Task outcome</h3>' + status(run.get("task_outcome_verified"))
              + f'<p>{esc(run.get("verification_scope"))}</p><h4>Agent’s declaration at stop</h4>'
              + f'<p>{esc(run.get("task_outcome_declared") or "Not recorded")}</p>'
              + f'<p class="small muted">Stop reason: {esc(run.get("task_stop_reason") or "Unknown")}</p>'
              + '<a href="verify.json">Inspect verification checks</a>')
    strengths = "".join(f'<li><strong>{esc(s.get("title"))}</strong><p>{esc(s.get("how_observed"))} '
                        f'{event_links(s.get("event_refs"))}</p></li>' for s in data.get("strengths") or [])
    gates = "".join(f'<li><strong>{esc(g.get("trigger"))}</strong> {badge("Resolved" if g.get("resolved") else "Unresolved", "good" if g.get("resolved") else "warn")}'
                    f'<p>{esc(g.get("agent_action"))} {event_links(g.get("event_refs"))}</p></li>' for g in data.get("gates") or [])
    journey_link = ('<a href="pretty-journey.html">Open journey map and event table</a>' if journey_exists else
                    '<span class="muted">Journey unavailable; inspect the evidence digest below.</span>')
    return (review_notice(run, data) + summary_strip(run, summary) + configuration_overview(run) + asks_section(run, data)
            + configuration_section(run)
            + '<section class="section"><div class="section-heading"><h2>Trace the experience</h2>' + journey_link + "</div>"
            + '<div class="two-columns"><div class="panel"><h3>What helped</h3>'
            + (f"<ul>{strengths}</ul>" if strengths else '<p class="muted">No specific strength recorded.</p>')
            + '</div><div class="panel"><h3>Human gates</h3>'
            + (f"<ul>{gates}</ul>" if gates else '<p class="muted">No human gate recorded.</p>') + "</div></div></section>"
            + '<section class="section" id="verification"><h2>Verification and coverage</h2><div class="two-columns">'
            + f'<div class="panel">{checks}</div><div class="panel"><h3>Evidence coverage</h3>{coverage_html}</div></div></section>'
            + '<section class="section" id="method"><h2>Method and limits</h2><div class="panel">'
            + f'<p class="small">{esc(agent.get("harness_name"))} {esc(agent.get("harness_version"))}; '
            f'model {esc(agent.get("model_id") or agent.get("model_requested") or "default")}; '
            f'configuration {esc((agent.get("settings") or {}).get("config") or "unknown")}.</p>'
            + '<ul>' + "".join(f"<li>{esc(note)}</li>" for note in run.get("limitations") or ["No additional limitation recorded."]) + "</ul>"
            + '<p class="small">Shared events can contribute to several asks. Their costs cannot be added as independent savings. '
            'Modeled savings require a comparator and follow-up measurement.</p></div>'
            + '<details class="panel"><summary>Full evidence digest</summary>' + mdlite.render(digest or "No digest available.") + "</details></section>")


def matrix_body(spec, rows, cells, consistency, registers, markdown, clusters=None):
    product = spec["trial"]["product"]
    out = ['<section class="section" data-filter-scope="runs"><div class="section-heading">'
           '<h2>Asks by configuration</h2>'
           f'<p>{len(rows)} runs across {len(cells)} configurations</p></div>'
           '<p class="section-intro">Read the requested changes side by side. Each column retains its product version, '
           'agent, evidence status, and measured costs.</p>']
    if not consistency["wall_clock_comparable"]:
        out.append('<div class="notice warn"><strong>Concurrent runs.</strong> Wall-clock values are shown for context and are not comparable.</div>')
    if len(consistency["product_versions"]) > 1:
        out.append('<div class="notice">Multiple product versions are present. Compare equivalent tasks and starting conditions; '
                   'a version change and an agent change are separate factors.</div>')
    out.append(toolbar("runs", [(h, h) for h in sorted({str(r["harness"]) for r in rows})],
                       [("asks", "Most asks"), ("elapsed", "Longest task time"), ("failed", "Most failed calls")], compare=True))
    out.append('<div class="compare-grid" data-items>')
    for i, row in enumerate(rows):
        data = registers[row["dir"]]
        prefix = "runs/" + quote(row["dir"], safe="") + "/"
        model = row.get("model_id") or row.get("model_requested") or "Default model"
        asks = data.get("asks") or []
        profiles = configuration_values(row)
        profile_header = "<br>".join(f"{esc(label)}: {esc(value)}" for label, value in profiles[:4])
        out.append(f'<article class="run-card" data-item data-order="{i}" data-kinds="{esc(row["harness"])}" '
                   f'data-asks="{len(asks)}" data-elapsed="{esc(row.get("elapsed_seconds"))}" data-failed="{esc(row.get("failed_tool_calls"))}">'
                   '<header><label class="select-run js-only"><input type="checkbox" data-select-run checked>'
                   f'{esc(row["run_id"])}</label><h3>{esc(product)} {esc(row.get("product_version") or "unknown version")}</h3>'
                   f'<div class="run-config"><strong>{esc(row["harness"])}</strong><br>{esc(model)}<br>'
                   f'{esc(row.get("config"))} configuration · {esc(row["run_id"])}<br>{profile_header}</div>{status(row.get("verified"))}'
                   '<dl class="run-facts"><div><dt>Task time</dt>'
                   f'<dd>{esc(duration(row.get("elapsed_seconds")))}</dd></div><div><dt>Output tokens</dt>'
                   f'<dd>{esc(number(row.get("output_tokens")))} <span class="small muted">({esc(row.get("usage_status") or "unavailable")})</span></dd></div>'
                   f'<div><dt>Tool calls / failed</dt><dd>{esc(number(row.get("tool_calls")))} / {esc(number(row.get("failed_tool_calls")))}</dd></div>'
                   f'<div><dt>Human gates</dt><dd>{row["gates"]}</dd></div></dl></header><div class="run-asks">')
        if row.get("review_status") == "incomplete":
            out.append('<div class="notice bad">Review incomplete. Do not interpret an empty register as a clean run.</div>')
        if row.get("teardown") in ("failed", "auth_errors", "skipped"):
            out.append(f'<div class="notice bad">Cleanup not confirmed. <a href="{prefix}teardown.json">Inspect cleanup</a>.</div>')
        out.append(environment_cleanup_notice(row, prefix))
        if row.get("verify_auth_errors"):
            out.append('<div class="notice warn">Verification encountered credential errors. Inspect the checks before comparing outcomes.</div>')
        out.append("".join(ask_card(ask, n, prefix=prefix, compact=True) for n, ask in enumerate(asks)) if asks else empty_register(data))
        out.append('<details><summary>Configuration coverage and limits</summary><p class="small">'
                   + esc("; ".join(f"{label}: {value}" for label, value in profiles[4:]))
                   + f'</p><p class="small">{esc(profile_coverage(row))}</p>'
                   '<p class="small">Enabled extensions do not prove use. Missing loaded/invoked telemetry remains unknown. '
                   'A private HOME is not a filesystem sandbox.</p>'
                   + '<ul>' + "".join(f'<li>{esc(note)}</li>' for note in profile_limitations(row)) + "</ul>"
                   + f'<div class="links">{profile_links(row, prefix)}<a href="{prefix}run.json">Complete run evidence</a></div></details>')
        out.append(f'</div><footer class="links"><a href="{prefix}report.html">Full ask register</a>'
                   f'<a href="{prefix}pretty-journey.html">Journey</a></footer></article>')
    out.append('</div><p class="empty" data-empty hidden>No runs match the current selection. Reset to see the complete matrix.</p></section>')
    out.append('<section class="section" id="measurements"><h2>Technical comparison</h2>'
               '<p class="section-intro">Complete run measurements. Missing telemetry stays unavailable. '
               'Output tokens use each model’s tokenizer; provider dollars and credits keep separate units.</p>'
               '<div class="table-wrap"><table><thead><tr>')
    headings = ["Run / product version", "Harness / model", "Environment / backend", "Agent profile / extensions",
                "Environment cleanup", "Verified", "Task time", "Calls / failed", "Output tokens", "Asks", "Provider USD", "Credits"]
    out.append("".join(f'<th scope="col">{h}</th>' for h in headings) + "</tr></thead><tbody>")
    for row in rows:
        prefix = "runs/" + quote(row["dir"], safe="") + "/"
        profiles = [value for _, value in configuration_values(row)]
        values = [f'<a class="mono" href="{prefix}report.html">{esc(row["run_id"])}</a><small>{esc(row.get("product_version"))}</small>',
                  f'{esc(row["harness"])}<small>{esc(row.get("model_id") or row.get("model_requested") or "default")}</small>',
                  f'{esc(profiles[0])}<small>{esc(profiles[1])}</small>'
                  f'<small><a href="{prefix}report.html#configuration">Capabilities and limits</a></small>',
                  f'{esc(profiles[2])}<small>Skills (declared): {esc(profiles[3])}</small>'
                  f'<small>Plugins (declared): {esc(profiles[4])}</small><small>Hooks (declared): {esc(profiles[5])}</small>',
                  badge(profiles[6], "bad" if cleanup_problem(row.get("environment_cleanup")) else ""),
                  status(row.get("verified")), esc(duration(row.get("elapsed_seconds"))),
                  f'{esc(number(row.get("tool_calls")))} / {esc(number(row.get("failed_tool_calls")))}<small>{esc(row.get("tool_status"))}</small>',
                  f'{esc(number(row.get("output_tokens")))}<small>{esc(row.get("usage_status"))}</small>',
                  str(row["asks"]), esc(number(row.get("cost_estimate_usd"))), esc(number(row.get("credits")))]
        out.append("<tr>" + "".join(f"<td>{value}</td>" for value in values) + "</tr>")
    out.append("</tbody></table></div></section>")
    if clusters:
        out.append('<section class="section"><h2>Related asks across runs</h2>'
                   '<p class="section-intro">Reporter-inferred groups. Inspect each member before treating them as the same defect.</p>')
        for cluster in clusters:
            links = " ".join(f'<a href="runs/{quote(str(m["run_id"]), safe="")}/report.html#{quote(str(m["ask_id"]), safe="")}">'
                             f'{esc(m["run_id"])}/{esc(m["ask_id"])}</a>' for m in cluster["members"])
            out.append(f'<div class="panel"><h3>{esc(cluster["title"])}</h3><div class="links">{links}</div></div>')
        out.append("</section>")
    out.append('<section class="section"><h2>Comparability and provenance</h2><div class="panel"><dl class="coverage">')
    for key, value in consistency.items():
        out.append(f'<dt>{esc(key.replace("_", " "))}</dt><dd>{esc(value)}</dd>')
    out.append('</dl></div><details class="panel"><summary>Complete matrix, variance, and source records</summary>'
               + mdlite.render(markdown) + "</details></section>")
    if not rows:
        out.insert(0, '<div class="empty">No runs have been recorded yet. Run the trial to populate this comparison.</div>')
    return "".join(out)
