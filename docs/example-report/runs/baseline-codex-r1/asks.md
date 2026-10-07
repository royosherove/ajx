# Asks: Demo Export CLI (baseline-codex-r1)

Harness codex synthetic fixture, model example-model, product version 1.2.0. Task outcome declared by the agent is recorded in run.json; verified outcome: **succeeded** (2/2 fictional checks passed).

Every ask below was extracted from `journey.md` and checked against `events.jsonl`. Measured costs are sums over the referenced events, computed by ajx, per ask; events shared between asks are listed so the overlap is visible (run-level totals in measurements.json count each event once). Priorities are rationales, not scores.

## Prioritized register

| ID | Ask | Status | Labels | Events | Tool calls (failed) | Span | Tokens | Priority rationale |
|---|---|---|---|---|---|---|---|---|
| ASK-001 | Make the quick-start export command executable | observed_friction | Roadblock, Misdirection | E-003, E-004, E-005 | 3 (1) | 69.0 s | 813 | Fix the broken first attempt before reducing optional friction. |
| ASK-002 | Expose progress while a CSV export is running | observed_friction | Wait, Blind Spot | E-005, E-006 | 1 (0) | 47.0 s | 683 | Make long-running work observable without extra polling. |
| ASK-003 | Explain when to choose export instead of dump | observed_friction | Fork | E-001, E-002 | 1 (0) | 8.0 s | 307 | Remove the ambiguous choice at the start of the task. |

Default order: as prioritized by the extraction pass using task impact and supported cost. Alternative rankings:

- **Time span** (measured, incurred): ASK-001 (69.0), ASK-002 (47.0), ASK-003 (8.0)
- **Output tokens** (measured, incurred): ASK-001 (813), ASK-002 (683), ASK-003 (307)
- **Tool calls** (measured, incurred): ASK-001 (3), ASK-002 (1), ASK-003 (1)
- **Failed calls** (measured, incurred): ASK-001 (1), ASK-002 (0), ASK-003 (0)

- **Human intervention**: 0 gate(s) recorded

## Detail

### ASK-001: Make the quick-start export command executable

- **Requested behavior:** Use the supported --output flag in the quick start, with a copyable CSV example.
- **Status:** observed_friction; product version 1.2.0
- **How observed:** The documented --out flag failed. Command help showed --output, which worked.
- **Consequence:** The first attempt failed and required a help lookup before retrying.
- **Evidence:** events E-003, E-004, E-005; journey anchors none
- **Measured cost:** 3 tool call(s), 1 failed, tool time 51.0 s, span 69.0 s, output tokens 813; shared events: E-005. Basis: sum over referenced events; tokens attributed per whole assistant message (two asks citing events from the same message both carry its tokens); span = first referenced event start to last referenced event end.
- **Modeled value:** not modeled
- **Workaround:** Replace --out with --output.
- **Priority:** Fix the broken first attempt before reducing optional friction.
- **Verification:** Run the documented command in a fresh workspace; it must produce a 100-row CSV.

### ASK-002: Expose progress while a CSV export is running

- **Requested behavior:** Emit bounded progress on stderr and a final structured completion result.
- **Status:** observed_friction; product version 1.2.0
- **How observed:** Only start and completion messages appeared during the 47-second export.
- **Consequence:** The agent could not distinguish productive work from a stalled command.
- **Evidence:** events E-005, E-006; journey anchors none
- **Measured cost:** 1 tool call(s), 0 failed, tool time 47.0 s, span 47.0 s, output tokens 683; shared events: E-005. Basis: sum over referenced events; tokens attributed per whole assistant message (two asks citing events from the same message both carry its tokens); span = first referenced event start to last referenced event end.
- **Modeled value:** not modeled
- **Workaround:** none recorded
- **Priority:** Make long-running work observable without extra polling.
- **Verification:** Run a delayed export; progress must remain separate from CSV stdout.

### ASK-003: Explain when to choose export instead of dump

- **Requested behavior:** State the output formats and intended use of both commands in top-level help.
- **Status:** observed_friction; product version 1.2.0
- **How observed:** Both commands were described as writing records; the required CSV path was unclear.
- **Consequence:** The agent had to choose without a selection signal.
- **Evidence:** events E-001, E-002; journey anchors none
- **Measured cost:** 1 tool call(s), 0 failed, tool time 2.0 s, span 8.0 s, output tokens 307. Basis: sum over referenced events; tokens attributed per whole assistant message (two asks citing events from the same message both carry its tokens); span = first referenced event start to last referenced event end.
- **Modeled value:** not modeled
- **Workaround:** none recorded
- **Priority:** Remove the ambiguous choice at the start of the task.
- **Verification:** A user reading only top-level help can identify the CSV command.

## Strengths observed

- **A verifiable row count**: Inspect confirmed CSV format and 100 data rows. (E-008)

## Method and limits

- Journey provenance: FICTIONAL FIRST-PERSON ACCOUNT: hand-authored demonstration, not a real agent recollection.
- Extraction: ok; fresh reporter session (fictional reporter), structured output validated against schema; event references validated against the run's events.
- Telemetry coverage: usage partial, tool calls partial, timestamps synthetic timestamps.
- Limitations:
  - All content and metrics are invented for this example.
  - A change across these fictional runs demonstrates the report, not a measured improvement.
  - Token subtotals are partial; the headless example intentionally has no usage telemetry.
