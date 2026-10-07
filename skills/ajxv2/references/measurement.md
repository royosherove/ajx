# Measurement protocol

## Boundaries and provenance

Use recorded timestamps and usage records, never recollection, for measured costs. Capture the original task stopping point separately from later verification and report generation. Define outcome milestones before comparing trials.

Parse large transcripts with code. Inspect a small schema sample omitting message content and secrets, then use an adapter for that actual schema. Preserve source event IDs and extraction rules. Do not guess provider token semantics.

The bundled calculator accepts normalized exports. It does not locate session files, collect live telemetry, or implement native Codex/Claude adapters. Those adapters need validation against the host's actual export format.

When telemetry is unavailable, report with the evidence that exists. Do not estimate missing tokens from text length or fill blanks with zero.

## Normalized inputs for `scripts/measure.py`

Supply a manifest and JSONL event file. The manifest names measurement windows and each phase's scope:

```json
{
  "sessions": [{
    "id": "run-1",
    "start": "2026-01-01T12:00:00Z",
    "end": "2026-01-01T12:02:00Z",
    "usage_status": "complete",
    "expected_output_tokens": 120,
    "tool_status": "complete",
    "expected_tool_calls": 2,
    "phases": [{
      "id": "try",
      "start": "2026-01-01T12:00:00Z",
      "end": "2026-01-01T12:02:00Z",
      "scope": "task"
    }]
  }]
}
```

Events have a unique source `id` within the session and a timezone-aware timestamp `at`. Usage records contain **incremental, final output-token counts**, not cumulative session counters or repeated streaming snapshots:

```jsonl
{"id":"message-1","session_id":"run-1","at":"2026-01-01T12:00:10Z","kind":"usage","output_tokens":50}
{"id":"call-1","session_id":"run-1","at":"2026-01-01T12:00:11Z","kind":"tool_call"}
{"id":"message-2","session_id":"run-1","at":"2026-01-01T12:01:10Z","kind":"usage","output_tokens":70}
{"id":"call-2","session_id":"run-1","at":"2026-01-01T12:01:11Z","kind":"tool_call"}
```

Run:

```sh
python3 /path/to/ajx/scripts/measure.py manifest.json events.jsonl --output measurements.json
```

`usage_status` and `tool_status` are independently `complete`, `partial`, or `unavailable`. Complete data requires an independently obtained expected total for the same window. Partial data reports an observed subtotal and leaves the full count null. Unavailable data must have no records of that kind.

Phases partition each session exactly using `[start, end)` intervals. Include human waiting, reporting, verification, and exclusions as explicit phases with their corresponding `scope`. Do not silently drop gaps. Choose an end boundary after the last included event.

The calculator rejects overlaps, gaps, unknown sessions, duplicate IDs, naive timestamps, negative/non-integer usage, and mismatched complete totals. It preserves seconds for arithmetic; round minutes only for presentation.

## Adapter checks

Before trusting a native transcript adapter:

1. Establish whether usage is incremental or cumulative. Convert cumulative counters to deltas and explain resets; never sum cumulative snapshots.
2. Deduplicate streaming or repeated usage using native message IDs and final-record semantics.
3. Identify tool calls by actual call records and IDs, not command-looking text. Match nested agents separately.
4. Define which timestamp assigns an assistant usage record to a phase. This is an accounting convention, not proof that every token was generated within that interval.
5. Reconcile independently obtained session totals against normalized totals. Matching phase sums to the same parsed events checks arithmetic, not source completeness.
6. Keep parent, worker, reporting, and verification records distinct. Include worker spend only when captured; disclose exclusions.

## Additional metrics and models

- **Elapsed time:** report the task window's duration. Parallel agents' durations are worker time; do not add them and call the sum wall-clock time.
- **Waiting:** use actual start/end evidence. Do not sum requested `sleep` arguments as actual elapsed time; commands can be interrupted, repeated, or overlap. Attribute avoidability only with evidence.
- **Input, cached, reasoning tokens:** retain provider field definitions and availability. Output tokens do not include every cost on every platform.
- **Context pressure:** use a supported context measurement. Cached input averages alone do not measure occupancy. Omit the metric when its meaning cannot be established.
- **Money:** derive only from known model, billable token categories, contemporaneous rates, and service charges. Otherwise use time and tokens without dollar estimates.
- **Modeled savings:** name the baseline event/phase, measured comparator, changed assumption, uncertainty, and overlap exclusions. Prefer comparable evidence from the same trial or a controlled follow-up. Do not invent floors.
- **Counterfactual removal:** a fixed defect does not remove work the successful task still needs, such as building, executing, or verifying.
- **Fix benefit:** preserve the measured baseline. Keep projections modeled until a comparable rerun measures the new complete task.
- **File timestamps:** use only for what they establish, such as latest write. Confirm timezone handling. Do not infer idle time, completion, or causation merely from absent writes.

Use canonical measurements across report views. Several rows can reference the same event, but its interval and token records contribute once to an aggregate.
