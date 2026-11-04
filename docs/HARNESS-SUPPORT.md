# Use AJX with your agent

The `skills/ajx` directory is a standard Agent Skills package: `SKILL.md` has a
name and description, and the remaining files supply instructions, scripts,
schemas, and report assets. Install the complete directory. The coordinator
needs file access and a shell. The native installation needs no Python;
the source checkout and Python plugins need Python 3.11+.

## Install and invoke

| Host | Personal skill directory | Explicit invocation |
|---|---|---|
| Codex | `~/.agents/skills/ajx` | `$ajx` |
| Claude Code | `~/.claude/skills/ajx` | `/ajx` |
| Kiro CLI | `~/.kiro/skills/ajx` | `/ajx` |
| Another Agent Skills host | Its documented skill directory | Its skill picker or “Use the AJX skill…” |

Start with the [one-line installer](../README.md#1-install-the-cli-and-skill) and
[first-run guide](GETTING-STARTED.md). Start a new session after installing.
The installer registers all three skill paths above. Use
[`--skill-dir`](INSTALL.md#updates-options-and-existing-installations) for a
single host or another compatible host.
The host's skill mechanism loads the coordinator instructions; the worker's
headless CLI is selected separately in the trial.

Kiro's default agent discovers personal and workspace skills. A custom Kiro
agent needs a resource entry such as `skill://~/.kiro/skills/ajx/SKILL.md` in its
existing `resources` list. Keep its other resource entries.

Installation references:

- [Agent Skills format](https://agentskills.io/specification)
- [Codex skills](https://developers.openai.com/codex/skills/)
- [Claude Code skills](https://code.claude.com/docs/en/skills)
- [Kiro skills](https://kiro.dev/docs/skills/)

## Worker and reporter adapters

Any compatible host can coordinate AJX. Automated trials need a worker adapter
for the CLI they launch, and a reporter adapter for evidence analysis.

| CLI | Worker | Reporter | Current validation |
|---|---|---|---|
| Claude Code | Included | Included; tools disabled | Worker exercised live during development; reporter covered by offline tests |
| Codex CLI | Included | Included; read-only sandbox | Documented CLI contract and offline tests; live adapter validation pending |
| Kiro CLI | Included | Included; no trusted tools in headless mode | Worker exercised live during development; reporter covered by offline tests |
| Gemini CLI, Cursor Agent, GitHub Copilot CLI | Included | Use one of the above or a plugin | Live adapter validation pending |
| Custom headless CLI | Command template or Python plugin | Python plugin implementing `report()` | Validate against the installed CLI |

These statuses describe the adapters. They do not limit which Agent Skills host
can load and coordinate the skill. Offline tests do not establish compatibility
with every CLI version or model provider.

A Claude Code session can emit more than one `result` event when the agent
schedules its own continuation. The adapter sums turns, durations and token
usage across those segments, reports the cumulative cost, records the segment
count as `result_events`, and notes the aggregation in the run's limitations.
Other adapters keep single-result handling until a live run shows otherwise.

```sh
ajx init trials/example-task --product my-cli --harness kiro-cli
ajx plugins
ajx doctor trials/example-task/trial.toml
```

`init` sets the selected worker as the reporter when it supports reporting.
Choose another reporter with `--reporter codex`, for example. When a hand-written
trial omits `reporter.harness`, AJX selects the first worker harness that has a
reporter adapter, with a Claude Code fallback if none does. The selection stays
fixed across the trial; it does not vary with each matrix cell.

```toml
[reporter]
harness = "codex"       # claude-code | codex | kiro-cli | a reporter plugin
# model = "your-model"
# auth = "your-profile"
```

AJX validates extraction and clustering responses against its bundled schemas,
including responses from CLIs without native schema support. Invalid output is
an incomplete reporting result. It does not silently become an empty register.
Copied reporter credentials are cleaned up after success or failure.

Codex's reporter sandbox permits read-only commands. Kiro retains user
configuration and reports that limit. A fresh session or restricted tool policy
is not a guarantee of complete filesystem or context isolation.

## Choose a model

`ajx init` and the web console show model suggestions for the selected harness,
for both the worker and reporter. Use the model picker rather than guessing a
provider's spelling. Leave the model empty for the agent's default. You can
always enter a custom model ID, including a gateway or cloud-provider ID.

Changing a worker's Agent CLI also updates its generated name, such as
`kiro-cli-default` to `claude-code-default`. Custom names such as `baseline`
stay unchanged. The model picker refreshes for the new agent; choose its model
and sign-in method before saving.

| Harness | Examples offered | How to check your account's models |
|---|---|---|
| Claude Code | `sonnet`, `opus`, `haiku`, `fable`, pinned Claude IDs | `/model` in Claude Code |
| Codex | `gpt-6.1-sol`, `gpt-6.1-terra`, `gpt-6-astra`, `gpt-6-luna` | `/model` in Codex |
| Kiro CLI | Claude Opus, Sonnet and Haiku versions; GPT-5.6 Sol, Terra and Luna; `auto` | `kiro-cli chat --list-models` |
| Gemini CLI | Gemini Pro, Flash and Flash Lite versions; automatic routing | `/model` in Gemini CLI |
| Cursor Agent | GPT-5.6 variants, Composer, `auto`; custom IDs for other providers | `agent models` or `cursor-agent --list-models` |
| Copilot CLI | Claude, OpenAI and Gemini choices; `auto` | `/model` in Copilot CLI |

For Kiro and Copilot, AJX expands `opus`, `sonnet`, and `haiku` into the matching
dotted IDs before saving a new choice. For example, `--harness kiro-cli --model
opus` writes `claude-opus-5.5`. In Claude Code, `opus` stays an alias so that
Claude can resolve it for your provider. Codex does not accept the `opus` alias;
AJX asks you to choose a Codex model before creating or saving that plan.
Existing trials and recorded runs are not silently migrated to newer models.

Short OpenAI names work too: with Codex, `sol` becomes `gpt-6.1-sol`, `terra`
becomes `gpt-6.1-terra`, `astra` becomes `gpt-6-astra`, and `luna` becomes
`gpt-6-luna`. With Kiro or Cursor, `sol`, `terra`, and `luna` select their
GPT-5.6 versions. The full saved ID makes the choice explicit for comparisons.

The bundled catalog was checked on **November 4, 2026** against the providers'
documentation. Suggestions do not confirm access: CLI version, sign-in method,
subscription, region, and organization policy can affect availability.
If a suggested model is unavailable, update the harness or choose a model its
own picker offers. AJX does not make a paid model call to populate its picker.

The shared [catalog](../skills/ajx/assets/model-catalog.json) records its sources:
[Claude Code](https://code.claude.com/docs/en/model-config),
[Codex](https://developers.openai.com/codex/models/),
[Kiro](https://kiro.dev/docs/models/),
[Gemini CLI](https://geminicli.com/docs/cli/model/),
[Cursor](https://cursor.com/docs/cli/reference/parameters), and
[Copilot CLI](https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-command-reference).

## Explicit worker skill profiles

[Environment and agent profiles](../skills/ajx/references/environments.md) add
`none`, `selected` and explicit `snapshot` skill modes for Codex, Claude Code and
Kiro CLI workers. They use fresh configuration directories and capability checks
inside the selected local/container environment. They do not change the fixed
reporter's configuration.

These controls require supported CLI versions and isolated authentication.
Codex can copy its login; Claude profiles use API/cloud-provider auth; Kiro
profiles require an explicit `KIRO_API_KEY` and the chat companion executable.
Unsupported controls fail before execution. Optional plugins and hooks support
`none`; selected entries remain future work.

Profiles record content hashes and separate installed/enabled state from
loaded/invoked telemetry. Built-in and managed sources retain explicit
limitations. This feature does not upgrade an adapter's live-validation status
in the table above.

## Extend an adapter

For a worker, define `[adapters.<name>]` with an argv template or implement the
`Harness` contract. For reporting, implement
`report(ctx, prompt, schema=None) -> {proc, text, structured}` and set
`can_report = True`. Use a fresh session with explicit tool restrictions.
AJX performs response validation and credential cleanup around the call.

See [ARCHITECTURE.md](ARCHITECTURE.md) for plugin discovery, normalization, and
measurement contracts. Do not map a reporter to a worker's unrestricted
execution command.
