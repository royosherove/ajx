"""Fictional report fixtures generated offline through the production renderer."""

import json
from datetime import datetime, timedelta, timezone
from pathlib import Path

from . import __version__, evidence, render
from .base import empty_telemetry, finish_tool, text_event, tool_event
from .util import read_json, sha256_bytes, write_json

TASK = "Export the 100 supplied fictional records as a CSV file and verify the row count."
START = datetime(2026, 1, 1, 10, tzinfo=timezone.utc)


def _at(seconds):
    return (START + timedelta(seconds=seconds)).isoformat().replace("+00:00", "Z")


def _ask(title, request, observation, consequence, refs, labels, priority, verification, workaround=None):
    return {"title": title, "requested_behavior": request, "evidence_status": "observed_friction",
            "how_observed": observation, "consequence": consequence, "event_refs": refs,
            "journey_anchors": [], "labels": labels, "priority_rationale": priority,
            "verification": verification, "workaround": workaround, "modeled_saving": None}


def _run(out, variant, spec):
    baseline, gated = variant == "baseline", variant == "headless"
    harness = "kiro-cli" if gated else "codex"
    version = "1.2.0" if baseline else "1.3.0"
    rid = f"{variant}-{harness}-r1"
    rd = out / "runs" / rid
    rd.mkdir(parents=True, exist_ok=True)
    # Every command and response is invented here, never executed.
    if baseline:
        records = [
            (0, 2, "demo-export --help", False, "Commands: export, dump. Both write records to a file."),
            (8, None, "I could use export or dump. The help does not explain which produces CSV.", False, ""),
            (16, 18, "demo-export export --format csv --out report.csv", True, "Unknown option: --out"),
            (29, 31, "demo-export export --help", False, "Use --output FILE. The quick start still shows --out."),
            (38, 85, "demo-export export --format csv --output report.csv", False, "Export started.\nComplete: 100 records."),
            (57, None, "The export has been quiet. I cannot tell whether it is still working.", False, ""),
            (90, None, "I trust the command help over the older quick start after the flag was rejected.", False, ""),
            (96, 98, "demo-export inspect report.csv", False, "Format: CSV. Data rows: 100. Validation: passed."),
            (105, None, "I exported the records and verified that the CSV contains 100 data rows.", False, ""),
        ]
        steps = [
            "1. 🔀 Fork. I read the help [E-001] and found two export commands without a selection rule [E-002].",
            "2. 🚧 Roadblock. I copied the quick-start flag. It failed [E-003]. The command help supplied a working alternative [E-004].",
            "3. ⏳ Wait. I started the corrected export [E-005]. While it ran, I recorded that there was no progress signal [E-006].",
            "4. 🧭 Trust Decision. I switched from the quick start to command help after the mismatch [E-007].",
            "5. ✅ Delight. The inspect command gave a machine-readable row count and a validation result [E-008].",
            "6. I declared the export complete after the row-count check [E-009].",
        ]
        asks = [
            _ask("Make the quick-start export command executable",
                 "Use the supported --output flag in the quick start, with a copyable CSV example.",
                 "The documented --out flag failed. Command help showed --output, which worked.",
                 "The first attempt failed and required a help lookup before retrying.", ["E-003", "E-004", "E-005"],
                 ["Roadblock", "Misdirection"], "Fix the broken first attempt before reducing optional friction.",
                 "Run the documented command in a fresh workspace; it must produce a 100-row CSV.",
                 "Replace --out with --output."),
            _ask("Expose progress while a CSV export is running",
                 "Emit bounded progress on stderr and a final structured completion result.",
                 "Only start and completion messages appeared during the 47-second export.",
                 "The agent could not distinguish productive work from a stalled command.", ["E-005", "E-006"],
                 ["Wait", "Blind Spot"], "Make long-running work observable without extra polling.",
                 "Run a delayed export; progress must remain separate from CSV stdout."),
            _ask("Explain when to choose export instead of dump",
                 "State the output formats and intended use of both commands in top-level help.",
                 "Both commands were described as writing records; the required CSV path was unclear.",
                 "The agent had to choose without a selection signal.", ["E-001", "E-002"],
                 ["Fork"], "Remove the ambiguous choice at the start of the task.",
                 "A user reading only top-level help can identify the CSV command."),
        ]
        strengths = [{"title": "A verifiable row count", "how_observed": "Inspect confirmed CSV format and 100 data rows.",
                      "event_refs": ["E-008"]}]
        gates, seconds, passed = [], 108, 2
    elif gated:
        records = [
            (0, 2, "demo-export export --help", False, "Use --output FILE. Exports require workspace approval."),
            (8, 11, "demo-export export --format csv --output report.csv", True, "Open the approval page to continue."),
            (17, None, "I cannot open an approval session or ask a human in this headless task.", False, ""),
            (24, None, "The export is blocked by required approval. I did not create the CSV.", False, ""),
        ]
        steps = [
            "1. I read the documented command [E-001].",
            "2. 🚪 Gate. The export required interactive approval [E-002]. I had no human available [E-003].",
            "3. 🧱 Wall. I stopped without creating the CSV because no authorized unattended path was available [E-004].",
        ]
        asks = [_ask("Check headless authorization before starting an export",
                     "Provide a non-interactive authorization preflight and document the required human approval.",
                     "The command requested browser approval after the agent had started the task.",
                     "The unattended task stopped without an output file.", ["E-002", "E-003", "E-004"],
                     ["Gate", "Wall"], "Expose the prerequisite early so teams can prepare an authorized run.",
                     "In headless mode, preflight reports the missing approval before an export begins.")]
        strengths = []
        gates = [{"trigger": "Workspace approval requires a human", "agent_action": "Stopped and reported the prerequisite.",
                  "resolved": False, "event_refs": ["E-002", "E-003"]}]
        seconds, passed = 25, 0
    else:
        records = [
            (0, 2, "demo-export --help", False, "Use export for CSV; dump writes a JSON snapshot."),
            (6, 26, "demo-export export --format csv --output report.csv", False, "Export started.\nComplete: 100 records."),
            (15, None, "The export is running, but I have no intermediate progress signal.", False, ""),
            (29, 31, "demo-export inspect report.csv", False, "Format: CSV. Data rows: 100. Validation: passed."),
            (33, None, "I created the CSV and verified all 100 data rows.", False, ""),
        ]
        steps = [
            "1. ✅ Delight. Help explicitly directed me to the CSV export command [E-001].",
            "2. ⏳ Wait. The documented command ran [E-002], but I still had no intermediate progress signal [E-003].",
            "3. ✅ Delight. Inspect confirmed the format and row count [E-004].",
            "4. I declared the requested export complete [E-005].",
        ]
        asks = [_ask("Expose progress while a CSV export is running",
                     "Emit bounded progress on stderr and a final structured completion result.",
                     "Only start and completion messages appeared during the 20-second export.",
                     "The agent could not observe intermediate progress.", ["E-002", "E-003"],
                     ["Wait"], "The remaining observed friction is visibility during execution.",
                     "Run a delayed export; progress must remain separate from CSV stdout.")]
        strengths = [{"title": "The help identifies the CSV path", "how_observed": "Top-level help explains export versus dump.", "event_refs": ["E-001"]},
                     {"title": "A verifiable row count", "how_observed": "Inspect confirms the requested output.", "event_refs": ["E-004"]}]
        gates, seconds, passed = [], 34, 2
    tel = empty_telemetry(model_ids=["example-model"], usage_status="unavailable" if gated else "partial",
                          tool_status="partial", timestamp_source="synthetic timestamps",
                          limitations=["Fictional fixture. No product or model was run."])
    for i, (start, end, action, failed, output) in enumerate(records):
        mid = f"example-message-{i}"
        if end is None:
            event = text_event(action, _at(start), mid)
        else:
            event = tool_event(f"example-tool-{i}", "shell", {"command": action}, _at(start), mid)
            finish_tool(event, _at(end), failed, output)
        tel["events"].append(event)
        if not gated:
            tel["usage"].append({"id": mid, "at": _at(start), "output_tokens": 130 + i * 47})
    tel["final_text"] = records[-1][2]
    evidence.assign_ids(tel)
    for i, ask in enumerate(asks, 1):
        ask["id"] = f"ASK-{i:03d}"
    evidence.ask_costs(asks, tel)
    data = {"asks": asks, "strengths": strengths, "gates": gates, "journey_errata": [],
            "extraction_status": "ok", "no_obstacles_observed": False, "extractor": {"model_ids": "fictional reporter"}}
    state = {"execute": {"started_at": _at(0), "stopped_at": _at(seconds), "exit_code": 0,
                         "stop_reason": "completed", "timed_out": False},
             "stages": {s: {"status": "done"} for s in ("execute", "verify", "narrate", "extract", "measure", "render")}}
    summary = evidence.task_summary(state, tel)
    outcome = "succeeded" if passed else "failed"
    verify = {"passed": passed, "total": 2, "outcome": outcome, "checks_with_auth_errors": [],
              "checks": [{"name": name, "passed": bool(passed), "type": "synthetic"}
                         for name in ("CSV exists", "CSV contains 100 data rows")]}
    run = {"schema_version": 1, "ajx_version": __version__, "example": True, "run_id": rid,
           "product": {"name": spec["trial"]["product"], "version": version},
           "agent": {"harness_name": harness, "harness_version": "synthetic fixture", "model_id": "example-model",
                     "model_requested": "example-model", "settings": {"config": "clean"}},
           "auth": {"type": "synthetic; no credentials"}, "runner": {"type": "offline example"},
           "task_prompt": {"path": "task-prompt.md", "sha256": spec["task"]["prompt_sha256"]},
           "evaluation_wrapper_sha256": sha256_bytes(b"AJX fictional example"), "ajx_skill_revision": "example",
           "task_outcome_verified": outcome, "task_outcome_declared": tel["final_text"],
           "verification_scope": f"{passed}/2 fictional checks passed", "task_stop_reason": "completed",
           "task_started_at": _at(0), "task_stopped_at": _at(seconds),
           "journey_provenance": "FICTIONAL FIRST-PERSON ACCOUNT: hand-authored demonstration, not a real agent recollection.",
           "review_status": "ready", "teardown": {"status": "none defined"},
           "evidence": {"usage_coverage": tel["usage_status"], "tool_call_coverage": "partial",
                        "timestamp_coverage": "synthetic timestamps"},
           "limitations": ["All content and metrics are invented for this example.",
                           "A change across these fictional runs demonstrates the report, not a measured improvement.",
                           "Token subtotals are partial; the headless example intentionally has no usage telemetry."]}
    journey = ("# Journey: export 100 records to CSV\n\n"
               "> Fictional first-person account. No agent performed this task.\n\n"
               "## Outcome I declared\n\n" + tel["final_text"] + "\n\n## Chronological account\n\n"
               + "\n\n".join(steps) + "\n\n## What I still do not know\n\n"
               "These fictional observations do not establish behavior for any real product or agent.\n")
    for name, value in (("run.json", run), ("state.json", state), ("asks.json", data), ("verify.json", verify),
                        ("telemetry.execute.json", tel), ("measurements.json", {"schema_version": 1, "task_summary": summary})):
        write_json(rd / name, value)
    (rd / "journey.md").write_text(journey, encoding="utf-8")
    (rd / "events.jsonl").write_text("".join(json.dumps(e, ensure_ascii=False) + "\n" for e in tel["events"]), encoding="utf-8")
    digest = evidence.build_digest(spec, {"harness": harness, "model": "example-model", "config": "clean"},
                                   state, tel, verify, {})
    (rd / "digest.md").write_text("> Fictional example; every event below is synthetic.\n\n" + digest, encoding="utf-8")
    render.run_report(rd)


def generate(output):
    """Create or refresh only an AJX-owned example directory; never call an agent."""
    out = Path(output)
    marker = out / "example.json"
    if out.exists() and any(out.iterdir()) and read_json(marker, {}).get("generator") != "ajx-fictional-example":
        raise ValueError("Choose an empty directory; the example generator will not overwrite existing work.")
    out.mkdir(parents=True, exist_ok=True)
    write_json(marker, {"generator": "ajx-fictional-example", "synthetic": True})
    (out / "task-prompt.md").write_text(TASK + "\n", encoding="utf-8")
    spec = {"trial": {"id": "csv-export-example", "product": "Demo Export CLI", "example": True,
                       "output_dir": str(out), "repetitions": 1, "parallel": 1, "order_seed": 0},
            "task": {"prompt_sha256": sha256_bytes((TASK + "\n").encode())}}
    write_json(out / "matrix-plan.json", {"wall_clock_comparable": True, "note": "Synthetic, sequential examples."})
    for variant in ("baseline", "revised", "headless"):
        _run(out, variant, spec)
    render.matrix_report(spec, synthesize=False)
    (out / "README.md").write_text(
        "# Fictional AJX example\n\nOpen `index.html` in a browser for the comparison. "
        "Each run includes `report.html` and `pretty-journey.html`.\n\n"
        "Every event, command, version, model name, and cost is invented. No agent or product "
        "was executed. These pages demonstrate the renderer, not benchmark results.\n",
        encoding="utf-8")
    return out
