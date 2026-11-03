"""Simulated worker for the AJX Console demo. It calls no agent, model or network.

The worker prints a scripted session for a fictional `orders-cli` while it writes out.csv, and the
narration and reporting calls return canned text, so the console's live feed, verification,
cleanup and reporting can be tried for free. Every command and output is invented, and the
reports carry a limitation saying so. AJX_DEMO_PACE scales the pauses (default 1, about a minute).
"""

import json
import os
import sys
from pathlib import Path

from ajx.base import Harness, empty_telemetry, finish_tool, stream_records, text_event, tool_event
from ajx.plugins import register
from ajx.util import read_json

SESSION = "console-demo-session"
LIMITATION = "Simulated worker (AJX Console demo): scripted events and canned reports; no agent or model was called."
CSV = "id,customer,total\n1,Ada,12.50\n2,Grace,7.25\n3,Linus,30.00\n"

STEPS = [  # (pause before the event, event)
    (2, {"kind": "text", "text": "I'll start with the tool's built-in help."}),
    (3, {"kind": "tool", "name": "Bash", "command": "orders-cli --help",
         "output": "usage: orders-cli convert FILE --to csv|json [--columns LIST] [--out FILE]"}),
    (4, {"kind": "tool", "name": "Bash", "command": "orders-cli convert orders.json --to csv", "error": True,
         "output": "error: --columns is required for csv"}),
    (3, {"kind": "text", "text": "The error names the flag but not the valid column names. I'll read the input to find them."}),
    (3, {"kind": "tool", "name": "Read", "command": "orders.json",
         "output": '[{"id": 1, "customer": "Ada", "total": "12.50"}, ... (3 orders)]'}),
    (5, {"kind": "tool", "name": "Bash", "command": "orders-cli convert orders.json --to csv "
         "--columns id,customer,total --out out.csv", "output": "", "writes": True}),
    (4, {"kind": "text", "text": "The command printed nothing, so I can't tell whether it worked. I'll check the file."}),
    (3, {"kind": "tool", "name": "Bash", "command": "cat out.csv", "output": CSV.strip()}),
    (2, {"kind": "text", "text": "Created out.csv with a header and the three orders in their original order."}),
]

WORKER = r"""
import json, sys, time
steps, pace, csv = json.loads(sys.argv[1]), float(sys.argv[2]), sys.argv[3]
for pause, event in steps:
    time.sleep(pause * pace)
    if event.get("writes"):
        with open("out.csv", "w", encoding="utf-8") as fh:
            fh.write(csv)
    print(json.dumps(event), flush=True)
"""
ECHO = "import sys, time; time.sleep(float(sys.argv[1])); sys.stdout.write(sys.stdin.read())"
SLEEP = "import sys, time; time.sleep(float(sys.argv[1])); print('ok')"

JOURNEY = """# Journey: convert three orders from JSON to CSV with orders-cli

## Outcome I declared
I created out.csv with a header row and the three orders in their original order [E-009]. I had printed the file before I stopped [E-008].

## What I asked for first
- An error for a missing --columns flag that lists the valid column names.
- A confirmation line after convert writes a file.
- One complete CSV example in --help.

## Chronological account
1. I read the built-in help [E-001] [E-002]. It listed convert and its flags but no example.
2. \U0001f6a7 Roadblock. My first convert call failed with "error: --columns is required for csv" [E-003]. The error named the flag but not the values it accepts.
3. \u21a9\ufe0f Detour. I read orders.json to find the field names [E-004] [E-005], then reran convert with them [E-006].
4. \U0001f441\ufe0f Blind Spot. The corrected command printed nothing [E-006], so I could not tell whether it had written the file [E-007].
5. I printed out.csv and saw the header and three rows [E-008]. I declared the task done [E-009].

## Where I was blocked, and how I recovered
The missing --columns flag blocked my first attempt [E-003]. I recovered by reading the input file [E-005].

## Human help I needed
None.

## What I trusted and when I changed my mind
I trusted the help text until the first error [E-003]. After that I took the column names from the input file [E-005].

## What helped
\u2705 Delight. The error named the missing flag directly [E-003].

## What I still do not know
Whether convert can take its columns from the input without --columns.
"""

ASKS = [
    {"title": "List the valid columns when --columns is missing",
     "requested_behavior": "When --columns is missing, name the available columns and show one complete command.",
     "evidence_status": "observed_friction",
     "how_observed": "The first convert call failed with 'error: --columns is required for csv' [E-003]. The worker "
                     "then read the input file to find the field names [E-004, E-005].",
     "consequence": "One failed command and an extra file read before the conversion could run.",
     "event_refs": ["E-003", "E-004", "E-005"], "journey_anchors": [], "labels": ["Roadblock", "Detour"],
     "priority_rationale": "It blocks the first conversion most new users will try.", "modeled_saving": None,
     "verification": "Run 'orders-cli convert orders.json --to csv' in a fresh folder; the error lists id, customer "
                     "and total and shows a working command.",
     "workaround": "Read the input file to find the field names."},
    {"title": "Confirm what convert wrote",
     "requested_behavior": "After writing a file, print its path and the number of rows written.",
     "evidence_status": "observed_friction",
     "how_observed": "The successful convert printed nothing [E-006]; the worker printed the file to confirm the "
                     "result [E-007, E-008].",
     "consequence": "An extra command on every successful conversion.",
     "event_refs": ["E-006", "E-007", "E-008"], "journey_anchors": [], "labels": ["Blind Spot"],
     "priority_rationale": "Cheap to add and removes a verification step from every run.", "modeled_saving": None,
     "verification": "Run the documented convert command; it prints 'wrote out.csv (3 rows)'.", "workaround": None},
    {"title": "Add a complete CSV example to --help",
     "requested_behavior": "Show one complete csv example with --columns and --out in the help text.",
     "evidence_status": "observed_friction",
     "how_observed": "The help listed the flags without an example [E-002]; the first attempt omitted --columns [E-003].",
     "consequence": "The first attempt failed.",
     "event_refs": ["E-002", "E-003"], "journey_anchors": [], "labels": ["Fork"],
     "priority_rationale": "The help is the only documentation a new user has here.", "modeled_saving": None,
     "verification": "orders-cli --help contains a runnable csv example.", "workaround": None},
]
STRENGTHS = [{"title": "The error named the missing flag",
              "how_observed": "'error: --columns is required for csv' pointed at the flag to add [E-003].",
              "event_refs": ["E-003"]}]


def _pace(ctx):
    value = (ctx.get("env") or {}).get("AJX_DEMO_PACE") or os.environ.get("AJX_DEMO_PACE") or "1"
    try:
        return max(0.0, float(value))
    except ValueError:
        return 1.0


@register("harness", "console-demo")
class ConsoleDemo(Harness):
    binary = "sh"
    can_resume = True
    can_report = True
    clean_supported = True
    verified_live = False
    default_auth = "inherit"
    telemetry = "simulated events (no tokens)"

    def version(self):
        return "console-demo 1 (simulated)"

    def plan(self, ctx):
        return {"session_id": SESSION}

    def execute(self, ctx):
        proc = self.run(ctx, [sys.executable, "-I", "-c", WORKER, json.dumps(STEPS), str(_pace(ctx)), CSV], "execute")
        proc.update(session_id=SESSION)
        return proc

    def progress(self, run_dir, stage="execute", limit=10):
        items = []
        for at, ev, _ in stream_records(Path(run_dir) / f"{stage}.raw.jsonl"):
            if not isinstance(ev, dict):
                continue
            if ev.get("kind") == "text":
                items.append({"at": at, "kind": "text", "text": ev.get("text") or ""})
            elif ev.get("kind") == "tool":
                text = f"{ev.get('name')}: {ev.get('command')}"
                if ev.get("error"):
                    text += f"  ->  {ev.get('output')}"
                items.append({"at": at, "kind": "tool", "text": text, "error": bool(ev.get("error"))})
        return items[-limit:] if limit and limit > 0 else items

    def normalize(self, ctx, stage, exclude_message_ids=()):
        rows = list(stream_records(Path(ctx["run_dir"]) / f"{stage}.raw.jsonl"))
        tel = empty_telemetry(session_id=SESSION, model_ids=["simulated"], timestamp_source="arrival",
                              limitations=[LIMITATION])
        if stage != "execute":
            tel["final_text"] = "\n".join(line for _, _, line in rows)
            return tel
        parsed = [(at, ev) for at, ev, _ in rows if isinstance(ev, dict)]
        events, ids, final = [], [], ""
        for i, (at, ev) in enumerate(parsed):
            mid = f"m{i + 1}"
            ids.append(mid)
            if ev.get("kind") == "tool":
                began = parsed[i - 1][0] if i else at  # the call ran during the pause before its output
                call = tool_event(f"t{i + 1}", ev.get("name") or "Bash", {"command": ev.get("command")}, began, mid)
                finish_tool(call, at, bool(ev.get("error")), ev.get("output") or "")
                events.append(call)
            else:
                events.append(text_event(ev.get("text") or "", at, mid))
                final = ev.get("text") or final
        tel.update(events=events, message_ids=ids, final_text=final, tool_status="complete",
                   expected_tool_calls=sum(1 for e in events if e["kind"] == "tool"),
                   stop_reason="success" if len(parsed) == len(STEPS) else "interrupted")
        return tel

    def narrate(self, ctx, prompt):
        tel = read_json(Path(ctx["run_dir"]) / "telemetry.execute.json", {}) or {}
        events = tel.get("events") or []
        text = JOURNEY if len(events) == len(STEPS) else _stopped_journey(events)
        proc = self.run(ctx, [sys.executable, "-I", "-c", ECHO, str(2 * _pace(ctx))], "narrate",
                        stdin_data=text, timeout=60)
        proc.update(session_id=SESSION, tools_disabled=True)
        return proc

    def report(self, ctx, prompt, schema=None):
        stage = ctx.get("stage_name") or "report"
        proc = self.run(ctx, [sys.executable, "-I", "-c", SLEEP, str(3 * _pace(ctx))], stage, timeout=60)
        data = None
        if "asks" in ((schema or {}).get("properties") or {}):
            tel = read_json(Path(ctx["run_dir"]) / "telemetry.execute.json", {}) or {}
            known = {e.get("eid") for e in tel.get("events") or []}
            data = {"asks": [a for a in ASKS if set(a["event_refs"]) <= known],
                    "strengths": [s for s in STRENGTHS if set(s["event_refs"]) <= known],
                    "journey_errata": [], "gates": [], "no_obstacles_observed": False}
        elif "clusters" in ((schema or {}).get("properties") or {}):
            groups = {}
            for path in sorted((Path(ctx["run_dir"]).parent / "runs").glob("*/asks.json")):
                for ask in (read_json(path, {}) or {}).get("asks", []):
                    key = (ask["title"], ask["requested_behavior"], ask["evidence_status"])
                    cluster = groups.setdefault(key, {
                        "title": ask["title"], "requested_behavior": ask["requested_behavior"],
                        "evidence_status": ask["evidence_status"], "members": [],
                        "representative_observation": {"run_id": path.parent.name, "text": ask["how_observed"]}})
                    cluster["members"].append({"run_id": path.parent.name, "ask_id": ask["id"]})
            data = {"clusters": list(groups.values())}
        proc.update(model_ids="simulated", output_tokens=None, result_subtype="success" if data else "unsupported",
                    is_error=data is None)
        return {"proc": proc, "text": json.dumps(data) if data else "", "structured": data}


def _stopped_journey(events):
    lines = ["# Journey: convert three orders from JSON to CSV with orders-cli (stopped early)", "",
             "## Outcome I declared", "I was stopped before I finished, so I declared no outcome.", "",
             "## What I asked for first", "- Nothing yet; I stopped too early to say.", "",
             "## Chronological account"]
    for i, ev in enumerate(events, 1):
        what = ev.get("text") if ev.get("kind") == "text" else f"I ran `{ev.get('input')}`."
        lines.append(f"{i}. {what} [{ev.get('eid')}]")
    if not events:
        lines.append("1. I was stopped before my first step.")
    lines += ["", "## What I still do not know", "Whether the conversion would have worked. The run was stopped "
              "from the AJX Console before I finished, and this account was written from the recorded steps."]
    return "\n".join(lines) + "\n"
