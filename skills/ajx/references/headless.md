# Run an already briefed trial without interaction

The current CLI can execute a prepared trial without a coordinator interview.
An unattended job needs a complete trial file, task prompt, supported harnesses,
and credentials configured before it starts. A pipeline can invoke the CLI
directly; loading the AJX skill in a coordinator is optional.

If a coordinator is present, reuse the supplied task, matrix, environment and
skill profiles, permissions, approved task identity, limits, verification,
cleanup, and output destination. Ask no questions when these are already
established. If required information or authorization is missing, report it and
stop before launching. Do not open a browser login or wait for a human in CI.

## Run and gate a trial with the native CLI

Install a [pinned AJX release](../../../docs/INSTALL.md) and the selected worker and reporter CLIs. Supply
their model credentials and the task's separate credentials through the job's
configuration. For container profiles, preload the pinned image and make the
local Docker daemon available; remote provisioning is not implemented.

With a completed `usability/trial.toml`:

```sh
set -eu
ajx validate usability/trial.toml --json </dev/null
ajx doctor usability/trial.toml --json </dev/null
ajx ci usability/trial.toml --output "$AJX_RESULTS_DIR" \
  --policy usability/acceptance.toml --timeout-grace 60 </dev/null
```

No `--headless` or `--yes` flag is needed by AJX. The selected harness, auth
profile, setup commands and plugins must themselves support unattended use.
Closing stdin does not qualify an interactive login flow for CI. Configure
timeouts and reserve time for evidence collection and cleanup before the job's
hard deadline.

For [environment profiles](environments.md), `doctor` checks prerequisites and
`prepare` checks the actual runtime and environment-local preflight commands.
A successful `doctor` is not proof that the worker can complete its task.

Use a fresh `trial.output_dir` for each new evaluation or product revision.
Reusing a directory resumes its saved attempt and does not rerun completed work.
Keep the complete output directory as a CI artifact even when execution fails;
it contains the matrix, per-run reports, traces and cleanup results. If a task
requires unavailable human input, retain the blocked outcome and describe the
interaction as a gate in its report.

An agent can create the trial with `ajx init --no-input --json`, supplying the
folder, `--product`, `--task` or `--task-file`, worker/reporter choices and repeatable `--check`
flags. Use the returned next steps, then inspect validation and readiness.
Missing prerequisites return errors; do not retry them as if the product failed.
`ajx ci` also validates and checks readiness itself, so a pipeline with a
prepared trial can invoke that command directly.

An explicit `--task` is the complete worker prompt and is written verbatim.
`--material` records supplied materials in the trial metadata; include access
instructions in the task text when the worker needs them. Shell command
substitution removes trailing newlines before AJX sees its argument. Use
`--task-file task.md` to copy a nonempty UTF-8 prompt without changing its bytes;
do not also supply `--task`. The output reports the saved `task-prompt.md` hash
as `prompt_sha256` in native JSON output. Existing files are kept unless
`--force` is supplied, and the hash describes the file actually kept or written.

Supply `--max-budget-usd 5` to write a positive worker budget into the trial.
Enforcement depends on the harness. This does not include reporting costs or
separately launched inner trials; inspect the full plan before approval.

### Check syntax when scaffolding

The native `ajx init` accepts repeatable `--check` arguments:

| Argument | Generated check |
|---|---|
| `--check 'test -s result.json'` | Shell command; exit zero passes |
| `--check 'shell:test -s result.json'` | Explicit shell form |
| `--check 'file:result.json'` | File exists beneath the worker workspace |
| `--check 'http:https://example.com/health'` | HTTP endpoint check |

These are syntax examples, not sufficient verification for every task. If a
task requires correct JSON values, compare the values, not just file existence.
Shell quoting is applied by the calling shell before AJX receives the command.
The annotated [trial configuration](../examples/trial.toml) shows `[[verify]]`
fields for checks authored directly in TOML.

## Interpret the result

`ajx ci` exits 0 only when every cell meets the policy's verified-success target
(default: every attempt). A failed target exits 1; missing, interrupted or
untrustworthy evidence, a doctor problem or a trial without checks exits 4
(inconclusive); configuration errors exit 2, a locked output folder 3, and
cancellation 130 after cleanup. It writes `ci-result.json` and `junit.xml` in the
output folder. An output reuse or concurrency refusal preserves the existing
result. Artifact-write failures return nonzero, and a failed provisional write
prevents execution.

Use `ajx status "$AJX_RESULTS_DIR" --json` to inspect a pipeline's output.
For a trial using its configured output folder, `ajx console since <trial>` or
`wait <trial>` exposes progress without opening a browser. Honor `trial_error`
and `process.stale_lock`; neither indicates readiness to start.

Resume an interrupted evaluation with the same inputs and `--allow-reuse`.
AJX checks its recorded worker ownership before recovery. If ownership cannot
be established, it preserves the workspace and reports unconfirmed cleanup.
Do not remove state files or change checks to force reuse. Choose a fresh output
folder for a changed product, prompt, check or lifecycle command.

`ajx run` performs an investigation without an acceptance policy. Its exit code
reflects stage and cleanup status; failed task verification can still exit 0.
Use `ajx ci` for an automated product gate, and inspect the evidence and asks
when deciding what to improve.

See [Read AJX results programmatically](artifacts.md) for artifact field paths,
raw transcript decoding, and examples of checking what a passing result proves.

`ajx improve`, baseline regression gates, cost and time limits in the policy, and
bounded automatic improvement loops remain proposals. The Python CLI has no
`ajx ci`. For Python plugins, use the Python launcher's `validate`, `doctor` and
`run`, then evaluate planned coverage, verification and cleanup explicitly.
