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
| `run.json` | `constraints.spending_limit` | Requested task-worker budget in USD; not total trial cost |
| `state.json` | `stages.<stage>.status` | Live stage state, such as `running`, `done`, or `error` |
| `state.json` | `execute.requested` | Requested harness, model, effort, configuration, and auth profile |
| `state.json` | `execute.argv` | Recorded worker launch arguments, when supplied by the adapter |
| `state.json` | `paths`, `archived` | Original per-run directories and their saved destinations; see below |
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

`ajx run` also prints timestamped stage transitions as state is saved, including
reporting stages. The native CLI sends these progress lines to stderr with
`--json`, leaving stdout as the final JSON result. A stage that remains
`running` needs a later completion or error transition; elapsed time alone
does not establish either.

## Environment and process records

`environment.json` describes preparation and any later environment updates.
It does not contain the worker launch arguments; look in `state.json` at
`execute.argv`. A failed preparation can leave a partial environment record.

| Field in `environment.json` | Meaning |
|---|---|
| `taken_at`, `probes` | Snapshot time and observed platform/tool versions |
| `harness` | Worker name, version and adapter capabilities |
| `product_version_before`, `product_version_after` | Product version probe results, when configured |
| `runner`, `config`, `cache_state` | Runner description and selected configuration/cache behavior |
| `auth`, `task_identity`, `default_aws_identity` | Recorded authentication route and identity probe results; these can contain private output |
| `worker_env_names`, `caller_env_names` | Selected environment variable names, not their values |
| `env_checks` | Configuration checks, observed overrides, collisions, and identity warnings |
| `execution_environment` | Actual explicit-environment report, including backend capabilities; `null` is normal for legacy local runs |
| `agent_configuration` | Explicit agent-profile manifest, when selected; otherwise it can be `null` |
| `environment_cleanup` | Explicit environment release result, when attempted; separate from task teardown |

An absent explicit environment report does not mean the worker did not run.
Likewise, `config = "clean"` describes a requested mode, not proof of a fresh
home directory or restricted filesystem. Check `state.execute.config_isolated`
for the adapter's recorded configuration-isolation result.

Within `env_checks`, `configuration` explains the requested mode and whether
the selected auth route permits config isolation. `shell_startup` records
prepared controls through `configured_env_names` and `unset_env_names`;
`evidence_scope` distinguishes a doctor probe from worker preparation.
`startup_files_read = null` means file reads were not traced. These fields do
not establish which startup files the worker actually opened.
`aws_settings_sources` lists source paths and variable names for effective
AWS-related harness settings, without their values. See
[the briefing reference](briefing.md) for interpreting inherited configuration.

The native CLI's `execute.process.json` has a different purpose: safely
identifying and stopping an owned process group during recovery.

| Field in `execute.process.json` | Meaning |
|---|---|
| `version` | Process-control record format version |
| `pid` | Owned process-group leader, which can differ from the worker PID |
| `host` | Host where the process was launched |
| `token`, `socket` | Private control authentication token and endpoint |
| `stopped` | Whether the launcher recorded that the owned process group stopped |

Keep this file private and unchanged; it is not a command trace or a public
report. It may be absent for older runs, the Python launcher, or a harness that
never launched a tracked process. Use `state.execute.worker_pid` when available
for the task process, and `state.execute` for its timing, exit and stop reason.
Do not signal a process based only on a copied or old PID; use AJX's recovery
commands.

## Locate files and interpret cleanup

`state.json` records original paths under `paths`. A path's role is determined
by its key, not by its random directory name:

| Key | Role | Usual destination recorded in `archived` |
|---|---|---|
| `workspace` | Task working files | `<run-dir>/workspace` |
| `config_dir` | Per-run harness configuration directory; actual use depends on adapter and auth | `<run-dir>/harness-config` |
| `cache_dir` | Package-cache directory used when isolated caches are selected | No archive entry; AJX attempts to remove it |
| `home_dir` | Agent home for an explicit environment | `<run-dir>/agent-home` |

Legacy local runs normally have no `home_dir`. Their `cache_dir` is not an
agent home, and creating a `config_dir` does not prove the harness used it.
After archiving, prefer the matching `archived` destination when present;
original `paths` entries can point to moved or removed directories.

```sh
jq '{paths, archived, worker_cleanup, teardown, environment_cleanup,
     archive: .stages.archive}' "$AJX_RUN_DIR/state.json"
```

The cleanup records answer different questions:

- `worker_cleanup.confirmed` records whether the native runner confirmed no
  owned worker remains.
- `teardown.status` summarizes the configured commands that clean up task
  resources; details are in `teardown.json`.
- `environment_cleanup.confirmed` records explicit backend release
  confirmation, when applicable.
- `stages.archive.status` and `archived` describe saving owned directories
  after cleanup and purging known credential files.

An archive marked `done` is not independent proof that every cache, remote
resource, or arbitrary secret was removed. Missing cleanup fields mean no such
record is available. Inspect the relevant results and verify task-specific
resources when that is part of the requested outcome. Archived files and
identity output can still contain private data.

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
Its `skills_inventory` entries preserve each reported `name` and explicit
`source`, with `source_status` set to `reported` or `unknown`. A string-only
skill listing has `source: null`; do not infer its origin from its name.
The same inventory is exposed in `run.json` at
`reported_extension_inventory.skills_inventory`. A missing list remains `null`,
while an empty list means the harness reported no entries.

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

For budget evidence, `run.json` records the requested `trial.max_budget_usd` as
`constraints.spending_limit`. Inspect `state.execute.argv` to see the arguments
the adapter recorded for the worker. For Claude Code, a supplied budget is
forwarded as `--max-budget-usd`. This shows that a limit was requested and
forwarded; it does not independently prove provider enforcement or cap the
reporter's calls, nested trials, or other tool spending. Other adapters may not
support this limit. A `null` spending limit means none was requested.

```sh
jq '.constraints | {time_limit_seconds, spending_limit}' "$AJX_RUN_DIR/run.json"
jq '.execute | {requested, argv, started_at, stopped_at, stop_reason}' \
  "$AJX_RUN_DIR/state.json"
```

Keep recorded arguments private: they can contain user-supplied values.

An exposure scan identifies patterns in observed tool inputs. No matches do
not prove that an agent could not access a file or that it made no network
requests. Distinguish supplied materials, observed access, and enforced
environment boundaries.
