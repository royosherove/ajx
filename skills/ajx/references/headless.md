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

## Execute with the existing CLI

Install a pinned AJX revision and the selected worker and reporter CLIs. Supply
their model credentials and the task's separate credentials through the job's
configuration. For container profiles, preload the pinned image and make the
local Docker daemon available; remote provisioning is not implemented.

With a completed `usability/trial.toml`:

```sh
set -eu
ajx validate usability/trial.toml </dev/null
ajx doctor usability/trial.toml </dev/null
ajx run usability/trial.toml </dev/null
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

## Execution status is not product acceptance

`ajx run` returns the current `status` result. Stage errors and recorded cleanup
failures produce a nonzero status. A failed task verification alone can still
leave the command with exit code zero. Missing or interrupted work is also not
covered by a complete acceptance policy today.

Evaluate the planned cells, `verify.json`, `run.json`, measurement coverage and
cleanup evidence before deciding whether a product is acceptable. Do not use
the CLI exit code or an empty asks list as the sole product quality gate.

## Decide acceptance with `ajx ci` (Go CLI)

The [Go CLI](../../../docs/GO-CLI.md) adds `ajx ci`. It runs the same validate,
doctor and run steps without prompting, in a fresh output folder, then decides
from the recorded task checks of every planned attempt:

```sh
ajx ci usability/trial.toml --output "$AJX_RESULTS_DIR" \
  --policy usability/acceptance.toml --timeout-grace 60 </dev/null
```

It exits 0 only when every cell meets the policy's verified-success target
(default: every attempt). A failed target exits 1; missing, interrupted or
untrustworthy evidence, a doctor problem or a trial without checks exits 4
(inconclusive); configuration errors exit 2, a locked output folder 3, and
cancellation 130 after cleanup. It writes `ci-result.json` and `junit.xml` in the
output folder, including when it fails.

`ajx improve`, baseline regression gates, cost and time limits in the policy, and
bounded automatic improvement loops remain proposals. The Python CLI has no
`ajx ci`; use the checks above with it.
