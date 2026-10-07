# AJX FAQ

## What is AJX?

AJX means Agent Journey Experience. It is a method and toolkit for reviewing a
product through an agent's attempt to complete a real task. The result is a
chronological journey and a prioritized set of product asks with supporting
evidence and available costs.

AJX v1 is the first public version. It includes a portable Agent Skill,
a command-line runner, agent adapters, reporting prompts, measurement code,
and local HTML and Markdown reports. The HTML views include an ask register,
a visual journey with an event map and happenings table, and a matrix with
each run's asks shown side by side.

## Who should use it?

Developers, maintainers, and product teams responsible for tools or documentation
that agents use. A review can help a maintainer reproduce a confusing CLI
interaction, an engineer investigate a failed task, or a product owner decide
which usability changes deserve attention.

Choose a task whose outcome matters to your users. The review describes that
task and configuration; broader conclusions require additional trials.

## Why preserve the journey?

The order of events helps explain how an obstacle arose. A later error may follow
from an earlier recovery step; a successful result may depend on a workaround or
an undocumented choice. Keeping commands, outputs, and later corrections in order
lets a reader investigate those relationships.

The journey records observations and supported explanations. It cannot prove
causation where the evidence leaves multiple explanations open.

## What is a product ask?

An ask is a concrete requested change connected to an observation. It states what
the agent attempted, what happened, the consequence, the relevant evidence, and
what result would show that the change worked.

Asks can describe defects, observed friction, missing capabilities, or untested
risks. Those categories remain distinct. An ask can be useful even when its cost
cannot be measured.

The register includes priority rationale and alternative rankings by available
metrics. Several asks may reference the same event; their costs overlap and must
not be added as independent savings.

## What does “cost” mean?

AJX records task elapsed time, output tokens, tool calls, failed calls, and related
metrics when the agent CLI exposes usable evidence. Task execution, verification,
and reporting have separate measurement windows.

A time span or message-level token allocation is an accounting convention, not
proof that every second or token was caused by one defect. The report states its
basis and shared costs.

Provider-reported dollar estimates and credits keep their own labels and units.
AJX does not turn incomplete token counts into a bill or combine unrelated units
into a single usability score.

## Can it tell me what a fix is worth?

An ask may include a modeled saving when there is a defensible comparator.
The model must state its assumption and uncertainty. Without that basis, the
saving remains unspecified.

A projection remains modeled after a fix ships. Run a comparable follow-up to
measure the result, including necessary work that the fix does not eliminate.
Improved results on one task do not establish general savings for every user.

## What do I provide before a run?

Provide the product, a realistic task, the materials a user would receive, and
observable success criteria. Choose the agent CLI, model, configuration,
repetitions, and permitted environment. Define time, spend, human participation,
and cleanup boundaries.

Keep expected findings, defect hints, and AJX reporting vocabulary out of the
worker's prompt. Verification checks should test the requested outcome.
AJX needs task evidence; reading documentation alone is not a journey review.

## Who writes the reports?

The coordinator sets up the trial. A worker attempts the task. After the task
stops, AJX tries to resume that worker's session for narration with restricted tools.
If resumption is unavailable, a reporter creates an explicitly labeled
reconstruction from the recorded evidence.

A separate extraction pass checks the journey against the digest and produces
asks. Code assigns event IDs, validates cited references, and computes measured
costs. Narrative claims can still be wrong and require review. A first-person
account does not provide access to hidden model reasoning.

## Which agents work with AJX v1?

The coordinator skill works in Codex, Claude Code, Kiro CLI, and other Agent
Skills hosts with file and shell access. The host can read `SKILL.md` and use
its own tools to coordinate the trial. See [harness setup](HARNESS-SUPPORT.md)
for installation and invocation.

The automated worker adapters have these capabilities:

| Agent CLI | Adapter status and main limits |
|---|---|
| Claude Code | Exercised live during development. Reconciles stream totals against transcript records when available; supports session resumption. |
| Kiro CLI | Exercised live during development. Exposes tool events and credits, but not token counts; user configuration cannot be fully isolated. |
| Codex CLI, Gemini CLI, Cursor Agent, GitHub Copilot CLI | Bundled adapters marked unverified until a live run confirms the relevant formats and behavior. |

Run `ajx plugins` to inspect installed tools and adapter status. The configuration
calls an agent CLI a `harness`. Custom command templates and Python plugins can
add adapters; see [the architecture](ARCHITECTURE.md).

Live development checks and synthetic regression tests do not guarantee
compatibility with every future CLI version. Report generation supports
Claude Code, Codex, Kiro CLI, and custom reporter plugins. One reporter is kept
fixed across the matrix. AJX validates its structured output locally.

## Can I compare models or products?

The runner can repeat a task across agent CLIs, models, and configurations. It
produces a report per run and a matrix with measurement and comparability notes.

To compare products, configure separate trials with equivalent user outcomes,
starting materials, success criteria, and boundaries. Preserve each product's
report before comparing them. AJX v1 does not provide an automatic cross-product
ranking.

For either comparison, state differences in versions, permissions, available
tools, concurrency, and evidence coverage. Output tokens use different model
tokenizers. Results describe the tested task and conditions.

## How does it fit alongside benchmarks and regression tests?

AJX focuses on discovering and explaining product friction encountered during a
task. Its asks can inform new regression checks. Benchmarks and test suites can
then evaluate those checks repeatedly under their own defined conditions.

A journey review examines one path through a product. Use broader testing or an
audit when you need feature coverage. AJX does not certify a product or establish
a universal ranking of agents.

## What happens when a run fails or produces no asks?

AJX preserves available evidence and attempts reporting when a worker stops,
fails, or times out. Lost evidence, a stub journey, or failed extraction must be
reported as incomplete. An empty register caused by a reporting failure is not a
clean result.

“No obstacles observed” is valid when the evidence supports it. It means that
review found no supported asks for that task. It does not prove there are no
defects or that the product is ready for every agent.

## Does AJX fix the product automatically?

AJX v1 produces reports and supports follow-up trials. A maintainer decides which
asks to address and verifies the changes. There is no built-in autonomous
review–fix loop.

You can invoke the CLI from your own automation, with explicit authority and
boundaries for any code changes or resource usage. Each follow-up still needs
usable evidence; reaching zero asks alone is not a release criterion.

## Does it require AWS or a hosted AJX service?

AJX runs from this repository. It has no required hosted AJX service. Model
requests use your configured agent CLI and provider. Local tasks need no AWS
account; cloud tasks and cloud model providers require their own credentials.

The software is MIT licensed. Agent subscriptions, model usage, reporting calls,
and resources created by a task may cost money. A trial's budget setting is
enforced only by adapters that support it; it is not a universal spending cap.

## Can I publish a report?

Review and redact it first. A report or archived workspace may contain prompts,
command output, account details, private paths, or source files. Record
redactions and their effect on reproducibility.

Only share material you are authorized to publish. See [SECURITY.md](../SECURITY.md)
for output handling and private vulnerability reporting.
