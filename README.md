# AJX

AJX (Agent Journey Experience) reviews how a product serves an agent attempting a real task. This repository contains AJX v2: one skill plus a deterministic CLI (`ajx2`) that runs the task in clean-context worker agents across a matrix of harnesses, models, configurations, and repetitions, then produces a journey, a set of evidence-backed asks, and a comparison across runs.

```
interview -> trial.toml + task-prompt.md -> ajx2 run -> runs/<cell>-r<n>/{journey.md, asks.md, run.json, measurements.json, report.html}
                                                      -> index.html, matrix.md, matrix.json
```

## What is different from v1

| | AJX v1 | AJXv2 |
|---|---|---|
| Who runs the task | the user's own agent session | a fresh worker per cell, launched by `ajx2`, that never sees AJX material, the trial file, or other runs |
| Harnesses | whatever the caller is | `claude-code`, `kiro-cli` (verified live); `codex`, `gemini-cli`, `cursor-agent`, `copilot-cli` (documented JSON formats, marked unverified until a live run); any other CLI via a no-code `[adapters.*]` template or a Python plugin |
| Auth targets | implicit | 18 auth profile types (Anthropic API, Bedrock, Vertex, Foundry, Claude subscription, ChatGPT/OpenAI/custom providers, Gemini API/Vertex/login, Cursor, GitHub, Kiro, generic env) with identity checks and no stored secrets |
| Where workers run | locally | `local`, `docker`, or any `wrapper` prefix (ssh, sandbox) |
| Telemetry | manual | normalized per harness: event timestamps, per-message output tokens reconciled against session totals, tool calls with error flags, harness cost estimates labeled as such |
| Verification | by the reporter | deterministic `[[verify]]` checks (`shell`, `file_exists`, `http`, plugins) run in their own window after the agent stops |
| Narration | same session | the worker's own session is resumed with tools disabled; if the harness cannot resume, a labeled editorial reconstruction |
| Asks | extracted by hand | fresh reporter session, schema-validated JSON, event references checked, costs computed by `ajx2` |
| Matrix | not supported | interleaved seeded order, per-cell variance, comparability notes, optional cross-run ask clustering |

The v1 rules still hold: first person journey, asks as a separate pass checked against evidence, measured vs inferred vs modeled, no invented findings, stable IDs, task cost separate from reporting cost.

## Install

```sh
git clone https://github.com/royosherove/ajx.git
cd ajx
mkdir -p ~/.claude/skills
ln -s "$PWD/skills/ajxv2" ~/.claude/skills/ajxv2     # Claude Code
export PATH="$PWD/skills/ajxv2/bin:$PATH"          # current shell
```

Requirements: Python 3.11+ on macOS or Linux (standard library only). Live trials need the selected worker CLI and credentials; the reporter currently uses Claude Code. `ajx2 plugins` lists adapters and installed tools. No build step or cloud account is needed for the offline test suite.

If a skill already exists at the symlink destination, move it aside deliberately before installing. To make the CLI available in later shells, add this checkout's `skills/ajxv2/bin` directory to your shell's `PATH`. The skill remains named `ajxv2` and the CLI remains `ajx2`.

## Use

In Claude Code: `/ajxv2 I want to test <some CLI> for agent usability`, or name the matrix directly (one cell per harness with `models = [...]`, one run per model). Use model identifiers available in your installed harness and account. The coordinator interviews you for the task and boundaries, writes the trial, shows you the account the worker will act in (checked in the worker's own tool shell, separately from the model's credentials) and the run plan, and runs the matrix after your go.

Directly:

```sh
ajx2 init trials/my-cli --product my-cli      # scaffold
$EDITOR trials/my-cli/trial.toml trials/my-cli/task-prompt.md
ajx2 validate trials/my-cli/trial.toml
ajx2 doctor   trials/my-cli/trial.toml        # availability; model vs task identity; env overrides; preflight
ajx2 run      trials/my-cli/trial.toml        # resumable
ajx2 status   trials/my-cli/trial.toml
open trials/my-cli/ajx-reports/my-cli/<id>/index.html
```

See [skills/ajxv2/examples/trial.toml](skills/ajxv2/examples/trial.toml) for every option, [skills/ajxv2/examples/aws-cloud/](skills/ajxv2/examples/aws-cloud/) for a task that uses a cloud account (task identity, credential preflight, tagged teardown, model credentials kept apart), and [skills/ajxv2/SKILL.md](skills/ajxv2/SKILL.md) for the coordinator flow and honesty rules.

Workers execute commands with the permissions of their process. A clean context is not a security sandbox. Use a disposable environment and limited credentials for live trials; cloud examples create billable resources. Reports and archived workspaces can contain task content, identifiers, and command output. Keep them private and review them before sharing; see [SECURITY.md](SECURITY.md).

## Layout

| Path | Purpose |
|---|---|
| `skills/ajxv2/SKILL.md` | coordinator instructions |
| `skills/ajxv2/bin/ajx2` | CLI: init, plugins, doctor, validate, run, status, report |
| `skills/ajxv2/lib/ajx2/` | `spec` (trial file), `plugins` (registry), `base` (contracts), `builtin/` (harness/auth/runner/check plugins), `runner` (lifecycle), `envcheck` (what the worker's harness and tool shell really see, task identity, preflight, credential errors in teardown/verify output), `evidence` (ids, digest, isolation scan, per-ask costs, v1 measurement), `reporter` (narrate/reconstruct/extract calls), `render` (asks.md, run.json, HTML, matrix), `mdlite` (markdown to HTML), `measure_v1.py` (unchanged v1 calculator) |
| `skills/ajxv2/prompts/` | narrate, reconstruct, extract, synthesize prompts |
| `skills/ajxv2/schemas/` | JSON schemas for structured reporter output |
| `skills/ajxv2/references/` | v1 reporting and measurement references, annotation vocabulary |
| `skills/ajxv2/tests/` | offline tests with synthetic harness fixtures and a fake harness |
| `docs/` | design review and code review history |

## Tests

```sh
python3 -I skills/ajxv2/tests/test_ajx2.py
```

The suite is offline and uses a fake `aws` command. It covers adapter parsing with synthetic Claude Code and Kiro streams, trial validation, evidence arithmetic, rendering, argv redaction, full lifecycles with a fake harness, interruption and resume, concurrency, credential cleanup, identity checks, and the example trials. It does not establish live compatibility with every installed harness version.

## Contributing and license

See [CONTRIBUTING.md](CONTRIBUTING.md) for development and local security checks, and [the release checklist](docs/releasing.md) before publishing a release or changing repository visibility.

Licensed under the [MIT license](LICENSE).
