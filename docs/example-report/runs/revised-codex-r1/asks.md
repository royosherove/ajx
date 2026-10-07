# Asks: Demo Export CLI (revised-codex-r1)

Harness codex synthetic fixture, model example-model, product version 1.3.0. Task outcome declared by the agent is recorded in run.json; verified outcome: **succeeded** (2/2 fictional checks passed).

Every ask below was extracted from `journey.md` and checked against `events.jsonl`. Measured costs are sums over the referenced events, computed by ajx, per ask; events shared between asks are listed so the overlap is visible (run-level totals in measurements.json count each event once). Priorities are rationales, not scores.

## Prioritized register

| ID | Ask | Status | Labels | Events | Tool calls (failed) | Span | Tokens | Priority rationale |
|---|---|---|---|---|---|---|---|---|
| ASK-001 | Expose progress while a CSV export is running | observed_friction | Wait | E-002, E-003 | 1 (0) | 20.0 s | 401 | The remaining observed friction is visibility during execution. |

Default order: as prioritized by the extraction pass using task impact and supported cost. Alternative rankings:

- **Time span** (measured, incurred): ASK-001 (20.0)
- **Output tokens** (measured, incurred): ASK-001 (401)
- **Tool calls** (measured, incurred): ASK-001 (1)
- **Failed calls** (measured, incurred): ASK-001 (0)

- **Human intervention**: 0 gate(s) recorded

## Detail

### ASK-001: Expose progress while a CSV export is running

- **Requested behavior:** Emit bounded progress on stderr and a final structured completion result.
- **Status:** observed_friction; product version 1.3.0
- **How observed:** Only start and completion messages appeared during the 20-second export.
- **Consequence:** The agent could not observe intermediate progress.
- **Evidence:** events E-002, E-003; journey anchors none
- **Measured cost:** 1 tool call(s), 0 failed, tool time 20.0 s, span 20.0 s, output tokens 401. Basis: sum over referenced events; tokens attributed per whole assistant message (two asks citing events from the same message both carry its tokens); span = first referenced event start to last referenced event end.
- **Modeled value:** not modeled
- **Workaround:** none recorded
- **Priority:** The remaining observed friction is visibility during execution.
- **Verification:** Run a delayed export; progress must remain separate from CSV stdout.

## Strengths observed

- **The help identifies the CSV path**: Top-level help explains export versus dump. (E-001)
- **A verifiable row count**: Inspect confirms the requested output. (E-004)

## Method and limits

- Journey provenance: FICTIONAL FIRST-PERSON ACCOUNT: hand-authored demonstration, not a real agent recollection.
- Extraction: ok; fresh reporter session (fictional reporter), structured output validated against schema; event references validated against the run's events.
- Telemetry coverage: usage partial, tool calls partial, timestamps synthetic timestamps.
- Limitations:
  - All content and metrics are invented for this example.
  - A change across these fictional runs demonstrates the report, not a measured improvement.
  - Token subtotals are partial; the headless example intentionally has no usage telemetry.
