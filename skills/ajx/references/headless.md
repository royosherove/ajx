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

Dedicated `ajx ci` and `ajx improve` commands, versioned acceptance policies,
baseline regression gates, JUnit export, and bounded automatic improvement loops
are proposed features. They are not available in the current CLI. The existing
headless execution path supplies their runner and evidence foundation.
