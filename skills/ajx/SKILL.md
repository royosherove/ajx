---
name: ajx
description: Orchestrate an Agent Journey Experience (AJX) trial end to end. Interview the user for a real task on a CLI/SDK/API under test, write a trial file, run the task in clean-context worker agents across a matrix of harnesses x models x configs x repetitions, then produce per-run first-person journeys, raw prioritized asks, measurements, and HTML/markdown matrix reports. Use for "AJX test this CLI", "compare how models/harnesses handle this tool", "rerun the AJX matrix", or "regenerate the AJX report".
---

# AJX: orchestrated agent journey trials

You are the coordinator. `bin/ajx` (Python 3.11+, stdlib only) does the deterministic work: fresh unnamed workspaces, worker launches, telemetry normalization, verification, measurement, rendering. It gives workers only the task prompt; configuration isolation depends on the adapter and auth profile, and possible exposure to AJX material is recorded. The method produces a first-person journey by the agent that did the task where supported, asks extracted in a separate pass and checked against evidence, measured costs kept apart from modeled savings, and explicit gaps where evidence is unavailable.

Paths below are relative to this skill directory. Run the CLI as `python3 <skill-dir>/bin/ajx ...`.

## Flow

### 1. Interview (do not research the product first)

Reuse answers already in the conversation. Ask only what changes the trial, with `AskUserQuestion` when a choice is the user's:

- **Product and version source**: name, how a version is printed (for `product_version_cmd`).
- **Task**: propose 2 or 3 candidate outcomes at different depths (local-only, with a real deploy, end to end with cleanup). Each must have an observable result a script can check. The user picks or edits. Do not add defect hunting, AJX words, or hints to the prompt.
- **Customer materials**: what the prompt points the agent to (README URL, docs). Only these go in the prompt.
- **Boundaries**: spend cap, timeout, what must be cleaned up, whether a human can answer questions (default: no, headless).
- **Account the worker acts in** (when the task touches a cloud or SaaS account): profile/project/subscription and region. Before offering a profile as an option, check that it authenticates with the CLI the task uses (`aws sts get-caller-identity --profile <name>`, `gcloud auth list`, `az account show`), and offer only profiles that do, each with the account it resolves to. Never offer an unchecked profile.
- **Model credentials**, asked separately from the account above: which provider each harness uses for its model (`ajx plugins` lists auth types; trial `[auth.*]` profiles reference env vars as `${VAR}`; nothing secret is stored). When the task and the model use the same cloud (Claude Code on Bedrock testing an AWS product), give the model its own explicit profile (`claude-bedrock` with its own `AWS_PROFILE`) and keep the task account out of `[env]`: name it in the prompt and in `--profile` flags.
- **Matrix**: which harnesses and, per harness, which models; configs (`clean` vs `user`); repetitions. "Claude Code with sonnet and opus, codex with example-model-a and example-model-b" is two `[[cells]]`, one with `models = ["sonnet", "opus"]` and one with `models = ["example-model-a", "example-model-b"]`: one cell per model, ids `<cell>-<model>`, and `--cells <cell>` selects the group. Pass model names exactly as the user says them; the harness resolves them and fails at run time on an unknown one. A cell without a model runs the harness default, which with `config = "user"` or `auth = "inherit"` can come from the user's own harness settings; say so. Run `ajx plugins` to show what is installed and which adapters are verified live. Default: one `claude-code` clean cell, repetitions 1.
- **Output dir**: default `ajx-reports/<product>/<trial-id>/` next to the trial file.

Do not look up the product's docs, issues, or defects before the run; that knowledge would leak into the prompt or the verify checks.

### 2. Write the trial

`ajx init <trial-dir> --product <name>` scaffolds `trial.toml` and `task-prompt.md`; fill them from the interview (see `examples/trial.toml` for every option). Keep the prompt exactly as the user approved it; its bytes are hashed. Verify checks must test the user's stated outcome, not the product's internals, and must not require evaluation knowledge. Add a `[[teardown]]` for anything the task creates outside the workspace; `$AJX_RUN_TOKEN` is available to tag and find resources.

For a task that uses an account (see `examples/aws-cloud/`): set `[task] identity_cmd` to show the account the way the agent is told to use it (`aws sts get-caller-identity --profile <task profile>`); add a `[[preflight]]` that authenticates with the credentials verify and teardown use (no worker starts until every preflight passes); have the prompt ask for a tag and tear down by that tag; let teardown commands fail instead of `set +e ... true`. A preflight, verify check or teardown that is meant to hit a denial ("the bucket is private") sets `allow_auth_errors = true` so its credential errors are not counted.

Run `ajx validate trial.toml` then `ajx doctor trial.toml`. Doctor prints, per cell, the harness auth identity (the model's credentials, nothing more), the task identity as the worker's own tool shell resolves it (same env, `$SHELL -c`, harness settings env), the default AWS identity a command without `--profile` acts as (with Bedrock, usually the model's own account), and warnings when an `[env]` value is replaced before the agent sees it or shares variables with the model's credentials; then the task identity and the `[[preflight]]` results in the verify/teardown shell. Show the user the task identity and the run plan and get an explicit go: workers run with all tool permissions auto-approved. Never present the harness auth identity as the account the worker acts in. Do not ask for the go while doctor prints NOT READY. Resolve each env warning in the trial; when one comes from the user's own machine (shell startup files, harness settings), tell the user what the worker will see instead (for example: a command without `--profile` acts as the shell's `AWS_PROFILE`) before asking. Refuse to start against a task identity the user has not confirmed is a sandbox.

### 3. Run

`ajx run trial.toml` in the background (`run_in_background`), then report progress from `ajx status trial.toml`. The runner is resumable: rerun the same command after a crash and it skips completed stages, finishes pending teardowns, and marks a worker that was cut off as `interrupted` (its evidence is kept; the task is never silently re-run). Use `--cells a,b` to run a subset, `--stages` to redo post-processing stages (for example `--stages extract,measure,render` after editing a prompt; `prepare`/`execute` cannot be redone, add a repetition instead), `--keep-workspace` to inspect a workspace, `--no-synthesis` to skip the cross-run clustering call.

A teardown that ran is not a teardown that cleaned up: `ajx status` marks one that failed `FAIL`, printed credential or permission errors `AUTH` (even with exit 0), or never ran `SKIP`, and `run`/`status` then exit 1. Tell the user at once that resources may still exist, and after the credentials are fixed rerun it with `ajx run trial.toml --cells <cell> --stages teardown,render` (render refreshes `run.json` and the reports).

Per run the stages are: prepare (fresh unnamed workspace, isolated harness config and caches where supported, `[[preflight]]`, setup, environment snapshot) -> execute (worker gets only the task prompt) -> verify (ajx checks, own window; servers the agent left running are still up) -> teardown (then leftover processes are reaped) -> normalize (events E-###, digest) -> narrate (resume the worker's own session with tools disabled, fed a digest *without* the verification results so its declared outcome stays its own; fallback: labeled reconstruction) -> extract (fresh reporter session with the full digest, schema-validated asks, event refs checked) -> measure -> render -> archive (workspace and harness config moved under the run dir, copied credentials purged).

One `ajx run` per matrix directory: a `.lock` file refuses a second concurrent run (which would otherwise mark live workers interrupted). A stale lock from a dead process is replaced automatically.

### 4. Deliver

Open `<output>/index.html` (matrix) and `runs/<run>/report.html`. Summarize for the user: verified vs declared outcome per run, wall clock, tool calls, failed calls, tokens (with their coverage status), gates, teardown status, and the top asks with their IDs and evidence anchors. A verify check that printed credential errors measured ajx's credentials, not the agent's work; say so next to its result. Quote the comparability notes (prompt hash, versions, concurrency, tokenizers). State what was not measured. Point to `journey.md`, `asks.md`, `run.json`, `measurements.json` per run and `matrix.md`/`matrix.json`.

Deliver files locally only. Filing tickets, publishing, or re-running paid trials needs the user's scope.

## Extending

- **New harness**: drop `plugins/<name>.py` with a `@register("harness", "<name>")` subclass of `ajx.base.Harness` (execute/narrate/normalize), or declare `[adapters.<name>]` in the trial file for a no-code argv template. Plugin dirs: `<skill>/plugins/`, `~/.config/ajx/plugins/`, `<trial-dir>/plugins/`.
- **New auth target**: `@register("auth", "<name>")` subclass of `Auth` (env/unset/args/identity, plus `model_env` for the variables its provider reads so doctor can flag collisions), or use `type = "env"` in a `[auth.*]` profile. Secrets are referenced as `${VAR}`; recorded argv and environment snapshots carry names only.
- Plugin files are ordinary Python executed by `ajx` (including `<trial-dir>/plugins/`); treat a trial directory from someone else like any other code you run.
- **New execution environment**: `@register("runner", ...)` (local, docker, wrapper exist).
- **New verification type**: `@register("check", ...)` (shell, file_exists, http exist).

Built-in harnesses: `claude-code` and `kiro-cli` are verified live; `codex`, `gemini-cli`, `cursor-agent`, `copilot-cli` follow their documented headless JSON output and are marked unverified until a live run confirms them (run.json records this).

## Rules that keep the trial honest

- Never edit `task-prompt.md` after the user approves it; create a new trial id instead.
- Never put expected problems, workarounds, or AJX vocabulary in the prompt, setup, or verify checks.
- Harness cost figures are estimates; Kiro credits and Claude USD are different units. Output tokens across models are resource usage, not reasoning quality.
- `auth = "inherit"` (the default) keeps the harness's own stored login, so for Claude Code the worker runs with `--safe-mode` but inside the user's config dir (`run.json` records this), and Claude Code applies `env` from that dir's `settings.json` over the worker env. For a fully isolated config dir use an env-driven profile (`claude-bedrock`, `anthropic-api`, `claude-vertex`, `claude-foundry`) or an `inherit` profile with `isolates_config = true`. Shell startup files (`~/.zshenv`) still apply in the agent's tool shell either way; doctor shows what the worker will actually see.
- A run whose `review_status` is `incomplete` has a stub journey or a failed extraction; say so rather than reporting it as a clean run.
- A reconstruction is labeled as such; never present it as the agent's recollection.
- Keep observed cost separate from modeled savings in anything you summarize.
- If a run blocks or times out, the artifacts are still produced; report the stop reason, do not retry silently.
