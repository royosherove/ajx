# Asks: Demo Export CLI (headless-kiro-cli-r1)

Harness kiro-cli synthetic fixture, model example-model, product version 1.3.0. Task outcome declared by the agent is recorded in run.json; verified outcome: **failed** (0/2 fictional checks passed).

Every ask below was extracted from `journey.md` and checked against `events.jsonl`. Measured costs are sums over the referenced events, computed by ajx, per ask; events shared between asks are listed so the overlap is visible (run-level totals in measurements.json count each event once). Priorities are rationales, not scores.

## Prioritized register

| ID | Ask | Status | Labels | Events | Tool calls (failed) | Span | Tokens | Priority rationale |
|---|---|---|---|---|---|---|---|---|
| ASK-001 | Check headless authorization before starting an export | observed_friction | Gate, Wall | E-002, E-003, E-004 | 1 (1) | 16.0 s | n/a | Expose the prerequisite early so teams can prepare an authorized run. |

Default order: as prioritized by the extraction pass using task impact and supported cost. Alternative rankings:

- **Time span** (measured, incurred): ASK-001 (16.0)
- **Output tokens** (measured, incurred): none measurable; unranked (no data): ASK-001
- **Tool calls** (measured, incurred): ASK-001 (1)
- **Failed calls** (measured, incurred): ASK-001 (1)

- **Human intervention**: 1 gate(s) recorded: Workspace approval requires a human

## Detail

### ASK-001: Check headless authorization before starting an export

- **Requested behavior:** Provide a non-interactive authorization preflight and document the required human approval.
- **Status:** observed_friction; product version 1.3.0
- **How observed:** The command requested browser approval after the agent had started the task.
- **Consequence:** The unattended task stopped without an output file.
- **Evidence:** events E-002, E-003, E-004; journey anchors none
- **Measured cost:** 1 tool call(s), 1 failed, tool time 3.0 s, span 16.0 s, output tokens n/a. Basis: sum over referenced events; tokens attributed per whole assistant message (two asks citing events from the same message both carry its tokens); span = first referenced event start to last referenced event end.
- **Modeled value:** not modeled
- **Workaround:** none recorded
- **Priority:** Expose the prerequisite early so teams can prepare an authorized run.
- **Verification:** In headless mode, preflight reports the missing approval before an export begins.

## Gates

- Workspace approval requires a human ; agent: Stopped and reported the prerequisite. ; resolved: False (E-002, E-003)

## Method and limits

- Journey provenance: FICTIONAL FIRST-PERSON ACCOUNT: hand-authored demonstration, not a real agent recollection.
- Extraction: ok; fresh reporter session (fictional reporter), structured output validated against schema; event references validated against the run's events.
- Telemetry coverage: usage unavailable, tool calls partial, timestamps synthetic timestamps.
- Limitations:
  - All content and metrics are invented for this example.
  - A change across these fictional runs demonstrates the report, not a measured improvement.
  - Token subtotals are partial; the headless example intentionally has no usage telemetry.
