# Read AJX results programmatically

Use `ajx validate trial.toml` to find the configured output directory. After a
run starts, `ajx status trial.toml` reads its live state. The native CLI also
supports `ajx status trial.toml --json`; its `output_dir` and `runs` fields locate
the results. Each run's files are under `<output_dir>/runs/<run_id>/`.

Do not infer task success from a process exit code. `ajx run` can complete
successfully while task checks fail. For an unattended acceptance decision, use
the native CLI's `ajx ci` and inspect `ci-result.json` and `junit.xml`.

## Files and field paths

JSON artifacts carry their own `schema_version` where available. Handle
additional keys, and distinguish a missing field or `null` from zero or false.
An older artifact may lack fields introduced by a later AJX version.

| File | Field path | Meaning |
|---|---|---|
| `run.json` | `run_id`, `ajx_version` | Run identity and producer version |
| `run.json` | `task_prompt.sha256` | Hash of the task prompt actually supplied |
| `run.json` | `agent.harness_name`, `agent.model_id` | Recorded worker harness and model; model can be unknown |
| `run.json` | `task_outcome_declared` | The worker's own result statement, not independent verification |
| `run.json` | `task_outcome_verified`, `verification_scope` | Outcome and scope of the configured checks |
| `run.json` | `review_status` | Reporting completeness; `ready` does not mean the task succeeded |
| `run.json` | `journey_provenance` | Whether the journey was self-narrated or reconstructed |
| `run.json` | `narration_failure` | When present, the fallback's `category`, `message`, and any recorded `stop_reason` |
| `run.json` | `stages` | Stage status strings in the rendered result |
| `run.json` | `artifacts`, `limitations` | Related artifact paths and known evidence limits |
| `run.json` | `reported_extension_inventory` | Available harness inventory fields; these do not prove activation, ownership, or use |
| `run.json` | `report_instructions_visible_during_task` | Unknown (`null`) when instruction visibility was not established |
| `state.json` | `stages.<stage>.status` | Live stage state, such as `running`, `done`, or `error` |
| `verify.json` | `outcome`, `passed`, `total` | Verification outcome and number of passing/total checks |
| `verify.json` | `checks`, `checks_with_auth_errors` | Individual check evidence and credential-related problems |
| `measurements.json` | `task_summary.elapsed_seconds` | Task process start-to-stop duration; not total trial duration |
| `measurements.json` | `task_summary.tool_calls`, `task_summary.tool_status` | Observed tool-call count and coverage |
| `measurements.json` | `task_summary.failed_tool_calls`, `task_summary.retries_after_error` | Failures and subsequent same-tool calls according to normalized error signals |
| `measurements.json` | `task_summary.failure_metric_basis`, `task_summary.tool_error_status` | Definition and coverage of those error signals |
| `measurements.json` | `task_summary.tool_results_without_error_status` | Tool events for which an explicit error status was unavailable |
| `measurements.json` | `task_summary.output_tokens`, `task_summary.usage_status` | Observed output-token usage and coverage |
| `measurements.json` | `task_summary.harness_reported` | Adapter-specific facts; fields and availability vary |
| `measurements.json` | `phase_measurements` | Separate task, verification, narration, and reporting windows |
| `events.jsonl` | `eid`, `kind`, `at` | Normalized evidence identity, event kind, and timestamp |
| `events.jsonl` | `input` | Compact tool-input display summary |
| `telemetry.execute.json` | `events[].input_raw` | Original structured tool input where the adapter captured it |

For example, set the directory to a real run found above:

```sh
AJX_RUN_DIR="ajx-reports/runs/example-r1"
jq '{run_id, task_outcome_verified, verification_scope,
     review_status, journey_provenance, stages}' "$AJX_RUN_DIR/run.json"
jq '{outcome, passed, total, checks_with_auth_errors}' "$AJX_RUN_DIR/verify.json"
jq '.task_summary | {elapsed_seconds, tool_calls, tool_status,
     failed_tool_calls, retries_after_error, output_tokens, usage_status}' \
  "$AJX_RUN_DIR/measurements.json"
```

Inspect `checks` before treating the outcome as proof of a particular claim. A
passing file-existence check proves that the file exists. It does not prove that
the file's contents or a prose explanation are accurate. For factual summaries,
request a structured result and check its relevant values against independent
artifacts or expected output. Preserve the scope of the actual checks when
reporting success.

Stage completion, task verification, report completeness, and confirmed cleanup
are separate decisions. Use live status while a run is active. For saved runs,
inspect the final stage states and any cleanup limitations as well as the task
outcome.

The native status JSON includes `runs[].active_stages` with each running stage's
name, `started_at`, and `elapsed_seconds` when timestamps are available. These
measure elapsed time; they do not predict remaining work or prove that a
long-running model call is making progress.

## Decode captured process output

AJX's captured process streams, such as `execute.raw.jsonl` and
`narrate.raw.jsonl`, use an outer JSONL envelope:

```json
{"at":"2026-01-01T12:00:00Z","line":"{\"type\":\"result\",\"result\":\"done\"}"}
```

`at` is the capture timestamp. `line` is one output line from the process, stored
as a string. It can contain a harness JSON event or ordinary text. Parsing the
outer record does not parse the inner line. These files differ from
`events.jsonl`, which already contains normalized AJX events.

To inspect original lines without assuming they are JSON:

```sh
jq -r '.line' "$AJX_RUN_DIR/execute.raw.jsonl"
```

To keep both structured events and non-JSON output, use an explicit fallback.
`fromjson?` alone silently drops lines it cannot parse:

```sh
jq '{at, event: (.line | try fromjson catch null),
     raw_line: .line}' "$AJX_RUN_DIR/execute.raw.jsonl"
```

The inner event schema belongs to the harness. For a Claude Code stream, for
example, inspect the initialization event with:

```sh
jq '.line | fromjson? |
    select(.type == "system" and .subtype == "init") |
    {model, skills, slash_commands}' "$AJX_RUN_DIR/execute.raw.jsonl"
```

Skills and slash commands are different lists. Presence in an initialization
event is not evidence of ownership, activation, or use. Do not call a listed
extension a user extension unless the source identifies its origin.
For Claude Code, `task_summary.harness_reported` keeps `skills_visible` and
`slash_commands_visible` separate; a missing inventory remains unknown.

## Interpret counts and limits

Tool failures depend on the adapter's recorded error signals. A shell tool can
return success after an earlier command in the same script failed. Conversely,
a nonzero exit can be an expected result, such as a difference reported by
`diff`. Zero failed tool calls does not establish that every command succeeded.
Do not classify failures solely because output contains the word “error.”

The compact `events.jsonl` file omits `input_raw`. Check
`telemetry.execute.json` at `events[].input_raw` or the captured process stream
before concluding that evidence was never recorded. The reporter digest also
has size bounds; respect its explicit truncation and omission notices.

Per-ask spans run from the first referenced event to the last; they are not
causal delay or automatically recoverable time. Several asks can share events
and tokens. Do not add overlapping ask totals. See the
[measurement protocol](measurement.md) for accounting rules.

A task process can launch other processes, including another AJX trial. Its
wall time includes waiting for them, but its model budget and usage figures are
not an aggregate bill for those processes. Nested workers and reporters have
their own usage and limits. Keep those scopes explicit.

An exposure scan identifies patterns in observed tool inputs. No matches do
not prove that an agent could not access a file or that it made no network
requests. Distinguish supplied materials, observed access, and enforced
environment boundaries.
