# Reporting an agent journey

Read after task execution. Give narrative guidance to the original task agent after preserving its stopping point. Extract asks in a separate pass after the journey exists. The journey and raw asks are the two primary artifacts.

## Raw asks: `asks.md`

Create the full asks register from the completed first-person journey. Each ask links to journey events and original evidence. Several observations can support one ask; several asks can address one underlying obstacle. Preserve those relationships and shared costs.

Use a scan-friendly prioritized table followed by the observation detail needed for each ask. Include all supported asks, even when their costs are unknown. This raw register is a primary deliverable, not a short executive summary.

For each ask, record:

- **ID and requested behavior:** one concrete requested change, status, and relevant product version.
- **How observed:** what I attempted; the command, API call, UI step, or document involved; expected behavior from the task or contract; actual output/result; how the mismatch or opportunity became visible; consequence.
- **Evidence:** journey anchor, original source/event/artifact, date, and evidence status. Distinguish experienced failures, observed missing capabilities, and untested near-miss risks.
- **Measured cost:** wall-clock seconds/minutes, output tokens, tool calls, retries, human interventions, and waiting where captured. Name shared phase costs and missing metrics.
- **Modeled value:** avoidable time/tokens/calls or gates, comparator, assumptions, uncertainty, and overlap exclusions. Unpriced asks remain valid.
- **Priority:** rationale with the metrics driving it; qualitative task failure or verification risk where relevant.
- **Verification:** how to reproduce the observation and what result would establish that the ask was met.

Choose a default priority order using task impact and supported avoidable cost, and state that basis. Also give compact alternative rankings by available metrics: time, tokens, calls/retries, and human intervention. Label whether a ranking uses measured incurred cost or modeled savings. Missing values are unranked, never zero. Do not normalize unrelated units into an unexplained score.

Do not count a shared event twice across asks. Show costs associated with each ask while explaining which totals overlap. A metric ranking describes this trial, not universal product severity.

## Optional team or merged report

A polished field/team report is a later derivative when requested. Lead with outcome and prioritized asks, then the journey summary, concrete strengths, and evidence limits. Link to both primary artifacts. A supplied audience template can shape this view without replacing or overwriting them.

## Full journey

Use this order, adapting section length to the trial:

1. **Title and provenance.** Product, version, task, dates, task agent and reporter, capabilities, and sources. Identify redactions instead of claiming every identifier is verbatim.
2. **Asks preview.** Brief actionable requests near the top, linked to stable IDs and the full raw register.
3. **How to read the numbers.** Sources, measurement windows, availability, models, exclusions, and limitations before the tables.
4. **Journey at a glance.** Phase, attempted outcome, product response, observed result, elapsed time, output tokens, calls, and attribution. Missing metrics get a reason.
5. **Chronological narrative.** Commands, decisive output, consequences, wrong hypotheses, and later corrections in their original order. Put reproduction detail where the engineer needs it.
6. **Walls and recovery.** Whether the task stopped, whether an alternate path worked, and what remains unverified. If no wall occurred, say so. Counterfactual outcomes are hypotheses.
7. **Gate log.** Required human action, trigger, agent action, wait, outcome, and verification limits. A prompt handled by an available browser tool is not automatically a human gate.
8. **Cost ledger.** Evidence-linked time, tokens, calls, and useful output recovered. Name shared or nested costs instead of adding them twice.
9. **Value of the asks.** Measured baseline, modeled alternative, comparator, uncertainty, overlap exclusions, projected savings. Unpriced asks remain valid.
10. **Trust record.** Observable source conflicts, high-level decisions documented by the task agent, and later corrections. Do not invent private thoughts or assume trust must decline.
11. **Attribution.** Product, agent, environment, task, human, mixed, or unresolved. Retain non-product costs in full accounting while excluding them from product-only totals.
12. **Observation and ask references.** Stable IDs and anchors connecting events to the raw asks register. Keep the complete register in `asks.md`.
13. **Method and limits.** Boundaries, isolation, source quality, telemetry method, arithmetic checks, exclusions, prior exposure, limits on generalization.

Add a comparison only when requested and supported by independent trials. Preserve the original findings and artifact identifiers when rewriting an existing report.

An incomplete task deserves a report. State how the trial was selected and whether evidence was lost for stopped agents. One trial does not establish population completion rates or a general product ranking.

## First-person narrative voice

Write the journey as “I”, preferably authored by the agent that performed the task. Do not turn it into “the agent encountered…” prose. The raw asks register can use concise product-team language while preserving how the agent observed each issue.

If the narrator is unavailable, clearly identify an editorial reconstruction from the recorded task. Describe only recorded actions, output, and supported high-level decision summaries. Missing recollection is a limit to state, not an invitation to invent an inner monologue.

Use short, concrete sentences, technically accurate identifiers, and decisive verbatim output. Keep the literal machine perspective readable in ordinary language. Dry reactions grounded in the account are fine. Avoid em dashes in prose, filler, invented emotions, and claims about product intentions. Record uncertainty plainly.

For example, as a fictional style illustration only: “The command printed `Complete`. It kept running. I had no progress signal. I checked whether the output file existed.” Actual reports must use their own observed commands and evidence.

Preserve wrong conclusions where they occurred, with corrections later. Update affected asks, severity, and costs without erasing the original evidence.

Use stable event anchors such as `E-001` when that helps traceability. Carry the same ask/finding IDs through the journey, raw register, optional merged report, and follow-up. Rewrites of one session are multiple views of the same evidence, not independent trials.

## Annotation vocabulary

These describe observed events. Do not give them to the task agent as a checklist.

| Label | Meaning |
|---|---|
| 🧱 **Wall** | No available authorized path allowed continuation; the task stopped or needed a human. |
| 🚧 **Roadblock** | The documented path failed; an alternate path outside that documentation worked. |
| ↩️ **Detour** | The intended path eventually worked after extra retry or recovery steps. |
| ⏳ **Wait** | The agent waited without a usable progress/completion signal. A sleep alone does not prove product-caused waiting. |
| 🚫 **Dead End** | A product-directed path did not achieve the needed result and was abandoned. |
| 🔀 **Fork** | Multiple plausible paths existed without a selection signal. Record options and the documented selection. |
| ❓ **Wrong Turn** | A selected path later proved wrong. Preserve the correction. An unsupported choice that worked is a Fork, not a Wrong Turn. |
| 🔴 **Misdirection** | Instructions or output pointed to an incorrect action or contradicted the actual result. |
| 👁️ **Blind Spot** | Relevant product information existed but was unavailable at the point of need. Establish its existence. |
| 🚪 **Gate** | Human participation was required. Record whether it was explicit, early, bounded, and resolved. |
| 🧭 **Trust Decision** | Evidence shows a choice between sources or a change in which source informed an action. |
| 💸 **Cost** | Supported resource cost, or an explicit statement that it is unknown/not separately isolable. |
| ✅ **Delight** | Concrete product behavior that helped the agent achieve its task. |

Each obstacle gets a cost line with evidence or “not separately isolable / unavailable” and a reason. Several labels may describe one event; count its cost once.

No label has a quota. “No observed wall” and “no separately recorded delight” are legitimate results.

When analyzing older reports, preserve their original quotations and labels. Explain mappings separately: Dark Signal may map to Misdirection; Human Gate to Gate; No Basis to a Fork or unresolved uncertainty. Do not label a historical No Basis as a Wrong Turn unless the choice proved wrong.

## Evidence and merging

Maintain **ask → finding → event or artifact → source** traceability. Several requests can address one finding without becoming independent defects.

Classify each claim's evidence:

- **Observed/measured:** direct captured output or telemetry.
- **Reported:** supplied by a human or another agent, with source and date.
- **Inferred:** an explanation consistent with evidence, not directly established.
- **Modeled:** a counterfactual projection with a named basis.
- **Unknown/not tested:** evidence is absent or does not answer the question.

Retain `AJX`, `Field`, or `Both` tags when merging. “Both” requires independent observations of the same specific behavior. Human approval or a rewrite of AJX text is not independent corroboration.

Keep documentation-only research separate from task evidence. Do not turn field-reported observations into agent-measured facts. Preserve differences in versions, dates, workloads, and coverage.

Check underlying evidence when sources disagree. Record unresolved discrepancies; do not average incompatible measurements or choose a convenient number. Reuse canonical measurements across prose and tables.

Secondary evidence can be useful without independent verification, but must remain attributed. Retesting after a trial is new evidence with a separate window.

## Priorities and status

Rank by impact, reach, recovery burden, and evidence strength. Supported savings inform the ordering. Do not label implementation cheap merely because its desired interface is small.

Use supplied severity definitions; otherwise describe impact plainly. A blocker in one task does not automatically establish a production-wide P0.

Suggested statuses: `open`, `workaround available`, `claimed fixed`, `verified fixed`, `regressed`, `not reproduced`, `needs evidence`.

A targeted rerun can verify a fix. Only a comparable complete rerun measures complete-task improvement. Keep projections labeled modeled after the underlying defect is fixed.
