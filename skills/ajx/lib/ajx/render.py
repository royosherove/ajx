"""Deterministic rendering: run.json, asks.md, report.html, matrix.json/md/index.html."""

import html
import json
import re
import statistics
from pathlib import Path

from . import __version__, envcheck, mdlite, report_ui
from .util import SKILL_ROOT, now, read_json, skill_fingerprint, write_json

page = report_ui.page


def _fmt(v, unit=""):
    if v is None:
        return "n/a"
    if isinstance(v, float):
        v = round(v, 1)
    return f"{v}{unit}"


def _cell(value, limit=None):
    """Markdown table cell: no pipes or newlines, optional truncation."""
    text = re.sub(r"\s+", " ", str(value if value is not None else "")).replace("|", "\\|").strip()
    return text if not limit or len(text) <= limit else text[: limit - 1] + "…"


def _run_sort_key(path):
    cell, _, rep = path.name.rpartition("-r")
    return (cell, int(rep) if rep.isdigit() else 0)


def _strip_prefix(version, name):
    if version and name and version.lower().startswith(name.lower()):
        return version[len(name):].strip()
    return version


def _mm(seconds):
    if seconds is None:
        return "n/a"
    return f"{int(seconds // 60)}m {int(seconds % 60):02d}s"


# ----------------------------------------------------------------------------- run.json

def write_run_json(spec, cell, rep, run_dir, state, harness, auth, runner):
    run_dir = Path(run_dir)
    env = read_json(run_dir / "environment.json", {})
    verify = read_json(run_dir / "verify.json", {})
    tel = read_json(run_dir / "telemetry.execute.json", {})
    meas = read_json(run_dir / "measurements.json", {})
    ex = state.get("execute", {})
    limitations = list(tel.get("limitations") or [])
    if env.get("auth", {}).get("problems"):
        limitations += [f"auth: {p}" for p in env["auth"]["problems"]]
    if not harness.clean_supported and cell["config"] == "clean":
        limitations.append(f"{harness.name} cannot disable user config; 'clean' here means only no AJX material was supplied")
    if tel.get("isolation_flags"):
        limitations.append(f"isolation flags on {len(tel['isolation_flags'])} tool call(s); see telemetry.execute.json")
    ajx_visible = bool((tel.get("harness_reported") or {}).get("ajx_skills_visible"))
    if ajx_visible:
        limitations.append("evaluation-aware run: AJX skills were listed in the worker's session (user config)")
    limitations += credential_limitations(env, verify, state.get("teardown"))
    run = {
        "schema_version": 1,
        "ajx_version": __version__,
        "run_id": f"{cell['id']}-r{rep}",
        "mode": "ci" if spec["task"].get("human") == "unavailable" else "human",
        "task_prompt": {"path": spec["task"]["prompt_path"], "sha256": spec["task"]["prompt_sha256"]},
        "task_amendments": [],
        "evaluation_wrapper_sha256": spec["sha256"],
        "ajx_skill_revision": skill_fingerprint(),
        "invocation_timing": "before_task",
        "report_instructions_visible_during_task": ajx_visible,
        "product": {"name": spec["trial"]["product"], "version": env.get("product_version_before"),
                    "version_after": env.get("product_version_after")},
        "agent": {
            "model_id": ",".join(tel.get("model_ids") or []) or None,
            "model_requested": cell.get("model"), "model_version": None,
            "harness_name": harness.name, "harness_version": env.get("harness", {}).get("version_before"),
            "harness_version_after": env.get("harness_version_after"),
            "settings": {k: v for k, v in {"effort": cell.get("effort"), "config": cell["config"],
                                           "permission_mode": tel.get("harness_reported", {}).get("permission_mode"),
                                           "args": cell.get("args")}.items() if v},
        },
        "auth": {k: v for k, v in env.get("auth", {}).items() if k != "identity"} | {"identity_recorded": bool(env.get("auth", {}).get("identity"))},
        "task_identity": {"checked": bool(env.get("task_identity")),
                          "ok": (env.get("task_identity") or {}).get("ok")},   # output stays in environment.json
        "teardown": teardown_record(state, spec),
        "runner": env.get("runner"),
        "starting_conditions": {
            "workspace_revision": None, "os": env.get("probes", {}).get("os"),
            "tools_and_versions": [f"{k}: {v}" for k, v in env.get("probes", {}).items() if k != "os" and v],
            "capabilities": ["shell", "file edits"] + (["web"] if harness.name != "copilot-cli" else []),
            "prior_context": "fresh session; no AJX material; unnamed workspace; " + ("isolated harness config" if ex.get("config_isolated") else "harness user config loaded"),
            "preinstalled_setup": [s["cmd"] for s in read_json(run_dir / "setup.json", [])],
            "cache_state": env.get("cache_state"),
            "credential_scope_description": f"auth profile {env.get('auth', {}).get('profile')} ({env.get('auth', {}).get('type')}); env var names only recorded",
            "customer_materials": spec["task"].get("materials"),
        },
        "constraints": {"human_available": spec["task"].get("human") != "unavailable",
                        "time_limit_seconds": spec["trial"]["timeout_seconds"],
                        "spending_limit": spec["trial"].get("max_budget_usd"),
                        "authorized_operations": ["all tool calls auto-approved in an unnamed workspace"]},
        "task_started_at": ex.get("started_at"), "task_stopped_at": ex.get("stopped_at"),
        "task_stop_reason": ex.get("stop_reason"),
        "task_outcome_declared": (tel.get("final_text") or "")[:2000] or None,
        "task_outcome_verified": verify.get("outcome"),
        "verification_scope": f"{verify.get('passed', 0)}/{verify.get('total', 0)} ajx checks passed; "
                              + "; ".join(c["name"] for c in verify.get("checks", [])) if verify.get("checks") else "no checks defined",
        "journey_provenance": state.get("journey_provenance"),
        "reporter": {k: spec["reporter"].get(k) for k in ("harness", "model", "effort")},
        "review_status": review_status(run_dir, state),
        "evidence": {
            "transcript_path": tel.get("transcript_path") or str(run_dir / "execute.raw.jsonl"),
            "timestamp_coverage": tel.get("timestamp_source") if tel.get("events") else "process window only",
            "usage_coverage": tel.get("usage_status", "unavailable"),
            "tool_call_coverage": tel.get("tool_status", "unavailable"),
        },
        "stages": {k: v.get("status") for k, v in state.get("stages", {}).items()},
        "artifacts": {"journey": "journey.md", "pretty_journey": "pretty-journey.html", "asks": "asks.md", "report": "report.html",
                      "measurements": "measurements.json" if meas else None, "digest": "digest.md",
                      "events": "events.jsonl", "verify": "verify.json" if verify else None},
        "limitations": limitations,
    }
    write_json(run_dir / "run.json", run)
    return run


TEARDOWN_PROBLEMS = {
    "auth_errors": "teardown printed credential or permission errors",
    "failed": "a teardown command failed or timed out",
    "skipped": "teardown did not run (the workspace was missing)",
}


def teardown_record(state, spec):
    if not spec["teardown"]:
        return {"status": "none defined"}
    summary = state.get("teardown")
    if not summary:  # never ran, or ran before ajx recorded a teardown summary
        ran = (state.get("stages", {}).get("teardown") or {}).get("status") == "done"
        return {"status": "unchecked" if ran else "not run"}
    return {k: summary.get(k) for k in ("status", "commands", "failed")} | {"auth_error_lines": len(summary.get("auth_errors") or [])}


def credential_limitations(env, verify, teardown):
    """Limitations for anything that makes the recorded environment, verification or cleanup untrustworthy."""
    out = []
    problem = TEARDOWN_PROBLEMS.get((teardown or {}).get("status"))
    if problem:
        out.append(f"cleanup: {problem}; resources the task created outside the workspace may still exist "
                   "(see teardown.json; redo with --stages teardown,render)")
    names = verify.get("checks_with_auth_errors") or []
    if names:
        out.append(f"verify: {len(names)} check(s) printed credential or permission errors ({'; '.join(names)}); "
                   "their pass/fail reflects ajx's verification credentials, not only the agent's work")
    out += [f"env: {note}" for note in envcheck.recorded_notes(env)]
    ident = env.get("task_identity")
    if ident and not ident.get("ok"):
        out.append(f"task identity check ({ident.get('cmd')}) failed in the worker's tool shell before the task started")
    return out


def review_status(run_dir, state):
    """ready = both primary artifacts are usable; a stub journey or a failed extraction is not."""
    run_dir = Path(run_dir)
    asks = read_json(run_dir / "asks.json", {})
    journey_ok = (run_dir / "journey.md").exists() and state.get("journey_provenance") not in (None, "unavailable")
    asks_ok = asks.get("extraction_status") == "ok"
    return "ready" if journey_ok and asks_ok else "incomplete"


# ----------------------------------------------------------------------------- asks.md

def _rank(asks, key, label, lower_is_worse=False):
    vals = [(a["id"], a["measured"].get(key)) for a in asks]
    ranked = sorted([v for v in vals if v[1] is not None], key=lambda x: x[1], reverse=not lower_is_worse)
    missing = [v[0] for v in vals if v[1] is None]
    line = ", ".join(f"{i} ({v})" for i, v in ranked) or "none measurable"
    if missing:
        line += f"; unranked (no data): {', '.join(missing)}"
    return f"- **{label}** (measured, incurred): {line}"


def asks_markdown(run, asks_data, tel):
    asks = asks_data.get("asks") or []
    lines = [f"# Asks: {run['product']['name']} ({run['run_id']})", "",
             f"Harness {run['agent']['harness_name']} {run['agent']['harness_version'] or ''}, model {run['agent']['model_id'] or run['agent']['model_requested'] or 'default'}, "
             f"product version {run['product']['version'] or 'unknown'}. Task outcome declared by the agent is recorded in run.json; "
             f"verified outcome: **{run['task_outcome_verified']}** ({run['verification_scope']}).", "",
             "Every ask below was extracted from `journey.md` and checked against `events.jsonl`. Measured costs are "
             "sums over the referenced events, computed by ajx, per ask; events shared between asks are listed so the "
             "overlap is visible (run-level totals in measurements.json count each event once). Priorities are "
             "rationales, not scores.", ""]
    status = asks_data.get("extraction_status", "unknown")
    if status != "ok":
        lines += ["## Extraction did not complete", "", f"Status: **{status}**. No asks were extracted; this is a reporting "
                  "failure, not evidence of a clean run. Rerun with `ajx run <trial> --stages extract,measure,render`.", ""]
    elif asks_data.get("no_obstacles_observed") and not asks:
        lines += ["## No obstacles observed", "", "The journey and telemetry show no supported ask. This is a legitimate result.", ""]
    elif not asks:
        lines += ["## No asks extracted", "", "The extraction pass returned an empty register without declaring "
                  "`no_obstacles_observed`; treat as needs review.", ""]
    if asks:
        lines += ["## Prioritized register", "",
                  "| ID | Ask | Status | Labels | Events | Tool calls (failed) | Span | Tokens | Priority rationale |",
                  "|---|---|---|---|---|---|---|---|---|"]
        for a in asks:
            m = a["measured"]
            lines.append(f"| {a['id']} | {_cell(a['title'])} | {a['evidence_status']} | {_cell(', '.join(a.get('labels') or []))} | "
                         f"{_cell(', '.join(a.get('event_refs') or []) or (', '.join(a.get('journey_anchors') or []) or 'none'))} | "
                         f"{m['tool_calls']} ({m['failed_tool_calls']}) | {_fmt(m['span_seconds'], ' s')} | {_fmt(m['output_tokens'])} | "
                         f"{_cell(a['priority_rationale'])} |")
        lines += ["", "Default order: as prioritized by the extraction pass using task impact and supported cost. Alternative rankings:", "",
                  _rank(asks, "span_seconds", "Time span"), _rank(asks, "output_tokens", "Output tokens"),
                  _rank(asks, "tool_calls", "Tool calls"), _rank(asks, "failed_tool_calls", "Failed calls"), ""]
        gates = asks_data.get("gates") or []
        lines.append(f"- **Human intervention**: {len(gates)} gate(s) recorded" + (": " + "; ".join(g['trigger'] for g in gates) if gates else ""))
        lines += ["", "## Detail", ""]
        for a in asks:
            m = a["measured"]
            lines += [f"### {a['id']}: {a['title']}", "",
                      f"- **Requested behavior:** {a['requested_behavior']}",
                      f"- **Status:** {a['evidence_status']}; product version {run['product']['version'] or 'unknown'}",
                      f"- **How observed:** {a['how_observed']}",
                      f"- **Consequence:** {a['consequence']}",
                      f"- **Evidence:** events {', '.join(a.get('event_refs') or []) or 'none'}; journey anchors {', '.join(a.get('journey_anchors') or []) or 'none'}"
                      + (f"; invalid refs dropped: {', '.join(a['invalid_refs'])}" if a.get("invalid_refs") else ""),
                      f"- **Measured cost:** {m['tool_calls']} tool call(s), {m['failed_tool_calls']} failed, tool time {_fmt(m['tool_seconds'], ' s')}, "
                      f"span {_fmt(m['span_seconds'], ' s')}, output tokens {_fmt(m['output_tokens'])}"
                      + (f"; shared events: {', '.join(m['shared_events'])}" if m.get("shared_events") else "")
                      + f". Basis: {m['basis']}.",
                      f"- **Modeled value:** " + (f"{a['modeled_saving']['claim']} (comparator: {a['modeled_saving']['comparator']}; assumption: {a['modeled_saving']['assumption']}; uncertainty: {a['modeled_saving']['uncertainty']})" if a.get("modeled_saving") else "not modeled"),
                      f"- **Workaround:** {a.get('workaround') or 'none recorded'}",
                      f"- **Priority:** {a['priority_rationale']}",
                      f"- **Verification:** {a['verification']}", ""]
    if asks_data.get("strengths"):
        lines += ["## Strengths observed", ""]
        lines += [f"- **{s['title']}**: {s['how_observed']} ({', '.join(s.get('event_refs') or []) or 'no anchor'})" for s in asks_data["strengths"]]
        lines.append("")
    if asks_data.get("gates"):
        lines += ["## Gates", ""]
        lines += [f"- {g['trigger']} ; agent: {g['agent_action']} ; resolved: {g['resolved']} ({', '.join(g.get('event_refs') or [])})" for g in asks_data["gates"]]
        lines.append("")
    if asks_data.get("journey_errata"):
        lines += ["## Journey corrections from the evidence check", ""]
        lines += [f"- Journey said: {e['journey_claim']} ; evidence: {e['evidence']} ({', '.join(e.get('event_refs') or [])})" for e in asks_data["journey_errata"]]
        lines.append("")
    lines += ["## Method and limits", "",
              f"- Journey provenance: {run.get('journey_provenance')}",
              f"- Extraction: {status}; fresh reporter session ({asks_data.get('extractor', {}).get('model_ids')}), structured output validated against schema; event references validated against the run's events.",
              f"- Telemetry coverage: usage {run['evidence']['usage_coverage']}, tool calls {run['evidence']['tool_call_coverage']}, timestamps {run['evidence']['timestamp_coverage']}.",
              "- Limitations:"] + [f"  - {l}" for l in run.get("limitations") or []] or ["  - none"]
    return "\n".join(lines) + "\n"


# ----------------------------------------------------------------------------- run report

def run_report(run_dir):
    run_dir = Path(run_dir)
    run = read_json(run_dir / "run.json", {})
    asks_data = read_json(run_dir / "asks.json", {"asks": []})
    tel = read_json(run_dir / "telemetry.execute.json", {"events": []})
    meas = read_json(run_dir / "measurements.json", {})
    (run_dir / "asks.md").write_text(asks_markdown(run, asks_data, tel), encoding="utf-8")
    run["review_status"] = review_status(run_dir, {"journey_provenance": run.get("journey_provenance")})
    run.setdefault("artifacts", {})["pretty_journey"] = "pretty-journey.html"
    write_json(run_dir / "run.json", run)
    journey_path = run_dir / "journey.md"
    journey = journey_path.read_text(encoding="utf-8") if journey_path.exists() else ""
    digest_path = run_dir / "digest.md"
    digest = digest_path.read_text(encoding="utf-8") if digest_path.exists() else ""
    summary = meas.get("task_summary") or {}
    agent = run.get("agent") or {}
    product = run.get("product") or {}
    subtitle = (f"{product.get('version') or 'Unknown product version'} / {agent.get('harness_name')} / "
                f"{agent.get('model_id') or agent.get('model_requested') or 'default model'} / {run.get('run_id')}")
    example = bool(run.get("example"))
    body = report_ui.run_body(run, asks_data, summary, digest, bool(journey))
    (run_dir / "report.html").write_text(page(f"{product.get('name')}: product asks", body, subtitle,
                                               view="report", example=example), encoding="utf-8")
    body = report_ui.journey_body(run, asks_data, tel, summary, journey)
    (run_dir / "pretty-journey.html").write_text(page(f"{product.get('name')}: agent journey", body, subtitle,
                                                       view="journey", example=example), encoding="utf-8")


# ----------------------------------------------------------------------------- matrix

def _cell_row(run_dir):
    run = read_json(run_dir / "run.json", {})
    meas = read_json(run_dir / "measurements.json", {})
    asks = read_json(run_dir / "asks.json", {"asks": []})
    state = read_json(run_dir / "state.json", {"stages": {}})
    s = meas.get("task_summary", {})
    hr = s.get("harness_reported") or {}
    return {
        "run_id": run.get("run_id") or run_dir.name, "dir": run_dir.name,
        "harness": run.get("agent", {}).get("harness_name"),
        "harness_version": _strip_prefix(run.get("agent", {}).get("harness_version"), run.get("agent", {}).get("harness_name")),
        "model_requested": run.get("agent", {}).get("model_requested"), "model_id": run.get("agent", {}).get("model_id"),
        "config": run.get("agent", {}).get("settings", {}).get("config"), "auth": run.get("auth", {}).get("type"),
        "product_version": run.get("product", {}).get("version"),
        "declared": (run.get("task_outcome_declared") or "")[:160], "verified": run.get("task_outcome_verified"),
        "stop_reason": run.get("task_stop_reason"), "elapsed_seconds": s.get("elapsed_seconds"),
        "tool_calls": s.get("tool_calls"), "tool_status": s.get("tool_status"), "failed_tool_calls": s.get("failed_tool_calls"),
        "retries": s.get("retries_after_error"), "output_tokens": s.get("output_tokens"), "usage_status": s.get("usage_status"),
        "cost_estimate_usd": hr.get("cost_estimate_usd"), "credits": hr.get("credits"),
        "asks": len(asks.get("asks") or []), "gates": len(asks.get("gates") or []),
        "ask_register": asks.get("asks") or [],
        "no_obstacles_observed": asks.get("no_obstacles_observed", False),
        "defects": sum(1 for a in asks.get("asks") or [] if a.get("evidence_status") == "verified_defect"),
        "journey_provenance": "self" if (run.get("journey_provenance") or "").startswith("self") else
                              ("unavailable" if run.get("journey_provenance") == "unavailable" else
                               ("reconstruction" if run.get("journey_provenance") else None)),
        "extraction_status": asks.get("extraction_status"),
        "review_status": run.get("review_status"), "stages": {k: v.get("status") for k, v in state.get("stages", {}).items()},
        "errors": [f"{k}: {v.get('error')}" for k, v in state.get("stages", {}).items() if v.get("status") == "error"],
        "teardown": (run.get("teardown") or {}).get("status"),
        "verify_auth_errors": len(read_json(run_dir / "verify.json", {}).get("checks_with_auth_errors") or []),
        "limitations": len(run.get("limitations") or []),
    }


def _variance(rows, key):
    if key == "output_tokens":  # partial subtotals are not totals; only reconciled counts aggregate
        rows = [r for r in rows if r.get("usage_status") == "complete"]
    vals = [r[key] for r in rows if r.get(key) is not None]
    if not vals:
        return None
    return {"n": len(vals), "min": min(vals), "median": statistics.median(vals), "max": max(vals)}


def matrix_report(spec, synthesize=True, log=print):
    out = Path(spec["trial"]["output_dir"])
    runs_dir = out / "runs"
    rows = [_cell_row(d) for d in sorted(runs_dir.iterdir(), key=_run_sort_key)
            if d.is_dir() and (d / "state.json").exists()] if runs_dir.exists() else []
    plan = read_json(out / "matrix-plan.json", {})
    by_cell = {}
    for r in rows:
        by_cell.setdefault(r["run_id"].rsplit("-r", 1)[0], []).append(r)
    cells = []
    for cid, cr in by_cell.items():
        cells.append({"cell": cid, "runs": len(cr), "harness": cr[0]["harness"], "model_requested": cr[0]["model_requested"],
                      "model_ids": sorted({r["model_id"] for r in cr if r["model_id"]}), "config": cr[0]["config"],
                      "verified": [r["verified"] for r in cr],
                      "elapsed_seconds": _variance(cr, "elapsed_seconds"), "tool_calls": _variance(cr, "tool_calls"),
                      "failed_tool_calls": _variance(cr, "failed_tool_calls"), "output_tokens": _variance(cr, "output_tokens"),
                      "asks": _variance(cr, "asks")})
    consistency = {
        "prompt_sha256": spec["task"]["prompt_sha256"],
        "product_versions": sorted({str(r["product_version"]) for r in rows}),
        "harness_versions": sorted({f"{r['harness']} {r['harness_version']}" for r in rows}),
        "wall_clock_comparable": plan.get("wall_clock_comparable", True),
        "token_note": "output tokens use each model's tokenizer; compare across models as resource usage, not reasoning; "
                      "per-cell token variance includes only runs with complete (reconciled) usage",
        "cost_note": "cost_estimate_usd (Claude Code) and credits (Kiro) are harness estimates in different units; never summed or sorted across harnesses",
    }
    clusters = None
    if synthesize and len(rows) > 1:
        try:
            clusters = synthesize_clusters(spec, out, rows, log)
        except Exception as exc:  # noqa: BLE001
            log(f"synthesis skipped: {exc}")
    matrix = {"schema_version": 1, "trial": {k: spec["trial"][k] for k in ("id", "product", "repetitions", "parallel", "order_seed")},
              "plan": plan, "consistency": consistency, "runs": rows, "cells": cells, "clusters": clusters, "generated_at": now()}
    write_json(out / "matrix.json", matrix)

    md = [f"# AJX matrix: {spec['trial']['product']} / {spec['trial']['id']}", "",
          f"Task prompt sha256 `{consistency['prompt_sha256'][:16]}…`; {len(rows)} run(s) across {len(cells)} cell(s); "
          f"order seed {spec['trial']['order_seed']}; parallel {spec['trial']['parallel']}"
          + ("" if consistency["wall_clock_comparable"] else " (**concurrent runs: wall clock not comparable**)") + ".", "",
          "## Runs", "",
          "| Run | Harness | Model (observed) | Config | Verified | Declared (excerpt) | Stop | Wall clock | Tool calls | Failed | Retries | Output tokens | Asks (defects) | Gates | Journey | Report |",
          "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for r in rows:
        md.append(f"| {r['run_id']} | {_cell(r['harness'])} {_cell(r['harness_version'])} | {_cell(r['model_id'] or r['model_requested'] or 'default')} | {r['config']} | "
                  f"**{r['verified']}** | {_cell(r['declared'], 90)} | {r['stop_reason']} | {_mm(r['elapsed_seconds'])} | "
                  f"{_fmt(r['tool_calls'])} ({r['tool_status']}) | {_fmt(r['failed_tool_calls'])} | {_fmt(r['retries'])} | {_fmt(r['output_tokens'])} ({r['usage_status']}) | "
                  f"{r['asks']} ({r['defects']}) | {r['gates']} | {r['journey_provenance'] or 'missing'} | [report](runs/{r['dir']}/report.html) |")
    if any(r["errors"] for r in rows):
        md += ["", "Stage errors:", ""] + [f"- {r['run_id']}: {'; '.join(r['errors'])}" for r in rows if r["errors"]]
    cleanup = [r for r in rows if r["teardown"] in TEARDOWN_PROBLEMS]
    if cleanup:
        md += ["", "Cleanup not confirmed (resources the task created may still exist; see each run's teardown.json):", ""]
        md += [f"- {r['run_id']}: {TEARDOWN_PROBLEMS[r['teardown']]}" for r in cleanup]
    creds = [r for r in rows if r["verify_auth_errors"]]
    if creds:
        md += ["", "Verify checks that hit credential errors (their result reflects ajx's credentials, not the agent's work):", ""]
        md += [f"- {r['run_id']}: {r['verify_auth_errors']} check(s); see verify.json" for r in creds]
    if int(spec["trial"]["repetitions"]) > 1:
        md += ["", "## Per-cell variance (min / median / max)", "",
               "| Cell | Runs | Verified outcomes | Wall clock s | Tool calls | Failed | Output tokens | Asks |", "|---|---|---|---|---|---|---|---|"]
        for c in cells:
            def v(x):
                return f"{_fmt(x['min'])} / {_fmt(x['median'])} / {_fmt(x['max'])}" if x else "n/a"
            md.append(f"| {c['cell']} | {c['runs']} | {', '.join(map(str, c['verified']))} | {v(c['elapsed_seconds'])} | {v(c['tool_calls'])} | "
                      f"{v(c['failed_tool_calls'])} | {v(c['output_tokens'])} | {v(c['asks'])} |")
    md += ["", "## Comparability", ""] + [f"- {k}: {v}" for k, v in consistency.items()]
    if clusters:
        md += ["", "## Asks across runs (clustered; clustering is inferred by the reporter model)", "",
               "| Cluster | Ask | Status | Observed in | Members |", "|---|---|---|---|---|"]
        for c in clusters:
            md.append(f"| {c['id']} | {c['title']} | {c['evidence_status']} | {c['observed_in']}/{len(rows)} runs | "
                      + ", ".join(f"[{m['run_id']}/{m['ask_id']}](runs/{m['run_id']}/asks.md)" for m in c["members"]) + " |")
        md += ["", "Representative observations:", ""]
        md += [f"- **{c['id']} {c['title']}** ({c['representative_observation']['run_id']}): {c['representative_observation']['text']}" for c in clusters]
    else:
        md += ["", "## Asks per run", ""]
        for r in rows:
            a = read_json(runs_dir / r["dir"] / "asks.json", {"asks": []})
            listing = "; ".join(f"{x['id']} {x['title']}" for x in a.get("asks") or []) or "none"
            tag = "" if a.get("extraction_status") == "ok" else f" [extraction {a.get('extraction_status', 'missing')}]"
            md.append(f"- **{r['run_id']}** ({len(a.get('asks') or [])}){tag}: {listing}")
    (out / "matrix.md").write_text("\n".join(md) + "\n", encoding="utf-8")

    registers = {r["dir"]: read_json(runs_dir / r["dir"] / "asks.json", {"asks": []}) for r in rows}
    body = report_ui.matrix_body(spec, rows, cells, consistency, registers, "\n".join(md), clusters)
    (out / "index.html").write_text(page(f"{spec['trial']['product']}: compare runs", body,
                                        f"{spec['trial']['id']} / {len(rows)} runs / {len(cells)} configurations",
                                        view="matrix", example=bool(spec["trial"].get("example"))), encoding="utf-8")
    return matrix


def synthesize_clusters(spec, out, rows, log):
    from . import reporter
    registers = []
    for r in rows:
        a = read_json(out / "runs" / r["dir"] / "asks.json", {"asks": []})
        if not a.get("asks"):
            continue
        registers.append(f"## Run {r['run_id']} ({r['harness']} / {r['model_id'] or r['model_requested']})\n\n" + "\n".join(
            f"- {x['id']}: {x['title']} [{x['evidence_status']}] requested: {x['requested_behavior']} observed: {x['how_observed'][:400]}"
            for x in a["asks"]))
    if len(registers) < 2:
        return None  # clustering needs at least two registers to compare
    schema = json.loads((SKILL_ROOT / "schemas" / "clusters.schema.json").read_text(encoding="utf-8"))
    prompt = reporter._prompt("synthesize.md", product=spec["trial"]["product"], registers="\n\n".join(registers))
    syn_dir = out / "synthesis"
    syn_dir.mkdir(exist_ok=True)
    res = reporter._reporter_call(spec, syn_dir, "synthesize", prompt, schema)
    if res["proc"].get("is_error"):
        raise ValueError("reporter returned an invalid clustering response")
    data = res["structured"] or {}
    known = {(r["run_id"], a["id"]) for r in rows
             for a in read_json(out / "runs" / r["dir"] / "asks.json", {}).get("asks", [])}
    clusters = []
    for i, c in enumerate(data.get("clusters") or [], 1):
        if any((m["run_id"], m["ask_id"]) not in known for m in c["members"]):
            raise ValueError("cluster references an unknown run or ask")
        runs_seen = {m["run_id"] for m in c["members"]}
        if c["representative_observation"]["run_id"] not in runs_seen:
            raise ValueError("cluster observation is not from a member run")
        clusters.append({"id": f"AJX-{i:03d}", **c, "observed_in": len(runs_seen)})
    write_json(syn_dir / "clusters.json", {"clusters": clusters, "reporter": res["proc"]})
    return clusters
