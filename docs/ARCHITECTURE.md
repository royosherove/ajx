# AJXv2 architecture

## Roles

- **Coordinator**: the user's agent running `SKILL.md`. Interviews, writes the trial file, confirms the task identity (the account the worker acts in, not the model's credentials) and the plan, runs `ajx2`, summarizes. Never researches the product before the run.
- **ajx2 CLI** (`bin/ajx2`, `lib/ajx2`): deterministic orchestration. Every number in the reports is computed here.
- **Workers**: one fresh harness session per (cell, repetition). Receive only the task prompt bytes. Run in an unnamed random workspace with isolated harness config (where the harness supports it) and isolated package caches. Session-coupling env vars from the parent agent are stripped per harness.
- **Narrator**: the same worker session, resumed after the task with tools disabled, asked for the first-person journey. Falls back to a labeled reconstruction by the reporter when the harness cannot resume.
- **Reporter**: a fresh clean Claude Code session with a fixed model for the whole trial. Produces schema-validated JSON (asks, errata, strengths, gates; optional cross-run clusters). Never computes costs.

## Plugin model

One registry (`plugins.py`) with four kinds, all discovered from `lib/ajx2/builtin/`, `<skill>/plugins/`, `~/.config/ajx2/plugins/`, `<trial>/plugins/`:

| Kind | Contract (`base.py`) | Built in |
|---|---|---|
| harness | `execute(ctx)`, `narrate(ctx, prompt)`, `normalize(ctx, stage)`, `settings_env(env)` (env the harness applies from its own settings), `tool_shell(env)`; flags `can_resume`, `clean_supported`, `verified_live`, `strip_env`, `default_auth` | claude-code, kiro-cli (verified live); codex, gemini-cli, cursor-agent, copilot-cli (documented formats, unverified); `Declarative` from `[adapters.*]` |
| auth | `env()`, `unset()`, `extra_args()`, `prepare(ctx)`, `problems()`, `identity(env)`, `model_vars(env)` (vars the model's access depends on), `isolates_config` | 18 types across providers; `env` generic |
| runner | `wrap(argv, ctx, env)` | local, docker, wrapper |
| check | `run(check, ctx) -> {passed, detail}` | shell, file_exists, http |

Auth values reference the caller's environment as `${VAR}`; only variable names are ever recorded.

## Credentials: three environments (`envcheck.py`)

A cloud task's credentials pass through three environments that can disagree, and a trial is only sound when doctor shows what each one resolves to:

| Environment | Built from | Used by |
|---|---|---|
| harness process | ajx2's worker env (trial `[env]`, cell env, auth env), then the harness's own settings env (Claude Code applies `env` from `settings.json` over its process env, `--safe-mode` included, unless the config dir is isolated) | the model calls |
| agent tool shell | the harness process env, then the startup files of `<shell> -c` (observed: Claude Code runs each Bash call as `$SHELL -c 'source <snapshot> && eval ...'`, not a login shell, so zsh reads `~/.zshenv`; the snapshot restores only functions, options and `PATH`) | the agent's commands |
| check shell | ajx2's env under `/bin/sh`, no settings, no startup files | `[[preflight]]`, `[[verify]]`, `[[teardown]]` |

`doctor` prints the harness auth identity (model credentials) apart from the task identity (`[task] identity_cmd`, run in the agent's tool shell and again in the check shell, failing loudly), the default AWS identity of the agent's tool shell when AWS is involved (a command without `--profile` inherits the model's Bedrock profile), warns when a trial `[env]`/cell env value is replaced before the worker sees it (by harness settings, an adapter's launch env, or shell startup files; `PATH` is reset from Claude Code's snapshot and flagged as unprobed) or matches an auth profile's `model_vars` (Bedrock: the AWS credential variables, not output settings such as `AWS_PAGER`), and runs `[[preflight]]`. `validate` prints the same warnings without running identity commands. The model of the worker env uses a full placeholder run ctx, so plugin `clean_env` hooks see what they see in a run; a failing hook becomes a warning. Values are printed only for allowlisted non-secret names (profiles, regions, projects). Tool shells are observed for Claude Code and labeled `(assumed)` for other harnesses. Only the local runner is probed; for docker/wrapper the worker's shell is elsewhere.

## Run lifecycle (resumable, per run dir `runs/<cell>-r<n>/state.json`)

Rules: a stage runs once its prerequisites reached a terminal status (`done`, `error`, `interrupted`) and degrades gracefully, so a failed or cut-off task still yields `journey.md` (possibly a labeled stub), `asks.md` (with an explicit extraction status), `run.json` and `measurements.json`. `execute` is never retried implicitly; a worker found `running` on resume is marked `interrupted` and its evidence kept. `prepare`/`execute` cannot be redone with `--stages`. Recorded argv is redacted (prompt marker, auth values). Session ids are chosen before launch where the harness allows, so a crash still leaves a findable transcript.

1. **prepare**: random unnamed `workspace`, `config_dir`, `cache_dir` under `workspace_root`; fixture copy; `[[preflight]]` in the verify/teardown env (exit 0 and no credential errors, or the stage fails and no worker starts; `preflight.json`); `[[setup]]`; environment snapshot (tool probes, harness and product versions, auth description and model identity under the harness's effective env, task identity from the agent's tool shell, env overrides and collisions by name and source, runner, cache state, caller env var names that look credential-like).
2. **execute**: `harness.plan()` facts (session id) saved first; worker launch with the prompt on stdin or argv; stdout lines stamped with arrival time (`execute.raw.jsonl`), stdin fed from a thread; process group kill on timeout and again after exit so leftover servers die; stop reason (`exit`, `exit_N`, `timeout`, `interrupted`) recorded separately from outcome.
3. **verify**: `[[verify]]` checks in their own time window -> `verify.json` (k/m passed; outcome succeeded/partial/failed/not_run). Checks whose stderr shows credential or permission errors are listed in `checks_with_auth_errors`, labeled in the digest and in `run.json` limitations: pass or fail, they measured ajx2's credentials (stdout is left alone: it is often the product's own output). Verify, preflight and teardown share one env (the caller's, minus the auth profile's `unset`, plus ajx2's). Any of them can set `allow_auth_errors = true` for a command that expects a denial.
4. **teardown**: always; `$AJX2_RUN_TOKEN` exported; versions re-probed for drift. Also forced on the next invocation if a crash skipped it. Output is scanned for credential errors (heuristic phrases from cloud and SaaS CLIs), so `state.json`/`run.json` record `teardown.status` = ok | failed | auth_errors | skipped regardless of exit code; `ajx2 status` shows `FAIL`/`AUTH`/`SKIP` instead of `ok`, `run`/`status` exit 1, and the report carries a "Cleanup not confirmed" banner. `--stages teardown,render` redoes it and refreshes `run.json` and the reports (the worker's process group is signalled only once, then marked `reaped`, so a redo never hits a reused pid). A prepare retried after a failure (a preflight, a setup command) clears the first attempt's archive state, so the retry gets fresh dirs and is archived in turn.
5. **normalize**: harness adapter -> normalized telemetry; `E-###` ids by timestamp; isolation scan of tool inputs (own paths scrubbed first); `digest.md` (factual timeline bounded to ~300 rows for long tasks, final message verbatim, verification results labeled as reporter work, limitations).
6. **narrate**: resume with tools disabled, prompt = `prompts/narrate.md` + `digest.narrator.md` (the digest without verification results or isolation flags, so the agent's "outcome I declared" is not colored by checks it never ran) -> `journey.md` with a provenance header. Narrate tokens are a separate measurement session (new message ids only). Cannot be redone after archive (the original cwd and config are gone).
7. **extract**: reporter with `--json-schema schemas/asks.schema.json`; event refs validated; costs attached per ask by `evidence.ask_costs` (shared events listed, counted once); errata appended to the journey, never merged. A reporter failure is recorded as `extraction_status` and rendered as a reporting failure, never as a clean run.
8. **measure**: v1 `measure.py` manifest with sessions execute(task), verify(verification), narrate(report), extract(report) -> `measurements.json`.
9. **render**: `run.json` (v1 schema_version 1 fields plus auth/runner/stages), `asks.md` (prioritized table, alternative rankings by measured metrics with unranked-missing, detail, strengths, gates, method), `report.html`.
10. **archive**: `auth.cleanup()` removes credentials an auth profile copied for the run; workspace and harness config moved under the run dir; known credential files purged from the moved config; caches deleted; evidence paths repointed; `workspace_root` left empty so later workers cannot discover siblings.

A `.lock` (pid, host) in the matrix dir makes `ajx2 run` single-instance per matrix; stale locks from dead pids are replaced. `report_instructions_visible_during_task` is derived, not assumed: a Claude Code `config="user"` worker whose session lists an `ajx*` skill is recorded as evaluation-aware.

## Matrix

A cell may list `models = [...]`: it becomes one cell per model (`<id>-<model>`, grouped under `<id>` for `--cells`), each passing its model to the harness's `--model`. `run_plan` interleaves cells (all r1 shuffled, then r2, ...) with a recorded seed. `matrix.json/md/index.html` carry per-run rows, per-cell min/median/max, and comparability notes (prompt hash, product and harness versions seen, concurrency, tokenizer and cost-unit caveats). Optional synthesis clusters asks across runs into `AJX-###` with "observed in k/n"; clustering is labeled inferred.

## Telemetry honesty per harness

| Harness | Timestamps | Output tokens | Tool calls | Money |
|---|---|---|---|---|
| claude-code | transcript event times | per message from on-disk transcript, reconciled to stream `result.usage` (complete) | stream ids == transcript ids (complete) | `total_cost_usd` = harness estimate |
| kiro-cli | ajx2 arrival times | unavailable | partial (no independent total) | credits (harness-reported) |
| codex | arrival | per turn, partial | partial | none |
| gemini-cli | event times | session total (complete, single record) | result.stats vs seen ids | none |
| cursor-agent | arrival | unavailable | partial | none |
| copilot-cli / declarative | process window | unavailable | unavailable | none |

## Review history

See `DESIGN-REVIEW-v0.md` for the first plan and the reviewer's must/should list; all "must" items are implemented: unnamed workspaces and archive, isolated config and caches, version drift probes, interleaved seeded order, tools-disabled narration with verification labeled as reporter work, separate narrate measurement, subagent detection (partial status), ajx2-computed costs, labeled harness cost estimates, process-group kill and teardown-on-resume, doctor identity confirmation, argv-only command templates, per-harness clean-config honesty.
