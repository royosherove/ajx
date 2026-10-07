#!/usr/bin/env python3
"""Validate normalized AJX event exports and compute reconciled phase costs."""

import argparse
import json
import sys
from datetime import datetime
from pathlib import Path


def require(condition, message):
    if not condition:
        raise ValueError(message)


def timestamp(value):
    require(isinstance(value, str), "Timestamp must be a string")
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    require(parsed.utcoffset() is not None, f"Timestamp needs timezone: {value}")
    return parsed


def count(value, label):
    require(type(value) is int and value >= 0, f"{label} must be a nonnegative integer")
    return value


def identifier(value, label):
    require(isinstance(value, str) and bool(value.strip()), f"{label} must be nonempty")
    return value


def measure(manifest, events):
    require(isinstance(manifest, dict), "Manifest must be an object")
    sessions = manifest.get("sessions")
    require(isinstance(sessions, list) and sessions, "Manifest needs sessions")
    prepared = {}
    for session in sessions:
        require(isinstance(session, dict), "Each session must be an object")
        sid = identifier(session["id"], "Session ID")
        require(sid not in prepared, f"Duplicate session: {sid}")
        start, end = timestamp(session["start"]), timestamp(session["end"])
        require(start < end, f"Invalid session window: {sid}")
        for field, expected in (
            ("usage_status", "expected_output_tokens"),
            ("tool_status", "expected_tool_calls"),
        ):
            status = session.get(field)
            require(status in ("complete", "partial", "unavailable"), f"{sid}: invalid {field}")
            if status == "complete":
                count(session.get(expected), f"{sid}.{expected}")
            elif expected in session:
                raise ValueError(f"{sid}: {expected} requires complete status")
        raw_phases = session.get("phases")
        require(isinstance(raw_phases, list) and raw_phases, f"{sid}: phases required")
        phases, phase_ids, cursor = [], set(), start
        for phase in raw_phases:
            require(isinstance(phase, dict), "Each phase must be an object")
            pid = identifier(phase["id"], "Phase ID")
            require(pid not in phase_ids, f"{sid}: duplicate phase {pid}")
            phase_ids.add(pid)
            pstart, pend = timestamp(phase["start"]), timestamp(phase["end"])
            require(pstart == cursor, f"{sid}/{pid}: phase gap or overlap")
            require(pstart < pend <= end, f"{sid}/{pid}: invalid phase window")
            scope = phase.get("scope")
            require(scope in ("task", "report", "verification", "human_wait", "excluded"),
                    f"{sid}/{pid}: invalid scope")
            phases.append({
                "id": pid,
                "scope": scope,
                "start": phase["start"],
                "end": phase["end"],
                "elapsed_seconds": (pend - pstart).total_seconds(),
                "observed_output_tokens": 0,
                "observed_tool_calls": 0,
                "usage_records": 0,
            })
            cursor = pend
        require(cursor == end, f"{sid}: phases do not cover the session")
        prepared[sid] = {"definition": session, "phases": phases, "seen": set()}

    for event in events:
        require(isinstance(event, dict), "Each event must be an object")
        sid = identifier(event["session_id"], "Event session ID")
        require(sid in prepared, f"Unknown event session: {sid}")
        current = prepared[sid]
        eid = identifier(event["id"], "Event ID")
        require(eid not in current["seen"], f"{sid}: duplicate event {eid}")
        current["seen"].add(eid)
        at = timestamp(event["at"])
        phase = next((p for p in current["phases"]
                      if timestamp(p["start"]) <= at < timestamp(p["end"])), None)
        require(phase is not None, f"{sid}/{eid}: event outside session window")
        if event["kind"] == "usage":
            require(current["definition"]["usage_status"] != "unavailable",
                    f"{sid}: usage records contradict unavailable status")
            phase["observed_output_tokens"] += count(event["output_tokens"], f"{sid}/{eid}")
            phase["usage_records"] += 1
        elif event["kind"] == "tool_call":
            require(current["definition"]["tool_status"] != "unavailable",
                    f"{sid}: tool calls contradict unavailable status")
            phase["observed_tool_calls"] += 1
        else:
            raise ValueError(f"{sid}/{eid}: unsupported event kind {event['kind']}")

    results = []
    for sid, current in prepared.items():
        definition, phases = current["definition"], current["phases"]
        result = {
            "id": sid,
            "start": definition["start"],
            "end": definition["end"],
            "elapsed_seconds": (
                timestamp(definition["end"]) - timestamp(definition["start"])
            ).total_seconds(),
            "phases": phases,
        }
        for metric, status_field, expected in (
            ("output_tokens", "usage_status", "expected_output_tokens"),
            ("tool_calls", "tool_status", "expected_tool_calls"),
        ):
            status = definition[status_field]
            observed = sum(p["observed_" + metric] for p in phases)
            if status == "complete":
                require(observed == definition[expected],
                        f"{sid}: {metric} reconciliation failed: "
                        f"observed {observed}, expected {definition[expected]}")
            result[status_field] = status
            result["observed_" + metric] = None if status == "unavailable" else observed
            result[metric] = observed if status == "complete" else None
            result[metric + "_reconciled"] = status == "complete"
            for phase in phases:
                phase[metric] = phase["observed_" + metric] if status == "complete" else None
                if status == "unavailable":
                    phase["observed_" + metric] = None
        results.append(result)
    return {
        "schema_version": 1,
        "method": "normalized incremental usage; half-open phase windows",
        "sessions": results,
        "note": "No combined wall-clock total: sessions may overlap or use different scopes.",
    }


def read_events(path):
    with path.open(encoding="utf-8") as stream:
        for line_number, line in enumerate(stream, 1):
            if not line.strip():
                continue
            try:
                yield json.loads(line)
            except json.JSONDecodeError as error:
                raise ValueError(f"{path.name}:{line_number}: invalid JSON") from error


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("manifest", type=Path)
    parser.add_argument("events", type=Path)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    try:
        manifest = json.loads(args.manifest.read_text(encoding="utf-8"))
        result = measure(manifest, read_events(args.events))
        rendered = json.dumps(result, indent=2, ensure_ascii=False) + "\n"
        if args.output:
            require(args.output.resolve() not in {args.manifest.resolve(), args.events.resolve()},
                    "Output must not overwrite an input")
            args.output.write_text(rendered, encoding="utf-8")
        else:
            sys.stdout.write(rendered)
    except (OSError, ValueError, KeyError, TypeError) as error:
        print(f"AJX measurement failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
