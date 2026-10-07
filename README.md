# AJX

See where AI agents struggle with your product, what it costs them, and what to fix.

AJX (Agent Journey Experience) is an open-source method and toolkit for reviewing how an AI agent uses a product to complete a real task. It turns recorded sessions into a chronological journey and a prioritized list of product changes, linked to evidence, measured costs, and ways to verify each fix.

This is **AJX v1**, the first public version.

## Who it is for

AJX is for developers, open-source maintainers, and product teams building CLIs, APIs, SDKs, and documentation that agents use. Run a review when you want to understand a specific task: where the agent got stuck, what it had to work around, which outputs it relied on, and whether it reached the intended result.

A command can complete successfully while leaving an agent unsure what happened next. A task can finish after expensive retries or human intervention. AJX records the path to the result so those costs remain visible.

## What you get

| Output | What it helps you do |
|---|---|
| `asks.md` | Prioritize concrete product changes. Each ask includes the observation, supporting evidence, available time/token/tool-call costs, a priority rationale, and verification steps. Modeled savings are optional and state their assumptions. |
| `journey.md` | Follow what the agent tried, what the product returned, and how later actions followed from earlier results. The original agent narrates where supported; reconstructions are labeled. |
| `run.json`, `measurements.json` | Inspect the outcome, configuration, measurement sources, and gaps in the evidence. |
| `report.html` | Read a run's asks, journey, timeline, and limitations in a local browser. |
| `index.html`, `matrix.md`, `matrix.json` | Compare repetitions or agent configurations for the same task, with comparability notes. |

Start with the asks to decide what to investigate. Follow their evidence references when reproducing an observation. The report also records helpful product behavior, required human intervention, and friction attributable to the agent or environment.

Measured cost describes the run that happened. A modeled saving describes a possible improvement. A comparable follow-up run is needed to measure what a fix actually changed. Missing measurements remain missing.

## How a review works

1. **Define a real task.** Supply the instructions and materials a user would have, success criteria, and boundaries. Keep expected defects and reporting instructions out of the task prompt.
2. **Run the task.** AJX starts a fresh worker session for each configuration and repetition, records available evidence, and runs your verification checks after the worker stops.
3. **Review the journey and asks.** Narration and extraction happen after task execution. Code computes measured costs from telemetry; the reporting model explains observations and proposed changes.
4. **Fix and rerun.** Preserve the baseline, remove the workaround for the fix being tested, and run a new comparable trial. Check both the specific behavior and the complete task outcome.

Read the [FAQ](docs/FAQ.md) for scope, cost, evidence limits, and comparisons.

## Install

Requirements: Python 3.11+ on macOS or Linux. AJX uses the Python standard library. Live reviews also need the selected agent CLI and credentials; report generation currently uses Claude Code.

```sh
git clone https://github.com/royosherove/ajx.git
cd ajx
mkdir -p ~/.claude/skills
ln -s "$PWD/skills/ajx" ~/.claude/skills/ajx
export PATH="$PWD/skills/ajx/bin:$PATH"
ajx --version
```

If a skill already exists at the destination, move it aside deliberately before installing. The `PATH` change applies to the current shell; add this checkout's `skills/ajx/bin` directory to your shell configuration to keep it.

## Run your first review

In Claude Code:

```text
/ajx Review my CLI by having an agent complete a real task with its public documentation.
```

The coordinator helps define the task, agent configuration, verification checks, and permitted actions. It shows the plan and relevant account identities before starting a live trial.

To configure a trial directly:

```sh
ajx init trials/my-cli --product my-cli
$EDITOR trials/my-cli/trial.toml trials/my-cli/task-prompt.md
ajx validate trials/my-cli/trial.toml
ajx doctor trials/my-cli/trial.toml
# Review the plan, credentials, permissions, and possible costs before running:
ajx run trials/my-cli/trial.toml
ajx status trials/my-cli/trial.toml
```

Open the `index.html` path printed by the runner. Local trials belong in the ignored `trials/` directory.

The [annotated trial file](skills/ajx/examples/trial.toml) covers the available settings. The [jq example](skills/ajx/examples/jq-smoke/) provides a small local task; its live agents still consume model usage. The [cloud example](skills/ajx/examples/aws-cloud/) shows account checks and teardown for a task that creates AWS resources.

## Supported agent CLIs

The bundled adapters cover Claude Code, Kiro CLI, Codex CLI, Gemini CLI, Cursor Agent, and GitHub Copilot CLI. Claude Code and Kiro CLI have been exercised in live development runs. The other adapters are marked unverified until their formats and behavior are confirmed in a live run.

```sh
ajx plugins
```

This lists available adapters, installed tools, and verification status. Token coverage, session resumption, and configuration isolation differ by CLI. Use model identifiers supported by your installed CLI and account. See the [FAQ](docs/FAQ.md) and [architecture](docs/ARCHITECTURE.md) for details.

## Running safely

Workers execute commands with the permissions of their process and may bypass interactive tool approvals. A fresh context is not a security sandbox. Use disposable environments and limited credentials, review setup and teardown commands, and investigate cleanup failures.

Reports and archived workspaces may contain task content, account identifiers, or command output. Keep them private and review them before sharing. See [SECURITY.md](SECURITY.md).

## Develop and contribute

The offline suite uses synthetic fixtures and fake agents; it needs no model credentials or cloud account:

```sh
python3 -I skills/ajx/tests/test_ajx.py
```

The [coordinator skill](skills/ajx/SKILL.md), [reporting method](skills/ajx/references/reporting.md), and [measurement protocol](skills/ajx/references/measurement.md) describe how reviews work. Implementation and extension points are in [ARCHITECTURE.md](docs/ARCHITECTURE.md).

See [CONTRIBUTING.md](CONTRIBUTING.md) for development and security checks, and [the release checklist](docs/releasing.md) before publishing a release.

Licensed under the [MIT license](LICENSE).
