# Use AJX with your agent

The `skills/ajx` directory is a standard Agent Skills package: `SKILL.md` has a
name and description, and the remaining files supply instructions, scripts,
schemas, and report assets. Install the complete directory. The coordinator
needs file access and a shell that can run Python 3.11+.

## Install and invoke

| Host | Personal skill directory | Explicit invocation |
|---|---|---|
| Codex | `~/.agents/skills/ajx` | `$ajx` |
| Claude Code | `~/.claude/skills/ajx` | `/ajx` |
| Kiro CLI | `~/.kiro/skills/ajx` | `/ajx` |
| Another Agent Skills host | Its documented skill directory | Its skill picker or “Use the AJX skill…” |

The README includes installation commands. Start a new session after installing.
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

## Extend an adapter

For a worker, define `[adapters.<name>]` with an argv template or implement the
`Harness` contract. For reporting, implement
`report(ctx, prompt, schema=None) -> {proc, text, structured}` and set
`can_report = True`. Use a fresh session with explicit tool restrictions.
AJX performs response validation and credential cleanup around the call.

See [ARCHITECTURE.md](ARCHITECTURE.md) for plugin discovery, normalization, and
measurement contracts. Do not map a reporter to a worker's unrestricted
execution command.
