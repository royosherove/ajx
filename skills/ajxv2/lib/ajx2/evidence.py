"""Evidence processing done by code, never by an LLM: event IDs, digest, isolation scan,
per-ask cost attribution, and AJX v1 measurement (measure_v1.measure)."""

import json
import re
from datetime import timedelta
from pathlib import Path

from .envcheck import recorded_notes
from .measure_v1 import measure as v1_measure
from .util import SKILL_ROOT, parse_ts, read_json, seconds_between, write_json


def _ts_key(ev):
    return ev.get("at") or "9999"


def assign_ids(tel):
    events = sorted(tel["events"], key=_ts_key)  # stable: preserves order for equal timestamps
    for i, ev in enumerate(events, 1):
        ev["eid"] = f"E-{i:03d}"
        if ev["kind"] == "tool":
            ev["duration_s"] = seconds_between(ev.get("at"), ev.get("end"))
    tel["events"] = events
    return tel


def isolation_scan(tel, spec, state):
    """Flag tool inputs that reach outside the run's own workspace toward evaluation material."""
    own = {str(Path(p)) for p in (state.get("paths") or {}).values()}
    root = spec["trial"]["workspace_root"]
    needles = {
        "report/output dir": spec["trial"]["output_dir"],
        "trial dir": str(Path(spec["path"]).parent),
        "ajx skill files": str(SKILL_ROOT),
        "skills dir": "/.claude/skills",
        "harness transcripts": "/.claude/projects",
    }
    flags = []
    for ev in tel["events"]:
        if ev["kind"] != "tool":
            continue
        blob = json.dumps(ev.get("input_raw"), ensure_ascii=False) if ev.get("input_raw") is not None else ev.get("input", "")
        # The run's own paths (and the user-chosen workspace root) are legitimate even when they sit
        # under the trial dir or contain "ajx"; scrub them before any needle check.
        scrubbed = blob
        for own_path in sorted(own | {root}, key=len, reverse=True):
            scrubbed = scrubbed.replace(own_path, "<own>")
        hits = [label for label, needle in needles.items() if needle and needle in scrubbed]
        if re.search(r"\bajx", scrubbed, re.I):
            hits.append("mentions 'ajx'")
        for m in re.finditer(re.escape(root) + r"/[0-9a-f]{10}(?![0-9A-Za-z_-])", blob):  # _rand() shape only
            if not any(m.group(0).startswith(o) for o in own):
                hits.append("another run's workspace root entry")
        if hits:
            flags.append({"eid": ev["eid"], "reasons": sorted(set(hits)), "input": ev.get("input", "")[:200]})
    return flags


def _mmss(seconds):
    if seconds is None:
        return "?"
    seconds = int(round(seconds))
    return f"{seconds // 60:02d}:{seconds % 60:02d}"


def _one_line(text, n):
    text = re.sub(r"\s+", " ", text or "").strip().replace("|", "\\|")
    return text if len(text) <= n else text[: n - 1] + "…"


def task_summary(state, tel):
    ex = state.get("execute", {})
    tools = [e for e in tel["events"] if e["kind"] == "tool"]
    failed = [e for e in tools if e.get("is_error")]
    retries = 0
    for prev, cur in zip(tools, tools[1:]):
        if prev.get("is_error") and prev.get("name") == cur.get("name"):
            retries += 1
    gaps = []
    timed = [e for e in tel["events"] if e.get("at")]
    for prev, cur in zip(timed, timed[1:]):
        prev_end = max(parse_ts(prev["at"]), parse_ts(prev.get("end") or prev["at"]))
        idle = (parse_ts(cur["at"]) - prev_end).total_seconds()
        if idle >= 60:
            gaps.append(round(idle, 1))
    tokens = sum(u["output_tokens"] for u in tel.get("usage") or [])
    return {
        "elapsed_seconds": seconds_between(ex.get("started_at"), ex.get("stopped_at")),
        "elapsed_source": "ajx2 process start/stop (same clock for every harness)",
        "stop_reason": ex.get("stop_reason"), "harness_stop_reason": tel.get("stop_reason"),
        "exit_code": ex.get("exit_code"), "timed_out": ex.get("timed_out"),
        "tool_calls": len(tools), "tool_status": tel.get("tool_status"),
        "failed_tool_calls": len(failed), "retries_after_error": retries,
        "output_tokens": tokens if tel.get("usage_status") != "unavailable" else None,
        "usage_status": tel.get("usage_status"),
        "longest_tool_seconds": max((e.get("duration_s") or 0 for e in tools), default=None),
        "gaps_over_60s": gaps,
        "model_ids": tel.get("model_ids"),
        "harness_reported": {k: v for k, v in (tel.get("harness_reported") or {}).items() if k != "model_usage"},
    }


def _fence(text):
    longest = max((len(m.group(0)) for m in re.finditer(r"`+", text or "")), default=0)
    return "`" * max(3, longest + 1)


def build_digest(spec, cell, state, tel, verify, environment, include_verification=True):
    """Factual digest. The narrator gets include_verification=False so the agent's own account of
    its outcome is not colored by checks it never ran; extract/reconstruct see the full digest."""
    ex = state.get("execute", {})
    s = task_summary(state, tel)
    start = ex.get("started_at")
    lines = [
        "# Evidence digest (factual, generated by ajx2)",
        "",
        "All numbers below were computed by ajx2 from recorded telemetry. Use them as given; do not recompute.",
        "",
        "## Run facts",
        "",
        f"- Harness: {cell['harness']} ({environment.get('harness', {}).get('version_before')})",
        f"- Model requested: {cell.get('model') or 'harness default'}; model ids observed: {', '.join(s['model_ids'] or []) or 'not exposed'}",
        f"- Harness config: {cell['config']}",
        f"- Task window: {start} to {ex.get('stopped_at')} ({s['elapsed_seconds']} s, {s['elapsed_source']})",
        f"- Stop: {s['stop_reason']} (harness: {s['harness_stop_reason']}), exit code {s['exit_code']}",
        f"- Tool calls: {s['tool_calls']} ({s['tool_status']}); failed: {s['failed_tool_calls']}; "
        f"same-tool retries after an error: {s['retries_after_error']}",
        f"- Output tokens: {s['output_tokens'] if s['output_tokens'] is not None else 'unavailable'} ({s['usage_status']})",
        f"- Gaps of 60 s or more between recorded events: {s['gaps_over_60s'] or 'none'}",
        f"- Timestamp source: {tel.get('timestamp_source')}",
    ]
    hr = s["harness_reported"]
    if hr:
        lines.append(f"- Harness-reported (estimates, labeled as such): "
                     + "; ".join(f"{k}={v}" for k, v in hr.items() if v not in (None, "", [], {})))
    lines += ["", "## Timeline", "",
              "| Event | +mm:ss | Kind | Tool | Input / text | Duration s | Error | Output excerpt |",
              "|---|---|---|---|---|---|---|---|"]
    for ev in digest_events(tel["events"]):
        if ev.get("kind") == "omitted":
            lines.append(f"| {ev['first']}..{ev['last']} | | omitted | | {ev['count']} routine events omitted from this "
                         f"digest for length ({ev['kept']} error or text events in that range are shown); "
                         f"the full list is in events.jsonl | | | |")
            continue
        off = _mmss(seconds_between(start, ev.get("at"))) if ev.get("at") and start else "?"
        if ev["kind"] == "tool":
            lines.append(f"| {ev['eid']} | {off} | tool | {_one_line(ev.get('name'), 30)} | "
                         f"{_one_line(ev.get('input'), 140)} | {ev.get('duration_s') if ev.get('duration_s') is not None else '?'} | "
                         f"{'yes' if ev.get('is_error') else ('no' if ev.get('is_error') is False else '?')} | "
                         f"{_one_line(ev.get('output'), 220)} |")
        else:
            lines.append(f"| {ev['eid']} | {off} | text |  | {_one_line(ev.get('text'), 260)} |  |  |  |")
    if not tel["events"]:
        lines.append("| - | - | - | - | No event-level telemetry for this harness | - | - | - |")
    final = (tel.get("final_text") or "(none captured)").strip()[:6000]
    fence = _fence(final)
    lines += ["", "## Final message at the stopping point (verbatim declared outcome)", "", fence + "text", final, fence, ""]
    if include_verification:
        lines += ["## Reporter verification (run by ajx2 AFTER the task agent stopped; not done by the task agent)", ""]
        if verify.get("checks"):
            lines.append(f"Result: {verify['passed']}/{verify['total']} checks passed -> {verify['outcome']}")
            lines.append("")
            for c in verify["checks"]:
                lines.append(f"- {'PASS' if c['passed'] else 'FAIL'}: {c['name']} ({c['type']}; {c.get('detail')})"
                             + (f" scope: {c['scope']}" if c.get("scope") else "")
                             + (" [this check's own output shows credential errors: the result reflects ajx2's "
                                "verification credentials, not the agent's work]" if c.get("auth_errors") else ""))
        else:
            lines.append("No verification checks were defined.")
    if include_verification and tel.get("isolation_flags"):
        lines += ["", "## Isolation flags", ""]
        lines += [f"- {f['eid']}: {', '.join(f['reasons'])}" for f in tel["isolation_flags"]]
    notes = recorded_notes(environment) if include_verification else []
    if notes:
        lines += ["", "## Environment notes (the trial machine's configuration, recorded by ajx2 before the task)", ""]
        lines += [f"- {n}" for n in notes]
    lines += ["", "## Telemetry limitations", ""]
    lines += [f"- {l}" for l in tel.get("limitations") or []] or ["- none recorded"]
    return "\n".join(lines) + "\n"


DIGEST_MAX_EVENTS = 300
DIGEST_EDGE = 110


def digest_events(events):
    """Bound the digest for very long tasks: keep the first and last DIGEST_EDGE events verbatim and,
    in between, only failed tool calls and assistant text; summarize the rest as one row."""
    if len(events) <= DIGEST_MAX_EVENTS:
        return list(events)
    head, middle, tail = events[:DIGEST_EDGE], events[DIGEST_EDGE:-DIGEST_EDGE], events[-DIGEST_EDGE:]
    kept = [e for e in middle if e.get("kind") == "text" or e.get("is_error")][:80]
    marker = {"kind": "omitted", "first": middle[0]["eid"], "last": middle[-1]["eid"],
              "count": len(middle) - len(kept), "kept": len(kept)}
    return head + [marker] + sorted(kept, key=_ts_key) + tail


def ask_costs(asks, tel):
    by_id = {e["eid"]: e for e in tel["events"]}
    usage = {u["id"]: u["output_tokens"] for u in tel.get("usage") or []}
    owners = {}
    for ask in asks:
        for ref in ask.get("event_refs") or []:
            owners.setdefault(ref, []).append(ask["id"])
    for ask in asks:
        refs = [r for r in ask.get("event_refs") or [] if r in by_id]
        ask["invalid_refs"] = [r for r in ask.get("event_refs") or [] if r not in by_id]
        evs = [by_id[r] for r in refs]
        tools = [e for e in evs if e["kind"] == "tool"]
        starts = [parse_ts(e["at"]) for e in evs if e.get("at")]
        ends = [parse_ts(e.get("end") or e["at"]) for e in evs if e.get("at")]
        mids = {e.get("message_id") for e in evs if e.get("message_id")}
        tokens = sum(usage.get(m, 0) for m in mids) if usage and mids else None
        ask["measured"] = {
            "events": len(evs),
            "tool_calls": len(tools),
            "failed_tool_calls": sum(1 for e in tools if e.get("is_error")),
            "tool_seconds": round(sum(e.get("duration_s") or 0 for e in tools), 1) if tools else None,
            "span_seconds": round((max(ends) - min(starts)).total_seconds(), 1) if starts else None,
            "output_tokens": tokens,
            "shared_events": sorted(r for r in refs if len(owners.get(r, [])) > 1),
            "basis": "sum over referenced events; tokens attributed per whole assistant message (two asks citing "
                     "events from the same message both carry its tokens); span = first referenced event start "
                     "to last referenced event end",
        }
    return asks


def _window(start, stop, events_at):
    times = [parse_ts(t) for t in events_at if t]
    s = min([parse_ts(start)] + times) if start else (min(times) if times else None)
    e = max([parse_ts(stop)] + times) if stop else (max(times) if times else None)
    if s is None or e is None:
        return None, None
    e = e + timedelta(milliseconds=1)
    return s.isoformat().replace("+00:00", "Z"), e.isoformat().replace("+00:00", "Z")


def measure_run(spec, cell, run_dir, state):
    run_dir = Path(run_dir)
    sessions, events = [], []

    def add(sid, scope, proc, tel=None, reported_tokens=None):
        if not proc or not proc.get("started_at"):
            return
        tel = tel or {}
        tool_evs = [e for e in tel.get("events") or [] if e["kind"] == "tool" and e.get("at")]
        usage = [u for u in tel.get("usage") or [] if u.get("at")]
        start, end = _window(proc["started_at"], proc.get("stopped_at"),
                             [e["at"] for e in tool_evs] + [u["at"] for u in usage])
        if not start:
            return
        session = {"id": sid, "start": start, "end": end,
                   "phases": [{"id": sid, "start": start, "end": end, "scope": scope}]}
        usage_status = tel.get("usage_status", "unavailable")
        if reported_tokens is not None and not usage:
            usage = [{"id": f"{sid}-total", "at": start, "output_tokens": int(reported_tokens)}]
            usage_status = "partial"
        if usage_status == "unavailable":
            usage = []
        session["usage_status"] = usage_status
        if usage_status == "complete":
            session["expected_output_tokens"] = tel["expected_output_tokens"]
        tool_status = tel.get("tool_status", "unavailable") if tool_evs else "unavailable"
        session["tool_status"] = tool_status
        if tool_status == "complete":
            session["expected_tool_calls"] = tel["expected_tool_calls"]
        sessions.append(session)
        for u in usage:
            events.append({"id": f"usage:{u['id']}", "session_id": sid, "at": u["at"], "kind": "usage",
                           "output_tokens": int(u["output_tokens"])})
        if tool_status != "unavailable":
            for e in tool_evs:
                events.append({"id": f"tool:{e.get('tool_use_id') or e['eid']}", "session_id": sid,
                               "at": e["at"], "kind": "tool_call"})

    add("execute", "task", state.get("execute"), read_json(run_dir / "telemetry.execute.json", {}))
    verify = read_json(run_dir / "verify.json", {})
    if verify.get("window"):
        add("verify", "verification", {"started_at": verify["window"]["start"], "stopped_at": verify["window"]["end"]})
    narr = state.get("narrate") or {}
    if narr.get("reconstruction"):
        add("narrate", "report", narr, None, narr.get("output_tokens"))
    else:
        add("narrate", "report", narr, read_json(run_dir / "telemetry.narrate.json", {}))
    ext = state.get("extract") or {}
    add("extract", "report", ext, None, ext.get("output_tokens"))

    manifest = {"sessions": sessions}
    write_json(run_dir / "measure.manifest.json", manifest)
    with (run_dir / "measure.events.jsonl").open("w", encoding="utf-8") as fh:
        for e in events:
            fh.write(json.dumps(e) + "\n")
    try:
        result = v1_measure(manifest, events)
        error = None
    except (ValueError, KeyError, TypeError) as exc:
        result, error = None, str(exc)
    tel = read_json(run_dir / "telemetry.execute.json", {"events": []})
    out = {"schema_version": 2, "v1_measurement": result, "v1_error": error,
           "task_summary": task_summary(state, tel),
           "note": "execute is the task window; verify/narrate/extract are separate windows and are "
                   "never added to task cost. Reporter token totals are single harness-reported values."}
    write_json(run_dir / "measurements.json", out)
    return out
