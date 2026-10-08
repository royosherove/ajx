---
name: ajx
description: Review how agents experience a CLI, SDK, API, or project while doing a real task. Prepare and run an AJX trial, then deliver evidence-linked product asks, visual journeys, and comparison reports. Use for agent usability reviews, comparing harnesses or models, resuming AJX trials, and regenerating AJX reports.
---

# AJX: orchestrated agent journey trials

AJX uses the common Agent Skills format. Coordinate it from Codex, Claude Code,
Kiro CLI, or another compatible harness with file access and shell execution.
Use the host's available tools for reading files, asking questions, running
commands, and showing reports; no vendor-specific tool name is required.

You are the coordinator. `bin/ajx` (Python 3.11+, stdlib only) does the deterministic work: fresh unnamed workspaces, worker launches, telemetry normalization, verification, measurement, rendering. It gives workers only the task prompt; configuration isolation depends on the adapter and auth profile, and possible exposure to AJX material is recorded. The method produces a first-person journey by the agent that did the task where supported, asks extracted in a separate pass and checked against evidence, measured costs kept apart from modeled savings, and explicit gaps where evidence is unavailable.

Paths below are relative to this skill directory. Run the CLI as `python3 <skill-dir>/bin/ajx ...`.

## Flow

For a complete, already authorized unattended trial, follow
[references/headless.md](references/headless.md). Reuse its supplied brief and
proceed to validation without another interview or approval prompt. Report
missing required inputs or authorization before starting; never invent them.

### 1. Turn the user's goal into a small brief

Reuse the conversation, supplied trial, and current repository context. Do not
research the product's docs, issues, or defects before the task: those findings
could leak into the worker's prompt or success checks. Read AJX's own setup and
adapter instructions as needed; checking installed tool versions is fine.

For a first trial, ask only for missing information in these three areas:

- **Outcome and materials:** what a real user should accomplish, the product or
  checkout, and the instructions that user would receive. If the task is already
  concrete, use it. If it is vague, propose one small, checkable outcome; offer
  alternatives only when a meaningful scope choice remains.
- **Boundaries:** what the worker may access or change, whether humans may help,
  and time, spend, or cleanup constraints. Explain actual process permissions,
  not just what the prompt asks the worker to avoid.
- **Starting conditions:** tools, filesystem/network access, and any requirement
  for a sandbox or particular skills. Ask about these in user terms before
  presenting profile syntax. Only expand cloud, container, or matrix choices
  when requested or needed by the task.

Check prerequisites yourself before asking the user to choose technical details:
Python 3.11+, installed worker and reporter CLIs, `ajx plugins` capabilities and
validation status, available starting tools, and the selected auth route. Do not
inspect or print secret values, initiate login flows, or run a paid model probe
as a setup check. Explain a missing prerequisite and its next action plainly.
A coordinator desktop login is not proof that the worker CLI can authenticate.

Propose one cell and one attempt using the current host's CLI when its adapter
and authentication are available; otherwise explain the installed alternatives.
Use the harness default model unless specified, and name that default choice.
Keep one supported reporter fixed. A fresh local workspace is a simple option
for an authorized low-risk task, but it is not a sandbox. Do not silently switch
an existing-login trial to an explicit profile that cannot use that login.

Use [references/briefing.md](references/briefing.md) for the compact plan format
and conditional environment, account, model-auth, and comparison choices. Use
[references/environments.md](references/environments.md) for explicit profiles.
Avoid a full questionnaire when only one decision is missing. Explain what the
user supplies, what you will handle, and what reports to expect.

### 2. Write and check the plan

`ajx init <trial-dir> --product <name> --harness <chosen-cli>` scaffolds `trial.toml` and `task-prompt.md`; fill them from the brief (see `examples/trial.toml` for every option). Keep the task prompt neutral and within the supplied scope; once approved or launched, preserve its exact bytes. Its bytes are hashed. Verify checks must test the user's stated outcome, not the product's internals, and must not require evaluation knowledge. Add a `[[teardown]]` for anything the task creates outside the workspace; `$AJX_RUN_TOKEN` is available to tag and find resources.

For explicit environment/agent profiles, see `examples/environment-profiles/`.
Select `environment` and `agent_configuration` per cell, or set trial defaults.
Use `config = "clean"` and isolated caches. Keep source skills outside the worker
fixture; AJX copies their pinned snapshots. Setup/preflight/teardown default to
the selected environment, while verification defaults to the host. Set shell
command `location` explicitly when necessary. A check inside the environment
shares its modified tools and files; report that limit.

Keep trial files and real output under an ignored `trials/` directory where
possible. Do not make the user author TOML unless they prefer direct CLI use.
The scaffold contains a placeholder prompt and no active success check: replace
the prompt and add a deterministic check of the requested outcome before launch.
When the task calls for fictional input, prepare a small fixture and reference
it with `task.fixture_dir`; keep the observable expected result in the check.
Do not ask the user to supply fictional records you can construct from the brief.
Record the product version source without inspecting defects or solving the task.

Run `ajx validate trial.toml`, then `ajx doctor trial.toml`. Summarize readiness
and any missing prerequisite. Doctor checks configuration and auth; explicit
profiles also perform runtime checks during preparation. It does not prove the
task will succeed. Do not launch while it reports NOT READY. For account-bearing
tasks, follow the identity/preflight rules in the briefing reference.

Show a compact plan: task, materials, success check, worker/model and fixed
reporter, starting machine and skills, effective access, time/cost limits,
cleanup, and output. Explain that workers may bypass tool approval prompts,
local directories do not enforce isolation, model/reporting calls may cost
money, and budget limits are adapter-dependent. Reporting takes additional time
after the task. Mark inherited configuration and unverified adapter behavior.

Proceed when the existing authorization covers the plan. If the user requested
plan review before running, obtain their go once the concrete plan is ready.
Do not re-ask permission for already authorized steps. An unattended trial uses
its supplied authorization; missing scope is an error, not an interactive wait.

### 3. Run

`ajx run trial.toml` using the host's background-command facility or a persistent terminal, then report progress from `ajx status trial.toml`. The runner is resumable: rerun the same command after a crash and it skips completed stages, finishes pending teardowns, and marks a worker that was cut off as `interrupted` (its evidence is kept; the task is never silently re-run). Use `--cells a,b` to run a subset, `--stages` to redo post-processing stages (for example `--stages extract,measure,render` to rebuild analysis from saved evidence; `prepare`/`execute` cannot be redone, use a fresh output for a new attempt), `--keep-workspace` to inspect a workspace, `--no-synthesis` to skip the cross-run clustering call.

Profiled runs record the actual environment and extension manifest. `doctor`
checks configuration/backend prerequisites; `prepare` checks the real runtime
before the worker starts. A missing/expired container is never recreated on
resume, and a changed profile or prompt requires a new output directory.
The `release` stage follows narration and precedes extraction; a full run releases
its container even with `--keep-workspace`. Failed release is a separate error,
retriable with the unchanged trial and `--stages release,render`.

A teardown that ran is not a teardown that cleaned up: `ajx status` marks one that failed `FAIL`, printed credential or permission errors `AUTH` (even with exit 0), or never ran `SKIP`, and `run`/`status` then exit 1. Tell the user at once that resources may still exist, and after the credentials are fixed rerun it with `ajx run trial.toml --cells <cell> --stages teardown,render` (render refreshes `run.json` and the reports).

Per run the stages are: prepare (fresh directories, environment/profile checks, preflight and setup) -> execute (task prompt only) -> verify (separate check window) -> teardown -> normalize (events and digest) -> narrate (resume with adapter restrictions; labeled reconstruction if unavailable) -> release (owned environment) -> extract (fixed reporter, schema-validated asks and event references) -> measure -> render -> archive (workspace/configuration/home, with copied credentials purged).

One `ajx run` per matrix directory: a `.lock` file refuses a second concurrent run (which would otherwise mark live workers interrupted). A stale lock from a dead process is replaced automatically.

### 4. Deliver

Open `<output>/index.html` (asks by matrix configuration), `runs/<run>/report.html` (asks and verification), and `runs/<run>/pretty-journey.html` (event map, happenings table, and first-person account). Lead with whether the requested outcome was verified, whether cleanup/reporting completed, and the top supported asks. Link each ask to evidence and explain one useful next action. Then summarize details as needed: verified vs declared outcome per run, wall clock, tool calls, failed calls, tokens (with their coverage status), gates, teardown status, and the top asks with their IDs and evidence anchors. A verify check that printed credential errors measured ajx's credentials, not the agent's work; say so next to its result. Quote the comparability notes (prompt hash, versions, concurrency, tokenizers). State what was not measured. Point to `pretty-journey.html`, `journey.md`, `asks.md`, `run.json`, `measurements.json` per run and `matrix.md`/`matrix.json`.

For profiles, include backend, enforced/observed/unsupported controls, selected
skill hashes, unknown or observed activation, and environment cleanup. Link
`environment.json` and `agent-configuration.json`; enabled does not mean used.

Deliver files locally only. Filing tickets, publishing, or re-running paid trials needs the user's scope.

## Extending

- **New harness**: drop `plugins/<name>.py` with a `@register("harness", "<name>")` subclass of `ajx.base.Harness` (execute/narrate/normalize), or declare `[adapters.<name>]` in the trial file for a no-code argv template. Plugin dirs: `<skill>/plugins/`, `~/.config/ajx/plugins/`, `<trial-dir>/plugins/`.
- **Reporter adapter**: implement `Harness.report(ctx, prompt, schema)` and set `can_report = True`. It must start a fresh session with restricted tools and return `{proc, text, structured}`. AJX validates the bundled response schemas and cleans up copied reporter credentials.
- **New auth target**: `@register("auth", "<name>")` subclass of `Auth` (env/unset/args/identity, plus `model_env` for the variables its provider reads so doctor can flag collisions), or use `type = "env"` in a `[auth.*]` profile. Secrets are referenced as `${VAR}`; recorded argv and environment snapshots carry names only.
- Plugin files are ordinary Python executed by `ajx` (including `<trial-dir>/plugins/`); treat a trial directory from someone else like any other code you run.
- **New execution environment**: extend the profile/capability lifecycle in `lib/ajx/environments.py`. Legacy `@register("runner", ...)` wrappers remain available separately.
- **New verification type**: `@register("check", ...)` (shell, file_exists, http exist).

Built-in harnesses: `claude-code` and `kiro-cli` are verified live; `codex`, `gemini-cli`, `cursor-agent`, `copilot-cli` follow their documented headless JSON output and are marked unverified until a live run confirms them (run.json records this).

## Rules that keep the trial honest

- Never edit `task-prompt.md` after the user approves it; create a new trial id instead.
- Never put expected problems, workarounds, or AJX vocabulary in the prompt, setup, or verify checks.
- Harness cost figures are estimates; Kiro credits and Claude USD are different units. Output tokens across models are resource usage, not reasoning quality.
- Inspect configuration and authentication inheritance; follow [references/briefing.md](references/briefing.md) and the stricter explicit-profile rules in [references/environments.md](references/environments.md). A fresh context does not prove a fresh configuration or a permission sandbox.
- A run whose `review_status` is `incomplete` has a stub journey or a failed extraction; say so rather than reporting it as a clean run.
- A reconstruction is labeled as such; never present it as the agent's recollection.
- Keep observed cost separate from modeled savings in anything you summarize.
- If a run blocks or times out, the artifacts are still produced; report the stop reason, do not retry silently.
