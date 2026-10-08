# AJX architecture

## Roles

- **Coordinator**: the user's agent running `SKILL.md`. Interviews, writes the trial file, confirms the task identity (the account the worker acts in, not the model's credentials) and the plan, runs `ajx`, summarizes. Never researches the product before the run.
- **ajx CLI** (`bin/ajx`, `lib/ajx`): deterministic orchestration and measured-cost calculations. Modeled savings are separate reporter claims with stated assumptions.
- **Workers**: one fresh harness session per (cell, repetition). Receive only the task prompt bytes. Run in an unnamed random workspace with isolated harness config (where the harness supports it) and isolated package caches. Session-coupling env vars from the parent agent are stripped per harness.
- **Narrator**: the same worker session, resumed after the task with the adapter's tool restrictions, asked for the first-person journey. Falls back to a labeled reconstruction by the reporter when the harness cannot resume.
- **Reporter**: a fresh restricted session in a report-capable harness, with its harness and model fixed for the whole trial. Built-in reporter adapters support Claude Code, Codex, and Kiro CLI. Produces schema-validated JSON (asks, errata, strengths, gates; optional cross-run clusters). Never computes costs.

## Plugin model

One registry (`plugins.py`) with four kinds, all discovered from `lib/ajx/builtin/`, `<skill>/plugins/`, `~/.config/ajx/plugins/`, `<trial>/plugins/`:

| Kind | Contract (`base.py`) | Built in |
|---|---|---|
| harness | `execute(ctx)`, `narrate(ctx, prompt)`, `report(ctx, prompt, schema)`, `normalize(ctx, stage)`, `settings_env(env)` (env the harness applies from its own settings), `tool_shell(env)`; flags `can_resume`, `can_report`, `clean_supported`, `verified_live`, `strip_env`, `default_auth` | claude-code, kiro-cli (workers verified live); codex, gemini-cli, cursor-agent, copilot-cli (documented formats, unverified); `Declarative` from `[adapters.*]`. Reporter adapters have offline command/response tests; see [harness support](HARNESS-SUPPORT.md). |
| auth | `env()`, `unset()`, `extra_args()`, `prepare(ctx)`, `problems()`, `identity(env)`, `model_vars(env)` (vars the model's access depends on), `isolates_config` | 18 types across providers; `env` generic |
| runner | `wrap(argv, ctx, env)` | local, docker, wrapper |
| check | `run(check, ctx) -> {passed, detail}` | shell, file_exists, http |

Auth values reference the caller's environment as `${VAR}`; only variable names are ever recorded.

## Environment and agent profiles

Named `[environments.NAME]` and `[agent_configurations.NAME]` profiles are
selected by each cell or by trial defaults. `environments.py` validates supported
permissions and manages a local process environment or a persistent local Docker
container. It persists ownership before creation, records capability probes,
keeps the environment through narration, and releases only its own resources.
Local profiles do not enforce a filesystem or network sandbox. Container profiles
use pinned images, bounded lifetimes and explicit mounts.

`agent_configuration.py` snapshots and hashes explicitly selected skill trees,
checks harness controls in the execution environment, and verifies prepared
content before worker/narrator launches. Optional plugins/hooks currently support
`none`. Reporter contexts have no worker profile. Runtime manifests distinguish
installed/enabled configuration from unknown or observed activation.

Profiled workers receive a minimal process environment plus declared variables
and required auth variables. Setup, preflight and teardown default to the
environment; verification defaults to the host. Shell commands can explicitly
select `location`. Host commands get private configuration directories and
coordinator tool lookup, excluding worker-writable search paths. Legacy runner
behavior remains unchanged.

`doctor` checks profiles and backend prerequisites without provisioning.
`prepare` performs actual inventory and permission checks before the task.
The [implementation guide](../skills/ajx/references/environments.md) describes the
supported schema, auth requirements and limits.

## Credentials: three environments (`envcheck.py`)

A cloud task's credentials pass through three environments that can disagree, and a trial is only sound when doctor shows what each one resolves to:

| Environment | Built from | Used by |
|---|---|---|
| harness process | ajx's worker env (trial `[env]`, cell env, auth env), then the harness's own settings env (Claude Code applies `env` from `settings.json` over its process env, `--safe-mode` included, unless the config dir is isolated) | the model calls |
| agent tool shell | the harness process env, then the startup files of `<shell> -c` (observed: Claude Code runs each Bash call as `$SHELL -c 'source <snapshot> && eval ...'`, not a login shell, so zsh reads `~/.zshenv`; the snapshot restores only functions, options and `PATH`) | the agent's commands |
| check shell | ajx's env under `/bin/sh`, no settings, no startup files | `[[preflight]]`, `[[verify]]`, `[[teardown]]` |

`doctor` prints the harness auth identity (model credentials) apart from the task identity (`[task] identity_cmd`, run in the agent's tool shell and again in the check shell, failing loudly), the default AWS identity of the agent's tool shell when AWS is involved (a command without `--profile` inherits the model's Bedrock profile), warns when a trial `[env]`/cell env value is replaced before the worker sees it (by harness settings, an adapter's launch env, or shell startup files; `PATH` is reset from Claude Code's snapshot and flagged as unprobed) or matches an auth profile's `model_vars` (Bedrock: the AWS credential variables, not output settings such as `AWS_PAGER`), and runs `[[preflight]]`. `validate` prints the same warnings without running identity commands. The model of the worker env uses a full placeholder run ctx, so plugin `clean_env` hooks see what they see in a run; a failing hook becomes a warning. Values are printed only for allowlisted non-secret names (profiles, regions, projects). Tool shells are observed for Claude Code and labeled `(assumed)` for other harnesses. Only the local runner is probed; for docker/wrapper the worker's shell is elsewhere.

## Run lifecycle (resumable, per run dir `runs/<cell>-r<n>/state.json`)

Rules: post-processing stages can run once their prerequisites reached a terminal status (`done`, `error`, `interrupted`) and degrade gracefully. Execution requires successful preparation. A failed or cut-off task can still yield `journey.md` (possibly a labeled stub), `asks.md` (with an explicit extraction status), `run.json` and `measurements.json`. `execute` is never retried implicitly; a worker found `running` on resume is marked `interrupted` and its evidence kept. `prepare`/`execute` cannot be redone with `--stages`; a failed profiled preparation is also preserved on resume. Recorded argv is redacted (prompt marker, auth values). Session ids are chosen before launch where the harness allows, so a crash still leaves a findable transcript.

1. **prepare**: random unnamed `workspace`, `config_dir`, `cache_dir` under `workspace_root`, plus `home_dir` for explicit environments; fixture copy; durable environment plan, preparation and skill snapshots; auth preparation; `[[preflight]]` at its declared location (exit 0 and no credential errors, or no worker starts); `[[setup]]`; environment snapshot with tool and version probes, profile evidence and credential checks.
2. **execute**: `harness.plan()` facts (session id) saved first; worker launch with the prompt on stdin or argv; stdout lines stamped with arrival time (`execute.raw.jsonl`), stdin fed from a thread; process group kill on timeout, with container-wide cancellation for managed containers. Servers started by the worker may survive a normal exit for verification; teardown reaps local descendants and environment release removes container workers. Stop reason (`exit`, `exit_N`, `timeout`, `interrupted`) is recorded separately from outcome.
3. **verify**: `[[verify]]` checks in their own time window -> `verify.json` (k/m passed; outcome succeeded/partial/failed/not_run). Checks whose stderr shows credential or permission errors are listed in `checks_with_auth_errors`, labeled in the digest and in `run.json` limitations (stdout is left alone: it is often the product's own output). Explicit environments use declared variables and required auth variables; verification defaults to the host, while preflight and teardown default to the environment. Legacy runners retain their inherited check environment. Any command can set `allow_auth_errors = true` when a denial is expected.
4. **teardown**: always; `$AJX_RUN_TOKEN` exported; versions re-probed for drift. Also forced on the next invocation if a crash skipped it. Output is scanned for credential errors (heuristic phrases from cloud and SaaS CLIs), so `state.json`/`run.json` record `teardown.status` = ok | failed | auth_errors | skipped regardless of exit code; `ajx status` shows `FAIL`/`AUTH`/`SKIP` instead of `ok`, `run`/`status` exit 1, and the report carries a "Cleanup not confirmed" banner. `--stages teardown,render` redoes it and refreshes `run.json` and the reports (the worker's process group is signalled only once, then marked `reaped`, so a redo never hits a reused pid). A prepare retried after a failure (a preflight, a setup command) clears the first attempt's archive state, so the retry gets fresh dirs and is archived in turn.
5. **normalize**: harness adapter -> normalized telemetry; `E-###` ids by timestamp; isolation scan of tool inputs (own paths scrubbed first); `digest.md` (factual timeline bounded to ~300 rows for long tasks, final message verbatim, verification results labeled as reporter work, limitations).
6. **narrate**: resume with the adapter's tool restrictions, prompt = `prompts/narrate.md` + `digest.narrator.md` (the digest without verification results or isolation flags, so the agent's "outcome I declared" is not colored by checks it never ran) -> `journey.md` with a provenance header identifying the restrictions. Codex uses its read-only sandbox; this is not a guarantee that no tool runs. Narrate tokens are a separate measurement session (new message ids only). Cannot be redone after archive (the original cwd and config are gone).
7. **release**: for environment profiles, stop/remove the owned environment after narration. Independent reporting uses captured evidence. Cleanup failure is recorded separately from task success. Missing containers are never recreated on resume. Legacy runners have no managed resource to release.
8. **extract**: one configured reporter harness (Claude Code, Codex, Kiro CLI, or a plugin), with a fresh restricted session. Native schema output is requested where supported; every response is also validated locally against `schemas/asks.schema.json`. Event refs are validated; costs are attached per ask by `evidence.ask_costs` (shared events listed, counted once); errata is appended to the journey, never merged. Copied reporter credentials are cleaned up after each call. A reporter failure stays an incomplete review.
9. **measure**: `measure.py` validates sessions execute(task), verify(verification), narrate(report), extract(report) -> `measurements.json`. Reconciled results are in `phase_measurements`; calculation failures are recorded in `measurement_error`.
10. **render**: `run.json`, `asks.md`, `report.html` and `pretty-journey.html` include environment and agent-profile evidence alongside asks, measurements, the event map and happenings table. Reports remain self-contained and escape untrusted content; no unseen branches or skill invocations are inferred.
11. **archive**: release any remaining owned environment; remove copied credentials; move workspace, harness config and agent home under the run directory; purge known credential files; delete caches; update evidence paths. Local isolation limits remain recorded.

A `.lock` (pid, host) in the matrix dir makes `ajx run` single-instance per matrix; stale locks from dead pids are replaced. `report_instructions_visible_during_task` is derived, not assumed: a Claude Code `config="user"` worker whose session lists an `ajx*` skill is recorded as evaluation-aware.

## Matrix

A cell may list `models = [...]`: it becomes one cell per model (`<id>-<model>`, grouped under `<id>` for `--cells`), each passing its model to the harness's `--model`. `run_plan` interleaves cells (all r1 shuffled, then r2, ...) with a recorded seed. `matrix.json/md/index.html` carry per-run rows, per-cell min/median/max, and comparability notes (prompt hash, product and harness versions seen, concurrency, tokenizer and cost-unit caveats). Optional synthesis clusters asks across runs into `AJX-###` with "observed in k/n"; clustering is labeled inferred.

## Telemetry honesty per harness

| Harness | Timestamps | Output tokens | Tool calls | Money |
|---|---|---|---|---|
| claude-code | transcript event times | per message from on-disk transcript, reconciled to stream `result.usage` (complete) | stream ids == transcript ids (complete) | `total_cost_usd` = harness estimate |
| kiro-cli | ajx arrival times | unavailable | partial (no independent total) | credits (harness-reported) |
| codex | arrival | per turn, partial | partial | none |
| gemini-cli | event times | session total (complete, single record) | result.stats vs seen ids | none |
| cursor-agent | arrival | unavailable | partial | none |
| copilot-cli / declarative | process window | unavailable | unavailable | none |

## Operational limits

- Configuration isolation depends on the adapter and authentication profile. A fresh worker process is not a security sandbox.
- A worker orphaned by an AJX crash is reported as interrupted on resume. AJX avoids killing a potentially reused process ID; inspect and clean up remaining work explicitly.
- The lock protects one matrix directory. It does not coordinate distributed execution across machines.
- Narration and extraction are model outputs checked against recorded evidence. Review their claims; valid event references alone do not establish a correct explanation.

## Proposed extensions

[Configurable agent environments](design/execution-environments.md) describes
the longer-term design beyond the initial implementation: additional permission
profiles, selected plugins/hooks, remote backends and optional AWS hosting.

[Live run dashboard](design/live-dashboard.md) describes a future local or remote
view of matrix progress, incremental traces, and artifacts before completion,
including partial-result labels and connection recovery. It can be developed
independently of environment management.

[CI/CD evaluation and improvement loops](design/ci-and-improvement-loops.md)
describes proposed headless evaluation, acceptance policies, pipeline results,
and bounded product changes followed by fresh matrix tests. The current CLI's
process exit status is not the proposed usability acceptance gate.
