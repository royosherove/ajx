"""Build synthetic run directories for the Go/Python render parity test.

Everything here is invented; no agent, model, or product is involved. Usage:
python3 -I build.py OUT_DIR. Writes OUT_DIR/runs/<run>/..., OUT_DIR/cases.json
(cell, rep, harness facts and spec for each run) and OUT_DIR/clusters.json.
"""
import json
import sys
from pathlib import Path

out = Path(sys.argv[1])
attack = '<img src=x onerror="attack()"> | [bad](javascript:bad) & "q" \'s\' `tick` *star* _u_'


def write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    if isinstance(value, str):
        path.write_text(value, encoding="utf-8")
    else:
        path.write_text(json.dumps(value, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")


def tool(eid, at, end, name, cmd, error, output, mid, duration):
    return {"kind": "tool", "tool_use_id": "t" + eid, "name": name, "input": "command: " + cmd,
            "input_raw": {"command": cmd}, "at": at, "end": end, "is_error": error, "output": output,
            "message_id": mid, "eid": eid, "duration_s": duration}


def text(eid, at, body, mid):
    return {"kind": "text", "text": body, "at": at, "message_id": mid, "eid": eid}


spec = {"sha256": "f" * 64, "teardown": [{"name": "cleanup", "run": "true"}],
        "reporter": {"harness": "claude-code", "model": "reporter-model", "effort": None, "auth": "inherit"},
        "trial": {"id": "parity", "product": "Parity <CLI> & co", "timeout_seconds": 600, "max_budget_usd": 1.5,
                  "output_dir": "", "repetitions": 2, "parallel": 2, "order_seed": 7},
        "task": {"prompt_path": "task-prompt.md", "prompt_sha256": "b" * 64, "human": "unavailable",
                 "materials": ["guide.md"]}}
cases = []

# --- alpha-r1: a rich, legacy-runner run with every limitation source
rd = out / "runs" / "alpha-r1"
events = [
    tool("E-001", "2026-01-01T00:00:01Z", "2026-01-01T00:00:03.250Z", "Bash", "tool --help", False, "usage: tool [opts]", "m1", 2.25),
    text("E-002", "2026-01-01T00:00:05Z", "Two commands look similar | which one?", "m2"),
    tool("E-003", "2026-01-01T00:00:09Z", "2026-01-01T00:00:10Z", "Bash", "tool run --bad", True, "error: unknown flag\nsee --help", "m3", 1.0),
    tool("E-004", "2026-01-01T00:00:11Z", "2026-01-01T00:01:35Z", "Bash", "tool run --good " + "x" * 40, False, "", "m3", 84.0),
    text("E-005", "2026-01-01T00:03:00Z", "Finished; the output looks right.", "m4"),
    tool("E-006", "2026-01-01T00:03:01Z", None, "Read", "notes.md", None, "", None, None),
]
write(rd / "telemetry.execute.json", {
    "session_id": "s1", "model_ids": ["model-a", "model-b"], "final_text": "Done.\nAll | good " + "y" * 30,
    "stop_reason": "success", "events": events,
    "usage": [{"id": "m1", "at": "2026-01-01T00:00:01Z", "output_tokens": 120}, {"id": "m3", "at": "2026-01-01T00:00:09Z", "output_tokens": 340},
              {"id": "m4", "at": "2026-01-01T00:03:00Z", "output_tokens": 77}],
    "usage_status": "complete", "tool_status": "complete", "timestamp_source": "harness",
    "harness_reported": {"ajx_skills_visible": True, "permission_mode": "bypassPermissions", "cost_estimate_usd": 0.0123},
    "isolation_flags": [{"eid": "E-003", "reasons": ["mentions 'ajx'"]}],
    "limitations": ["tool outputs truncated at 1500 chars"], "transcript_path": "/x/sessions/s1.jsonl"})
write(rd / "environment.json", {
    "auth": {"type": "api-key", "profile": "main", "env_names": ["X_KEY"], "identity": "someone@example",
             "problems": ["missing env var X_KEY"]},
    "harness": {"version_before": "claude-code 2.1.0 (Claude Code)"}, "harness_version_after": "2.1.0",
    "product_version_before": "3.4.5", "product_version_after": "3.4.6",
    "probes": {"os": "SyntheticOS 1", "git": "2.40", "node": "", "python": "3.12"},
    "runner": {"type": "local"}, "cache_state": "warm",
    "task_identity": {"ok": False, "cmd": "whoami-check"},
    "env_checks": {"overrides": [{"origin": "trial env", "name": "FOO", "settings_source": "~/.harness/settings.json",
                                  "shell_startup": True, "model_affected": True}], "collisions": ["CLOUD_PROFILE"]}})
write(rd / "verify.json", {"passed": 1, "total": 2, "outcome": "partial", "checks_with_auth_errors": ["cloud check"],
                           "checks": [{"name": "file exists", "passed": True, "type": "shell"},
                                      {"name": "cloud check", "passed": False, "type": "shell"}]})
write(rd / "measurements.json", {"schema_version": 1, "task_summary": {
    "elapsed_seconds": 185.5, "tool_calls": 4, "tool_status": "complete", "failed_tool_calls": 1, "retries_after_error": 1,
    "output_tokens": 537, "usage_status": "complete", "harness_reported": {"cost_estimate_usd": 0.0123}}})
write(rd / "setup.json", [{"cmd": "npm install"}, {"cmd": "make fixtures"}])
write(rd / "asks.json", {
    "asks": [
        {"id": "ASK-001", "title": "Reject | unknown flags " + attack, "requested_behavior": "Name the valid flag.",
         "evidence_status": "verified_defect", "how_observed": "The run failed.\nThen retried.", "consequence": "A retry.",
         "event_refs": ["E-003", "E-004", "E-999"], "journey_anchors": ["step 2"], "labels": ["Roadblock", "Misdirection", "NotALabel"],
         "priority_rationale": "Blocks | the first attempt.", "verification": "Run with --bad.",
         "workaround": "Read --help.", "invalid_refs": ["E-999"],
         "modeled_saving": {"claim": "Saves a retry", "comparator": "baseline", "assumption": "same task", "uncertainty": "high"},
         "measured": {"events": 2, "tool_calls": 2, "failed_tool_calls": 1, "tool_seconds": 85.0, "span_seconds": 86.0,
                      "output_tokens": 340, "shared_events": ["E-003"], "basis": "sum over referenced events"}},
        {"id": "ASK-002", "title": "Explain the two commands", "requested_behavior": "Say which command to use.",
         "evidence_status": "observed_friction", "how_observed": "Ambiguous help.", "consequence": "Hesitation.",
         "event_refs": ["E-001", "E-002", "E-003"], "journey_anchors": [], "labels": ["Fork"],
         "priority_rationale": "Early ambiguity.", "verification": "Read help.",
         "measured": {"events": 3, "tool_calls": 2, "failed_tool_calls": 1, "tool_seconds": 3.25, "span_seconds": 9.0,
                      "output_tokens": 460, "shared_events": ["E-003"], "basis": "sum over referenced events"}},
        {"id": "ASK-003", "title": "No data ask", "requested_behavior": "x", "evidence_status": "untested_risk",
         "how_observed": "y", "consequence": "z", "event_refs": [], "journey_anchors": [], "labels": [],
         "priority_rationale": "p", "verification": "v",
         "measured": {"events": 0, "tool_calls": 0, "failed_tool_calls": 0, "tool_seconds": None, "span_seconds": None,
                      "output_tokens": None, "shared_events": [], "basis": "no events"}}],
    "strengths": [{"title": "Clear help", "how_observed": "The help listed options.", "event_refs": ["E-001"]},
                  {"title": "Unanchored", "how_observed": "Generally fine.", "event_refs": []}],
    "gates": [{"trigger": "Approval prompt", "agent_action": "Waited", "resolved": True, "event_refs": ["E-004"]},
              {"trigger": "Second gate", "agent_action": "Stopped", "resolved": False, "event_refs": []}],
    "journey_errata": [{"journey_claim": "It worked first time", "evidence": "E-003 failed", "event_refs": ["E-003"]}],
    "no_obstacles_observed": False, "extraction_status": "ok",
    "extractor": {"model_ids": "reporter-model", "output_tokens": 900}})
write(rd / "journey.md", "# Journey: alpha\n\n## Outcome I declared\n\nDone.\n\n## Chronological account\n\n"
      "1. 🔀 **Fork**. I read the help [E-001] and wondered [E-002].\n"
      "2. 🚧 Roadblock. The flag failed [E-003]; see [E-003](#E-003) and an unknown [E-099].\n"
      "   It continued [E-004].\n"
      "3. Waited a while [E-005] *quietly* with `code | pipes`.\n"
      "4) ⏳Wait without space [E-006]\n\n## What I still do not know\n\n- [E-002] again\n\n"
      "| a | b |\n|---|---|\n| <x> | [l](http://example.invalid) |\n")
write(rd / "digest.md", "# Evidence digest\n\n| Event | Kind |\n|---|---|\n| E-001 | tool |\n\n```text\nfinal <msg>\n```\n")
state = {"journey_provenance": "self-narrated by the worker (resumed session)",
         "execute": {"started_at": "2026-01-01T00:00:00Z", "stopped_at": "2026-01-01T00:03:05.500Z", "stop_reason": "completed",
                     "config_isolated": True, "requested": {"model": "opus"}},
         "stages": {"prepare": {"status": "done"}, "execute": {"status": "done"}, "verify": {"status": "done"},
                    "teardown": {"status": "done"}, "narrate": {"status": "error", "error": "RuntimeError: boom"},
                    "render": {"status": "done"}},
         "teardown": {"status": "failed", "commands": 2, "failed": 1, "auth_errors": ["denied", "denied again"]}}
write(rd / "state.json", state)
cases.append({"dir": "alpha-r1", "rep": 1, "harness": "claude-code", "clean_supported": True,
              "cell": {"id": "alpha", "harness": "claude-code", "config": "clean", "effort": "high", "args": ["--x"], "model": "opus"}})

# --- beta-r1 / beta-r2: environment and agent configuration profiles
def environment(profile, backend):
    return {"profile_id": profile, "backend": backend,
            "inventory": {"os": "synthetic OS", "tools": [{"name": "sample-tool", "version": "1.0"}]},
            "capabilities": {"filesystem": {"declared": "workspace only", "enforcement": "unsupported", "observed": None},
                             "network": {"state": "observed-only", "observed": "synthetic probe", "evidence": ["probe-1"]},
                             "shell": True, "gpu": "enforced", "secrets": {"enforcement": "weird"}},
            "limitations": ["Host permissions apply; directory isolation is not enforced."],
            "launch": {"env": {"PRIVATE": "private-launch-value"}}, "_internal": 1, "api_token": "private-token"}


def configuration(profile, digest, mode="selected"):
    return {"profile_id": profile,
            "skills": {"mode": mode, "entries": [
                {"name": "product-guide", "content_hash": digest, "version": "1.2",
                 "states": {"installed": True, "discoverable": True, "enabled": True, "loaded_into_context": None, "invoked": "unknown"}},
                {"name": "disabled", "states": {"enabled": False}},
                {"id": "observed", "loaded": True, "executed": False, "sha256": "c" * 8}]},
            "plugins": {"mode": "none"}, "hooks": {"mode": "selected", "selected": ["pre-commit", {"name": "post"}]},
            "uncontrolled_sources": [{"id": "managed-policy", "installed": "unknown"}, "loose-source"],
            "capabilities": [{"name": "skill_discovery", "enforcement": "enforced", "observed": None}, "bare-capability"],
            "limitations": "The adapter does not report skill invocation events.",
            "settings": {"private": "private-settings-value"}}


for rep, digest, cleanup, extraction in ((1, "a" * 64, {"status": "failed", "error": "release " + attack}, "ok"),
                                         (2, "b" * 64, {"status": "released", "confirmed": True, "limitations": ["Expiry cause unknown."]},
                                          "failed: reporter returned no valid register (timeout)")):
    rd = out / "runs" / f"beta-r{rep}"
    actual = environment("resolved-local", "container")
    manifest = configuration("product-guide", digest)
    write(rd / "environment.json", {"harness": {"version_before": "codex-cli 0.9"}, "product_version_before": "3.5.0",
                                    "execution_environment": actual if rep == 1 else None,
                                    "runner": None, "auth": {"type": "inherit", "profile": "inherit"}})
    write(rd / "agent-configuration.json", manifest)
    write(rd / "telemetry.execute.json", {"events": [], "usage_status": "partial", "tool_status": "unavailable",
                                         "model_ids": [], "final_text": ""})
    write(rd / "measurements.json", {"task_summary": {"elapsed_seconds": 2 + rep, "output_tokens": 50 * rep,
                                                      "usage_status": "partial", "tool_calls": rep, "failed_tool_calls": 0,
                                                      "tool_status": "partial", "retries_after_error": 0,
                                                      "harness_reported": {"credits": 1.25 * rep}}})
    asks = {"extraction_status": extraction, "asks": [], "no_obstacles_observed": rep == 1, "strengths": [], "gates": [],
            "journey_errata": [], "extractor": {"model_ids": None}}
    write(rd / "asks.json", asks)
    write(rd / "journey.md", "# Synthetic journey\n\nNo live task was run.\n")
    state = {"journey_provenance": "reconstruction by the reporter",
             "execute": {"started_at": "2026-01-01T00:00:00", "stopped_at": "2026-01-01T00:00:02", "stop_reason": "timeout"},
             "stages": {"execute": {"status": "done"}, "release": {"status": "error", "error": "release failed"}},
             "environment": {"profile_id": "requested-local", "backend": "container", "ownership": "owned",
                             "owner": "synthetic-owner", "env": {"KEY": "private-plan-value"}, "limitations": ["plan note"]},
             "environment_report": actual, "agent_configuration": manifest, "environment_cleanup": cleanup}
    write(rd / "state.json", state)
    cases.append({"dir": f"beta-r{rep}", "rep": rep, "harness": "codex", "clean_supported": False,
                  "cell": {"id": "beta", "harness": "codex", "config": "clean", "environment": "requested-local",
                           "agent_configuration": {"profile_id": "product-guide"}}})

# --- gamma-r1: no journey, no measurements, no verify, skipped teardown
rd = out / "runs" / "gamma-r1"
write(rd / "telemetry.execute.json", {"events": [text("E-001", "2026-01-01T00:00:00+00:00", "only note", None)],
                                     "usage_status": "unavailable", "tool_status": "unavailable", "model_ids": ["k-1"]})
write(rd / "asks.json", {"extraction_status": "skipped: no journey to extract from", "asks": []})
write(rd / "state.json", {"journey_provenance": "unavailable", "stages": {"teardown": {"status": "done"}},
                          "teardown": {"status": "skipped"}, "execute": {}})
cases.append({"dir": "gamma-r1", "rep": 1, "harness": "kiro-cli", "clean_supported": True,
              "cell": {"id": "gamma", "harness": "kiro-cli", "config": "user"}})

# a directory without state.json and a stray file are not runs
(out / "runs" / "not-a-run").mkdir(parents=True)
write(out / "runs" / "stray.txt", "x")
write(out / "matrix-plan.json", {"wall_clock_comparable": False, "note": "parallel"})
write(out / "cases.json", {"spec": spec, "cases": cases})
write(out / "clusters.json", [
    {"id": "AJX-001", "title": "Unknown flags | " + attack, "evidence_status": "verified_defect", "observed_in": 1,
     "members": [{"run_id": "alpha-r1", "ask_id": "ASK-001"}],
     "representative_observation": {"run_id": "alpha-r1", "text": "The run failed."}}])
