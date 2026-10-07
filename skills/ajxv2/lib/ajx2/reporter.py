"""Reporter-side LLM calls: narration prompt, editorial reconstruction, asks extraction.

The reporter is a fresh, clean Claude Code session (fixed per trial so report quality is not a
matrix confounder). ajx2 validates every structured result and computes every number itself.
"""

import json
import uuid
from pathlib import Path

from . import plugins
from .base import stream_records
from .util import SKILL_ROOT, read_json, write_json

PROMPTS = SKILL_ROOT / "prompts"
SCHEMAS = SKILL_ROOT / "schemas"


def _prompt(name, **values):
    text = (PROMPTS / name).read_text(encoding="utf-8")
    for k, v in values.items():
        text = text.replace("{{" + k + "}}", str(v))
    return text


def narrate_prompt(spec, cell, run_id, digest):
    return _prompt("narrate.md", run_id=run_id, product=spec["trial"]["product"],
                   vocabulary=(SKILL_ROOT / "references" / "vocabulary.md").read_text(encoding="utf-8"),
                   digest=digest)


def _reporter_ctx(spec, run_dir, stage):
    """Minimal ctx for a reporter harness call (clean, isolated, no project files visible)."""
    rep = spec["reporter"]
    cell = {"id": "reporter", "harness": rep["harness"], "model": rep.get("model"), "effort": rep.get("effort"),
            "config": "clean", "args": [], "env": {}, "auth": rep.get("auth"), "runner": None}
    from . import spec as specmod
    auth = specmod.auth_for(spec, cell)
    runner = plugins.get("runner", "local")()
    scratch = Path(run_dir) / f"{stage}.reporter"
    scratch.mkdir(exist_ok=True)
    (scratch / "config").mkdir(exist_ok=True)
    return {"spec": spec, "cell": cell, "run_dir": Path(run_dir), "workspace": scratch,
            "config_dir": str(scratch / "config"), "cache_dir": "", "run_token": "",
            "timeout": int(spec["trial"].get("narrate_timeout_seconds") or 900),
            "prompt_text": "", "state": {}, "env": auth.env(), "unset_env": auth.unset(), "runner": runner,
            "auth": auth, "stage_name": stage}


def _reporter_call(spec, run_dir, stage, prompt, schema=None):
    """Run the reporter harness once; return dict(proc, text, structured, output_tokens)."""
    ctx = _reporter_ctx(spec, run_dir, stage)
    harness = plugins.get("harness", spec["reporter"]["harness"])()
    argv = ["claude", "-p", "--output-format", "stream-json", "--verbose", "--safe-mode",
            "--strict-mcp-config", "--tools", "", "--session-id", str(uuid.uuid4())]
    if ctx["cell"].get("model"):
        argv += ["--model", ctx["cell"]["model"]]
    if ctx["cell"].get("effort"):
        argv += ["--effort", ctx["cell"]["effort"]]
    if schema:
        argv += ["--json-schema", json.dumps(schema)]
    argv += ctx["auth"].extra_args()
    env = {"DISABLE_AUTOUPDATER": "1", "CLAUDE_CONFIG_DIR": ctx["config_dir"]} \
        if ctx["auth"].isolates_config else {"DISABLE_AUTOUPDATER": "1"}
    proc = harness.run(ctx, argv, stage, stdin_data=prompt, timeout=ctx["timeout"], extra_env=env)
    text, structured, tokens, model = "", None, None, None
    proc["is_error"] = None
    for _, ev, _ in stream_records(Path(run_dir) / f"{stage}.raw.jsonl"):
        if ev and ev.get("type") == "result":
            text = ev.get("result") or ""
            structured = ev.get("structured_output")
            tokens = (ev.get("usage") or {}).get("output_tokens")
            model = ",".join((ev.get("modelUsage") or {}).keys()) or None
            proc["cost_estimate_usd"] = ev.get("total_cost_usd")
            proc["is_error"] = bool(ev.get("is_error"))
            proc["result_subtype"] = ev.get("subtype")
    proc.update(output_tokens=tokens, model_ids=model, role="reporter")
    if proc.get("timed_out"):
        proc["is_error"] = True
    return {"proc": proc, "text": text, "structured": structured}


def reconstruct(spec, run_dir):
    digest = (Path(run_dir) / "digest.md").read_text(encoding="utf-8")
    prompt = _prompt("reconstruct.md", digest=digest,
                     vocabulary=(SKILL_ROOT / "references" / "vocabulary.md").read_text(encoding="utf-8"))
    res = _reporter_call(spec, run_dir, "reconstruct", prompt)
    if res["proc"].get("is_error"):
        raise RuntimeError(f"reporter reconstruction failed ({res['proc'].get('result_subtype') or 'error'})")
    return {"text": res["text"], "proc": res["proc"]}


ASK_REQUIRED = ("title", "requested_behavior", "evidence_status", "how_observed", "consequence",
                "priority_rationale", "verification")
ERRATA_START, ERRATA_END = "<!-- ajx2:errata:start -->", "<!-- ajx2:errata:end -->"


def _valid_register(data):
    """Shape check for a register that did not come through --json-schema validation."""
    asks = data.get("asks")
    if not isinstance(asks, list):
        return False
    return all(isinstance(a, dict) and all(isinstance(a.get(k), str) for k in ASK_REQUIRED)
               and isinstance(a.get("event_refs", []), list) for a in asks)


def write_errata(run_dir, errata):
    """Append (or replace on rerun) the corrections block; the original journey text is untouched."""
    path = Path(run_dir) / "journey.md"
    text = path.read_text(encoding="utf-8")
    if ERRATA_START in text:
        text = text[:text.index(ERRATA_START)].rstrip() + "\n"
    if errata:
        block = [ERRATA_START, "", "## Corrections added after evidence check", "",
                 "The extraction pass compared this journey with the recorded evidence. The text above is "
                 "preserved as written; these corrections are appended, not merged.", ""]
        block += [f"- {e.get('journey_claim', '')} -> evidence: {e.get('evidence', '')} "
                  f"({', '.join(e.get('event_refs') or [])})" for e in errata]
        block += ["", ERRATA_END, ""]
        text = text.rstrip() + "\n\n" + "\n".join(block)
    path.write_text(text, encoding="utf-8")


def write_asks_stub(run_dir, status, extractor=None):
    write_json(Path(run_dir) / "asks.json", {
        "asks": [], "journey_errata": [], "strengths": [], "gates": [], "no_obstacles_observed": False,
        "extraction_status": status, "extractor": extractor or {}})


def extract(spec, run_dir):
    run_dir = Path(run_dir)
    journey = (run_dir / "journey.md").read_text(encoding="utf-8")
    digest = (run_dir / "digest.md").read_text(encoding="utf-8")
    tel = read_json(run_dir / "telemetry.execute.json", {"events": []})
    valid_ids = [e["eid"] for e in tel["events"]]
    schema = json.loads((SCHEMAS / "asks.schema.json").read_text(encoding="utf-8"))
    prompt = _prompt("extract.md", journey=journey, digest=digest, product=spec["trial"]["product"],
                     valid_ids=", ".join(valid_ids) if valid_ids else "(none: this harness exposes no events; use journey_anchors only)")
    (run_dir / "extract.prompt.md").write_text(prompt, encoding="utf-8")
    res = _reporter_call(spec, run_dir, "extract", prompt, schema)
    data = res["structured"] or {}
    if not data and res["text"]:
        try:
            data = json.loads(res["text"])
        except json.JSONDecodeError:
            data = {}
    extractor = {"model_ids": res["proc"].get("model_ids"), "output_tokens": res["proc"].get("output_tokens")}
    if isinstance(data, dict) and not _valid_register(data):
        data = {}
    if not isinstance(data, dict) or "asks" not in data:
        reason = res["proc"].get("result_subtype") or ("timeout" if res["proc"].get("timed_out") else "no structured output")
        write_asks_stub(run_dir, f"failed: reporter returned no valid register ({reason})", extractor)
        return {"proc": res["proc"], "asks": read_json(run_dir / "asks.json")}
    asks = data.get("asks") or []
    for i, ask in enumerate(asks, 1):
        ask["id"] = f"ASK-{i:03d}"
        ask.setdefault("event_refs", [])
        ask.setdefault("journey_anchors", [])
    from .evidence import ask_costs
    ask_costs(asks, tel)
    out = {"asks": asks, "journey_errata": data.get("journey_errata") or [],
           "strengths": data.get("strengths") or [], "gates": data.get("gates") or [],
           "no_obstacles_observed": bool(data.get("no_obstacles_observed")),
           "extraction_status": "ok", "extractor": extractor}
    write_json(run_dir / "asks.json", out)
    write_errata(run_dir, out["journey_errata"])
    return {"proc": res["proc"], "asks": out}
