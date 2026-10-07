# AJXv2 skill: architecture plan (draft for review)

## Goal
One skill that (1) helps the user pick a real task for a CLI/product under test, (2) runs that task
in clean-context worker agents across a matrix of {harness x model x config x repetition},
(3) produces per-run AJX artifacts (first-person journey + raw prioritized asks + run.json +
measurements.json) and (4) a matrix comparison, rendered as clean static HTML plus markdown.

AJX v1 rules still apply (first-person journey by the original executing agent, asks extracted in a
separate pass, measured vs inferred vs modeled, no invented findings, stable IDs, task cost kept
separate from reporting cost). v2 automates the orchestration v1 left to a person.

## Location / layout
```
~/.claude/skills/ajxv2/
  SKILL.md                 coordinator instructions (interview -> trial file -> run -> report)
  bin/ajx2                 deterministic CLI entry point (python3 stdlib only, >=3.11 for tomllib)
  lib/ajx2/                package
    spec.py                trial.toml load + validation + prompt hashing
    harness.py             harness adapters: claude-code, kiro-cli, command (generic template)
    normalize.py           native transcript -> normalized events.jsonl (E-### ids), digest.md
    measure.py             copied from AJX v1 (unchanged), plus manifest builder
    runner.py              cell lifecycle: setup -> execute -> verify -> teardown -> narrate -> extract -> measure -> render
    render.py              markdown->html (tiny renderer), run report, matrix report
    mdlite.py              minimal markdown renderer
  prompts/narrate.md       post-task prompt sent by RESUMING the worker's own session
  prompts/extract.md       asks-extraction prompt for a fresh reporter agent (JSON schema output)
  prompts/synthesize.md    matrix cross-cell ask clustering prompt for a fresh reporter agent
  schemas/asks.schema.json, schemas/clusters.schema.json
  references/              v1 reporting.md + measurement.md vocabulary (copied, referenced by prompts)
  examples/trial.toml      annotated example
  tests/                   unittest suite with fake harness + fixture transcripts
```

## Trial file (trial.toml) - the single source of truth
```toml
[trial]
id = "agentcore-local-01"          # matrix id; output under ajx-reports/<product>/<id>/
product = "agentcore-cli"
product_version_cmd = "agentcore --version"   # recorded before and after; null allowed
workspace_root = "/tmp/ajx2"        # workers run OUTSIDE the report dir (no leakage of verify cmds)
repetitions = 1
parallel = 1
timeout_seconds = 1800
max_budget_usd = 5                  # passed to harnesses that support it

[task]
prompt_file = "task-prompt.md"      # exact bytes hashed (sha256); never edited by ajx2
materials = ["https://github.com/aws/agentcore-cli"]   # recorded; urls are just in the prompt
fixture_dir = ""                    # optional dir copied into each fresh workspace
human = "unavailable"               # headless; any required human action is recorded as a Gate

[[setup]]      # runs before the clock, recorded as preinstalled setup
run = "git init -q"
[[verify]]     # deterministic checks run by ajx2 after the worker stops, own time window
name = "agent project exists"
run = "test -f */pyproject.toml || test -f */package.json"
expect_exit = 0
[[teardown]]   # always runs (e.g. destroy cloud resources)
run = "true"

[reporter]     # fixed across all cells so report quality isn't a confounder
harness = "claude-code"
model = "opus"

[[cells]]
id = "cc-opus"
harness = "claude-code"
model = "opus"
config = "clean"        # clean = --safe-mode --strict-mcp-config (no skills/CLAUDE.md/MCP); "user" = user's normal config
[[cells]]
id = "cc-sonnet"
harness = "claude-code"
model = "sonnet"
[[cells]]
id = "kiro-sonnet"
harness = "kiro-cli"
model = "claude-sonnet-4.5"
[[cells]]
id = "codex"
harness = "command"
command = "codex exec --model {model} --full-auto -C {workspace} {prompt}"   # generic, telemetry = wall clock + stdout only
model = "gpt-5"
```

## CLI commands
- `ajx2 init <dir> --product P` scaffold trial.toml + task-prompt.md
- `ajx2 doctor [trial.toml]` harness presence/versions, python, auth hints
- `ajx2 validate trial.toml` schema check, prompt hash, cell uniqueness, prints plan + estimated runs
- `ajx2 run trial.toml [--cells a,b] [--stages ...] [--dry-run]` run matrix; resumable (skips stages with done markers)
- `ajx2 status <matrix-dir>` per cell stage table
- `ajx2 report <matrix-dir> [--no-synthesis]` (re)render matrix index.html/matrix.md, optional cross-cell clustering
- `ajx2 normalize/measure/render <run-dir>` individual stages for debugging

## Cell lifecycle (each cell x repetition = one ordinary AJX run)
Run dir: `<matrix>/runs/<cell>-r<n>/` ; workspace: `<workspace_root>/<matrix-id>/<cell>-r<n>/`
1. **prepare**: fresh empty workspace, copy fixture, run setup cmds, env snapshot (os, tool versions,
   product version, AWS/ANTHROPIC env var NAMES only), write run.json skeleton. Strip parent-session
   env vars (CLAUDECODE, CLAUDE_CODE_SESSION_ID, CLAUDE_CODE_CHILD_SESSION, CLAUDE_PID, messaging socket...)
   but keep provider auth vars (CLAUDE_CODE_USE_BEDROCK, AWS_*, ANTHROPIC_*).
2. **execute**: worker gets ONLY the task prompt bytes. No AJX text. Pre-assigned session id where the
   harness allows (claude --session-id). Capture stdout lines with arrival timestamps (raw.jsonl), stderr,
   exit code, timeout kill. Record task_started_at/stopped_at and declared outcome (final text).
3. **verify**: ajx2 runs [[verify]] checks in workspace, separate window -> verify.json. Outcome verified =
   succeeded (all pass) / partial / failed / not_run. Never claims more than the checks test.
4. **teardown**: always.
5. **normalize**: adapter turns native transcript into events.jsonl (E-001.. ids: tool_call, tool_result
   with error flag, assistant_text, usage records with dedup by message id, final result) and digest.md
   (compact timeline with E-ids, durations between events, failing tool results excerpts, totals).
   Claude: use on-disk transcript ~/.claude/projects/*/<sid>.jsonl (final per-message usage, dedupe by
   message.id) and reconcile to stream `result.usage.output_tokens` => usage_status complete/partial.
   Kiro: stream-json ACP events (tool_call/tool_call_update/agent_message_chunk/metadata.meteringUsage),
   arrival timestamps, tokens unavailable, credits recorded. Command: wall clock only.
6. **narrate**: RESUME the worker's own session (claude --resume <sid> -p, kiro --resume-id) with
   prompts/narrate.md + digest.md + verify.json. Original agent writes journey.md first-person to a
   given path (fallback: final text). Separate measurement window (reporting scope).
   For harnesses that cannot resume: reporter agent writes a labeled editorial reconstruction.
7. **extract**: fresh reporter agent (fixed harness/model, clean config) gets journey.md + digest +
   events + verify + measurements and returns asks.json via --json-schema. ajx2 validates every
   ask's event refs exist, renders asks.md deterministically (prioritized table + detail, rankings
   by time / tokens / calls / retries / human, missing = unranked).
8. **measure**: build manifest (sessions: execute, verify, narrate, extract as separate windows/scopes)
   and run v1 measure.py -> measurements.json.
9. **render**: report.html (outcome banner, asks preview, journey, asks table, timeline, costs, method).

## Matrix report
`matrix.json` + `index.html` + `matrix.md`: per-cell row (harness, model, config, rep, declared vs
verified outcome, task wall time, output tokens, tool calls, failed tool calls, cost usd/credits if
reported, asks count, gates), plus prompt hash/product version consistency check (flags cells that
differ). Optional synthesis agent clusters asks across cells into canonical `AJX-###` IDs with
"observed in k/n cells" and links to per-run ask IDs. Deterministic fallback = per-cell listing.
Variance across repetitions shown as min/median/max when repetitions > 1. No composite score.

## Determinism levers
- exact prompt bytes + sha256; trial.toml sha256; skill revision fingerprint (hash of skill files)
- fresh workspace per run + fixture copy + setup cmds; workspaces outside the report tree
- clean harness config by default (no user skills/memory/MCP => no AJX exposure in workers)
- deterministic verify checks + teardown; timeouts; budgets
- fixed reporter model across cells; structured JSON output for extraction; deterministic rendering
- resumable stage markers; repetitions for variance; everything recorded in run.json (v1 schema_version 1 + v2 fields)

## Coordinator (SKILL.md) flow
1. Interview the user (AskUserQuestion): product, task outcome (propose 2-3 candidate tasks of different
   depth; the USER decides), customer materials, boundaries (AWS account/spend), cells, repetitions.
   Coordinator must NOT research product docs/defects into the prompt.
2. Write trial.toml + task-prompt.md in the trial dir; `ajx2 validate`; `ajx2 doctor`.
3. Show run plan + cost/permission implications; get explicit go (workers run with bypassed permissions).
4. `ajx2 run` (background); report progress; `ajx2 report`; open index.html; summarize outcome +
   top asks to user, with paths. No publishing/ticket filing.

## Open questions for reviewer
- Is narration-by-resume correct w.r.t. AJX "original agent narrates"? Any contamination concern?
- Parallel cells sharing rate limits distort wall-clock comparisons - default parallel=1?
- Human gates in headless mode: just record, or support scripted replies?
- Is bypassPermissions default acceptable given CLIs may touch cloud accounts?
