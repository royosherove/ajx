# Change note: fixes from coordinating container trials

**Status: implemented on `feature/trial-experience-fixes`; awaiting review.**

This note describes a set of changes made after a coordinator ran three
container-backed trials from a macOS machine (Colima as the Docker engine,
Claude Code as the worker, Bedrock as the model provider) against a cloud
product. Every change below removes a concrete obstacle that cost the
coordinator time or weakened the evidence. The trial contents are not part of
this repository; only the mechanics that AJX controls are described.

Reviewers: each section names the behavior before, the behavior after, the
soundness argument where a check was touched, and the tests that cover it.

## Summary of changes

| Area | Before | After |
|---|---|---|
| Container creation failure | "Docker creation failed; no fallback or task retry was attempted" | Same message plus the engine's exit code and stderr tail, a VM file-sharing hint when it applies, and a `creation_failure` record in the environment report |
| `doctor` for container backends | Inspected image and daemon only | Also runs a short, unprivileged bind-mount probe of `trial.workspace_root`; a failed probe makes doctor NOT READY |
| Fixture/mount inspection race | "starting directory changed while being inspected", no entry or attribute | Names the entry and attribute; one full re-inspection before failing closed |
| Skill frontmatter | `allowed-tools`, `version`, and `.gitkeep` rejected; coordinators forked the skill under test | Accepted; `allowed-tools` recorded as declared-not-enforced; empty `.gitkeep`/`.keep` allowed |
| Claude Code sessions with several `result` events | Only the last event counted; tokens flagged partial | All result events aggregated; reconciliation completes; segment count recorded |
| Sharing a run | Manual scrubbing of HTML paths and links | `ajx export` writes `report.html`, `journey.html`, `asks.md` with neutral paths |
| Watching a long run | `status` showed only `execute: run` | `status --tail N` prints recent events through `Harness.progress()` |
| `status` without secrets | Failed on unresolved `${VAR}` references | Cleanup-only loading skips caller-secret readiness checks |
| Bedrock region for tools | Only `AWS_REGION` set; botocore-based tools saw no region | `AWS_DEFAULT_REGION` mirrored; identical explicit value is not a collision |
| Command environment | `AJX_RUN_TOKEN` only | `AJX_TRIAL_DIR` added for setup, preflight, verify and teardown |
| Credential purge | `auth.json`, `credentials*`, `token*.json` | Also `.npmrc`, `.netrc`, `.pypirc`, `.git-credentials` |

## 1. Container creation diagnostics

`environments.py` already captured the engine's stderr in `_docker()` but the
`prepare()` failure path discarded it. The real cause in the observed case was
`bind source path does not exist: /private/tmp/...`: VM-based engines (Colima,
Docker Desktop, Rancher Desktop) can only bind-mount directories the VM shares,
usually the home directory, and the scaffolded `workspace_root = "/tmp"` is not
shared.

After: the `EnvironmentError` includes the exit code and a sanitized stderr
tail; when stderr matches the known file-sharing patterns the message adds a
hint to move `workspace_root` under a shared directory. The failure is also
recorded as `creation_failure` in the environment report so a later reader of
`environment.json` sees why the container was never present. `_docker_json`
failures (image inspection) carry the same detail.

No security boundary changed. The stderr tail comes from the management
process's own engine call and contains no worker or caller secrets.

## 2. Doctor bind-mount probe

`doctor()` previously promised to create no containers, which is why it could
not detect the mount failure above. It now creates a private temporary
directory directly under `trial.workspace_root`, starts a container with the
same posture as the real attempt (`--rm`, `--pull never`, `--network none`,
read-only root, all capabilities dropped, no privilege escalation, non-root
numeric user) that bind-mounts that directory read-only and runs
`test -d /ajx-probe`, then removes the directory. The result is reported as
`mount_probe` with `ok`, `failed` or `skipped`; a failed probe sets the doctor
status to `unavailable`, which `ajx doctor` prints as NOT READY. The probe is
skipped, with a reason, for the local backend, when no `workspace_root` is in
the doctor context, when the root does not exist yet (doctor creates nothing
outside its own temporary directory; `ajx run` creates the root), or after a
failed image or daemon check.

Trade-off: doctor is no longer a pure read-only inspection. The probe runs
nothing from the trial, mounts nothing but an empty directory, and uses the
pinned image only. It is the cheapest check that distinguishes "the engine
exists" from "the engine can run this trial".

## 3. Fixture inspection diagnostics and single retry

`_tree_manifest` hashes and copies fixture and mount trees through descriptor
relative reads and compares size, mtime, ctime and link count before and after
reading each file. On macOS, indexer and extended-attribute writes change
`ctime` shortly after a file is created, which produced repeated false
positives with a message that named neither the entry nor the attribute.

After: the message names the relative entry and the attributes that differed
(`dev/ino`, `type`, `size`, `mtime`, `ctime`, `nlink`). The condition is a
dedicated `ValueError` subclass, `_InspectionChanged`, so callers that already
catch `ValueError` are unchanged. If the first pass fails on it, the whole
inspection runs again from scratch (fresh entries, totals and descriptors; a
partially written destination is removed first) after a 0.5 s pause. A second
failure raises with the detailed message and `(reproduced on re-inspection)`.

Soundness: the recorded hash is always computed from the same bytes that were
copied, within one pass. A retry does not mix passes; it discards the first
pass entirely. An adversary who changes the tree between passes still cannot
make the recorded hash differ from the copied bytes.

## 4. Skill frontmatter fidelity

AJX exists to test a skill as shipped and to pin its content hash. Rejecting
`allowed-tools` and `version`, and rejecting an empty `scripts/.gitkeep`,
forced the coordinator to edit the skill before every trial, so the recorded
hash matched no published artifact.

After: `allowed-tools` is parsed into `declared_tool_grants` on the skill entry
(grants split on whitespace or commas outside parentheses, so both the Agent
Skills and Claude Code spellings keep `Bash(git add:*)` intact) and the
configuration manifest carries a limitation that AJX records tool grants but
does not enforce them (a worker with approval prompts bypassed is not restricted
by them); reports show it under profile limitations. `version` is recorded as
descriptive metadata. Empty `.gitkeep` and `.keep` files are copied and hashed
like other regular files; non-empty placeholders and all other hidden entries
are still rejected. Fields that imply execution semantics AJX cannot resolve
(hooks, dependencies) remain rejected.

Compatibility: `resume_contract()` omits `version` when absent and
`declared_tool_grants` when empty, so matrix plans saved before this change
still match their attempts on resume. Skills that declare these fields had no
saved plans, because the old parser rejected them.

## 5. Several `result` events in one Claude Code session

In `claude -p` stream-json output, a session can emit more than one `result`
event when the agent schedules its own continuation. In the observed run the
four events carried per-segment `num_turns`, `duration_ms` and `usage`, while
`total_cost_usd` was cumulative. `normalize()` kept only the last event, so the
report showed one turn and five seconds for a fourteen-minute run, and the
output-token reconciliation flagged a complete transcript as partial.

After: all result events are collected. Turns, durations and usage are summed;
cost is taken as the final cumulative value when the series is non-decreasing
and summed otherwise, with a limitation noting the fallback. `result_events`
is recorded, and a limitation explains the aggregation when there is more than
one. `report()` uses the last event for text and structured output.

## 6. `ajx export`

`ajx export <trial|output> --run <id> [--dest DIR] [--force]` writes exactly
three files, `report.html`, `journey.html` (renamed from `pretty-journey.html`)
and `asks.md`, with the attempt's directories recorded in `state.json`
replaced by neutral labels (`workspace` to `/workspace`, `home_dir` to
`/agent-home`, `config_dir` to `/agent-home/.config/harness`, `cache_dir` to
`/agent-home/.cache`), their dash-encoded forms (as they appear in Claude Code
project directory names) and archived copies replaced likewise, sibling attempt
directories under the same root replaced by `/agent-home/attempt`, the bare
root by `/agent-home/attempts`, and the coordinator's home replaced by `~`. All rules apply in one longest-first pass,
so inserted labels are never rewritten. Links to files outside the bundle
(`run.json`, `verify.json`, `environment.json`, `agent-configuration.json`, the
matrix index) are unwrapped to plain text and list items that only carry such a
link are dropped; the brand link loses its href. The command prints the
replacement counts and a reminder that exported reports still contain commands,
outputs, account identifiers and URLs. The default destination is
`<output>/share/<run-id>`; a non-empty destination needs `--force`.

This is text post-processing of rendered files, not a second renderer, and not
a redaction: strings the agent itself typed (for example a directory name quoted
in a command) are left as recorded. The run's own `report.html` and
`pretty-journey.html` are untouched.

## 7. `ajx status --tail N`

`Harness.progress(run_dir, stage, limit)` is a new optional adapter hook that
returns recent human-readable events from the stage's raw stream. The Claude
Code adapter implements it from `execute.raw.jsonl` (assistant text, tool calls
with a compact input excerpt, and result events), tolerating a file that is
still being written; the default returns an empty list. `status --tail N` prints
the last N events for runs whose execute stage is still running, or for every
run when none is. The adapter comes from the trial cell, or from `run.json` /
`environment.json` when `status` is given an output directory. A missing hook,
an unknown harness or a raising `progress()` prints a one-line note; `status`
never fails because of it.

## 8. `status` without caller secrets

`spec.load(path, cleanup_only=True)` still performed auth readiness checks
that resolve `${VAR}` references against the caller's environment, so
`ajx status` failed in any shell without the cell secrets exported. Cleanup-only
loading now skips those checks; structural validation still runs, and `run`,
`validate` and `doctor` keep the full checks.

## 9. Bedrock region for worker tools

The `claude-bedrock` route declares `AWS_REGION`, which the AWS CLI honors but
pip-installed botocore does not. Tools in the worker that use botocore saw no
region. The route now mirrors `AWS_REGION` into `AWS_DEFAULT_REGION` when the
latter is not declared, and `envcheck` no longer reports a collision when the
task declares the same key with an identical value. Different values still
produce the collision warning.

## 10. `AJX_TRIAL_DIR`

Setup, preflight, verify and teardown commands receive `AJX_TRIAL_DIR`, the
absolute directory containing `trial.toml`, next to `AJX_RUN_TOKEN`
(`envcheck.check_vars()` builds both for the run, doctor and validate paths).
The worker's harness never receives either variable, and a container does not
see the host path, so environment-located commands should not rely on it. Host
located checks no longer need hard-coded absolute paths to trial fixtures.

## 11. Credential purge names

`CREDENTIAL_FILES` gains `.npmrc`, `.netrc`, `.pypirc` and `.git-credentials`.
A registry token written to the worker home by a setup step is now removed
from the archived home. The purge still walks only the home and configuration
directories, so a project-level `.npmrc` inside the archived workspace is kept
as evidence. This remains a name-based purge; content scanning is listed below
as future work.

## Not changed, recorded for follow-up

- `ajx init` does not scaffold a container profile; coordinators copy from
  `examples/environment-profiles/`. A `--environment container` flag would help.
- The management PATH requires a `docker` CLI; Finch and nerdctl are not
  supported. Making the engine binary configurable is a separate decision.
- Multi-segment sessions were only observed with Claude Code. Codex and Kiro
  adapters keep their current single-result behavior.
- Archived homes are purged by file name only. A content scan for token-like
  strings would catch files this list does not name.
- A live dashboard remains a separate proposal (see `live-dashboard.md`).

## Verification

Run the offline suite from the repository root:

```sh
python3 -I -m unittest discover -s skills/ajx/tests -p 'test_*.py'
```

New tests use synthetic fixtures and fake engines only; none run a live agent
or contact a cloud provider. The suite grew from 286 to 353 tests:
`test_environment_diagnostics.py` (creation failure, doctor probe, inspection
retry), `test_agent_configuration.py` (frontmatter, placeholders, resume
contract), `test_claude_code_stream.py` (result folding, `progress()`),
`test_export.py` (export rules, CLI, `status --tail`) and
`test_coordinator_fixes.py` (cleanup-only loading, region mirroring and
collisions, `AJX_TRIAL_DIR`, purge names).

The author also checked the changes against existing local trial output and a
running Colima daemon without committing any of it: re-normalizing a recorded
four-segment session produced summed turns and duration with a complete token
reconciliation; `ajx export` left no coordinator path in the bundle; and
`ajx doctor` reported the bind-mount probe `ok` for a home-rooted workspace and
`FAILED` with the engine's error and the hint for a `/tmp` root.
